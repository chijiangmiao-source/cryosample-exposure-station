// Package store persists freezer batch state and exposure events in SQLite.
//
// A batch starts inside the cabinet ("in") with zero accumulated exposure.
// Only alternating transitions are allowed: in -> takeout -> out, then
// out -> return -> in. A return adds (return time - matching takeout time)
// seconds to the accumulated exposure. The batch stays usable while the
// accumulated total is <= the allowed seconds; exceeding the limit scraps
// the batch permanently. Every event timestamp must be strictly later than
// the batch's previous timestamp.
//
// A mistaken scan can be revoked: only the latest non-revoked event of the
// batch may be revoked, and the revocation records its own whole-second
// timestamp plus a non-empty reason. Revoking a takeout moves the batch back
// into the cabinet; revoking a return subtracts that exposure again, restores
// the out-of-cabinet state and re-evaluates usability. Revocation never
// deletes rows: the event keeps revoked_at / revoke_reason for the audit
// trail.
//
// Cabinet locations are tracked independently of the exposure state machine:
// an in-cabinet usable batch may occupy at most one location and a location
// may hold at most one batch. A successful takeout releases the occupancy in
// the very same event transaction (a batch outside the cabinet occupies no
// slot); a return leaves the batch unlocated until it is placed again, and
// revoking a return likewise vacates its slot.
//
// Backup barcodes (aliases) provide an alternate label for the same batch:
// scanning either the primary barcode or a bound alias resolves to the same
// canonical batch, so timing, revocation and location records stay shared.
// Aliases live in their own namespace ledger (batch_aliases): an alias is
// globally unique, may never equal any primary barcode, and creating a batch
// reuses that same shared namespace.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Batch states and lifecycle statuses.
const (
	StateIn  = "in"
	StateOut = "out"

	StatusUsable   = "usable"
	StatusScrapped = "scrapped"

	EventTakeout = "takeout"
	EventReturn  = "return"
)

var (
	// ErrNotFound is returned when the barcode does not exist.
	ErrNotFound = errors.New("batch not found")
	// ErrDuplicate is returned when creating a batch with an existing barcode.
	ErrDuplicate = errors.New("barcode already exists")
)

// ConflictError describes a rejected state transition (mapped to HTTP 409).
type ConflictError struct {
	Code    string
	Message string
}

func (e *ConflictError) Error() string { return e.Message }

func conflict(code, format string, args ...any) *ConflictError {
	return &ConflictError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Batch is the persisted freezer-batch aggregate.
type Batch struct {
	Barcode            string
	AllowedSeconds     int64
	State              string // StateIn | StateOut
	Status             string // StatusUsable | StatusScrapped
	AccumulatedSeconds int64
	CreatedAt          time.Time
	LastAt             time.Time  // createdAt, or the last accepted event time
	LastEventType      *string    // nil when no event has happened yet
	LastEventAt        *time.Time // nil when no event has happened yet
	LastTakeoutAt      *time.Time // open takeout time, set while State == StateOut
	// Location is the cabinet slot currently occupied by the batch; nil while
	// the batch is unplaced (outside the cabinet, freshly returned, or not yet
	// shelved).
	Location *string
}

// Event is one accepted takeout/return record. A revoked event stays in the
// log: RevokedAt/RevokeReason carry the audit trail of the undo.
type Event struct {
	ID           int64
	Barcode      string
	Type         string
	At           time.Time
	DeltaSeconds *int64 // exposure seconds added by a return; NULL for takeouts
	RevokedAt    *time.Time
	RevokeReason *string
}

// Store wraps the SQLite handle.
type Store struct {
	db *sql.DB
}

// Open opens (and migrates) the SQLite database at path.
func Open(ctx context.Context, path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_busy_timeout=5000&_txlock=immediate", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// SQLite allows a single writer; one connection serializes all
	// transactions so a stale state can never be committed twice.
	db.SetMaxOpenConns(1)
	if err := migrate(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

func migrate(ctx context.Context, db *sql.DB) error {
	// Additive migration for databases created before revocation existed:
	// SQLite has no "ADD COLUMN IF NOT EXISTS", so check pragma first.
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'events'`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		for _, col := range []string{"revoked_at", "revoke_reason"} {
			var exists int
			if err := db.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM pragma_table_info('events') WHERE name = ?`, col).Scan(&exists); err != nil {
				return err
			}
			if exists == 0 {
				if _, err := db.ExecContext(ctx,
					`ALTER TABLE events ADD COLUMN `+col+` TEXT`); err != nil {
					return err
				}
			}
		}
	}

	stmts := []string{
		`CREATE TABLE IF NOT EXISTS batches (
			barcode             TEXT PRIMARY KEY,
			allowed_seconds     INTEGER NOT NULL CHECK (allowed_seconds > 0),
			state               TEXT NOT NULL CHECK (state IN ('in', 'out')),
			status              TEXT NOT NULL CHECK (status IN ('usable', 'scrapped')),
			accumulated_seconds INTEGER NOT NULL DEFAULT 0 CHECK (accumulated_seconds >= 0),
			created_at          TEXT NOT NULL,
			last_at             TEXT NOT NULL,
			last_event_type     TEXT,
			last_event_at       TEXT,
			last_takeout_at     TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS events (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			barcode       TEXT NOT NULL REFERENCES batches (barcode),
			type          TEXT NOT NULL CHECK (type IN ('takeout', 'return')),
			at            TEXT NOT NULL,
			delta_seconds INTEGER,
			revoked_at    TEXT,
			revoke_reason TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS idx_events_barcode ON events (barcode, id)`,
		// location_occupancy is the independent shelving ledger: at most one
		// active row per batch (PRIMARY KEY) and per location (UNIQUE). A batch
		// outside the cabinet, a freshly returned batch and a not-yet-shelved
		// batch simply have no row; a takeout deletes its row inside the same
		// event transaction.
		`CREATE TABLE IF NOT EXISTS location_occupancy (
			barcode  TEXT PRIMARY KEY REFERENCES batches (barcode),
			location TEXT NOT NULL UNIQUE
		)`,
		// batch_codes is the shared namespace of every scannable code: each
		// batch owns exactly one "primary" row (code == barcode) and zero or
		// more "alias" rows (backup barcodes bound to the same canonical
		// batch). The PRIMARY KEY on code makes a code globally unique: an
		// alias can neither duplicate another alias nor equal any primary
		// barcode, and creating a batch reuses the same constraint.
		`CREATE TABLE IF NOT EXISTS batch_codes (
			code    TEXT PRIMARY KEY,
			barcode TEXT NOT NULL REFERENCES batches (barcode),
			kind    TEXT NOT NULL CHECK (kind IN ('primary', 'alias'))
		)`,
		`CREATE INDEX IF NOT EXISTS idx_batch_codes_barcode ON batch_codes (barcode)`,
	}
	for _, q := range stmts {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	// Backfill primary codes for databases created before aliases existed
	// (every existing batch barcode is its own primary code).
	if _, err := db.ExecContext(ctx,
		`INSERT OR IGNORE INTO batch_codes (code, barcode, kind)
		 SELECT barcode, barcode, 'primary' FROM batches`); err != nil {
		return err
	}
	return nil
}

// ts formats a timestamp the way it is stored: RFC3339 whole seconds, UTC.
func ts(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func parseTS(s string) (time.Time, error) {
	return time.Parse(time.RFC3339, s)
}

// CreateBatch inserts a new batch in the initial state (in cabinet, zero
// accumulated exposure). It returns ErrDuplicate if the barcode is already
// taken by another primary barcode or by a bound alias — every scannable code
// shares one namespace. The batch row and its primary code row commit together.
func (s *Store) CreateBatch(ctx context.Context, barcode string, allowedSeconds int64, createdAt time.Time) (*Batch, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `INSERT INTO batches
		(barcode, allowed_seconds, state, status, accumulated_seconds, created_at, last_at)
		VALUES (?, ?, ?, ?, 0, ?, ?)`,
		barcode, allowedSeconds, StateIn, StatusUsable, ts(createdAt), ts(createdAt)); err != nil {
		if isUniqueViolation(err) {
			return nil, ErrDuplicate
		}
		return nil, err
	}
	// Register the primary code in the shared namespace. Its PRIMARY KEY also
	// rejects a barcode that collides with an existing alias; the foreign key
	// binds it to the batch just inserted.
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO batch_codes (code, barcode, kind) VALUES (?, ?, 'primary')`,
		barcode, barcode); err != nil {
		if isUniqueViolation(err) {
			return nil, ErrDuplicate
		}
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetBatch(ctx, barcode)
}

func isUniqueViolation(err error) bool {
	// modernc.org/sqlite reports "UNIQUE constraint failed" (code 1555/2067).
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

const batchCols = `b.barcode, b.allowed_seconds, b.state, b.status, b.accumulated_seconds,
	b.created_at, b.last_at, b.last_event_type, b.last_event_at, b.last_takeout_at, o.location`

// batchSelect loads the batch aggregate together with its current slot from
// the independent location_occupancy ledger (NULL when the batch is unplaced).
const batchSelect = `SELECT ` + batchCols + `
	FROM batches b
	LEFT JOIN location_occupancy o ON o.barcode = b.barcode
	WHERE b.barcode = ?`

// GetBatch loads a batch by barcode, or ErrNotFound.
func (s *Store) GetBatch(ctx context.Context, barcode string) (*Batch, error) {
	return scanBatch(s.db.QueryRowContext(ctx, batchSelect, barcode))
}

func scanBatch(row *sql.Row) (*Batch, error) {
	var b Batch
	var createdAt, lastAt string
	var lastEventType, lastEventAt, lastTakeoutAt, location sql.NullString
	err := row.Scan(&b.Barcode, &b.AllowedSeconds, &b.State, &b.Status,
		&b.AccumulatedSeconds, &createdAt, &lastAt,
		&lastEventType, &lastEventAt, &lastTakeoutAt, &location)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if b.CreatedAt, err = parseTS(createdAt); err != nil {
		return nil, err
	}
	if b.LastAt, err = parseTS(lastAt); err != nil {
		return nil, err
	}
	if lastEventType.Valid {
		v := lastEventType.String
		b.LastEventType = &v
	}
	if lastEventAt.Valid {
		t, err := parseTS(lastEventAt.String)
		if err != nil {
			return nil, err
		}
		b.LastEventAt = &t
	}
	if lastTakeoutAt.Valid {
		t, err := parseTS(lastTakeoutAt.String)
		if err != nil {
			return nil, err
		}
		b.LastTakeoutAt = &t
	}
	if location.Valid {
		v := location.String
		b.Location = &v
	}
	return &b, nil
}

// Projection is a read-only risk estimate of a batch's exposure totals at a
// given time. It is never persisted and never changes the batch: a projected
// over-limit result does not scrap anything.
type Projection struct {
	AsOf time.Time
	// AccumulatedSeconds is the settled total for an in-cabinet batch, or the
	// settled accumulated seconds plus the open takeout's elapsed seconds for
	// a batch currently out.
	AccumulatedSeconds int64
	RemainingSeconds   int64
	Usable             bool
	// Settled is true for in-cabinet batches: the figures are already settled
	// totals rather than a projection over an open takeout.
	Settled bool
}

// EvaluateAt returns a batch together with a non-persistent exposure
// evaluation at asOf. For a batch out of the cabinet it projects the total
// from the settled accumulated seconds and the still-open takeout time; for a
// batch inside the cabinet it returns the settled totals. It fails with a
// time_not_monotonic ConflictError when asOf is earlier than the batch's last
// event (or its creation time when it has no events). Nothing is written: the
// event log, accumulated total, status and subsequent timing are unaffected.
func (s *Store) EvaluateAt(ctx context.Context, barcode string, asOf time.Time) (*Batch, *Projection, error) {
	b, err := s.GetBatch(ctx, barcode)
	if err != nil {
		return nil, nil, err
	}
	if asOf.Before(b.LastAt) {
		return nil, nil, conflict("time_not_monotonic",
			"evaluation time %s must not be earlier than the batch's last event time %s",
			ts(asOf), ts(b.LastAt))
	}
	p := &Projection{
		AsOf:               asOf,
		AccumulatedSeconds: b.AccumulatedSeconds,
	}
	if b.State == StateOut && b.LastTakeoutAt != nil {
		p.AccumulatedSeconds += int64(asOf.Sub(*b.LastTakeoutAt).Seconds())
	} else {
		p.Settled = true
	}
	p.RemainingSeconds = b.AllowedSeconds - p.AccumulatedSeconds
	p.Usable = p.AccumulatedSeconds <= b.AllowedSeconds
	return b, p, nil
}

// ListEvents returns the whole event log of a batch in insertion order,
// including revoked events with their revocation audit fields.
func (s *Store) ListEvents(ctx context.Context, barcode string) ([]Event, error) {
	if _, err := s.GetBatch(ctx, barcode); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, barcode, type, at, delta_seconds, revoked_at, revoke_reason
		 FROM events WHERE barcode = ? ORDER BY id`, barcode)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		ev, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *ev)
	}
	return out, rows.Err()
}

// LastEvent returns the latest non-revoked event of a batch, or nil when the
// batch has no active event.
func (s *Store) LastEvent(ctx context.Context, barcode string) (*Event, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, barcode, type, at, delta_seconds, revoked_at, revoke_reason
		 FROM events WHERE barcode = ? AND revoked_at IS NULL ORDER BY id DESC LIMIT 1`, barcode)
	return scanEventRow(row)
}

type scanner interface {
	Scan(dest ...any) error
}

func scanEventRow(row *sql.Row) (*Event, error) {
	ev, err := scanEvent(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return ev, nil
}

func scanEvent(row scanner) (*Event, error) {
	var ev Event
	var at string
	var delta sql.NullInt64
	var revokedAt, reason sql.NullString
	if err := row.Scan(&ev.ID, &ev.Barcode, &ev.Type, &at, &delta, &revokedAt, &reason); err != nil {
		return nil, err
	}
	t, err := parseTS(at)
	if err != nil {
		return nil, err
	}
	ev.At = t
	if delta.Valid {
		d := delta.Int64
		ev.DeltaSeconds = &d
	}
	if revokedAt.Valid {
		t, err := parseTS(revokedAt.String)
		if err != nil {
			return nil, err
		}
		ev.RevokedAt = &t
	}
	if reason.Valid {
		v := reason.String
		ev.RevokeReason = &v
	}
	return &ev, nil
}

// ApplyEvent validates and commits one takeout/return event inside a single
// transaction. Rejected transitions write nothing: no event row, no state or
// accumulated change. The conditional UPDATEs pin the previously read state
// so that, even under concurrency, one stale state can succeed at most once.
func (s *Store) ApplyEvent(ctx context.Context, barcode, eventType string, at time.Time) (*Batch, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	b, err := scanBatch(tx.QueryRowContext(ctx, batchSelect, barcode))
	if err != nil {
		return nil, err
	}

	if !at.After(b.LastAt) {
		return nil, conflict("time_not_monotonic",
			"event time %s must be strictly later than the batch's previous time %s",
			ts(at), ts(b.LastAt))
	}

	var delta *int64
	switch eventType {
	case EventTakeout:
		if b.Status == StatusScrapped {
			return nil, conflict("batch_scrapped", "scrapped batch cannot be taken out again")
		}
		if b.State != StateIn {
			return nil, conflict("invalid_transition", "batch is already out of the cabinet")
		}
		res, err := tx.ExecContext(ctx, `UPDATE batches
			SET state = ?, last_at = ?, last_event_type = ?, last_event_at = ?, last_takeout_at = ?
			WHERE barcode = ? AND state = ? AND status = ? AND last_at = ?`,
			StateOut, ts(at), EventTakeout, ts(at), ts(at),
			barcode, StateIn, StatusUsable, ts(b.LastAt))
		if err != nil {
			return nil, err
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			return nil, conflict("invalid_transition", "batch state changed concurrently; retry")
		}
		// Leaving the cabinet vacates the slot in the very same transaction so
		// a takeout can never leave a dangling occupancy behind.
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM location_occupancy WHERE barcode = ?`, barcode); err != nil {
			return nil, err
		}

	case EventReturn:
		if b.State != StateOut || b.LastTakeoutAt == nil {
			return nil, conflict("invalid_transition", "batch is not out of the cabinet")
		}
		d := int64(at.Sub(*b.LastTakeoutAt).Seconds())
		newAccumulated := b.AccumulatedSeconds + d
		newStatus := StatusUsable
		if newAccumulated > b.AllowedSeconds {
			newStatus = StatusScrapped
		}
		delta = &d
		res, err := tx.ExecContext(ctx, `UPDATE batches
			SET state = ?, accumulated_seconds = ?, status = ?,
			    last_at = ?, last_event_type = ?, last_event_at = ?, last_takeout_at = NULL
			WHERE barcode = ? AND state = ? AND last_at = ?`,
			StateIn, newAccumulated, newStatus, ts(at), EventReturn, ts(at),
			barcode, StateOut, ts(b.LastAt))
		if err != nil {
			return nil, err
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			return nil, conflict("invalid_transition", "batch state changed concurrently; retry")
		}
		// A returned batch always comes back unplaced: the operator scans its
		// new slot afterwards. The row is normally already gone (the takeout
		// deleted it), but the DELETE restates that inside this transaction.
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM location_occupancy WHERE barcode = ?`, barcode); err != nil {
			return nil, err
		}

	default:
		return nil, fmt.Errorf("unknown event type %q", eventType)
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO events (barcode, type, at, delta_seconds) VALUES (?, ?, ?, ?)`,
		barcode, eventType, ts(at), delta); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetBatch(ctx, barcode)
}

const eventCols = `id, barcode, type, at, delta_seconds, revoked_at, revoke_reason`

// RevokeEvent undoes the latest non-revoked event of a batch inside a single
// transaction. The event row is never deleted: it is stamped with the
// revocation time and a non-empty reason for the audit trail. Revoking a
// takeout moves the batch back into the cabinet; revoking a return subtracts
// that exposure, restores the out-of-cabinet state (with the takeout open
// again) and re-evaluates usability. The batch aggregate is rewound to the
// previous active event (or to creation time when there is none).
//
// It fails with a ConflictError when:
//   - the event id is unknown for the batch (ErrNotFound otherwise),
//   - the event was already revoked (event_already_revoked),
//   - it is not the latest non-revoked event (event_not_latest),
//   - the revocation time is not strictly later than the batch's last
//     operation time (time_not_monotonic),
//   - the batch changed concurrently (invalid_transition).
//
// Every failure rolls the whole transaction back: no revocation row change,
// no aggregate or in/out change.
func (s *Store) RevokeEvent(ctx context.Context, barcode string, eventID int64, at time.Time, reason string) (*Batch, *Event, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()

	b, err := scanBatch(tx.QueryRowContext(ctx, batchSelect, barcode))
	if err != nil {
		return nil, nil, err
	}

	ev, err := scanEventRow(tx.QueryRowContext(ctx,
		`SELECT `+eventCols+` FROM events WHERE id = ? AND barcode = ?`, eventID, barcode))
	if err != nil {
		return nil, nil, err
	}
	if ev == nil {
		return nil, nil, ErrNotFound
	}
	if ev.RevokedAt != nil {
		return nil, nil, conflict("event_already_revoked",
			"event %d was already revoked at %s", ev.ID, ts(*ev.RevokedAt))
	}

	latest, err := scanEventRow(tx.QueryRowContext(ctx,
		`SELECT `+eventCols+` FROM events WHERE barcode = ? AND revoked_at IS NULL ORDER BY id DESC LIMIT 1`,
		barcode))
	if err != nil {
		return nil, nil, err
	}
	if latest == nil {
		return nil, nil, conflict("event_not_revocable", "batch has no active event to revoke")
	}
	if latest.ID != ev.ID {
		return nil, nil, conflict("event_not_latest",
			"only the latest non-revoked event (id %d) can be revoked", latest.ID)
	}

	if !at.After(b.LastAt) {
		return nil, nil, conflict("time_not_monotonic",
			"revocation time %s must be strictly later than the batch's previous time %s",
			ts(at), ts(b.LastAt))
	}

	// Active event immediately before the one being revoked; the aggregate is
	// rewound to it (or to the batch creation when nothing came before).
	prev, err := scanEventRow(tx.QueryRowContext(ctx,
		`SELECT `+eventCols+` FROM events WHERE barcode = ? AND revoked_at IS NULL ORDER BY id DESC LIMIT 1 OFFSET 1`,
		barcode))
	if err != nil {
		return nil, nil, err
	}

	// Stamp the audit fields. The revoked_at IS NULL predicate guarantees a
	// duplicate/concurrent revocation of the same event can commit at most once.
	res, err := tx.ExecContext(ctx,
		`UPDATE events SET revoked_at = ?, revoke_reason = ?
		 WHERE id = ? AND barcode = ? AND revoked_at IS NULL`,
		ts(at), reason, ev.ID, barcode)
	if err != nil {
		return nil, nil, err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return nil, nil, conflict("event_already_revoked", "event %d was revoked concurrently", ev.ID)
	}

	var (
		newState    string
		pinnedState string
		newAccum    = b.AccumulatedSeconds
		newLastAt   = b.CreatedAt
		prevType    sql.NullString
		prevAt      sql.NullString
		prevTakeout sql.NullString
	)
	switch ev.Type {
	case EventTakeout:
		// Undo the takeout: the batch is back in the cabinet; exposure
		// accumulated by earlier cycles is untouched.
		newState, pinnedState = StateIn, StateOut
	case EventReturn:
		// Undo the return: give back that exposure and reopen the takeout.
		if b.State != StateIn || ev.DeltaSeconds == nil {
			return nil, nil, conflict("invalid_transition", "event %d is not reversible in the current state", ev.ID)
		}
		newState, pinnedState = StateOut, StateIn
		newAccum = b.AccumulatedSeconds - *ev.DeltaSeconds
		if newAccum < 0 {
			return nil, nil, fmt.Errorf("invariant violated: accumulated exposure would become %d", newAccum)
		}
	default:
		return nil, nil, fmt.Errorf("unknown event type %q", ev.Type)
	}
	newStatus := StatusUsable
	if newAccum > b.AllowedSeconds {
		newStatus = StatusScrapped
	}
	if prev != nil {
		newLastAt = prev.At
		prevType = sql.NullString{String: prev.Type, Valid: true}
		prevAt = sql.NullString{String: ts(prev.At), Valid: true}
		if prev.Type == EventTakeout {
			prevTakeout = sql.NullString{String: ts(prev.At), Valid: true}
		}
	}

	// The conditional UPDATE pins the state and last_at read above so a
	// concurrent change makes the transaction affect zero rows.
	res, err = tx.ExecContext(ctx, `UPDATE batches
		SET state = ?, status = ?, accumulated_seconds = ?, last_at = ?,
		    last_event_type = ?, last_event_at = ?, last_takeout_at = ?
		WHERE barcode = ? AND state = ? AND last_at = ?`,
		newState, newStatus, newAccum, ts(newLastAt),
		prevType, prevAt, prevTakeout,
		barcode, pinnedState, ts(b.LastAt))
	if err != nil {
		return nil, nil, err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return nil, nil, conflict("invalid_transition", "batch state changed concurrently; retry")
	}
	// Undoing the latest event rewinds the aggregate: a takeout undo lands
	// back in the cabinet and a return undo reopens the out-of-cabinet state.
	// In neither case is a previously recorded slot still authoritative, so the
	// independent occupancy row is dropped in the same transaction.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM location_occupancy WHERE barcode = ?`, barcode); err != nil {
		return nil, nil, err
	}

	revoked, err := scanEventRow(tx.QueryRowContext(ctx,
		`SELECT `+eventCols+` FROM events WHERE id = ?`, ev.ID))
	if err != nil {
		return nil, nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	out, err := s.GetBatch(ctx, barcode)
	if err != nil {
		return nil, nil, err
	}
	return out, revoked, nil
}

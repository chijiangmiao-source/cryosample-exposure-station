package store_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"timingstation/api/internal/store"

	_ "modernc.org/sqlite"
)

// legacySchema is the database shape as it existed before revocation support
// (events has no revoked_at / revoke_reason columns).
const legacySchema = `
CREATE TABLE batches (
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
);
CREATE TABLE events (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	barcode       TEXT NOT NULL REFERENCES batches (barcode),
	type          TEXT NOT NULL CHECK (type IN ('takeout', 'return')),
	at            TEXT NOT NULL,
	delta_seconds INTEGER
);
CREATE INDEX idx_events_barcode ON events (barcode, id);
`

// TestMigrateLegacyDatabaseThenRevoke creates a database with the pre-revoke
// schema, fills it with a scrapped batch, reopens it through the migrating
// store and revokes the mistaken return. The additive migration must add the
// audit columns and leave historical rows treated as non-revoked.
func TestMigrateLegacyDatabaseThenRevoke(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "legacy.db")

	// 1. Build the legacy database with the plain driver.
	db, err := sql.Open("sqlite", "file:"+dbPath)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, legacySchema)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO batches
		(barcode, allowed_seconds, state, status, accumulated_seconds, created_at, last_at,
		 last_event_type, last_event_at, last_takeout_at)
		VALUES ('M-1', 10, 'in', 'scrapped', 11,
			'2026-09-13T08:00:00Z', '2026-09-13T08:00:16Z',
			'return', '2026-09-13T08:00:16Z', NULL)`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx,
		`INSERT INTO events (barcode, type, at, delta_seconds) VALUES ('M-1', 'takeout', '2026-09-13T08:00:05Z', NULL)`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx,
		`INSERT INTO events (barcode, type, at, delta_seconds) VALUES ('M-1', 'return', '2026-09-13T08:00:16Z', 11)`)
	require.NoError(t, err)
	// Confirm the legacy table really lacks the new columns.
	var colCount int
	err = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pragma_table_info('events') WHERE name IN ('revoked_at', 'revoke_reason')`).
		Scan(&colCount)
	require.NoError(t, err)
	require.Equal(t, 0, colCount)
	require.NoError(t, db.Close())

	// 2. Reopen with the migrating store.
	st, err := store.Open(ctx, dbPath)
	require.NoError(t, err)
	defer st.Close()

	// 3. Revoke the mistaken return (event id 2) as on the new schema.
	revokeAt := mustTime(t, "2026-09-13T08:00:30Z")
	b, ev, err := st.RevokeEvent(ctx, "M-1", 2, revokeAt, "误扫归还，样本仍在柜外")
	require.NoError(t, err)
	assert.Equal(t, "out", b.State)
	assert.Equal(t, "usable", b.Status)
	assert.Equal(t, int64(0), b.AccumulatedSeconds)
	require.NotNil(t, b.LastTakeoutAt)
	require.NotNil(t, ev)
	require.NotNil(t, ev.RevokedAt)
	assert.Equal(t, "2026-09-13T08:00:30Z", ev.RevokedAt.UTC().Format("2006-01-02T15:04:05Z"))
	require.NotNil(t, ev.RevokeReason)
	assert.Equal(t, "误扫归还，样本仍在柜外", *ev.RevokeReason)

	// 4. The takeout is the active event again, and a correct return works.
	events, err := st.ListEvents(ctx, "M-1")
	require.NoError(t, err)
	require.Len(t, events, 2)
	assert.Nil(t, events[0].RevokedAt)
	assert.NotNil(t, events[1].RevokedAt)
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse(time.RFC3339, s)
	require.NoError(t, err)
	return tm
}

// TestEvaluateAtProjectsOpenTakeoutWithoutWriting checks the read-only risk
// projection directly at the store layer: while out of the cabinet the
// projected total grows with asOf, crossing the limit flips the conclusion
// without scrapping the batch, and in-cabinet batches get settled values.
func TestEvaluateAtProjectsOpenTakeoutWithoutWriting(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "eval.db"))
	require.NoError(t, err)
	defer st.Close()

	created := mustTime(t, "2026-09-13T08:00:00Z")
	_, err = st.CreateBatch(ctx, "E-1", 10, created)
	require.NoError(t, err)

	// No events yet: asOf may equal creation time, projection is settled at 0.
	b, p, err := st.EvaluateAt(ctx, "E-1", mustTime(t, "2026-09-13T08:00:00Z"))
	require.NoError(t, err)
	assert.Equal(t, int64(0), p.AccumulatedSeconds)
	assert.Equal(t, int64(10), p.RemainingSeconds)
	assert.True(t, p.Usable)
	assert.True(t, p.Settled)
	assert.Equal(t, "in", b.State)

	// Earlier than creation -> conflict.
	_, _, err = st.EvaluateAt(ctx, "E-1", mustTime(t, "2026-09-13T07:59:59Z"))
	require.Error(t, err)
	var ce *store.ConflictError
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, "time_not_monotonic", ce.Code)

	// Out of the cabinet since 08:00:05 with 10s allowed.
	_, err = st.ApplyEvent(ctx, "E-1", store.EventTakeout, mustTime(t, "2026-09-13T08:00:05Z"))
	require.NoError(t, err)

	for _, tc := range []struct {
		at     string
		accum  int64
		remain int64
		usable bool
	}{
		{"2026-09-13T08:00:05Z", 0, 10, true}, // equal to the open takeout
		{"2026-09-13T08:00:06Z", 1, 9, true},
		{"2026-09-13T08:00:15Z", 10, 0, true},   // == limit still usable
		{"2026-09-13T08:00:16Z", 11, -1, false}, // projected over limit
	} {
		b, p, err := st.EvaluateAt(ctx, "E-1", mustTime(t, tc.at))
		require.NoError(t, err, "at=%s", tc.at)
		assert.Equal(t, tc.accum, p.AccumulatedSeconds, "at=%s", tc.at)
		assert.Equal(t, tc.remain, p.RemainingSeconds, "at=%s", tc.at)
		assert.Equal(t, tc.usable, p.Usable, "at=%s", tc.at)
		assert.False(t, p.Settled, "open takeout is never settled, at=%s", tc.at)
		// The persisted aggregate never moves.
		assert.Equal(t, int64(0), b.AccumulatedSeconds, "at=%s", tc.at)
		assert.Equal(t, store.StatusUsable, b.Status, "at=%s", tc.at)
		assert.Equal(t, store.StateOut, b.State, "at=%s", tc.at)
	}

	// Earlier than the last event -> conflict, still no writes.
	_, _, err = st.EvaluateAt(ctx, "E-1", mustTime(t, "2026-09-13T08:00:04Z"))
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, "time_not_monotonic", ce.Code)

	events, err := st.ListEvents(ctx, "E-1")
	require.NoError(t, err)
	require.Len(t, events, 1, "evaluations never write events")

	// A settled cycle (+10, exactly at the limit) then a reopened takeout: the
	// projection extends the settled total.
	_, err = st.ApplyEvent(ctx, "E-1", store.EventReturn, mustTime(t, "2026-09-13T08:00:15Z"))
	require.NoError(t, err)
	_, err = st.ApplyEvent(ctx, "E-1", store.EventTakeout, mustTime(t, "2026-09-13T08:00:20Z"))
	require.NoError(t, err)
	b, p, err = st.EvaluateAt(ctx, "E-1", mustTime(t, "2026-09-13T08:00:21Z"))
	require.NoError(t, err)
	assert.Equal(t, int64(11), p.AccumulatedSeconds, "10 settled + 1 open")
	assert.False(t, p.Usable)
	assert.False(t, p.Settled)

	// In-cabinet evaluation returns settled figures regardless of how far
	// ahead asOf is.
	_, err = st.ApplyEvent(ctx, "E-1", store.EventReturn, mustTime(t, "2026-09-13T08:00:21Z"))
	require.NoError(t, err)
	b, p, err = st.EvaluateAt(ctx, "E-1", mustTime(t, "2026-09-14T00:00:00Z"))
	require.NoError(t, err)
	assert.True(t, p.Settled)
	assert.Equal(t, int64(11), p.AccumulatedSeconds)
	assert.Equal(t, store.StatusScrapped, b.Status)
	assert.False(t, p.Usable)

	_, _, err = st.EvaluateAt(ctx, "GHOST", created)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

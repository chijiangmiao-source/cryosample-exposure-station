package store_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"timingstation/api/internal/store"

	_ "modernc.org/sqlite"
)

func openLocationStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "location.db")
	st, err := store.Open(context.Background(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() })
	return st, dbPath
}

func requireConflict(t *testing.T, err error, code string) {
	t.Helper()
	require.Error(t, err)
	var ce *store.ConflictError
	require.ErrorAs(t, err, &ce, "expected ConflictError, got %v", err)
	assert.Equal(t, code, ce.Code)
}

// rawDB opens a read-only second connection to the same SQLite file for
// ledger-level assertions. All store writes are committed by the time these
// run, and the store's single connection is not used concurrently here.
func rawDB(t *testing.T, dbPath string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro")
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	return db
}

// TestPutLocationFirstPlacementPersistsAndSurvivesReload covers the headline
// "placed, then visible after refresh" behaviour.
func TestPutLocationFirstPlacementPersistsAndSurvivesReload(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "loc.db")
	st, err := store.Open(ctx, dbPath)
	require.NoError(t, err)
	_, err = st.CreateBatch(ctx, "L-1", 100, mustTime(t, "2026-09-13T08:00:00Z"))
	require.NoError(t, err)

	// A freshly created batch is unplaced.
	b, err := st.GetBatch(ctx, "L-1")
	require.NoError(t, err)
	assert.Nil(t, b.Location)

	b, err = st.PutLocation(ctx, "L-1", "A-01")
	require.NoError(t, err)
	require.NotNil(t, b.Location)
	assert.Equal(t, "A-01", *b.Location)

	require.NoError(t, st.Close())

	// Refresh: reopen the database, the occupancy is restored from the ledger.
	st2, err := store.Open(ctx, dbPath)
	require.NoError(t, err)
	defer st2.Close()
	b, err = st2.GetBatch(ctx, "L-1")
	require.NoError(t, err)
	require.NotNil(t, b.Location)
	assert.Equal(t, "A-01", *b.Location)
}

// TestPutLocationMoveReleasesTheOldSlot verifies a move vacates the previous
// slot inside the same transaction, so another batch can take it.
func TestPutLocationMoveReleasesTheOldSlot(t *testing.T) {
	ctx := context.Background()
	st, _ := openLocationStore(t)
	_, err := st.CreateBatch(ctx, "M-1", 100, mustTime(t, "2026-09-13T08:00:00Z"))
	require.NoError(t, err)
	_, err = st.CreateBatch(ctx, "M-2", 100, mustTime(t, "2026-09-13T08:00:00Z"))
	require.NoError(t, err)

	_, err = st.PutLocation(ctx, "M-1", "A-01")
	require.NoError(t, err)

	// Move M-1 to A-02.
	b, err := st.PutLocation(ctx, "M-1", "A-02")
	require.NoError(t, err)
	require.NotNil(t, b.Location)
	assert.Equal(t, "A-02", *b.Location)

	// The old slot is free: M-2 can now occupy A-01.
	b, err = st.PutLocation(ctx, "M-2", "A-01")
	require.NoError(t, err)
	require.NotNil(t, b.Location)
	assert.Equal(t, "A-01", *b.Location)

	// Every batch and every slot has at most one active occupancy.
	b1, err := st.GetBatch(ctx, "M-1")
	require.NoError(t, err)
	b2, err := st.GetBatch(ctx, "M-2")
	require.NoError(t, err)
	assert.Equal(t, "A-02", *b1.Location)
	assert.Equal(t, "A-01", *b2.Location)
}

// TestPutLocationIdempotentSameSlot treats a repeat PUT into the held slot as
// success without disturbing anything.
func TestPutLocationIdempotentSameSlot(t *testing.T) {
	ctx := context.Background()
	st, _ := openLocationStore(t)
	_, err := st.CreateBatch(ctx, "I-1", 100, mustTime(t, "2026-09-13T08:00:00Z"))
	require.NoError(t, err)
	_, err = st.PutLocation(ctx, "I-1", "A-01")
	require.NoError(t, err)
	b, err := st.PutLocation(ctx, "I-1", "A-01")
	require.NoError(t, err)
	assert.Equal(t, "A-01", *b.Location)
}

// TestPutLocationConflicts covers out-of-cabinet, scrapped and occupied
// targets; none of the failures may change the previous occupancy.
func TestPutLocationConflicts(t *testing.T) {
	ctx := context.Background()
	st, _ := openLocationStore(t)
	_, err := st.CreateBatch(ctx, "X-1", 10, mustTime(t, "2026-09-13T08:00:00Z"))
	require.NoError(t, err)
	_, err = st.CreateBatch(ctx, "X-2", 100, mustTime(t, "2026-09-13T08:00:00Z"))
	require.NoError(t, err)

	// Out-of-cabinet batch cannot be placed.
	_, err = st.ApplyEvent(ctx, "X-1", store.EventTakeout, mustTime(t, "2026-09-13T08:00:05Z"))
	require.NoError(t, err)
	_, err = st.PutLocation(ctx, "X-1", "A-01")
	requireConflict(t, err, "batch_not_in_cabinet")
	b, err := st.GetBatch(ctx, "X-1")
	require.NoError(t, err)
	assert.Nil(t, b.Location, "rejected placement leaves no occupancy")

	// Another batch grabs A-01.
	_, err = st.PutLocation(ctx, "X-2", "A-01")
	require.NoError(t, err)

	// Return X-1 (usable, +5s within the 10s allowance) then fight for A-01:
	// the slot is occupied by X-2, so the placement fails and X-1 stays unplaced.
	_, err = st.ApplyEvent(ctx, "X-1", store.EventReturn, mustTime(t, "2026-09-13T08:00:10Z"))
	require.NoError(t, err)
	_, err = st.PutLocation(ctx, "X-1", "A-01")
	requireConflict(t, err, "location_occupied")
	b, err = st.GetBatch(ctx, "X-1")
	require.NoError(t, err)
	assert.Nil(t, b.Location)
	b, err = st.GetBatch(ctx, "X-2")
	require.NoError(t, err)
	assert.Equal(t, "A-01", *b.Location, "the holder keeps the slot")

	// A failed move into an occupied slot must not release the old slot:
	// place X-1 in B-09, then fail to move it onto A-01.
	_, err = st.PutLocation(ctx, "X-1", "B-09")
	require.NoError(t, err)
	_, err = st.PutLocation(ctx, "X-1", "A-01")
	requireConflict(t, err, "location_occupied")
	b, err = st.GetBatch(ctx, "X-1")
	require.NoError(t, err)
	assert.Equal(t, "B-09", *b.Location, "old occupancy survives the rejected move")

	// A scrapped in-cabinet batch cannot occupy a slot.
	_, err = st.CreateBatch(ctx, "X-3", 10, mustTime(t, "2026-09-13T09:00:00Z"))
	require.NoError(t, err)
	_, err = st.ApplyEvent(ctx, "X-3", store.EventTakeout, mustTime(t, "2026-09-13T09:00:05Z"))
	require.NoError(t, err)
	_, err = st.ApplyEvent(ctx, "X-3", store.EventReturn, mustTime(t, "2026-09-13T09:00:16Z")) // +11 > 10
	require.NoError(t, err)
	_, err = st.PutLocation(ctx, "X-3", "C-01")
	requireConflict(t, err, "batch_scrapped")

	// Unknown batch is not found rather than a conflict.
	_, err = st.PutLocation(ctx, "GHOST", "A-01")
	assert.ErrorIs(t, err, store.ErrNotFound)

	// Rejections wrote no events and moved no accumulated totals.
	evs, err := st.ListEvents(ctx, "X-1")
	require.NoError(t, err)
	assert.Len(t, evs, 2)
	b, err = st.GetBatch(ctx, "X-1")
	require.NoError(t, err)
	assert.Equal(t, int64(5), b.AccumulatedSeconds)
}

// TestTakeoutReleasesOccupancyInTheSameTransaction is the core automation
// requirement: after 取出 the slot is empty and reusable; after 归还 the batch
// comes back unplaced and must be shelved again.
func TestTakeoutReleasesOccupancyInTheSameTransaction(t *testing.T) {
	ctx := context.Background()
	st, dbPath := openLocationStore(t)
	_, err := st.CreateBatch(ctx, "T-1", 100, mustTime(t, "2026-09-13T08:00:00Z"))
	require.NoError(t, err)
	_, err = st.CreateBatch(ctx, "T-2", 100, mustTime(t, "2026-09-13T08:00:00Z"))
	require.NoError(t, err)
	_, err = st.PutLocation(ctx, "T-1", "A-01")
	require.NoError(t, err)

	_, err = st.ApplyEvent(ctx, "T-1", store.EventTakeout, mustTime(t, "2026-09-13T08:00:05Z"))
	require.NoError(t, err)
	b, err := st.GetBatch(ctx, "T-1")
	require.NoError(t, err)
	assert.Nil(t, b.Location, "takeout vacates the slot automatically")

	// The slot is immediately reusable by another in-cabinet batch.
	_, err = st.PutLocation(ctx, "T-2", "A-01")
	require.NoError(t, err)

	// T-1 returns: still unplaced ("归还后保持未定位"), then can be shelved
	// again (into a different slot since A-01 is held by T-2).
	_, err = st.ApplyEvent(ctx, "T-1", store.EventReturn, mustTime(t, "2026-09-13T08:00:15Z"))
	require.NoError(t, err)
	b, err = st.GetBatch(ctx, "T-1")
	require.NoError(t, err)
	assert.Nil(t, b.Location, "a return leaves the batch unplaced")
	b, err = st.PutLocation(ctx, "T-1", "A-02")
	require.NoError(t, err)
	assert.Equal(t, "A-02", *b.Location)

	// The ledger itself holds exactly the two active occupancies, one each.
	db := rawDB(t, dbPath)
	rows, err := db.QueryContext(ctx, `SELECT barcode, location FROM location_occupancy ORDER BY barcode`)
	require.NoError(t, err)
	defer rows.Close()
	type occ struct{ barcode, location string }
	var got []occ
	for rows.Next() {
		var o occ
		require.NoError(t, rows.Scan(&o.barcode, &o.location))
		got = append(got, o)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, []occ{{"T-1", "A-02"}, {"T-2", "A-01"}}, got)
}

// TestReleaseLocationManualVacate exercises the independent 腾空 operation,
// including its conflict when nothing is held.
func TestReleaseLocationManualVacate(t *testing.T) {
	ctx := context.Background()
	st, _ := openLocationStore(t)
	_, err := st.CreateBatch(ctx, "V-1", 100, mustTime(t, "2026-09-13T08:00:00Z"))
	require.NoError(t, err)

	_, err = st.ReleaseLocation(ctx, "V-1")
	requireConflict(t, err, "location_not_occupied")

	_, err = st.PutLocation(ctx, "V-1", "A-01")
	require.NoError(t, err)
	b, err := st.ReleaseLocation(ctx, "V-1")
	require.NoError(t, err)
	assert.Nil(t, b.Location)

	// Events and exposure are untouched by the occupancy-only operation.
	evs, err := st.ListEvents(ctx, "V-1")
	require.NoError(t, err)
	assert.Empty(t, evs)
	assert.Equal(t, int64(0), b.AccumulatedSeconds)

	_, err = st.ReleaseLocation(ctx, "GHOST")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// TestConcurrentPutLocationSameSlotOnlyOneWins: several batches racing for
// one slot — exactly one placement commits.
func TestConcurrentPutLocationSameSlotOnlyOneWins(t *testing.T) {
	ctx := context.Background()
	st, dbPath := openLocationStore(t)
	const n = 8
	for i := 1; i <= n; i++ {
		bc := "R-" + strconv.Itoa(i)
		_, err := st.CreateBatch(ctx, bc, 100, mustTime(t, "2026-09-13T08:00:00Z"))
		require.NoError(t, err)
	}
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			bc := "R-" + strconv.Itoa(i+1)
			_, errs[i] = st.PutLocation(ctx, bc, "Z-99")
		}(i)
	}
	wg.Wait()

	wins := 0
	for _, err := range errs {
		if err == nil {
			wins++
			continue
		}
		var ce *store.ConflictError
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, "location_occupied", ce.Code)
	}
	assert.Equal(t, 1, wins, "the same slot may be won by at most one batch")

	// Exactly one active occupancy row points at Z-99, and losers keep no
	// half-written occupancies.
	db := rawDB(t, dbPath)
	var holders int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM location_occupancy WHERE location = 'Z-99'`).Scan(&holders))
	assert.Equal(t, 1, holders)
	var total int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM location_occupancy`).Scan(&total))
	assert.Equal(t, 1, total, "losers keep no half-written occupancy")
}

// TestOccupancyLedgerConstraintsDirectly is belt-and-braces: the schema
// enforces one-row-per-batch and one-batch-per-location even against a buggy
// caller.
func TestOccupancyLedgerConstraintsDirectly(t *testing.T) {
	ctx := context.Background()
	st, dbPath := openLocationStore(t)
	_, err := st.CreateBatch(ctx, "D-1", 100, mustTime(t, "2026-09-13T08:00:00Z"))
	require.NoError(t, err)
	_, err = st.CreateBatch(ctx, "D-2", 100, mustTime(t, "2026-09-13T08:00:00Z"))
	require.NoError(t, err)

	db := rawDB(t, dbPath)
	// Read-only connection cannot insert; use the main DB handle through the
	// store-created file via a second read-write connection instead.
	require.NoError(t, db.Close())
	rw, err := sql.Open("sqlite", "file:"+dbPath)
	require.NoError(t, err)
	defer rw.Close()
	_, err = rw.ExecContext(ctx, `INSERT INTO location_occupancy (barcode, location) VALUES ('D-1', 'A-01')`)
	require.NoError(t, err)
	_, err = rw.ExecContext(ctx, `INSERT INTO location_occupancy (barcode, location) VALUES ('D-1', 'A-02')`)
	require.Error(t, err, "one batch cannot hold two slots")
	_, err = rw.ExecContext(ctx, `INSERT INTO location_occupancy (barcode, location) VALUES ('D-2', 'A-01')`)
	require.Error(t, err, "one slot cannot hold two batches")
}

// TestLegacyDatabaseMigratesWithEmptyOccupancy verifies the additive
// migration on the pre-occupancy schema: existing batches reopen as unplaced
// and can then be shelved normally.
func TestLegacyDatabaseMigratesWithEmptyOccupancy(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "legacy-loc.db")
	db, err := sql.Open("sqlite", "file:"+dbPath)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, legacySchema)
	require.NoError(t, err)
	// Existing batch created before location support existed.
	_, err = db.ExecContext(ctx, `INSERT INTO batches
		(barcode, allowed_seconds, state, status, accumulated_seconds, created_at, last_at)
		VALUES ('M-1', 100, 'in', 'usable', 0,
			'2026-09-13T08:00:00Z', '2026-09-13T08:00:00Z')`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	st, err := store.Open(ctx, dbPath)
	require.NoError(t, err)
	defer st.Close()
	b, err := st.GetBatch(ctx, "M-1")
	require.NoError(t, err)
	assert.Nil(t, b.Location, "legacy batches start unplaced")

	b, err = st.PutLocation(ctx, "M-1", "A-01")
	require.NoError(t, err)
	assert.Equal(t, "A-01", *b.Location)
}

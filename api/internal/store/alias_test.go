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

func openAliasStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "alias.db"))
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() })
	return st
}

// TestBindAliasResolvesToSameBatch is the headline: after binding a backup
// barcode, scanning either code reaches the same canonical batch, and its
// timing/location records are shared.
func TestBindAliasResolvesToSameBatch(t *testing.T) {
	ctx := context.Background()
	st := openAliasStore(t)
	_, err := st.CreateBatch(ctx, "B-1", 100, mustTime(t, "2026-09-13T08:00:00Z"))
	require.NoError(t, err)
	_, err = st.PutLocation(ctx, "B-1", "A-01")
	require.NoError(t, err)

	b, aliases, err := st.BindAlias(ctx, "B-1", "BACKUP-1")
	require.NoError(t, err)
	assert.Equal(t, "B-1", b.Barcode, "the canonical barcode never changes")
	assert.Equal(t, []string{"BACKUP-1"}, aliases)

	canonical, err := st.ResolveCode(ctx, "BACKUP-1")
	require.NoError(t, err)
	assert.Equal(t, "B-1", canonical)
	canonical, err = st.ResolveCode(ctx, "B-1")
	require.NoError(t, err)
	assert.Equal(t, "B-1", canonical)

	// Scanning the alias reaches the same aggregate, incl. its slot.
	b, err = st.GetBatch(ctx, canonical)
	require.NoError(t, err)
	require.NotNil(t, b.Location)
	assert.Equal(t, "A-01", *b.Location)

	// Events submitted via the alias key share the same log and totals.
	_, err = st.ApplyEvent(ctx, canonical, store.EventTakeout, mustTime(t, "2026-09-13T08:00:05Z"))
	require.NoError(t, err)
	evs, err := st.ListEvents(ctx, "B-1")
	require.NoError(t, err)
	require.Len(t, evs, 1)
	assert.Equal(t, "takeout", evs[0].Type)

	// The takeout auto-vacated the slot through the shared aggregate.
	b, err = st.GetBatch(ctx, "B-1")
	require.NoError(t, err)
	assert.Nil(t, b.Location)
}

// TestBindAliasPersistsAcrossReopen verifies the relation is durable.
func TestBindAliasPersistsAcrossReopen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "alias-persist.db")
	st, err := store.Open(ctx, dbPath)
	require.NoError(t, err)
	_, err = st.CreateBatch(ctx, "B-1", 100, mustTime(t, "2026-09-13T08:00:00Z"))
	require.NoError(t, err)
	_, _, err = st.BindAlias(ctx, "B-1", "BACKUP-1")
	require.NoError(t, err)
	require.NoError(t, st.Close())

	st2, err := store.Open(ctx, dbPath)
	require.NoError(t, err)
	defer st2.Close()
	canonical, err := st2.ResolveCode(ctx, "BACKUP-1")
	require.NoError(t, err)
	assert.Equal(t, "B-1", canonical)
	aliases, err := st2.ListAliases(ctx, "B-1")
	require.NoError(t, err)
	assert.Equal(t, []string{"BACKUP-1"}, aliases)
}

// TestBindAliasConflicts covers every namespace collision; none may change the
// batch or write a relation.
func TestBindAliasConflicts(t *testing.T) {
	ctx := context.Background()
	st := openAliasStore(t)
	_, err := st.CreateBatch(ctx, "B-1", 100, mustTime(t, "2026-09-13T08:00:00Z"))
	require.NoError(t, err)
	_, err = st.CreateBatch(ctx, "B-2", 100, mustTime(t, "2026-09-13T08:00:00Z"))
	require.NoError(t, err)

	// Binding on an unknown batch is not found, not a conflict.
	_, _, err = st.BindAlias(ctx, "GHOST", "X")
	assert.ErrorIs(t, err, store.ErrNotFound)

	// An alias cannot equal any primary barcode (incl. the batch's own).
	_, _, err = st.BindAlias(ctx, "B-1", "B-1")
	requireConflict(t, err, "alias_conflicts_barcode")
	_, _, err = st.BindAlias(ctx, "B-1", "B-2")
	requireConflict(t, err, "alias_conflicts_barcode")

	// First bind succeeds.
	_, _, err = st.BindAlias(ctx, "B-1", "BACKUP-1")
	require.NoError(t, err)

	// Rebinding the same alias to the same batch is a duplicate.
	_, _, err = st.BindAlias(ctx, "B-1", "BACKUP-1")
	requireConflict(t, err, "duplicate_alias")

	// Binding it to another batch is an elsewhere conflict.
	_, _, err = st.BindAlias(ctx, "B-2", "BACKUP-1")
	requireConflict(t, err, "alias_bound_elsewhere")

	// Creating a batch reuses the namespace: it cannot take an alias' code.
	_, err = st.CreateBatch(ctx, "BACKUP-1", 10, mustTime(t, "2026-09-13T09:00:00Z"))
	assert.ErrorIs(t, err, store.ErrDuplicate)

	// Nothing extra was written.
	aliases, err := st.ListAliases(ctx, "B-1")
	require.NoError(t, err)
	assert.Equal(t, []string{"BACKUP-1"}, aliases)
	aliases, err = st.ListAliases(ctx, "B-2")
	require.NoError(t, err)
	assert.Empty(t, aliases)
}

// TestCreateBatchConflictIsBidirectionalWithAliases checks the other
// direction: a primary barcode created first blocks an alias of the same code.
func TestCreateBatchConflictIsBidirectionalWithAliases(t *testing.T) {
	ctx := context.Background()
	st := openAliasStore(t)
	_, err := st.CreateBatch(ctx, "PRIMARY-X", 100, mustTime(t, "2026-09-13T08:00:00Z"))
	require.NoError(t, err)
	_, err = st.CreateBatch(ctx, "B-9", 100, mustTime(t, "2026-09-13T08:00:00Z"))
	require.NoError(t, err)
	_, _, err = st.BindAlias(ctx, "B-9", "PRIMARY-X")
	requireConflict(t, err, "alias_conflicts_barcode")
}

// TestUnbindAliasRules covers removal, foreign ownership and the guarantee
// that events/exposure/occupancy are untouched.
func TestUnbindAliasRules(t *testing.T) {
	ctx := context.Background()
	st := openAliasStore(t)
	_, err := st.CreateBatch(ctx, "B-1", 100, mustTime(t, "2026-09-13T08:00:00Z"))
	require.NoError(t, err)
	_, err = st.PutLocation(ctx, "B-1", "A-01")
	require.NoError(t, err)
	_, err = st.ApplyEvent(ctx, "B-1", store.EventTakeout, mustTime(t, "2026-09-13T08:00:05Z"))
	require.NoError(t, err)
	_, err = st.ApplyEvent(ctx, "B-1", store.EventReturn, mustTime(t, "2026-09-13T08:00:15Z"))
	require.NoError(t, err) // +10, back in cabinet
	_, err = st.PutLocation(ctx, "B-1", "A-02")
	require.NoError(t, err)
	_, _, err = st.BindAlias(ctx, "B-1", "BACKUP-1")
	require.NoError(t, err)

	// Unknown batch.
	_, _, err = st.UnbindAlias(ctx, "GHOST", "BACKUP-1")
	assert.ErrorIs(t, err, store.ErrNotFound)
	// Code that never existed.
	_, _, err = st.UnbindAlias(ctx, "B-1", "NOPE")
	requireConflict(t, err, "alias_not_bound")
	// The primary barcode is not an alias and cannot be unbound.
	_, _, err = st.UnbindAlias(ctx, "B-1", "B-1")
	requireConflict(t, err, "alias_not_bound")

	// A foreign batch cannot remove another batch's alias.
	_, err = st.CreateBatch(ctx, "B-2", 100, mustTime(t, "2026-09-13T08:00:00Z"))
	require.NoError(t, err)
	_, _, err = st.UnbindAlias(ctx, "B-2", "BACKUP-1")
	requireConflict(t, err, "alias_not_bound")

	// The rejected unbind changes nothing: alias still resolves.
	canonical, err := st.ResolveCode(ctx, "BACKUP-1")
	require.NoError(t, err)
	assert.Equal(t, "B-1", canonical)

	// The owner unbinds it: events, accumulated exposure and occupancy survive.
	b, aliases, err := st.UnbindAlias(ctx, "B-1", "BACKUP-1")
	require.NoError(t, err)
	assert.Empty(t, aliases)
	assert.Equal(t, int64(10), b.AccumulatedSeconds)
	require.NotNil(t, b.Location)
	assert.Equal(t, "A-02", *b.Location)
	evs, err := st.ListEvents(ctx, "B-1")
	require.NoError(t, err)
	assert.Len(t, evs, 2)

	// The code can no longer be queried.
	_, err = st.ResolveCode(ctx, "BACKUP-1")
	assert.ErrorIs(t, err, store.ErrNotFound)

	// Unbinding again is now alias_not_bound.
	_, _, err = st.UnbindAlias(ctx, "B-1", "BACKUP-1")
	requireConflict(t, err, "alias_not_bound")

	// The freed code can be rebound to another batch.
	_, _, err = st.BindAlias(ctx, "B-2", "BACKUP-1")
	require.NoError(t, err)
	canonical, err = st.ResolveCode(ctx, "BACKUP-1")
	require.NoError(t, err)
	assert.Equal(t, "B-2", canonical)
}

// TestConcurrentBindSameAliasOnlyOneWins: racing stations binding the same
// code (even to different batches) — exactly one commit.
func TestConcurrentBindSameAliasOnlyOneWins(t *testing.T) {
	ctx := context.Background()
	st := openAliasStore(t)
	const n = 8
	for i := 1; i <= n; i++ {
		_, err := st.CreateBatch(ctx, "CB-"+strconv.Itoa(i), 100, mustTime(t, "2026-09-13T08:00:00Z"))
		require.NoError(t, err)
	}
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, errs[i] = st.BindAlias(ctx, "CB-"+strconv.Itoa(i+1), "RACE-ALIAS")
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
		assert.Contains(t, []string{"duplicate_alias", "alias_bound_elsewhere"}, ce.Code)
	}
	assert.Equal(t, 1, wins, "the same alias may be bound at most once")
	canonical, err := st.ResolveCode(ctx, "RACE-ALIAS")
	require.NoError(t, err)
	aliases, err := st.ListAliases(ctx, canonical)
	require.NoError(t, err)
	assert.Equal(t, []string{"RACE-ALIAS"}, aliases)
}

// TestLegacyDatabaseMigratesAliasesBackfill ensures an existing database
// reopens with every primary barcode registered, so an alias cannot shadow an
// existing batch and primary lookups keep resolving.
func TestLegacyDatabaseMigratesAliasesBackfill(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "legacy-alias.db")
	mustMigrateLegacyDB(t, ctx, dbPath)

	st, err := store.Open(ctx, dbPath)
	require.NoError(t, err)
	defer st.Close()
	canonical, err := st.ResolveCode(ctx, "M-1")
	require.NoError(t, err)
	assert.Equal(t, "M-1", canonical)

	// The backfilled primary occupies the namespace: a new alias collides.
	_, err = st.CreateBatch(ctx, "OTHER", 100, mustTime(t, "2026-09-13T09:00:00Z"))
	require.NoError(t, err)
	_, _, err = st.BindAlias(ctx, "OTHER", "M-1")
	requireConflict(t, err, "alias_conflicts_barcode")
}

// mustMigrateLegacyDB builds a pre-alias database (legacySchema) holding one
// batch M-1, for the backfill-migration test.
func mustMigrateLegacyDB(t *testing.T, ctx context.Context, dbPath string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+dbPath)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, legacySchema)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO batches
		(barcode, allowed_seconds, state, status, accumulated_seconds, created_at, last_at)
		VALUES ('M-1', 100, 'in', 'usable', 0,
			'2026-09-13T08:00:00Z', '2026-09-13T08:00:00Z')`)
	require.NoError(t, err)
	require.NoError(t, db.Close())
}

package app_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// aliasResp extends the shared batch response with the alias fields.
type aliasResp struct {
	batchResp
}

func bindAlias(t *testing.T, srv *httptest.Server, barcode, alias string) (int, aliasResp, errResp) {
	t.Helper()
	code, raw := doJSON(t, http.MethodPost, srv.URL+"/api/batches/"+barcode+"/aliases",
		map[string]string{"alias": alias})
	var b aliasResp
	var e errResp
	if code == http.StatusOK {
		require.NoError(t, json.Unmarshal(raw, &b))
	} else {
		require.NoError(t, json.Unmarshal(raw, &e))
	}
	return code, b, e
}

func unbindAlias(t *testing.T, srv *httptest.Server, barcode, alias string) (int, aliasResp, errResp) {
	t.Helper()
	code, raw := doJSON(t, http.MethodDelete,
		fmt.Sprintf("%s/api/batches/%s/aliases/%s", srv.URL, barcode, alias), nil)
	var b aliasResp
	var e errResp
	if code == http.StatusOK {
		require.NoError(t, json.Unmarshal(raw, &b))
	} else {
		require.NoError(t, json.Unmarshal(raw, &e))
	}
	return code, b, e
}

func getAliasBatch(t *testing.T, srv *httptest.Server, code string) aliasResp {
	t.Helper()
	status, raw := doJSON(t, http.MethodGet, srv.URL+"/api/batches/"+code, nil)
	require.Equal(t, http.StatusOK, status, string(raw))
	var b aliasResp
	require.NoError(t, json.Unmarshal(raw, &b))
	return b
}

// mustBind binds an alias and requires HTTP 200, returning the batch.
func mustBind(t *testing.T, srv *httptest.Server, barcode, alias string) aliasResp {
	t.Helper()
	code, b, _ := bindAlias(t, srv, barcode, alias)
	require.Equal(t, http.StatusOK, code)
	return b
}

// statusOf runs a raw request and returns only the status code.
func statusOf(code int, _ []byte) int { return code }

func TestAliasBindThenScanAliasDrivesSameBatch(t *testing.T) {
	srv := newServer(t)
	createBatch(t, srv, "A-1", 100, "2026-09-13T08:00:00Z")

	code, b, _ := bindAlias(t, srv, "A-1", "A-1-BACKUP")
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "A-1", b.Barcode)
	assert.Equal(t, []string{"A-1-BACKUP"}, b.Aliases)
	assert.Empty(t, b.MatchedBarcode, "a primary-barcode bind carries no hit code")

	// Scanning the alias resolves to the canonical batch and reports the hit.
	got := getAliasBatch(t, srv, "A-1-BACKUP")
	assert.Equal(t, "A-1", got.Barcode, "the response always carries the primary barcode")
	assert.Equal(t, "A-1-BACKUP", got.MatchedBarcode, "optional hit code reports the scanned alias")
	assert.Equal(t, []string{"A-1-BACKUP"}, got.Aliases)

	// Scanning the primary barcode carries no matchedBarcode (old semantics).
	primary := getAliasBatch(t, srv, "A-1")
	assert.Equal(t, "A-1", primary.Barcode)
	assert.Empty(t, primary.MatchedBarcode)

	// Events via the alias land on the canonical aggregate.
	code2, eb, _ := postEvent(t, srv, "A-1-BACKUP", "takeout", "2026-09-13T08:00:05Z")
	require.Equal(t, http.StatusCreated, code2)
	assert.Equal(t, "A-1", eb.Barcode)
	assert.Equal(t, "out", eb.State)
	assert.Equal(t, "A-1-BACKUP", eb.MatchedBarcode)

	// Revocation via the alias rewinds the same aggregate.
	code2, rb, _ := revokeEvent(t, srv, "A-1-BACKUP", 1, "2026-09-13T08:00:20Z", "误扫取出")
	require.Equal(t, http.StatusOK, code2)
	assert.Equal(t, "in", rb.State)
	assert.Equal(t, "A-1", rb.Barcode)
	assert.Equal(t, "A-1-BACKUP", rb.MatchedBarcode)

	// The event log is the canonical one, reachable via either code.
	viaAlias := listEvents(t, srv, "A-1-BACKUP")
	viaPrimary := listEvents(t, srv, "A-1")
	require.Len(t, viaAlias.Events, 1)
	require.Len(t, viaPrimary.Events, 1)
	assert.Equal(t, "takeout", viaAlias.Events[0].Type)
}

func TestAliasTakeoutReturnRelocationThroughAlias(t *testing.T) {
	srv := newServer(t)
	createBatch(t, srv, "A-2", 100, "2026-09-13T08:00:00Z")
	mustBind(t, srv, "A-2", "A-2-BACKUP")

	// Place via primary, then relocate through the alias.
	code, raw := doPutLocation(t, srv.URL, "A-2", "SLOT-A")
	require.Equal(t, http.StatusOK, code)
	lb := decodeLocation(t, code, raw)
	require.NotNil(t, lb.Location)
	assert.Equal(t, "SLOT-A", *lb.Location)

	code, raw = doPutLocation(t, srv.URL, "A-2-BACKUP", "SLOT-B")
	require.Equal(t, http.StatusOK, code, string(raw))
	lb = decodeLocation(t, code, raw)
	require.NotNil(t, lb.Location)
	assert.Equal(t, "SLOT-B", *lb.Location)
	assert.Equal(t, "A-2", lb.Barcode)

	// Takeout through the alias auto-vacates the shared slot.
	code, eb, _ := postEvent(t, srv, "A-2-BACKUP", "takeout", "2026-09-13T08:00:05Z")
	require.Equal(t, http.StatusCreated, code)
	assert.Equal(t, "out", eb.State)
	assert.Equal(t, "A-2", eb.Barcode)
	assert.Equal(t, "A-2-BACKUP", eb.MatchedBarcode)
	_, outRaw := doJSON(t, http.MethodGet, srv.URL+"/api/batches/A-2-BACKUP", nil)
	var outLoc locationResp
	require.NoError(t, json.Unmarshal(outRaw, &outLoc))
	assert.Nil(t, outLoc.Location, "the takeout auto-vacates the shared slot")

	// Return through the alias counts exposure on the canonical batch and
	// leaves it unplaced.
	code, eb, _ = postEvent(t, srv, "A-2-BACKUP", "return", "2026-09-13T08:00:15Z")
	require.Equal(t, http.StatusCreated, code)
	assert.Equal(t, "in", eb.State)
	assert.Equal(t, int64(10), eb.AccumulatedSeconds)

	// Re-place through the alias: still the same batch/slot ledger.
	code, raw = doPutLocation(t, srv.URL, "A-2-BACKUP", "SLOT-C")
	require.Equal(t, http.StatusOK, code)
	lb = decodeLocation(t, code, raw)
	require.NotNil(t, lb.Location)
	assert.Equal(t, "SLOT-C", *lb.Location)

	// A refresh via the primary barcode shows the alias-driven final state.
	_, refreshedRaw := doJSON(t, http.MethodGet, srv.URL+"/api/batches/A-2", nil)
	var refreshed locationResp
	require.NoError(t, json.Unmarshal(refreshedRaw, &refreshed))
	assert.Equal(t, "in", refreshed.State)
	require.NotNil(t, refreshed.Location)
	assert.Equal(t, "SLOT-C", *refreshed.Location)
	assert.Equal(t, []string{"A-2-BACKUP"}, refreshed.Aliases)
}

func TestAliasBindConflictsAndValidation(t *testing.T) {
	srv := newServer(t)
	createBatch(t, srv, "A-3", 100, "2026-09-13T08:00:00Z")
	createBatch(t, srv, "A-4", 100, "2026-09-13T08:00:00Z")

	// Empty / blank alias.
	code, raw := doJSON(t, http.MethodPost, srv.URL+"/api/batches/A-3/aliases",
		map[string]string{"alias": "   "})
	require.Equal(t, http.StatusBadRequest, code)
	var e errResp
	require.NoError(t, json.Unmarshal(raw, &e))
	assert.Equal(t, "alias_required", e.Error.Code)

	// Alias equal to the batch's own primary barcode.
	code, _, e = bindAlias(t, srv, "A-3", "A-3")
	require.Equal(t, http.StatusConflict, code)
	assert.Equal(t, "alias_conflicts_barcode", e.Error.Code)

	// Alias equal to another primary barcode.
	code, _, e = bindAlias(t, srv, "A-3", "A-4")
	require.Equal(t, http.StatusConflict, code)
	assert.Equal(t, "alias_conflicts_barcode", e.Error.Code)

	// First bind succeeds; duplicate on the same batch is rejected.
	mustBind(t, srv, "A-3", "DUP")
	code, _, e = bindAlias(t, srv, "A-3", "DUP")
	require.Equal(t, http.StatusConflict, code)
	assert.Equal(t, "duplicate_alias", e.Error.Code)

	// The same alias bound to another batch is rejected.
	code, _, e = bindAlias(t, srv, "A-4", "DUP")
	require.Equal(t, http.StatusConflict, code)
	assert.Equal(t, "alias_bound_elsewhere", e.Error.Code)

	// Creating a new batch with a code already used by an alias conflicts.
	code, raw = doJSON(t, http.MethodPost, srv.URL+"/api/batches", map[string]any{
		"barcode": "DUP", "allowedSeconds": 10, "createdAt": "2026-09-13T09:00:00Z",
	})
	require.Equal(t, http.StatusConflict, code, string(raw))
	require.NoError(t, json.Unmarshal(raw, &e))
	assert.Equal(t, "duplicate_barcode", e.Error.Code)

	// Binding to an unknown batch is 404.
	code, _, e = bindAlias(t, srv, "GHOST", "X")
	require.Equal(t, http.StatusNotFound, code)
	assert.Equal(t, "not_found", e.Error.Code)
	// Scanning an unknown code (neither primary nor alias) is 404.
	assert.Equal(t, http.StatusNotFound, statusOf(getBatchRaw(t, srv, "UNKNOWN", "")))

	// Current batch/input unchanged: A-3 still has exactly the one alias.
	got := getAliasBatch(t, srv, "A-3")
	assert.Equal(t, []string{"DUP"}, got.Aliases)
}

func TestAliasUnbindRules(t *testing.T) {
	srv := newServer(t)
	createBatch(t, srv, "A-5", 100, "2026-09-13T08:00:00Z")
	createBatch(t, srv, "A-6", 100, "2026-09-13T08:00:00Z")
	doPutLocation(t, srv.URL, "A-5", "SLOT-A")
	postEvent(t, srv, "A-5", "takeout", "2026-09-13T08:00:05Z")
	postEvent(t, srv, "A-5", "return", "2026-09-13T08:00:15Z") // +10
	doPutLocation(t, srv.URL, "A-5", "SLOT-B")
	mustBind(t, srv, "A-5", "A-5-BACKUP")

	// Unknown / non-bound / primary-code unbind all conflict, changing nothing.
	code, _, e := unbindAlias(t, srv, "A-5", "NOPE")
	require.Equal(t, http.StatusConflict, code)
	assert.Equal(t, "alias_not_bound", e.Error.Code)
	code, _, e = unbindAlias(t, srv, "A-5", "A-5")
	require.Equal(t, http.StatusConflict, code)
	assert.Equal(t, "alias_not_bound", e.Error.Code)

	// A different batch cannot unbind an alias it does not own.
	code, _, e = unbindAlias(t, srv, "A-6", "A-5-BACKUP")
	require.Equal(t, http.StatusConflict, code)
	assert.Equal(t, "alias_not_bound", e.Error.Code)

	// Events, exposure and occupancy survive the rejected attempts.
	_, hitRaw := doJSON(t, http.MethodGet, srv.URL+"/api/batches/A-5-BACKUP", nil)
	var hit locationResp
	require.NoError(t, json.Unmarshal(hitRaw, &hit))
	require.NotNil(t, hit.Location)
	assert.Equal(t, "SLOT-B", *hit.Location)
	assert.Equal(t, []string{"A-5-BACKUP"}, hit.Aliases)
	assert.Equal(t, "A-5-BACKUP", hit.MatchedBarcode)

	// Owner unbinds: the alias can no longer be queried, state stays intact.
	code, b, _ := unbindAlias(t, srv, "A-5", "A-5-BACKUP")
	require.Equal(t, http.StatusOK, code)
	assert.Empty(t, b.Aliases)
	assert.Equal(t, http.StatusNotFound, statusOf(getBatchRaw(t, srv, "A-5-BACKUP", "")))
	_, stillRaw := doJSON(t, http.MethodGet, srv.URL+"/api/batches/A-5", nil)
	var still locationResp
	require.NoError(t, json.Unmarshal(stillRaw, &still))
	require.NotNil(t, still.Location)
	assert.Equal(t, "SLOT-B", *still.Location)
	assert.Equal(t, int64(10), still.AccumulatedSeconds)

	// Unbinding again conflicts; the primary barcode timing flow is unaffected.
	code, _, e = unbindAlias(t, srv, "A-5", "A-5-BACKUP")
	require.Equal(t, http.StatusConflict, code)
	assert.Equal(t, "alias_not_bound", e.Error.Code)
	c2, pb, _ := postEvent(t, srv, "A-5", "takeout", "2026-09-13T08:00:20Z")
	require.Equal(t, http.StatusCreated, c2)
	assert.Equal(t, "out", pb.State)
}

func TestConcurrentBindSameAliasOnlyOneWins(t *testing.T) {
	srv := newServer(t)
	const n = 8
	for i := 0; i < n; i++ {
		createBatch(t, srv, fmt.Sprintf("A-C-%d", i), 100, "2026-09-13T08:00:00Z")
	}
	var wg sync.WaitGroup
	codes := make([]int, n)
	errCodes := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			raw, _ := json.Marshal(map[string]string{"alias": "RACE-BACKUP"})
			res, err := http.Post(fmt.Sprintf("%s/api/batches/A-C-%d/aliases", srv.URL, i),
				"application/json", bytes.NewReader(raw))
			if err != nil {
				codes[i] = -1
				return
			}
			defer res.Body.Close()
			body, _ := io.ReadAll(res.Body)
			codes[i] = res.StatusCode
			if res.StatusCode != http.StatusOK {
				var e errResp
				_ = json.Unmarshal(body, &e)
				errCodes[i] = e.Error.Code
			}
		}(i)
	}
	wg.Wait()

	wins := 0
	for i, c := range codes {
		if c == http.StatusOK {
			wins++
			continue
		}
		assert.Equal(t, http.StatusConflict, c)
		assert.Contains(t, []string{"duplicate_alias", "alias_bound_elsewhere"}, errCodes[i])
	}
	assert.Equal(t, 1, wins, "only one batch may bind the same alias")

	// The winning canonical batch resolves the alias.
	got := getAliasBatch(t, srv, "RACE-BACKUP")
	assert.Equal(t, "RACE-BACKUP", got.MatchedBarcode)
	assert.Contains(t, got.Barcode, "A-C-")
	assert.Equal(t, []string{"RACE-BACKUP"}, got.Aliases)
}

func TestLegacyBatchResponseStillCompatible(t *testing.T) {
	srv := newServer(t)
	createBatch(t, srv, "A-7", 100, "2026-09-13T08:00:00Z")

	// Primary scan: no matchedBarcode key, aliases is an additive empty list.
	var keys map[string]json.RawMessage
	code, raw := doJSON(t, http.MethodGet, srv.URL+"/api/batches/A-7", nil)
	require.Equal(t, http.StatusOK, code)
	require.NoError(t, json.Unmarshal(raw, &keys))
	_, hasMatched := keys["matchedBarcode"]
	assert.False(t, hasMatched, "primary scan omits matchedBarcode")
	_, hasAliases := keys["aliases"]
	assert.True(t, hasAliases, "the aliases key is additive")

	// Alias scan: the response gains matchedBarcode but keeps the primary code.
	mustBind(t, srv, "A-7", "A-7-BACKUP")
	code, raw = doJSON(t, http.MethodGet, srv.URL+"/api/batches/A-7-BACKUP", nil)
	require.Equal(t, http.StatusOK, code)
	require.NoError(t, json.Unmarshal(raw, &keys))
	_, hasMatched = keys["matchedBarcode"]
	assert.True(t, hasMatched)
}

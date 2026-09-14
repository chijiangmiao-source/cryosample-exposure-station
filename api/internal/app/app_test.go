package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"timingstation/api/internal/app"
	"timingstation/api/internal/store"
)

// --- helpers ---------------------------------------------------------------

type batchResp struct {
	Barcode            string `json:"barcode"`
	AllowedSeconds     int64  `json:"allowedSeconds"`
	State              string `json:"state"`
	Status             string `json:"status"`
	Usable             bool   `json:"usable"`
	AccumulatedSeconds int64  `json:"accumulatedSeconds"`
	RemainingSeconds   int64  `json:"remainingSeconds"`
	CreatedAt          string `json:"createdAt"`
	LastEvent          *struct {
		ID           int64   `json:"id"`
		Type         string  `json:"type"`
		At           string  `json:"at"`
		DeltaSeconds *int64  `json:"deltaSeconds"`
		RevokedAt    *string `json:"revokedAt"`
		RevokeReason *string `json:"revokeReason"`
	} `json:"lastEvent"`
	Projection *struct {
		AsOf               string `json:"asOf"`
		AccumulatedSeconds int64  `json:"accumulatedSeconds"`
		RemainingSeconds   int64  `json:"remainingSeconds"`
		Usable             bool   `json:"usable"`
		ProjectedOverLimit bool   `json:"projectedOverLimit"`
		Settled            bool   `json:"settled"`
	} `json:"projection"`
	// MatchedBarcode reports the scanned code when it was a bound alias
	// (omitted on a primary-barcode scan); Aliases lists bound backup codes.
	MatchedBarcode string   `json:"matchedBarcode"`
	Aliases        []string `json:"aliases"`
}

type errResp struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type eventsResp struct {
	Events []struct {
		ID           int64   `json:"id"`
		Type         string  `json:"type"`
		At           string  `json:"at"`
		DeltaSeconds *int64  `json:"deltaSeconds"`
		RevokedAt    *string `json:"revokedAt"`
		RevokeReason *string `json:"revokeReason"`
	} `json:"events"`
}

func newServer(t *testing.T) *httptest.Server {
	t.Helper()
	gin.SetMode(gin.TestMode)
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() })
	srv := httptest.NewServer(app.NewRouter(st))
	t.Cleanup(srv.Close)
	return srv
}

func doJSON(t *testing.T, method, url string, body any) (int, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, url, rdr)
	require.NoError(t, err)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	return res.StatusCode, raw
}

func createBatch(t *testing.T, srv *httptest.Server, barcode string, allowed int64, createdAt string) (int, batchResp) {
	t.Helper()
	code, raw := doJSON(t, http.MethodPost, srv.URL+"/api/batches", map[string]any{
		"barcode": barcode, "allowedSeconds": allowed, "createdAt": createdAt,
	})
	var b batchResp
	if code == http.StatusCreated {
		require.NoError(t, json.Unmarshal(raw, &b))
	}
	return code, b
}

func postEvent(t *testing.T, srv *httptest.Server, barcode, typ, at string) (int, batchResp, errResp) {
	t.Helper()
	code, raw := doJSON(t, http.MethodPost, srv.URL+"/api/batches/"+barcode+"/events", map[string]string{
		"type": typ, "at": at,
	})
	var b batchResp
	var e errResp
	if code == http.StatusCreated {
		require.NoError(t, json.Unmarshal(raw, &b))
	} else {
		require.NoError(t, json.Unmarshal(raw, &e))
	}
	return code, b, e
}

func getBatch(t *testing.T, srv *httptest.Server, barcode string) batchResp {
	t.Helper()
	code, raw := doJSON(t, http.MethodGet, srv.URL+"/api/batches/"+barcode, nil)
	require.Equal(t, http.StatusOK, code)
	var b batchResp
	require.NoError(t, json.Unmarshal(raw, &b))
	return b
}

// getBatchRaw GETs a batch with an optional raw query string (e.g. asOf) and
// returns the raw status/body.
func getBatchRaw(t *testing.T, srv *httptest.Server, barcode, query string) (int, []byte) {
	t.Helper()
	url := srv.URL + "/api/batches/" + barcode
	if query != "" {
		url += "?" + query
	}
	return doJSON(t, http.MethodGet, url, nil)
}

func listEvents(t *testing.T, srv *httptest.Server, barcode string) eventsResp {
	t.Helper()
	code, raw := doJSON(t, http.MethodGet, srv.URL+"/api/batches/"+barcode+"/events", nil)
	require.Equal(t, http.StatusOK, code)
	var ev eventsResp
	require.NoError(t, json.Unmarshal(raw, &ev))
	return ev
}

func revokeEvent(t *testing.T, srv *httptest.Server, barcode string, id int64, at, reason string) (int, batchResp, errResp) {
	t.Helper()
	code, raw := doJSON(t, http.MethodPost,
		fmt.Sprintf("%s/api/batches/%s/events/%d/revoke", srv.URL, barcode, id),
		map[string]string{"at": at, "reason": reason})
	var b batchResp
	var e errResp
	if code == http.StatusOK {
		require.NoError(t, json.Unmarshal(raw, &b))
	} else {
		require.NoError(t, json.Unmarshal(raw, &e))
	}
	return code, b, e
}

// revokeRaw is a goroutine-safe variant returning (status, errorCode).
func revokeRaw(srv *httptest.Server, barcode string, id int64, at, reason string) (int, string) {
	raw, err := json.Marshal(map[string]string{"at": at, "reason": reason})
	if err != nil {
		return -1, err.Error()
	}
	res, err := http.Post(fmt.Sprintf("%s/api/batches/%s/events/%d/revoke", srv.URL, barcode, id),
		"application/json", bytes.NewReader(raw))
	if err != nil {
		return -1, err.Error()
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		var e errResp
		if json.Unmarshal(body, &e) == nil {
			return res.StatusCode, e.Error.Code
		}
	}
	return res.StatusCode, ""
}

// --- creation & validation ---------------------------------------------------

func TestCreateBatchInitialState(t *testing.T) {
	srv := newServer(t)
	code, b := createBatch(t, srv, "B-1", 3600, "2026-09-13T08:00:00Z")
	require.Equal(t, http.StatusCreated, code)
	assert.Equal(t, "B-1", b.Barcode)
	assert.Equal(t, int64(3600), b.AllowedSeconds)
	assert.Equal(t, "in", b.State, "batch must start inside the cabinet")
	assert.Equal(t, "usable", b.Status)
	assert.True(t, b.Usable)
	assert.Equal(t, int64(0), b.AccumulatedSeconds, "accumulated exposure starts at zero")
	assert.Equal(t, int64(3600), b.RemainingSeconds)
	assert.Equal(t, "2026-09-13T08:00:00Z", b.CreatedAt)
	assert.Nil(t, b.LastEvent)
}

func TestCreateBatchDuplicateBarcode(t *testing.T) {
	srv := newServer(t)
	code, _ := createBatch(t, srv, "B-DUP", 10, "2026-09-13T08:00:00Z")
	require.Equal(t, http.StatusCreated, code)
	code, raw := doJSON(t, http.MethodPost, srv.URL+"/api/batches", map[string]any{
		"barcode": "B-DUP", "allowedSeconds": 20, "createdAt": "2026-09-13T09:00:00Z",
	})
	assert.Equal(t, http.StatusConflict, code)
	var e errResp
	require.NoError(t, json.Unmarshal(raw, &e))
	assert.Equal(t, "duplicate_barcode", e.Error.Code)
}

func TestCreateBatchValidation(t *testing.T) {
	srv := newServer(t)
	cases := []struct {
		name string
		body map[string]any
		code string
	}{
		{"empty barcode", map[string]any{"barcode": "  ", "allowedSeconds": 10, "createdAt": "2026-09-13T08:00:00Z"}, "barcode_required"},
		{"zero allowed", map[string]any{"barcode": "V-1", "allowedSeconds": 0, "createdAt": "2026-09-13T08:00:00Z"}, "invalid_allowed_seconds"},
		{"negative allowed", map[string]any{"barcode": "V-2", "allowedSeconds": -5, "createdAt": "2026-09-13T08:00:00Z"}, "invalid_allowed_seconds"},
		{"fractional seconds", map[string]any{"barcode": "V-3", "allowedSeconds": 10, "createdAt": "2026-09-13T08:00:00.5Z"}, "invalid_time"},
		{"offset not Z", map[string]any{"barcode": "V-4", "allowedSeconds": 10, "createdAt": "2026-09-13T08:00:00+08:00"}, "invalid_time"},
		{"space separator", map[string]any{"barcode": "V-5", "allowedSeconds": 10, "createdAt": "2026-09-13 08:00:00"}, "invalid_time"},
		{"missing Z", map[string]any{"barcode": "V-6", "allowedSeconds": 10, "createdAt": "2026-09-13T08:00:00"}, "invalid_time"},
		{"garbage", map[string]any{"barcode": "V-7", "allowedSeconds": 10, "createdAt": "tomorrow"}, "invalid_time"},
		{"impossible date", map[string]any{"barcode": "V-8", "allowedSeconds": 10, "createdAt": "2026-13-40T25:61:61Z"}, "invalid_time"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, raw := doJSON(t, http.MethodPost, srv.URL+"/api/batches", tc.body)
			require.Equal(t, http.StatusBadRequest, code, "body: %s", raw)
			var e errResp
			require.NoError(t, json.Unmarshal(raw, &e))
			assert.Equal(t, tc.code, e.Error.Code)
		})
	}
}

func TestGetUnknownBatch(t *testing.T) {
	srv := newServer(t)
	code, raw := doJSON(t, http.MethodGet, srv.URL+"/api/batches/NOPE", nil)
	assert.Equal(t, http.StatusNotFound, code)
	var e errResp
	require.NoError(t, json.Unmarshal(raw, &e))
	assert.Equal(t, "not_found", e.Error.Code)
}

// --- asOf projection (non-persistent risk evaluation) -------------------------

func TestGetBatchWithoutAsOfKeepsLegacySemantics(t *testing.T) {
	srv := newServer(t)
	createBatch(t, srv, "Q-0", 10, "2026-09-13T08:00:00Z")
	postEvent(t, srv, "Q-0", "takeout", "2026-09-13T08:00:05Z")

	b := getBatch(t, srv, "Q-0")
	assert.Nil(t, b.Projection, "no projection field without asOf")
	assert.Equal(t, int64(0), b.AccumulatedSeconds, "settled totals are returned")
	assert.Equal(t, int64(10), b.RemainingSeconds)
	assert.Equal(t, "out", b.State)
	assert.Equal(t, "usable", b.Status)

	// An explicitly present but empty asOf is a malformed time, not "absent".
	code, raw := getBatchRaw(t, srv, "Q-0", "asOf=")
	require.Equal(t, http.StatusBadRequest, code)
	var pe errResp
	require.NoError(t, json.Unmarshal(raw, &pe))
	assert.Equal(t, "invalid_time", pe.Error.Code)

	// Whitespace-only is malformed as well.
	code, raw = getBatchRaw(t, srv, "Q-0", "asOf=%20%20")
	require.Equal(t, http.StatusBadRequest, code)
	require.NoError(t, json.Unmarshal(raw, &pe))
	assert.Equal(t, "invalid_time", pe.Error.Code)
}

func TestProjectionGrowsWithAsOfAndCrossesTheLimit(t *testing.T) {
	srv := newServer(t)
	createBatch(t, srv, "Q-1", 10, "2026-09-13T08:00:00Z")
	postEvent(t, srv, "Q-1", "takeout", "2026-09-13T08:00:05Z")

	// One second into the open takeout: projected 1/10.
	code, raw := getBatchRaw(t, srv, "Q-1", "asOf=2026-09-13T08:00:06Z")
	require.Equal(t, http.StatusOK, code)
	var b batchResp
	require.NoError(t, json.Unmarshal(raw, &b))
	require.NotNil(t, b.Projection)
	assert.Equal(t, "2026-09-13T08:00:06Z", b.Projection.AsOf)
	assert.Equal(t, int64(1), b.Projection.AccumulatedSeconds)
	assert.Equal(t, int64(9), b.Projection.RemainingSeconds)
	assert.True(t, b.Projection.Usable)
	assert.False(t, b.Projection.ProjectedOverLimit)
	assert.False(t, b.Projection.Settled)
	// Settled batch fields stay untouched while the projection is over limit.
	assert.Equal(t, int64(0), b.AccumulatedSeconds)
	assert.Equal(t, int64(10), b.RemainingSeconds)
	assert.Equal(t, "usable", b.Status)

	// Exactly at the limit: still usable.
	code, raw = getBatchRaw(t, srv, "Q-1", "asOf=2026-09-13T08:00:15Z")
	require.Equal(t, http.StatusOK, code)
	require.NoError(t, json.Unmarshal(raw, &b))
	require.NotNil(t, b.Projection)
	assert.Equal(t, int64(10), b.Projection.AccumulatedSeconds)
	assert.Equal(t, int64(0), b.Projection.RemainingSeconds)
	assert.True(t, b.Projection.Usable)
	assert.False(t, b.Projection.ProjectedOverLimit)

	// One second past the limit: projected over limit, but nothing is scrapped.
	code, raw = getBatchRaw(t, srv, "Q-1", "asOf=2026-09-13T08:00:16Z")
	require.Equal(t, http.StatusOK, code)
	require.NoError(t, json.Unmarshal(raw, &b))
	require.NotNil(t, b.Projection)
	assert.Equal(t, int64(11), b.Projection.AccumulatedSeconds)
	assert.Equal(t, int64(-1), b.Projection.RemainingSeconds)
	assert.False(t, b.Projection.Usable)
	assert.True(t, b.Projection.ProjectedOverLimit)
	assert.Equal(t, "usable", b.Status, "a projection must not scrap the batch")
	assert.Equal(t, "out", b.State)

	// The real return then settles the exposure and scraps the batch.
	code, rb, _ := postEvent(t, srv, "Q-1", "return", "2026-09-13T08:00:16Z")
	require.Equal(t, http.StatusCreated, code)
	assert.Equal(t, int64(11), rb.AccumulatedSeconds)
	assert.Equal(t, "scrapped", rb.Status)
	assert.Nil(t, rb.Projection, "POST /events never carries a projection")
	assert.Len(t, listEvents(t, srv, "Q-1").Events, 2, "evaluations write no events")
}

func TestProjectionAddsOntoSettledAccumulated(t *testing.T) {
	srv := newServer(t)
	createBatch(t, srv, "Q-2", 100, "2026-09-13T08:00:00Z")
	postEvent(t, srv, "Q-2", "takeout", "2026-09-13T08:00:10Z")
	postEvent(t, srv, "Q-2", "return", "2026-09-13T08:00:40Z") // +30 settled
	postEvent(t, srv, "Q-2", "takeout", "2026-09-13T08:01:00Z")

	code, raw := getBatchRaw(t, srv, "Q-2", "asOf=2026-09-13T08:01:30Z")
	require.Equal(t, http.StatusOK, code)
	var b batchResp
	require.NoError(t, json.Unmarshal(raw, &b))
	require.NotNil(t, b.Projection)
	assert.Equal(t, int64(60), b.Projection.AccumulatedSeconds, "30 settled + 30 open")
	assert.Equal(t, int64(40), b.Projection.RemainingSeconds)
	assert.True(t, b.Projection.Usable)
	assert.False(t, b.Projection.Settled)
	assert.Equal(t, int64(30), b.AccumulatedSeconds, "settled total unchanged")
}

func TestProjectionForInCabinetBatchReturnsSettledValues(t *testing.T) {
	srv := newServer(t)
	createBatch(t, srv, "Q-3", 100, "2026-09-13T08:00:00Z")
	postEvent(t, srv, "Q-3", "takeout", "2026-09-13T08:00:10Z")
	postEvent(t, srv, "Q-3", "return", "2026-09-13T08:00:40Z") // +30, in cabinet

	// At the last event time itself the evaluation must succeed (not earlier).
	code, raw := getBatchRaw(t, srv, "Q-3", "asOf=2026-09-13T08:00:40Z")
	require.Equal(t, http.StatusOK, code)
	var b batchResp
	require.NoError(t, json.Unmarshal(raw, &b))
	require.NotNil(t, b.Projection)
	assert.True(t, b.Projection.Settled)
	assert.Equal(t, int64(30), b.Projection.AccumulatedSeconds)
	assert.Equal(t, int64(70), b.Projection.RemainingSeconds)
	assert.True(t, b.Projection.Usable)
	assert.False(t, b.Projection.ProjectedOverLimit)

	// A much later asOf changes nothing for an in-cabinet batch: there is no
	// open takeout to extend.
	code, raw = getBatchRaw(t, srv, "Q-3", "asOf=2026-09-13T09:00:00Z")
	require.Equal(t, http.StatusOK, code)
	require.NoError(t, json.Unmarshal(raw, &b))
	require.NotNil(t, b.Projection)
	assert.Equal(t, int64(30), b.Projection.AccumulatedSeconds)
	assert.True(t, b.Projection.Settled)

	// A scrapped in-cabinet batch reports settled, unusable figures.
	postEvent(t, srv, "Q-3", "takeout", "2026-09-13T08:01:00Z")
	postEvent(t, srv, "Q-3", "return", "2026-09-13T08:02:11Z") // +71 -> 101 > 100
	b = getBatch(t, srv, "Q-3")
	require.Equal(t, "scrapped", b.Status)
	code, raw = getBatchRaw(t, srv, "Q-3", "asOf=2026-09-13T08:03:00Z")
	require.Equal(t, http.StatusOK, code)
	require.NoError(t, json.Unmarshal(raw, &b))
	require.NotNil(t, b.Projection)
	assert.True(t, b.Projection.Settled)
	assert.Equal(t, int64(101), b.Projection.AccumulatedSeconds)
	assert.False(t, b.Projection.Usable)
	assert.True(t, b.Projection.ProjectedOverLimit)
}

func TestProjectionBeforeLastEventConflicts(t *testing.T) {
	srv := newServer(t)
	createBatch(t, srv, "Q-4", 100, "2026-09-13T08:00:00Z")

	// Before the first event the floor is the creation time.
	code, _, e := getBatchAsOfErr(t, srv, "Q-4", "2026-09-13T07:59:59Z")
	assert.Equal(t, http.StatusConflict, code)
	assert.Equal(t, "time_not_monotonic", e.Error.Code)

	postEvent(t, srv, "Q-4", "takeout", "2026-09-13T08:00:10Z")
	code, _, e = getBatchAsOfErr(t, srv, "Q-4", "2026-09-13T08:00:09Z")
	assert.Equal(t, http.StatusConflict, code)
	assert.Equal(t, "time_not_monotonic", e.Error.Code)

	// The rejected evaluation leaves the batch exactly as it was.
	b := getBatch(t, srv, "Q-4")
	assert.Equal(t, "out", b.State)
	assert.Equal(t, int64(0), b.AccumulatedSeconds)
	assert.Len(t, listEvents(t, srv, "Q-4").Events, 1)
}

func TestProjectionInvalidTimeIsBadRequest(t *testing.T) {
	srv := newServer(t)
	createBatch(t, srv, "Q-5", 100, "2026-09-13T08:00:00Z")
	for _, q := range []string{
		"asOf=",                            // present but empty
		"asOf=%20%20",                      // whitespace only
		"asOf=2026-09-13T08:00:00",         // missing Z
		"asOf=2026-09-13T08:00:00%2B08:00", // offset
		"asOf=2026-09-13T08:00:00.000Z",    // fractional seconds
		"asOf=not-a-time",
	} {
		code, raw := getBatchRaw(t, srv, "Q-5", q)
		assert.Equal(t, http.StatusBadRequest, code, "query=%s", q)
		var e errResp
		require.NoError(t, json.Unmarshal(raw, &e))
		assert.Equal(t, "invalid_time", e.Error.Code, "query=%s", q)
	}
	// Not-found still wins over a malformed asOf only when the batch is absent;
	// validation order keeps unknown barcodes at 404 regardless of asOf.
	code, raw := getBatchRaw(t, srv, "GHOST", "asOf=garbage")
	assert.Equal(t, http.StatusNotFound, code, string(raw))
}

func getBatchAsOfErr(t *testing.T, srv *httptest.Server, barcode, asOf string) (int, batchResp, errResp) {
	t.Helper()
	code, raw := getBatchRaw(t, srv, barcode, "asOf="+asOf)
	var b batchResp
	var e errResp
	if code == http.StatusOK {
		require.NoError(t, json.Unmarshal(raw, &b))
	} else {
		require.NoError(t, json.Unmarshal(raw, &e))
	}
	return code, b, e
}

// --- transition rules ---------------------------------------------------------

func TestTakeoutReturnHappyPath(t *testing.T) {
	srv := newServer(t)
	createBatch(t, srv, "F-1", 100, "2026-09-13T08:00:00Z")

	code, b, _ := postEvent(t, srv, "F-1", "takeout", "2026-09-13T08:00:10Z")
	require.Equal(t, http.StatusCreated, code)
	assert.Equal(t, "out", b.State)
	assert.Equal(t, int64(0), b.AccumulatedSeconds)
	require.NotNil(t, b.LastEvent)
	assert.Equal(t, "takeout", b.LastEvent.Type)
	assert.Equal(t, "2026-09-13T08:00:10Z", b.LastEvent.At)
	assert.Nil(t, b.LastEvent.DeltaSeconds)

	code, b, _ = postEvent(t, srv, "F-1", "return", "2026-09-13T08:00:40Z")
	require.Equal(t, http.StatusCreated, code)
	assert.Equal(t, "in", b.State)
	assert.Equal(t, int64(30), b.AccumulatedSeconds, "return adds return-minus-takeout seconds")
	assert.Equal(t, int64(70), b.RemainingSeconds)
	require.NotNil(t, b.LastEvent)
	assert.Equal(t, "return", b.LastEvent.Type)
	require.NotNil(t, b.LastEvent.DeltaSeconds)
	assert.Equal(t, int64(30), *b.LastEvent.DeltaSeconds)
	assert.Equal(t, "usable", b.Status)
}

func TestConsecutiveTakeoutRejected(t *testing.T) {
	srv := newServer(t)
	createBatch(t, srv, "F-2", 100, "2026-09-13T08:00:00Z")
	code, _, _ := postEvent(t, srv, "F-2", "takeout", "2026-09-13T08:00:10Z")
	require.Equal(t, http.StatusCreated, code)

	code, _, e := postEvent(t, srv, "F-2", "takeout", "2026-09-13T08:00:20Z")
	assert.Equal(t, http.StatusConflict, code)
	assert.Equal(t, "invalid_transition", e.Error.Code)

	// The failed takeout must not have written anything.
	b := getBatch(t, srv, "F-2")
	assert.Equal(t, "out", b.State)
	assert.Equal(t, "2026-09-13T08:00:10Z", b.LastEvent.At)
	assert.Len(t, listEvents(t, srv, "F-2").Events, 1)
}

func TestReturnWithoutTakeoutRejected(t *testing.T) {
	srv := newServer(t)
	createBatch(t, srv, "F-3", 100, "2026-09-13T08:00:00Z")

	code, _, e := postEvent(t, srv, "F-3", "return", "2026-09-13T08:00:10Z")
	assert.Equal(t, http.StatusConflict, code)
	assert.Equal(t, "invalid_transition", e.Error.Code)

	b := getBatch(t, srv, "F-3")
	assert.Equal(t, "in", b.State)
	assert.Equal(t, int64(0), b.AccumulatedSeconds)
	assert.Empty(t, listEvents(t, srv, "F-3").Events)
}

func TestDuplicateReturnRejectedAndNotDoubleCounted(t *testing.T) {
	srv := newServer(t)
	createBatch(t, srv, "F-4", 100, "2026-09-13T08:00:00Z")
	postEvent(t, srv, "F-4", "takeout", "2026-09-13T08:00:10Z")
	code, b, _ := postEvent(t, srv, "F-4", "return", "2026-09-13T08:00:20Z")
	require.Equal(t, http.StatusCreated, code)
	require.Equal(t, int64(10), b.AccumulatedSeconds)

	// Paper-log bug scenario: the same return recorded a second time.
	code, _, e := postEvent(t, srv, "F-4", "return", "2026-09-13T08:00:30Z")
	assert.Equal(t, http.StatusConflict, code)
	assert.Equal(t, "invalid_transition", e.Error.Code)

	b = getBatch(t, srv, "F-4")
	assert.Equal(t, int64(10), b.AccumulatedSeconds, "duplicate return must not add exposure twice")
	assert.Len(t, listEvents(t, srv, "F-4").Events, 2)
}

func TestOutOfOrderTimeRejected(t *testing.T) {
	srv := newServer(t)
	createBatch(t, srv, "F-5", 100, "2026-09-13T08:00:00Z")
	postEvent(t, srv, "F-5", "takeout", "2026-09-13T08:00:10Z")

	// Equal to the previous timestamp.
	code, _, e := postEvent(t, srv, "F-5", "return", "2026-09-13T08:00:10Z")
	assert.Equal(t, http.StatusConflict, code)
	assert.Equal(t, "time_not_monotonic", e.Error.Code)

	// Strictly before the previous timestamp.
	code, _, e = postEvent(t, srv, "F-5", "return", "2026-09-13T08:00:05Z")
	assert.Equal(t, http.StatusConflict, code)
	assert.Equal(t, "time_not_monotonic", e.Error.Code)

	// Equal to createdAt is also rejected for the very first event.
	createBatch(t, srv, "F-5b", 100, "2026-09-13T08:00:00Z")
	code, _, e = postEvent(t, srv, "F-5b", "takeout", "2026-09-13T08:00:00Z")
	assert.Equal(t, http.StatusConflict, code)
	assert.Equal(t, "time_not_monotonic", e.Error.Code)

	b := getBatch(t, srv, "F-5")
	assert.Equal(t, "out", b.State)
	assert.Equal(t, int64(0), b.AccumulatedSeconds)
	assert.Len(t, listEvents(t, srv, "F-5").Events, 1)
}

func TestEventTimeFormatRejected(t *testing.T) {
	srv := newServer(t)
	createBatch(t, srv, "F-6", 100, "2026-09-13T08:00:00Z")
	for _, at := range []string{
		"2026-09-13T08:00:10",       // missing Z
		"2026-09-13T08:00:10+08:00", // offset
		"2026-09-13T08:00:10.000Z",  // fractional seconds
		"2026-09-13 08:00:10",       // space separator
		"not-a-time",
	} {
		code, _, e := postEvent(t, srv, "F-6", "takeout", at)
		assert.Equal(t, http.StatusBadRequest, code, "at=%q", at)
		assert.Equal(t, "invalid_time", e.Error.Code, "at=%q", at)
	}
	code, _, e := postEvent(t, srv, "F-6", "freeze", "2026-09-13T08:00:10Z")
	assert.Equal(t, http.StatusBadRequest, code)
	assert.Equal(t, "invalid_event_type", e.Error.Code)
}

// --- exposure limit boundaries -------------------------------------------------

func TestBoundaryReturnExactlyAtLimitStaysUsable(t *testing.T) {
	srv := newServer(t)
	createBatch(t, srv, "L-1", 10, "2026-09-13T08:00:00Z")
	postEvent(t, srv, "L-1", "takeout", "2026-09-13T08:00:05Z")
	code, b, _ := postEvent(t, srv, "L-1", "return", "2026-09-13T08:00:15Z")
	require.Equal(t, http.StatusCreated, code)
	assert.Equal(t, int64(10), b.AccumulatedSeconds)
	assert.Equal(t, int64(0), b.RemainingSeconds)
	assert.Equal(t, "usable", b.Status, "accumulated == limit must remain usable")
	assert.True(t, b.Usable)

	// And the batch can still go out and come back within its remaining 0s...
	// any positive exposure now exceeds the limit.
	postEvent(t, srv, "L-1", "takeout", "2026-09-13T08:00:20Z")
	code, b, _ = postEvent(t, srv, "L-1", "return", "2026-09-13T08:00:21Z")
	require.Equal(t, http.StatusCreated, code)
	assert.Equal(t, int64(11), b.AccumulatedSeconds)
	assert.Equal(t, "scrapped", b.Status)
	assert.False(t, b.Usable)
}

func TestOverLimitReturnScrapsImmediately(t *testing.T) {
	srv := newServer(t)
	createBatch(t, srv, "L-2", 10, "2026-09-13T08:00:00Z")
	postEvent(t, srv, "L-2", "takeout", "2026-09-13T08:00:05Z")
	code, b, _ := postEvent(t, srv, "L-2", "return", "2026-09-13T08:00:16Z")
	require.Equal(t, http.StatusCreated, code)
	assert.Equal(t, int64(11), b.AccumulatedSeconds)
	assert.Equal(t, int64(-1), b.RemainingSeconds)
	assert.Equal(t, "scrapped", b.Status, "exceeding the limit scraps the batch on return")
}

func TestScrappedBatchCannotBeTakenOut(t *testing.T) {
	srv := newServer(t)
	createBatch(t, srv, "L-3", 10, "2026-09-13T08:00:00Z")
	postEvent(t, srv, "L-3", "takeout", "2026-09-13T08:00:05Z")
	postEvent(t, srv, "L-3", "return", "2026-09-13T08:00:16Z")

	code, _, e := postEvent(t, srv, "L-3", "takeout", "2026-09-13T08:00:20Z")
	assert.Equal(t, http.StatusConflict, code)
	assert.Equal(t, "batch_scrapped", e.Error.Code)

	b := getBatch(t, srv, "L-3")
	assert.Equal(t, "in", b.State)
	assert.Len(t, listEvents(t, srv, "L-3").Events, 2, "rejected takeout must not write an event")
}

func TestAccumulationAcrossMultipleCycles(t *testing.T) {
	srv := newServer(t)
	createBatch(t, srv, "L-4", 60, "2026-09-13T08:00:00Z")
	postEvent(t, srv, "L-4", "takeout", "2026-09-13T08:00:10Z")
	postEvent(t, srv, "L-4", "return", "2026-09-13T08:00:30Z") // +20
	postEvent(t, srv, "L-4", "takeout", "2026-09-13T08:01:00Z")
	postEvent(t, srv, "L-4", "return", "2026-09-13T08:01:25Z") // +25
	b := getBatch(t, srv, "L-4")
	assert.Equal(t, int64(45), b.AccumulatedSeconds)
	assert.Equal(t, int64(15), b.RemainingSeconds)
	assert.Equal(t, "usable", b.Status)
	assert.Len(t, listEvents(t, srv, "L-4").Events, 4)
}

// --- persistence ------------------------------------------------------------

func TestStateSurvivesReopen(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "persist.db")

	st, err := store.Open(context.Background(), dbPath)
	require.NoError(t, err)
	srv := httptest.NewServer(app.NewRouter(st))
	createBatch(t, srv, "P-1", 100, "2026-09-13T08:00:00Z")
	postEvent(t, srv, "P-1", "takeout", "2026-09-13T08:00:10Z")
	postEvent(t, srv, "P-1", "return", "2026-09-13T08:00:40Z")
	srv.Close()
	require.NoError(t, st.Close())

	st2, err := store.Open(context.Background(), dbPath)
	require.NoError(t, err)
	defer st2.Close()
	srv2 := httptest.NewServer(app.NewRouter(st2))
	defer srv2.Close()

	b := getBatch(t, srv2, "P-1")
	assert.Equal(t, "in", b.State)
	assert.Equal(t, int64(30), b.AccumulatedSeconds)
	assert.Equal(t, "return", b.LastEvent.Type)
	assert.Len(t, listEvents(t, srv2, "P-1").Events, 2)
}

// --- concurrency --------------------------------------------------------------

// postEventRaw is a goroutine-safe variant returning (status, errorCode).
func postEventRaw(srv *httptest.Server, barcode, typ, at string) (int, string) {
	raw, err := json.Marshal(map[string]string{"type": typ, "at": at})
	if err != nil {
		return -1, err.Error()
	}
	res, err := http.Post(srv.URL+"/api/batches/"+barcode+"/events", "application/json", bytes.NewReader(raw))
	if err != nil {
		return -1, err.Error()
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusCreated {
		var e errResp
		if json.Unmarshal(body, &e) == nil {
			return res.StatusCode, e.Error.Code
		}
	}
	return res.StatusCode, ""
}

func TestConcurrentTakeoutOnlyOneWins(t *testing.T) {
	srv := newServer(t)
	createBatch(t, srv, "C-1", 100, "2026-09-13T08:00:00Z")

	const n = 16
	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			code, _ := postEventRaw(srv, "C-1", "takeout", "2026-09-13T08:00:10Z")
			codes[i] = code
		}(i)
	}
	wg.Wait()

	wins := 0
	for _, c := range codes {
		if c == http.StatusCreated {
			wins++
		} else {
			assert.Equal(t, http.StatusConflict, c)
		}
	}
	assert.Equal(t, 1, wins, "the same old state may succeed at most once")

	b := getBatch(t, srv, "C-1")
	assert.Equal(t, "out", b.State)
	assert.Len(t, listEvents(t, srv, "C-1").Events, 1)
}

func TestConcurrentReturnCountsExposureOnce(t *testing.T) {
	srv := newServer(t)
	createBatch(t, srv, "C-2", 100, "2026-09-13T08:00:00Z")
	postEvent(t, srv, "C-2", "takeout", "2026-09-13T08:00:10Z")

	const n = 16
	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			code, _ := postEventRaw(srv, "C-2", "return", "2026-09-13T08:00:20Z")
			codes[i] = code
		}(i)
	}
	wg.Wait()

	wins := 0
	for _, c := range codes {
		if c == http.StatusCreated {
			wins++
		} else {
			assert.Equal(t, http.StatusConflict, c)
		}
	}
	assert.Equal(t, 1, wins, "racing returns: exactly one may commit")

	b := getBatch(t, srv, "C-2")
	assert.Equal(t, "in", b.State)
	assert.Equal(t, int64(10), b.AccumulatedSeconds, "exposure must be counted exactly once")
	assert.Len(t, listEvents(t, srv, "C-2").Events, 2)
}

func TestConcurrentCreateSameBarcode(t *testing.T) {
	srv := newServer(t)
	const n = 8
	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			raw, _ := json.Marshal(map[string]any{
				"barcode": "C-3", "allowedSeconds": 10, "createdAt": "2026-09-13T08:00:00Z",
			})
			res, err := http.Post(srv.URL+"/api/batches", "application/json", bytes.NewReader(raw))
			if err != nil {
				codes[i] = -1
				return
			}
			res.Body.Close()
			codes[i] = res.StatusCode
		}(i)
	}
	wg.Wait()
	wins := 0
	for _, c := range codes {
		if c == http.StatusCreated {
			wins++
		} else {
			assert.Equal(t, http.StatusConflict, c)
		}
	}
	assert.Equal(t, 1, wins)
}

// --- misc -------------------------------------------------------------------

func TestHealth(t *testing.T) {
	srv := newServer(t)
	code, raw := doJSON(t, http.MethodGet, srv.URL+"/api/health", nil)
	assert.Equal(t, http.StatusOK, code)
	assert.JSONEq(t, `{"ok": true}`, string(raw))
}

func TestEventOnUnknownBatch(t *testing.T) {
	srv := newServer(t)
	code, _, e := postEvent(t, srv, "GHOST", "takeout", "2026-09-13T08:00:10Z")
	assert.Equal(t, http.StatusNotFound, code)
	assert.Equal(t, "not_found", e.Error.Code)
}

// --- revocation ---------------------------------------------------------------

// The headline scenario: a mistaken return scraps the batch; revoking it
// restores the out-of-cabinet state and the previous accumulated exposure,
// usability is re-evaluated, and the correct return can then be recorded.
func TestRevokeMistakenReturnThatScrapped(t *testing.T) {
	srv := newServer(t)
	createBatch(t, srv, "R-1", 10, "2026-09-13T08:00:00Z")
	postEvent(t, srv, "R-1", "takeout", "2026-09-13T08:00:05Z")
	code, b, _ := postEvent(t, srv, "R-1", "return", "2026-09-13T08:00:16Z") // +11 > 10
	require.Equal(t, http.StatusCreated, code)
	require.Equal(t, "scrapped", b.Status)

	// Revoke the mistaken return (event id 2).
	code, b, _ = revokeEvent(t, srv, "R-1", 2, "2026-09-13T08:00:30Z", "误扫归还，实际样本仍在柜外")
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "out", b.State, "revoking a return restores out-of-cabinet")
	assert.Equal(t, int64(0), b.AccumulatedSeconds, "the exposure is deducted back")
	assert.Equal(t, int64(10), b.RemainingSeconds)
	assert.Equal(t, "usable", b.Status, "usability is re-evaluated on the restored total")
	assert.True(t, b.Usable)
	require.NotNil(t, b.LastEvent, "the takeout becomes the last active event again")
	assert.Equal(t, "takeout", b.LastEvent.Type)
	assert.Equal(t, "2026-09-13T08:00:05Z", b.LastEvent.At)
	assert.Nil(t, b.LastEvent.RevokedAt)

	// The event log keeps every row; the revoked one carries the audit fields.
	evs := listEvents(t, srv, "R-1").Events
	require.Len(t, evs, 2, "revocation must never delete the event row")
	assert.Equal(t, "takeout", evs[0].Type)
	assert.Nil(t, evs[0].RevokedAt)
	assert.Equal(t, "return", evs[1].Type)
	require.NotNil(t, evs[1].RevokedAt)
	assert.Equal(t, "2026-09-13T08:00:30Z", *evs[1].RevokedAt)
	require.NotNil(t, evs[1].RevokeReason)
	assert.Equal(t, "误扫归还，实际样本仍在柜外", *evs[1].RevokeReason)

	// Continue operating on the correct state: the correct return (+10, at the
	// limit) is accepted and stays usable — it is written as a new row.
	code, b, _ = postEvent(t, srv, "R-1", "return", "2026-09-13T08:00:15Z")
	require.Equal(t, http.StatusCreated, code)
	assert.Equal(t, "in", b.State)
	assert.Equal(t, int64(10), b.AccumulatedSeconds)
	assert.Equal(t, "usable", b.Status)
	require.NotNil(t, b.LastEvent)
	assert.Equal(t, "return", b.LastEvent.Type)
	assert.Equal(t, int64(3), b.LastEvent.ID, "the corrected return is a new event row")
	assert.Equal(t, int64(10), *b.LastEvent.DeltaSeconds)
	evs = listEvents(t, srv, "R-1").Events
	require.Len(t, evs, 3)
	assert.Nil(t, evs[2].RevokedAt, "the new event is active")
}

func TestRevokeMistakenTakeout(t *testing.T) {
	srv := newServer(t)
	createBatch(t, srv, "R-2", 100, "2026-09-13T08:00:00Z")
	postEvent(t, srv, "R-2", "takeout", "2026-09-13T08:00:10Z")

	code, b, _ := revokeEvent(t, srv, "R-2", 1, "2026-09-13T08:00:20Z", "误扫取出")
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "in", b.State, "revoking a takeout puts the batch back in the cabinet")
	assert.Equal(t, "usable", b.Status)
	assert.Equal(t, int64(0), b.AccumulatedSeconds)
	assert.Nil(t, b.LastEvent, "no active event remains")

	// last_at is rewound to creation time: equal timestamps are still rejected.
	code, _, e := postEvent(t, srv, "R-2", "takeout", "2026-09-13T08:00:00Z")
	assert.Equal(t, http.StatusConflict, code)
	assert.Equal(t, "time_not_monotonic", e.Error.Code)

	// The takeout can be recorded again at its correct (later) time.
	code, b, _ = postEvent(t, srv, "R-2", "takeout", "2026-09-13T08:00:30Z")
	require.Equal(t, http.StatusCreated, code)
	assert.Equal(t, "out", b.State)
	require.NotNil(t, b.LastEvent)
	assert.Equal(t, int64(2), b.LastEvent.ID)

	evs := listEvents(t, srv, "R-2").Events
	require.Len(t, evs, 2)
	require.NotNil(t, evs[0].RevokedAt)
	assert.Equal(t, "误扫取出", *evs[0].RevokeReason)
	assert.Nil(t, evs[1].RevokedAt)
}

func TestRevokeNonLatestEventRejected(t *testing.T) {
	srv := newServer(t)
	createBatch(t, srv, "R-3", 100, "2026-09-13T08:00:00Z")
	postEvent(t, srv, "R-3", "takeout", "2026-09-13T08:00:05Z")
	postEvent(t, srv, "R-3", "return", "2026-09-13T08:00:15Z") // +10

	// The takeout (id 1) is no longer the latest active event.
	code, _, e := revokeEvent(t, srv, "R-3", 1, "2026-09-13T08:00:20Z", "nope")
	assert.Equal(t, http.StatusConflict, code)
	assert.Equal(t, "event_not_latest", e.Error.Code)

	b := getBatch(t, srv, "R-3")
	assert.Equal(t, "in", b.State)
	assert.Equal(t, int64(10), b.AccumulatedSeconds, "failed revocation changes nothing")
	require.NotNil(t, b.LastEvent)
	assert.Equal(t, "return", b.LastEvent.Type)
	for _, ev := range listEvents(t, srv, "R-3").Events {
		assert.Nil(t, ev.RevokedAt, "no audit row may be written on rejection")
	}
}

func TestRevokeAlreadyRevokedEventRejected(t *testing.T) {
	srv := newServer(t)
	createBatch(t, srv, "R-4", 100, "2026-09-13T08:00:00Z")
	postEvent(t, srv, "R-4", "takeout", "2026-09-13T08:00:05Z")
	code, b, _ := postEvent(t, srv, "R-4", "return", "2026-09-13T08:00:15Z")
	require.Equal(t, http.StatusCreated, code)
	require.Equal(t, int64(10), b.AccumulatedSeconds)

	code, _, _ = revokeEvent(t, srv, "R-4", 2, "2026-09-13T08:00:20Z", "first undo")
	require.Equal(t, http.StatusOK, code)

	// Repeated revocation of the same event is rejected.
	code, _, e := revokeEvent(t, srv, "R-4", 2, "2026-09-13T08:00:25Z", "second undo")
	assert.Equal(t, http.StatusConflict, code)
	assert.Equal(t, "event_already_revoked", e.Error.Code)

	b = getBatch(t, srv, "R-4")
	assert.Equal(t, "out", b.State)
	assert.Equal(t, int64(0), b.AccumulatedSeconds, "aggregate is unchanged by the failed retry")
	require.NotNil(t, b.LastEvent)
	assert.Equal(t, "takeout", b.LastEvent.Type)

	// Original audit fields are preserved, not overwritten.
	evs := listEvents(t, srv, "R-4").Events
	require.Len(t, evs, 2)
	require.NotNil(t, evs[1].RevokedAt)
	assert.Equal(t, "2026-09-13T08:00:20Z", *evs[1].RevokedAt)
	assert.Equal(t, "first undo", *evs[1].RevokeReason)
}

func TestRevokeTimeNotAfterLastOperationRejected(t *testing.T) {
	srv := newServer(t)
	createBatch(t, srv, "R-5", 100, "2026-09-13T08:00:00Z")
	postEvent(t, srv, "R-5", "takeout", "2026-09-13T08:00:10Z")

	for _, at := range []string{
		"2026-09-13T08:00:10Z", // equal to the last operation
		"2026-09-13T08:00:09Z", // earlier
	} {
		code, _, e := revokeEvent(t, srv, "R-5", 1, at, "late undo")
		assert.Equal(t, http.StatusConflict, code, "at=%s", at)
		assert.Equal(t, "time_not_monotonic", e.Error.Code, "at=%s", at)
	}

	b := getBatch(t, srv, "R-5")
	assert.Equal(t, "out", b.State, "state unchanged")
	assert.Nil(t, b.LastEvent.RevokedAt)
}

func TestRevokeValidation(t *testing.T) {
	srv := newServer(t)
	createBatch(t, srv, "R-6", 100, "2026-09-13T08:00:00Z")
	postEvent(t, srv, "R-6", "takeout", "2026-09-13T08:00:10Z")

	// Empty / blank reason.
	code, _, e := revokeEvent(t, srv, "R-6", 1, "2026-09-13T08:00:20Z", "")
	assert.Equal(t, http.StatusBadRequest, code)
	assert.Equal(t, "reason_required", e.Error.Code)
	code, _, e = revokeEvent(t, srv, "R-6", 1, "2026-09-13T08:00:20Z", "   ")
	assert.Equal(t, http.StatusBadRequest, code)
	assert.Equal(t, "reason_required", e.Error.Code)

	// Bad time shape.
	code, _, e = revokeEvent(t, srv, "R-6", 1, "2026-09-13T08:00:20", "x")
	assert.Equal(t, http.StatusBadRequest, code)
	assert.Equal(t, "invalid_time", e.Error.Code)
	code, _, e = revokeEvent(t, srv, "R-6", 1, "2026-09-13T08:00:20+08:00", "x")
	assert.Equal(t, http.StatusBadRequest, code)
	assert.Equal(t, "invalid_time", e.Error.Code)

	// Bad id.
	code, raw := doJSON(t, http.MethodPost, srv.URL+"/api/batches/R-6/events/abc/revoke",
		map[string]string{"at": "2026-09-13T08:00:20Z", "reason": "x"})
	assert.Equal(t, http.StatusBadRequest, code)
	require.NoError(t, json.Unmarshal(raw, &e))
	assert.Equal(t, "invalid_event_id", e.Error.Code)

	// Unknown batch.
	code, _, e = revokeEvent(t, srv, "GHOST", 1, "2026-09-13T08:00:20Z", "x")
	assert.Equal(t, http.StatusNotFound, code)
	assert.Equal(t, "not_found", e.Error.Code)

	// Unknown event id for an existing batch.
	code, _, e = revokeEvent(t, srv, "R-6", 999, "2026-09-13T08:00:20Z", "x")
	assert.Equal(t, http.StatusNotFound, code)
	assert.Equal(t, "not_found", e.Error.Code)

	// Nothing was written.
	assert.Equal(t, "out", getBatch(t, srv, "R-6").State)
	assert.Len(t, listEvents(t, srv, "R-6").Events, 1)
}

func TestRevokeRewindsAcrossMultipleCycles(t *testing.T) {
	srv := newServer(t)
	createBatch(t, srv, "R-7", 60, "2026-09-13T08:00:00Z")
	postEvent(t, srv, "R-7", "takeout", "2026-09-13T08:00:10Z")
	postEvent(t, srv, "R-7", "return", "2026-09-13T08:00:30Z") // +20
	postEvent(t, srv, "R-7", "takeout", "2026-09-13T08:01:00Z")
	code, b, _ := postEvent(t, srv, "R-7", "return", "2026-09-13T08:01:30Z") // +30, acc 50
	require.Equal(t, http.StatusCreated, code)

	// Undo the last return: out again, takeout@01:00 reopened, acc back to 20.
	code, b, _ = revokeEvent(t, srv, "R-7", 4, "2026-09-13T08:02:00Z", "误扫归还")
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "out", b.State)
	assert.Equal(t, int64(20), b.AccumulatedSeconds)
	require.NotNil(t, b.LastEvent)
	assert.Equal(t, "takeout", b.LastEvent.Type)
	assert.Equal(t, "2026-09-13T08:01:00Z", b.LastEvent.At)

	// Correct return adds exposure from the reopened takeout.
	code, b, _ = postEvent(t, srv, "R-7", "return", "2026-09-13T08:01:25Z") // +25
	require.Equal(t, http.StatusCreated, code)
	assert.Equal(t, int64(45), b.AccumulatedSeconds)
	assert.Equal(t, "usable", b.Status)

	// Undo that corrected return, then undo the reopened takeout: the batch is
	// back in the cabinet after the first cycle, with only its +20 exposure.
	code, _, _ = revokeEvent(t, srv, "R-7", 5, "2026-09-13T08:02:10Z", "归还时刻录错")
	require.Equal(t, http.StatusOK, code)
	code, b, _ = revokeEvent(t, srv, "R-7", 3, "2026-09-13T08:02:20Z", "取出误扫")
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "in", b.State)
	assert.Equal(t, int64(20), b.AccumulatedSeconds)
	assert.Equal(t, "usable", b.Status)
	require.NotNil(t, b.LastEvent)
	assert.Equal(t, "return", b.LastEvent.Type)
	assert.Equal(t, "2026-09-13T08:00:30Z", b.LastEvent.At)
}

func TestConcurrentRevokeOnlyOneWins(t *testing.T) {
	srv := newServer(t)
	createBatch(t, srv, "R-8", 100, "2026-09-13T08:00:00Z")
	postEvent(t, srv, "R-8", "takeout", "2026-09-13T08:00:05Z")
	postEvent(t, srv, "R-8", "return", "2026-09-13T08:00:15Z") // +10, in cabinet

	const n = 8
	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Two stations racing to undo the same latest event.
			codes[i], _ = revokeRaw(srv, "R-8", 2, "2026-09-13T08:00:30Z", "racing undo")
		}(i)
	}
	wg.Wait()

	wins := 0
	for _, c := range codes {
		if c == http.StatusOK {
			wins++
		} else {
			assert.Equal(t, http.StatusConflict, c)
		}
	}
	assert.Equal(t, 1, wins, "the same event may be revoked at most once")

	b := getBatch(t, srv, "R-8")
	assert.Equal(t, "out", b.State)
	assert.Equal(t, int64(0), b.AccumulatedSeconds, "exposure is deducted exactly once")
	require.NotNil(t, b.LastEvent)
	assert.Equal(t, "takeout", b.LastEvent.Type)
	evs := listEvents(t, srv, "R-8").Events
	require.Len(t, evs, 2)
	require.NotNil(t, evs[1].RevokedAt)
	assert.Equal(t, "2026-09-13T08:00:30Z", *evs[1].RevokedAt)
}

func TestRevokeStateSurvivesReopen(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "persist-revoke.db")

	st, err := store.Open(context.Background(), dbPath)
	require.NoError(t, err)
	srv := httptest.NewServer(app.NewRouter(st))
	createBatch(t, srv, "R-9", 10, "2026-09-13T08:00:00Z")
	postEvent(t, srv, "R-9", "takeout", "2026-09-13T08:00:05Z")
	postEvent(t, srv, "R-9", "return", "2026-09-13T08:00:16Z") // scraps
	revokeEvent(t, srv, "R-9", 2, "2026-09-13T08:00:30Z", "误扫归还")
	srv.Close()
	require.NoError(t, st.Close())

	st2, err := store.Open(context.Background(), dbPath)
	require.NoError(t, err)
	defer st2.Close()
	srv2 := httptest.NewServer(app.NewRouter(st2))
	defer srv2.Close()

	b := getBatch(t, srv2, "R-9")
	assert.Equal(t, "out", b.State)
	assert.Equal(t, "usable", b.Status)
	assert.Equal(t, int64(0), b.AccumulatedSeconds)
	require.NotNil(t, b.LastEvent)
	assert.Equal(t, "takeout", b.LastEvent.Type)
	evs := listEvents(t, srv2, "R-9").Events
	require.Len(t, evs, 2)
	require.NotNil(t, evs[1].RevokedAt)
	assert.Equal(t, "误扫归还", *evs[1].RevokeReason)
}

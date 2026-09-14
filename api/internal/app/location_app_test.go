package app_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// locationResp extends the shared batch response with the new cabinet-slot
// field. Older clients simply ignore it; this suite asserts the timing flow
// keeps working with the extra field present.
type locationResp struct {
	batchResp
	Location *string `json:"location"`
}

func doPutLocation(t *testing.T, baseURL, barcode, location string) (int, []byte) {
	t.Helper()
	raw, err := json.Marshal(map[string]string{"location": location})
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPut,
		fmt.Sprintf("%s/api/batches/%s/location", baseURL, barcode), bytes.NewReader(raw))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	return res.StatusCode, body
}

func doDeleteLocation(baseURL, barcode string) (int, []byte) {
	req, err := http.NewRequest(http.MethodDelete,
		fmt.Sprintf("%s/api/batches/%s/location", baseURL, barcode), nil)
	if err != nil {
		return -1, []byte(err.Error())
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return -1, []byte(err.Error())
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	return res.StatusCode, body
}

func decodeLocation(t *testing.T, code int, raw []byte) locationResp {
	t.Helper()
	var b locationResp
	require.Equal(t, http.StatusOK, code, string(raw))
	require.NoError(t, json.Unmarshal(raw, &b))
	return b
}

func TestPutLocationPlaceMoveAndVacate(t *testing.T) {
	srv := newServer(t)
	createBatch(t, srv, "LOC-1", 100, "2026-09-13T08:00:00Z")
	createBatch(t, srv, "LOC-2", 100, "2026-09-13T08:00:00Z")

	// Fresh batch: the added field is present as null and the legacy timing
	// semantics are untouched.
	fresh := getBatch(t, srv, "LOC-1")
	assert.Equal(t, "in", fresh.State)
	assert.Equal(t, int64(0), fresh.AccumulatedSeconds)

	code, raw := doPutLocation(t, srv.URL, "LOC-1", "A-01")
	b := decodeLocation(t, code, raw)
	require.NotNil(t, b.Location)
	assert.Equal(t, "A-01", *b.Location)

	// Refresh (plain GET) restores the real slot from the server.
	var reloaded locationResp
	code, raw = doJSON(t, http.MethodGet, srv.URL+"/api/batches/LOC-1", nil)
	require.Equal(t, http.StatusOK, code)
	require.NoError(t, json.Unmarshal(raw, &reloaded))
	assert.Equal(t, "A-01", *reloaded.Location)

	// Move LOC-1 to A-02: the old slot is released in the same transaction.
	code, raw = doPutLocation(t, srv.URL, "LOC-1", "A-02")
	b = decodeLocation(t, code, raw)
	assert.Equal(t, "A-02", *b.Location)

	// LOC-2 can now occupy the freed A-01.
	code, raw = doPutLocation(t, srv.URL, "LOC-2", "A-01")
	b2 := decodeLocation(t, code, raw)
	assert.Equal(t, "A-01", *b2.Location)

	// Manual 腾空.
	code, raw = doDeleteLocation(srv.URL, "LOC-1")
	b = decodeLocation(t, code, raw)
	assert.Nil(t, b.Location)
	code, raw = doDeleteLocation(srv.URL, "LOC-1")
	assert.Equal(t, http.StatusConflict, code)
	var e errResp
	require.NoError(t, json.Unmarshal(raw, &e))
	assert.Equal(t, "location_not_occupied", e.Error.Code)
}

func TestPutLocationConflicts(t *testing.T) {
	srv := newServer(t)
	createBatch(t, srv, "OC-1", 10, "2026-09-13T08:00:00Z")
	createBatch(t, srv, "OC-2", 100, "2026-09-13T08:00:00Z")

	// Empty / blank location.
	for _, loc := range []string{"", "   "} {
		code, raw := doPutLocation(t, srv.URL, "OC-2", loc)
		assert.Equal(t, http.StatusBadRequest, code, string(raw))
		var e errResp
		require.NoError(t, json.Unmarshal(raw, &e))
		assert.Equal(t, "location_required", e.Error.Code)
	}

	// Out-of-cabinet batch cannot be placed; the rejected attempt changes
	// nothing (still out, no slot, no extra event).
	postEvent(t, srv, "OC-1", "takeout", "2026-09-13T08:00:05Z")
	code, raw := doPutLocation(t, srv.URL, "OC-1", "A-01")
	assert.Equal(t, http.StatusConflict, code)
	var e errResp
	require.NoError(t, json.Unmarshal(raw, &e))
	assert.Equal(t, "batch_not_in_cabinet", e.Error.Code)

	// OC-2 owns A-01; the returned OC-1 (usable) cannot steal it, and after the
	// failed first placement OC-1 stays unplaced.
	code, _ = doPutLocation(t, srv.URL, "OC-2", "A-01")
	require.Equal(t, http.StatusOK, code)
	postEvent(t, srv, "OC-1", "return", "2026-09-13T08:00:10Z")
	code, raw = doPutLocation(t, srv.URL, "OC-1", "A-01")
	assert.Equal(t, http.StatusConflict, code)
	require.NoError(t, json.Unmarshal(raw, &e))
	assert.Equal(t, "location_occupied", e.Error.Code)
	oc1 := getBatch(t, srv, "OC-1")
	assert.Equal(t, int64(5), oc1.AccumulatedSeconds, "exposure is untouched")
	assert.Len(t, listEvents(t, srv, "OC-1").Events, 2, "no event written by the failed placement")

	// A failed move must keep the old slot.
	code, _ = doPutLocation(t, srv.URL, "OC-1", "B-07")
	require.Equal(t, http.StatusOK, code)
	code, raw = doPutLocation(t, srv.URL, "OC-1", "A-01")
	require.Equal(t, http.StatusConflict, code)
	var moved locationResp
	code, raw = doJSON(t, http.MethodGet, srv.URL+"/api/batches/OC-1", nil)
	require.NoError(t, json.Unmarshal(raw, &moved))
	assert.Equal(t, "B-07", *moved.Location)

	// Scrapped in-cabinet batch cannot occupy a slot.
	createBatch(t, srv, "OC-3", 10, "2026-09-13T09:00:00Z")
	postEvent(t, srv, "OC-3", "takeout", "2026-09-13T09:00:05Z")
	postEvent(t, srv, "OC-3", "return", "2026-09-13T09:00:16Z") // scraps
	code, raw = doPutLocation(t, srv.URL, "OC-3", "C-01")
	assert.Equal(t, http.StatusConflict, code)
	require.NoError(t, json.Unmarshal(raw, &e))
	assert.Equal(t, "batch_scrapped", e.Error.Code)

	// Unknown batch -> 404.
	code, _ = doPutLocation(t, srv.URL, "GHOST", "A-01")
	assert.Equal(t, http.StatusNotFound, code)
	code, _ = doDeleteLocation(srv.URL, "GHOST")
	assert.Equal(t, http.StatusNotFound, code)
}

func TestTakeoutReleasesLocationAndReturnStaysUnplaced(t *testing.T) {
	srv := newServer(t)
	createBatch(t, srv, "TR-1", 100, "2026-09-13T08:00:00Z")
	code, _ := doPutLocation(t, srv.URL, "TR-1", "A-01")
	require.Equal(t, http.StatusOK, code)

	// The takeout transaction releases the occupancy itself.
	c, b, _ := postEvent(t, srv, "TR-1", "takeout", "2026-09-13T08:00:05Z")
	require.Equal(t, http.StatusCreated, c)
	var out locationResp
	raw, _ := json.Marshal(b)
	require.NoError(t, json.Unmarshal(raw, &out))
	assert.Nil(t, out.Location)

	// Returning leaves the batch unplaced ("归还后保持未定位").
	c, rb, _ := postEvent(t, srv, "TR-1", "return", "2026-09-13T08:00:15Z")
	require.Equal(t, http.StatusCreated, c)
	raw, _ = json.Marshal(rb)
	require.NoError(t, json.Unmarshal(raw, &out))
	assert.Nil(t, out.Location)
	assert.Equal(t, int64(10), out.AccumulatedSeconds)

	// It can be shelved again afterwards.
	code, raw = doPutLocation(t, srv.URL, "TR-1", "A-09")
	require.Equal(t, http.StatusOK, code, string(raw))
	var back locationResp
	require.NoError(t, json.Unmarshal(raw, &back))
	assert.Equal(t, "A-09", *back.Location)
}

// TestConcurrentPutLocationSameSlotOnlyOneWins proves the concurrency
// requirement over real HTTP: two (or more) batches racing for one slot yield
// exactly one win and the rest get an explicit location_occupied conflict.
func TestConcurrentPutLocationSameSlotOnlyOneWins(t *testing.T) {
	srv := newServer(t)
	const n = 8
	for i := 1; i <= n; i++ {
		createBatch(t, srv, fmt.Sprintf("RC-%d", i), 100, "2026-09-13T08:00:00Z")
	}
	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req, _ := http.NewRequest(http.MethodPut,
				fmt.Sprintf("%s/api/batches/RC-%d/location", srv.URL, i+1),
				bytes.NewReader([]byte(`{"location":"Z-50"}`)))
			req.Header.Set("Content-Type", "application/json")
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				codes[i] = -1
				return
			}
			res.Body.Close()
			codes[i] = res.StatusCode
		}(i)
	}
	wg.Wait()

	wins, conflicts := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusOK:
			wins++
		case http.StatusConflict:
			conflicts++
		default:
			t.Fatalf("unexpected status %d", c)
		}
	}
	assert.Equal(t, 1, wins)
	assert.Equal(t, n-1, conflicts)
}

// TestAddedLocationFieldDoesNotAffectLegacyTimingFlow is the backwards-
// compatibility check: an old client that only understands the original
// fields still gets exactly the old create/takeout/return behaviour while the
// server additionally carries location (which it ignores).
func TestAddedLocationFieldDoesNotAffectLegacyTimingFlow(t *testing.T) {
	srv := newServer(t)
	code, raw := doJSON(t, http.MethodPost, srv.URL+"/api/batches", map[string]any{
		"barcode": "OLD-1", "allowedSeconds": 10, "createdAt": "2026-09-13T08:00:00Z",
	})
	require.Equal(t, http.StatusCreated, code)
	// Old client decodes only the fields it knows: the unknown "location"
	// key is ignored.
	var oldView batchResp
	require.NoError(t, json.Unmarshal(raw, &oldView))
	assert.Equal(t, "in", oldView.State)
	assert.Equal(t, "usable", oldView.Status)
	assert.Nil(t, oldView.LastEvent)
	// The new field is present on the wire for new clients.
	var keys map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &keys))
	_, hasLocation := keys["location"]
	assert.True(t, hasLocation, "new clients receive the current slot")

	code, _, _ = postEvent(t, srv, "OLD-1", "takeout", "2026-09-13T08:00:05Z")
	require.Equal(t, http.StatusCreated, code)
	code, rb, _ := postEvent(t, srv, "OLD-1", "return", "2026-09-13T08:00:15Z")
	require.Equal(t, http.StatusCreated, code)
	assert.Equal(t, int64(10), rb.AccumulatedSeconds)
	assert.Equal(t, "usable", rb.Status)
	assert.Nil(t, rb.Projection, "POST /events response stays projection-free")

	// Boundary + scrapped rules keep their original outcomes as well.
	createBatch(t, srv, "OLD-2", 10, "2026-09-13T09:00:00Z")
	code, _, e := postEvent(t, srv, "OLD-2", "return", "2026-09-13T09:00:05Z")
	assert.Equal(t, http.StatusConflict, code)
	assert.Equal(t, "invalid_transition", e.Error.Code)
}

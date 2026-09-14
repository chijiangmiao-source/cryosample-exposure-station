// Command verify runs the one-shot acceptance suite against a deployed
// timing-station stack (API + web). It exits 0 when every check passes,
// 1 otherwise. Configure targets with API_BASE / WEB_BASE.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

var (
	apiBase = envOr("API_BASE", "http://api:8080/api")
	webBase = envOr("WEB_BASE", "http://web:80")

	client = &http.Client{Timeout: 10 * time.Second}

	failures int
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func check(name string, ok bool, detail ...string) {
	if ok {
		fmt.Printf("PASS  %s\n", name)
		return
	}
	failures++
	fmt.Printf("FAIL  %s  %s\n", name, strings.Join(detail, " "))
}

type batch struct {
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
	Projection *projection `json:"projection"`
}

type projection struct {
	AsOf               string `json:"asOf"`
	AccumulatedSeconds int64  `json:"accumulatedSeconds"`
	RemainingSeconds   int64  `json:"remainingSeconds"`
	Usable             bool   `json:"usable"`
	ProjectedOverLimit bool   `json:"projectedOverLimit"`
	Settled            bool   `json:"settled"`
}

type event struct {
	ID           int64   `json:"id"`
	Type         string  `json:"type"`
	At           string  `json:"at"`
	DeltaSeconds *int64  `json:"deltaSeconds"`
	RevokedAt    *string `json:"revokedAt"`
	RevokeReason *string `json:"revokeReason"`
}

type apiErr struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func do(method, url string, body any) (int, []byte) {
	var rdr io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rdr = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, url, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := client.Do(req)
	if err != nil {
		return -1, []byte(err.Error())
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	return res.StatusCode, raw
}

func createBatch(barcode string, allowed int64, createdAt string) (int, batch, apiErr) {
	code, raw := do("POST", apiBase+"/batches", map[string]any{
		"barcode": barcode, "allowedSeconds": allowed, "createdAt": createdAt,
	})
	return decodeBatch(code, raw)
}

func postEvent(barcode, typ, at string) (int, batch, apiErr) {
	code, raw := do("POST", apiBase+"/batches/"+barcode+"/events", map[string]string{
		"type": typ, "at": at,
	})
	return decodeBatch(code, raw)
}

func revokeEvent(barcode string, id int64, at, reason string) (int, batch, apiErr) {
	code, raw := do("POST",
		fmt.Sprintf("%s/batches/%s/events/%d/revoke", apiBase, barcode, id),
		map[string]string{"at": at, "reason": reason})
	return decodeBatch(code, raw)
}

func listEvents(barcode string) (int, []event) {
	code, raw := do("GET", apiBase+"/batches/"+barcode+"/events", nil)
	var res struct {
		Events []event `json:"events"`
	}
	_ = json.Unmarshal(raw, &res)
	return code, res.Events
}

func decodeBatch(code int, raw []byte) (int, batch, apiErr) {
	var b batch
	var e apiErr
	if code == 200 || code == 201 {
		_ = json.Unmarshal(raw, &b)
	} else {
		_ = json.Unmarshal(raw, &e)
	}
	return code, b, e
}

func getBatch(barcode string) (int, batch) {
	code, raw := do("GET", apiBase+"/batches/"+barcode, nil)
	var b batch
	_ = json.Unmarshal(raw, &b)
	return code, b
}

// getBatchAsOf GETs a batch with the optional asOf projection parameter.
func getBatchAsOf(barcode, asOf string) (int, batch, apiErr) {
	code, raw := do("GET",
		apiBase+"/batches/"+barcode+"?asOf="+url.QueryEscape(asOf), nil)
	return decodeBatch(code, raw)
}

func eventCount(barcode string) int {
	code, raw := do("GET", apiBase+"/batches/"+barcode+"/events", nil)
	if code != 200 {
		return -1
	}
	var res struct {
		Events []json.RawMessage `json:"events"`
	}
	_ = json.Unmarshal(raw, &res)
	return len(res.Events)
}

func waitForAPI() {
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if code, _ := do("GET", apiBase+"/health", nil); code == 200 {
			return
		}
		time.Sleep(time.Second)
	}
	fmt.Println("FAIL  api did not become healthy within 60s")
	os.Exit(1)
}

func waitForWeb() (int, []byte) {
	deadline := time.Now().Add(30 * time.Second)
	for {
		code, raw := do("GET", webBase+"/", nil)
		if code == 200 {
			return code, raw
		}
		if time.Now().After(deadline) {
			return code, raw
		}
		time.Sleep(time.Second)
	}
}

func main() {
	fmt.Printf("verify: API_BASE=%s WEB_BASE=%s\n", apiBase, webBase)
	waitForAPI()

	// 0. Web UI is served.
	code, raw := waitForWeb()
	check("web serves the scan-station page", code == 200 && strings.Contains(string(raw), "id=\"app\""),
		fmt.Sprintf("status=%d", code))

	uniq := time.Now().UnixNano()

	// 1. Lifecycle with boundary return: accumulated == limit stays usable.
	b1 := fmt.Sprintf("VERIFY-A-%d", uniq)
	code, b, _ := createBatch(b1, 10, "2026-01-01T00:00:00Z")
	check("create batch (allowed=10s)", code == 201 && b.State == "in" && b.AccumulatedSeconds == 0 && b.Status == "usable",
		fmt.Sprintf("status=%d state=%s", code, b.State))

	code, _, e := createBatch(b1, 10, "2026-01-01T00:00:00Z")
	check("duplicate barcode -> 409", code == 409 && e.Error.Code == "duplicate_barcode", fmt.Sprintf("status=%d", code))

	code, _, e = postEvent(b1, "takeout", "2026-01-01T00:00:00Z")
	check("event time equal to previous -> 409", code == 409 && e.Error.Code == "time_not_monotonic", fmt.Sprintf("status=%d code=%s", code, e.Error.Code))

	code, _, e = postEvent(b1, "return", "2026-01-01T00:00:05Z")
	check("return without takeout -> 409", code == 409 && e.Error.Code == "invalid_transition", fmt.Sprintf("status=%d code=%s", code, e.Error.Code))

	code, b, _ = postEvent(b1, "takeout", "2026-01-01T00:00:05Z")
	check("takeout succeeds", code == 201 && b.State == "out", fmt.Sprintf("status=%d state=%s", code, b.State))

	code, _, e = postEvent(b1, "takeout", "2026-01-01T00:00:06Z")
	check("consecutive takeout -> 409", code == 409 && e.Error.Code == "invalid_transition", fmt.Sprintf("status=%d code=%s", code, e.Error.Code))

	code, _, e = postEvent(b1, "return", "2026-01-01T00:00:04Z")
	check("out-of-order return -> 409", code == 409 && e.Error.Code == "time_not_monotonic", fmt.Sprintf("status=%d code=%s", code, e.Error.Code))

	code, b, _ = postEvent(b1, "return", "2026-01-01T00:00:15Z")
	check("boundary return (== limit) stays usable",
		code == 201 && b.AccumulatedSeconds == 10 && b.RemainingSeconds == 0 && b.Status == "usable" && b.Usable,
		fmt.Sprintf("status=%d acc=%d batchStatus=%s", code, b.AccumulatedSeconds, b.Status))

	code, _, e = postEvent(b1, "return", "2026-01-01T00:00:16Z")
	check("duplicate return -> 409", code == 409 && e.Error.Code == "invalid_transition", fmt.Sprintf("status=%d code=%s", code, e.Error.Code))
	_, b = getBatch(b1)
	check("duplicate return did not count twice", b.AccumulatedSeconds == 10, fmt.Sprintf("acc=%d", b.AccumulatedSeconds))
	check("rejected events wrote nothing", eventCount(b1) == 2, fmt.Sprintf("events=%d", eventCount(b1)))

	// 2. Over-limit return scraps immediately and permanently.
	code, b, _ = postEvent(b1, "takeout", "2026-01-01T00:00:20Z")
	check("takeout at boundary still allowed", code == 201, fmt.Sprintf("status=%d", code))
	code, b, _ = postEvent(b1, "return", "2026-01-01T00:00:21Z")
	check("over-limit return scraps immediately",
		code == 201 && b.AccumulatedSeconds == 11 && b.Status == "scrapped" && !b.Usable,
		fmt.Sprintf("status=%d acc=%d batchStatus=%s", code, b.AccumulatedSeconds, b.Status))
	code, _, e = postEvent(b1, "takeout", "2026-01-01T00:00:30Z")
	check("scrapped batch cannot be taken out -> 409", code == 409 && e.Error.Code == "batch_scrapped", fmt.Sprintf("status=%d code=%s", code, e.Error.Code))

	// 3. Concurrency: the same old state succeeds at most once.
	b2 := fmt.Sprintf("VERIFY-B-%d", uniq)
	createBatch(b2, 1000, "2026-01-01T00:00:00Z")
	const racers = 8
	codes := make([]int, racers)
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c, _, _ := postEvent(b2, "takeout", "2026-01-01T00:00:05Z")
			codes[i] = c
		}(i)
	}
	wg.Wait()
	check("racing takeouts: exactly one wins", countCode(codes, 201) == 1 && countCode(codes, 409) == racers-1,
		fmt.Sprintf("codes=%v", codes))

	for i := range codes {
		codes[i] = 0
	}
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c, _, _ := postEvent(b2, "return", "2026-01-01T00:00:15Z")
			codes[i] = c
		}(i)
	}
	wg.Wait()
	check("racing returns: exactly one wins", countCode(codes, 201) == 1 && countCode(codes, 409) == racers-1,
		fmt.Sprintf("codes=%v", codes))
	_, b = getBatch(b2)
	check("racing returns counted exposure exactly once", b.AccumulatedSeconds == 10 && b.State == "in",
		fmt.Sprintf("acc=%d state=%s", b.AccumulatedSeconds, b.State))
	check("race wrote exactly two events", eventCount(b2) == 2, fmt.Sprintf("events=%d", eventCount(b2)))

	// 4. State is persisted and re-readable (what the page shows after refresh).
	_, b = getBatch(b1)
	check("final state of scrapped batch is re-readable",
		b.State == "in" && b.Status == "scrapped" && b.AccumulatedSeconds == 11 && b.LastEvent != nil && b.LastEvent.Type == "return",
		fmt.Sprintf("state=%s status=%s acc=%d", b.State, b.Status, b.AccumulatedSeconds))

	// 5. Headline undo: a mistaken return scrapped the batch; revoking it
	// restores out-of-cabinet and the previous accumulated total, then the
	// correct return can be recorded again.
	b3 := fmt.Sprintf("VERIFY-C-%d", uniq)
	code, b, _ = createBatch(b3, 10, "2026-01-01T00:00:00Z")
	check("revoke: create batch (allowed=10s)", code == 201, fmt.Sprintf("status=%d", code))
	code, b, _ = postEvent(b3, "takeout", "2026-01-01T00:00:05Z")
	check("revoke: takeout before mistaken return", code == 201 && b.State == "out", fmt.Sprintf("status=%d", code))
	takeoutID := b.LastEvent.ID
	code, b, _ = postEvent(b3, "return", "2026-01-01T00:00:16Z") // +11 > 10
	check("revoke: mistaken return scraps the batch",
		code == 201 && b.Status == "scrapped" && b.AccumulatedSeconds == 11,
		fmt.Sprintf("status=%d acc=%d status=%s", code, b.AccumulatedSeconds, b.Status))
	returnID := b.LastEvent.ID

	code, _, e = revokeEvent(b3, returnID+9000, "2026-01-01T00:00:20Z", "unknown id")
	check("revoke of an unknown event -> 404", code == 404 && e.Error.Code == "not_found",
		fmt.Sprintf("status=%d code=%s", code, e.Error.Code))
	code, _, e = revokeEvent(b3, takeoutID, "2026-01-01T00:00:20Z", "not the latest event")
	check("revoke of a non-latest event -> 409", code == 409 && e.Error.Code == "event_not_latest",
		fmt.Sprintf("status=%d code=%s", code, e.Error.Code))
	code, _, e = revokeEvent(b3, returnID, "2026-01-01T00:00:16Z", "not later than the last op")
	check("revocation time not later than last operation -> 409",
		code == 409 && e.Error.Code == "time_not_monotonic", fmt.Sprintf("status=%d code=%s", code, e.Error.Code))
	code, raw = do("POST", fmt.Sprintf("%s/batches/%s/events/%d/revoke", apiBase, b3, returnID),
		map[string]string{"at": "2026-01-01T00:00:20Z", "reason": "   "})
	_ = json.Unmarshal(raw, &e)
	check("revocation requires a non-empty reason -> 400", code == 400 && e.Error.Code == "reason_required",
		fmt.Sprintf("status=%d code=%s", code, e.Error.Code))

	code, b, _ = revokeEvent(b3, returnID, "2026-01-01T00:00:20Z", "误扫归还，样本实际仍在柜外")
	check("revoke mistaken return restores out state and original accumulated",
		code == 200 && b.State == "out" && b.Status == "usable" && b.Usable &&
			b.AccumulatedSeconds == 0 && b.RemainingSeconds == 10,
		fmt.Sprintf("status=%d state=%s status=%s acc=%d", code, b.State, b.Status, b.AccumulatedSeconds))
	check("revoke makes the takeout the last active event again",
		b.LastEvent != nil && b.LastEvent.ID == takeoutID && b.LastEvent.Type == "takeout" &&
			b.LastEvent.At == "2026-01-01T00:00:05Z",
		fmt.Sprintf("last=%v", b.LastEvent))

	code, evs := listEvents(b3)
	var revoked *event
	for i := range evs {
		if evs[i].ID == returnID {
			revoked = &evs[i]
		}
	}
	check("revoked return keeps its row with audit time and reason",
		code == 200 && len(evs) == 2 && revoked != nil &&
			revoked.RevokedAt != nil && *revoked.RevokedAt == "2026-01-01T00:00:20Z" &&
			revoked.RevokeReason != nil && *revoked.RevokeReason == "误扫归还，样本实际仍在柜外",
		fmt.Sprintf("events=%d revoked=%v", len(evs), revoked))

	code, _, e = revokeEvent(b3, returnID, "2026-01-01T00:00:25Z", "duplicate undo")
	check("duplicate revoke -> 409", code == 409 && e.Error.Code == "event_already_revoked",
		fmt.Sprintf("status=%d code=%s", code, e.Error.Code))

	code, b, _ = postEvent(b3, "return", "2026-01-01T00:00:15Z") // correct: +10 == limit
	check("correct return after undo is accepted and stays usable",
		code == 201 && b.State == "in" && b.AccumulatedSeconds == 10 && b.RemainingSeconds == 0 && b.Status == "usable",
		fmt.Sprintf("status=%d acc=%d status=%s", code, b.AccumulatedSeconds, b.Status))

	// 6. Revoking a mistaken takeout puts the batch back in the cabinet.
	b4 := fmt.Sprintf("VERIFY-D-%d", uniq)
	createBatch(b4, 100, "2026-01-01T00:00:00Z")
	code, b, _ = postEvent(b4, "takeout", "2026-01-01T00:00:10Z")
	check("revoke: mistaken takeout goes out", code == 201 && b.State == "out", fmt.Sprintf("status=%d", code))
	b4Takeout := b.LastEvent.ID
	code, b, _ = revokeEvent(b4, b4Takeout, "2026-01-01T00:00:20Z", "误扫取出")
	check("revoke takeout restores in-cabinet with zero exposure",
		code == 200 && b.State == "in" && b.AccumulatedSeconds == 0 && b.LastEvent == nil,
		fmt.Sprintf("status=%d state=%s acc=%d last=%v", code, b.State, b.AccumulatedSeconds, b.LastEvent))
	code, _, e = postEvent(b4, "takeout", "2026-01-01T00:00:00Z")
	check("last_at rewound to creation: equal time still rejected",
		code == 409 && e.Error.Code == "time_not_monotonic", fmt.Sprintf("status=%d code=%s", code, e.Error.Code))
	code, b, _ = postEvent(b4, "takeout", "2026-01-01T00:00:30Z")
	check("takeout can be recorded again at the correct time",
		code == 201 && b.State == "out" && b.LastEvent != nil && b.LastEvent.ID != b4Takeout,
		fmt.Sprintf("status=%d state=%s", code, b.State))

	// 7. Concurrency: two stations racing to revoke the same latest event —
	// exactly one wins, the exposure is deducted exactly once.
	b5 := fmt.Sprintf("VERIFY-E-%d", uniq)
	createBatch(b5, 100, "2026-01-01T00:00:00Z")
	postEvent(b5, "takeout", "2026-01-01T00:00:05Z")
	code, b, _ = postEvent(b5, "return", "2026-01-01T00:00:15Z") // +10, in cabinet
	b5Return := b.LastEvent.ID
	rcodes := make([]int, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rcodes[i], _, _ = revokeEvent(b5, b5Return, "2026-01-01T00:00:30Z", "racing undo")
		}(i)
	}
	wg.Wait()
	check("racing revokes: exactly one wins", countCode(rcodes, 200) == 1 && countCode(rcodes, 409) == racers-1,
		fmt.Sprintf("codes=%v", rcodes))
	_, b = getBatch(b5)
	check("racing revoke restored out state and deducted exposure once",
		b.State == "out" && b.AccumulatedSeconds == 0 && b.Status == "usable",
		fmt.Sprintf("state=%s acc=%d status=%s", b.State, b.AccumulatedSeconds, b.Status))
	code, evs = listEvents(b5)
	revokedCount := 0
	for _, ev := range evs {
		if ev.RevokedAt != nil {
			revokedCount++
		}
	}
	check("racing revoke stamped the audit row exactly once", code == 200 && len(evs) == 2 && revokedCount == 1,
		fmt.Sprintf("events=%d revoked=%d", len(evs), revokedCount))

	// 8. asOf advisory risk evaluation: projections grow with time and cross
	// the limit without writing events or scrapping the batch; only the real
	// return through the original endpoint settles the exposure.
	b6 := fmt.Sprintf("VERIFY-F-%d", uniq)
	code, b, _ = createBatch(b6, 10, "2026-01-01T00:00:00Z")
	check("asOf: create batch (allowed=10s)", code == 201 && b.State == "in", fmt.Sprintf("status=%d", code))

	// Legacy query: no projection key, unchanged response semantics.
	var legacyKeys map[string]json.RawMessage
	code, raw = do("GET", apiBase+"/batches/"+b6, nil)
	_ = json.Unmarshal(raw, &legacyKeys)
	_, hasProjectionKey := legacyKeys["projection"]
	check("query without asOf keeps the legacy response (no projection key)",
		code == 200 && !hasProjectionKey, fmt.Sprintf("status=%d", code))

	code, b, _ = postEvent(b6, "takeout", "2026-01-01T00:00:05Z")
	check("asOf: takeout before projections", code == 201 && b.State == "out", fmt.Sprintf("status=%d", code))

	code, b, _ = getBatchAsOf(b6, "2026-01-01T00:00:06Z")
	check("projection grows with asOf (1s projected, still usable)",
		code == 200 && b.Projection != nil &&
			b.Projection.AsOf == "2026-01-01T00:00:06Z" &&
			b.Projection.AccumulatedSeconds == 1 && b.Projection.RemainingSeconds == 9 &&
			b.Projection.Usable && !b.Projection.ProjectedOverLimit && !b.Projection.Settled &&
			b.AccumulatedSeconds == 0 && b.RemainingSeconds == 10 &&
			b.Status == "usable" && b.State == "out",
		fmt.Sprintf("status=%d proj=%v", code, b.Projection))

	code, b, _ = getBatchAsOf(b6, "2026-01-01T00:00:15Z")
	check("projection exactly at the limit is still usable",
		code == 200 && b.Projection.AccumulatedSeconds == 10 && b.Projection.RemainingSeconds == 0 &&
			b.Projection.Usable && !b.Projection.ProjectedOverLimit,
		fmt.Sprintf("status=%d proj=%v", code, b.Projection))

	code, b, _ = getBatchAsOf(b6, "2026-01-01T00:00:16Z")
	check("projection crosses the limit: 已预计超限 without scrapping the batch",
		code == 200 && b.Projection.AccumulatedSeconds == 11 && b.Projection.RemainingSeconds == -1 &&
			!b.Projection.Usable && b.Projection.ProjectedOverLimit && !b.Projection.Settled &&
			b.Status == "usable" && b.State == "out" && b.AccumulatedSeconds == 0,
		fmt.Sprintf("status=%d proj=%v status=%s", code, b.Projection, b.Status))

	check("evaluations wrote no events", eventCount(b6) == 1, fmt.Sprintf("events=%d", eventCount(b6)))

	code, _, e = getBatchAsOf(b6, "2026-01-01T00:00:04Z")
	check("asOf earlier than the last event -> 409 time_not_monotonic",
		code == 409 && e.Error.Code == "time_not_monotonic", fmt.Sprintf("status=%d code=%s", code, e.Error.Code))

	code, raw = do("GET", apiBase+"/batches/"+b6+"?asOf=2026-01-01T00:00:16", nil)
	_ = json.Unmarshal(raw, &e)
	check("malformed asOf -> 400 invalid_time", code == 400 && e.Error.Code == "invalid_time",
		fmt.Sprintf("status=%d code=%s", code, e.Error.Code))

	_, b = getBatch(b6)
	check("failed evaluations leave batch, totals and event log untouched",
		b.State == "out" && b.Status == "usable" && b.AccumulatedSeconds == 0 && b.Projection == nil &&
			eventCount(b6) == 1,
		fmt.Sprintf("state=%s status=%s acc=%d events=%d", b.State, b.Status, b.AccumulatedSeconds, eventCount(b6)))

	code, b, _ = postEvent(b6, "return", "2026-01-01T00:00:16Z")
	check("only the original return endpoint settles exposure and scraps",
		code == 201 && b.State == "in" && b.AccumulatedSeconds == 11 && b.Status == "scrapped" &&
			b.Projection == nil,
		fmt.Sprintf("status=%d acc=%d status=%s", code, b.AccumulatedSeconds, b.Status))
	check("return timing used the open takeout exactly once", eventCount(b6) == 2,
		fmt.Sprintf("events=%d", eventCount(b6)))

	code, b, _ = getBatchAsOf(b6, "2026-01-01T01:00:00Z")
	check("scrapped in-cabinet evaluation returns settled values far in the future",
		code == 200 && b.Projection != nil && b.Projection.Settled &&
			b.Projection.AccumulatedSeconds == 11 && b.Projection.RemainingSeconds == -1 &&
			!b.Projection.Usable && b.Projection.ProjectedOverLimit,
		fmt.Sprintf("status=%d proj=%v", code, b.Projection))

	// A usable in-cabinet batch returns time-independent settled figures.
	code, b, _ = getBatchAsOf(b2, "2026-01-01T00:05:00Z")
	check("usable in-cabinet evaluation is settled and time-independent",
		code == 200 && b.Projection != nil && b.Projection.Settled &&
			b.Projection.AccumulatedSeconds == 10 && b.Projection.RemainingSeconds == 990 &&
			b.Projection.Usable && !b.Projection.ProjectedOverLimit,
		fmt.Sprintf("status=%d proj=%v", code, b.Projection))

	code, _, _ = getBatchAsOf(fmt.Sprintf("NOPE-%d", uniq), "2026-01-01T00:00:00Z")
	check("unknown barcode with asOf -> 404 not_found", code == 404, fmt.Sprintf("status=%d", code))

	if failures > 0 {
		fmt.Printf("\nverify: %d check(s) FAILED\n", failures)
		os.Exit(1)
	}
	fmt.Println("\nverify: all checks passed")
}

func countCode(codes []int, want int) int {
	n := 0
	for _, c := range codes {
		if c == want {
			n++
		}
	}
	return n
}

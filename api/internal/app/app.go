// Package app wires the HTTP API (Gin) on top of the SQLite store.
package app

import (
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"timingstation/api/internal/store"
)

// Server holds the dependencies of the HTTP API.
type Server struct {
	st *store.Store
}

// NewRouter builds the Gin engine with all API routes under /api.
func NewRouter(st *store.Store) *gin.Engine {
	r := gin.New()
	r.Use(gin.Recovery(), cors())

	s := &Server{st: st}
	api := r.Group("/api")
	api.GET("/health", s.health)
	api.POST("/batches", s.createBatch)
	api.GET("/batches/:barcode", s.getBatch)
	api.GET("/batches/:barcode/events", s.listEvents)
	api.POST("/batches/:barcode/events", s.createEvent)
	api.POST("/batches/:barcode/events/:id/revoke", s.revokeEvent)
	api.PUT("/batches/:barcode/location", s.putLocation)
	api.DELETE("/batches/:barcode/location", s.deleteLocation)
	api.GET("/batches/:barcode/aliases", s.listAliases)
	api.POST("/batches/:barcode/aliases", s.bindAlias)
	api.DELETE("/batches/:barcode/aliases/:alias", s.unbindAlias)
	return r
}

func cors() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Content-Type")
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}

// zTimeRe enforces the canonical event-time format: RFC3339, whole seconds,
// UTC "Z" suffix — e.g. 2026-09-13T10:00:00Z. No offsets, no fractions.
var zTimeRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$`)

func parseEventTime(s string) (time.Time, error) {
	if !zTimeRe.MatchString(s) {
		return time.Time{}, errInvalidTime
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, errInvalidTime
	}
	return t.UTC(), nil
}

var errInvalidTime = errors.New("time must be RFC3339 whole seconds with Z suffix, e.g. 2026-09-13T10:00:00Z")

// ctxMatchedBarcode holds the code actually scanned when it was a bound
// alias; respondBatch surfaces it as the optional matchedBarcode field.
const ctxMatchedBarcode = "matchedBarcode"

// resolveCode maps a scanned code — either a primary barcode or a bound
// backup barcode — to the canonical batch barcode. It writes the error
// response itself (404 not_found, or 500) and returns ok=false when the code
// matches nothing. When an alias was scanned it records the hit code in the
// Gin context so the response can surface matchedBarcode while the batch
// payload keeps carrying the primary barcode.
func (s *Server) resolveCode(c *gin.Context, code string) (string, bool) {
	canonical, err := s.st.ResolveCode(c.Request.Context(), code)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(c, http.StatusNotFound, "not_found", "no batch with this barcode")
		return "", false
	}
	if err != nil {
		writeErr(c, http.StatusInternalServerError, "internal", err.Error())
		return "", false
	}
	if canonical != code {
		c.Set(ctxMatchedBarcode, code)
	}
	return canonical, true
}

func writeErr(c *gin.Context, status int, code, msg string) {
	c.AbortWithStatusJSON(status, gin.H{"error": gin.H{"code": code, "message": msg}})
}

func (s *Server) health(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

type createBatchReq struct {
	Barcode        string `json:"barcode"`
	AllowedSeconds int64  `json:"allowedSeconds"`
	CreatedAt      string `json:"createdAt"`
}

func (s *Server) createBatch(c *gin.Context) {
	var req createBatchReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeErr(c, http.StatusBadRequest, "bad_request", "invalid JSON body: "+err.Error())
		return
	}
	req.Barcode = strings.TrimSpace(req.Barcode)
	if req.Barcode == "" {
		writeErr(c, http.StatusBadRequest, "barcode_required", "barcode must be a non-empty string")
		return
	}
	if len(req.Barcode) > 128 {
		writeErr(c, http.StatusBadRequest, "barcode_too_long", "barcode must be at most 128 characters")
		return
	}
	if req.AllowedSeconds <= 0 {
		writeErr(c, http.StatusBadRequest, "invalid_allowed_seconds", "allowedSeconds must be a positive integer")
		return
	}
	createdAt, err := parseEventTime(req.CreatedAt)
	if err != nil {
		writeErr(c, http.StatusBadRequest, "invalid_time", err.Error())
		return
	}
	b, err := s.st.CreateBatch(c.Request.Context(), req.Barcode, req.AllowedSeconds, createdAt)
	if errors.Is(err, store.ErrDuplicate) {
		writeErr(c, http.StatusConflict, "duplicate_barcode", "a batch with this barcode already exists")
		return
	}
	if err != nil {
		writeErr(c, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.respondBatch(c, http.StatusCreated, b, nil)
}

func (s *Server) getBatch(c *gin.Context) {
	// Optional asOf projects the exposure totals at the given time without
	// persisting anything. Absent: the response keeps its old semantics
	// (settled values only). Present but malformed (including an empty
	// value) -> 400 invalid_time; earlier than the batch's last event ->
	// 409 time_not_monotonic.
	rawAsOf, hasAsOf := c.GetQuery("asOf")
	canonical, ok := s.resolveCode(c, c.Param("barcode"))
	if !ok {
		return
	}
	b, err := s.st.GetBatch(c.Request.Context(), canonical)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(c, http.StatusNotFound, "not_found", "no batch with this barcode")
		return
	}
	if err != nil {
		writeErr(c, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	var projection *store.Projection
	if hasAsOf {
		asOf, err := parseEventTime(rawAsOf)
		if err != nil {
			writeErr(c, http.StatusBadRequest, "invalid_time", err.Error())
			return
		}
		_, projection, err = s.st.EvaluateAt(c.Request.Context(), b.Barcode, asOf)
		var ce *store.ConflictError
		if errors.As(err, &ce) {
			writeErr(c, http.StatusConflict, ce.Code, ce.Message)
			return
		}
		if err != nil {
			writeErr(c, http.StatusInternalServerError, "internal", err.Error())
			return
		}
	}
	s.respondBatch(c, http.StatusOK, b, projection)
}

func (s *Server) listEvents(c *gin.Context) {
	canonical, ok := s.resolveCode(c, c.Param("barcode"))
	if !ok {
		return
	}
	events, err := s.st.ListEvents(c.Request.Context(), canonical)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(c, http.StatusNotFound, "not_found", "no batch with this barcode")
		return
	}
	if err != nil {
		writeErr(c, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	out := make([]eventJSON, 0, len(events))
	for _, ev := range events {
		out = append(out, toEventJSON(&ev))
	}
	// A primary-barcode scan keeps the legacy {events} envelope exactly; when a
	// backup barcode was scanned, the response additionally reports both the
	// canonical barcode and the code actually hit.
	resp := gin.H{"events": out}
	if hit, ok := c.Get(ctxMatchedBarcode); ok {
		resp["barcode"] = canonical
		resp["matchedBarcode"] = hit
	}
	c.JSON(http.StatusOK, resp)
}

type createEventReq struct {
	Type string `json:"type"`
	At   string `json:"at"`
}

func (s *Server) createEvent(c *gin.Context) {
	canonical, ok := s.resolveCode(c, c.Param("barcode"))
	if !ok {
		return
	}
	var req createEventReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeErr(c, http.StatusBadRequest, "bad_request", "invalid JSON body: "+err.Error())
		return
	}
	if req.Type != store.EventTakeout && req.Type != store.EventReturn {
		writeErr(c, http.StatusBadRequest, "invalid_event_type", "type must be \"takeout\" or \"return\"")
		return
	}
	at, err := parseEventTime(req.At)
	if err != nil {
		writeErr(c, http.StatusBadRequest, "invalid_time", err.Error())
		return
	}
	b, err := s.st.ApplyEvent(c.Request.Context(), canonical, req.Type, at)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(c, http.StatusNotFound, "not_found", "no batch with this barcode")
		return
	}
	var ce *store.ConflictError
	if errors.As(err, &ce) {
		writeErr(c, http.StatusConflict, ce.Code, ce.Message)
		return
	}
	if err != nil {
		writeErr(c, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.respondBatch(c, http.StatusCreated, b, nil)
}

type revokeEventReq struct {
	At     string `json:"at"`
	Reason string `json:"reason"`
}

// revokeEvent undoes the latest non-revoked event of the batch. See
// store.RevokeEvent for the transactional rules.
func (s *Server) revokeEvent(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(c, http.StatusBadRequest, "invalid_event_id", "event id must be a positive integer")
		return
	}
	canonical, ok := s.resolveCode(c, c.Param("barcode"))
	if !ok {
		return
	}
	var req revokeEventReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeErr(c, http.StatusBadRequest, "bad_request", "invalid JSON body: "+err.Error())
		return
	}
	req.Reason = strings.TrimSpace(req.Reason)
	if req.Reason == "" {
		writeErr(c, http.StatusBadRequest, "reason_required", "reason must be a non-empty string")
		return
	}
	at, err := parseEventTime(req.At)
	if err != nil {
		writeErr(c, http.StatusBadRequest, "invalid_time", err.Error())
		return
	}
	b, _, err := s.st.RevokeEvent(c.Request.Context(), canonical, id, at, req.Reason)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(c, http.StatusNotFound, "not_found", "no batch or event with this id")
		return
	}
	var ce *store.ConflictError
	if errors.As(err, &ce) {
		writeErr(c, http.StatusConflict, ce.Code, ce.Message)
		return
	}
	if err != nil {
		writeErr(c, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.respondBatch(c, http.StatusOK, b, nil)
}

// --- cabinet locations -------------------------------------------------------

type putLocationReq struct {
	Location string `json:"location"`
}

// putLocation scans a location code and places (or moves) an in-cabinet usable
// batch into that slot in one atomic transaction. See store.PutLocation for
// the conflict rules; a rejected request never changes the previous
// occupancy, the event log or the accumulated total.
func (s *Server) putLocation(c *gin.Context) {
	canonical, ok := s.resolveCode(c, c.Param("barcode"))
	if !ok {
		return
	}
	var req putLocationReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeErr(c, http.StatusBadRequest, "bad_request", "invalid JSON body: "+err.Error())
		return
	}
	req.Location = strings.TrimSpace(req.Location)
	if req.Location == "" {
		writeErr(c, http.StatusBadRequest, "location_required", "location must be a non-empty string")
		return
	}
	if len(req.Location) > 64 {
		writeErr(c, http.StatusBadRequest, "location_too_long", "location must be at most 64 characters")
		return
	}
	b, err := s.st.PutLocation(c.Request.Context(), canonical, req.Location)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(c, http.StatusNotFound, "not_found", "no batch with this barcode")
		return
	}
	var ce *store.ConflictError
	if errors.As(err, &ce) {
		writeErr(c, http.StatusConflict, ce.Code, ce.Message)
		return
	}
	if err != nil {
		writeErr(c, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.respondBatch(c, http.StatusOK, b, nil)
}

// deleteLocation vacates the batch's current slot without changing any event
// or exposure total.
func (s *Server) deleteLocation(c *gin.Context) {
	canonical, ok := s.resolveCode(c, c.Param("barcode"))
	if !ok {
		return
	}
	b, err := s.st.ReleaseLocation(c.Request.Context(), canonical)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(c, http.StatusNotFound, "not_found", "no batch with this barcode")
		return
	}
	var ce *store.ConflictError
	if errors.As(err, &ce) {
		writeErr(c, http.StatusConflict, ce.Code, ce.Message)
		return
	}
	if err != nil {
		writeErr(c, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.respondBatch(c, http.StatusOK, b, nil)
}

// --- backup barcodes (aliases) ------------------------------------------------

type bindAliasReq struct {
	Alias string `json:"alias"`
}

// listAliases returns the backup barcodes bound to the batch. A primary
// barcode or an alias in the path both resolve to the canonical batch.
func (s *Server) listAliases(c *gin.Context) {
	canonical, ok := s.resolveCode(c, c.Param("barcode"))
	if !ok {
		return
	}
	aliases, err := s.st.ListAliases(c.Request.Context(), canonical)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(c, http.StatusNotFound, "not_found", "no batch with this barcode")
		return
	}
	if err != nil {
		writeErr(c, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"barcode": canonical, "aliases": aliases})
}

// bindAlias binds a scanned backup barcode to the existing batch. See
// store.BindAlias for the conflict rules; the batch and the typed alias are
// left untouched on rejection.
func (s *Server) bindAlias(c *gin.Context) {
	canonical, ok := s.resolveCode(c, c.Param("barcode"))
	if !ok {
		return
	}
	var req bindAliasReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeErr(c, http.StatusBadRequest, "bad_request", "invalid JSON body: "+err.Error())
		return
	}
	req.Alias = strings.TrimSpace(req.Alias)
	if req.Alias == "" {
		writeErr(c, http.StatusBadRequest, "alias_required", "alias must be a non-empty string")
		return
	}
	if len(req.Alias) > 128 {
		writeErr(c, http.StatusBadRequest, "alias_too_long", "alias must be at most 128 characters")
		return
	}
	b, aliases, err := s.st.BindAlias(c.Request.Context(), canonical, req.Alias)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(c, http.StatusNotFound, "not_found", "no batch with this barcode")
		return
	}
	var ce *store.ConflictError
	if errors.As(err, &ce) {
		writeErr(c, http.StatusConflict, ce.Code, ce.Message)
		return
	}
	if err != nil {
		writeErr(c, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.respondBatchWithAliases(c, http.StatusOK, b, nil, aliases)
}

// unbindAlias removes a backup barcode that belongs to the batch. An alias
// that is missing, is a primary barcode, or belongs to another batch yields
// 409 alias_not_bound without touching events, exposure or occupancy.
func (s *Server) unbindAlias(c *gin.Context) {
	canonical, ok := s.resolveCode(c, c.Param("barcode"))
	if !ok {
		return
	}
	alias := strings.TrimSpace(c.Param("alias"))
	b, aliases, err := s.st.UnbindAlias(c.Request.Context(), canonical, alias)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(c, http.StatusNotFound, "not_found", "no batch with this barcode")
		return
	}
	var ce *store.ConflictError
	if errors.As(err, &ce) {
		writeErr(c, http.StatusConflict, ce.Code, ce.Message)
		return
	}
	if err != nil {
		writeErr(c, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.respondBatchWithAliases(c, http.StatusOK, b, nil, aliases)
}

// projectionJSON carries the non-persistent risk estimate included only when
// the query carried an asOf parameter.
type projectionJSON struct {
	AsOf               string `json:"asOf"`
	AccumulatedSeconds int64  `json:"accumulatedSeconds"`
	RemainingSeconds   int64  `json:"remainingSeconds"`
	Usable             bool   `json:"usable"`
	ProjectedOverLimit bool   `json:"projectedOverLimit"`
	Settled            bool   `json:"settled"`
}

// respondBatch renders the batch together with its latest event (if any). When
// projection is non-nil the response additionally carries the asOf evaluation;
// the batch's own settled fields are never altered by it. When the request
// scanned a bound alias, matchedBarcode (omitempty) reports the code actually
// scanned while Barcode stays the canonical primary barcode.
func (s *Server) respondBatch(c *gin.Context, status int, b *store.Batch, projection *store.Projection) {
	aliases, err := s.st.ListAliases(c.Request.Context(), b.Barcode)
	if err != nil {
		writeErr(c, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.respondBatchWithAliases(c, status, b, projection, aliases)
}

// respondBatchWithAliases is respondBatch with an already-loaded alias list
// (used by the bind/unbind endpoints that just mutated it).
func (s *Server) respondBatchWithAliases(c *gin.Context, status int, b *store.Batch, projection *store.Projection, aliases []string) {
	var last *eventJSON
	if ev, err := s.st.LastEvent(c.Request.Context(), b.Barcode); err == nil && ev != nil {
		e := toEventJSON(ev)
		last = &e
	}
	var proj *projectionJSON
	if projection != nil {
		proj = &projectionJSON{
			AsOf:               projection.AsOf.UTC().Format(time.RFC3339),
			AccumulatedSeconds: projection.AccumulatedSeconds,
			RemainingSeconds:   projection.RemainingSeconds,
			Usable:             projection.Usable,
			ProjectedOverLimit: !projection.Usable,
			Settled:            projection.Settled,
		}
	}
	if aliases == nil {
		aliases = []string{}
	}
	var matched string
	if hit, ok := c.Get(ctxMatchedBarcode); ok {
		matched, _ = hit.(string)
	}
	c.JSON(status, batchJSON{
		Barcode:            b.Barcode,
		MatchedBarcode:     matched,
		AllowedSeconds:     b.AllowedSeconds,
		State:              b.State,
		Status:             b.Status,
		Usable:             b.Status == store.StatusUsable,
		AccumulatedSeconds: b.AccumulatedSeconds,
		RemainingSeconds:   b.AllowedSeconds - b.AccumulatedSeconds,
		CreatedAt:          b.CreatedAt.UTC().Format(time.RFC3339),
		LastEvent:          last,
		Projection:         proj,
		Location:           b.Location,
		Aliases:            aliases,
	})
}

type eventJSON struct {
	ID           int64   `json:"id"`
	Type         string  `json:"type"`
	At           string  `json:"at"`
	DeltaSeconds *int64  `json:"deltaSeconds"`
	RevokedAt    *string `json:"revokedAt,omitempty"`
	RevokeReason *string `json:"revokeReason,omitempty"`
}

func toEventJSON(ev *store.Event) eventJSON {
	e := eventJSON{
		ID:           ev.ID,
		Type:         ev.Type,
		At:           ev.At.UTC().Format(time.RFC3339),
		DeltaSeconds: ev.DeltaSeconds,
		RevokeReason: ev.RevokeReason,
	}
	if ev.RevokedAt != nil {
		v := ev.RevokedAt.UTC().Format(time.RFC3339)
		e.RevokedAt = &v
	}
	return e
}

type batchJSON struct {
	Barcode string `json:"barcode"`
	// MatchedBarcode reports the code actually scanned when it was a bound
	// alias; it is omitted for a primary-barcode scan. Barcode always carries
	// the canonical primary barcode, so older clients keep working unchanged.
	MatchedBarcode     string          `json:"matchedBarcode,omitempty"`
	AllowedSeconds     int64           `json:"allowedSeconds"`
	State              string          `json:"state"`  // "in" | "out"
	Status             string          `json:"status"` // "usable" | "scrapped"
	Usable             bool            `json:"usable"` // status == "usable"
	AccumulatedSeconds int64           `json:"accumulatedSeconds"`
	RemainingSeconds   int64           `json:"remainingSeconds"` // allowedSeconds - accumulatedSeconds
	CreatedAt          string          `json:"createdAt"`
	LastEvent          *eventJSON      `json:"lastEvent"`
	Projection         *projectionJSON `json:"projection,omitempty"` // present only with ?asOf=...
	// Location is the cabinet slot currently occupied by the batch; nil when
	// the batch is unplaced (outside the cabinet / freshly returned / not yet
	// shelved). The added field is ignored by older clients.
	Location *string `json:"location"`
	// Aliases are the backup barcodes currently bound to the batch (empty
	// list when none). Additive like Location: old clients simply ignore it.
	Aliases []string `json:"aliases"`
}

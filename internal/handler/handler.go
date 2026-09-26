package handler

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"logchipper/internal/db"
)

type Handler struct {
	DB               *db.DB
	Broker           *Broker
	MaxQueryLimit    int // upper bound for ?limit= on GET /api/logs
	MaxStreamClients int // max concurrent /api/stream subscribers
}

func (h *Handler) IngestLog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var e db.Event
	if err := json.NewDecoder(r.Body).Decode(&e); err != nil {
		bodyError(w, err, "bad request")
		return
	}
	if e.Message == "" {
		http.Error(w, "message required", http.StatusBadRequest)
		return
	}
	if e.Level == "" {
		e.Level = "info"
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now()
	}

	id, err := h.DB.Insert(e)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	e.ID = id

	h.Broker.Publish(e)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]int64{"id": id})
}

func (h *Handler) IngestSyslog(w http.ResponseWriter, r *http.Request) {
	// Accepts plain-text POST
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	buf := new(strings.Builder)
	if _, err := io.Copy(buf, r.Body); err != nil {
		bodyError(w, err, "read error")
		return
	}

	source := r.Header.Get("X-Source")
	if source == "" {
		source = r.RemoteAddr
	}
	level := r.Header.Get("X-Level")
	if level == "" {
		level = "info"
	}

	e := db.Event{
		CreatedAt: time.Now(),
		Source:    source,
		Level:     level,
		Message:   strings.TrimSpace(buf.String()),
	}

	id, err := h.DB.Insert(e)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	e.ID = id
	h.Broker.Publish(e)

	fmt.Fprintln(w, id)
}

func (h *Handler) QueryLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	limit := 200
	if l := q.Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > h.MaxQueryLimit {
		limit = h.MaxQueryLimit
	}

	var since time.Time
	if s := q.Get("since"); s != "" {
		since, _ = time.Parse(time.RFC3339, s)
	}

	var until time.Time
	if s := q.Get("until"); s != "" {
		until, _ = time.Parse(time.RFC3339, s)
	}

	var afterID int64
	if a := q.Get("after_id"); a != "" {
		afterID, _ = strconv.ParseInt(a, 10, 64)
	}

	var beforeID int64
	if b := q.Get("before_id"); b != "" {
		beforeID, _ = strconv.ParseInt(b, 10, 64)
	}

	events, err := h.DB.Query(db.QueryParams{
		BeforeID: beforeID,
		Source:   q.Get("source"),
		Level:    q.Get("level"),
		Search:   q.Get("q"),
		Since:    since,
		Until:    until,
		Limit:    limit,
		AfterID:  afterID,
		Asc:      q.Get("order") == "asc",
	})
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(events)
}

// Histogram serves bucketed event counts for the graph view, e.g.
// GET /api/logs/histogram?since=<RFC3339>&bucket_seconds=1800&level=&source=&q=
func (h *Handler) Histogram(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	since := time.Now().Add(-48 * time.Hour)
	if s := q.Get("since"); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			since = t
		}
	}

	bucketSeconds := 1800
	if b := q.Get("bucket_seconds"); b != "" {
		if n, err := strconv.Atoi(b); err == nil && n > 0 {
			bucketSeconds = n
		}
	}
	if bucketSeconds < 10 {
		bucketSeconds = 10
	}

	buckets, err := h.DB.Histogram(db.HistogramParams{
		Source:        q.Get("source"),
		Level:         q.Get("level"),
		Search:        q.Get("q"),
		Since:         since,
		BucketSeconds: bucketSeconds,
	})
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"bucket_seconds": bucketSeconds,
		"since":          since.Format(time.RFC3339),
		"buckets":        buckets,
	})
}

func (h *Handler) Sources(w http.ResponseWriter, r *http.Request) {
	sources, err := h.DB.Sources()
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(sources)
}

func (h *Handler) Stream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	ch := h.Broker.TrySubscribe(h.MaxStreamClients)
	if ch == nil {
		http.Error(w, "too many stream clients", http.StatusServiceUnavailable)
		return
	}
	defer h.Broker.Unsubscribe(ch)

	// Send headers now — Go holds them until the first write, so without this
	// the browser's EventSource wouldn't fire onopen until the first event or
	// the 15s heartbeat.
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	// Heartbeat ticker
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case data, ok := <-ch:
			if !ok {
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		case <-ticker.C:
			fmt.Fprintf(w, ": heartbeat\n\n")
			flusher.Flush()
		}
	}
}

func (h *Handler) DBSize(w http.ResponseWriter, r *http.Request) {
	n, err := h.DB.Size()
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	events, err := h.DB.Count()
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]int64{"bytes": n, "events": events})
}

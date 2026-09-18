package shortener

import (
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Server wires the store to HTTP: a JSON API, redirects, and a small page.
type Server struct {
	store   *Store
	baseURL string
	limiter *rateLimiter
	tmpl    *template.Template
	mux     *http.ServeMux
}

func NewServer(store *Store, baseURL string, page string) *Server {
	s := &Server{
		store:   store,
		baseURL: strings.TrimRight(baseURL, "/"),
		limiter: newRateLimiter(30, time.Minute),
		tmpl:    template.Must(template.New("index").Parse(page)),
		mux:     http.NewServeMux(),
	}
	s.routes()
	return s
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /health", s.handleHealth)
	s.mux.HandleFunc("GET /api/stats", s.handleStats)
	s.mux.HandleFunc("GET /api/links", s.handleList)
	s.mux.HandleFunc("POST /api/links", s.handleCreate)
	s.mux.HandleFunc("GET /api/links/{code}", s.handleGet)
	s.mux.HandleFunc("DELETE /api/links/{code}", s.handleDelete)
	s.mux.HandleFunc("GET /{$}", s.handleIndex)
	s.mux.HandleFunc("GET /{code}", s.handleRedirect)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	s.mux.ServeHTTP(recorder, r)
	log.Printf("%s %s -> %d in %s", r.Method, r.URL.Path, recorder.status, time.Since(start).Round(time.Microsecond))
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	stats, err := s.store.Stats()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

type createRequest struct {
	URL       string `json:"url"`
	Code      string `json:"code,omitempty"`
	ExpiresIn string `json:"expires_in,omitempty"` // e.g. "24h", "30m"
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	if !s.limiter.allow(clientIP(r)) {
		writeError(w, http.StatusTooManyRequests, errors.New("too many links from this address, slow down"))
		return
	}
	var body createRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, errors.New("expected a JSON body with a url field"))
		return
	}
	var ttl time.Duration
	if body.ExpiresIn != "" {
		parsed, err := time.ParseDuration(body.ExpiresIn)
		if err != nil || parsed <= 0 {
			writeError(w, http.StatusUnprocessableEntity, errors.New("expires_in must be a duration like 24h"))
			return
		}
		ttl = parsed
	}
	link, err := s.store.Create(body.URL, body.Code, ttl)
	switch {
	case errors.Is(err, ErrBadURL), errors.Is(err, ErrBadCode):
		writeError(w, http.StatusUnprocessableEntity, err)
	case errors.Is(err, ErrCodeTaken):
		writeError(w, http.StatusConflict, err)
	case err != nil:
		writeError(w, http.StatusInternalServerError, err)
	default:
		writeJSON(w, http.StatusCreated, s.decorate(link))
	}
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	link, err := s.store.Get(r.PathValue("code"))
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, s.decorate(link))
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	links, err := s.store.List(limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]map[string]any, 0, len(links))
	for _, link := range links {
		out = append(out, s.decorate(link))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	err := s.store.Delete(r.PathValue("code"))
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRedirect(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")
	if code == "favicon.ico" {
		http.NotFound(w, r)
		return
	}
	link, err := s.store.Resolve(code)
	switch {
	case errors.Is(err, ErrNotFound):
		http.Error(w, "no such link", http.StatusNotFound)
	case errors.Is(err, ErrExpired):
		http.Error(w, "this link has expired", http.StatusGone)
	case err != nil:
		http.Error(w, "something went wrong", http.StatusInternalServerError)
	default:
		// 302, not 301: a permanent redirect is cached by browsers for ever, and
		// then the visit counter never sees the hit again.
		http.Redirect(w, r, link.Target, http.StatusFound)
	}
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	links, err := s.store.List(10)
	if err != nil {
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}
	rows := make([]map[string]any, 0, len(links))
	for _, link := range links {
		rows = append(rows, s.decorate(link))
	}
	w.Header().Set("content-type", "text/html; charset=utf-8")
	if err := s.tmpl.Execute(w, map[string]any{"Links": rows, "BaseURL": s.baseURL}); err != nil {
		log.Printf("template: %v", err)
	}
}

func (s *Server) decorate(link *Link) map[string]any {
	return map[string]any{
		"code": link.Code, "target": link.Target, "short_url": fmt.Sprintf("%s/%s", s.baseURL, link.Code),
		"created_at": link.CreatedAt, "expires_at": link.ExpiresAt,
		"visits": link.Visits, "last_visit": link.LastVisit,
	}
}

func clientIP(r *http.Request) string {
	if forwarded := r.Header.Get("x-forwarded-for"); forwarded != "" {
		return strings.TrimSpace(strings.Split(forwarded, ",")[0])
	}
	host, _, found := strings.Cut(r.RemoteAddr, ":")
	if !found {
		return r.RemoteAddr
	}
	return host
}

// rateLimiter is a fixed-window counter per client. Enough to stop one script
// filling the database; a real deployment would put this at the edge.
type rateLimiter struct {
	mu       sync.Mutex
	limit    int
	window   time.Duration
	counters map[string]*counter
}

type counter struct {
	count int
	until time.Time
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{limit: limit, window: window, counters: map[string]*counter{}}
}

func (l *rateLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	c, ok := l.counters[key]
	if !ok || now.After(c.until) {
		l.counters[key] = &counter{count: 1, until: now.Add(l.window)}
		return true
	}
	if c.count >= l.limit {
		return false
	}
	c.count++
	return true
}

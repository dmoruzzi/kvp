// Package server implements the HTTP router, handlers and middleware for the
// KVP store (spec §5–§7).
package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/dmoruzzi/kvp/internal/store"
)

// Options configures the server. Zero values fall back to spec defaults.
type Options struct {
	MaxBodyBytes   int64
	MaxKeyBytes    int
	TTL            time.Duration
	APIKey         string
	RateLimitRPS   float64
	RateLimitBurst int
	TrustedProxies []netip.Prefix
	UI             fs.FS
	Logger         *slog.Logger
	Metrics        Metrics
	// AfterWrite is invoked (asynchronously) after a successful store; wired to
	// size-based eviction by the cleanup package.
	AfterWrite func()
	// Wrap is an optional middleware applied last, before the router
	// (e.g. otelhttp in production).
	Wrap func(http.Handler) http.Handler
}

// Server holds the handler state.
type Server struct {
	store *store.Store
	opts  Options
	rl    *rateLimiter
}

func newServer(st *store.Store, o Options) *Server {
	if o.MaxBodyBytes <= 0 {
		o.MaxBodyBytes = 1048576
	}
	if o.MaxKeyBytes <= 0 {
		o.MaxKeyBytes = 256
	}
	if o.TTL <= 0 {
		o.TTL = 24 * time.Hour
	}
	if o.RateLimitRPS <= 0 {
		o.RateLimitRPS = 10
	}
	if o.RateLimitBurst <= 0 {
		o.RateLimitBurst = 20
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Metrics == nil {
		o.Metrics = noopMetrics{}
	}
	return &Server{
		store: st,
		opts:  o,
		rl:    newRateLimiter(o.RateLimitRPS, o.RateLimitBurst),
	}
}

// New builds the full middleware chain wrapping the router.
func New(st *store.Store, o Options) http.Handler {
	return newServer(st, o).handler()
}

// handler composes middleware in the spec order (§7):
// recovery → request-id → access log → rate limit → auth → headers → otelhttp → router.
func (s *Server) handler() http.Handler {
	var h http.Handler = http.HandlerFunc(s.route)
	h = s.securityHeaders(h)
	h = s.auth(h)
	h = s.rateLimit(h)
	h = s.accessLog(h)
	h = s.requestID(h)
	h = s.recoverMiddleware(h)
	if s.opts.Wrap != nil {
		h = s.opts.Wrap(h)
	}
	return h
}

func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/", "/index.html", "/style.css", "/app.js":
		if r.Method != http.MethodGet {
			s.methodNotAllowed(w, http.MethodGet)
			return
		}
		s.serveUI(w, r, r.URL.Path)
		return
	}
	s.serveKeyDirect(w, r, r.URL.Path[1:])
}

// routeForPath maps a URL path to its route template. Raw key paths map to
// "kvp"; the UI paths map to "index".
func routeForPath(path string) string {
	switch path {
	case "/", "/index.html", "/style.css", "/app.js":
		return "index"
	}
	return "kvp"
}

func (s *Server) serveKeyDirect(w http.ResponseWriter, r *http.Request, key string) {
	if key == "" {
		s.writeError(w, http.StatusBadRequest, "key required")
		return
	}
	if len(key) > s.opts.MaxKeyBytes {
		s.writeError(w, http.StatusBadRequest, "key too long")
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.serveGet(w, r, key)
	case http.MethodPost:
		s.servePost(w, r, key)
	default:
		s.methodNotAllowed(w, http.MethodGet, http.MethodPost)
	}
}

func (s *Server) serveUI(w http.ResponseWriter, r *http.Request, name string) {
	file, ctype := "index.html", "text/html; charset=utf-8"
	switch {
	case strings.HasSuffix(name, ".js"):
		file, ctype = "app.js", "application/javascript; charset=utf-8"
	case strings.HasSuffix(name, ".css"):
		file, ctype = "style.css", "text/css; charset=utf-8"
	}
	f, err := s.opts.UI.Open(file)
	if err != nil {
		s.internalError(w, r, "open ui", err)
		return
	}
	defer func() { _ = f.Close() }()
	w.Header().Set("Content-Type", ctype)
	if _, err := io.Copy(w, f); err != nil {
		s.internalError(w, r, "serve ui", err)
	}
}

func (s *Server) serveGet(w http.ResponseWriter, r *http.Request, key string) {
	start := time.Now()
	res, err := s.store.Get(r.Context(), key)
	s.opts.Metrics.DBQuery("get", time.Since(start))
	if err != nil {
		s.internalError(w, r, "get", err)
		return
	}
	if !res.Found {
		s.writeError(w, http.StatusNotFound, "key not found")
		return
	}
	if res.Expired {
		s.opts.Metrics.KeyExpired()
		s.writeError(w, http.StatusNotFound, "key expired")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(res.Value)
}

func (s *Server) servePost(w http.ResponseWriter, r *http.Request, key string) {
	r.Body = http.MaxBytesReader(w, r.Body, s.opts.MaxBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			s.writeError(w, http.StatusRequestEntityTooLarge, "payload too large")
			return
		}
		s.writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	start := time.Now()
	err = s.store.Put(r.Context(), key, body, s.opts.TTL)
	s.opts.Metrics.DBQuery("put", time.Since(start))
	if err != nil {
		s.internalError(w, r, "put", err)
		return
	}
	s.opts.Metrics.KeyStored()

	if s.opts.AfterWrite != nil {
		go s.opts.AfterWrite()
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write([]byte("stored"))
}

func (s *Server) internalError(w http.ResponseWriter, r *http.Request, op string, err error) {
	s.opts.Logger.Error("internal error",
		"op", op,
		"error", err,
		"request_id", requestIDFrom(r.Context()),
		"route", routeForPath(r.URL.Path),
	)
	s.writeError(w, http.StatusInternalServerError, "internal error")
}

func (s *Server) methodNotAllowed(w http.ResponseWriter, methods ...string) {
	w.Header().Set("Allow", joinMethods(methods))
	s.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
}

func joinMethods(methods []string) string {
	out := ""
	for i, m := range methods {
		if i > 0 {
			out += ", "
		}
		out += m
	}
	return out
}

// writeError writes a JSON error response per the spec §5.4.
func (s *Server) writeError(w http.ResponseWriter, status int, msg string) {
	b, _ := json.Marshal(map[string]string{"error": msg})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

type ctxKey int

const ctxKeyRequestID ctxKey = iota

func withRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKeyRequestID, id)
}

func requestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(ctxKeyRequestID).(string)
	return id
}

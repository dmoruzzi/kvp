package server

import (
	"context"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/dmoruzzi/kvp/internal/store"
)

const testIndex = `<!doctype html><html><head><title>kvp</title></head><body>kvp ui</body></html>`

var testIndexFS fs.FS = fstest.MapFS{
	"index.html": &fstest.MapFile{Data: []byte(testIndex)},
	"style.css":  &fstest.MapFile{Data: []byte(`body{background:#000}`)},
	"app.js":     &fstest.MapFile{Data: []byte(`console.log("kvp")`)},
}

func newTestServer(t *testing.T, o Options) (http.Handler, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "kvp.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if o.UI == nil {
		o.UI = testIndexFS
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.NewTextHandler(discard{}, nil))
	}
	h := newServer(st, o).handler()
	return h, st
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

func doReq(t *testing.T, h http.Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *strings.Reader
	if body == "" {
		rdr = strings.NewReader("")
	} else {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func body(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	return rr.Body.String()
}

func TestGetIndexServesHTML(t *testing.T) {
	h, _ := newTestServer(t, Options{})
	for _, path := range []string{"/", "/index.html"} {
		rr := doReq(t, h, "GET", path, "", nil)
		if rr.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, rr.Code)
		}
		if body(t, rr) != testIndex {
			t.Errorf("GET %s body = %q, want index", path, body(t, rr))
		}
		if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("GET %s Content-Type = %q, want text/html", path, ct)
		}
	}
}

func TestGetAppJS(t *testing.T) {
	h, _ := newTestServer(t, Options{})
	rr := doReq(t, h, "GET", "/app.js", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /app.js = %d, want 200", rr.Code)
	}
	if got := body(t, rr); got != `console.log("kvp")` {
		t.Errorf("GET /app.js body = %q", got)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/javascript") {
		t.Errorf("GET /app.js Content-Type = %q, want application/javascript", ct)
	}
}

func TestIndexRejectsNonGET(t *testing.T) {
	h, _ := newTestServer(t, Options{})
	rr := doReq(t, h, "POST", "/", "x", nil)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST / = %d, want 405", rr.Code)
	}
	if allow := rr.Header().Get("Allow"); allow != "GET" {
		t.Errorf("POST / Allow = %q, want GET", allow)
	}
}

func TestPostAndGet(t *testing.T) {
	h, _ := newTestServer(t, Options{})
	rr := doReq(t, h, "POST", "/foo", "hello world", nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("POST = %d, want 201", rr.Code)
	}
	if body(t, rr) != "stored" {
		t.Errorf("POST body = %q, want stored", body(t, rr))
	}

	rr = doReq(t, h, "GET", "/foo", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200", rr.Code)
	}
	if body(t, rr) != "hello world" {
		t.Errorf("GET body = %q, want %q", body(t, rr), "hello world")
	}
}

func TestPostBinaryAndEmptyValue(t *testing.T) {
	h, _ := newTestServer(t, Options{})
	rr := doReq(t, h, "POST", "/bin", string([]byte{0x00, 0x01, 0xFF}), nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("POST = %d, want 201", rr.Code)
	}
	rr = doReq(t, h, "GET", "/bin", "", nil)
	if body(t, rr) != string([]byte{0x00, 0x01, 0xFF}) {
		t.Errorf("GET binary body = %q", body(t, rr))
	}

	// Empty value round-trips.
	if rr := doReq(t, h, "POST", "/empty", "", nil); rr.Code != http.StatusCreated {
		t.Errorf("POST /empty = %d, want 201", rr.Code)
	}
	if rr := doReq(t, h, "GET", "/empty", "", nil); rr.Code != http.StatusOK || body(t, rr) != "" {
		t.Errorf("GET /empty = %d body %q, want 200 empty", rr.Code, body(t, rr))
	}
}

func TestPostOversizeBody(t *testing.T) {
	h, _ := newTestServer(t, Options{MaxBodyBytes: 4})
	rr := doReq(t, h, "POST", "/big", "12345", nil)
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("POST = %d, want 413", rr.Code)
	}
	if body(t, rr) != `{"error":"payload too large"}` {
		t.Errorf("body = %q", body(t, rr))
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

func TestGetMissingKey(t *testing.T) {
	h, _ := newTestServer(t, Options{})
	rr := doReq(t, h, "GET", "/missing", "", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("GET = %d, want 404", rr.Code)
	}
	if body(t, rr) != `{"error":"key not found"}` {
		t.Errorf("body = %q", body(t, rr))
	}
}

func TestGetExpiredLazyDelete(t *testing.T) {
	h, st := newTestServer(t, Options{TTL: time.Millisecond})
	if rr := doReq(t, h, "POST", "/k", "v", nil); rr.Code != http.StatusCreated {
		t.Fatalf("POST = %d, want 201", rr.Code)
	}
	time.Sleep(20 * time.Millisecond)
	rr := doReq(t, h, "GET", "/k", "", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("GET = %d, want 404", rr.Code)
	}
	if body(t, rr) != `{"error":"key expired"}` {
		t.Errorf("body = %q", body(t, rr))
	}
	rows, err := st.RowCount(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Errorf("RowCount after lazy delete = %d, want 0", rows)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	h, _ := newTestServer(t, Options{})
	for _, method := range []string{"PUT", "DELETE", "PATCH"} {
		rr := doReq(t, h, method, "/foo", "x", nil)
		if rr.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /foo = %d, want 405", method, rr.Code)
		}
		if allow := rr.Header().Get("Allow"); allow != "GET, POST" {
			t.Errorf("%s Allow = %q, want \"GET, POST\"", method, allow)
		}
	}
}

func TestEmptyKeyBadRequest(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "kvp.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()
	s := newServer(st, Options{Logger: slog.New(slog.NewTextHandler(discard{}, nil))})
	req := httptest.NewRequest("GET", "/", nil)
	rr := httptest.NewRecorder()
	s.serveKeyDirect(rr, req, "")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("empty key = %d, want 400", rr.Code)
	}
	if body(t, rr) != `{"error":"key required"}` {
		t.Errorf("body = %q", body(t, rr))
	}
}

func TestKeyTooLong(t *testing.T) {
	h, _ := newTestServer(t, Options{MaxKeyBytes: 5})
	rr := doReq(t, h, "POST", "/toolongkey", "v", nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("POST = %d, want 400", rr.Code)
	}
	if body(t, rr) != `{"error":"key too long"}` {
		t.Errorf("body = %q", body(t, rr))
	}
}

func TestAuthRequired(t *testing.T) {
	h, _ := newTestServer(t, Options{APIKey: "secret"})
	cases := []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{"no key", nil, http.StatusUnauthorized},
		{"wrong key", map[string]string{"X-API-Key": "wrong"}, http.StatusUnauthorized},
		{"right key", map[string]string{"X-API-Key": "secret"}, http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := doReq(t, h, "GET", "/authkey", "", tc.headers)
			if rr.Code != tc.want {
				t.Errorf("GET = %d, want %d", rr.Code, tc.want)
			}
		})
	}
	if rr := doReq(t, h, "GET", "/authkey", "", map[string]string{"X-API-Key": "wrong"}); body(t, rr) != `{"error":"unauthorized"}` {
		t.Errorf("unauthorized body = %q", body(t, rr))
	}
}

func TestUIPublicWhenAuthConfigured(t *testing.T) {
	h, _ := newTestServer(t, Options{APIKey: "secret"})
	for _, path := range []string{"/", "/index.html", "/style.css", "/app.js"} {
		rr := doReq(t, h, "GET", path, "", nil)
		if rr.Code != http.StatusOK {
			t.Errorf("GET %s without key = %d, want 200", path, rr.Code)
		}
	}
	if rr := doReq(t, h, "POST", "/", "x", nil); rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST / without key = %d, want 405 (UI route, not auth)", rr.Code)
	}
	if rr := doReq(t, h, "GET", "/datakey", "", nil); rr.Code != http.StatusUnauthorized {
		t.Errorf("GET /datakey without key = %d, want 401 (data still protected)", rr.Code)
	}
}

func TestAuthDisabledOpen(t *testing.T) {
	h, _ := newTestServer(t, Options{})
	if rr := doReq(t, h, "GET", "/open", "", nil); rr.Code != http.StatusNotFound {
		t.Errorf("GET without auth = %d, want 404 (open access)", rr.Code)
	}
	if rr := doReq(t, h, "POST", "/open", "v", nil); rr.Code != http.StatusCreated {
		t.Errorf("POST without auth = %d, want 201", rr.Code)
	}
}

func TestRateLimit(t *testing.T) {
	h, _ := newTestServer(t, Options{RateLimitRPS: 1, RateLimitBurst: 1})
	if rr := doReq(t, h, "GET", "/rl", "", nil); rr.Code != http.StatusNotFound {
		t.Fatalf("first GET = %d, want 404 (not rate limited)", rr.Code)
	}
	rr := doReq(t, h, "GET", "/rl", "", nil)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("second GET = %d, want 429", rr.Code)
	}
	if body(t, rr) != `{"error":"rate limited"}` {
		t.Errorf("body = %q", body(t, rr))
	}
	if ra := rr.Header().Get("Retry-After"); ra != "1" {
		t.Errorf("Retry-After = %q, want 1", ra)
	}
}

func TestTrustedProxyClientIP(t *testing.T) {
	h, _ := newTestServer(t, Options{RateLimitRPS: 1, RateLimitBurst: 1})
	headers := map[string]string{
		"X-Forwarded-For": "203.0.113.9",
		"CF-Connecting-IP": "198.51.100.7",
	}
	// Untrusted proxies: forwarded headers ignored → all requests keyed on peer IP.
	if rr := doReq(t, h, "GET", "/x", "", headers); rr.Code != http.StatusNotFound {
		t.Fatalf("first = %d, want 404", rr.Code)
	}
	if rr := doReq(t, h, "GET", "/x", "", headers); rr.Code != http.StatusTooManyRequests {
		t.Errorf("second (untrusted) = %d, want 429", rr.Code)
	}
}

func TestSecurityHeaders(t *testing.T) {
	h, _ := newTestServer(t, Options{})
	rr := doReq(t, h, "GET", "/", "", nil)
	if got := rr.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := rr.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("X-Frame-Options = %q, want DENY", got)
	}
	if got := rr.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("Referrer-Policy = %q, want no-referrer", got)
	}
	if got := rr.Header().Get("Content-Security-Policy"); got != "default-src 'self'" {
		t.Errorf("Content-Security-Policy = %q, want default-src 'self'", got)
	}
}

func TestRequestIDEcho(t *testing.T) {
	h, _ := newTestServer(t, Options{})
	rr := doReq(t, h, "GET", "/rid", "", nil)
	id := rr.Header().Get("X-Request-ID")
	if id == "" {
		t.Fatal("X-Request-ID not set on response")
	}
	// Client-provided ID is echoed verbatim.
	rr = doReq(t, h, "GET", "/rid", "", map[string]string{"X-Request-ID": "client-id-42"})
	if got := rr.Header().Get("X-Request-ID"); got != "client-id-42" {
		t.Errorf("echoed X-Request-ID = %q, want client-id-42", got)
	}
}

func TestMetricsRouteTemplates(t *testing.T) {
	rec := &recordingMetrics{}
	h, _ := newTestServer(t, Options{Metrics: rec})
	doReq(t, h, "GET", "/", "", nil)
	doReq(t, h, "POST", "/some-key", "v", nil)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.requests) < 2 {
		t.Fatalf("recorded %d requests, want >= 2", len(rec.requests))
	}
	indexSeen, kvpSeen := false, false
	for _, e := range rec.requests {
		if e.route == "index" && e.path == "/" && e.status == "200" && e.method == "GET" {
			indexSeen = true
		}
		if e.route == "kvp" && e.path == "/some-key" && e.status == "201" && e.method == "POST" {
			kvpSeen = true
		}
	}
	if !indexSeen {
		t.Error("missing index route metric")
	}
	if !kvpSeen {
		t.Error("missing kvp route metric with raw key path /some-key")
	}
}

func TestPanicRecovery(t *testing.T) {
	// The only panic point reachable is via a custom wrap. Exercise the
	// recovery middleware directly.
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})
	s := newServer(nil, Options{Logger: slog.New(slog.NewTextHandler(discard{}, nil))})
	recovered := s.recoverMiddleware(inner)
	req := httptest.NewRequest("GET", "/", nil)
	rr := httptest.NewRecorder()
	recovered.ServeHTTP(rr, req)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("panicked handler = %d, want 500", rr.Code)
	}
	if body(t, rr) != `{"error":"internal error"}` {
		t.Errorf("body = %q", body(t, rr))
	}
}

func TestAccessLogFields(t *testing.T) {
	capt := &captureLog{records: []map[string]any{}}
	logger := slog.New(capt)
	h, _ := newTestServer(t, Options{Logger: logger})
	doReq(t, h, "POST", "/log-me", "payload", nil)

	capt.mu.Lock()
	defer capt.mu.Unlock()
	if len(capt.records) != 1 {
		t.Fatalf("logged %d records, want 1", len(capt.records))
	}
	rec := capt.records[0]
	if rec["msg"] != "request" {
		t.Errorf("msg = %v, want request", rec["msg"])
	}
	if rec["method"] != "POST" {
		t.Errorf("method = %v, want POST", rec["method"])
	}
	if rec["path"] != "/log-me" {
		t.Errorf("path = %v, want /log-me", rec["path"])
	}
	if rec["route"] != "kvp" {
		t.Errorf("route = %v, want kvp", rec["route"])
	}
	if rec["status"] != int64(201) {
		t.Errorf("status = %v, want 201", rec["status"])
	}
	if rec["request_id"] == "" {
		t.Error("request_id missing from access log")
	}
	if _, ok := rec["latency_ms"]; !ok {
		t.Error("latency_ms missing from access log")
	}
}

// captureLog is a minimal slog.Handler that records each record's attrs.
type captureLog struct {
	mu      sync.Mutex
	records []map[string]any
}

func (c *captureLog) Enabled(context.Context, slog.Level) bool { return true }

func (c *captureLog) Handle(_ context.Context, r slog.Record) error {
	m := map[string]any{"msg": r.Message}
	r.Attrs(func(a slog.Attr) bool {
		m[a.Key] = a.Value.Any()
		return true
	})
	c.mu.Lock()
	c.records = append(c.records, m)
	c.mu.Unlock()
	return nil
}

func (c *captureLog) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *captureLog) WithGroup(string) slog.Handler      { return c }

type reqEvent struct {
	method, route, path, status string
}

type recordingMetrics struct {
	mu       sync.Mutex
	requests []reqEvent
}

func (m *recordingMetrics) Request(method, route, path, status string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests = append(m.requests, reqEvent{method, route, path, status})
}
func (m *recordingMetrics) RequestDuration(string, string, string, time.Duration) {}
func (m *recordingMetrics) RequestInFlight(string, string, string, int)          {}
func (m *recordingMetrics) DBQuery(string, time.Duration)                         {}
func (m *recordingMetrics) KeyStored()                                            {}
func (m *recordingMetrics) KeyExpired()                                           {}
func (m *recordingMetrics) Error(string, string, string)                          {}

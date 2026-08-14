package server

import (
	"crypto/subtle"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"runtime/debug"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/trace"
)

// requestID assigns a UUIDv4 X-Request-ID when the caller did not provide one,
// echoes it in the response, and stashes it in the context.
func (s *Server) requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" {
			id = uuid.NewString()
		}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(withRequestID(r.Context(), id)))
	})
}

// responseRecorder captures status code and byte count for the access log and
// metrics without buffering the body.
type responseRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (r *responseRecorder) WriteHeader(code int) {
	if r.status != 0 {
		return
	}
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

// Flush/Hijack/ReadFrom passthroughs so the recorder stays a transparent wrapper.
func (r *responseRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *responseRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// accessLog logs one line per request (spec §10.1) and drives the RED request
// metrics: count, duration, in-flight gauge, and error counter.
func (s *Server) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &responseRecorder{ResponseWriter: w}
		method := r.Method
		route := routeForPath(r.URL.Path)
		path := r.URL.Path
		s.opts.Metrics.RequestInFlight(method, route, path, 1)
		defer s.opts.Metrics.RequestInFlight(method, route, path, -1)

		next.ServeHTTP(rec, r)

		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}
		latency := time.Since(start)
		s.opts.Metrics.RequestDuration(method, route, path, latency)
		s.opts.Metrics.Request(method, route, path, fmt.Sprintf("%d", status))
		if status >= 400 {
			s.opts.Metrics.Error(route, path, fmt.Sprintf("%d", status))
		}

		sc := trace.SpanFromContext(r.Context()).SpanContext()
		attrs := []any{
			"method", method,
			"path", r.URL.Path,
			"route", route,
			"status", status,
			"latency_ms", float64(latency.Microseconds()) / 1000.0,
			"bytes", rec.bytes,
			"remote_ip", s.clientIP(r),
			"request_id", requestIDFrom(r.Context()),
		}
		if sc.IsValid() {
			attrs = append(attrs, "trace_id", sc.TraceID().String(), "span_id", sc.SpanID().String())
		}
		s.opts.Logger.Info("request", attrs...)
	})
}

// rateLimit applies the per-IP token bucket; the admin endpoints are exempt
// because they live on a separate listener with its own mux.
func (s *Server) rateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(s.opts.TrustedProxies) > 0 {
			s.rl.prune(time.Now())
		}
		ok, retryAfter := s.rl.allow(s.clientIP(r), time.Now())
		if !ok {
			w.Header().Set("Retry-After", fmt.Sprintf("%d", retryAfter))
			s.writeError(w, http.StatusTooManyRequests, "rate limited")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// auth enforces the static API key with a constant-time comparison when
// configured (§6). WWW-Authenticate is deliberately not emitted.
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.opts.APIKey != "" {
			got := r.Header.Get("X-API-Key")
			if !constantTimeEqual(got, s.opts.APIKey) {
				s.writeError(w, http.StatusUnauthorized, "unauthorized")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// securityHeaders applies the hardening headers from the spec §7.
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'")
		next.ServeHTTP(w, r)
	})
}

// recoverMiddleware turns panics into 500s and never lets a panic kill the
// process (spec §7).
func (s *Server) recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				s.opts.Logger.Error("panic recovered",
					"panic", fmt.Sprintf("%v", err),
					"stack", string(debug.Stack()),
					"request_id", requestIDFrom(r.Context()),
				)
				s.writeError(w, http.StatusInternalServerError, "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// clientIP resolves the caller IP: forwarded headers are trusted only when the
// direct peer is inside KVP_TRUSTED_PROXIES (§7).
func (s *Server) clientIP(r *http.Request) string {
	peer, ok := splitHostPort(r.RemoteAddr)
	if !ok {
		return r.RemoteAddr
	}
	if s.peerTrusted(peer) {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			first := strings.TrimSpace(strings.Split(xff, ",")[0])
			if first != "" {
				return first
			}
		}
		if cf := r.Header.Get("CF-Connecting-IP"); cf != "" {
			return strings.TrimSpace(cf)
		}
	}
	return peer
}

func (s *Server) peerTrusted(peer string) bool {
	ip, err := netip.ParseAddr(peer)
	if err != nil {
		return false
	}
	for _, p := range s.opts.TrustedProxies {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

func splitHostPort(addr string) (string, bool) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr, false
	}
	return host, true
}

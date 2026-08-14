// Package telemetry wires the observability stack (spec §10): stdlib slog with
// JSON output, OpenTelemetry metrics and traces over OTLP, a Prometheus text
// endpoint for local debugging, and the health registry. All signals share the
// service.name resource and are correlated by trace_id/request_id.
package telemetry

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
)

// Config holds the telemetry-relevant configuration subset.
type Config struct {
	ServiceName  string
	LogLevel     string
	OTLPEndpoint string
	StackID      string
	APIToken     string
	Region       string
}

// Telemetry bundles the logger, metrics, tracer and health handlers.
type Telemetry struct {
	Logger         *slog.Logger
	Metrics        *Metrics
	Tracer         trace.Tracer
	TracerProvider trace.TracerProvider
	Health         *Health

	promRegistry    *prometheus.Registry
	meterProvider   *sdkmetric.MeterProvider
	metricShutdown  func(context.Context) error
	traceShutdown   func(context.Context) error
}

// New builds the telemetry stack. An empty OTLPEndpoint leaves metrics on the
// Prometheus reader and traces as a no-op (dev mode) — it never fails to start
// when no collector is present.
func New(cfg Config) (*Telemetry, error) {
	if cfg.ServiceName == "" {
		cfg.ServiceName = "kvp"
	}
	level, err := parseLevel(cfg.LogLevel)
	if err != nil {
		return nil, err
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})).With("service", cfg.ServiceName)

	res, err := resource.New(context.Background(),
		resource.WithAttributes(semconv.ServiceName(cfg.ServiceName)))
	if err != nil {
		return nil, err
	}

	tel := &Telemetry{Logger: logger}

	mp, reg, err := newMeterProvider(res, cfg.OTLPEndpoint)
	if err != nil {
		return nil, err
	}
	tel.meterProvider = mp
	tel.promRegistry = reg

	tp, err := newTraceProvider(cfg.OTLPEndpoint, res)
	if err != nil {
		tel.Shutdown(context.Background())
		return nil, err
	}
	tel.TracerProvider = tp
	tel.Tracer = tp.Tracer(cfg.ServiceName)
	otel.SetTracerProvider(tp)

	tel.Metrics, err = newMetrics(mp.Meter(cfg.ServiceName))
	if err != nil {
		tel.Shutdown(context.Background())
		return nil, err
	}
	return tel, nil
}

// parseLevel maps a KVP_LOG_LEVEL string to a slog.Level.
func parseLevel(v string) (slog.Level, error) {
	switch v {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return slog.LevelInfo, errInvalidLevel(v)
}

type invalidLevel string

func (e invalidLevel) Error() string { return "invalid log level " + string(e) }

func errInvalidLevel(v string) error { return invalidLevel(v) }

func newMeterProvider(res *resource.Resource, endpoint string) (*sdkmetric.MeterProvider, *prometheus.Registry, error) {
	reg := prometheus.NewRegistry()
	promExp, err := otelprom.New(otelprom.WithRegisterer(reg))
	if err != nil {
		return nil, nil, err
	}
	opts := []sdkmetric.Option{
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(promExp),
		sdkmetric.WithView(sdkmetric.NewView(
			sdkmetric.Instrument{Name: "kvp_http_request_duration_seconds"},
			sdkmetric.Stream{Aggregation: sdkmetric.AggregationExplicitBucketHistogram{
				Boundaries: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5},
			}},
		)),
		sdkmetric.WithView(sdkmetric.NewView(
			sdkmetric.Instrument{Name: "kvp_db_query_duration_seconds"},
			sdkmetric.Stream{Aggregation: sdkmetric.AggregationExplicitBucketHistogram{
				Boundaries: []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5},
			}},
		)),
	}

	host, insecure := parseEndpoint(endpoint)
	if host != "" {
		expOpts := []otlpmetrichttp.Option{otlpmetrichttp.WithEndpoint(host)}
		if insecure {
			expOpts = append(expOpts, otlpmetrichttp.WithInsecure())
		}
		exp, err := otlpmetrichttp.New(context.Background(), expOpts...)
		if err != nil {
			return nil, nil, err
		}
		opts = append(opts, sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp)))
	}
	mp := sdkmetric.NewMeterProvider(opts...)
	return mp, reg, nil
}

func newTraceProvider(endpoint string, res *resource.Resource) (trace.TracerProvider, error) {
	host, insecure := parseEndpoint(endpoint)
	if host == "" {
		return sdktrace.NewTracerProvider(sdktrace.WithResource(res)), nil
	}
	expOpts := []otlptracehttp.Option{otlptracehttp.WithEndpoint(host)}
	if insecure {
		expOpts = append(expOpts, otlptracehttp.WithInsecure())
	}
	exp, err := otlptracehttp.New(context.Background(), expOpts...)
	if err != nil {
		return nil, err
	}
	return sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp), sdktrace.WithResource(res)), nil
}

// parseEndpoint converts an OTLP endpoint into host:port + insecure flag.
func parseEndpoint(endpoint string) (host string, insecure bool) {
	if endpoint == "" {
		return "", false
	}
	if !strings.Contains(endpoint, "://") {
		return endpoint, true
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return endpoint, false
	}
	return u.Host, u.Scheme == "http"
}

// MetricsHandler serves the Prometheus text exposition of all OTel metrics
// (§10.2) for the admin port.
func (tel *Telemetry) MetricsHandler() http.Handler {
	return promhttp.HandlerFor(tel.promRegistry, promhttp.HandlerOpts{})
}

// Shutdown flushes and closes the OTLP exporters and meter provider.
func (tel *Telemetry) Shutdown(ctx context.Context) error {
	var first error
	if tel.metricShutdown != nil {
		if err := tel.metricShutdown(ctx); err != nil {
			first = err
		}
	}
	if tel.traceShutdown != nil {
		if err := tel.traceShutdown(ctx); err != nil && first == nil {
			first = err
		}
	}
	if tel.meterProvider != nil {
		if err := tel.meterProvider.Shutdown(ctx); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// Metrics implements the server.Metrics and cleanup.Metrics surfaces with OTel
// instruments (§10.2). Raw key paths are recorded in the `path` label.
type Metrics struct {
	reqCount       metric.Int64Counter
	reqDuration    metric.Float64Histogram
	reqInFlight    metric.Int64UpDownCounter
	dbQuery        metric.Float64Histogram
	dbSize         metric.Int64Gauge
	dbLimit        metric.Int64Gauge
	dbRows         metric.Int64Gauge
	keysStored     metric.Int64Counter
	keysExpired    metric.Int64Counter
	cleanupRuns    metric.Int64Counter
	cleanupDeleted metric.Int64Counter
	httpErrors     metric.Int64Counter
}

func newMetrics(m metric.Meter) (*Metrics, error) {
	mk := &Metrics{}
	var err error
	if mk.reqCount, err = m.Int64Counter("kvp_http_requests_total"); err != nil {
		return nil, err
	}
	if mk.reqDuration, err = m.Float64Histogram("kvp_http_request_duration_seconds"); err != nil {
		return nil, err
	}
	if mk.reqInFlight, err = m.Int64UpDownCounter("kvp_http_request_in_flight"); err != nil {
		return nil, err
	}
	if mk.dbQuery, err = m.Float64Histogram("kvp_db_query_duration_seconds"); err != nil {
		return nil, err
	}
	if mk.dbSize, err = m.Int64Gauge("kvp_db_size_bytes"); err != nil {
		return nil, err
	}
	if mk.dbLimit, err = m.Int64Gauge("kvp_db_size_limit_bytes"); err != nil {
		return nil, err
	}
	if mk.dbRows, err = m.Int64Gauge("kvp_db_rows"); err != nil {
		return nil, err
	}
	if mk.keysStored, err = m.Int64Counter("kvp_keys_stored_total"); err != nil {
		return nil, err
	}
	if mk.keysExpired, err = m.Int64Counter("kvp_keys_expired_total"); err != nil {
		return nil, err
	}
	if mk.cleanupRuns, err = m.Int64Counter("kvp_cleanup_runs_total"); err != nil {
		return nil, err
	}
	if mk.cleanupDeleted, err = m.Int64Counter("kvp_cleanup_deleted_keys_total"); err != nil {
		return nil, err
	}
	if mk.httpErrors, err = m.Int64Counter("kvp_http_errors_total"); err != nil {
		return nil, err
	}
	return mk, nil
}

var mctx = context.Background()

func attrs(pairs ...string) []attribute.KeyValue {
	out := make([]attribute.KeyValue, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, attribute.String(pairs[i], pairs[i+1]))
	}
	return out
}

func (m *Metrics) Request(method, route, path, status string) {
	m.reqCount.Add(mctx, 1, metric.WithAttributes(attrs("method", method, "route", route, "path", path, "status", status)...))
}

func (m *Metrics) RequestDuration(method, route, path string, d time.Duration) {
	m.reqDuration.Record(mctx, d.Seconds(), metric.WithAttributes(attrs("method", method, "route", route, "path", path)...))
}

func (m *Metrics) RequestInFlight(method, route, path string, delta int) {
	m.reqInFlight.Add(mctx, int64(delta), metric.WithAttributes(attrs("method", method, "route", route, "path", path)...))
}

func (m *Metrics) DBQuery(operation string, d time.Duration) {
	m.dbQuery.Record(mctx, d.Seconds(), metric.WithAttributes(attrs("operation", operation)...))
}

func (m *Metrics) KeyStored()  { m.keysStored.Add(mctx, 1) }
func (m *Metrics) KeyExpired() { m.keysExpired.Add(mctx, 1) }

func (m *Metrics) Error(route, path, status string) {
	m.httpErrors.Add(mctx, 1, metric.WithAttributes(attrs("route", route, "path", path, "status", status)...))
}

func (m *Metrics) CleanupRun(kind, result string) {
	m.cleanupRuns.Add(mctx, 1, metric.WithAttributes(attrs("kind", kind, "result", result)...))
}

func (m *Metrics) CleanupDeleted(kind string, n int64) {
	m.cleanupDeleted.Add(mctx, n, metric.WithAttributes(attrs("kind", kind)...))
}

// SetDBSize and SetDBRows sample the DB gauge metrics from the maintenance tick.
func (m *Metrics) SetDBSize(bytes int64) { m.dbSize.Record(mctx, bytes) }
func (m *Metrics) SetDBRows(n int64)     { m.dbRows.Record(mctx, n) }

// SetDBLimit records the configured size budget (recorded once at startup).
func (m *Metrics) SetDBLimit(bytes int64) { m.dbLimit.Record(mctx, bytes) }

// Health backs the /healthz and /readyz endpoints (§10.4).
type Health struct {
	ping     func(context.Context) error
	draining atomic.Bool

	mu       sync.Mutex
	last     time.Time
	lastOK   bool
	cacheTTL time.Duration
}

func NewHealth(ping func(context.Context) error) *Health {
	return &Health{ping: ping, cacheTTL: time.Second}
}

// SetDraining flips readiness off during graceful shutdown.
func (h *Health) SetDraining(v bool) { h.draining.Store(v) }

func (h *Health) pingOK(full bool) bool {
	if full {
		return h.ping(context.Background()) == nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if time.Since(h.last) < h.cacheTTL {
		return h.lastOK
	}
	h.last = time.Now()
	h.lastOK = h.ping(context.Background()) == nil
	return h.lastOK
}

// Handler returns a mux serving /healthz and /readyz.
func (h *Health) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/healthz", h.LivenessHandler())
	mux.Handle("/readyz", h.ReadinessHandler())
	return mux
}

// LivenessHandler reports process liveness; always 200 once the server is up.
func (h *Health) LivenessHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"status": "error"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
}

// ReadinessHandler reports whether the process can serve traffic: not draining
// and able to reach the database. ?full=1 forces a live DB probe.
func (h *Health) ReadinessHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"status": "error"})
			return
		}
		if h.draining.Load() {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not ready", "reason": "draining"})
			return
		}
		full := r.URL.Query().Get("full") == "1"
		if !h.pingOK(full) {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not ready", "reason": "db"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

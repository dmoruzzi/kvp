# KVP Store (decikvp) — Production Specification v2

An HTTP key/value store with SQLite persistence, TTL expiry, and a web UI. Zero-auth by default, optional static API key. This revision redesigns the v1 spec for production: it restructures the codebase into packages, fixes all known v1 bugs (see §15), and adds first-class observability (structured logs, OpenTelemetry metrics/traces/logs, health checks, and GrafanaCloud-hosted dashboards and alerting).

## 1. Goals & Non-Goals

**Goals**
- Correctness under load: no data-loss paths, bounded memory use, bounded cleanup latency.
- Operational visibility: every request, DB operation, and background job is logged, metered, and traceable.
- Configurable by environment, no code changes for deployment tuning.
- Secure by default posture (headers, constant-time auth, rate limiting, secret hygiene).

**Non-Goals**
- Horizontal scaling / clustering. Single process, single SQLite file.
- Public write access from multiple trust domains.
- Replacing SQLite with a client/server database.
- Multi-tenancy.

## 2. Architecture

```
                    ┌──────────────────────────────────────────────────────────┐
                    │                          kvp (container)                 │
                    │                                                          │
 cloudflared ──────▶│  :8080  (public)                                         │
 (public entry)     │    http.Server (timeouts, graceful shutdown)             │
                    │      middleware chain: request-id → access-log →         │
                    │        ratelimit → auth → headers → otelhttp → router    │
                    │                                                          │
                    │  :9090  (admin, bound 127.0.0.1 only)                    │
                    │    /healthz  /readyz   /metrics (debug/fallback only)    │
                    │                                                          │
                    │  internal/store ── SQLite (WAL, busy_timeout)            │
                    │  internal/cleanup ── expiry sweep + size eviction         │
                    │  internal/telemetry ── slog+OTel (logs, metrics, traces) │
                    └──────────────────────────────────────────────────────────┘
                        │ OTLP (logs + metrics + traces)
                        ▼
                 otel-collector ── OTLP (Basic auth: stack ID + API token) ──▶
                                                                               ▼
                                                    GrafanaCloud (Metrics, Tempo, Loki)
                                                    dashboards + alerting (Terraform-provisioned)
```

- **Public listener** (`KVP_PORT`, default `:8080`) serves the app and UI.
- **Admin listener** (`KVP_METRICS_PORT`, default `127.0.0.1:9090`) serves `/healthz`, `/readyz`, `/metrics`. It is bound to `127.0.0.1` inside the container and is **never** exposed through cloudflared or published on the host. In production, metrics travel to GrafanaCloud over OTLP; `/metrics` is retained only for local debugging and as a fallback scrape target (e.g. a laptop Prometheus).

## 3. Repository Layout

| Path | Purpose |
|---|---|
| `cmd/kvp/main.go` | Entrypoint: load config, build telemetry, wire dependencies, start servers, graceful shutdown |
| `internal/config/` | Env-var config parsing + validation (see §11) |
| `internal/store/` | SQLite: schema/migration, CRUD, size stats, `VACUUM INTO` backup, incremental vacuum |
| `internal/server/` | Router, handlers, middleware (request-id, access log, ratelimit, auth, security headers, timeouts) |
| `internal/cleanup/` | Expiry sweep job + size-based batch eviction job |
| `internal/telemetry/` | OTel setup (logger, tracer, meter), OTLP exporter, Prometheus text endpoint for local debugging, health registry |
| `web/index.html` | Self-contained UI (no JS framework, no CDN, no CDN deps) |
| `deploy/Dockerfile` | Multi-stage, non-root, healthcheck |
| `deploy/docker-compose.yml` | App + cloudflared + otel-collector |
| `deploy/otel-collector.yaml` | Collector config (OTLP → GrafanaCloud, dev fallback to stdout) |
| `terraform/` | GrafanaCloud provisioning: dashboards, alert rules (via `grafana/` provider) |
| `.github/workflows/ci.yml` | Lint + test on PRs/pushes |
| `.github/workflows/docker.yml` | Multi-arch build & push to GHCR |
| `.env.example` | Documented env template (never committed with secrets) |
| `.gitignore` | Excludes binaries, `*.db`, `.env`, backups |

## 4. Data Model & Storage

Single SQLite file (path from `KVP_DB_PATH`). Schema created at startup via embedded migration:

```sql
PRAGMA journal_mode=WAL;
PRAGMA synchronous=NORMAL;
PRAGMA auto_vacuum=INCREMENTAL;
PRAGMA busy_timeout=5000;

CREATE TABLE IF NOT EXISTS kv_store (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    expires_at DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_kv_store_expires_at ON kv_store(expires_at);
```

- **key**: URL path (`path[1:]`), decoded, never empty, max length `KVP_MAX_KEY_BYTES` (default 256).
- **value**: raw request body bytes stored as TEXT (any content incl. binary), max size `KVP_MAX_BODY_BYTES` (default 1 MiB).
- **expires_at**: set to `now + KVP_TTL` (default 24h). TTL is server-configurable, not client-configurable. (v1 had an internal inconsistency: §2 said 24h, §3 said 1h — now a single config knob.)
- **Index**: `idx_kv_store_expires_at` backs the expiry sweep and oldest-first eviction; without it both jobs degrade to full scans.
- **WAL mode** allows concurrent readers with the writer; `busy_timeout` prevents immediate `SQLITE_BUSY`.
- **`auto_vacuum=INCREMENTAL`**: deleted space stays in the file until `PRAGMA incremental_vacuum(N)` is run by the maintenance job (§8.4), avoiding unbounded file growth.

## 5. HTTP API

### 5.1 Routing

One dispatcher. Key is `r.URL.Path[1:]` (URL-decoded).

| Request | Behavior |
|---|---|
| `GET /`, `GET /index.html` | Serve `web/index.html`; any other method → `405` |
| `GET /healthz` (admin port) | Liveness: `200 {"status":"ok"}` if process alive |
| `GET /readyz` (admin port) | Readiness: `200 {"status":"ready"}` if DB pingable and not draining; else `503` |
| `GET /metrics` (admin port) | Prometheus text exposition of OTel metrics (§10.2) |
| `POST /<key>` | Store value (§5.2) |
| `GET /<key>` | Retrieve value (§5.3) |
| any other method on `/<key>` | `405` |

Empty key → `400 {"error":"key required"}`.

### 5.2 `POST /<key>`

1. Body read with `http.MaxBytesReader` limit `KVP_MAX_BODY_BYTES` → `413 {"error":"payload too large"}` if exceeded.
2. Upsert: `INSERT INTO kv_store(key, value, expires_at) VALUES(?,?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value, expires_at=excluded.expires_at`.
3. Trigger async size-based eviction (§8.2).
4. Respond `201 Created`, body `stored` (text/plain; kept for v1 compatibility).

### 5.3 `GET /<key>`

1. `SELECT value, expires_at FROM kv_store WHERE key = ?`.
2. No row → `404 {"error":"key not found"}`.
3. Row exists but `now > expires_at` → delete on the spot, `404 {"error":"key expired"}`.
4. Else `200` with raw value bytes.

### 5.4 Status codes

| Code | Meaning |
|---|---|
| `200` | Value retrieved |
| `201` | Value stored |
| `400` | Key missing / invalid (also `413` for oversize body) |
| `401` | Missing/mismatched API key (when auth enabled) |
| `404` | Key not found / key expired |
| `405` | Method not allowed (with `Allow` header) |
| `413` | Body exceeds `KVP_MAX_BODY_BYTES` |
| `429` | Rate limited (with `Retry-After`) |
| `503` | Draining / not ready |
| `500` | Internal error (never leaks details; logged with stack) |

All error responses are JSON `{"error": "<message>"}` with `Content-Type: application/json`. v1 returned plain-text errors; this is a documented breaking change.

## 6. Authentication

- If `KVP_API_KEY` is set: every request on the **public** listener must carry `X-API-Key` matching exactly, else `401`. The static UI assets (`GET /`, `GET /index.html`, `GET /app.js`) are **exempt** so the index loads in a browser without a key; the UI authenticates its data operations by sending `X-API-Key` (entered by the user, remembered in `sessionStorage`) on its PUT/GET requests, which stay fully protected.
- Comparison uses `crypto/subtle.ConstantTimeCompare` (v1 used `!=` — timing side channel).
- `WWW-Authenticate` header is **removed** (v1 emitted `Basic`, which is misleading for a header scheme).
- If `KVP_API_KEY` is empty: open access (documented default).
- The API key is never logged, metered, or traced.
- Admin port endpoints (`/healthz`, `/readyz`, `/metrics`) are not API-key protected because they are unreachable outside the container network.

## 7. Middleware & Server Hardening

Middleware runs in this order (public listener): request-id → access log → rate limit → auth → security headers → otelhttp → router.

| Concern | Design |
|---|---|
| Request IDs | Assign `X-Request-ID` (UUIDv4) if absent; echo in response, propagate into logs, traces, and error messages. |
| Server timeouts | `ReadHeaderTimeout` 5s, `ReadTimeout` 30s, `WriteTimeout` 30s, `IdleTimeout` 120s. |
| Body limits | `http.MaxBytesReader` per request (§5.2). |
| Rate limiting | In-memory token bucket per client IP (`KVP_RATE_LIMIT_RPS`, burst `KVP_RATE_LIMIT_BURST`, default 10/20). `429` + `Retry-After`. Exempts `/healthz`, `/readyz`, `/metrics` on the admin port. |
| Client IP | Trust `CF-Connecting-IP`/`X-Forwarded-For` only when the peer is in `KVP_TRUSTED_PROXIES` (default empty → never trust forwarded headers). Compose sets it to the cloudflared service. |
| Security headers | `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`, `Referrer-Policy: no-referrer`, and a strict CSP for `/` (default-src 'self'; no inline scripts — the UI is static, so this is feasible). |
| Graceful shutdown | On SIGINT/SIGTERM: stop accepting, wait up to `KVP_SHUTDOWN_TIMEOUT` (default 15s) for in-flight requests, mark not-ready (`/readyz` → 503), flush OTel, close DB. |
| Panic recovery | Recover middleware returns `500`, logs stack with trace ID, and records the error metric — a panic never kills the process. |

## 8. Cleanup & Maintenance

All jobs are launched from `main`, run in their own goroutines, share a `context.Context` that is cancelled on shutdown, and never silently fail (v1 bug: errors ignored).

### 8.1 Expiry sweep (background)

Every `KVP_CLEANUP_INTERVAL` (default 1h): `DELETE FROM kv_store WHERE expires_at < ?` using the `expires_at` index. Counts and errors are logged and metered.

### 8.2 Size-based eviction (on POST + background)

Fixed algorithm — **never deletes the whole table** (v1 bug #1):

1. If `db_size < KVP_MAX_DB_BYTES` (default 64 MiB) → no-op.
2. Loop in batches of `KVP_CLEANUP_BATCH_SIZE` (default 1000):
   `DELETE FROM kv_store WHERE key IN (SELECT key FROM kv_store ORDER BY expires_at ASC LIMIT ?)`
   Oldest-expiring rows are evicted first; each batch is a bounded statement, so a run cannot wipe the table.
3. Stop when size < limit, a batch affects 0 rows, or `KVP_CLEANUP_MAX_RUNS` (default 64) batches executed (bounds run latency).
4. Throttled to once per `KVP_SIZE_CLEANUP_THROTTLE` (default 1m) via a singleflight-style guard; serialized with other writers by a `sync.Mutex` (v1 `cleanupMu`).

### 8.3 On-read lazy delete

`GET` deletes expired rows on the spot (§5.3).

### 8.4 Vacuum & backup (background)

- After a size eviction run, `PRAGMA incremental_vacuum(KVP_CLEANUP_BATCH_SIZE)` reclaims freed pages.
- Backup job every `KVP_BACKUP_INTERVAL` (default 24h): `VACUUM INTO 'KVP_BACKUP_DIR/kvp-<timestamp>.db'` (works with CGO-free `modernc.org/sqlite`). Retention: keep newest `KVP_BACKUP_RETENTION` (default 7) files, delete the rest. Failures are logged and metered.

## 9. Concurrency Model

- `net/http` stdlib servers; shared `*sql.DB` (safe for concurrent use), WAL mode + `busy_timeout`.
- One `sync.Mutex` guards size eviction runs (not row writes).
- Jobs (expiry, size, vacuum/backup) run concurrently with handlers under a cancellable root context.
- All shared state (rate-limiter buckets, last-run timestamps) is mutex-protected; no package-level mutable vars (v1 kept `lastCleanup`/`apiKey` at package scope).

## 10. Observability

The three pillars (logs, metrics, traces) are emitted through the OpenTelemetry SDK from a single `internal/telemetry` package, so every signal carries the same `service.name=kvp` and is correlated by `trace_id`/`span_id`/`request_id`.

### 10.1 Structured logging

`slog` (stdlib) with a JSON handler bridged to OTel logs (`go.opentelemetry.io/otel/log/slog`). Level from `KVP_LOG_LEVEL`. Format: one JSON object per line.

| Field | Source | Notes |
|---|---|---|
| `ts`, `level`, `msg` | slog | standard |
| `service` | config | `kvp` |
| `request_id` | middleware | per-request |
| `trace_id`, `span_id` | OTel context | correlation to traces |
| `method`, `path`, `route` | middleware | `path` carries the **raw key path** (real key names are expected in logs); `route` keeps the template (`index` / `kvp`). Cardinality is accepted as a deliberate product decision. |
| `status`, `latency_ms`, `bytes` | middleware | response observability |
| `remote_ip` | middleware | resolved per §7 trusted-proxy rules |
| `key` | handlers/jobs | key name only; **values and API key never logged** |

Access log: `level=info` per request. Job logs: one line per run with counts/error. Errors log at `level=error` with stack.

### 10.2 Metrics

Instrumented via OTel meters; no direct Prom client calls. Primary path: OTel SDK → OTLP → otel-collector → GrafanaCloud Metrics. A Prometheus text endpoint remains on `/metrics` (admin port) for local debugging and fallback scraping.

| Metric | Type | Labels | Description |
|---|---|---|---|
| `kvp_http_requests_total` | Counter | `method`, `route`, `path`, `status` | Request count. `route` is a template (`index`, `kvp`); `path` carries the **raw key path** (real key names are expected in the labels, accepted cardinality). |
| `kvp_http_request_duration_seconds` | Histogram | `method`, `route`, `path` | Buckets: 5ms..5s. Source for RED/SLO latency alerts. `path` carries the raw key path (accepted cardinality). |
| `kvp_http_request_in_flight` | Gauge | — | Concurrent requests. |
| `kvp_db_query_duration_seconds` | Histogram | `operation` (`get`, `put`, `delete_expired`, …) | DB call latency. |
| `kvp_db_size_bytes` | Gauge | — | On-disk DB file size. |
| `kvp_db_rows` | Gauge | — | `COUNT(*)` sampled on the maintenance tick. |
| `kvp_keys_stored_total` | Counter | — | Successful writes (logical writes, not evictions). |
| `kvp_keys_expired_total` | Counter | — | Expired rows deleted (sweep + lazy delete). |
| `kvp_cleanup_runs_total` | Counter | `kind` (`expiry`, `size`, `vacuum`, `backup`), `result` (`ok`, `error`) | Job runs. |
| `kvp_cleanup_deleted_keys_total` | Counter | `kind` | Keys evicted/deleted per job. |
| `kvp_http_errors_total` | Counter | `route`, `status` | 4xx/5xx count (drives the error-rate alert). |

Runtime metrics: `process_*`/`go_*` (GOMAXPROCS, goroutines, GC) auto-registered by the exporter. Use RED for API health (Rate/Errors/Duration above) and USE for resources (SQLite file size, connections via `db.Stats` → `kvp_db_size_bytes` and a `kvp_sql_connections` gauge if desired).

### 10.3 Tracing

- OTel SDK with OTLP exporter (`KVP_OTEL_EXPORTER_OTLP_ENDPOINT`). Production: otel-collector → GrafanaCloud Tempo. If unset → stdout/no-op exporter (dev mode); no crash when no collector is present.
- Spans:
  - `kvp.http` — per request (from `otelhttp`), carrying `request_id` as attribute.
  - `kvp.db.query` — per DB operation, with `operation` attribute.
  - `kvp.cleanup.*` — one span per job run, with deleted-count attributes.
- Context propagation: W3C `traceparent` honored on inbound requests; `X-Request-ID` is attached as a span attribute so HTTP and traces correlate even when the caller doesn't propagate traces.

### 10.4 Health checks

- `/healthz` (liveness): always `200` while the process runs.
- `/readyz` (readiness): `200` when the DB pings and the process is not draining; else `503`. Used by Docker `HEALTHCHECK` and Compose `healthcheck`.
- Optional `?full=1` on `/readyz` runs an actual `SELECT 1` (default is cached-ping to keep it cheap).

### 10.5 Telemetry data flow

```
kvp ──slog ─────────┐
kvp ──OTel meter ───┤ OTLP (single connection, one signal pipeline each)
kvp ──OTel tracer ──┘        │
                            ▼
                 otel-collector ──OTLP──▶ GrafanaCloud OTLP gateway
                                          ├──▶ Metrics (PromQL)
                                          ├──▶ Tempo (traces)
                                          └──▶ Loki (logs)
                          dashboards + alert rules provisioned via Terraform

Local debugging (dev): collector → stdout/file; /metrics scrapeable on 127.0.0.1:9090.
```

### 10.6 Dashboards & alerts (GrafanaCloud)

Dashboards and alert rules are provisioned with Terraform in `terraform/` (Terraform Cloud/CLI + the `grafana/` provider), authenticated with a scoped GrafanaCloud service-account token. Datasource is GrafanaCloud Metrics (queried with PromQL). Logs and traces are linked from dashboard panels into Loki and Tempo for full drill-down.

| Alert | Expression (5m) | Severity |
|---|---|---|
| High 5xx rate | `sum(rate(kvp_http_requests_total{status=~"5.."}[5m])) / sum(rate(kvp_http_requests_total[5m])) > 0.01` | critical |
| High latency | `histogram_quantile(0.95, sum(rate(kvp_http_request_duration_seconds_bucket[5m])) by (le)) > 1` | warning |
| DB near limit | `kvp_db_size_bytes > KVP_MAX_DB_BYTES * 0.9` | warning |
| Cleanup failures | `rate(kvp_cleanup_runs_total{result="error"}[5m]) > 0` | critical |
| Instance down | `absent(up{job="kvp"})` | critical |

Dashboard panels: RPS by route/status, latency p50/p95/p99, error rates, DB size vs limit, rows over time, cleanup runs/deleted keys, Go runtime. Terraform `plan`/`apply` runs in CI (see §13.1).

### 10.7 SLOs

| Objective | Target |
|---|---|
| Availability | ≥ 99.9% monthly |
| Latency p95 | < 500ms |
| Latency p99 | < 2s |
| Error budget burn | Alert when error budget > 2% of month consumed in 1h |

### 10.8 Observability security

- `/metrics`, `/healthz`, `/readyz` live only on `127.0.0.1:9090` in the container; cloudflared never routes to them.
- The only egress to GrafanaCloud is OTLP over TLS to the gateway, authenticated with Basic auth (stack instance ID + scoped API token) held in `.env`; the token has only `metrics:write`, `traces:write`, `logs:write` scopes and is never committed or logged.
- No user **values** or the API key in logs, metrics, or trace attributes. **Raw key names are expected** in access logs and as the `path` metric label (a deliberate product decision; cardinality is accepted and bounded only by `KVP_MAX_KEY_BYTES`).

## 11. Configuration

All config via environment variables, parsed/validated in `internal/config` (fail fast on invalid values, with a clear error listing the offending var).

| Env | Default | Description |
|---|---|---|
| `KVP_PORT` | `:8080` | Public listener address |
| `KVP_METRICS_PORT` | `127.0.0.1:9090` | Admin listener (health + metrics); keep loopback-only |
| `KVP_DB_PATH` | `./kvp.db` | SQLite file path |
| `KVP_API_KEY` | *(empty)* | Enables auth when set; constant-time comparison |
| `KVP_MAX_BODY_BYTES` | `1048576` (1 MiB) | Max POST body |
| `KVP_MAX_KEY_BYTES` | `256` | Max key length |
| `KVP_MAX_DB_BYTES` | `67108864` (64 MiB) | Size-eviction threshold |
| `KVP_TTL` | `24h` | Value lifetime (Go duration) |
| `KVP_CLEANUP_INTERVAL` | `1h` | Expiry sweep period |
| `KVP_SIZE_CLEANUP_THROTTLE` | `1m` | Min interval between size evictions |
| `KVP_CLEANUP_BATCH_SIZE` | `1000` | Rows per eviction statement |
| `KVP_CLEANUP_MAX_RUNS` | `64` | Max batches per eviction run |
| `KVP_RATE_LIMIT_RPS` | `10` | Per-IP token refill rate |
| `KVP_RATE_LIMIT_BURST` | `20` | Per-IP burst |
| `KVP_TRUSTED_PROXIES` | *(empty)* | CIDRs allowed to set forwarded client-IP headers |
| `KVP_SHUTDOWN_TIMEOUT` | `15s` | Graceful drain window |
| `KVP_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `KVP_OTEL_SERVICE_NAME` | `kvp` | OTel service name |
| `KVP_OTEL_EXPORTER_OTLP_ENDPOINT` | *(empty)* | OTLP endpoint (compose sets it to the collector); empty = stdout/no-op dev mode |
| `GRAFANA_CLOUD_STACK_ID` | *(empty)* | GrafanaCloud stack instance ID (Basic-auth user) |
| `GRAFANA_CLOUD_API_TOKEN` | *(empty)* | Scoped GrafanaCloud token (`metrics:write`, `traces:write`, `logs:write`); Basic-auth password |
| `GRAFANA_CLOUD_REGION` | `prod-us-central-0` | GrafanaCloud region; collector builds `otlp-gateway-<region>.grafana.net:443` |
| `KVP_BACKUP_DIR` | *(empty, disabled)* | Directory for `VACUUM INTO` backups |
| `KVP_BACKUP_INTERVAL` | `24h` | Backup cadence |
| `KVP_BACKUP_RETENTION` | `7` | Backup files to keep |

## 12. Deployment

### 12.1 Dockerfile (`deploy/Dockerfile`)

- Builder: `golang:1.26-alpine`, `CGO_ENABLED=0`, pure-Go `modernc.org/sqlite` → static binary.
- Runtime: `alpine:3.23`, non-root user (`USER 65532:65532`), copies binary + `web/index.html`, exposes 8080, `ENTRYPOINT` with `STOPSIGNAL SIGTERM`.
- `HEALTHCHECK` hitting `http://127.0.0.1:9090/readyz` (liveness on the admin port, loopback-only).
- The DB volume is mounted with the app running as non-root (volume ownership handled via a compose init step or named-volume copy).

### 12.2 docker-compose.yml (`deploy/docker-compose.yml`)

| Service | Notes |
|---|---|
| `kvp` | image `ghcr.io/dmoruzzi/decikvp:latest`; host port `127.0.0.1:8080:8080`; volume `kvp_data:/app`; `healthcheck` against `/readyz`; `restart: unless-stopped`; env from `.env` (no inline secrets). |
| `cloudflared` | public entrypoint; `depends_on: kvp (service_healthy)`; token from `.env`, **not** committed (v1 bug #9). |
| `otel-collector` | `profiles: ["observability"]`; receives OTLP from `kvp`, exports OTLP → GrafanaCloud gateway (Basic auth from `.env`); if GrafanaCloud vars are unset, falls back to file/stdout for local dev. |

Default `docker compose up -d` runs app + tunnel (same as v1). The `observability` profile adds only the collector. There is **no self-hosted Prometheus or Grafana** in production — dashboards and alerts live in GrafanaCloud. An optional `local-dev` profile (standalone Prometheus scraping `/metrics`) is available purely for laptop debugging.

### 12.3 Secrets

- `.env` holds `TUNNEL_TOKEN`, `KVP_API_KEY`, `GRAFANA_CLOUD_STACK_ID`, `GRAFANA_CLOUD_API_TOKEN`, `GRAFANA_CLOUD_REGION`; `.env` is git-ignored (v1 committed the tunnel token).
- `.env.example` documents every variable with no real values.

## 13. CI/CD

### 13.1 CI (`.github/workflows/ci.yml`)
- Trigger: PRs and pushes to `main`.
- Jobs: `golangci-lint`, `go vet`, `go test ./...` (unit + integration), `go build`.
- Observability provisioning: `terraform fmt` + `terraform validate` on `terraform/` (no apply on PRs).

### 13.2 Build & push (`.github/workflows/docker.yml`)
- Trigger: push to `main` (and PRs → build-only).
- Buildx multi-arch (`linux/amd64`, `linux/arm64`), tags: branch ref, short SHA, `latest` on default branch.
- Push to `ghcr.io/<repo>` with `GITHUB_TOKEN`; scan image (e.g. `trivy`) and fail on critical findings.

### 13.3 GrafanaCloud provisioning
- On push to `main`: `terraform apply` for dashboards/alert rules, authenticated with a `GRAFANA_CLOUD_SERVICE_ACCOUNT_TOKEN` stored as a CI secret (separate from the runtime write-only token in §11).

## 14. Testing

| Layer | Scope |
|---|---|
| Unit — server | Handler tests with `httptest`: routing, status codes, body limit → 413, rate limit → 429, auth pass/fail, constant-time path. |
| Unit — store | Temp-file SQLite: upsert/retrieve/expiry, lazy delete, eviction ordering. |
| Unit — cleanup | Size eviction with a small `KVP_MAX_DB_BYTES`: asserts bounded batches, oldest-first order, stop conditions, and that the table is **never fully emptied** (regression test for v1 bug #1). |
| Integration | Compose smoke test: post/get/expire cycle, `/healthz`+`/readyz`, `/metrics` returns Prometheus text and contains `kvp_http_requests_total`, and the collector receives OTLP for all three signals (assert against its debug/file exporter). |
| Observability checks | Every metric that must exist is listed and asserted in a golden metrics test; logs are valid JSON; the `path` label carries the raw key as expected, and no metric or log carries a **value** or the API key. |

## 15. v1 Bugs → Fixes

| # | v1 issue | v2 fix |
|---|---|---|
| 1 | Size cleanup deletes ALL rows (unbounded `DELETE`) | Batch eviction with `LIMIT` + max runs (§8.2) |
| 2 | No body size limit | `http.MaxBytesReader` + 413 (§5.2, §7) |
| 3 | No rate limiting | Token bucket per IP + 429 (§7) |
| 4 | `!=` key compare, misleading `WWW-Authenticate` | `subtle.ConstantTimeCompare`, header removed (§6) |
| 5 | UI `.trim()` corrupts stored values | Trim removed; exact bytes stored/rendered; UI still validates empty |
| 6 | Unbounded history / no eviction order | Oldest-first batch eviction with bounded runs (§8.2) |
| 7 | Cleanup errors ignored | Logged + metered (`kvp_cleanup_runs_total{result="error"}`) |
| 8 | Inline tunnel token committed | `.env` + gitignored, `.env.example` template (§12.3) |
| 9 | No backup/VACUUM → unbounded file growth | `auto_vacuum=INCREMENTAL`, incremental vacuum, `VACUUM INTO` backups with retention (§8.4) |
| 10 | No observability at all | §10: logs, metrics, traces, health, dashboards, alerts |
| 11 | 24h vs 1h TTL inconsistency | Single `KVP_TTL` knob, default 24h (§4, §11) |
| 12 | Global mutable state, single 1000-line `main.go` | Package layout + `internal/config` (§3, §9) |

## 16. Security Notes

- Default (no `KVP_API_KEY`) is open access; in Compose this is mitigated by `127.0.0.1` host binding + tunnel. **Recommendation:** set `KVP_API_KEY` for anything reachable beyond localhost.
- v1 stored-XSS (`index.html` used `innerHTML`): UI now renders values via `textContent`; CSP `default-src 'self'` blocks inline script/style. Malicious values remain plain data.
- Rate limiting keyed on client IP; IPs come only from trusted proxies (§7).
- Admin endpoints unreachable from the tunnel; observability egress is TLS-only to GrafanaCloud with scoped write tokens (§10.8).
- No values or API key in logs, metrics, or trace attributes. Raw key names are expected in logs and the `path` metric label (§10.1, §10.8).

## 17. Operational Runbooks (condensed)

| Symptom | Check |
|---|---|
| High 5xx / latency | GrafanaCloud error-rate + p95 panel; drill into the trace in Tempo by `request_id` (from Loki logs). |
| DB size near limit | `kvp_db_size_bytes` vs `kvp_cleanup_deleted_keys_total{kind="size"}`; confirm eviction is running. |
| Cleanup alert | `kvp_cleanup_runs_total{result="error"}`; check job logs (trace-correlated). |
| Auth misconfig | Confirm `KVP_API_KEY` set in `.env`; logs show 401s with `remote_ip`. |
| Disk/backup | Verify `KVP_BACKUP_DIR` retention; `kvp_cleanup_runs_total{kind="backup"}`. |

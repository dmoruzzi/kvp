// Command kvp runs the KVP store server: the public API on the public port,
// the admin endpoints (health, metrics) on the loopback metrics port, and the
// background maintenance jobs (expiry sweep, size eviction, backups). It shuts
// down gracefully on SIGINT/SIGTERM: readiness flips off, connections drain
// within KVP_SHUTDOWN_TIMEOUT, jobs stop, telemetry flushes, DB closes.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "time/tzdata"

	"github.com/dmoruzzi/kvp/internal/cleanup"
	"github.com/dmoruzzi/kvp/internal/config"
	"github.com/dmoruzzi/kvp/internal/server"
	"github.com/dmoruzzi/kvp/internal/store"
	"github.com/dmoruzzi/kvp/internal/telemetry"
	"github.com/dmoruzzi/kvp/web"

	otelhttp "go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "kvp:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	tel, err := telemetry.New(telemetry.Config{
		ServiceName:  cfg.OTELServiceName,
		LogLevel:     cfg.LogLevel,
		OTLPEndpoint: cfg.OTLPEndpoint,
		StackID:      cfg.GrafanaCloudStackID,
		APIToken:     cfg.GrafanaCloudAPIToken,
		Region:       cfg.GrafanaCloudRegion,
	})
	if err != nil {
		return fmt.Errorf("telemetry: %w", err)
	}
	logger := tel.Logger

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		_ = tel.Shutdown(context.Background())
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = st.Close() }()

	health := telemetry.NewHealth(func(ctx context.Context) error { return st.Ping(ctx) })
	tel.Metrics.SetDBLimit(cfg.MaxDBBytes)

	expiry := cleanup.NewExpiryRunner(st, cfg.CleanupInterval, logger, tel.Metrics)
	evictor := cleanup.NewEvictor(st, cfg.MaxDBBytes, cfg.CleanupBatchSize, cfg.CleanupMaxRuns, cfg.SizeCleanupThrottle, logger, tel.Metrics)
	backuper := cleanup.NewBackuper(st, cfg.BackupDir, cfg.BackupInterval, cfg.BackupRetention, logger, tel.Metrics)

	handler := server.New(st, server.Options{
		MaxBodyBytes:   cfg.MaxBodyBytes,
		MaxKeyBytes:    cfg.MaxKeyBytes,
		TTL:            cfg.TTL,
		APIKey:         cfg.APIKey,
		RateLimitRPS:   cfg.RateLimitRPS,
		RateLimitBurst: cfg.RateLimitBurst,
		TrustedProxies: cfg.TrustedProxies,
		UI:             web.FS,
		Logger:         logger,
		Metrics:        tel.Metrics,
		AfterWrite:     func() { _, _ = evictor.MaybeEvict(context.Background()) },
		Wrap: func(h http.Handler) http.Handler {
			return otelhttp.NewHandler(h, "kvp.http")
		},
	})

	publicSrv := &http.Server{
		Addr:              cfg.Port,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	adminMux := http.NewServeMux()
	adminMux.Handle("/healthz", health.LivenessHandler())
	adminMux.Handle("/readyz", health.ReadinessHandler())
	adminMux.Handle("/metrics", tel.MetricsHandler())
	adminSrv := &http.Server{
		Addr:              cfg.MetricsPort,
		Handler:           adminMux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	jobCtx, stopJobs := context.WithCancel(context.Background())
	defer stopJobs()
	go expiry.Run(jobCtx)
	go evictor.RunLoop(jobCtx)
	go backuper.Run(jobCtx)
	go sampleDBGauges(jobCtx, cfg.CleanupInterval, logger, st, tel.Metrics)

	errCh := make(chan error, 2)
	go func() { errCh <- publicSrv.ListenAndServe() }()
	go func() { errCh <- adminSrv.ListenAndServe() }()
	logger.Info("kvp listening",
		"public", cfg.Port,
		"admin", cfg.MetricsPort,
		"db", cfg.DBPath,
		"backup_dir", cfg.BackupDir)

	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server failed", "error", err)
			stop()
		}
	case <-sigCtx.Done():
		logger.Info("shutdown signal received")
	}

	health.SetDraining(true)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := publicSrv.Shutdown(shutdownCtx); err != nil {
		logger.Warn("public server shutdown", "error", err)
	}
	if err := adminSrv.Shutdown(shutdownCtx); err != nil {
		logger.Warn("admin server shutdown", "error", err)
	}
	stopJobs()

	if err := tel.Shutdown(shutdownCtx); err != nil {
		logger.Warn("telemetry shutdown", "error", err)
	}
	if err := st.Close(); err != nil {
		logger.Warn("store close", "error", err)
	}
	logger.Info("shutdown complete")
	return nil
}

// sampleDBGauges reports the DB size and row gauges on every maintenance tick.
func sampleDBGauges(ctx context.Context, interval time.Duration, logger *slog.Logger, st *store.Store, m *telemetry.Metrics) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if sz, err := st.Size(ctx); err == nil {
				m.SetDBSize(sz)
			} else {
				logger.Warn("db size sample failed", "error", err)
			}
			if n, err := st.RowCount(ctx); err == nil {
				m.SetDBRows(n)
			} else {
				logger.Warn("db rows sample failed", "error", err)
			}
		}
	}
}

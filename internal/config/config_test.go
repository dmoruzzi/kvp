package config

import (
	"strings"
	"testing"
	"time"
)

func envNone(string) string { return "" }

func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestDefaults(t *testing.T) {
	cfg, err := Parse(envNone)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Port != ":8080" {
		t.Errorf("Port = %q, want %q", cfg.Port, ":8080")
	}
	if cfg.MetricsPort != "127.0.0.1:9090" {
		t.Errorf("MetricsPort = %q, want %q", cfg.MetricsPort, "127.0.0.1:9090")
	}
	if cfg.DBPath != "./kvp.db" {
		t.Errorf("DBPath = %q, want %q", cfg.DBPath, "./kvp.db")
	}
	if cfg.APIKey != "" {
		t.Errorf("APIKey = %q, want empty", cfg.APIKey)
	}
	if cfg.MaxBodyBytes != 1048576 {
		t.Errorf("MaxBodyBytes = %d, want 1048576", cfg.MaxBodyBytes)
	}
	if cfg.MaxKeyBytes != 256 {
		t.Errorf("MaxKeyBytes = %d, want 256", cfg.MaxKeyBytes)
	}
	if cfg.MaxDBBytes != 67108864 {
		t.Errorf("MaxDBBytes = %d, want 67108864", cfg.MaxDBBytes)
	}
	if cfg.TTL != 24*time.Hour {
		t.Errorf("TTL = %v, want 24h", cfg.TTL)
	}
	if cfg.CleanupInterval != time.Hour {
		t.Errorf("CleanupInterval = %v, want 1h", cfg.CleanupInterval)
	}
	if cfg.SizeCleanupThrottle != time.Minute {
		t.Errorf("SizeCleanupThrottle = %v, want 1m", cfg.SizeCleanupThrottle)
	}
	if cfg.CleanupBatchSize != 1000 {
		t.Errorf("CleanupBatchSize = %d, want 1000", cfg.CleanupBatchSize)
	}
	if cfg.CleanupMaxRuns != 64 {
		t.Errorf("CleanupMaxRuns = %d, want 64", cfg.CleanupMaxRuns)
	}
	if cfg.RateLimitRPS != 10 {
		t.Errorf("RateLimitRPS = %v, want 10", cfg.RateLimitRPS)
	}
	if cfg.RateLimitBurst != 20 {
		t.Errorf("RateLimitBurst = %d, want 20", cfg.RateLimitBurst)
	}
	if len(cfg.TrustedProxies) != 0 {
		t.Errorf("TrustedProxies = %v, want empty", cfg.TrustedProxies)
	}
	if cfg.ShutdownTimeout != 15*time.Second {
		t.Errorf("ShutdownTimeout = %v, want 15s", cfg.ShutdownTimeout)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want info", cfg.LogLevel)
	}
	if cfg.OTELServiceName != "kvp" {
		t.Errorf("OTELServiceName = %q, want kvp", cfg.OTELServiceName)
	}
	if cfg.OTLPEndpoint != "" {
		t.Errorf("OTLPEndpoint = %q, want empty", cfg.OTLPEndpoint)
	}
	if cfg.GrafanaCloudRegion != "prod-us-central-0" {
		t.Errorf("GrafanaCloudRegion = %q, want prod-us-central-0", cfg.GrafanaCloudRegion)
	}
	if cfg.BackupDir != "" {
		t.Errorf("BackupDir = %q, want empty (disabled)", cfg.BackupDir)
	}
	if cfg.BackupInterval != 24*time.Hour {
		t.Errorf("BackupInterval = %v, want 24h", cfg.BackupInterval)
	}
	if cfg.BackupRetention != 7 {
		t.Errorf("BackupRetention = %d, want 7", cfg.BackupRetention)
	}
	if cfg.CleanupSweepLimit != 10000 {
		t.Errorf("CleanupSweepLimit = %d, want 10000", cfg.CleanupSweepLimit)
	}
}

func TestCustomValues(t *testing.T) {
	cfg, err := Parse(envMap(map[string]string{
		"KVP_PORT":                          ":9000",
		"KVP_METRICS_PORT":                  "127.0.0.1:9100",
		"KVP_DB_PATH":                       "/tmp/kvp.db",
		"KVP_API_KEY":                       "sekrit",
		"KVP_MAX_BODY_BYTES":                "2048",
		"KVP_MAX_KEY_BYTES":                 "64",
		"KVP_MAX_DB_BYTES":                  "1024",
		"KVP_TTL":                           "5m",
		"KVP_CLEANUP_INTERVAL":              "10s",
		"KVP_SIZE_CLEANUP_THROTTLE":         "15s",
		"KVP_CLEANUP_BATCH_SIZE":            "50",
		"KVP_CLEANUP_MAX_RUNS":              "10",
		"KVP_RATE_LIMIT_RPS":                "3.5",
		"KVP_RATE_LIMIT_BURST":              "7",
		"KVP_TRUSTED_PROXIES":               "10.0.0.0/8,192.168.1.1",
		"KVP_SHUTDOWN_TIMEOUT":              "7s",
		"KVP_LOG_LEVEL":                     "debug",
		"KVP_OTEL_SERVICE_NAME":             "custom",
		"KVP_OTEL_EXPORTER_OTLP_ENDPOINT":   "http://collector:4318",
		"GRAFANA_CLOUD_STACK_ID":            "12345",
		"GRAFANA_CLOUD_API_TOKEN":           "tok",
		"GRAFANA_CLOUD_REGION":              "prod-us-east-1",
		"KVP_BACKUP_DIR":                    "/backups",
		"KVP_BACKUP_INTERVAL":               "2h",
		"KVP_BACKUP_RETENTION":              "3",
		"KVP_CLEANUP_SWEEP_LIMIT":           "500",
	}))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Port != ":9000" {
		t.Errorf("Port = %q", cfg.Port)
	}
	if cfg.MetricsPort != "127.0.0.1:9100" {
		t.Errorf("MetricsPort = %q", cfg.MetricsPort)
	}
	if cfg.DBPath != "/tmp/kvp.db" {
		t.Errorf("DBPath = %q", cfg.DBPath)
	}
	if cfg.APIKey != "sekrit" {
		t.Errorf("APIKey = %q", cfg.APIKey)
	}
	if cfg.MaxBodyBytes != 2048 {
		t.Errorf("MaxBodyBytes = %d", cfg.MaxBodyBytes)
	}
	if cfg.MaxKeyBytes != 64 {
		t.Errorf("MaxKeyBytes = %d", cfg.MaxKeyBytes)
	}
	if cfg.MaxDBBytes != 1024 {
		t.Errorf("MaxDBBytes = %d", cfg.MaxDBBytes)
	}
	if cfg.TTL != 5*time.Minute {
		t.Errorf("TTL = %v", cfg.TTL)
	}
	if cfg.CleanupInterval != 10*time.Second {
		t.Errorf("CleanupInterval = %v", cfg.CleanupInterval)
	}
	if cfg.SizeCleanupThrottle != 15*time.Second {
		t.Errorf("SizeCleanupThrottle = %v", cfg.SizeCleanupThrottle)
	}
	if cfg.CleanupBatchSize != 50 {
		t.Errorf("CleanupBatchSize = %d", cfg.CleanupBatchSize)
	}
	if cfg.CleanupMaxRuns != 10 {
		t.Errorf("CleanupMaxRuns = %d", cfg.CleanupMaxRuns)
	}
	if cfg.RateLimitRPS != 3.5 {
		t.Errorf("RateLimitRPS = %v", cfg.RateLimitRPS)
	}
	if cfg.RateLimitBurst != 7 {
		t.Errorf("RateLimitBurst = %d", cfg.RateLimitBurst)
	}
	if len(cfg.TrustedProxies) != 2 {
		t.Fatalf("TrustedProxies = %v, want 2 entries", cfg.TrustedProxies)
	}
	if cfg.TrustedProxies[0].String() != "10.0.0.0/8" {
		t.Errorf("TrustedProxies[0] = %v", cfg.TrustedProxies[0])
	}
	if cfg.TrustedProxies[1].String() != "192.168.1.1/32" {
		t.Errorf("TrustedProxies[1] = %v", cfg.TrustedProxies[1])
	}
	if cfg.ShutdownTimeout != 7*time.Second {
		t.Errorf("ShutdownTimeout = %v", cfg.ShutdownTimeout)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q", cfg.LogLevel)
	}
	if cfg.OTELServiceName != "custom" {
		t.Errorf("OTELServiceName = %q", cfg.OTELServiceName)
	}
	if cfg.OTLPEndpoint != "http://collector:4318" {
		t.Errorf("OTLPEndpoint = %q", cfg.OTLPEndpoint)
	}
	if cfg.GrafanaCloudStackID != "12345" {
		t.Errorf("GrafanaCloudStackID = %q", cfg.GrafanaCloudStackID)
	}
	if cfg.GrafanaCloudAPIToken != "tok" {
		t.Errorf("GrafanaCloudAPIToken = %q", cfg.GrafanaCloudAPIToken)
	}
	if cfg.GrafanaCloudRegion != "prod-us-east-1" {
		t.Errorf("GrafanaCloudRegion = %q", cfg.GrafanaCloudRegion)
	}
	if cfg.BackupDir != "/backups" {
		t.Errorf("BackupDir = %q", cfg.BackupDir)
	}
	if cfg.BackupInterval != 2*time.Hour {
		t.Errorf("BackupInterval = %v", cfg.BackupInterval)
	}
	if cfg.BackupRetention != 3 {
		t.Errorf("BackupRetention = %d", cfg.BackupRetention)
	}
	if cfg.CleanupSweepLimit != 500 {
		t.Errorf("CleanupSweepLimit = %d, want 500", cfg.CleanupSweepLimit)
	}
}

func TestInvalidValueErrors(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"bad TTL", map[string]string{"KVP_TTL": "banana"}, "KVP_TTL"},
		{"zero TTL", map[string]string{"KVP_TTL": "0s"}, "KVP_TTL"},
		{"negative TTL", map[string]string{"KVP_TTL": "-1h"}, "KVP_TTL"},
		{"bad body bytes", map[string]string{"KVP_MAX_BODY_BYTES": "abc"}, "KVP_MAX_BODY_BYTES"},
		{"zero body bytes", map[string]string{"KVP_MAX_BODY_BYTES": "0"}, "KVP_MAX_BODY_BYTES"},
		{"bad key bytes", map[string]string{"KVP_MAX_KEY_BYTES": "-5"}, "KVP_MAX_KEY_BYTES"},
		{"bad db bytes", map[string]string{"KVP_MAX_DB_BYTES": "x"}, "KVP_MAX_DB_BYTES"},
		{"bad batch size", map[string]string{"KVP_CLEANUP_BATCH_SIZE": "0"}, "KVP_CLEANUP_BATCH_SIZE"},
		{"bad max runs", map[string]string{"KVP_CLEANUP_MAX_RUNS": "-1"}, "KVP_CLEANUP_MAX_RUNS"},
		{"bad rps", map[string]string{"KVP_RATE_LIMIT_RPS": "-1"}, "KVP_RATE_LIMIT_RPS"},
		{"bad burst", map[string]string{"KVP_RATE_LIMIT_BURST": "0"}, "KVP_RATE_LIMIT_BURST"},
		{"bad port", map[string]string{"KVP_PORT": "no-port"}, "KVP_PORT"},
		{"bad cleanup interval", map[string]string{"KVP_CLEANUP_INTERVAL": "1"}, "KVP_CLEANUP_INTERVAL"},
		{"bad shutdown timeout", map[string]string{"KVP_SHUTDOWN_TIMEOUT": "soon"}, "KVP_SHUTDOWN_TIMEOUT"},
		{"bad log level", map[string]string{"KVP_LOG_LEVEL": "loud"}, "KVP_LOG_LEVEL"},
		{"bad proxies", map[string]string{"KVP_TRUSTED_PROXIES": "999.1.1.1/33"}, "KVP_TRUSTED_PROXIES"},
		{"bad backup interval", map[string]string{"KVP_BACKUP_INTERVAL": "x"}, "KVP_BACKUP_INTERVAL"},
		{"bad backup retention", map[string]string{"KVP_BACKUP_RETENTION": "0"}, "KVP_BACKUP_RETENTION"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(envMap(tc.env))
			if err == nil {
				t.Fatalf("Parse(%v): nil error, want error mentioning %q", tc.env, tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Parse error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestBackupDisabledWhenDirEmpty(t *testing.T) {
	cfg, err := Parse(envMap(map[string]string{"KVP_BACKUP_DIR": ""}))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.BackupDir != "" {
		t.Errorf("BackupDir = %q, want empty", cfg.BackupDir)
	}
}

func TestSweepLimitZeroIsUnlimited(t *testing.T) {
	cfg, err := Parse(envMap(map[string]string{"KVP_CLEANUP_SWEEP_LIMIT": "0"}))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.CleanupSweepLimit != 0 {
		t.Errorf("CleanupSweepLimit = %d, want 0 (unlimited)", cfg.CleanupSweepLimit)
	}
}

func TestSweepLimitNegativeRejected(t *testing.T) {
	_, err := Parse(envMap(map[string]string{"KVP_CLEANUP_SWEEP_LIMIT": "-1"}))
	if err == nil {
		t.Fatal("Parse: nil error, want error for negative sweep limit")
	}
}

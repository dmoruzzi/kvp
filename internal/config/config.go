package config

import (
	"fmt"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds all runtime configuration parsed from environment variables.
// Every field has a documented default from the spec §11.
type Config struct {
	Port                 string
	MetricsPort          string
	DBPath               string
	APIKey               string
	MaxBodyBytes         int64
	MaxKeyBytes          int
	MaxDBBytes           int64
	MemoryCacheBytes     int64
	TTL                  time.Duration
	CleanupInterval      time.Duration
	SizeCleanupThrottle  time.Duration
	CleanupBatchSize     int
	CleanupMaxRuns       int
	RateLimitRPS         float64
	RateLimitBurst       int
	TrustedProxies       []netip.Prefix
	ShutdownTimeout      time.Duration
	LogLevel             string
	OTELServiceName      string
	OTLPEndpoint         string
	GrafanaCloudStackID  string
	GrafanaCloudAPIToken string
	GrafanaCloudRegion   string
	BackupDir            string
	BackupInterval       time.Duration
	BackupRetention      int
	CleanupSweepLimit    int
}

// Load parses configuration from the process environment.
func Load() (Config, error) {
	return Parse(os.Getenv)
}

// Parse builds a Config from the given env lookup, failing fast on the first
// invalid value with an error that names the offending variable.
func Parse(getenv func(string) string) (Config, error) {
	cfg := Config{
		Port:                 ":8080",
		MetricsPort:          "127.0.0.1:9090",
		DBPath:               "./kvp.db",
		MaxBodyBytes:         1048576,
		MaxKeyBytes:          256,
		MaxDBBytes:           67108864,
		TTL:                  24 * time.Hour,
		CleanupInterval:      time.Hour,
		SizeCleanupThrottle:  time.Minute,
		CleanupBatchSize:     1000,
		CleanupMaxRuns:       64,
		RateLimitRPS:         10,
		RateLimitBurst:       20,
		ShutdownTimeout:      15 * time.Second,
		LogLevel:             "info",
		OTELServiceName:      "kvp",
		GrafanaCloudRegion:   "prod-us-central-0",
		BackupInterval:       24 * time.Hour,
		BackupRetention:      7,
		CleanupSweepLimit:    10000,
		OTLPEndpoint:         getenv("KVP_OTEL_EXPORTER_OTLP_ENDPOINT"),
		GrafanaCloudStackID:  getenv("GRAFANA_CLOUD_STACK_ID"),
		GrafanaCloudAPIToken: getenv("GRAFANA_CLOUD_API_TOKEN"),
	}

	var err error
	assign := func(name string, set func(string) error) {
		if err != nil {
			return
		}
		if setErr := set(getenv(name)); setErr != nil {
			err = fmt.Errorf("%s: %w", name, setErr)
		}
	}

	assign("KVP_PORT", func(v string) error {
		if v != "" {
			cfg.Port = v
		}
		return validateListenAddr("KVP_PORT", cfg.Port)
	})
	assign("KVP_METRICS_PORT", func(v string) error {
		if v != "" {
			cfg.MetricsPort = v
		}
		return validateListenAddr("KVP_METRICS_PORT", cfg.MetricsPort)
	})
	assign("KVP_DB_PATH", func(v string) error {
		if v != "" {
			cfg.DBPath = v
		}
		return nil
	})
	assign("KVP_API_KEY", func(v string) error {
		cfg.APIKey = v
		return nil
	})
	assign("KVP_MAX_BODY_BYTES", func(v string) error {
		if v != "" {
			cfg.MaxBodyBytes, err = parsePositiveInt64(v)
		}
		return err
	})
	assign("KVP_MAX_KEY_BYTES", func(v string) error {
		if v != "" {
			cfg.MaxKeyBytes, err = parsePositiveInt(v)
		}
		return err
	})
	assign("KVP_MAX_DB_BYTES", func(v string) error {
		if v != "" {
			cfg.MaxDBBytes, err = parsePositiveInt64(v)
		}
		return err
	})
	memCacheSet := false
	memCacheMB := int64(0)
	assign("KVP_MEMORY_CACHE_MB", func(v string) error {
		if v != "" {
			memCacheMB, err = parseNonNegativeInt64(v)
			memCacheSet = true
		}
		return err
	})
	assign("KVP_TTL", func(v string) error {
		if v != "" {
			cfg.TTL, err = parsePositiveDuration(v)
		}
		return err
	})
	assign("KVP_CLEANUP_INTERVAL", func(v string) error {
		if v != "" {
			cfg.CleanupInterval, err = parsePositiveDuration(v)
		}
		return err
	})
	assign("KVP_SIZE_CLEANUP_THROTTLE", func(v string) error {
		if v != "" {
			cfg.SizeCleanupThrottle, err = parsePositiveDuration(v)
		}
		return err
	})
	assign("KVP_CLEANUP_BATCH_SIZE", func(v string) error {
		if v != "" {
			cfg.CleanupBatchSize, err = parsePositiveInt(v)
		}
		return err
	})
	assign("KVP_CLEANUP_MAX_RUNS", func(v string) error {
		if v != "" {
			cfg.CleanupMaxRuns, err = parsePositiveInt(v)
		}
		return err
	})
	assign("KVP_RATE_LIMIT_RPS", func(v string) error {
		if v != "" {
			cfg.RateLimitRPS, err = parsePositiveFloat(v)
		}
		return err
	})
	assign("KVP_RATE_LIMIT_BURST", func(v string) error {
		if v != "" {
			cfg.RateLimitBurst, err = parsePositiveInt(v)
		}
		return err
	})
	assign("KVP_SHUTDOWN_TIMEOUT", func(v string) error {
		if v != "" {
			cfg.ShutdownTimeout, err = parsePositiveDuration(v)
		}
		return err
	})
	assign("KVP_TRUSTED_PROXIES", func(v string) error {
		if v != "" {
			cfg.TrustedProxies, err = parsePrefixes(v)
		}
		return err
	})
	assign("KVP_LOG_LEVEL", func(v string) error {
		if v != "" {
			cfg.LogLevel = v
		}
		return validateLogLevel(cfg.LogLevel)
	})
	assign("KVP_OTEL_SERVICE_NAME", func(v string) error {
		if v != "" {
			cfg.OTELServiceName = v
		}
		return nil
	})
	assign("GRAFANA_CLOUD_REGION", func(v string) error {
		if v != "" {
			cfg.GrafanaCloudRegion = v
		}
		return nil
	})
	assign("KVP_BACKUP_DIR", func(v string) error {
		cfg.BackupDir = v
		return nil
	})
	assign("KVP_BACKUP_INTERVAL", func(v string) error {
		if v != "" {
			cfg.BackupInterval, err = parsePositiveDuration(v)
		}
		return err
	})
	assign("KVP_BACKUP_RETENTION", func(v string) error {
		if v != "" {
			cfg.BackupRetention, err = parsePositiveInt(v)
		}
		return err
	})
	assign("KVP_CLEANUP_SWEEP_LIMIT", func(v string) error {
		if v != "" {
			cfg.CleanupSweepLimit, err = parseNonNegativeInt(v)
		}
		return err
	})

	// Unset KVP_MEMORY_CACHE_MB binds the memory layer to the DB size budget;
	// "0" disables it (SQLite-only mode); N caps it at N MiB.
	if memCacheSet {
		cfg.MemoryCacheBytes = memCacheMB * 1024 * 1024
	} else {
		cfg.MemoryCacheBytes = cfg.MaxDBBytes
	}

	return cfg, err
}

func validateListenAddr(name, addr string) error {
	host, port, err := splitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid listen address %q", addr)
	}
	if port == "" {
		return fmt.Errorf("invalid listen address %q: missing port", addr)
	}
	if host != "" && host != "localhost" {
		if _, err := netip.ParseAddr(host); err != nil && host != "0.0.0.0" && host != "::" {
			return fmt.Errorf("invalid listen host %q in %s", host, name)
		}
	}
	return nil
}

func splitHostPort(addr string) (host, port string, err error) {
	i := strings.LastIndexByte(addr, ':')
	if i < 0 {
		return "", "", fmt.Errorf("missing port")
	}
	host = addr[:i]
	port = addr[i+1:]
	if port == "" {
		return "", "", fmt.Errorf("empty port")
	}
	for _, c := range port {
		if c < '0' || c > '9' {
			return "", "", fmt.Errorf("non-numeric port %q", port)
		}
	}
	return host, port, nil
}

func parsePositiveInt64(v string) (int64, error) {
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid integer %q", v)
	}
	if n <= 0 {
		return 0, fmt.Errorf("must be > 0, got %d", n)
	}
	return n, nil
}

func parseNonNegativeInt64(v string) (int64, error) {
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid integer %q", v)
	}
	if n < 0 {
		return 0, fmt.Errorf("must be >= 0, got %d", n)
	}
	return n, nil
}

func parsePositiveInt(v string) (int, error) {
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("invalid integer %q", v)
	}
	if n <= 0 {
		return 0, fmt.Errorf("must be > 0, got %d", n)
	}
	return n, nil
}

func parseNonNegativeInt(v string) (int, error) {
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("invalid integer %q", v)
	}
	if n < 0 {
		return 0, fmt.Errorf("must be >= 0, got %d", n)
	}
	return n, nil
}

func parsePositiveFloat(v string) (float64, error) {
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid number %q", v)
	}
	if f <= 0 {
		return 0, fmt.Errorf("must be > 0, got %v", f)
	}
	return f, nil
}

func parsePositiveDuration(v string) (time.Duration, error) {
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q", v)
	}
	if d <= 0 {
		return 0, fmt.Errorf("must be > 0, got %v", v)
	}
	return d, nil
}

func validateLogLevel(v string) error {
	switch v {
	case "debug", "info", "warn", "error":
		return nil
	}
	return fmt.Errorf("invalid log level %q (want debug|info|warn|error)", v)
}

func parsePrefixes(v string) ([]netip.Prefix, error) {
	parts := strings.Split(v, ",")
	out := make([]netip.Prefix, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if !strings.Contains(p, "/") {
			p += "/32"
		}
		prefix, err := netip.ParsePrefix(p)
		if err != nil {
			return nil, fmt.Errorf("invalid CIDR %q", p)
		}
		out = append(out, prefix.Masked())
	}
	return out, nil
}

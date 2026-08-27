package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type RateLimiterConfig struct {
	DefaultRate  int  `yaml:"default_rate"`
	DefaultBurst int  `yaml:"default_burst"`
	ShadowMode   bool `yaml:"shadow_mode"`
}

type ConcurrencyLimiterConfig struct {
	DefaultMaxConcurrent int   `yaml:"default_max_concurrent"`
	MaxRequestDurationMs int64 `yaml:"max_request_duration_ms"`
	ShadowMode           bool  `yaml:"shadow_mode"`
	QueueingEnabled      bool  `yaml:"queueing_enabled"`
	MaxBacklog           int   `yaml:"max_backlog"`
	MaxQueueWaitMs       int64 `yaml:"max_queue_wait_ms"`
	PollIntervalMs       int64 `yaml:"poll_interval_ms"`
}

type FleetShedderConfig struct {
	DefaultMaxConcurrent int    `yaml:"default_max_concurrent"`
	ReservedCriticalPct  int    `yaml:"reserved_critical_pct"`
	MaxRequestDurationMs int64  `yaml:"max_request_duration_ms"`
	DefaultPriority      string `yaml:"default_priority"`
	ShadowMode           bool   `yaml:"shadow_mode"`
}

type Config struct {
	SyncRate int `yaml:"sync_rate"`
	Tiers    struct {
		RateLimiter        RateLimiterConfig        `yaml:"rate_limiter"`
		ConcurrencyLimiter ConcurrencyLimiterConfig `yaml:"concurrency_limiter"`
		FleetShedder       FleetShedderConfig       `yaml:"fleet_shedder"`
	} `yaml:"tiers"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}

	return &cfg, nil
}

func (c *Config) Validate() error {
	if c.Tiers.RateLimiter.DefaultRate <= 0 {
		return fmt.Errorf("tiers.rate_limiter.default_rate must be > 0, got %d (is the rate_limiter block missing from your config?)", c.Tiers.RateLimiter.DefaultRate)
	}
	if c.Tiers.RateLimiter.DefaultBurst < 0 {
		return fmt.Errorf("tiers.rate_limiter.default_burst must be >= 0, got %d", c.Tiers.RateLimiter.DefaultBurst)
	}
	if c.Tiers.ConcurrencyLimiter.DefaultMaxConcurrent <= 0 {
		return fmt.Errorf("tiers.concurrency_limiter.default_max_concurrent must be > 0, got %d (is the concurrency_limiter block missing from your config?)", c.Tiers.ConcurrencyLimiter.DefaultMaxConcurrent)
	}
	if c.Tiers.FleetShedder.DefaultMaxConcurrent <= 0 {
		return fmt.Errorf("tiers.fleet_shedder.default_max_concurrent must be > 0, got %d (is the fleet_shedder block missing from your config?)", c.Tiers.FleetShedder.DefaultMaxConcurrent)
	}
	if c.Tiers.FleetShedder.ReservedCriticalPct < 0 || c.Tiers.FleetShedder.ReservedCriticalPct > 100 {
		return fmt.Errorf("tiers.fleet_shedder.reserved_critical_pct must be between 0 and 100 inclusive, got %d", c.Tiers.FleetShedder.ReservedCriticalPct)
	}
	if c.Tiers.ConcurrencyLimiter.QueueingEnabled {
		if c.Tiers.ConcurrencyLimiter.MaxBacklog <= 0 {
			return fmt.Errorf("tiers.concurrency_limiter.max_backlog must be > 0 when queueing_enabled is true, got %d", c.Tiers.ConcurrencyLimiter.MaxBacklog)
		}
		if c.Tiers.ConcurrencyLimiter.MaxQueueWaitMs <= 0 {
			return fmt.Errorf("tiers.concurrency_limiter.max_queue_wait_ms must be > 0 when queueing_enabled is true, got %d", c.Tiers.ConcurrencyLimiter.MaxQueueWaitMs)
		}
		if c.Tiers.ConcurrencyLimiter.PollIntervalMs <= 0 {
			return fmt.Errorf("tiers.concurrency_limiter.poll_interval_ms must be > 0 when queueing_enabled is true, got %d", c.Tiers.ConcurrencyLimiter.PollIntervalMs)
		}
		if c.Tiers.ConcurrencyLimiter.PollIntervalMs > c.Tiers.ConcurrencyLimiter.MaxQueueWaitMs {
			return fmt.Errorf("tiers.concurrency_limiter.poll_interval_ms (%d) must not exceed max_queue_wait_ms (%d) — a waiter would never get a chance to poll before timing out", c.Tiers.ConcurrencyLimiter.PollIntervalMs, c.Tiers.ConcurrencyLimiter.MaxQueueWaitMs)
		}
	}
	return nil
}

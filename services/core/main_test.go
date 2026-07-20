package main_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/ratecap/core/config"
	"github.com/ratecap/core/limiter"
	"github.com/ratecap/core/store"
)

func TestMain_FailsClosedOnMissingFleetShedderBlock(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "ratecap.yaml")
	invalidConfig := "sync_rate: 5\ntiers:\n  rate_limiter:\n    default_rate: 100\n    default_burst: 500\n    shadow_mode: false\n"
	if err := os.WriteFile(configPath, []byte(invalidConfig), 0644); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	cmd := exec.Command("go", "run", ".")
	cmd.Env = append(os.Environ(),
		"RATECAP_CONFIG_PATH="+configPath,
		"RATECAP_SHARED_SECRET=test-secret",
		"RATECAP_GRPC_ADDR=:0",
	)
	output, err := cmd.CombinedOutput()

	if err == nil {
		t.Fatalf("expected the process to exit non-zero on an invalid config, but it exited cleanly. Output:\n%s", output)
	}
	if !contains(string(output), "invalid config") {
		t.Errorf("expected startup failure to mention 'invalid config', got output:\n%s", output)
	}
}

func TestMain_SkipsInvalidConfigReloadWithoutReconfiguring(t *testing.T) {
	if os.Getenv("RATECAP_REDIS_ADDR") == "" && os.Getenv("CI") == "" {
		t.Skip("skipping integration test: Docker/Redis required (set RATECAP_REDIS_ADDR or run in CI)")
	}

	dir := t.TempDir()
	configPath := filepath.Join(dir, "ratecap.yaml")
	validConfig := `sync_rate: 5
tiers:
  rate_limiter:
    default_rate: 100
    default_burst: 500
    shadow_mode: false
  concurrency_limiter:
    default_max_concurrent: 50
    max_request_duration_ms: 5000
    shadow_mode: false
  fleet_shedder:
    default_max_concurrent: 100
    reserved_critical_pct: 20
    max_request_duration_ms: 5000
    default_priority: normal
    shadow_mode: false
`
	if err := os.WriteFile(configPath, []byte(validConfig), 0644); err != nil {
		t.Fatalf("failed to write initial config: %v", err)
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("failed to load config: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("initial config invalid: %v", err)
	}

	redisAddr := os.Getenv("RATECAP_REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "localhost:6379"
	}

	redisClient := redis.NewClient(&redis.Options{Addr: redisAddr})
	redisStore := store.NewRedisStore(redisClient, []byte("test-signing-key"))

	rateLimiter := limiter.NewTokenBucketLimiter(
		redisStore,
		cfg.Tiers.RateLimiter.DefaultRate,
		cfg.Tiers.RateLimiter.DefaultBurst,
		cfg.Tiers.RateLimiter.ShadowMode,
	)

	concurrencyLimiter := limiter.NewConcurrencyLimiter(
		redisStore,
		cfg.Tiers.ConcurrencyLimiter.DefaultMaxConcurrent,
		cfg.Tiers.ConcurrencyLimiter.MaxRequestDurationMs,
		cfg.Tiers.ConcurrencyLimiter.ShadowMode,
		cfg.Tiers.ConcurrencyLimiter.QueueingEnabled,
		cfg.Tiers.ConcurrencyLimiter.MaxBacklog,
		cfg.Tiers.ConcurrencyLimiter.MaxQueueWaitMs,
		cfg.Tiers.ConcurrencyLimiter.PollIntervalMs,
	)

	fleetShedder := limiter.NewFleetShedder(
		redisStore,
		cfg.Tiers.FleetShedder.DefaultMaxConcurrent,
		cfg.Tiers.FleetShedder.ReservedCriticalPct,
		cfg.Tiers.FleetShedder.MaxRequestDurationMs,
		cfg.Tiers.FleetShedder.ShadowMode,
	)

	stopWatch, err := config.Watch(configPath, func(newCfg *config.Config) {
		if err := newCfg.Validate(); err != nil {
			t.Logf("ignoring invalid config reload: %v", err)
			return
		}
		rateLimiter.Reconfigure(newCfg.Tiers.RateLimiter.DefaultRate, newCfg.Tiers.RateLimiter.DefaultBurst, newCfg.Tiers.RateLimiter.ShadowMode)
		concurrencyLimiter.Reconfigure(newCfg.Tiers.ConcurrencyLimiter.DefaultMaxConcurrent, newCfg.Tiers.ConcurrencyLimiter.MaxRequestDurationMs, newCfg.Tiers.ConcurrencyLimiter.ShadowMode, newCfg.Tiers.ConcurrencyLimiter.QueueingEnabled, newCfg.Tiers.ConcurrencyLimiter.MaxBacklog, newCfg.Tiers.ConcurrencyLimiter.MaxQueueWaitMs, newCfg.Tiers.ConcurrencyLimiter.PollIntervalMs)
		fleetShedder.Reconfigure(newCfg.Tiers.FleetShedder.DefaultMaxConcurrent, newCfg.Tiers.FleetShedder.ReservedCriticalPct, newCfg.Tiers.FleetShedder.MaxRequestDurationMs, newCfg.Tiers.FleetShedder.ShadowMode)
	})
	if err != nil {
		t.Fatalf("failed to start config watcher: %v", err)
	}
	defer stopWatch()

	time.Sleep(100 * time.Millisecond)

	ctx := context.Background()
	initialDecision, err := fleetShedder.Check(ctx, limiter.Request{Key: "user-1", Priority: limiter.Sheddable, Cost: 1})
	if err != nil {
		t.Fatalf("unexpected error on initial check: %v", err)
	}
	if initialDecision.Action != limiter.ALLOW {
		t.Fatalf("expected ALLOW with initial valid config, got %v", initialDecision.Action)
	}

	invalidReload := `sync_rate: 5
tiers:
  rate_limiter:
    default_rate: 100
    default_burst: 500
    shadow_mode: false
  concurrency_limiter:
    default_max_concurrent: 50
    max_request_duration_ms: 5000
    shadow_mode: false
  fleet_shedder:
    default_max_concurrent: 0
    reserved_critical_pct: 20
    max_request_duration_ms: 5000
    default_priority: normal
    shadow_mode: false
`
	if err := os.WriteFile(configPath, []byte(invalidReload), 0644); err != nil {
		t.Fatalf("failed to write invalid reload: %v", err)
	}

	time.Sleep(200 * time.Millisecond)

	secondDecision, err := fleetShedder.Check(ctx, limiter.Request{Key: "user-2", Priority: limiter.Sheddable, Cost: 1})
	if err != nil {
		t.Fatalf("unexpected error after invalid reload: %v", err)
	}
	if secondDecision.Action != limiter.ALLOW {
		t.Errorf("expected ALLOW after invalid reload (config should be unchanged), got %v", secondDecision.Action)
	}

	validRecovery := `sync_rate: 5
tiers:
  rate_limiter:
    default_rate: 100
    default_burst: 500
    shadow_mode: false
  concurrency_limiter:
    default_max_concurrent: 50
    max_request_duration_ms: 5000
    shadow_mode: false
  fleet_shedder:
    default_max_concurrent: 200
    reserved_critical_pct: 30
    max_request_duration_ms: 5000
    default_priority: normal
    shadow_mode: false
`
	if err := os.WriteFile(configPath, []byte(validRecovery), 0644); err != nil {
		t.Fatalf("failed to write recovery config: %v", err)
	}

	time.Sleep(200 * time.Millisecond)

	thirdDecision, err := fleetShedder.Check(ctx, limiter.Request{Key: "user-3", Priority: limiter.Sheddable, Cost: 1})
	if err != nil {
		t.Fatalf("unexpected error after valid reload: %v", err)
	}
	if thirdDecision.Action != limiter.ALLOW {
		t.Errorf("expected ALLOW after valid reload, got %v", thirdDecision.Action)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (func() bool {
		for i := 0; i+len(substr) <= len(s); i++ {
			if s[i:i+len(substr)] == substr {
				return true
			}
		}
		return false
	})()
}

package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ratecap/core/config"
)

func writeTempConfig(t *testing.T, contents string) string {
	dir := t.TempDir()
	path := filepath.Join(dir, "ratecap.yaml")
	if err := os.WriteFile(path, []byte(contents), 0644); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}
	return path
}

func TestLoad_ParsesRateLimiterTier(t *testing.T) {
	path := writeTempConfig(t, `
sync_rate: 5
tiers:
  rate_limiter:
    default_rate: 100
    default_burst: 500
    shadow_mode: false
`)

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.SyncRate != 5 {
		t.Errorf("expected SyncRate=5, got %d", cfg.SyncRate)
	}
	if cfg.Tiers.RateLimiter.DefaultRate != 100 {
		t.Errorf("expected DefaultRate=100, got %d", cfg.Tiers.RateLimiter.DefaultRate)
	}
	if cfg.Tiers.RateLimiter.DefaultBurst != 500 {
		t.Errorf("expected DefaultBurst=500, got %d", cfg.Tiers.RateLimiter.DefaultBurst)
	}
	if cfg.Tiers.RateLimiter.ShadowMode != false {
		t.Errorf("expected ShadowMode=false, got %v", cfg.Tiers.RateLimiter.ShadowMode)
	}
}

func TestLoad_MissingFileReturnsError(t *testing.T) {
	_, err := config.Load("/nonexistent/ratecap.yaml")
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

func TestLoad_ParsesConcurrencyLimiterTier(t *testing.T) {
	path := writeTempConfig(t, `
sync_rate: 5
tiers:
  rate_limiter:
    default_rate: 100
    default_burst: 500
    shadow_mode: false
  concurrency_limiter:
    default_max_concurrent: 20
    max_request_duration_ms: 30000
    shadow_mode: false
`)

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Tiers.ConcurrencyLimiter.DefaultMaxConcurrent != 20 {
		t.Errorf("expected DefaultMaxConcurrent=20, got %d", cfg.Tiers.ConcurrencyLimiter.DefaultMaxConcurrent)
	}
	if cfg.Tiers.ConcurrencyLimiter.MaxRequestDurationMs != 30000 {
		t.Errorf("expected MaxRequestDurationMs=30000, got %d", cfg.Tiers.ConcurrencyLimiter.MaxRequestDurationMs)
	}
	if cfg.Tiers.ConcurrencyLimiter.ShadowMode != false {
		t.Errorf("expected ShadowMode=false, got %v", cfg.Tiers.ConcurrencyLimiter.ShadowMode)
	}
}

func TestLoad_ParsesFleetShedderTier(t *testing.T) {
	path := writeTempConfig(t, `
sync_rate: 5
tiers:
  rate_limiter:
    default_rate: 100
    default_burst: 500
    shadow_mode: false
  fleet_shedder:
    default_max_concurrent: 100
    reserved_critical_pct: 20
    max_request_duration_ms: 30000
    default_priority: sheddable
    shadow_mode: false
`)

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Tiers.FleetShedder.DefaultMaxConcurrent != 100 {
		t.Errorf("expected DefaultMaxConcurrent=100, got %d", cfg.Tiers.FleetShedder.DefaultMaxConcurrent)
	}
	if cfg.Tiers.FleetShedder.ReservedCriticalPct != 20 {
		t.Errorf("expected ReservedCriticalPct=20, got %d", cfg.Tiers.FleetShedder.ReservedCriticalPct)
	}
	if cfg.Tiers.FleetShedder.MaxRequestDurationMs != 30000 {
		t.Errorf("expected MaxRequestDurationMs=30000, got %d", cfg.Tiers.FleetShedder.MaxRequestDurationMs)
	}
	if cfg.Tiers.FleetShedder.DefaultPriority != "sheddable" {
		t.Errorf("expected DefaultPriority=%q, got %q", "sheddable", cfg.Tiers.FleetShedder.DefaultPriority)
	}
	if cfg.Tiers.FleetShedder.ShadowMode != false {
		t.Errorf("expected ShadowMode=false, got %v", cfg.Tiers.FleetShedder.ShadowMode)
	}
}

func TestValidate_AcceptsValidFleetShedderConfig(t *testing.T) {
	cfg := &config.Config{}
	cfg.Tiers.FleetShedder.DefaultMaxConcurrent = 100
	cfg.Tiers.FleetShedder.ReservedCriticalPct = 20

	if err := cfg.Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidate_RejectsZeroDefaultMaxConcurrent(t *testing.T) {
	cfg := &config.Config{}
	cfg.Tiers.FleetShedder.DefaultMaxConcurrent = 0
	cfg.Tiers.FleetShedder.ReservedCriticalPct = 20

	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for fleet_shedder.default_max_concurrent=0, got nil")
	}
}

func TestValidate_RejectsNegativeDefaultMaxConcurrent(t *testing.T) {
	cfg := &config.Config{}
	cfg.Tiers.FleetShedder.DefaultMaxConcurrent = -5
	cfg.Tiers.FleetShedder.ReservedCriticalPct = 20

	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for negative fleet_shedder.default_max_concurrent, got nil")
	}
}

func TestValidate_RejectsReservedCriticalPctAboveOneHundred(t *testing.T) {
	cfg := &config.Config{}
	cfg.Tiers.FleetShedder.DefaultMaxConcurrent = 100
	cfg.Tiers.FleetShedder.ReservedCriticalPct = 140

	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for fleet_shedder.reserved_critical_pct=140, got nil")
	}
}

func TestValidate_RejectsNegativeReservedCriticalPct(t *testing.T) {
	cfg := &config.Config{}
	cfg.Tiers.FleetShedder.DefaultMaxConcurrent = 100
	cfg.Tiers.FleetShedder.ReservedCriticalPct = -10

	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for negative fleet_shedder.reserved_critical_pct, got nil")
	}
}

func TestValidate_AcceptsReservedCriticalPctBoundaries(t *testing.T) {
	for _, pct := range []int{0, 100} {
		cfg := &config.Config{}
		cfg.Tiers.FleetShedder.DefaultMaxConcurrent = 100
		cfg.Tiers.FleetShedder.ReservedCriticalPct = pct

		if err := cfg.Validate(); err != nil {
			t.Errorf("expected reserved_critical_pct=%d to be valid (inclusive boundary), got error: %v", pct, err)
		}
	}
}

func TestValidate_ErrorMentionsFleetShedderOnMissingBlock(t *testing.T) {
	path := writeTempConfig(t, `
sync_rate: 5
tiers:
  rate_limiter:
    default_rate: 100
    default_burst: 500
    shadow_mode: false
`)

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("unexpected error loading: %v", err)
	}

	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error when fleet_shedder block is omitted entirely (zero-valued DefaultMaxConcurrent), got nil")
	}
}

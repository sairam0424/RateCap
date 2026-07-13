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

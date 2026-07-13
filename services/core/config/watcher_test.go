package config_test

import (
	"os"
	"testing"
	"time"

	"github.com/ratecap/core/config"
)

func TestWatch_TriggersOnChangeOnFileWrite(t *testing.T) {
	path := writeTempConfig(t, `
sync_rate: 5
tiers:
  rate_limiter:
    default_rate: 100
    default_burst: 500
    shadow_mode: false
`)

	changes := make(chan *config.Config, 1)
	stop, err := config.Watch(path, func(cfg *config.Config) {
		changes <- cfg
	})
	if err != nil {
		t.Fatalf("unexpected error starting watch: %v", err)
	}
	defer stop()

	time.Sleep(100 * time.Millisecond)

	newContents := `
sync_rate: 10
tiers:
  rate_limiter:
    default_rate: 200
    default_burst: 1000
    shadow_mode: true
`
	if err := os.WriteFile(path, []byte(newContents), 0644); err != nil {
		t.Fatalf("failed to update config: %v", err)
	}

	select {
	case cfg := <-changes:
		if cfg.Tiers.RateLimiter.DefaultRate != 200 {
			t.Errorf("expected reloaded DefaultRate=200, got %d", cfg.Tiers.RateLimiter.DefaultRate)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for config reload callback")
	}
}

package config_test

import (
	"os"
	"testing"
	"time"

	"github.com/sairam0424/RateCap/services/core/config"
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
	stop, err := config.Watch(path, func(cfg *config.Config, err error) {
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

func TestWatch_DebouncesRapidFireEvents(t *testing.T) {
	path := writeTempConfig(t, `
sync_rate: 5
tiers:
  rate_limiter:
    default_rate: 100
    default_burst: 500
    shadow_mode: false
`)

	changes := make(chan *config.Config, 10)
	stop, err := config.Watch(path, func(cfg *config.Config, err error) {
		changes <- cfg
	})
	if err != nil {
		t.Fatalf("unexpected error starting watch: %v", err)
	}
	defer stop()

	time.Sleep(100 * time.Millisecond)

	firstContents := `
sync_rate: 10
tiers:
  rate_limiter:
    default_rate: 200
    default_burst: 1000
    shadow_mode: true
`
	secondContents := `
sync_rate: 20
tiers:
  rate_limiter:
    default_rate: 300
    default_burst: 1500
    shadow_mode: false
`
	if err := os.WriteFile(path, []byte(firstContents), 0644); err != nil {
		t.Fatalf("failed to write first rapid update: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(path, []byte(secondContents), 0644); err != nil {
		t.Fatalf("failed to write second rapid update: %v", err)
	}

	var cfg *config.Config
	select {
	case cfg = <-changes:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for debounced config reload callback")
	}

	select {
	case extra := <-changes:
		t.Fatalf("expected exactly one onChange call for two rapid-fire writes, got a second callback with SyncRate=%d", extra.SyncRate)
	case <-time.After(300 * time.Millisecond):
	}

	if cfg.SyncRate != 20 {
		t.Errorf("expected debounced reload to reflect the LAST write (SyncRate=20), got %d", cfg.SyncRate)
	}
	if cfg.Tiers.RateLimiter.DefaultRate != 300 {
		t.Errorf("expected debounced reload to reflect the LAST write (DefaultRate=300), got %d", cfg.Tiers.RateLimiter.DefaultRate)
	}
}

func TestWatch_SkipsInvalidConfigWithoutCrashing(t *testing.T) {
	path := writeTempConfig(t, `
sync_rate: 5
tiers:
  rate_limiter:
    default_rate: 100
    default_burst: 500
    shadow_mode: false
`)

	changes := make(chan *config.Config, 1)
	stop, err := config.Watch(path, func(cfg *config.Config, err error) {
		changes <- cfg
	})
	if err != nil {
		t.Fatalf("unexpected error starting watch: %v", err)
	}
	defer stop()

	time.Sleep(100 * time.Millisecond)

	invalidContents := `
sync_rate: "not a number"
tiers:
  rate_limiter:
    default_rate: invalid
`
	if err := os.WriteFile(path, []byte(invalidContents), 0644); err != nil {
		t.Fatalf("failed to update config: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	select {
	case cfg := <-changes:
		// A Load failure now invokes onChange(nil, err) so main.go can record
		// a config_reload_total{result="failure"} metric — this is the new
		// contract, so the callback firing is fine as long as it never
		// surfaces a valid cfg for input that failed to parse.
		if cfg != nil {
			t.Errorf("onChange should not be called with a valid cfg for invalid config, got %+v", cfg)
		}
	case <-time.After(500 * time.Millisecond):
	}

	validContents := `
sync_rate: 15
tiers:
  rate_limiter:
    default_rate: 300
    default_burst: 1500
    shadow_mode: true
`
	if err := os.WriteFile(path, []byte(validContents), 0644); err != nil {
		t.Fatalf("failed to write valid config: %v", err)
	}

	select {
	case cfg := <-changes:
		if cfg.SyncRate != 15 {
			t.Errorf("expected recovery to valid config with SyncRate=15, got %d", cfg.SyncRate)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for recovery after invalid config")
	}
}

func TestWatch_CallsOnChangeWithErrorWhenLoadFails(t *testing.T) {
	path := writeTempConfig(t, `
sync_rate: 5
tiers:
  rate_limiter:
    default_rate: 100
    default_burst: 500
    shadow_mode: false
`)

	type result struct {
		cfg *config.Config
		err error
	}
	changes := make(chan result, 1)
	stop, err := config.Watch(path, func(cfg *config.Config, loadErr error) {
		changes <- result{cfg, loadErr}
	})
	if err != nil {
		t.Fatalf("unexpected error starting watch: %v", err)
	}
	defer stop()

	time.Sleep(100 * time.Millisecond)

	malformedYAML := "not: valid: yaml: at: all: [unterminated"
	if err := os.WriteFile(path, []byte(malformedYAML), 0644); err != nil {
		t.Fatalf("failed to write malformed config: %v", err)
	}

	select {
	case r := <-changes:
		if r.err == nil {
			t.Error("expected onChange to be called with a non-nil error for malformed YAML")
		}
		if r.cfg != nil {
			t.Errorf("expected cfg=nil when Load fails, got %+v", r.cfg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for onChange to be called with the Load error")
	}
}

func TestWatch_SurvivesPartialWrite(t *testing.T) {
	path := writeTempConfig(t, `
sync_rate: 5
tiers:
  rate_limiter:
    default_rate: 100
    default_burst: 500
    shadow_mode: false
`)

	changes := make(chan *config.Config, 5)
	stop, err := config.Watch(path, func(cfg *config.Config, loadErr error) {
		if loadErr == nil {
			changes <- cfg
		}
	})
	if err != nil {
		t.Fatalf("unexpected error starting watch: %v", err)
	}
	defer stop()
	time.Sleep(100 * time.Millisecond)

	// Simulate a partial write: truncate then write only the first half of
	// valid YAML, without the closing content — a real interrupted-write
	// scenario, distinct from TestWatch_SkipsInvalidConfigWithoutCrashing's
	// well-formed-but-semantically-invalid case.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		t.Fatalf("failed to open for partial write: %v", err)
	}
	if _, err := f.WriteString("sync_rate: 10\ntiers:\n  rate_lim"); err != nil {
		t.Fatalf("failed to write partial content: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("failed to close after partial write: %v", err)
	}

	time.Sleep(150 * time.Millisecond)
	select {
	case cfg := <-changes:
		t.Errorf("expected no onChange for a partial/truncated write, got a config with SyncRate=%d", cfg.SyncRate)
	default:
	}

	validContents := `
sync_rate: 20
tiers:
  rate_limiter:
    default_rate: 300
    default_burst: 1500
    shadow_mode: false
`
	if err := os.WriteFile(path, []byte(validContents), 0644); err != nil {
		t.Fatalf("failed to write recovery content: %v", err)
	}

	select {
	case cfg := <-changes:
		if cfg.SyncRate != 20 {
			t.Errorf("expected recovery reload with SyncRate=20, got %d", cfg.SyncRate)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for recovery reload after a partial write")
	}
}

func TestWatch_SurvivesAtomicRenameSwap(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/ratecap.yaml"
	if err := os.WriteFile(path, []byte(`
sync_rate: 5
tiers:
  rate_limiter:
    default_rate: 100
    default_burst: 500
    shadow_mode: false
`), 0644); err != nil {
		t.Fatalf("failed to write initial config: %v", err)
	}

	changes := make(chan *config.Config, 5)
	stop, err := config.Watch(path, func(cfg *config.Config, loadErr error) {
		if loadErr == nil {
			changes <- cfg
		}
	})
	if err != nil {
		t.Fatalf("unexpected error starting watch: %v", err)
	}
	defer stop()
	time.Sleep(100 * time.Millisecond)

	// Atomic rename-swap: write to a temp file in the same directory, then
	// os.Rename over the watched path — the pattern tools like Kubernetes
	// ConfigMap volume mounts and `mv` use, distinct from an in-place write.
	tmpPath := dir + "/ratecap.yaml.tmp"
	if err := os.WriteFile(tmpPath, []byte(`
sync_rate: 30
tiers:
  rate_limiter:
    default_rate: 400
    default_burst: 2000
    shadow_mode: false
`), 0644); err != nil {
		t.Fatalf("failed to write temp file: %v", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		t.Fatalf("failed to atomically rename over watched path: %v", err)
	}

	select {
	case cfg := <-changes:
		if cfg.SyncRate != 30 {
			t.Errorf("expected reload via rename-swap with SyncRate=30, got %d", cfg.SyncRate)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for reload after an atomic rename-swap")
	}
}

func TestWatch_SurvivesDeleteAndRecreate(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/ratecap.yaml"
	if err := os.WriteFile(path, []byte(`
sync_rate: 5
tiers:
  rate_limiter:
    default_rate: 100
    default_burst: 500
    shadow_mode: false
`), 0644); err != nil {
		t.Fatalf("failed to write initial config: %v", err)
	}

	changes := make(chan *config.Config, 5)
	stop, err := config.Watch(path, func(cfg *config.Config, loadErr error) {
		if loadErr == nil {
			changes <- cfg
		}
	})
	if err != nil {
		t.Fatalf("unexpected error starting watch: %v", err)
	}
	defer stop()
	time.Sleep(100 * time.Millisecond)

	if err := os.Remove(path); err != nil {
		t.Fatalf("failed to delete watched file: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	if err := os.WriteFile(path, []byte(`
sync_rate: 40
tiers:
  rate_limiter:
    default_rate: 500
    default_burst: 2500
    shadow_mode: false
`), 0644); err != nil {
		t.Fatalf("failed to recreate watched file: %v", err)
	}

	select {
	case cfg := <-changes:
		if cfg.SyncRate != 40 {
			t.Errorf("expected reload after delete+recreate with SyncRate=40, got %d", cfg.SyncRate)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for reload after delete+recreate — fsnotify.Add watches a directory here, so a recreated file under the same dir should still be seen")
	}
}

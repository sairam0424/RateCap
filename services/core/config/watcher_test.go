package config_test

import (
	"os"
	"testing"
	"time"

	"github.com/sairam0424/RateCap/services/core/config"
)

func TestWatch_TriggersOnChangeOnFileWrite(t *testing.T) {
	path := writeTempConfig(t, `
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
tiers:
  rate_limiter:
    default_rate: 200
    default_burst: 1000
    shadow_mode: true
`
	if err := os.WriteFile(path, []byte(newContents), 0600); err != nil {
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
tiers:
  rate_limiter:
    default_rate: 200
    default_burst: 1000
    shadow_mode: true
`
	secondContents := `
tiers:
  rate_limiter:
    default_rate: 300
    default_burst: 1500
    shadow_mode: false
`
	if err := os.WriteFile(path, []byte(firstContents), 0600); err != nil {
		t.Fatalf("failed to write first rapid update: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(path, []byte(secondContents), 0600); err != nil {
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
		t.Fatalf("expected exactly one onChange call for two rapid-fire writes, got a second callback with DefaultRate=%d", extra.Tiers.RateLimiter.DefaultRate)
	case <-time.After(300 * time.Millisecond):
	}

	if cfg.Tiers.RateLimiter.DefaultRate != 300 {
		t.Errorf("expected debounced reload to reflect the LAST write (DefaultRate=300), got %d", cfg.Tiers.RateLimiter.DefaultRate)
	}
}

func TestWatch_SkipsInvalidConfigWithoutCrashing(t *testing.T) {
	path := writeTempConfig(t, `
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
tiers:
  rate_limiter:
    default_rate: invalid
`
	if err := os.WriteFile(path, []byte(invalidContents), 0600); err != nil {
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
tiers:
  rate_limiter:
    default_rate: 300
    default_burst: 1500
    shadow_mode: true
`
	if err := os.WriteFile(path, []byte(validContents), 0600); err != nil {
		t.Fatalf("failed to write valid config: %v", err)
	}

	select {
	case cfg := <-changes:
		if cfg.Tiers.RateLimiter.DefaultRate != 300 {
			t.Errorf("expected recovery to valid config with DefaultRate=300, got %d", cfg.Tiers.RateLimiter.DefaultRate)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for recovery after invalid config")
	}
}

func TestWatch_CallsOnChangeWithErrorWhenLoadFails(t *testing.T) {
	path := writeTempConfig(t, `
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
	if err := os.WriteFile(path, []byte(malformedYAML), 0600); err != nil {
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
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0600) //nolint:gosec // path is from writeTempConfig, a test-owned t.TempDir() fixture
	if err != nil {
		t.Fatalf("failed to open for partial write: %v", err)
	}
	if _, err := f.WriteString("tiers:\n  rate_lim"); err != nil {
		t.Fatalf("failed to write partial content: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("failed to close after partial write: %v", err)
	}

	time.Sleep(150 * time.Millisecond)
	select {
	case cfg := <-changes:
		t.Errorf("expected no onChange for a partial/truncated write, got a config with DefaultRate=%d", cfg.Tiers.RateLimiter.DefaultRate)
	default:
	}

	validContents := `
tiers:
  rate_limiter:
    default_rate: 300
    default_burst: 1500
    shadow_mode: false
`
	if err := os.WriteFile(path, []byte(validContents), 0600); err != nil {
		t.Fatalf("failed to write recovery content: %v", err)
	}

	select {
	case cfg := <-changes:
		if cfg.Tiers.RateLimiter.DefaultRate != 300 {
			t.Errorf("expected recovery reload with DefaultRate=300, got %d", cfg.Tiers.RateLimiter.DefaultRate)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for recovery reload after a partial write")
	}
}

func TestWatch_SurvivesAtomicRenameSwap(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/ratecap.yaml"
	if err := os.WriteFile(path, []byte(`
tiers:
  rate_limiter:
    default_rate: 100
    default_burst: 500
    shadow_mode: false
`), 0600); err != nil {
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
tiers:
  rate_limiter:
    default_rate: 400
    default_burst: 2000
    shadow_mode: false
`), 0600); err != nil {
		t.Fatalf("failed to write temp file: %v", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		t.Fatalf("failed to atomically rename over watched path: %v", err)
	}

	select {
	case cfg := <-changes:
		if cfg.Tiers.RateLimiter.DefaultRate != 400 {
			t.Errorf("expected reload via rename-swap with DefaultRate=400, got %d", cfg.Tiers.RateLimiter.DefaultRate)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for reload after an atomic rename-swap")
	}
}

func TestWatch_SurvivesDeleteAndRecreate(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/ratecap.yaml"
	if err := os.WriteFile(path, []byte(`
tiers:
  rate_limiter:
    default_rate: 100
    default_burst: 500
    shadow_mode: false
`), 0600); err != nil {
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
tiers:
  rate_limiter:
    default_rate: 500
    default_burst: 2500
    shadow_mode: false
`), 0600); err != nil {
		t.Fatalf("failed to recreate watched file: %v", err)
	}

	select {
	case cfg := <-changes:
		if cfg.Tiers.RateLimiter.DefaultRate != 500 {
			t.Errorf("expected reload after delete+recreate with DefaultRate=500, got %d", cfg.Tiers.RateLimiter.DefaultRate)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for reload after delete+recreate — fsnotify.Add watches a directory here, so a recreated file under the same dir should still be seen")
	}
}

func TestWatch_StopEndsTheWatcher(t *testing.T) {
	path := writeTempConfig(t, `
tiers:
  rate_limiter:
    default_rate: 100
    default_burst: 500
    shadow_mode: false
`)

	stop, err := config.Watch(path, func(cfg *config.Config, err error) {})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	stop()
	stop()
}

// TestWatch_StopBlocksUntilWatcherGoroutineExits proves stop() is genuinely
// synchronous, not just idempotent: a file rewrite issued immediately after
// stop() returns must never be observed, because by then the watcher
// goroutine has already exited and its fsnotify watcher is closed. Before
// the fix (close(done) without waiting for the goroutine to exit), this test
// was flaky -- the still-running goroutine could win the race and pick up
// the post-stop rewrite.
func TestWatch_StopBlocksUntilWatcherGoroutineExits(t *testing.T) {
	path := writeTempConfig(t, `
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
		t.Fatalf("unexpected error: %v", err)
	}

	stop()
	if err := os.WriteFile(path, []byte(`
tiers:
  rate_limiter:
    default_rate: 999
    default_burst: 999
    shadow_mode: true
`), 0600); err != nil {
		t.Fatalf("failed to write config after stop: %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	select {
	case cfg := <-changes:
		t.Fatalf("expected the watcher to have stopped reloading after stop() returned, got a reload with DefaultRate=%d", cfg.Tiers.RateLimiter.DefaultRate)
	default:
	}
}

package cmd_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/ratecap/cli/cmd"
)

func writeTempConfig(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "ratecap.yaml")
	if err := os.WriteFile(path, []byte(contents), 0644); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}
	return path
}

func TestConfigValidate_ExitsZeroOnValidConfig(t *testing.T) {
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
  fleet_shedder:
    default_max_concurrent: 50
    reserved_critical_pct: 20
    max_request_duration_ms: 30000
    default_priority: sheddable
    shadow_mode: false
`)

	var out bytes.Buffer
	root := cmd.NewRootCmd()
	root.SetOut(&out)
	root.SetArgs([]string{"config", "validate", path})

	if err := root.Execute(); err != nil {
		t.Fatalf("expected no error for a valid config, got: %v", err)
	}
	if !bytes.Contains(out.Bytes(), []byte("valid")) {
		t.Errorf("expected output to confirm validity, got: %q", out.String())
	}
}

func TestConfigValidate_ReturnsErrorOnInvalidConfig(t *testing.T) {
	path := writeTempConfig(t, `
sync_rate: 5
tiers:
  rate_limiter:
    default_rate: 100
    default_burst: 500
    shadow_mode: false
`)

	var out bytes.Buffer
	root := cmd.NewRootCmd()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"config", "validate", path})

	err := root.Execute()
	if err == nil {
		t.Fatal("expected an error for a config missing concurrency_limiter/fleet_shedder blocks")
	}
	if !bytes.Contains(out.Bytes(), []byte("concurrency_limiter")) {
		t.Errorf("expected the underlying validation error to mention concurrency_limiter, got: %q", out.String())
	}
}

func TestConfigValidate_ReturnsErrorWhenFileMissing(t *testing.T) {
	var out bytes.Buffer
	root := cmd.NewRootCmd()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"config", "validate", "/nonexistent/ratecap.yaml"})

	if err := root.Execute(); err == nil {
		t.Fatal("expected an error for a nonexistent config file")
	}
}

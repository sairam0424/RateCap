package main_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
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

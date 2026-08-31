package cmd

import (
	"context"
	"os/exec"
	"testing"
)

// fakeRunner returns canned output for a fixed set of commands and
// exec.ErrNotFound for anything else, simulating a missing binary.
func fakeRunner(outputs map[string][]byte) commandRunner {
	return func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if out, ok := outputs[name]; ok {
			return out, nil
		}
		return nil, exec.ErrNotFound
	}
}

func TestCaptureResources_MissingBinariesYieldEmptySnapshot(t *testing.T) {
	run := fakeRunner(nil) // every command "missing"

	snap := captureResources(context.Background(), run, []string{"core", "sidecar"}, "redis://localhost:6379")

	if snap.DockerStats != "" {
		t.Errorf("expected empty DockerStats when docker is missing, got %q", snap.DockerStats)
	}
	if snap.RedisInfo != "" {
		t.Errorf("expected empty RedisInfo when redis-cli is missing, got %q", snap.RedisInfo)
	}
}

func TestCaptureResources_SuccessfulCommandsPopulateFields(t *testing.T) {
	run := fakeRunner(map[string][]byte{
		"docker":    []byte(`{"Name":"core","CPUPerc":"1.23%"}`),
		"redis-cli": []byte("# Server\nredis_version:7.2.0\n"),
	})

	snap := captureResources(context.Background(), run, []string{"core"}, "redis://localhost:6379")

	if snap.DockerStats != `{"Name":"core","CPUPerc":"1.23%"}` {
		t.Errorf("expected DockerStats to be populated from the fake docker output, got %q", snap.DockerStats)
	}
	if snap.RedisInfo != "# Server\nredis_version:7.2.0\n" {
		t.Errorf("expected RedisInfo to be populated from the fake redis-cli output, got %q", snap.RedisInfo)
	}
}

func TestCaptureResources_NoContainersSkipsDockerCall(t *testing.T) {
	called := false
	run := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "docker" {
			called = true
		}
		return nil, exec.ErrNotFound
	}

	snap := captureResources(context.Background(), run, nil, "")

	if called {
		t.Error("expected docker not to be invoked when containers list is empty")
	}
	if snap.DockerStats != "" || snap.RedisInfo != "" {
		t.Errorf("expected fully empty snapshot when no containers and no redis addr given, got %+v", snap)
	}
}

func TestCaptureResources_EmptyRedisAddrSkipsRedisCall(t *testing.T) {
	called := false
	run := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "redis-cli" {
			called = true
		}
		return []byte("stats"), nil
	}

	snap := captureResources(context.Background(), run, []string{"core"}, "")

	if called {
		t.Error("expected redis-cli not to be invoked when redisAddr is empty")
	}
	if snap.DockerStats != "stats" {
		t.Errorf("expected DockerStats populated from the docker call, got %q", snap.DockerStats)
	}
	if snap.RedisInfo != "" {
		t.Errorf("expected RedisInfo to remain empty, got %q", snap.RedisInfo)
	}
}

func TestCaptureResources_OneBinaryMissingOtherSucceedsIndependently(t *testing.T) {
	run := fakeRunner(map[string][]byte{
		"redis-cli": []byte("redis_version:7.2.0"),
		// "docker" intentionally absent -> exec.ErrNotFound
	})

	snap := captureResources(context.Background(), run, []string{"core"}, "redis://localhost:6379")

	if snap.DockerStats != "" {
		t.Errorf("expected DockerStats empty when docker is missing, got %q", snap.DockerStats)
	}
	if snap.RedisInfo != "redis_version:7.2.0" {
		t.Errorf("expected RedisInfo populated even though docker failed, got %q", snap.RedisInfo)
	}
}

func TestIsSnapshotEmpty(t *testing.T) {
	cases := []struct {
		name string
		snap *ResourceSnapshot
		want bool
	}{
		{"nil pointer", nil, true},
		{"zero value", &ResourceSnapshot{}, true},
		{"docker only", &ResourceSnapshot{DockerStats: "x"}, false},
		{"redis only", &ResourceSnapshot{RedisInfo: "x"}, false},
	}
	for _, tc := range cases {
		if got := isSnapshotEmpty(tc.snap); got != tc.want {
			t.Errorf("%s: isSnapshotEmpty() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

package cmd

import (
	"context"
	"os/exec"
)

type commandRunner func(ctx context.Context, name string, args ...string) ([]byte, error)

func defaultRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

type ResourceSnapshot struct {
	DockerStats string `json:"docker_stats,omitempty"`
	RedisInfo   string `json:"redis_info,omitempty"`
}

// captureResources is best-effort: a missing docker/redis-cli binary, or a
// non-Docker deployment target, must never fail the benchmark run itself —
// only the snapshot fields are omitted.
func captureResources(ctx context.Context, run commandRunner, containers []string, redisAddr string) ResourceSnapshot {
	var snap ResourceSnapshot
	if len(containers) > 0 {
		args := append([]string{"stats", "--no-stream", "--format", "{{json .}}"}, containers...)
		if out, err := run(ctx, "docker", args...); err == nil {
			snap.DockerStats = string(out)
		}
	}
	if redisAddr != "" {
		if out, err := run(ctx, "redis-cli", "-u", redisAddr, "INFO"); err == nil {
			snap.RedisInfo = string(out)
		}
	}
	return snap
}

// isSnapshotEmpty reports whether a captured snapshot has nothing to show —
// used to decide whether the human-readable summary prints a resource
// section at all.
func isSnapshotEmpty(snap *ResourceSnapshot) bool {
	return snap == nil || (snap.DockerStats == "" && snap.RedisInfo == "")
}

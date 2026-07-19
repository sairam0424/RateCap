package main_test

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

func TestMain_HealthServerRespondsServingRegardlessOfRedisReachability(t *testing.T) {
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
		t.Fatalf("failed to write config: %v", err)
	}

	// `go run .` execs the compiled binary as a subprocess of the "go run"
	// wrapper; killing the wrapper does not reliably kill that subprocess
	// (SIGKILL is not forwarded), which leaks an orphaned server bound to
	// the health port on every test run. Building the binary once and
	// exec'ing it directly makes cmd.Process the real server process, so
	// cmd.Process.Kill() actually terminates it.
	binPath := filepath.Join(dir, "core-under-test")
	buildCmd := exec.Command("go", "build", "-o", binPath, ".")
	if output, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to build test binary: %v\n%s", err, output)
	}

	healthLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to reserve an ephemeral health port: %v", err)
	}
	healthAddr := healthLis.Addr().String()
	if err := healthLis.Close(); err != nil {
		t.Fatalf("failed to release the reserved health port: %v", err)
	}

	cmd := exec.Command(binPath)
	cmd.Env = append(os.Environ(),
		"RATECAP_CONFIG_PATH="+configPath,
		"RATECAP_SHARED_SECRET=test-secret",
		"RATECAP_GRPC_ADDR=:0",
		"RATECAP_HEALTH_ADDR="+healthAddr,
		"RATECAP_REDIS_ADDR=127.0.0.1:1",
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start process: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	var conn *grpc.ClientConn
	conn, err = grpc.NewClient(healthAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("failed to construct health client: %v", err)
	}
	defer conn.Close()

	// The health binary is a freshly compiled process started concurrently
	// with this repo's other packages under `go test ./...`, so its actual
	// startup time varies with system load (observed flaky under -race
	// alongside store's Docker-container-backed tests) — poll for up to 10s
	// rather than a fixed handful of short-timeout attempts.
	client := healthpb.NewHealthClient(conn)
	var resp *healthpb.HealthCheckResponse
	deadline := time.Now().Add(10 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		resp, err = client.Check(ctx, &healthpb.HealthCheckRequest{})
		cancel()
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("health check failed after retrying until deadline: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if resp.Status != healthpb.HealthCheckResponse_SERVING {
		t.Errorf("expected SERVING, got %v", resp.Status)
	}
}

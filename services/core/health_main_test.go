package main_test

import (
	"context"
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

	cmd := exec.Command("go", "run", ".")
	cmd.Env = append(os.Environ(),
		"RATECAP_CONFIG_PATH="+configPath,
		"RATECAP_SHARED_SECRET=test-secret",
		"RATECAP_GRPC_ADDR=:0",
		"RATECAP_HEALTH_ADDR=:19191",
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
	var err error
	for i := 0; i < 20; i++ {
		conn, err = grpc.NewClient("127.0.0.1:19191", grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("failed to dial health server: %v", err)
	}
	defer conn.Close()

	client := healthpb.NewHealthClient(conn)
	var resp *healthpb.HealthCheckResponse
	for i := 0; i < 20; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		resp, err = client.Check(ctx, &healthpb.HealthCheckRequest{})
		cancel()
		if err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("health check failed: %v", err)
	}
	if resp.Status != healthpb.HealthCheckResponse_SERVING {
		t.Errorf("expected SERVING, got %v", resp.Status)
	}
}

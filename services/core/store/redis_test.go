package store_test

import (
	"context"
	"testing"

	"github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/ratecap/core/store"
)

func startRedis(t *testing.T) *redis.Client {
	ctx := context.Background()
	req := testcontainers.ContainerRequest{
		Image:        "redis:7-alpine",
		ExposedPorts: []string{"6379/tcp"},
		WaitingFor:   wait.ForListeningPort("6379/tcp"),
	}
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		t.Fatalf("failed to start redis container: %v", err)
	}
	t.Cleanup(func() { _ = c.Terminate(ctx) })

	host, err := c.Host(ctx)
	if err != nil {
		t.Fatalf("failed to get container host: %v", err)
	}
	port, err := c.MappedPort(ctx, "6379")
	if err != nil {
		t.Fatalf("failed to get mapped port: %v", err)
	}
	return redis.NewClient(&redis.Options{Addr: host + ":" + port.Port()})
}

func TestCheckAndDecrement_AllowsWithinBurst(t *testing.T) {
	client := startRedis(t)
	s := store.NewRedisStore(client)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		allowed, _, err := s.CheckAndDecrement(ctx, "test-key-burst", 10, 5, 1)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !allowed {
			t.Fatalf("request %d should be allowed within burst of 5", i)
		}
	}
}

func TestCheckAndDecrement_RejectsOverBurst(t *testing.T) {
	client := startRedis(t)
	s := store.NewRedisStore(client)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		if _, _, err := s.CheckAndDecrement(ctx, "test-key-reject", 10, 5, 1); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}

	allowed, retryAfterMs, err := s.CheckAndDecrement(ctx, "test-key-reject", 10, 5, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if allowed {
		t.Fatalf("6th request should be rejected, burst is 5")
	}
	if retryAfterMs <= 0 {
		t.Fatalf("expected positive retryAfterMs, got %d", retryAfterMs)
	}
}

func TestCheckAndDecrement_ConcurrentAtomicity(t *testing.T) {
	client := startRedis(t)
	s := store.NewRedisStore(client)
	ctx := context.Background()

	const attempts = 50
	const burst = 10
	results := make(chan bool, attempts)

	for i := 0; i < attempts; i++ {
		go func() {
			allowed, _, err := s.CheckAndDecrement(ctx, "test-key-concurrent", 1, burst, 1)
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			results <- allowed
		}()
	}

	allowedCount := 0
	for i := 0; i < attempts; i++ {
		if <-results {
			allowedCount++
		}
	}

	if allowedCount != burst {
		t.Fatalf("expected exactly %d allowed under concurrent load, got %d", burst, allowedCount)
	}
}

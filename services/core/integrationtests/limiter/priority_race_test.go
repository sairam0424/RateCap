package limiter_test

import (
	"context"
	"sync"
	"testing"

	"github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/sairam0424/RateCap/services/core/limiter"
	"github.com/sairam0424/RateCap/services/core/store"
)

var raceTestSigningKey = []byte("test-signing-key-do-not-use-in-production")

func startRaceTestRedis(t *testing.T) *redis.Client {
	ctx := context.Background()
	req := testcontainers.ContainerRequest{
		Image:        "redis:7-alpine",
		ExposedPorts: []string{"6379/tcp"},
		WaitingFor:   wait.ForListeningPort("6379/tcp"),
	}
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{ContainerRequest: req, Started: true})
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

// TestConcurrencyLimiter_PriorityPartitionAtCapacityRace replicates the
// Netflix concurrency-limits bug class (PR #233/#234): many goroutines
// racing to acquire the last slots when the limit is already at (or just
// under) capacity. Tier 2 has no priority partitioning of its own (that's
// Tier 3's job) — this proves the plain at-capacity race is clean here too.
func TestConcurrencyLimiter_PriorityPartitionAtCapacityRace(t *testing.T) {
	client := startRaceTestRedis(t)
	s := store.NewRedisStore(client, raceTestSigningKey)
	const cap = 20
	l := limiter.NewConcurrencyLimiter(s, cap, 60000, false, false, 0, 0, 0)

	const goroutines = 200
	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			decision, err := l.Check(context.Background(), limiter.Request{Key: "race-key", Cost: 1})
			if err != nil {
				return
			}
			if decision.Action == limiter.ALLOW {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if allowed > cap {
		t.Errorf("expected at most %d allowed under concurrent load at capacity, got %d — a slot was double-issued", cap, allowed)
	}
}

// TestFleetShedder_MixedPriorityAtCapacityRace drives BOTH critical and
// sheddable traffic concurrently at exactly the effective-cap boundary for
// each partition, asserting neither partition's own effective cap is ever
// exceeded even under race — the property store/redis_test.go's
// TestIncrConcurrent_MixedPriorityConcurrentAtomicity already proves at the
// raw-store level; this proves it survives composition with FleetShedder's
// own effective-cap arithmetic.
func TestFleetShedder_MixedPriorityAtCapacityRace(t *testing.T) {
	client := startRaceTestRedis(t)
	s := store.NewRedisStore(client, raceTestSigningKey)
	const cap = 20
	const reservedCriticalPct = 50
	l := limiter.NewFleetShedder(s, cap, reservedCriticalPct, 60000, false)
	sheddableEffectiveCap := cap * (100 - reservedCriticalPct) / 100

	const goroutinesPerPriority = 100
	var wg sync.WaitGroup
	var mu sync.Mutex
	allowedCritical, allowedSheddable := 0, 0

	launch := func(priority limiter.Priority, count *int) {
		for i := 0; i < goroutinesPerPriority; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				decision, err := l.Check(context.Background(), limiter.Request{Key: "race-key", Cost: 1, Priority: priority})
				if err != nil {
					return
				}
				if decision.Action == limiter.ALLOW {
					mu.Lock()
					*count++
					mu.Unlock()
				}
			}()
		}
	}
	launch(limiter.Critical, &allowedCritical)
	launch(limiter.Sheddable, &allowedSheddable)
	wg.Wait()

	if allowedCritical > cap {
		t.Errorf("expected at most %d critical allows (full cap), got %d", cap, allowedCritical)
	}
	if allowedSheddable > sheddableEffectiveCap {
		t.Errorf("expected at most %d sheddable allows (effective cap after reservation), got %d", sheddableEffectiveCap, allowedSheddable)
	}
}

package reliability_test

import (
	"context"
	"testing"
	"time"

	toxiproxyclient "github.com/Shopify/toxiproxy/v2/client"
	"github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/toxiproxy"
	"github.com/testcontainers/testcontainers-go/network"
	tcwait "github.com/testcontainers/testcontainers-go/wait"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/ratecap/core/limiter"
	coremetrics "github.com/ratecap/core/metrics"
	"github.com/ratecap/core/store"
)

var testSigningKey = []byte("test-signing-key-do-not-use-in-production")

// redisBehindToxiproxy starts a real Redis container plus a Toxiproxy
// container proxying to it, and returns a client pointed at the proxy so
// the test can sever the connection on demand via toxiproxy's API without
// tearing down Redis itself — a closer match to a real network partition
// than closing the client connection directly.
//
// The pinned testcontainers-go/modules/toxiproxy@v0.44.0 API differs from
// the roadmap spec's illustrative sketch (no NewProxy/ProxiedAddr methods):
// the real API pre-declares the proxy via toxiproxy.WithProxy at Run time,
// reads back its address via ProxiedEndpoint, and toggles it through a
// separate github.com/Shopify/toxiproxy/v2/client — matching this module's
// own ExampleRun_connectionCut.
func redisBehindToxiproxy(t *testing.T) (*redis.Client, func(cutConnection bool)) {
	ctx := context.Background()

	nw, err := network.New(ctx)
	if err != nil {
		t.Fatalf("failed to create docker network: %v", err)
	}
	t.Cleanup(func() { _ = nw.Remove(ctx) })

	redisReq := testcontainers.ContainerRequest{
		Image:        "redis:7-alpine",
		ExposedPorts: []string{"6379/tcp"},
		WaitingFor:   tcwait.ForListeningPort("6379/tcp"),
		Networks:     []string{nw.Name},
		NetworkAliases: map[string][]string{
			nw.Name: {"redis-target"},
		},
	}
	redisContainer, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: redisReq,
		Started:          true,
	})
	if err != nil {
		t.Fatalf("failed to start redis container: %v", err)
	}
	t.Cleanup(func() { _ = redisContainer.Terminate(ctx) })

	toxiproxyContainer, err := toxiproxy.Run(ctx, "ghcr.io/shopify/toxiproxy:2.9.0",
		toxiproxy.WithProxy("redis", "redis-target:6379"),
		network.WithNetwork([]string{"toxiproxy"}, nw),
	)
	if err != nil {
		t.Fatalf("failed to start toxiproxy container: %v", err)
	}
	t.Cleanup(func() { _ = toxiproxyContainer.Terminate(ctx) })

	host, port, err := toxiproxyContainer.ProxiedEndpoint(8666)
	if err != nil {
		t.Fatalf("failed to get proxied endpoint: %v", err)
	}

	client := redis.NewClient(&redis.Options{Addr: host + ":" + port, DialTimeout: 500 * time.Millisecond, ReadTimeout: 500 * time.Millisecond})

	toxiURI, err := toxiproxyContainer.URI(ctx)
	if err != nil {
		t.Fatalf("failed to get toxiproxy container uri: %v", err)
	}
	proxies, err := toxiproxyclient.NewClient(toxiURI).Proxies()
	if err != nil {
		t.Fatalf("failed to list toxiproxy proxies: %v", err)
	}
	proxy := proxies["redis"]

	cut := func(cutConnection bool) {
		var toggleErr error
		if cutConnection {
			toggleErr = proxy.Disable()
		} else {
			toggleErr = proxy.Enable()
		}
		if toggleErr != nil {
			t.Fatalf("failed to toggle proxy (cut=%v): %v", cutConnection, toggleErr)
		}
	}
	return client, cut
}

func TestTier1_RedisUnavailable_FailsOpen(t *testing.T) {
	client, cut := redisBehindToxiproxy(t)
	s := store.NewRedisStore(client, testSigningKey)
	tokenBucket := limiter.NewTokenBucketLimiter(s, 100, 500, false)

	cut(true)
	defer cut(false)

	before := testutil.ToFloat64(coremetrics.FailOpenTotal.WithLabelValues("rate_limiter", "store_error"))
	decision, err := tokenBucket.Check(context.Background(), limiter.Request{Key: "toxiproxy-tier1", Cost: 1})
	after := testutil.ToFloat64(coremetrics.FailOpenTotal.WithLabelValues("rate_limiter", "store_error"))

	if err != nil {
		t.Fatalf("expected Tier 1 to fail OPEN (no error) on a real network partition, got: %v", err)
	}
	if decision.Action != limiter.ALLOW {
		t.Errorf("expected Action=ALLOW on a real Redis outage, got %v", decision.Action)
	}
	if after != before+1 {
		t.Errorf("expected ratecap_fail_open_total{tier=rate_limiter} to increment, before=%v after=%v", before, after)
	}
}

func TestTier2_RedisUnavailable_FailsClosed(t *testing.T) {
	client, cut := redisBehindToxiproxy(t)
	s := store.NewRedisStore(client, testSigningKey)
	concurrencyLimiter := limiter.NewConcurrencyLimiter(s, 100, 60000, false, false, 0, 0, 0)

	cut(true)
	defer cut(false)

	_, err := concurrencyLimiter.Check(context.Background(), limiter.Request{Key: "toxiproxy-tier2", Cost: 1})
	if err == nil {
		t.Fatal("expected Tier 2 to fail CLOSED (return an error) on a real network partition, got no error")
	}
}

func TestTier3_RedisUnavailable_FailsClosed(t *testing.T) {
	client, cut := redisBehindToxiproxy(t)
	s := store.NewRedisStore(client, testSigningKey)
	fleetShedder := limiter.NewFleetShedder(s, 100, 20, 60000, false)

	cut(true)
	defer cut(false)

	_, err := fleetShedder.Check(context.Background(), limiter.Request{Key: "toxiproxy-tier3", Cost: 1, Priority: limiter.Sheddable})
	if err == nil {
		t.Fatal("expected Tier 3 to fail CLOSED (return an error) on a real network partition, got no error")
	}
}

func TestTier1_RedisRecovers_ResumesNormalOperation(t *testing.T) {
	client, cut := redisBehindToxiproxy(t)
	s := store.NewRedisStore(client, testSigningKey)
	tokenBucket := limiter.NewTokenBucketLimiter(s, 1, 1, false)

	cut(true)
	decision, err := tokenBucket.Check(context.Background(), limiter.Request{Key: "toxiproxy-tier1-recovery", Cost: 1})
	if err != nil || decision.Action != limiter.ALLOW {
		t.Fatalf("expected fail-open ALLOW while cut, got action=%v err=%v", decision.Action, err)
	}

	cut(false)
	time.Sleep(200 * time.Millisecond)

	// A fresh-key ALLOW alone can't distinguish genuine recovery from Tier 1
	// still failing open (both produce the identical {ALLOW, err=nil} shape)
	// — assert the fail-open counter does NOT increment on this call, and
	// that a second check against the SAME key is rejected once burst is
	// exhausted, which only happens if Redis is really back and decrementing
	// server-side state (fail-open never persists any state at all).
	before := testutil.ToFloat64(coremetrics.FailOpenTotal.WithLabelValues("rate_limiter", "store_error"))
	decision, err = tokenBucket.Check(context.Background(), limiter.Request{Key: "toxiproxy-tier1-recovery-2", Cost: 1})
	after := testutil.ToFloat64(coremetrics.FailOpenTotal.WithLabelValues("rate_limiter", "store_error"))
	if err != nil {
		t.Fatalf("expected a normal decision once Redis recovers, got error: %v", err)
	}
	if decision.Action != limiter.ALLOW {
		t.Errorf("expected a fresh key's first request to be ALLOW once Redis recovers, got %v", decision.Action)
	}
	if after != before {
		t.Errorf("expected FailOpenTotal unchanged once Redis recovers (this ALLOW must be a real decision, not fail-open), before=%v after=%v", before, after)
	}

	decision, err = tokenBucket.Check(context.Background(), limiter.Request{Key: "toxiproxy-tier1-recovery-2", Cost: 1})
	if err != nil {
		t.Fatalf("expected a normal decision on the second call to the same key, got error: %v", err)
	}
	if decision.Action == limiter.ALLOW {
		t.Error("expected the second request against a burst-of-1 key to be rejected — an ALLOW here would mean no real server-side state was ever decremented, i.e. Tier 1 is still failing open despite the proxy being re-enabled")
	}
}

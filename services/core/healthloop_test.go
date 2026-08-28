package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

func TestRunRedisHealthLoop_SetsServingWhenPingSucceeds(t *testing.T) {
	var mu sync.Mutex
	var lastStatus healthpb.HealthCheckResponse_ServingStatus
	setStatus := func(s healthpb.HealthCheckResponse_ServingStatus) {
		mu.Lock()
		defer mu.Unlock()
		lastStatus = s
	}
	ping := func(ctx context.Context) error { return nil }
	stop := make(chan struct{})

	go runRedisHealthLoop(10*time.Millisecond, ping, setStatus, stop)
	time.Sleep(50 * time.Millisecond)
	close(stop)

	mu.Lock()
	defer mu.Unlock()
	if lastStatus != healthpb.HealthCheckResponse_SERVING {
		t.Errorf("expected SERVING when ping succeeds, got %v", lastStatus)
	}
}

func TestRunRedisHealthLoop_SetsNotServingWhenPingFails(t *testing.T) {
	var mu sync.Mutex
	var lastStatus healthpb.HealthCheckResponse_ServingStatus
	setStatus := func(s healthpb.HealthCheckResponse_ServingStatus) {
		mu.Lock()
		defer mu.Unlock()
		lastStatus = s
	}
	ping := func(ctx context.Context) error { return errors.New("dial tcp: connection refused") }
	stop := make(chan struct{})

	go runRedisHealthLoop(10*time.Millisecond, ping, setStatus, stop)
	time.Sleep(50 * time.Millisecond)
	close(stop)

	mu.Lock()
	defer mu.Unlock()
	if lastStatus != healthpb.HealthCheckResponse_NOT_SERVING {
		t.Errorf("expected NOT_SERVING when ping fails, got %v", lastStatus)
	}
}

func TestRunRedisHealthLoop_StopsWhenStopChannelClosed(t *testing.T) {
	calls := 0
	var mu sync.Mutex
	ping := func(ctx context.Context) error {
		mu.Lock()
		calls++
		mu.Unlock()
		return nil
	}
	setStatus := func(healthpb.HealthCheckResponse_ServingStatus) {}
	stop := make(chan struct{})

	go runRedisHealthLoop(5*time.Millisecond, ping, setStatus, stop)
	time.Sleep(30 * time.Millisecond)
	close(stop)
	time.Sleep(20 * time.Millisecond)

	mu.Lock()
	callsAtStop := calls
	mu.Unlock()
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if calls != callsAtStop {
		t.Errorf("expected no further ping calls after stop was closed, had %d at stop, now %d", callsAtStop, calls)
	}
}

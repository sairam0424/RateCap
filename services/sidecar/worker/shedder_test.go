package worker_test

import (
	"sync"
	"testing"

	"github.com/ratecap/sidecar/worker"
)

func TestShedder_AllowsExactlyMaxConcurrent(t *testing.T) {
	s := worker.NewShedder(3)

	for i := 0; i < 3; i++ {
		if !s.Allow() {
			t.Fatalf("request %d: expected Allow() to return true within max of 3", i)
		}
	}

	if s.Allow() {
		t.Fatal("4th request: expected Allow() to return false, max of 3 exceeded")
	}
}

func TestShedder_ReleaseFreesASlot(t *testing.T) {
	s := worker.NewShedder(1)

	if !s.Allow() {
		t.Fatal("expected first Allow() to return true")
	}
	if s.Allow() {
		t.Fatal("expected second Allow() to return false, max of 1 exceeded")
	}

	s.Release()

	if !s.Allow() {
		t.Fatal("expected Allow() to return true after Release() frees the slot")
	}
}

func TestShedder_ConcurrentAllowAndReleaseIsRaceFree(t *testing.T) {
	s := worker.NewShedder(10)

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s.Allow() {
				s.Release()
			}
		}()
	}
	wg.Wait()
}

func TestShedder_AllowedCountNeverExceedsMaxUnderConcurrency(t *testing.T) {
	s := worker.NewShedder(5)

	var wg sync.WaitGroup
	var mu sync.Mutex
	allowedCount := 0

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s.Allow() {
				mu.Lock()
				allowedCount++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if allowedCount != 5 {
		t.Fatalf("expected exactly 5 concurrent Allow() calls to succeed (none released), got %d", allowedCount)
	}
}

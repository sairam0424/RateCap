package negativecache_test

import (
	"testing"
	"time"

	"github.com/ratecap/sidecar/negativecache"
)

func TestIsDenied_FalseForUnknownKey(t *testing.T) {
	c := negativecache.New()
	denied, _ := c.IsDenied("never-marked")
	if denied {
		t.Error("expected false for a key that was never marked denied")
	}
}

func TestIsDenied_TrueImmediatelyAfterMarkDenied(t *testing.T) {
	c := negativecache.New()
	c.MarkDenied("user-1", 500*time.Millisecond)

	denied, remaining := c.IsDenied("user-1")
	if !denied {
		t.Fatal("expected true immediately after MarkDenied")
	}
	if remaining <= 0 || remaining > 500*time.Millisecond {
		t.Errorf("expected remaining in (0, 500ms], got %v", remaining)
	}
}

func TestIsDenied_FalseAfterWindowElapses(t *testing.T) {
	now := time.Now()
	clock := &now
	c := negativecache.NewWithClock(func() time.Time { return *clock })

	c.MarkDenied("user-1", 100*time.Millisecond)
	*clock = clock.Add(200 * time.Millisecond)

	denied, _ := c.IsDenied("user-1")
	if denied {
		t.Error("expected false once the denial window has elapsed")
	}
}

func TestMarkDenied_OverwritesAnEarlierShorterWindow(t *testing.T) {
	now := time.Now()
	clock := &now
	c := negativecache.NewWithClock(func() time.Time { return *clock })

	c.MarkDenied("user-1", 100*time.Millisecond)
	c.MarkDenied("user-1", 5*time.Second)

	*clock = clock.Add(200 * time.Millisecond)

	denied, _ := c.IsDenied("user-1")
	if !denied {
		t.Error("expected the later, longer MarkDenied call to have taken effect")
	}
}

func TestCache_ConcurrentAccessIsRaceFree(t *testing.T) {
	c := negativecache.New()
	done := make(chan struct{})
	for i := 0; i < 50; i++ {
		go func(n int) {
			c.MarkDenied("key", 10*time.Millisecond)
			c.IsDenied("key")
			done <- struct{}{}
		}(i)
	}
	for i := 0; i < 50; i++ {
		<-done
	}
}

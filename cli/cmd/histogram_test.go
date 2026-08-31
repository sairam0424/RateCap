package cmd

import (
	"math"
	"testing"
	"time"
)

func TestHistogram_PercentilesAreMonotonic(t *testing.T) {
	h := newHistogram()
	for i := 1; i <= 10000; i++ {
		h.Record(time.Duration(i) * time.Microsecond * 100) // 0.1ms .. 1000ms, roughly
	}

	p50 := h.Percentile(0.50)
	p99 := h.Percentile(0.99)
	p999 := h.Percentile(0.999)

	if !(p50 <= p99 && p99 <= p999) {
		t.Errorf("expected p50 <= p99 <= p999, got p50=%v p99=%v p999=%v", p50, p99, p999)
	}
}

func TestHistogram_SingleValueApproximatesPercentile(t *testing.T) {
	const targetMs = 100.0
	h := newHistogram()
	h.Record(time.Duration(targetMs * float64(time.Millisecond)))

	got := h.Percentile(0.5)

	idx := bucketIndex(targetMs)
	upper := bucketUpperBoundMs(idx)
	lower := histMinMs
	if idx > 0 {
		lower = bucketUpperBoundMs(idx - 1)
	}
	width := upper - lower

	if diff := math.Abs(got - targetMs); diff > width {
		t.Errorf("expected Percentile(0.5) within one bucket-width (%.4fms) of recorded value %.2fms, got %.4fms (diff %.4fms)", width, targetMs, got, diff)
	}
}

func TestHistogram_BucketCountDoesNotGrowWithSamples(t *testing.T) {
	h := newHistogram()
	for i := 0; i < 1_000_000; i++ {
		h.Record(time.Duration(i%5000) * time.Microsecond)
	}

	if len(h.buckets) != histBuckets {
		t.Errorf("expected bucket array to stay fixed at %d entries, got %d", histBuckets, len(h.buckets))
	}
	if h.Count() != 1_000_000 {
		t.Errorf("expected count to track total recorded samples (1000000), got %d", h.Count())
	}
}

func TestHistogram_ResetZeroesCountAndBuckets(t *testing.T) {
	h := newHistogram()
	for i := 0; i < 100; i++ {
		h.Record(time.Duration(i+1) * time.Millisecond)
	}
	if h.Count() != 100 {
		t.Fatalf("expected count 100 before reset, got %d", h.Count())
	}

	h.Reset()

	if h.Count() != 0 {
		t.Errorf("expected count 0 after Reset, got %d", h.Count())
	}
	if h.Percentile(0.5) != 0 {
		t.Errorf("expected Percentile to return 0 on an empty histogram after Reset, got %v", h.Percentile(0.5))
	}
	for i, c := range h.buckets {
		if c != 0 {
			t.Fatalf("expected all buckets zeroed after Reset, bucket %d has count %d", i, c)
		}
	}
}

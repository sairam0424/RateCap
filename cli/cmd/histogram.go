package cmd

import (
	"math"
	"sync"
	"time"
)

// Histogram is a fixed-memory latency histogram: O(histBuckets) regardless
// of how many samples are recorded, so a multi-hour soak run has the same
// memory footprint as a one-second run.
type Histogram struct {
	mu      sync.Mutex
	buckets [histBuckets]uint64
	count   uint64
}

const (
	histMinMs   = 0.01
	histMaxMs   = 60000.0
	histBuckets = 2048
)

func newHistogram() *Histogram { return &Histogram{} }

func (h *Histogram) Record(d time.Duration) {
	ms := float64(d.Microseconds()) / 1000.0
	idx := bucketIndex(ms)
	h.mu.Lock()
	h.buckets[idx]++
	h.count++
	h.mu.Unlock()
}

func bucketIndex(ms float64) int {
	if ms <= histMinMs {
		return 0
	}
	if ms >= histMaxMs {
		return histBuckets - 1
	}
	ratio := math.Log(ms/histMinMs) / math.Log(histMaxMs/histMinMs)
	idx := int(ratio * float64(histBuckets))
	if idx >= histBuckets {
		idx = histBuckets - 1
	}
	return idx
}

func bucketUpperBoundMs(idx int) float64 {
	return histMinMs * math.Pow(histMaxMs/histMinMs, float64(idx+1)/float64(histBuckets))
}

// Percentile returns an approximate p-th percentile (p in [0,1]) as the
// upper bound of the bucket where the cumulative count first reaches the
// target rank. Error is bounded by the bucket's own width at that point in
// the log scale, not by sample count.
func (h *Histogram) Percentile(p float64) float64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.count == 0 {
		return 0
	}
	target := uint64(math.Ceil(p * float64(h.count)))
	var cum uint64
	for i, c := range h.buckets {
		cum += c
		if cum >= target {
			return bucketUpperBoundMs(i)
		}
	}
	return histMaxMs
}

func (h *Histogram) Count() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.count
}

// Reset zeroes the histogram in place, for periodic windowed reporting
// without allocating a new one every interval.
func (h *Histogram) Reset() {
	h.mu.Lock()
	for i := range h.buckets {
		h.buckets[i] = 0
	}
	h.count = 0
	h.mu.Unlock()
}

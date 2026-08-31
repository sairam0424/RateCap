package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/time/rate"

	ratecap "github.com/ratecap/sdk-go"
)

type benchResult struct {
	TotalRequests  int               `json:"total_requests"`
	Accepted       int               `json:"accepted"`
	Rejected       int               `json:"rejected"`
	Errored        int               `json:"errored"`
	ElapsedMs      int64             `json:"elapsed_ms"`
	ThroughputRPS  float64           `json:"throughput_rps"`
	P50Ms          float64           `json:"p50_ms"`
	P99Ms          float64           `json:"p99_ms"`
	P999Ms         float64           `json:"p999_ms"`
	ResourceBefore *ResourceSnapshot `json:"resource_before,omitempty"`
	ResourceAfter  *ResourceSnapshot `json:"resource_after,omitempty"`
}

// benchCounters tracks accepted/rejected/errored totals plus an accepted-
// latency histogram. Counters are atomics (workers increment concurrently);
// the histogram is internally mutex-guarded but fixed-memory regardless of
// sample count.
type benchCounters struct {
	accepted uint64
	rejected uint64
	errored  uint64
	hist     *Histogram
}

func newBenchCounters() *benchCounters {
	return &benchCounters{hist: newHistogram()}
}

func (c *benchCounters) record(kind string, elapsed time.Duration) {
	switch kind {
	case "accepted":
		atomic.AddUint64(&c.accepted, 1)
		c.hist.Record(elapsed)
	case "rejected":
		atomic.AddUint64(&c.rejected, 1)
	case "errored":
		atomic.AddUint64(&c.errored, 1)
	}
}

func (c *benchCounters) totals() (accepted, rejected, errored uint64) {
	return atomic.LoadUint64(&c.accepted), atomic.LoadUint64(&c.rejected), atomic.LoadUint64(&c.errored)
}

// reportSnapshot prints a windowed progress line and resets the window's
// counters/histogram in place, so a multi-hour --duration run never retains
// more than one window's worth of samples at a time.
func (c *benchCounters) reportSnapshot(w io.Writer, elapsed time.Duration) {
	accepted := atomic.SwapUint64(&c.accepted, 0)
	rejected := atomic.SwapUint64(&c.rejected, 0)
	errored := atomic.SwapUint64(&c.errored, 0)
	p50 := c.hist.Percentile(0.50)
	p99 := c.hist.Percentile(0.99)
	c.hist.Reset()
	// Best-effort: a transient write failure on one progress line must never
	// abort a multi-hour --duration soak run.
	fmt.Fprintf(w, "[%ds] accepted=%d rejected=%d errored=%d p50=%.2fms p99=%.2fms\n", //nolint:errcheck // see comment above
		int(elapsed.Seconds()), accepted, rejected, errored, p50, p99)
}

func newBenchRunCmd() *cobra.Command {
	var sidecarAddr string
	var concurrency int
	var requests int
	var keyPrefix string
	var useAcquire bool
	var jsonOutput bool
	var duration time.Duration
	var reportInterval time.Duration
	var captureResourcesFlag bool
	var dockerContainers string
	var redisAddr string
	var qps float64

	cmd := &cobra.Command{
		Use:   "run",
		Short: "Drive concurrent load against a running sidecar and report latency percentiles",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Windowed snapshot lines are diagnostic progress output, not
			// part of the result. When --json is set, the caller expects
			// exactly one JSON object on stdout (e.g. `> result.json`), so
			// snapshot lines must never share that writer — suppress them
			// entirely rather than routing to stderr, keeping this a
			// single, self-contained writer decision.
			progress := cmd.OutOrStdout()
			if jsonOutput {
				progress = io.Discard
			}

			var containers []string
			if dockerContainers != "" {
				containers = strings.Split(dockerContainers, ",")
			}

			// Resource snapshots are captured immediately before and after
			// the run, outside runBench itself, so opting out via
			// --capture-resources=false (the default) is zero behavior
			// change for every existing caller and test.
			var resourceBefore, resourceAfter *ResourceSnapshot
			if captureResourcesFlag {
				snap := captureResources(cmd.Context(), defaultRunner, containers, redisAddr)
				resourceBefore = &snap
			}

			result := runBench(cmd.Context(), progress, sidecarAddr, concurrency, requests, keyPrefix, useAcquire, duration, reportInterval, qps)

			if captureResourcesFlag {
				snap := captureResources(cmd.Context(), defaultRunner, containers, redisAddr)
				resourceAfter = &snap
			}
			result.ResourceBefore = resourceBefore
			result.ResourceAfter = resourceAfter

			if jsonOutput {
				enc := json.NewEncoder(cmd.OutOrStdout())
				return enc.Encode(result)
			}
			out := cmd.OutOrStdout()
			summaryLines := []string{
				fmt.Sprintf("Total requests: %d\n", result.TotalRequests),
				fmt.Sprintf("Accepted: %d  Rejected: %d  Errored: %d\n", result.Accepted, result.Rejected, result.Errored),
				fmt.Sprintf("Elapsed: %dms\n", result.ElapsedMs),
				fmt.Sprintf("Throughput: %.1f req/s\n", result.ThroughputRPS),
				fmt.Sprintf("P50: %.2fms  P99: %.2fms  P99.9: %.2fms (accepted requests only)\n", result.P50Ms, result.P99Ms, result.P999Ms),
			}
			for _, line := range summaryLines {
				if _, err := io.WriteString(out, line); err != nil {
					return err
				}
			}
			if err := printResourceSection(out, "before", result.ResourceBefore); err != nil {
				return err
			}
			if err := printResourceSection(out, "after", result.ResourceAfter); err != nil {
				return err
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&sidecarAddr, "sidecar-addr", "http://localhost:8080", "target sidecar address")
	cmd.Flags().IntVar(&concurrency, "concurrency", 10, "number of parallel workers")
	cmd.Flags().IntVar(&requests, "requests", 1000, "total number of requests across all workers (ignored when --duration is set)")
	cmd.Flags().StringVar(&keyPrefix, "key-prefix", "bench", "prefix for generated request keys")
	cmd.Flags().BoolVar(&useAcquire, "acquire", false, "use Acquire()/Release() (tier 2) instead of Allow() (tier 1)")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit machine-readable JSON instead of a human-readable summary")
	cmd.Flags().DurationVar(&duration, "duration", 0, "run for this long instead of a fixed --requests count (e.g. \"30s\", \"1h\") — a soak run with fixed memory usage")
	cmd.Flags().DurationVar(&reportInterval, "report-interval", 5*time.Second, "how often to print a windowed progress snapshot (only meaningful when --duration is set)")
	cmd.Flags().BoolVar(&captureResourcesFlag, "capture-resources", false, "capture best-effort docker/redis resource snapshots immediately before and after the run")
	cmd.Flags().StringVar(&dockerContainers, "docker-containers", "", "comma-separated container names/IDs to snapshot with `docker stats` (only used with --capture-resources)")
	cmd.Flags().StringVar(&redisAddr, "redis-addr", "", "redis connection URI (e.g. redis://localhost:6379) to snapshot with `redis-cli INFO` (only used with --capture-resources)")
	cmd.Flags().Float64Var(&qps, "qps", 0, "cap the aggregate request rate across all workers to this many requests/sec (0 = unlimited, unchanged behavior)")

	return cmd
}

// printResourceSection prints a short human-readable resource snapshot
// section only when the snapshot actually captured something — an empty
// snapshot (capture disabled, or both docker/redis-cli unavailable) prints
// nothing.
func printResourceSection(w io.Writer, label string, snap *ResourceSnapshot) error {
	if isSnapshotEmpty(snap) {
		return nil
	}
	if _, err := fmt.Fprintf(w, "Resources (%s):\n", label); err != nil {
		return err
	}
	if snap.DockerStats != "" {
		if _, err := fmt.Fprintf(w, "  docker stats: %s\n", snap.DockerStats); err != nil {
			return err
		}
	}
	if snap.RedisInfo != "" {
		if _, err := fmt.Fprintf(w, "  redis info: %s\n", snap.RedisInfo); err != nil {
			return err
		}
	}
	return nil
}

// runBench drives load against the sidecar and returns the cumulative
// result. Latency is tracked via a bounded streaming histogram rather than
// an unbounded slice of raw samples, so memory usage is constant whether
// the run issues a thousand requests or runs for hours under --duration.
func runBench(ctx context.Context, progress io.Writer, sidecarAddr string, concurrency, requests int, keyPrefix string, useAcquire bool, duration, reportInterval time.Duration, qps float64) benchResult {
	client := ratecap.NewClient(sidecarAddr)
	cumulative := newBenchCounters()

	var window *benchCounters
	if duration > 0 {
		window = newBenchCounters()
	}

	// A single limiter shared across every worker paces the *aggregate*
	// request rate to --qps, not a per-worker rate — matching how an
	// operator reasons about "cap this benchmark at N req/s overall".
	var limiter *rate.Limiter
	if qps > 0 {
		limiter = rate.NewLimiter(rate.Limit(qps), 1)
	}

	issue := func(ctx context.Context, workerID, seq int) {
		if limiter != nil {
			// A context cancellation/deadline while waiting for a token means
			// this request is skipped entirely (it never actually happened),
			// not counted as errored/rejected — the caller's deadline (e.g.
			// --duration's ctx) is the thing that decided to stop, not the
			// limiter.
			if err := limiter.Wait(ctx); err != nil {
				return
			}
		}
		key := fmt.Sprintf("%s-%d-%d", keyPrefix, workerID, seq)
		reqStart := time.Now()
		kind := "accepted"
		if useAcquire {
			ticket, err := client.Acquire(ctx, key)
			switch {
			case err != nil:
				kind = "errored"
			case !ticket.Allowed:
				kind = "rejected"
			}
			if err == nil {
				// Best-effort per Ticket.Release's own godoc; this request's
				// outcome is already recorded via kind above, and a release
				// failure doesn't change what the benchmark measured.
				ticket.Release(ctx) //nolint:errcheck // see comment above
			}
		} else {
			allowed, _, err := client.Allow(ctx, key)
			switch {
			case err != nil:
				kind = "errored"
			case !allowed:
				kind = "rejected"
			}
		}
		elapsed := time.Since(reqStart)
		cumulative.record(kind, elapsed)
		if window != nil {
			window.record(kind, elapsed)
		}
	}

	start := time.Now()
	if duration > 0 {
		runBenchDuration(ctx, progress, concurrency, duration, reportInterval, window, issue)
	} else {
		runBenchFixedRequests(ctx, concurrency, requests, issue)
	}
	totalElapsed := time.Since(start)

	accepted, rejected, errored := cumulative.totals()
	total := accepted + rejected + errored

	return benchResult{
		TotalRequests: int(total),
		Accepted:      int(accepted),
		Rejected:      int(rejected),
		Errored:       int(errored),
		ElapsedMs:     totalElapsed.Milliseconds(),
		ThroughputRPS: float64(total) / totalElapsed.Seconds(),
		P50Ms:         cumulative.hist.Percentile(0.50),
		P99Ms:         cumulative.hist.Percentile(0.99),
		P999Ms:        cumulative.hist.Percentile(0.999),
	}
}

// runBenchFixedRequests preserves the original behavior: a fixed number of
// requests distributed across a worker pool via a buffered jobs channel.
func runBenchFixedRequests(ctx context.Context, concurrency, requests int, issue func(ctx context.Context, workerID, seq int)) {
	var wg sync.WaitGroup
	jobs := make(chan int, requests)
	for i := 0; i < requests; i++ {
		jobs <- i
	}
	close(jobs)

	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for seq := range jobs {
				issue(ctx, workerID, seq)
			}
		}(w)
	}
	wg.Wait()
}

// runBenchDuration runs every worker until the deadline instead of a fixed
// request count. A separate goroutine ticks every reportInterval and prints
// a windowed snapshot from window, which is reset after each print — the
// cumulative counters/histogram used for the final summary (tracked inside
// issue itself) are untouched by windowing.
func runBenchDuration(
	ctx context.Context,
	progress io.Writer,
	concurrency int,
	duration, reportInterval time.Duration,
	window *benchCounters,
	issue func(ctx context.Context, workerID, seq int),
) {
	deadlineCtx, cancel := context.WithTimeout(ctx, duration)
	defer cancel()

	var wg sync.WaitGroup
	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			seq := 0
			for {
				select {
				case <-deadlineCtx.Done():
					return
				default:
					issue(deadlineCtx, workerID, seq)
					seq++
				}
			}
		}(w)
	}

	reportDone := make(chan struct{})
	go func() {
		defer close(reportDone)
		start := time.Now()
		ticker := time.NewTicker(reportInterval)
		defer ticker.Stop()
		for {
			select {
			case <-deadlineCtx.Done():
				return
			case <-ticker.C:
				window.reportSnapshot(progress, time.Since(start))
			}
		}
	}()

	wg.Wait()
	<-reportDone
}

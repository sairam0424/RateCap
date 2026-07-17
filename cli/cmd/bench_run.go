package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/spf13/cobra"

	ratecap "github.com/ratecap/sdk-go"
)

type benchResult struct {
	TotalRequests int     `json:"total_requests"`
	ElapsedMs     int64   `json:"elapsed_ms"`
	ThroughputRPS float64 `json:"throughput_rps"`
	P50Ms         float64 `json:"p50_ms"`
	P99Ms         float64 `json:"p99_ms"`
	P999Ms        float64 `json:"p999_ms"`
}

func newBenchRunCmd() *cobra.Command {
	var sidecarAddr string
	var concurrency int
	var requests int
	var keyPrefix string
	var useAcquire bool
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "run",
		Short: "Drive concurrent load against a running sidecar and report latency percentiles",
		RunE: func(cmd *cobra.Command, args []string) error {
			result := runBench(cmd.Context(), sidecarAddr, concurrency, requests, keyPrefix, useAcquire)
			if jsonOutput {
				enc := json.NewEncoder(cmd.OutOrStdout())
				return enc.Encode(result)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Total requests: %d\n", result.TotalRequests)
			fmt.Fprintf(cmd.OutOrStdout(), "Elapsed: %dms\n", result.ElapsedMs)
			fmt.Fprintf(cmd.OutOrStdout(), "Throughput: %.1f req/s\n", result.ThroughputRPS)
			fmt.Fprintf(cmd.OutOrStdout(), "P50: %.2fms  P99: %.2fms  P99.9: %.2fms\n", result.P50Ms, result.P99Ms, result.P999Ms)
			return nil
		},
	}

	cmd.Flags().StringVar(&sidecarAddr, "sidecar-addr", "http://localhost:8080", "target sidecar address")
	cmd.Flags().IntVar(&concurrency, "concurrency", 10, "number of parallel workers")
	cmd.Flags().IntVar(&requests, "requests", 1000, "total number of requests across all workers")
	cmd.Flags().StringVar(&keyPrefix, "key-prefix", "bench", "prefix for generated request keys")
	cmd.Flags().BoolVar(&useAcquire, "acquire", false, "use Acquire()/Release() (tier 2) instead of Allow() (tier 1)")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit machine-readable JSON instead of a human-readable summary")

	return cmd
}

func runBench(ctx context.Context, sidecarAddr string, concurrency, requests int, keyPrefix string, useAcquire bool) benchResult {
	client := ratecap.NewClient(sidecarAddr)

	var mu sync.Mutex
	var latencies []time.Duration

	var wg sync.WaitGroup
	jobs := make(chan int, requests)
	for i := 0; i < requests; i++ {
		jobs <- i
	}
	close(jobs)

	start := time.Now()
	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for seq := range jobs {
				key := fmt.Sprintf("%s-%d-%d", keyPrefix, workerID, seq)
				reqStart := time.Now()
				if useAcquire {
					ticket, err := client.Acquire(ctx, key)
					if err == nil {
						ticket.Release(ctx)
					}
				} else {
					client.Allow(ctx, key)
				}
				elapsed := time.Since(reqStart)
				mu.Lock()
				latencies = append(latencies, elapsed)
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()
	totalElapsed := time.Since(start)

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })

	return benchResult{
		TotalRequests: len(latencies),
		ElapsedMs:     totalElapsed.Milliseconds(),
		ThroughputRPS: float64(len(latencies)) / totalElapsed.Seconds(),
		P50Ms:         percentileMs(latencies, 0.50),
		P99Ms:         percentileMs(latencies, 0.99),
		P999Ms:        percentileMs(latencies, 0.999),
	}
}

func percentileMs(sorted []time.Duration, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(float64(len(sorted)-1) * p)
	return float64(sorted[idx].Microseconds()) / 1000.0
}

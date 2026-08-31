package cmd_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ratecap/cli/cmd"
)

func TestBenchRun_AllModeReportsAllRequestsAgainstFakeSidecar(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	var out bytes.Buffer
	root := cmd.NewRootCmd()
	root.SetOut(&out)
	root.SetArgs([]string{"bench", "run", "--sidecar-addr", server.URL, "--requests", "20", "--concurrency", "4"})

	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !bytes.Contains(out.Bytes(), []byte("Total requests: 20")) {
		t.Errorf("expected output to report 20 total requests, got:\n%s", out.String())
	}
	if !bytes.Contains(out.Bytes(), []byte("P50")) || !bytes.Contains(out.Bytes(), []byte("P99")) {
		t.Errorf("expected output to report P50/P99 latencies, got:\n%s", out.String())
	}
}

func TestBenchRun_JSONModeEmitsValidJSONWithExpectedFields(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	var out bytes.Buffer
	root := cmd.NewRootCmd()
	root.SetOut(&out)
	root.SetArgs([]string{"bench", "run", "--sidecar-addr", server.URL, "--requests", "10", "--concurrency", "2", "--json"})

	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatalf("expected valid JSON, got error %v for output %q", err, out.String())
	}
	for _, field := range []string{"total_requests", "elapsed_ms", "throughput_rps", "p50_ms", "p99_ms", "p999_ms"} {
		if _, ok := result[field]; !ok {
			t.Errorf("expected field %q in JSON output, got %v", field, result)
		}
	}
	if result["total_requests"].(float64) != 10 {
		t.Errorf("expected total_requests=10, got %v", result["total_requests"])
	}
}

func TestBenchRun_AcquireFlagUsesCheckThenReleaseFlow(t *testing.T) {
	var checkCount, releaseCount int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/check":
			checkCount++
			w.Header().Set("Concurrency-Token-0", "tok")
			w.Header().Set("Concurrency-Key-0", "k")
			w.WriteHeader(http.StatusOK)
		case "/release":
			releaseCount++
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	var out bytes.Buffer
	root := cmd.NewRootCmd()
	root.SetOut(&out)
	root.SetArgs([]string{"bench", "run", "--sidecar-addr", server.URL, "--requests", "5", "--concurrency", "1", "--acquire"})

	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if checkCount != 5 {
		t.Errorf("expected 5 /check calls, got %d", checkCount)
	}
	if releaseCount != 5 {
		t.Errorf("expected 5 /release calls (one per acquired ticket), got %d", releaseCount)
	}
}

func TestBenchRun_KeyPrefixIsUsedInGeneratedKeys(t *testing.T) {
	var capturedKeys []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedKeys = append(capturedKeys, r.URL.Query().Get("key"))
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	var out bytes.Buffer
	root := cmd.NewRootCmd()
	root.SetOut(&out)
	root.SetArgs([]string{"bench", "run", "--sidecar-addr", server.URL, "--requests", "3", "--concurrency", "1", "--key-prefix", "mytest"})

	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(capturedKeys) != 3 {
		t.Fatalf("expected 3 requests, got %d", len(capturedKeys))
	}
	for _, k := range capturedKeys {
		if len(k) < len("mytest") || k[:len("mytest")] != "mytest" {
			t.Errorf("expected key %q to start with prefix %q", k, "mytest")
		}
	}
}

func TestBenchRun_TracksAcceptedRejectedAndErroredSeparately(t *testing.T) {
	var count int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count++
		switch count % 3 {
		case 0:
			w.WriteHeader(http.StatusOK)
		case 1:
			w.WriteHeader(http.StatusTooManyRequests)
		case 2:
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer server.Close()

	var out bytes.Buffer
	root := cmd.NewRootCmd()
	root.SetOut(&out)
	root.SetArgs([]string{"bench", "run", "--sidecar-addr", server.URL, "--requests", "9", "--concurrency", "1", "--json"})

	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatalf("expected valid JSON, got error %v for output %q", err, out.String())
	}

	accepted, ok := result["accepted"].(float64)
	if !ok || accepted != 3 {
		t.Errorf("expected 3 accepted (every 3rd of 9 requests returns 200), got %v (present=%v)", accepted, ok)
	}
	rejected, ok := result["rejected"].(float64)
	if !ok || rejected != 6 {
		t.Errorf("expected 6 rejected (429+503 responses), got %v (present=%v)", rejected, ok)
	}
	errored, ok := result["errored"].(float64)
	if !ok || errored != 0 {
		t.Errorf("expected 0 errored (no transport failures in this test), got %v (present=%v)", errored, ok)
	}
}

func TestBenchRun_AcquireReleaseIsCalledEvenForRejectedTickets(t *testing.T) {
	var checkCount, releaseCount int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/check":
			checkCount++
			w.Header().Set("Concurrency-Token-0", "tok-tier2")
			w.Header().Set("Concurrency-Key-0", "k-tier2")
			w.WriteHeader(http.StatusTooManyRequests)
		case "/release":
			releaseCount++
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	var out bytes.Buffer
	root := cmd.NewRootCmd()
	root.SetOut(&out)
	root.SetArgs([]string{"bench", "run", "--sidecar-addr", server.URL, "--requests", "4", "--concurrency", "1", "--acquire"})

	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if checkCount != 4 {
		t.Errorf("expected 4 /check calls, got %d", checkCount)
	}
	if releaseCount != 4 {
		t.Errorf("expected 4 /release calls even though /check returned 429, got %d", releaseCount)
	}
}

func TestBenchRun_DurationModeStopsNearDeadline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	var out bytes.Buffer
	root := cmd.NewRootCmd()
	root.SetOut(&out)
	root.SetArgs([]string{
		"bench", "run",
		"--sidecar-addr", server.URL,
		"--concurrency", "4",
		"--duration", "200ms",
		"--report-interval", "1h", // long enough that no snapshot line fires during this short run
	})

	started := time.Now()
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	elapsed := time.Since(started)

	if elapsed < 150*time.Millisecond || elapsed > 500*time.Millisecond {
		t.Errorf("expected wall-clock elapsed within [150ms, 500ms] of the 200ms duration, got %v", elapsed)
	}

	var result map[string]any
	if err := json.Unmarshal(out.Bytes(), &result); err == nil {
		t.Fatalf("expected human-readable output (not JSON) by default, got parseable JSON: %v", result)
	}
	if !bytes.Contains(out.Bytes(), []byte("Total requests:")) {
		t.Errorf("expected duration-mode output to still report total requests, got:\n%s", out.String())
	}
}

func TestBenchRun_DurationModeWithJSONFlagProducesOnlyValidJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	var out bytes.Buffer
	root := cmd.NewRootCmd()
	root.SetOut(&out)
	root.SetArgs([]string{
		"bench", "run",
		"--sidecar-addr", server.URL,
		"--concurrency", "4",
		"--duration", "150ms",
		"--report-interval", "30ms", // short enough to fire several snapshots during the run
		"--json",
	})

	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if bytes.Contains(out.Bytes(), []byte("accepted=")) {
		t.Errorf("expected --json to suppress windowed snapshot lines, got:\n%s", out.String())
	}

	var result map[string]any
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatalf("expected output to be valid, unpolluted JSON, got error %v for output %q", err, out.String())
	}
	for _, field := range []string{"total_requests", "elapsed_ms", "p50_ms", "p99_ms"} {
		if _, ok := result[field]; !ok {
			t.Errorf("expected field %q in JSON output, got %v", field, result)
		}
	}
}

func TestBenchRun_CaptureResourcesOffByDefaultOmitsFieldsFromJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	var out bytes.Buffer
	root := cmd.NewRootCmd()
	root.SetOut(&out)
	root.SetArgs([]string{"bench", "run", "--sidecar-addr", server.URL, "--requests", "3", "--concurrency", "1", "--json"})

	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatalf("expected valid JSON, got error %v for output %q", err, out.String())
	}
	if _, ok := result["resource_before"]; ok {
		t.Errorf("expected resource_before omitted from JSON when --capture-resources is unset, got %v", result)
	}
	if _, ok := result["resource_after"]; ok {
		t.Errorf("expected resource_after omitted from JSON when --capture-resources is unset, got %v", result)
	}
}

func TestBenchRun_CaptureResourcesFlagDoesNotFailRunWhenBinariesAreUnavailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	var out bytes.Buffer
	root := cmd.NewRootCmd()
	root.SetOut(&out)
	root.SetArgs([]string{
		"bench", "run",
		"--sidecar-addr", server.URL,
		"--requests", "3",
		"--concurrency", "1",
		"--capture-resources",
		"--docker-containers", "core,sidecar",
		"--redis-addr", "redis://localhost:6379",
		"--json",
	})

	// captureResources is best-effort: whether docker/redis-cli happen to be
	// installed in the test environment must never change whether this
	// command succeeds.
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error with --capture-resources set: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatalf("expected valid JSON, got error %v for output %q", err, out.String())
	}
	if _, ok := result["total_requests"]; !ok {
		t.Errorf("expected total_requests still present alongside resource capture, got %v", result)
	}
}

func TestBenchRun_QPSFlagPacesRequests(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	var out bytes.Buffer
	root := cmd.NewRootCmd()
	root.SetOut(&out)
	root.SetArgs([]string{
		"bench", "run",
		"--sidecar-addr", server.URL,
		"--concurrency", "5",
		"--requests", "10",
		"--qps", "20", // 10 requests at 20/s implies >= ~450ms (9 intervals of 50ms, first token free)
	})

	started := time.Now()
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	elapsed := time.Since(started)

	// Unpaced, this fake in-memory server would finish in low single-digit
	// milliseconds — a generous floor well below the pacing-implied minimum
	// still clearly distinguishes "paced" from "unpaced," while tolerating
	// CI timing jitter.
	if elapsed < 200*time.Millisecond {
		t.Errorf("expected --qps to pace requests to at least 200ms elapsed, got %v", elapsed)
	}

	if !bytes.Contains(out.Bytes(), []byte("Total requests: 10")) {
		t.Errorf("expected output to report 10 total requests, got:\n%s", out.String())
	}
}

func TestBenchRun_DurationModePrintsWindowedSnapshots(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	var out bytes.Buffer
	root := cmd.NewRootCmd()
	root.SetOut(&out)
	root.SetArgs([]string{
		"bench", "run",
		"--sidecar-addr", server.URL,
		"--concurrency", "4",
		"--duration", "300ms",
		"--report-interval", "50ms",
	})

	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !bytes.Contains(out.Bytes(), []byte("accepted=")) {
		t.Errorf("expected at least one windowed snapshot line containing 'accepted=', got:\n%s", out.String())
	}
}

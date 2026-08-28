package cmd_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

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

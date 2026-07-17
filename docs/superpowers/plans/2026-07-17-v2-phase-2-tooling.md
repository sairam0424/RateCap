# RateCap v2 Phase 2: Developer Tooling Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a `ratecapctl` CLI (Cobra, `config validate` + `bench run` subcommands) and a Python SDK (`packages/sdks/python`, zero dependencies) to RateCap.

**Architecture:** `ratecapctl` is a new top-level Go module at `cli/`, built on Cobra, added to `go.work`. `config validate` is a thin wrapper around the already-tested `services/core/config` package. `bench run` drives concurrent load against a running sidecar using `packages/sdks/go`'s existing `Allow`/`Acquire`/`Ticket.Release` API, computing percentile latencies from real round-trip timings. The Python SDK is an independent, hand-written port of the Go SDK's exact wire behavior (2 HTTP endpoints, indexed multi-reservation headers) — no shared code with the Go side, no proto/gRPC involved, since the SDK boundary is plain HTTP.

**Tech Stack:** Go 1.26 (`cli/`, reusing `packages/sdks/go`), `github.com/spf13/cobra` v1.10.2 (new dependency), Python 3.10+ stdlib only (`urllib.request`, no third-party dependencies), `python3 -m unittest` (pytest is not installed in this environment; do not add it as a dependency for this phase).

## Global Constraints

- TDD: write the failing test first, confirm it fails for the right reason, then write the minimal implementation, then confirm it passes.
- `gofmt -l` must report zero files before any commit touching Go code.
- Run `go test ./... -race` for any Go module touched, before every commit touching that module.
- Python: run `python3 -m py_compile <file>` on every new/modified `.py` file as a syntax-sanity check before committing. Run `python3 -m unittest discover -s packages/sdks/python/tests -v` before every commit touching the Python SDK.
- No comments except non-obvious WHY, in both languages.
- No `Co-Authored-By` trailers in any commit.
- Exact commands and exact expected output are given in every step; run them verbatim.
- `ratecapctl decisions tail` is explicitly OUT OF SCOPE for this plan — no command, no stub, do not create it or reference it anywhere in code.
- Docker must be confirmed reachable (`docker info > /dev/null 2>&1`) before the live e2e step in Task 4; if unreachable, report this explicitly rather than skipping silently.

---

### Task 1: `ratecapctl` module scaffold + `config validate` subcommand

**Files:**
- Create: `cli/go.mod`
- Create: `cli/main.go`
- Create: `cli/cmd/root.go`
- Create: `cli/cmd/config_validate.go`
- Create: `cli/cmd/config_validate_test.go`
- Modify: `go.work`

**Interfaces:**
- Consumes: `github.com/ratecap/core/config` — `config.Load(path string) (*config.Config, error)` and `(*config.Config).Validate() error`, both already implemented and tested in `services/core/config/config.go`.
- Produces: `cli/cmd.NewRootCmd() *cobra.Command` — Task 2 adds `bench run` as a sibling subcommand registered on the same root command this task creates.

- [ ] **Step 1: Create the `cli` module**

Run: `mkdir -p cli/cmd`

Create `cli/go.mod`:

```
module github.com/ratecap/cli

go 1.26.2

require (
	github.com/ratecap/core v0.0.0
	github.com/ratecap/sdk-go v0.0.0
	github.com/spf13/cobra v1.10.2
)

replace github.com/ratecap/core => ../services/core

replace github.com/ratecap/sdk-go => ../packages/sdks/go
```

- [ ] **Step 2: Add `cli` to the workspace**

In `go.work`, replace:

```
go 1.26.2

use ./proto

use ./services/core

use (
	./deploy/sampleapp
	./packages/sdks/go
	./services/sidecar
)
```

with:

```
go 1.26.2

use ./proto

use ./services/core

use (
	./cli
	./deploy/sampleapp
	./packages/sdks/go
	./services/sidecar
)
```

Run: `go work sync`
Expected: no output. Then run `cd cli && go mod tidy 2>&1 | tail -20` — expected: `go.mod`/`go.sum` populate with `cobra`, `pflag`, `mousetrap`, and the local `core`/`sdk-go` replace-directive modules resolved; no errors.

- [ ] **Step 3: Write the failing test for `config validate`**

Create `cli/cmd/config_validate_test.go`:

```go
package cmd_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/ratecap/cli/cmd"
)

func writeTempConfig(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "ratecap.yaml")
	if err := os.WriteFile(path, []byte(contents), 0644); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}
	return path
}

func TestConfigValidate_ExitsZeroOnValidConfig(t *testing.T) {
	path := writeTempConfig(t, `
sync_rate: 5
tiers:
  rate_limiter:
    default_rate: 100
    default_burst: 500
    shadow_mode: false
  concurrency_limiter:
    default_max_concurrent: 20
    max_request_duration_ms: 30000
    shadow_mode: false
  fleet_shedder:
    default_max_concurrent: 50
    reserved_critical_pct: 20
    max_request_duration_ms: 30000
    default_priority: sheddable
    shadow_mode: false
`)

	var out bytes.Buffer
	root := cmd.NewRootCmd()
	root.SetOut(&out)
	root.SetArgs([]string{"config", "validate", path})

	if err := root.Execute(); err != nil {
		t.Fatalf("expected no error for a valid config, got: %v", err)
	}
	if !bytes.Contains(out.Bytes(), []byte("valid")) {
		t.Errorf("expected output to confirm validity, got: %q", out.String())
	}
}

func TestConfigValidate_ReturnsErrorOnInvalidConfig(t *testing.T) {
	path := writeTempConfig(t, `
sync_rate: 5
tiers:
  rate_limiter:
    default_rate: 100
    default_burst: 500
    shadow_mode: false
`)

	var out bytes.Buffer
	root := cmd.NewRootCmd()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"config", "validate", path})

	err := root.Execute()
	if err == nil {
		t.Fatal("expected an error for a config missing concurrency_limiter/fleet_shedder blocks")
	}
	if !bytes.Contains(out.Bytes(), []byte("concurrency_limiter")) {
		t.Errorf("expected the underlying validation error to mention concurrency_limiter, got: %q", out.String())
	}
}

func TestConfigValidate_ReturnsErrorWhenFileMissing(t *testing.T) {
	var out bytes.Buffer
	root := cmd.NewRootCmd()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"config", "validate", "/nonexistent/ratecap.yaml"})

	if err := root.Execute(); err == nil {
		t.Fatal("expected an error for a nonexistent config file")
	}
}
```

- [ ] **Step 4: Run the test to verify it fails**

Run: `cd cli && go test ./cmd/... -v 2>&1 | tail -30`
Expected: FAIL to compile — the `cmd` package and `NewRootCmd` do not exist yet.

- [ ] **Step 5: Implement the root command and `config validate`**

Create `cli/cmd/root.go`:

```go
package cmd

import (
	"github.com/spf13/cobra"
)

func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "ratecapctl",
		Short: "Operator CLI for RateCap — validate config, benchmark a running sidecar",
	}
	root.AddCommand(newConfigCmd())
	return root
}

func newConfigCmd() *cobra.Command {
	configCmd := &cobra.Command{
		Use:   "config",
		Short: "Config-related commands",
	}
	configCmd.AddCommand(newConfigValidateCmd())
	return configCmd
}
```

Create `cli/cmd/config_validate.go`:

```go
package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/ratecap/core/config"
)

func newConfigValidateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "validate <path>",
		Short: "Validate a ratecap.yaml config file",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := args[0]

			cfg, err := config.Load(path)
			if err != nil {
				return fmt.Errorf("loading %s: %w", path, err)
			}

			if err := cfg.Validate(); err != nil {
				return fmt.Errorf("invalid config: %w", err)
			}

			fmt.Fprintf(cmd.OutOrStdout(), "%s is valid\n", path)
			return nil
		},
	}
}
```

Create `cli/main.go`:

```go
package main

import (
	"fmt"
	"os"

	"github.com/ratecap/cli/cmd"
)

func main() {
	if err := cmd.NewRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `cd cli && go test ./cmd/... -v 2>&1 | tail -30`
Expected: PASS — all 3 tests report `--- PASS`, final line `ok      github.com/ratecap/cli/cmd`.

- [ ] **Step 7: Manually verify the built binary**

Run:

```bash
cd cli
go build -o /tmp/ratecapctl .
echo "sync_rate: 5" > /tmp/bad-config.yaml
/tmp/ratecapctl config validate /tmp/bad-config.yaml; echo "exit=$?"
```

Expected: a non-zero exit code and an error mentioning `concurrency_limiter`.

Run: `/tmp/ratecapctl config validate ../deploy/ratecap.yaml; echo "exit=$?"`
Expected: `../deploy/ratecap.yaml is valid` and `exit=0` (the demo config is real and already valid).

- [ ] **Step 8: Run the full `cli` build and test suite**

Run: `cd cli && go build ./... && go test ./... -race 2>&1 | tail -20`
Expected: `ok github.com/ratecap/cli/cmd`; `go build ./...` produces no output.

- [ ] **Step 9: gofmt check and commit**

Run: `gofmt -l cli/main.go cli/cmd/root.go cli/cmd/config_validate.go cli/cmd/config_validate_test.go`
Expected: no output.

```bash
git add go.work cli/
git commit -m "feat(cli): scaffold ratecapctl, add config validate subcommand

New top-level cli/ module (Cobra), matching the original v1 spec's
directory sketch. config validate is a thin wrapper around the
already-tested services/core/config package — zero new validation
logic, just a CLI entry point that surfaces its existing error
messages."
```

---

### Task 2: `bench run` subcommand

**Files:**
- Create: `cli/cmd/bench.go`
- Create: `cli/cmd/bench_run.go`
- Create: `cli/cmd/bench_run_test.go`

**Interfaces:**
- Consumes: `cmd.NewRootCmd()` (Task 1); `github.com/ratecap/sdk-go`'s `ratecap.NewClient(sidecarAddr string) *ratecap.Client`, `(*Client).Allow(ctx, key) (bool, int64, error)`, `(*Client).Acquire(ctx, key) (*ratecap.Ticket, error)`, `(*Ticket).Release(ctx) error` — the exact existing signatures in `packages/sdks/go/client.go`, unchanged by this task.
- Produces: nothing consumed by later tasks in this plan (Task 4 uses the built `ratecapctl` binary's `bench run` command as an external process, not as a Go import).

- [ ] **Step 1: Write the failing tests**

Create `cli/cmd/bench_run_test.go`:

```go
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
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd cli && go test ./cmd/... -run TestBenchRun -v 2>&1 | tail -40`
Expected: FAIL to compile — `bench` is not a registered subcommand yet.

- [ ] **Step 3: Implement `bench run`**

Create `cli/cmd/bench.go`:

```go
package cmd

import "github.com/spf13/cobra"

func newBenchCmd() *cobra.Command {
	benchCmd := &cobra.Command{
		Use:   "bench",
		Short: "Benchmarking commands",
	}
	benchCmd.AddCommand(newBenchRunCmd())
	return benchCmd
}
```

Modify `cli/cmd/root.go` — replace:

```go
	root.AddCommand(newConfigCmd())
	return root
```

with:

```go
	root.AddCommand(newConfigCmd())
	root.AddCommand(newBenchCmd())
	return root
```

Create `cli/cmd/bench_run.go`:

```go
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
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd cli && go test ./cmd/... -race -v 2>&1 | tail -60`
Expected: PASS — all 4 new tests plus the 3 from Task 1 report `--- PASS`, final line `ok      github.com/ratecap/cli/cmd`.

- [ ] **Step 5: Manually verify the built binary against a real local server**

Run:

```bash
cd cli
go build -o /tmp/ratecapctl .
python3 -m http.server 18080 &
HTTPPID=$!
sleep 1
/tmp/ratecapctl bench run --sidecar-addr http://localhost:18080 --requests 20 --concurrency 4
kill $HTTPPID
```

Expected: a report with `Total requests: 20`, a nonzero `Throughput`, and nonzero `P50`/`P99`/`P99.9` values (Python's built-in HTTP server returns 200s for any path with a small real network round-trip, giving genuine non-zero timings — this is a quick sanity check, not the full live-stack verification, which happens in Task 4).

- [ ] **Step 6: Run the full `cli` build and test suite**

Run: `cd cli && go build ./... && go test ./... -race 2>&1 | tail -20`
Expected: `ok github.com/ratecap/cli/cmd`.

- [ ] **Step 7: gofmt check and commit**

Run: `gofmt -l cli/cmd/bench.go cli/cmd/bench_run.go cli/cmd/bench_run_test.go cli/cmd/root.go`
Expected: no output.

```bash
git add cli/
git commit -m "feat(cli): add bench run subcommand

Reuses packages/sdks/go's Allow/Acquire/Ticket.Release directly rather
than reimplementing HTTP calls. Closes the gap the original v1 design
spec's Testing Strategy section promised (P99/P99.9 numbers) but never
delivered. --acquire opts into tier 2's Acquire/Release path instead
of the default Allow (tier 1 only); --json emits a machine-readable
summary for future automated benchmark regeneration."
```

---

### Task 3: Python SDK

**Files:**
- Create: `packages/sdks/python/pyproject.toml`
- Create: `packages/sdks/python/src/ratecap/__init__.py`
- Create: `packages/sdks/python/src/ratecap/client.py`
- Create: `packages/sdks/python/tests/__init__.py`
- Create: `packages/sdks/python/tests/fake_sidecar.py`
- Create: `packages/sdks/python/tests/test_client.py`

**Interfaces:**
- Consumes: nothing from Tasks 1-2 (independent module, different language, zero shared code).
- Produces: `ratecap.Client(sidecar_addr: str)`, `Client.allow(key: str) -> AllowResult`, `Client.acquire(key: str) -> Ticket`, `Ticket.release() -> None`, `Ticket.__enter__`/`__exit__` — consumed by Task 4's live e2e script, not by any other task in this plan.

- [ ] **Step 1: Create the package skeleton**

Run: `mkdir -p packages/sdks/python/src/ratecap packages/sdks/python/tests`

Create `packages/sdks/python/pyproject.toml`:

```toml
[project]
name = "ratecap"
version = "0.1.0"
description = "Thin Python client for the RateCap sidecar"
requires-python = ">=3.10"
dependencies = []

[build-system]
requires = ["setuptools>=68"]
build-backend = "setuptools.build_meta"

[tool.setuptools.packages.find]
where = ["src"]
```

Create `packages/sdks/python/src/ratecap/__init__.py`:

```python
from ratecap.client import AllowResult, Client, Ticket

__all__ = ["AllowResult", "Client", "Ticket"]
```

Create an empty `packages/sdks/python/tests/__init__.py` (no content needed — it exists so `python3 -m unittest discover` treats `tests/` as a package).

- [ ] **Step 2: Write the fake sidecar test helper**

Create `packages/sdks/python/tests/fake_sidecar.py`:

```python
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer
from urllib.parse import parse_qs, urlparse


class FakeSidecar:
    def __init__(self, handler):
        self._handler = handler
        self.requests = []
        server = self

        class _Handler(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def do_GET(self):
                self._dispatch()

            def do_POST(self):
                self._dispatch()

            def _dispatch(self):
                parsed = urlparse(self.path)
                query = {k: v[0] for k, v in parse_qs(parsed.query).items()}
                server.requests.append((self.command, parsed.path, query))
                status, headers = server._handler(self.command, parsed.path, query)
                self.send_response(status)
                for key, value in headers.items():
                    self.send_header(key, value)
                self.end_headers()

        self._httpd = HTTPServer(("127.0.0.1", 0), _Handler)
        self._thread = threading.Thread(target=self._httpd.serve_forever, daemon=True)

    @property
    def url(self):
        host, port = self._httpd.server_address
        return f"http://{host}:{port}"

    def __enter__(self):
        self._thread.start()
        return self

    def __exit__(self, *exc):
        self._httpd.shutdown()
        self._httpd.server_close()
```

- [ ] **Step 3: Write the failing tests**

Create `packages/sdks/python/tests/test_client.py`:

```python
import unittest

from ratecap import Client

from tests.fake_sidecar import FakeSidecar


class TestAllow(unittest.TestCase):
    def test_returns_true_on_200(self):
        with FakeSidecar(lambda method, path, query: (200, {})) as sidecar:
            client = Client(sidecar.url)
            result = client.allow("user-1")
            self.assertTrue(result.allowed)

    def test_returns_false_with_retry_after_on_429(self):
        def handler(method, path, query):
            return 429, {"Retry-After-Ms": "750"}

        with FakeSidecar(handler) as sidecar:
            client = Client(sidecar.url)
            result = client.allow("user-1")
            self.assertFalse(result.allowed)
            self.assertEqual(result.retry_after_ms, 750)

    def test_requests_skip_reservations(self):
        captured = {}

        def handler(method, path, query):
            captured.update(query)
            return 200, {}

        with FakeSidecar(handler) as sidecar:
            client = Client(sidecar.url)
            client.allow("user-1")
            self.assertEqual(captured.get("skip_reservations"), "true")


class TestAcquire(unittest.TestCase):
    def test_acquire_returns_allowed_true_on_200(self):
        def handler(method, path, query):
            if path == "/check":
                return 200, {"Concurrency-Token-0": "tok-abc", "Concurrency-Key-0": "user-1"}
            return 200, {}

        with FakeSidecar(handler) as sidecar:
            client = Client(sidecar.url)
            ticket = client.acquire("user-1")
            self.assertTrue(ticket.allowed)

    def test_acquire_does_not_send_skip_reservations(self):
        captured = {}

        def handler(method, path, query):
            if path == "/check":
                captured.update(query)
                return 200, {}
            return 200, {}

        with FakeSidecar(handler) as sidecar:
            client = Client(sidecar.url)
            client.acquire("user-1")
            self.assertNotIn("skip_reservations", captured)

    def test_release_releases_every_reservation(self):
        release_calls = []

        def handler(method, path, query):
            if path == "/check":
                return 200, {
                    "Concurrency-Token-0": "tok-abc",
                    "Concurrency-Key-0": "user-1",
                    "Concurrency-Token-1": "tok-xyz",
                    "Concurrency-Key-1": "fleet",
                }
            if path == "/release":
                release_calls.append(dict(query))
                return 200, {}
            return 404, {}

        with FakeSidecar(handler) as sidecar:
            client = Client(sidecar.url)
            ticket = client.acquire("user-1")
            ticket.release()

        self.assertEqual(len(release_calls), 2)
        by_key = {c["key"]: c["token"] for c in release_calls}
        self.assertEqual(by_key["user-1"], "tok-abc")
        self.assertEqual(by_key["fleet"], "tok-xyz")

    def test_release_is_noop_when_no_token_was_issued(self):
        release_called = []

        def handler(method, path, query):
            if path == "/release":
                release_called.append(True)
                return 200, {}
            return 429, {}

        with FakeSidecar(handler) as sidecar:
            client = Client(sidecar.url)
            ticket = client.acquire("user-1")
            ticket.release()

        self.assertEqual(release_called, [])

    def test_release_raises_when_a_reservation_fails_to_release(self):
        def handler(method, path, query):
            if path == "/check":
                return 200, {"Concurrency-Token-0": "tok-abc", "Concurrency-Key-0": "user-1"}
            if path == "/release":
                return 500, {}
            return 404, {}

        with FakeSidecar(handler) as sidecar:
            client = Client(sidecar.url)
            ticket = client.acquire("user-1")
            with self.assertRaises(RuntimeError):
                ticket.release()

    def test_context_manager_auto_releases(self):
        release_calls = []

        def handler(method, path, query):
            if path == "/check":
                return 200, {"Concurrency-Token-0": "tok-abc", "Concurrency-Key-0": "user-1"}
            if path == "/release":
                release_calls.append(dict(query))
                return 200, {}
            return 404, {}

        with FakeSidecar(handler) as sidecar:
            client = Client(sidecar.url)
            with client.acquire("user-1") as ticket:
                self.assertTrue(ticket.allowed)

        self.assertEqual(len(release_calls), 1)


if __name__ == "__main__":
    unittest.main()
```

- [ ] **Step 4: Run the tests to verify they fail**

Run: `cd packages/sdks/python && python3 -m unittest discover -s tests -v 2>&1 | tail -30`
Expected: FAIL — `ModuleNotFoundError: No module named 'ratecap'` (the package doesn't exist yet).

- [ ] **Step 5: Implement the client**

Create `packages/sdks/python/src/ratecap/client.py`:

```python
import urllib.parse
import urllib.request
from dataclasses import dataclass, field


@dataclass
class AllowResult:
    allowed: bool
    retry_after_ms: int = 0


@dataclass
class _Reservation:
    key: str
    token: str


class Ticket:
    def __init__(self, client, allowed, retry_after_ms=0, reservations=None):
        self.allowed = allowed
        self.retry_after_ms = retry_after_ms
        self._client = client
        self._reservations = reservations or []

    def release(self):
        errors = []
        for reservation in self._reservations:
            try:
                self._client._release_one(reservation)
            except Exception as exc:
                errors.append(f"{reservation.key}: {exc}")
        if errors:
            raise RuntimeError("failed to release reservation(s): " + "; ".join(errors))

    def __enter__(self):
        return self

    def __exit__(self, exc_type, exc_val, exc_tb):
        self.release()
        return False


class Client:
    def __init__(self, sidecar_addr):
        self._sidecar_addr = sidecar_addr.rstrip("/")

    def allow(self, key):
        query = urllib.parse.urlencode({"key": key, "skip_reservations": "true"})
        url = f"{self._sidecar_addr}/check?{query}"
        req = urllib.request.Request(url, method="GET")
        try:
            with urllib.request.urlopen(req) as resp:
                return AllowResult(allowed=True)
        except urllib.error.HTTPError as err:
            retry_after_ms = int(err.headers.get("Retry-After-Ms", 0) or 0)
            return AllowResult(allowed=False, retry_after_ms=retry_after_ms)

    def acquire(self, key):
        query = urllib.parse.urlencode({"key": key})
        url = f"{self._sidecar_addr}/check?{query}"
        req = urllib.request.Request(url, method="GET")
        try:
            with urllib.request.urlopen(req) as resp:
                reservations = self._parse_reservations(resp.headers)
                return Ticket(self, allowed=True, reservations=reservations)
        except urllib.error.HTTPError as err:
            reservations = self._parse_reservations(err.headers)
            retry_after_ms = int(err.headers.get("Retry-After-Ms", 0) or 0)
            return Ticket(self, allowed=False, retry_after_ms=retry_after_ms, reservations=reservations)

    def _parse_reservations(self, headers):
        reservations = []
        i = 0
        while True:
            token = headers.get(f"Concurrency-Token-{i}")
            if not token:
                break
            key = headers.get(f"Concurrency-Key-{i}", "")
            reservations.append(_Reservation(key=key, token=token))
            i += 1
        return reservations

    def _release_one(self, reservation):
        query = urllib.parse.urlencode({"key": reservation.key, "token": reservation.token})
        url = f"{self._sidecar_addr}/release?{query}"
        req = urllib.request.Request(url, method="POST")
        with urllib.request.urlopen(req) as resp:
            if resp.status != 200:
                raise RuntimeError(f"release failed with status {resp.status}")
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `cd packages/sdks/python && python3 -m unittest discover -s tests -v 2>&1 | tail -40`
Expected: PASS — all 10 tests report `ok`, final line `OK`.

- [ ] **Step 7: Syntax-sanity check every new Python file**

Run:

```bash
python3 -m py_compile packages/sdks/python/src/ratecap/__init__.py
python3 -m py_compile packages/sdks/python/src/ratecap/client.py
python3 -m py_compile packages/sdks/python/tests/fake_sidecar.py
python3 -m py_compile packages/sdks/python/tests/test_client.py
```

Expected: no output from any of the 4 commands (a syntax error would print a traceback; silence means success).

- [ ] **Step 8: Verify the package installs cleanly**

Run:

```bash
cd packages/sdks/python
python3 -m venv /tmp/ratecap-py-venv
source /tmp/ratecap-py-venv/bin/activate
pip install -e . 2>&1 | tail -10
python3 -c "import ratecap; print(ratecap.Client)"
deactivate
rm -rf /tmp/ratecap-py-venv
```

Expected: `pip install -e .` succeeds, and the `python3 -c` line prints something like `<class 'ratecap.client.Client'>` with no import errors.

- [ ] **Step 9: Commit**

```bash
git add packages/sdks/python/
git commit -m "feat(sdk-python): add a zero-dependency Python client

Direct, hand-written port of packages/sdks/go's exact wire behavior —
GET /check (+skip_reservations for allow(), matching Tier 1's
fire-and-forget contract), POST /release, indexed
Concurrency-Token-N/Concurrency-Key-N headers for multi-reservation
Acquire() calls. stdlib urllib.request only, no third-party
dependencies. Ticket supports both explicit release() and Python's
context-manager protocol."
```

---

### Task 4: Live end-to-end verification

**Files:** none modified — this task only runs the demo stack and the built binary/scripts against it.

**Interfaces:**
- Consumes: `cli/`'s `bench run` (Task 2) and `packages/sdks/python` (Task 3).
- Produces: nothing — a verification report appended to this task's own execution log.

- [ ] **Step 1: Confirm Docker is reachable**

Run: `docker info > /dev/null 2>&1 && echo "docker reachable" || echo "docker NOT reachable — start Docker Desktop before continuing"`

If not reachable, start Docker Desktop and re-run until it reports reachable before continuing.

- [ ] **Step 2: Bring up the full demo stack**

Run from `deploy/`:

```bash
cd deploy
./generate-demo-certs.sh
docker compose down 2>&1
docker compose build --no-cache 2>&1 | tail -20
docker compose up -d 2>&1
sleep 3
docker compose ps
```

Expected: all 4 containers (`redis`, `core`, `sidecar`, `sampleapp`) report `Up`.

- [ ] **Step 3: Run `ratecapctl bench run` against the real sidecar**

Run from the repo root:

```bash
cd cli && go build -o /tmp/ratecapctl . && cd ..
/tmp/ratecapctl bench run --sidecar-addr http://localhost:8080 --requests 200 --concurrency 10 --key-prefix "e2e-bench"
```

Expected: `Total requests: 200`, a nonzero `Throughput`, and nonzero, non-degenerate `P50`/`P99`/`P99.9` values (i.e. not all zero, not absurdly large — a few milliseconds to a few tens of milliseconds is reasonable for a local docker-compose stack). Each generated key is unique (`e2e-bench-<workerID>-<seq>`), so this exercises Tier 1's per-key token bucket without tripping Tier 2/3's shared caps.

Run the same command with `--json` and confirm valid JSON:

```bash
/tmp/ratecapctl bench run --sidecar-addr http://localhost:8080 --requests 50 --concurrency 5 --key-prefix "e2e-bench-json" --json | python3 -m json.tool
```

Expected: pretty-printed JSON with `total_requests`, `elapsed_ms`, `throughput_rps`, `p50_ms`, `p99_ms`, `p999_ms` fields, `total_requests` equal to `50`.

- [ ] **Step 4: Run a live Python SDK script against the real sidecar**

Create a throwaway script (not committed — this step verifies behavior, it doesn't add a permanent file):

```bash
cat > /tmp/ratecap_e2e_check.py <<'EOF'
import sys
sys.path.insert(0, "packages/sdks/python/src")

from ratecap import Client

client = Client("http://localhost:8080")

allow_result = client.allow("e2e-python-allow-check")
assert allow_result.allowed, f"expected first Allow to succeed, got {allow_result}"
print("allow(): OK, first call allowed")

results = []
for i in range(6):
    r = client.allow("e2e-python-shared-key")
    results.append(r.allowed)
print(f"allow() burst results: {results}")
assert False in results, "expected at least one rejection once the token bucket burst (5) is exceeded by 6 rapid calls to the same key"
print("allow(): OK, burst correctly triggered a rejection")

with client.acquire("e2e-python-acquire-check") as ticket:
    assert ticket.allowed, f"expected Acquire to succeed, got {ticket}"
    print("acquire(): OK, ticket allowed, context manager entered")
print("acquire(): OK, context manager exited (auto-released)")

print("ALL PYTHON SDK E2E CHECKS PASSED")
EOF
python3 /tmp/ratecap_e2e_check.py
rm /tmp/ratecap_e2e_check.py
```

Expected: `ALL PYTHON SDK E2E CHECKS PASSED` printed at the end, with no assertion errors along the way. This confirms `allow()` genuinely round-trips through the real sidecar (including observing a real rejection once `deploy/ratecap.yaml`'s `rate_limiter.default_burst: 5` is exceeded by 6 rapid same-key calls), and `acquire()`/context-manager auto-release genuinely works against Tier 2's concurrency limiter.

- [ ] **Step 5: Regression-check all 4 tiers still behave identically to pre-Phase-2**

Run:

```bash
for i in 1 2 3 4 5 6 7; do curl -s -o /dev/null -w "checkout %{http_code}\n" http://localhost:3000/checkout; done
```
Expected: exactly 5x `checkout 200` then 2x `checkout 429`.

```bash
for i in 1 2 3 4 5; do curl -s -o /dev/null -w "fleet-demo %{http_code}\n" "http://localhost:3000/fleet-demo?priority=sheddable" & done
wait
```
Expected: exactly 3x `fleet-demo 200` and 2x `fleet-demo 503`.

- [ ] **Step 6: Teardown**

Run: `cd deploy && docker compose down 2>&1 && cd ..`
Expected: containers and network removed, no errors.

- [ ] **Step 7: Run every module's full test suite one final time**

Run:

```bash
(cd services/core && go test ./... -race 2>&1 | tail -20)
(cd services/sidecar && go test ./... -race 2>&1 | tail -20)
(cd packages/sdks/go && go test ./... -race 2>&1 | tail -20)
(cd cli && go test ./... -race 2>&1 | tail -20)
(cd packages/sdks/python && python3 -m unittest discover -s tests -v 2>&1 | tail -20)
```

Expected: `ok` for every Go package (`services/core/store` needs Docker, which is reachable per Step 1, so it runs live); `OK` for the Python test suite.

No commit for this task — it verifies, it doesn't change code.

---

## Post-plan note

This closes the `ratecapctl` CLI and Python SDK gaps from RateCap's original v1 design spec, per the approved `docs/superpowers/specs/2026-07-17-v2-phase-2-tooling-design.md`. `ratecapctl decisions tail` is explicitly deferred — no command, no stub — per the spec's YAGNI reasoning. Once this plan's 4 tasks are reviewed and merged, this branch is ready for a PR into `develop`, and the v2 roadmap moves to Phase 3 (bounded queueing) — which requires a fresh design spec and the user's explicit sign-off per `CLAUDE.md`'s "5th mechanism" gate before any code, since it's a new limiting mechanism, not an additive tooling change like Phases 1-2.

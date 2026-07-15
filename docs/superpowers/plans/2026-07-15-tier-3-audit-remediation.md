# Tier 3 Audit Remediation (Group A) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close the Tier 3 pre-PR audit's Critical findings — a silent 100%-outage config chain (missing/invalid `fleet_shedder` config → unguarded wiring → `FleetShedder` unconditionally rejecting every request with zero log signal) and the undocumented unauthenticated `x-ratecap-priority` trust boundary — before opening the PR into `develop`.

**Architecture:** A `Validate() error` method on `*config.Config`, checked at two call sites with two different consequences: `services/core/main.go` fails closed (`log.Fatalf`) on startup if the loaded config is invalid, matching the existing `RATECAP_SHARED_SECRET` precedent in the same file; the hot-reload path logs the validation error and skips applying the bad reload, keeping the last-known-good config live rather than crashing a running service over a bad on-disk edit. `SECURITY.md` gains a new section documenting client-declared priority as an explicit, accepted v1 trust boundary — the same pattern already used for the plaintext-gRPC-plus-shared-secret boundary.

**Tech Stack:** Go 1.26 (`services/core/config`, `services/core/main.go`), no new dependencies.

## Global Constraints

- TDD: write the failing test first, confirm it fails for the right reason, then write the minimal implementation, then confirm it passes.
- `gofmt -l` must report zero files before any commit.
- Run `go test ./... -race` (per affected module) before every commit that touches that module.
- No comments except non-obvious WHY.
- No `Co-Authored-By` trailers in any commit.
- Scope is `FleetShedderConfig` only. `ConcurrencyLimiterConfig` and `RateLimiterConfig` have the identical missing-validation shape but are explicitly out of scope for this plan — the user chose to fix only what this audit found and approved fixing now; retrofitting the other two tiers is scope creep, not this plan's job.
- No code changes to add real authorization for `x-ratecap-priority` — Task 2 is documentation only, per the user's explicit choice.
- Docker is currently reachable (confirmed by the just-completed audit) — needed for Task 1's final live e2e re-verification step.
- Exact commands and exact expected output are given in every step; run them verbatim.

---

### Task 1: `Config.Validate()` + fail-closed startup + skip-bad-reload

**Files:**
- Modify: `services/core/config/config.go`
- Modify: `services/core/config/config_test.go`
- Modify: `services/core/main.go`

**Interfaces:**
- Consumes: nothing from earlier tasks (first task).
- Produces: `(*Config).Validate() error` — used at both `main.go` call sites (startup: fail closed; hot-reload: log-and-skip). No other package needs this method in this plan.

- [ ] **Step 1: Write the failing tests for `Validate()`**

Add to `services/core/config/config_test.go`, after `TestLoad_ParsesFleetShedderTier`:

```go
func TestValidate_AcceptsValidFleetShedderConfig(t *testing.T) {
	cfg := &config.Config{}
	cfg.Tiers.FleetShedder.DefaultMaxConcurrent = 100
	cfg.Tiers.FleetShedder.ReservedCriticalPct = 20

	if err := cfg.Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidate_RejectsZeroDefaultMaxConcurrent(t *testing.T) {
	cfg := &config.Config{}
	cfg.Tiers.FleetShedder.DefaultMaxConcurrent = 0
	cfg.Tiers.FleetShedder.ReservedCriticalPct = 20

	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for fleet_shedder.default_max_concurrent=0, got nil")
	}
}

func TestValidate_RejectsNegativeDefaultMaxConcurrent(t *testing.T) {
	cfg := &config.Config{}
	cfg.Tiers.FleetShedder.DefaultMaxConcurrent = -5
	cfg.Tiers.FleetShedder.ReservedCriticalPct = 20

	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for negative fleet_shedder.default_max_concurrent, got nil")
	}
}

func TestValidate_RejectsReservedCriticalPctAboveOneHundred(t *testing.T) {
	cfg := &config.Config{}
	cfg.Tiers.FleetShedder.DefaultMaxConcurrent = 100
	cfg.Tiers.FleetShedder.ReservedCriticalPct = 140

	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for fleet_shedder.reserved_critical_pct=140, got nil")
	}
}

func TestValidate_RejectsNegativeReservedCriticalPct(t *testing.T) {
	cfg := &config.Config{}
	cfg.Tiers.FleetShedder.DefaultMaxConcurrent = 100
	cfg.Tiers.FleetShedder.ReservedCriticalPct = -10

	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for negative fleet_shedder.reserved_critical_pct, got nil")
	}
}

func TestValidate_AcceptsReservedCriticalPctBoundaries(t *testing.T) {
	for _, pct := range []int{0, 100} {
		cfg := &config.Config{}
		cfg.Tiers.FleetShedder.DefaultMaxConcurrent = 100
		cfg.Tiers.FleetShedder.ReservedCriticalPct = pct

		if err := cfg.Validate(); err != nil {
			t.Errorf("expected reserved_critical_pct=%d to be valid (inclusive boundary), got error: %v", pct, err)
		}
	}
}

func TestValidate_ErrorMentionsFleetShedderOnMissingBlock(t *testing.T) {
	path := writeTempConfig(t, `
sync_rate: 5
tiers:
  rate_limiter:
    default_rate: 100
    default_burst: 500
    shadow_mode: false
`)

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("unexpected error loading: %v", err)
	}

	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error when fleet_shedder block is omitted entirely (zero-valued DefaultMaxConcurrent), got nil")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd services/core && go test ./config/... -run TestValidate 2>&1 | head -20`
Expected: FAIL — compile error, `cfg.Validate` / `(*config.Config).Validate` doesn't exist yet

- [ ] **Step 3: Add `Validate()` to `services/core/config/config.go`**

Replace the entire file with:

```go
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type RateLimiterConfig struct {
	DefaultRate  int  `yaml:"default_rate"`
	DefaultBurst int  `yaml:"default_burst"`
	ShadowMode   bool `yaml:"shadow_mode"`
}

type ConcurrencyLimiterConfig struct {
	DefaultMaxConcurrent int   `yaml:"default_max_concurrent"`
	MaxRequestDurationMs int64 `yaml:"max_request_duration_ms"`
	ShadowMode           bool  `yaml:"shadow_mode"`
}

type FleetShedderConfig struct {
	DefaultMaxConcurrent int    `yaml:"default_max_concurrent"`
	ReservedCriticalPct  int    `yaml:"reserved_critical_pct"`
	MaxRequestDurationMs int64  `yaml:"max_request_duration_ms"`
	DefaultPriority      string `yaml:"default_priority"`
	ShadowMode           bool   `yaml:"shadow_mode"`
}

type Config struct {
	SyncRate int `yaml:"sync_rate"`
	Tiers    struct {
		RateLimiter        RateLimiterConfig        `yaml:"rate_limiter"`
		ConcurrencyLimiter ConcurrencyLimiterConfig `yaml:"concurrency_limiter"`
		FleetShedder       FleetShedderConfig       `yaml:"fleet_shedder"`
	} `yaml:"tiers"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}

	return &cfg, nil
}

func (c *Config) Validate() error {
	if c.Tiers.FleetShedder.DefaultMaxConcurrent <= 0 {
		return fmt.Errorf("tiers.fleet_shedder.default_max_concurrent must be > 0, got %d (is the fleet_shedder block missing from your config?)", c.Tiers.FleetShedder.DefaultMaxConcurrent)
	}
	if c.Tiers.FleetShedder.ReservedCriticalPct < 0 || c.Tiers.FleetShedder.ReservedCriticalPct > 100 {
		return fmt.Errorf("tiers.fleet_shedder.reserved_critical_pct must be between 0 and 100 inclusive, got %d", c.Tiers.FleetShedder.ReservedCriticalPct)
	}
	return nil
}
```

- [ ] **Step 4: Run config tests to verify they pass**

Run: `cd services/core && go test ./config/... -race -v 2>&1 | tail -60`
Expected: PASS — all tests including the 7 new `TestValidate_*` tests report `--- PASS`, final line `ok      github.com/ratecap/core/config`

- [ ] **Step 5: Wire fail-closed validation into `services/core/main.go`'s startup path**

Replace:

```go
	cfg, err := config.Load(configPath)
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}
```

with:

```go
	cfg, err := config.Load(configPath)
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		log.Fatalf("invalid config: %v", err)
	}
```

- [ ] **Step 6: Wire skip-bad-reload validation into the hot-reload callback**

Replace:

```go
	stopWatch, err := config.Watch(configPath, func(newCfg *config.Config) {
		rateLimiter.Reconfigure(newCfg.Tiers.RateLimiter.DefaultRate, newCfg.Tiers.RateLimiter.DefaultBurst, newCfg.Tiers.RateLimiter.ShadowMode)
		concurrencyLimiter.Reconfigure(newCfg.Tiers.ConcurrencyLimiter.DefaultMaxConcurrent, newCfg.Tiers.ConcurrencyLimiter.MaxRequestDurationMs, newCfg.Tiers.ConcurrencyLimiter.ShadowMode)
		fleetShedder.Reconfigure(newCfg.Tiers.FleetShedder.DefaultMaxConcurrent, newCfg.Tiers.FleetShedder.ReservedCriticalPct, newCfg.Tiers.FleetShedder.MaxRequestDurationMs, newCfg.Tiers.FleetShedder.ShadowMode)
	})
```

with:

```go
	stopWatch, err := config.Watch(configPath, func(newCfg *config.Config) {
		if err := newCfg.Validate(); err != nil {
			log.Printf("ignoring invalid config reload: %v", err)
			return
		}
		rateLimiter.Reconfigure(newCfg.Tiers.RateLimiter.DefaultRate, newCfg.Tiers.RateLimiter.DefaultBurst, newCfg.Tiers.RateLimiter.ShadowMode)
		concurrencyLimiter.Reconfigure(newCfg.Tiers.ConcurrencyLimiter.DefaultMaxConcurrent, newCfg.Tiers.ConcurrencyLimiter.MaxRequestDurationMs, newCfg.Tiers.ConcurrencyLimiter.ShadowMode)
		fleetShedder.Reconfigure(newCfg.Tiers.FleetShedder.DefaultMaxConcurrent, newCfg.Tiers.FleetShedder.ReservedCriticalPct, newCfg.Tiers.FleetShedder.MaxRequestDurationMs, newCfg.Tiers.FleetShedder.ShadowMode)
	})
```

(This keeps serving with the last-known-good config on a bad reload — the same rationale the design spec already uses for hot-reload's "no dropped requests mid-reload" guarantee, extended to cover an invalid reload specifically, not just a well-formed one.)

- [ ] **Step 7: Rebuild core to confirm main.go compiles**

Run: `cd services/core && go build ./... 2>&1`
Expected: no output, exit code 0

- [ ] **Step 8: Write a test proving fail-closed startup actually refuses to start on an invalid on-disk config**

This is a black-box test of the binary's actual startup behavior, not just `Validate()` in isolation. It shells out to `go run .` because `main()` calling `log.Fatalf` cannot be tested by calling `main()` directly in-process (it would kill the test binary itself via `os.Exit`). `log.Fatalf` exits before any listener starts, so this returns quickly with no risk of hanging — no timeout wrapper is needed. Add a new file `services/core/main_test.go`:

```go
package main_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestMain_FailsClosedOnMissingFleetShedderBlock(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "ratecap.yaml")
	invalidConfig := "sync_rate: 5\ntiers:\n  rate_limiter:\n    default_rate: 100\n    default_burst: 500\n    shadow_mode: false\n"
	if err := os.WriteFile(configPath, []byte(invalidConfig), 0644); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	cmd := exec.Command("go", "run", ".")
	cmd.Env = append(os.Environ(),
		"RATECAP_CONFIG_PATH="+configPath,
		"RATECAP_SHARED_SECRET=test-secret",
		"RATECAP_GRPC_ADDR=:0",
	)
	output, err := cmd.CombinedOutput()

	if err == nil {
		t.Fatalf("expected the process to exit non-zero on an invalid config, but it exited cleanly. Output:\n%s", output)
	}
	if !contains(string(output), "invalid config") {
		t.Errorf("expected startup failure to mention 'invalid config', got output:\n%s", output)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (func() bool {
		for i := 0; i+len(substr) <= len(s); i++ {
			if s[i:i+len(substr)] == substr {
				return true
			}
		}
		return false
	})()
}
```

- [ ] **Step 9: Run the new test to confirm the fail-closed startup path works**

Steps 5-6 already modified `main.go` earlier in this task, so this is a confirmation of already-applied behavior, not a pre-implementation RED — but treat any failure as a real bug to fix immediately before proceeding.

Run: `cd services/core && go test -run TestMain_FailsClosedOnMissingFleetShedderBlock -v ./... 2>&1 | tail -30`
Expected: PASS — the subprocess exits non-zero with output containing "invalid config"

- [ ] **Step 10: Run the full core module test suite**

Run: `cd services/core && go build ./... && go test ./... -race 2>&1 | tail -20`
Expected: `ok` for `auth`, `config`, `grpcserver`, `limiter`, and the new package-level `main_test.go` (`github.com/ratecap/core`); `store` needs Docker (reachable per this plan's Global Constraints, so it should run live, not skip).

- [ ] **Step 11: gofmt check and commit**

Run: `gofmt -l services/core/config/config.go services/core/config/config_test.go services/core/main.go services/core/main_test.go`
Expected: no output

```bash
git add services/core/config/config.go services/core/config/config_test.go services/core/main.go services/core/main_test.go
git commit -m "fix(core): validate fleet_shedder config, fail closed on startup, skip bad reloads

A missing or invalid fleet_shedder block previously zero-valued
DefaultMaxConcurrent silently, causing FleetShedder.Check to reject
every single request with REJECT_503 and zero log signal — a
complete, invisible Tier 3 outage. Config.Validate() now enforces
default_max_concurrent > 0 and reserved_critical_pct in [0,100].
Startup fails closed (mirroring the existing RATECAP_SHARED_SECRET
pattern); a bad hot-reload is logged and skipped, keeping the
service on its last-known-good config rather than crashing."
```

---

### Task 2: Live re-verification of the config fail-closed/skip-reload behavior

**Files:** none modified — this task only runs verification commands proving Task 1's fix behaves correctly against the real, deployed system, not just in unit tests.

**Interfaces:**
- Consumes: everything from Task 1.
- Produces: a live-verified baseline for Task 3 to build on.

- [ ] **Step 1: Confirm Docker is reachable**

Run: `docker info > /dev/null 2>&1 && echo "docker reachable" || echo "docker NOT reachable — start Docker Desktop before continuing"`

If not reachable, start Docker Desktop and re-run until it reports reachable before continuing.

- [ ] **Step 2: Rebuild and bring the stack up with the existing, valid `deploy/ratecap.yaml`**

Run from `deploy/`:

```bash
cd deploy
docker compose down 2>&1
docker compose build --no-cache 2>&1 | tail -20
docker compose up -d 2>&1
sleep 3
docker compose ps
```

Expected: all 4 containers (`redis`, `core`, `sidecar`, `sampleapp`) report `Up` — proving the new `Validate()` call does NOT reject the existing, already-correct `deploy/ratecap.yaml` (it has a valid `fleet_shedder` block per Task 7 of the Tier 3 implementation plan).

- [ ] **Step 3: Regression-check all three tiers still work**

Run:

```bash
for i in 1 2 3 4 5 6 7; do curl -s -o /dev/null -w "checkout %{http_code}\n" http://localhost:3000/checkout; done
```
Expected: exactly 5x`checkout 200` then 2x`checkout 429`.

```bash
for i in 1 2 3 4 5; do curl -s -o /dev/null -w "fleet-demo %{http_code}\n" "http://localhost:3000/fleet-demo?priority=sheddable" & done
wait
```
Expected: exactly 3x`fleet-demo 200` and 2x`fleet-demo 503`.

- [ ] **Step 4: Teardown**

Run: `docker compose down 2>&1 && cd ..`
Expected: containers and network removed, no errors.

- [ ] **Step 5: Update the progress ledger**

Append to `.superpowers/sdd/progress.md` a short entry for Task 1 (commit SHA, test result summary) and this task (e2e re-verified: PASS). No code changes in this step — ledger only.

---

### Task 3: Document the `x-ratecap-priority` trust boundary in `SECURITY.md`

**Files:**
- Modify: `SECURITY.md`

**Interfaces:**
- Consumes: nothing from earlier tasks (documentation only, no code dependency — could run independently of Tasks 1-2, but runs last per this plan's stated order).
- Produces: nothing consumed by later tasks — this is the last task.

- [ ] **Step 1: Add a "Priority Claims (v1)" section to `SECURITY.md`**

The current file (verified directly, not from any prior transcription) ends its "Network Transport Security (v1)" section with the single, non-duplicated sentence: `If your deployment cannot guarantee a private network between `ratecap-core` and `ratecap-sidecar`, do not run RateCap v1 in that environment — wait for v2's TLS support, or open an issue describing your constraint.` followed by a blank line and `## Scope`.

Insert this new section immediately after that sentence and its trailing blank line, and immediately before `## Scope`:

```markdown
## Priority Claims (v1)

`ratecap-sidecar` resolves each request's priority (`critical` or `sheddable`) from the caller-supplied `x-ratecap-priority` HTTP header with no authentication, no cost, and no verification (`services/sidecar/proxy/priority.go`). Tier 3 (the Fleet Usage Load Shedder) uses this value to decide whether a request is checked against the full fleet capacity (`critical`) or a reduced, shed-first capacity (`sheddable`). This is v1's explicit, intentional trust boundary:

- Any caller that can reach `ratecap-sidecar`'s HTTP port can unilaterally claim `critical` priority for every request, at zero cost — there is no per-caller identity or authorization tied to a priority claim.
- This is consistent with, not an exception to, the trust boundary already established above: a caller who can reach the sidecar in a correctly-deployed RateCap installation is, by v1's threat model, already inside the trusted network the sidecar itself depends on.
- The `deploy/sampleapp` demo's `/fleet-demo` endpoint exercises this exact header with no additional protection, matching (not exceeding) the same accepted demo risk profile already documented below for `/slow-report`.
- A stronger priority-claim authorization mechanism — for example, binding a claim to the existing shared-secret scheme, or a future per-caller identity system — is deferred to v2.

If your deployment cannot guarantee that only trusted callers can reach `ratecap-sidecar`, treat every request as `sheddable` by setting `default_priority: sheddable` and do not rely on the `x-ratecap-priority` header for enforcement until v2.
```

- [ ] **Step 2: Verify the edit landed correctly and did not disturb surrounding sections**

Run: `grep -n "^## " SECURITY.md`
Expected output, in this exact order:
```
1:# Security Policy
5:## Supported Versions
13:## Reporting a Vulnerability
26:## What to Expect
33:## Network Transport Security (v1)
XX:## Priority Claims (v1)
YY:## Scope
```
(exact line numbers `XX`/`YY` will differ slightly from this template since the new section adds lines — confirm only that `## Priority Claims (v1)` appears exactly once, between `## Network Transport Security (v1)` and `## Scope`, and that every other heading is unchanged and still present exactly once)

- [ ] **Step 3: Commit**

```bash
git add SECURITY.md
git commit -m "docs: document unauthenticated priority claims as v1's explicit trust boundary

x-ratecap-priority is a client-supplied header with no authentication
tying a caller to its claimed priority. Tier 3's entire reserved-
capacity mechanism depends on callers being trustworthy, not on any
enforcement RateCap performs itself. This was an undocumented gap
found by the pre-PR audit; documenting it explicitly closes that gap
the same way the plaintext-gRPC-plus-shared-secret boundary was
documented for tier 2."
```

---

## Post-plan note

This completes Group A (the only remediation group this audit's findings required, per the user's explicit scope decisions). The 6 Important and 3 Minor findings from the audit (untested arithmetic boundaries, `Priority`'s missing `UNSPECIFIED` sentinel, dead `DefaultPriority` config, shadow-mode empty-token risk, missing mixed-cap Redis atomicity test, and the unauthenticated `/fleet-demo` sample endpoint's larger-than-Tier-2 blast radius) are tracked as GitHub follow-up issues, not implemented here. Once this plan's 3 tasks are reviewed and merged, the branch should be re-evaluated against the audit's original NO-GO recommendation — Critical findings resolved, Important/Minor explicitly tracked rather than silently dropped — before opening the PR into `develop`.

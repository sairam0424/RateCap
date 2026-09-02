package criticalroutes_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sairam0424/RateCap/services/sidecar/criticalroutes"
)

func writeRoutesFile(t *testing.T, dir, name, contents string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatalf("failed to write %s: %v", path, err)
	}
	return path
}

func TestLoad_ParsesValidFileIntoExpectedSet(t *testing.T) {
	dir := t.TempDir()
	path := writeRoutesFile(t, dir, "critical-routes.yaml", `critical_routes:
  - "POST /v1/charges"
  - "POST /v1/payment_intents"
`)

	routes, err := criticalroutes.Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(routes) != 2 {
		t.Fatalf("expected 2 routes, got %d: %v", len(routes), routes)
	}
	if _, ok := routes["POST /v1/charges"]; !ok {
		t.Error(`expected "POST /v1/charges" in the parsed set`)
	}
	if _, ok := routes["POST /v1/payment_intents"]; !ok {
		t.Error(`expected "POST /v1/payment_intents" in the parsed set`)
	}
}

func TestLoad_ReturnsErrorOnMalformedYAML(t *testing.T) {
	dir := t.TempDir()
	path := writeRoutesFile(t, dir, "critical-routes.yaml", "critical_routes: [\n  not valid yaml")

	if _, err := criticalroutes.Load(path); err == nil {
		t.Fatal("expected an error parsing malformed YAML, got nil")
	}
}

func TestLoad_ReturnsErrorWhenFileMissing(t *testing.T) {
	dir := t.TempDir()
	if _, err := criticalroutes.Load(filepath.Join(dir, "does-not-exist.yaml")); err == nil {
		t.Fatal("expected an error reading a missing file, got nil")
	}
}

func TestSet_ContainsNilReceiverReturnsFalse(t *testing.T) {
	var s *criticalroutes.Set
	if s.Contains("POST /v1/charges") {
		t.Error("expected Contains on a nil *Set to return false, not panic")
	}
}

func TestSet_ContainsEmptyRouteReturnsFalse(t *testing.T) {
	dir := t.TempDir()
	path := writeRoutesFile(t, dir, "critical-routes.yaml", `critical_routes:
  - "POST /v1/charges"
`)
	s, stop, err := criticalroutes.Watch(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer stop()

	if s.Contains("") {
		t.Error("expected Contains(\"\") to return false")
	}
}

func TestSet_ContainsExactMatch(t *testing.T) {
	dir := t.TempDir()
	path := writeRoutesFile(t, dir, "critical-routes.yaml", `critical_routes:
  - "POST /v1/charges"
`)
	s, stop, err := criticalroutes.Watch(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer stop()

	if !s.Contains("POST /v1/charges") {
		t.Error("expected an exact-match configured route to be found")
	}
}

func TestSet_ContainsDoesNotPrefixMatch(t *testing.T) {
	dir := t.TempDir()
	path := writeRoutesFile(t, dir, "critical-routes.yaml", `critical_routes:
  - "POST /v1/charges"
`)
	s, stop, err := criticalroutes.Watch(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer stop()

	if s.Contains("POST /v1/charges/refund") {
		t.Error(`expected "POST /v1/charges/refund" to NOT match a configured "POST /v1/charges" entry (exact-match only, no prefix matching)`)
	}
}

func TestWatch_ReloadsAndAtomicallySwapsOnFileWrite(t *testing.T) {
	dir := t.TempDir()
	path := writeRoutesFile(t, dir, "critical-routes.yaml", `critical_routes:
  - "POST /v1/charges"
`)

	s, stop, err := criticalroutes.Watch(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer stop()

	if !s.Contains("POST /v1/charges") {
		t.Fatal("expected the initially-loaded route to be found")
	}

	writeRoutesFile(t, dir, "critical-routes.yaml", `critical_routes:
  - "POST /v1/refunds"
`)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s.Contains("POST /v1/refunds") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !s.Contains("POST /v1/refunds") {
		t.Fatal("timed out waiting for Contains to reflect the rewritten routes file")
	}
	if s.Contains("POST /v1/charges") {
		t.Error("expected the old route to no longer match after the swap")
	}
}

func TestWatch_KeepsLastKnownGoodOnMalformedReload(t *testing.T) {
	dir := t.TempDir()
	path := writeRoutesFile(t, dir, "critical-routes.yaml", `critical_routes:
  - "POST /v1/charges"
`)

	s, stop, err := criticalroutes.Watch(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer stop()

	writeRoutesFile(t, dir, "critical-routes.yaml", "critical_routes: [\n  not valid yaml")
	time.Sleep(200 * time.Millisecond)

	if !s.Contains("POST /v1/charges") {
		t.Error("expected the last-known-good route set to still be served after a malformed rewrite")
	}
}

func TestWatch_StopEndsTheWatcher(t *testing.T) {
	dir := t.TempDir()
	path := writeRoutesFile(t, dir, "critical-routes.yaml", `critical_routes:
  - "POST /v1/charges"
`)

	_, stop, err := criticalroutes.Watch(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	stop()
	stop()
}

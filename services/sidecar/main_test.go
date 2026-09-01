package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sairam0424/RateCap/services/sidecar/ratelimit"
)

func TestResolveMaxInflight_EmptyStringReturnsDefault(t *testing.T) {
	got := resolveMaxInflight("", 500)
	if got != 500 {
		t.Errorf("expected 500 for empty string, got %d", got)
	}
}

func TestResolveMaxInflight_ValidPositiveValueIsUsed(t *testing.T) {
	got := resolveMaxInflight("3", 500)
	if got != 3 {
		t.Errorf("expected 3, got %d", got)
	}
}

func TestResolveMaxInflight_UnparseableStringReturnsDefault(t *testing.T) {
	got := resolveMaxInflight("not-a-number", 500)
	if got != 500 {
		t.Errorf("expected 500 for an unparseable value, got %d", got)
	}
}

func TestResolveMaxInflight_ZeroReturnsDefault(t *testing.T) {
	got := resolveMaxInflight("0", 500)
	if got != 500 {
		t.Errorf("expected 500 for a zero value (would shed every request), got %d", got)
	}
}

func TestResolveMaxInflight_NegativeReturnsDefault(t *testing.T) {
	got := resolveMaxInflight("-5", 500)
	if got != 500 {
		t.Errorf("expected 500 for a negative value, got %d", got)
	}
}

func TestResolveRampStartPct_EmptyStringReturnsDefault(t *testing.T) {
	got := resolveRampStartPct("", 100)
	if got != 100 {
		t.Errorf("expected 100 for empty string, got %d", got)
	}
}

func TestResolveRampStartPct_ValidValueIsUsed(t *testing.T) {
	got := resolveRampStartPct("80", 100)
	if got != 80 {
		t.Errorf("expected 80, got %d", got)
	}
}

func TestResolveRampStartPct_UnparseableStringReturnsDefault(t *testing.T) {
	got := resolveRampStartPct("not-a-number", 100)
	if got != 100 {
		t.Errorf("expected 100 for an unparseable value, got %d", got)
	}
}

func TestResolveRampStartPct_ZeroReturnsDefault(t *testing.T) {
	got := resolveRampStartPct("0", 100)
	if got != 100 {
		t.Errorf("expected 100 for a zero value (out of range), got %d", got)
	}
}

func TestResolveRampStartPct_AboveOneHundredReturnsDefault(t *testing.T) {
	got := resolveRampStartPct("150", 100)
	if got != 100 {
		t.Errorf("expected 100 for a value above 100, got %d", got)
	}
}

func TestResolveMaxRPS_EmptyStringReturnsDefault(t *testing.T) {
	got := resolveMaxRPS("", 1000)
	if got != 1000 {
		t.Errorf("expected 1000 for empty string, got %v", got)
	}
}

func TestResolveMaxRPS_ValidPositiveValueIsUsed(t *testing.T) {
	got := resolveMaxRPS("50", 1000)
	if got != 50 {
		t.Errorf("expected 50, got %v", got)
	}
}

func TestResolveMaxRPS_UnparseableStringReturnsDefault(t *testing.T) {
	got := resolveMaxRPS("not-a-number", 1000)
	if got != 1000 {
		t.Errorf("expected 1000 for an unparseable value, got %v", got)
	}
}

func TestResolveMaxRPS_ZeroReturnsDefault(t *testing.T) {
	got := resolveMaxRPS("0", 1000)
	if got != 1000 {
		t.Errorf("expected 1000 for a zero value (would reject every request), got %v", got)
	}
}

func TestResolveMaxRPS_NegativeReturnsDefault(t *testing.T) {
	got := resolveMaxRPS("-5", 1000)
	if got != 1000 {
		t.Errorf("expected 1000 for a negative value, got %v", got)
	}
}

func TestNewTopMux_CheckIsThrottled(t *testing.T) {
	protected := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	tinyLimiter := ratelimit.NewWithClock(0, 0, time.Now)
	metricsHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	healthz := func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }
	adminHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	mux := newTopMux(protected, tinyLimiter, metricsHandler, healthz, adminHandler)
	server := httptest.NewServer(mux)
	defer server.Close()

	resp, err := http.Get(server.URL + "/check")
	if err != nil {
		t.Fatalf("unexpected error calling /check: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("expected /check to be throttled by a zero-burst limiter, got status %d", resp.StatusCode)
	}
}

func TestNewTopMux_MetricsIsThrottled(t *testing.T) {
	protected := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	tinyLimiter := ratelimit.NewWithClock(0, 0, time.Now) // zero burst: every call is throttled
	metricsHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	healthz := func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }
	adminHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	mux := newTopMux(protected, tinyLimiter, metricsHandler, healthz, adminHandler)
	server := httptest.NewServer(mux)
	defer server.Close()

	resp, err := http.Get(server.URL + "/metrics")
	if err != nil {
		t.Fatalf("unexpected error calling /metrics: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("expected /metrics to be throttled by a zero-burst limiter, got status %d", resp.StatusCode)
	}
}

func TestNewTopMux_HealthzIsThrottled(t *testing.T) {
	protected := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	tinyLimiter := ratelimit.NewWithClock(0, 0, time.Now)
	metricsHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	healthz := func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }
	adminHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	mux := newTopMux(protected, tinyLimiter, metricsHandler, healthz, adminHandler)
	server := httptest.NewServer(mux)
	defer server.Close()

	resp, err := http.Get(server.URL + "/healthz")
	if err != nil {
		t.Fatalf("unexpected error calling /healthz: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("expected /healthz to be throttled by a zero-burst limiter, got status %d", resp.StatusCode)
	}
}

func TestNewTopMux_AdminSetLimitIsThrottled(t *testing.T) {
	protected := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	tinyLimiter := ratelimit.NewWithClock(0, 0, time.Now)
	metricsHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	healthz := func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }
	adminHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	mux := newTopMux(protected, tinyLimiter, metricsHandler, healthz, adminHandler)
	server := httptest.NewServer(mux)
	defer server.Close()

	resp, err := http.Post(server.URL+"/admin/set-limit", "application/json", nil)
	if err != nil {
		t.Fatalf("unexpected error calling /admin/set-limit: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("expected /admin/set-limit to be throttled by a zero-burst limiter before reaching its own auth check, got status %d", resp.StatusCode)
	}
}

// TestNewTopMux_AllFiveRoutesShareOneTokenBucket proves the limiter passed
// into newTopMux is the single source of truth for all five routes — not
// five independent allowances — by exhausting the shared bucket via /check
// and confirming the other four routes are immediately rejected too.
func TestNewTopMux_AllFiveRoutesShareOneTokenBucket(t *testing.T) {
	protected := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	sharedLimiter := ratelimit.NewWithClock(1, 1, time.Now)
	metricsHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	healthz := func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }
	adminHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	mux := newTopMux(protected, sharedLimiter, metricsHandler, healthz, adminHandler)
	server := httptest.NewServer(mux)
	defer server.Close()

	resp, err := http.Get(server.URL + "/check")
	if err != nil {
		t.Fatalf("unexpected error calling /check: %v", err)
	}
	resp.Body.Close() //nolint:gosec,errcheck // status code already read; Close error carries no new information
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected the single available token to be consumed by /check, got %d", resp.StatusCode)
	}

	for _, path := range []string{"/release", "/metrics", "/healthz"} {
		resp, err := http.Get(server.URL + path)
		if err != nil {
			t.Fatalf("unexpected error calling %s: %v", path, err)
		}
		resp.Body.Close() //nolint:gosec,errcheck // status code already read; Close error carries no new information
		if resp.StatusCode != http.StatusTooManyRequests {
			t.Errorf("expected %s to be rejected because /check already consumed the shared token, got %d", path, resp.StatusCode)
		}
	}

	resp, err = http.Post(server.URL+"/admin/set-limit", "application/json", nil)
	if err != nil {
		t.Fatalf("unexpected error calling /admin/set-limit: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("expected /admin/set-limit to be rejected because /check already consumed the shared token, got %d", resp.StatusCode)
	}
}

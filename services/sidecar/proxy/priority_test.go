package proxy_test

import (
	"testing"

	"github.com/sairam0424/RateCap/services/sidecar/proxy"
)

func TestResolvePriority_HeaderCriticalOverridesDefault(t *testing.T) {
	got := proxy.ResolvePriority("critical", false, proxy.Sheddable)
	if got != proxy.Critical {
		t.Errorf("expected Critical, got %v", got)
	}
}

func TestResolvePriority_HeaderSheddableOverridesDefault(t *testing.T) {
	got := proxy.ResolvePriority("sheddable", false, proxy.Critical)
	if got != proxy.Sheddable {
		t.Errorf("expected Sheddable, got %v", got)
	}
}

func TestResolvePriority_EmptyHeaderFallsBackToDefault(t *testing.T) {
	got := proxy.ResolvePriority("", false, proxy.Critical)
	if got != proxy.Critical {
		t.Errorf("expected fallback to default Critical, got %v", got)
	}
}

func TestResolvePriority_InvalidHeaderFallsBackToDefault(t *testing.T) {
	got := proxy.ResolvePriority("not-a-real-priority", false, proxy.Critical)
	if got != proxy.Critical {
		t.Errorf("expected fallback to default Critical for invalid header, got %v", got)
	}
}

// TestResolvePriority_PrecedenceTable is the full 8-row table from the
// design spec's Testing Strategy, proving the 3-step order (header, then
// route, then default) — not just that route match works in isolation.
func TestResolvePriority_PrecedenceTable(t *testing.T) {
	tests := []struct {
		name            string
		headerValue     string
		routeMatched    bool
		defaultPriority proxy.Priority
		want            proxy.Priority
	}{
		{"critical header, no route match", "critical", false, proxy.Sheddable, proxy.Critical},
		{"sheddable header, no route match", "sheddable", false, proxy.Critical, proxy.Sheddable},
		{"empty header, no route match, falls back to default", "", false, proxy.Sheddable, proxy.Sheddable},
		{"garbage header, no route match, falls back to default", "garbage", false, proxy.Sheddable, proxy.Sheddable},
		{"empty header, route match applies", "", true, proxy.Sheddable, proxy.Critical},
		{"garbage header falls through to route match, not straight to default", "garbage", true, proxy.Sheddable, proxy.Critical},
		{"header explicitly outranks a route match", "sheddable", true, proxy.Critical, proxy.Sheddable},
		{"critical header and route match agree", "critical", true, proxy.Sheddable, proxy.Critical},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := proxy.ResolvePriority(tc.headerValue, tc.routeMatched, tc.defaultPriority)
			if got != tc.want {
				t.Errorf("ResolvePriority(%q, %v, %v) = %v, want %v", tc.headerValue, tc.routeMatched, tc.defaultPriority, got, tc.want)
			}
		})
	}
}

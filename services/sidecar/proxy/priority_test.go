package proxy_test

import (
	"testing"

	"github.com/ratecap/sidecar/proxy"
)

func TestResolvePriority_HeaderCriticalOverridesDefault(t *testing.T) {
	got := proxy.ResolvePriority("critical", proxy.Sheddable)
	if got != proxy.Critical {
		t.Errorf("expected Critical, got %v", got)
	}
}

func TestResolvePriority_HeaderSheddableOverridesDefault(t *testing.T) {
	got := proxy.ResolvePriority("sheddable", proxy.Critical)
	if got != proxy.Sheddable {
		t.Errorf("expected Sheddable, got %v", got)
	}
}

func TestResolvePriority_EmptyHeaderFallsBackToDefault(t *testing.T) {
	got := proxy.ResolvePriority("", proxy.Critical)
	if got != proxy.Critical {
		t.Errorf("expected fallback to default Critical, got %v", got)
	}
}

func TestResolvePriority_InvalidHeaderFallsBackToDefault(t *testing.T) {
	got := proxy.ResolvePriority("not-a-real-priority", proxy.Critical)
	if got != proxy.Critical {
		t.Errorf("expected fallback to default Critical for invalid header, got %v", got)
	}
}

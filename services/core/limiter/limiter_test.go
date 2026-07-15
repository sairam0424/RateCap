package limiter_test

import (
	"testing"

	"github.com/ratecap/core/limiter"
)

func TestRequest_PriorityDefaultsToSheddable(t *testing.T) {
	var req limiter.Request
	if req.Priority != limiter.Sheddable {
		t.Errorf("expected zero-value Priority to be Sheddable, got %v", req.Priority)
	}
}

func TestPriority_CriticalIsDistinctFromSheddable(t *testing.T) {
	if limiter.Critical == limiter.Sheddable {
		t.Error("expected Critical and Sheddable to be distinct values")
	}
}

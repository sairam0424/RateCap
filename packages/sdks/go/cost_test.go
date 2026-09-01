package ratecap_test

import (
	"testing"

	ratecap "github.com/sairam0424/RateCap/packages/sdks/go"
)

func TestEstimateLLMCost_SumsInputAndMaxTokens(t *testing.T) {
	got := ratecap.EstimateLLMCost(500, 1000)
	if got != 1500 {
		t.Errorf("expected 500+1000=1500, got %d", got)
	}
}

func TestEstimateLLMCost_ZeroInputTokensStillCountsMaxTokens(t *testing.T) {
	got := ratecap.EstimateLLMCost(0, 1000)
	if got != 1000 {
		t.Errorf("expected 1000, got %d", got)
	}
}

func TestEstimateLLMCost_NegativeInputsClampToZero(t *testing.T) {
	got := ratecap.EstimateLLMCost(-10, -5)
	if got != 0 {
		t.Errorf("expected negative inputs to clamp to a 0 cost (never a negative Cost sent to core), got %d", got)
	}
}

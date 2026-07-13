package shadow_test

import (
	"os"
	"testing"

	ratecapv1 "github.com/ratecap/proto/ratecap/v1"

	"github.com/ratecap/sidecar/shadow"
)

func TestGlobalOverrideEnabled_TrueWhenEnvSet(t *testing.T) {
	os.Setenv("RATECAP_SHADOW_MODE", "true")
	defer os.Unsetenv("RATECAP_SHADOW_MODE")

	if !shadow.GlobalOverrideEnabled() {
		t.Error("expected GlobalOverrideEnabled to be true when RATECAP_SHADOW_MODE=true")
	}
}

func TestGlobalOverrideEnabled_FalseWhenEnvUnset(t *testing.T) {
	os.Unsetenv("RATECAP_SHADOW_MODE")

	if shadow.GlobalOverrideEnabled() {
		t.Error("expected GlobalOverrideEnabled to be false when RATECAP_SHADOW_MODE is unset")
	}
}

func TestCoerceIfShadowOverridden_CoercesRejectToShadowLog(t *testing.T) {
	got := shadow.CoerceIfShadowOverridden(ratecapv1.Action_REJECT_429, true)
	if got != ratecapv1.Action_SHADOW_LOG {
		t.Errorf("expected SHADOW_LOG, got %v", got)
	}
}

func TestCoerceIfShadowOverridden_PassesThroughWhenOverrideDisabled(t *testing.T) {
	got := shadow.CoerceIfShadowOverridden(ratecapv1.Action_REJECT_429, false)
	if got != ratecapv1.Action_REJECT_429 {
		t.Errorf("expected REJECT_429 unchanged, got %v", got)
	}
}

func TestCoerceIfShadowOverridden_AllowPassesThroughRegardless(t *testing.T) {
	got := shadow.CoerceIfShadowOverridden(ratecapv1.Action_ALLOW, true)
	if got != ratecapv1.Action_ALLOW {
		t.Errorf("expected ALLOW unchanged even in override, got %v", got)
	}
}

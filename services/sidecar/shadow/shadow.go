package shadow

import "os"

import ratecapv1 "github.com/ratecap/proto/ratecap/v1"

func GlobalOverrideEnabled() bool {
	return os.Getenv("RATECAP_SHADOW_MODE") == "true"
}

func CoerceIfShadowOverridden(action ratecapv1.Action, override bool) ratecapv1.Action {
	if !override {
		return action
	}
	if action == ratecapv1.Action_REJECT_429 || action == ratecapv1.Action_REJECT_503 {
		return ratecapv1.Action_SHADOW_LOG
	}
	return action
}

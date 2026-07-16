package tlsconfig_test

import (
	"testing"

	"github.com/ratecap/core/tlsconfig"
)

func TestEnvVarsPartiallySet_AllEmptyIsNotPartial(t *testing.T) {
	if tlsconfig.EnvVarsPartiallySet("", "", "") {
		t.Error("expected all-empty to not be considered partial (TLS simply disabled)")
	}
}

func TestEnvVarsPartiallySet_AllSetIsNotPartial(t *testing.T) {
	if tlsconfig.EnvVarsPartiallySet("cert.pem", "key.pem", "ca.pem") {
		t.Error("expected all-set to not be considered partial (TLS fully configured)")
	}
}

func TestEnvVarsPartiallySet_OnlyCertSetIsPartial(t *testing.T) {
	if !tlsconfig.EnvVarsPartiallySet("cert.pem", "", "") {
		t.Error("expected cert-only to be considered partial")
	}
}

func TestEnvVarsPartiallySet_CertAndKeySetButNoCAIsPartial(t *testing.T) {
	if !tlsconfig.EnvVarsPartiallySet("cert.pem", "key.pem", "") {
		t.Error("expected cert+key without CA to be considered partial")
	}
}

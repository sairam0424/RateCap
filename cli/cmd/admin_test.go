package cmd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestAdminSetLimit_SendsCorrectRequestAndPrintsResult(t *testing.T) {
	var receivedBody map[string]any
	var receivedSecret string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedSecret = r.Header.Get("X-RateCap-Admin-Secret")
		_ = json.NewDecoder(r.Body).Decode(&receivedBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"tier": "rate_limiter", "previous_value": 100, "new_value": 500})
	}))
	defer server.Close()

	root := NewRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"admin", "set-limit", "--sidecar-addr", server.URL, "--admin-secret", "test-secret", "--tier", "rate_limiter", "--value", "500"})

	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if receivedSecret != "test-secret" {
		t.Errorf("expected admin secret header to be forwarded, got %q", receivedSecret)
	}
	if receivedBody["tier"] != "rate_limiter" || receivedBody["value"].(float64) != 500 {
		t.Errorf("expected request body {tier: rate_limiter, value: 500}, got %v", receivedBody)
	}
	if out.String() == "" {
		t.Error("expected some output confirming the result")
	}
}

func TestAdminSetLimit_FallsBackToEnvVarWhenFlagOmitted(t *testing.T) {
	var receivedSecret string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedSecret = r.Header.Get("X-RateCap-Admin-Secret")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"tier": "rate_limiter", "previous_value": 100, "new_value": 500})
	}))
	defer server.Close()

	t.Setenv("RATECAP_ADMIN_SECRET", "env-secret")

	root := NewRootCmd()
	root.SetOut(&bytes.Buffer{})
	root.SetArgs([]string{"admin", "set-limit", "--sidecar-addr", server.URL, "--tier", "rate_limiter", "--value", "500"})

	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if receivedSecret != "env-secret" {
		t.Errorf("expected the RATECAP_ADMIN_SECRET env var to be used when --admin-secret is omitted, got %q", receivedSecret)
	}
}

func TestAdminSetLimit_ErrorsWhenNoSecretProvidedEitherWay(t *testing.T) {
	os.Unsetenv("RATECAP_ADMIN_SECRET")

	root := NewRootCmd()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"admin", "set-limit", "--sidecar-addr", "http://localhost:1", "--tier", "rate_limiter", "--value", "500"})

	if err := root.Execute(); err == nil {
		t.Error("expected an error when neither --admin-secret nor RATECAP_ADMIN_SECRET is set")
	}
}

func TestAdminSetLimit_RequiresTierFlag(t *testing.T) {
	root := NewRootCmd()
	root.SetArgs([]string{"admin", "set-limit", "--value", "500"})
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})

	if err := root.Execute(); err == nil {
		t.Error("expected an error when --tier is not provided")
	}
}

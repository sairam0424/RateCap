package ratecap_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	ratecap "github.com/ratecap/sdk-go"
)

func TestAllow_ReturnsTrueOn200(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := ratecap.NewClient(server.URL)
	allowed, _, err := client.Allow(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !allowed {
		t.Error("expected allowed=true on 200 response")
	}
}

func TestAllow_ReturnsFalseWithRetryAfterOn429(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After-Ms", "750")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	client := ratecap.NewClient(server.URL)
	allowed, retryAfterMs, err := client.Allow(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if allowed {
		t.Error("expected allowed=false on 429 response")
	}
	if retryAfterMs != 750 {
		t.Errorf("expected retryAfterMs=750, got %d", retryAfterMs)
	}
}

func TestAllow_ReturnsFalseOn503(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	client := ratecap.NewClient(server.URL)
	allowed, _, err := client.Allow(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if allowed {
		t.Error("expected allowed=false on 503 response")
	}
}

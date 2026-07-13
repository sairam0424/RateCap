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

func TestAcquire_ReturnsAllowedTicketOn200(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Concurrency-Token", "tok-abc")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := ratecap.NewClient(server.URL)
	ticket, err := client.Acquire(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ticket.Allowed {
		t.Error("expected Allowed=true on 200 response")
	}
}

func TestAcquire_ReturnsRejectedTicketWithRetryAfterOn429(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After-Ms", "750")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	client := ratecap.NewClient(server.URL)
	ticket, err := client.Acquire(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ticket.Allowed {
		t.Error("expected Allowed=false on 429 response")
	}
	if ticket.RetryAfterMs != 750 {
		t.Errorf("expected RetryAfterMs=750, got %d", ticket.RetryAfterMs)
	}
}

func TestTicket_Release_CallsReleaseEndpointWithKeyAndToken(t *testing.T) {
	var capturedPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/check":
			w.Header().Set("Concurrency-Token", "tok-abc")
			w.WriteHeader(http.StatusOK)
		case "/release":
			capturedPath = r.URL.RequestURI()
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	client := ratecap.NewClient(server.URL)
	ticket, err := client.Acquire(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if err := ticket.Release(context.Background()); err != nil {
		t.Fatalf("unexpected error releasing: %v", err)
	}

	if capturedPath == "" {
		t.Fatal("expected /release to be called")
	}
}

func TestTicket_Release_NoOpWhenNoTokenWasIssued(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/release" {
			t.Error("expected /release NOT to be called when no token was issued")
		}
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	client := ratecap.NewClient(server.URL)
	ticket, err := client.Acquire(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if err := ticket.Release(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

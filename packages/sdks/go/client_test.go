package ratecap_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
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

func TestAllow_RequestsSkipReservations(t *testing.T) {
	var capturedQuery url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedQuery = r.URL.Query()
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := ratecap.NewClient(server.URL)
	if _, _, err := client.Allow(context.Background(), "user-1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := capturedQuery.Get("skip_reservations"); got != "true" {
		t.Errorf("expected skip_reservations=true on Allow()'s /check request, got %q", got)
	}
}

func TestAcquire_DoesNotRequestSkipReservations(t *testing.T) {
	var capturedQuery url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedQuery = r.URL.Query()
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := ratecap.NewClient(server.URL)
	if _, err := client.Acquire(context.Background(), "user-1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := capturedQuery.Get("skip_reservations"); got != "" {
		t.Errorf("expected no skip_reservations param on Acquire()'s /check request, got %q", got)
	}
}

func TestAcquire_ReturnsAllowedTicketOn200(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Concurrency-Token-0", "tok-abc")
		w.Header().Set("Concurrency-Key-0", "user-1")
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

func TestTicket_Release_UsesServerSuppliedConcurrencyKeyNotCallerKey(t *testing.T) {
	var capturedQuery url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/check":
			w.Header().Set("Concurrency-Token-0", "tok-abc")
			w.Header().Set("Concurrency-Key-0", "server-assigned-key")
			w.WriteHeader(http.StatusOK)
		case "/release":
			capturedQuery = r.URL.Query()
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	client := ratecap.NewClient(server.URL)
	ticket, err := client.Acquire(context.Background(), "caller-supplied-key")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if err := ticket.Release(context.Background()); err != nil {
		t.Fatalf("unexpected error releasing: %v", err)
	}

	if capturedQuery == nil {
		t.Fatal("expected /release to be called")
	}
	if got := capturedQuery.Get("key"); got != "server-assigned-key" {
		t.Errorf("expected key=server-assigned-key (from Concurrency-Key-0 header, not the caller's Acquire key), got %q", got)
	}
	if got := capturedQuery.Get("token"); got != "tok-abc" {
		t.Errorf("expected token=tok-abc, got %q", got)
	}
}

func TestTicket_Release_ReleasesEveryReservation(t *testing.T) {
	var releaseCalls []url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/check":
			w.Header().Set("Concurrency-Token-0", "tok-abc")
			w.Header().Set("Concurrency-Key-0", "user-1")
			w.Header().Set("Concurrency-Token-1", "tok-xyz")
			w.Header().Set("Concurrency-Key-1", "fleet")
			w.WriteHeader(http.StatusOK)
		case "/release":
			releaseCalls = append(releaseCalls, r.URL.Query())
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

	if len(releaseCalls) != 2 {
		t.Fatalf("expected 2 /release calls (one per reservation), got %d", len(releaseCalls))
	}

	byKey := map[string]string{}
	for _, q := range releaseCalls {
		byKey[q.Get("key")] = q.Get("token")
	}
	if byKey["user-1"] != "tok-abc" {
		t.Errorf("expected a release call for key=user-1 token=tok-abc, got %+v", byKey)
	}
	if byKey["fleet"] != "tok-xyz" {
		t.Errorf("expected a release call for key=fleet token=tok-xyz, got %+v", byKey)
	}
}

func TestTicket_Release_ReturnsErrorOnNon200FromSidecar(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/check":
			w.Header().Set("Concurrency-Token-0", "tok-abc")
			w.Header().Set("Concurrency-Key-0", "user-1")
			w.WriteHeader(http.StatusOK)
		case "/release":
			http.Error(w, "upstream release failed", http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	client := ratecap.NewClient(server.URL)
	ticket, err := client.Acquire(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if err := ticket.Release(context.Background()); err == nil {
		t.Fatal("expected error when sidecar returns non-200 from /release")
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

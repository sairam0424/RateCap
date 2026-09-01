package ratecap_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	ratecap "github.com/sairam0424/RateCap/packages/sdks/go"
)

func TestAllow_ReturnsTrueOn200(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := ratecap.NewClient(server.URL)
	allowed, _, _, err := client.Allow(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !allowed {
		t.Error("expected allowed=true on 200 response")
	}
}

func TestAllow_ReturnsFalseWithRetryAfterAndRateLimitResetOn429(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After-Ms", "750")
		w.Header().Set("RateLimit-Reset", "3")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	client := ratecap.NewClient(server.URL)
	allowed, retryAfterMs, rateLimitReset, err := client.Allow(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if allowed {
		t.Error("expected allowed=false on 429 response")
	}
	if retryAfterMs != 750 {
		t.Errorf("expected retryAfterMs=750, got %d", retryAfterMs)
	}
	if rateLimitReset != 3 {
		t.Errorf("expected rateLimitReset=3, got %d", rateLimitReset)
	}
}

func TestAllow_ReturnsFalseOn503(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	client := ratecap.NewClient(server.URL)
	allowed, _, _, err := client.Allow(context.Background(), "user-1")
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
	if _, _, _, err := client.Allow(context.Background(), "user-1"); err != nil {
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

func TestAcquire_ReturnsRejectedTicketWithRetryAfterAndRateLimitResetOn429(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After-Ms", "750")
		w.Header().Set("RateLimit-Reset", "3")
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
	if ticket.RateLimitReset != 3 {
		t.Errorf("expected RateLimitReset=3, got %d", ticket.RateLimitReset)
	}
}

func TestTicket_Release_UsesServerSuppliedConcurrencyKeyNotCallerKey(t *testing.T) {
	var capturedHeaders http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/check":
			w.Header().Set("Concurrency-Token-0", "tok-abc")
			w.Header().Set("Concurrency-Key-0", "server-assigned-key")
			w.WriteHeader(http.StatusOK)
		case "/release":
			capturedHeaders = r.Header
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

	if capturedHeaders == nil {
		t.Fatal("expected /release to be called")
	}
	if got := capturedHeaders.Get("X-RateCap-Concurrency-Key"); got != "server-assigned-key" {
		t.Errorf("expected key=server-assigned-key (from Concurrency-Key-0 header, not the caller's Acquire key), got %q", got)
	}
	if got := capturedHeaders.Get("X-RateCap-Concurrency-Token"); got != "tok-abc" {
		t.Errorf("expected token=tok-abc, got %q", got)
	}
}

func TestTicket_Release_ReleasesEveryReservation(t *testing.T) {
	var releaseCalls []http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/check":
			w.Header().Set("Concurrency-Token-0", "tok-abc")
			w.Header().Set("Concurrency-Key-0", "user-1")
			w.Header().Set("Concurrency-Token-1", "tok-xyz")
			w.Header().Set("Concurrency-Key-1", "fleet")
			w.WriteHeader(http.StatusOK)
		case "/release":
			releaseCalls = append(releaseCalls, r.Header)
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
	for _, h := range releaseCalls {
		byKey[h.Get("X-RateCap-Concurrency-Key")] = h.Get("X-RateCap-Concurrency-Token")
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

func TestAllow_WithCost_SendsCostQueryParam(t *testing.T) {
	var capturedQuery url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedQuery = r.URL.Query()
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := ratecap.NewClient(server.URL)
	if _, _, _, err := client.Allow(context.Background(), "user-1", ratecap.WithCost(5)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := capturedQuery.Get("cost"); got != "5" {
		t.Errorf("expected cost=5 on the /check request, got %q", got)
	}
}

func TestAllow_WithoutCostOption_OmitsCostQueryParam(t *testing.T) {
	var capturedQuery url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedQuery = r.URL.Query()
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := ratecap.NewClient(server.URL)
	if _, _, _, err := client.Allow(context.Background(), "user-1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := capturedQuery.Get("cost"); got != "" {
		t.Errorf("expected no cost param when WithCost is not used (server-side default of 1 applies), got %q", got)
	}
}

func TestAllow_WithPriority_SendsPriorityHeader(t *testing.T) {
	var capturedHeader string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedHeader = r.Header.Get("x-ratecap-priority")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := ratecap.NewClient(server.URL)
	if _, _, _, err := client.Allow(context.Background(), "user-1", ratecap.WithPriority(ratecap.Critical)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if capturedHeader != "critical" {
		t.Errorf(`expected x-ratecap-priority: critical, got %q`, capturedHeader)
	}
}

func TestAcquire_WithCostAndPriority_SendsBoth(t *testing.T) {
	var capturedQuery url.Values
	var capturedHeader string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedQuery = r.URL.Query()
		capturedHeader = r.Header.Get("x-ratecap-priority")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := ratecap.NewClient(server.URL)
	if _, err := client.Acquire(context.Background(), "user-1", ratecap.WithCost(1500), ratecap.WithPriority(ratecap.Critical)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := capturedQuery.Get("cost"); got != "1500" {
		t.Errorf("expected cost=1500, got %q", got)
	}
	if capturedHeader != "critical" {
		t.Errorf(`expected x-ratecap-priority: critical, got %q`, capturedHeader)
	}
}

func TestTicket_Refund_SendsRefundHeaders(t *testing.T) {
	var refundCalls []http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/check":
			w.WriteHeader(http.StatusOK)
		case "/release":
			refundCalls = append(refundCalls, r.Header)
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	client := ratecap.NewClient(server.URL)
	ticket, err := client.Acquire(context.Background(), "user-1", ratecap.WithCost(1500))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if err := ticket.Refund(context.Background(), 1200); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(refundCalls) != 1 {
		t.Fatalf("expected exactly 1 /release call for the refund, got %d", len(refundCalls))
	}
	if got := refundCalls[0].Get("X-RateCap-Refund-Key"); got != "user-1" {
		t.Errorf("expected X-RateCap-Refund-Key=user-1, got %q", got)
	}
	if got := refundCalls[0].Get("X-RateCap-Refund-Amount"); got != "1200" {
		t.Errorf("expected X-RateCap-Refund-Amount=1200, got %q", got)
	}
}

func TestTicket_Refund_ReturnsErrorOnNon200(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/check":
			w.WriteHeader(http.StatusOK)
		case "/release":
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	client := ratecap.NewClient(server.URL)
	ticket, err := client.Acquire(context.Background(), "user-1", ratecap.WithCost(1500))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if err := ticket.Refund(context.Background(), 1200); err == nil {
		t.Fatal("expected an error when the sidecar returns non-200 for the refund")
	}
}

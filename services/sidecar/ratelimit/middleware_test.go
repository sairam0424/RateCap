package ratelimit_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sairam0424/RateCap/services/sidecar/ratelimit"
)

func TestMiddleware_AllowsRequestAndCallsNextHandler(t *testing.T) {
	l := ratelimit.NewWithClock(5, 5, fixedClock(time.Unix(0, 0)))

	nextCalled := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusOK)
	})

	handler := ratelimit.Middleware(l, next)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/check", nil)
	handler.ServeHTTP(rec, req)

	if !nextCalled {
		t.Fatal("expected next handler to be called when a token is available")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 from next handler, got %d", rec.Code)
	}
}

func TestMiddleware_RejectsWith429WhenBucketExhausted(t *testing.T) {
	l := ratelimit.NewWithClock(1, 1, fixedClock(time.Unix(0, 0)))

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := ratelimit.Middleware(l, next)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/check", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected first request to succeed with 200, got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/check", nil))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected second request to be rejected with 429, got %d", rec.Code)
	}
}

func TestMiddleware_DoesNotCallNextHandlerWhenRejected(t *testing.T) {
	l := ratelimit.NewWithClock(1, 1, fixedClock(time.Unix(0, 0)))
	l.Allow() // exhaust the single token before the middleware ever runs

	nextCalled := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
	})
	handler := ratelimit.Middleware(l, next)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/check", nil))

	if nextCalled {
		t.Fatal("expected next handler NOT to be called when the request is rejected")
	}
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", rec.Code)
	}
}

func TestMiddleware_SetsRetryAfterHeaderOnRejection(t *testing.T) {
	l := ratelimit.NewWithClock(1, 1, fixedClock(time.Unix(0, 0)))
	l.Allow()

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	handler := ratelimit.Middleware(l, next)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/check", nil))

	retryAfter := rec.Header().Get("Retry-After")
	if retryAfter == "" {
		t.Fatal("expected Retry-After header to be set on a 429 rejection")
	}
}

func TestMiddleware_DoesNotSetRetryAfterHeaderWhenAllowed(t *testing.T) {
	l := ratelimit.NewWithClock(5, 5, fixedClock(time.Unix(0, 0)))

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := ratelimit.Middleware(l, next)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/check", nil))

	if rec.Header().Get("Retry-After") != "" {
		t.Fatal("expected no Retry-After header when the request is allowed")
	}
}

func TestMiddleware_AppliesUniformlyAcrossDifferentPaths(t *testing.T) {
	l := ratelimit.NewWithClock(1, 1, fixedClock(time.Unix(0, 0)))

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := ratelimit.Middleware(l, next)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/check", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected /check to consume the single shared token, got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/release", nil))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected /release to be rejected because the shared token bucket is process-global, got %d", rec.Code)
	}
}

package proxy_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"google.golang.org/grpc"

	ratecapv1 "github.com/ratecap/proto/ratecap/v1"

	"github.com/ratecap/sidecar/proxy"
)

type fakeRatecapClient struct {
	resp    *ratecapv1.CheckRateLimitResponse
	err     error
	lastReq *ratecapv1.CheckRateLimitRequest
}

func (f *fakeRatecapClient) CheckRateLimit(_ context.Context, in *ratecapv1.CheckRateLimitRequest, _ ...grpc.CallOption) (*ratecapv1.CheckRateLimitResponse, error) {
	f.lastReq = in
	return f.resp, f.err
}

func TestServeHTTP_AllowReturns200(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_ALLOW}}
	h := proxy.NewHandler(client, proxy.Sheddable)

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}

func TestServeHTTP_Reject429Returns429(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_REJECT_429, RetryAfterMs: 500}}
	h := proxy.NewHandler(client, proxy.Sheddable)

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429, got %d", rec.Code)
	}
	if rec.Header().Get("Retry-After-Ms") != "500" {
		t.Errorf("expected Retry-After-Ms header of 500, got %q", rec.Header().Get("Retry-After-Ms"))
	}
}

func TestServeHTTP_ShadowLogReturns200(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_SHADOW_LOG}}
	h := proxy.NewHandler(client, proxy.Sheddable)

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 in shadow mode, got %d", rec.Code)
	}
}

func TestServeHTTP_ParsesPriorityHeaderWithoutError(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_ALLOW}}
	h := proxy.NewHandler(client, proxy.Sheddable)

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	req.Header.Set("x-ratecap-priority", "critical")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 regardless of priority header (tier 1 ignores it), got %d", rec.Code)
	}
}

func TestServeHTTP_SetsConcurrencyTokenAndKeyHeadersWhenReservationPresent(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{
		Action:       ratecapv1.Action_ALLOW,
		Reservations: []*ratecapv1.TokenReservation{{Key: "user-1", Token: "tok-abc"}},
	}}
	h := proxy.NewHandler(client, proxy.Sheddable)

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Header().Get("Concurrency-Token") != "tok-abc" {
		t.Errorf("expected Concurrency-Token header %q, got %q", "tok-abc", rec.Header().Get("Concurrency-Token"))
	}
	if rec.Header().Get("Concurrency-Key") != "user-1" {
		t.Errorf("expected Concurrency-Key header %q, got %q", "user-1", rec.Header().Get("Concurrency-Key"))
	}
}

func TestServeHTTP_OmitsConcurrencyHeadersWhenNoReservations(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_ALLOW}}
	h := proxy.NewHandler(client, proxy.Sheddable)

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Header().Get("Concurrency-Token") != "" {
		t.Errorf("expected no Concurrency-Token header, got %q", rec.Header().Get("Concurrency-Token"))
	}
	if rec.Header().Get("Concurrency-Key") != "" {
		t.Errorf("expected no Concurrency-Key header, got %q", rec.Header().Get("Concurrency-Key"))
	}
}

func TestServeHTTP_SkipConcurrencyParamSetsSkipConcurrencyLimitOnRequest(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_ALLOW}}
	h := proxy.NewHandler(client, proxy.Sheddable)

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1&skip_concurrency=true", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if client.lastReq == nil {
		t.Fatal("expected CheckRateLimit to be called")
	}
	if !client.lastReq.SkipConcurrencyLimit {
		t.Error("expected SkipConcurrencyLimit=true when skip_concurrency=true query param is set")
	}
}

func TestServeHTTP_NoSkipConcurrencyParamLeavesSkipConcurrencyLimitFalse(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_ALLOW}}
	h := proxy.NewHandler(client, proxy.Sheddable)

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if client.lastReq == nil {
		t.Fatal("expected CheckRateLimit to be called")
	}
	if client.lastReq.SkipConcurrencyLimit {
		t.Error("expected SkipConcurrencyLimit=false when skip_concurrency param is absent")
	}
}

type fakeReleaseClient struct {
	lastKey   string
	lastToken string
	err       error
}

func (f *fakeReleaseClient) ReleaseConcurrency(_ context.Context, in *ratecapv1.ReleaseConcurrencyRequest, _ ...grpc.CallOption) (*ratecapv1.ReleaseConcurrencyResponse, error) {
	f.lastKey = in.Key
	f.lastToken = in.ConcurrencyToken
	return &ratecapv1.ReleaseConcurrencyResponse{}, f.err
}

func TestReleaseHandler_ServeHTTP_CallsReleaseConcurrencyWithKeyAndToken(t *testing.T) {
	client := &fakeReleaseClient{}
	h := proxy.NewReleaseHandler(client)

	req := httptest.NewRequest(http.MethodPost, "/release?key=user-1&token=tok-abc", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if client.lastKey != "user-1" {
		t.Errorf("expected ReleaseConcurrency called with key=%q, got %q", "user-1", client.lastKey)
	}
	if client.lastToken != "tok-abc" {
		t.Errorf("expected ReleaseConcurrency called with token=%q, got %q", "tok-abc", client.lastToken)
	}
}

func TestReleaseHandler_ServeHTTP_MissingKeyReturns400(t *testing.T) {
	client := &fakeReleaseClient{}
	h := proxy.NewReleaseHandler(client)

	req := httptest.NewRequest(http.MethodPost, "/release?token=tok-abc", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}
}

func TestReleaseHandler_ServeHTTP_UpstreamErrorReturns500(t *testing.T) {
	client := &fakeReleaseClient{err: errors.New("core unavailable")}
	h := proxy.NewReleaseHandler(client)

	req := httptest.NewRequest(http.MethodPost, "/release?key=user-1&token=tok-abc", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", rec.Code)
	}
}

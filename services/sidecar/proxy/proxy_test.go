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
	"github.com/ratecap/sidecar/worker"
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
	h := proxy.NewHandler(client, proxy.Sheddable, worker.NewShedder(1000))

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}

func TestServeHTTP_Reject429Returns429(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_REJECT_429, RetryAfterMs: 500}}
	h := proxy.NewHandler(client, proxy.Sheddable, worker.NewShedder(1000))

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
	h := proxy.NewHandler(client, proxy.Sheddable, worker.NewShedder(1000))

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 in shadow mode, got %d", rec.Code)
	}
}

func TestServeHTTP_ParsesPriorityHeaderWithoutError(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_ALLOW}}
	h := proxy.NewHandler(client, proxy.Sheddable, worker.NewShedder(1000))

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	req.Header.Set("x-ratecap-priority", "critical")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 regardless of priority header (tier 1 ignores it), got %d", rec.Code)
	}
}

func TestServeHTTP_ThreadsCriticalPriorityHeaderIntoRequest(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_ALLOW}}
	h := proxy.NewHandler(client, proxy.Sheddable, worker.NewShedder(1000))

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	req.Header.Set("x-ratecap-priority", "critical")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if client.lastReq == nil {
		t.Fatal("expected CheckRateLimit to be called")
	}
	if client.lastReq.Priority != ratecapv1.Priority_CRITICAL {
		t.Errorf("expected Priority_CRITICAL on the outgoing request, got %v", client.lastReq.Priority)
	}
}

func TestServeHTTP_DefaultsToSheddablePriorityWhenNoHeader(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_ALLOW}}
	h := proxy.NewHandler(client, proxy.Sheddable, worker.NewShedder(1000))

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if client.lastReq == nil {
		t.Fatal("expected CheckRateLimit to be called")
	}
	if client.lastReq.Priority != ratecapv1.Priority_SHEDDABLE {
		t.Errorf("expected Priority_SHEDDABLE by default, got %v", client.lastReq.Priority)
	}
}

func TestServeHTTP_SetsIndexedConcurrencyHeadersForEachReservation(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{
		Action: ratecapv1.Action_ALLOW,
		Reservations: []*ratecapv1.TokenReservation{
			{Key: "user-1", Token: "tok-abc"},
			{Key: "fleet", Token: "tok-xyz"},
		},
	}}
	h := proxy.NewHandler(client, proxy.Sheddable, worker.NewShedder(1000))

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Header().Get("Concurrency-Token-0") != "tok-abc" {
		t.Errorf("expected Concurrency-Token-0 %q, got %q", "tok-abc", rec.Header().Get("Concurrency-Token-0"))
	}
	if rec.Header().Get("Concurrency-Key-0") != "user-1" {
		t.Errorf("expected Concurrency-Key-0 %q, got %q", "user-1", rec.Header().Get("Concurrency-Key-0"))
	}
	if rec.Header().Get("Concurrency-Token-1") != "tok-xyz" {
		t.Errorf("expected Concurrency-Token-1 %q, got %q", "tok-xyz", rec.Header().Get("Concurrency-Token-1"))
	}
	if rec.Header().Get("Concurrency-Key-1") != "fleet" {
		t.Errorf("expected Concurrency-Key-1 %q, got %q", "fleet", rec.Header().Get("Concurrency-Key-1"))
	}
	if rec.Header().Get("Concurrency-Token-2") != "" {
		t.Errorf("expected no Concurrency-Token-2 header (only 2 reservations), got %q", rec.Header().Get("Concurrency-Token-2"))
	}
}

func TestServeHTTP_OmitsIndexedConcurrencyHeadersWhenNoReservations(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_ALLOW}}
	h := proxy.NewHandler(client, proxy.Sheddable, worker.NewShedder(1000))

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Header().Get("Concurrency-Token-0") != "" {
		t.Errorf("expected no Concurrency-Token-0 header, got %q", rec.Header().Get("Concurrency-Token-0"))
	}
	if rec.Header().Get("Concurrency-Key-0") != "" {
		t.Errorf("expected no Concurrency-Key-0 header, got %q", rec.Header().Get("Concurrency-Key-0"))
	}
}

func TestServeHTTP_SkipReservationsParamSetsSkipReservationsOnRequest(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_ALLOW}}
	h := proxy.NewHandler(client, proxy.Sheddable, worker.NewShedder(1000))

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1&skip_reservations=true", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if client.lastReq == nil {
		t.Fatal("expected CheckRateLimit to be called")
	}
	if !client.lastReq.SkipReservations {
		t.Error("expected SkipReservations=true when skip_reservations=true query param is set")
	}
}

func TestServeHTTP_NoSkipReservationsParamLeavesSkipReservationsFalse(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_ALLOW}}
	h := proxy.NewHandler(client, proxy.Sheddable, worker.NewShedder(1000))

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if client.lastReq == nil {
		t.Fatal("expected CheckRateLimit to be called")
	}
	if client.lastReq.SkipReservations {
		t.Error("expected SkipReservations=false when skip_reservations param is absent")
	}
}

func TestServeHTTP_RejectsNonGETMethod(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_ALLOW}}
	h := proxy.NewHandler(client, proxy.Sheddable, worker.NewShedder(1000))

	req := httptest.NewRequest(http.MethodPost, "/check?key=user-1", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", rec.Code)
	}
}

func TestServeHTTP_ShedsWithoutCallingClientWhenOverInFlightLimit(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_ALLOW}}
	shedder := worker.NewShedder(0)
	h := proxy.NewHandler(client, proxy.Sheddable, shedder)

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", rec.Code)
	}
	if client.lastReq != nil {
		t.Error("expected CheckRateLimit to never be called when the in-flight limit is exceeded")
	}
}

func TestServeHTTP_AllowsRequestAndReleasesSlotWhenUnderLimit(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_ALLOW}}
	shedder := worker.NewShedder(1)
	h := proxy.NewHandler(client, proxy.Sheddable, shedder)

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if client.lastReq == nil {
		t.Fatal("expected CheckRateLimit to be called when under the in-flight limit")
	}

	if !shedder.Allow() {
		t.Fatal("expected the slot to have been released after ServeHTTP returned, but Allow() still reports over-limit")
	}
}

func TestServeHTTP_ShadowModeProceedsToClientInsteadOfShedding(t *testing.T) {
	t.Setenv("RATECAP_SHADOW_MODE", "true")

	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_ALLOW}}
	shedder := worker.NewShedder(0)
	h := proxy.NewHandler(client, proxy.Sheddable, shedder)

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if client.lastReq == nil {
		t.Fatal("expected CheckRateLimit to be called even though the in-flight limit was exceeded, since shadow mode is active")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 (core's own ALLOW response, shadow mode doesn't force a code here), got %d", rec.Code)
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

func TestReleaseHandler_ServeHTTP_RejectsNonPOSTMethod(t *testing.T) {
	client := &fakeReleaseClient{}
	h := proxy.NewReleaseHandler(client)

	req := httptest.NewRequest(http.MethodGet, "/release?key=user-1&token=tok-abc", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", rec.Code)
	}
}

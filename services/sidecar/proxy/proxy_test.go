package proxy_test

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/prometheus/client_golang/prometheus/testutil"

	ratecapv1 "github.com/sairam0424/RateCap/proto/ratecap/v1"

	"github.com/sairam0424/RateCap/services/sidecar/criticalroutes"
	"github.com/sairam0424/RateCap/services/sidecar/decisionlog"
	"github.com/sairam0424/RateCap/services/sidecar/metrics"
	"github.com/sairam0424/RateCap/services/sidecar/negativecache"
	"github.com/sairam0424/RateCap/services/sidecar/proxy"
	"github.com/sairam0424/RateCap/services/sidecar/worker"
)

type fakeRatecapClient struct {
	resp    *ratecapv1.CheckRateLimitResponse
	err     error
	lastReq *ratecapv1.CheckRateLimitRequest
	lastCtx context.Context
}

func (f *fakeRatecapClient) CheckRateLimit(ctx context.Context, in *ratecapv1.CheckRateLimitRequest, _ ...grpc.CallOption) (*ratecapv1.CheckRateLimitResponse, error) {
	f.lastReq = in
	f.lastCtx = ctx
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

func TestServeHTTP_LogsRealErrorWhenUpstreamCheckFails(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	client := &fakeRatecapClient{err: errors.New("core unavailable")}
	h := proxy.NewHandler(client, proxy.Sheddable, worker.NewShedder(1000))

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rec.Code)
	}
	if !strings.Contains(buf.String(), "core unavailable") {
		t.Errorf("expected the real upstream error to be logged, got:\n%s", buf.String())
	}
}

func TestServeHTTP_RecordsDecisionMetricWithTierFromResponse(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_REJECT_429, Tier: "concurrency_limiter"}}
	h := proxy.NewHandler(client, proxy.Sheddable, worker.NewShedder(1000))

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	got := testutil.ToFloat64(metrics.DecisionsTotal.WithLabelValues("concurrency_limiter", "reject_429"))
	if got < 1 {
		t.Errorf("expected ratecap_decisions_total{tier=\"concurrency_limiter\",action=\"reject_429\"} >= 1, got %v", got)
	}
}

func TestServeHTTP_RecordsPreCoercionDecisionUnderShadowMode(t *testing.T) {
	t.Setenv("RATECAP_SHADOW_MODE", "true")

	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_REJECT_503, Tier: "fleet_shedder"}}
	h := proxy.NewHandler(client, proxy.Sheddable, worker.NewShedder(1000))

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (shadow-coerced), got %d", rec.Code)
	}

	got := testutil.ToFloat64(metrics.DecisionsTotal.WithLabelValues("fleet_shedder", "reject_503"))
	if got < 1 {
		t.Errorf("expected the PRE-coercion action (reject_503) to be recorded despite the 200 response, got %v", got)
	}

	shadowGot := testutil.ToFloat64(metrics.ShadowWouldRejectTotal.WithLabelValues("fleet_shedder"))
	if shadowGot < 1 {
		t.Errorf("expected ratecap_shadow_would_reject_total{tier=\"fleet_shedder\"} >= 1, got %v", shadowGot)
	}
}

func TestServeHTTP_RecordsWorkerShedderMetricOnRealShed(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_ALLOW}}
	shedder := worker.NewShedder(0)
	h := proxy.NewHandler(client, proxy.Sheddable, shedder)

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}

	got := testutil.ToFloat64(metrics.DecisionsTotal.WithLabelValues("worker_shedder", "reject_503"))
	if got < 1 {
		t.Errorf("expected ratecap_decisions_total{tier=\"worker_shedder\",action=\"reject_503\"} >= 1, got %v", got)
	}
}

func TestServeHTTP_LogsRealPathWorkerShedderDecision(t *testing.T) {
	var buf bytes.Buffer
	decisionlog.SetOutput(&buf)
	defer decisionlog.SetOutput(nil)

	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_ALLOW}}
	h := proxy.NewHandler(client, proxy.Sheddable, worker.NewShedder(0))

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-42", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !strings.Contains(buf.String(), `"tier":"worker_shedder"`) {
		t.Errorf("expected a worker_shedder log entry, got:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), `"key":"user-42"`) {
		t.Errorf("expected key=user-42 in the log entry, got:\n%s", buf.String())
	}
}

func TestServeHTTP_LogsRealPathTierDecisionFromResponse(t *testing.T) {
	var buf bytes.Buffer
	decisionlog.SetOutput(&buf)
	defer decisionlog.SetOutput(nil)

	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_REJECT_429, Tier: "concurrency_limiter"}}
	h := proxy.NewHandler(client, proxy.Sheddable, worker.NewShedder(1000))

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-7", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !strings.Contains(buf.String(), `"tier":"concurrency_limiter"`) {
		t.Errorf("expected a concurrency_limiter log entry, got:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), `"action":"reject_429"`) {
		t.Errorf("expected action=reject_429 in the log entry, got:\n%s", buf.String())
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

func TestServeHTTP_QueueActionReturns200(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_QUEUE, Tier: "concurrency_limiter"}}
	h := proxy.NewHandler(client, proxy.Sheddable, worker.NewShedder(1000))

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 for a queued-then-served request (transparent to the client), got %d", rec.Code)
	}
}

func TestServeHTTP_RecordsQueueActionMetricLabel(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_QUEUE, Tier: "concurrency_limiter"}}
	h := proxy.NewHandler(client, proxy.Sheddable, worker.NewShedder(1000))

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	got := testutil.ToFloat64(metrics.DecisionsTotal.WithLabelValues("concurrency_limiter", "queue"))
	if got < 1 {
		t.Errorf(`expected ratecap_decisions_total{tier="concurrency_limiter",action="queue"} >= 1, got %v`, got)
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

func newCriticalRoutesSet(t *testing.T, routes ...string) *criticalroutes.Set {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "critical-routes.yaml")

	var sb strings.Builder
	sb.WriteString("critical_routes:\n")
	for _, route := range routes {
		sb.WriteString("  - \"" + route + "\"\n")
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0600); err != nil {
		t.Fatalf("failed to write critical routes fixture: %v", err)
	}

	set, stop, err := criticalroutes.Watch(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	t.Cleanup(stop)
	return set
}

func TestServeHTTP_ThreadsRouteMatchIntoRequestWhenNoPriorityHeader(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_ALLOW}}
	critical := newCriticalRoutesSet(t, "POST /v1/charges")
	h := proxy.NewHandlerWithCriticalRoutes(client, proxy.Sheddable, worker.NewShedder(1000), negativecache.New(), critical)

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	req.Header.Set("x-ratecap-route", "POST /v1/charges")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if client.lastReq == nil {
		t.Fatal("expected CheckRateLimit to be called")
	}
	if client.lastReq.Priority != ratecapv1.Priority_CRITICAL {
		t.Errorf("expected Priority_CRITICAL on the outgoing request from a route match with no priority header, got %v", client.lastReq.Priority)
	}
}

func TestServeHTTP_HeaderPriorityOutranksRouteMatch(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_ALLOW}}
	critical := newCriticalRoutesSet(t, "POST /v1/charges")
	h := proxy.NewHandlerWithCriticalRoutes(client, proxy.Sheddable, worker.NewShedder(1000), negativecache.New(), critical)

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	req.Header.Set("x-ratecap-route", "POST /v1/charges")
	req.Header.Set("x-ratecap-priority", "sheddable")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if client.lastReq == nil {
		t.Fatal("expected CheckRateLimit to be called")
	}
	if client.lastReq.Priority != ratecapv1.Priority_SHEDDABLE {
		t.Errorf("expected an explicit sheddable priority header to outrank a matching critical route, got %v", client.lastReq.Priority)
	}
}

func TestServeHTTP_NoRouteHeaderWithCriticalRoutesConfiguredFallsBackToDefault(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_ALLOW}}
	critical := newCriticalRoutesSet(t, "POST /v1/charges")
	h := proxy.NewHandlerWithCriticalRoutes(client, proxy.Sheddable, worker.NewShedder(1000), negativecache.New(), critical)

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if client.lastReq == nil {
		t.Fatal("expected CheckRateLimit to be called")
	}
	if client.lastReq.Priority != ratecapv1.Priority_SHEDDABLE {
		t.Errorf("expected default Sheddable priority when neither header nor route matches, got %v", client.lastReq.Priority)
	}
}

func TestServeHTTP_NilCriticalRoutesIsANoOp(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_ALLOW}}
	h := proxy.NewHandlerWithCriticalRoutes(client, proxy.Sheddable, worker.NewShedder(1000), negativecache.New(), nil)

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	req.Header.Set("x-ratecap-route", "POST /v1/charges")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if client.lastReq == nil {
		t.Fatal("expected CheckRateLimit to be called")
	}
	if client.lastReq.Priority != ratecapv1.Priority_SHEDDABLE {
		t.Errorf("expected a nil *criticalroutes.Set to never match, falling back to default Sheddable, got %v", client.lastReq.Priority)
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

func TestServeHTTP_RealWorkerShedSetsShedTierHeaderTo4(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_ALLOW}}
	shedder := worker.NewShedder(0)
	h := proxy.NewHandler(client, proxy.Sheddable, shedder)

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
	if got := rec.Header().Get("X-RateCap-Shed-Tier"); got != "4" {
		t.Errorf("expected X-RateCap-Shed-Tier=4, got %q", got)
	}
}

func TestServeHTTP_Reject503FromCoreSetsShedTierHeaderTo3(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_REJECT_503, Tier: "fleet_shedder"}}
	h := proxy.NewHandler(client, proxy.Sheddable, worker.NewShedder(1000))

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
	if got := rec.Header().Get("X-RateCap-Shed-Tier"); got != "3" {
		t.Errorf("expected X-RateCap-Shed-Tier=3, got %q", got)
	}
}

func TestServeHTTP_AllowedRequestDoesNotSetShedTierHeader(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_ALLOW}}
	h := proxy.NewHandler(client, proxy.Sheddable, worker.NewShedder(1000))

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("X-RateCap-Shed-Tier"); got != "" {
		t.Errorf("expected no X-RateCap-Shed-Tier header on an allowed request, got %q", got)
	}
}

type gaugeSnapshotClient struct {
	fakeRatecapClient
	gaugeDuringCall float64
}

func (f *gaugeSnapshotClient) CheckRateLimit(ctx context.Context, in *ratecapv1.CheckRateLimitRequest, opts ...grpc.CallOption) (*ratecapv1.CheckRateLimitResponse, error) {
	f.gaugeDuringCall = testutil.ToFloat64(metrics.WorkerInFlightRequests)
	return f.fakeRatecapClient.CheckRateLimit(ctx, in, opts...)
}

func TestServeHTTP_UpdatesWorkerInFlightGaugeOnAllowAndRelease(t *testing.T) {
	client := &gaugeSnapshotClient{fakeRatecapClient: fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_ALLOW}}}
	shedder := worker.NewShedder(1000)
	h := proxy.NewHandler(client, proxy.Sheddable, shedder)

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if client.gaugeDuringCall != 1 {
		t.Errorf("expected ratecap_worker_inflight_requests == 1 while the request was held by the shedder, got %v", client.gaugeDuringCall)
	}

	got := testutil.ToFloat64(metrics.WorkerInFlightRequests)
	if got != float64(shedder.InFlight()) {
		t.Errorf("expected ratecap_worker_inflight_requests to match shedder.InFlight() (%d) after ServeHTTP returns and releases its slot, got %v", shedder.InFlight(), got)
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

func TestServeHTTP_CriticalPriorityBypassesShedderWhenOverLimit(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_ALLOW}}
	shedder := worker.NewShedder(0)
	h := proxy.NewHandler(client, proxy.Sheddable, shedder)

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	req.Header.Set("x-ratecap-priority", "critical")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 (critical priority bypasses the shedder even at max=0), got %d", rec.Code)
	}
	if client.lastReq == nil {
		t.Fatal("expected CheckRateLimit to be called for a critical-priority request, even though the in-flight limit was exceeded")
	}
	if client.lastReq.Priority != ratecapv1.Priority_CRITICAL {
		t.Errorf("expected Priority_CRITICAL on the outgoing request, got %v", client.lastReq.Priority)
	}
}

func TestServeHTTP_SheddablePriorityStillShedsWhenOverLimit(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_ALLOW}}
	shedder := worker.NewShedder(0)
	h := proxy.NewHandler(client, proxy.Sheddable, shedder)

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	req.Header.Set("x-ratecap-priority", "sheddable")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 (sheddable priority still sheds at max=0, unchanged behavior), got %d", rec.Code)
	}
	if client.lastReq != nil {
		t.Error("expected CheckRateLimit to never be called for a sheddable-priority request over the in-flight limit")
	}
}

func TestServeHTTP_CriticalPriorityDoesNotConsumeOrReleaseAShedderSlot(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_ALLOW}}
	shedder := worker.NewShedder(1)
	h := proxy.NewHandler(client, proxy.Sheddable, shedder)

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	req.Header.Set("x-ratecap-priority", "critical")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	if !shedder.Allow() {
		t.Fatal("expected the shedder's single slot to still be free after a critical-priority request, since critical never calls Allow()/Release()")
	}
	shedder.Release()
}

type fakeReleaseClient struct {
	lastKey          string
	lastToken        string
	lastRefundKey    string
	lastRefundAmount int32
	err              error
	refundErr        error
}

func (f *fakeReleaseClient) ReleaseConcurrency(_ context.Context, in *ratecapv1.ReleaseConcurrencyRequest, _ ...grpc.CallOption) (*ratecapv1.ReleaseConcurrencyResponse, error) {
	f.lastKey = in.Key
	f.lastToken = in.ConcurrencyToken
	return &ratecapv1.ReleaseConcurrencyResponse{}, f.err
}

func (f *fakeReleaseClient) RefundCost(_ context.Context, in *ratecapv1.RefundCostRequest, _ ...grpc.CallOption) (*ratecapv1.RefundCostResponse, error) {
	f.lastRefundKey = in.Key
	f.lastRefundAmount = in.RefundAmount
	return &ratecapv1.RefundCostResponse{}, f.refundErr
}

func TestReleaseHandler_ServeHTTP_CallsReleaseConcurrencyWithKeyAndToken(t *testing.T) {
	client := &fakeReleaseClient{}
	h := proxy.NewReleaseHandler(client)

	req := httptest.NewRequest(http.MethodPost, "/release", nil)
	req.Header.Set("X-RateCap-Concurrency-Key", "user-1")
	req.Header.Set("X-RateCap-Concurrency-Token", "tok-abc")
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

func TestReleaseHandler_ServeHTTP_ReadsFromHeaderNotQuery(t *testing.T) {
	client := &fakeReleaseClient{}
	h := proxy.NewReleaseHandler(client)

	req := httptest.NewRequest(http.MethodPost, "/release?key=query-key&token=query-token", nil)
	req.Header.Set("X-RateCap-Concurrency-Key", "header-key")
	req.Header.Set("X-RateCap-Concurrency-Token", "header-token")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if client.lastKey != "header-key" {
		t.Errorf("expected the header value to win, got key=%q — the query-string path must be dead, not just unused", client.lastKey)
	}
	if client.lastToken != "header-token" {
		t.Errorf("expected the header value to win, got token=%q — the query-string path must be dead, not just unused", client.lastToken)
	}
}

func TestReleaseHandler_ServeHTTP_MissingKeyReturns400(t *testing.T) {
	client := &fakeReleaseClient{}
	h := proxy.NewReleaseHandler(client)

	req := httptest.NewRequest(http.MethodPost, "/release", nil)
	req.Header.Set("X-RateCap-Concurrency-Token", "tok-abc")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}
}

func TestReleaseHandler_ServeHTTP_UpstreamErrorReturns500(t *testing.T) {
	client := &fakeReleaseClient{err: errors.New("core unavailable")}
	h := proxy.NewReleaseHandler(client)

	req := httptest.NewRequest(http.MethodPost, "/release", nil)
	req.Header.Set("X-RateCap-Concurrency-Key", "user-1")
	req.Header.Set("X-RateCap-Concurrency-Token", "tok-abc")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", rec.Code)
	}
}

func TestReleaseHandler_ServeHTTP_LogsRealErrorWhenUpstreamReleaseFails(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	client := &fakeReleaseClient{err: errors.New("core unavailable")}
	h := proxy.NewReleaseHandler(client)

	req := httptest.NewRequest(http.MethodPost, "/release", nil)
	req.Header.Set("X-RateCap-Concurrency-Key", "user-1")
	req.Header.Set("X-RateCap-Concurrency-Token", "tok-abc")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rec.Code)
	}
	if !strings.Contains(buf.String(), "core unavailable") {
		t.Errorf("expected the real upstream error to be logged, got:\n%s", buf.String())
	}
}

func TestReleaseHandler_ServeHTTP_RejectsNonPOSTMethod(t *testing.T) {
	client := &fakeReleaseClient{}
	h := proxy.NewReleaseHandler(client)

	req := httptest.NewRequest(http.MethodGet, "/release", nil)
	req.Header.Set("X-RateCap-Concurrency-Key", "user-1")
	req.Header.Set("X-RateCap-Concurrency-Token", "tok-abc")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", rec.Code)
	}
}

func TestReleaseHandler_ServeHTTP_RefundKeyAloneIsSufficient(t *testing.T) {
	client := &fakeReleaseClient{}
	h := proxy.NewReleaseHandler(client)

	req := httptest.NewRequest(http.MethodPost, "/release", nil)
	req.Header.Set("X-RateCap-Refund-Key", "user-1")
	req.Header.Set("X-RateCap-Refund-Amount", "7")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body=%s", rec.Code, rec.Body.String())
	}
	if client.lastRefundKey != "user-1" || client.lastRefundAmount != 7 {
		t.Errorf("expected RefundCost called with key=user-1 amount=7, got key=%q amount=%d", client.lastRefundKey, client.lastRefundAmount)
	}
}

func TestReleaseHandler_ServeHTTP_RefundAndConcurrencyReleaseBothHappenInOneCall(t *testing.T) {
	client := &fakeReleaseClient{}
	h := proxy.NewReleaseHandler(client)

	req := httptest.NewRequest(http.MethodPost, "/release", nil)
	req.Header.Set("X-RateCap-Concurrency-Key", "user-1")
	req.Header.Set("X-RateCap-Concurrency-Token", "tok-abc")
	req.Header.Set("X-RateCap-Refund-Key", "user-1")
	req.Header.Set("X-RateCap-Refund-Amount", "7")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body=%s", rec.Code, rec.Body.String())
	}
	if client.lastKey != "user-1" || client.lastToken != "tok-abc" {
		t.Errorf("expected ReleaseConcurrency called, got key=%q token=%q", client.lastKey, client.lastToken)
	}
	if client.lastRefundKey != "user-1" || client.lastRefundAmount != 7 {
		t.Errorf("expected RefundCost also called, got key=%q amount=%d", client.lastRefundKey, client.lastRefundAmount)
	}
}

func TestReleaseHandler_ServeHTTP_InvalidRefundAmountReturns400(t *testing.T) {
	client := &fakeReleaseClient{}
	h := proxy.NewReleaseHandler(client)

	req := httptest.NewRequest(http.MethodPost, "/release", nil)
	req.Header.Set("X-RateCap-Refund-Key", "user-1")
	req.Header.Set("X-RateCap-Refund-Amount", "not-a-number")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for an unparseable refund amount, got %d", rec.Code)
	}
}

func TestReleaseHandler_ServeHTTP_RefundAmountOverflowingInt32Returns400(t *testing.T) {
	client := &fakeReleaseClient{}
	h := proxy.NewReleaseHandler(client)

	req := httptest.NewRequest(http.MethodPost, "/release", nil)
	req.Header.Set("X-RateCap-Refund-Key", "user-1")
	req.Header.Set("X-RateCap-Refund-Amount", "2147483648")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for a refund amount overflowing int32 (would otherwise wrap negative), got %d", rec.Code)
	}
	if client.lastRefundKey != "" {
		t.Errorf("expected RefundCost to never be called with a corrupted amount, got key=%q amount=%d", client.lastRefundKey, client.lastRefundAmount)
	}
}

func TestReleaseHandler_ServeHTTP_RefundUpstreamErrorReturns500(t *testing.T) {
	client := &fakeReleaseClient{refundErr: errors.New("core unavailable")}
	h := proxy.NewReleaseHandler(client)

	req := httptest.NewRequest(http.MethodPost, "/release", nil)
	req.Header.Set("X-RateCap-Refund-Key", "user-1")
	req.Header.Set("X-RateCap-Refund-Amount", "7")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", rec.Code)
	}
}

func TestServeHTTP_UsesCostQueryParamWhenPresent(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_ALLOW}}
	h := proxy.NewHandler(client, proxy.Sheddable, worker.NewShedder(100))

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1&cost=5", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if client.lastReq.Cost != 5 {
		t.Errorf("expected Cost=5 forwarded to core, got %d", client.lastReq.Cost)
	}
}

func TestServeHTTP_DefaultsCostToOneWhenAbsent(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_ALLOW}}
	h := proxy.NewHandler(client, proxy.Sheddable, worker.NewShedder(100))

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if client.lastReq.Cost != 1 {
		t.Errorf("expected default Cost=1, got %d", client.lastReq.Cost)
	}
}

func TestServeHTTP_InvalidCostFallsBackToOne(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_ALLOW}}
	h := proxy.NewHandler(client, proxy.Sheddable, worker.NewShedder(100))

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1&cost=not-a-number", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if client.lastReq.Cost != 1 {
		t.Errorf("expected fallback Cost=1 for an unparseable value, got %d", client.lastReq.Cost)
	}
}

func TestServeHTTP_NonPositiveCostFallsBackToOne(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_ALLOW}}
	h := proxy.NewHandler(client, proxy.Sheddable, worker.NewShedder(100))

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1&cost=0", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if client.lastReq.Cost != 1 {
		t.Errorf("expected fallback Cost=1 for a non-positive value, got %d", client.lastReq.Cost)
	}
}

func TestServeHTTP_CostOverflowingInt32FallsBackToOne(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_ALLOW}}
	h := proxy.NewHandler(client, proxy.Sheddable, worker.NewShedder(100))

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1&cost=2147483648", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if client.lastReq.Cost != 1 {
		t.Errorf("expected fallback Cost=1 for a value overflowing int32 (would otherwise wrap negative), got %d", client.lastReq.Cost)
	}
}

func TestServeHTTP_Reject429SetsIETFRateLimitResetHeader(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_REJECT_429, RetryAfterMs: 2500}}
	h := proxy.NewHandler(client, proxy.Sheddable, worker.NewShedder(100))

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("RateLimit-Reset"); got != "3" {
		t.Errorf(`expected RateLimit-Reset="3" (2500ms rounded up to 3s), got %q`, got)
	}
}

func TestServeHTTP_Reject429FromRateLimiterSetsLimitAndRemainingHeaders(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_REJECT_429, Tier: "rate_limiter", Limit: 500, Remaining: 0}}
	h := proxy.NewHandler(client, proxy.Sheddable, worker.NewShedder(100))

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("RateLimit-Limit"); got != "500" {
		t.Errorf(`expected RateLimit-Limit="500", got %q`, got)
	}
	if got := rec.Header().Get("RateLimit-Remaining"); got != "0" {
		t.Errorf(`expected RateLimit-Remaining="0", got %q`, got)
	}
}

func TestServeHTTP_Reject429FromConcurrencyLimiterOmitsLimitAndRemainingHeaders(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_REJECT_429, Tier: "concurrency_limiter", RetryAfterMs: 500}}
	h := proxy.NewHandler(client, proxy.Sheddable, worker.NewShedder(100))

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("RateLimit-Limit"); got != "" {
		t.Errorf("expected no RateLimit-Limit header on a concurrency_limiter rejection, got %q", got)
	}
	if got := rec.Header().Get("RateLimit-Remaining"); got != "" {
		t.Errorf("expected no RateLimit-Remaining header on a concurrency_limiter rejection, got %q", got)
	}
	if got := rec.Header().Get("Retry-After-Ms"); got != "500" {
		t.Errorf("expected Retry-After-Ms to still be set on a concurrency_limiter rejection, got %q", got)
	}
	if got := rec.Header().Get("RateLimit-Reset"); got != "1" {
		t.Errorf("expected RateLimit-Reset to still be set on a concurrency_limiter rejection, got %q", got)
	}
}

func TestServeHTTP_AllowDoesNotSetRateLimitResetHeader(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_ALLOW}}
	h := proxy.NewHandler(client, proxy.Sheddable, worker.NewShedder(100))

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("RateLimit-Reset"); got != "" {
		t.Errorf("expected no RateLimit-Reset header on an allowed request, got %q", got)
	}
}

func TestServeHTTP_ShortCircuitsOnCachedDenial(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_ALLOW}}
	cache := negativecache.New()
	cache.MarkDenied("user-1", 5*time.Second)
	h := proxy.NewHandlerWithCache(client, proxy.Sheddable, worker.NewShedder(100), cache)

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429 from the cached denial, got %d", rec.Code)
	}
	if client.lastReq != nil {
		t.Error("expected core to never be called for a cache-short-circuited request")
	}
}

func TestServeHTTP_CachedDenialSetsIETFRateLimitResetHeader(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_ALLOW}}
	cache := negativecache.New()
	cache.MarkDenied("user-1", 2500*time.Millisecond)
	h := proxy.NewHandlerWithCache(client, proxy.Sheddable, worker.NewShedder(100), cache)

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("RateLimit-Reset"); got != "3" {
		t.Errorf(`expected RateLimit-Reset="3" (~2.5s rounded up) on a cache-short-circuited 429, same as a fresh REJECT_429 from core, got %q`, got)
	}
}

func TestServeHTTP_CachedDenialRecordsMetricsAndDecisionLog(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_ALLOW}}
	cache := negativecache.New()
	cache.MarkDenied("user-1", 5*time.Second)
	h := proxy.NewHandlerWithCache(client, proxy.Sheddable, worker.NewShedder(100), cache)

	before := testutil.ToFloat64(metrics.DecisionsTotal.WithLabelValues("negative_cache", "reject_429"))

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	after := testutil.ToFloat64(metrics.DecisionsTotal.WithLabelValues("negative_cache", "reject_429"))
	if after != before+1 {
		t.Errorf("expected ratecap_decisions_total{tier=negative_cache,action=reject_429} to increment on a cache-short-circuited denial (so dashboards/alerts don't under-report rejection volume for repeat offenders), before=%v after=%v", before, after)
	}
}

func TestServeHTTP_NegativeCacheShortCircuitOmitsLimitAndRemainingHeaders(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_ALLOW}}
	cache := negativecache.New()
	cache.MarkDenied("user-1", 2500*time.Millisecond)
	h := proxy.NewHandlerWithCache(client, proxy.Sheddable, worker.NewShedder(100), cache)

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("RateLimit-Limit"); got != "" {
		t.Errorf("expected no RateLimit-Limit header on a cache-short-circuited 429 (negativecache.Cache has no tier/limit data), got %q", got)
	}
	if got := rec.Header().Get("RateLimit-Remaining"); got != "" {
		t.Errorf("expected no RateLimit-Remaining header on a cache-short-circuited 429, got %q", got)
	}
	if got := rec.Header().Get("Retry-After-Ms"); got == "" {
		t.Error("expected Retry-After-Ms to still be set on a cache-short-circuited 429")
	}
	if got := rec.Header().Get("RateLimit-Reset"); got == "" {
		t.Error("expected RateLimit-Reset to still be set on a cache-short-circuited 429")
	}
}

func TestServeHTTP_MarksDeniedOnRealReject429(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_REJECT_429, RetryAfterMs: 5000}}
	cache := negativecache.New()
	h := proxy.NewHandlerWithCache(client, proxy.Sheddable, worker.NewShedder(100), cache)

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-2", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	denied, _ := cache.IsDenied("user-2")
	if !denied {
		t.Error("expected a real REJECT_429 to mark the key denied in the cache")
	}
}

func TestServeHTTP_DoesNotMarkDeniedOnAllow(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_ALLOW}}
	cache := negativecache.New()
	h := proxy.NewHandlerWithCache(client, proxy.Sheddable, worker.NewShedder(100), cache)

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-3", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	denied, _ := cache.IsDenied("user-3")
	if denied {
		t.Error("expected ALLOW to never mark a key denied")
	}
}

func TestNewHandler_HasNoNegativeCacheByDefault(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_ALLOW}}
	h := proxy.NewHandler(client, proxy.Sheddable, worker.NewShedder(100))

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected NewHandler (no cache) to work exactly as before, got %d", rec.Code)
	}
}

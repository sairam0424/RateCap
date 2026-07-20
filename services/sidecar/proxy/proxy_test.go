package proxy_test

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"google.golang.org/grpc"

	"github.com/prometheus/client_golang/prometheus/testutil"

	ratecapv1 "github.com/ratecap/proto/ratecap/v1"

	"github.com/ratecap/sidecar/decisionlog"
	"github.com/ratecap/sidecar/metrics"
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

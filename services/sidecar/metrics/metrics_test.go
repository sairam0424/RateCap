package metrics_test

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/ratecap/sidecar/metrics"
)

func TestRecordDecision_IncrementsCounterForTierAndAction(t *testing.T) {
	metrics.RecordDecision("rate_limiter", "reject_429")

	got := testutil.ToFloat64(metrics.DecisionsTotal.WithLabelValues("rate_limiter", "reject_429"))
	if got < 1 {
		t.Errorf("expected ratecap_decisions_total{tier=\"rate_limiter\",action=\"reject_429\"} >= 1, got %v", got)
	}
}

func TestRecordShadowWouldReject_IncrementsCounterForTier(t *testing.T) {
	metrics.RecordShadowWouldReject("fleet_shedder")

	got := testutil.ToFloat64(metrics.ShadowWouldRejectTotal.WithLabelValues("fleet_shedder"))
	if got < 1 {
		t.Errorf("expected ratecap_shadow_would_reject_total{tier=\"fleet_shedder\"} >= 1, got %v", got)
	}
}

func TestSetWorkerInFlight_SetsGaugeToGivenValue(t *testing.T) {
	metrics.SetWorkerInFlight(7)

	got := testutil.ToFloat64(metrics.WorkerInFlightRequests)
	if got != 7 {
		t.Errorf("expected ratecap_worker_inflight_requests == 7, got %v", got)
	}

	metrics.SetWorkerInFlight(2)

	got = testutil.ToFloat64(metrics.WorkerInFlightRequests)
	if got != 2 {
		t.Errorf("expected ratecap_worker_inflight_requests == 2 after a second set, got %v", got)
	}
}

func TestHandler_ServesPrometheusExpositionFormat(t *testing.T) {
	metrics.RecordDecision("worker_shedder", "reject_503")

	req := newRequest(t)
	rec := newRecorder()
	metrics.Handler().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "ratecap_decisions_total") {
		t.Errorf("expected response body to contain ratecap_decisions_total, got:\n%s", rec.Body.String())
	}
}

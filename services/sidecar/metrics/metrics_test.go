package metrics_test

import (
	"strings"
	"testing"
	"time"

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

func TestRecordDecisionLatency_ObservesByTier(t *testing.T) {
	before := testutil.CollectAndCount(metrics.DecisionLatency)
	metrics.RecordDecisionLatency("rate_limiter", 5*time.Millisecond)
	after := testutil.CollectAndCount(metrics.DecisionLatency)

	if after <= before {
		t.Errorf("expected DecisionLatency observation count to increase, before=%d after=%d", before, after)
	}
}

func TestRecordReleaseResult_IncrementsByResult(t *testing.T) {
	before := testutil.ToFloat64(metrics.ReleaseTotal.WithLabelValues("success"))
	metrics.RecordReleaseResult("success")
	after := testutil.ToFloat64(metrics.ReleaseTotal.WithLabelValues("success"))

	if after != before+1 {
		t.Errorf("expected ReleaseTotal{result=success} to increment by 1, before=%v after=%v", before, after)
	}
}

func TestRecordUpstreamError_IncrementsByEndpoint(t *testing.T) {
	before := testutil.ToFloat64(metrics.UpstreamErrorsTotal.WithLabelValues("check_rate_limit"))
	metrics.RecordUpstreamError("check_rate_limit")
	after := testutil.ToFloat64(metrics.UpstreamErrorsTotal.WithLabelValues("check_rate_limit"))

	if after != before+1 {
		t.Errorf("expected UpstreamErrorsTotal{endpoint=check_rate_limit} to increment by 1, before=%v after=%v", before, after)
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

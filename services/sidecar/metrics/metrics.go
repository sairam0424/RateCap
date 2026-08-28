package metrics

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var DecisionsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "ratecap_decisions_total",
	Help: "Total number of rate-limit decisions, labeled by the tier that produced them and the resulting action.",
}, []string{"tier", "action"})

var ShadowWouldRejectTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "ratecap_shadow_would_reject_total",
	Help: "Total number of decisions that would have rejected/shed the request but were coerced to allow by shadow mode.",
}, []string{"tier"})

var WorkerInFlightRequests = promauto.NewGauge(prometheus.GaugeOpts{
	Name: "ratecap_worker_inflight_requests",
	Help: "Current number of in-flight requests held by the Tier 4 worker shedder on this sidecar instance.",
})

var DecisionLatency = promauto.NewHistogramVec(prometheus.HistogramOpts{
	Name: "ratecap_decision_latency_seconds",
	Help: "End-to-end latency of a /check decision as observed by the sidecar, labeled by the tier that produced the final action.",
}, []string{"tier"})

var ReleaseTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "ratecap_release_total",
	Help: "Total number of /release calls handled by the sidecar, labeled by result (success or failure).",
}, []string{"result"})

var UpstreamErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "ratecap_upstream_errors_total",
	Help: "Total number of failed gRPC calls from the sidecar to ratecap-core, labeled by the endpoint that made the call.",
}, []string{"endpoint"})

func RecordDecision(tier, action string) {
	DecisionsTotal.WithLabelValues(tier, action).Inc()
}

func RecordShadowWouldReject(tier string) {
	ShadowWouldRejectTotal.WithLabelValues(tier).Inc()
}

func SetWorkerInFlight(v int64) {
	WorkerInFlightRequests.Set(float64(v))
}

func RecordDecisionLatency(tier string, latency time.Duration) {
	DecisionLatency.WithLabelValues(tier).Observe(latency.Seconds())
}

func RecordReleaseResult(result string) {
	ReleaseTotal.WithLabelValues(result).Inc()
}

func RecordUpstreamError(endpoint string) {
	UpstreamErrorsTotal.WithLabelValues(endpoint).Inc()
}

func Handler() http.Handler {
	return promhttp.Handler()
}

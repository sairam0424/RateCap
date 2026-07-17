package metrics

import (
	"net/http"

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

func RecordDecision(tier, action string) {
	DecisionsTotal.WithLabelValues(tier, action).Inc()
}

func RecordShadowWouldReject(tier string) {
	ShadowWouldRejectTotal.WithLabelValues(tier).Inc()
}

func Handler() http.Handler {
	return promhttp.Handler()
}

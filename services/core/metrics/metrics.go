package metrics

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var GRPCRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "ratecap_core_grpc_requests_total",
	Help: "Total number of gRPC requests handled by ratecap-core, labeled by method and resulting status code.",
}, []string{"method", "code"})

var GRPCRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
	Name: "ratecap_core_grpc_request_duration_seconds",
	Help: "Latency of gRPC requests handled by ratecap-core, labeled by method.",
}, []string{"method"})

var RedisCallDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
	Name: "ratecap_core_redis_call_duration_seconds",
	Help: "Latency of Redis calls made by ratecap-core, labeled by operation.",
}, []string{"operation"})

var RedisErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "ratecap_core_redis_errors_total",
	Help: "Total number of failed Redis calls made by ratecap-core, labeled by operation.",
}, []string{"operation"})

var ConfigReloadTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "ratecap_core_config_reload_total",
	Help: "Total number of config hot-reload attempts, labeled by result (success or failure).",
}, []string{"result"})

// FailOpenTotal has no ratecap_core_ prefix: fail-open is a fleet-wide safety
// signal meaningful regardless of which binary emits it, and the spec names
// it ratecap_fail_open_total verbatim.
var FailOpenTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "ratecap_fail_open_total",
	Help: "Total number of requests allowed through via fail-open behavior after a tier's backing store call errored.",
}, []string{"tier", "reason"})

var ConfigVersionInfo = promauto.NewGaugeVec(prometheus.GaugeOpts{
	Name: "ratecap_core_config_version_info",
	Help: "Info metric (always 1) whose hash label is the currently-active config's content hash — compare this label across replicas to detect hot-reload divergence.",
}, []string{"hash"})

var ConnectionSecurityTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "ratecap_core_connection_security_total",
	Help: "Total gRPC calls by transport (plaintext or tls) and whether a client certificate was presented (present, absent, or n/a for plaintext) — the 'is anything still on plaintext' signal a default flip needs before it's safe.",
}, []string{"transport", "client_cert"})

var currentConfigHash string

func RecordGRPCRequest(method, code string, duration time.Duration) {
	GRPCRequestsTotal.WithLabelValues(method, code).Inc()
	GRPCRequestDuration.WithLabelValues(method).Observe(duration.Seconds())
}

func RecordRedisCall(operation string, duration time.Duration, err error) {
	RedisCallDuration.WithLabelValues(operation).Observe(duration.Seconds())
	if err != nil {
		RedisErrorsTotal.WithLabelValues(operation).Inc()
	}
}

func RecordConfigReload(result string) {
	ConfigReloadTotal.WithLabelValues(result).Inc()
}

func RecordFailOpen(tier, reason string) {
	FailOpenTotal.WithLabelValues(tier, reason).Inc()
}

// RecordConfigVersion clears the previous hash's series (if any) and sets
// the new one to 1, so ratecap_core_config_version_info always has exactly
// one active series per replica rather than accumulating a stale series per
// historical hash forever.
func RecordConfigVersion(hash string) {
	if currentConfigHash != "" && currentConfigHash != hash {
		ConfigVersionInfo.DeleteLabelValues(currentConfigHash)
	}
	currentConfigHash = hash
	ConfigVersionInfo.WithLabelValues(hash).Set(1)
}

func RecordConnectionSecurity(transport, clientCert string) {
	ConnectionSecurityTotal.WithLabelValues(transport, clientCert).Inc()
}

func Handler() http.Handler {
	return promhttp.Handler()
}

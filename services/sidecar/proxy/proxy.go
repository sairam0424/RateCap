package proxy

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"

	ratecapv1 "github.com/sairam0424/RateCap/proto/ratecap/v1"

	"github.com/sairam0424/RateCap/services/sidecar/decisionlog"
	"github.com/sairam0424/RateCap/services/sidecar/metrics"
	"github.com/sairam0424/RateCap/services/sidecar/negativecache"
	"github.com/sairam0424/RateCap/services/sidecar/shadow"
	"github.com/sairam0424/RateCap/services/sidecar/worker"
)

type ratecapClient interface {
	CheckRateLimit(ctx context.Context, in *ratecapv1.CheckRateLimitRequest, opts ...grpc.CallOption) (*ratecapv1.CheckRateLimitResponse, error)
}

func resolveCost(raw string) int {
	if raw == "" {
		return 1
	}
	// ParseInt with bitSize=32 (not Atoi) so an out-of-int32-range value is
	// rejected here, not silently wrapped to a negative Cost by the int32()
	// cast at the call site.
	parsed, err := strconv.ParseInt(raw, 10, 32)
	if err != nil || parsed <= 0 {
		log.Printf("sidecar: /check: cost=%q is invalid, defaulting to 1", raw)
		return 1
	}
	return int(parsed)
}

type Handler struct {
	client          ratecapClient
	defaultPriority Priority
	shedder         *worker.Shedder
	negativeCache   *negativecache.Cache
}

func NewHandler(client ratecapClient, defaultPriority Priority, shedder *worker.Shedder) *Handler {
	return &Handler{client: client, defaultPriority: defaultPriority, shedder: shedder}
}

// NewHandlerWithCache is NewHandler plus an explicit negative cache — kept
// as a separate constructor (rather than a parameter on NewHandler) so
// every existing call site of NewHandler keeps compiling unchanged.
func NewHandlerWithCache(client ratecapClient, defaultPriority Priority, shedder *worker.Shedder, cache *negativecache.Cache) *Handler {
	return &Handler{client: client, defaultPriority: defaultPriority, shedder: shedder, negativeCache: cache}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	key := r.URL.Query().Get("key")
	if key == "" {
		http.Error(w, "missing key parameter", http.StatusBadRequest)
		return
	}

	priority := ResolvePriority(r.Header.Get("x-ratecap-priority"), h.defaultPriority)
	protoPriority := ratecapv1.Priority_SHEDDABLE
	if priority == Critical {
		protoPriority = ratecapv1.Priority_CRITICAL
	}

	// A cache-short-circuited denial must look identical, on the wire and in
	// observability, to the real REJECT_429 it's standing in for — it's the
	// same decision, just served from local memory instead of a fresh core
	// round trip. Priority resolution moved above this block (from its
	// original position further down) so decisionlog has a real label here
	// instead of a duplicate ResolvePriority call.
	if h.negativeCache != nil {
		if denied, remaining := h.negativeCache.IsDenied(key); denied {
			metrics.RecordDecision("negative_cache", "reject_429")
			decisionlog.Log("negative_cache", key, "reject_429", priorityLabel(priority), time.Since(start))
			metrics.RecordDecisionLatency("negative_cache", time.Since(start))
			remainingMs := remaining.Milliseconds()
			w.Header().Set("Retry-After-Ms", strconv.FormatInt(remainingMs, 10))
			w.Header().Set("RateLimit-Reset", strconv.FormatInt((remainingMs+999)/1000, 10))
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
	}

	if priority != Critical {
		if !h.shedder.Allow() {
			if !shadow.GlobalOverrideEnabled() {
				metrics.RecordDecision("worker_shedder", "reject_503")
				metrics.SetWorkerInFlight(h.shedder.InFlight())
				decisionlog.Log("worker_shedder", key, "reject_503", priorityLabel(priority), time.Since(start))
				metrics.RecordDecisionLatency("worker_shedder", time.Since(start))
				w.Header().Set("X-RateCap-Shed-Tier", "4")
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			metrics.RecordDecision("worker_shedder", "reject_503")
			metrics.RecordShadowWouldReject("worker_shedder")
			metrics.SetWorkerInFlight(h.shedder.InFlight())
			decisionlog.Log("worker_shedder", key, "reject_503", priorityLabel(priority), time.Since(start))
			metrics.RecordDecisionLatency("worker_shedder", time.Since(start))
			log.Printf("worker shedder: would have shed request, shadow mode active")
		} else {
			metrics.SetWorkerInFlight(h.shedder.InFlight())
			defer func() {
				h.shedder.Release()
				metrics.SetWorkerInFlight(h.shedder.InFlight())
			}()
		}
	}

	skipReservations := r.URL.Query().Get("skip_reservations") == "true"

	// otelgrpc's client stats handler creates its own per-RPC span inside
	// grpc.ClientConn.Invoke and never hands the derived context back to
	// caller code, so trace.SpanFromContext(r.Context()) before the call
	// cannot reach it — confirmed by reading otelgrpc's clientHandler.TagRPC
	// (it returns the span-bearing context to grpc-go's internal call
	// machinery only). Starting an explicit client-kind span here is the
	// only way to attach a real, per-request attribute to a span that is
	// actually exported: it becomes otelgrpc's span's parent, so the trace ID
	// still links this hop to core's server span end-to-end.
	//
	// Fixed, small-cardinality label only — never the caller-controlled key,
	// a header, or a query param (SECURITY.md's decision-log stance applies
	// equally to span data leaving the process via OTLP export).
	callCtx, span := otel.Tracer("github.com/sairam0424/RateCap/services/sidecar/proxy").Start(
		r.Context(), "ratecap.sidecar.check_rate_limit", trace.WithSpanKind(trace.SpanKindClient),
	)
	span.SetAttributes(attribute.String("ratecap.priority", priorityLabel(priority)))

	resp, err := h.client.CheckRateLimit(callCtx, &ratecapv1.CheckRateLimitRequest{
		Key:              key,
		Cost:             int32(resolveCost(r.URL.Query().Get("cost"))),
		SkipReservations: skipReservations,
		Priority:         protoPriority,
	})
	span.End()
	if err != nil {
		log.Printf("sidecar: /check: upstream call failed: %v", err)
		metrics.RecordUpstreamError("check_rate_limit")
		http.Error(w, "upstream check failed", http.StatusInternalServerError)
		return
	}

	for i, reservation := range resp.Reservations {
		w.Header().Set(fmt.Sprintf("Concurrency-Token-%d", i), reservation.Token)
		w.Header().Set(fmt.Sprintf("Concurrency-Key-%d", i), reservation.Key)
	}

	realAction := resp.Action
	action := realAction
	if shadow.GlobalOverrideEnabled() {
		action = shadow.CoerceIfShadowOverridden(action, true)
	}

	metrics.RecordDecision(resp.Tier, actionLabel(realAction))
	decisionlog.Log(resp.Tier, key, actionLabel(realAction), priorityLabel(priority), time.Since(start))
	metrics.RecordDecisionLatency(resp.Tier, time.Since(start))
	if action != realAction {
		metrics.RecordShadowWouldReject(resp.Tier)
	}

	// Scoped to REJECT_429 specifically, not REJECT_503: a 503 fleet/worker
	// shed is a capacity signal that can flip to ALLOW moments later once
	// load drops, whereas a 429 has a caller-specific RetryAfterMs that's
	// the exact right cache window — caching a 503 risks needlessly
	// extending an outage's blast radius past when capacity actually
	// recovered.
	if h.negativeCache != nil && realAction == ratecapv1.Action_REJECT_429 {
		h.negativeCache.MarkDenied(key, time.Duration(resp.RetryAfterMs)*time.Millisecond)
	}

	switch action {
	case ratecapv1.Action_ALLOW, ratecapv1.Action_SHADOW_LOG, ratecapv1.Action_QUEUE:
		w.WriteHeader(http.StatusOK)
	case ratecapv1.Action_REJECT_429:
		w.Header().Set("Retry-After-Ms", strconv.FormatInt(resp.RetryAfterMs, 10))
		// (ms + 999) / 1000 is integer ceiling division — the IETF draft's
		// reset field is in whole seconds, and rounding down would tell a
		// caller it's safe to retry slightly before it actually is.
		w.Header().Set("RateLimit-Reset", strconv.FormatInt((resp.RetryAfterMs+999)/1000, 10))
		w.WriteHeader(http.StatusTooManyRequests)
	case ratecapv1.Action_REJECT_503:
		w.Header().Set("X-RateCap-Shed-Tier", "3")
		w.WriteHeader(http.StatusServiceUnavailable)
	}
}

func actionLabel(a ratecapv1.Action) string {
	switch a {
	case ratecapv1.Action_ALLOW:
		return "allow"
	case ratecapv1.Action_REJECT_429:
		return "reject_429"
	case ratecapv1.Action_REJECT_503:
		return "reject_503"
	case ratecapv1.Action_SHADOW_LOG:
		return "shadow_log"
	case ratecapv1.Action_QUEUE:
		return "queue"
	default:
		return "unknown"
	}
}

func priorityLabel(p Priority) string {
	if p == Critical {
		return "critical"
	}
	return "sheddable"
}

type releaseClient interface {
	ReleaseConcurrency(ctx context.Context, in *ratecapv1.ReleaseConcurrencyRequest, opts ...grpc.CallOption) (*ratecapv1.ReleaseConcurrencyResponse, error)
	RefundCost(ctx context.Context, in *ratecapv1.RefundCostRequest, opts ...grpc.CallOption) (*ratecapv1.RefundCostResponse, error)
}

type ReleaseHandler struct {
	client releaseClient
}

func NewReleaseHandler(client releaseClient) *ReleaseHandler {
	return &ReleaseHandler{client: client}
}

func (h *ReleaseHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	concurrencyKey := r.Header.Get("X-RateCap-Concurrency-Key")
	refundKey := r.Header.Get("X-RateCap-Refund-Key")
	if concurrencyKey == "" && refundKey == "" {
		http.Error(w, "missing key parameter", http.StatusBadRequest)
		return
	}

	if concurrencyKey != "" {
		token := r.Header.Get("X-RateCap-Concurrency-Token")
		_, err := h.client.ReleaseConcurrency(r.Context(), &ratecapv1.ReleaseConcurrencyRequest{Key: concurrencyKey, ConcurrencyToken: token})
		if err != nil {
			log.Printf("sidecar: /release: upstream release failed: %v", err)
			metrics.RecordUpstreamError("release_concurrency")
			metrics.RecordReleaseResult("failure")
			http.Error(w, "upstream release failed", http.StatusInternalServerError)
			return
		}
		metrics.RecordReleaseResult("success")
	}

	if refundKey != "" {
		// ParseInt with bitSize=32 (not Atoi), matching resolveCost above —
		// rejects an out-of-int32-range value here rather than silently
		// wrapping it to a negative RefundAmount via the int32() cast below.
		refundAmount, err := strconv.ParseInt(r.Header.Get("X-RateCap-Refund-Amount"), 10, 32)
		if err != nil || refundAmount <= 0 {
			http.Error(w, "invalid or missing X-RateCap-Refund-Amount", http.StatusBadRequest)
			return
		}
		_, err = h.client.RefundCost(r.Context(), &ratecapv1.RefundCostRequest{Key: refundKey, RefundAmount: int32(refundAmount)})
		if err != nil {
			log.Printf("sidecar: /release: upstream refund failed: %v", err)
			metrics.RecordUpstreamError("refund_cost")
			http.Error(w, "upstream refund failed", http.StatusInternalServerError)
			return
		}
	}

	w.WriteHeader(http.StatusOK)
}

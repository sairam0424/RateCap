package proxy

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"google.golang.org/grpc"

	ratecapv1 "github.com/ratecap/proto/ratecap/v1"

	"github.com/ratecap/sidecar/decisionlog"
	"github.com/ratecap/sidecar/metrics"
	"github.com/ratecap/sidecar/shadow"
	"github.com/ratecap/sidecar/worker"
)

type ratecapClient interface {
	CheckRateLimit(ctx context.Context, in *ratecapv1.CheckRateLimitRequest, opts ...grpc.CallOption) (*ratecapv1.CheckRateLimitResponse, error)
}

type Handler struct {
	client          ratecapClient
	defaultPriority Priority
	shedder         *worker.Shedder
}

func NewHandler(client ratecapClient, defaultPriority Priority, shedder *worker.Shedder) *Handler {
	return &Handler{client: client, defaultPriority: defaultPriority, shedder: shedder}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	priority := ResolvePriority(r.Header.Get("x-ratecap-priority"), h.defaultPriority)
	protoPriority := ratecapv1.Priority_SHEDDABLE
	if priority == Critical {
		protoPriority = ratecapv1.Priority_CRITICAL
	}

	if priority != Critical {
		if !h.shedder.Allow() {
			shedKey := r.URL.Query().Get("key")
			if !shadow.GlobalOverrideEnabled() {
				metrics.RecordDecision("worker_shedder", "reject_503")
				metrics.SetWorkerInFlight(h.shedder.InFlight())
				decisionlog.Log("worker_shedder", shedKey, "reject_503", priorityLabel(priority), time.Since(start))
				w.Header().Set("X-RateCap-Shed-Tier", "4")
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			metrics.RecordDecision("worker_shedder", "reject_503")
			metrics.RecordShadowWouldReject("worker_shedder")
			metrics.SetWorkerInFlight(h.shedder.InFlight())
			decisionlog.Log("worker_shedder", shedKey, "reject_503", priorityLabel(priority), time.Since(start))
			log.Printf("worker shedder: would have shed request, shadow mode active")
		} else {
			metrics.SetWorkerInFlight(h.shedder.InFlight())
			defer func() {
				h.shedder.Release()
				metrics.SetWorkerInFlight(h.shedder.InFlight())
			}()
		}
	}

	key := r.URL.Query().Get("key")
	if key == "" {
		http.Error(w, "missing key parameter", http.StatusBadRequest)
		return
	}

	skipReservations := r.URL.Query().Get("skip_reservations") == "true"

	resp, err := h.client.CheckRateLimit(r.Context(), &ratecapv1.CheckRateLimitRequest{
		Key:              key,
		Cost:             1,
		SkipReservations: skipReservations,
		Priority:         protoPriority,
	})
	if err != nil {
		log.Printf("sidecar: /check: upstream call failed: %v", err)
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
	if action != realAction {
		metrics.RecordShadowWouldReject(resp.Tier)
	}

	switch action {
	case ratecapv1.Action_ALLOW, ratecapv1.Action_SHADOW_LOG, ratecapv1.Action_QUEUE:
		w.WriteHeader(http.StatusOK)
	case ratecapv1.Action_REJECT_429:
		w.Header().Set("Retry-After-Ms", strconv.FormatInt(resp.RetryAfterMs, 10))
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

	key := r.URL.Query().Get("key")
	if key == "" {
		http.Error(w, "missing key parameter", http.StatusBadRequest)
		return
	}
	token := r.URL.Query().Get("token")

	_, err := h.client.ReleaseConcurrency(r.Context(), &ratecapv1.ReleaseConcurrencyRequest{Key: key, ConcurrencyToken: token})
	if err != nil {
		log.Printf("sidecar: /release: upstream call failed: %v", err)
		http.Error(w, "upstream release failed", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

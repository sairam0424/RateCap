package proxy

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strconv"

	"google.golang.org/grpc"

	ratecapv1 "github.com/ratecap/proto/ratecap/v1"

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
			if !shadow.GlobalOverrideEnabled() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			log.Printf("worker shedder: would have shed request, shadow mode active")
		} else {
			defer h.shedder.Release()
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
		http.Error(w, "upstream check failed", http.StatusInternalServerError)
		return
	}

	for i, reservation := range resp.Reservations {
		w.Header().Set(fmt.Sprintf("Concurrency-Token-%d", i), reservation.Token)
		w.Header().Set(fmt.Sprintf("Concurrency-Key-%d", i), reservation.Key)
	}

	action := resp.Action
	if shadow.GlobalOverrideEnabled() {
		action = shadow.CoerceIfShadowOverridden(action, true)
	}

	switch action {
	case ratecapv1.Action_ALLOW, ratecapv1.Action_SHADOW_LOG:
		w.WriteHeader(http.StatusOK)
	case ratecapv1.Action_REJECT_429:
		w.Header().Set("Retry-After-Ms", strconv.FormatInt(resp.RetryAfterMs, 10))
		w.WriteHeader(http.StatusTooManyRequests)
	case ratecapv1.Action_REJECT_503:
		w.WriteHeader(http.StatusServiceUnavailable)
	}
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
		http.Error(w, "upstream release failed", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

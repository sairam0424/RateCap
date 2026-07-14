package proxy

import (
	"context"
	"net/http"
	"strconv"

	"google.golang.org/grpc"

	ratecapv1 "github.com/ratecap/proto/ratecap/v1"

	"github.com/ratecap/sidecar/shadow"
)

type ratecapClient interface {
	CheckRateLimit(ctx context.Context, in *ratecapv1.CheckRateLimitRequest, opts ...grpc.CallOption) (*ratecapv1.CheckRateLimitResponse, error)
}

type Handler struct {
	client          ratecapClient
	defaultPriority Priority
}

func NewHandler(client ratecapClient, defaultPriority Priority) *Handler {
	return &Handler{client: client, defaultPriority: defaultPriority}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	if key == "" {
		http.Error(w, "missing key parameter", http.StatusBadRequest)
		return
	}

	_ = ResolvePriority(r.Header.Get("x-ratecap-priority"), h.defaultPriority)

	skipConcurrency := r.URL.Query().Get("skip_concurrency") == "true"

	resp, err := h.client.CheckRateLimit(r.Context(), &ratecapv1.CheckRateLimitRequest{
		Key:                  key,
		Cost:                 1,
		SkipConcurrencyLimit: skipConcurrency,
	})
	if err != nil {
		http.Error(w, "upstream check failed", http.StatusInternalServerError)
		return
	}

	if len(resp.Reservations) > 0 {
		w.Header().Set("Concurrency-Token", resp.Reservations[0].Token)
		w.Header().Set("Concurrency-Key", resp.Reservations[0].Key)
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

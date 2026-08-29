package admin

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"log"
	"net/http"

	"google.golang.org/grpc"

	ratecapv1 "github.com/ratecap/proto/ratecap/v1"
)

type adminClient interface {
	SetDynamicLimit(ctx context.Context, in *ratecapv1.SetDynamicLimitRequest, opts ...grpc.CallOption) (*ratecapv1.SetDynamicLimitResponse, error)
}

type Handler struct {
	client adminClient
	secret string
}

func NewHandler(client adminClient, secret string) *Handler {
	return &Handler{client: client, secret: secret}
}

type setLimitBody struct {
	Tier  string `json:"tier"`
	Value int32  `json:"value"`
}

// ServeHTTP checks X-RateCap-Admin-Secret with constant-time comparison
// (matching this repo's existing pattern in core/auth and grpcserver's
// token verification) BEFORE ever forwarding to core — a capability with
// fleet-wide, unbounded blast radius gets its own gate, not a reuse of the
// general sidecar<->core shared secret, per the explicit design decision
// this task implements.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	provided := r.Header.Get("X-RateCap-Admin-Secret")
	if provided == "" || subtle.ConstantTimeCompare([]byte(provided), []byte(h.secret)) != 1 {
		log.Printf("sidecar: /admin/set-limit: rejected request with missing/invalid admin secret")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var body setLimitBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	resp, err := h.client.SetDynamicLimit(r.Context(), &ratecapv1.SetDynamicLimitRequest{Tier: body.Tier, Value: body.Value})
	if err != nil {
		log.Printf("sidecar: /admin/set-limit: upstream call failed: %v", err)
		http.Error(w, "upstream call failed", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

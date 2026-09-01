package admin_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/grpc"

	ratecapv1 "github.com/sairam0424/RateCap/proto/ratecap/v1"

	"github.com/sairam0424/RateCap/services/sidecar/admin"
)

type fakeAdminClient struct {
	lastReq *ratecapv1.SetDynamicLimitRequest
	resp    *ratecapv1.SetDynamicLimitResponse
	err     error
}

func (f *fakeAdminClient) SetDynamicLimit(ctx context.Context, in *ratecapv1.SetDynamicLimitRequest, opts ...grpc.CallOption) (*ratecapv1.SetDynamicLimitResponse, error) {
	f.lastReq = in
	return f.resp, f.err
}

func TestServeHTTP_RejectsMissingAdminSecret(t *testing.T) {
	client := &fakeAdminClient{}
	h := admin.NewHandler(client, "correct-secret")

	req := httptest.NewRequest(http.MethodPost, "/admin/set-limit", strings.NewReader(`{"tier":"rate_limiter","value":500}`))
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for a missing admin secret, got %d", rec.Code)
	}
	if client.lastReq != nil {
		t.Error("expected the request to never reach core when the admin secret is missing")
	}
}

func TestServeHTTP_DisabledWhenHandlerSecretIsEmpty(t *testing.T) {
	// RATECAP_ADMIN_SECRET unset (h.secret == "") must disable the endpoint
	// outright via an explicit guard — not merely happen to reject because
	// ConstantTimeCompare never matches an empty secret. Cover both an
	// attacker sending no header and one sending a real-looking value,
	// since either could slip through if the explicit guard were removed
	// and only the comparison-based rejection remained.
	client := &fakeAdminClient{}
	h := admin.NewHandler(client, "")

	for _, provided := range []string{"", "some-guessed-value"} {
		req := httptest.NewRequest(http.MethodPost, "/admin/set-limit", strings.NewReader(`{"tier":"rate_limiter","value":500}`))
		if provided != "" {
			req.Header.Set("X-RateCap-Admin-Secret", provided)
		}
		rec := httptest.NewRecorder()

		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("provided=%q: expected 503 (endpoint disabled) when handler secret is empty, got %d", provided, rec.Code)
		}
		if client.lastReq != nil {
			t.Errorf("provided=%q: expected the request to never reach core when the admin endpoint is disabled", provided)
		}
	}
}

func TestServeHTTP_RejectsWrongAdminSecret(t *testing.T) {
	client := &fakeAdminClient{}
	h := admin.NewHandler(client, "correct-secret")

	req := httptest.NewRequest(http.MethodPost, "/admin/set-limit", strings.NewReader(`{"tier":"rate_limiter","value":500}`))
	req.Header.Set("X-RateCap-Admin-Secret", "wrong-secret")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for a wrong admin secret, got %d", rec.Code)
	}
	if client.lastReq != nil {
		t.Error("expected the request to never reach core when the admin secret is wrong")
	}
}

func TestServeHTTP_ForwardsValidRequestToCore(t *testing.T) {
	client := &fakeAdminClient{resp: &ratecapv1.SetDynamicLimitResponse{Tier: "rate_limiter", PreviousValue: 100, NewValue: 500}}
	h := admin.NewHandler(client, "correct-secret")

	req := httptest.NewRequest(http.MethodPost, "/admin/set-limit", strings.NewReader(`{"tier":"rate_limiter","value":500}`))
	req.Header.Set("X-RateCap-Admin-Secret", "correct-secret")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body=%s", rec.Code, rec.Body.String())
	}
	if client.lastReq == nil || client.lastReq.Tier != "rate_limiter" || client.lastReq.Value != 500 {
		t.Errorf("expected forwarded request {rate_limiter, 500}, got %+v", client.lastReq)
	}
}

func TestServeHTTP_RejectsNonPostMethod(t *testing.T) {
	client := &fakeAdminClient{}
	h := admin.NewHandler(client, "correct-secret")

	req := httptest.NewRequest(http.MethodGet, "/admin/set-limit", nil)
	req.Header.Set("X-RateCap-Admin-Secret", "correct-secret")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405 for GET, got %d", rec.Code)
	}
}

func TestServeHTTP_PropagatesCoreErrorAsBadRequest(t *testing.T) {
	client := &fakeAdminClient{err: context.DeadlineExceeded}
	h := admin.NewHandler(client, "correct-secret")

	req := httptest.NewRequest(http.MethodPost, "/admin/set-limit", strings.NewReader(`{"tier":"rate_limiter","value":500}`))
	req.Header.Set("X-RateCap-Admin-Secret", "correct-secret")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 when the upstream call fails, got %d", rec.Code)
	}
}

package contracttest_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"testing"

	"google.golang.org/grpc"

	ratecapv1 "github.com/sairam0424/RateCap/proto/ratecap/v1"

	"github.com/sairam0424/RateCap/services/sidecar/proxy"
	"github.com/sairam0424/RateCap/services/sidecar/worker"
)

type fakeCoreClient struct {
	lastCheckReq   *ratecapv1.CheckRateLimitRequest
	lastReleaseReq *ratecapv1.ReleaseConcurrencyRequest
	lastRefundReq  *ratecapv1.RefundCostRequest
}

func (f *fakeCoreClient) CheckRateLimit(_ context.Context, in *ratecapv1.CheckRateLimitRequest, _ ...grpc.CallOption) (*ratecapv1.CheckRateLimitResponse, error) {
	f.lastCheckReq = in
	return &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_ALLOW}, nil
}

func (f *fakeCoreClient) ReleaseConcurrency(_ context.Context, in *ratecapv1.ReleaseConcurrencyRequest, _ ...grpc.CallOption) (*ratecapv1.ReleaseConcurrencyResponse, error) {
	f.lastReleaseReq = in
	return &ratecapv1.ReleaseConcurrencyResponse{}, nil
}

func (f *fakeCoreClient) RefundCost(_ context.Context, in *ratecapv1.RefundCostRequest, _ ...grpc.CallOption) (*ratecapv1.RefundCostResponse, error) {
	f.lastRefundReq = in
	return &ratecapv1.RefundCostResponse{}, nil
}

func newTestSidecar(core *fakeCoreClient) *httptest.Server {
	mux := http.NewServeMux()
	mux.Handle("/check", proxy.NewHandler(core, proxy.Sheddable, worker.NewShedder(1000)))
	mux.Handle("/release", proxy.NewReleaseHandler(core))
	return httptest.NewServer(mux)
}

func TestGoSDK_CheckRequestMatchesSidecarExpectedFormat(t *testing.T) {
	core := &fakeCoreClient{}
	server := newTestSidecar(core)
	defer server.Close()

	resp, err := http.Get(server.URL + "/check?key=contract-test-key&cost=42")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if core.lastCheckReq.Key != "contract-test-key" {
		t.Errorf("expected key=contract-test-key, got %q", core.lastCheckReq.Key)
	}
	if core.lastCheckReq.Cost != 42 {
		t.Errorf("expected cost=42, got %d", core.lastCheckReq.Cost)
	}
}

func TestPythonSDK_CheckRequestMatchesSidecarExpectedFormat(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available in this environment")
	}

	core := &fakeCoreClient{}
	server := newTestSidecar(core)
	defer server.Close()

	// Drives the REAL Python SDK against the REAL sidecar handler (not a
	// second, independently-written description of the protocol) — this is
	// the actual drift-detection mechanism this task exists for.
	script := `
import sys
sys.path.insert(0, "../../../packages/sdks/python/src")
from ratecap import Client
client = Client(sys.argv[1])
result = client.allow("contract-test-key-py", cost=42)
print("ok" if result.allowed else "rejected")
`
	cmd := exec.Command("python3", "-c", script, server.URL) //nolint:gosec // script is a fixed literal above; server.URL is this test's own local httptest.NewServer address
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("python3 invocation failed: %v, output: %s", err, output)
	}

	if core.lastCheckReq.Key != "contract-test-key-py" {
		t.Errorf("expected key=contract-test-key-py, got %q", core.lastCheckReq.Key)
	}
	if core.lastCheckReq.Cost != 42 {
		t.Errorf("expected cost=42 sent by the Python SDK, got %d", core.lastCheckReq.Cost)
	}
}

func TestGoSDK_ReleaseRequestUsesHeadersNotQuery(t *testing.T) {
	core := &fakeCoreClient{}
	server := newTestSidecar(core)
	defer server.Close()

	req, err := http.NewRequest(http.MethodPost, server.URL+"/release", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	req.Header.Set("X-RateCap-Concurrency-Key", "contract-key")
	req.Header.Set("X-RateCap-Concurrency-Token", "contract-token")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if core.lastReleaseReq.Key != "contract-key" || core.lastReleaseReq.ConcurrencyToken != "contract-token" {
		t.Errorf("expected key=contract-key token=contract-token, got key=%q token=%q", core.lastReleaseReq.Key, core.lastReleaseReq.ConcurrencyToken)
	}
}

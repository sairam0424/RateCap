package main

import (
	"net"
	"net/http"
	"testing"
)

func TestResolvePprofEnabled_EmptyDefaultsFalse(t *testing.T) {
	if got := resolvePprofEnabled(""); got {
		t.Errorf("expected false for an empty value, got %v", got)
	}
}

func TestResolvePprofEnabled_TrueEnables(t *testing.T) {
	if got := resolvePprofEnabled("true"); !got {
		t.Errorf("expected true, got %v", got)
	}
}

func TestResolvePprofEnabled_InvalidDefaultsFalse(t *testing.T) {
	if got := resolvePprofEnabled("not-a-bool"); got {
		t.Errorf("expected false for an invalid value, got %v", got)
	}
}

func TestMaybeStartPprofServer_DisabledDoesNotListen(t *testing.T) {
	addr := "127.0.0.1:16060" // arbitrary unused test port, distinct from the real 6060 pprof port

	lis, err := maybeStartPprofServer(false, addr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if lis != nil {
		t.Fatalf("expected a nil listener when pprof is disabled, got %v", lis)
	}

	conn, dialErr := net.Dial("tcp", addr)
	if dialErr == nil {
		conn.Close()
		t.Fatalf("expected connection refused on %s when pprof is disabled, but dial succeeded", addr)
	}
}

func TestMaybeStartPprofServer_EnabledServesDebugIndex(t *testing.T) {
	lis, err := maybeStartPprofServer(true, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if lis == nil {
		t.Fatalf("expected a non-nil listener when pprof is enabled")
	}
	defer lis.Close()

	resp, err := http.Get("http://" + lis.Addr().String() + "/debug/pprof/")
	if err != nil {
		t.Fatalf("unexpected error calling /debug/pprof/: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 from /debug/pprof/, got %d", resp.StatusCode)
	}
}

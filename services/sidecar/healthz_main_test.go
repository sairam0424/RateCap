package main

import (
	"net/http/httptest"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func TestNewHealthzHandler_ReturnsOKWhenConnectionIsNotInTransientFailure(t *testing.T) {
	// A freshly constructed, never-dialed client starts in the Idle state
	// (grpc.NewClient never dials eagerly) — that must read as healthy, not
	// as a false-negative outage.
	conn, err := grpc.NewClient("localhost:1", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("unexpected error creating client: %v", err)
	}
	defer conn.Close()

	handler := newHealthzHandler(conn)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/healthz", nil)

	handler(rec, req)

	if rec.Code != 200 {
		t.Errorf("expected 200 for an idle (never-used) connection, got %d", rec.Code)
	}
}

func TestNewHealthzHandler_Returns503WhenConnectionIsInTransientFailure(t *testing.T) {
	// Dialing a port nothing listens on and forcing a connection attempt
	// drives the connection into TransientFailure, which is the one state
	// this handler must treat as unhealthy.
	conn, err := grpc.NewClient("127.0.0.1:1", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("unexpected error creating client: %v", err)
	}
	defer conn.Close()
	conn.Connect()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if conn.GetState().String() == "TRANSIENT_FAILURE" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	handler := newHealthzHandler(conn)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/healthz", nil)

	handler(rec, req)

	if rec.Code != 503 {
		t.Errorf("expected 503 for a connection stuck in TRANSIENT_FAILURE, got %d", rec.Code)
	}
}

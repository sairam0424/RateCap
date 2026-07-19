package main

import (
	"net/http/httptest"
	"testing"
)

func TestHealthzHandler_AlwaysReturns200(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/healthz", nil)

	healthzHandler(rec, req)

	if rec.Code != 200 {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}

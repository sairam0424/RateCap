package metrics_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func newRequest(t *testing.T) *http.Request {
	t.Helper()
	return httptest.NewRequest(http.MethodGet, "/metrics", nil)
}

func newRecorder() *httptest.ResponseRecorder {
	return httptest.NewRecorder()
}

package decisionlog_test

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/ratecap/sidecar/decisionlog"
)

func TestLog_WritesJSONWithAllFields(t *testing.T) {
	var buf bytes.Buffer
	decisionlog.SetOutput(&buf)
	defer decisionlog.SetOutput(nil)

	decisionlog.Log("rate_limiter", "user-1", "reject_429", "sheddable", 12*time.Millisecond)

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("expected valid JSON, got error %v for output %q", err, buf.String())
	}

	for _, field := range []string{"time", "tier", "key", "action", "priority", "latency_ms"} {
		if _, ok := entry[field]; !ok {
			t.Errorf("expected field %q in log entry, got %v", field, entry)
		}
	}
	if entry["tier"] != "rate_limiter" {
		t.Errorf(`expected tier="rate_limiter", got %v`, entry["tier"])
	}
	if entry["key"] != "user-1" {
		t.Errorf(`expected key="user-1", got %v`, entry["key"])
	}
	if entry["action"] != "reject_429" {
		t.Errorf(`expected action="reject_429", got %v`, entry["action"])
	}
	if entry["priority"] != "sheddable" {
		t.Errorf(`expected priority="sheddable", got %v`, entry["priority"])
	}
}

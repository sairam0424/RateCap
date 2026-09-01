package decisionlog_test

import (
	"bytes"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/sairam0424/RateCap/services/sidecar/decisionlog"
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

func TestLog_ConcurrentCallsAreRaceFree(t *testing.T) {
	var buf bytes.Buffer
	decisionlog.SetOutput(&buf)
	defer decisionlog.SetOutput(nil)

	const goroutines = 100
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			decisionlog.Log("rate_limiter", "concurrent-key", "allow", "sheddable", time.Duration(n)*time.Microsecond)
		}(i)
	}
	wg.Wait()

	lines := bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n"))
	if len(lines) != goroutines {
		t.Fatalf("expected %d log lines, got %d — some writes may have been lost or interleaved", goroutines, len(lines))
	}
	for i, line := range lines {
		var entry map[string]any
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatalf("line %d is not valid JSON (likely torn/interleaved write under concurrency): %v — line was %q", i, err, line)
		}
	}
}

func TestSetOutput_ConcurrentWithLogIsRaceFree(t *testing.T) {
	var buf1, buf2 bytes.Buffer
	defer decisionlog.SetOutput(nil)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			decisionlog.SetOutput(&buf1)
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			decisionlog.Log("rate_limiter", "k", "allow", "sheddable", time.Millisecond)
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			decisionlog.SetOutput(&buf2)
		}()
	}
	wg.Wait()
}

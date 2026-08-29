package decisionlog

import (
	"io"
	"log/slog"
	"os"
	"sync"
	"time"
)

var (
	mu     sync.Mutex
	logger = slog.New(slog.NewJSONHandler(os.Stdout, nil))
)

// SetOutput redirects logging output for tests; passing nil restores stdout.
// Production code never calls this — main() and proxy.go only call Log().
func SetOutput(w io.Writer) {
	mu.Lock()
	defer mu.Unlock()
	if w == nil {
		w = os.Stdout
	}
	logger = slog.New(slog.NewJSONHandler(w, nil))
}

// Log holds mu for the full write, not just the logger read: SetOutput
// builds a brand-new *slog.Logger (with its own internal handler mutex)
// wrapping the same io.Writer each call, so two different logger snapshots
// can otherwise race on that shared writer even though each individually
// looks single-writer-safe.
func Log(tier, key, action, priority string, latency time.Duration) {
	mu.Lock()
	defer mu.Unlock()
	logger.Info("decision",
		"tier", tier,
		"key", key,
		"action", action,
		"priority", priority,
		"latency_ms", latency.Milliseconds(),
	)
}

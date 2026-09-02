package criticalroutes

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
	"gopkg.in/yaml.v3"
)

// debounceWindow coalesces the multiple fsnotify events a single logical
// file write can produce (e.g. os.WriteFile's truncate-then-write on Linux
// inotify) into one reload of the final on-disk content -- mirroring
// services/core/config/watcher.go's identical mechanism and rationale.
// Without this, the watcher goroutine can observe the file mid-write, in a
// transiently-truncated (empty) state; an empty document parses as valid,
// empty YAML (no error), so the "keep last-known-good on malformed reload"
// guarantee never engages -- the set is silently, successfully "reloaded"
// to empty instead. Decision #1 of this package's design spec characterized
// debouncing as purely an efficiency concern for a low-frequency file and
// deliberately omitted it; that was incomplete -- it also guards a real
// correctness gap, confirmed by a genuine (not test-only) CI failure.
const debounceWindow = 50 * time.Millisecond

type fileFormat struct {
	CriticalRoutes []string `yaml:"critical_routes"`
}

// Load parses a critical-routes YAML file into a set of exact "METHOD PATH"
// strings, matching the literal file format from
// docs/superpowers/specs/2026-09-02-tier-3-critical-routes-design.md.
func Load(path string) (map[string]struct{}, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is an operator-supplied env var (RATECAP_CRITICAL_ROUTES_PATH), not attacker input
	if err != nil {
		return nil, fmt.Errorf("reading critical routes %s: %w", path, err)
	}

	var parsed fileFormat
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		return nil, fmt.Errorf("parsing critical routes %s: %w", path, err)
	}

	routes := make(map[string]struct{}, len(parsed.CriticalRoutes))
	for _, route := range parsed.CriticalRoutes {
		routes[route] = struct{}{}
	}
	return routes, nil
}

// Set holds the current critical-routes set behind an atomic pointer so
// Contains never blocks on a concurrent Watch reload.
type Set struct {
	routes atomic.Pointer[map[string]struct{}]
}

// Contains is nil-receiver-safe (false, never a panic) and exact-match only
// — no glob/prefix matching, per Decision #3 of the design spec.
func (s *Set) Contains(route string) bool {
	if s == nil || route == "" {
		return false
	}
	routes := s.routes.Load()
	if routes == nil {
		return false
	}
	_, ok := (*routes)[route]
	return ok
}

// Watch loads path once, then hot-reloads on every (debounced) write to it,
// atomically swapping in the new set and logging-and-keeping-the-last-known-
// good set on a malformed reload. Debounces on the same window and for the
// same correctness reason as config/watcher.go (see debounceWindow) —
// Decision #1 of this package's design spec originally omitted this, which
// was a real gap, not just a missed optimization.
func Watch(path string) (*Set, func(), error) {
	routes, err := Load(path)
	if err != nil {
		return nil, nil, err
	}

	s := &Set{}
	s.routes.Store(&routes)

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, nil, err
	}
	if err := watcher.Add(filepath.Dir(path)); err != nil {
		_ = watcher.Close()
		return nil, nil, err
	}

	done := make(chan struct{})
	stopped := make(chan struct{})
	timer := time.NewTimer(debounceWindow)
	if !timer.Stop() {
		<-timer.C
	}
	go func() {
		defer close(stopped)
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if event.Name == path && (event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename) != 0) {
					// Draining before Reset avoids a stray already-fired value
					// sitting in timer.C from being misread as the new window
					// elapsing instantly; safe non-blockingly since this
					// goroutine is the channel's only consumer.
					if !timer.Stop() {
						select {
						case <-timer.C:
						default:
						}
					}
					timer.Reset(debounceWindow)
				}
			case <-timer.C:
				reloaded, err := Load(path)
				if err != nil {
					log.Printf("criticalroutes: failed to reload %s, keeping last-known-good: %v", path, err)
					continue
				}
				s.routes.Store(&reloaded)
			case _, ok := <-watcher.Errors:
				if !ok {
					return
				}
			case <-done:
				timer.Stop()
				_ = watcher.Close()
				return
			}
		}
	}()

	// stop is synchronous: it blocks until the watcher goroutine has actually
	// exited, not just until the shutdown signal has been sent. Without this,
	// a caller (or, in tests, the next test's t.TempDir() cleanup racing a
	// still-running goroutine from the PREVIOUS test) can observe the
	// goroutine still alive and reacting to filesystem events after stop()
	// returns -- exactly the cross-test interference that caused
	// TestWatch_KeepsLastKnownGoodOnMalformedReload to flake under CI's -race
	// overhead (a leaked goroutine from an earlier, already-torn-down test
	// was still running concurrently).
	var once sync.Once
	stop := func() {
		once.Do(func() {
			close(done)
			<-stopped
		})
	}
	return s, stop, nil
}

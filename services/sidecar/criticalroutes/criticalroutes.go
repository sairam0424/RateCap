package criticalroutes

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/fsnotify/fsnotify"
	"gopkg.in/yaml.v3"
)

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

// Watch loads path once, then hot-reloads on every write to it, atomically
// swapping in the new set and logging-and-keeping-the-last-known-good set on
// a malformed reload — mirroring tlsconfig/reload.go's watchCert exactly,
// deliberately without config/watcher.go's debounce window (Decision #1: a
// route-allowlist file changes at ops cadence, not a cadence worth guarding
// against duplicate fsnotify events for).
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
	go func() {
		defer close(stopped)
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if event.Name == path {
					reloaded, err := Load(path)
					if err != nil {
						log.Printf("criticalroutes: failed to reload %s, keeping last-known-good: %v", path, err)
						continue
					}
					s.routes.Store(&reloaded)
				}
			case _, ok := <-watcher.Errors:
				if !ok {
					return
				}
			case <-done:
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

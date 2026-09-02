package config

import (
	"log"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// debounceWindow coalesces the multiple fsnotify events that a single
// logical file write can produce (e.g. os.WriteFile's truncate-then-write
// on Linux inotify) into one reload of the final on-disk content.
const debounceWindow = 50 * time.Millisecond

func Watch(path string, onChange func(*Config, error)) (stop func(), err error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	dir := filepath.Dir(path)

	if err := watcher.Add(dir); err != nil {
		_ = watcher.Close()
		return nil, err
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
					// elapsing instantly; safe to do non-blockingly since this
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
				cfg, loadErr := Load(path)
				if loadErr != nil {
					log.Printf("error reloading config %s: %v", path, loadErr)
					onChange(nil, loadErr)
					continue
				}
				onChange(cfg, nil)
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				log.Printf("error watching config %s: %v", path, err)
			case <-done:
				timer.Stop()
				_ = watcher.Close()
				return
			}
		}
	}()

	// stop is synchronous: it blocks until the watcher goroutine has actually
	// exited, not just until the shutdown signal has been sent. Without this,
	// a caller can observe the goroutine still alive and reacting to
	// filesystem events after stop() returns -- the same async-stop gap fixed
	// in criticalroutes.Watch's stop() (see that package's git history).
	var once sync.Once
	stop = func() {
		once.Do(func() {
			close(done)
			<-stopped
		})
	}
	return stop, nil
}

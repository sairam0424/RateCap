package config

import (
	"log"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// debounceWindow coalesces the multiple fsnotify events that a single
// logical file write can produce (e.g. os.WriteFile's truncate-then-write
// on Linux inotify) into one reload of the final on-disk content.
const debounceWindow = 50 * time.Millisecond

func Watch(path string, onChange func(*Config)) (stop func(), err error) {
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
	timer := time.NewTimer(debounceWindow)
	if !timer.Stop() {
		<-timer.C
	}
	go func() {
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
				cfg, err := Load(path)
				if err != nil {
					log.Printf("error reloading config %s: %v", path, err)
					continue
				}
				onChange(cfg)
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				log.Printf("error watching config %s: %v", path, err)
			case <-done:
				timer.Stop()
				return
			}
		}
	}()

	stop = func() {
		close(done)
		_ = watcher.Close()
	}
	return stop, nil
}

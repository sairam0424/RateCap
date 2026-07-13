package config

import (
	"path/filepath"

	"github.com/fsnotify/fsnotify"
)

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
	go func() {
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if event.Name == path && (event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename) != 0) {
					cfg, err := Load(path)
					if err != nil {
						continue
					}
					onChange(cfg)
				}
			case _, ok := <-watcher.Errors:
				if !ok {
					return
				}
			case <-done:
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

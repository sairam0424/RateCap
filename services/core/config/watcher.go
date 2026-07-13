package config

import (
	"github.com/fsnotify/fsnotify"
)

func Watch(path string, onChange func(*Config)) (stop func(), err error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	if err := watcher.Add(path); err != nil {
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
				if event.Op&(fsnotify.Write|fsnotify.Create) != 0 {
					cfg, err := Load(path)
					if err == nil {
						onChange(cfg)
					}
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

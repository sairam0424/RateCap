package tlsconfig

import (
	"crypto/tls"
	"log"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/fsnotify/fsnotify"
)

type reloadableCert struct {
	certPath, keyPath string
	current           atomic.Pointer[tls.Certificate]
}

func watchCert(certPath, keyPath string) (*reloadableCert, func(), error) {
	r := &reloadableCert{certPath: certPath, keyPath: keyPath}
	if err := r.reload(); err != nil {
		return nil, nil, err
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, nil, err
	}
	dirs := map[string]bool{filepath.Dir(certPath): true, filepath.Dir(keyPath): true}
	for dir := range dirs {
		if err := watcher.Add(dir); err != nil {
			_ = watcher.Close()
			return nil, nil, err
		}
	}

	done := make(chan struct{})
	go func() {
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if event.Name == certPath || event.Name == keyPath {
					if err := r.reload(); err != nil {
						log.Printf("tlsconfig: failed to reload cert/key, keeping last-known-good: %v", err)
					}
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

	var once sync.Once
	stop := func() { once.Do(func() { close(done) }) }
	return r, stop, nil
}

func (r *reloadableCert) reload() error {
	cert, err := tls.LoadX509KeyPair(r.certPath, r.keyPath)
	if err != nil {
		return err
	}
	r.current.Store(&cert)
	return nil
}

func (r *reloadableCert) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return r.current.Load(), nil
}

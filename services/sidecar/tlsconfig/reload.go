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
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
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

	// stop is synchronous: it blocks until the watcher goroutine has actually
	// exited, not just until the shutdown signal has been sent. Without this,
	// a caller can observe the goroutine still alive and reacting to
	// filesystem events after stop() returns -- the same async-stop gap fixed
	// in criticalroutes.Watch's stop() (see that package's git history).
	var once sync.Once
	stop := func() {
		once.Do(func() {
			close(done)
			<-stopped
		})
	}
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

func (r *reloadableCert) GetClientCertificate(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	return r.current.Load(), nil
}

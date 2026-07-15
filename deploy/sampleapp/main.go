package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"sync/atomic"
	"time"

	ratecap "github.com/ratecap/sdk-go"
)

var fleetDemoCounter atomic.Int64

func main() {
	sidecarAddr := os.Getenv("RATECAP_SIDECAR_ADDR")
	if sidecarAddr == "" {
		sidecarAddr = "http://localhost:8080"
	}

	client := ratecap.NewClient(sidecarAddr)
	sidecarBase := sidecarAddr

	http.HandleFunc("/checkout", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		allowed, retryAfterMs, err := client.Allow(ctx, "demo-user")
		if err != nil {
			http.Error(w, "rate limit check failed", http.StatusInternalServerError)
			return
		}

		if !allowed {
			w.Header().Set("Retry-After-Ms", fmt.Sprintf("%d", retryAfterMs))
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}

		fmt.Fprintln(w, "checkout processed")
	})

	http.HandleFunc("/slow-report", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		ticket, err := client.Acquire(ctx, "demo-user-reports")
		if err != nil {
			http.Error(w, "concurrency check failed", http.StatusInternalServerError)
			return
		}
		defer ticket.Release(ctx)

		if !ticket.Allowed {
			w.Header().Set("Retry-After-Ms", fmt.Sprintf("%d", ticket.RetryAfterMs))
			http.Error(w, "too many concurrent reports in flight", http.StatusTooManyRequests)
			return
		}

		time.Sleep(2 * time.Second)
		fmt.Fprintln(w, "report generated")
	})

	http.HandleFunc("/fleet-demo", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		priority := r.URL.Query().Get("priority")

		// A fresh key per request keeps tier 1 (per-key token bucket) and
		// tier 2 (per-key concurrency cap) from ever tripping here — this
		// endpoint exists to demonstrate tier 3 specifically, which ignores
		// req.Key entirely and checks a single shared "fleet" key instead,
		// so every request's tier-3 reservation still accumulates into one
		// shared count regardless of each request using a distinct key.
		key := fmt.Sprintf("fleet-demo-%d", fleetDemoCounter.Add(1))

		checkReq, err := http.NewRequestWithContext(ctx, http.MethodGet, sidecarBase+"/check?key="+url.QueryEscape(key), nil)
		if err != nil {
			http.Error(w, "request construction failed", http.StatusInternalServerError)
			return
		}
		if priority == "critical" {
			checkReq.Header.Set("x-ratecap-priority", "critical")
		} else {
			checkReq.Header.Set("x-ratecap-priority", "sheddable")
		}

		resp, err := http.DefaultClient.Do(checkReq)
		if err != nil {
			http.Error(w, "fleet check failed", http.StatusInternalServerError)
			return
		}

		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			w.WriteHeader(resp.StatusCode)
			fmt.Fprintf(w, "shed (priority=%s)\n", priority)
			return
		}

		var releaseParams []url.Values
		for i := 0; ; i++ {
			tok := resp.Header.Get(fmt.Sprintf("Concurrency-Token-%d", i))
			if tok == "" {
				break
			}
			resKey := resp.Header.Get(fmt.Sprintf("Concurrency-Key-%d", i))
			params := url.Values{}
			params.Set("key", resKey)
			params.Set("token", tok)
			releaseParams = append(releaseParams, params)
		}
		resp.Body.Close()

		time.Sleep(2 * time.Second)

		for _, params := range releaseParams {
			releaseReq, err := http.NewRequestWithContext(context.Background(), http.MethodPost, sidecarBase+"/release?"+params.Encode(), nil)
			if err != nil {
				continue
			}
			if relResp, err := http.DefaultClient.Do(releaseReq); err == nil {
				relResp.Body.Close()
			}
		}

		fmt.Fprintf(w, "fleet request processed (priority=%s)\n", priority)
	})

	log.Println("sample app listening on :3000")
	log.Fatal(http.ListenAndServe(":3000", nil))
}

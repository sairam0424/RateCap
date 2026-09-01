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

type releaseKey struct {
	key string
	tok string
}

// relayedCheckHeaders lists the /check response headers a caller-facing demo
// endpoint must forward — the sidecar's own proxy.go (X-RateCap-Shed-Tier,
// Retry-After-Ms, RateLimit-Reset) sets these on a decision-by-decision basis,
// so any given response may carry zero, one, or several of them.
var relayedCheckHeaders = []string{"X-RateCap-Shed-Tier", "Retry-After-Ms", "RateLimit-Reset"}

// relayCheckHeaders copies whichever of relayedCheckHeaders are present on the
// sidecar's /check response onto the outgoing response, for every status code
// — must be called before the first WriteHeader/Write on w, since headers set
// afterward are silently dropped by net/http.
func relayCheckHeaders(w http.ResponseWriter, checkResp http.Header) {
	for _, h := range relayedCheckHeaders {
		if v := checkResp.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
}

// releaseAll sends one /release call per reservation, using the headers
// services/sidecar/proxy/proxy.go's ReleaseHandler actually reads
// (X-RateCap-Concurrency-Key/-Token) — a query-param release is silently a
// no-op there (400, discarded below), which is how this leaked before.
func releaseAll(keys []releaseKey, sidecarBase string) {
	for _, rk := range keys {
		releaseReq, err := http.NewRequestWithContext(context.Background(), http.MethodPost, sidecarBase+"/release", nil)
		if err != nil {
			continue
		}
		releaseReq.Header.Set("X-RateCap-Concurrency-Key", rk.key)
		releaseReq.Header.Set("X-RateCap-Concurrency-Token", rk.tok)
		if relResp, err := http.DefaultClient.Do(releaseReq); err == nil {
			relResp.Body.Close() //nolint:errcheck // best-effort release response; the release call itself already succeeded by the time we get here
		}
	}
}

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

		fmt.Fprintln(w, "checkout processed") //nolint:errcheck // demo response write; nothing actionable if the client already disconnected
	})

	http.HandleFunc("/slow-report", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		ticket, err := client.Acquire(ctx, "demo-user-reports")
		if err != nil {
			http.Error(w, "concurrency check failed", http.StatusInternalServerError)
			return
		}
		defer ticket.Release(ctx) //nolint:errcheck // best-effort per Ticket.Release's own godoc; a lost release is freed by the core's reaper TTL, not by retrying here

		if !ticket.Allowed {
			w.Header().Set("Retry-After-Ms", fmt.Sprintf("%d", ticket.RetryAfterMs))
			http.Error(w, "too many concurrent reports in flight", http.StatusTooManyRequests)
			return
		}

		time.Sleep(2 * time.Second)
		fmt.Fprintln(w, "report generated") //nolint:errcheck // demo response write; nothing actionable if the client already disconnected
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

		// Reservation headers must be read before the status-code branch below:
		// a Tier-3 shed (503) still carries the Tier-2 per-key reservation the
		// core granted before Tier 3 rejected the request (proxy.go sets
		// Concurrency-Token-N/Key-N ahead of its final action switch), so
		// skipping this on non-200 leaks that slot until the reaper TTL expires.
		var releaseKeys []releaseKey
		for i := 0; ; i++ {
			tok := resp.Header.Get(fmt.Sprintf("Concurrency-Token-%d", i))
			if tok == "" {
				break
			}
			resKey := resp.Header.Get(fmt.Sprintf("Concurrency-Key-%d", i))
			releaseKeys = append(releaseKeys, releaseKey{key: resKey, tok: tok})
		}

		relayCheckHeaders(w, resp.Header)

		if resp.StatusCode != http.StatusOK {
			resp.Body.Close() //nolint:errcheck // status code already read; Close error carries no new information
			w.WriteHeader(resp.StatusCode)
			fmt.Fprintf(w, "shed (priority=%s)\n", priority) //nolint:errcheck // demo response write; nothing actionable if the client already disconnected
			releaseAll(releaseKeys, sidecarBase)
			return
		}
		resp.Body.Close() //nolint:errcheck // headers already read; Close error carries no new information

		time.Sleep(2 * time.Second)

		releaseAll(releaseKeys, sidecarBase)

		fmt.Fprintf(w, "fleet request processed (priority=%s)\n", priority) //nolint:errcheck // demo response write; nothing actionable if the client already disconnected
	})

	http.HandleFunc("/worker-demo", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		key := fmt.Sprintf("worker-demo-%d", fleetDemoCounter.Add(1))

		checkReq, err := http.NewRequestWithContext(ctx, http.MethodGet, sidecarBase+"/check?key="+url.QueryEscape(key)+"&skip_reservations=true", nil)
		if err != nil {
			http.Error(w, "request construction failed", http.StatusInternalServerError)
			return
		}

		resp, err := http.DefaultClient.Do(checkReq)
		if err != nil {
			http.Error(w, "worker check failed", http.StatusInternalServerError)
			return
		}
		defer resp.Body.Close() //nolint:errcheck // status code already read; Close error carries no new information

		relayCheckHeaders(w, resp.Header)

		if resp.StatusCode != http.StatusOK {
			w.WriteHeader(resp.StatusCode)
			fmt.Fprintf(w, "shed (status=%d)\n", resp.StatusCode) //nolint:errcheck // demo response write; nothing actionable if the client already disconnected
			return
		}

		time.Sleep(2 * time.Second)
		fmt.Fprintln(w, "worker request processed") //nolint:errcheck // demo response write; nothing actionable if the client already disconnected
	})

	log.Println("sample app listening on :3000")
	log.Fatal(http.ListenAndServe(":3000", nil))
}

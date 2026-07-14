package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	ratecap "github.com/ratecap/sdk-go"
)

func main() {
	sidecarAddr := os.Getenv("RATECAP_SIDECAR_ADDR")
	if sidecarAddr == "" {
		sidecarAddr = "http://localhost:8080"
	}

	client := ratecap.NewClient(sidecarAddr)

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

	log.Println("sample app listening on :3000")
	log.Fatal(http.ListenAndServe(":3000", nil))
}

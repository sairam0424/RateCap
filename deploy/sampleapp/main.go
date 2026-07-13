// deploy/sampleapp/main.go
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

	log.Println("sample app listening on :3000")
	log.Fatal(http.ListenAndServe(":3000", nil))
}

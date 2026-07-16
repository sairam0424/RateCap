package main

import (
	"log"
	"net/http"
	"os"
	"strconv"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	ratecapv1 "github.com/ratecap/proto/ratecap/v1"

	"github.com/ratecap/sidecar/auth"
	"github.com/ratecap/sidecar/metrics"
	"github.com/ratecap/sidecar/proxy"
	"github.com/ratecap/sidecar/worker"
)

func resolveMaxInflight(envVal string, defaultVal int64) int64 {
	if envVal == "" {
		return defaultVal
	}
	parsed, err := strconv.ParseInt(envVal, 10, 64)
	if err != nil {
		log.Printf("RATECAP_MAX_INFLIGHT_REQUESTS=%q is not a valid integer, using default of %d: %v", envVal, defaultVal, err)
		return defaultVal
	}
	if parsed <= 0 {
		log.Printf("RATECAP_MAX_INFLIGHT_REQUESTS=%q must be a positive integer, using default of %d", envVal, defaultVal)
		return defaultVal
	}
	return parsed
}

func main() {
	coreAddr := os.Getenv("RATECAP_CORE_ADDR")
	if coreAddr == "" {
		coreAddr = "localhost:9090"
	}

	sharedSecret := os.Getenv("RATECAP_SHARED_SECRET")
	if sharedSecret == "" {
		log.Fatalf("RATECAP_SHARED_SECRET must be set — ratecap-sidecar refuses to start without gRPC authentication configured")
	}

	conn, err := grpc.NewClient(
		coreAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(auth.UnaryClientInterceptor(sharedSecret)),
	)
	if err != nil {
		log.Fatalf("failed to connect to ratecap-core at %s: %v", coreAddr, err)
	}
	defer conn.Close()

	client := ratecapv1.NewRatecapServiceClient(conn)

	maxInflight := resolveMaxInflight(os.Getenv("RATECAP_MAX_INFLIGHT_REQUESTS"), 500)
	shedder := worker.NewShedder(maxInflight)

	mux := http.NewServeMux()
	mux.Handle("/check", proxy.NewHandler(client, proxy.Sheddable, shedder))
	mux.Handle("/release", proxy.NewReleaseHandler(client))
	mux.Handle("/metrics", metrics.Handler())

	listenAddr := os.Getenv("RATECAP_SIDECAR_ADDR")
	if listenAddr == "" {
		listenAddr = ":8080"
	}

	log.Printf("ratecap-sidecar listening on %s, forwarding to core at %s", listenAddr, coreAddr)
	if err := http.ListenAndServe(listenAddr, mux); err != nil {
		log.Fatalf("sidecar http server failed: %v", err)
	}
}

package main

import (
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	ratecapv1 "github.com/ratecap/proto/ratecap/v1"

	"github.com/ratecap/sidecar/admin"
	"github.com/ratecap/sidecar/auth"
	"github.com/ratecap/sidecar/metrics"
	"github.com/ratecap/sidecar/negativecache"
	"github.com/ratecap/sidecar/proxy"
	"github.com/ratecap/sidecar/ratelimit"
	"github.com/ratecap/sidecar/tlsconfig"
	"github.com/ratecap/sidecar/worker"
)

// newHealthzHandler treats every connectivity.State except TransientFailure
// and Shutdown as healthy — including Idle, since grpc.NewClient never
// dials eagerly, so a never-yet-used connection would otherwise read as a
// false-negative outage on a sidecar that just started and hasn't served a
// real request yet.
func newHealthzHandler(conn *grpc.ClientConn) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		state := conn.GetState()
		if state == connectivity.TransientFailure || state == connectivity.Shutdown {
			http.Error(w, "core connection unhealthy: "+state.String(), http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}
}

// newTopMux keeps /metrics and /healthz off the same rate limiter that
// throttles real traffic on /check and /release — a Prometheus scrape must
// never compete with production requests for the same token bucket, exactly
// when an operator needs visibility most (e.g. during a real overload event
// this limiter is throttling).
func newTopMux(protected http.Handler, limiter *ratelimit.Limiter, metricsHandler http.Handler, healthz http.HandlerFunc, adminHandler http.Handler) *http.ServeMux {
	throttled := ratelimit.Middleware(limiter, protected)

	mux := http.NewServeMux()
	mux.Handle("/check", throttled)
	mux.Handle("/release", throttled)
	mux.Handle("/metrics", metricsHandler)
	mux.HandleFunc("/healthz", healthz)
	mux.Handle("/admin/set-limit", adminHandler)
	return mux
}

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

func resolveRampStartPct(envVal string, defaultVal int) int {
	if envVal == "" {
		return defaultVal
	}
	parsed, err := strconv.Atoi(envVal)
	if err != nil {
		log.Printf("RATECAP_SHED_RAMP_START_PCT=%q is not a valid integer, using default of %d: %v", envVal, defaultVal, err)
		return defaultVal
	}
	if parsed <= 0 || parsed > 100 {
		log.Printf("RATECAP_SHED_RAMP_START_PCT=%q must be in (0, 100], using default of %d", envVal, defaultVal)
		return defaultVal
	}
	return parsed
}

func resolveMaxRPS(envVal string, defaultVal float64) float64 {
	if envVal == "" {
		return defaultVal
	}
	parsed, err := strconv.ParseFloat(envVal, 64)
	if err != nil {
		log.Printf("RATECAP_SIDECAR_MAX_RPS=%q is not a valid number, using default of %v: %v", envVal, defaultVal, err)
		return defaultVal
	}
	if parsed <= 0 {
		log.Printf("RATECAP_SIDECAR_MAX_RPS=%q must be a positive number, using default of %v", envVal, defaultVal)
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

	adminSecret := os.Getenv("RATECAP_ADMIN_SECRET")
	if adminSecret == "" {
		log.Fatalf("RATECAP_ADMIN_SECRET must be set — ratecap-sidecar refuses to start without the admin-lever endpoint's own authentication configured")
	}

	tlsCertPath := os.Getenv("RATECAP_TLS_CERT_PATH")
	tlsKeyPath := os.Getenv("RATECAP_TLS_KEY_PATH")
	tlsCAPath := os.Getenv("RATECAP_TLS_CA_PATH")
	if tlsconfig.EnvVarsPartiallySet(tlsCertPath, tlsKeyPath, tlsCAPath) {
		log.Fatalf("RATECAP_TLS_CERT_PATH, RATECAP_TLS_KEY_PATH, and RATECAP_TLS_CA_PATH must be set together or not at all — got cert=%q key=%q ca=%q", tlsCertPath, tlsKeyPath, tlsCAPath)
	}

	transportCreds := insecure.NewCredentials()
	if tlsCertPath != "" {
		tlsConf, stopCertWatch, err := tlsconfig.Load(tlsCertPath, tlsKeyPath, tlsCAPath)
		if err != nil {
			log.Fatalf("failed to load TLS config: %v", err)
		}
		defer stopCertWatch()
		transportCreds = credentials.NewTLS(tlsConf)
		log.Printf("ratecap-sidecar: mTLS enabled")
	}

	conn, err := grpc.NewClient(
		coreAddr,
		grpc.WithTransportCredentials(transportCreds),
		grpc.WithUnaryInterceptor(auth.UnaryClientInterceptor(sharedSecret)),
	)
	if err != nil {
		log.Fatalf("failed to connect to ratecap-core at %s: %v", coreAddr, err)
	}
	defer conn.Close()

	client := ratecapv1.NewRatecapServiceClient(conn)

	maxInflight := resolveMaxInflight(os.Getenv("RATECAP_MAX_INFLIGHT_REQUESTS"), 500)
	rampStartPct := resolveRampStartPct(os.Getenv("RATECAP_SHED_RAMP_START_PCT"), 100)
	shedder := worker.NewShedderWithRamp(maxInflight, rampStartPct)

	protectedMux := http.NewServeMux()
	protectedMux.Handle("/check", proxy.NewHandlerWithCache(client, proxy.Sheddable, shedder, negativecache.New()))
	protectedMux.Handle("/release", proxy.NewReleaseHandler(client))

	maxRPS := resolveMaxRPS(os.Getenv("RATECAP_SIDECAR_MAX_RPS"), 1000)
	limiter := ratelimit.New(maxRPS)
	handler := newTopMux(protectedMux, limiter, metrics.Handler(), newHealthzHandler(conn), admin.NewHandler(client, adminSecret))

	listenAddr := os.Getenv("RATECAP_SIDECAR_ADDR")
	if listenAddr == "" {
		listenAddr = ":8080"
	}

	server := &http.Server{
		Addr:              listenAddr,
		Handler:           handler,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Printf("ratecap-sidecar listening on %s, forwarding to core at %s", listenAddr, coreAddr)
	if err := server.ListenAndServe(); err != nil {
		log.Fatalf("sidecar http server failed: %v", err)
	}
}

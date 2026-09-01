package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	ratecapv1 "github.com/sairam0424/RateCap/proto/ratecap/v1"

	"github.com/sairam0424/RateCap/services/core/auth"
	"github.com/sairam0424/RateCap/services/core/config"
	"github.com/sairam0424/RateCap/services/core/grpcserver"
	"github.com/sairam0424/RateCap/services/core/limiter"
	coremetrics "github.com/sairam0424/RateCap/services/core/metrics"
	"github.com/sairam0424/RateCap/services/core/otelinit"
	"github.com/sairam0424/RateCap/services/core/store"
	"github.com/sairam0424/RateCap/services/core/tlsconfig"
)

// runRedisHealthLoop periodically pings Redis and reflects the result into
// the gRPC health service, so a probe actually detects a Redis outage
// instead of the SERVING status set once at startup and never touched again.
func runRedisHealthLoop(interval time.Duration, ping func(context.Context) error, setStatus func(healthpb.HealthCheckResponse_ServingStatus), stop <-chan struct{}) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), interval)
			err := ping(ctx)
			cancel()
			if err != nil {
				setStatus(healthpb.HealthCheckResponse_NOT_SERVING)
				continue
			}
			setStatus(healthpb.HealthCheckResponse_SERVING)
		}
	}
}

func parseSentinelAddrs(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	addrs := make([]string, 0, len(parts))
	for _, p := range parts {
		trimmed := strings.TrimSpace(p)
		if trimmed != "" {
			addrs = append(addrs, trimmed)
		}
	}
	return addrs
}

// newRedisClient switches to a Sentinel-aware failover client only when
// sentinel addrs are configured — unset, behavior is byte-for-byte
// identical to the pre-existing single-instance redis.NewClient.
func newRedisClient(redisAddr, sentinelAddrsRaw, sentinelMasterName string) *redis.Client {
	sentinelAddrs := parseSentinelAddrs(sentinelAddrsRaw)
	if len(sentinelAddrs) == 0 {
		return redis.NewClient(&redis.Options{Addr: redisAddr})
	}
	log.Printf("ratecap-core: using Redis Sentinel (master=%s, sentinels=%v)", sentinelMasterName, sentinelAddrs)
	return redis.NewFailoverClient(&redis.FailoverOptions{
		MasterName:    sentinelMasterName,
		SentinelAddrs: sentinelAddrs,
	})
}

// resolveTLSMode defaults an unset RATECAP_TLS_MODE to "off" — the exact
// pre-Phase-3 behavior (single listener, TLS-only if cert env vars happen
// to be set, plaintext-only otherwise) — preserved unconditionally so no
// existing deployment's behavior changes just because this env var now
// exists.
func resolveTLSMode(raw string) (string, error) {
	switch raw {
	case "":
		return "off", nil
	case "off", "permissive", "strict":
		return raw, nil
	default:
		return "", fmt.Errorf("RATECAP_TLS_MODE=%q is invalid — must be one of: off, permissive, strict", raw)
	}
}

// resolvePprofEnabled defaults an unset or invalid RATECAP_PPROF_ENABLED to
// false — profiling must never turn on just because the env var exists with
// an unexpected value.
func resolvePprofEnabled(raw string) bool {
	if raw == "" {
		return false
	}
	enabled, err := strconv.ParseBool(raw)
	if err != nil {
		log.Printf("RATECAP_PPROF_ENABLED=%q is not a valid boolean, defaulting to false", raw)
		return false
	}
	return enabled
}

// newPprofMux builds a dedicated mux for net/http/pprof's handlers rather
// than relying on the package's side-effect registration on
// http.DefaultServeMux, which this repo's public servers never use.
func newPprofMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	for _, name := range []string{"goroutine", "heap", "allocs", "block", "mutex", "threadcreate"} {
		mux.Handle("/debug/pprof/"+name, pprof.Handler(name))
	}
	return mux
}

// startPprofServer binds addr and serves the pprof mux in the background.
// It places no restriction on addr itself — callers must pass a
// loopback-only address in production; tests can pass "127.0.0.1:0" to get
// an OS-assigned ephemeral port instead.
func startPprofServer(addr string) (net.Listener, error) {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	server := &http.Server{Handler: newPprofMux(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := server.Serve(lis); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("ratecap-core pprof server on %s failed: %v", addr, err)
		}
	}()
	return lis, nil
}

// maybeStartPprofServer only listens when enabled — the whole point of the
// flag is that profiling stays off by default, everywhere, unconditionally.
func maybeStartPprofServer(enabled bool, addr string) (net.Listener, error) {
	if !enabled {
		return nil, nil
	}
	lis, err := startPprofServer(addr)
	if err != nil {
		return nil, err
	}
	log.Printf("ratecap-core: pprof enabled on %s — do not expose this port", addr)
	return lis, nil
}

func main() {
	otelShutdown, err := otelinit.Init(context.Background(), "ratecap-core")
	if err != nil {
		log.Fatalf("failed to initialize OpenTelemetry: %v", err)
	}
	defer func() {
		if err := otelShutdown(context.Background()); err != nil {
			log.Printf("otel shutdown failed: %v", err)
		}
	}()

	configPath := os.Getenv("RATECAP_CONFIG_PATH")
	if configPath == "" {
		configPath = "/etc/ratecap/ratecap.yaml"
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		log.Fatalf("invalid config: %v", err)
	}
	coremetrics.RecordConfigVersion(cfg.Hash())

	redisAddr := os.Getenv("RATECAP_REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "localhost:6379"
	}

	sharedSecret := os.Getenv("RATECAP_SHARED_SECRET")
	if sharedSecret == "" {
		log.Fatalf("RATECAP_SHARED_SECRET must be set — ratecap-core refuses to start without gRPC authentication configured")
	}

	concurrencySigningKey := os.Getenv("RATECAP_CONCURRENCY_SIGNING_KEY")
	if concurrencySigningKey == "" {
		log.Fatalf("RATECAP_CONCURRENCY_SIGNING_KEY must be set — ratecap-core refuses to start without Tier 2 concurrency-token signing configured")
	}

	tlsCertPath := os.Getenv("RATECAP_TLS_CERT_PATH")
	tlsKeyPath := os.Getenv("RATECAP_TLS_KEY_PATH")
	tlsCAPath := os.Getenv("RATECAP_TLS_CA_PATH")
	if tlsconfig.EnvVarsPartiallySet(tlsCertPath, tlsKeyPath, tlsCAPath) {
		log.Fatalf("RATECAP_TLS_CERT_PATH, RATECAP_TLS_KEY_PATH, and RATECAP_TLS_CA_PATH must be set together or not at all — got cert=%q key=%q ca=%q", tlsCertPath, tlsKeyPath, tlsCAPath)
	}

	tlsMode, err := resolveTLSMode(os.Getenv("RATECAP_TLS_MODE"))
	if err != nil {
		log.Fatalf("%v", err)
	}
	if (tlsMode == "permissive" || tlsMode == "strict") && tlsCertPath == "" {
		log.Fatalf("RATECAP_TLS_MODE=%s requires RATECAP_TLS_CERT_PATH/RATECAP_TLS_KEY_PATH/RATECAP_TLS_CA_PATH to be set", tlsMode)
	}

	sentinelMasterName := os.Getenv("RATECAP_REDIS_SENTINEL_MASTER_NAME")
	if sentinelMasterName == "" {
		sentinelMasterName = "mymaster"
	}
	redisClient := newRedisClient(redisAddr, os.Getenv("RATECAP_REDIS_SENTINEL_ADDRS"), sentinelMasterName)
	redisStore := store.NewRedisStore(redisClient, []byte(concurrencySigningKey))

	rateLimiter := limiter.NewTokenBucketLimiter(
		redisStore,
		cfg.Tiers.RateLimiter.DefaultRate,
		cfg.Tiers.RateLimiter.DefaultBurst,
		cfg.Tiers.RateLimiter.ShadowMode,
	)

	concurrencyLimiter := limiter.NewConcurrencyLimiter(
		redisStore,
		cfg.Tiers.ConcurrencyLimiter.DefaultMaxConcurrent,
		cfg.Tiers.ConcurrencyLimiter.MaxRequestDurationMs,
		cfg.Tiers.ConcurrencyLimiter.ShadowMode,
		cfg.Tiers.ConcurrencyLimiter.QueueingEnabled,
		cfg.Tiers.ConcurrencyLimiter.MaxBacklog,
		cfg.Tiers.ConcurrencyLimiter.MaxQueueWaitMs,
		cfg.Tiers.ConcurrencyLimiter.PollIntervalMs,
	)

	fleetShedder := limiter.NewFleetShedder(
		redisStore,
		cfg.Tiers.FleetShedder.DefaultMaxConcurrent,
		cfg.Tiers.FleetShedder.ReservedCriticalPct,
		cfg.Tiers.FleetShedder.MaxRequestDurationMs,
		cfg.Tiers.FleetShedder.ShadowMode,
	)

	pipeline := limiter.NewPipeline(rateLimiter, concurrencyLimiter, fleetShedder)

	stopWatch, err := config.Watch(configPath, func(newCfg *config.Config, loadErr error) {
		if loadErr != nil {
			coremetrics.RecordConfigReload("failure")
			return
		}
		if err := newCfg.Validate(); err != nil {
			log.Printf("ignoring invalid config reload: %v", err)
			coremetrics.RecordConfigReload("failure")
			return
		}
		coremetrics.RecordConfigReload("success")
		coremetrics.RecordConfigVersion(newCfg.Hash())
		log.Printf("ratecap-core: config reloaded, hash=%s", newCfg.Hash())
		rateLimiter.Reconfigure(newCfg.Tiers.RateLimiter.DefaultRate, newCfg.Tiers.RateLimiter.DefaultBurst, newCfg.Tiers.RateLimiter.ShadowMode)
		concurrencyLimiter.Reconfigure(newCfg.Tiers.ConcurrencyLimiter.DefaultMaxConcurrent, newCfg.Tiers.ConcurrencyLimiter.MaxRequestDurationMs, newCfg.Tiers.ConcurrencyLimiter.ShadowMode, newCfg.Tiers.ConcurrencyLimiter.QueueingEnabled, newCfg.Tiers.ConcurrencyLimiter.MaxBacklog, newCfg.Tiers.ConcurrencyLimiter.MaxQueueWaitMs, newCfg.Tiers.ConcurrencyLimiter.PollIntervalMs)
		fleetShedder.Reconfigure(newCfg.Tiers.FleetShedder.DefaultMaxConcurrent, newCfg.Tiers.FleetShedder.ReservedCriticalPct, newCfg.Tiers.FleetShedder.MaxRequestDurationMs, newCfg.Tiers.FleetShedder.ShadowMode)
	})
	if err != nil {
		log.Fatalf("failed to start config watcher: %v", err)
	}
	defer stopWatch()

	listenAddr := os.Getenv("RATECAP_GRPC_ADDR")
	if listenAddr == "" {
		listenAddr = ":9090"
	}

	lis, err := net.Listen("tcp", listenAddr)
	if err != nil {
		log.Fatalf("failed to listen on %s: %v", listenAddr, err)
	}

	serverOpts := []grpc.ServerOption{
		grpc.ChainUnaryInterceptor(auth.UnaryServerInterceptor(sharedSecret), coremetrics.UnaryServerInterceptor()),
		grpc.StatsHandler(otelgrpc.NewServerHandler()),
	}
	if tlsCertPath != "" && tlsMode != "permissive" {
		tlsConf, stopCertWatch, err := tlsconfig.Load(tlsCertPath, tlsKeyPath, tlsCAPath)
		if err != nil {
			log.Fatalf("failed to load TLS config: %v", err)
		}
		defer stopCertWatch()
		serverOpts = append(serverOpts, grpc.Creds(credentials.NewTLS(tlsConf)))
		log.Printf("ratecap-core: mTLS enabled (mode=%s)", tlsMode)
	}
	grpcServer := grpc.NewServer(serverOpts...)
	coreServer := grpcserver.NewServer(pipeline, redisStore, rateLimiter, fleetShedder, redisStore, []byte(concurrencySigningKey))
	ratecapv1.RegisterRatecapServiceServer(grpcServer, coreServer)

	if tlsMode == "permissive" {
		tlsAddr := os.Getenv("RATECAP_GRPC_TLS_ADDR")
		if tlsAddr == "" {
			tlsAddr = ":9443"
		}
		tlsLis, err := net.Listen("tcp", tlsAddr)
		if err != nil {
			log.Fatalf("failed to listen on %s: %v", tlsAddr, err)
		}
		permissiveConf, stopCertWatch, err := tlsconfig.Load(tlsCertPath, tlsKeyPath, tlsCAPath)
		if err != nil {
			log.Fatalf("failed to load TLS config for permissive listener: %v", err)
		}
		defer stopCertWatch()
		// VerifyClientCertIfGiven (not RequireAndVerifyClientCert): permissive
		// mode's whole purpose is letting sidecars migrate one at a time — a
		// sidecar without a cert yet must still be able to connect over this
		// listener's TLS transport (server-authenticated only), while one that
		// does present a cert gets it verified.
		permissiveConf.ClientAuth = tls.VerifyClientCertIfGiven
		tlsServerOpts := []grpc.ServerOption{
			grpc.ChainUnaryInterceptor(auth.UnaryServerInterceptor(sharedSecret), coremetrics.UnaryServerInterceptor()),
			grpc.Creds(credentials.NewTLS(permissiveConf)),
			grpc.StatsHandler(otelgrpc.NewServerHandler()),
		}
		tlsGrpcServer := grpc.NewServer(tlsServerOpts...)
		ratecapv1.RegisterRatecapServiceServer(tlsGrpcServer, coreServer)
		go func() {
			log.Printf("ratecap-core permissive-mode TLS listener (optional client cert) on %s", tlsAddr)
			if err := tlsGrpcServer.Serve(tlsLis); err != nil {
				log.Fatalf("permissive-mode TLS grpc server failed: %v", err)
			}
		}()
		log.Printf("ratecap-core: TLS_MODE=permissive — plaintext still serving on %s, TLS available on %s", listenAddr, tlsAddr)
	}

	// The health service is served on its own plaintext, unauthenticated
	// listener rather than the main gRPC port: Kubernetes' native grpc probe
	// action has no TLS/client-cert support, so a probe on the mTLS-enforcing
	// main port would always fail once mTLS is enabled.
	healthAddr := os.Getenv("RATECAP_HEALTH_ADDR")
	if healthAddr == "" {
		healthAddr = ":9091"
	}
	healthLis, err := net.Listen("tcp", healthAddr)
	if err != nil {
		log.Fatalf("failed to listen on %s: %v", healthAddr, err)
	}
	healthServer := health.NewServer()
	healthServer.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	stopHealthLoop := make(chan struct{})
	defer close(stopHealthLoop)
	go runRedisHealthLoop(5*time.Second, func(ctx context.Context) error {
		return redisClient.Ping(ctx).Err()
	}, func(s healthpb.HealthCheckResponse_ServingStatus) {
		healthServer.SetServingStatus("", s)
	}, stopHealthLoop)
	healthGRPCServer := grpc.NewServer()
	healthpb.RegisterHealthServer(healthGRPCServer, healthServer)
	go func() {
		log.Printf("ratecap-core health server listening on %s", healthAddr)
		if err := healthGRPCServer.Serve(healthLis); err != nil {
			log.Fatalf("health grpc server failed: %v", err)
		}
	}()

	if _, err := maybeStartPprofServer(resolvePprofEnabled(os.Getenv("RATECAP_PPROF_ENABLED")), "127.0.0.1:6060"); err != nil {
		log.Fatalf("failed to start pprof server: %v", err)
	}

	metricsAddr := os.Getenv("RATECAP_METRICS_ADDR")
	if metricsAddr == "" {
		metricsAddr = ":9092"
	}
	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", coremetrics.Handler())
	metricsServer := &http.Server{Addr: metricsAddr, Handler: metricsMux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		log.Printf("ratecap-core metrics server listening on %s", metricsAddr)
		if err := metricsServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("metrics http server failed: %v", err)
		}
	}()

	log.Printf("ratecap-core listening on %s", listenAddr)
	if err := grpcServer.Serve(lis); err != nil {
		log.Fatalf("grpc server failed: %v", err)
	}
}

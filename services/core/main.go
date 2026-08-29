package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	ratecapv1 "github.com/ratecap/proto/ratecap/v1"

	"github.com/ratecap/core/auth"
	"github.com/ratecap/core/config"
	"github.com/ratecap/core/grpcserver"
	"github.com/ratecap/core/limiter"
	coremetrics "github.com/ratecap/core/metrics"
	"github.com/ratecap/core/store"
	"github.com/ratecap/core/tlsconfig"
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

func main() {
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

	redisClient := redis.NewClient(&redis.Options{Addr: redisAddr})
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
	}
	if tlsCertPath != "" {
		tlsConf, err := tlsconfig.Load(tlsCertPath, tlsKeyPath, tlsCAPath)
		if err != nil {
			log.Fatalf("failed to load TLS config: %v", err)
		}
		serverOpts = append(serverOpts, grpc.Creds(credentials.NewTLS(tlsConf)))
		log.Printf("ratecap-core: mTLS enabled")
	}
	grpcServer := grpc.NewServer(serverOpts...)
	ratecapv1.RegisterRatecapServiceServer(grpcServer, grpcserver.NewServer(pipeline, redisStore, []byte(concurrencySigningKey)))

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

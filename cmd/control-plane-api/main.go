// Command control-plane-api is the SAOAF control plane API process.
//
// I02 scope: process skeleton only — HTTP server wiring, health probes, and
// graceful shutdown. Domain endpoints arrive with I06–I17.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/davidchen516/saoaf/internal/platform/approval"
	"github.com/davidchen516/saoaf/internal/platform/audit"
	"github.com/davidchen516/saoaf/internal/platform/authn"
	"github.com/davidchen516/saoaf/internal/platform/authz"
	"github.com/davidchen516/saoaf/internal/platform/httpapi"
	"github.com/davidchen516/saoaf/internal/policy"
	"github.com/davidchen516/saoaf/internal/resolver"
)

func main() {
	addr := flag.String("addr", envOr("CONTROL_PLANE_API_ADDR", ":8080"), "listen address")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("component", "control-plane-api")

	// Startup readiness: the process is ready once it can serve traffic.
	// Future issues replace this with dependency checks (I04 database, I05 identity).
	health := httpapi.NewHealth(func() bool { return true })
	router := chi.NewMux()
	router.Get("/healthz", health.LivenessHandler)
	router.Get("/readyz", health.ReadinessHandler)

	// I05 admin demo endpoints: enabled only when the full adapter config
	// is present (issuer + PDP + approval); otherwise the admin API stays
	// CLOSED rather than open (fail-closed by default).
	if cfg := adminConfigFromEnv(); cfg != nil {
		httpapi.MountAdmin(router, *cfg)
	}

	// I09 runtime resolver API: mounted only when identity + database are
	// configured; otherwise the runtime surface stays CLOSED.
	if cfg := resolverConfigFromEnv(); cfg != nil {
		httpapi.MountResolver(router, cfg)
	}

	srv := &http.Server{
		Addr:              *addr,
		Handler:           router,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		logger.Info("api listening", "addr", *addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		logger.Error("server failed", "error", err)
		os.Exit(1)
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", "error", err)
		os.Exit(1)
	}
	logger.Info("api stopped")
}

// resolverConfigFromEnv returns the I09 resolver wiring when identity +
// database are configured; nil keeps the runtime surface closed.
func resolverConfigFromEnv() *httpapi.ResolverMountConfig {
	issuer := os.Getenv("SAOAF_OIDC_ISSUER")
	dsn := os.Getenv("SAOAF_DB_DSN")
	if issuer == "" || dsn == "" {
		return nil
	}
	v, err := authn.NewValidator(issuer, "saoaf-control-plane")
	if err != nil {
		return nil
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		return nil
	}
	// optional policy filtering (issue: Policy 过滤携带 policy revision —
	// no configured set means a documented pass-through)
	policySetID := envOr("SAOAF_RESOLVER_POLICY_SET", "")
	var pol *policy.Store
	if policySetID != "" {
		pol = &policy.Store{DSN: dsn, Pool: pool}
	}
	cache := resolver.NewSnapshotCache(resolver.NewRegistrySnapshotLoader(pool), 30*time.Second)
	svc := &resolver.Service{
		Plans:       &resolver.Store{Pool: pool},
		Cache:       cache,
		Pool:        pool,
		Policy:      pol,
		Eval:        policy.NewEvaluator(),
		PolicySetID: policySetID,
		Now:         time.Now,
		NewID:       resolver.NewPlanID,
		DefaultTTL:  300 * time.Second,
		MaxTTL:      3600 * time.Second,
	}
	// ready: DB readable AND at least one published snapshot loaded
	// (specs §3.3 — readiness reports dependency state, nothing else)
	health := httpapi.NewHealth(func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		return pool.Ping(ctx) == nil && cache.Loaded() > 0
	})
	return &httpapi.ResolverMountConfig{
		Service: svc, Authn: v, Health: health,
		RatePerSec: 100, RateBurst: 200,
	}
}

// adminConfigFromEnv returns the I05 admin wiring when all adapter
// endpoints are configured; nil keeps the admin surface closed.
func adminConfigFromEnv() *httpapi.AdminConfig {
	issuer := os.Getenv("SAOAF_OIDC_ISSUER")
	pdp := os.Getenv("SAOAF_PDP_URL")
	appr := os.Getenv("SAOAF_APPROVAL_URL")
	if issuer == "" || pdp == "" || appr == "" {
		return nil
	}
	v, err := authn.NewValidator(issuer, "saoaf-control-plane")
	if err != nil {
		return nil
	}
	var sink audit.Sink = audit.NoopSink{}
	if dsn := os.Getenv("SAOAF_DB_DSN"); dsn != "" {
		sink = audit.PostgresSink{DSN: dsn}
	}
	return &httpapi.AdminConfig{
		Authn:      v,
		PDP:        authz.NewPDPClient(pdp),
		Approvals:  approval.NewClient(appr),
		Audit:      sink,
		RatePerSec: 100,
		RateBurst:  200,
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

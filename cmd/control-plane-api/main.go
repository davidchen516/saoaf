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

	"github.com/davidchen516/saoaf/internal/hub"
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
	if cfg := resolverConfigFromEnv(logger); cfg != nil {
		resolver.MountResolver(router, cfg)
	}

	// I16 resource hub read API (module 03.2): mounted when the database
	// is configured; reads only (publish remains on the approval-gated
	// admin chain from I05)
	if dsn := os.Getenv("SAOAF_DB_DSN"); dsn != "" {
		pool, err := pgxpool.New(context.Background(), dsn)
		if err == nil {
			hub.Mount(router, hub.Config{Pool: pool, BindingsDSN: dsn})
		}
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
func resolverConfigFromEnv(logger *slog.Logger) *resolver.ResolverMountConfig {
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
	// no configured set means a documented pass-through). ADR-0006: the
	// resolver module cannot import the policy module; this composition
	// root adapts one onto the other.
	policySetID := envOr("SAOAF_RESOLVER_POLICY_SET", "")
	var engine resolver.PolicyEngine
	if policySetID != "" {
		engine = policyEngine{
			store: &policy.Store{DSN: dsn, Pool: pool},
			eval:  policy.NewEvaluator(),
		}
	}
	cache := resolver.NewSnapshotCache(resolver.NewRegistrySnapshotLoader(pool), 30*time.Second)
	// startup warmup (review P2-1): readiness requires ≥1 loaded snapshot;
	// preloading here keeps a fresh instance ready before first traffic
	if n, err := cache.WarmupActive(context.Background(), resolver.ActiveSnapshotIDs(pool)); err != nil {
		// warmup failure is NOT fatal: readiness reflects it (Loaded()==0)
		// and traffic-driven loads can still warm the cache
		logger.Warn("resolver snapshot warmup failed", "error", err)
	} else {
		logger.Info("resolver snapshots warmed", "loaded", n)
	}
	svc := &resolver.Service{
		Plans:       &resolver.Store{Pool: pool},
		Cache:       cache,
		Pool:        pool,
		Policy:      engine,
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
	return &resolver.ResolverMountConfig{
		Service: svc, Authn: v, Health: health,
		RatePerSec: 100, RateBurst: 200,
	}
}

// policyEngine adapts the policy module onto resolver.PolicyEngine
// (the composition root is the only place allowed to see both modules).
type policyEngine struct {
	store *policy.Store
	eval  *policy.Evaluator
}

func (p policyEngine) ActiveRevision(ctx context.Context, setID string) (*resolver.PolicyRevision, error) {
	r, err := p.store.ActiveRevision(ctx, setID)
	if errors.Is(err, policy.ErrNotFound) {
		return nil, resolver.ErrNoActivePolicy
	}
	if err != nil {
		return nil, err
	}
	return &resolver.PolicyRevision{
		SetID: r.SetID, Version: r.Version, Expression: r.Content.EligibilityCEL,
	}, nil
}

func (p policyEngine) Evaluate(ctx context.Context, rev *resolver.PolicyRevision, in resolver.PolicyInput) (bool, string, error) {
	pr := &policy.Revision{
		SetID: rev.SetID, Version: rev.Version,
		Content: policy.Content{EligibilityCEL: rev.Expression},
	}
	ev, reason, err := p.eval.Evaluate(ctx, pr, policy.EligibilityInput{
		Region: in.Region, DataClass: in.DataClass,
		Environment: in.Environment, TenantRef: in.TenantRef,
	})
	if err != nil {
		return false, reason, err
	}
	return ev.Allow, reason, nil
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

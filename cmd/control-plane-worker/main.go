// Command control-plane-worker is the SAOAF control plane worker process.
//
// I02 scope: process skeleton only — a supervised run loop with graceful
// shutdown and an admin health endpoint. Real work consumers arrive with
// I10 (outbox) and I12 (evidence consumption).
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/davidchen516/saoaf/internal/events"
	"github.com/davidchen516/saoaf/internal/platform/httpapi"
	"github.com/davidchen516/saoaf/internal/platform/worker"
)

func main() {
	adminAddr := flag.String("admin-addr", envOr("CONTROL_PLANE_WORKER_ADMIN_ADDR", "127.0.0.1:8081"), "admin health listen address (loopback: the Phase 0 boundary until I22 wires production identity)")
	interval := flag.Duration("interval", time.Second, "no-op tick interval (skeleton)")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("component", "control-plane-worker")

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// I10 outbox worker: mounted only when the database and the event
	// transport are configured; otherwise the skeleton tick keeps the
	// process alive (fail-closed wiring).
	var outbox *events.OutboxWorker
	dsn := os.Getenv("SAOAF_DB_DSN")
	natsURL := os.Getenv("SAOAF_NATS_URL")
	if dsn == "" || natsURL == "" {
		logger.Warn("outbox worker disabled: SAOAF_DB_DSN / SAOAF_NATS_URL not set")
	}
	if dsn != "" && natsURL != "" {
		pool, err := pgxpool.New(ctx, dsn)
		if err != nil {
			logger.Error("worker pool failed", "error", err)
			os.Exit(1)
		}
		defer pool.Close()
		transport, err := events.NewNATSTransport(ctx, events.NATSConfig{
			URL:      natsURL,
			Replicas: int(envInt("SAOAF_NATS_REPLICAS", 1)),
		})
		if err != nil {
			logger.Error("nats transport failed", "error", err)
			os.Exit(1)
		}
		outbox = events.NewOutboxWorker(events.WorkerConfig{
			Pool:         pool,
			Transport:    transport,
			WorkerID:     envOr("SAOAF_WORKER_ID", "worker-1"),
			BacklogAlert: int(envInt("SAOAF_OUTBOX_BACKLOG_ALERT", 10000)),
			Logger:       logger,
		}, events.Hooks{})
		go func() {
			if err := outbox.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
				logger.Error("outbox worker exited", "error", err)
			}
		}()
	}

	// Skeleton tick: superseded by the outbox worker when enabled.
	loop := worker.NewLoop(*interval, func(ctx context.Context) error {
		return nil
	})

	health := httpapi.NewHealth(func() bool {
		if outbox == nil {
			return true // outbox disabled: process-level readiness
		}
		return outbox.TransportHealthy(context.Background())
	})

	// Admin surface: worker metrics + pause/resume management operations
	// (GWT#6: management actions are scope-gated; the local loopback
	// binding is the Phase 0 boundary — I22 wires production identity).
	admin := chi.NewRouter()
	admin.Get("/healthz", health.LivenessHandler)
	admin.Get("/readyz", health.ReadinessHandler)
	admin.Get("/metrics", func(w http.ResponseWriter, r *http.Request) {
		if outbox == nil {
			httpapi.WriteJSON(w, http.StatusOK, map[string]any{"outbox": "disabled"})
			return
		}
		s := outbox.Stats()
		httpapi.WriteJSON(w, http.StatusOK, map[string]any{
			"published": s.Published, "failed": s.Failed, "retries": s.Retries,
			"publish_errors": s.PublishErrors, "backlog_depth": s.BacklogDepth,
			"publish_max_ms": s.PublishMaxMS, "dlq_depth": s.DLQDepth,
		})
	})
	admin.Post("/admin/v1/events/worker:pause", func(w http.ResponseWriter, r *http.Request) {
		if outbox == nil {
			httpapi.WriteJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "outbox disabled"})
			return
		}
		outbox.Pause()
		httpapi.WriteJSON(w, http.StatusOK, map[string]any{"status": "paused"})
	})
	admin.Post("/admin/v1/events/worker:resume", func(w http.ResponseWriter, r *http.Request) {
		if outbox == nil {
			httpapi.WriteJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "outbox disabled"})
			return
		}
		outbox.Resume()
		httpapi.WriteJSON(w, http.StatusOK, map[string]any{"status": "resumed"})
	})

	adminSrv := &http.Server{
		Addr:              *adminAddr,
		Handler:           admin,
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 2)
	go func() {
		logger.Info("worker admin health listening", "addr", *adminAddr)
		if err := adminSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	go func() {
		if err := loop.Run(ctx); err != nil {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		logger.Error("worker failed", "error", err)
		stop()
		os.Exit(1)
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := adminSrv.Shutdown(shutdownCtx); err != nil {
		logger.Error("admin shutdown failed", "error", err)
	}
	logger.Info("worker stopped")
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int64) int64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return fallback
}

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
	"syscall"
	"time"

	"github.com/davidchen516/saoaf/internal/platform/httpapi"
	"github.com/davidchen516/saoaf/internal/platform/worker"
)

func main() {
	adminAddr := flag.String("admin-addr", envOr("CONTROL_PLANE_WORKER_ADMIN_ADDR", ":8081"), "admin health listen address")
	interval := flag.Duration("interval", time.Second, "no-op tick interval (skeleton)")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("component", "control-plane-worker")

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Skeleton tick: proves the loop is alive. Domain work replaces this in I10/I12.
	loop := worker.NewLoop(*interval, func(ctx context.Context) error {
		return nil
	})

	health := httpapi.NewHealth(func() bool { return true })
	adminSrv := &http.Server{
		Addr:              *adminAddr,
		Handler:           httpapi.NewRouter(health),
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

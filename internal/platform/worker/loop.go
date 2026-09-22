// Package worker provides the supervised run loop shared by control plane
// background processes. Real consumers (I10 outbox publisher, I12 evidence
// consumer) supply the tick function; the loop owns timing, panic recovery,
// and shutdown semantics.
package worker

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"
)

// Loop runs fn on every tick until ctx is cancelled. A panic inside fn is
// recovered and logged as an error rather than killing the process — the
// process health plane (I05/I21 runbook) owns restart policy.
type Loop struct {
	interval time.Duration
	fn       func(ctx context.Context) error
	logger   *slog.Logger
}

// NewLoop builds a Loop. interval must be positive; fn must be non-nil.
// Both are constructor contracts — violations panic with an explicit message
// instead of the cryptic time.NewTicker panic later.
func NewLoop(interval time.Duration, fn func(ctx context.Context) error, opts ...Option) *Loop {
	if interval <= 0 {
		panic(fmt.Sprintf("worker: NewLoop interval must be positive, got %v", interval))
	}
	if fn == nil {
		panic("worker: NewLoop fn must be non-nil")
	}
	l := &Loop{interval: interval, fn: fn, logger: slog.Default()}
	for _, opt := range opts {
		opt(l)
	}
	return l
}

// Option customizes a Loop.
type Option func(*Loop)

// WithLogger sets the loop logger.
func WithLogger(logger *slog.Logger) Option {
	return func(l *Loop) { l.logger = logger }
}

// Run blocks until ctx is done. Ticks that fail are logged and the loop
// continues; shutdown is only via context cancellation.
func (l *Loop) Run(ctx context.Context) error {
	ticker := time.NewTicker(l.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			l.tick(ctx)
		}
	}
}

func (l *Loop) tick(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			l.logger.Error("worker tick panicked",
				"panic", fmt.Sprint(r),
				"stack", string(debug.Stack()))
		}
	}()
	if err := l.fn(ctx); err != nil {
		l.logger.Error("worker tick failed", "error", err)
	}
}

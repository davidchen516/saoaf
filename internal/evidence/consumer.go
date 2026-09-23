package evidence

// Consumer loop (I12): scan → ingest → alarm; crash windows are the
// IngestBatch tx boundary (nothing committed → redo; committed → dedup
// absorbs the rescan). Hooks support the kill -9 two-window matrix.

import (
	"context"
	"log/slog"
	"time"
)

// Consumer drains the outbox into the evidence index.
type Consumer struct {
	Index      Index
	ConsumerID string
	BatchSize  int
	Interval   time.Duration
	Logger     *slog.Logger

	// crash-injection hooks (nil in production)
	// OnScanned: after scanning a batch, before the ingest tx (窗口 A:
	// DB 提交前)
	OnScanned func(ctx context.Context, n int)
	// OnIngested: after IngestBatch returned (窗口 B: 提交后、下次扫描
	// 即 checkpoint 已随批提交 —— 该窗口下重扫经幂等吸收)
	OnIngested func(ctx context.Context, created int)
}

// Run consumes until ctx cancels. Quarantined batches alarm per record.
func (c Consumer) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		events, err := c.Index.ScanBatch(ctx, c.ConsumerID, c.batch())
		if err != nil {
			c.log().Error("evidence scan failed", "error", err)
			if !sleepCtx(ctx, c.interval()) {
				return ctx.Err()
			}
			continue
		}
		if len(events) == 0 {
			if !sleepCtx(ctx, c.interval()) {
				return ctx.Err()
			}
			continue
		}
		if c.OnScanned != nil {
			c.OnScanned(ctx, len(events))
		}
		created, quarantined, err := c.Index.IngestBatch(ctx, c.ConsumerID, events)
		if err != nil {
			c.log().Error("evidence ingest failed", "error", err)
			if !sleepCtx(ctx, c.interval()) {
				return ctx.Err()
			}
			continue
		}
		for _, q := range quarantined {
			c.log().Warn("evidence quarantined (隔离告警)",
				"event", q.EventID, "topic", q.SourceTopic, "reason", q.QuarantineReason)
		}
		if c.OnIngested != nil {
			c.OnIngested(ctx, created)
		}
	}
}

func (c Consumer) batch() int {
	if c.BatchSize <= 0 {
		return 100
	}
	return c.BatchSize
}

func (c Consumer) interval() time.Duration {
	if c.Interval <= 0 {
		return 250 * time.Millisecond
	}
	return c.Interval
}

func (c Consumer) log() *slog.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return slog.Default()
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

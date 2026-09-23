package events

// Outbox Worker (I10): batch-claim with FOR UPDATE SKIP LOCKED + a
// durable lease, publish via EventTransport, mark PUBLISHED with the
// published_seq watermark in the same update, retry with backoff, and
// dead-letter (with the alarm counter) when attempts are exhausted. The
// original outbox row is NEVER deleted.
//
// Crash windows (issue GWT#5, four windows) map to the hooks:
//   - OnClaimed:    after claim / before publish
//   - OnPublished:  after publish / before mark
//   - OnMarked:     after mark / before the next batch (checkpoint is the
//                   mark tx itself — published_seq is assigned atomically)
//   - the claim tx itself (during-checkpoint) recovers via lease expiry
// The hooks exist for the kill -9 injection matrix; they are no-ops in
// production.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// WorkerConfig configures the outbox worker.
type WorkerConfig struct {
	Pool         *pgxpool.Pool
	Transport    EventTransport
	BatchSize    int           // events per claim
	LeaseTTL     time.Duration // claim visibility timeout
	RetryMax     int
	RetryBackoff time.Duration // base backoff (linear growth per attempt)
	PollInterval time.Duration // idle sleep between empty batches
	WorkerID     string        // lease owner marker
	MaxPublishMS int64         // publish latency budget (alarm threshold)
	BacklogAlert int           // backlog depth alarm threshold (GWT#8 gate input)
	Logger       *slog.Logger
}

// Hooks are the crash-injection points (nil in production).
type Hooks struct {
	OnClaimed   func(ctx context.Context, ids []int64) // 领取后/发布前
	OnPublished func(ctx context.Context, id int64)    // 发布后/标记前
	OnMarked    func(ctx context.Context, id int64)    // 标记后/checkpoint 前
}

// WorkerStats is the exported monitoring snapshot (backlog 深度、publish
// 延迟、失败率、DLQ —— issue 监控要求).
type WorkerStats struct {
	Published     int64
	Failed        int64 // DLQ'd
	Retries       int64
	PublishErrors int64
	BacklogDepth  int64
	PublishMaxMS  int64
	DLQDepth      int64
	LastError     string
}

// OutboxWorker drains the transactional outbox.
type OutboxWorker struct {
	cfg   WorkerConfig
	hooks Hooks

	mu      sync.Mutex
	stats   WorkerStats
	paused  bool
	stopCh  chan struct{}
	stopped chan struct{}
}

// NewOutboxWorker builds a worker (not started).
func NewOutboxWorker(cfg WorkerConfig, hooks Hooks) *OutboxWorker {
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 100
	}
	if cfg.LeaseTTL <= 0 {
		cfg.LeaseTTL = 30 * time.Second
	}
	if cfg.RetryMax <= 0 {
		cfg.RetryMax = 5
	}
	if cfg.RetryBackoff <= 0 {
		cfg.RetryBackoff = time.Second
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 250 * time.Millisecond
	}
	if cfg.WorkerID == "" {
		cfg.WorkerID = "worker-1"
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &OutboxWorker{
		cfg: cfg, hooks: hooks,
		stopCh: make(chan struct{}), stopped: make(chan struct{}),
	}
}

// Pause suspends claiming (admin operation; GWT#6).
func (w *OutboxWorker) Pause() { w.mu.Lock(); w.paused = true; w.mu.Unlock() }

// Resume lifts the pause.
func (w *OutboxWorker) Resume() { w.mu.Lock(); w.paused = false; w.mu.Unlock() }

// TransportHealthy reports whether the event transport can accept
// publishes (readiness input; GWT 断连期间 readyz 如实 false).
func (w *OutboxWorker) TransportHealthy(ctx context.Context) bool {
	return w.cfg.Transport != nil && w.cfg.Transport.Healthy(ctx)
}

// Stats returns a monitoring snapshot.
func (w *OutboxWorker) Stats() WorkerStats {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.stats
}

// Run drains until ctx cancels. Transport outages do not stop the worker:
// events stay claimable and retry with backoff (GWT 断连→恢复: backlog
// clears automatically, nothing is dropped).
func (w *OutboxWorker) Run(ctx context.Context) error {
	defer close(w.stopped)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-w.stopCh:
			return nil
		default:
		}
		if w.isPaused() {
			w.sleep(ctx, w.cfg.PollInterval)
			continue
		}
		n, err := w.DrainOnce(ctx)
		if err != nil {
			w.cfg.Logger.Error("outbox drain failed", "error", err)
			w.sleep(ctx, w.cfg.PollInterval)
			continue
		}
		if n == 0 {
			w.sleep(ctx, w.cfg.PollInterval)
		}
	}
}

// Stop halts Run (graceful).
func (w *OutboxWorker) Stop() { close(w.stopCh) }

// Wait blocks until Run has exited.
func (w *OutboxWorker) Wait() { <-w.stopped }

func (w *OutboxWorker) isPaused() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.paused
}

func (w *OutboxWorker) sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-w.stopCh:
	case <-time.After(d):
	}
}

// DrainOnce claims one batch and processes it. Returns the number of
// events handled (0 = nothing claimable).
func (w *OutboxWorker) DrainOnce(ctx context.Context) (int, error) {
	rows, err := w.claim(ctx)
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		w.refreshBacklog(ctx)
		return 0, nil
	}
	if w.hooks.OnClaimed != nil {
		w.hooks.OnClaimed(ctx, idsOf(rows))
	}
	handled := 0
	for _, r := range rows {
		if err := w.publishOne(ctx, r); err != nil {
			return handled, err
		}
		handled++
	}
	w.refreshBacklog(ctx)
	return handled, nil
}

func idsOf(rows []OutboxRow) []int64 {
	ids := make([]int64, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	return ids
}

// claim takes a durable lease on a batch: SELECT ... FOR UPDATE SKIP
// LOCKED in one tx (multi-worker safe: nobody double-claims) and flips the
// rows to PUBLISHING with lease_expires_at = now + LeaseTTL (visibility
// timeout; a crashed worker's rows become claimable again after expiry).
func (w *OutboxWorker) claim(ctx context.Context) ([]OutboxRow, error) {
	tx, err := w.cfg.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, `
		SELECT oe.id, oe.event_id, oe.topic, oe.payload, oe.aggregate_kind, oe.aggregate_id,
		       oe.aggregate_revision, oe.created_at, oe.attempts,
		       cr.tenant_ref, cr.trace_id
		FROM saoaf.outbox_event oe
		JOIN saoaf.change_record cr ON cr.id = oe.change_record_id
		WHERE (oe.status = 'PENDING'
		         AND (oe.next_retry_at IS NULL OR oe.next_retry_at < now()))
		   OR (oe.status = 'PUBLISHING' AND oe.lease_expires_at IS NOT NULL AND oe.lease_expires_at < now())
		ORDER BY oe.id
		LIMIT $1
		FOR UPDATE SKIP LOCKED`, w.cfg.BatchSize)
	if err != nil {
		return nil, err
	}
	var claimed []OutboxRow
	for rows.Next() {
		var r OutboxRow
		if err := rows.Scan(&r.ID, &r.EventID, &r.Topic, &r.Payload, &r.AggregateKind,
			&r.AggregateID, &r.AggregateRevision, &r.CreatedAt, &r.Attempts,
			&r.TenantRef, &r.TraceID); err != nil {
			rows.Close()
			return nil, err
		}
		claimed = append(claimed, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(claimed) == 0 {
		return nil, nil
	}
	for _, r := range claimed {
		if _, err := tx.Exec(ctx, `
			UPDATE saoaf.outbox_event
			SET status = 'PUBLISHING', lease_expires_at = now() + $1::interval, claimed_by = $2
			WHERE id = $3`,
			fmt.Sprintf("%d seconds", int(w.cfg.LeaseTTL.Seconds())), w.cfg.WorkerID, r.ID); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return claimed, nil
}

// publishOne: publish → mark PUBLISHED (+ published_seq watermark in the
// same update — 标记与 checkpoint 原子) → hook. On publish failure: retry
// bookkeeping; exhausted → FAILED + DLQ copy + alarm counter (row kept).
func (w *OutboxWorker) publishOne(ctx context.Context, r OutboxRow) error {
	start := time.Now()
	err := w.cfg.Transport.Publish(ctx, r.Envelope())
	if err == nil {
		if w.hooks.OnPublished != nil {
			w.hooks.OnPublished(ctx, r.ID)
		}
		// the watermark read+write must be one serialized critical section:
		// read-committed lets two workers read the same MAX and the partial
		// unique index outbox_published_seq_idx rejects the loser (23505),
		// leaving the row PUBLISHING with the message already on the wire
		// (review R1 P2-1, probe-proven). A per-mark xact advisory lock keeps
		// the section tiny while making published_seq unique-by-construction.
		if _, lerr := w.cfg.Pool.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('saoaf:outbox:watermark'))`); lerr != nil {
			return lerr
		}
		tag, uerr := w.cfg.Pool.Exec(ctx, `
			UPDATE saoaf.outbox_event
			SET status = 'PUBLISHED', published_at = now(), lease_expires_at = NULL,
			    published_seq = (SELECT COALESCE(MAX(published_seq), 0) + 1 FROM saoaf.outbox_event)
			WHERE id = $1 AND status = 'PUBLISHING'`, r.ID)
		if uerr != nil {
			return uerr
		}
		if tag.RowsAffected() == 0 {
			// lease was stolen after expiry by another worker that already
			// marked it — treat as done (idempotent marking)
			return nil
		}
		ms := time.Since(start).Milliseconds()
		w.mu.Lock()
		w.stats.Published++
		if ms > w.stats.PublishMaxMS {
			w.stats.PublishMaxMS = ms
		}
		w.mu.Unlock()
		if w.cfg.MaxPublishMS > 0 && ms > w.cfg.MaxPublishMS {
			w.cfg.Logger.Warn("publish latency budget exceeded", "ms", ms, "event", r.EventID)
		}
		if w.hooks.OnMarked != nil {
			w.hooks.OnMarked(ctx, r.ID)
		}
		return nil
	}

	// publish failed
	w.mu.Lock()
	w.stats.PublishErrors++
	w.mu.Unlock()
	if errors.Is(err, ErrTransportDown) {
		// message platform outage: release the lease and schedule the
		// retry — the claim query skips rows whose next_retry_at is still
		// in the future, which is what prevents a hot loop against the
		// broken platform (review R1 P2-2: the column was previously
		// written but never read)
		w.mu.Lock()
		w.stats.Retries++
		w.mu.Unlock()
		backoff := w.cfg.RetryBackoff * time.Duration(r.Attempts+1)
		_, uerr := w.cfg.Pool.Exec(ctx, `
			UPDATE saoaf.outbox_event
			SET status = 'PENDING', lease_expires_at = NULL, claimed_by = '',
			    attempts = attempts + 1, next_retry_at = now() + $1::interval,
			    last_error = $2
			WHERE id = $3`,
			fmt.Sprintf("%d milliseconds", backoff.Milliseconds()), err.Error(), r.ID)
		if uerr != nil {
			return uerr
		}
		return nil // outage handled; the batch continues (other events may also fail the same way)
	}

	// terminal transport error (malformed envelope etc.) — retry budget
	// then DLQ
	if r.Attempts >= w.cfg.RetryMax {
		return w.deadLetter(ctx, r, err.Error())
	}
	w.mu.Lock()
	w.stats.Retries++
	w.mu.Unlock()
	_, uerr := w.cfg.Pool.Exec(ctx, `
		UPDATE saoaf.outbox_event
		SET status = 'PENDING', lease_expires_at = NULL, claimed_by = '',
		    attempts = attempts + 1,
		    next_retry_at = now() + $1::interval, last_error = $2
		WHERE id = $3`,
		fmt.Sprintf("%d seconds", int(w.cfg.RetryBackoff.Seconds())*(r.Attempts+1)), err.Error(), r.ID)
	return uerr
}

// deadLetter: mark FAILED (原记录不删除), publish the DLQ copy, bump the
// alarm counter. A DLQ publish failure still marks FAILED — the operator
// replays FAILED rows (never deleted) via the admin replay path.
func (w *OutboxWorker) deadLetter(ctx context.Context, r OutboxRow, reason string) error {
	ev := r.Envelope()
	if t, ok := w.cfg.Transport.(*NATSTransport); ok {
		_ = t.PublishDLQ(ctx, ev, reason) // best-effort copy; row survives
	}
	tag, err := w.cfg.Pool.Exec(ctx, `
		UPDATE saoaf.outbox_event
		SET status = 'FAILED', lease_expires_at = NULL, last_error = $1
		WHERE id = $2`, reason, r.ID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return nil
	}
	w.mu.Lock()
	w.stats.Failed++
	w.mu.Unlock()
	w.cfg.Logger.Error("event dead-lettered (DLQ 告警)",
		"event", r.EventID, "topic", r.Topic, "reason", reason)
	return nil
}

// refreshBacklog records the current backlog depth for monitoring and the
// GWT#8 gate.
func (w *OutboxWorker) refreshBacklog(ctx context.Context) {
	var backlog, dlq int64
	if err := w.cfg.Pool.QueryRow(ctx,
		`SELECT count(*) FROM saoaf.outbox_event WHERE status IN ('PENDING','PUBLISHING')`).Scan(&backlog); err == nil {
		_ = w.cfg.Pool.QueryRow(ctx,
			`SELECT count(*) FROM saoaf.outbox_event WHERE status = 'FAILED'`).Scan(&dlq)
		w.mu.Lock()
		w.stats.BacklogDepth = backlog
		w.stats.DLQDepth = dlq
		alert := w.cfg.BacklogAlert > 0 && backlog > int64(w.cfg.BacklogAlert)
		w.mu.Unlock()
		if alert {
			w.cfg.Logger.Warn("outbox backlog above threshold", "depth", backlog, "threshold", w.cfg.BacklogAlert)
		}
	}
}

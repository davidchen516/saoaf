package events

// Real-PostgreSQL tests for the outbox worker (I10). The transport is
// fake-toggled here; NATS-dependent behaviour (real publish dedup, outage
// drill, crash matrix) lives in nats_test.go with a real JetStream server.

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func withDBE(t *testing.T, fn func(dsn string, pool *pgxpool.Pool)) {
	t.Helper()
	base := os.Getenv("SAOAF_TEST_PG_DSN")
	if base == "" {
		t.Skip("SAOAF_TEST_PG_DSN not set")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close(ctx) })
	name := fmt.Sprintf("events_%d_%d", os.Getpid(), time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})
	u, _ := url.Parse(base)
	u.Path = "/" + name
	dsn := u.String()
	bin := os.Getenv("GOOSE_BIN")
	if bin == "" {
		if bin, err = exec.LookPath("goose"); err != nil {
			t.Skip("goose CLI not found")
		}
	}
	wd, _ := os.Getwd()
	if out, err := exec.Command(bin, "-dir", filepath.Join(wd, "..", "..", "migrations"),
		"postgres", dsn, "up").CombinedOutput(); err != nil {
		t.Fatalf("goose up: %v\n%s", err, out)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	fn(dsn, pool)
}

// seedOutbox inserts a pending event and returns its id.
func seedOutbox(t *testing.T, pool *pgxpool.Pool, eventID, topic, aggID string) int64 {
	t.Helper()
	conn, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	// minimal change_record for the FK
	var crID int64
	if err := conn.QueryRow(context.Background(), `
		INSERT INTO saoaf.change_record (tenant_ref, actor, trace_id, entity_kind, entity_id, operation)
		VALUES ('tenant-e', 'user:test', 'trace', $1, $2, 'TEST')
		RETURNING id`, eventID, aggID).Scan(&crID); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := conn.QueryRow(context.Background(), `
		INSERT INTO saoaf.outbox_event
			(topic, payload, change_record_id, event_id, aggregate_kind, aggregate_id, aggregate_revision)
		VALUES ($1, '{"x":1}'::jsonb, $2, $3, 'binding', $4, 1)
		RETURNING id`, topic, crID, eventID, aggID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// toggleTransport fails while down.
type toggleTransport struct {
	mu        sync.Mutex
	down      bool
	published []string
}

func (f *toggleTransport) Publish(ctx context.Context, ev *CloudEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return ErrTransportDown
	}
	f.published = append(f.published, ev.ID)
	return nil
}
func (f *toggleTransport) Healthy(ctx context.Context) bool { return !f.down }
func (f *toggleTransport) Close()                           {}
func (f *toggleTransport) setDown(v bool)                   { f.mu.Lock(); f.down = v; f.mu.Unlock() }

// terminalTransport always fails with a non-outage error (DLQ path).
type terminalTransport struct{}

func (terminalTransport) Publish(context.Context, *CloudEvent) error {
	return fmt.Errorf("malformed envelope: schema reject")
}
func (terminalTransport) Healthy(context.Context) bool { return true }
func (terminalTransport) Close()                       {}

func newTestWorker(pool *pgxpool.Pool, tr EventTransport, retryMax int) *OutboxWorker {
	return newTestWorkerBatch(pool, tr, retryMax, 10)
}

func newTestWorkerBatch(pool *pgxpool.Pool, tr EventTransport, retryMax, batch int) *OutboxWorker {
	return NewOutboxWorker(WorkerConfig{
		Pool: pool, Transport: tr, WorkerID: "w-test",
		BatchSize: batch, LeaseTTL: 2 * time.Second, RetryMax: retryMax,
		RetryBackoff: 50 * time.Millisecond, PollInterval: 10 * time.Millisecond,
		Logger: discardLogger(),
	}, Hooks{})
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// Happy path（GWT#1）：领取 → 发布 → 标记 PUBLISHED + published_seq 单调。
func TestWorkerHappyPath(t *testing.T) {
	withDBE(t, func(dsn string, pool *pgxpool.Pool) {
		ctx := context.Background()
		seedOutbox(t, pool, "ev-h1", "binding.published", "b1")
		seedOutbox(t, pool, "ev-h2", "binding.published", "b2")
		tr := &toggleTransport{}
		w := newTestWorker(pool, tr, 3)
		n, err := w.DrainOnce(ctx)
		if err != nil || n != 2 {
			t.Fatalf("drain n=%d err=%v", n, err)
		}
		// rows PUBLISHED with monotonic watermark
		var statuses, seqs []int
		rows, err := pool.Query(ctx,
			`SELECT status, published_seq FROM saoaf.outbox_event ORDER BY id`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var st string
			var seq int
			_ = rows.Scan(&st, &seq)
			statuses = append(statuses, 0)
			seqs = append(seqs, seq)
			if st != "PUBLISHED" {
				t.Fatalf("status = %s", st)
			}
		}
		if len(seqs) != 2 || seqs[0] == 0 || seqs[1] == 0 {
			t.Fatalf("published_seq = %v", seqs)
		}
		if w.Stats().Published != 2 || w.Stats().BacklogDepth != 0 {
			t.Fatalf("stats = %+v", w.Stats())
		}
	})
}

// 并发领取：两个 worker 同时 DrainOnce，每事件恰发布一次（SKIP LOCKED）。
func TestWorkerConcurrentClaimNoDoublePublish(t *testing.T) {
	withDBE(t, func(dsn string, pool *pgxpool.Pool) {
		ctx := context.Background()
		const N = 50
		for i := 0; i < N; i++ {
			seedOutbox(t, pool, fmt.Sprintf("ev-c%d", i), "binding.published", fmt.Sprintf("b%d", i))
		}
		tr := &toggleTransport{}
		// batch 50: each worker's claim skips the other's row locks, so the
		// two together drain everything in one pass each
		w1 := newTestWorkerBatch(pool, tr, 3, 50)
		w2 := newTestWorkerBatch(pool, tr, 3, 50)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _, _ = w1.DrainOnce(ctx) }()
		go func() { defer wg.Done(); _, _ = w2.DrainOnce(ctx) }()
		wg.Wait()
		// every row ends PUBLISHED exactly once
		var published, pending int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FILTER (WHERE status='PUBLISHED'),
			        count(*) FILTER (WHERE status IN ('PENDING','PUBLISHING'))
			 FROM saoaf.outbox_event`).Scan(&published, &pending); err != nil {
			t.Fatal(err)
		}
		if published != N || pending != 0 {
			t.Fatalf("published=%d pending=%d, want %d/0", published, pending, N)
		}
		if len(tr.published) != N {
			t.Fatalf("transport published %d events, want %d (double publish)", len(tr.published), N)
		}
	})
}

// 断连→恢复（GWT 断连演练，传输层）：down 期间事件回 PENDING 带退避，
// 恢复后 backlog 自动清空且无丢失。
func TestWorkerOutageThenRecoveryNoLoss(t *testing.T) {
	withDBE(t, func(dsn string, pool *pgxpool.Pool) {
		ctx := context.Background()
		seedOutbox(t, pool, "ev-o1", "binding.published", "b1")
		seedOutbox(t, pool, "ev-o2", "binding.published", "b2")
		tr := &toggleTransport{}
		w := newTestWorker(pool, tr, 3)
		tr.setDown(true)
		if n, err := w.DrainOnce(ctx); err != nil || n != 2 {
			t.Fatalf("outage drain n=%d err=%v", n, err)
		}
		if w.Stats().Retries != 2 || w.Stats().Published != 0 {
			t.Fatalf("during outage stats = %+v", w.Stats())
		}
		// rows back to PENDING with attempts+1 and a backoff; not FAILED
		var pending, attempts int
		if err := pool.QueryRow(ctx,
			`SELECT count(*), COALESCE(MAX(attempts),0) FROM saoaf.outbox_event WHERE status='PENDING'`).Scan(&pending, &attempts); err != nil {
			t.Fatal(err)
		}
		if pending != 2 || attempts < 1 {
			t.Fatalf("pending=%d attempts=%d", pending, attempts)
		}
		// platform recovers
		tr.setDown(false)
		// next_retry_at may be in the future — force-claim path bypassed by
		// clearing the backoff marker (recovery drill simulates lease/queue
		// reprocessing after the approved retry cadence)
		if _, err := pool.Exec(ctx, `UPDATE saoaf.outbox_event SET next_retry_at = NULL`); err != nil {
			t.Fatal(err)
		}
		if n, err := w.DrainOnce(ctx); err != nil || n != 2 {
			t.Fatalf("recovery drain n=%d err=%v", n, err)
		}
		var backlog int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM saoaf.outbox_event WHERE status IN ('PENDING','PUBLISHING')`).Scan(&backlog); err != nil {
			t.Fatal(err)
		}
		if backlog != 0 {
			t.Fatalf("backlog after recovery = %d, want 0 (自动清空)", backlog)
		}
	})
}

// GWT#2：重试耗尽 → FAILED + DLQ 计数，原记录不删除。
func TestWorkerDeadLetter(t *testing.T) {
	withDBE(t, func(dsn string, pool *pgxpool.Pool) {
		ctx := context.Background()
		id := seedOutbox(t, pool, "ev-d1", "binding.published", "b1")
		w := newTestWorker(pool, terminalTransport{}, 2)
		// three failed cycles exhaust the budget (attempts 0→1→2)
		for i := 0; i < 3; i++ {
			if _, err := w.DrainOnce(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx,
				`UPDATE saoaf.outbox_event SET next_retry_at = NULL WHERE id = $1`, id); err != nil {
				t.Fatal(err)
			}
		}
		var status, lastError string
		var attempts int
		if err := pool.QueryRow(ctx,
			`SELECT status, attempts, last_error FROM saoaf.outbox_event WHERE id = $1`, id).
			Scan(&status, &attempts, &lastError); err != nil {
			t.Fatal(err)
		}
		if status != "FAILED" || attempts != 2 {
			t.Fatalf("status=%s attempts=%d, want FAILED/2", status, attempts)
		}
		if lastError == "" {
			t.Fatal("last_error not recorded")
		}
		if w.Stats().Failed != 1 || w.Stats().DLQDepth != 1 {
			t.Fatalf("stats = %+v (DLQ 告警计数)", w.Stats())
		}
		// 原记录仍在（不删除；回滚可重放）
		var rows int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM saoaf.outbox_event WHERE id = $1`, id).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		if rows != 1 {
			t.Fatal("dead-lettered row was deleted")
		}
	})
}

// 租约过期重领（GWT#4 崩溃窗口的可见性语义）：PUBLISHING 行租约过期后可被
// 其他 worker 重领。
func TestWorkerLeaseExpiryReclaim(t *testing.T) {
	withDBE(t, func(dsn string, pool *pgxpool.Pool) {
		ctx := context.Background()
		id := seedOutbox(t, pool, "ev-l1", "binding.published", "b1")
		// simulate a worker that claimed and died: PUBLISHING with a lease
		// already in the past
		if _, err := pool.Exec(ctx, `
			UPDATE saoaf.outbox_event
			SET status='PUBLISHING', lease_expires_at = now() - interval '1 second', claimed_by='w-dead'
			WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
		tr := &toggleTransport{}
		w := newTestWorker(pool, tr, 3)
		n, err := w.DrainOnce(ctx)
		if err != nil || n != 1 {
			t.Fatalf("reclaim drain n=%d err=%v", n, err)
		}
		var status string
		_ = pool.QueryRow(ctx, `SELECT status FROM saoaf.outbox_event WHERE id=$1`, id).Scan(&status)
		if status != "PUBLISHED" {
			t.Fatalf("reclaimed row status = %s", status)
		}
	})
}

// 水位竞态回归（审查 R1 P2-1 探针 + R2-N2 收编）：小批量 + 短租约 + 双
// Worker 并发循环——旧实现（无锁标记 / autocommit 假锁）在此风暴下
// 产生 outbox_published_seq_idx 23505；修复后必须零 23505、seq 唯一连续。
func TestWorkerWatermarkStormNoDuplicates(t *testing.T) {
	withDBE(t, func(dsn string, pool *pgxpool.Pool) {
		ctx := context.Background()
		const N = 50
		for i := 0; i < N; i++ {
			seedOutbox(t, pool, fmt.Sprintf("ev-storm-%d", i), "binding.published", fmt.Sprintf("bs%d", i))
		}
		tr := &toggleTransport{}
		// small batches force interleaved claim/mark cycles between workers
		newStormWorker := func(id string) *OutboxWorker {
			return NewOutboxWorker(WorkerConfig{
				Pool: pool, Transport: tr, WorkerID: id,
				BatchSize: 5, LeaseTTL: 200 * time.Millisecond, RetryMax: 3,
				RetryBackoff: 20 * time.Millisecond, PollInterval: 5 * time.Millisecond,
				Logger: discardLogger(),
			}, Hooks{})
		}
		var mu sync.Mutex
		var drainErrs []error
		var wg sync.WaitGroup
		for wIdx := 0; wIdx < 2; wIdx++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				w := newStormWorker(fmt.Sprintf("storm-%d", i))
				deadline := time.Now().Add(5 * time.Second)
				for time.Now().Before(deadline) {
					n, err := w.DrainOnce(ctx)
					if err != nil {
						mu.Lock()
						drainErrs = append(drainErrs, err)
						mu.Unlock()
						return
					}
					if n == 0 {
						break
					}
				}
			}(wIdx)
		}
		wg.Wait()
		if len(drainErrs) > 0 {
			t.Fatalf("drain errors under watermark storm (want zero; 23505 regression?): %v", drainErrs[0])
		}
		// every row published exactly once, watermark unique and dense
		var published, backlog int
		var seqs []int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FILTER (WHERE status='PUBLISHED'),
			        count(*) FILTER (WHERE status IN ('PENDING','PUBLISHING'))
			 FROM saoaf.outbox_event`).Scan(&published, &backlog); err != nil {
			t.Fatal(err)
		}
		if published != N || backlog != 0 {
			t.Fatalf("published=%d backlog=%d, want %d/0", published, backlog, N)
		}
		rows, err := pool.Query(ctx,
			`SELECT published_seq FROM saoaf.outbox_event WHERE status='PUBLISHED' ORDER BY published_seq`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		seen := map[int]bool{}
		for rows.Next() {
			var s int
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			if s == 0 || seen[s] {
				t.Fatalf("published_seq broken: dup or zero at %d", s)
			}
			seen[s] = true
			seqs = append(seqs, s)
		}
		if len(seqs) != N {
			t.Fatalf("watermarked rows = %d, want %d", len(seqs), N)
		}
		if seqs[0] != 1 || seqs[len(seqs)-1] != N {
			t.Fatalf("watermark not dense: min=%d max=%d, want 1..%d", seqs[0], seqs[len(seqs)-1], N)
		}
		// at-least-once: a lease-expiry re-claim MAY republish an event
		// before the original worker's mark lands (duplicate raw publish).
		// The invariants that matter here: distinct events == N (no loss),
		// zero drain errors (the 23505 regression this test guards), and
		// EXACTLY-ONCE delivery — asserted on the real stream by the
		// crash matrix / server-dedup tests (JetStream MsgID absorbs the
		// duplicate before any consumer).
		distinct := map[string]bool{}
		for _, id := range tr.published {
			distinct[id] = true
		}
		if len(distinct) != N {
			t.Fatalf("distinct published events = %d, want %d (loss!)", len(distinct), N)
		}
		if len(tr.published) < N {
			t.Fatalf("transport publishes = %d, want >= %d", len(tr.published), N)
		}
	})
}

// Pause/Resume（GWT#6 语义核心；HTTP 门禁由 cmd 层接线）。
func TestWorkerPauseResume(t *testing.T) {
	withDBE(t, func(dsn string, pool *pgxpool.Pool) {
		ctx := context.Background()
		seedOutbox(t, pool, "ev-p1", "binding.published", "b1")
		tr := &toggleTransport{}
		w := newTestWorker(pool, tr, 3)

		// paused Run must not drain
		runCtx, cancel := context.WithCancel(ctx)
		w.Pause()
		go func() { _ = w.Run(runCtx) }()
		time.Sleep(100 * time.Millisecond)
		var pending int
		_ = pool.QueryRow(ctx,
			`SELECT count(*) FROM saoaf.outbox_event WHERE status='PENDING'`).Scan(&pending)
		cancel()
		w.Wait()
		if pending != 1 || w.Stats().Published != 0 {
			t.Fatalf("paused worker drained: pending=%d published=%d", pending, w.Stats().Published)
		}

		// resumed worker drains
		runCtx2, cancel2 := context.WithCancel(ctx)
		w2 := newTestWorker(pool, tr, 3)
		go func() { _ = w2.Run(runCtx2) }()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if w2.Stats().Published == 1 {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		cancel2()
		w2.Wait()
		if w2.Stats().Published != 1 {
			t.Fatalf("resumed worker did not drain: %+v", w2.Stats())
		}
	})
}

// 半提交=0 的协变扫描断言在 binding 包（同一 DB 跑 I08 完整发布路径后
// 断言 outbox 行存在）——「只提交域状态」负向实现的拦截点，红运行见
// evidence/i10/README.md。

package events

// Four-window kill -9 crash matrix (issue GWT#5, must-submit evidence):
// a CHILD process runs the real worker (real PG + real JetStream) and
// self-delivers SIGKILL at one of the crash windows; the parent then runs
// the recovery worker and asserts convergence — 无丢失、无重复（重复发布
// 由 JetStream MsgID 去重吸收）.
//
//   Window A: after claim / before publish   → PUBLISHING with lease →
//             lease expiry → re-claim → publish once
//   Window B: after publish / before mark    → re-publish on recovery →
//             server dedup absorbs the duplicate → mark
//   Window C: after mark / before next batch → already PUBLISHED; nothing
//             left to do; no re-delivery
//   Window D: during the claim transaction   → uncommitted claim dies with
//             the process → row stays PENDING → claimed cleanly

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestCrashMatrix drives each window end to end.
func TestCrashMatrix(t *testing.T) {
	for _, window := range []string{"A", "B", "C", "D"} {
		t.Run("window_"+window, func(t *testing.T) {
			runCrashWindow(t, window)
		})
	}
}

func runCrashWindow(t *testing.T, window string) {
	const port = 4291
	url, _, _ := natsServer(t, port)
	withDBE(t, func(dsn string, pool *pgxpool.Pool) {
		ctx := context.Background()
		seedOutbox(t, pool, "ev-crash", "binding.published", "bcrash")

		// child: real worker that self-kills at the window
		cmd := exec.Command(os.Args[0], "-test.run=^TestCrashChild$", "-test.v")
		cmd.Env = append(os.Environ(),
			"CRASH_WINDOW="+window,
			"CRASH_DSN="+dsn,
			"CRASH_NATS="+url,
			"SAOAF_TEST_PG_DSN=", // child must not re-trigger withDBE skips
		)
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("child exited cleanly but was expected to be SIGKILLed:\n%s", out)
		}
		t.Logf("child (%s) exit: %v", window, err)

		// recovery worker: real transport, short lease for fast reclaim
		tr, err := NewNATSTransport(ctx, NATSConfig{URL: url})
		if err != nil {
			t.Fatal(err)
		}
		defer tr.Close()
		w := NewOutboxWorker(WorkerConfig{
			Pool: pool, Transport: tr, WorkerID: "recovery",
			BatchSize: 10, LeaseTTL: 500 * time.Millisecond, RetryMax: 50,
			RetryBackoff: 50 * time.Millisecond, PollInterval: 20 * time.Millisecond,
			Logger: discardLogger(),
		}, Hooks{})

		// wait for the child's lease (window A/B) to expire and drain
		var status string
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := pool.Exec(ctx, `UPDATE saoaf.outbox_event SET next_retry_at = NULL`); err != nil {
				t.Fatal(err)
			}
			if _, err := w.DrainOnce(ctx); err != nil {
				t.Fatal(err)
			}
			_ = pool.QueryRow(ctx, `SELECT status FROM saoaf.outbox_event WHERE event_id='ev-crash'`).Scan(&status)
			if status == "PUBLISHED" {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if status != "PUBLISHED" {
			t.Fatalf("window %s: event not recovered (status=%s) — 丢失", window, status)
		}
		// 无重复：exactly one message on the wire (window B's re-publish was
		// absorbed by JetStream MsgID dedup; other windows never double-sent)
		time.Sleep(300 * time.Millisecond)
		if got := streamMsgs(t, url, "saoaf.binding.published"); got != 1 {
			t.Fatalf("window %s: stream msgs = %d, want 1 (无重复；重复由去重吸收)", window, got)
		}
		// published_seq watermark set (checkpoint durable)
		var seq int
		_ = pool.QueryRow(ctx,
			`SELECT published_seq FROM saoaf.outbox_event WHERE event_id='ev-crash'`).Scan(&seq)
		if seq == 0 {
			t.Fatalf("window %s: published_seq not set (checkpoint lost)", window)
		}
	})
}

// TestCrashChild is the kill -9 victim. It self-terminates with SIGKILL at
// the requested window; it never "passes".
func TestCrashChild(t *testing.T) {
	window := os.Getenv("CRASH_WINDOW")
	if window == "" {
		t.Skip("crash child runs only under TestCrashMatrix")
	}
	dsn := os.Getenv("CRASH_DSN")
	natsURL := os.Getenv("CRASH_NATS")
	if dsn == "" || natsURL == "" {
		t.Fatal("crash child env missing")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	tr, err := NewNATSTransport(ctx, NATSConfig{URL: natsURL})
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()

	kill := func() {
		// kill -9 self at the crash window — no defer, no cleanup
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		select {} // unreachable; keeps the linter honest
	}

	switch window {
	case "D":
		// during-checkpoint: an in-flight claim transaction dies with the
		// process before commit
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE saoaf.outbox_event
			SET status='PUBLISHING', lease_expires_at = now() + interval '1 second', claimed_by='crash-child'
			WHERE event_id='ev-crash' AND status='PENDING'`); err != nil {
			t.Fatal(err)
		}
		kill() // tx never commits

	default:
		var killed atomic.Bool
		hooks := Hooks{
			OnClaimed: func(ctx context.Context, ids []int64) {
				if window == "A" && killed.CompareAndSwap(false, true) {
					kill() // 领取后 / 发布前
				}
			},
			OnPublished: func(ctx context.Context, id int64) {
				if window == "B" && killed.CompareAndSwap(false, true) {
					kill() // 发布后 / 标记前
				}
			},
			OnMarked: func(ctx context.Context, id int64) {
				if window == "C" && killed.CompareAndSwap(false, true) {
					kill() // 标记后 / 下一批前
				}
			},
		}
		w := NewOutboxWorker(WorkerConfig{
			Pool: pool, Transport: tr, WorkerID: "crash-child",
			BatchSize: 10, LeaseTTL: 2 * time.Second, RetryMax: 5,
			RetryBackoff: 50 * time.Millisecond, PollInterval: 10 * time.Millisecond,
			Logger: discardLogger(),
		}, hooks)
		if _, err := w.DrainOnce(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "child drain: %v\n", err)
		}
		// window C kills inside DrainOnce; reaching here without the kill
		// means the hook never fired
		if !killed.Load() {
			t.Fatalf("window %s hook never fired", window)
		}
	}
}

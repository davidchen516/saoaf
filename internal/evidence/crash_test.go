package evidence

// Two-window kill -9 crash matrix (issue GWT#5, must-submit evidence):
// the child process runs the real consumer and self-delivers SIGKILL at
//   Window A: scanned, ingest tx NOT yet committed → replay reprocesses,
//             nothing persisted, idempotent redo from the old checkpoint
//   Window B: ingest tx committed → the rescan after restart hits the
//             event_id unique and absorbs (零重复 Evidence)，checkpoint
//             re-advances
//
// The hooks in consumer.go are the injection points (nil in production).

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestEvidenceCrashMatrix(t *testing.T) {
	for _, window := range []string{"A", "B"} {
		t.Run("window_"+window, func(t *testing.T) {
			withDBE(t, func(dsn string, pool *pgxpool.Pool) {
				ctx := context.Background()
				// a linkable plan target
				if _, err := pool.Exec(ctx, `
					INSERT INTO resolver.resource_plan
						(id, caller_ref, tenant_ref, task_ref, status, fingerprint, request_digest,
						 idempotency_key, created_at, expires_at)
					VALUES ('plan-crash', 'c', 'tenant-ev', 't', 'RESOLVED',
						'sha256:1111111111111111111111111111111111111111111111111111111111111111',
						'sha256:2222222222222222222222222222222222222222222222222222222222222222',
						'k', now(), now() + interval '1 hour')`); err != nil {
					t.Fatal(err)
				}
				publishEvent(t, pool, 1, "plan:plan-crash", "plan.resolved", "plan", "plan-crash",
					[]byte(`{"plan_id":"plan-crash"}`), 0)

				// child: real consumer, SIGKILL at the window hook
				runEvidenceChild(t, dsn, window)

				// recovery: a fresh consumer completes the loop
				ix := Index{Pool: pool}
				c := Consumer{Index: ix, ConsumerID: "recover", BatchSize: 10, Interval: 20 * time.Millisecond}
				runCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				defer cancel()
				go func() { _ = c.Run(runCtx) }()
				deadline := time.Now().Add(4 * time.Second)
				for time.Now().Before(deadline) {
					var linked, total int
					_ = pool.QueryRow(ctx,
						`SELECT count(*) FILTER (WHERE state='LINKED'), count(*) FROM saoaf.evidence_record`).Scan(&linked, &total)
					if total == 1 && linked == 1 {
						cancel()
						// exactly ONE evidence row (幂等吸收重投)
						if total != 1 {
							t.Fatalf("records = %d, want 1", total)
						}
						return
					}
					time.Sleep(30 * time.Millisecond)
				}
				var total int
				_ = pool.QueryRow(ctx, `SELECT count(*) FROM saoaf.evidence_record`).Scan(&total)
				t.Fatalf("window %s: convergence failed (records=%d)", window, total)
			})
		})
	}
}

// runEvidenceChild executes TestEvidenceCrashChild in a subprocess; the
// parent asserts the child died by signal (never a clean exit).
func runEvidenceChild(t *testing.T, dsn, window string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestEvidenceCrashChild$", "-test.v")
	cmd.Env = append(os.Environ(),
		"CRASH_WINDOW_EV="+window,
		"CRASH_DSN_EV="+dsn,
		"SAOAF_TEST_PG_DSN=",
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("child exited cleanly but must be SIGKILLed:\n%s", out)
	}
	// P2-1 (review): a t.Fatal exit(1) would silently degrade this to an
	// ordinary consumption test — the kill must be an actual SIGKILL
	if !strings.Contains(err.Error(), "signal: killed") {
		t.Fatalf("child (%s) did not die by SIGKILL (hook misfired?): %v\n%s", window, err, out)
	}
	t.Logf("child (%s): %v", window, err)
}

func TestEvidenceCrashChild(t *testing.T) {
	window := os.Getenv("CRASH_WINDOW_EV")
	if window == "" {
		t.Skip("crash child runs only under TestEvidenceCrashMatrix")
	}
	dsn := os.Getenv("CRASH_DSN_EV")
	if dsn == "" {
		t.Fatal("crash child env missing")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	ix := Index{Pool: pool}
	var killed atomic.Bool
	kill := func() {
		if killed.CompareAndSwap(false, true) {
			_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		}
		select {}
	}
	c := Consumer{
		Index: ix, ConsumerID: "crash-child", BatchSize: 10,
		Interval: 20 * time.Millisecond,
		OnScanned: func(ctx context.Context, n int) {
			if window == "A" {
				kill() // after scan, BEFORE the ingest tx commits
			}
		},
		OnIngested: func(ctx context.Context, created int) {
			if window == "B" {
				kill() // after commit; checkpoint already in the same tx —
				// the restart rescans from it; here the window proves the
				// loop dies between cycles and the restart converges
			}
		},
	}
	runCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_ = c.Run(runCtx)
}

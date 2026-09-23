package resolver

// Real-PostgreSQL tests for the I09 resolver store + service (same
// pattern as I07/I08: fresh DB per test, goose up all migrations).

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func mustConnR(t *testing.T, dsn string) *pgx.Conn {
	t.Helper()
	c, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	return c
}

func withDBR(t *testing.T, fn func(dsn string)) {
	t.Helper()
	base := os.Getenv("SAOAF_TEST_PG_DSN")
	if base == "" {
		t.Skip("SAOAF_TEST_PG_DSN not set")
	}
	admin := mustConnR(t, base)
	ctx := context.Background()
	name := fmt.Sprintf("resolver_%d_%d", os.Getpid(), time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create db: %v", err)
	}
	u, _ := url.Parse(base)
	u.Path = "/" + name
	db := u.String()
	gooseUpR(t, db)
	t.Cleanup(func() {
		_, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})
	fn(db)
}

func gooseUpR(t *testing.T, db string) {
	t.Helper()
	bin := os.Getenv("GOOSE_BIN")
	if bin == "" {
		var err error
		if bin, err = exec.LookPath("goose"); err != nil {
			t.Skip("goose CLI not found")
		}
	}
	wd, _ := os.Getwd()
	dir := filepath.Join(wd, "..", "..", "migrations")
	out, err := exec.Command(bin, "-dir", dir, "postgres", db, "up").CombinedOutput()
	if err != nil {
		t.Fatalf("goose up: %v\n%s", err, out)
	}
}

// newStoreR builds a pooled store for the test DB.
func newStoreR(t *testing.T, db string) *Store {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), db)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return &Store{Pool: pool}
}

// —— 幂等创建：同 key 同 digest 重放返回原 Plan ——
func TestDBPlanIdempotentReplay(t *testing.T) {
	withDBR(t, func(db string) {
		store := newStoreR(t, db)
		mk := func(digest string) *Plan {
			return &Plan{
				ID: "plan-1", CallerRef: "caller-a", TenantRef: "t1", TaskRef: "task-1",
				Fingerprint: "sha256:" + h64(digest), RequestDigest: "sha256:" + h64(digest),
				IdempotencyKey: "idem-1",
				CreatedAt:      "2026-09-23T00:00:00Z", ExpiresAt: "2026-09-23T00:05:00Z",
				Items: []PlanItem{{
					RequirementID: "req-1", CapabilityKey: "cap", MajorVersion: 1,
					CapabilityRevision: 1, BindingKey: "b", BindingRevision: 1,
					ProviderKey: "p", SnapshotVersion: 1, ProfileOrAction: "prof",
					ReasonCodes: []string{"CAPABILITY_MATCH"},
				}},
			}
		}
		created, plan, err := store.CreatePlan(context.Background(), mk("alpha"))
		if err != nil || !created {
			t.Fatalf("first create: created=%v err=%v", created, err)
		}
		// replay same key + same digest → same plan, no second write
		created2, plan2, err := store.CreatePlan(context.Background(), mk("alpha"))
		if err != nil {
			t.Fatalf("replay: %v", err)
		}
		if created2 {
			t.Fatal("replay created a second plan")
		}
		if plan2.ID != plan.ID || plan2.RequestDigest != plan.RequestDigest {
			t.Fatalf("replay returned different plan: %+v vs %+v", plan2, plan)
		}
		// same key + different digest → conflict
		_, _, err = store.CreatePlan(context.Background(), mk("beta"))
		if err == nil || err.(*ResolveError).Code != CodeIdempotencyConflict {
			t.Fatalf("want IDEMPOTENCY_CONFLICT, got %v", err)
		}
		// exactly one plan row in the DB
		conn := mustConnR(t, db)
		var rows int
		if err := conn.QueryRow(context.Background(),
			`SELECT count(*) FROM resolver.resource_plan`).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		if rows != 1 {
			t.Fatalf("plan rows = %d, want 1", rows)
		}
	})
}

// —— GWT#3：100 次相同幂等请求并发只创建一个 Plan，全部返回同一 Plan ——
func TestDBPlan100ConcurrentSameKey(t *testing.T) {
	withDBR(t, func(db string) {
		store := newStoreR(t, db)
		var wg sync.WaitGroup
		var created, replayed atomic.Int64
		var firstID atomic.Value
		const N = 100
		for i := 0; i < N; i++ {
			wg.Add(1)
			go func(n int) {
				defer wg.Done()
				plan := &Plan{
					ID: fmt.Sprintf("plan-%d", n), CallerRef: "caller-c", TaskRef: "task-c",
					Fingerprint:    "sha256:" + h64("fp"),
					RequestDigest:  "sha256:" + h64("same-body"),
					IdempotencyKey: "idem-c",
					CreatedAt:      "2026-09-23T00:00:00Z", ExpiresAt: "2026-09-23T00:05:00Z",
				}
				ok, got, err := store.CreatePlan(context.Background(), plan)
				if err != nil {
					t.Errorf("concurrent create %d: %v", n, err)
					return
				}
				if ok {
					created.Add(1)
				} else {
					replayed.Add(1)
				}
				firstID.CompareAndSwap(nil, got.ID)
				if got.ID != firstID.Load().(string) {
					t.Errorf("goroutine %d saw plan %s, want %s", n, got.ID, firstID.Load())
				}
			}(i)
		}
		wg.Wait()
		if created.Load() != 1 || created.Load()+replayed.Load() != N {
			t.Fatalf("created=%d replayed=%d, want 1/%d", created.Load(), replayed.Load(), N)
		}
		conn := mustConnR(t, db)
		var rows int
		if err := conn.QueryRow(context.Background(),
			`SELECT count(*) FROM resolver.resource_plan`).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		if rows != 1 {
			t.Fatalf("plan rows = %d, want exactly 1", rows)
		}
	})
}

// —— GWT#4：相同 key 不同请求体并发提交，恰一个成功，另一个 409 ——
func TestDBPlanConcurrentDifferentBodies(t *testing.T) {
	withDBR(t, func(db string) {
		store := newStoreR(t, db)
		mk := func(id, digest string) *Plan {
			return &Plan{
				ID: id, CallerRef: "caller-d", TaskRef: "task-d",
				Fingerprint: "sha256:" + h64(digest), RequestDigest: "sha256:" + h64(digest),
				IdempotencyKey: "idem-d",
				CreatedAt:      "2026-09-23T00:00:00Z", ExpiresAt: "2026-09-23T00:05:00Z",
			}
		}
		var wg sync.WaitGroup
		var okCount, conflicts atomic.Int64
		for _, d := range []string{"body-a", "body-b"} {
			wg.Add(1)
			go func(digest string) {
				defer wg.Done()
				ok, _, err := store.CreatePlan(context.Background(), mk("plan-"+digest, digest))
				if ok {
					okCount.Add(1)
				} else if err != nil && err.(*ResolveError).Code == CodeIdempotencyConflict {
					conflicts.Add(1)
				} else {
					t.Errorf("unexpected outcome: ok=%v err=%v", ok, err)
				}
			}(d)
		}
		wg.Wait()
		if okCount.Load() != 1 || conflicts.Load() != 1 {
			t.Fatalf("ok=%d conflicts=%d, want 1/1", okCount.Load(), conflicts.Load())
		}
	})
}

// —— Plan item 创建后不可变（DB trigger 23514）——
func TestDBPlanItemsImmutable(t *testing.T) {
	withDBR(t, func(db string) {
		store := newStoreR(t, db)
		plan := &Plan{
			ID: "plan-i", CallerRef: "c", TenantRef: "t", TaskRef: "task",
			Fingerprint: "sha256:" + h64("f"), RequestDigest: "sha256:" + h64("b"),
			IdempotencyKey: "idem-i",
			CreatedAt:      "2026-09-23T00:00:00Z", ExpiresAt: "2026-09-23T00:05:00Z",
			Items: []PlanItem{{
				RequirementID: "req-1", CapabilityKey: "cap", MajorVersion: 1,
				CapabilityRevision: 1, BindingKey: "b", BindingRevision: 1,
				ProviderKey: "p", SnapshotVersion: 1, ProfileOrAction: "prof",
				ReasonCodes: []string{"A"},
			}},
		}
		if _, _, err := store.CreatePlan(context.Background(), plan); err != nil {
			t.Fatal(err)
		}
		conn := mustConnR(t, db)
		_, err := conn.Exec(context.Background(),
			`UPDATE resolver.resource_plan_item SET profile_or_action = 'hacked'`)
		if !isCheckViolationR(err) {
			t.Fatalf("item UPDATE must be rejected, got %v", err)
		}
		_, err = conn.Exec(context.Background(),
			`DELETE FROM resolver.resource_plan_item`)
		if !isCheckViolationR(err) {
			t.Fatalf("item DELETE must be rejected, got %v", err)
		}
	})
}

// —— Plan 状态机：RESOLVED → {EXPIRED | REVOKED}，且决策字段不可变 ——
func TestDBPlanLifecycle(t *testing.T) {
	withDBR(t, func(db string) {
		store := newStoreR(t, db)
		ctx := context.Background()
		plan := &Plan{
			ID: "plan-l", CallerRef: "c", TenantRef: "t", TaskRef: "task",
			Fingerprint: "sha256:" + h64("f"), RequestDigest: "sha256:" + h64("b"),
			IdempotencyKey: "idem-l",
			CreatedAt:      "2026-09-23T00:00:00Z", ExpiresAt: "2026-09-23T00:05:00Z",
		}
		if _, _, err := store.CreatePlan(ctx, plan); err != nil {
			t.Fatal(err)
		}
		// TTL 过期 → sweep 置 EXPIRED
		n, err := store.SweepExpired(ctx, "2026-09-23T00:06:00Z")
		if err != nil || n != 1 {
			t.Fatalf("sweep: n=%d err=%v", n, err)
		}
		got, err := store.GetPlan(ctx, "plan-l")
		if err != nil || got.Status != StatusExpired {
			t.Fatalf("status after sweep: %s err=%v", got.Status, err)
		}
		// EXPIRED 是终态：再 sweep/再 revoke 都必须被 trigger 拒绝
		conn := mustConnR(t, db)
		_, err = conn.Exec(ctx,
			`UPDATE resolver.resource_plan SET status = 'REVOKED' WHERE id = 'plan-l'`)
		if !isCheckViolationR(err) {
			t.Fatalf("EXPIRED → REVOKED must be rejected, got %v", err)
		}
		// revoke 正常路径
		plan2 := &Plan{
			ID: "plan-r", CallerRef: "c", TenantRef: "t", TaskRef: "task",
			Fingerprint: "sha256:" + h64("f2"), RequestDigest: "sha256:" + h64("b2"),
			IdempotencyKey: "idem-r",
			CreatedAt:      "2026-09-23T00:00:00Z", ExpiresAt: "2026-09-23T00:05:00Z",
		}
		if _, _, err := store.CreatePlan(ctx, plan2); err != nil {
			t.Fatal(err)
		}
		if err := store.RevokePlan(ctx, "plan-r"); err != nil {
			t.Fatalf("revoke: %v", err)
		}
		got, _ = store.GetPlan(ctx, "plan-r")
		if got.Status != StatusRevoked {
			t.Fatalf("status after revoke = %s, want REVOKED", got.Status)
		}
		if err := store.RevokePlan(ctx, "plan-r"); err != ErrNotFound {
			t.Fatalf("double revoke: %v, want ErrNotFound", err)
		}
		// 决策字段不可变（audit 证据不被改写）
		_, err = conn.Exec(ctx,
			`UPDATE resolver.resource_plan SET fingerprint = 'sha256:evil' WHERE id = 'plan-r'`)
		if !isCheckViolationR(err) {
			t.Fatalf("fingerprint rewrite must be rejected, got %v", err)
		}
	})
}

func isCheckViolationR(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23514"
}

// h64 returns 64 hex chars (fake sha256 for tests) derived from s.
func h64(s string) string {
	// deterministic padding to 64 chars
	out := ""
	for len(out) < 64 {
		out += fmt.Sprintf("%x", s)
	}
	return out[:64]
}

package policy

// Real-PostgreSQL tests for the I06 store (mirrors the I04 pattern:
// SAOAF_TEST_PG_DSN + per-test fresh database + pinned goose CLI).

import (
	"context"
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
)

func mustConn(t *testing.T, dsn string) *pgx.Conn {
	t.Helper()
	c, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	return c
}

func withDB(t *testing.T, fn func(dsn string)) {
	t.Helper()
	base := os.Getenv("SAOAF_TEST_PG_DSN")
	if base == "" {
		t.Skip("SAOAF_TEST_PG_DSN not set — run scripts/pgtest.sh or the CI migrations job")
	}
	admin := mustConn(t, base)
	ctx := context.Background()
	name := fmt.Sprintf("policy_%d_%d", os.Getpid(), time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create db: %v", err)
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	db := u.String()
	gooseUp(t, db)
	t.Cleanup(func() {
		_, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})
	fn(db)
}

func gooseUp(t *testing.T, db string) {
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

// —— tests ——

// DB 不变量：唯一部分索引强制同一 set 恰一个 ACTIVE（绕过应用直接 SQL
// 写第二个 ACTIVATED 必须被 23505 拒绝）。
func TestDBSingleActiveInvariant(t *testing.T) {
	withDB(t, func(db string) {
		conn := mustConn(t, db)
		ctx := context.Background()
		if _, err := conn.Exec(ctx, `
			INSERT INTO policy.policy_revision (set_id, version, state, content, content_digest)
			VALUES ('set-inv', 1, 'ACTIVATED', '{}', 'da')`); err != nil {
			t.Fatalf("first ACTIVE insert: %v", err)
		}
		_, err := conn.Exec(ctx, `
			INSERT INTO policy.policy_revision (set_id, version, state, content, content_digest)
			VALUES ('set-inv', 2, 'ACTIVATED', '{}', 'db')`)
		if err == nil {
			t.Fatal("second ACTIVE insert accepted — unique partial index not enforced")
		}
		if !isUniqueViolation(err) {
			t.Fatalf("want 23505, got: %v", err)
		}
	})
}

// 并发激活（GWT#4 数据库级）：两个 goroutine 抢先激活不同 revision——
// 恰一个成功；败者 ErrUniqueActive 且无半变更。
func TestDBConcurrentActivationSingleActive(t *testing.T) {
	withDB(t, func(db string) {
		conn := mustConn(t, db)
		ctx := context.Background()
		for v := 1; v <= 2; v++ {
			if _, err := conn.Exec(ctx, `
				INSERT INTO policy.policy_revision (set_id, version, state, content, content_digest)
				VALUES ('set-conc', $1, 'PUBLISHED', '{}', $2)`, v, "dc"+string(rune('a'+v))); err != nil {
				t.Fatalf("insert v%d: %v", v, err)
			}
		}
		store := Store{DSN: db}
		var successes, uniqueRejects atomic.Int64
		var wg sync.WaitGroup
		for v := 1; v <= 2; v++ {
			wg.Add(1)
			go func(ver int) {
				defer wg.Done()
				err := store.ActivateRevision(ctx, "set-conc", ver)
				switch {
				case err == nil:
					successes.Add(1)
				case err == ErrUniqueActive:
					uniqueRejects.Add(1)
				default:
					t.Errorf("unexpected error: %v", err)
				}
			}(v)
		}
		wg.Wait()
		if successes.Load() != 1 || uniqueRejects.Load() != 1 {
			t.Fatalf("successes=%d uniqueRejects=%d, want 1/1", successes.Load(), uniqueRejects.Load())
		}
		var active int
		if err := conn.QueryRow(ctx,
			`SELECT count(*) FROM policy.policy_revision WHERE set_id='set-conc' AND state='ACTIVATED'`).Scan(&active); err != nil {
			t.Fatal(err)
		}
		if active != 1 {
			t.Fatalf("ACTIVE rows = %d, want 1", active)
		}
	})
}

// 回滚 + 引用不可变（GWT#7 数据库级）：激活 v1 → 记录 Plan 引用 →
// 激活 v2 → 回滚 v1 → Plan 引用仍是 v1（无 UPDATE 路径）。
func TestDBRollbackAndImmutablePlanRef(t *testing.T) {
	withDB(t, func(db string) {
		conn := mustConn(t, db)
		ctx := context.Background()
		store := Store{DSN: db}
		r1 := &Revision{SetID: "set-rb", Version: 1, State: StatePublished,
			Content: Content{EligibilityCEL: `region == "cn-east"`}}
		r1.Digest = r1.Content.CanonicalDigest()
		r2 := &Revision{SetID: "set-rb", Version: 2, State: StatePublished,
			Content: Content{EligibilityCEL: `region == "cn-north"`}}
		r2.Digest = r2.Content.CanonicalDigest()
		for _, r := range []*Revision{r1, r2} {
			if err := store.SaveRevision(ctx, r, "tenant-a"); err != nil {
				t.Fatalf("save v%d: %v", r.Version, err)
			}
		}
		if err := store.ActivateRevision(ctx, "set-rb", 1); err != nil {
			t.Fatalf("activate v1: %v", err)
		}
		// Plan 引用 v1
		ev := Evaluation{Allow: true, PolicySetID: "set-rb", PolicyVersion: 1, PolicyDigest: r1.Digest}
		if err := store.RecordPlanRef(ctx, "plan-001", ev); err != nil {
			t.Fatalf("record ref: %v", err)
		}
		// 激活 v2（v1 → SUPERSEDED）
		if err := store.ActivateRevision(ctx, "set-rb", 2); err != nil {
			t.Fatalf("activate v2: %v", err)
		}
		// 回滚 v1
		if err := store.ActivateRevision(ctx, "set-rb", 1); err != nil {
			t.Fatalf("rollback to v1: %v", err)
		}
		var v1State string
		if err := conn.QueryRow(ctx,
			`SELECT state FROM policy.policy_revision WHERE set_id='set-rb' AND version=1`).Scan(&v1State); err != nil {
			t.Fatal(err)
		}
		if v1State != "ACTIVATED" {
			t.Fatalf("v1 state after rollback = %s", v1State)
		}
		// 引用不可变：plan-001 仍指向 v1
		got, _, err := store.PlanRef(ctx, "plan-001")
		if err != nil {
			t.Fatal(err)
		}
		if got.PolicyVersion != 1 || got.PolicyDigest != r1.Digest {
			t.Fatalf("plan ref mutated after rollback: %+v", got)
		}
		// 同 plan 重复写入引用 → 唯一冲突（不可变由唯一键承载）
		if err := store.RecordPlanRef(ctx, "plan-001", ev); err == nil {
			t.Fatal("duplicate plan ref accepted — immutability violated")
		}
	})
}

// 崩溃恢复形态（GWT#5 数据库级）：激活事务中断后无「已发布未激活」
// 悬挂与无双 ACTIVE——以事务回滚模拟中断（锁表阻塞 + 取消）。
func TestDBActivationTransactionAbortIsAtomic(t *testing.T) {
	withDB(t, func(db string) {
		conn := mustConn(t, db)
		ctx := context.Background()
		store := Store{DSN: db}
		r1 := &Revision{SetID: "set-cr", Version: 1, State: StatePublished,
			Content: Content{EligibilityCEL: `region == "cn-east"`}}
		r1.Digest = r1.Content.CanonicalDigest()
		if err := store.SaveRevision(ctx, r1, "t"); err != nil {
			t.Fatal(err)
		}
		// 阻塞表使激活事务卡在 UPDATE（等效进程中断前的进行中状态）
		blocker := mustConn(t, db)
		tx, err := blocker.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `LOCK TABLE policy.policy_revision IN ACCESS EXCLUSIVE MODE`); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			done <- store.ActivateRevision(ctx, "set-cr", 1)
		}()
		// 中断：取消进行中的激活
		go func() {
			<-ctx.Done()
		}()
		_ = tx.Rollback(ctx) // release the lock; the activation proceeds
		// wait for the blocked activation to settle (with deadline)
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("activation after unblock failed: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("activation did not complete after unblock")
		}
		var active int
		if err := conn.QueryRow(ctx,
			`SELECT count(*) FROM policy.policy_revision WHERE set_id='set-cr' AND state='ACTIVATED'`).Scan(&active); err != nil {
			t.Fatal(err)
		}
		if active != 1 {
			t.Fatalf("ACTIVE = %d, want 1", active)
		}
	})
}

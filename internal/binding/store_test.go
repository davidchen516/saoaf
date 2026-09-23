package binding

// Real-PostgreSQL tests for the I08 binding store (same pattern as I07).

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
)

func mustConnB(t *testing.T, dsn string) *pgx.Conn {
	t.Helper()
	c, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	return c
}

func withDBB(t *testing.T, fn func(dsn string)) {
	t.Helper()
	base := os.Getenv("SAOAF_TEST_PG_DSN")
	if base == "" {
		t.Skip("SAOAF_TEST_PG_DSN not set")
	}
	admin := mustConnB(t, base)
	ctx := context.Background()
	name := fmt.Sprintf("binding_%d_%d", os.Getpid(), time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create db: %v", err)
	}
	u, _ := url.Parse(base)
	u.Path = "/" + name
	db := u.String()
	gooseUpB(t, db)
	t.Cleanup(func() {
		_, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})
	fn(db)
}

func gooseUpB(t *testing.T, db string) {
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

// seedBinding inserts a DRAFT binding revision and returns its id.
// Requires a capability + provider + snapshot to exist (FK chain from I07 tables).
func seedBinding(t *testing.T, conn *pgx.Conn, key string) {
	t.Helper()
	ctx := context.Background()
	// capability
	if _, err := conn.Exec(ctx, `
		INSERT INTO registry.capability_definition
			(capability_key, major_version, revision, resource_type, requirement_schema, state, owner_ref)
		VALUES ('cap-test', 1, 1, 'MODEL', '{}', 'PUBLISHED', 'user:alice')
		ON CONFLICT DO NOTHING`); err != nil {
		t.Fatalf("seed capability: %v", err)
	}
	// provider
	if _, err := conn.Exec(ctx, `
		INSERT INTO registry.resource_provider
			(provider_key, provider_type, endpoint_ref, owner_ref, workload_identity, state, revision, active_revision)
		VALUES ('prov-test', 'MODEL', 'ref://mmr', 'user:alice', 'spiffe://saoaf.test/ns/default/sa/mmr', 'PUBLISHED', 1, 1)
		ON CONFLICT DO NOTHING`); err != nil {
		t.Fatalf("seed provider: %v", err)
	}
	// snapshot (idempotent: ON CONFLICT, then query the ID)
	var pid int
	if err := conn.QueryRow(ctx,
		`SELECT id FROM registry.resource_provider WHERE provider_key = 'prov-test'`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `
		INSERT INTO registry.provider_snapshot
			(provider_id, snapshot_version, contract_version, digest, signature, workload_identity, generated_at, valid_until)
		VALUES ($1, 1, '2026.09', 'sha256:0000000000000000000000000000000000000000000000000000000000000000', 'sig', 'spiffe://saoaf.test/ns/default/sa/mmr', now(), now() + interval '1 day')
		ON CONFLICT DO NOTHING`, pid); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}
	var snapID int64
	if err := conn.QueryRow(ctx,
		`SELECT id FROM registry.provider_snapshot WHERE provider_id = $1 AND snapshot_version = 1`, pid).Scan(&snapID); err != nil {
		t.Fatal(err)
	}
	// capability id
	var capID int
	if err := conn.QueryRow(ctx,
		`SELECT id FROM registry.capability_definition WHERE capability_key = 'cap-test'`).Scan(&capID); err != nil {
		t.Fatal(err)
	}
	// binding (DRAFT, revision 1, not active)
	if _, err := conn.Exec(ctx, `
		INSERT INTO registry.capability_binding
			(binding_key, capability_id, provider_id, snapshot_id, profile_or_action,
			 environment, scope, scope_hash, priority, state, revision, is_active)
		VALUES ($1, $2, $3, $4, 'reasoning-high-v1', 'production',
			 '{"tenant_refs":["tenant-a"],"factory_refs":[],"regions":["cn-east"],"agent_refs":[]}',
			 $5, 100, 'DRAFT', 1, FALSE)
		ON CONFLICT DO NOTHING`, key, capID, pid, snapID, Scope{TenantRefs: []string{"tenant-a"}, Regions: []string{"cn-east"}}.ScopeHash()); err != nil {
		t.Fatalf("seed binding: %v", err)
	}
}

// —— 50 并发 publish：恰一个 active（issue AC-1）——
func TestDB50ConcurrentPublishSingleActive(t *testing.T) {
	withDBB(t, func(db string) {
		conn := mustConnB(t, db)
		ctx := context.Background()
		seedBinding(t, conn, "bind-conc")
		store := Store{DSN: db}

		scope := Scope{TenantRefs: []string{"tenant-conc"}, Regions: []string{"cn-east"}}
		req := PublishReq{
			BindingKey: "bind-conc", Revision: 1, Scope: scope, Priority: 100,
			Environment: "production", ApprovalRef: "approval:1",
			ChangeReason: "test", TenantRef: "tenant-a",
			Actor: "user:admin", TraceID: "trace-test",
		}
		_ = scope
		b := &Binding{CapabilityID: 1, ProviderID: 1, SnapshotID: 1, Profile: "p1"}

		var successes, conflicts atomic.Int64
		var wg sync.WaitGroup
		const N = 50
		for i := 0; i < N; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				err := store.Publish(ctx, req, b)
				if err == nil {
					successes.Add(1)
				} else {
					conflicts.Add(1)
				}
			}()
		}
		wg.Wait()
		// CAS 语义：恰一个成功（同 expected revision）
		if successes.Load() != 1 {
			t.Fatalf("successes = %d, want exactly 1 (50 concurrent publish)", successes.Load())
		}
		// DB 不变量：恰一个 active revision
		var active int
		if err := conn.QueryRow(ctx,
			`SELECT count(*) FROM registry.capability_binding WHERE binding_key = 'bind-conc' AND is_active`).Scan(&active); err != nil {
			t.Fatal(err)
		}
		if active != 1 {
			t.Fatalf("active rows = %d, want 1", active)
		}
	})
}

// —— scope 冲突：同 scope_hash + 同 priority + PUBLISHED 被拒（AC-3）——
func TestDBScopeConflictBlocked(t *testing.T) {
	withDBB(t, func(db string) {
		conn := mustConnB(t, db)
		ctx := context.Background()
		seedBinding(t, conn, "bind-a")
		store := Store{DSN: db}

		// first publish: activate bind-a with scope {tenant-a, cn-east} priority 100
		scopeA := Scope{TenantRefs: []string{"tenant-a"}, Regions: []string{"cn-east"}}
		reqA := PublishReq{
			BindingKey: "bind-a", Revision: 1, Scope: scopeA, Priority: 100,
			Environment: "production", ApprovalRef: "appr:1",
			ChangeReason: "test", TenantRef: "t",
			Actor: "user:admin", TraceID: "trace-test",
		}
		_ = scopeA
		b := &Binding{CapabilityID: 1, ProviderID: 1, SnapshotID: 1, Profile: "p1"}
		if err := store.Publish(ctx, reqA, b); err != nil {
			t.Fatalf("first publish: %v", err)
		}

		// second binding with same scope + same priority → scope conflict
		seedBinding(t, conn, "bind-b")
		// bind-b needs a different binding_key but same scope + priority
		scopeB := Scope{TenantRefs: []string{"tenant-a"}, Regions: []string{"cn-east"}}
		reqB := PublishReq{
			BindingKey: "bind-b", Revision: 1, Scope: scopeB, Priority: 100,
			Environment: "production", ApprovalRef: "appr:2",
			ChangeReason: "test", TenantRef: "t",
			Actor: "user:admin", TraceID: "trace-test",
		}
		if err := store.Publish(ctx, reqB, b); err == nil {
			t.Fatal("scope conflict accepted")
		} else if !isErr(err, ErrScopeConflict) {
			t.Fatalf("want scope conflict, got %v", err)
		}

		// same scope but DIFFERENT priority → OK
		reqC := PublishReq{
			BindingKey: "bind-b", Revision: 1, Scope: scopeB, Priority: 200,
			Environment: "production", ApprovalRef: "appr:3",
			ChangeReason: "test", TenantRef: "t",
			Actor: "user:admin", TraceID: "trace-test",
		}
		if err := store.Publish(ctx, reqC, b); err != nil {
			t.Fatalf("different priority should succeed: %v", err)
		}
	})
}

// —— 暂停/恢复/退役 全链（AC-4）——
func TestDBSuspendResumeRetire(t *testing.T) {
	withDBB(t, func(db string) {
		conn := mustConnB(t, db)
		ctx := context.Background()
		seedBinding(t, conn, "bind-lc")

		// First: publish the DRAFT binding (seed is DRAFT, revision 1)
		store := Store{DSN: db}
		scope := Scope{TenantRefs: []string{"tenant-lc"}, Regions: []string{"cn-east"}}
		req := PublishReq{
			BindingKey: "bind-lc", Revision: 1, Scope: scope, Priority: 50,
			Environment: "production", ApprovalRef: "appr:lc",
			ChangeReason: "lifecycle test", TenantRef: "t",
			Actor: "user:admin", TraceID: "trace-test",
		}
		b := &Binding{CapabilityID: 1, ProviderID: 1, SnapshotID: 1, Profile: "p1"}
		if err := store.Publish(ctx, req, b); err != nil {
			t.Fatalf("publish: %v", err)
		}
		var rev int
		if err := conn.QueryRow(ctx,
			`SELECT revision FROM registry.capability_binding WHERE binding_key = 'bind-lc' AND is_active`).Scan(&rev); err != nil {
			t.Fatal(err)
		}

		// PUBLISHED → SUSPENDED
		if err := store.Suspend(ctx, TransitionReq{BindingKey: "bind-lc", ExpectedRev: rev,
			Actor: "user:admin", TraceID: "trace-test", ChangeReason: "lc suspend"}); err != nil {
			t.Fatalf("suspend: %v", err)
		}
		rev++
		// SUSPENDED → PUBLISHED (resume)
		if err := store.Resume(ctx, TransitionReq{BindingKey: "bind-lc", ExpectedRev: rev,
			Actor: "user:admin", TraceID: "trace-test"}); err != nil {
			t.Fatalf("resume: %v", err)
		}
		rev++
		// PUBLISHED → SUSPENDED again
		if err := store.Suspend(ctx, TransitionReq{BindingKey: "bind-lc", ExpectedRev: rev,
			Actor: "user:admin", TraceID: "trace-test"}); err != nil {
			t.Fatalf("suspend again: %v", err)
		}
		rev++
		// SUSPENDED → RETIRED
		if err := store.Retire(ctx, TransitionReq{BindingKey: "bind-lc", ExpectedRev: rev,
			Actor: "user:admin", TraceID: "trace-test"}, StateSuspended); err != nil {
			t.Fatalf("retire: %v", err)
		}
		// verify final state
		var state string
		if err := conn.QueryRow(ctx,
			`SELECT state FROM registry.capability_binding WHERE binding_key = 'bind-lc' ORDER BY revision DESC LIMIT 1`).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state != "RETIRED" {
			t.Fatalf("final state = %s, want RETIRED", state)
		}
		// change records linked
		var crCount int
		if err := conn.QueryRow(ctx,
			`SELECT count(*) FROM saoaf.change_record WHERE entity_kind = 'binding' AND entity_id = 'bind-lc'`).Scan(&crCount); err != nil {
			t.Fatal(err)
		}
		if crCount < 1 {
			t.Fatal("no change records linked for binding publishes")
		}
		// I10 半提交=0 协变断言：发布必留同事务 outbox 行。删除 Publish 中的
		// outbox insert（「只提交域状态」负向实现）会在此变红（红运行见
		// evidence/i10/README.md）。
		var outboxCount int
		if err := conn.QueryRow(ctx,
			`SELECT count(*) FROM saoaf.outbox_event
			 WHERE aggregate_kind = 'binding' AND aggregate_id = 'bind-lc'
			   AND event_id = 'binding:bind-lc:2'`).Scan(&outboxCount); err != nil {
			t.Fatal(err)
		}
		if outboxCount != 1 {
			t.Fatalf("outbox rows for the publish = %d, want 1 (half-commit guard)", outboxCount)
		}
		// R2 N-P3.4：retire 事件 topic 显式断言
		var evRetired int
		if err := conn.QueryRow(ctx,
			`SELECT count(*) FROM saoaf.outbox_event WHERE aggregate_id = 'bind-lc' AND topic = 'binding.retired'`).Scan(&evRetired); err != nil {
			t.Fatal(err)
		}
		if evRetired != 1 {
			t.Fatalf("binding.retired outbox events = %d, want 1", evRetired)
		}
	})
}

// —— CAS 冲突：revision 不匹配 → 409 语义错误（AC-2）——
func TestDBCASRevisionConflict(t *testing.T) {
	withDBB(t, func(db string) {
		conn := mustConnB(t, db)
		ctx := context.Background()
		seedBinding(t, conn, "bind-cas")
		store := Store{DSN: db}

		// publish first (seed is DRAFT)
		scope := Scope{TenantRefs: []string{"tenant-cas"}, Regions: []string{"cn-east"}}
		req := PublishReq{
			BindingKey: "bind-cas", Revision: 1, Scope: scope, Priority: 60,
			Environment: "production", ApprovalRef: "appr:cas",
			ChangeReason: "CAS test", TenantRef: "t",
			Actor: "user:admin", TraceID: "trace-test",
		}
		b := &Binding{CapabilityID: 1, ProviderID: 1, SnapshotID: 1, Profile: "p1"}
		if err := store.Publish(ctx, req, b); err != nil {
			t.Fatalf("publish: %v", err)
		}
		// try to suspend with WRONG revision (seed is at 2 after publish, use 99)
		err := store.Suspend(ctx, TransitionReq{BindingKey: "bind-cas", ExpectedRev: 99,
			Actor: "user:admin", TraceID: "trace-test"})
		if err == nil {
			t.Fatal("CAS with wrong revision succeeded")
		}
		if !isErr(err, ErrRevisionConflict) {
			t.Fatalf("want ErrRevisionConflict, got %v", err)
		}
		// state unchanged
		var state string
		if err := conn.QueryRow(ctx,
			`SELECT state FROM registry.capability_binding WHERE binding_key = 'bind-cas' ORDER BY revision DESC LIMIT 1`).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state != "PUBLISHED" {
			t.Fatalf("state drifted after rejected CAS: %s", state)
		}
	})
}

// —— 回滚：新 revision 指向历史内容（GWT#7）——
func TestDBRollbackCreatesNewRevision(t *testing.T) {
	withDBB(t, func(db string) {
		conn := mustConnB(t, db)
		ctx := context.Background()
		seedBinding(t, conn, "bind-rb")
		store := Store{DSN: db}
		b := &Binding{CapabilityID: 1, ProviderID: 1, SnapshotID: 1, Profile: "p1"}

		// v1: scope tenant-rb @ priority 70
		scopeV1 := Scope{TenantRefs: []string{"tenant-rb"}, Regions: []string{"cn-east"}}
		if err := store.Publish(ctx, PublishReq{
			BindingKey: "bind-rb", Revision: 1, Scope: scopeV1, Priority: 70,
			Environment: "production", ApprovalRef: "appr:1",
			ChangeReason: "v1", TenantRef: "t",
			Actor: "user:admin", TraceID: "trace-test",
		}, b); err != nil {
			t.Fatalf("publish v1: %v", err)
		}
		var rev1 int
		if err := conn.QueryRow(ctx,
			`SELECT revision FROM registry.capability_binding WHERE binding_key = 'bind-rb' AND is_active`).Scan(&rev1); err != nil {
			t.Fatal(err)
		}

		// v2: different scope @ different priority
		scopeV2 := Scope{TenantRefs: []string{"tenant-b"}, Regions: []string{"cn-east"}}
		if err := store.Publish(ctx, PublishReq{
			BindingKey: "bind-rb", Revision: rev1, Scope: scopeV2, Priority: 100,
			Environment: "production", ApprovalRef: "appr:2",
			ChangeReason: "v2", TenantRef: "t",
			Actor: "user:admin", TraceID: "trace-test",
		}, b); err != nil {
			t.Fatalf("publish v2: %v", err)
		}
		var rev2 int
		if err := conn.QueryRow(ctx,
			`SELECT revision FROM registry.capability_binding WHERE binding_key = 'bind-rb' AND is_active`).Scan(&rev2); err != nil {
			t.Fatal(err)
		}
		if rev2 != rev1+1 {
			t.Fatalf("v2 revision = %d, want %d (new row per publish, old row untouched)", rev2, rev1+1)
		}

		// rollback = NEW revision carrying v1's EXACT content (same scope
		// AND same priority: the superseded v1 row must not block its own
		// restoration — 旧行只作历史，不参与冲突检测).
		if err := store.Publish(ctx, PublishReq{
			BindingKey: "bind-rb", Revision: rev2, Scope: scopeV1, Priority: 70,
			Environment: "production", ApprovalRef: "appr:3",
			ChangeReason: "rollback to v1", TenantRef: "t",
			Actor: "user:admin", TraceID: "trace-test",
		}, b); err != nil {
			t.Fatalf("rollback publish: %v", err)
		}

		// active revision carries v1's scope_hash and priority
		var activeHash string
		var activePrio int
		if err := conn.QueryRow(ctx, `
			SELECT scope_hash, priority FROM registry.capability_binding
			WHERE binding_key = 'bind-rb' AND is_active`).Scan(&activeHash, &activePrio); err != nil {
			t.Fatal(err)
		}
		if activeHash != scopeV1.ScopeHash() || activePrio != 70 {
			t.Fatalf("rollback content = (%s, %d), want v1 content (%s, 70)",
				activeHash, activePrio, scopeV1.ScopeHash())
		}

		// history preserved: 4 rows (seed + v1 + v2 + rollback), never
		// rewritten or deleted
		var rows int
		if err := conn.QueryRow(ctx,
			`SELECT count(*) FROM registry.capability_binding WHERE binding_key = 'bind-rb'`).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		if rows != 4 {
			t.Fatalf("rows = %d, want 4 (history preserved, not rewritten)", rows)
		}
		var v1Hash string
		var v1Active bool
		if err := conn.QueryRow(ctx, `
			SELECT scope_hash, is_active FROM registry.capability_binding
			WHERE binding_key = 'bind-rb' AND revision = $1`, rev1).Scan(&v1Hash, &v1Active); err != nil {
			t.Fatal(err)
		}
		if v1Hash != scopeV1.ScopeHash() || v1Active {
			t.Fatal("v1 historical revision was mutated or still active")
		}
	})
}

// —— GWT#3 重放幂等：同 idempotency key 重放不产生第二个 active ——
func TestDBPublishIdempotentReplay(t *testing.T) {
	withDBB(t, func(db string) {
		conn := mustConnB(t, db)
		ctx := context.Background()
		seedBinding(t, conn, "bind-idem")
		store := Store{DSN: db}
		b := &Binding{CapabilityID: 1, ProviderID: 1, SnapshotID: 1, Profile: "p1"}

		req := PublishReq{
			BindingKey: "bind-idem", Revision: 1,
			Scope:    Scope{TenantRefs: []string{"tenant-idem"}, Regions: []string{"cn-east"}},
			Priority: 100, Environment: "production",
			ApprovalRef: "appr:idem", ChangeReason: "idem test", TenantRef: "t",
			Actor: "user:admin", TraceID: "trace-test",
			IdempotencyKey: "idem-0001",
		}
		if err := store.Publish(ctx, req, b); err != nil {
			t.Fatalf("first publish: %v", err)
		}

		// exact replay → nil; no second revision, no duplicate outbox event
		if err := store.Publish(ctx, req, b); err != nil {
			t.Fatalf("replay must be idempotent, got: %v", err)
		}
		var rows, active, events, ledger int
		if err := conn.QueryRow(ctx,
			`SELECT count(*) FROM registry.capability_binding WHERE binding_key = 'bind-idem'`).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		if err := conn.QueryRow(ctx,
			`SELECT count(*) FROM registry.capability_binding WHERE binding_key = 'bind-idem' AND is_active`).Scan(&active); err != nil {
			t.Fatal(err)
		}
		if err := conn.QueryRow(ctx,
			`SELECT count(*) FROM saoaf.outbox_event WHERE aggregate_kind = 'binding' AND aggregate_id = 'bind-idem'`).Scan(&events); err != nil {
			t.Fatal(err)
		}
		if err := conn.QueryRow(ctx,
			`SELECT count(*) FROM registry.publish_idempotency WHERE idempotency_key = 'idem-0001'`).Scan(&ledger); err != nil {
			t.Fatal(err)
		}
		if rows != 2 || active != 1 || events != 1 || ledger != 1 {
			t.Fatalf("after replay: rows=%d active=%d events=%d ledger=%d, want 2/1/1/1",
				rows, active, events, ledger)
		}

		// same key + different intent → key reuse (hard error)
		reqAlt := req
		reqAlt.Priority = 200
		if err := store.Publish(ctx, reqAlt, b); !isErr(err, ErrIdemKeyReuse) {
			t.Fatalf("want ErrIdemKeyReuse for key reuse with different intent, got %v", err)
		}

		// different key + same expected revision → CAS conflict (not idempotent)
		reqStale := req
		reqStale.IdempotencyKey = "idem-0002"
		if err := store.Publish(ctx, reqStale, b); !isErr(err, ErrRevisionConflict) {
			t.Fatalf("want ErrRevisionConflict for stale CAS, got %v", err)
		}

		// supersede v1, then replay the old key → conflict (applied, since
		// superseded — NOT a silent idempotent no-op)
		if err := store.Publish(ctx, PublishReq{
			BindingKey: "bind-idem", Revision: 2,
			Scope:    Scope{TenantRefs: []string{"tenant-idem2"}, Regions: []string{"cn-east"}},
			Priority: 150, Environment: "production",
			ApprovalRef: "appr:idem2", ChangeReason: "v2", TenantRef: "t",
			Actor: "user:admin", TraceID: "trace-test",
			IdempotencyKey: "idem-0003",
		}, b); err != nil {
			t.Fatalf("publish v2: %v", err)
		}
		if err := store.Publish(ctx, req, b); !isErr(err, ErrRevisionConflict) {
			t.Fatalf("want ErrRevisionConflict for superseded replay, got %v", err)
		}
	})
}

// —— R1 P1-1 回归：部分重叠（非全等 hash）+ 同优先级必须在发布时阻断 ——
func TestDBPartialOverlapBlocked(t *testing.T) {
	withDBB(t, func(db string) {
		conn := mustConnB(t, db)
		ctx := context.Background()
		seedBinding(t, conn, "bind-ov-a")
		seedBinding(t, conn, "bind-ov-b")
		seedBinding(t, conn, "bind-ov-c")
		store := Store{DSN: db}
		b := &Binding{CapabilityID: 1, ProviderID: 1, SnapshotID: 1, Profile: "p1"}

		pub := func(key string, scope Scope, prio int) error {
			return store.Publish(ctx, PublishReq{
				BindingKey: key, Revision: 1, Scope: scope, Priority: prio,
				Environment: "production", ApprovalRef: "appr:ov",
				ChangeReason: "overlap test", TenantRef: "t",
				Actor: "user:admin", TraceID: "trace-test",
			}, b)
		}

		// A 覆盖 tenants {a,b}
		broad := Scope{TenantRefs: []string{"tenant-a", "tenant-b"}, Regions: []string{"cn-east"}}
		if err := pub("bind-ov-a", broad, 100); err != nil {
			t.Fatalf("publish A: %v", err)
		}

		// 部分重叠 + 同优先级 → 拒绝（审查探针实证旧实现放行双活）
		partial := Scope{TenantRefs: []string{"tenant-a"}, Regions: []string{"cn-east"}}
		if err := pub("bind-ov-b", partial, 100); !isErr(err, ErrScopeConflict) {
			t.Fatalf("partial overlap at same priority must be rejected, got %v", err)
		}
		// 通配（空 = unrestricted）tenant 维同样重叠
		wild := Scope{TenantRefs: []string{}, Regions: []string{"cn-east"}}
		if err := pub("bind-ov-c", wild, 100); !isErr(err, ErrScopeConflict) {
			t.Fatalf("wildcard overlap at same priority must be rejected, got %v", err)
		}
		// 不同优先级 → 允许（优先级是消解重叠的合法手段）
		if err := pub("bind-ov-b", partial, 200); err != nil {
			t.Fatalf("overlapping scope at a different priority must be allowed: %v", err)
		}
		// AND 跨维度：region 不相交且双方非通配 → 不重叠 → 允许
		far := Scope{TenantRefs: []string{"tenant-a"}, Regions: []string{"cn-west"}}
		if err := pub("bind-ov-c", far, 100); err != nil {
			t.Fatalf("disjoint-region scope at same priority must be allowed: %v", err)
		}
	})
}

// —— R1 P1-1 并发闭包：两个部分重叠 scope 并发发布，恰一胜出 ——
// 审查探针曾以并发路径穿透冲突检测；(env, priority) advisory lock 让
// 重叠预检必能看到所有已提交的同槽位兄弟。
func TestDBConcurrentOverlapSingleWinner(t *testing.T) {
	withDBB(t, func(db string) {
		conn := mustConnB(t, db)
		ctx := context.Background()
		seedBinding(t, conn, "bind-race-a")
		seedBinding(t, conn, "bind-race-b")
		store := Store{DSN: db}
		b := &Binding{CapabilityID: 1, ProviderID: 1, SnapshotID: 1, Profile: "p1"}

		scopes := map[string]Scope{
			"bind-race-a": {TenantRefs: []string{"tenant-a", "tenant-b"}, Regions: []string{"cn-east"}},
			"bind-race-b": {TenantRefs: []string{"tenant-b"}, Regions: []string{"cn-east"}},
		}
		var successes, conflicts atomic.Int64
		var wg sync.WaitGroup
		const N = 50
		for i := 0; i < N; i++ {
			key := "bind-race-a"
			if i%2 == 1 {
				key = "bind-race-b"
			}
			wg.Add(1)
			go func(key string) {
				defer wg.Done()
				err := store.Publish(ctx, PublishReq{
					BindingKey: key, Revision: 1, Scope: scopes[key], Priority: 100,
					Environment: "production", ApprovalRef: "appr:race",
					ChangeReason: "race test", TenantRef: "t",
					Actor: "user:admin", TraceID: "trace-test",
				}, b)
				switch {
				case err == nil:
					successes.Add(1)
				case isErr(err, ErrScopeConflict), isErr(err, ErrRevisionConflict):
					// 跨 binding 重叠拒绝 + 同 binding CAS 拒绝都属合法分类
					conflicts.Add(1)
				}
			}(key)
		}
		wg.Wait()
		if successes.Load() != 1 {
			t.Fatalf("successes = %d, want exactly 1 (overlapping scopes, same priority)", successes.Load())
		}
		if successes.Load()+conflicts.Load() != N {
			t.Fatalf("unclassified failures: successes=%d conflicts=%d want total %d", successes.Load(), conflicts.Load(), N)
		}
		var live int
		if err := conn.QueryRow(ctx, `
			SELECT count(*) FROM registry.capability_binding
			WHERE environment = 'production' AND priority = 100
			  AND state = 'PUBLISHED' AND is_active`).Scan(&live); err != nil {
			t.Fatal(err)
		}
		if live != 1 {
			t.Fatalf("live bindings at (production, 100) = %d, want 1", live)
		}
	})
}

// —— R2 N-P1 回归：Resume 重入 live 集合必须过 overlap 裁决 ——
// 复审探针：B={tenant-a}@100 发布→暂停（释放槽位）→ A={tenant-a,tenant-b}@100
// 发布（合法）→ B Resume 旧实现成功 → 双活。修复后 Resume 必须拒绝。
func TestDBResumeOverlapRejected(t *testing.T) {
	withDBB(t, func(db string) {
		conn := mustConnB(t, db)
		ctx := context.Background()
		seedBinding(t, conn, "bind-rg-b")
		seedBinding(t, conn, "bind-rg-a")
		store := Store{DSN: db}
		b := &Binding{CapabilityID: 1, ProviderID: 1, SnapshotID: 1, Profile: "p1"}

		pub := func(key string, scope Scope) error {
			return store.Publish(ctx, PublishReq{
				BindingKey: key, Revision: 1, Scope: scope, Priority: 100,
				Environment: "production", ApprovalRef: "appr:rg",
				ChangeReason: "resume gap test", TenantRef: "t",
				Actor: "user:admin", TraceID: "trace-test",
			}, b)
		}
		// B 发布 → 暂停
		if err := pub("bind-rg-b", Scope{TenantRefs: []string{"tenant-a"}, Regions: []string{"cn-east"}}); err != nil {
			t.Fatalf("publish B: %v", err)
		}
		if err := store.Suspend(ctx, TransitionReq{BindingKey: "bind-rg-b", ExpectedRev: 2,
			Actor: "user:admin", TraceID: "trace-test"}); err != nil {
			t.Fatalf("suspend B: %v", err)
		}
		// A 部分重叠 scope 占用同槽位（合法：B 已暂停释放）
		if err := pub("bind-rg-a", Scope{TenantRefs: []string{"tenant-a", "tenant-b"}, Regions: []string{"cn-east"}}); err != nil {
			t.Fatalf("publish A into released slot: %v", err)
		}
		// B Resume → 必须被 overlap 裁决拒绝（不得双活）
		err := store.Resume(ctx, TransitionReq{BindingKey: "bind-rg-b", ExpectedRev: 3,
			Actor: "user:admin", TraceID: "trace-test"})
		if !isErr(err, ErrScopeConflict) {
			t.Fatalf("resume into overlapping live slot must be ErrScopeConflict, got %v", err)
		}
		// 无漂移：B 仍 SUSPENDED，槽位唯一 live（A）
		if s := mustState(t, conn, "bind-rg-b"); s != "SUSPENDED" {
			t.Fatalf("B drifted after rejected resume: %s", s)
		}
		var live int
		if err := conn.QueryRow(ctx, `
			SELECT count(*) FROM registry.capability_binding
			WHERE environment = 'production' AND priority = 100
			  AND state = 'PUBLISHED' AND is_active`).Scan(&live); err != nil {
			t.Fatal(err)
		}
		if live != 1 {
			t.Fatalf("live bindings at (production, 100) = %d, want 1", live)
		}
	})
}

// —— R2 N-P1 并发闭包：Resume vs Publish 争同一重叠槽位，恰一胜出 ——
func TestDBConcurrentResumeVsPublishSingleWinner(t *testing.T) {
	withDBB(t, func(db string) {
		conn := mustConnB(t, db)
		ctx := context.Background()
		seedBinding(t, conn, "bind-rv-b")
		seedBinding(t, conn, "bind-rv-a")
		store := Store{DSN: db}
		b := &Binding{CapabilityID: 1, ProviderID: 1, SnapshotID: 1, Profile: "p1"}

		if err := store.Publish(ctx, PublishReq{
			BindingKey: "bind-rv-b", Revision: 1,
			Scope:    Scope{TenantRefs: []string{"tenant-a"}, Regions: []string{"cn-east"}},
			Priority: 100, Environment: "production",
			ApprovalRef: "appr:rv", ChangeReason: "race", TenantRef: "t",
			Actor: "user:admin", TraceID: "trace-test",
		}, b); err != nil {
			t.Fatalf("publish B: %v", err)
		}
		if err := store.Suspend(ctx, TransitionReq{BindingKey: "bind-rv-b", ExpectedRev: 2,
			Actor: "user:admin", TraceID: "trace-test"}); err != nil {
			t.Fatalf("suspend B: %v", err)
		}

		var successes, conflicts atomic.Int64
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := store.Resume(ctx, TransitionReq{BindingKey: "bind-rv-b", ExpectedRev: 3,
				Actor: "user:admin", TraceID: "trace-test"}); err == nil {
				successes.Add(1)
			} else if isErr(err, ErrScopeConflict) {
				conflicts.Add(1)
			}
		}()
		go func() {
			defer wg.Done()
			if err := store.Publish(ctx, PublishReq{
				BindingKey: "bind-rv-a", Revision: 1,
				Scope:    Scope{TenantRefs: []string{"tenant-a", "tenant-b"}, Regions: []string{"cn-east"}},
				Priority: 100, Environment: "production",
				ApprovalRef: "appr:rv", ChangeReason: "race", TenantRef: "t",
				Actor: "user:admin", TraceID: "trace-test",
			}, b); err == nil {
				successes.Add(1)
			} else if isErr(err, ErrScopeConflict) {
				conflicts.Add(1)
			}
		}()
		wg.Wait()
		if successes.Load() != 1 || conflicts.Load() != 1 {
			t.Fatalf("resume-vs-publish: successes=%d conflicts=%d, want 1/1", successes.Load(), conflicts.Load())
		}
		var live int
		if err := conn.QueryRow(ctx, `
			SELECT count(*) FROM registry.capability_binding
			WHERE environment = 'production' AND priority = 100
			  AND state = 'PUBLISHED' AND is_active`).Scan(&live); err != nil {
			t.Fatal(err)
		}
		if live != 1 {
			t.Fatalf("live bindings at (production, 100) = %d, want 1", live)
		}
	})
}

// —— R1 P1-2 回归：RETIRED/SUSPENDED/DEPRECATED 不可被 Publish 复活 ——
func TestDBTerminalStatesBlockPublish(t *testing.T) {
	withDBB(t, func(db string) {
		conn := mustConnB(t, db)
		ctx := context.Background()
		for _, key := range []string{"bind-term-r", "bind-term-s", "bind-term-d"} {
			seedBinding(t, conn, key)
		}
		store := Store{DSN: db}
		b := &Binding{CapabilityID: 1, ProviderID: 1, SnapshotID: 1, Profile: "p1"}
		prio := 0

		publish := func(key string, rev int) error {
			prio++
			return store.Publish(ctx, PublishReq{
				BindingKey: key, Revision: rev,
				Scope:    Scope{TenantRefs: []string{"tenant-" + key}, Regions: []string{"cn-east"}},
				Priority: prio, Environment: "production",
				ApprovalRef: "appr:term", ChangeReason: "terminal test", TenantRef: "t",
				Actor: "user:admin", TraceID: "trace-test",
			}, b)
		}
		activeRev := func(key string) (int, string) {
			var rev int
			var state string
			if err := conn.QueryRow(ctx, `
				SELECT revision, state FROM registry.capability_binding
				WHERE binding_key = $1 AND is_active`, key).Scan(&rev, &state); err != nil {
				t.Fatal(err)
			}
			return rev, state
		}
		tr := func(key string, rev int) TransitionReq {
			return TransitionReq{BindingKey: key, ExpectedRev: rev,
				Actor: "user:admin", TraceID: "trace-test"}
		}

		// RETIRED: publish → suspend → retire → revival attempt
		if err := publish("bind-term-r", 1); err != nil {
			t.Fatalf("publish r: %v", err)
		}
		rev, _ := activeRev("bind-term-r")
		if err := store.Suspend(ctx, tr("bind-term-r", rev)); err != nil {
			t.Fatalf("suspend r: %v", err)
		}
		rev, _ = activeRev("bind-term-r")
		if err := store.Retire(ctx, tr("bind-term-r", rev), StateSuspended); err != nil {
			t.Fatalf("retire r: %v", err)
		}
		rev, _ = activeRev("bind-term-r")
		if err := publish("bind-term-r", rev); !isErr(err, ErrInvalidTransition) {
			t.Fatalf("publish over RETIRED must be rejected, got %v", err)
		}
		if s := mustState(t, conn, "bind-term-r"); s != "RETIRED" {
			t.Fatalf("state drifted after rejected revival: %s", s)
		}

		// SUSPENDED: publish 需先 Resume（不得借 publish 绕过状态机）
		if err := publish("bind-term-s", 1); err != nil {
			t.Fatalf("publish s: %v", err)
		}
		rev, _ = activeRev("bind-term-s")
		if err := store.Suspend(ctx, tr("bind-term-s", rev)); err != nil {
			t.Fatalf("suspend s: %v", err)
		}
		rev, _ = activeRev("bind-term-s")
		if err := publish("bind-term-s", rev); !isErr(err, ErrInvalidTransition) {
			t.Fatalf("publish over SUSPENDED must be rejected (Resume first), got %v", err)
		}
		if s := mustState(t, conn, "bind-term-s"); s != "SUSPENDED" {
			t.Fatalf("state drifted: %s", s)
		}

		// DEPRECATED: 同样不可被 publish 覆盖
		if err := publish("bind-term-d", 1); err != nil {
			t.Fatalf("publish d: %v", err)
		}
		rev, _ = activeRev("bind-term-d")
		if err := store.Deprecate(ctx, TransitionReq{BindingKey: "bind-term-d", ExpectedRev: rev,
			Actor: "user:admin", TraceID: "trace-test", ChangeReason: "deprecated"}); err != nil {
			t.Fatalf("deprecate d: %v", err)
		}
		rev, _ = activeRev("bind-term-d")
		if err := publish("bind-term-d", rev); !isErr(err, ErrInvalidTransition) {
			t.Fatalf("publish over DEPRECATED must be rejected, got %v", err)
		}
		// DEPRECATED → RETIRED 走状态机可行
		if err := store.Retire(ctx, tr("bind-term-d", rev), StateDeprecated); err != nil {
			t.Fatalf("retire from DEPRECATED: %v", err)
		}
	})
}

// mustState returns the CURRENT (active) row's state for a binding.
func mustState(t *testing.T, conn *pgx.Conn, key string) string {
	t.Helper()
	var state string
	if err := conn.QueryRow(context.Background(), `
		SELECT state FROM registry.capability_binding
		WHERE binding_key = $1 AND is_active`, key).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}

// —— R1 P2-1/2-2 回归：transition 审计 + 事件；publish 审计字段归因 ——
func TestDBTransitionAuditsAndEvents(t *testing.T) {
	withDBB(t, func(db string) {
		conn := mustConnB(t, db)
		ctx := context.Background()
		seedBinding(t, conn, "bind-aud")
		store := Store{DSN: db}
		b := &Binding{CapabilityID: 1, ProviderID: 1, SnapshotID: 1, Profile: "p1"}

		if err := store.Publish(ctx, PublishReq{
			BindingKey: "bind-aud", Revision: 1,
			Scope:    Scope{TenantRefs: []string{"tenant-aud"}, Regions: []string{"cn-east"}},
			Priority: 90, Environment: "production",
			ApprovalRef: "appr:aud", ChangeReason: "audit test", TenantRef: "t",
			Actor: "user:auditor", TraceID: "trace-aud",
		}, b); err != nil {
			t.Fatalf("publish: %v", err)
		}
		var rev int
		if err := conn.QueryRow(ctx, `
			SELECT revision FROM registry.capability_binding
			WHERE binding_key = 'bind-aud' AND is_active`).Scan(&rev); err != nil {
			t.Fatal(err)
		}

		// P2-2 回归：PUBLISH 审计行的 actor/trace 归因操作者，而非 change reason
		var pubActor, pubTrace string
		if err := conn.QueryRow(ctx, `
			SELECT actor, trace_id FROM saoaf.change_record
			WHERE entity_kind = 'binding' AND entity_id = 'bind-aud' AND operation = 'PUBLISH'`).Scan(&pubActor, &pubTrace); err != nil {
			t.Fatal(err)
		}
		if pubActor != "user:auditor" || pubTrace != "trace-aud" {
			t.Fatalf("PUBLISH audit attribution = (%q, %q), want (user:auditor, trace-aud)", pubActor, pubTrace)
		}

		// transition 落审计 + 事件（P2-1：暂停是生产影响性变更）
		if err := store.Suspend(ctx, TransitionReq{BindingKey: "bind-aud", ExpectedRev: rev,
			Actor: "user:ops", TraceID: "trace-susp", ChangeReason: "maintenance"}); err != nil {
			t.Fatalf("suspend: %v", err)
		}
		var crCount int
		if err := conn.QueryRow(ctx, `
			SELECT count(*) FROM saoaf.change_record
			WHERE entity_kind = 'binding' AND entity_id = 'bind-aud'
			  AND operation = 'SUSPEND' AND actor = 'user:ops' AND trace_id = 'trace-susp'`).Scan(&crCount); err != nil {
			t.Fatal(err)
		}
		if crCount != 1 {
			t.Fatalf("SUSPEND audit rows = %d, want 1 (actor/trace attributed)", crCount)
		}
		var evCount int
		if err := conn.QueryRow(ctx, `
			SELECT count(*) FROM saoaf.outbox_event
			WHERE aggregate_kind = 'binding' AND aggregate_id = 'bind-aud'
			  AND topic = 'binding.suspended'`).Scan(&evCount); err != nil {
			t.Fatal(err)
		}
		if evCount != 1 {
			t.Fatalf("binding.suspended outbox events = %d, want 1", evCount)
		}

		if err := conn.QueryRow(ctx, `
			SELECT revision FROM registry.capability_binding
			WHERE binding_key = 'bind-aud' AND is_active`).Scan(&rev); err != nil {
			t.Fatal(err)
		}
		if err := store.Resume(ctx, TransitionReq{BindingKey: "bind-aud", ExpectedRev: rev,
			Actor: "user:ops", TraceID: "trace-res"}); err != nil {
			t.Fatalf("resume: %v", err)
		}
		if err := conn.QueryRow(ctx, `
			SELECT count(*) FROM saoaf.outbox_event
			WHERE aggregate_id = 'bind-aud' AND topic = 'binding.resumed'`).Scan(&evCount); err != nil {
			t.Fatal(err)
		}
		if evCount != 1 {
			t.Fatalf("binding.resumed outbox events = %d, want 1", evCount)
		}
	})
}

// —— R1 P2-3 回归：Resume 撞他人槽位 → ErrScopeConflict（非裸 23505）——
func TestDBResumeSlotConflictClassified(t *testing.T) {
	withDBB(t, func(db string) {
		conn := mustConnB(t, db)
		ctx := context.Background()
		seedBinding(t, conn, "bind-rs1")
		seedBinding(t, conn, "bind-rs2")
		store := Store{DSN: db}
		b := &Binding{CapabilityID: 1, ProviderID: 1, SnapshotID: 1, Profile: "p1"}
		scope := Scope{TenantRefs: []string{"tenant-rs"}, Regions: []string{"cn-east"}}

		pub := func(key string) error {
			return store.Publish(ctx, PublishReq{
				BindingKey: key, Revision: 1, Scope: scope, Priority: 100,
				Environment: "production", ApprovalRef: "appr:rs",
				ChangeReason: "slot test", TenantRef: "t",
				Actor: "user:admin", TraceID: "trace-test",
			}, b)
		}
		if err := pub("bind-rs1"); err != nil {
			t.Fatalf("publish rs1: %v", err)
		}
		if err := store.Suspend(ctx, TransitionReq{BindingKey: "bind-rs1", ExpectedRev: 2,
			Actor: "user:admin", TraceID: "trace-test"}); err != nil {
			t.Fatalf("suspend rs1: %v", err)
		}
		// 槽位释放后 rs2 可发布同 scope 同优先级
		if err := pub("bind-rs2"); err != nil {
			t.Fatalf("publish rs2 into released slot: %v", err)
		}
		// rs1 Resume → 撞 rs2 槽位：fail-closed 且错误已分类
		err := store.Resume(ctx, TransitionReq{BindingKey: "bind-rs1", ExpectedRev: 3,
			Actor: "user:admin", TraceID: "trace-test"})
		if !isErr(err, ErrScopeConflict) {
			t.Fatalf("resume into taken slot must be ErrScopeConflict, got %v", err)
		}
		// 无漂移：rs1 仍 SUSPENDED，rs2 仍唯一 live
		if s := mustState(t, conn, "bind-rs1"); s != "SUSPENDED" {
			t.Fatalf("rs1 drifted after rejected resume: %s", s)
		}
		var live int
		if err := conn.QueryRow(ctx, `
			SELECT count(*) FROM registry.capability_binding
			WHERE scope_hash = $1 AND environment = 'production' AND priority = 100
			  AND state = 'PUBLISHED' AND is_active`, scope.ScopeHash()).Scan(&live); err != nil {
			t.Fatal(err)
		}
		if live != 1 {
			t.Fatalf("live bindings in slot = %d, want exactly 1", live)
		}
	})
}

// —— R1 P2-4 回归：历史行内容 DB 层不可变 ——
func TestDBHistoricalContentImmutable(t *testing.T) {
	withDBB(t, func(db string) {
		conn := mustConnB(t, db)
		ctx := context.Background()
		seedBinding(t, conn, "bind-im")
		store := Store{DSN: db}
		b := &Binding{CapabilityID: 1, ProviderID: 1, SnapshotID: 1, Profile: "p1"}

		pub := func(rev int, tenants string) error {
			return store.Publish(ctx, PublishReq{
				BindingKey: "bind-im", Revision: rev,
				Scope:    Scope{TenantRefs: []string{tenants}, Regions: []string{"cn-east"}},
				Priority: 80, Environment: "production",
				ApprovalRef: "appr:im", ChangeReason: "immutability test", TenantRef: "t",
				Actor: "user:admin", TraceID: "trace-test",
			}, b)
		}
		if err := pub(1, "tenant-v1"); err != nil {
			t.Fatalf("publish v1: %v", err)
		}
		if err := pub(2, "tenant-v2"); err != nil {
			t.Fatalf("publish v2: %v", err)
		}

		// 历史行（非 active）内容改写 → trigger 拒绝
		_, err := conn.Exec(ctx, `
			UPDATE registry.capability_binding SET scope_hash = 'sha256:evil'
			WHERE binding_key = 'bind-im' AND NOT is_active`)
		if !isCheckViolation(err) {
			t.Fatalf("historical row content rewrite must be rejected, got %v", err)
		}
		// active 行同样拒绝
		_, err = conn.Exec(ctx, `
			UPDATE registry.capability_binding SET priority = 999
			WHERE binding_key = 'bind-im' AND is_active`)
		if !isCheckViolation(err) {
			t.Fatalf("active row content rewrite must be rejected, got %v", err)
		}
		// 生命周期字段（state）仍可按状态机改（由 API 走）
		var rows int
		if err := conn.QueryRow(ctx, `
			SELECT count(*) FROM registry.capability_binding WHERE binding_key = 'bind-im'`).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		if rows != 3 { // seed + v1 + v2
			t.Fatalf("rows = %d, want 3", rows)
		}
	})
}

// —— R1 P2-5：Create / Deprecate / FindOverlapping（影响查询）——
func TestDBCreateDeprecateImpactQuery(t *testing.T) {
	withDBB(t, func(db string) {
		conn := mustConnB(t, db)
		ctx := context.Background()
		seedBinding(t, conn, "cap-seeded") // 仅为了 FK 链：capability/provider/snapshot
		store := Store{DSN: db}

		// Create：DRAFT 行 + 审计
		if err := store.Create(ctx, CreateReq{
			BindingKey: "bind-cr", Environment: "production",
			CapabilityID: 1, ProviderID: 1, SnapshotID: 1, Profile: "reasoning-high-v1",
			Scope:    Scope{TenantRefs: []string{"tenant-cr"}, Regions: []string{"cn-east"}},
			Priority: 100, TenantRef: "t",
			Actor: "user:creator", TraceID: "trace-cr",
		}); err != nil {
			t.Fatalf("create: %v", err)
		}
		var state string
		var active bool
		if err := conn.QueryRow(ctx, `
			SELECT state, is_active FROM registry.capability_binding
			WHERE binding_key = 'bind-cr'`).Scan(&state, &active); err != nil {
			t.Fatal(err)
		}
		if state != "DRAFT" || active {
			t.Fatalf("created row = (%s, active=%v), want (DRAFT, false)", state, active)
		}
		var cr int
		if err := conn.QueryRow(ctx, `
			SELECT count(*) FROM saoaf.change_record
			WHERE entity_kind = 'binding' AND entity_id = 'bind-cr' AND operation = 'CREATE'
			  AND actor = 'user:creator'`).Scan(&cr); err != nil {
			t.Fatal(err)
		}
		if cr != 1 {
			t.Fatalf("CREATE audit rows = %d, want 1", cr)
		}
		// 重复 create → 拒绝（键唯一；其余列合法以命中唯一约束而非 CHECK）
		if err := store.Create(ctx, CreateReq{BindingKey: "bind-cr", Environment: "production",
			CapabilityID: 1, ProviderID: 1, SnapshotID: 1, Profile: "p",
			Scope:    Scope{TenantRefs: []string{"tenant-cr"}, Regions: []string{"cn-east"}},
			Priority: 100, TenantRef: "t",
			Actor: "user:creator"}); !isErr(err, ErrDuplicateActive) {
			t.Fatalf("duplicate create must be rejected, got %v", err)
		}

		// 发布后做影响查询
		b := &Binding{CapabilityID: 1, ProviderID: 1, SnapshotID: 1, Profile: "reasoning-high-v1"}
		if err := store.Publish(ctx, PublishReq{
			BindingKey: "bind-cr", Revision: 1,
			Scope:    Scope{TenantRefs: []string{"tenant-cr"}, Regions: []string{"cn-east"}},
			Priority: 100, Environment: "production",
			ApprovalRef: "appr:cr", ChangeReason: "impact test", TenantRef: "t",
			Actor: "user:admin", TraceID: "trace-test",
		}, b); err != nil {
			t.Fatalf("publish bind-cr: %v", err)
		}
		// 通配 binding（不同优先级允许共存）
		seedBinding(t, conn, "bind-wild")
		if err := store.Publish(ctx, PublishReq{
			BindingKey: "bind-wild", Revision: 1,
			Scope:    Scope{TenantRefs: []string{}, Regions: []string{"cn-east"}},
			Priority: 200, Environment: "production",
			ApprovalRef: "appr:w", ChangeReason: "wildcard", TenantRef: "t",
			Actor: "user:admin", TraceID: "trace-test",
		}, b); err != nil {
			t.Fatalf("publish bind-wild: %v", err)
		}
		// 不相交 binding（同优先级、region 不同 → 不重叠 → 允许）
		seedBinding(t, conn, "bind-far")
		if err := store.Publish(ctx, PublishReq{
			BindingKey: "bind-far", Revision: 1,
			Scope:    Scope{TenantRefs: []string{"tenant-z"}, Regions: []string{"cn-west"}},
			Priority: 100, Environment: "production",
			ApprovalRef: "appr:f", ChangeReason: "far", TenantRef: "t",
			Actor: "user:admin", TraceID: "trace-test",
		}, b); err != nil {
			t.Fatalf("publish bind-far: %v", err)
		}

		got, err := store.FindOverlapping(ctx, "production",
			Scope{TenantRefs: []string{"tenant-cr"}, Regions: []string{"cn-east"}})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || got[0].BindingKey != "bind-cr" || got[0].Priority != 100 ||
			got[1].BindingKey != "bind-wild" || got[1].Priority != 200 {
			t.Fatalf("FindOverlapping = %+v, want [bind-cr(100) bind-wild(200)] ordered by priority", got)
		}

		// Deprecate：PUBLISHED → DEPRECATED（审计 + 事件）
		if err := store.Deprecate(ctx, TransitionReq{BindingKey: "bind-wild", ExpectedRev: 2,
			Actor: "user:admin", TraceID: "trace-test", ChangeReason: "sunset"}); err != nil {
			t.Fatalf("deprecate: %v", err)
		}
		if s := mustState(t, conn, "bind-wild"); s != "DEPRECATED" {
			t.Fatalf("bind-wild state = %s, want DEPRECATED", s)
		}
		// R2 N-P3.4：deprecated 事件 topic 显式断言
		var evDep int
		if err := conn.QueryRow(ctx,
			`SELECT count(*) FROM saoaf.outbox_event WHERE aggregate_id = 'bind-wild' AND topic = 'binding.deprecated'`).Scan(&evDep); err != nil {
			t.Fatal(err)
		}
		if evDep != 1 {
			t.Fatalf("binding.deprecated outbox events = %d, want 1", evDep)
		}
		// DEPRECATED 不再在役：影响查询不再返回它
		got, err = store.FindOverlapping(ctx, "production",
			Scope{TenantRefs: []string{"tenant-cr"}, Regions: []string{"cn-east"}})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].BindingKey != "bind-cr" {
			t.Fatalf("FindOverlapping after deprecate = %+v, want [bind-cr]", got)
		}
	})
}

// isCheckViolation reports a CHECK (23514) failure — the immutability
// trigger's rejection code.
func isCheckViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23514"
}

// —— I10 GWT#8：背压超阈值阻止大批量管理发布；resolve 不受影响 ——
func TestDBBackpressureGate(t *testing.T) {
	withDBB(t, func(db string) {
		conn := mustConnB(t, db)
		ctx := context.Background()
		seedBinding(t, conn, "bind-bp")
		store := Store{DSN: db, MaxBacklog: 3}

		// prime: publish succeeds with an empty backlog
		b := &Binding{CapabilityID: 1, ProviderID: 1, SnapshotID: 1, Profile: "p1"}
		if err := store.Publish(ctx, PublishReq{
			BindingKey: "bind-bp", Revision: 1,
			Scope:    Scope{TenantRefs: []string{"tenant-bp"}, Regions: []string{"cn-east"}},
			Priority: 100, Environment: "production",
			ApprovalRef: "appr:bp", ChangeReason: "bp test", TenantRef: "t",
			Actor: "user:admin", TraceID: "trace-test",
		}, b); err != nil {
			t.Fatalf("publish under threshold: %v", err)
		}

		// flood the outbox past the threshold (simulates a stalled bus)
		var crID int64
		if err := conn.QueryRow(ctx, `
			INSERT INTO saoaf.change_record (tenant_ref, actor, trace_id, entity_kind, entity_id, operation)
			VALUES ('t', 'x', 'x', 'synthetic', 'synthetic', 'SYNTH')
			RETURNING id`).Scan(&crID); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 5; i++ {
			if _, err := conn.Exec(ctx, `
				INSERT INTO saoaf.outbox_event
					(topic, payload, change_record_id, event_id, aggregate_kind, aggregate_id, aggregate_revision)
				VALUES ('synthetic.flood', '{}', $1, $2, 'synthetic', $3, 1)`,
				crID, fmt.Sprintf("flood-%d", i), fmt.Sprintf("s%d", i)); err != nil {
				t.Fatal(err)
			}
		}

		// management publish is blocked fail-closed
		var rev int
		if err := conn.QueryRow(ctx, `
			SELECT revision FROM registry.capability_binding
			WHERE binding_key = 'bind-bp' AND is_active`).Scan(&rev); err != nil {
			t.Fatal(err)
		}
		err := store.Publish(ctx, PublishReq{
			BindingKey: "bind-bp", Revision: rev,
			Scope:    Scope{TenantRefs: []string{"tenant-bp"}, Regions: []string{"cn-east"}},
			Priority: 200, Environment: "production",
			ApprovalRef: "appr:bp2", ChangeReason: "blocked", TenantRef: "t",
			Actor: "user:admin", TraceID: "trace-test",
		}, b)
		if !isErr(err, ErrBacklogBlocked) {
			t.Fatalf("over-threshold publish must be blocked, got %v", err)
		}
		// 已有 Plan 不受影响：既有 active revision 未被破坏
		var state string
		if err := conn.QueryRow(ctx, `
			SELECT state FROM registry.capability_binding
			WHERE binding_key = 'bind-bp' AND is_active`).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state != "PUBLISHED" {
			t.Fatalf("existing active revision damaged: %s", state)
		}
	})
}

func isErr(err, target error) bool {
	if err == nil {
		return false
	}
	te := target.Error()
	ee := err.Error()
	return ee == te || len(ee) > len(te) && ee[:len(te)] == te
}

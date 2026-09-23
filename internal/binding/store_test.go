package binding

// Real-PostgreSQL tests for the I08 binding store (same pattern as I07).

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
		if err := store.Suspend(ctx, "bind-lc", rev); err != nil {
			t.Fatalf("suspend: %v", err)
		}
		rev++
		// SUSPENDED → PUBLISHED (resume)
		if err := store.Resume(ctx, "bind-lc", rev); err != nil {
			t.Fatalf("resume: %v", err)
		}
		rev++
		// PUBLISHED → SUSPENDED again
		if err := store.Suspend(ctx, "bind-lc", rev); err != nil {
			t.Fatalf("suspend again: %v", err)
		}
		rev++
		// SUSPENDED → RETIRED
		if err := store.Retire(ctx, "bind-lc", rev, StateSuspended); err != nil {
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
		}
		b := &Binding{CapabilityID: 1, ProviderID: 1, SnapshotID: 1, Profile: "p1"}
		if err := store.Publish(ctx, req, b); err != nil {
			t.Fatalf("publish: %v", err)
		}
		// try to suspend with WRONG revision (seed is at 2 after publish, use 99)
		err := store.Suspend(ctx, "bind-cas", 99)
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
			IdempotencyKey: "idem-0003",
		}, b); err != nil {
			t.Fatalf("publish v2: %v", err)
		}
		if err := store.Publish(ctx, req, b); !isErr(err, ErrRevisionConflict) {
			t.Fatalf("want ErrRevisionConflict for superseded replay, got %v", err)
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

package binding

// Real-PostgreSQL tests for Store.Republish (I16 hub publish path). The
// republish MUST run the FULL Publish chain — review R3 P1-2 proved a
// reduced "minimal CAS" revived SUSPENDED bindings and skipped
// change_record/outbox/approval_ref/published_at; every test here asserts
// those invariants directly.
import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func republishReq(key string, expected int) RepublishReq {
	return RepublishReq{
		BindingKey: key, ExpectedRev: expected,
		ApprovalRef: "appr:rp", ChangeReason: "hub re-publish",
		TenantRef: "tenant-a", Actor: "user:admin", TraceID: "trace-rp",
	}
}

// DRAFT → PUBLISHED through Republish: full invariants — change record,
// outbox event, approval_ref + published_at persisted (the R3 P1-2 probe
// found all four missing on the minimal-CAS path).
func TestDBRepublishDraftToPublished(t *testing.T) {
	withDBB(t, func(db string) {
		conn := mustConnB(t, db)
		ctx := context.Background()
		seedBinding(t, conn, "bind-rp1")
		store := Store{DSN: db}

		res, err := store.Republish(ctx, republishReq("bind-rp1", 1))
		if err != nil {
			t.Fatalf("republish: %v", err)
		}
		if res.Revision != 2 || res.State != StatePublished || res.Replayed {
			t.Fatalf("result = %+v, want {2 PUBLISHED replayed=false}", res)
		}

		// active row carries the approval chain (R3 P1-2 probe: approval_ref
		// was empty and published_at NULL on the bypass path)
		var state, approvalRef, changeReason string
		var publishedAt *time.Time
		if err := conn.QueryRow(ctx, `
			SELECT state, approval_ref, change_reason, published_at
			FROM registry.capability_binding
			WHERE binding_key = 'bind-rp1' AND is_active`).Scan(&state, &approvalRef, &changeReason, &publishedAt); err != nil {
			t.Fatal(err)
		}
		if state != "PUBLISHED" || approvalRef != "appr:rp" || changeReason != "hub re-publish" || publishedAt == nil {
			t.Fatalf("active row: state=%s approval_ref=%q change_reason=%q published_at=%v", state, approvalRef, changeReason, publishedAt)
		}

		// change record + outbox in the same tx (R3 P1-2 probe: both were 0)
		var cr int
		if err := conn.QueryRow(ctx, `
			SELECT count(*) FROM saoaf.change_record
			WHERE entity_kind = 'binding' AND entity_id = 'bind-rp1'
			  AND operation = 'PUBLISH' AND decision_ref = 'appr:rp'`).Scan(&cr); err != nil {
			t.Fatal(err)
		}
		if cr != 1 {
			t.Fatalf("change records = %d, want 1", cr)
		}
		var ob int
		if err := conn.QueryRow(ctx, `
			SELECT count(*) FROM saoaf.outbox_event
			WHERE aggregate_kind = 'binding' AND aggregate_id = 'bind-rp1'
			  AND event_id = 'binding:bind-rp1:2' AND topic = 'binding.published'`).Scan(&ob); err != nil {
			t.Fatal(err)
		}
		if ob != 1 {
			t.Fatalf("outbox events = %d, want 1 (resolver feed)", ob)
		}
	})
}

// Republishing a live PUBLISHED binding supersedes it as a new revision
// (history kept, old row deactivated).
func TestDBRepublishSupersedesPublished(t *testing.T) {
	withDBB(t, func(db string) {
		conn := mustConnB(t, db)
		ctx := context.Background()
		seedBinding(t, conn, "bind-rp2")
		store := Store{DSN: db}

		if _, err := store.Republish(ctx, republishReq("bind-rp2", 1)); err != nil {
			t.Fatalf("first republish: %v", err)
		}
		res, err := store.Republish(ctx, republishReq("bind-rp2", 2))
		if err != nil {
			t.Fatalf("supersede republish: %v", err)
		}
		if res.Revision != 3 || res.State != StatePublished || res.Replayed {
			t.Fatalf("result = %+v, want {3 PUBLISHED}", res)
		}
		var active, total int
		if err := conn.QueryRow(ctx, `
			SELECT count(*) FROM registry.capability_binding
			WHERE binding_key = 'bind-rp2' AND is_active`).Scan(&active); err != nil {
			t.Fatal(err)
		}
		if err := conn.QueryRow(ctx, `
			SELECT count(*) FROM registry.capability_binding
			WHERE binding_key = 'bind-rp2'`).Scan(&total); err != nil {
			t.Fatal(err)
		}
		if active != 1 || total != 3 {
			t.Fatalf("active=%d total=%d, want 1 active of 3 revisions", active, total)
		}
		var cr int
		if err := conn.QueryRow(ctx, `
			SELECT count(*) FROM saoaf.change_record
			WHERE entity_kind = 'binding' AND entity_id = 'bind-rp2'`).Scan(&cr); err != nil {
			t.Fatal(err)
		}
		if cr != 2 {
			t.Fatalf("change records = %d, want 2 (one per publish)", cr)
		}
	})
}

// R3 P1-2 probe: a SUSPENDED binding must NOT be revivable by publish —
// "publish supersedes PUBLISHED only" (the legal path is Resume).
func TestDBRepublishSuspendedRejected(t *testing.T) {
	withDBB(t, func(db string) {
		conn := mustConnB(t, db)
		ctx := context.Background()
		seedBinding(t, conn, "bind-rp3")
		store := Store{DSN: db}

		if _, err := store.Republish(ctx, republishReq("bind-rp3", 1)); err != nil {
			t.Fatalf("initial publish: %v", err)
		}
		if err := store.Suspend(ctx, TransitionReq{BindingKey: "bind-rp3", ExpectedRev: 2,
			Actor: "user:admin", TraceID: "trace-rp", ChangeReason: "suspension"}); err != nil {
			t.Fatalf("suspend: %v", err)
		}

		_, err := store.Republish(ctx, republishReq("bind-rp3", 3))
		if !isErr(err, ErrInvalidTransition) {
			t.Fatalf("SUSPENDED republish: want ErrInvalidTransition, got %v", err)
		}
		// no revival: latest stays rev 3 SUSPENDED, no rev 4, no new audit rows
		var rev int
		var state string
		if err := conn.QueryRow(ctx, `
			SELECT revision, state FROM registry.capability_binding
			WHERE binding_key = 'bind-rp3'
			ORDER BY revision DESC LIMIT 1`).Scan(&rev, &state); err != nil {
			t.Fatal(err)
		}
		if rev != 3 || state != "SUSPENDED" {
			t.Fatalf("latest = rev %d %s, want rev 3 SUSPENDED (no revival)", rev, state)
		}
		var cr, ob int
		if err := conn.QueryRow(ctx, `
			SELECT count(*) FROM saoaf.change_record
			WHERE entity_kind = 'binding' AND entity_id = 'bind-rp3'`).Scan(&cr); err != nil {
			t.Fatal(err)
		}
		if err := conn.QueryRow(ctx, `
			SELECT count(*) FROM saoaf.outbox_event
			WHERE aggregate_kind = 'binding' AND aggregate_id = 'bind-rp3'`).Scan(&ob); err != nil {
			t.Fatal(err)
		}
		if cr != 2 || ob != 2 { // publish + suspend only
			t.Fatalf("change records = %d, outbox = %d; want 2/2 (no audit trail for the rejected attempt)", cr, ob)
		}
	})
}

// RETIRED is terminal: republish must be rejected, not revive.
func TestDBRepublishRetiredRejected(t *testing.T) {
	withDBB(t, func(db string) {
		conn := mustConnB(t, db)
		ctx := context.Background()
		seedBinding(t, conn, "bind-rp4")
		store := Store{DSN: db}

		if _, err := store.Republish(ctx, republishReq("bind-rp4", 1)); err != nil {
			t.Fatalf("initial publish: %v", err)
		}
		if err := store.Suspend(ctx, TransitionReq{BindingKey: "bind-rp4", ExpectedRev: 2,
			Actor: "user:admin", TraceID: "trace-rp", ChangeReason: "suspension"}); err != nil {
			t.Fatalf("suspend: %v", err)
		}
		if err := store.Retire(ctx, TransitionReq{BindingKey: "bind-rp4", ExpectedRev: 3,
			Actor: "user:admin", TraceID: "trace-rp", ChangeReason: "retirement"}, StateSuspended); err != nil {
			t.Fatalf("retire: %v", err)
		}

		_, err := store.Republish(ctx, republishReq("bind-rp4", 4))
		if !isErr(err, ErrInvalidTransition) {
			t.Fatalf("RETIRED republish: want ErrInvalidTransition, got %v", err)
		}
		var state string
		if err := conn.QueryRow(ctx, `
			SELECT state FROM registry.capability_binding
			WHERE binding_key = 'bind-rp4'
			ORDER BY revision DESC LIMIT 1`).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state != "RETIRED" {
			t.Fatalf("latest state = %s, want RETIRED (terminal)", state)
		}
	})
}

// Stale expected revision → CAS conflict (409 at the HTTP surface), never
// an overwrite.
func TestDBRepublishRevisionConflict(t *testing.T) {
	withDBB(t, func(db string) {
		ctx := context.Background()
		conn := mustConnB(t, db)
		_ = conn
		seedBinding(t, conn, "bind-rp5")
		store := Store{DSN: db}

		if _, err := store.Republish(ctx, republishReq("bind-rp5", 1)); err != nil {
			t.Fatalf("initial publish: %v", err)
		}
		// current is rev 2; replaying with the stale expected 1 must conflict
		_, err := store.Republish(ctx, republishReq("bind-rp5", 1))
		if !isErr(err, ErrRevisionConflict) {
			t.Fatalf("stale CAS: want ErrRevisionConflict, got %v", err)
		}
	})
}

func TestDBRepublishNotFound(t *testing.T) {
	withDBB(t, func(db string) {
		conn := mustConnB(t, db)
		_ = conn
		store := Store{DSN: db}
		if _, err := store.Republish(context.Background(), republishReq("no-such-binding", 1)); !isErr(err, ErrNotFound) {
			t.Fatalf("want ErrNotFound, got %v", err)
		}
	})
}

// GWT#3: the idempotency ledger — same key replays without a new revision;
// the same key on a different intent is a hard key-reuse error.
func TestDBRepublishIdempotentReplay(t *testing.T) {
	withDBB(t, func(db string) {
		conn := mustConnB(t, db)
		ctx := context.Background()
		seedBinding(t, conn, "bind-rp6")
		store := Store{DSN: db}

		req := republishReq("bind-rp6", 1)
		req.IdempotencyKey = "idem-rp-1"
		res, err := store.Republish(ctx, req)
		if err != nil {
			t.Fatalf("first publish: %v", err)
		}
		if res.Replayed || res.Revision != 2 {
			t.Fatalf("first = %+v, want fresh publish at rev 2", res)
		}

		// replay the same intent (e.g. double-click retry): no new revision
		res, err = store.Republish(ctx, req)
		if err != nil {
			t.Fatalf("replay: %v", err)
		}
		if !res.Replayed || res.Revision != 2 {
			t.Fatalf("replay = %+v, want {rev 2 replayed}", res)
		}
		var total, cr int
		if err := conn.QueryRow(ctx, `
			SELECT count(*) FROM registry.capability_binding WHERE binding_key = 'bind-rp6'`).Scan(&total); err != nil {
			t.Fatal(err)
		}
		if err := conn.QueryRow(ctx, `
			SELECT count(*) FROM saoaf.change_record
			WHERE entity_kind = 'binding' AND entity_id = 'bind-rp6'`).Scan(&cr); err != nil {
			t.Fatal(err)
		}
		if total != 2 || cr != 1 {
			t.Fatalf("rows=%d change_records=%d after replay, want 2/1 (replay writes nothing)", total, cr)
		}

		// same key on a DIFFERENT intent → key reuse error
		seedBinding(t, conn, "bind-rp7")
		other := republishReq("bind-rp7", 1)
		other.IdempotencyKey = "idem-rp-1"
		if _, err := store.Republish(ctx, other); !isErr(err, ErrIdemKeyReuse) {
			t.Fatalf("key reuse: want ErrIdemKeyReuse, got %v", err)
		}
	})
}

// R3 P2-1: concurrent same-revision republishes — exactly one winner, the
// losers get CLASSIFIED conflict errors (409 at the HTTP surface), never an
// unclassified 500.
func TestDBRepublishConcurrentSingleWinner(t *testing.T) {
	withDBB(t, func(db string) {
		conn := mustConnB(t, db)
		ctx := context.Background()
		seedBinding(t, conn, "bind-rp8")
		store := Store{DSN: db}

		const N = 20
		start := make(chan struct{})
		var successes, classified atomic.Int64
		var wg sync.WaitGroup
		for i := 0; i < N; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, err := store.Republish(ctx, republishReq("bind-rp8", 1))
				switch {
				case err == nil:
					successes.Add(1)
				case isErr(err, ErrRevisionConflict) || isErr(err, ErrDuplicateActive) || isErr(err, ErrScopeConflict):
					classified.Add(1)
				default:
					t.Errorf("unclassified concurrent error: %v", err)
				}
			}()
		}
		close(start)
		wg.Wait()

		if successes.Load() != 1 {
			t.Fatalf("successes = %d, want exactly 1", successes.Load())
		}
		if classified.Load() != N-1 {
			t.Fatalf("classified conflicts = %d, want %d", classified.Load(), N-1)
		}
		var active int
		if err := conn.QueryRow(ctx, `
			SELECT count(*) FROM registry.capability_binding
			WHERE binding_key = 'bind-rp8' AND is_active`).Scan(&active); err != nil {
			t.Fatal(err)
		}
		if active != 1 {
			t.Fatalf("active rows = %d, want 1", active)
		}
	})
}

// The domain publish gate runs inside Republish: missing change reason (or
// approval ref) is a validation rejection, not a store write.
func TestDBRepublishValidationGate(t *testing.T) {
	withDBB(t, func(db string) {
		conn := mustConnB(t, db)
		_ = conn
		seedBinding(t, conn, "bind-rp9")
		store := Store{DSN: db}

		req := republishReq("bind-rp9", 1)
		req.ChangeReason = ""
		if _, err := store.Republish(context.Background(), req); err == nil {
			t.Fatal("missing change_reason accepted")
		}
		req = republishReq("bind-rp9", 1)
		req.ApprovalRef = ""
		if _, err := store.Republish(context.Background(), req); err == nil {
			t.Fatal("missing approval_ref accepted")
		}
	})
}

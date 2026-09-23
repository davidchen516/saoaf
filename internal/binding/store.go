package binding

// pgx-backed store (I08): publish with revision CAS + single-active guard,
// scope conflict detection at publish time, idempotency-key replay ledger,
// transactional outbox event, suspend/resume/retire, rollback as
// new-revision.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var (
	ErrNotFound         = errors.New("binding: not found")
	ErrRevisionConflict = errors.New("binding: revision conflict (CAS)")
	ErrScopeConflict    = errors.New("binding: scope/priority conflict")
	ErrDuplicateActive  = errors.New("binding: already an active revision")
	ErrIdemKeyReuse     = errors.New("binding: idempotency key reuse")
)

// StoreError pairs a sentinel (errors.Is / HTTP mapping) with a domain
// reason code (issue GWT#2: 每类拒绝须带 reason code).
type StoreError struct {
	Sentinel error
	Reason   string
	Msg      string
}

func (e *StoreError) Error() string { return e.Sentinel.Error() + ": " + e.Msg }
func (e *StoreError) Unwrap() error { return e.Sentinel }

func errS(sent error, reason, msg string) error {
	return &StoreError{Sentinel: sent, Reason: reason, Msg: msg}
}

// Store persists bindings.
type Store struct{ DSN string }

// PublishReq carries the publish inputs.
type PublishReq struct {
	BindingKey     string
	Revision       int // expected current revision (CAS)
	Scope          Scope
	Priority       int
	Environment    string
	ApprovalRef    string
	ChangeReason   string
	TicketRef      string
	TenantRef      string
	IdempotencyKey string // optional; same key replays idempotently (GWT#3)
}

// requestFingerprint identifies the publish intent for idempotency-key
// comparison: same key + different fingerprint = key reuse (hard error).
func (r PublishReq) requestFingerprint(b *Binding) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|%d|%s|%d|%d|%d|%s",
		r.BindingKey, r.Scope.ScopeHash(), r.Priority, r.Environment,
		b.CapabilityID, b.ProviderID, b.SnapshotID, b.Profile)
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// Publish atomically: gate checks → idempotency ledger → scope conflict
// pre-check → deactivate old active (CAS, row keeps its revision as
// history) → insert new PUBLISHED active revision → change record +
// outbox event → ledger record. Scope conflict via partial unique index
// on live (PUBLISHED + active) rows only: superseded history must never
// block rollback to identical content.
func (s Store) Publish(ctx context.Context, req PublishReq, b *Binding) error {
	// publish-side gates
	if err := PublishGate(req.ApprovalRef, req.ChangeReason, req.TicketRef); err != nil {
		return err
	}
	if err := CheckScopeFields(mustJSON(req.Scope)); err != nil {
		return err
	}

	conn, err := pgx.Connect(ctx, s.DSN)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// idempotency ledger (GWT#3): same key → return already-applied state
	// instead of a second active revision.
	if req.IdempotencyKey != "" {
		var recBinding, recFP string
		var recRev int
		err := tx.QueryRow(ctx, `
			SELECT binding_key, resulting_revision, request_fingerprint
			FROM registry.publish_idempotency
			WHERE idempotency_key = $1`, req.IdempotencyKey).Scan(&recBinding, &recRev, &recFP)
		if err == nil {
			if recFP != req.requestFingerprint(b) {
				return errS(ErrIdemKeyReuse, ReasonIdemKeyReuse,
					fmt.Sprintf("key %s was used for a different publish intent", req.IdempotencyKey))
			}
			var active bool
			if qerr := tx.QueryRow(ctx, `
				SELECT EXISTS(SELECT 1 FROM registry.capability_binding
					WHERE binding_key = $1 AND revision = $2 AND is_active)`,
				recBinding, recRev).Scan(&active); qerr != nil {
				return qerr
			}
			if active {
				return tx.Commit(ctx) // replay: already in effect, no new revision
			}
			return errS(ErrRevisionConflict, ReasonRevisionConflict,
				fmt.Sprintf("idempotency key %s applied at revision %d, since superseded", req.IdempotencyKey, recRev))
		}
		if err != pgx.ErrNoRows {
			return err
		}
	}

	// scope conflict: same scope_hash + same priority + PUBLISHED + active
	// + same environment on ANOTHER binding. The partial unique index
	// (binding_scope_priority_unique) is the DB 兜底 for the race where two
	// publishes pass this pre-check concurrently.
	scopeHash := req.Scope.ScopeHash()
	var conflicting string
	err = tx.QueryRow(ctx, `
		SELECT binding_key FROM registry.capability_binding
		WHERE scope_hash = $1 AND priority = $2 AND environment = $3
		  AND state = 'PUBLISHED' AND is_active AND binding_key <> $4
		LIMIT 1`, scopeHash, req.Priority, req.Environment, req.BindingKey).Scan(&conflicting)
	if err == nil {
		return errS(ErrScopeConflict, ReasonScopeOverlap,
			fmt.Sprintf("binding %s holds scope_hash %s at priority %d", conflicting, scopeHash, req.Priority))
	}
	if err != pgx.ErrNoRows {
		return err
	}

	// deactivate previous active revision. CAS on revision; the old row
	// KEEPS its revision — historical revisions are immutable records and
	// the new row takes revision+1 (no (binding_key, revision) collision).
	tag, err := tx.Exec(ctx, `
		UPDATE registry.capability_binding
		SET is_active = FALSE
		WHERE binding_key = $1 AND is_active AND revision = $2`,
		req.BindingKey, req.Revision)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		// CAS arbitration against the LATEST row (active or not): publish
		// requires the caller to have seen the current revision.
		var curRev int
		var curActive bool
		qerr := tx.QueryRow(ctx, `
			SELECT revision, is_active FROM registry.capability_binding
			WHERE binding_key = $1
			ORDER BY revision DESC LIMIT 1`, req.BindingKey).Scan(&curRev, &curActive)
		if qerr == pgx.ErrNoRows {
			return errS(ErrNotFound, ReasonNotFound, "binding does not exist")
		}
		if qerr != nil {
			return qerr
		}
		if curRev != req.Revision {
			return errS(ErrRevisionConflict, ReasonRevisionConflict,
				fmt.Sprintf("expected revision %d, latest is %d", req.Revision, curRev))
		}
		// curRev == req.Revision && !curActive → first publish (legal);
		// curActive here is impossible: the UPDATE above would have matched.
	}

	newRev := req.Revision + 1
	scopeJSON, _ := json.Marshal(req.Scope.CanonicalScope())
	_, err = tx.Exec(ctx, `
		INSERT INTO registry.capability_binding
			(binding_key, capability_id, provider_id, snapshot_id, profile_or_action,
			 environment, scope, scope_hash, priority, state, revision, is_active,
			 change_reason, ticket_ref, approval_ref, tenant_ref, published_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'PUBLISHED', $10, TRUE,
				$11, $12, $13, $14, now())`,
		req.BindingKey, b.CapabilityID, b.ProviderID, b.SnapshotID, b.Profile,
		req.Environment, scopeJSON, scopeHash, req.Priority, newRev,
		req.ChangeReason, req.TicketRef, req.ApprovalRef, req.TenantRef)
	if err != nil {
		if c, ok := constraintOf(err); ok {
			switch c {
			case "capability_binding_binding_key_revision_key":
				return errS(ErrRevisionConflict, ReasonRevisionConflict,
					fmt.Sprintf("revision %d already exists for %s", newRev, req.BindingKey))
			case "binding_scope_priority_unique":
				return errS(ErrScopeConflict, ReasonScopeOverlap,
					fmt.Sprintf("live binding already holds scope_hash %s at priority %d", scopeHash, req.Priority))
			case "binding_single_active":
				return errS(ErrDuplicateActive, ReasonAlreadyActive,
					"concurrent publish won the active slot")
			}
		}
		return err
	}

	// link change record (审计关联) and outbox event in the same tx
	// (事件发布：transport 归 I10，事务性落盘在此保证域状态与 outbox 一致).
	var changeID int64
	err = tx.QueryRow(ctx, `
		INSERT INTO saoaf.change_record
			(tenant_ref, actor, trace_id, entity_kind, entity_id, operation, decision_ref, summary)
		VALUES ($1, $2, 'binding-publish', 'binding', $3, 'PUBLISH', $4, $5)
		RETURNING id`,
		req.TenantRef, req.ChangeReason, req.BindingKey, req.ApprovalRef,
		map[string]any{"revision": newRev, "scope_hash": scopeHash}).Scan(&changeID)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(map[string]any{
		"binding_key": req.BindingKey, "revision": newRev,
		"scope_hash": scopeHash, "priority": req.Priority,
		"environment": req.Environment, "state": string(StatePublished),
	})
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO saoaf.outbox_event
			(topic, payload, change_record_id, event_id, aggregate_kind, aggregate_id, aggregate_revision)
		VALUES ('binding.published', $1, $2, $3, 'binding', $4, $5)`,
		payload, changeID, fmt.Sprintf("binding:%s:%d", req.BindingKey, newRev),
		req.BindingKey, newRev)
	if err != nil {
		return err
	}

	// record the idempotency ledger only on the success path (same tx):
	// a later replay with this key returns without writing.
	if req.IdempotencyKey != "" {
		_, err = tx.Exec(ctx, `
			INSERT INTO registry.publish_idempotency
				(idempotency_key, binding_key, resulting_revision, request_fingerprint)
			VALUES ($1, $2, $3, $4)`,
			req.IdempotencyKey, req.BindingKey, newRev, req.requestFingerprint(b))
		if err != nil {
			if isUniqueViolation(err) {
				// same key committed concurrently; the client retry hits
				// the ledger read above and replays cleanly.
				return errS(ErrRevisionConflict, ReasonRevisionConflict,
					"concurrent duplicate idempotency key")
			}
			return err
		}
	}

	return tx.Commit(ctx)
}

// Suspend transitions PUBLISHED → SUSPENDED (rev CAS).
func (s Store) Suspend(ctx context.Context, bindingKey string, expectedRevision int) error {
	return s.transition(ctx, bindingKey, expectedRevision, StatePublished, StateSuspended)
}

// Resume transitions SUSPENDED → PUBLISHED (rev CAS).
func (s Store) Resume(ctx context.Context, bindingKey string, expectedRevision int) error {
	return s.transition(ctx, bindingKey, expectedRevision, StateSuspended, StatePublished)
}

// Retire transitions SUSPENDED/DEPRECATED → RETIRED (rev CAS).
func (s Store) Retire(ctx context.Context, bindingKey string, expectedRevision int, from State) error {
	return s.transition(ctx, bindingKey, expectedRevision, from, StateRetired)
}

func (s Store) transition(ctx context.Context, bindingKey string, expectedRevision int, from, to State) error {
	if !ValidateTransition(from, to) {
		return errV(ReasonInvalidTransition, string(from)+" → "+string(to))
	}
	conn, err := pgx.Connect(ctx, s.DSN)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	// only the CURRENT (active) row transitions; historical rows keep their
	// state forever.
	tag, err := conn.Exec(ctx, `
		UPDATE registry.capability_binding
		SET state = $1, revision = revision + 1
		WHERE binding_key = $2 AND revision = $3 AND state = $4 AND is_active`,
		string(to), bindingKey, expectedRevision, string(from))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		var curRev int
		var curState string
		qerr := conn.QueryRow(ctx, `
			SELECT revision, state FROM registry.capability_binding
			WHERE binding_key = $1
			ORDER BY revision DESC LIMIT 1`, bindingKey).Scan(&curRev, &curState)
		if qerr == pgx.ErrNoRows {
			return errS(ErrNotFound, ReasonNotFound, "binding does not exist")
		}
		if qerr != nil {
			return qerr
		}
		if curRev != expectedRevision {
			return errS(ErrRevisionConflict, ReasonRevisionConflict,
				fmt.Sprintf("expected %d, latest is %d", expectedRevision, curRev))
		}
		return errV(ReasonInvalidTransition, curState+" → "+string(to))
	}
	return nil
}

// constraintOf extracts the constraint name of a unique-violation (23505).
func constraintOf(err error) (string, bool) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return pgErr.ConstraintName, true
	}
	return "", false
}

func isUniqueViolation(err error) bool {
	_, ok := constraintOf(err)
	return ok
}

func mustJSON(s Scope) []byte {
	b, _ := json.Marshal(s)
	return b
}

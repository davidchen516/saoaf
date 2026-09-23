package binding

// pgx-backed store (I08): publish with revision CAS + single-active guard,
// overlap-based scope conflict detection (review R1 P1-1), lifecycle state
// enforcement on publish (R1 P1-2), idempotency-key replay ledger,
// transactional outbox events + audit on every mutation (R1 P2-1/2),
// create/deprecate/impact query (R1 P2-5), suspend/resume/retire, rollback
// as new-revision.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var (
	ErrNotFound          = errors.New("binding: not found")
	ErrRevisionConflict  = errors.New("binding: revision conflict (CAS)")
	ErrScopeConflict     = errors.New("binding: scope/priority conflict")
	ErrDuplicateActive   = errors.New("binding: already an active revision")
	ErrIdemKeyReuse      = errors.New("binding: idempotency key reuse")
	ErrInvalidTransition = errors.New("binding: invalid transition")
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
	Actor          string // audit attribution (R1 P2-2)
	TraceID        string // audit trace linkage (R1 P2-2)
	IdempotencyKey string // optional; same key replays idempotently (GWT#3)
}

// TransitionReq carries the lifecycle-transition inputs (R1 P2-1/2:
// transitions are audited and attributed like publishes).
type TransitionReq struct {
	BindingKey   string
	ExpectedRev  int
	TenantRef    string
	Actor        string
	TraceID      string
	ChangeReason string
}

// CreateReq carries the DRAFT creation inputs (issue Scope: Binding 创建).
type CreateReq struct {
	BindingKey   string
	Environment  string
	CapabilityID int64
	ProviderID   int64
	SnapshotID   int64
	Profile      string
	Scope        Scope
	Priority     int
	TenantRef    string
	Actor        string
	TraceID      string
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

// Publish atomically: gate checks → idempotency ledger → advisory lock on
// (environment, priority) → overlap-based conflict pre-check → deactivate
// old PUBLISHED active (CAS, row keeps its revision as history) → insert
// new PUBLISHED active revision → change record + outbox event → ledger
// record.
//
// Conflict detection (R1 P1-1): scope conflict means OVERLAP (domain
// Overlaps), not scope_hash equality — {tenant-a,tenant-b} and {tenant-a}
// at the same priority must not coexist. The (env, priority) advisory lock
// serializes competing publishes so the pre-check sees every committed
// sibling; binding_scope_priority_unique stays as an exact-duplicate
// backstop.
func (s Store) Publish(ctx context.Context, req PublishReq, b *Binding) error {
	// publish-side gates (approval enforced for ALL environments — stricter
	// than the issue's production-only floor, disclosed in evidence/i08)
	if err := PublishGate(req.ApprovalRef, req.ChangeReason, req.Actor); err != nil {
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

	// serialize competing publishes at the same (environment, priority) so
	// the overlap check below observes every committed sibling (R1 P1-1
	// concurrency-closure; xact-scoped, released on commit/rollback).
	lockKey := "saoaf:binding:conflict:" + req.Environment + ":" + strconv.Itoa(req.Priority)
	if _, err := tx.Exec(ctx, `
		SELECT pg_advisory_xact_lock(hashtext($1))`, lockKey); err != nil {
		return err
	}

	// overlap conflict (R1 P1-1): any OTHER live PUBLISHED binding in the
	// same environment at the same priority whose scope overlaps the
	// request's scope is a conflict. Subsumes the previous exact-hash check.
	rows, err := tx.Query(ctx, `
		SELECT binding_key, scope FROM registry.capability_binding
		WHERE environment = $1 AND priority = $2
		  AND state = 'PUBLISHED' AND is_active AND binding_key <> $3`,
		req.Environment, req.Priority, req.BindingKey)
	if err != nil {
		return err
	}
	defer rows.Close()
	reqScope := req.Scope.CanonicalScope()
	for rows.Next() {
		var otherKey string
		var scopeJSON []byte
		if err := rows.Scan(&otherKey, &scopeJSON); err != nil {
			return err
		}
		var other Scope
		if err := json.Unmarshal(scopeJSON, &other); err != nil {
			return fmt.Errorf("stored scope for %s is not parseable: %w", otherKey, err)
		}
		if reqScope.Overlaps(other.CanonicalScope()) {
			return errS(ErrScopeConflict, ReasonScopeOverlap,
				fmt.Sprintf("binding %s holds an overlapping scope at priority %d in %s",
					otherKey, req.Priority, req.Environment))
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	// deactivate previous PUBLISHED active revision (R1 P1-2: only a
	// PUBLISHED active row may be superseded — RETIRED/SUSPENDED/DEPRECATED
	// cannot be revived or bypassed by publish). CAS on revision; the old
	// row KEEPS its revision — historical revisions are immutable records.
	tag, err := tx.Exec(ctx, `
		UPDATE registry.capability_binding
		SET is_active = FALSE
		WHERE binding_key = $1 AND is_active AND revision = $2 AND state = 'PUBLISHED'`,
		req.BindingKey, req.Revision)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		// CAS arbitration against the LATEST row (active or not): publish
		// requires the caller to have seen the current revision AND the
		// current row to be in a publishable state.
		var curRev int
		var curActive bool
		var curState string
		qerr := tx.QueryRow(ctx, `
			SELECT revision, is_active, state FROM registry.capability_binding
			WHERE binding_key = $1
			ORDER BY revision DESC LIMIT 1`, req.BindingKey).Scan(&curRev, &curActive, &curState)
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
		if curActive {
			// active row in a non-PUBLISHED state cannot be superseded by
			// publish: RETIRED is terminal; SUSPENDED must Resume first;
			// DEPRECATED must go through Retire.
			return errS(ErrInvalidTransition, ReasonInvalidTransition,
				fmt.Sprintf("active revision is %s (publish supersedes PUBLISHED only)", curState))
		}
		if curState != string(StateDraft) {
			// no active row: only a DRAFT latest row can take the first
			// publish (DRAFT → PUBLISHED)
			return errS(ErrInvalidTransition, ReasonInvalidTransition,
				fmt.Sprintf("latest revision is %s without an active row", curState))
		}
	}

	newRev := req.Revision + 1
	scopeJSON, _ := json.Marshal(req.Scope.CanonicalScope())
	scopeHash := req.Scope.ScopeHash()
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
	// actor/trace come from the request (R1 P2-2): audit must attribute the
	// operator and correlate the trace, not the change reason.
	var changeID int64
	err = tx.QueryRow(ctx, `
		INSERT INTO saoaf.change_record
			(tenant_ref, actor, trace_id, entity_kind, entity_id, operation, decision_ref, summary)
		VALUES ($1, $2, $3, 'binding', $4, 'PUBLISH', $5, $6)
		RETURNING id`,
		req.TenantRef, req.Actor, req.TraceID, req.BindingKey, req.ApprovalRef,
		map[string]any{"revision": newRev, "scope_hash": scopeHash, "change_reason": req.ChangeReason}).Scan(&changeID)
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

// Create inserts a DRAFT binding revision (issue Scope: Binding 创建) with
// an audit record. The DRAFT row is the publishable base: no active row, no
// outbox event (nothing is live yet).
func (s Store) Create(ctx context.Context, req CreateReq) error {
	if req.BindingKey == "" || req.Environment == "" || req.Actor == "" {
		return errV(ReasonMissingApproval, "create requires binding_key, environment and actor")
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

	scopeJSON, _ := json.Marshal(req.Scope.CanonicalScope())
	_, err = tx.Exec(ctx, `
		INSERT INTO registry.capability_binding
			(binding_key, capability_id, provider_id, snapshot_id, profile_or_action,
			 environment, scope, scope_hash, priority, state, revision, is_active, tenant_ref)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'DRAFT', 1, FALSE, $10)`,
		req.BindingKey, req.CapabilityID, req.ProviderID, req.SnapshotID,
		req.Profile, req.Environment, scopeJSON, req.Scope.ScopeHash(),
		req.Priority, req.TenantRef)
	if err != nil {
		if isUniqueViolation(err) {
			return errS(ErrDuplicateActive, ReasonAlreadyActive,
				fmt.Sprintf("binding %s already exists", req.BindingKey))
		}
		return err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO saoaf.change_record
			(tenant_ref, actor, trace_id, entity_kind, entity_id, operation, decision_ref, summary)
		VALUES ($1, $2, $3, 'binding', $4, 'CREATE', '', $5)`,
		req.TenantRef, req.Actor, req.TraceID, req.BindingKey,
		map[string]any{"revision": 1, "state": string(StateDraft)})
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Suspend transitions PUBLISHED → SUSPENDED (rev CAS).
func (s Store) Suspend(ctx context.Context, req TransitionReq) error {
	return s.transition(ctx, req, StatePublished, StateSuspended, "SUSPEND", "binding.suspended")
}

// Resume transitions SUSPENDED → PUBLISHED (rev CAS).
func (s Store) Resume(ctx context.Context, req TransitionReq) error {
	return s.transition(ctx, req, StateSuspended, StatePublished, "RESUME", "binding.resumed")
}

// Deprecate transitions PUBLISHED → DEPRECATED (rev CAS).
func (s Store) Deprecate(ctx context.Context, req TransitionReq) error {
	return s.transition(ctx, req, StatePublished, StateDeprecated, "DEPRECATE", "binding.deprecated")
}

// Retire transitions SUSPENDED/DEPRECATED → RETIRED (rev CAS).
func (s Store) Retire(ctx context.Context, req TransitionReq, from State) error {
	return s.transition(ctx, req, from, StateRetired, "RETIRED", "binding.retired")
}

// transition applies a lifecycle change to the CURRENT (active) row only,
// with an audit record and an outbox event in one transaction (R1 P2-1:
// transitions are production-affecting changes — they must show up in the
// event stream I09/I10 consume and in the audit trail).
func (s Store) transition(ctx context.Context, req TransitionReq, from, to State, operation, topic string) error {
	if !ValidateTransition(from, to) {
		return errV(ReasonInvalidTransition, string(from)+" → "+string(to))
	}
	if req.Actor == "" {
		return errV(ReasonMissingApproval, operation+" requires actor for audit attribution")
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

	tag, err := tx.Exec(ctx, `
		UPDATE registry.capability_binding
		SET state = $1, revision = revision + 1
		WHERE binding_key = $2 AND revision = $3 AND state = $4 AND is_active`,
		string(to), req.BindingKey, req.ExpectedRev, string(from))
	if err != nil {
		if c, ok := constraintOf(err); ok {
			switch c {
			case "binding_scope_priority_unique":
				// e.g. Resume while another binding took the released
				// (scope, priority) slot — fail-closed, but classified (R1
				// P2-3).
				return errS(ErrScopeConflict, ReasonScopeOverlap,
					"slot (scope_hash, environment, priority) taken by another live binding")
			case "binding_single_active":
				return errS(ErrDuplicateActive, ReasonAlreadyActive, "concurrent transition won")
			}
		}
		return err
	}
	if tag.RowsAffected() == 0 {
		var curRev int
		var curState string
		qerr := tx.QueryRow(ctx, `
			SELECT revision, state FROM registry.capability_binding
			WHERE binding_key = $1
			ORDER BY revision DESC LIMIT 1`, req.BindingKey).Scan(&curRev, &curState)
		if qerr == pgx.ErrNoRows {
			return errS(ErrNotFound, ReasonNotFound, "binding does not exist")
		}
		if qerr != nil {
			return qerr
		}
		if curRev != req.ExpectedRev {
			return errS(ErrRevisionConflict, ReasonRevisionConflict,
				fmt.Sprintf("expected %d, latest is %d", req.ExpectedRev, curRev))
		}
		return errV(ReasonInvalidTransition, curState+" → "+string(to))
	}

	var newRev int
	var tenantRef, env string
	if err := tx.QueryRow(ctx, `
		SELECT revision, tenant_ref, environment FROM registry.capability_binding
		WHERE binding_key = $1 AND is_active`, req.BindingKey).Scan(&newRev, &tenantRef, &env); err != nil {
		return err
	}

	var changeID int64
	err = tx.QueryRow(ctx, `
		INSERT INTO saoaf.change_record
			(tenant_ref, actor, trace_id, entity_kind, entity_id, operation, decision_ref, summary)
		VALUES ($1, $2, $3, 'binding', $4, $5, '', $6)
		RETURNING id`,
		tenantRef, req.Actor, req.TraceID, req.BindingKey, operation,
		map[string]any{"revision": newRev, "from": string(from), "to": string(to),
			"change_reason": req.ChangeReason}).Scan(&changeID)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(map[string]any{
		"binding_key": req.BindingKey, "revision": newRev,
		"environment": env, "state": string(to),
	})
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO saoaf.outbox_event
			(topic, payload, change_record_id, event_id, aggregate_kind, aggregate_id, aggregate_revision)
		VALUES ($1, $2, $3, $4, 'binding', $5, $6)`,
		topic, payload, changeID,
		fmt.Sprintf("binding:%s:%d:%s", req.BindingKey, newRev, operation),
		req.BindingKey, newRev)
	if err != nil {
		return err
	}

	return tx.Commit(ctx)
}

// StoredBinding is a stored binding revision as returned by queries.
type StoredBinding struct {
	BindingKey string
	Revision   int
	Scope      Scope
	Priority   int
	State      State
	Profile    string
}

// FindOverlapping returns live PUBLISHED bindings in the environment whose
// scope overlaps the query scope, ordered by priority ascending (issue
// Scope: 影响查询; the resolve input for I09).
func (s Store) FindOverlapping(ctx context.Context, environment string, query Scope) ([]StoredBinding, error) {
	conn, err := pgx.Connect(ctx, s.DSN)
	if err != nil {
		return nil, err
	}
	defer conn.Close(ctx)
	rows, err := conn.Query(ctx, `
		SELECT binding_key, revision, scope, priority, profile_or_action
		FROM registry.capability_binding
		WHERE environment = $1 AND state = 'PUBLISHED' AND is_active
		ORDER BY priority ASC`, environment)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	q := query.CanonicalScope()
	var out []StoredBinding
	for rows.Next() {
		var sb StoredBinding
		var scopeJSON []byte
		if err := rows.Scan(&sb.BindingKey, &sb.Revision, &scopeJSON, &sb.Priority, &sb.Profile); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(scopeJSON, &sb.Scope); err != nil {
			return nil, fmt.Errorf("stored scope for %s is not parseable: %w", sb.BindingKey, err)
		}
		if q.Overlaps(sb.Scope.CanonicalScope()) {
			sb.State = StatePublished
			out = append(out, sb)
		}
	}
	return out, rows.Err()
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

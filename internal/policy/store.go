package policy

// DB-backed policy store (I06): real PostgreSQL via pgx; activation
// atomicity is enforced by the policy_single_active_per_set unique
// partial index (migration 00003) inside a single transaction that also
// supersedes the previous ACTIVE revision.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Store persists policy revisions with DB-enforced invariants.
type Store struct{ DSN string }

// ErrUniqueActive is returned when a concurrent activation wins the race
// (the unique partial index rejected our insert/update).
var ErrUniqueActive = errors.New("concurrent activation: another revision is ACTIVE")

// SaveRevision inserts a revision row (DRAFT or PUBLISHED states only).
func (s Store) SaveRevision(ctx context.Context, r *Revision, tenantRef string) error {
	conn, err := pgx.Connect(ctx, s.DSN)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	content, err := json.Marshal(r.Content)
	if err != nil {
		return err
	}
	_, err = conn.Exec(ctx, `
		INSERT INTO policy.policy_revision
			(set_id, version, state, content, content_digest, tenant_ref, published_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		r.SetID, r.Version, string(r.State), content, r.Digest, tenantRef, r.PublishedAt)
	return err
}

// ActivateRevision atomically: supersede current ACTIVE rows for the set,
// then set this row ACTIVE — single transaction. On a concurrent winner
// the unique partial index aborts the transaction (ErrUniqueActive).
func (s Store) ActivateRevision(ctx context.Context, setID string, version int) error {
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

	// publish-if-draft first (domain: activation requires PUBLISHED)
	tag, err := tx.Exec(ctx, `
		UPDATE policy.policy_revision
		SET state = 'PUBLISHED', published_at = now()
		WHERE set_id = $1 AND version = $2 AND state = 'DRAFT'`, setID, version)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		// must already be PUBLISHED / ACTIVATED / SUPERSEDED — verify exists
		var state string
		err = tx.QueryRow(ctx, `
			SELECT state FROM policy.policy_revision WHERE set_id = $1 AND version = $2`,
			setID, version).Scan(&state)
		if err == pgx.ErrNoRows {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if state == "DRAFT" { // unreachable, defensive
			return fmt.Errorf("unexpected DRAFT after publish update")
		}
	}

	// supersede previous ACTIVE
	if _, err := tx.Exec(ctx, `
		UPDATE policy.policy_revision
		SET state = 'SUPERSEDED'
		WHERE set_id = $1 AND state = 'ACTIVATED' AND version <> $2`,
		setID, version); err != nil {
		return err
	}
	// activate target
	tag, err = tx.Exec(ctx, `
		UPDATE policy.policy_revision
		SET state = 'ACTIVATED', activated_at = now()
		WHERE set_id = $1 AND version = $2 AND state IN ('PUBLISHED','SUPERSEDED')`,
		setID, version)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrUniqueActive
		}
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: revision %s/%d not in activatable state", ErrTransition, setID, version)
	}
	// uniqueness guard: the partial index makes a concurrent double-ACTIVE
	// impossible; a lost race surfaces as a unique violation here
	if err := tx.Commit(ctx); err != nil {
		if isUniqueViolation(err) {
			return ErrUniqueActive
		}
		return err
	}
	return nil
}

// ActiveRevision loads the single ACTIVE revision for a set.
func (s Store) ActiveRevision(ctx context.Context, setID string) (*Revision, error) {
	conn, err := pgx.Connect(ctx, s.DSN)
	if err != nil {
		return nil, err
	}
	defer conn.Close(ctx)
	var r Revision
	var state string
	var content []byte
	err = conn.QueryRow(ctx, `
		SELECT set_id, version, state, content, content_digest,
		       created_at, published_at, activated_at
		FROM policy.policy_revision
		WHERE set_id = $1 AND state = 'ACTIVATED'`, setID).
		Scan(&r.SetID, &r.Version, &state, &content, &r.Digest,
			&r.CreatedAt, &r.PublishedAt, &r.ActivatedAt)
	if err == pgx.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	r.State = State(state)
	if err := json.Unmarshal(content, &r.Content); err != nil {
		return nil, err
	}
	return &r, nil
}

// RecordPlanRef writes the immutable policy reference for a Resource Plan
// (issue: Plan 携带 policy revision/reference；引用不可变 = 不 UPDATE)。
func (s Store) RecordPlanRef(ctx context.Context, planID string, e Evaluation) error {
	conn, err := pgx.Connect(ctx, s.DSN)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	_, err = conn.Exec(ctx, `
		INSERT INTO policy.plan_policy_ref
			(resource_plan_id, policy_set_id, policy_version, policy_digest)
		VALUES ($1, $2, $3, $4)`,
		planID, e.PolicySetID, e.PolicyVersion, e.PolicyDigest)
	return err
}

// PlanRef loads the recorded reference (proves immutability after rollback).
func (s Store) PlanRef(ctx context.Context, planID string) (Evaluation, *time.Time, error) {
	conn, err := pgx.Connect(ctx, s.DSN)
	if err != nil {
		return Evaluation{}, nil, err
	}
	defer conn.Close(ctx)
	var e Evaluation
	var created time.Time
	err = conn.QueryRow(ctx, `
		SELECT policy_set_id, policy_version, policy_digest, created_at
		FROM policy.plan_policy_ref WHERE resource_plan_id = $1`, planID).
		Scan(&e.PolicySetID, &e.PolicyVersion, &e.PolicyDigest, &created)
	if err == pgx.ErrNoRows {
		return Evaluation{}, nil, ErrNotFound
	}
	return e, &created, err
}

func isUniqueViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	if errors.As(err, &pgErr) {
		return pgErr.SQLState() == "23505"
	}
	return false
}

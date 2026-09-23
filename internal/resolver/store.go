package resolver

// pgx-backed Resource Plan store (I09): idempotent creation (caller +
// Idempotency-Key unique; same key + same request digest returns the
// original plan, same key + different digest is a conflict), immutable
// items (DB trigger), status lifecycle RESOLVED → EXPIRED/REVOKED.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store persists resource plans over a shared connection pool (the
// resolver's QPS budget forbids per-call TCP connects).
type Store struct{ Pool *pgxpool.Pool }

// NewStore builds a pooled plan store.
func NewStore(ctx context.Context, dsn string, maxConns int32) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	if maxConns > 0 {
		cfg.MaxConns = maxConns
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &Store{Pool: pool}, nil
}

// CreatePlan persists plan + items atomically.
//
// Idempotency: an existing (caller_ref, idempotency_key) row with the SAME
// request digest returns created=false plus the original plan (no second
// write); a DIFFERENT digest is ErrIdemConflict. The unique index is the
// concurrency arbiter: the loser of a same-key race re-reads and classifies
// instead of guessing (100 次相同幂等请求只创建一个 Plan, GWT#3).
func (s Store) CreatePlan(ctx context.Context, plan *Plan) (bool, *Plan, error) {
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return false, nil, err
	}
	defer conn.Release()

	// replay check (fast path)
	if existing, ok, err := s.byKey(ctx, conn, plan.CallerRef, plan.IdempotencyKey); err != nil {
		return false, nil, err
	} else if ok {
		if existing.RequestDigest != plan.RequestDigest {
			return false, nil, resErr(CodeIdempotencyConflict, 409,
				fmt.Sprintf("idempotency key %s was used for a different request body", plan.IdempotencyKey))
		}
		return false, existing, nil
	}

	tx, err := conn.Begin(ctx)
	if err != nil {
		return false, nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	_, err = tx.Exec(ctx, `
		INSERT INTO resolver.resource_plan
			(id, caller_ref, tenant_ref, task_ref, status, fingerprint, request_digest,
			 idempotency_key, policy_set_id, policy_version, trace_id, created_at, expires_at)
		VALUES ($1, $2, $3, $4, 'RESOLVED', $5, $6, $7, $8, $9, $10, $11, $12)`,
		plan.ID, plan.CallerRef, plan.TenantRef, plan.TaskRef, plan.Fingerprint,
		plan.RequestDigest, plan.IdempotencyKey, plan.PolicySetID, plan.PolicyVersion,
		plan.TraceID, plan.CreatedAt, plan.ExpiresAt)
	if err != nil {
		if c, ok := constraintOf(err); ok {
			// concurrent same-key commit: the tx is ABORTED (25P02) —
			// roll it back before re-reading on the same pooled conn.
			// Only the (caller_ref, idempotency_key) constraint is an
			// idempotency replay; a PK hit (plan-ID collision) is a
			// distinct failure the caller must retry with a new request
			// (review P3-4).
			if c != "resource_plan_caller_ref_idempotency_key_key" {
				_ = tx.Rollback(ctx)
				return false, nil, resErr(CodeResolverUnavailable, 500,
					"plan id collision; caller must retry with a new request")
			}
			_ = tx.Rollback(ctx)
			existing, ok, qerr := s.byKey(ctx, conn, plan.CallerRef, plan.IdempotencyKey)
			if qerr != nil {
				return false, nil, qerr
			}
			if ok && existing.RequestDigest == plan.RequestDigest {
				return false, existing, nil
			}
			return false, nil, resErr(CodeIdempotencyConflict, 409,
				fmt.Sprintf("idempotency key %s was used for a different request body", plan.IdempotencyKey))
		}
		return false, nil, err
	}

	for i := range plan.Items {
		it := &plan.Items[i]
		reasons, err := json.Marshal(it.ReasonCodes)
		if err != nil {
			return false, nil, err
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO resolver.resource_plan_item
				(plan_id, requirement_id, capability_key, major_version, capability_revision,
				 binding_key, binding_revision, provider_key, snapshot_version,
				 contract_version, profile_or_action, reason_codes)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
			plan.ID, it.RequirementID, it.CapabilityKey, it.MajorVersion, it.CapabilityRevision,
			it.BindingKey, it.BindingRevision, it.ProviderKey, it.SnapshotVersion,
			it.ContractVersion, it.ProfileOrAction, reasons)
		if err != nil {
			return false, nil, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return false, nil, err
	}
	plan.Status = StatusResolved
	return true, plan, nil
}

// GetPlan returns the plan with items; ErrNotFound when absent.
func (s Store) GetPlan(ctx context.Context, id string) (*Plan, error) {
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Release()
	return s.byId(ctx, conn, id)
}

// SweepExpired moves RESOLVED plans past expires_at to EXPIRED (lazy TTL
// enforcement on read + background sweep both rely on the same state
// machine). Returns the number of plans expired.
func (s Store) SweepExpired(ctx context.Context, now string) (int, error) {
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Release()
	tag, err := conn.Exec(ctx, `
		UPDATE resolver.resource_plan
		SET status = 'EXPIRED'
		WHERE status = 'RESOLVED' AND expires_at < $1::timestamptz`, now)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// RevokePlan moves a RESOLVED plan to REVOKED (回滚场景：已生成 Plan 保留
// 不删除; revoke is the only non-TTL exit edge).
func (s Store) RevokePlan(ctx context.Context, id string) error {
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	tag, err := conn.Exec(ctx, `
		UPDATE resolver.resource_plan
		SET status = 'REVOKED'
		WHERE id = $1 AND status = 'RESOLVED'`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s Store) byKey(ctx context.Context, conn *pgxpool.Conn, caller, key string) (*Plan, bool, error) {
	var p Plan
	var expiresAt, createdAt time.Time
	err := conn.QueryRow(ctx, `
		SELECT id, caller_ref, tenant_ref, task_ref, status, fingerprint, request_digest,
		       policy_set_id, policy_version, created_at, expires_at
		FROM resolver.resource_plan
		WHERE caller_ref = $1 AND idempotency_key = $2`, caller, key).
		Scan(&p.ID, &p.CallerRef, &p.TenantRef, &p.TaskRef, &p.Status, &p.Fingerprint,
			&p.RequestDigest, &p.PolicySetID, &p.PolicyVersion, &createdAt, &expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	p.CreatedAt, p.ExpiresAt = createdAt.UTC().Format(time.RFC3339Nano), expiresAt.UTC().Format(time.RFC3339Nano)
	items, err := s.items(ctx, conn, p.ID)
	if err != nil {
		return nil, false, err
	}
	p.Items = items
	return &p, true, nil
}

func (s Store) byId(ctx context.Context, conn *pgxpool.Conn, id string) (*Plan, error) {
	var p Plan
	var expiresAt, createdAt time.Time
	err := conn.QueryRow(ctx, `
		SELECT id, caller_ref, tenant_ref, task_ref, status, fingerprint, request_digest,
		       policy_set_id, policy_version, created_at, expires_at
		FROM resolver.resource_plan
		WHERE id = $1`, id).
		Scan(&p.ID, &p.CallerRef, &p.TenantRef, &p.TaskRef, &p.Status, &p.Fingerprint,
			&p.RequestDigest, &p.PolicySetID, &p.PolicyVersion, &createdAt, &expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	p.CreatedAt, p.ExpiresAt = createdAt.UTC().Format(time.RFC3339Nano), expiresAt.UTC().Format(time.RFC3339Nano)
	items, err := s.items(ctx, conn, p.ID)
	if err != nil {
		return nil, err
	}
	p.Items = items
	return &p, nil
}

func (s Store) items(ctx context.Context, conn *pgxpool.Conn, planID string) ([]PlanItem, error) {
	rows, err := conn.Query(ctx, `
		SELECT requirement_id, capability_key, major_version, capability_revision,
		       binding_key, binding_revision, provider_key, snapshot_version,
		       contract_version, profile_or_action, reason_codes
		FROM resolver.resource_plan_item
		WHERE plan_id = $1
		ORDER BY requirement_id`, planID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PlanItem
	for rows.Next() {
		var it PlanItem
		var reasons []byte
		if err := rows.Scan(&it.RequirementID, &it.CapabilityKey, &it.MajorVersion,
			&it.CapabilityRevision, &it.BindingKey, &it.BindingRevision, &it.ProviderKey,
			&it.SnapshotVersion, &it.ContractVersion, &it.ProfileOrAction, &reasons); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(reasons, &it.ReasonCodes); err != nil {
			return nil, err
		}
		out = append(out, it)
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

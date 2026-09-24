// Package exitdrill implements the Exit Drill state machine and
// remediation loop (I15, module 03.8, architecture §Drill Management):
// plan → approve → schedule → run → {succeed|fail|abort} → remediate →
// close, with idempotent external steps, worker leases, timeouts and
// evidence linkage.
//
// 合法转换（issue 显式全矩阵）:
//
//	DRAFT → APPROVED → SCHEDULED → RUNNING → {SUCCEEDED | FAILED | ABORTED}
//	SUCCEEDED →（存在 Finding 时）REMEDIATION_OPEN
//	REMEDIATION_OPEN → CLOSED（无未关闭 Finding + 结果 Evidence 完备）
//	SCHEDULED → ABORTED；REMEDIATION_OPEN → ABORTED（人工终止）
//
// 矩阵外任意转换 → 422 语义拒绝且状态不变；审批人 ≠ 发起人。
package exitdrill

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// States.
const (
	StateDraft           = "DRAFT"
	StateApproved        = "APPROVED"
	StateScheduled       = "SCHEDULED"
	StateRunning         = "RUNNING"
	StateSucceeded       = "SUCCEEDED"
	StateFailed          = "FAILED"
	StateAborted         = "ABORTED"
	StateRemediationOpen = "REMEDIATION_OPEN"
	StateClosed          = "CLOSED"
)

// Reason codes.
const (
	ReasonInvalidTransition = "DRILL_INVALID_TRANSITION"
	ReasonSelfApproval      = "DRILL_SELF_APPROVAL_FORBIDDEN"
	ReasonNotFound          = "DRILL_NOT_FOUND"
	ReasonOpenFindings      = "DRILL_OPEN_FINDINGS_BLOCK_CLOSE"
	ReasonMissingEvidence   = "DRILL_RESULT_EVIDENCE_REQUIRED"
	ReasonCasConflict       = "DRILL_CAS_CONFLICT"
	ReasonStepNotClaimable  = "DRILL_STEP_NOT_CLAIMABLE"
)

// Sentinel errors.
var (
	ErrNotFound     = errors.New("exitdrill: drill not found")
	ErrCasConflict  = errors.New("exitdrill: concurrent command won")
	ErrInvalidTrans = errors.New("exitdrill: invalid transition")
)

// legalTransitions is the explicit full matrix (issue 核心验收逻辑).
var legalTransitions = map[string][]string{
	StateDraft:           {StateApproved},
	StateApproved:        {StateScheduled},
	StateScheduled:       {StateRunning, StateAborted},
	StateRunning:         {StateSucceeded, StateFailed, StateAborted},
	StateSucceeded:       {StateRemediationOpen},
	StateFailed:          {},
	StateAborted:         {},
	StateRemediationOpen: {StateClosed, StateAborted},
	StateClosed:          {},
}

// ValidateTransition reports whether from→to is legal.
func ValidateTransition(from, to string) bool {
	for _, t := range legalTransitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

// ErrTyped carries a reason code.
type ErrTyped struct {
	Reason string
	Msg    string
}

func (e *ErrTyped) Error() string { return e.Reason + ": " + e.Msg }

func errT(reason, msg string) error { return &ErrTyped{reason, msg} }

// Store persists drills.
type Store struct{ Pool *pgxpool.Pool }

// Drill is the aggregate root.
type Drill struct {
	DrillKey       string
	Vendor         string
	ExitPackKey    string
	Initiator      string
	Approver       string
	State          string
	ResultEvidence string
}

// Create inserts a DRAFT drill.
func (s Store) Create(ctx context.Context, d *Drill) error {
	if d.DrillKey == "" || d.Vendor == "" || d.Initiator == "" {
		return errT(ReasonInvalidTransition, "drill_key, vendor and initiator required")
	}
	tag, err := s.Pool.Exec(ctx, `
		INSERT INTO saoaf.exit_drill (drill_key, vendor, exit_pack_key, initiator, state)
		VALUES ($1, $2, $3, $4, 'DRAFT')`,
		d.DrillKey, d.Vendor, d.ExitPackKey, d.Initiator)
	if err != nil {
		return err
	}
	_ = tag
	d.State = StateDraft
	return nil
}

// Approve moves DRAFT → APPROVED with the 审批人 ≠ 发起人 constraint
// （服务端强制）. Idempotent: re-approving an already-APPROVED drill by the
// same approver is a no-op（GWT#3 重复审批）.
func (s Store) Approve(ctx context.Context, drillKey, approver string) error {
	return s.transition(ctx, drillKey, StateDraft, StateApproved, func(tx pgx.Tx, d *Drill) error {
		if approver == d.Initiator {
			return errT(ReasonSelfApproval, "approver must differ from initiator (separation of duties)")
		}
		if _, err := tx.Exec(ctx, `
			UPDATE saoaf.exit_drill SET approver = $1, approved_at = now()
			WHERE drill_key = $2`, approver, drillKey); err != nil {
			return err
		}
		return nil
	}, approver)
}

// Schedule moves APPROVED → SCHEDULED. Idempotent on the same actor（重复调度）.
func (s Store) Schedule(ctx context.Context, drillKey, actor string) error {
	return s.transition(ctx, drillKey, StateApproved, StateScheduled, nil, actor)
}

// Start moves SCHEDULED → RUNNING.
func (s Store) Start(ctx context.Context, drillKey, workerID string) error {
	return s.transition(ctx, drillKey, StateScheduled, StateRunning, nil, workerID)
}

// Abort performs a legal manual termination（SCHEDULED/RUNNING/REMEDIATION_OPEN
// → ABORTED per the matrix）. The undo evidence for external systems is the
// caller's duty — the DB never pretends an external action was undone.
func (s Store) Abort(ctx context.Context, drillKey, actor string) error {
	d, err := s.Get(ctx, drillKey)
	if err != nil {
		return err
	}
	if !ValidateTransition(d.State, StateAborted) {
		return errT(ReasonInvalidTransition, d.State+" → ABORTED outside the matrix")
	}
	return s.transition(ctx, drillKey, d.State, StateAborted, nil, actor)
}

// Finish moves RUNNING → {SUCCEEDED | FAILED} with result evidence.
func (s Store) Finish(ctx context.Context, drillKey, to, actor, resultEvidence string) error {
	if to != StateSucceeded && to != StateFailed {
		return errT(ReasonInvalidTransition, "finish target must be SUCCEEDED or FAILED")
	}
	return s.transition(ctx, drillKey, StateRunning, to, func(tx pgx.Tx, d *Drill) error {
		if _, err := tx.Exec(ctx, `
			UPDATE saoaf.exit_drill SET result_evidence = $1, finished_at = now()
			WHERE drill_key = $2`, resultEvidence, drillKey); err != nil {
			return err
		}
		// SUCCEEDED with findings → open the remediation phase in the same tx
		if to == StateSucceeded {
			var openFindings int
			if err := tx.QueryRow(ctx, `
				SELECT count(*) FROM saoaf.exit_drill_finding WHERE drill_key = $1`, drillKey).Scan(&openFindings); err != nil {
				return err
			}
			if openFindings > 0 {
				if _, err := tx.Exec(ctx, `
					UPDATE saoaf.exit_drill SET state = 'REMEDIATION_OPEN', remediation_opened_at = now()
					WHERE drill_key = $1`, drillKey); err != nil {
					return err
				}
			}
		}
		return nil
	}, actor)
}

// Close moves REMEDIATION_OPEN → CLOSED with the two hard preconditions:
// no OPEN findings AND result evidence present（缺一拒绝）.
func (s Store) Close(ctx context.Context, drillKey, actor string) error {
	return s.transition(ctx, drillKey, StateRemediationOpen, StateClosed, func(tx pgx.Tx, d *Drill) error {
		var open int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM saoaf.exit_drill_finding
			WHERE drill_key = $1 AND state = 'OPEN'`, drillKey).Scan(&open); err != nil {
			return err
		}
		if open > 0 {
			return errT(ReasonOpenFindings, fmt.Sprintf("%d findings still open", open))
		}
		if d.ResultEvidence == "" {
			return errT(ReasonMissingEvidence, "result evidence required before CLOSED")
		}
		return nil
	}, actor)
}

// transition is the CAS core: read state, validate the matrix, apply the
// mutation, and write the audit row — one tx. Concurrent duplicate commands
// race on the CAS; the loser gets ErrCasConflict (GWT#4).
func (s Store) transition(ctx context.Context, drillKey, from, to string, mutate func(tx pgx.Tx, d *Drill) error, actor string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var d Drill
	err = tx.QueryRow(ctx, `
		SELECT drill_key, vendor, exit_pack_key, initiator, approver, state, result_evidence
		FROM saoaf.exit_drill WHERE drill_key = $1`, drillKey).
		Scan(&d.DrillKey, &d.Vendor, &d.ExitPackKey, &d.Initiator, &d.Approver, &d.State, &d.ResultEvidence)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if d.State == to && from == to {
		return tx.Commit(ctx) // idempotent replay of the same transition
	}
	if d.State != from {
		if d.State == to {
			// already at the target (e.g. duplicate approve) — classify by
			// the matrix: replay no-op, not an error
			return tx.Commit(ctx)
		}
		if !ValidateTransition(d.State, to) {
			return errT(ReasonInvalidTransition, d.State+" → "+to+" outside the matrix")
		}
		return errT(ReasonCasConflict, "concurrent command changed state to "+d.State)
	}
	if !ValidateTransition(from, to) {
		return errT(ReasonInvalidTransition, from+" → "+to+" outside the matrix")
	}
	// base state transition FIRST; the mutate hook may then advance the
	// row further (e.g. Finish: RUNNING → SUCCEEDED → REMEDIATION_OPEN in
	// one tx when findings exist)
	tag, err := tx.Exec(ctx, `
		UPDATE saoaf.exit_drill SET state = $1 WHERE drill_key = $2 AND state = $3`,
		to, drillKey, from)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errT(ReasonCasConflict, "concurrent command won the CAS")
	}
	if mutate != nil {
		if err := mutate(tx, &d); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO saoaf.exit_drill_transition (drill_key, from_state, to_state, actor)
		VALUES ($1, $2, $3, $4)`, drillKey, from, to, actor); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Get returns the drill.
func (s Store) Get(ctx context.Context, drillKey string) (*Drill, error) {
	var d Drill
	err := s.Pool.QueryRow(ctx, `
		SELECT drill_key, vendor, exit_pack_key, initiator, approver, state, result_evidence
		FROM saoaf.exit_drill WHERE drill_key = $1`, drillKey).
		Scan(&d.DrillKey, &d.Vendor, &d.ExitPackKey, &d.Initiator, &d.Approver, &d.State, &d.ResultEvidence)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// AddFinding records a finding (drills in SUCCEEDED/REMEDIATION_OPEN may
// accumulate findings before closing).
func (s Store) AddFinding(ctx context.Context, drillKey, findingKey, description, severity string) error {
	if _, err := s.Pool.Exec(ctx, `
		INSERT INTO saoaf.exit_drill_finding (drill_key, finding_key, description, severity)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (drill_key, finding_key) DO NOTHING`,
		drillKey, findingKey, description, severity); err != nil {
		return err
	}
	// move a SUCCEEDED drill into the remediation phase
	_, err := s.Pool.Exec(ctx, `
		UPDATE saoaf.exit_drill SET state = 'REMEDIATION_OPEN', remediation_opened_at = now()
		WHERE drill_key = $1 AND state = 'SUCCEEDED'`, drillKey)
	return err
}

// ResolveFinding closes one finding with remediation + evidence.
func (s Store) ResolveFinding(ctx context.Context, drillKey, findingKey, remediation, evidence string) error {
	tag, err := s.Pool.Exec(ctx, `
		UPDATE saoaf.exit_drill_finding
		SET state = 'RESOLVED', remediation = $1, remediation_evidence = $2, resolved_at = now()
		WHERE drill_key = $3 AND finding_key = $4 AND state = 'OPEN'`,
		remediation, evidence, drillKey, findingKey)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errT(ReasonNotFound, "finding not found or already resolved")
	}
	return nil
}

// RecoverableDrills lists RUNNING drills（Worker 重启后从持久化状态恢复）.
func (s Store) RecoverableDrills(ctx context.Context) ([]string, error) {
	rows, err := s.Pool.Query(ctx,
		`SELECT drill_key FROM saoaf.exit_drill WHERE state = 'RUNNING'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// TransitionLog returns the audit trail (monitoring export).
func (s Store) TransitionLog(ctx context.Context, drillKey string) ([]struct {
	From, To, Actor string
	At              time.Time
}, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT from_state, to_state, actor, at FROM saoaf.exit_drill_transition
		WHERE drill_key = $1 ORDER BY at`, drillKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type T = struct {
		From, To, Actor string
		At              time.Time
	}
	var out []T
	for rows.Next() {
		var t T
		if err := rows.Scan(&t.From, &t.To, &t.Actor, &t.At); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

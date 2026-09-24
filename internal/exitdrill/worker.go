package exitdrill

// Drill Worker: external steps with idempotency keys + leases + timeouts.
// 崩溃窗口一：external action done, confirmation write lost → the
// restart re-claims the step and re-invokes the executor, which the
// EXTERNAL system dedups by idempotency key（副作用不重复）。
// 崩溃窗口二：Worker restart while RUNNING → steps load from the
// persisted state; PENDING/RUNNING-with-expired-lease re-execute, DONE
// never repeats（无步骤丢失/重复）。

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// StepExecutor performs one external action idempotently. The external
// system dedups by the idempotency key — the executor MUST be safe to
// call twice（at-least-once + 外部去重 = 有效一次）.
type StepExecutor func(ctx context.Context, drillKey string, step Step) (evidence string, err error)

// Step is one persisted external step.
type Step struct {
	DrillKey       string
	StepNo         int
	Action         string
	IdempotencyKey string
	State          string
	Attempts       int
	TimeoutAt      *time.Time
	ResultEvidence string
}

// Worker runs drill steps.
type Worker struct {
	Store       Store
	Executor    StepExecutor
	WorkerID    string
	LeaseTTL    time.Duration
	MaxAttempts int
}

// ClaimNext claims the next runnable step: PENDING, or RUNNING with an
// expired lease（visible-timeout re-claim）. SKIP LOCKED keeps multiple
// workers from double-claiming.
func (w Worker) ClaimNext(ctx context.Context, drillKey string) (*Step, error) {
	tx, err := w.Store.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var s Step
	var timeoutAt *time.Time
	err = tx.QueryRow(ctx, `
		SELECT drill_key, step_no, action, idempotency_key, state, attempts, timeout_at, result_evidence
		FROM saoaf.exit_drill_step
		WHERE drill_key = $1
		  AND (state = 'PENDING' OR (state = 'RUNNING' AND lease_expires_at < now()))
		ORDER BY step_no
		FOR UPDATE SKIP LOCKED
		LIMIT 1`, drillKey).
		Scan(&s.DrillKey, &s.StepNo, &s.Action, &s.IdempotencyKey, &s.State, &s.Attempts, &timeoutAt, &s.ResultEvidence)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s.TimeoutAt = timeoutAt
	if s.Attempts >= w.maxAttempts() {
		// retry budget exhausted → FAILED（超时/失败按定义转换并留证）
		if _, err := tx.Exec(ctx, `
			UPDATE saoaf.exit_drill_step SET state = 'FAILED', lease_expires_at = NULL
			WHERE drill_key = $1 AND step_no = $2`, drillKey, s.StepNo); err != nil {
			return nil, err
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("exitdrill: step %d exceeded retry budget (%d)", s.StepNo, w.maxAttempts())
	}
	if _, err := tx.Exec(ctx, `
		UPDATE saoaf.exit_drill_step
		SET state = 'RUNNING', claimed_by = $1, lease_expires_at = now() + $2::interval,
		    attempts = attempts + 1
		WHERE drill_key = $3 AND step_no = $4`,
		w.WorkerID, fmt.Sprintf("%d seconds", int(w.leaseTTL().Seconds())), drillKey, s.StepNo); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	s.State = "RUNNING"
	s.Attempts++
	return &s, nil
}

// RunStep executes one claimed step and marks it DONE with evidence in
// ONE statement（外部动作完成 + 确认写入的原子性由幂等键承载：若确认写入
// 前崩溃，重启重claim 重执行，外部系统按幂等键去重）。
func (w Worker) RunStep(ctx context.Context, s *Step) error {
	evidence, err := w.Executor(ctx, s.DrillKey, *s)
	if err != nil {
		// release the lease for a retry (state back to PENDING)
		_, _ = w.Store.Pool.Exec(ctx, `
			UPDATE saoaf.exit_drill_step SET state = 'PENDING', lease_expires_at = NULL
			WHERE drill_key = $1 AND step_no = $2`, s.DrillKey, s.StepNo)
		return err
	}
	tag, err := w.Store.Pool.Exec(ctx, `
		UPDATE saoaf.exit_drill_step
		SET state = 'DONE', result_evidence = $1, lease_expires_at = NULL
		WHERE drill_key = $2 AND step_no = $3 AND state IN ('RUNNING','PENDING')`,
		evidence, s.DrillKey, s.StepNo)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return nil // already marked (idempotent confirmation)
	}
	return nil
}

// RunDrill drives a RUNNING drill to completion: claim → execute → mark,
// step by step; SUCCEEDED when all steps DONE, FAILED when any exceeds
// the budget. Recovery on restart: call with the drill key from
// RecoverableDrills（持久化状态恢复）.
func (w Worker) RunDrill(ctx context.Context, drillKey string) error {
	for {
		s, err := w.ClaimNext(ctx, drillKey)
		if err != nil {
			return err
		}
		if s == nil {
			break // no claimable steps remain
		}
		if err := w.RunStep(ctx, s); err != nil {
			return err
		}
	}
	// aggregate the step states
	var done, failed, total int
	if err := w.Store.Pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE state = 'DONE'),
		       count(*) FILTER (WHERE state = 'FAILED'),
		       count(*)
		FROM saoaf.exit_drill_step WHERE drill_key = $1`, drillKey).Scan(&done, &failed, &total); err != nil {
		return err
	}
	if total == 0 {
		return nil // no steps defined — leave state management to the caller
	}
	if failed > 0 {
		return w.Store.Finish(ctx, drillKey, StateFailed, w.WorkerID, "steps failed (retry budget exhausted)")
	}
	if done == total {
		return w.Store.Finish(ctx, drillKey, StateSucceeded, w.WorkerID, "all steps done")
	}
	return nil
}

// TimeoutSweep marks timed-out RUNNING steps for retry/FAILURE.
func (w Worker) TimeoutSweep(ctx context.Context, now time.Time) (int, error) {
	tag, err := w.Store.Pool.Exec(ctx, `
		UPDATE saoaf.exit_drill_step
		SET state = 'PENDING', lease_expires_at = NULL
		WHERE state = 'RUNNING' AND timeout_at IS NOT NULL AND timeout_at < $1`, now)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

func (w Worker) leaseTTL() time.Duration {
	if w.LeaseTTL > 0 {
		return w.LeaseTTL
	}
	return 30 * time.Second
}

func (w Worker) maxAttempts() int {
	if w.MaxAttempts > 0 {
		return w.MaxAttempts
	}
	return 3
}

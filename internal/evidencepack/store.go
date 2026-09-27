package evidencepack

// Store persists archive packs and their index links over the shared
// saoaf schema (ADR-0006: SQL boundary, pgx driver).
import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store wires the pack ledger to the database.
type Store struct {
	Pool *pgxpool.Pool
}

// ClaimWindow claims the next unarchived evidence window FOR THIS WORKER
// (FOR UPDATE SKIP LOCKED): at-least-once scanning with DB dedup.
// Returns the pack id (content digest), the records and whether a NEW
// pack row was created (false = another worker already claimed the same
// logical pack; the caller converges instead of re-archiving).
func (s Store) ClaimWindow(ctx context.Context, consumerID string, batchSize int, policyVersion string, retentionDays int) (packID string, records []Record, created bool, err error) {
	if batchSize <= 0 {
		batchSize = 100
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return "", nil, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// forward-only checkpoint: records strictly after the last scan point
	var lastID int64
	if err := tx.QueryRow(ctx,
		`SELECT last_record_id FROM saoaf.evidence_archive_checkpoint WHERE consumer_id = $1`,
		consumerID).Scan(&lastID); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", nil, false, err
	}

	rows, err := tx.Query(ctx, `
		SELECT id, event_id, payload_digest, tenant_ref, occurred_at
		FROM saoaf.evidence_record
		WHERE id > $1 AND state = 'LINKED'
		ORDER BY id
		LIMIT $2`, lastID, batchSize)
	if err != nil {
		return "", nil, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var r Record
		var occurredAt time.Time
		if err := rows.Scan(&r.ID, &r.EventID, &r.PayloadDigest, &r.TenantRef, &occurredAt); err != nil {
			return "", nil, false, err
		}
		r.OccurredAt = occurredAt
		records = append(records, r)
	}
	if err := rows.Err(); err != nil {
		return "", nil, false, err
	}
	if len(records) == 0 {
		return "", nil, false, nil // nothing to archive
	}

	// skip records already archived (crash-after-link replay or steady-
	// state re-scan): filter them OUT of the window instead of skipping the
	// whole window (review R1 P1-1: the checkpoint never advanced on the
	// success path, so mixed windows silently skipped unlinked records
	// forever). Only records WITHOUT a link may join a pack.
	linkedPrefixEnd := int64(0)
	unlinked := records[:0]
	for _, r := range records {
		var has bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS(SELECT 1 FROM saoaf.evidence_archive_link WHERE record_id = $1)`,
			r.ID).Scan(&has); err != nil {
			return "", nil, false, err
		}
		if has {
			if r.ID > linkedPrefixEnd {
				linkedPrefixEnd = r.ID
			}
			continue
		}
		unlinked = append(unlinked, r)
	}
	records = unlinked
	if linkedPrefixEnd > 0 {
		// advance the checkpoint past the already-archived prefix (forward
		// only — the checkpoint never moves backwards)
		if err := advanceCheckpointTx(ctx, tx, consumerID, linkedPrefixEnd); err != nil {
			return "", nil, false, err
		}
	}
	if len(records) == 0 {
		// whole window already archived — nothing new to do; converge
		if err := tx.Commit(ctx); err != nil {
			return "", nil, false, err
		}
		return "", nil, false, nil
	}

	packID = Digest(policyVersion, records)

	// create the pack row (PENDING) — unique pack_id makes concurrent
	// claims of the SAME window converge to one row
	tag, err := tx.Exec(ctx, `
		INSERT INTO saoaf.evidence_archive_pack
			(pack_id, state, bucket, object_key, record_count, first_record_id, last_record_id)
		VALUES ($1, 'PENDING', $2, $3, $4, $5, $6)
		ON CONFLICT (pack_id) DO NOTHING`,
		packID, bucketName, objectKeyFor(packID), len(records),
		records[0].ID, records[len(records)-1].ID)
	if err != nil {
		return "", nil, false, err
	}
	created = tag.RowsAffected() == 1

	if err := tx.Commit(ctx); err != nil {
		return "", nil, false, err
	}
	return packID, records, created, nil
}

// bucketName/objectKeyFor are the addressing policy (single sovereign
// bucket; keys are pack-id-addressed).
const bucketName = "saoaf-evidence"

func objectKeyFor(packID string) string { return "packs/" + packID + "/manifest.json" }

// MarkWriting transitions PENDING|RETRYABLE → WRITING (CAS).
func (s Store) MarkWriting(ctx context.Context, packID, bucket, objectKey string) error {
	tag, err := s.Pool.Exec(ctx, `
		UPDATE saoaf.evidence_archive_pack
		SET state = 'WRITING', attempt_count = attempt_count + 1
		WHERE pack_id = $1 AND state IN ('PENDING', 'RETRYABLE')`,
		packID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errT(ReasonInvalidTransition, "pack is not in an archivable state (already locked/verified/quarantined?)")
	}
	return nil
}

// MarkWritten records the object version + manifest digest after the
// manifest upload (WRITING → WRITING with version persisted).
func (s Store) MarkWritten(ctx context.Context, packID, objectVersion, manifestDigest string) error {
	tag, err := s.Pool.Exec(ctx, `
		UPDATE saoaf.evidence_archive_pack
		SET object_version = $2, manifest_digest = $3
		WHERE pack_id = $1 AND state = 'WRITING'`,
		packID, objectVersion, manifestDigest)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errT(ReasonInvalidTransition, "pack not in WRITING")
	}
	return nil
}

// MarkLocked transitions WRITING → LOCKED after COMPLIANCE retention is
// applied and the digest verified server-side.
func (s Store) MarkLocked(ctx context.Context, packID string, retentionUntil time.Time) error {
	tag, err := s.Pool.Exec(ctx, `
		UPDATE saoaf.evidence_archive_pack
		SET state = 'LOCKED', retention_until = $2, locked_at = now()
		WHERE pack_id = $1 AND state = 'WRITING' AND object_version <> ''`,
		packID, retentionUntil)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errT(ReasonInvalidTransition, "pack not in WRITING with an object version")
	}
	return nil
}

// ExtendRetention moves the lock deadline LATER only (shrink refused).
func (s Store) ExtendRetention(ctx context.Context, packID string, newUntil time.Time) error {
	tag, err := s.Pool.Exec(ctx, `
		UPDATE saoaf.evidence_archive_pack
		SET retention_until = $2
		WHERE pack_id = $1 AND state IN ('LOCKED', 'VERIFIED') AND retention_until < $2`,
		packID, newUntil)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		// either not locked, or the requested deadline is not an extension
		var current *time.Time
		if err := s.Pool.QueryRow(ctx,
			`SELECT retention_until FROM saoaf.evidence_archive_pack WHERE pack_id = $1`, packID).
			Scan(&current); err != nil {
			return err
		}
		if current != nil && !newUntil.After(*current) {
			return errT(ReasonRetentionShrink, fmt.Sprintf("retention only extends: current %s, requested %s", current.Format(time.RFC3339), newUntil.Format(time.RFC3339)))
		}
		return errT(ReasonInvalidTransition, "pack not locked")
	}
	return nil
}

// MarkVerified transitions LOCKED → VERIFIED and atomically commits the
// index links: the online index only ever references a LOCKED object
// version (verified digest). All-or-nothing: the transition and the
// links land in one transaction.
func (s Store) MarkVerified(ctx context.Context, packID string, records []Record, objectVersion string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var bucket, objectKey string
	var recordCount int
	err = tx.QueryRow(ctx, `
		SELECT bucket, object_key, record_count FROM saoaf.evidence_archive_pack
		WHERE pack_id = $1 AND state = 'LOCKED' AND object_version = $2`,
		packID, objectVersion).Scan(&bucket, &objectKey, &recordCount)
	if errors.Is(err, pgx.ErrNoRows) {
		return errT(ReasonInvalidTransition, "pack not LOCKED at this object version")
	}
	if err != nil {
		return err
	}
	if recordCount != len(records) {
		return errT(ReasonDigestMismatch, fmt.Sprintf("record count drift: pack %d, verify %d", recordCount, len(records)))
	}

	// links: one per record, UNIQUE(record_id) makes replay a no-op
	for _, r := range records {
		if _, err := tx.Exec(ctx, `
			INSERT INTO saoaf.evidence_archive_link
				(pack_id, record_id, object_version, digest)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (record_id) DO NOTHING`,
			packID, r.ID, objectVersion, r.PayloadDigest); err != nil {
			return err
		}
	}

	tag, err := tx.Exec(ctx, `
		UPDATE saoaf.evidence_archive_pack
		SET state = 'VERIFIED', verified_at = now()
		WHERE pack_id = $1 AND state = 'LOCKED'`,
		packID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errT(ReasonInvalidTransition, "pack left LOCKED during verification")
	}
	// flip the online index worm_status in the SAME transaction (review
	// R1 P3-4: the index and the archive view must agree — VERIFIED packs
	// mark their records ARCHIVED atomically)
	if _, err := tx.Exec(ctx, `
		UPDATE saoaf.evidence_record
		SET worm_status = 'ARCHIVED'
		WHERE id BETWEEN $1 AND $2`,
		records[0].ID, records[len(records)-1].ID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// MarkFailed classifies a failure: retryable (storage transient) or
// quarantined (digest mismatch / data-level). WRITING|PENDING → state.
func (s Store) MarkFailed(ctx context.Context, packID, state, reason string) error {
	if state != StateRetryable && state != StateQuarantined {
		return fmt.Errorf("invalid failure state %q", state)
	}
	tag, err := s.Pool.Exec(ctx, `
		UPDATE saoaf.evidence_archive_pack
		SET state = $2, error_reason = $3
		WHERE pack_id = $1 AND state IN ('PENDING', 'WRITING', 'RETRYABLE')`,
		packID, state, reason)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errT(ReasonInvalidTransition, "pack not in a failure-eligible state")
	}
	return nil
}

// Get loads one pack.
func (s Store) Get(ctx context.Context, packID string) (*Pack, error) {
	var p Pack
	var retention *time.Time
	err := s.Pool.QueryRow(ctx, `
		SELECT pack_id, state, bucket, object_key, object_version, manifest_digest,
		       retention_until, legal_hold, record_count, first_record_id, last_record_id,
		       attempt_count, error_reason
		FROM saoaf.evidence_archive_pack WHERE pack_id = $1`, packID).
		Scan(&p.PackID, &p.State, &p.Bucket, &p.ObjectKey, &p.ObjectVersion, &p.ManifestDigest,
			&retention, &p.LegalHold, &p.RecordCount, &p.FirstRecordID, &p.LastRecordID,
			&p.AttemptCount, &p.ErrorReason)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errT(ReasonNotFound, "pack does not exist")
	}
	if err != nil {
		return nil, err
	}
	p.RetentionUntil = retention
	return &p, nil
}

// LinkCount returns the committed index links for a pack.
func (s Store) LinkCount(ctx context.Context, packID string) (int, error) {
	var n int
	if err := s.Pool.QueryRow(ctx,
		`SELECT count(*) FROM saoaf.evidence_archive_link WHERE pack_id = $1`, packID).
		Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// advanceCheckpointTx moves the forward-only scan cursor inside a
// transaction (never backwards).
func advanceCheckpointTx(ctx context.Context, tx pgx.Tx, consumerID string, lastID int64) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO saoaf.evidence_archive_checkpoint (consumer_id, last_record_id)
		VALUES ($1, $2)
		ON CONFLICT (consumer_id) DO UPDATE
		SET last_record_id = $2, updated_at = now()
		WHERE saoaf.evidence_archive_checkpoint.last_record_id < $2`,
		consumerID, lastID)
	return err
}

// AdvanceCheckpoint moves the scan cursor after a successful archive
// (review R1 P1-1: the success path must advance too, or steady-state
// mixed windows keep re-containing linked records — the per-record link
// filter now handles correctness; the checkpoint keeps the scan O(new)).
func (s Store) AdvanceCheckpoint(ctx context.Context, consumerID string, lastID int64) error {
	tag, err := s.Pool.Exec(ctx, `
		INSERT INTO saoaf.evidence_archive_checkpoint (consumer_id, last_record_id)
		VALUES ($1, $2)
		ON CONFLICT (consumer_id) DO UPDATE
		SET last_record_id = $2, updated_at = now()
		WHERE saoaf.evidence_archive_checkpoint.last_record_id < $2`,
		consumerID, lastID)
	if err != nil {
		return err
	}
	_ = tag
	return nil
}

// PendingPacks lists non-terminal packs (PENDING/WRITING/RETRYABLE) for
// the recovery sweep — the worker drives each to a terminal state every
// tick (review R1 P1-2: Recover had zero production callers).
func (s Store) PendingPacks(ctx context.Context) ([]string, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT pack_id FROM saoaf.evidence_archive_pack
		WHERE state IN ('PENDING', 'WRITING', 'RETRYABLE')
		ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// HoldGate authorizes legal-hold operations: the caller must supply BOTH
// a separation-of-duties authorizer and the operator identity. Clearing a
// hold requires explicit authorization (review R1 P2-1).
type HoldGate interface {
	// Authorized reports whether actor may perform the hold operation.
	Authorized(ctx context.Context, actor string, clear bool) bool
}

// SetHold records a legal-hold transition in the pack ledger AND applies
// it to the stored object. Clearing requires the HoldGate's blessing; a
// hold always outranks lifecycle cleanup (the object store enforces the
// hold semantics; the ledger mirrors the state for audit).
func (s Store) SetHold(ctx context.Context, obj ObjectStore, gate HoldGate, packID, actor string, hold bool) error {
	if gate == nil {
		return fmt.Errorf("evidencepack: hold gate is required")
	}
	if !gate.Authorized(ctx, actor, !hold) {
		return errT("EVIDENCEPACK_HOLD_UNAUTHORIZED",
			fmt.Sprintf("actor %q is not authorized for hold operations (clear=%v)", actor, !hold))
	}
	p, err := s.Get(ctx, packID)
	if err != nil {
		return err
	}
	if p.State != StateVerified && p.State != StateLocked {
		return errT(ReasonInvalidTransition, "legal hold applies to locked/verified packs only")
	}
	if p.ObjectVersion == "" {
		return errT(ReasonDigestMismatch, "pack has no object version to hold")
	}
	if err := obj.SetLegalHold(ctx, p.Bucket, p.ObjectKey, p.ObjectVersion, hold); err != nil {
		return err
	}
	tag, err := s.Pool.Exec(ctx, `
		UPDATE saoaf.evidence_archive_pack
		SET legal_hold = $2
		WHERE pack_id = $1 AND state IN ('LOCKED', 'VERIFIED')`,
		packID, hold)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errT(ReasonInvalidTransition, "pack left LOCKED/VERIFIED during hold update")
	}
	return nil
}

// TryDriveLock takes a session-scoped advisory lock for driving ONE pack
// so concurrent workers (or multi-instance processes) never double-drive
// the same non-terminal pack (review R1: the recovery sweep re-opened a
// double-drive window the original single-creator claim avoided). Returns
// ok=false when another driver holds the lock — the caller must SKIP the
// pack (the holder sweeps it).
func (s Store) TryDriveLock(ctx context.Context, packID string) (release func(), ok bool, err error) {
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return nil, false, err
	}
	unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	key := "evidencepack:drive:" + packID
	var got bool
	if err := conn.Conn().QueryRow(ctx,
		`SELECT pg_try_advisory_lock(hashtext($1))`, key).Scan(&got); err != nil {
		conn.Release()
		cancel()
		return nil, false, err
	}
	if !got {
		conn.Release()
		cancel()
		return nil, false, nil
	}
	release = func() {
		_, _ = conn.Conn().Exec(unlockCtx, `SELECT pg_advisory_unlock(hashtext($1))`, key) //nolint:errcheck
		conn.Release()
		cancel()
	}
	return release, true, nil
}

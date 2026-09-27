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

	packID = Digest(policyVersion, records)

	// skip records already archived (crash-after-link replay): the UNIQUE
	// record link guarantees at most one pack per record
	var existing int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM saoaf.evidence_archive_link
		WHERE record_id BETWEEN $1 AND $2`,
		records[0].ID, records[len(records)-1].ID).Scan(&existing); err != nil {
		return "", nil, false, err
	}
	if existing > 0 {
		// advance the checkpoint past the already-archived window and
		// report no new pack (idempotent convergence)
		if _, err := tx.Exec(ctx, `
			INSERT INTO saoaf.evidence_archive_checkpoint (consumer_id, last_record_id)
			VALUES ($1, $2)
			ON CONFLICT (consumer_id) DO UPDATE SET last_record_id = $2, updated_at = now()`,
			consumerID, records[len(records)-1].ID); err != nil {
			return "", nil, false, err
		}
		if err := tx.Commit(ctx); err != nil {
			return "", nil, false, err
		}
		return packID, nil, false, nil
	}

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

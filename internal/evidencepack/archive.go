package evidencepack

// Archive executes ONE pack through the WORM archive chain:
// claim → write manifest → lock (COMPLIANCE) → verify + link.
//
// Crash-recovery contract (issue: four windows): every step is
// idempotent against the pack row — a worker resuming a half-done pack
// sees WRITING/LOCKED and picks up where the state points, never
// duplicating or marking unverified success. The four windows:
//
//	1. after body/manifest upload, before MarkWritten  → re-Put (same
//	   content, new version id), MarkWritten records the new version
//	2. after MarkWritten, before lock                   → re-run lock on
//	   the recorded version
//	3. after lock, before links                         → links only land
//	   in MarkVerified (all-or-nothing), re-verify links them
//	4. after links, before VERIFIED                     → the same tx
//	   commits both (no window at all — MarkVerified is atomic)
import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// ArchiveOnce runs one archive batch for a worker. Returns the archived
// pack id ("" when nothing was pending). policyVersion is recorded in
// the manifest; retentionDays drives the COMPLIANCE lock window.
func ArchiveOnce(ctx context.Context, s Store, obj ObjectStore, consumerID string, batchSize int, policyVersion string, retentionDays int, now func() time.Time) (string, error) {
	if now == nil {
		now = time.Now
	}
	packID, records, created, err := s.ClaimWindow(ctx, consumerID, batchSize, policyVersion, retentionDays)
	if err != nil {
		return "", err
	}
	if packID == "" {
		return "", nil // nothing to archive
	}
	if !created {
		// the same logical pack is already being driven (concurrent
		// worker or crashed predecessor) — converge: do not re-archive
		return packID, nil
	}
	return packID, archivePack(ctx, s, obj, packID, records, policyVersion, retentionDays, now)
}

// archivePack drives one claimed pack to VERIFIED (or failure).
func archivePack(ctx context.Context, s Store, obj ObjectStore, packID string, records []Record, policyVersion string, retentionDays int, now func() time.Time) error {
	p, err := s.Get(ctx, packID)
	if err != nil {
		return err
	}
	retainUntil := now().AddDate(0, 0, retentionDays)

	switch p.State {
	case StatePending, StateRetryable:
		if err := s.MarkWriting(ctx, packID, p.Bucket, p.ObjectKey); err != nil {
			return err
		}
		p.State = StateWriting
		fallthrough
	case StateWriting:
		manifest := BuildManifest(packID, policyVersion, records, retentionDays, now())
		digest := "sha256:" + hex.EncodeToString(func() []byte { h := sha256.Sum256(manifest); return h[:] }())

		version, err := obj.PutLocked(ctx, p.Bucket, p.ObjectKey, manifest,
			"application/json", retainUntil)
		if err != nil {
			_ = s.MarkFailed(ctx, packID, StateRetryable, ReasonStorageRetryable+": "+err.Error()) //nolint:errcheck // primary error below
			return err
		}
		if err := s.MarkWritten(ctx, packID, version, digest); err != nil {
			return err
		}
		p.ObjectVersion, p.ManifestDigest, p.State = version, digest, StateWriting

		// verify the stored object digest server-side before locking the
		// ledger state
		got, err := obj.Head(ctx, p.Bucket, p.ObjectKey, version)
		if err != nil {
			_ = s.MarkFailed(ctx, packID, StateRetryable, ReasonStorageRetryable+": "+err.Error()) //nolint:errcheck
			return err
		}
		if got != digest {
			_ = s.MarkFailed(ctx, packID, StateQuarantined,
				fmt.Sprintf("%s: stored %s, computed %s", ReasonDigestMismatch, got, digest)) //nolint:errcheck
			return errT(ReasonDigestMismatch, "stored manifest digest differs from computed")
		}
		fallthrough
	case StateLocked:
		if p.State == StateWriting {
			if err := s.MarkLocked(ctx, packID, retainUntil); err != nil {
				return err
			}
			p.State = StateLocked
		}
		// the lock must be verifiable: retention readable back
		lockUntil, err := obj.RetentionUntil(ctx, p.Bucket, p.ObjectKey, p.ObjectVersion)
		if err != nil {
			return err
		}
		if lockUntil == nil || lockUntil.Before(retainUntil.Add(-time.Minute)) {
			return errT(ReasonDigestMismatch, "object lock retention does not cover the pack window")
		}
		if err := s.MarkVerified(ctx, packID, records, p.ObjectVersion); err != nil {
			return err
		}
		return nil
	default:
		return errT(ReasonInvalidTransition, "pack is already terminal: "+p.State)
	}
}

// Recover resumes a crashed pack (any mid-flight state) to a terminal
// state. Used by the four-window crash-recovery drills: the caller
// simulates the crash (e.g. kills the process after window N), then
// runs Recover on the same pack.
func Recover(ctx context.Context, s Store, obj ObjectStore, packID string, policyVersion string, retentionDays int, now func() time.Time) error {
	p, err := s.Get(ctx, packID)
	if err != nil {
		return err
	}
	switch p.State {
	case StateVerified, StateQuarantined:
		return nil // already terminal
	case StatePending:
		// never claimed records — re-claim via a fresh batch is the
		// normal path; a direct recover re-runs the window
		return errT(ReasonInvalidTransition, "PENDING pack recovers through the normal batch loop")
	default:
		// WRITING / LOCKED / RETRYABLE: load the window records and re-drive
		rows, err := s.Pool.Query(ctx, `
			SELECT id, event_id, payload_digest, tenant_ref, occurred_at
			FROM saoaf.evidence_record
			WHERE id BETWEEN $1 AND $2 AND state = 'LINKED'
			ORDER BY id`, p.FirstRecordID, p.LastRecordID)
		if err != nil {
			return err
		}
		defer rows.Close()
		var records []Record
		for rows.Next() {
			var r Record
			var occurredAt time.Time
			if err := rows.Scan(&r.ID, &r.EventID, &r.PayloadDigest, &r.TenantRef, &occurredAt); err != nil {
				return err
			}
			r.OccurredAt = occurredAt
			records = append(records, r)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(records) != p.RecordCount {
			return errT(ReasonDigestMismatch,
				fmt.Sprintf("record window drift: pack %d, recovered %d", p.RecordCount, len(records)))
		}
		return archivePack(ctx, s, obj, packID, records, policyVersion, retentionDays, now)
	}
}

// IsRetryable reports whether an archive error is a transient storage
// failure (drives the RETRYABLE classification).
func IsRetryable(err error) bool {
	var e *ErrTyped
	return errors.As(err, &e) && e.Reason == ReasonStorageRetryable
}

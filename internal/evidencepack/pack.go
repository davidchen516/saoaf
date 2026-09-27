// Package evidencepack implements the I23 Evidence WORM archive chain:
// building Evidence Packs from the online evidence index, writing them to
// an S3-compatible WORM object store, locking them (COMPLIANCE retention),
// verifying digests and linking the online index to the locked object
// versions.
//
// Pack state machine (issue core acceptance logic):
//
//	PENDING → WRITING → LOCKED → VERIFIED
//	          (failure) → RETRYABLE | QUARANTINED
//
// Invariants this module owns:
//   - idempotent logical packs: pack_id is content-derived; two workers
//     archiving the same window produce ONE logical result (unique
//     pack_id + CAS transitions + record-level UNIQUE links)
//   - the online index never references a missing or unlocked object
//     version (links commit only in the VERIFIED transition)
//   - retention only extends, never shortens; legal hold outranks
//     lifecycle cleanup (the object store's Object Lock COMPLIANCE mode
//     enforces deletion/shrink refusal — this module records and
//     verifies it, it does not implement WORM itself)
//   - crash recovery at four windows (body/manifest, manifest/lock,
//     lock/link, link/verify): resuming the same pack converges without
//     loss, duplication or unverified success
//
// The manifest carries ONLY allowed metadata (record ids, digests,
// retention, policy version, tenant alias) — archived content stays the
// minimal event payloads the evidence index already stores; prompts,
// tool parameters, credentials and business payloads are never archived
// (mirrors the index's payload hygiene).
package evidencepack

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// Pack states.
const (
	StatePending     = "PENDING"
	StateWriting     = "WRITING"
	StateLocked      = "LOCKED"
	StateVerified    = "VERIFIED"
	StateRetryable   = "RETRYABLE"
	StateQuarantined = "QUARANTINED"
)

// ValidTransitions documents the pack lifecycle matrix — the ENFORCEMENT
// authority is the SQL WHERE clauses in store.go (single source); this
// matrix mirrors them exactly for readability and test-time validation.
var ValidTransitions = map[string][]string{
	StatePending:     {StateWriting, StateRetryable, StateQuarantined},
	StateWriting:     {StateLocked, StateRetryable, StateQuarantined},
	StateRetryable:   {StateWriting, StateQuarantined},
	StateLocked:      {StateVerified},
	StateQuarantined: {},
	StateVerified:    {},
}

// ValidateTransition reports whether from→to is legal.
func ValidateTransition(from, to string) bool {
	for _, t := range ValidTransitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

// ErrTyped carries a machine reason (retryable vs quarantined).
type ErrTyped struct {
	Reason string
	Msg    string
}

func (e *ErrTyped) Error() string { return e.Reason + ": " + e.Msg }

func errT(reason, msg string) error { return &ErrTyped{Reason: reason, Msg: msg} }

// Reason codes.
const (
	ReasonInvalidTransition = "EVIDENCEPACK_INVALID_TRANSITION"
	ReasonNotFound          = "EVIDENCEPACK_NOT_FOUND"
	ReasonStorageRetryable  = "EVIDENCEPACK_STORAGE_RETRYABLE"
	ReasonDigestMismatch    = "EVIDENCEPACK_DIGEST_MISMATCH"
	ReasonRetentionShrink   = "EVIDENCEPACK_RETENTION_SHRINK"
)

// Record is one evidence record selected for a pack.
type Record struct {
	ID            int64
	EventID       string
	PayloadDigest string
	TenantRef     string
	OccurredAt    time.Time
}

// Pack is the archive pack aggregate.
type Pack struct {
	PackID         string
	State          string
	Bucket         string
	ObjectKey      string
	ObjectVersion  string
	ManifestDigest string
	RetentionUntil *time.Time
	LegalHold      bool
	RecordCount    int
	FirstRecordID  int64
	LastRecordID   int64
	AttemptCount   int
	ErrorReason    string
}

// ObjectStore abstracts the S3-compatible WORM store. Implementations
// must: upload with versioning enabled, apply COMPLIANCE Object Lock on
// PutLocked, and refuse delete/shrink server-side (MinIO implements
// versioning + retention but is NOT a WORM-compliance evidence source —
// production admission requires the enterprise store; see evidence/i23).
type ObjectStore interface {
	// Put uploads bytes and returns the object version id.
	Put(ctx context.Context, bucket, key string, body []byte, contentType string) (version string, err error)
	// PutLocked uploads bytes under COMPLIANCE retention until the given
	// time and returns the version id.
	PutLocked(ctx context.Context, bucket, key string, body []byte, contentType string, retainUntil time.Time) (version string, err error)
	// Head fetchs size + digest metadata for a version.
	Head(ctx context.Context, bucket, key, version string) (digest string, err error)
	// ExtendRetention moves the lock deadline LATER only (shrinking is
	// refused by the store; this surface exists for the extend-only rule).
	ExtendRetention(ctx context.Context, bucket, key, version string, retainUntil time.Time) error
	// SetLegalHold / ClearLegalHold manage the hold flag (clear requires
	// authorization — see the caller's checks).
	SetLegalHold(ctx context.Context, bucket, key, version string, hold bool) error
	// RetentionUntil returns the lock deadline for a version.
	RetentionUntil(ctx context.Context, bucket, key, version string) (*time.Time, error)
}

// manifest is the pack manifest: allowed metadata only.
type manifest struct {
	PackID        string        `json:"pack_id"`
	PolicyVersion string        `json:"policy_version"`
	Records       []manifestRec `json:"records"`
	FirstRecordID int64         `json:"first_record_id"`
	LastRecordID  int64         `json:"last_record_id"`
	GeneratedAt   string        `json:"generated_at"`
	RetentionDays int           `json:"retention_days"`
}

type manifestRec struct {
	RecordID      int64  `json:"record_id"`
	EventID       string `json:"event_id"`
	PayloadDigest string `json:"payload_digest"`
	TenantAlias   string `json:"tenant_alias"`
	OccurredAt    string `json:"occurred_at"`
}

// Digest computes the pack id: content-derived from the canonical record
// window (sorted event ids + payload digests + policy version) so the
// same window always yields the same logical pack.
func Digest(policyVersion string, records []Record) string {
	h := sha256.New()
	fmt.Fprintf(h, "policy:%s\n", policyVersion)
	ids := make([]string, 0, len(records))
	for _, r := range records {
		ids = append(ids, fmt.Sprintf("%d:%s:%s", r.ID, r.EventID, r.PayloadDigest))
	}
	sort.Strings(ids)
	for _, id := range ids {
		fmt.Fprintln(h, id)
	}
	return "pack-" + hex.EncodeToString(h.Sum(nil)[:16])
}

// BuildManifest renders the canonical manifest bytes for a window.
func BuildManifest(packID, policyVersion string, records []Record, retentionDays int, now time.Time) []byte {
	recs := make([]manifestRec, 0, len(records))
	for _, r := range records {
		recs = append(recs, manifestRec{
			RecordID:      r.ID,
			EventID:       r.EventID,
			PayloadDigest: r.PayloadDigest,
			TenantAlias:   aliasTenant(r.TenantRef),
			OccurredAt:    r.OccurredAt.UTC().Format(time.RFC3339),
		})
	}
	m := manifest{
		PackID:        packID,
		PolicyVersion: policyVersion,
		Records:       recs,
		RetentionDays: retentionDays,
		GeneratedAt:   now.UTC().Format(time.RFC3339),
	}
	if len(records) > 0 {
		m.FirstRecordID, m.LastRecordID = records[0].ID, records[len(records)-1].ID
	}
	// canonical: json.Marshal sorts map keys; struct field order is stable
	// and records arrive sorted — deterministic bytes
	b, err := json.Marshal(m)
	if err != nil {
		panic("manifest marshal: " + err.Error())
	}
	return b
}

// aliasTenant maps a tenant_ref to the allowed alias form for manifests
// (no raw tenant identifiers beyond the alias surface the policy allows).
func aliasTenant(tenant string) string {
	if tenant == "" {
		return "unattributed"
	}
	return "t-" + tenant
}

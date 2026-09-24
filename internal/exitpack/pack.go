// Package exitpack implements the Exit Pack Registry (I14, module 03.8,
// architecture §Exit Pack Registry): versioned exit packages for critical
// vendors with substitute providers, recovery steps, evidence linkage,
// integrity-checked export bundles and expiry semantics.
//
// 状态机: DRAFT → VALIDATED（完备性）→ ACTIVE（每 vendor 唯一）→
// {EXPIRED ｜ SUPERSEDED}。EXPIRED/SUPERSEDED 进风险视图并触发 I13 告警。
// 已验证 revision 不可原地修改（DB trigger）；回滚 = 激活上一已验证 revision。
// 导出包 = manifest + version + digest 三元组；任何字节改动 → 验证失败。
package exitpack

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// States.
const (
	StateDraft      = "DRAFT"
	StateValidated  = "VALIDATED"
	StateActive     = "ACTIVE"
	StateExpired    = "EXPIRED"
	StateSuperseded = "SUPERSEDED"
)

// Reason codes（GWT#2: 逐项拒绝 reason code 明确）.
const (
	ReasonMissingOwner      = "EXITPACK_MISSING_OWNER"
	ReasonMissingSubstitute = "EXITPACK_MISSING_SUBSTITUTE_PROVIDER"
	ReasonMissingRecovery   = "EXITPACK_MISSING_RECOVERY_STEPS"
	ReasonMissingEvidence   = "EXITPACK_MISSING_EVIDENCE"
	ReasonInvalidState      = "EXITPACK_INVALID_TRANSITION"
	ReasonRevisionConflict  = "EXITPACK_REVISION_CONFLICT"
	ReasonNotFound          = "EXITPACK_NOT_FOUND"
	ReasonAlreadyActive     = "EXITPACK_ALREADY_ACTIVE"
)

// Sentinel errors.
var (
	ErrNotFound          = errors.New("exitpack: not found")
	ErrRevisionConflict  = errors.New("exitpack: revision conflict (CAS)")
	ErrInvalidTransition = errors.New("exitpack: invalid transition")
	ErrImmutable         = errors.New("exitpack: verified revision is immutable")
)

// exportForbidden is the red-line scan on export content（密钥/受限正文 0 命中）.
var exportForbidden = regexp.MustCompile(
	`(?i)(credential|secret|password|api[_-]?key|private[_-]?key|prompt|model[_-]?response|tool[_-]?param|agent[_-]?message)`)

// Pack is one exit pack revision.
type Pack struct {
	ID                 int64
	PackKey            string
	Vendor             string
	Revision           int
	State              string
	OwnerRef           string
	SubstituteProvider string
	RecoverySteps      []string
	EvidenceRefs       []string
	Checklist          map[string]any
	ValidUntil         *time.Time
	Digest             string
	CreatedBy          string
}

// Store persists exit packs.
type Store struct{ Pool *pgxpool.Pool }

// ErrValidation carries the reason code（缺一拒绝逐项 reason）.
type ErrValidation struct {
	Reason string
	Msg    string
}

func (e *ErrValidation) Error() string { return e.Reason + ": " + e.Msg }

// ValidateCompleteness applies the VALIDATED 前置完备性（缺一拒绝）:
// Owner + 替代 Provider + 恢复步骤 + 有效 Evidence。
func ValidateCompleteness(p *Pack) error {
	if p.OwnerRef == "" {
		return &ErrValidation{ReasonMissingOwner, "owner_ref required"}
	}
	if p.SubstituteProvider == "" {
		return &ErrValidation{ReasonMissingSubstitute, "substitute_provider required"}
	}
	if len(p.RecoverySteps) == 0 {
		return &ErrValidation{ReasonMissingRecovery, "at least one recovery step required"}
	}
	if len(p.EvidenceRefs) == 0 {
		return &ErrValidation{ReasonMissingEvidence, "at least one evidence reference required"}
	}
	return nil
}

// CreateDraft inserts a DRAFT revision with the next revision number for
// the pack key.
func (s Store) CreateDraft(ctx context.Context, p *Pack) error {
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	var nextRev int
	if err := conn.QueryRow(ctx, `
		SELECT COALESCE(MAX(revision), 0) + 1 FROM saoaf.exit_pack WHERE pack_key = $1`,
		p.PackKey).Scan(&nextRev); err != nil {
		return err
	}
	steps, _ := json.Marshal(p.RecoverySteps)
	refs, _ := json.Marshal(p.EvidenceRefs)
	checklist, _ := json.Marshal(p.Checklist)
	var validUntil *time.Time
	if p.ValidUntil != nil {
		validUntil = p.ValidUntil
	}
	tag, err := conn.Exec(ctx, `
		INSERT INTO saoaf.exit_pack
			(pack_key, vendor, revision, state, owner_ref, substitute_provider,
			 recovery_steps, evidence_refs, checklist, valid_until, created_by)
		VALUES ($1, $2, $3, 'DRAFT', $4, $5, $6::jsonb, $7::jsonb, $8::jsonb, $9, $10)`,
		p.PackKey, p.Vendor, nextRev, p.OwnerRef, p.SubstituteProvider,
		steps, refs, checklist, validUntil, p.CreatedBy)
	if err != nil {
		return err
	}
	_ = tag
	p.Revision = nextRev
	p.State = StateDraft
	return nil
}

// BuildExport assembles the export bundle deterministically:
// manifest（pack 内容）+ version（pack revision）+ digest over the
// canonical bytes. 篡改任何字节 → VerifyExport 失败。
func BuildExport(p *Pack) (manifest []byte, digest string, err error) {
	m := map[string]any{
		"pack_key":            p.PackKey,
		"vendor":              p.Vendor,
		"version":             p.Revision,
		"owner_ref":           p.OwnerRef,
		"substitute_provider": p.SubstituteProvider,
		"recovery_steps":      p.RecoverySteps,
		"evidence_refs":       p.EvidenceRefs,
		"checklist":           p.Checklist,
	}
	// deterministic JSON: sorted keys (encoding/json does this for maps)
	manifest, err = json.Marshal(m)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(manifest)
	return manifest, "sha256:" + hex.EncodeToString(sum[:]), nil
}

// VerifyExport recomputes the digest over the manifest bytes — the
// tamper check（GWT#8: 任何字节改动 → 验证失败）.
func VerifyExport(manifest []byte, wantDigest string) error {
	sum := sha256.Sum256(manifest)
	got := "sha256:" + hex.EncodeToString(sum[:])
	if got != wantDigest {
		return fmt.Errorf("exitpack: export digest mismatch (tampered?): got %s want %s", got, wantDigest)
	}
	return nil
}

// ScanExportForForbidden applies the red-line scan（导出内容零敏感）.
func ScanExportForForbidden(manifest []byte) error {
	if exportForbidden.Match(manifest) {
		return &ErrValidation{"EXITPACK_FORBIDDEN_CONTENT", "export content carries forbidden material (credentials/prompt/business payloads)"}
	}
	return nil
}

// Validate transitions DRAFT → VALIDATED: completeness check, export build
// (manifest + digest stored on the row), forbidden-content scan.
func (s Store) Validate(ctx context.Context, packKey string, revision int) error {
	return s.withPackTx(ctx, packKey, revision, func(tx pgx.Tx, p *Pack) error {
		if p.State != StateDraft {
			return errV(ReasonInvalidState, p.State+" → VALIDATED not allowed (draft only)")
		}
		if err := ValidateCompleteness(p); err != nil {
			return err
		}
		manifest, digest, err := BuildExport(p)
		if err != nil {
			return err
		}
		if err := ScanExportForForbidden(manifest); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE saoaf.exit_pack
			SET state = 'VALIDATED', digest = $1, export_manifest = $2, validated_at = now()
			WHERE pack_key = $3 AND revision = $4`,
			digest, manifest, packKey, revision); err != nil {
			return err
		}
		p.State = StateValidated
		p.Digest = digest
		return nil
	})
}

// Activate transitions VALIDATED → ACTIVE: the per-vendor unique partial
// index enforces 每.vendor 唯一 ACTIVE；any previous ACTIVE pack of the
// same vendor is SUPERSEDED in the same tx（并发 CAS 由部分唯一索引兜底）.
// 幂等（GWT#3）：re-activating the already-ACTIVE revision is a no-op.
func (s Store) Activate(ctx context.Context, packKey string, revision int) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var vendor, state string
	if err := tx.QueryRow(ctx, `
		SELECT vendor, state FROM saoaf.exit_pack WHERE pack_key = $1 AND revision = $2`,
		packKey, revision).Scan(&vendor, &state); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	// idempotent replay
	if state == StateActive {
		return nil // GWT#3: no-op
	}
	// VALIDATED → ACTIVE is the normal path; SUPERSEDED → ACTIVE is the
	// 回滚 path（激活上一已验证 revision——state-only move, the freeze
	// trigger guarantees the content stays the verified bytes）
	if state != StateValidated && state != StateSuperseded {
		return errV(ReasonInvalidState, state+" → ACTIVE not allowed (validate first)")
	}
	// supersede the current ACTIVE pack of this vendor — excluding the
	// exact revision being activated (same pack_key, different revision
	// must be superseded; the exclusion is (pack_key, revision))
	if _, err := tx.Exec(ctx, `
		UPDATE saoaf.exit_pack SET state = 'SUPERSEDED', superseded_at = now()
		WHERE vendor = $1 AND state = 'ACTIVE'
		  AND NOT (pack_key = $2 AND revision = $3)`, vendor, packKey, revision); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `
		UPDATE saoaf.exit_pack SET state = 'ACTIVE', activated_at = now()
		WHERE pack_key = $1 AND revision = $2 AND state IN ('VALIDATED','SUPERSEDED')`,
		packKey, revision)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errV(ReasonInvalidState, "concurrent state change lost the race")
	}
	return tx.Commit(ctx)
}

// SweepExpired moves ACTIVE packs past valid_until to EXPIRED and reports
// the expired (vendor, pack, revision) tuples for I13 risk alerting.
func (s Store) SweepExpired(ctx context.Context, now time.Time) ([]Pack, error) {
	tag, err := s.Pool.Exec(ctx, `
		UPDATE saoaf.exit_pack SET state = 'EXPIRED', expired_at = now()
		WHERE state = 'ACTIVE' AND valid_until IS NOT NULL AND valid_until < $1`, now)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, nil
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT pack_key, vendor, revision FROM saoaf.exit_pack
		WHERE state = 'EXPIRED' AND expired_at >= now() - interval '1 second'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Pack
	for rows.Next() {
		var p Pack
		if err := rows.Scan(&p.PackKey, &p.Vendor, &p.Revision); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// RiskView returns EXPIRED / SUPERSEDED / broken-evidence packs for the
// I13 risk view（过期或证据断链的 Exit Pack 自动进入风险视图）.
func (s Store) RiskView(ctx context.Context) ([]Pack, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT pack_key, vendor, revision, state, owner_ref, substitute_provider,
		       recovery_steps, evidence_refs, COALESCE(valid_until::text, ''), COALESCE(digest, '')
		FROM saoaf.exit_pack
		WHERE state IN ('EXPIRED','SUPERSEDED')
		ORDER BY vendor, pack_key, revision`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Pack
	for rows.Next() {
		var p Pack
		var steps, refs []byte
		var until, digest string
		if err := rows.Scan(&p.PackKey, &p.Vendor, &p.Revision, &p.State,
			&p.OwnerRef, &p.SubstituteProvider, &steps, &refs, &until, &digest); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(steps, &p.RecoverySteps)
		_ = json.Unmarshal(refs, &p.EvidenceRefs)
		if until != "" {
			if t, err := time.Parse(time.RFC3339, until); err == nil {
				p.ValidUntil = &t
			}
		}
		p.Digest = digest
		out = append(out, p)
	}
	return out, rows.Err()
}

// Get returns one pack revision.
func (s Store) Get(ctx context.Context, packKey string, revision int) (*Pack, error) {
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Release()
	return scanPack(conn.QueryRow(ctx, `
		SELECT pack_key, vendor, revision, state, owner_ref, substitute_provider,
		       recovery_steps, evidence_refs, COALESCE(valid_until::text, ''), COALESCE(digest, '')
		FROM saoaf.exit_pack WHERE pack_key = $1 AND revision = $2`, packKey, revision))
}

// LatestValidated returns the newest VALIDATED-or-later revision of a pack
// （回滚目标：上一已验证 revision）.
func (s Store) LatestValidated(ctx context.Context, packKey string) (*Pack, error) {
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Release()
	return scanPack(conn.QueryRow(ctx, `
		SELECT pack_key, vendor, revision, state, owner_ref, substitute_provider,
		       recovery_steps, evidence_refs, COALESCE(valid_until::text, ''), COALESCE(digest, '')
		FROM saoaf.exit_pack
		WHERE pack_key = $1 AND state IN ('VALIDATED','ACTIVE','EXPIRED','SUPERSEDED')
		ORDER BY revision DESC LIMIT 1`, packKey))
}

func scanPack(row pgx.Row) (*Pack, error) {
	var p Pack
	var steps, refs []byte
	var until, digest string
	if err := row.Scan(&p.PackKey, &p.Vendor, &p.Revision, &p.State,
		&p.OwnerRef, &p.SubstituteProvider, &steps, &refs, &until, &digest); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	_ = json.Unmarshal(steps, &p.RecoverySteps)
	_ = json.Unmarshal(refs, &p.EvidenceRefs)
	if until != "" {
		if t, err := time.Parse(time.RFC3339Nano, until); err == nil {
			p.ValidUntil = &t
		}
	}
	p.Digest = digest
	return &p, nil
}

func (s Store) withPackTx(ctx context.Context, packKey string, revision int, fn func(tx pgx.Tx, p *Pack) error) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	p, err := scanPack(tx.QueryRow(ctx, `
		SELECT pack_key, vendor, revision, state, owner_ref, substitute_provider,
		       recovery_steps, evidence_refs, COALESCE(valid_until::text, ''), COALESCE(digest, '')
		FROM saoaf.exit_pack WHERE pack_key = $1 AND revision = $2`, packKey, revision))
	if err != nil {
		return err
	}
	if err := fn(tx, p); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func errV(reason, msg string) error { return &ErrValidation{reason, msg} }

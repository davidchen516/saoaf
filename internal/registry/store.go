package registry

// pgx-backed store (I07): lifecycle transitions with revision CAS, snapshot
// submission with order/digest checks, active pointer, publishable queries.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

var (
	// ErrNotFound is the coarse not-found.
	ErrNotFound = errors.New("registry: not found")
	// ErrRevisionConflict is the CAS failure (concurrent update lost race).
	ErrRevisionConflict = errors.New("registry: revision conflict (CAS)")
	// ErrDuplicate is the unique-constraint rejection (idempotent replay
	// with different content or out-of-order snapshot).
	ErrDuplicate = errors.New("registry: duplicate/conflict")
)

// Store persists the registry.
type Store struct{ DSN string }

func (s Store) connect(ctx context.Context) (*pgx.Conn, error) {
	return pgx.Connect(ctx, s.DSN)
}

// TransitionProvider applies a lifecycle transition with revision CAS:
// the UPDATE matches expectedRevision; a concurrent writer bumping the
// revision first makes our match fail → ErrRevisionConflict.
func (s Store) TransitionProvider(ctx context.Context, providerKey string, expectedRevision int, from, to State) error {
	// P1-1: the lifecycle matrix is enforced at the ONLY write path —
	// raw SQL can still update state (admin/recovery) but the store API
	// never allows an illegal jump (issue: 全矩阵).
	if !ValidateTransition(from, to) {
		return errV(ReasonInvalidTransition, string(from)+" → "+string(to))
	}
	conn, err := s.connect(ctx)
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
		UPDATE registry.resource_provider
		SET state = $1, revision = revision + 1,
		    published_at = CASE WHEN $1 = 'PUBLISHED' THEN now() ELSE published_at END
		WHERE provider_key = $2 AND revision = $3 AND state = $4`,
		string(to), providerKey, expectedRevision, string(from))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		// either wrong revision (CAS), wrong from-state, or not found
		var curRev int
		var curState string
		qerr := tx.QueryRow(ctx,
			`SELECT revision, state FROM registry.resource_provider WHERE provider_key = $1`,
			providerKey).Scan(&curRev, &curState)
		if qerr == pgx.ErrNoRows {
			return ErrNotFound
		}
		if qerr != nil {
			return qerr
		}
		if curRev != expectedRevision {
			return fmt.Errorf("%w: expected revision %d, current %d", ErrRevisionConflict, expectedRevision, curRev)
		}
		return errV(ReasonInvalidTransition, curState+" → "+string(to))
	}
	return tx.Commit(ctx)
}

// SubmitSnapshot inserts a snapshot; duplicate (provider_id, version) →
// ErrDuplicate (covers both replay and same-version-different-digest).
// Profiles are validated recursively for forbidden fields (P2-1).
func (s Store) SubmitSnapshot(ctx context.Context, snap *Snapshot, profiles map[string]any) error {
	if err := CheckForbiddenFieldsRecursive(profiles); err != nil {
		return err
	}
	conn, err := s.connect(ctx)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	// monotonic version: reject lower-or-equal than the max existing
	var maxVer int
	err = conn.QueryRow(ctx, `
		SELECT COALESCE(max(snapshot_version), 0)
		FROM registry.provider_snapshot WHERE provider_id = $1`, snap.ProviderID).Scan(&maxVer)
	if err != nil {
		return err
	}
	if snap.SnapshotVersion <= maxVer {
		return errV(ReasonOutOfOrder, fmt.Sprintf("snapshot_version %d <= max %d", snap.SnapshotVersion, maxVer))
	}
	_, err = conn.Exec(ctx, `
		INSERT INTO registry.provider_snapshot
			(provider_id, snapshot_version, contract_version, digest, signature,
			 workload_identity, generated_at, valid_until, state)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'DRAFT')`,
		snap.ProviderID, snap.SnapshotVersion, snap.ContractVersion, snap.Digest,
		snap.Signature, snap.WorkloadIdentity, snap.GeneratedAt, snap.ValidUntil)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: (provider, snapshot_version) exists", ErrDuplicate)
		}
		return err
	}
	return nil
}

// ActivateSnapshot validates + publishes the snapshot and swings the active
// pointer in one transaction.
func (s Store) ActivateSnapshot(ctx context.Context, snap *Snapshot, profiles map[string]any, expectedWorkload, expectedContractMajor string, now func() time.Time) error {
	if err := ValidateSnapshot(snap, profiles, expectedWorkload, expectedContractMajor, now()); err != nil {
		return err
	}
	conn, err := s.connect(ctx)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// For first-time activation: transition DRAFT/VALIDATED → PUBLISHED.
	// For rollback: the snapshot is ALREADY PUBLISHED — the pointer just
	// moves back to it (issue Rollback: 切换 active pointer 到上一已发布
	// revision). The UPDATE is a no-op when already PUBLISHED (0 rows
	// affected is legal for the rollback path).
	_, err = tx.Exec(ctx, `
		UPDATE registry.provider_snapshot
		SET state = 'PUBLISHED'
		WHERE provider_id = $1 AND snapshot_version = $2 AND state IN ('DRAFT','VALIDATED')`,
		snap.ProviderID, snap.SnapshotVersion)
	if err != nil {
		return err
	}
	var snapID int64
	var snapState string
	if err := tx.QueryRow(ctx, `
		SELECT id, state FROM registry.provider_snapshot
		WHERE provider_id = $1 AND snapshot_version = $2`,
		snap.ProviderID, snap.SnapshotVersion).Scan(&snapID, &snapState); err != nil {
		return err
	}
	// P2-4: the pointer must point to a PUBLISHED snapshot — never DRAFT
	// or SUSPENDED (issue invariant: active pointer 指向 PUBLISHED revision)
	if snapState != "PUBLISHED" {
		return errV(ReasonInvalidTransition, "cannot point active pointer at state "+snapState)
	}
	// upsert active pointer (exactly one per provider)
	if _, err := tx.Exec(ctx, `
		INSERT INTO registry.provider_active_pointer (provider_id, snapshot_id, activated_at)
		VALUES ($1, $2, now())
		ON CONFLICT (provider_id) DO UPDATE SET snapshot_id = $2, activated_at = now()`,
		snap.ProviderID, snapID); err != nil {
		return err
	}
	// I10/I11: provider.snapshot-changed outbox event + audit row in the
	// SAME tx (半提交=0；事件目录 specs §5.2). Minimal payload: provider
	// key + snapshot version + digest — never the full profile content.
	var providerKey string
	if err := tx.QueryRow(ctx, `
		SELECT provider_key FROM registry.resource_provider WHERE id = $1`,
		snap.ProviderID).Scan(&providerKey); err != nil {
		return err
	}
	// audit actor: the verified workload identity (empty for legacy
	// callers → sentinel; the change_record CHECK requires non-empty)
	auditActor := expectedWorkload
	if auditActor == "" {
		auditActor = "platform:snapshot-activation"
	}
	var auditID int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO saoaf.change_record
			(tenant_ref, actor, trace_id, entity_kind, entity_id, operation, decision_ref, summary)
		VALUES ($1, $2, '', 'provider-snapshot', $3, 'ACTIVATE', $4, $5)
		RETURNING id`,
		"_platform", auditActor, providerKey, snap.Signature,
		map[string]any{"snapshot_version": snap.SnapshotVersion, "digest": snap.Digest}).Scan(&auditID); err != nil {
		return err
	}
	// the event fires ONCE per (provider, snapshot_version) — a pointer
	// ROLLBACK back to an already-announced snapshot re-announces nothing
	// (the outbox schema's unique keys carry that semantics); the audit
	// row above still records the rollback action itself.
	if _, err := tx.Exec(ctx, `
		INSERT INTO saoaf.outbox_event
			(topic, payload, change_record_id, event_id, aggregate_kind, aggregate_id, aggregate_revision)
		VALUES ('provider.snapshot-changed', $1::jsonb, $2, $3, 'provider', $4, $5)
		ON CONFLICT DO NOTHING`,
		[]byte(fmt.Sprintf(`{"provider_id":%q,"snapshot_version":%d,"digest":%q,"valid_until":%q}`,
			providerKey, snap.SnapshotVersion, snap.Digest, snap.ValidUntil.UTC().Format(time.RFC3339Nano))),
		auditID, fmt.Sprintf("provider-snapshot:%s:%d", providerKey, snap.SnapshotVersion),
		providerKey, snap.SnapshotVersion); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// IngestVerdict classifies a snapshot ingest against existing state
// (spec §3.2).
type IngestVerdict int

const (
	IngestNew               IngestVerdict = iota // higher version than any existing
	IngestIdempotent                             // same version AND digest AND PUBLISHED — true no-op
	IngestResumable                              // same version AND digest but NOT PUBLISHED — activation retry
	IngestDigestConflict                         // same version, different digest — reject + alarm
	IngestVersionRegression                      // version <= an existing higher version (non-monotonic)
)

// CheckSnapshotIngest classifies the incoming (version, digest) for the
// provider against existing rows. Idempotency (review R1 P1) requires the
// snapshot to be PUBLISHED — a half-committed DRAFT row (submit succeeded,
// activate failed or crashed) must NOT be reported as idempotent: the
// replay resumes activation instead (IngestResumable), so a failed window
// can never leave a version stuck behind a fake success.
func (s Store) CheckSnapshotIngest(ctx context.Context, providerKey string, version int, digest string) (IngestVerdict, error) {
	conn, err := s.connect(ctx)
	if err != nil {
		return IngestNew, err
	}
	defer conn.Close(ctx)
	var atDigest, atState string
	err = conn.QueryRow(ctx, `
		SELECT ps.digest, ps.state FROM registry.provider_snapshot ps
		JOIN registry.resource_provider rp ON rp.id = ps.provider_id
		WHERE rp.provider_key = $1 AND ps.snapshot_version = $2`,
		providerKey, version).Scan(&atDigest, &atState)
	if errors.Is(err, pgx.ErrNoRows) {
		// not found at this version — monotonicity check below
	} else if err != nil {
		return IngestNew, err
	} else {
		if atDigest != digest {
			return IngestDigestConflict, nil
		}
		if atState == "PUBLISHED" {
			return IngestIdempotent, nil
		}
		return IngestResumable, nil
	}
	var maxVersion int
	if err := conn.QueryRow(ctx, `
		SELECT COALESCE(MAX(ps.snapshot_version), 0)
		FROM registry.provider_snapshot ps
		JOIN registry.resource_provider rp ON rp.id = ps.provider_id
		WHERE rp.provider_key = $1`, providerKey).Scan(&maxVersion); err != nil {
		return IngestNew, err
	}
	if maxVersion > 0 && version < maxVersion {
		return IngestVersionRegression, nil
	}
	return IngestNew, nil
}

// ProviderByKey returns the provider row for a logical key.
func (s Store) ProviderByKey(ctx context.Context, providerKey string) (*Provider, error) {
	conn, err := s.connect(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close(ctx)
	var p Provider
	var st string
	err = conn.QueryRow(ctx, `
		SELECT id, provider_key, provider_type, endpoint_ref, owner_ref, workload_identity, state
		FROM registry.resource_provider WHERE provider_key = $1`, providerKey).
		Scan(&p.ID, &p.ProviderKey, &p.ProviderType, &p.EndpointRef, &p.OwnerRef, &p.WorkloadIdentity, &st)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	p.State = State(st)
	return &p, nil
}

// PublishableProviders returns providers eligible for new binding/plan
// usage: PUBLISHED state only (issue: RETIRED/SUSPENDED 不出现在可发布候选).
func (s Store) PublishableProviders(ctx context.Context, providerType string) ([]Provider, error) {
	conn, err := s.connect(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close(ctx)
	rows, err := conn.Query(ctx, `
		SELECT id, provider_key, provider_type, endpoint_ref, owner_ref, workload_identity, state, revision, active_revision
		FROM registry.resource_provider
		WHERE state = 'PUBLISHED' AND provider_type = $1
		ORDER BY provider_key`, providerType)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Provider
	for rows.Next() {
		var p Provider
		var st string
		if err := rows.Scan(&p.ID, &p.ProviderKey, &p.ProviderType, &p.EndpointRef,
			&p.OwnerRef, &p.WorkloadIdentity, &st, &p.Revision, &p.ActiveRevision); err != nil {
			return nil, err
		}
		p.State = State(st)
		out = append(out, p)
	}
	return out, rows.Err()
}

// ActiveSnapshot returns the active pointer's snapshot for a provider.
func (s Store) ActiveSnapshot(ctx context.Context, providerID int64) (*Snapshot, error) {
	conn, err := s.connect(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close(ctx)
	var s2 Snapshot
	var st string
	err = conn.QueryRow(ctx, `
		SELECT ps.id, ps.provider_id, ps.snapshot_version, ps.contract_version,
		       ps.digest, ps.signature, ps.workload_identity, ps.generated_at, ps.valid_until, ps.state
		FROM registry.provider_active_pointer pap
		JOIN registry.provider_snapshot ps ON ps.id = pap.snapshot_id
		WHERE pap.provider_id = $1`, providerID).
		Scan(&s2.ID, &s2.ProviderID, &s2.SnapshotVersion, &s2.ContractVersion,
			&s2.Digest, &s2.Signature, &s2.WorkloadIdentity, &s2.GeneratedAt, &s2.ValidUntil, &st)
	if err == pgx.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	s2.State = State(st)
	return &s2, nil
}

func isUniqueViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	if errors.As(err, &pgErr) {
		return pgErr.SQLState() == "23505"
	}
	return false
}

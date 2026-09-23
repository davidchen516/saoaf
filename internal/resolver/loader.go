package resolver

// Registry-backed snapshot loader (I09): loads a binding's PINNED snapshot
// and its provider, classifying "no longer the provider's active published
// snapshot" as ErrSnapshotUnavailable → PROVIDER_SNAPSHOT_EXPIRED.
// Determinism: resolution uses the binding's pinned snapshot (the revision
// set pins it), and requires it to still be the provider's active pointer —
// a new snapshot publication forces bindings to be re-published rather
// than silently serving stale provider content.

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// NewRegistrySnapshotLoader returns a SnapshotLoader over the registry
// tables (pooled: the resolver's QPS budget forbids per-call connects).
func NewRegistrySnapshotLoader(pool *pgxpool.Pool) SnapshotLoader {
	return func(ctx context.Context, snapshotID int64) (*LoadedSnapshot, error) {
		conn, err := pool.Acquire(ctx)
		if err != nil {
			return nil, err // transport failure → RESOLVER_UNAVAILABLE
		}
		defer conn.Release()
		var snap LoadedSnapshot
		var profilesJSON []byte
		var activeID *int64
		err = conn.QueryRow(ctx, `
			SELECT ps.id, ps.provider_id, ps.snapshot_version, ps.contract_version,
			       ps.profiles, ps.generated_at, ps.valid_until,
			       rp.provider_key, rp.provider_type, rp.endpoint_ref,
			       pap.snapshot_id
			FROM registry.provider_snapshot ps
			JOIN registry.resource_provider rp ON rp.id = ps.provider_id
			LEFT JOIN registry.provider_active_pointer pap ON pap.provider_id = ps.provider_id
			WHERE ps.id = $1 AND ps.state = 'PUBLISHED' AND rp.state = 'PUBLISHED'`,
			snapshotID).
			Scan(&snap.SnapshotID, &snap.ProviderID, &snap.SnapshotVersion, &snap.ContractVersion,
				&profilesJSON, &snap.GeneratedAt, &snap.ValidUntil,
				&snap.ProviderKey, &snap.ProviderType, &snap.EndpointRef, &activeID)
		if errors.Is(err, pgx.ErrNoRows) {
			// snapshot unpublished, provider not PUBLISHED, or gone
			return nil, ErrSnapshotUnavailable
		}
		if err != nil {
			return nil, err
		}
		if activeID == nil || *activeID != snapshotID {
			// pinned snapshot is no longer the active pointer
			return nil, ErrSnapshotUnavailable
		}
		if err := json.Unmarshal(profilesJSON, &snap.Profiles); err != nil {
			return nil, err
		}
		return &snap, nil
	}
}

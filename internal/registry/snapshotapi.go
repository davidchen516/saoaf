package registry

// Snapshot ingest API (I11, specs §3.2 + resource-resolver-api §4.3):
// the MMR control plane pushes logical profile Snapshots here. The
// endpoint lives in the registry module (its own management surface —
// ADR-0006); the caller must present an mTLS workload certificate verified
// against the SPIFFE trust domain, and the snapshot must satisfy:
//
//   - digest format + signature presence + validity window
//   - profile states ∈ AVAILABLE/DEGRADED/UNAVAILABLE/RETIRED
//   - monotonic snapshot_version; (version, digest) re-ingest is
//     IDEMPOTENT (no-op, 200); same version + different digest is a
//     contract drift alarm → 409 (spec: 版本相同而 digest 不同必须拒绝并告警)
//
// Only the registry store path mutates rows; the response carries no
// internal model/provider routing detail (禁止字段 boundary).

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/davidchen516/saoaf/internal/platform/httpapi"
	"github.com/davidchen516/saoaf/internal/platform/workload"
)

// SnapshotAPIConfig wires the ingest endpoint.
type SnapshotAPIConfig struct {
	Store            *Store
	WorkloadVerifier *workload.Verifier
	ExpectedIdentity string // e.g. spiffe://saoaf.test/ns/mmr/sa/publisher
	// ExpectedContractMajor is the ARR-side accepted contract major for MMR
	// snapshots (spec §3.2) — configured, never derived from the request
	// itself (review R1 P3-3: the previous self-referential check passed
	// majorOf(req.ContractVersion) as its own expectation and was vacuous).
	ExpectedContractMajor string
	Now                   func() time.Time
}

// profileStateSet (spec §3.2): profile 状态只允许四态.
var profileStateSet = map[string]bool{
	"AVAILABLE": true, "DEGRADED": true, "UNAVAILABLE": true, "RETIRED": true,
}

var digestFormat = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// IngestRequest is the MMR-facing snapshot body (specs §3.1).
type IngestRequest struct {
	ProviderID      string          `json:"provider_id"`
	SnapshotVersion int             `json:"snapshot_version"`
	ContractVersion string          `json:"contract_version"`
	GeneratedAt     string          `json:"generated_at"`
	ValidUntil      string          `json:"valid_until"`
	Profiles        []IngestProfile `json:"profiles"`
	Digest          string          `json:"digest"`
	Signature       string          `json:"signature"`
	TenantRef       string          `json:"tenant_ref"`
}

// IngestProfile is one logical profile entry (minimal ARR-facing fields;
// forbidden fields are rejected below).
type IngestProfile struct {
	ProfileID               string         `json:"profile_id"`
	CapabilityKeys          []string       `json:"capability_keys"`
	Regions                 []string       `json:"regions"`
	DataClassificationMax   string         `json:"data_classification_max"`
	Features                map[string]any `json:"features"`
	ConstraintSchemaVersion string         `json:"constraint_schema_version"`
	Status                  string         `json:"status"`
}

// MountSnapshotAPI wires POST /providers/{provider_key}/snapshots.
func MountSnapshotAPI(r chi.Router, cfg SnapshotAPIConfig) {
	r.Post("/providers/{provider_key}/snapshots", cfg.handleIngest)
}

// rejectReason carries the spec §5 semantic for the response code.
type rejectReason struct {
	Code    string
	Status  int
	Message string
}

func (cfg SnapshotAPIConfig) handleIngest(w http.ResponseWriter, r *http.Request) {
	// 1. workload identity: mTLS client certificate, SPIFFE-verified
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		httpapi.WriteJSON(w, http.StatusForbidden, map[string]any{
			"error_code": "WORKLOAD_IDENTITY_REQUIRED",
			"message":    "snapshot ingest requires an mTLS workload certificate",
		})
		return
	}
	chain := workload.Chain{Leaf: r.TLS.PeerCertificates[0], Intermediates: r.TLS.PeerCertificates[1:]}
	if err := cfg.WorkloadVerifier.Verify(chain); err != nil {
		httpapi.WriteJSON(w, http.StatusForbidden, map[string]any{
			"error_code": "WORKLOAD_IDENTITY_REJECTED",
			"message":    "workload certificate rejected",
		})
		return
	}
	san, err := workload.SPIFFESAN(r.TLS.PeerCertificates[0])
	if err != nil || san != cfg.ExpectedIdentity {
		httpapi.WriteJSON(w, http.StatusForbidden, map[string]any{
			"error_code": "WORKLOAD_IDENTITY_MISMATCH",
			"message":    "snapshot ingest requires the configured publisher identity",
		})
		return
	}

	// 2. body: strict decode, size-capped
	var req IngestRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256*1024))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		httpapi.WriteJSON(w, http.StatusBadRequest, map[string]any{
			"error_code": "INVALID_SNAPSHOT",
			"message":    err.Error(),
		})
		return
	}
	providerKey := chi.URLParam(r, "provider_key")

	// 3. spec §3.2 validation
	if rej := validateIngest(&req, providerKey); rej != nil {
		httpapi.WriteJSON(w, rej.Status, map[string]any{
			"error_code": rej.Code,
			"message":    rej.Message,
		})
		return
	}

	// 4. monotonic version + idempotency + version/digest conflict
	verdict, err := cfg.Store.CheckSnapshotIngest(r.Context(), providerKey, req.SnapshotVersion, req.Digest)
	if err != nil {
		httpapi.WriteJSON(w, http.StatusInternalServerError, map[string]any{
			"error_code": "REGISTRY_UNAVAILABLE", "message": "registry store failed",
		})
		return
	}
	switch verdict {
	case IngestDigestConflict:
		// spec: 相同 snapshot_version 不同 digest → 拒绝并告警（契约漂移）
		httpapi.WriteJSON(w, http.StatusConflict, map[string]any{
			"error_code": "SNAPSHOT_DIGEST_CONFLICT",
			"message":    "same snapshot_version ingested with a different digest (contract drift)",
		})
		return
	case IngestVersionRegression:
		httpapi.WriteJSON(w, http.StatusConflict, map[string]any{
			"error_code": "SNAPSHOT_VERSION_REGRESSION",
			"message":    "snapshot_version must be monotonic",
		})
		return
	case IngestIdempotent:
		// 相同 (version, digest) → 幂等 no-op（MMR 内部变更不触发外部契约变化
		// → ARR Plan 不变，I11 不变性机制）
		httpapi.WriteJSON(w, http.StatusOK, map[string]any{
			"status": "idempotent", "snapshot_version": req.SnapshotVersion,
		})
		return
	}

	// 5. store path: submit + activate (the registry store performs its own
	// snapshot validations: workload identity, contract major, window).
	// IngestResumable skips the submit (row exists, same digest) and
	// retries activation only — the P1 half-commit recovery path.
	nowFn := cfg.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	ctx := r.Context()
	provider, err := cfg.Store.ProviderByKey(ctx, providerKey)
	if err != nil {
		httpapi.WriteJSON(w, http.StatusNotFound, map[string]any{
			"error_code": "PROVIDER_NOT_FOUND", "message": "unknown provider key",
		})
		return
	}
	generatedAt, gerr := time.Parse(time.RFC3339, req.GeneratedAt)
	if gerr != nil {
		httpapi.WriteJSON(w, http.StatusBadRequest, map[string]any{
			"error_code": "INVALID_SNAPSHOT", "message": "generated_at must be RFC3339",
		})
		return
	}
	validUntil, verr := time.Parse(time.RFC3339, req.ValidUntil)
	if verr != nil {
		httpapi.WriteJSON(w, http.StatusBadRequest, map[string]any{
			"error_code": "INVALID_SNAPSHOT", "message": "valid_until must be RFC3339",
		})
		return
	}
	if !validUntil.After(generatedAt) {
		httpapi.WriteJSON(w, http.StatusBadRequest, map[string]any{
			"error_code": "INVALID_WINDOW", "message": "valid_until must be after generated_at",
		})
		return
	}
	snap := &Snapshot{
		ProviderID:       provider.ID,
		SnapshotVersion:  req.SnapshotVersion,
		ContractVersion:  req.ContractVersion,
		Digest:           req.Digest,
		Signature:        req.Signature,
		WorkloadIdentity: cfg.ExpectedIdentity,
		GeneratedAt:      generatedAt,
		ValidUntil:       validUntil,
	}
	profiles := map[string]any{}
	for _, p := range req.Profiles {
		profiles[p.ProfileID] = map[string]any{
			"profile_id":                 p.ProfileID,
			"capability_keys":            p.CapabilityKeys,
			"regions":                    p.Regions,
			"data_classification_max":    p.DataClassificationMax,
			"features":                   p.Features,
			"constraints_schema_version": p.ConstraintSchemaVersion,
			"status":                     p.Status,
		}
	}
	// contract major is a CONFIGURED expectation (spec §3.2), checked
	// BEFORE any write so a mismatch leaves no garbage DRAFT row
	// occupying the version (review R2 P3-3)
	if cfg.ExpectedContractMajor != "" &&
		majorOf(req.ContractVersion) != cfg.ExpectedContractMajor {
		httpapi.WriteJSON(w, http.StatusBadRequest, map[string]any{
			"error_code": "CONTRACT_MAJOR_REJECTED",
			"message":    "snapshot contract major not accepted by ARR (configured expectation)",
		})
		return
	}
	if verdict != IngestResumable {
		if err := cfg.Store.SubmitSnapshot(ctx, snap, profiles); err != nil {
			// concurrent same-version submit won the race (review R1
			// P2-2): re-classify against the committed row instead of
			// surfacing a raw unique-violation as an opaque 400
			reclass, rerr := cfg.Store.CheckSnapshotIngest(ctx, providerKey, req.SnapshotVersion, req.Digest)
			if rerr != nil {
				httpapi.WriteJSON(w, http.StatusBadRequest, map[string]any{
					"error_code": "SNAPSHOT_REJECTED", "message": err.Error(),
				})
				return
			}
			switch reclass {
			case IngestIdempotent:
				httpapi.WriteJSON(w, http.StatusOK, map[string]any{
					"status": "idempotent", "snapshot_version": req.SnapshotVersion,
				})
				return
			case IngestDigestConflict:
				httpapi.WriteJSON(w, http.StatusConflict, map[string]any{
					"error_code": "SNAPSHOT_DIGEST_CONFLICT",
					"message":    "same snapshot_version ingested with a different digest (contract drift)",
				})
				return
			case IngestResumable:
				// fall through: retry activation below
			case IngestVersionRegression:
				// a higher version landed while we were submitting —
				// classify honestly (review R2 P3-1: the default branch
				// surfaced 400 instead of the 409 regression class)
				httpapi.WriteJSON(w, http.StatusConflict, map[string]any{
					"error_code": "SNAPSHOT_VERSION_REGRESSION",
					"message":    "snapshot_version must be monotonic",
				})
				return
			default:
				httpapi.WriteJSON(w, http.StatusBadRequest, map[string]any{
					"error_code": "SNAPSHOT_REJECTED", "message": err.Error(),
				})
				return
			}
		}
	}
	if err := cfg.Store.ActivateSnapshot(ctx, snap, profiles,
		cfg.ExpectedIdentity, majorOf(req.ContractVersion), nowFn); err != nil {
		httpapi.WriteJSON(w, http.StatusBadRequest, map[string]any{
			"error_code": "SNAPSHOT_REJECTED", "message": err.Error(),
		})
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{
		"status": "activated", "snapshot_version": req.SnapshotVersion,
	})
}

// validateIngest applies spec §3.2 rules; nil means OK.
func validateIngest(req *IngestRequest, providerKey string) *rejectReason {
	if req.ProviderID != providerKey {
		return &rejectReason{"PROVIDER_MISMATCH", http.StatusBadRequest,
			fmt.Sprintf("body provider_id %q does not match path", req.ProviderID)}
	}
	if !digestFormat.MatchString(req.Digest) {
		return &rejectReason{"INVALID_DIGEST", http.StatusBadRequest, "digest must be sha256:<64hex>"}
	}
	if req.Signature == "" {
		return &rejectReason{"SIGNATURE_REQUIRED", http.StatusBadRequest, "signature required"}
	}
	if req.SnapshotVersion < 1 {
		return &rejectReason{"INVALID_VERSION", http.StatusBadRequest, "snapshot_version must be >= 1"}
	}
	if len(req.Profiles) == 0 {
		return &rejectReason{"NO_PROFILES", http.StatusBadRequest, "at least one logical profile required"}
	}
	for _, p := range req.Profiles {
		if p.ProfileID == "" {
			return &rejectReason{"INVALID_PROFILE", http.StatusBadRequest, "profile_id required"}
		}
		if !profileStateSet[p.Status] {
			return &rejectReason{"INVALID_PROFILE_STATE", http.StatusBadRequest,
				fmt.Sprintf("profile %s state %q must be AVAILABLE/DEGRADED/UNAVAILABLE/RETIRED", p.ProfileID, p.Status)}
		}
	}
	return nil
}

package resolver

// Resolve service (I09): deterministic pipeline with the FIXED error
// routing order from the issue 核心验收逻辑:
//
//	Schema 校验 → 禁止字段 → Capability 查找 → Policy 过滤（携带 policy
//	revision）→ Binding 消歧 → Snapshot 有效性 → 候选过滤
//
// first hit wins, exactly one reason code per rejection. Determinism: every
// choice is ordered (priority, binding_key) and the fingerprint pins the
// full revision set (capability/binding/snapshot/policy) — the same
// normalized input + revision set + policy revision produces the same
// fingerprint and the same items.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/davidchen516/saoaf/internal/policy"
)

// ErrSnapshotUnavailable marks a binding whose pinned snapshot is no longer
// the provider's active published snapshot (or the snapshot itself is not
// published) — maps to PROVIDER_SNAPSHOT_EXPIRED.
var ErrSnapshotUnavailable = errors.New("resolver: pinned snapshot unavailable")

// CallerMeta is the verified caller identity (from token; request bodies
// must not override identity claims — specs §2).
type CallerMeta struct {
	CallerRef   string
	TenantRef   string
	Environment string
	TraceID     string
}

// Service resolves resource plans.
type Service struct {
	Plans       *Store
	Cache       *SnapshotCache
	Pool        *pgxpool.Pool
	Policy      *policy.Store
	Eval        *policy.Evaluator
	PolicySetID string

	Now        func() time.Time
	NewID      func() string
	DefaultTTL time.Duration
	MaxTTL     time.Duration
}

// planSeq makes NewPlanID unique within a process.
var planSeq atomic.Uint64

// NewPlanID mints a lexicographically-sortable plan ID: timestamp-first
// (RFC3339 compact) + process-unique suffix — ordered IDs keep cursor
// pagination stable (specs §1/§5).
func NewPlanID() string {
	now := time.Now().UTC().Format("20060102T150405.000000000")
	return fmt.Sprintf("plan-%s-%06d", now, planSeq.Add(1))
}

// capabilityRow is the published capability lookup result.
type capabilityRow struct {
	ID                int64
	Revision          int
	ResourceType      string
	RequirementSchema []byte
}

// bindingCandidate is one live binding joined with its provider state.
type bindingCandidate struct {
	BindingKey      string
	Revision        int
	ProviderID      int64
	ProviderKey     string
	ProviderState   string
	SnapshotID      int64
	Profile         string
	Priority        int
	Scope           bindingScope
	ConstraintsJSON []byte
}

type bindingScope struct {
	TenantRefs []string `json:"tenant_refs"`
	Regions    []string `json:"regions"`
}

// Resolve runs the pipeline and persists the plan idempotently.
func (s *Service) Resolve(ctx context.Context, req *Request, meta CallerMeta, idemKey string) (*Plan, bool, error) {
	// 1. Schema/semantic validation
	if err := ValidateRequest(req); err != nil {
		return nil, false, err
	}
	// 2. 禁止字段检查 runs at the HTTP boundary on the RAW body (the
	// closed request struct cannot carry undeclared keys, and scanning
	// re-marshaled struct VALUES would false-positive on substrings —
	// same class of issue the I08 review flagged); the handler invokes
	// CheckForbiddenFields before calling Resolve.

	// policy revision (carried by the filter step and the fingerprint)
	polSet, polVer := "", 0
	if s.Policy != nil && s.PolicySetID != "" {
		rev, err := s.Policy.ActiveRevision(ctx, s.PolicySetID)
		if err == nil && rev != nil {
			polSet, polVer = s.PolicySetID, rev.Version
			// 3. Policy filter: per requirement; denial is terminal for it
			for i := range req.Requirements {
				in := policy.EligibilityInput{
					Region:      strConstraint(req.Requirements[i].Constraints, "region"),
					DataClass:   strConstraint(req.Requirements[i].Constraints, "data_classification_max"),
					Environment: meta.Environment,
					TenantRef:   meta.TenantRef,
				}
				ev, reason, perr := s.Eval.Evaluate(ctx, rev, in)
				if perr != nil {
					return nil, false, resErr(CodeResolverUnavailable, http.StatusServiceUnavailable,
						"policy evaluation failed")
				}
				if !ev.Allow {
					return nil, false, resErr(CodeNoCompatibleProvider, http.StatusUnprocessableEntity,
						fmt.Sprintf("requirement %s denied by policy", req.Requirements[i].RequirementID),
						ItemDetail{RequirementID: req.Requirements[i].RequirementID,
							ReasonCodes: []string{ReasonPolicyDenied, reason}})
				}
			}
		} else if err != nil && !errors.Is(err, policy.ErrNotFound) {
			return nil, false, resErr(CodeResolverUnavailable, http.StatusServiceUnavailable,
				"policy store unavailable")
		}
		// no active revision for the set → policy step is a documented
		// pass-through (empty revision recorded in the plan)
	}

	now := s.Now()
	items := make([]PlanItem, 0, len(req.Requirements))
	revInputs := make([]RevisionInput, 0, len(req.Requirements))
	digest := RequestDigest(req)

	// deterministic processing order: sorted requirement_ids
	ids := make([]string, len(req.Requirements))
	byID := map[string]*Requirement{}
	for i := range req.Requirements {
		ids[i] = req.Requirements[i].RequirementID
		byID[req.Requirements[i].RequirementID] = &req.Requirements[i]
	}
	sort.Strings(ids)

	for _, rid := range ids {
		r := byID[rid]
		// 3b. capability lookup (CAPABILITY_NOT_FOUND)
		cap, err := s.capability(ctx, r)
		if err != nil {
			return nil, false, err
		}
		// 4. binding disambiguation: live published bindings for this
		// capability+environment whose scope matches the caller; ties at
		// the same priority are a 409 configuration error.
		cands, err := s.candidates(ctx, cap.ID, meta.Environment, r, meta)
		if err != nil {
			return nil, false, err
		}
		if len(cands) == 0 {
			return nil, false, resErr(CodeNoCompatibleProvider, http.StatusUnprocessableEntity,
				fmt.Sprintf("no active binding satisfies requirement %s", rid),
				ItemDetail{RequirementID: rid, ReasonCodes: []string{ReasonConstraintMismatch}})
		}
		if len(cands) >= 2 && cands[0].Priority == cands[1].Priority {
			return nil, false, resErr(CodeAmbiguousBinding, http.StatusConflict,
				fmt.Sprintf("requirement %s has %d bindings at priority %d", rid, len(cands), cands[0].Priority),
				ItemDetail{RequirementID: rid, ReasonCodes: []string{"PRIORITY_TIE"}})
		}
		top := cands[0]
		// 5. snapshot validity: the binding's pinned snapshot must still be
		// the provider's active published snapshot and inside its window.
		snap, err := s.Cache.Get(ctx, top.SnapshotID)
		if err != nil {
			if errors.Is(err, ErrSnapshotUnavailable) {
				return nil, false, resErr(CodeSnapshotExpired, http.StatusFailedDependency,
					fmt.Sprintf("snapshot for binding %s is no longer active/published", top.BindingKey),
					ItemDetail{RequirementID: rid, ReasonCodes: []string{"SNAPSHOT_INACTIVE"}})
			}
			return nil, false, resErr(CodeResolverUnavailable, http.StatusServiceUnavailable,
				"snapshot source unavailable")
		}
		if !snap.ValidUntil.After(now) {
			return nil, false, resErr(CodeSnapshotExpired, http.StatusFailedDependency,
				fmt.Sprintf("snapshot for binding %s expired", top.BindingKey),
				ItemDetail{RequirementID: rid, ReasonCodes: []string{"SNAPSHOT_EXPIRED"}})
		}
		// 6. candidate filter: profile availability + constraint
		// compatibility on the chosen binding.
		reasons, err := s.matchProfile(snap, top, r, cap)
		if err != nil {
			return nil, false, err
		}

		items = append(items, PlanItem{
			RequirementID:      rid,
			CapabilityKey:      r.CapabilityID,
			MajorVersion:       r.CapabilityMajorVersion,
			CapabilityRevision: cap.Revision,
			BindingKey:         top.BindingKey,
			BindingRevision:    top.Revision,
			ProviderKey:        top.ProviderKey,
			ProviderType:       snap.ProviderType,
			EndpointRef:        snap.EndpointRef,
			SnapshotVersion:    snap.SnapshotVersion,
			ProfileOrAction:    top.Profile,
			ReasonCodes:        reasons,
		})
		revInputs = append(revInputs, RevisionInput{
			CapabilityKey: r.CapabilityID, CapabilityRevision: cap.Revision,
			BindingKey: top.BindingKey, BindingRevision: top.Revision,
			ProviderKey: top.ProviderKey, SnapshotVersion: snap.SnapshotVersion,
			Profile: top.Profile,
		})
	}

	// fingerprint over the full decision basis
	fp := Fingerprint(req, revInputs, polSet, polVer)

	ttl := s.DefaultTTL
	if req.Options.MaxPlanTTLSeconds > 0 {
		ttl = time.Duration(req.Options.MaxPlanTTLSeconds) * time.Second
	}
	if s.MaxTTL > 0 && ttl > s.MaxTTL {
		ttl = s.MaxTTL
	}
	plan := &Plan{
		ID:             s.NewID(),
		CallerRef:      meta.CallerRef,
		TenantRef:      meta.TenantRef,
		TaskRef:        req.TaskRef,
		Fingerprint:    fp,
		RequestDigest:  digest,
		IdempotencyKey: idemKey,
		PolicySetID:    polSet,
		PolicyVersion:  polVer,
		TraceID:        meta.TraceID,
		CreatedAt:      now.UTC().Format(time.RFC3339Nano),
		ExpiresAt:      now.Add(ttl).UTC().Format(time.RFC3339Nano),
		Items:          items,
	}
	created, existing, err := s.Plans.CreatePlan(ctx, plan)
	if err != nil {
		return nil, false, err
	}
	return existing, created, nil
}

// capability loads the latest PUBLISHED revision of key+major.
func (s *Service) capability(ctx context.Context, r *Requirement) (*capabilityRow, error) {
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return nil, resErr(CodeResolverUnavailable, http.StatusServiceUnavailable, "registry unavailable")
	}
	defer conn.Release()
	var c capabilityRow
	err = conn.QueryRow(ctx, `
		SELECT id, revision, resource_type, requirement_schema
		FROM registry.capability_definition
		WHERE capability_key = $1 AND major_version = $2 AND state = 'PUBLISHED'
		ORDER BY revision DESC LIMIT 1`, r.CapabilityID, r.CapabilityMajorVersion).
		Scan(&c.ID, &c.Revision, &c.ResourceType, &c.RequirementSchema)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, resErr(CodeCapabilityNotFound, http.StatusNotFound,
			fmt.Sprintf("capability %s@%d not found", r.CapabilityID, r.CapabilityMajorVersion),
			ItemDetail{RequirementID: r.RequirementID, ReasonCodes: []string{"CAPABILITY_MISSING"}})
	}
	if err != nil {
		return nil, resErr(CodeResolverUnavailable, http.StatusServiceUnavailable, "registry unavailable")
	}
	// constraints must satisfy the capability's requirement_schema
	if len(c.RequirementSchema) > 0 && string(c.RequirementSchema) != "{}" && string(c.RequirementSchema) != "null" {
		if verr := validateAgainstSchema(r, c.RequirementSchema); verr != nil {
			return nil, verr
		}
	}
	return &c, nil
}

// candidates returns live PUBLISHED bindings for the capability whose
// scope matches the caller, joined with provider state, ordered
// deterministically by (priority ASC, binding_key ASC). Bindings whose
// provider is not PUBLISHED are filtered (GWT#8 I08: 暂停后新 Plan 不再
// 使用该 Provider).
func (s *Service) candidates(ctx context.Context, capabilityID int64, environment string, r *Requirement, meta CallerMeta) ([]bindingCandidate, error) {
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return nil, resErr(CodeResolverUnavailable, http.StatusServiceUnavailable, "registry unavailable")
	}
	defer conn.Release()
	rows, err := conn.Query(ctx, `
		SELECT cb.binding_key, cb.revision, cb.provider_id, cb.snapshot_id, cb.profile_or_action,
		       cb.priority, cb.scope, cb.constraints, rp.provider_key, rp.state
		FROM registry.capability_binding cb
		JOIN registry.resource_provider rp ON rp.id = cb.provider_id
		WHERE cb.capability_id = $1 AND cb.environment = $2
		  AND cb.state = 'PUBLISHED' AND cb.is_active
		ORDER BY cb.priority ASC, cb.binding_key ASC`, capabilityID, environment)
	if err != nil {
		return nil, resErr(CodeResolverUnavailable, http.StatusServiceUnavailable, "registry unavailable")
	}
	defer rows.Close()
	region := strConstraint(r.Constraints, "region")
	var out []bindingCandidate
	for rows.Next() {
		var c bindingCandidate
		var scopeJSON, consJSON []byte
		if err := rows.Scan(&c.BindingKey, &c.Revision, &c.ProviderID, &c.SnapshotID, &c.Profile,
			&c.Priority, &scopeJSON, &consJSON, &c.ProviderKey, &c.ProviderState); err != nil {
			return nil, resErr(CodeResolverUnavailable, http.StatusServiceUnavailable, "registry unavailable")
		}
		if c.ProviderState != "PUBLISHED" {
			continue // provider suspended/deprecated: candidate filtered
		}
		if err := json.Unmarshal(scopeJSON, &c.Scope); err != nil {
			return nil, resErr(CodeResolverUnavailable, http.StatusServiceUnavailable, "binding scope unreadable")
		}
		c.ConstraintsJSON = consJSON
		if !scopeMatches(c.Scope, meta.TenantRef, region) {
			continue
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// scopeMatches: a binding matches the caller when the caller's tenant is
// in tenant_refs (or the set is empty = unrestricted) AND the requested
// region is in regions (or the set is empty). No region constraint means
// no region preference, so any binding region set matches.
func scopeMatches(sc bindingScope, tenantRef, region string) bool {
	if !inSet(sc.TenantRefs, tenantRef) {
		return false
	}
	if region == "" || len(sc.Regions) == 0 {
		return true
	}
	return inSet(sc.Regions, region)
}

func inSet(set []string, v string) bool {
	if len(set) == 0 {
		return true // unrestricted
	}
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}

// matchProfile filters the chosen binding's profile against the
// requirement constraints; a miss is NO_COMPATIBLE_PROVIDER with the
// per-requirement reason codes.
func (s *Service) matchProfile(snap *LoadedSnapshot, top bindingCandidate, r *Requirement, cap *capabilityRow) ([]string, error) {
	var profile map[string]any
	for _, p := range snap.Profiles {
		if pid, _ := p["profile_id"].(string); pid == top.Profile {
			profile = p
			break
		}
	}
	if profile == nil {
		return nil, resErr(CodeNoCompatibleProvider, http.StatusUnprocessableEntity,
			fmt.Sprintf("profile %s is not present in the published snapshot for binding %s", top.Profile, top.BindingKey),
			ItemDetail{RequirementID: r.RequirementID, ReasonCodes: []string{ReasonProfileUnavailable}})
	}
	var reasons []string
	if st, _ := profile["status"].(string); st != "AVAILABLE" {
		return nil, resErr(CodeNoCompatibleProvider, http.StatusUnprocessableEntity,
			fmt.Sprintf("profile %s is %s", top.Profile, st),
			ItemDetail{RequirementID: r.RequirementID, ReasonCodes: []string{ReasonProfileUnavailable}})
	}
	// capability keys
	if keys, ok := profile["capability_keys"].([]any); ok && len(keys) > 0 {
		found := false
		for _, k := range keys {
			if ks, _ := k.(string); ks == r.CapabilityID {
				found = true
				break
			}
		}
		if !found {
			return nil, resErr(CodeNoCompatibleProvider, http.StatusUnprocessableEntity,
				fmt.Sprintf("profile %s does not serve capability %s", top.Profile, r.CapabilityID),
				ItemDetail{RequirementID: r.RequirementID, ReasonCodes: []string{ReasonCapabilityMatch + "_FAIL"}})
		}
	}
	// region
	if region := strConstraint(r.Constraints, "region"); region != "" {
		if regions, ok := profile["regions"].([]any); ok && len(regions) > 0 && !anyInSet(regions, region) {
			return nil, resErr(CodeNoCompatibleProvider, http.StatusUnprocessableEntity,
				fmt.Sprintf("profile %s does not serve region %s", top.Profile, region),
				ItemDetail{RequirementID: r.RequirementID, ReasonCodes: []string{ReasonRegionMismatch}})
		}
	}
	// data classification: request max must be within the profile/binding max
	if class := strConstraint(r.Constraints, "data_classification_max"); class != "" {
		profileMax, _ := profile["data_classification_max"].(string)
		bindingMax := ""
		var bc map[string]any
		if len(top.ConstraintsJSON) > 0 {
			_ = json.Unmarshal(top.ConstraintsJSON, &bc)
			bindingMax, _ = bc["data_classification_max"].(string)
		}
		if rankOf(dataClassRank, class) > rankOf(dataClassRank, profileMax) ||
			(bindingMax != "" && rankOf(dataClassRank, class) > rankOf(dataClassRank, bindingMax)) {
			return nil, resErr(CodeNoCompatibleProvider, http.StatusUnprocessableEntity,
				fmt.Sprintf("profile %s does not allow data classification %s", top.Profile, class),
				ItemDetail{RequirementID: r.RequirementID, ReasonCodes: []string{ReasonDataClassMismatch}})
		}
	}
	// positive reasons (deterministic order)
	reasons = append(reasons, ReasonCapabilityMatch, ReasonRegionMatch, ReasonDataClassMatch)
	sort.Strings(reasons)
	return reasons, nil
}

func anyInSet(set []any, v string) bool {
	for _, s := range set {
		if ss, _ := s.(string); ss == v {
			return true
		}
	}
	return false
}

func rankOf(rank map[string]int, v string) int {
	return rank[v] // unknown → 0 (least restrictive)
}

func strConstraint(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

func validateAgainstSchema(r *Requirement, schemaJSON []byte) error {
	var schema map[string]any
	if err := json.Unmarshal(schemaJSON, &schema); err != nil {
		return resErr(CodeResolverUnavailable, http.StatusServiceUnavailable, "requirement_schema unreadable")
	}
	if len(schema) == 0 {
		return nil
	}
	// minimal structural validation: declared required constraint keys
	// must be present; unknown-key rejection already happened in
	// ValidateRequest via the constraints whitelist.
	if req, ok := schema["required"].([]any); ok {
		for _, k := range req {
			ks, _ := k.(string)
			if _, present := r.Constraints[ks]; !present {
				return resErr(CodeInvalidRequirement, http.StatusBadRequest,
					fmt.Sprintf("requirement %s: constraint %q is required by the capability schema", r.RequirementID, ks))
			}
		}
	}
	if enum, ok := schema["enum_constraints"].(map[string]any); ok {
		for k, vals := range enum {
			got, present := r.Constraints[k]
			if !present {
				continue
			}
			gs, _ := got.(string)
			if list, ok := vals.([]any); ok {
				found := false
				for _, v := range list {
					if vs, _ := v.(string); vs == gs {
						found = true
						break
					}
				}
				if !found {
					return resErr(CodeInvalidRequirement, http.StatusBadRequest,
						fmt.Sprintf("requirement %s: constraint %q value %q is not allowed by the capability schema", r.RequirementID, k, gs))
				}
			}
		}
	}
	return nil
}

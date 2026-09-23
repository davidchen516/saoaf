// Package resolver implements the deterministic Resource Resolver and
// Resource Plan store (I09, specs/resource-resolver-api.md §3).
//
// Determinism: the same normalized input + revision set + policy revision
// always produces the same fingerprint and the same ordered plan items.
// The error routing order is fixed by the issue: Schema 校验 → 禁止字段 →
// Capability 查找 → Policy 过滤（携带 policy revision）→ Binding 消歧 →
// Snapshot 有效性 → 候选过滤; first hit wins, one unique reason code.
package resolver

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
)

// Reason codes / error classification (spec §3.1 错误表).
const (
	CodeInvalidRequirement   = "INVALID_REQUIREMENT"       // 400
	CodeUnauthenticated      = "UNAUTHENTICATED"           // 401
	CodeCallerNotAllowed     = "CALLER_NOT_ALLOWED"        // 403
	CodeCapabilityNotFound   = "CAPABILITY_NOT_FOUND"      // 404
	CodeIdempotencyConflict  = "IDEMPOTENCY_CONFLICT"      // 409
	CodeAmbiguousBinding     = "AMBIGUOUS_BINDING"         // 409
	CodeNoCompatibleProvider = "NO_COMPATIBLE_PROVIDER"    // 422
	CodeSnapshotExpired      = "PROVIDER_SNAPSHOT_EXPIRED" // 424
	CodeRateLimited          = "RATE_LIMITED"              // 429
	CodeResolverUnavailable  = "RESOLVER_UNAVAILABLE"      // 503
	CodePlanNotFound         = "PLAN_NOT_FOUND"            // 404 (GET plan)
)

// Per-item detail reason codes (negative filtering evidence).
const (
	ReasonCapabilityMatch    = "CAPABILITY_MATCH"
	ReasonRegionMatch        = "REGION_MATCH"
	ReasonDataClassMatch     = "DATA_CLASS_MATCH"
	ReasonPolicyDenied       = "POLICY_DENIED"
	ReasonRegionMismatch     = "REGION_MISMATCH"
	ReasonDataClassMismatch  = "DATA_CLASS_MISMATCH"
	ReasonProfileUnavailable = "PROFILE_UNAVAILABLE"
	ReasonConstraintMismatch = "CONSTRAINT_MISMATCH"
)

// Sentinel errors for the store/service layers; ResolveError carries the
// HTTP code + unique reason code for the envelope.
var (
	ErrIdemConflict  = errors.New("resolver: idempotency conflict")
	ErrNotFound      = errors.New("resolver: plan not found")
	ErrAmbiguous     = errors.New("resolver: ambiguous binding")
	ErrNoCandidate   = errors.New("resolver: no compatible provider")
	ErrSnapshotStale = errors.New("resolver: snapshot expired")
	ErrUnavailable   = errors.New("resolver: unavailable")
	ErrInvalid       = errors.New("resolver: invalid requirement")
	ErrCapNotFound   = errors.New("resolver: capability not found")
)

// ResolveError is the classified resolve failure: exactly one code.
type ResolveError struct {
	Code    string // unique reason code
	Status  int    // HTTP status
	Msg     string
	Details []ItemDetail // per-requirement negative evidence
}

func (e *ResolveError) Error() string { return e.Code + ": " + e.Msg }

func resErr(code string, status int, msg string, details ...ItemDetail) *ResolveError {
	return &ResolveError{Code: code, Status: status, Msg: msg, Details: details}
}

// ItemDetail is the negative-evidence entry in the error envelope.
type ItemDetail struct {
	RequirementID string   `json:"requirement_id"`
	ReasonCodes   []string `json:"reason_codes"`
}

// Plan states (核心验收逻辑: RESOLVED → {EXPIRED | REVOKED}，仅此两条出边).
const (
	StatusResolved = "RESOLVED"
	StatusExpired  = "EXPIRED"
	StatusRevoked  = "REVOKED"
)

// ValidatePlanTransition reports whether from→to is legal for a plan.
func ValidatePlanTransition(from, to string) bool {
	return from == StatusResolved && (to == StatusExpired || to == StatusRevoked)
}

// Request is the resolve request body (spec §3.1).
type Request struct {
	ContractVersion string         `json:"contract_version"`
	TaskRef         string         `json:"task_ref"`
	Requirements    []Requirement  `json:"requirements"`
	Options         Options        `json:"options"`
	Extensions      map[string]any `json:"extensions,omitempty"`
}

// Requirement is one requested capability slot.
type Requirement struct {
	RequirementID          string         `json:"requirement_id"`
	CapabilityID           string         `json:"capability_id"`
	CapabilityMajorVersion int            `json:"capability_major_version"`
	ResourceType           string         `json:"resource_type"`
	Constraints            map[string]any `json:"constraints"`
}

// Options are the resolve options.
type Options struct {
	MaxPlanTTLSeconds   int  `json:"max_plan_ttl_seconds"`
	IncludeExplanations bool `json:"include_explanations"`
}

// Request limits (spec §3.1).
const (
	MaxRequirements    = 20
	DefaultPlanTTL     = 300
	MaxRequestBytes    = 256 * 1024
	MaxRequirementKeys = 16 // constraints 白名单外的 key 一律拒绝
)

// forbiddenPattern: no credentials, prompts, or business payloads ride the
// control-plane request (common delivery constraints).
var forbiddenPattern = regexp.MustCompile(`(?i)(credential|secret|password|api_key|prompt|system_message|user_message)`)

// allowedConstraintKeys is the constraints whitelist; unknown fields are
// rejected in v1 (specs §1: 未声明字段默认拒绝).
var allowedConstraintKeys = map[string]bool{
	"region":                  true,
	"data_classification_max": true,
	"streaming_required":      true,
	"idempotency_required":    true,
	"risk_level_max":          true,
}

// allowedResourceTypes: 首期生产链只启用 MODEL（issue Non-goals);
// CONTEXT/TOOL 仅验证契约模型（可解析，不执行）。
var allowedResourceTypes = map[string]bool{
	"MODEL_PROVIDER":   true,
	"CONTEXT_PROVIDER": true,
	"TOOL_PROVIDER":    true,
}

// dataClassRank orders classifications; higher rank = more restrictive.
var dataClassRank = map[string]int{
	"PUBLIC": 1, "INTERNAL": 2, "CONFIDENTIAL": 3, "RESTRICTED": 4,
}

// riskRank orders risk levels.
var riskRank = map[string]int{
	"LOW": 1, "MEDIUM": 2, "HIGH": 3,
}

// ValidateRequest enforces schema/semantic rules on the parsed request.
// Routing step 1 (INVALID_REQUIREMENT): everything structural.
func ValidateRequest(req *Request) error {
	if req.ContractVersion != "1.0" {
		return resErr(CodeInvalidRequirement, http.StatusBadRequest,
			"contract_version must be \"1.0\"")
	}
	if req.TaskRef == "" {
		return resErr(CodeInvalidRequirement, http.StatusBadRequest,
			"task_ref is required")
	}
	if len(req.Requirements) == 0 || len(req.Requirements) > MaxRequirements {
		return resErr(CodeInvalidRequirement, http.StatusBadRequest,
			fmt.Sprintf("requirements must be 1–%d items", MaxRequirements))
	}
	seen := map[string]bool{}
	for i := range req.Requirements {
		r := &req.Requirements[i]
		if r.RequirementID == "" {
			return resErr(CodeInvalidRequirement, http.StatusBadRequest,
				fmt.Sprintf("requirements[%d].requirement_id is required", i))
		}
		if seen[r.RequirementID] {
			return resErr(CodeInvalidRequirement, http.StatusBadRequest,
				fmt.Sprintf("requirement_id %q is duplicated", r.RequirementID))
		}
		seen[r.RequirementID] = true
		if r.CapabilityID == "" {
			return resErr(CodeInvalidRequirement, http.StatusBadRequest,
				fmt.Sprintf("requirement %s: capability_id is required", r.RequirementID))
		}
		if r.CapabilityMajorVersion < 1 {
			return resErr(CodeInvalidRequirement, http.StatusBadRequest,
				fmt.Sprintf("requirement %s: capability_major_version must be >= 1", r.RequirementID))
		}
		if !allowedResourceTypes[r.ResourceType] {
			return resErr(CodeInvalidRequirement, http.StatusBadRequest,
				fmt.Sprintf("requirement %s: resource_type %q is not known in v1", r.RequirementID, r.ResourceType))
		}
		if len(r.Constraints) > MaxRequirementKeys {
			return resErr(CodeInvalidRequirement, http.StatusBadRequest,
				fmt.Sprintf("requirement %s: too many constraints", r.RequirementID))
		}
		for k, v := range r.Constraints {
			if !allowedConstraintKeys[k] {
				return resErr(CodeInvalidRequirement, http.StatusBadRequest,
					fmt.Sprintf("requirement %s: undeclared constraint %q rejected in v1", r.RequirementID, k))
			}
			switch v.(type) {
			case string, bool:
			case float64:
				return resErr(CodeInvalidRequirement, http.StatusBadRequest,
					fmt.Sprintf("requirement %s: constraint %q must not be a number (金额/配额禁浮点)", r.RequirementID, k))
			default:
				return resErr(CodeInvalidRequirement, http.StatusBadRequest,
					fmt.Sprintf("requirement %s: constraint %q has unsupported type", r.RequirementID, k))
			}
		}
		if err := validateConstraintSemantics(r); err != nil {
			return err
		}
	}
	return nil
}

// validateConstraintSemantics checks value semantics (rank existence etc).
func validateConstraintSemantics(r *Requirement) error {
	if v, ok := r.Constraints["data_classification_max"]; ok {
		s, _ := v.(string)
		if _, known := dataClassRank[s]; !known {
			return resErr(CodeInvalidRequirement, http.StatusBadRequest,
				fmt.Sprintf("requirement %s: unknown data_classification_max %q", r.RequirementID, s))
		}
	}
	if v, ok := r.Constraints["risk_level_max"]; ok {
		s, _ := v.(string)
		if _, known := riskRank[s]; !known {
			return resErr(CodeInvalidRequirement, http.StatusBadRequest,
				fmt.Sprintf("requirement %s: unknown risk_level_max %q", r.RequirementID, s))
		}
	}
	return nil
}

// CheckForbiddenFields scans the RAW request JSON for forbidden payloads
// (routing step 2, before any registry/policy work).
func CheckForbiddenFields(raw []byte) error {
	if forbiddenPattern.Match(raw) {
		return resErr(CodeInvalidRequirement, http.StatusBadRequest,
			"request carries forbidden fields (credentials/prompt payloads)")
	}
	return nil
}

// normalized is the canonical form feeding the fingerprint: sorted
// requirements with canonicalized constraints — nothing environment-
// dependent (no timestamps, no caller identity).
type normalized struct {
	ContractVersion string           `json:"contract_version"`
	TaskRef         string           `json:"task_ref"`
	Requirements    []normalizedReqs `json:"requirements"`
	Options         Options          `json:"options"`
}

type normalizedReqs struct {
	RequirementID string            `json:"requirement_id"`
	CapabilityID  string            `json:"capability_id"`
	Major         int               `json:"capability_major_version"`
	ResourceType  string            `json:"resource_type"`
	Constraints   map[string]string `json:"constraints"`
}

// Normalize canonicalizes a request for fingerprinting.
func Normalize(req *Request) *normalized {
	out := &normalized{
		ContractVersion: req.ContractVersion,
		TaskRef:         req.TaskRef,
		Options:         req.Options,
	}
	ids := make([]string, 0, len(req.Requirements))
	byID := map[string]*Requirement{}
	for i := range req.Requirements {
		ids = append(ids, req.Requirements[i].RequirementID)
		byID[req.Requirements[i].RequirementID] = &req.Requirements[i]
	}
	sort.Strings(ids)
	for _, id := range ids {
		r := byID[id]
		cs := map[string]string{}
		for k, v := range r.Constraints {
			switch tv := v.(type) {
			case string:
				cs[k] = tv
			case bool:
				if tv {
					cs[k] = "true"
				} else {
					cs[k] = "false"
				}
			default:
				cs[k] = fmt.Sprintf("%v", v)
			}
		}
		out.Requirements = append(out.Requirements, normalizedReqs{
			RequirementID: r.RequirementID, CapabilityID: r.CapabilityID,
			Major: r.CapabilityMajorVersion, ResourceType: r.ResourceType,
			Constraints: cs,
		})
	}
	return out
}

// RequestDigest is the idempotency content digest: same key + different
// digest = conflict (409).
func RequestDigest(req *Request) string {
	b, err := json.Marshal(Normalize(req))
	if err != nil {
		panic("normalize: " + err.Error())
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// RevisionInput is the revision set the decision pinned (capability/
// binding/provider/snapshot/policy revisions) — the other fingerprint half.
type RevisionInput struct {
	CapabilityKey      string `json:"capability_key"`
	CapabilityRevision int    `json:"capability_revision"`
	BindingKey         string `json:"binding_key"`
	BindingRevision    int    `json:"binding_revision"`
	ProviderKey        string `json:"provider_key"`
	SnapshotVersion    int    `json:"snapshot_version"`
	Profile            string `json:"profile_or_action"`
}

// Fingerprint = H(规范化输入, 输入 revision set, policy revision)
// (核心验收逻辑: fingerprint = H(normalized input, revision set, policy
// revision); 同输入必同 fingerprint).
func Fingerprint(req *Request, items []RevisionInput, policySetID string, policyVersion int) string {
	h := sha256.New()
	_ = json.NewEncoder(h).Encode(Normalize(req))
	sorted := append([]RevisionInput(nil), items...)
	sort.Slice(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if a.CapabilityKey != b.CapabilityKey {
			return a.CapabilityKey < b.CapabilityKey
		}
		if a.BindingKey != b.BindingKey {
			return a.BindingKey < b.BindingKey
		}
		return a.Profile < b.Profile
	})
	_ = json.NewEncoder(h).Encode(sorted)
	fmt.Fprintf(h, "|policy=%s@%d", policySetID, policyVersion)
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// Plan is the persisted resource plan with its items.
type Plan struct {
	ID             string
	CallerRef      string
	TenantRef      string
	TaskRef        string
	Status         string
	Fingerprint    string
	RequestDigest  string
	IdempotencyKey string
	PolicySetID    string
	PolicyVersion  int
	TraceID        string
	CreatedAt      string
	ExpiresAt      string
	Items          []PlanItem
}

// PlanItem is one immutable resolved slot.
type PlanItem struct {
	RequirementID      string   `json:"requirement_id"`
	CapabilityKey      string   `json:"capability_key"`
	MajorVersion       int      `json:"major_version"`
	CapabilityRevision int      `json:"capability_revision"`
	BindingKey         string   `json:"binding_key"`
	BindingRevision    int      `json:"binding_revision"`
	ProviderKey        string   `json:"provider_key"`
	ProviderType       string   `json:"provider_type"`
	EndpointRef        string   `json:"endpoint_ref"`
	SnapshotVersion    int      `json:"snapshot_version"`
	ContractVersion    string   `json:"contract_version"`
	ProfileOrAction    string   `json:"profile_or_action"`
	ReasonCodes        []string `json:"reason_codes"`
}

// Package binding implements the Capability Binding lifecycle (I08):
// scope normalization with canonical hashing, publish/suspend/resume/retire,
// conflict detection, and rollback-as-new-revision semantics.
package binding

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"

	"golang.org/x/text/unicode/norm"
)

// State is the binding lifecycle state.
type State string

const (
	StateDraft      State = "DRAFT"
	StatePublished  State = "PUBLISHED"
	StateSuspended  State = "SUSPENDED"
	StateDeprecated State = "DEPRECATED"
	StateRetired    State = "RETIRED"
)

// ValidTransitions is the full lifecycle matrix.
var ValidTransitions = map[State][]State{
	StateDraft:      {StatePublished},
	StatePublished:  {StateSuspended, StateDeprecated},
	StateSuspended:  {StatePublished, StateRetired},
	StateDeprecated: {StateRetired},
	StateRetired:    {},
}

// ValidateTransition reports whether from→to is legal.
func ValidateTransition(from, to State) bool {
	for _, t := range ValidTransitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

// Reason codes.
const (
	ReasonInvalidTransition = "BINDING_INVALID_TRANSITION"
	ReasonScopeNotCanonical = "BINDING_SCOPE_NOT_CANONICAL"
	ReasonScopeOverlap      = "BINDING_SCOPE_OVERLAP"
	ReasonMissingApproval   = "BINDING_MISSING_APPROVAL"
	ReasonRevisionConflict  = "BINDING_REVISION_CONFLICT"
	ReasonNotFound          = "BINDING_NOT_FOUND"
	ReasonAlreadyActive     = "BINDING_ALREADY_ACTIVE"
	ReasonIdemKeyReuse      = "BINDING_IDEMPOTENCY_KEY_REUSE"
	ReasonBacklogBlocked    = "BINDING_BACKLOG_BLOCKED"
)

// ValidationError carries a reason code.
type ValidationError struct {
	Reason string
	Msg    string
}

func (e *ValidationError) Error() string { return e.Reason + ": " + e.Msg }

func errV(reason, msg string) error { return &ValidationError{Reason: reason, Msg: msg} }

// Scope is the binding scope structure (specs §1.2).
type Scope struct {
	TenantRefs  []string `json:"tenant_refs"`
	FactoryRefs []string `json:"factory_refs"`
	Regions     []string `json:"regions"`
	AgentRefs   []string `json:"agent_refs"`
}

// forbiddenScopeFields: scope cannot carry credentials or business payloads.
var scopeFieldPattern = regexp.MustCompile(`(?i)(credential|secret|password|api_key|prompt)`)

// CanonicalScope returns the normalized scope: each array is sorted,
// deduplicated, Unicode NFC-normalized; empty arrays stay empty (denotes
// "not restricted" — deny is NOT expressible here per specs §1.2).
func (s Scope) CanonicalScope() Scope {
	return Scope{
		TenantRefs:  canonicalizeStrings(s.TenantRefs),
		FactoryRefs: canonicalizeStrings(s.FactoryRefs),
		Regions:     canonicalizeStrings(s.Regions),
		AgentRefs:   canonicalizeStrings(s.AgentRefs),
	}
}

func canonicalizeStrings(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, s := range in {
		nfc := norm.NFC.String(s)
		if !seen[nfc] {
			seen[nfc] = true
			out = append(out, nfc)
		}
	}
	sort.Strings(out)
	return out
}

// ScopeHash computes the canonical hash: JSON with sorted keys and
// normalized arrays, then SHA-256.
func (s Scope) ScopeHash() string {
	c := s.CanonicalScope()
	b, err := json.Marshal(c)
	if err != nil {
		panic("scope marshal: " + err.Error())
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Overlaps reports whether two canonical scopes can match the same request
// (issue 冲突不变量：同优先级重叠 scope 不可同时 PUBLISHED).
//
// A binding matches a request when EVERY dimension matches, where a
// dimension matches if the request value is in the set OR the set is empty
// (specs §1.2: empty = unrestricted, deny is not expressible). Therefore two
// scopes overlap iff EVERY dimension co-matches, where a dimension
// co-matches when either side is empty (wildcard) or the sets intersect.
func (a Scope) Overlaps(b Scope) bool {
	return overlapDim(a.TenantRefs, b.TenantRefs) &&
		overlapDim(a.FactoryRefs, b.FactoryRefs) &&
		overlapDim(a.Regions, b.Regions) &&
		overlapDim(a.AgentRefs, b.AgentRefs)
}

func overlapDim(a, b []string) bool {
	// empty = unrestricted（通配）: covers any value the other side may carry
	if len(a) == 0 || len(b) == 0 {
		return true
	}
	am := setOf(a)
	for _, v := range b {
		if am[v] {
			return true
		}
	}
	return false
}

func setOf(s []string) map[string]bool {
	m := map[string]bool{}
	for _, v := range s {
		m[v] = true
	}
	return m
}

// CheckScopeFields validates no forbidden keys ride on RAW scope JSON. It is
// meaningful only at untrusted-JSON boundaries (HTTP layer, I16) — the store
// layer's Scope is a closed struct, so Publish does not re-run it (review R1
// P3-2: value-substring false positives, e.g. a legit "tenant-prompt-team").
func CheckScopeFields(raw []byte) error {
	if scopeFieldPattern.Match(raw) {
		return errV(ReasonScopeNotCanonical, "scope carries forbidden keys (credentials/prompt)")
	}
	return nil
}

// PublishGate checks the production publish prerequisites (issue: 审批引用
// + change record 存在，否则拒绝). Approval is enforced for ALL environments
// (stricter than the issue's production-only floor — disclosed in
// evidence/i08); actor is always required so audit rows can attribute the
// change (review R1 P2-2).
func PublishGate(approvalRef, changeReason, actor string) error {
	if approvalRef == "" {
		return errV(ReasonMissingApproval, "publish requires approval_ref")
	}
	if changeReason == "" {
		return errV(ReasonMissingApproval, "publish requires change_reason")
	}
	if actor == "" {
		return errV(ReasonMissingApproval, "publish requires actor for audit attribution")
	}
	return nil
}

// Binding is the domain aggregate.
type Binding struct {
	ID           int64
	BindingKey   string
	CapabilityID int64
	ProviderID   int64
	SnapshotID   int64
	Profile      string
	Environment  string
	Scope        Scope
	ScopeHash    string
	Priority     int
	State        State
	Revision     int
	IsActive     bool
	ChangeReason string
	TicketRef    string
	ApprovalRef  string
}

// String provides a compact representation for debugging.
func (b *Binding) String() string {
	return fmt.Sprintf("binding(%s#%d %s %s)", b.BindingKey, b.Revision, b.State, b.ScopeHash)
}

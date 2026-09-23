// Package registry implements the Capability/Provider/Snapshot registry
// (I07): lifecycle state machine, revision CAS, snapshot validation, and
// publishable-candidate queries.
package registry

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"time"
)

// State is the shared lifecycle state for Capability/Provider/Snapshot.
type State string

const (
	StateDraft      State = "DRAFT"
	StateValidated  State = "VALIDATED"
	StatePublished  State = "PUBLISHED"
	StateDeprecated State = "DEPRECATED"
	StateSuspended  State = "SUSPENDED"
	StateRetired    State = "RETIRED"
)

// ValidTransitions is the full legal-transition matrix (issue: 全矩阵).
var ValidTransitions = map[State][]State{
	StateDraft:      {StateValidated},
	StateValidated:  {StatePublished},
	StatePublished:  {StateDeprecated, StateSuspended},
	StateDeprecated: {StateRetired},
	StateSuspended:  {StatePublished, StateRetired},
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

// Reason codes for the error envelope.
const (
	ReasonInvalidTransition = "REGISTRY_INVALID_TRANSITION"
	ReasonForbiddenField    = "REGISTRY_FORBIDDEN_FIELD"
	ReasonDigestFormat      = "REGISTRY_DIGEST_FORMAT"
	ReasonDigestMismatch    = "REGISTRY_DIGEST_MISMATCH"
	ReasonSignatureInvalid  = "REGISTRY_SIGNATURE_INVALID"
	ReasonWorkloadIdentity  = "REGISTRY_WORKLOAD_IDENTITY"
	ReasonContractMajor     = "REGISTRY_CONTRACT_MAJOR_MISMATCH"
	ReasonExpired           = "REGISTRY_SNAPSHOT_EXPIRED"
	ReasonOutOfOrder        = "REGISTRY_SNAPSHOT_OUT_OF_ORDER"
	ReasonRevisionConflict  = "REGISTRY_REVISION_CONFLICT"
)

// ValidationError carries a reason code.
type ValidationError struct {
	Reason string
	Msg    string
}

func (e *ValidationError) Error() string { return e.Reason + ": " + e.Msg }

func errV(reason, msg string) error { return &ValidationError{Reason: reason, Msg: msg} }

// forbiddenFields: fields that must never appear in profiles or any
// registry payload (issue: 禁止字段——物理模型、价格、权重、实例、回退链).
var forbiddenFields = []string{
	"physical_model", "price", "pricing", "weight", "weights",
	"instances", "fallback_chain", "cascade", "backend_health",
	"candidate_models", "credentials", "prompt",
}

// CheckForbiddenFields scans a map for forbidden keys.
func CheckForbiddenFields(m map[string]any) error {
	for k := range m {
		for _, f := range forbiddenFields {
			if k == f {
				return errV(ReasonForbiddenField, "field not allowed: "+k)
			}
		}
	}
	return nil
}

var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// Digest computes the canonical sha256 digest of a payload.
func Digest(payload string) string {
	sum := sha256.Sum256([]byte(payload))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Provider is the domain provider aggregate.
type Provider struct {
	ID               int64
	ProviderKey      string
	ProviderType     string
	EndpointRef      string
	OwnerRef         string
	WorkloadIdentity string
	State            State
	Revision         int
	ActiveRevision   int
}

// Snapshot is the domain snapshot aggregate.
type Snapshot struct {
	ID               int64
	ProviderID       int64
	SnapshotVersion  int
	ContractVersion  string
	Digest           string
	Signature        string
	WorkloadIdentity string
	GeneratedAt      time.Time
	ValidUntil       time.Time
	State            State
}

// ValidateSnapshot checks the snapshot acceptance contract (issue GWT#2):
// digest format, signature presence, workload identity, contract major
// compatibility, validity window, and forbidden fields in profiles.
func ValidateSnapshot(s *Snapshot, profiles map[string]any, expectedWorkload string, expectedContractMajor string, now time.Time) error {
	if !digestPattern.MatchString(s.Digest) {
		return errV(ReasonDigestFormat, "digest must be sha256:<64hex>")
	}
	if s.Signature == "" {
		return errV(ReasonSignatureInvalid, "signature required")
	}
	if s.WorkloadIdentity == "" {
		return errV(ReasonWorkloadIdentity, "workload identity required")
	}
	if expectedWorkload != "" && s.WorkloadIdentity != expectedWorkload {
		return errV(ReasonWorkloadIdentity, "workload identity mismatch with provider")
	}
	if expectedContractMajor != "" {
		gotMajor := majorOf(s.ContractVersion)
		if gotMajor != expectedContractMajor {
			return errV(ReasonContractMajor, fmt.Sprintf("contract major %s != expected %s", gotMajor, expectedContractMajor))
		}
	}
	if !s.ValidUntil.After(s.GeneratedAt) {
		return errV(ReasonExpired, "valid_until must be after generated_at")
	}
	if now.After(s.ValidUntil) {
		return errV(ReasonExpired, "snapshot expired")
	}
	if err := CheckForbiddenFields(profiles); err != nil {
		return err
	}
	return nil
}

// majorOf extracts the major component of a "major.minor" contract version.
func majorOf(version string) string {
	for i := 0; i < len(version); i++ {
		if version[i] == '.' {
			return version[:i]
		}
	}
	return version
}

// Transition applies a lifecycle transition to a state (pure function).
func Transition(from State, to State) (State, error) {
	if !ValidateTransition(from, to) {
		return from, errV(ReasonInvalidTransition, string(from)+" → "+string(to))
	}
	return to, nil
}

// PublishableStates are the states eligible for new binding/plan usage.
var PublishableStates = map[State]bool{StatePublished: true}

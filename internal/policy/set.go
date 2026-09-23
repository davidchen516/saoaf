// Package policy implements the minimal Sovereignty Policy module (I06):
// versioned Policy Sets with a single-active-revision lifecycle and
// deterministic, sandboxed CEL eligibility expressions.
//
// Lifecycle (issue acceptance logic):
//
//	DRAFT → PUBLISHED → ACTIVATED → SUPERSEDED
//
// Rules: publishing freezes content (no in-place edits); activation is
// exclusive per Policy Set; rollback = activating the previous PUBLISHED
// revision; history is never rewritten.
package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"
)

// State of a policy revision.
type State string

const (
	StateDraft      State = "DRAFT"
	StatePublished  State = "PUBLISHED"
	StateActivated  State = "ACTIVATED"
	StateSuperseded State = "SUPERSEDED"
)

// ValidTransitions is the full legal-transition matrix; anything else is
// rejected (issue GWT#2 full-matrix requirement).
var ValidTransitions = map[State][]State{
	StateDraft:     {StatePublished},
	StatePublished: {StateActivated},
	StateActivated: {StateSuperseded},
	// rollback: re-activating a superseded revision is the ONLY legal exit
	// from SUPERSEDED (issue Rollback: 激活上一 PUBLISHED revision)
	StateSuperseded: {StateActivated},
}

// Revision is an immutable, content-addressed policy revision.
type Revision struct {
	SetID       string
	Version     int
	State       State
	Content     Content
	Digest      string
	CreatedAt   time.Time
	PublishedAt *time.Time
	ActivatedAt *time.Time
}

// Content is the policy payload: constraints plus the CEL eligibility
// expression evaluated at plan time.
type Content struct {
	Regions            []string `json:"regions"`
	DataClassMax       string   `json:"data_classification_max"`
	VendorRestrictions []string `json:"vendor_restrictions"`
	ExportRequirements []string `json:"export_requirements"`
	EligibilityCEL     string   `json:"eligibility_cel"`
}

// CanonicalDigest hashes the canonical (sorted, stable) form of content —
// identical content yields identical digests regardless of map ordering.
func (c Content) CanonicalDigest() string {
	parts := []string{
		"regions:" + joinSorted(c.Regions),
		"data_class:" + c.DataClassMax,
		"vendors:" + joinSorted(c.VendorRestrictions),
		"exports:" + joinSorted(c.ExportRequirements),
		"eligibility:" + c.EligibilityCEL,
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func joinSorted(s []string) string {
	out := append([]string(nil), s...)
	sort.Strings(out)
	return strings.Join(out, ",")
}

// Set aggregates revisions of one policy set with its activation pointer.
type Set struct {
	ID        string
	Revisions map[int]*Revision
	ActiveVer int // 0 = none
}

// ErrTransition / ErrConflict / ErrNotFound are the domain errors.
var (
	ErrTransition = fmt.Errorf("illegal policy state transition")
	ErrConflict   = fmt.Errorf("policy conflict")
	ErrNotFound   = fmt.Errorf("policy revision not found")
)

// NewSet creates a policy set with its first DRAFT revision.
func NewSet(id string, content Content) (*Set, *Revision, error) {
	if id == "" {
		return nil, nil, fmt.Errorf("policy set id required")
	}
	r := &Revision{SetID: id, Version: 1, State: StateDraft,
		Content: content, Digest: content.CanonicalDigest(), CreatedAt: time.Now().UTC()}
	s := &Set{ID: id, Revisions: map[int]*Revision{1: r}}
	return s, r, nil
}

// NewDraft appends a NEW revision (never edits an existing one).
func (s *Set) NewDraft(content Content) (*Revision, error) {
	ver := len(s.Revisions) + 1
	r := &Revision{SetID: s.ID, Version: ver, State: StateDraft,
		Content: content, Digest: content.CanonicalDigest(), CreatedAt: time.Now().UTC()}
	s.Revisions[ver] = r
	return r, nil
}

// Publish freezes a draft. Idempotent: republishing the same PUBLISHED
// revision is a no-op; publishing a non-draft, non-published revision is
// an illegal transition (GWT#3).
func (s *Set) Publish(version int) (*Revision, error) {
	r, ok := s.Revisions[version]
	if !ok {
		return nil, ErrNotFound
	}
	switch r.State {
	case StatePublished:
		return r, nil // idempotent no-op
	case StateDraft:
		now := time.Now().UTC()
		r.State = StatePublished
		r.PublishedAt = &now
		return r, nil
	default:
		return nil, fmt.Errorf("%w: %s → PUBLISHED", ErrTransition, r.State)
	}
}

// Activate publishes-then-activates atomically at the domain level:
// the previous ACTIVE revision becomes SUPERSEDED first, exactly one
// ACTIVE revision exists at any point (issue invariant).
func (s *Set) Activate(version int) (*Revision, error) {
	r, ok := s.Revisions[version]
	if !ok {
		return nil, ErrNotFound
	}
	switch r.State {
	case StateActivated:
		return r, nil // idempotent
	case StateDraft:
		if _, err := s.Publish(version); err != nil {
			return nil, err
		}
	case StatePublished, StateSuperseded:
		// direct path; SUPERSEDED = rollback target (re-activation)
	default:
		return nil, fmt.Errorf("%w: %s → ACTIVATED", ErrTransition, r.State)
	}
	if s.ActiveVer != 0 && s.ActiveVer != version {
		prev := s.Revisions[s.ActiveVer]
		prev.State = StateSuperseded
	}
	s.ActiveVer = version
	now := time.Now().UTC()
	r.State = StateActivated
	r.ActivatedAt = &now
	return r, nil
}

// Active returns the single ACTIVE revision (nil if none).
func (s *Set) Active() *Revision {
	if s.ActiveVer == 0 {
		return nil
	}
	return s.Revisions[s.ActiveVer]
}

// ValidateTransition reports whether from→to is legal (matrix helper for
// tests and API layers).
func ValidateTransition(from, to State) bool {
	for _, t := range ValidTransitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

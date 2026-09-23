package binding

import (
	"fmt"
	"testing"
)

// scope 规范化 golden 套件（issue 关闭判定：同语义 scope 必同 hash）。
func TestScopeCanonicalGolden(t *testing.T) {
	a := Scope{TenantRefs: []string{"b", "a", "a", "a"}, Regions: []string{"cn-east", "cn-north", "cn-east"}}
	b := Scope{TenantRefs: []string{"a", "b"}, Regions: []string{"cn-north", "cn-east"}}
	if a.ScopeHash() != b.ScopeHash() {
		t.Fatalf("same-semantics scopes got different hashes:\n  a=%s (canonical %v)\n  b=%s (canonical %v)",
			a.ScopeHash(), a.CanonicalScope(), b.ScopeHash(), b.CanonicalScope())
	}

	// Unicode NFC: decomposed and composed forms produce the same hash
	decomposed := Scope{TenantRefs: []string{"é"}} // e + combining acute
	composed := Scope{TenantRefs: []string{"é"}}    // é as single code point
	if decomposed.ScopeHash() != composed.ScopeHash() {
		t.Fatalf("NFC mismatch: decomposed=%s composed=%s",
			decomposed.ScopeHash(), composed.ScopeHash())
	}

	// empty vs non-empty differ
	empty := Scope{TenantRefs: []string{}}
	nonEmpty := Scope{TenantRefs: []string{""}}
	if empty.ScopeHash() == nonEmpty.ScopeHash() {
		t.Fatal("empty scope same hash as [\"\"] — semantics differ")
	}

	// different order within same dimension is same hash (already covered)
	// different dimensions with same values differ
	s1 := Scope{TenantRefs: []string{"a"}, FactoryRefs: []string{}}
	s2 := Scope{TenantRefs: []string{}, FactoryRefs: []string{"a"}}
	if s1.ScopeHash() == s2.ScopeHash() {
		t.Fatal("tenant_refs=[a] same hash as factory_refs=[a] — dimension position matters")
	}
}

// scope 重叠检测（issue 冲突不变量）。
func TestScopeOverlap(t *testing.T) {
	broad := Scope{TenantRefs: []string{}, Regions: []string{"cn-east"}}
	narrow := Scope{TenantRefs: []string{"tenant-a"}, Regions: []string{"cn-east"}}
	other := Scope{TenantRefs: []string{"tenant-b"}, Regions: []string{"cn-west"}}

	if !broad.Overlaps(narrow) {
		t.Fatal("broad overlaps narrow in regions")
	}
	if !narrow.Overlaps(broad) {
		t.Fatal("overlap should be symmetric")
	}
	if narrow.Overlaps(other) {
		t.Fatal("different tenant + different region should not overlap")
	}
}

// 生命周期矩阵全测试。
func TestTransitionMatrix(t *testing.T) {
	states := []State{StateDraft, StatePublished, StateSuspended, StateDeprecated, StateRetired}
	legal := map[State][]State{
		StateDraft:      {StatePublished},
		StatePublished:  {StateSuspended, StateDeprecated},
		StateSuspended:  {StatePublished, StateRetired},
		StateDeprecated: {StateRetired},
		StateRetired:    {},
	}
	for _, from := range states {
		for _, to := range states {
			if from == to {
				continue
			}
			want := false
			for _, x := range legal[from] {
				if x == to {
					want = true
				}
			}
			if got := ValidateTransition(from, to); got != want {
				t.Errorf("ValidateTransition(%s→%s) = %v, want %v", from, to, got, want)
			}
		}
	}
}

// 发布门控。
func TestPublishGate(t *testing.T) {
	cases := []struct {
		approval, reason string
		wantErr          bool
	}{
		{"approval:1", "fix", false},
		{"", "fix", true},
		{"approval:1", "", true},
		{"", "", true},
	}
	for _, tc := range cases {
		err := PublishGate(tc.approval, tc.reason, "")
		if (err != nil) != tc.wantErr {
			t.Fatalf("PublishGate(%q,%q) = %v, wantErr %v", tc.approval, tc.reason, err, tc.wantErr)
		}
	}
}

// 禁止字段。
func TestScopeForbiddenFields(t *testing.T) {
	bad := []byte(`{"tenant_refs":["a"],"credentials":"secret"}`)
	if err := CheckScopeFields(bad); err == nil {
		t.Fatal("forbidden scope key accepted")
	}
	good := []byte(`{"tenant_refs":["a"],"regions":["cn-east"]}`)
	if err := CheckScopeFields(good); err != nil {
		t.Fatalf("clean scope rejected: %v", err)
	}
}

func TestBindingString(t *testing.T) {
	b := &Binding{BindingKey: "b1", Revision: 3, State: StatePublished, ScopeHash: "sha256:abc"}
	_ = fmt.Sprintf("%v", b) // no panic
}

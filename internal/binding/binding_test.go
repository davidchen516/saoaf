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

	// R1 P1-1 通配符语义：空 = unrestricted（specs §1.2），通配维与任何具体
	// 集合同维重叠——审查探针实证过旧实现放行 {tenant-a} vs {tenant-a,tenant-b}。
	wildcardTenant := Scope{TenantRefs: []string{}, Regions: []string{"cn-east"}}
	specificTenant := Scope{TenantRefs: []string{"tenant-a"}, Regions: []string{"cn-east"}}
	if !wildcardTenant.Overlaps(specificTenant) || !specificTenant.Overlaps(wildcardTenant) {
		t.Fatal("empty (unrestricted) tenant_refs must overlap any specific tenant set")
	}

	// 部分重叠（非完全相等）：tenant 维交集非空即重叠
	sup := Scope{TenantRefs: []string{"tenant-a", "tenant-b"}, Regions: []string{"cn-east"}}
	sub := Scope{TenantRefs: []string{"tenant-a"}, Regions: []string{"cn-east"}}
	if !sup.Overlaps(sub) {
		t.Fatal("partially intersecting scopes must overlap")
	}

	// AND 跨维度：一个维度不相交（且双方都非通配）→ 不重叠，即使另一维重叠
	left := Scope{TenantRefs: []string{}, Regions: []string{"cn-east"}}
	right := Scope{TenantRefs: []string{"tenant-a"}, Regions: []string{"cn-west"}}
	if left.Overlaps(right) {
		t.Fatal("disjoint regions must prevent overlap even with wildcard tenants")
	}

	// 全通配 scope 与一切重叠
	all := Scope{}
	if !all.Overlaps(other) {
		t.Fatal("fully unrestricted scope overlaps everything")
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
		approval, reason, actor string
		wantErr                 bool
	}{
		{"approval:1", "fix", "user:alice", false},
		{"", "fix", "user:alice", true},
		{"approval:1", "", "user:alice", true},
		{"approval:1", "fix", "", true}, // R1 P2-2：审计必须能归因操作主体
		{"", "", "", true},
	}
	for _, tc := range cases {
		err := PublishGate(tc.approval, tc.reason, tc.actor)
		if (err != nil) != tc.wantErr {
			t.Fatalf("PublishGate(%q,%q,%q) = %v, wantErr %v", tc.approval, tc.reason, tc.actor, err, tc.wantErr)
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

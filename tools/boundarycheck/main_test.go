package main

import "testing"

const modulePath = "github.com/davidchen516/saoaf"

func TestModuleOf(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{modulePath, "root"},
		{modulePath + "/cmd/control-plane-api", "cmd"},
		{modulePath + "/tools/boundarycheck", "tools"},
		{modulePath + "/internal/platform/httpapi", "platform"},
		{modulePath + "/internal/registry", "internal:registry"},
		{modulePath + "/internal/registry/store", "internal:registry"},
		{"github.com/go-chi/chi/v5", ""},
		{"net/http", ""},
	}
	for _, tc := range cases {
		if got := moduleOf(tc.path, modulePath); got != tc.want {
			t.Errorf("moduleOf(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestAllowedEdges(t *testing.T) {
	cases := []struct {
		src, dst string
		want     bool
	}{
		{"cmd", "internal:registry", true},
		{"cmd", "platform", true},
		{"cmd", "cmd", true},
		{"internal:registry", "platform", true},
		{"internal:registry", "internal:registry", true},
		{"internal:registry", "internal:resolver", false}, // module → module: forbidden
		{"internal:resolver", "internal:registry", false}, // reverse direction: also forbidden
		{"platform", "internal:registry", false},
		{"platform", "platform", true},
		{"tools", "internal:registry", false},
		{"tools", "tools", true},
		{"root", "internal:registry", false},
	}
	for _, tc := range cases {
		if got := allowedEdge(tc.src, tc.dst); got != tc.want {
			t.Errorf("allowedEdge(%q, %q) = %v, want %v", tc.src, tc.dst, got, tc.want)
		}
	}
}

func TestCheckDetectsReverseDependency(t *testing.T) {
	// Red-run shape used by the I02 injection evidence: resolver (a future
	// consumer) importing registry is forbidden; registry→platform is fine.
	packages := []pkg{
		{ImportPath: modulePath + "/internal/registry", Imports: []string{modulePath + "/internal/platform/httpapi"}},
		{ImportPath: modulePath + "/internal/resolver", Imports: []string{modulePath + "/internal/registry"}},
		{ImportPath: modulePath + "/cmd/control-plane-api", Imports: []string{modulePath + "/internal/registry", modulePath + "/internal/resolver"}},
	}
	violations := check(packages, modulePath)
	if len(violations) != 1 {
		t.Fatalf("violations = %v, want exactly the resolver→registry edge", violations)
	}
	if violations[0].To != modulePath+"/internal/registry" {
		t.Fatalf("violation target = %s, want internal/registry", violations[0].To)
	}
}

func TestCheckPassesOnCleanGraph(t *testing.T) {
	packages := []pkg{
		{ImportPath: modulePath + "/internal/platform/httpapi", Imports: []string{"net/http"}},
		{ImportPath: modulePath + "/internal/registry", Imports: []string{modulePath + "/internal/platform/httpapi"}},
		{ImportPath: modulePath + "/cmd/control-plane-api", Imports: []string{modulePath + "/internal/platform/httpapi"}},
	}
	if v := check(packages, modulePath); len(v) != 0 {
		t.Fatalf("clean graph produced violations: %v", v)
	}
}

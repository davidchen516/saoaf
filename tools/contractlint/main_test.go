package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestJSONDepth(t *testing.T) {
	cases := []struct {
		doc  string
		want int
	}{
		{`{}`, 1},
		{`{"a":1}`, 2},
		{`{"a":{"b":1}}`, 3},
		{`[1,[2,[3]]]`, 4},
		{`"scalar"`, 1},
	}
	for _, tc := range cases {
		var doc any
		if err := json.Unmarshal([]byte(tc.doc), &doc); err != nil {
			t.Fatal(err)
		}
		if got := jsonDepth(doc); got != tc.want {
			t.Errorf("jsonDepth(%s) = %d, want %d", tc.doc, got, tc.want)
		}
	}
}

func TestClassifySchemaError(t *testing.T) {
	cases := []struct {
		msg, want string
	}{
		{"- at '': additional properties 'zz' not allowed", "VALIDATION_UNKNOWN_FIELD"},
		{"- at '/error_code': value must be one of 'A', 'B'", "VALIDATION_INVALID_ENUM"},
		{"- at '': missing property 'request_id'", "VALIDATION_MISSING_REQUIRED"},
		{"- at '/x': got string, want number", "SEMANTIC_INVALID"},
	}
	for _, tc := range cases {
		if got := classifySchemaError(tc.msg); got != tc.want {
			t.Errorf("classify(%q) = %s, want %s", tc.msg, got, tc.want)
		}
	}
}

func TestExampleSchemaFor(t *testing.T) {
	// the mapping reads the real schemas dir via a repo-root relative path;
	// go test runs with cwd = package dir, so step up to the repo root.
	wd, _ := os.Getwd()
	if err := os.Chdir(filepath.Join(wd, "..", "..")); err != nil {
		t.Skipf("cannot chdir to repo root: %v", err)
	}
	defer func() { _ = os.Chdir(wd) }()
	cases := map[string]string{
		"error-envelope.json":        "error-envelope.json",
		"cloudevents-published.json": "cloudevents-envelope.json",
		"pagination.json":            "pagination.json",
		"idempotency.json":           "idempotency.json",
		"no-such-example.json":       "",
	}
	for name, want := range cases {
		if got := exampleSchemaFor(name); got != want {
			t.Errorf("exampleSchemaFor(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestForbiddenScanDetectsPayloadFragment(t *testing.T) {
	var problems []problem
	// in-memory check via walkForbidden on a temp dir
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/bad.json", []byte(`{"prompt": "user prompt text"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// walkForbidden walks a root dir relative to cwd; use absolute path
	if err := walkForbidden(dir, func(f, m string) {
		problems = append(problems, problem{f, m})
	}); err != nil {
		t.Fatal(err)
	}
	if len(problems) == 0 {
		t.Fatal("forbidden scan missed injected 'prompt' field")
	}
	if !strings.Contains(problems[0].msg, "forbidden") {
		t.Fatalf("unexpected problem: %v", problems[0])
	}
}

// TestSyncInfraScanDetectionPins locks the I20 R1 P1/P2 hardening: the
// canonical violation shapes must be caught (camelCase operationIds,
// REST POST-on-noun, async-word bypass attempts) while async planning
// operations stay green. Each case reproduces an operation block and
// runs the scanner's decision logic over the parsed document.
func TestSyncInfraScanDetectionPins(t *testing.T) {
	// reuse the scanner's regexes via the shared helpers
	if testing.Short() {
		t.Skip("short mode")
	}
	cases := []struct {
		name    string
		method  string
		path    string
		opID    string
		summary string
		desc    string
		wantHit bool
	}{
		{"A createGpuNode no desc", "post", "/v1/zones/{zoneId}/gpus", "createGpuNode", "", "", true},
		{"C provisionNode camel", "post", "/v1/nodes", "provisionNode", "", "", true},
		{"D kebab create-gpu-node", "post", "/v1/cluster", "create-gpu-node", "", "", true},
		{"E async-word bypass attempt", "post", "/v1/zones/{zoneId}/gpus", "createGpuNode", "", "Create a GPU node in the zone. async.", true},
		{"F canonical REST POST on noun", "post", "/v1/gpu-nodes", "createGpuNode", "", "", true},
		{"B sync description", "post", "/v1/zones/{zoneId}/gpus", "createGpuNode", "", "Create a GPU node synchronously.", true},
		{"legit async plan submit", "post", "/v1/placement-plans", "submitPlacementPlan", "", "Submit an ASYNC placement plan request (control-plane): the Owner applies it with the AI Factory.", false},
		{"legit validate profile", "post", "/v1/profiles/validate", "validatePortableProfile", "", "Validate a Portable Profile against a zone — a PURE, read-only check.", false},
		{"legit plan status", "get", "/v1/placement-plans/{planId}", "getPlacementPlanStatus", "", "Status query — the disconnect-recovery convergence point.", false},
	}
	for _, tc := range cases {
		got := syncInfraViolation(tc.method, tc.path, tc.opID, tc.summary, tc.desc)
		if got != tc.wantHit {
			t.Errorf("%s: violation=%v, want %v", tc.name, got, tc.wantHit)
		}
	}
}

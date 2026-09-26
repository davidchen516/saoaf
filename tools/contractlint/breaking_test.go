package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestGitOutRefPathValidation locks the P0 regression: `git show ref:path`
// arguments must validate the REF part (charset) and the PATH part
// (repo-relative) separately — the whole-argument check broke every
// breaking invocation once contracts existed at the base ref.
func TestGitOutRefPathValidation(t *testing.T) {
	cases := []struct {
		name    string
		refArg  string
		wantErr bool
	}{
		{"plain sha", "3aef102bf00:contracts/README.md", false},
		{"head", "HEAD:contracts/README.md", false},
		{"head tilde", "HEAD~1:contracts/schemas/v1/error-envelope.json", false},
		{"colon injection in ref", "HEAD;rm -rf:contracts/README.md", true},
		{"space in ref", "HEAD x:contracts/README.md", true},
		{"path traversal", "HEAD:../outside.txt", true},
		{"absolute path", "HEAD:/etc/passwd", true},
	}
	_ = cases
	// structural validation only (no git invocation)
	for _, tc := range cases {
		ref, path, isShow := strings.Cut(tc.refArg, ":")
		if !isShow {
			t.Fatalf("%s: expected ref:path shape", tc.name)
		}
		errRef := validateGitRef(ref)
		errPath := gitPathPattern.MatchString(path)
		gotErr := errRef != nil || !errPath
		if gotErr != tc.wantErr {
			t.Errorf("%s: refErr=%v pathOK=%v, wantErr=%v", tc.name, errRef, errPath, tc.wantErr)
		}
	}
}

// TestGitOutAcceptsRefPath runs the real gate entry against a scratch git
// repository — the regression the P0 slipped through: after committing a
// contract file, breaking against the commit with NO changes must pass.
func TestGitOutAcceptsRefPath(t *testing.T) {
	dir := t.TempDir()
	if err := run(t, dir, "git", "init", "-q"); err != nil {
		t.Skipf("git init unavailable: %v", err)
	}
	_ = run(t, dir, "git", "config", "user.email", "t@t")
	_ = run(t, dir, "git", "config", "user.name", "t")
	if err := os.MkdirAll(filepath.Join(dir, "contracts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "contracts", "a.json"), []byte(`{"v":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "contracts", "b.json"), []byte(`{"v":2,"required":["x","y"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = run(t, dir, "git", "add", "-A")
	if err := run(t, dir, "git", "commit", "-qm", "base"); err != nil {
		t.Fatal(err)
	}

	wd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(wd) }()

	// git show ref:path must be accepted (P0: was rejected wholesale)
	if _, err := gitOut("show", "HEAD:contracts/a.json"); err != nil {
		t.Fatalf("gitOut rejected valid ref:path argument: %v", err)
	}
	// malicious refs must be rejected
	if _, err := gitOut("show", "HEAD;evil:contracts/a.json"); err == nil {
		t.Fatal("gitOut accepted malicious ref")
	}
	// full breaking pass on unchanged tree
	if err := cmdBreaking("HEAD"); err != nil {
		t.Fatalf("breaking on unchanged tree must PASS, got: %v", err)
	}
	// additive change must pass
	if err := os.WriteFile(filepath.Join(dir, "contracts", "b.json"), []byte(`{"v":2}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cmdBreaking("HEAD"); err != nil {
		t.Fatalf("breaking on additive change must PASS, got: %v", err)
	}
	// breaking change must fail: required-entry REMOVAL from the committed b.json
	if err := os.WriteFile(filepath.Join(dir, "contracts", "b.json"), []byte(`{"v":2,"required":["x"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cmdBreaking("HEAD"); err == nil {
		t.Fatal("breaking on required-entry removal must FAIL")
	}
	// commit a version WITH enum, then narrow it — removal must fail
	if err := os.WriteFile(filepath.Join(dir, "contracts", "b.json"), []byte(`{"v":2,"required":["x","y"],"enum":["a","b"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run(t, dir, "git", "add", "-A"); err != nil {
		t.Fatal(err)
	}
	if err := run(t, dir, "git", "commit", "-qm", "with enum"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "contracts", "b.json"), []byte(`{"v":2,"required":["x","y"],"enum":["a"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cmdBreaking("HEAD"); err == nil {
		t.Fatal("breaking on enum narrowing must FAIL")
	}
}

func run(t *testing.T, dir string, args ...string) error {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("%v: %s", args, out)
	}
	return err
}

// TestFindBreakingWholePathRemoval locks the P1-3 fix: deleting an entire
// OpenAPI path is breaking even though the map-key rule for ".paths."
// never fires at the ".paths" level.
func TestFindBreakingWholePathRemoval(t *testing.T) {
	base := map[string]any{
		"paths": map[string]any{
			"/healthz": map[string]any{"get": map[string]any{"operationId": "getLiveness"}},
			"/readyz":  map[string]any{"get": map[string]any{"operationId": "getReadiness"}},
		},
	}
	cur := map[string]any{
		"paths": map[string]any{
			"/healthz": map[string]any{"get": map[string]any{"operationId": "getLiveness"}},
		},
	}
	var out []string
	findBreaking("openapi.yaml", base, cur, "$", &out)
	found := false
	for _, o := range out {
		if strings.Contains(o, "path \"/readyz\" removed") {
			found = true
		}
	}
	if !found {
		t.Fatalf("whole-path removal not detected: %v", out)
	}
}

// TestObjectArrayShrinkageIsBreaking locks the I19 R1 P2-1 regression:
// deleting a TAIL entry of an object array (e.g. the last if/then
// invariant in an allOf) must be flagged — the positional compare
// previously stopped at min(len) and tail deletions evaded the gate.
func TestObjectArrayShrinkageIsBreaking(t *testing.T) {
	base := map[string]any{
		"allOf": []any{
			map[string]any{"if": map[string]any{"properties": map[string]any{"state": map[string]any{"enum": []any{"COMPLETED"}}}}},
			map[string]any{"if": map[string]any{"properties": map[string]any{"state": map[string]any{"enum": []any{"FAILED"}}}}},
			map[string]any{"if": map[string]any{"properties": map[string]any{"state": map[string]any{"const": "CANCELED"}}}},
		},
	}
	cur := map[string]any{
		"allOf": []any{
			base["allOf"].([]any)[0],
			base["allOf"].([]any)[1],
			// entry [2] dropped — the gate must say so
		},
	}
	var out []string
	findBreaking("schema.json", base, cur, "$", &out)
	if len(out) == 0 {
		t.Fatal("tail-entry deletion of an object array was not flagged as breaking")
	}
	found := false
	for _, m := range out {
		if strings.Contains(m, "shrinkage") && strings.Contains(m, "[2]") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a shrinkage hit at [2], got: %v", out)
	}
	// growing the array stays additive (no hit)
	grown := map[string]any{
		"allOf": append(append([]any{}, base["allOf"].([]any)...), map[string]any{"x": true}),
	}
	out = nil
	findBreaking("schema.json", base, grown, "$", &out)
	for _, m := range out {
		if strings.Contains(m, "shrinkage") {
			t.Fatalf("array GROWTH must not be flagged, got: %s", m)
		}
	}
}

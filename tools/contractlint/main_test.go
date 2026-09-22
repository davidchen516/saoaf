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
		if got := classifySchemaError(tc.msg, ""); got != tc.want {
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

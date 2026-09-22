package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadAllowlistIgnoresCommentsAndBlanks(t *testing.T) {
	f := filepath.Join(t.TempDir(), "allow.txt")
	writeFile(t, f, "# comment\n\ngithub.com/go-chi/chi/v5  # trailing comment\n\n")
	got, err := loadAllowlist(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got["github.com/go-chi/chi/v5"] {
		t.Fatalf("allowlist = %v", got)
	}
}

func TestNpmResolvedPackages(t *testing.T) {
	lock := packageLock{Packages: map[string]struct {
		Version string `json:"version"`
	}{
		"":                                       {Version: "0.1.0"}, // root: excluded
		"node_modules/react":                     {Version: "19.3.0"},
		"node_modules/@vitejs/plugin-react":      {Version: "5.2.0"},
		"node_modules/vite/node_modules/esbuild": {Version: "1.2.3"},
		"node_modules/postcss":                   {Version: "8.5.6"},
	}}
	got := npmResolvedPackages(lock)
	want := []string{"@vitejs/plugin-react", "esbuild", "postcss", "react"}
	if len(got) != len(want) {
		t.Fatalf("resolved = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("resolved[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}

// TestNpmTransitiveCoverageAgainstRealLockfile guards review finding P2-2:
// the gate must see every resolved package, not only direct declarations.
// It runs against the real web/package-lock.json when present.
func TestNpmTransitiveCoverageAgainstRealLockfile(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "web", "package-lock.json"))
	if err != nil {
		t.Skipf("lockfile not reachable from test cwd: %v", err)
	}
	var lock packageLock
	if err := json.Unmarshal(raw, &lock); err != nil {
		t.Fatalf("parse lockfile: %v", err)
	}
	resolved := npmResolvedPackages(lock)
	if len(resolved) < 50 {
		t.Fatalf("expected a realistic transitive graph (>=50), got %d — extraction broken?", len(resolved))
	}
	allow, err := loadAllowlist(filepath.Join("..", "allow-npm.txt"))
	if err != nil {
		t.Fatalf("load allowlist: %v", err)
	}
	var missing []string
	for _, name := range resolved {
		if !allow[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("npm packages resolved but not allowlisted (transitive coverage gap): %v", missing)
	}
}

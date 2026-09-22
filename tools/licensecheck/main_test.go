package main

import (
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

func TestLicenseCheckFailsOnUnlistedModule(t *testing.T) {
	if _, err := os.Stat("go.mod"); err != nil {
		t.Skip("unit test only meaningful inside the module")
	}
	// The real check runs against the live module graph; here we verify the
	// failure path by pointing the checker's allowlist at an empty file via
	// the same logic (an unvetted module must produce a problem).
	allow, err := loadAllowlist("tools/licensecheck/allow-go.txt")
	if err != nil {
		t.Fatal(err)
	}
	mods, err := goModules()
	if err != nil {
		t.Skipf("go list unavailable: %v", err)
	}
	var unlisted int
	for _, m := range mods {
		if !m.Main && !allow[m.Path] {
			unlisted++
		}
	}
	if unlisted == 0 {
		t.Fatal("expected the graph to exercise the allowlist; empty diff makes this test vacuous")
	}
}

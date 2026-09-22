// Command boundarycheck enforces SAOAF module dependency direction (I02).
//
// Rules (see docs/architecture/technology-stack.md §1 — "模块间禁止跨边界
// 直接写表" — and docs/programs/03-sovereign-ai-open-ai-fabric/development-plan.md §3):
//
//   - cmd/*            may import any internal package.
//   - internal/platform/* may import internal/platform/* only (shared leaf).
//   - internal/<module>/* may import internal/platform/* and internal/<module>/*.
//   - tools/*          may import tools/* only (CI helpers stay decoupled).
//   - Root packages    may import root packages only.
//
// Any module-to-module import (e.g. internal/registry importing
// internal/resolver) fails the check with a nonzero exit. Future issues that
// need a sanctioned edge must add it here with the referencing issue.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

type pkg struct {
	ImportPath   string   `json:"ImportPath"`
	Imports      []string `json:"Imports"`
	TestImports  []string `json:"TestImports"`
	XTestImports []string `json:"XTestImports"`
}

// moduleOf maps an import path inside this repository to its module bucket:
// "cmd", "platform", "tools", "root", or the internal module name.
// Paths outside the repository return "".
func moduleOf(importPath, modulePath string) string {
	if importPath == modulePath {
		return "root"
	}
	if !strings.HasPrefix(importPath, modulePath+"/") {
		return "" // external or stdlib
	}
	rel := strings.TrimPrefix(importPath, modulePath+"/")
	switch {
	case rel == "cmd" || strings.HasPrefix(rel, "cmd/"):
		return "cmd"
	case rel == "tools" || strings.HasPrefix(rel, "tools/"):
		return "tools"
	case rel == "internal/platform" || strings.HasPrefix(rel, "internal/platform/"):
		return "platform"
	case strings.HasPrefix(rel, "internal/"):
		rest := strings.TrimPrefix(rel, "internal/")
		name, _, _ := strings.Cut(rest, "/")
		return "internal:" + name
	default:
		return "root"
	}
}

// allowedEdge reports whether src (module bucket) may import dst (bucket).
func allowedEdge(src, dst string) bool {
	if src == dst {
		return true // same module: always fine
	}
	switch src {
	case "cmd":
		return dst == "platform" || strings.HasPrefix(dst, "internal:")
	case "platform":
		return false // platform imports only itself
	case "tools":
		return false // tools stay decoupled from the app
	case "root":
		return false
	default: // internal:<module>
		return dst == "platform"
	}
}

type violation struct {
	From, To, Rule string
}

// check validates a package graph. It returns one violation per disallowed
// internal import edge. Test files (_test.go in-package and external test
// packages) are included: a reverse dependency smuggled through a test file
// is still a boundary violation.
func check(packages []pkg, modulePath string) []violation {
	var out []violation
	seen := map[string]bool{}
	for _, p := range packages {
		src := moduleOf(p.ImportPath, modulePath)
		if src == "" {
			continue
		}
		allImports := append(append(append([]string{}, p.Imports...), p.TestImports...), p.XTestImports...)
		for _, imp := range allImports {
			dst := moduleOf(imp, modulePath)
			if dst == "" {
				continue // external or stdlib: allowed
			}
			if !allowedEdge(src, dst) && !seen[p.ImportPath+"->"+imp] {
				seen[p.ImportPath+"->"+imp] = true
				out = append(out, violation{
					From: p.ImportPath,
					To:   imp,
					Rule: fmt.Sprintf("%s may not import %s", src, dst),
				})
			}
		}
	}
	return out
}

func main() {
	// resolve module path via go list so the tool works from any cwd inside the module
	out, err := exec.Command("go", "list", "-m").Output()
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolving module path: %v\n", err)
		os.Exit(2)
	}
	modulePath := strings.TrimSpace(string(out))
	// Test-file imports are surfaced explicitly: go 1.27's `go list -json`
	// does not include _test.go imports (the historical -tests flag was
	// removed), so a second list pass with a template extracts
	// TestImports/XTestImports for every package. Imports smuggled through
	// test files are boundary-checked like production imports (review P2-1).
	raw, err := exec.Command("go", "list", "-json", "./...").Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			fmt.Fprintf(os.Stderr, "go list ./...: %v\n%s\n", err, ee.Stderr)
		} else {
			fmt.Fprintf(os.Stderr, "go list ./...: %v\n", err)
		}
		os.Exit(2)
	}
	testRaw, err := exec.Command("go", "list",
		"-f", `{{if .TestImports}}{{range .TestImports}}{{$.ImportPath}} {{.}}{{"\n"}}{{end}}{{end}}{{if .XTestImports}}{{range .XTestImports}}{{$.ImportPath}} {{.}}{{"\n"}}{{end}}{{end}}`,
		"./...").Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			fmt.Fprintf(os.Stderr, "go list (tests): %v\n%s\n", err, ee.Stderr)
		} else {
			fmt.Fprintf(os.Stderr, "go list (tests): %v\n", err)
		}
		os.Exit(2)
	}
	// Merge test-import edges into the package list so check() sees them.
	testImports := map[string][]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(testRaw)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, " ", 2)
		if len(parts) == 2 {
			testImports[parts[0]] = append(testImports[parts[0]], parts[1])
		}
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	var packages []pkg
	for dec.More() {
		var p pkg
		if err := dec.Decode(&p); err != nil {
			fmt.Fprintf(os.Stderr, "decode go list output: %v\n", err)
			os.Exit(2)
		}
		if ti, ok := testImports[p.ImportPath]; ok {
			p.TestImports = append(p.TestImports, ti...)
		}
		packages = append(packages, p)
	}
	violations := check(packages, modulePath)
	if len(violations) == 0 {
		fmt.Printf("BOUNDARY CHECK: PASS (%d packages)\n", len(packages))
		return
	}
	fmt.Println("BOUNDARY CHECK: FAIL")
	for _, v := range violations {
		fmt.Printf("  - %s imports %s (%s)\n", v.From, v.To, v.Rule)
	}
	os.Exit(1)
}

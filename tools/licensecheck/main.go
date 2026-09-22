// Command licensecheck enforces the SAOAF dependency license policy (I02).
//
// Every third-party dependency must appear on an explicit allowlist:
//   - Go modules:   allow-go.txt    (checked against `go list -m -json all`,
//     including indirect modules)
//   - npm packages: allow-npm.txt   (checked against every package resolved
//     in web/package-lock.json — direct AND transitive)
//
// Adding a dependency therefore requires a conscious allowlist entry — an
// unvetted transitive dependency fails CI. This is the "license policy"
// merge gate; the SBOM (syft) is produced separately in the release job.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
)

type goModule struct {
	Path     string `json:"Path"`
	Main     bool   `json:"Main"`
	Indirect bool   `json:"Indirect"`
}

// packageJSON models the direct-declaration part of web/package.json.
type packageJSON struct {
	Name            string            `json:"name"`
	Dependencies    map[string]string `json:"dependencies"`
	DevDependencies map[string]string `json:"devDependencies"`
}

// packageLock models the resolved-package graph of web/package-lock.json.
// Only the `packages` map is needed: keys are node_modules paths, values
// carry the resolved version. Root is the "" key.
type packageLock struct {
	Packages map[string]struct {
		Version string `json:"version"`
	} `json:"packages"`
}

func loadAllowlist(path string) (map[string]bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(strings.SplitN(sc.Text(), "#", 2)[0])
		if line != "" {
			out[line] = true
		}
	}
	return out, sc.Err()
}

func goModules() ([]goModule, error) {
	raw, err := exec.Command("go", "list", "-m", "-json", "all").Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("go list -m: %v\n%s", err, ee.Stderr)
		}
		return nil, err
	}
	var mods []goModule
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	for dec.More() {
		var m goModule
		if err := dec.Decode(&m); err != nil {
			return nil, err
		}
		mods = append(mods, m)
	}
	return mods, nil
}

// Paths are module-root relative constants: the tool is invoked from the
// repository root (CI: go run ./tools/licensecheck). No runtime path input
// exists, so there is no path-traversal surface (gosec G703 stays clean
// without suppressions).
const (
	goAllowlistPath  = "tools/licensecheck/allow-go.txt"
	npmAllowlistPath = "tools/licensecheck/allow-npm.txt"
	webPkgPath       = "web/package.json"
	npmLockPath      = "web/package-lock.json"
)

// npmResolvedPackages extracts every resolved package name from a
// package-lock.json packages map (node_modules paths, excluding the root).
func npmResolvedPackages(lock packageLock) []string {
	var names []string
	for p := range lock.Packages {
		if p == "" {
			continue // root package
		}
		// keys look like "node_modules/react" or "node_modules/vite/node_modules/esbuild"
		if !strings.HasPrefix(p, "node_modules/") {
			continue
		}
		parts := strings.Split(p, "/")
		name := parts[len(parts)-1]
		if len(parts) >= 3 && strings.HasPrefix(parts[len(parts)-2], "@") {
			// scoped package: "@scope/name" spans the last two segments
			name = parts[len(parts)-2] + "/" + name
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func main() {
	var problems []string

	goAllow, err := loadAllowlist(goAllowlistPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load go allowlist: %v\n", err)
		os.Exit(2)
	}
	mods, err := goModules()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(2)
	}
	var goDeps []string
	for _, m := range mods {
		if m.Main {
			continue
		}
		goDeps = append(goDeps, m.Path)
		if !goAllow[m.Path] {
			problems = append(problems, fmt.Sprintf("go module not on allowlist: %s", m.Path))
		}
	}

	npmAllow, err := loadAllowlist(npmAllowlistPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load npm allowlist: %v\n", err)
		os.Exit(2)
	}
	pjRaw, err := os.ReadFile(webPkgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read %s: %v\n", webPkgPath, err)
		os.Exit(2)
	}
	var pj packageJSON
	if err := json.Unmarshal(pjRaw, &pj); err != nil {
		fmt.Fprintf(os.Stderr, "parse %s: %v\n", webPkgPath, err)
		os.Exit(2)
	}
	direct := append(keys(pj.Dependencies), keys(pj.DevDependencies)...)

	// Transitive coverage: every package the lockfile resolves must be
	// allowlisted too — an unvetted transitive dependency fails CI, same
	// as the Go side (go list -m all includes indirect).
	lockRaw, err := os.ReadFile(npmLockPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read %s: %v (run npm install in web/ to regenerate)\n", npmLockPath, err)
		os.Exit(2)
	}
	var lock packageLock
	if err := json.Unmarshal(lockRaw, &lock); err != nil {
		fmt.Fprintf(os.Stderr, "parse %s: %v\n", npmLockPath, err)
		os.Exit(2)
	}
	resolved := npmResolvedPackages(lock)

	for _, name := range direct {
		if !npmAllow[name] {
			problems = append(problems, fmt.Sprintf("npm package not on allowlist: %s", name))
		}
	}
	for _, name := range resolved {
		if !npmAllow[name] {
			problems = append(problems, fmt.Sprintf("npm transitive package not on allowlist: %s", name))
		}
	}
	fmt.Printf("LICENSE CHECK: npm direct: %d, resolved: %d\n", len(direct), len(resolved))

	sort.Strings(goDeps)
	fmt.Printf("LICENSE CHECK: go deps: %s\n", strings.Join(goDeps, ", "))
	if len(problems) > 0 {
		fmt.Println("LICENSE CHECK: FAIL")
		for _, p := range problems {
			fmt.Println("  - " + p)
		}
		fmt.Println("hint: add the dependency to tools/licensecheck/allow-go.txt or allow-npm.txt after vetting its license")
		os.Exit(1)
	}
	fmt.Println("LICENSE CHECK: PASS (all third-party dependencies allowlisted)")
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

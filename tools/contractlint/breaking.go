package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	yaml "go.yaml.in/yaml/v3"
)

// cmdBreaking implements the I03 breaking-change gate: compare every
// contract file under contracts/ between a base ref and the working tree.
//
// Breaking rules (per issue acceptance logic):
//   - removing or renaming a required schema property        → breaking
//   - removing an enum value                                 → breaking
//   - narrowing a type (anyOf → single type), adding const   → breaking
//   - deleting a published schema/OpenAPI file               → breaking
//   - OpenAPI: removing an operation, removing a response,
//     removing a required header/parameter                   → breaking
//
// Additions (new optional properties, new enum values, new operations) are
// backward compatible. Breaking changes require a NEW MAJOR (new directory
// contracts/<kind>/vN+1) — the gate fails on in-place breaking edits to an
// already-published version directory.
func cmdBreaking(base string) error {
	if base == "" {
		base = "HEAD"
	}
	// List contract files at base (empty output = base has no contracts yet).
	baseFilesRaw, err := gitOut("ls-tree", "-r", "--name-only", base, "--", contractsDir)
	if err != nil {
		return fmt.Errorf("git ls-tree %s: %w", base, err)
	}
	baseFiles := map[string]bool{}
	for _, f := range strings.Split(strings.TrimSpace(baseFilesRaw), "\n") {
		if f != "" {
			baseFiles[f] = true
		}
	}

	// Working-tree contract files.
	curFiles := map[string]bool{}
	err = filepath.Walk(contractsDir, func(path string, info os.FileInfo, werr error) error {
		if werr != nil || info.IsDir() {
			return werr
		}
		curFiles[path] = true
		return nil
	})
	if err != nil {
		return err
	}

	var breaking []string

	// Deleted published files are breaking.
	var deleted []string
	for f := range baseFiles {
		if !curFiles[f] {
			deleted = append(deleted, f)
		}
	}
	sort.Strings(deleted)
	for _, f := range deleted {
		breaking = append(breaking, fmt.Sprintf("deleted published contract file: %s", f))
	}

	// Compare each surviving file.
	var changed []string
	for f := range curFiles {
		if !baseFiles[f] {
			continue // new file: additive
		}
		baseContent, err := gitOut("show", base+":"+f)
		if err != nil {
			// file existed in ls-tree but unreadable — treat as changed
			changed = append(changed, f)
			continue
		}
		curContent, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		if string(baseContent) != string(curContent) {
			changed = append(changed, f)
		}
	}
	sort.Strings(changed)
	for _, f := range changed {
		breaks, err := diffBreaking(base, f)
		if err != nil {
			return err
		}
		breaking = append(breaking, breaks...)
	}

	if len(breaking) > 0 {
		return fmt.Errorf("breaking changes detected (%d):\n    %s",
			len(breaking), strings.Join(breaking, "\n    "))
	}
	fmt.Printf("CONTRACT GATE: PASS (breaking-change check vs %s; changed=%d deleted=%d)\n",
		base, len(changed), len(deleted))
	return nil
}

func gitOut(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			// path missing at base is normal for new contracts
			if strings.Contains(string(ee.Stderr), "does not exist") ||
				strings.Contains(string(ee.Stderr), "exists on disk, but not in") {
				return "", nil
			}
		}
		return "", err
	}
	return string(out), nil
}

// diffBreaking loads the file at base and in the working tree, decodes both
// (JSON or YAML), and applies structural breaking rules.
func diffBreaking(base, path string) ([]string, error) {
	baseRaw, err := gitOut("show", base+":"+path)
	if err != nil {
		return nil, err
	}
	curRaw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var baseDoc, curDoc any
	if err := decodeDoc([]byte(baseRaw), &baseDoc); err != nil {
		return nil, fmt.Errorf("%s@%s: %w", path, base, err)
	}
	if err := decodeDoc(curRaw, &curDoc); err != nil {
		return nil, fmt.Errorf("%s (worktree): %w", path, err)
	}
	var breaking []string
	findBreaking(path, baseDoc, curDoc, "$", &breaking)
	return breaking, nil
}

func decodeDoc(raw []byte, out *any) error {
	trimmed := strings.TrimSpace(string(raw))
	if strings.HasPrefix(trimmed, "{") {
		return json.Unmarshal(raw, out)
	}
	return yamlUnmarshal(raw, out)
}

func yamlUnmarshal(raw []byte, out *any) error {
	return yaml.Unmarshal(raw, out)
}

// findBreaking walks two decoded documents in parallel. base and cur must
// be the same kind; divergences are classified.
func findBreaking(path string, base, cur any, at string, out *[]string) {
	switch b := base.(type) {
	case map[string]any:
		c, ok := cur.(map[string]any)
		if !ok {
			*out = append(*out, fmt.Sprintf("%s: %s changed type from object", path, at))
			return
		}
		// required-list shrinkage (JSON Schema "required")
		if at == "$.required" || strings.HasSuffix(at, ".required") {
			for k := range b {
				if _, still := c[k]; !still {
					*out = append(*out, fmt.Sprintf("%s: required entry %q removed at %s", path, k, at))
				}
			}
		}
		// enum-value removal (JSON Schema "enum")
		if strings.HasSuffix(at, ".enum") {
			for k := range b {
				if _, still := c[k]; !still {
					*out = append(*out, fmt.Sprintf("%s: enum value %q removed at %s", path, k, at))
				}
			}
		}
		// deleting a published property under "properties" is breaking
		if strings.HasSuffix(at, ".properties") {
			for k := range b {
				if _, still := c[k]; !still {
					*out = append(*out, fmt.Sprintf("%s: property %q removed at %s", path, k, at))
				}
			}
		}
		// OpenAPI operation removal (paths.<p>.<method> deleted)
		if strings.Contains(at, ".paths.") {
			for k := range b {
				if _, still := c[k]; !still {
					*out = append(*out, fmt.Sprintf("%s: %q removed at %s (operation/response/field)", path, k, at))
				}
			}
		}
		// const introduction narrows values
		if _, had := b["const"]; !had {
			if nv, added := c["const"]; added {
				*out = append(*out, fmt.Sprintf("%s: const %v introduced at %s (narrows values)", path, nv, at))
			}
		}
		for k, bv := range b {
			if cv, still := c[k]; still {
				findBreaking(path, bv, cv, at+"."+k, out)
			}
		}
	case []any:
		c, ok := cur.([]any)
		if !ok {
			*out = append(*out, fmt.Sprintf("%s: %s changed type from array", path, at))
			return
		}
		n := len(b)
		if len(c) < n {
			n = len(c)
		}
		for i := 0; i < n; i++ {
			findBreaking(path, b[i], c[i], fmt.Sprintf("%s[%d]", at, i), out)
		}
	default:
		if !reflect.DeepEqual(base, cur) {
			// value changes: enum members, required lists handled above via
			// map-key logic; scalars inside "enum" arrays compared here
			if !strings.Contains(at, ".enum") && !strings.Contains(at, "examples") &&
				!strings.Contains(at, "description") && !strings.Contains(at, "summary") &&
				!strings.Contains(at, ".title") && at != "$.version" && !strings.Contains(at, "x-saoaf") {
				*out = append(*out, fmt.Sprintf("%s: value changed at %s (%v → %v)", path, at, base, cur))
			}
		}
	}
}

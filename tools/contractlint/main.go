// Command contractlint is the SAOAF contract gate (I03).
//
// Subcommands:
//
//	contractlint validate           — golden examples vs schemas (must pass),
//	                                  negative fixtures (must fail with the
//	                                  expected error code), forbidden-field
//	                                  scan, OpenAPI structural lint.
//	contractlint breaking [base]    — breaking-change detection between a
//	                                  base ref and the working tree (omit
//	                                  base to compare against HEAD).
//	contractlint deployable-scan    — no deployable units for contract-only
//	                                  modules: Dockerfiles / k8s manifests /
//	                                  deploy targets under contract-only
//	                                  module paths (03.5/03.6/03.7, I18+)
//	                                  fail the gate. Mocks are NOT deployable
//	                                  units (compose files under mocks/ are
//	                                  dev-time fixtures, not production
//	                                  deployments).
//
// Validation routing order (issue acceptance logic): payload size → payload
// depth → JSON Schema validation → semantic/forbidden scan → compatibility.
// Each negative input maps to exactly one error-envelope code.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"go.yaml.in/yaml/v3"
)

const (
	maxPayloadBytes = 1 << 20 // 1 MiB request envelope cap
	maxPayloadDepth = 64
	contractsDir    = "contracts"
)

// forbidden-field scan: keys/names that must never appear in contracts or
// examples (issue AC; common delivery constraints).
var forbiddenKeyPatterns = []string{
	"prompt", "credential", "secret", "api_key", "apikey", "password",
	"tool_parameter", "toolparam", "model_response", "agent_message",
	"routing_table", "cascade_path", "backend_health", "candidate_model",
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "validate":
		err = cmdValidate()
	case "deployable-scan":
		err = cmdDeployableScan()
	case "breaking":
		base := ""
		if len(os.Args) > 2 {
			base = os.Args[2]
		}
		err = cmdBreaking(base)
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "CONTRACT GATE: FAIL\n  - %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: contractlint validate | contractlint breaking [base-ref] | contractlint deployable-scan")
	os.Exit(2)
}

// cmdDeployableScan enforces the contract-only invariant (I18+): the repo
// must contain NO deployable production units for contract-only modules
// (03.5 MCP Fabric, 03.6 A2A Fabric, 03.7 Placement Fabric — until their
// runtime issues land). A "deployable unit" is any runtime artifact that
// could run the fabric as production code:
//
//   - a Go module package under internal/ for the fabric runtime
//   - a binary target under cmd/
//   - a container image build (build/*/Dockerfile) for the fabric
//   - any Dockerfile or kubernetes manifest anywhere that references a
//     fabric binary path
//
// Contracts (contracts/protocols/*) and dev mocks (mocks/*/compose.yaml)
// are NOT deployable units — mocks are dev-time fixtures with no image
// build of repo code.
func cmdDeployableScan() error {
	// runtime artifact name markers per contract-only module
	fabricMarkers := []string{"mcpfabric", "a2afabric", "placementfabric", "mcp-gateway", "a2a-gateway", "placement-fabric"}
	var hits []string

	err := filepath.Walk(".", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // unreadable entries are not deployable evidence
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "node_modules", "dist":
				return filepath.SkipDir
			}
			// fabric runtime packages/binaries/build targets are red
			if matchesFabricMarker(info.Name(), fabricMarkers) {
				rel, _ := filepath.Rel(".", path)
				hits = append(hits, rel+"/ (contract-only module runtime directory)")
			}
			return nil
		}
		rel, _ := filepath.Rel(".", path)
		base := strings.ToLower(info.Name())
		// any Dockerfile / k8s manifest referencing a fabric binary path
		if strings.Contains(base, "dockerfile") || strings.Contains(base, "containerfile") || strings.HasSuffix(base, ".yaml") || strings.HasSuffix(base, ".yml") {
			if referencesFabricBinary(path, fabricMarkers) {
				hits = append(hits, rel+" (deployable unit referencing a contract-only fabric)")
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(hits) > 0 {
		sort.Strings(hits)
		return fmt.Errorf("deployable units found for contract-only modules:\n    %s", strings.Join(hits, "\n    "))
	}
	fmt.Println("DEPLOYABLE SCAN: PASS (0 deployable units for contract-only modules)")
	return nil
}

// matchesFabricMarker reports whether a path segment (directory or binary
// name) identifies a contract-only fabric runtime artifact.
func matchesFabricMarker(name string, markers []string) bool {
	n := strings.ToLower(strings.TrimSuffix(name, filepath.Ext(name)))
	n = strings.NewReplacer("_", "-", ".", "-").Replace(n)
	for _, m := range markers {
		if strings.Contains(n, m) {
			return true
		}
	}
	return false
}

// referencesFabricBinary reports whether a build/deploy file mentions a
// fabric binary or package path (e.g. ./cmd/control-plane-mcp-gateway or
// internal/mcpfabric).
func referencesFabricBinary(path string, markers []string) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	s := strings.ToLower(string(raw))
	for _, m := range markers {
		if strings.Contains(s, m) {
			// k8s manifests must also look like resources to avoid matching
			// random YAML that merely mentions the word
			if strings.HasSuffix(strings.ToLower(path), ".yaml") || strings.HasSuffix(strings.ToLower(path), ".yml") {
				if !strings.Contains(s, "apiversion:") && !strings.Contains(s, "kind:") {
					continue
				}
			}
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// validate
// ---------------------------------------------------------------------------

type problem struct{ file, msg string }

func cmdValidate() error {
	var problems []problem
	add := func(f, m string) { problems = append(problems, problem{f, m}) }

	compiler := jsonschema.NewCompiler()
	compiler.AssertFormat() // format assertions (date-time etc.)

	// Load every v1 schema (recursive: top level + module subdirectories
	// like schemas/v1/mcp/ from I18).
	schemaFiles, err := schemaGlob()
	if err != nil {
		return err
	}
	for _, f := range schemaFiles {
		if err := loadJSONorYAML(compiler, f); err != nil {
			return fmt.Errorf("load schema %s: %w", f, err)
		}
	}

	// 1. Golden examples must validate against their schema (match by stem).
	// Recursive: module subdirectories like examples/valid/mcp/ (I18) are
	// first-class goldens (review R1 P1-1: a top-level-only glob left the
	// MCP goldens silently unvalidated).
	examples, _ := filepath.Glob(filepath.Join(contractsDir, "examples", "valid", "*.json"))
	subExamples, _ := filepath.Glob(filepath.Join(contractsDir, "examples", "valid", "*", "*.json"))
	examples = append(examples, subExamples...)
	validByStem := map[string]string{}
	for _, f := range schemaFiles {
		validByStem[filepath.Base(f)] = f
	}
	for _, ex := range examples {
		stem := exampleSchemaForPath(ex)
		if stem == "" {
			add(ex, "no schema mapping for example (name must match <schema-stem>[-variant].json)")
			continue
		}
		if err := validateExample(compiler, stem, ex); err != nil {
			add(ex, err.Error())
		}
	}

	// 1b. Coverage assertion (I18 R2 OBS-2): every .json under
	// examples/valid — at ANY depth — must be discovered by the globs.
	// A silent skip here is how the MCP goldens escaped the gate once.
	var discovered = map[string]bool{}
	for _, ex := range examples {
		discovered[ex] = true
	}
	_ = filepath.Walk(filepath.Join(contractsDir, "examples", "valid"), func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".json") {
			return nil
		}
		if !discovered[path] {
			add(path, "valid example not covered by the discovery globs (examples= counter lied)")
		}
		return nil
	})

	// 2. Negative fixtures must FAIL with the expected error code.
	negatives, _ := filepath.Glob(filepath.Join(contractsDir, "examples", "negative", "*.json"))
	for _, f := range negatives {
		if err := checkNegative(compiler, f); err != nil {
			add(f, err.Error())
		}
	}

	// 3. N/N-1 consumer fixtures: pinned-version consumer samples must
	// still validate against the CURRENT schemas (backward compatibility).
	consumers, _ := filepath.Glob(filepath.Join(contractsDir, "compatibility", "*", "consumer-fixtures", "*.json"))
	for _, cf := range consumers {
		stem := exampleSchemaForPath(cf)
		if stem == "" {
			add(cf, "no schema mapping for consumer fixture")
			continue
		}
		if err := validateExample(compiler, stem, cf); err != nil {
			add(cf, "N/N-1 backward-compat failure: "+err.Error())
		}
	}

	// 4. Forbidden-field scan over all contracts and examples.
	if err := walkForbidden(contractsDir, add); err != nil {
		return err
	}

	// 5. Migration destructive-DDL lint (I04 review): expand/migrate files
	// must not contain destructive operations; a contract-phase migration
	// must be explicitly marked with "-- +goose contract".
	migrations, _ := filepath.Glob("migrations/*.sql")
	for _, m := range migrations {
		rawM, rerr := os.ReadFile(m)
		if rerr != nil {
			return rerr
		}
		if isContractPhase(rawM) {
			continue
		}
		// Only the Up section is expand/migrate phase; the Down section is
		// the documented local-dev rollback and may contain DROPs.
		upSection := splitUpSection(rawM)
		for _, pat := range []string{"DROP TABLE", "DROP COLUMN", "ALTER TYPE", "RENAME TO"} {
			if matchesOutsideComments(upSection, pat) {
				add(m, "destructive DDL '"+pat+"' outside a contract-phase migration (mark with -- +goose contract)")
			}
		}
	}

	// 6. OpenAPI structural lint — control-plane API AND module protocol
	// mocks (I18 R1 P3-1: protocols/ joined the lint scope; every file
	// must expose the unified components.responses.Error envelope).
	apis, _ := filepath.Glob(filepath.Join(contractsDir, "openapi", "v*", "*.yaml"))
	protos, _ := filepath.Glob(filepath.Join(contractsDir, "protocols", "*", "*.yaml"))
	for _, f := range append(apis, protos...) {
		if err := lintOpenAPI(f, add); err != nil {
			return err
		}
	}

	if len(problems) > 0 {
		sort.Slice(problems, func(i, j int) bool { return problems[i].file < problems[j].file })
		var b strings.Builder
		for _, p := range problems {
			fmt.Fprintf(&b, "%s: %s; ", p.file, p.msg)
		}
		return fmt.Errorf("%d problem(s): %s", len(problems), b.String())
	}
	fmt.Printf("CONTRACT GATE: PASS (schemas=%d examples=%d negatives=%d consumers=%d forbidden-hits=0)\n",
		len(schemaFiles), len(examples), len(negatives), len(consumers))
	return nil
}

// exampleSchemaFor maps an example filename to its schema file, e.g.
// exampleSchemaForPath resolves the schema for an example/consumer-fixture
// PATH: an example nested in a module subdirectory (examples/valid/mcp/
// tool-snapshot.json) maps to schemas/v1/mcp/tool-snapshot.json directly;
// top-level files fall back to the legacy name heuristics.
func exampleSchemaForPath(path string) string {
	rel, err := filepath.Rel(filepath.Join(contractsDir, "examples", "valid"), path)
	if err == nil && rel != path && !strings.HasPrefix(rel, "..") {
		parts := strings.SplitN(rel, string(filepath.Separator), 2)
		if len(parts) == 2 {
			// module subdirectory: <module>/<name>.json must map to
			// schemas/v1/<module>/<name>.json or its stem-prefix variant
			module, name := parts[0], strings.TrimSuffix(parts[1], ".json")
			try := func(r string) bool {
				_, err := os.Stat(filepath.Join(contractsDir, "schemas", "v1", r))
				return err == nil
			}
			if try(filepath.Join(module, name+".json")) {
				return filepath.Join(module, name+".json")
			}
			if i := strings.LastIndex(name, "-"); i > 0 {
				if try(filepath.Join(module, name[:i]+".json")) {
					return filepath.Join(module, name[:i]+".json")
				}
			}
		}
	}
	return exampleSchemaFor(filepath.Base(path))
}

// "cloudevents-published.json" → "cloudevents-envelope.json".
var exampleSchemaOverrides = map[string]string{
	"cloudevents-published": "cloudevents-envelope.json",
}

func exampleSchemaFor(name string) string {
	base := strings.TrimSuffix(name, ".json")
	if m, ok := exampleSchemaOverrides[base]; ok {
		return m
	}
	try := func(rel string) bool {
		_, err := os.Stat(filepath.Join(contractsDir, "schemas", "v1", rel))
		return err == nil
	}
	for _, candidate := range []string{
		base + ".json",
		strings.SplitN(base, "-", 2)[0] + ".json", // error-envelope-variant → error-envelope.json
	} {
		if try(candidate) {
			return candidate
		}
	}
	// subdirectory schemas (I18 mcp, I19 a2a): "<module>-<name>.json" maps
	// to schemas/v1/<module>/<name>.json for ANY existing module directory
	if i := strings.Index(base, "-"); i > 0 {
		dir, rest := base[:i], base[i+1:]
		if try(filepath.Join(dir, rest+".json")) {
			return filepath.Join(dir, rest+".json")
		}
		// "<module>-<sub>-…-variant": mcp-tool-snapshot-variant → mcp/tool-snapshot.json
		if j := strings.Index(rest, "-"); j > 0 {
			if try(filepath.Join(dir, rest[:j]+".json")) {
				return filepath.Join(dir, rest[:j]+".json")
			}
		}
		// top-level stem prefix (e.g. cloudevents-published)
		stem := base[:i] + ".json"
		if try(stem) {
			return stem
		}
	}
	return ""
}

// schemaGlob returns every v1 schema file: the top level plus module
// subdirectories (e.g. schemas/v1/mcp/tool-snapshot.json).
func schemaGlob() ([]string, error) {
	top, err := filepath.Glob(filepath.Join(contractsDir, "schemas", "v1", "*.json"))
	if err != nil || len(top) == 0 {
		return nil, fmt.Errorf("no schemas found under %s/schemas/v1", contractsDir)
	}
	all := append([]string{}, top...)
	sub, _ := filepath.Glob(filepath.Join(contractsDir, "schemas", "v1", "*", "*.json"))
	return append(all, sub...), nil
}

// schemaRelPath resolves a schema file path relative to schemas/v1
// (e.g. mcp/tool-snapshot.json) for example-name matching.
func schemaRelPath(path string) string {
	rel, err := filepath.Rel(filepath.Join(contractsDir, "schemas", "v1"), path)
	if err != nil {
		return filepath.Base(path)
	}
	return rel
}

func loadJSONorYAML(c *jsonschema.Compiler, path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc any
	if strings.HasSuffix(path, ".yaml") || strings.HasSuffix(path, ".yml") {
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			return err
		}
	} else if err := json.Unmarshal(raw, &doc); err != nil {
		return err
	}
	return c.AddResource("https://contracts.saoaf.dev/schemas/v1/"+schemaRelPath(path), doc)
}

func schemaFor(c *jsonschema.Compiler, stem string) (*jsonschema.Schema, error) {
	return c.Compile("https://contracts.saoaf.dev/schemas/v1/" + stem)
}

func validateExample(c *jsonschema.Compiler, schemaStem, path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if len(raw) > maxPayloadBytes {
		return fmt.Errorf("payload exceeds %d bytes", maxPayloadBytes)
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return err
	}
	if d := jsonDepth(doc); d > maxPayloadDepth {
		return fmt.Errorf("payload depth %d exceeds %d", d, maxPayloadDepth)
	}
	s, err := schemaFor(c, schemaStem)
	if err != nil {
		return err
	}
	if err := s.Validate(doc); err != nil {
		return fmt.Errorf("schema violation: %v", err)
	}
	return nil
}

// checkNegative runs a negative fixture and asserts it fails with the
// fixture's expected_error_code. The routing order (size → depth → schema)
// is applied to the reconstructed input.
type negativeFixture struct {
	Case        string          `json:"case"`
	Schema      string          `json:"schema"`
	Input       json.RawMessage `json:"input"`
	PadToBytes  int             `json:"pad_to_bytes"`
	ExpectedErr string          `json:"expected_error_code"`
}

func checkNegative(c *jsonschema.Compiler, path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var fx negativeFixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		return err
	}
	if fx.ExpectedErr == "" {
		return fmt.Errorf("negative fixture missing expected_error_code")
	}

	// size injection: expand @pad@ to reach the byte budget
	input := fx.Input
	if fx.PadToBytes > 0 {
		padded := bytes.Replace(input, []byte("@pad@"), bytes.Repeat([]byte("p"), fx.PadToBytes), 1)
		input = padded
	}
	var doc any
	if err := json.Unmarshal(input, &doc); err != nil {
		// malformed JSON is not this fixture's target; surface as problem
		return fmt.Errorf("fixture input not valid JSON after padding: %v", err)
	}

	code := ""
	switch {
	case len(input) > maxPayloadBytes:
		code = "VALIDATION_PAYLOAD_TOO_LARGE"
	case jsonDepth(doc) > maxPayloadDepth:
		code = "VALIDATION_PAYLOAD_TOO_DEEP"
	default:
		s, err := schemaFor(c, fx.Schema)
		if err != nil {
			return err
		}
		if err := s.Validate(doc); err == nil {
			return fmt.Errorf("fixture %s: input unexpectedly PASSED schema %s", fx.Case, fx.Schema)
		} else {
			code = classifySchemaError(err.Error())
		}
	}
	if code != fx.ExpectedErr {
		return fmt.Errorf("fixture %s: expected error code %s, got %s", fx.Case, fx.ExpectedErr, code)
	}
	return nil
}

// classifySchemaError maps a jsonschema violation to the unified code by
// keyword (the library's error strings are stable enough for the gate's
// closed fixture set; unexpected shapes fall through to INVALID_ENUM only
// when matched, else SEMANTIC_INVALID which fails the fixture mapping).
func classifySchemaError(msg string) string {
	switch {
	case strings.Contains(msg, "additional properties") && strings.Contains(msg, "not allowed"):
		return "VALIDATION_UNKNOWN_FIELD"
	case strings.Contains(msg, "value must be"):
		return "VALIDATION_INVALID_ENUM"
	case strings.Contains(msg, "missing property"):
		return "VALIDATION_MISSING_REQUIRED"
	default:
		// anything unmapped by the schema classifier is a semantic failure;
		// depth violations are caught before schema validation via jsonDepth
		return "SEMANTIC_INVALID"
	}
}

// jsonDepth computes the maximum nesting depth of a decoded JSON document.
func jsonDepth(v any) int {
	switch t := v.(type) {
	case map[string]any:
		max := 0
		for _, c := range t {
			if d := jsonDepth(c); d > max {
				max = d
			}
		}
		return 1 + max
	case []any:
		max := 0
		for _, c := range t {
			if d := jsonDepth(c); d > max {
				max = d
			}
		}
		return 1 + max
	default:
		return 1
	}
}

// ---------------------------------------------------------------------------
// forbidden-field scan
// ---------------------------------------------------------------------------

func walkForbidden(root string, add func(f, m string)) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		lower := strings.ToLower(string(raw))
		for _, pat := range forbiddenKeyPatterns {
			// word-boundary-ish match inside keys/strings
			for _, needle := range []string{
				"\"" + pat + "\"", "\"" + pat + "_", "_" + pat + "\"", "\"" + pat + "-",
			} {
				if strings.Contains(lower, needle) {
					add(path, fmt.Sprintf("forbidden field/payload fragment '%s' present", pat))
					break
				}
			}
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// OpenAPI structural lint
// ---------------------------------------------------------------------------

type openapiDoc struct {
	OpenAPI    string                    `yaml:"openapi"`
	Info       map[string]any            `yaml:"info"`
	Paths      map[string]map[string]any `yaml:"paths"`
	Components struct {
		Responses map[string]any `yaml:"responses"`
	} `yaml:"components"`
}

func lintOpenAPI(path string, add func(f, m string)) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc openapiDoc
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		add(path, "not parseable YAML: "+err.Error())
		return nil
	}
	if !strings.HasPrefix(doc.OpenAPI, "3.1") {
		add(path, "openapi version must be 3.1.x, got "+doc.OpenAPI)
	}
	if len(doc.Paths) == 0 {
		add(path, "paths must not be empty")
	}
	if _, ok := doc.Components.Responses["Error"]; !ok {
		add(path, "components.responses.Error (unified error envelope) is required")
	}
	// every 4xx/5xx response should reference the Error envelope (directly
	// or via $ref) — walk paths
	for p, ops := range doc.Paths {
		for method, op := range ops {
			m, ok := op.(map[string]any)
			if !ok || method == "parameters" {
				continue
			}
			resps, ok := m["responses"].(map[string]any)
			if !ok {
				continue
			}
			for code, r := range resps {
				cs := fmt.Sprintf("%v", code)
				if !strings.HasPrefix(cs, "4") && !strings.HasPrefix(cs, "5") {
					continue
				}
				blob, _ := json.Marshal(r)
				// a bare $ref to a named response dereferences ONE level —
				// the named components.responses.* entry carries the envelope
				if m, ok := r.(map[string]any); ok {
					if ref, ok := m["$ref"].(string); ok {
						name := strings.TrimPrefix(ref, "#/components/responses/")
						if named, ok := doc.Components.Responses[name]; ok {
							nb, _ := json.Marshal(named)
							blob = append(blob, nb...)
						}
					}
				}
				if !bytes.Contains(blob, []byte("Error")) &&
					!bytes.Contains(blob, []byte("error-envelope")) {
					add(path, fmt.Sprintf("%s %s: %s response does not reference the unified error envelope", strings.ToUpper(method), p, cs))
				}
			}
		}
	}
	return nil
}

// isContractPhase reports whether a migration file is explicitly marked as
// a contract-phase migration (the only place destructive DDL is allowed).
func isContractPhase(raw []byte) bool {
	return bytes.Contains(raw, []byte("-- +goose contract"))
}

// splitUpSection returns the bytes from the Up marker up to (excluding)
// the Down marker; files without markers are treated as fully Up.
func splitUpSection(raw []byte) []byte {
	idx := bytes.Index(raw, []byte("-- +goose Down"))
	if idx < 0 {
		return raw
	}
	return raw[:idx]
}

// matchesOutsideComments does a naive case-insensitive scan, skipping lines
// starting with "--" (SQL comments).
func matchesOutsideComments(raw []byte, needle string) bool {
	upper := strings.ToUpper(string(raw))
	n := strings.ToUpper(needle)
	for _, line := range strings.Split(upper, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "--") {
			continue
		}
		if strings.Contains(trimmed, n) {
			return true
		}
	}
	return false
}

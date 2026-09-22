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
	fmt.Fprintln(os.Stderr, "usage: contractlint validate | contractlint breaking [base-ref]")
	os.Exit(2)
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

	// Load every v1 schema.
	schemaFiles, err := filepath.Glob(filepath.Join(contractsDir, "schemas", "v1", "*.json"))
	if err != nil || len(schemaFiles) == 0 {
		return fmt.Errorf("no schemas found under %s/schemas/v1", contractsDir)
	}
	for _, f := range schemaFiles {
		if err := loadJSONorYAML(compiler, f); err != nil {
			return fmt.Errorf("load schema %s: %w", f, err)
		}
	}

	// 1. Golden examples must validate against their schema (match by stem).
	examples, _ := filepath.Glob(filepath.Join(contractsDir, "examples", "valid", "*.json"))
	validByStem := map[string]string{}
	for _, f := range schemaFiles {
		validByStem[filepath.Base(f)] = f
	}
	for _, ex := range examples {
		stem := exampleSchemaFor(filepath.Base(ex))
		if stem == "" {
			add(ex, "no schema mapping for example (name must match <schema-stem>[-variant].json)")
			continue
		}
		if err := validateExample(compiler, stem, ex); err != nil {
			add(ex, err.Error())
		}
	}

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
		stem := exampleSchemaFor(filepath.Base(cf))
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

	// 6. OpenAPI structural lint.
	apis, _ := filepath.Glob(filepath.Join(contractsDir, "openapi", "v*", "*.yaml"))
	for _, f := range apis {
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
// "cloudevents-published.json" → "cloudevents-envelope.json".
var exampleSchemaOverrides = map[string]string{
	"cloudevents-published": "cloudevents-envelope.json",
}

func exampleSchemaFor(name string) string {
	base := strings.TrimSuffix(name, ".json")
	if m, ok := exampleSchemaOverrides[base]; ok {
		return m
	}
	for _, candidate := range []string{
		base + ".json",
		strings.SplitN(base, "-", 2)[0] + ".json", // error-envelope-variant → error-envelope.json
	} {
		if _, err := os.Stat(filepath.Join(contractsDir, "schemas", "v1", candidate)); err == nil {
			return candidate
		}
	}
	// explicit stem prefix before first "-" (e.g. cloudevents-published)
	if i := strings.Index(base, "-"); i > 0 {
		stem := base[:i] + ".json"
		if _, err := os.Stat(filepath.Join(contractsDir, "schemas", "v1", stem)); err == nil {
			return stem
		}
	}
	return ""
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
	return c.AddResource("https://contracts.saoaf.dev/schemas/v1/"+filepath.Base(path), doc)
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

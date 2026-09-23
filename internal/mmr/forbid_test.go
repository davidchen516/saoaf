package mmr

// 禁止字段回归（specs §3.1 禁止字段 + vLLM Semantic Router integration
// boundary）：ARR 代码不得读取/持久化 canonical YAML、Signals、Decisions、
// 候选模型、provider endpoints、权重、回退链、backend health。扫描 internal/
// + cmd/ 的非测试源码，断言禁止概念零命中（数据面隔离的代码证据）。

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var quotedLiteral = regexp.MustCompile(`"[^"]*"`)

var forbiddenConcepts = regexp.MustCompile(
	`canonical[_-]?yaml|semantic[_-]?router|candidate[_-]?model|backend[_-]?health|cascade[_-]?path|` +
		`signal[_-]?definition|decision[_-]?graph|model[_-]?weight`)

// ForbidFieldScan walks the control-plane sources and fails on any
// forbidden routing concept leaking into ARR code (docs/mocks/excluded).
func TestForbidFieldScan(t *testing.T) {
	roots := []string{"../../internal", "../../cmd"}
	violations := 0
	for _, root := range roots {
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for i, line := range strings.Split(string(data), "\n") {
				trimmed := strings.TrimSpace(line)
				// comments that document the forbidden boundary are the
				// spec itself — only executable code is scanned
				if strings.HasPrefix(trimmed, "//") {
					continue
				}
				// quoted string literals are REJECTION PATTERNS (the
				// forbid-field checkers carry the names of what they
				// refuse — registry/domain.go's forbidden-field table).
				// Real leaks would reference the concepts as identifiers
				// or imports, which are unquoted.
				codeOnly := quotedLiteral.ReplaceAllString(line, "")
				if forbiddenConcepts.MatchString(codeOnly) {
					t.Errorf("forbidden routing concept in %s:%d: %s", path, i+1, trimmed)
					violations++
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if violations > 0 {
		t.Fatalf("%d forbidden-concept violations — MMR internal routing must never leak into ARR", violations)
	}
}

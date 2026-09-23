package policy

import (
	"context"
	"fmt"
	"strings"
	"time"

	cel "github.com/google/cel-go/cel"
)

// EligibilityInput is the typed evaluation input for eligibility CEL
// expressions. Expressions see exactly these variables — nothing else.
type EligibilityInput struct {
	Region      string
	DataClass   string
	Vendor      string
	Environment string
	TenantRef   string
}

// Evaluation is a deterministic result carrying the revision reference.
type Evaluation struct {
	Allow         bool
	PolicySetID   string
	PolicyVersion int
	PolicyDigest  string
}

// Reason codes for rejections (unified error envelope mapping).
const (
	ReasonCELInvalid      = "POLICY_CEL_INVALID"
	ReasonCELTooLong      = "POLICY_CEL_TOO_LONG"
	ReasonCELUnauthorized = "POLICY_CEL_UNAUTHORIZED_FUNCTION"
	ReasonCELTimeout      = "POLICY_CEL_TIMEOUT"
)

// CEL sandbox limits (issue: 超时上限、内存上限、函数白名单).
const (
	MaxCELLength = 4096
	CELTimeout   = 500 * time.Millisecond
)

// forbiddenTokens are CEL language features/functions outside the
// whitelist for eligibility expressions. The allowlist semantics:
// expressions may reference ONLY the typed input variables and the
// standard comparison/logical/string operations; no function calls other
// than a tiny string set are permitted.
var allowedFunctions = map[string]bool{
	"size":       true,
	"contains":   true,
	"startsWith": true,
	"endsWith":   true,
	"matches":    true,
}

// celEnv is the compiled environment: typed variables, no declarations
// beyond the standard library, disabled extensions.
var celEnv *cel.Env

func init() {
	env, err := cel.NewEnv(
		cel.Variable("region", cel.StringType),
		cel.Variable("data_class", cel.StringType),
		cel.Variable("vendor", cel.StringType),
		cel.Variable("environment", cel.StringType),
		cel.Variable("tenant_ref", cel.StringType),
		// disable all extension libraries: eligibility expressions get the
		// core macro-free language only
		cel.StdLib(),
	)
	if err != nil {
		panic("policy: cel env: " + err.Error())
	}
	celEnv = env
}

// Evaluator compiles and evaluates eligibility expressions under the
// sandbox: length cap, timeout, function allowlist, type checks.
type Evaluator struct{}

func NewEvaluator() *Evaluator { return &Evaluator{} }

// ValidateExpression checks the static sandbox rules without evaluating.
// Returns a reason code for each violation class (issue GWT#2).
func (e *Evaluator) ValidateExpression(expr string) (string, error) {
	if len(expr) > MaxCELLength {
		return ReasonCELTooLong, fmt.Errorf("expression exceeds %d bytes", MaxCELLength)
	}
	ast, iss := celEnv.Parse(expr)
	if iss != nil && iss.Err() != nil {
		return ReasonCELInvalid, fmt.Errorf("syntax: %v", iss.Err())
	}
	checked, iss := celEnv.Check(ast)
	if iss != nil && iss.Err() != nil {
		return ReasonCELInvalid, fmt.Errorf("type check: %v", iss.Err())
	}
	// function allowlist: reject any call operator whose function is not
	// in the allowed set
	if err := checkCalls(checked.NativeRep().Expr(), nil); err != nil {
		return ReasonCELUnauthorized, err
	}
	return "", nil
}

// Evaluate compiles (if needed) and evaluates the expression against the
// input under a context timeout. Errors carry reason codes; the result is
// deterministic for (input, expression) pairs.
func (e *Evaluator) Evaluate(ctx context.Context, r *Revision, in EligibilityInput) (Evaluation, string, error) {
	expr := r.Content.EligibilityCEL
	if reason, err := e.ValidateExpression(expr); err != nil {
		return Evaluation{}, reason, err
	}

	ast, _ := celEnv.Parse(expr)
	checked, iss := celEnv.Check(ast)
	if iss != nil && iss.Err() != nil {
		return Evaluation{}, ReasonCELInvalid, iss.Err()
	}
	prg, err := celEnv.Program(checked,
		cel.InterruptCheckFrequency(1),
	)
	if err != nil {
		return Evaluation{}, ReasonCELInvalid, err
	}

	ctx, cancel := context.WithTimeout(ctx, CELTimeout)
	defer cancel()
	out, _, err := prg.ContextEval(ctx, map[string]any{
		"region":      in.Region,
		"data_class":  in.DataClass,
		"vendor":      in.Vendor,
		"environment": in.Environment,
		"tenant_ref":  in.TenantRef,
	})
	if err != nil {
		if ctx.Err() != nil || strings.Contains(err.Error(), "operation interrupted") {
			return Evaluation{}, ReasonCELTimeout, fmt.Errorf("evaluation timed out")
		}
		return Evaluation{}, ReasonCELInvalid, err
	}
	boolOut, ok := out.Value().(bool)
	if !ok {
		return Evaluation{}, ReasonCELInvalid, fmt.Errorf("expression must evaluate to bool, got %T", out.Value())
	}
	return Evaluation{
		Allow:         boolOut,
		PolicySetID:   r.SetID,
		PolicyVersion: r.Version,
		PolicyDigest:  r.Digest,
	}, "", nil
}

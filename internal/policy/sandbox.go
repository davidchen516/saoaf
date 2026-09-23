package policy

import (
	"strings"

	"github.com/google/cel-go/common/ast"
)

// checkCalls walks the checked AST and rejects any call to a function not
// on the allowlist (the sandbox's core guarantee: no arbitrary effects).
func checkCalls(e ast.Expr, src *ast.SourceInfo) error {
	if e == nil {
		return nil
	}
	switch e.Kind() {
	case ast.CallKind:
		fn := e.AsCall().FunctionName()
		// CEL represents operators (&&, +, ==, in …) as internal calls
		// with names like "_&&_" or "@in"; those are core-language,
		// always allowed. Only user-callable named functions (identifier
		// style) go through the allowlist.
		if isInternalName(fn) {
			// core operator
		} else if !allowedFunctions[fn] && !isMacro(fn) {
			return errUnauthorized(fn)
		}
		for _, arg := range e.AsCall().Args() {
			if err := checkCalls(arg, src); err != nil {
				return err
			}
		}
		// target-based calls (x.contains(...)) arrive as target + args
		if tgt := e.AsCall().Target(); tgt != nil {
			if err := checkCalls(tgt, src); err != nil {
				return err
			}
		}
	case ast.ListKind:
		for _, el := range e.AsList().Elements() {
			if err := checkCalls(el, src); err != nil {
				return err
			}
		}
	case ast.MapKind:
		for _, entry := range e.AsMap().Entries() {
			if err := checkCalls(entry.AsMapEntry().Key(), src); err != nil {
				return err
			}
			if err := checkCalls(entry.AsMapEntry().Value(), src); err != nil {
				return err
			}
		}
	case ast.StructKind:
		return errUnauthorized(e.AsStruct().TypeName())
	case ast.LiteralKind, ast.IdentKind:
		return nil
	case ast.SelectKind:
		return checkCalls(e.AsSelect().Operand(), src)
	case ast.ComprehensionKind:
		c := e.AsComprehension()
		for _, sub := range []ast.Expr{c.IterRange(), c.AccuInit(), c.LoopCondition(), c.LoopStep(), c.Result()} {
			if err := checkCalls(sub, src); err != nil {
				return err
			}
		}
	}
	return nil
}

// isInternalName reports whether fn is a CEL internal operator encoding:
// "_&&_"-style operator wrappers or "@"-prefixed fused/inline operators.
func isInternalName(fn string) bool {
	if strings.HasPrefix(fn, "@") {
		return true
	}
	return strings.HasPrefix(fn, "_") && strings.HasSuffix(fn, "_") && len(fn) > 2
}

func isMacro(fn string) bool {
	// macros are compile-time sugar over allowed builtins
	switch fn {
	case "all", "exists", "exists_one", "filter", "has", "map":
		return true
	}
	return false
}

func errUnauthorized(fn string) error {
	return &UnauthorizedFunctionError{Function: fn}
}

// UnauthorizedFunctionError names the rejected function.
type UnauthorizedFunctionError struct{ Function string }

func (u *UnauthorizedFunctionError) Error() string {
	return "unauthorized function: " + u.Function
}

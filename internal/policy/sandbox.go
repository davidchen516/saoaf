package policy

import (
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

// celOperators is the EXACT set of cel-go v0.26 internal operator names
// (see cel/common/operators/operators.go). Anything not here and not on
// the function allowlist is rejected — no shape heuristics.
var celOperators = map[string]bool{
	"_||_": true, "_&&_": true, "!_": true, "_-_-": true, "_==_": true,
	"_!=_": true, "_<_": true, "_<=_": true, "_>_": true, "_>=_": true,
	"_+_": true, "_-_": true, "_*_": true, "_/_": true, "_%_": true,
	"_in_": true, "_[_": true, "_?_:_": true, "@not_strictly_false": true,
	"@not_strictly_true": true, "@type": true, "@range": true,
	"@list": true, "@map": true, "@struct": true, "@in": true,
	// macro internal plumbing
	"@filter": true, "@map_macro": true, "@exists": true, "@all": true,
	"@exists_one": true, "has": true,
}

// isInternalName reports whether fn is a cel-go internal operator/macro
// name (exact table, not pattern matching — prevents user-crafted
// lookalike identifiers from slipping through).
func isInternalName(fn string) bool {
	return celOperators[fn]
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

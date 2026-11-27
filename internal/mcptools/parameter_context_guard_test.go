package mcptools

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// requestCallees are helpers that can send a backend request.
var requestCallees = map[string]bool{
	"resolveSearchCalendars": true, "resolveCalendarPaths": true, "selectCalendarReadPaths": true,
	"parallelCalendarReads": true, "parallelCalendarReadsWaves": true, "collectAvailabilityIntervals": true,
}

// TestParameterContextsPrecedeRequests guards the errResult suffix rule: a
// "<field> parameter" context gives any error the validation code. Each such
// context must occur before the first backend request in its function, so a
// request failure can never get the validation code.
func TestParameterContextsPrecedeRequests(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	sites := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		source, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, name, source, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			firstRequest := token.NoPos
			var contexts []*ast.BasicLit
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				if isRequestCall(call) && (firstRequest == token.NoPos || call.Pos() < firstRequest) {
					firstRequest = call.Pos()
				}
				if isErrResultCall(call) && !isContextArgument(call.Args[1]) {
					t.Errorf("%s: errResult context must be a string literal or the deny parameter", fset.Position(call.Pos()))
				}
				for _, arg := range call.Args {
					if lit, ok := arg.(*ast.BasicLit); ok && lit.Kind == token.STRING {
						if value, err := strconv.Unquote(lit.Value); err == nil && strings.HasSuffix(value, " parameter") {
							contexts = append(contexts, lit)
						}
					}
				}
				return true
			})
			for _, lit := range contexts {
				sites++
				if firstRequest != token.NoPos && lit.Pos() > firstRequest {
					t.Errorf("%s: %s follows a backend request at %s", fset.Position(lit.Pos()), lit.Value, fset.Position(firstRequest))
				}
			}
		}
	}
	if sites == 0 {
		t.Fatal("found no parameter contexts; the guard no longer sees the handlers")
	}
}

func isErrResultCall(call *ast.CallExpr) bool {
	ident, ok := call.Fun.(*ast.Ident)
	return ok && ident.Name == "errResult" && len(call.Args) == 3
}

// isContextArgument accepts a literal or the context parameter of a deny
// closure, whose call sites the guard checks as literals.
func isContextArgument(arg ast.Expr) bool {
	switch value := arg.(type) {
	case *ast.BasicLit:
		return value.Kind == token.STRING
	case *ast.Ident:
		return value.Name == "context"
	}
	return false
}

// isRequestCall reports a call to a request helper or to a method of a
// Calendar, Contacts, or Mail service.
func isRequestCall(call *ast.CallExpr) bool {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return requestCallees[fun.Name]
	case *ast.SelectorExpr:
		switch receiver := fun.X.(type) {
		case *ast.Ident:
			return receiver.Name == "service"
		case *ast.SelectorExpr:
			return strings.HasSuffix(receiver.Sel.Name, "Service")
		}
	}
	return false
}

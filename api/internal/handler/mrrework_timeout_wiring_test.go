package handler

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// This checks source selection at the actual call, not HTTP response timing.
func TestMRReworkAssessmentTimeoutWiring(t *testing.T) {
	for _, tc := range []struct {
		path, function string
		interactive    bool
	}{
		{"mrrework_run.go", "StartRunRework", true},
		{"../poller/mr_review_watch.go", "", false},
	} {
		t.Run(tc.path, func(t *testing.T) {
			file, err := parser.ParseFile(token.NewFileSet(), tc.path, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			var root ast.Node = file
			if tc.function != "" {
				var funcs []*ast.FuncDecl
				for _, decl := range file.Decls {
					if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == tc.function {
						funcs = append(funcs, fn)
					}
				}
				if len(funcs) != 1 || funcs[0].Body == nil {
					t.Fatalf("expected unique implemented %s, got %d", tc.function, len(funcs))
				}
				root = funcs[0].Body
			}
			var calls []*ast.CallExpr
			ast.Inspect(root, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if ok && sel.Sel.Name == "Begin" {
					id, ok := sel.X.(*ast.Ident)
					if !ok {
						t.Fatal("unsupported Begin receiver")
					}
					if id.Name == "assessor" {
						calls = append(calls, call)
					}
				}
				return true
			})
			if len(calls) != 1 || len(calls[0].Args) != 2 {
				t.Fatalf("expected unique assessor.Begin(ctx, params), got %d", len(calls))
			}
			literal, ok := calls[0].Args[1].(*ast.CompositeLit)
			if !ok {
				t.Fatal("unsupported Begin params")
			}
			typ, ok := literal.Type.(*ast.SelectorExpr)
			if !ok || typ.Sel.Name != "ReviewAssessParams" {
				t.Fatal("unsupported params type")
			}
			pkg, ok := typ.X.(*ast.Ident)
			if !ok || pkg.Name != "workersvc" {
				t.Fatal("unsupported params package")
			}
			fields := 0
			for _, elt := range literal.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					t.Fatal("unsupported unkeyed params")
				}
				key, ok := kv.Key.(*ast.Ident)
				if !ok {
					t.Fatal("unsupported params key")
				}
				if key.Name != "AssessmentTimeout" {
					continue
				}
				fields++
				if tc.interactive {
					sel, ok := kv.Value.(*ast.SelectorExpr)
					if !ok || sel.Sel.Name != "InteractiveAssessmentTimeout" {
						t.Fatal("interactive total timeout missing")
					}
					pkg, ok := sel.X.(*ast.Ident)
					if !ok || pkg.Name != "issueinput" {
						t.Fatal("interactive timeout must use issueinput constant")
					}
				} else {
					zero, ok := kv.Value.(*ast.BasicLit)
					if !ok || zero.Kind != token.INT || zero.Value != "0" {
						t.Fatal("automatic watcher must use default total deadline")
					}
				}
			}
			if fields > 1 || (tc.interactive && fields != 1) {
				t.Fatalf("AssessmentTimeout fields=%d", fields)
			}
		})
	}
}

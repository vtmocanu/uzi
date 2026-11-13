package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// Inspect the production builder: the render-side field is the NEGATIVE of the
// config's positive bool, so a dropped `!` inverts the knob and no config-to-pod
// test that builds RenderConfig directly can see it.
func TestRetireScratchOutcomesDisabledWiring(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	builders := 0
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		typ, ok := lit.Type.(*ast.SelectorExpr)
		if !ok || typ.Sel.Name != "RenderConfig" {
			return true
		}
		builders++
		found := false
		for _, elt := range lit.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok || key.Name != "RetireScratchOutcomesDisabled" {
				continue
			}
			not, ok := kv.Value.(*ast.UnaryExpr)
			if !ok || not.Op != token.NOT {
				continue
			}
			value, ok := not.X.(*ast.SelectorExpr)
			if ok && value.Sel.Name == "WorkerRetireScratchOutcomes" {
				receiver, ok := value.X.(*ast.Ident)
				found = ok && receiver.Name == "cfg"
			}
		}
		if !found {
			t.Error("RenderConfig builder must set RetireScratchOutcomesDisabled to !cfg.WorkerRetireScratchOutcomes")
		}
		return true
	})
	if builders != 1 {
		t.Fatalf("found %d RenderConfig builders; audit all command wiring points", builders)
	}
}

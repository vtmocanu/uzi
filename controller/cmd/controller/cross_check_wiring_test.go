package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// Inspect the production builder: config-to-pod tests cannot detect a field
// accidentally omitted at the command's materializer wiring point.
func TestCrossCheckSlotsMaterializerWiring(t *testing.T) {
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
			if !ok || key.Name != "CrossCheckSlots" {
				continue
			}
			value, ok := kv.Value.(*ast.SelectorExpr)
			if ok && value.Sel.Name == "WorkerCrossCheckSlots" {
				receiver, ok := value.X.(*ast.Ident)
				found = ok && receiver.Name == "cfg"
			}
		}
		if !found {
			t.Error("RenderConfig builder must relay cfg.WorkerCrossCheckSlots directly")
		}
		return true
	})
	if builders != 1 {
		t.Fatalf("found %d RenderConfig builders; audit all command wiring points", builders)
	}
}

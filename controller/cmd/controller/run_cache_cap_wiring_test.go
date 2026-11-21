package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// Inspect the production builder: config-to-pod tests cannot detect a field
// accidentally omitted at the command's materializer wiring point.
func TestRunCacheCapMaterializerWiring(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	wanted := []struct{ field, cfgField string }{
		{"RunCacheCapMaxBytes", "WorkerRunCacheCapMaxBytes"},
		{"RunCacheCapFraction", "WorkerRunCacheCapFraction"},
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
		for _, w := range wanted {
			found := false
			for _, elt := range lit.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, ok := kv.Key.(*ast.Ident)
				if !ok || key.Name != w.field {
					continue
				}
				value, ok := kv.Value.(*ast.SelectorExpr)
				if ok && value.Sel.Name == w.cfgField {
					receiver, ok := value.X.(*ast.Ident)
					found = ok && receiver.Name == "cfg"
				}
			}
			if !found {
				t.Errorf("RenderConfig builder must relay cfg.%s directly as %s", w.cfgField, w.field)
			}
		}
		return true
	})
	if builders != 1 {
		t.Fatalf("found %d RenderConfig builders; audit all command wiring points", builders)
	}
}

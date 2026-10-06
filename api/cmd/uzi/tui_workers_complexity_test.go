package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// These hot paths must consume a frame snapshot and precomputed sort keys.
// Rendering one row or comparing two rows must not rescan or rerank the fleet.
func TestWorkersRenderAvoidsRepeatedFleetWork(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "tui_workers.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	checkedRow, checkedComparator := false, false
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if fn.Name.Name == "workerRowLine" {
			checkedRow = true
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				switch sel.Sel.Name {
				case "visible", "workerMemoryWidth", "workerVersionWidth", "workerTableWidths":
					t.Errorf("row renderer repeats fleet work through %s", sel.Sel.Name)
				}
				return true
			})
		}
		if fn.Name.Name == "visible" {
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "SortStableFunc" {
					return true
				}
				checkedComparator = true
				ast.Inspect(call.Args[len(call.Args)-1], func(inner ast.Node) bool {
					nested, ok := inner.(*ast.CallExpr)
					if !ok {
						return true
					}
					name, ok := nested.Fun.(*ast.Ident)
					if ok && name.Name == "workerSeverity" {
						t.Error("sort comparator rebuilds worker attention")
					}
					return true
				})
				return true
			})
		}
	}
	if !checkedRow || !checkedComparator {
		t.Fatal("fleet complexity guard did not inspect both hot paths")
	}
}

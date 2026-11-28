package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

type recordingSetter struct{ got workersvc.RunLifecycle }

func (r *recordingSetter) SetLifecycle(l workersvc.RunLifecycle) { r.got = l }

type recordingLifecycle struct{ ids []uuid.UUID }

func (r *recordingLifecycle) Notify(id uuid.UUID, _ string) { r.ids = append(r.ids, id) }

type fakeFold struct {
	recordingLifecycle
	passCalls int
	passN     int64
}

func (f *fakeFold) Pass(context.Context) (int64, error) { f.passCalls++; return f.passN, nil }

func TestWireUsageFolderInstallsTeeAndReturnsThePass(t *testing.T) {
	svc := &recordingSetter{}
	board := &recordingLifecycle{}
	folder := &fakeFold{passN: 7}

	pass := wireUsageFolder(svc, board, folder)

	if svc.got == nil {
		t.Fatal("SetLifecycle was never called")
	}
	id := uuid.New()
	svc.got.Notify(id, "completed")
	if len(board.ids) != 1 || board.ids[0] != id {
		t.Fatalf("existing lifecycle not reached: %v", board.ids)
	}
	if len(folder.ids) != 1 || folder.ids[0] != id {
		t.Fatalf("usage folder not reached: %v", folder.ids)
	}

	if pass.Name != "usage_fold_materialize" {
		t.Fatalf("pass name = %q", pass.Name)
	}
	if pass.Run == nil {
		t.Fatal("returned pass has no Run: the safety net would never run")
	}
	n, err := pass.Run(context.Background())
	if err != nil || n != 7 || folder.passCalls != 1 {
		t.Fatalf("pass.Run = (%d, %v) with %d folder Pass calls, want the folder's Pass (7, nil, 1)", n, err, folder.passCalls)
	}
}

func TestWireUsageFolderToleratesNoExistingLifecycle(t *testing.T) {
	svc := &recordingSetter{}
	folder := &fakeFold{}
	wireUsageFolder(svc, nil, folder)
	svc.got.Notify(uuid.New(), "failed")
	if len(folder.ids) != 1 {
		t.Fatalf("usage folder not reached without an existing lifecycle: %v", folder.ids)
	}
}

// main.go is not unit-testable end to end, so this pins its usage-fold wiring from source:
// wireUsageFolder is called, the pass it returns is the last element of the pass slice given to
// sweeper.New (dropping it silently disables the safety net), and the same folder's Wait is called at shutdown.
func TestMainWiresUsageFolderPassAndWait(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var passVar, folderVar string
	// main.go builds the passes as the `sweepPasses` slice literal, lets wireAnalyticsPrecompute
	// insert before its last element, and spreads it into sweeper.New.
	sweepPassesLast, sweeperSpread := "", ""
	waited := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.AssignStmt:
			if len(x.Lhs) == 1 && len(x.Rhs) == 1 {
				if l, ok := x.Lhs[0].(*ast.Ident); ok && l.Name == "sweepPasses" {
					if lit, ok := x.Rhs[0].(*ast.CompositeLit); ok && len(lit.Elts) > 0 {
						if id, ok := lit.Elts[len(lit.Elts)-1].(*ast.Ident); ok {
							sweepPassesLast = id.Name
						}
					}
				}
				if call, ok := x.Rhs[0].(*ast.CallExpr); ok {
					if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "wireUsageFolder" && len(call.Args) == 3 {
						if l, ok := x.Lhs[0].(*ast.Ident); ok {
							passVar = l.Name
						}
						if a, ok := call.Args[2].(*ast.Ident); ok {
							folderVar = a.Name
						}
					}
				}
			}
		case *ast.CallExpr:
			sel, ok := x.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			recv, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			if recv.Name == "sweeper" && sel.Sel.Name == "New" && x.Ellipsis.IsValid() {
				if id, ok := x.Args[len(x.Args)-1].(*ast.Ident); ok {
					sweeperSpread = id.Name
				}
			}
			if sel.Sel.Name == "Wait" {
				waited[recv.Name] = true
			}
		}
		return true
	})
	if passVar == "" || folderVar == "" {
		t.Fatalf("main.go does not call wireUsageFolder with an identifier folder (pass=%q folder=%q)", passVar, folderVar)
	}
	if sweeperSpread != "sweepPasses" {
		t.Errorf("sweeper.New must spread the sweepPasses slice (got %q)", sweeperSpread)
	}
	if sweepPassesLast != passVar {
		t.Errorf("the usage-fold pass (%s) must be the LAST element of the sweepPasses literal so its slow lanes delay no other extra pass; last is %q", passVar, sweepPassesLast)
	}
	if !waited[folderVar] {
		t.Errorf("%s.Wait is never called in main.go: in-flight usage-fold kicks would not be drained at shutdown", folderVar)
	}
}

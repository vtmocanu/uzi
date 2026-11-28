package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
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
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range shutdownWaitViolations(string(src), folderVar) {
		t.Error(v)
	}
}

// shutdownWaitViolations checks, from source, that func run drains the usage folder at shutdown
// as a reachable, bounded step. The top-level statement list of run must contain, in order:
//
//  1. drainWG.Wait()
//  2. <ctx>, <cancel> := context.WithTimeout(<anything>, usageFolderShutdownBudget)
//  3. go func() { defer close(<ch>); <first non-defer statement> }() where that statement is
//     <folderVar>.Wait(<ctx>), alone or as the Init of an if statement
//  4. <-<ch>
//  5. lifecycle.Wait()
//
// An unlaunched closure, a Wait nested in an inner closure or an if body, an early return before
// it, a ctx not bound by the WithTimeout, or a receive on another channel are all reported.
func shutdownWaitViolations(src, folderVar string) []string {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", src, 0)
	if err != nil {
		return []string{"parse: " + err.Error()}
	}
	var stmts []ast.Stmt
	for _, d := range file.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == "run" && fn.Body != nil {
			stmts = fn.Body.List
		}
	}
	if stmts == nil {
		return []string{"func run not found"}
	}
	isMethodCall := func(e ast.Expr, recv, name string, nargs int) (*ast.CallExpr, bool) {
		call, ok := e.(*ast.CallExpr)
		if !ok || len(call.Args) != nargs {
			return nil, false
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != name {
			return nil, false
		}
		id, ok := sel.X.(*ast.Ident)
		return call, ok && id.Name == recv
	}
	exprCall := func(s ast.Stmt, recv, name string) bool {
		es, ok := s.(*ast.ExprStmt)
		if !ok {
			return false
		}
		_, ok = isMethodCall(es.X, recv, name, 0)
		return ok
	}
	next := func(from int, pred func(ast.Stmt) bool) int {
		for i := from; i < len(stmts); i++ {
			if pred(stmts[i]) {
				return i
			}
		}
		return -1
	}
	drainIdx := next(0, func(s ast.Stmt) bool { return exprCall(s, "drainWG", "Wait") })
	if drainIdx < 0 {
		return []string{"run: top-level drainWG.Wait() not found"}
	}
	var ctxName string
	ctxIdx := next(drainIdx+1, func(s ast.Stmt) bool {
		as, ok := s.(*ast.AssignStmt)
		if !ok || as.Tok != token.DEFINE || len(as.Lhs) != 2 || len(as.Rhs) != 1 {
			return false
		}
		c, ok1 := as.Lhs[0].(*ast.Ident)
		_, ok2 := as.Lhs[1].(*ast.Ident)
		call, ok3 := isMethodCall(as.Rhs[0], "context", "WithTimeout", 2)
		if !ok1 || !ok2 || !ok3 {
			return false
		}
		if b, ok := call.Args[1].(*ast.Ident); !ok || b.Name != "usageFolderShutdownBudget" {
			return false
		}
		ctxName = c.Name
		return true
	})
	if ctxIdx < 0 {
		return []string{"run: no `ctx, cancel := context.WithTimeout(..., usageFolderShutdownBudget)` after drainWG.Wait()"}
	}
	// waitStmt reports whether s is <folderVar>.Wait(<ctxName>), alone or as an if Init.
	waitStmt := func(s ast.Stmt) bool {
		var e ast.Expr
		switch x := s.(type) {
		case *ast.ExprStmt:
			e = x.X
		case *ast.IfStmt:
			as, ok := x.Init.(*ast.AssignStmt)
			if !ok || len(as.Rhs) != 1 {
				return false
			}
			e = as.Rhs[0]
		default:
			return false
		}
		call, ok := isMethodCall(e, folderVar, "Wait", 1)
		if !ok {
			return false
		}
		a, ok := call.Args[0].(*ast.Ident)
		return ok && a.Name == ctxName
	}
	var chName string
	goIdx := next(ctxIdx+1, func(s ast.Stmt) bool {
		gs, ok := s.(*ast.GoStmt)
		if !ok || len(gs.Call.Args) != 0 {
			return false
		}
		lit, ok := gs.Call.Fun.(*ast.FuncLit)
		if !ok || len(lit.Body.List) < 2 {
			return false
		}
		ds, ok := lit.Body.List[0].(*ast.DeferStmt)
		if !ok || len(ds.Call.Args) != 1 {
			return false
		}
		if f, ok := ds.Call.Fun.(*ast.Ident); !ok || f.Name != "close" {
			return false
		}
		ch, ok := ds.Call.Args[0].(*ast.Ident)
		if !ok {
			return false
		}
		for _, st := range lit.Body.List[1:] {
			if _, isDefer := st.(*ast.DeferStmt); isDefer {
				continue
			}
			if !waitStmt(st) {
				return false
			}
			chName = ch.Name
			return true
		}
		return false
	})
	if goIdx < 0 {
		return []string{"run: no launched `go func() { defer close(ch); <folder>.Wait(ctx) ... }()` whose first non-defer statement is the folder Wait with the bounded ctx"}
	}
	recvIdx := next(goIdx+1, func(s ast.Stmt) bool {
		es, ok := s.(*ast.ExprStmt)
		if !ok {
			return false
		}
		u, ok := es.X.(*ast.UnaryExpr)
		if !ok || u.Op != token.ARROW {
			return false
		}
		id, ok := u.X.(*ast.Ident)
		return ok && id.Name == chName
	})
	if recvIdx < 0 {
		return []string{"run: no top-level receive `<-" + chName + "` after the launching go statement"}
	}
	if next(recvIdx+1, func(s ast.Stmt) bool { return exprCall(s, "lifecycle", "Wait") }) < 0 {
		return []string{"run: top-level lifecycle.Wait() not found after the folder drain receive"}
	}
	return nil
}

func TestShutdownWaitViolationsFixtures(t *testing.T) {
	base := map[string]string{
		"drain":  "drainWG.Wait()\n",
		"assign": "c, cancel := context.WithTimeout(context.Background(), usageFolderShutdownBudget)\n",
		"ch":     "done := make(chan struct{})\n",
		"launch": "go func() {\ndefer close(done)\nif err := usageFolder.Wait(c); err != nil {\nlog()\n}\n}()\n",
		"recv":   "<-done\n",
		"tail":   "cancel()\nlifecycle.Wait()\n",
	}
	build := func(over map[string]string) string {
		var b strings.Builder
		b.WriteString("package main\nfunc run() error {\n")
		order := []string{"drain", "assign", "ch", "launch", "recv", "tail"}
		for _, k := range order {
			if v, ok := over[k]; ok {
				b.WriteString(v)
			} else {
				b.WriteString(base[k])
			}
		}
		b.WriteString("return nil\n}\n")
		return b.String()
	}
	if v := shutdownWaitViolations(build(nil), "usageFolder"); len(v) != 0 {
		t.Fatalf("good fixture rejected: %v", v)
	}
	if v := shutdownWaitViolations(build(map[string]string{"launch": "go func() {\ndefer close(done)\nusageFolder.Wait(c)\n}()\n"}), "usageFolder"); len(v) != 0 {
		t.Errorf("plain ExprStmt Wait fixture rejected: %v", v)
	}
	bad := map[string]string{
		"unlaunched closure":      build(map[string]string{"launch": "func() {\ndefer close(done)\nusageFolder.Wait(c)\n}()\n"}),
		"wait in inner closure":   build(map[string]string{"launch": "go func() {\ndefer close(done)\nfunc() { usageFolder.Wait(c) }()\n}()\n"}),
		"wait in if body":         build(map[string]string{"launch": "go func() {\ndefer close(done)\nif x {\nusageFolder.Wait(c)\n}\n}()\n"}),
		"early return before":     build(map[string]string{"launch": "go func() {\ndefer close(done)\nif x {\nreturn\n}\nusageFolder.Wait(c)\n}()\n"}),
		"wait before drainWG":     build(map[string]string{"drain": "", "assign": base["assign"] + base["ch"] + base["launch"] + base["recv"] + "drainWG.Wait()\n", "ch": "", "launch": "", "recv": ""}),
		"ctx not from timeout":    build(map[string]string{"launch": "go func() {\ndefer close(done)\nif err := usageFolder.Wait(context.Background()); err != nil {\n}\n}()\n"}),
		"ctx bound to wrong budg": build(map[string]string{"assign": "c, cancel := context.WithTimeout(context.Background(), time.Hour)\n"}),
		"receive on other chan":   build(map[string]string{"recv": "<-other\n"}),
		"no receive":              build(map[string]string{"recv": ""}),
		"no lifecycle wait":       build(map[string]string{"tail": "cancel()\n"}),
		"wait in other function":  "package main\nfunc other() {\ndrainWG.Wait()\nc, cancel := context.WithTimeout(context.Background(), usageFolderShutdownBudget)\ndone := make(chan struct{})\ngo func() {\ndefer close(done)\nusageFolder.Wait(c)\n}()\n<-done\ncancel()\nlifecycle.Wait()\n}\nfunc run() error { return nil }\n",
	}
	for name, src := range bad {
		if len(shutdownWaitViolations(src, "usageFolder")) == 0 {
			t.Errorf("fixture %q accepted, want rejected", name)
		}
	}
}

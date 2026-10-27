package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
	"time"
)

// The production refresh callback performs one OAuth exchange capped at 5s, leaving
// request-entry-anchored margin for commit/recheck and response delivery. Nonrotating
// identity and usage reads retain their separate 2.5s cap.
func TestCodexProviderRequestTimeoutLeavesCommitMargin(t *testing.T) {
	if codexRefreshTimeout != 5*time.Second {
		t.Fatalf("refresh timeout = %s, want 5s", codexRefreshTimeout)
	}
	if codexProviderRequestTimeout != 2500*time.Millisecond {
		t.Fatalf("provider request timeout = %s, want 2.5s", codexProviderRequestTimeout)
	}
}

// Pin the real production construction site. A service-only test with an injected fake
// remains green if main forgets the setter, which is exactly the deployment regression
// this test must catch.
func TestProductionWiresCodexRefreshClient(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse production main.go: %v", err)
	}
	found := false
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || len(call.Args) != 1 {
			return true
		}
		setter, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || setter.Sel.Name != "SetCodexRefresh" {
			return true
		}
		receiver, ok := setter.X.(*ast.Ident)
		if !ok || receiver.Name != "wsvc" {
			return true
		}
		constructor, ok := call.Args[0].(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := constructor.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, pkgOK := selector.X.(*ast.Ident)
		if !pkgOK || pkg.Name != "codexauth" || selector.Sel.Name != "NewClient" {
			return true
		}
		want := map[string]string{
			"WithPerRequestTimeout": "codexProviderRequestTimeout",
			"WithRefreshTimeout":    "codexRefreshTimeout",
		}
		seen := make(map[string]bool)
		for _, arg := range constructor.Args {
			timeoutCall, ok := arg.(*ast.CallExpr)
			if !ok {
				continue
			}
			timeoutSelector, ok := timeoutCall.Fun.(*ast.SelectorExpr)
			if !ok {
				continue
			}
			timeoutPkg, ok := timeoutSelector.X.(*ast.Ident)
			if !ok || timeoutPkg.Name != "codexauth" {
				continue
			}
			name := timeoutSelector.Sel.Name
			wantArg, required := want[name]
			if !required {
				continue
			}
			if seen[name] || len(timeoutCall.Args) != 1 {
				t.Fatalf("production refresh client has duplicate or malformed %s", name)
			}
			timeoutArg, ok := timeoutCall.Args[0].(*ast.Ident)
			if !ok || timeoutArg.Name != wantArg {
				t.Fatalf("production refresh client %s must use %s", name, wantArg)
			}
			seen[name] = true
		}
		if !seen["WithPerRequestTimeout"] || !seen["WithRefreshTimeout"] {
			t.Fatal("production refresh client must set both distinct timeout options")
		}
		found = true
		return true
	})
	if !found {
		t.Fatal("production worker service does not inject codexauth.NewClient with the pinned timeout options")
	}
}

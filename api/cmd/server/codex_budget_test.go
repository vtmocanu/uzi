package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
	"time"
)

// The production refresh path performs an OAuth exchange and a verified identity read
// serially. Two and a half seconds per call caps network time at five seconds, preserving
// the request-entry-anchored commit/recheck and response reserves.
func TestCodexProviderRequestTimeoutLeavesCommitMargin(t *testing.T) {
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
		if !pkgOK || pkg.Name != "codexauth" || selector.Sel.Name != "NewClient" || len(constructor.Args) != 1 {
			return true
		}
		timeoutCall, ok := constructor.Args[0].(*ast.CallExpr)
		if !ok || len(timeoutCall.Args) != 1 {
			return true
		}
		timeoutSelector, ok := timeoutCall.Fun.(*ast.SelectorExpr)
		if !ok || timeoutSelector.Sel.Name != "WithPerRequestTimeout" {
			return true
		}
		timeoutPkg, pkgOK := timeoutSelector.X.(*ast.Ident)
		timeoutArg, argOK := timeoutCall.Args[0].(*ast.Ident)
		found = pkgOK && argOK && timeoutPkg.Name == "codexauth" && timeoutArg.Name == "codexProviderRequestTimeout"
		return !found
	})
	if !found {
		t.Fatal("production worker service does not inject codexauth.NewClient with the pinned per-request timeout")
	}
}

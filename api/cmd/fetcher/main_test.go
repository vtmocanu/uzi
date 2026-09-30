package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/fetcher"
)

// main builds the Fetcher's Options only through fetcher.Config.FetcherOptions (whose
// output carries no test seam; internal/fetcher's TestFetcherOptionsHaveNoTestSeam),
// never from a literal it could add RootCAs or a Resolver to, and serves TLS only.
func TestMainBuildsOptionsFromConfigOnly(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	isSel := func(e ast.Expr, pkg, name string) bool {
		sel, ok := e.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != name {
			return false
		}
		id, ok := sel.X.(*ast.Ident)
		return ok && (pkg == "" || id.Name == pkg)
	}
	var news, serveTLS int
	ast.Inspect(file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CompositeLit:
			if isSel(n.Type, "fetcher", "Options") {
				t.Errorf("%s: main builds a fetcher.Options literal", fset.Position(n.Pos()))
			}
		case *ast.CallExpr:
			switch {
			case isSel(n.Fun, "fetcher", "New"):
				news++
				arg, ok := n.Args[0].(*ast.CallExpr)
				if len(n.Args) != 1 || !ok || !isSel(arg.Fun, "cfg", "FetcherOptions") {
					t.Errorf("%s: fetcher.New is not called with cfg.FetcherOptions(...)", fset.Position(n.Pos()))
				}
			case isSel(n.Fun, "srv", "ServeTLS"):
				serveTLS++
			case isSel(n.Fun, "srv", "Serve"), isSel(n.Fun, "srv", "ListenAndServe"):
				t.Errorf("%s: a plain-HTTP listener", fset.Position(n.Pos()))
			}
		}
		return true
	})
	if news != 1 || serveTLS != 1 {
		t.Fatalf("fetcher.New calls = %d, ServeTLS calls = %d; want 1 each", news, serveTLS)
	}
}

// The write deadline (armed when the request headers are read, so it spans reading the
// body too) and the shutdown budget both cover the handler's worst case: Begin, the whole
// fetch, the source-log write and streaming the largest body.
func TestServerTimeoutsCoverTheHandler(t *testing.T) {
	for _, cfg := range []fetcher.Config{
		{Timeout: fetcher.DefaultTimeout, MaxFileBytes: fetcher.DefaultMaxFileBytes},
		{Timeout: 5 * time.Minute, MaxFileBytes: 200 << 20},
		{Timeout: time.Second, MaxFileBytes: 1},
	} {
		write, shutdown := serverTimeouts(cfg)
		need := readTimeout + fetcher.HandlerBudget(fetcher.DefaultControlTimeout, cfg.Timeout, cfg.MaxFileBytes)
		if write < need {
			t.Errorf("timeout %s, max %d bytes: WriteTimeout %s < worst case %s", cfg.Timeout, cfg.MaxFileBytes, write, need)
		}
		if shutdown < write {
			t.Errorf("timeout %s, max %d bytes: shutdown budget %s < WriteTimeout %s", cfg.Timeout, cfg.MaxFileBytes, shutdown, write)
		}
	}
}

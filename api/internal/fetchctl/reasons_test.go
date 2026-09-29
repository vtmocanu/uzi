package fetchctl

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/fetcher"
)

// fetcherReasons reads every exported Reason* string constant declared in the fetcher's
// non-test sources.
func fetcherReasons(t *testing.T) map[string]string {
	t.Helper()
	dir := filepath.Join("..", "fetcher")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read fetcher dir: %v", err)
	}
	out := map[string]string{}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, id := range vs.Names {
					if !strings.HasPrefix(id.Name, "Reason") || i >= len(vs.Values) {
						continue
					}
					lit, ok := vs.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						t.Fatalf("%s is not a string literal constant", id.Name)
					}
					v, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatalf("unquote %s: %v", id.Name, err)
					}
					out[id.Name] = v
				}
			}
		}
	}
	return out
}

// TestRefusalReasonsMatchFetcher pins refusalReasons to the fetcher's Reason* constants in
// both directions.
func TestRefusalReasonsMatchFetcher(t *testing.T) {
	got := fetcherReasons(t)
	if len(got) == 0 {
		t.Fatal("found no Reason* constants in api/internal/fetcher: the parser is blind")
	}
	if _, ok := got["ReasonCancelled"]; !ok {
		t.Fatal("ReasonCancelled not found: the parser is not reading the fetcher's constants")
	}
	values := map[string]bool{}
	for name, v := range got {
		values[v] = true
		if !KnownReason(v) {
			t.Errorf("fetcher.%s = %q is not in fetchctl.refusalReasons: the api would refuse to log it", name, v)
		}
	}
	for v := range refusalReasons {
		if !values[v] {
			t.Errorf("fetchctl.refusalReasons has %q, which is no fetcher Reason* constant", v)
		}
	}
}

// TestControlWireMatchesFetcher pins the route paths and the credential reason to the
// fetcher's HTTPControl (a test-only import: the api binary never links the fetcher).
func TestControlWireMatchesFetcher(t *testing.T) {
	if BeginPath != fetcher.BeginPath || CompletePath != fetcher.CompletePath {
		t.Fatalf("paths %q %q, fetcher calls %q %q", BeginPath, CompletePath, fetcher.BeginPath, fetcher.CompletePath)
	}
	if ReasonCredentialInvalid != fetcher.ReasonCredentialInvalid {
		t.Fatalf("credential reason %q, fetcher expects %q", ReasonCredentialInvalid, fetcher.ReasonCredentialInvalid)
	}
	for _, code := range []string{AdmissionRunBytes, AdmissionRunFiles, AdmissionConcurrency, AdmissionAttempts} {
		if !admissionCodeShape.MatchString(code) {
			t.Errorf("admission code %q does not match the shape the fetcher relays", code)
		}
	}
}

// admissionCodeShape is controlhttp.go's admissionCode (unexported there): a code of any
// other shape reaches the worker as "unspecified".
var admissionCodeShape = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

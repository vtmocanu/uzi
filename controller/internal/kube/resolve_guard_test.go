package kube

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every hosted worker must resolve through Materializer.resolveWorker, which feeds the
// integers the api sent into the preset. A direct `.resolver.Resolve(` call silently
// falls back to the compiled-in table, so a size edit would reach the render at one site
// and not another (PRD #4213). Plain Resolve stays legal only on a builtins-only caller
// that has no DesiredWorker (ValidatePVCCeilings), which does not go through
// a `.resolver.` selector.
func TestNoDirectResolverResolveOutsideTheHelper(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("glob: %v (%d files)", err, len(files))
	}
	scanned := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		scanned++
		raw, err := os.ReadFile(f) //nolint:gosec // G304: a glob of this package's own sources
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(raw), "\n") {
			if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "//") {
				continue
			}
			if strings.Contains(line, ".resolver.Resolve(") {
				t.Errorf("%s:%d calls .resolver.Resolve( directly; use Materializer.resolveWorker", f, i+1)
			}
		}
	}
	if scanned == 0 {
		t.Fatal("scanned no non-test sources")
	}
}

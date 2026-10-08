package recovery

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"
)

// Parse the actual server-only vocabulary without importing workersvc into production.
func TestCustodyExhaustionCauseMatchesServerVocabulary(t *testing.T) {
	const custodyRecoveryCauseWorkerRequeueExhausted = "worker_requeue_exhausted"
	file, err := parser.ParseFile(token.NewFileSet(), "../workersvc/forgepark.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	ast.Inspect(file, func(node ast.Node) bool {
		spec, ok := node.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for _, name := range spec.Names {
			if name.Name != "serverRecoveryWaitCauses" {
				continue
			}
			for _, value := range spec.Values {
				literal, ok := value.(*ast.CompositeLit)
				if !ok {
					t.Fatal("server vocabulary is no longer a map literal")
				}
				for _, element := range literal.Elts {
					pair, ok := element.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					key, ok := pair.Key.(*ast.BasicLit)
					if !ok || key.Kind != token.STRING {
						continue
					}
					cause, err := strconv.Unquote(key.Value)
					if err != nil {
						t.Fatal(err)
					}
					if cause == custodyRecoveryCauseWorkerRequeueExhausted {
						enabled, ok := pair.Value.(*ast.Ident)
						if !ok || enabled.Name != "true" {
							t.Fatal("exhaustion cause is disabled")
						}
						found = true
					}
				}
			}
		}
		return true
	})
	if !found {
		t.Fatalf("custody cause %q is absent from serverRecoveryWaitCauses", custodyRecoveryCauseWorkerRequeueExhausted)
	}
}

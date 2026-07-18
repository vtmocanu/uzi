package main

import (
	"os/exec"
	"strings"
	"testing"
)

// TestNoServerDeps is Success Criterion 8's tripwire: the CLI binary must never
// link the server's storage/router stack. `apitypes` exists precisely so the
// CLI can decode DTOs without importing internal/handler — which would drag
// github.com/jackc/pgx and github.com/go-chi/chi into `go list -deps ./cmd/uzi`.
// If someone reaches into internal/handler for a convenient DTO, this test goes
// red (a build failure, not a review nit).
//
// Buildvcs note: in a linked git worktree, `go list` on a main package trips
// "error obtaining VCS status". `-buildvcs=false` only affects binary VCS
// stamping, which is irrelevant to listing a package's dependency closure, so
// passing it makes this check run unconditionally — worktree or normal checkout,
// GOFLAGS set or not — instead of skipping and leaving CI as the only gate.
func TestNoServerDeps(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "-buildvcs=false", ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps -buildvcs=false .: %v\n%s", err, out)
	}
	for _, banned := range []string{"github.com/jackc/pgx", "github.com/go-chi/chi"} {
		if strings.Contains(string(out), banned) {
			t.Errorf("cmd/uzi must not depend on %s — it drags the server stack into the CLI binary (import internal/apitypes for DTOs, never internal/handler)", banned)
		}
	}
}

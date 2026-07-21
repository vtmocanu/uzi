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
	// internal/workersvc is banned BY NAME, not merely via pgx. It is the service layer and
	// the CLI must not link it, but the two pgx/chi entries only catch it TRANSITIVELY —
	// they happen to be reachable from workersvc today. If workersvc ever became pgx-free, a
	// direct `import ".../internal/workersvc"` from cmd/uzi would sail past this gate while
	// violating exactly the layering it exists to protect. Naming it states the invariant
	// where it is enforced instead of relying on a dependency of a dependency.
	//
	// It is a live temptation rather than a hypothetical: workersvc owns the bucket and
	// scope constants the CLI forwards, and PRD #98 M7 reaches for them from a TEST-ONLY
	// import for exactly that reason (a test import does not appear in `go list -deps` on
	// the non-test package, which is what keeps that legal).
	for _, banned := range []string{
		"github.com/jackc/pgx",
		"github.com/go-chi/chi",
		"gitlab.example.com/vtmocanu/uzi/api/internal/workersvc",
	} {
		if strings.Contains(string(out), banned) {
			t.Errorf("cmd/uzi must not depend on %s — it drags the server stack into the CLI binary (import internal/apitypes for DTOs, never internal/handler or internal/workersvc)", banned)
		}
	}
}

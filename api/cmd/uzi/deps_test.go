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
// "error obtaining VCS status" unless buildvcs is off. The documented local
// gate runs `GOFLAGS=-buildvcs=false go test ./...`, and this subprocess
// inherits that GOFLAGS; CI runs from a normal checkout where it is not needed.
// If neither applies (a bare `go test` inside a worktree), we skip with
// guidance rather than commit -buildvcs=false or report a false layering
// failure — CI, the authoritative gate, always runs the real check.
func TestNoServerDeps(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "buildvcs") {
			t.Skipf("go list tripped VCS stamping in this worktree; rerun with GOFLAGS=-buildvcs=false (CI runs it unconditionally). output:\n%s", out)
		}
		t.Fatalf("go list -deps .: %v\n%s", err, out)
	}
	for _, banned := range []string{"github.com/jackc/pgx", "github.com/go-chi/chi"} {
		if strings.Contains(string(out), banned) {
			t.Errorf("cmd/uzi must not depend on %s — it drags the server stack into the CLI binary (import internal/apitypes for DTOs, never internal/handler)", banned)
		}
	}
}

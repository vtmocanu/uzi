package config

import (
	"strings"
	"testing"
)

// SECOND GITLEAKS CANARY -- DO NOT DELETE, DO NOT "FIX", DO NOT ANNOTATE.
//
// This file has nothing to do with `config`. It lives in a real test package on
// purpose: it is the half of `scripts/scan-secrets.sh`'s liveness check that has
// to sit INSIDE the test corpus, and a test package is where the test corpus is.
//
// The first canary (`scripts/gitleaks-canary.txt`) catches a scanner that has
// been switched OFF: any `.gitleaks.toml` allowlist broad enough to hide a real
// secret hides that canary too. It cannot catch a scanner that has been NARROWED.
//
// AND THE NARROWING THAT MATTERS IS THE ONE WRITTEN CORRECTLY. A bare
// `[allowlist] paths` REPLACES gitleaks' ruleset, so nothing loads at all, every
// canary dies, and that spelling was caught before this file existed. Put
// `[extend] useDefault = true` above it -- what a careful contributor writes --
// and the rules stay loaded while only the named paths go unread. That is the
// form measured below.
//
// Measured 2026-08-03 with one planted secret in a tracked `_test.go`: with
// `[extend] useDefault = true` plus an `[allowlist]` whose `paths` entry is the
// regex `.*_test\.go` (in gitleaks' own triple-single-quoted TOML form, which is
// not written literally here because gofmt rewrites three consecutive apostrophes
// inside a comment into a typographic quote), the scan went from exit 1 with the
// finding named to exit 0 with the first canary still cheerfully reported, and
// the only trace was the byte counter dropping 28.24 MB -> 24.40 MB. That
// allowlist is the tempting disposal for this repo's fake-token fixtures, and it
// turns off secret scanning for every test file forever.
//
// With this file present, that same config kills THIS canary and the wrapper
// exits 2. It closes the measured instance and not narrowing in general: an
// allowlist scoped to some other directory still slips past both canaries. A
// canary only sees the region it is planted in.
//
// WHY IT IS A REAL TEST AND NOT A BARE CONST. An unexported const nothing reads
// is a `golangci-lint` `unused` finding, and this repo's linter is ratcheted with
// `whole-files: true`, so a new file carrying one would block on its own
// introduction. The assertion below is not filler for that, though: it fails if
// the token is edited into a shape gitleaks' `gitlab-pat` rule would stop
// matching, which is one of the three ways a canary dies. So the death shows up
// twice -- as `task gate:repo` exit 2 and as a named failing test.
const secondCanaryToken = "glpat-uziCANARYcorpus09876"

func TestGitleaksSecondCanaryIsStillDetectable(t *testing.T) {
	if !strings.HasPrefix(secondCanaryToken, "glpat-") {
		t.Fatalf("second gitleaks canary no longer starts with glpat-: %q", secondCanaryToken)
	}
	// gitleaks' gitlab-pat rule wants 20+ characters after the prefix. Asserting
	// the floor rather than an exact length leaves the token free to change while
	// keeping it detectable.
	if body := strings.TrimPrefix(secondCanaryToken, "glpat-"); len(body) < 20 {
		t.Fatalf("second gitleaks canary body is %d chars, under the 20 gitlab-pat needs", len(body))
	}
}

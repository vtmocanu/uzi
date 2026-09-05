package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// PRD #1130 M4, success criterion 4: a FAILED board poll must still render the
// "could not refresh" error banner WITH the last-good run list beneath it — the
// poll-resilience changes must not regress boardState.apply's error path, which
// keeps b.runs intact on error and only sets b.err. This guards the deterministic
// View()->string seam (no devbox/uxlab env, no screenshots) so the poll-loop
// hardening cannot silently break the visible board.
func TestBoardErrorLineKeepsLastGoodRuns(t *testing.T) {
	fake := &uzicli.FakeClient{Runs: []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: "aaaaaaaa-1111-2222-3333-444444444444", Kind: "issue", Status: "running", IssueTitle: "last good issue"}},
	}}
	m := tuiTestModel(t, fake, "")

	// Populate the board with a successful reply so the run list is rendered (baseline).
	next, _ := m.Update(boardRunsMsg{runs: fake.Runs})
	m = next.(tuiModel)

	baseline := m.View().Content
	for _, want := range []string{"aaaaaaaa", "last good issue"} {
		if !strings.Contains(baseline, want) {
			t.Fatalf("baseline board does not render the seeded run %q\n%s", want, baseline)
		}
	}
	if strings.Contains(baseline, "could not refresh") {
		t.Fatalf("baseline board already shows the error banner before any failed poll\n%s", baseline)
	}

	// A FAILED board poll: match m.board.admin (default false) so apply does not
	// early-return on an admin mismatch, and set an error like the bug report's.
	next, _ = m.Update(boardRunsMsg{admin: m.board.admin, err: errors.New("reading response from uzi: context deadline exceeded")})
	m = next.(tuiModel)

	out := m.View().Content
	// The error banner is present...
	if !strings.Contains(out, "could not refresh") {
		t.Errorf("failed poll did not render the error banner\n%s", out)
	}
	if !strings.Contains(out, "context deadline exceeded") {
		t.Errorf("failed poll did not render the wrapped error message\n%s", out)
	}
	// ...and the last-good run list is STILL rendered beneath it (b.runs preserved).
	for _, want := range []string{"aaaaaaaa", "last good issue"} {
		if !strings.Contains(out, want) {
			t.Errorf("the last-good run %q was wiped from the board on a failed poll; apply must keep b.runs on error\n%s", want, out)
		}
	}
}

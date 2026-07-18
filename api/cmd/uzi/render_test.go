package main

import (
	"bytes"
	"strings"
	"testing"

	"gitlab.example.com/vtmocanu/uzi/api/internal/apitypes"
	"gitlab.example.com/vtmocanu/uzi/api/internal/uzicli"
)

// TestMrAbbrev pins the forge-aware merge/pull-request label (PRD #65 D2), the
// CLI twin of the web's mrAbbrev and slacksvc's forgeMrAbbrev: Forgejo "PR",
// everything else (GitLab, empty, unknown) "MR".
func TestMrAbbrev(t *testing.T) {
	for _, tc := range []struct {
		forge string
		want  string
	}{
		{"forgejo", "PR"},
		{"gitlab", "MR"},
		{"", "MR"},
		{"something_else", "MR"},
	} {
		if got := mrAbbrev(tc.forge); got != tc.want {
			t.Errorf("mrAbbrev(%q) = %q, want %q", tc.forge, got, tc.want)
		}
	}
}

// TestRenderRunDetailForgeAwareMRColumn is the end-to-end proof that `uzi run get`
// labels the request column per the run's forge — a Forgejo run reads "PR", a
// GitLab run "MR", off r.ForgeType (the field the landing merge threads onto
// apitypes.RunDTO). This is the forge-blind "MR" hardcode the CLI had until #65.
func TestRenderRunDetailForgeAwareMRColumn(t *testing.T) {
	iid := int64(42)
	render := func(forge string) string {
		var buf bytes.Buffer
		p := uzicli.NewPrinter(&buf, false, false, true, false) // non-tty, non-json, no colour
		r := apitypes.RunDTO{
			ID:         "run-1",
			Kind:       "issue",
			Status:     "completed",
			IssueTitle: "do the thing",
			ForgeType:  forge,
			MrIID:      &iid,
			Health:     "ok",
		}
		if err := renderRunDetail(p, r); err != nil {
			t.Fatalf("renderRunDetail(%q): %v", forge, err)
		}
		return buf.String()
	}

	fj := render("forgejo")
	if !strings.Contains(fj, "PR") {
		t.Errorf("a Forgejo run must render a PR column, got:\n%s", fj)
	}
	// The GitLab label must not leak onto a Forgejo run's detail.
	if strings.Contains(fj, "MR ") || strings.Contains(fj, "MR\t") {
		t.Errorf("a Forgejo run must NOT render an MR column label, got:\n%s", fj)
	}

	gl := render("gitlab")
	if !strings.Contains(gl, "MR") {
		t.Errorf("a GitLab run must render an MR column, got:\n%s", gl)
	}
}

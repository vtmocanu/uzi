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

// TestRenderRunDetailAnthropicToken pins `uzi run get`'s ANTHROPIC_TOKEN row
// (PRD #111 M1) and, more importantly, the sanitization it goes through.
//
// The label is the first genuinely USER-AUTHORED string this block renders, and the
// two facts that make it dangerous are both measured, not assumed:
// validateSecretLabel (handler/secrets.go) rejects control characters and U+FFFD but
// NOT unicode.Cf, so a bidi-override label is storable and passes the DB CHECK; and
// uzicli.Printer.Table does not sanitize what it is handed — it joins the cells and
// flushes. cellText is what closes that, and this asserts it rather than the row's
// mere presence, because a row rendered through the wrong helper looks identical
// until someone stores a hostile label.
func TestRenderRunDetailAnthropicToken(t *testing.T) {
	render := func(label *string) string {
		t.Helper()
		var buf bytes.Buffer
		p := uzicli.NewPrinter(&buf, false, false, true, false) // non-tty, non-json, no colour
		r := apitypes.RunDTO{
			ID: "run-1", Kind: "issue", Status: "completed",
			IssueTitle: "do the thing", ForgeType: "gitlab", Health: "ok",
			AnthropicSecretLabel: label,
		}
		if err := renderRunDetail(p, r); err != nil {
			t.Fatalf("renderRunDetail: %v", err)
		}
		return buf.String()
	}

	label := "console-key"
	if out := render(&label); !strings.Contains(out, "ANTHROPIC_TOKEN") || !strings.Contains(out, "console-key") {
		t.Errorf("a run with a recorded credential must name it, got:\n%s", out)
	}

	// Absent and empty both render NO row: a run that cannot say which account it
	// billed must not appear to have said something. Empty is unreachable through the
	// API (user_secrets.label is NOT NULL with a 1..64 CHECK) and is pinned anyway,
	// because the guard has to be emptiness-based rather than a nil check.
	if out := render(nil); strings.Contains(out, "ANTHROPIC_TOKEN") {
		t.Errorf("a run with no recorded credential must render no row, got:\n%s", out)
	}
	empty := ""
	if out := render(&empty); strings.Contains(out, "ANTHROPIC_TOKEN") {
		t.Errorf("an empty label must render no row, got:\n%s", out)
	}

	// A bidi override (U+202E RIGHT-TO-LEFT OVERRIDE) plus a CSI escape and a newline:
	// the escape would repaint the terminal, the override would visually reverse the
	// label so it READS as a different account, and the newline would break the table
	// rail. All three must be gone, and the printable text must survive.
	//
	// 🔴 THIS FIXTURE IS DELIBERATELY UN-STORABLE THROUGH THE API, AND THE TEST STILL
	// EARNS ITS KEEP. Since PRD #111 M2, validateSecretLabel (handler/secrets.go)
	// rejects both unicode.IsControl and unicode.Cf, so no label containing these bytes
	// can be written through any handler today. Do NOT "tidy" this into a reachable
	// fixture, and do NOT drop the cellText routing it pins:
	//
	//   - The two live on opposite sides of a trust boundary. The validator is what the
	//     SERVER accepts; this is what the RENDERER does with whatever it is handed.
	//     Depending on the far side of a boundary for local safety is exactly the
	//     coupling that turns one regression into two.
	//   - Three routes reach the renderer without passing that validator: a label
	//     stored before M2 landed (existing rows are not re-validated), a future write
	//     path that forgets the check, and a row written straight to the database.
	//
	// 🔴 THE NEWLINE PROBE IS THE ONLY ONE THAT DISCRIMINATES, AND IT WAS WRONG.
	// It read `"\n\nnext-line"` — a DOUBLE newline neither implementation ever emits —
	// so it was satisfied by construction. The bidi and ESC probes do not discriminate
	// either: sanitizeTTY and cellText both strip unicode.Cf and unicode.IsControl, so
	// two of the three agree under either helper. Measured: swapping cellText for
	// sanitizeTTY left this test GREEN.
	//
	// Newline FOLDING is the whole difference between them (cellText → compactText
	// replaces "\n" with a space; sanitizeTTY deliberately spares "\n"), so a single
	// "\nnext-line" is what tells them apart, and it is what is asserted now.
	//
	// The class this belongs to is worth naming: the broken implementation and the
	// correct one AGREE on the case that was picked, so the fixture passed against
	// both and read as proof of something it never tested.
	hostile := "safe‮dnetsop\x1b[31m\nnext-line"
	out := render(&hostile)
	// The first two pin the shared floor (either helper satisfies them); the third is
	// the discriminating one.
	for _, bad := range []string{"‮", "\x1b", "\nnext-line"} {
		if strings.Contains(out, bad) {
			t.Errorf("hostile label reached the terminal carrying %q, got:\n%q", bad, out)
		}
	}
	if !strings.Contains(out, "safe") || !strings.Contains(out, "next-line") {
		t.Errorf("sanitizing dropped the printable text too, got:\n%q", out)
	}
}

package main

import (
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// PRD #1247 M8 — the CLI half: `uzi run get` surfaces the per-run token CHOICE, the in-flight
// switch, and the applied-switch history, DISTINCT from the ANTHROPIC_TOKEN (spent) row.

// TestRenderRunDetailCredentialOverride pins the TOKEN row for each override mode plus the pending
// switch it folds onto that row. sptr/iptr live in run_credential_test.go (same package).
func TestRenderRunDetailCredentialOverride(t *testing.T) {
	base := func(co *apitypes.CredentialOverrideDTO, sw *string) apitypes.RunDTO {
		return apitypes.RunDTO{
			ID: "run-1", Kind: "issue", Status: "running",
			IssueTitle: "do the thing", ForgeType: "gitlab", Health: "ok",
			CredentialOverride: co, CredentialSwitch: sw,
		}
	}

	// A pinned override names its label + the run-pinned reason; a requested switch folds onto the
	// same TOKEN row.
	out := renderDetail(t, base(&apitypes.CredentialOverrideDTO{Mode: "pinned", Label: sptr("primary-key")}, sptr("requested")))
	if !strings.Contains(out, "TOKEN") || !strings.Contains(out, "primary-key (run-pinned)") {
		t.Errorf("a pinned override must render its label and run-pinned reason, got:\n%s", out)
	}
	if !strings.Contains(out, "switch requested") {
		t.Errorf("a requested switch must surface on the TOKEN row, got:\n%s", out)
	}

	// auto / default render the bare mode.
	if out := renderDetail(t, base(&apitypes.CredentialOverrideDTO{Mode: "auto"}, nil)); !strings.Contains(out, "TOKEN") || !credTokenValueContains(out, "auto") {
		t.Errorf("an auto override must render 'auto' on the TOKEN row, got:\n%s", out)
	}
	if out := renderDetail(t, base(&apitypes.CredentialOverrideDTO{Mode: "default"}, nil)); !strings.Contains(out, "TOKEN") || !credTokenValueContains(out, "default") {
		t.Errorf("a default override must render 'default' on the TOKEN row, got:\n%s", out)
	}

	// A "released" switch spells out the awaiting-reclaim state; here with NO override, so the switch
	// stands on the TOKEN row alone — the CredentialSwitch field is independent of the choice.
	if out := renderDetail(t, base(nil, sptr("released"))); !strings.Contains(out, "TOKEN") || !strings.Contains(out, "switch released, awaiting reclaim") {
		t.Errorf("a released switch with no override must stand alone on the TOKEN row, got:\n%s", out)
	}

	// The inherit case (no override, no switch, no multi-credential history) renders NO TOKEN row —
	// the back-compat floor a run that predates the feature keeps.
	inherit := apitypes.RunDTO{ID: "run-2", Kind: "issue", Status: "running", Health: "ok"}
	if out := renderDetail(t, inherit); strings.Contains(out, "\nTOKEN") || strings.Contains(out, "switch ") {
		t.Errorf("an inheriting run must render no TOKEN choice row, got:\n%s", out)
	}
}

// TestRenderRunDetailCredentialEpochs pins the epoch history: it renders ONLY when an applied switch
// actually changed the credential (more than one distinct label), and each epoch names its token,
// reason and applied_at.
func TestRenderRunDetailCredentialEpochs(t *testing.T) {
	applied := time.Date(2026, 9, 16, 10, 30, 0, 0, time.UTC)
	twoTokens := []apitypes.CredentialEpochDTO{
		{ClaimGeneration: 1, Label: sptr("primary-key"), SelectReason: sptr("run_pinned"), AppliedAt: applied},
		{ClaimGeneration: 2, Label: sptr("backup-key"), SelectReason: sptr("run_default"), AppliedAt: applied.Add(time.Hour)},
	}
	run := apitypes.RunDTO{
		ID: "run-1", Kind: "issue", Status: "running", Health: "ok",
		CredentialEpochs: twoTokens,
	}
	out := renderDetail(t, run)
	if !strings.Contains(out, "TOKEN_HISTORY") {
		t.Errorf("a multi-credential history must render a TOKEN_HISTORY block, got:\n%s", out)
	}
	// Both generations, both labels, both reasons (via selectReasonText), and the applied_at stamps.
	for _, want := range []string{
		"gen 1", "gen 2", "primary-key", "backup-key",
		"run-pinned", "run-default", "2026-09-16T10:30:00Z", "2026-09-16T11:30:00Z",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("epoch history missing %q, got:\n%s", want, out)
		}
	}

	// A single-credential history (a plain reclaim across a resume — same token, two generations) is
	// NOT a switch and renders no history block: the ANTHROPIC_TOKEN row already names the one token.
	reclaim := apitypes.RunDTO{
		ID: "run-2", Kind: "issue", Status: "running", Health: "ok",
		CredentialEpochs: []apitypes.CredentialEpochDTO{
			{ClaimGeneration: 1, Label: sptr("primary-key"), SelectReason: sptr("run_pinned"), AppliedAt: applied},
			{ClaimGeneration: 2, Label: sptr("primary-key"), SelectReason: sptr("run_pinned"), AppliedAt: applied.Add(time.Hour)},
		},
	}
	if out := renderDetail(t, reclaim); strings.Contains(out, "TOKEN_HISTORY") {
		t.Errorf("a same-token reclaim is not a switch and must render no history block, got:\n%s", out)
	}

	// A deleted-token generation reads "(deleted)" rather than a bare em dash, and still counts as a
	// distinct credential against a named one so the switch history renders.
	deleted := apitypes.RunDTO{
		ID: "run-3", Kind: "issue", Status: "running", Health: "ok",
		CredentialEpochs: []apitypes.CredentialEpochDTO{
			{ClaimGeneration: 1, Label: nil, SelectReason: sptr("run_pinned"), AppliedAt: applied},
			{ClaimGeneration: 2, Label: sptr("backup-key"), SelectReason: sptr("run_default"), AppliedAt: applied.Add(time.Hour)},
		},
	}
	if out := renderDetail(t, deleted); !strings.Contains(out, "TOKEN_HISTORY") || !strings.Contains(out, "(deleted)") {
		t.Errorf("a deleted-token epoch must read (deleted) and still render the history, got:\n%s", out)
	}
}

// TestRenderRunDetailCredentialOverrideSanitizes: the pinned override label is USER-AUTHORED and
// lands in a table cell, so it must be folded (cellText) exactly like the ANTHROPIC_TOKEN row. A row
// stored before PRD #111 M2's validator landed is never re-validated, so a hostile label still
// arrives here through history. The single "\nnext-line" newline is the discriminating probe (a
// table cell must fold it; sanitizeTTY alone would spare it and break the rail).
func TestRenderRunDetailCredentialOverrideSanitizes(t *testing.T) {
	hostileLabel := "safe\u202ednetsop\x1b[31m\nnext-line"
	hostileEpoch := "epoch\u202ednetsop\x1b[31m\nfollowing"
	run := apitypes.RunDTO{
		ID: "run-1", Kind: "issue", Status: "running", Health: "ok",
		CredentialOverride: &apitypes.CredentialOverrideDTO{Mode: "pinned", Label: sptr(hostileLabel)},
		CredentialEpochs: []apitypes.CredentialEpochDTO{
			{ClaimGeneration: 1, Label: sptr(hostileEpoch), SelectReason: sptr("run_pinned"), AppliedAt: time.Unix(0, 0).UTC()},
			{ClaimGeneration: 2, Label: sptr("backup-key"), SelectReason: sptr("run_default"), AppliedAt: time.Unix(3600, 0).UTC()},
		},
	}
	out := renderDetail(t, run)
	for _, bad := range []string{"\u202e", "\x1b", "\nnext-line", "\nfollowing"} {
		if strings.Contains(out, bad) {
			t.Errorf("hostile credential text reached the terminal carrying %q, got:\n%q", bad, out)
		}
	}
	// The printable text survives on both the override cell and the epoch cell.
	if !strings.Contains(out, "safe") || !strings.Contains(out, "next-line") {
		t.Errorf("sanitizing dropped the printable override label, got:\n%q", out)
	}
	if !strings.Contains(out, "epoch") || !strings.Contains(out, "following") {
		t.Errorf("sanitizing dropped the printable epoch label, got:\n%q", out)
	}
}

// credTokenValueContains reports whether the TOKEN row's VALUE column contains want. The bare
// substring check on the whole frame would also match the TOKEN_HISTORY block or an unrelated row,
// so this narrows to the "TOKEN " line — enough to prove the mode landed on the choice row.
func credTokenValueContains(out, want string) bool {
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "TOKEN ") && strings.Contains(line, want) {
			return true
		}
	}
	return false
}

package main

import (
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// PRD #1247 M5b (D8): `uzi run approve --token` — the held-state credential switch composed
// AHEAD of the plan approval. These pin the CLI's composition through the FakeClient:
// SetRunCredential (with the resolved override) lands FIRST, then the approve_plan input;
// a resolution/switch error aborts before the approval; and without --token the command is
// byte-identical to today's approve (no SetRunCredential round-trip). The --token flag
// reuses the SAME client-side resolution as `run create --token` (resolveTokenFlagValue),
// so its label/keyword semantics match set-token and create.

// TestRunApproveTokenLabelResolvesBeforeApprove: `run approve <id> --token <label>` resolves
// the label client-side to {pinned, secret_id}, calls SetRunCredential with it, AND then
// submits approve_plan — both recorded on the fake, proving the switch composes with (not
// replaces) the approval.
func TestRunApproveTokenLabelResolvesBeforeApprove(t *testing.T) {
	fc := &uzicli.FakeClient{
		SetTokenRun: apitypes.RunDTO{ID: "run-9", Status: "awaiting_approval", Kind: "issue"},
		Secrets: []apitypes.SecretDTO{
			{ID: "sec-a", Kind: "anthropic_token", Label: "default-key"},
			{ID: "sec-b", Kind: "anthropic_token", Label: "prod-key"},
		},
	}
	_, _, code := runCLI(t, fakeEnv(fc), "run", "approve", "run-9", "--token", "prod-key")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	// The credential switch happened first, with the resolved pinned override.
	if fc.LastSetTokenRunID != "run-9" {
		t.Errorf("SetRunCredential run id = %q, want run-9", fc.LastSetTokenRunID)
	}
	if fc.LastSetTokenOverride == nil {
		t.Fatal("--token <label> sent no credential override")
	}
	if fc.LastSetTokenOverride.Mode != "pinned" {
		t.Errorf("mode = %q, want pinned", fc.LastSetTokenOverride.Mode)
	}
	if fc.LastSetTokenOverride.SecretID != "sec-b" {
		t.Errorf("secret_id = %q, want the resolved id sec-b (not the label)", fc.LastSetTokenOverride.SecretID)
	}
	// The approve still happened, on the same run.
	if fc.LastInputRunID != "run-9" || fc.LastInputKind != kindApprovePlan {
		t.Errorf("approve not submitted: run=%q kind=%q, want run-9/%s", fc.LastInputRunID, fc.LastInputKind, kindApprovePlan)
	}
}

// TestRunApproveTokenKeywords: --token auto|default|inherit ride as a bare mode with NO
// secret (no ListSecrets round-trip for a label), and the approve still fires afterwards.
func TestRunApproveTokenKeywords(t *testing.T) {
	for _, mode := range []string{"auto", "default", "inherit"} {
		fc := &uzicli.FakeClient{SetTokenRun: apitypes.RunDTO{ID: "run-9", Status: "awaiting_approval", Kind: "issue"}}
		_, _, code := runCLI(t, fakeEnv(fc), "run", "approve", "run-9", "--token", mode)
		if code != uzicli.ExitOK {
			t.Fatalf("--token %s: exit = %d, want 0", mode, code)
		}
		if fc.LastSetTokenOverride == nil {
			t.Fatalf("--token %s sent no credential override", mode)
		}
		if fc.LastSetTokenOverride.Mode != mode {
			t.Errorf("--token %s: mode = %q, want %q", mode, fc.LastSetTokenOverride.Mode, mode)
		}
		if fc.LastSetTokenOverride.SecretID != "" {
			t.Errorf("--token %s: secret_id = %q, want empty (a keyword carries no secret)", mode, fc.LastSetTokenOverride.SecretID)
		}
		if fc.LastInputKind != kindApprovePlan {
			t.Errorf("--token %s: approve not submitted (kind=%q)", mode, fc.LastInputKind)
		}
	}
}

// TestRunApproveWithoutTokenSkipsSetCredential: back-compat — a plain `run approve <id>`
// calls ONLY the approve (SetRunCredential is never reached, so LastSetTokenRunID stays
// empty), byte-identical to a pre-#1247 approve.
func TestRunApproveWithoutTokenSkipsSetCredential(t *testing.T) {
	fc := &uzicli.FakeClient{}
	_, _, code := runCLI(t, fakeEnv(fc), "run", "approve", "run-9")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.LastSetTokenRunID != "" || fc.LastSetTokenOverride != nil {
		t.Errorf("a --token-less approve must NOT touch SetRunCredential, but it ran (run=%q override=%+v)",
			fc.LastSetTokenRunID, fc.LastSetTokenOverride)
	}
	if fc.LastInputRunID != "run-9" || fc.LastInputKind != kindApprovePlan {
		t.Errorf("approve not submitted: run=%q kind=%q, want run-9/%s", fc.LastInputRunID, fc.LastInputKind, kindApprovePlan)
	}
}

// TestRunApproveTokenSetCredentialErrorAbortsApprove: when SetRunCredential fails (e.g. a
// 409 on a run whose switch cannot land), the approve is NOT sent and the error is returned.
// This pins the ORDER (credential switch first) and the abort-on-error: the write is reached
// (LastSetTokenRunID recorded) but the approve is not (LastInputKind stays empty).
func TestRunApproveTokenSetCredentialErrorAbortsApprove(t *testing.T) {
	fc := &uzicli.FakeClient{
		Secrets:             []apitypes.SecretDTO{{ID: "sec-b", Kind: "anthropic_token", Label: "prod-key"}},
		SetRunCredentialErr: uzicli.Exitf(uzicli.ExitConflict, "the worker has not advertised credential_switch_v1"),
	}
	_, _, code := runCLI(t, fakeEnv(fc), "run", "approve", "run-9", "--token", "prod-key")
	if code != uzicli.ExitConflict {
		t.Fatalf("exit = %d, want %d (conflict)", code, uzicli.ExitConflict)
	}
	if fc.LastSetTokenRunID != "run-9" {
		t.Errorf("SetRunCredential should have been reached first (run=%q, want run-9)", fc.LastSetTokenRunID)
	}
	if fc.LastInputKind != "" {
		t.Errorf("a failed credential switch must NOT approve, but approve_plan was sent (kind=%q)", fc.LastInputKind)
	}
}

// TestRunApproveTokenUnknownLabelRefusedClientSide: an unknown --token label is the SAME
// client-side usage refusal as `run set-token` (naming `uzi token list`), sent before any
// request — neither SetRunCredential nor the approve runs.
func TestRunApproveTokenUnknownLabelRefusedClientSide(t *testing.T) {
	fc := &uzicli.FakeClient{
		Secrets: []apitypes.SecretDTO{{ID: "sec-a", Kind: "anthropic_token", Label: "default-key"}},
	}
	_, errb, code := runCLI(t, fakeEnv(fc), "run", "approve", "run-9", "--token", "no-such-token")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want %d (usage)", code, uzicli.ExitUsage)
	}
	if fc.LastSetTokenRunID != "" {
		t.Errorf("an unknown label must NOT send a credential request, but SetRunCredential ran (run=%q)", fc.LastSetTokenRunID)
	}
	if fc.LastInputKind != "" {
		t.Errorf("an unknown label must NOT approve, but approve_plan was sent (kind=%q)", fc.LastInputKind)
	}
	if !containsAll(errb, "no Anthropic token", "uzi token list") {
		t.Errorf("refusal should name the read command; got: %q", errb)
	}
}

// TestRunApproveTokenComposesWithSelection: --token composes with --agent-source — the
// credential switch lands AND the approve carries the resolved agent selection, so a
// held-state token switch and a roster-scoped approval coexist.
func TestRunApproveTokenComposesWithSelection(t *testing.T) {
	fc := &uzicli.FakeClient{
		SetTokenRun: apitypes.RunDTO{ID: "run-9", Status: "awaiting_approval", Kind: "issue"},
		Secrets:     []apitypes.SecretDTO{{ID: "sec-b", Kind: "anthropic_token", Label: "prod-key"}},
	}
	_, _, code := runCLI(t, fakeEnv(fc), "run", "approve", "run-9", "--token", "prod-key", "--agent-source", "repo")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.LastSetTokenOverride == nil || fc.LastSetTokenOverride.SecretID != "sec-b" {
		t.Fatalf("the credential switch did not land with the resolved override: %+v", fc.LastSetTokenOverride)
	}
	if fc.LastInputKind != kindApprovePlan {
		t.Fatalf("approve not submitted (kind=%q)", fc.LastInputKind)
	}
	if fc.LastInputSelection == nil || fc.LastInputSelection.Source != "repo" {
		t.Errorf("approve did not carry the --agent-source selection: %+v", fc.LastInputSelection)
	}
}

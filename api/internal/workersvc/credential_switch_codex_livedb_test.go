package workersvc

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// These tests exercise the service seam and compare all run/journal/message
// state before and after; no provider client or credential opener is installed.
func TestCodexSwitchPreflightLiveDB(t *testing.T) {
	for _, scenario := range []string{
		"ready", "missing baseline", "disabled target", "foreign target",
		"stale nonce", "stale generation", "wrong operation", "released",
		"claimed", "awaiting_approval", "awaiting_input", "awaiting_followup",
		"policy refusal", "binding mismatch", "epoch mismatch",
	} {
		t.Run(scenario, func(t *testing.T) {
			f := newApplyFx(t)
			run, op := f.run(t, "running")
			old := f.env.seedStaticAPIKey(t, f.o.userID, "old-api", "old-test-key")
			target := f.env.seedStaticAPIKey(t, f.o.userID, "new-api", "new-test-key")
			if err := f.svc.FreezeCodexBinding(f.env.ctx, f.o.userID, run, old, codexAuthModeAPIKey); err != nil {
				t.Fatal(err)
			}
			if scenario != "missing baseline" {
				if err := f.svc.recordCodexEpoch(context.Background(), mustRun(t, f.env, run)); err != nil {
					t.Fatal(err)
				}
			}
			req := CodexSwitchPreflightRequest{CredentialSwitchApplyRequest: f.req(op), TargetSecretID: target}
			want, reason := "ready", ""
			switch scenario {
			case "missing baseline":
				want, reason = ApplyOutcomeNotApplied, "baseline_mismatch"
			case "disabled target":
				f.env.exec("UPDATE user_secrets SET disabled_at = now() WHERE id = $1", target)
				want, reason = ApplyOutcomeNotApplied, applyReasonCredentialDisabled
			case "foreign target":
				req.TargetSecretID = uuid.New()
				want, reason = ApplyOutcomeNotApplied, applyReasonCredentialGone
			case "stale nonce":
				req.RegisterNonce = "stale-nonce"
				want = ApplyOutcomeFenced
			case "stale generation":
				req.ClaimGeneration++
				want = ApplyOutcomeFenced
			case "wrong operation":
				req.OpID = uuid.NewString()
				want = ApplyOutcomeSuperseded
			case "released":
				f.env.exec("UPDATE runs SET claim_released_at = now() WHERE id = $1", run)
				want = ApplyOutcomeFenced
			case "claimed":
				f.env.exec("UPDATE runs SET status = 'claimed' WHERE id = $1", run)
				want = ApplyOutcomeFenced
			case "awaiting_approval", "awaiting_input", "awaiting_followup":
				f.env.exec("UPDATE runs SET status = $2 WHERE id = $1", run, scenario)
			case "policy refusal":
				f.env.exec("UPDATE runs SET policy_refusal_count = 1 WHERE id = $1", run)
				want, reason = ApplyOutcomeNotApplied, "policy_refusal"
			case "binding mismatch":
				f.env.exec("UPDATE run_credential_epochs SET codex_material_revision = codex_material_revision + 1 WHERE run_id = $1", run)
				want, reason = ApplyOutcomeNotApplied, "baseline_mismatch"
			case "epoch mismatch":
				f.env.exec("UPDATE run_credential_epochs SET codex_claim_epoch = codex_claim_epoch + 1 WHERE run_id = $1", run)
				want, reason = ApplyOutcomeNotApplied, "baseline_mismatch"
			}
			before := f.snap(t, run)
			got, err := f.svc.PreflightCodexCredentialSwitch(f.env.ctx, f.worker(t), run, req)
			if err != nil || got.Outcome != want || got.Reason != reason {
				t.Fatalf("preflight = %+v, %v; want %s/%s", got, err, want, reason)
			}
			if after := f.snap(t, run); after != before {
				t.Fatal("preflight changed run, epochs or messages")
			}
		})
	}
}

func TestCodexSwitchApplyDoesNotUseAnthropicLiveDB(t *testing.T) {
	f := newApplyFx(t)
	run, op := f.run(t, "running")
	alias := f.env.seedStaticAPIKey(t, f.o.userID, "codex-api", "test-api-key")
	if err := f.svc.FreezeCodexBinding(f.env.ctx, f.o.userID, run, alias, codexAuthModeAPIKey); err != nil {
		t.Fatal(err)
	}
	before := f.snap(t, run)
	got := f.apply(t, run, f.req(op))
	if got.Outcome != ApplyOutcomeNotApplied || got.Token != "" {
		t.Fatalf("Codex apply = %v", got)
	}
	if after := f.snap(t, run); after != before {
		t.Fatal("unsupported Codex apply changed state")
	}
}

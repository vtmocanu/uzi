package workersvc

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

// These tests exercise the service seam and compare all run/journal/message
// state before and after. The real local secret opener is configured; no provider client is installed.
func TestCodexSwitchPreflightLiveDB(t *testing.T) {
	for _, scenario := range []string{
		"ready", "locked vault", "subscription", "missing baseline", "disabled target", "foreign target",
		"stale nonce", "stale generation", "wrong operation", "released",
		"claimed", "awaiting_approval", "awaiting_input", "awaiting_followup",
		"policy refusal", "binding mismatch", "epoch mismatch",
		"wrong target", "pending outcome", "policy hold", "nullable snapshots",
		"corrupt material", "missing negotiation", "cleared target", "alias locked", "empty material",
	} {
		t.Run(scenario, func(t *testing.T) {
			f := newApplyFx(t)
			f.env.exec("UPDATE workers SET credential_switch_apply_nonce=snapshot_register_nonce, protocol_capabilities = array_append(protocol_capabilities, $2) WHERE id = $1", f.o.workerID, capability.CodexCredentialSwitchV1)
			run, _ := f.run(t, "running")
			old := f.env.seedStaticAPIKey(t, f.o.userID, "old-api", "old-test-key")
			target := f.env.seedStaticAPIKey(t, f.o.userID, "new-api", "new-test-key")
			if scenario == "subscription" {
				target = f.env.seedLinkedSubscription(t, f.o.userID, "subscription-target", codexToken("target-access"), codexToken("target-refresh"))
			}
			if err := f.svc.FreezeCodexBinding(f.env.ctx, f.o.userID, run, old, codexAuthModeAPIKey); err != nil {
				t.Fatal(err)
			}
			if scenario != "missing baseline" {
				if err := f.svc.recordCodexEpoch(context.Background(), mustRun(t, f.env, run)); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.svc.SetRunCredential(f.env.ctx, f.o.userID, run, BindModePinned, &target); err != nil {
				t.Fatal(err)
			}
			op := f.opOf(t, run)
			req := CodexSwitchPreflightRequest{CredentialSwitchApplyRequest: f.req(op), TargetSecretID: target}
			want, reason := "ready", ""
			switch scenario {
			case "locked vault":
				vlt := vault.NewPostgres(f.env.box, f.env.pool)
				f.svc.SetVault(vlt)
				enrollCodexProtectedVault(t, f.env, vlt, f.o.userID, codexVaultLockedTestPassword)
				sealed, err := vlt.Seal(f.env.ctx, f.o.userID, store.KindOpenAIAPIKey, []byte("locked-target-key"), vault.SealForUse)
				if err != nil {
					t.Fatal(err)
				}
				f.env.exec("UPDATE user_secrets SET ciphertext=$2, sealed_with=$3 WHERE id=$1", target, sealed.Ciphertext, sealed.SealedWith)
				vlt.Lock(f.o.userID)
				want, reason = ApplyOutcomeNotApplied, applyReasonVaultLocked
			case "missing baseline":
				want, reason = ApplyOutcomeNotApplied, "baseline_missing"
			case "disabled target":
				f.env.exec("UPDATE user_secrets SET disabled_at = now() WHERE id = $1", target)
				want, reason = ApplyOutcomeNotApplied, applyReasonCredentialDisabled
			case "foreign target":
				req.TargetSecretID = uuid.New()
				want, reason = ApplyOutcomeNotApplied, "target_mismatch"
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
			case "alias locked":
				tx, err := f.env.pool.Begin(f.env.ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = tx.Rollback(context.Background()) }()
				if _, err := tx.Exec(f.env.ctx, "SELECT user_secret_id FROM codex_credential_state WHERE user_secret_id = $1 FOR UPDATE", target); err != nil {
					t.Fatal(err)
				}
				want, reason = ApplyOutcomeNotApplied, applyReasonCredentialBusy
			case "empty material":
				sealed, err := f.env.box.Seal([]byte("   "))
				if err != nil {
					t.Fatal(err)
				}
				f.env.exec("UPDATE user_secrets SET ciphertext = $2 WHERE id = $1", target, sealed)
				want, reason = ApplyOutcomeNotApplied, applyReasonCredentialUnavail
			case "wrong target":
				req.TargetSecretID = old
				want, reason = ApplyOutcomeNotApplied, "target_mismatch"
			case "pending outcome":
				f.env.exec("UPDATE runs SET policy_outcome_id = $2, policy_preserve_required = true, policy_continuation_mode = 'same_conversation', policy_applying_worker_id = worker_id, policy_applying_generation = claim_generation, policy_source_generation = claim_generation WHERE id = $1", run, uuid.New())
				want, reason = ApplyOutcomeNotApplied, "policy_refusal"
			case "policy hold":
				f.env.exec("UPDATE runs SET status = 'paused', hold_reason = 'codex_policy_hold', policy_outcome_id = $2, policy_preserve_required = true, policy_continuation_mode = 'same_conversation', policy_applying_worker_id = worker_id, policy_applying_generation = claim_generation, policy_source_generation = claim_generation WHERE id = $1", run, uuid.New())
				want = ApplyOutcomeFenced
			case "nullable snapshots":
				f.env.exec("UPDATE run_credential_epochs SET codex_auth_mode = NULL, codex_material_revision = NULL WHERE run_id = $1", run)
			case "corrupt material":
				f.env.exec("UPDATE user_secrets SET ciphertext = $2 WHERE id = $1", target, []byte("invalid ciphertext"))
				want, reason = ApplyOutcomeNotApplied, applyReasonCredentialUnavail
			case "missing negotiation":
				f.env.exec("UPDATE workers SET protocol_capabilities = array_remove(protocol_capabilities, $2) WHERE id = $1", f.o.workerID, capability.CodexCredentialSwitchV1)
				want = ApplyOutcomeFenced
			case "cleared target":
				f.restamp(t, run)
				want = ApplyOutcomeSuperseded
			case "policy refusal":
				f.env.exec("UPDATE runs SET policy_pending_fresh = true WHERE id = $1", run)
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
			if got.ClaimGeneration != f.gen || got.RegisterNonce != applyFixtureNonce || got.OpID != f.opOf(t, run).String() {
				t.Fatalf("missing authoritative fence: %+v", got)
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
	if got.Outcome != ApplyOutcomeFenced || got.Token != "" || got.Codex != nil {
		t.Fatalf("Claude-only worker was not fenced from Codex apply: %v", got)
	}
	if after := f.snap(t, run); after != before {
		t.Fatal("unsupported Codex apply changed state")
	}
}

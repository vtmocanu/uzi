package workersvc

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

// Match runs_policy_hold_shape (00397): a fresh decision and a held decision
// both require the complete outcome/continuation/claim tuple, not a lone flag.
func setCodexSwitchPolicyEpisode(f applyFx, run uuid.UUID, fresh, held bool) {
	f.env.exec("UPDATE runs SET policy_outcome_id=$2, policy_preserve_required=true, policy_continuation_mode='same_conversation', policy_applying_worker_id=worker_id, policy_applying_generation=claim_generation, policy_source_generation=claim_generation, policy_pending_fresh=$3, status=CASE WHEN $4 THEN 'paused' ELSE status END, hold_reason=CASE WHEN $4 THEN 'codex_policy_hold' ELSE hold_reason END WHERE id=$1", run, uuid.New(), fresh, held)
}

// Exercise the public owner request, not just its policy predicate. An existing
// pending operation must survive a refused replacement unchanged.
func TestOwnerCodexUnresolvedPolicyRefusesReplacementLiveDB(t *testing.T) {
	for _, episode := range []string{"fresh pending", "unresolved outcome", "policy hold"} {
		t.Run(episode, func(t *testing.T) {
			f, run, _ := codexApplyFixture(t, false)
			next := f.env.seedStaticAPIKey(t, f.o.userID, "policy-next", "policy-next-key")
			setCodexSwitchPolicyEpisode(f, run, episode == "fresh pending", episode == "policy hold")
			f.noWrite(t, run, func() {
				_, err := f.svc.SetRunCredential(f.env.ctx, f.o.userID, run, BindModePinned, &next)
				if episode == "policy hold" {
					if !errors.Is(err, ErrCodexSwitchIneligible) {
						t.Fatal("parked policy hold was not refused as ineligible", err)
					}
				} else {
					var held *RunOwnerHeldError
					if !errors.As(err, &held) {
						t.Fatal("unresolved policy episode was not owner-held", err)
					}
				}
			})
		})
	}
}

// Prepare the pending snapshot through the locked resolver so public owner-route
// refusals are exercised in the assertion rather than during fixture setup.
func TestOwnerCodexPolicyRefusalFromPreparedBindingLiveDB(t *testing.T) {
	f, run, target, _ := codexUnrequestedFixture(t, false, false, false)
	if _, err := f.svc.requestCodexCredentialSwitch(f.env.ctx, mustRun(t, f.env, run), BindModePinned, &target); err != nil {
		t.Fatal(err)
	}
	setCodexSwitchPolicyEpisode(f, run, false, false)
	next := f.env.seedStaticAPIKey(t, f.o.userID, "prepared-policy-next", "prepared-policy-next-key")
	f.noWrite(t, run, func() {
		_, err := f.svc.SetRunCredential(f.env.ctx, f.o.userID, run, BindModePinned, &next)
		var held *RunOwnerHeldError
		if !errors.As(err, &held) {
			t.Fatal("public policy replacement was not owner-held", err)
		}
	})
}

// A ready preflight is advisory: a policy episode can start after it and must
// still prevent the locked apply from changing the binding, capability or journal.
func TestCodexPolicyEpisodeBetweenPreflightAndApplyLiveDB(t *testing.T) {
	for _, episode := range []string{"fresh pending", "unresolved outcome", "policy hold"} {
		t.Run(episode, func(t *testing.T) {
			f, run, target := codexApplyFixture(t, false)
			request := f.req(f.opOf(t, run))
			preflight, err := f.svc.PreflightCodexCredentialSwitch(f.env.ctx, f.worker(t), run, CodexSwitchPreflightRequest{
				CredentialSwitchApplyRequest: request, TargetSecretID: target,
			})
			if err != nil || preflight.Outcome != "ready" {
				t.Fatal("preflight not ready", preflight.Outcome, err)
			}
			want := ApplyOutcomeNotApplied
			setCodexSwitchPolicyEpisode(f, run, episode == "fresh pending", episode == "policy hold")
			if episode == "policy hold" {
				want = ApplyOutcomeFenced
			}
			f.noWrite(t, run, func() {
				result := f.apply(t, run, request)
				if result.Outcome != want || (want == ApplyOutcomeNotApplied && result.Reason != "policy_refusal") ||
					result.Codex != nil || result.Token != "" {
					t.Fatal("policy episode did not fence apply without material", result.Outcome, result.Reason)
				}
			})
		})
	}
}

func TestOwnerCodexWorkerHeldGateRequestLiveDB(t *testing.T) {
	for _, status := range []string{"awaiting_approval", "awaiting_input", "awaiting_followup"} {
		t.Run(status, func(t *testing.T) {
			f, run, _ := codexApplyFixture(t, false)
			before := mustRun(t, f.env, run)
			next := f.env.seedStaticAPIKey(t, f.o.userID, "gate-target", "gate-target-key")
			f.env.exec("UPDATE runs SET status=$2 WHERE id=$1", run, status)
			result, err := f.svc.SetRunCredential(f.env.ctx, f.o.userID, run, BindModePinned, &next)
			if err != nil {
				t.Fatal("worker-held gate refused pending choice", err)
			}
			after := result.Run
			if after.Status != status || after.ClaimGeneration != before.ClaimGeneration ||
				after.CodexSecretID != before.CodexSecretID || after.CodexClaimEpoch != before.CodexClaimEpoch ||
				!after.CodexSwitchTargetSecretID.Valid || uuid.UUID(after.CodexSwitchTargetSecretID.Bytes) != next ||
				after.CredentialSwitchOpID == before.CredentialSwitchOpID {
				t.Fatal("gate request changed committed authority or did not stamp a new target")
			}
		})
	}
}

func TestOwnerCodexForeignRunOrCredentialRefusedLiveDB(t *testing.T) {
	f, run, target := codexApplyFixture(t, false)
	other := f.env.seedUser(t)
	foreign := f.env.seedStaticAPIKey(t, other, "foreign-codex", "foreign-test-key")
	f.noWrite(t, run, func() {
		_, err := f.svc.SetRunCredential(f.env.ctx, other, run, BindModePinned, &target)
		if !errors.Is(err, ErrRunNotFound) {
			t.Fatal("foreign owner accessed run", err)
		}
	})
	f.noWrite(t, run, func() {
		_, err := f.svc.SetRunCredential(f.env.ctx, f.o.userID, run, BindModePinned, &foreign)
		if !errors.Is(err, ErrCredentialOverrideSecretNotFound) {
			t.Fatal("foreign credential accepted", err)
		}
	})
}

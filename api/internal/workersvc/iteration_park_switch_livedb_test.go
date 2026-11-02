package workersvc

import (
	"reflect"
	"testing"

	"github.com/google/uuid"
)

// The held switch verb is Anthropic-only. Codex retains its existing unsupported
// override response; iteration parking grants it no new credential-switch route.
func TestIterationParkPendingSwitchResumeClaimLiveDB(t *testing.T) {
	f := newHoldFx(t)
	f.svc.SetVault(f.env.vlt)
	f.env.sealBotPAT(t, f.o.userID)
	id := f.run(t)
	f.env.exec(`UPDATE runs SET budget_max_iterations=5, budget_wall_seconds=7200,
 credential_switch_requested_at=NULL, credential_switch_generation=NULL WHERE id=$1`, id)
	if _, err := f.svc.SetRunCredential(f.env.ctx, f.o.userID, id, CredentialOverrideModePinned, &f.o.altTok); err != nil {
		t.Fatalf("stamp selected credential: %v", err)
	}
	stamped := mustRun(t, f.env, id)
	if !stamped.CredentialSwitchRequestedAt.Valid || stamped.CredentialSwitchGeneration.Int64 != f.gen {
		t.Fatal("owner switch did not stamp the current generation")
	}
	evidence := IterationParkEvidence{ClaimGeneration: f.gen, IterationCount: 5, MaxIterations: 5}
	parked, applied, reason, err := f.svc.ReportIterationPark(f.env.ctx, f.worker(t, f.o.workerID), id, evidence)
	if err != nil || !applied || reason != "" || parked.Status != "paused" || parked.HoldReason.String != IterationBudgetExhausted {
		t.Fatalf("park: applied=%v reason=%q status=%q err=%v", applied, reason, parked.Status, err)
	}
	if parked.CredentialSwitchRequestedAt != stamped.CredentialSwitchRequestedAt ||
		parked.CredentialSwitchGeneration != stamped.CredentialSwitchGeneration ||
		parked.CredentialOverrideSecretID != stamped.CredentialOverrideSecretID {
		t.Fatal("park applied or lost the pending credential choice")
	}
	replayed, applied, reason, err := f.svc.ReportIterationPark(f.env.ctx, f.worker(t, f.o.workerID), id, evidence)
	if err != nil || !applied || reason != "" || !reflect.DeepEqual(parked, replayed) {
		t.Fatalf("park replay changed state: applied=%v reason=%q err=%v", applied, reason, err)
	}
	if err := f.ownerResume(id); err != nil {
		t.Fatalf("owner Resume: %v", err)
	}
	resumed := mustRun(t, f.env, id)
	if resumed.Status != "queued" || resumed.CredentialSwitchRequestedAt != stamped.CredentialSwitchRequestedAt {
		t.Fatal("Resume lost the unapplied switch")
	}
	payload, err := f.svc.Claim(f.env.ctx, f.worker(t, f.o.workerID), nil)
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if payload == nil || payload.RunID != id.String() {
		t.Fatal("reclaim did not return the resumed run")
	}
	claimed := mustRun(t, f.env, id)
	if claimed.ClaimGeneration <= f.gen || !claimed.AnthropicSecretID.Valid ||
		uuid.UUID(claimed.AnthropicSecretID.Bytes) != f.o.altTok || claimed.CredentialSwitchRequestedAt.Valid ||
		claimed.CredentialSwitchGeneration.Valid || !claimed.CredentialOverrideSecretID.Valid ||
		uuid.UUID(claimed.CredentialOverrideSecretID.Bytes) != f.o.altTok {
		t.Fatal("reclaim did not apply the selected credential and settle its switch stamp")
	}
	_, applied, reason, err = f.svc.ReportIterationPark(f.env.ctx, f.worker(t, f.o.workerID), id, evidence)
	if err != nil || applied || reason != "stale_claim" {
		t.Fatalf("old generation after reclaim: applied=%v reason=%q err=%v", applied, reason, err)
	}
}

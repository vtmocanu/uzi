package workersvc

import (
	"errors"
	"testing"
)

// Pins that ReleaseCodexCredential re-authorizes after reading the subscription
// material: a real switch apply that commits between the first authorize and the
// final reauthorization must refuse the release and return no token. It does not
// claim anything about releases that finish before the apply commits.
func TestCodexReleaseRecheckRefusesAfterSwitchApplyLiveDB(t *testing.T) {
	f, run, _, capabilityA := codexApplyFromSubscription(t)
	worker := f.worker(t)
	aliasBefore := mustRun(t, f.env, run).CodexSecretID

	// Positive control: before any apply, A's capability releases A's token.
	before, err := f.svc.ReleaseCodexCredential(f.env.ctx, worker, run, capabilityA)
	if err != nil || before.AccessToken == "" {
		t.Fatalf("pre-apply release with A's capability failed: err=%v", err)
	}

	fired := false
	f.svc.q = releaseHookStore{
		Queries: f.env.q,
		onAccountRead: func() {
			if fired {
				return
			}
			fired = true
			if got := f.apply(t, run, f.req(f.opOf(t, run))); got.Outcome != ApplyOutcomeApplied {
				t.Fatal("switch apply did not commit inside the release window", got)
			}
		},
	}
	res, err := f.svc.ReleaseCodexCredential(f.env.ctx, worker, run, capabilityA)
	if !fired {
		t.Fatal("hook did not fire: the subscription account read was not reached")
	}
	if res.AccessToken != "" {
		t.Fatal("release returned A's token after a switch apply committed before the final reauthorization")
	}
	if err == nil {
		t.Fatal("release succeeded after a switch apply committed before the final reauthorization")
	}
	// A committed apply re-mints the capability and bumps the claim epoch, so the
	// stale A capability surfaces as ErrCodexCapabilityEpoch (not a hash mismatch).
	if !errors.Is(err, ErrCodexCapabilityEpoch) {
		t.Fatalf("recheck error is not the stale-capability authorization error: %v", err)
	}

	// Without the hook, A's capability is refused at the first authorize.
	f.svc.q = f.env.q
	res, err = f.svc.ReleaseCodexCredential(f.env.ctx, worker, run, capabilityA)
	if err == nil || res.AccessToken != "" {
		t.Fatal("A's capability still releases after the apply committed")
	}
	if !errors.Is(err, ErrCodexCapabilityEpoch) {
		t.Fatalf("first-authorize refusal differs from the recheck refusal: %v", err)
	}
	after := mustRun(t, f.env, run)
	if after.CodexSecretID == aliasBefore || after.CredentialSwitchRequestedAt.Valid {
		t.Fatal("run is not bound to the switch target after the apply")
	}
}

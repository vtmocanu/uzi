package workersvc

import (
	"strings"
	"testing"
)

// TestOversizeRunClaimFinishesMintedCodexRunLiveDB: an ordinary Codex issue run whose real
// assembled claim exceeds the (lowered) limit goes through assembleAndFinishRunClaim and fails
// terminally as provisioning_failed with a typed reason. The minted capability's identity rides
// the refusal, so the exact-claim finish settles it (revoked, epoch bumped) rather than going
// idle on a superseded mint and leaving the run claimed.
func TestOversizeRunClaimFinishesMintedCodexRunLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	f := newSubscriptionFixture(t, env)
	resealBotPAT(t, env, f.userID)
	svc := New(env.q, env.box, testParams())
	svc.SetTxBeginner(env.pool)
	svc.claimSizeLimit = 1024

	before := mustRun(t, env, f.runID)
	payload, err := svc.assembleAndFinishRunClaim(env.ctx, f.wkr, before, false)
	if payload != nil || err != nil {
		t.Fatalf("assembleAndFinishRunClaim = (%v, %v), want (nil, nil)", payload, err)
	}
	after := mustRun(t, env, f.runID)
	if after.Status != "failed" || after.FailOrigin.String != "provisioning_failed" {
		t.Fatalf("run left status=%s origin=%q reason=%q", after.Status, after.FailOrigin.String, after.FailureReason.String)
	}
	if !strings.HasPrefix(after.FailureReason.String, "claim too large:") {
		t.Fatalf("failure_reason = %q", after.FailureReason.String)
	}
	if after.CodexClaimEpoch <= before.CodexClaimEpoch || len(after.CodexCapHash) != 0 {
		t.Fatalf("capability not revoked: hash=%x epoch=%d (before %d)", after.CodexCapHash, after.CodexClaimEpoch, before.CodexClaimEpoch)
	}
}

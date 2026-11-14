package workersvc

import (
	"github.com/google/uuid"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/codexauth"
)

func TestCodexRefreshAdmittedBeforeApplyLiveDB(t *testing.T) {
	f, run, account, capability := codexApplyFromSubscription(t)
	acct := f.env.mustAccount(t, f.o.userID, account)
	checked := false
	refreshToken := codexToken("admitted-refresh")
	fake := &fakeRefreshClient{result: codexauth.RefreshResult{AccessToken: codexToken("admitted-new"), RefreshToken: &refreshToken}}
	setFakeMatchIdentity(fake, codexauth.Identity{ProviderUserID: acct.ProviderUserID, WorkspaceAccountID: acct.WorkspaceAccountID})
	fake.onRefresh = func() {
		checked = true
		before := mustRun(t, f.env, run)
		f.noWrite(t, run, func() {
			got := f.apply(t, run, f.req(f.opOf(t, run)))
			if got.Outcome != ApplyOutcomeNotApplied || got.Reason != applyReasonCredentialBusy {
				t.Fatal("admitted refresh did not block switch", got)
			}
		})
		after := mustRun(t, f.env, run)
		if after.CodexClaimEpoch != before.CodexClaimEpoch || string(after.CodexCapHash) != string(before.CodexCapHash) {
			t.Fatal("apply revoked admitted refresh")
		}
	}
	f.svc.codexRefresh = fake
	res, err := f.svc.CoordinatedCodexRefresh(f.env.ctx, f.worker(t), run, capability, uuid.New(), 0)
	if err != nil || !checked || fake.calls != 1 || res.AccessToken != codexToken("admitted-new") {
		t.Fatalf("admitted refresh failed: checked=%t calls=%d err=%v", checked, fake.calls, err)
	}
}

func TestCodexApplyAcceptsIdleSameAuthorityNewGenerationLiveDB(t *testing.T) {
	f, run, target := codexApplyFixture(t, true)
	// Token generations are not authority revisions. An idle, fully reconciled
	// refresh can advance generation while preserving the request's authority.
	f.env.exec("UPDATE codex_provider_account SET generation=9 WHERE id=(SELECT provider_account_id FROM codex_credential_state WHERE user_secret_id=$1)", target)
	got := f.apply(t, run, f.req(f.opOf(t, run)))
	if got.Outcome != ApplyOutcomeApplied || got.Codex == nil || got.Codex.Generation == nil || *got.Codex.Generation != 9 {
		t.Fatalf("generation confused with authority revision: %v", got)
	}
}

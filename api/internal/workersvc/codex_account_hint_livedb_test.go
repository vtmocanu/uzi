package workersvc

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// A1b exercises the real early stale-material refusal with the re-login link cleared.
func TestCodexAccountHintRealStaleMaterialLiveDB(t *testing.T) {
	for _, control := range []string{"hold", "old-worker", "hash", "epoch", "worker", "released"} {
		t.Run(control, func(t *testing.T) {
			env := setupCodexLiveDB(t)
			fake := &fakeRefreshClient{}
			f := newRefreshFixture(t, env, fake)
			env.exec("UPDATE runs SET status = 'running' WHERE id = $1", f.runID)
			cap := env.mintCap(t, f.runID, f.workerID)
			if _, err := f.svc.AuthorizeCodexCredentialOp(env.ctx, f.wkr, f.runID, cap, ScopeStartRefresh); err != nil {
				t.Fatal(err)
			}
			n, err := env.q.BumpCodexMaterialRevision(env.ctx, store.BumpCodexMaterialRevisionParams{UserSecretID: f.aliasID, UserID: f.userID, Status: "staging"})
			if n != 1 || err != nil {
				t.Fatalf("PATCH: %d %v", n, err)
			}
			st, err := env.q.GetCodexCredentialState(env.ctx, store.GetCodexCredentialStateParams{UserSecretID: f.aliasID, UserID: f.userID})
			if err != nil || st.ProviderAccountID.Valid {
				t.Fatalf("re-login link not cleared: %v %v", st.ProviderAccountID, err)
			}
			f.wkr.ProtocolCapabilities = []string{capability.CodexAccountParkV1}
			switch control {
			case "old-worker":
				f.wkr.ProtocolCapabilities = nil
			case "hash":
				cap += "-wrong"
			case "epoch":
				env.exec("UPDATE runs SET codex_claim_epoch = codex_claim_epoch + 1 WHERE id = $1", f.runID)
			case "worker":
				f.wkr.ID = uuid.New()
			case "released":
				env.exec("UPDATE runs SET claim_released_at = now() WHERE id = $1", f.runID)
			}
			res, err := f.svc.CoordinatedCodexRefresh(env.ctx, f.wkr, f.runID, cap, uuid.New(), 0)
			hold, computed := CodexAccountHoldVerdict(err)
			wantComputed := control == "hold" || control == "old-worker"
			if computed != wantComputed || hold != wantComputed || errors.Is(err, ErrCodexAccountUnavailable) != (control == "hold") {
				t.Fatalf("err=%v hold=%v computed=%v", err, hold, computed)
			}
			if wantComputed && !errors.Is(err, ErrCodexMaterialRevisionStale) {
				t.Fatalf("authorization weakened: %v", err)
			}
			if fake.calls != 0 || fake.discoverCalls != 0 || res.AccessToken != "" {
				t.Fatalf("provider called/token released: %d/%d", fake.calls, fake.discoverCalls)
			}
		})
	}
}

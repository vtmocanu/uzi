package workersvc

import (
	"net/http"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/codexauth"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestCodexLinkedProbeAuthorityLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name, coord, wantStatus, wantReason string
		renewable, moveGeneration           bool
		result                              usageResult
		wantCalls                           int
	}{
		{name: "renewable access rejected", renewable: true, result: usageResult{err: usageAuthError(http.StatusUnauthorized, 0)}, wantStatus: "inconclusive", wantReason: "generic", wantCalls: 1},
		{name: "unrenewable access rejected", result: usageResult{err: usageAuthError(http.StatusUnauthorized, 0)}, wantStatus: "rejected", wantCalls: 1},
		{name: "forbidden", renewable: true, result: usageResult{err: usageAuthError(http.StatusForbidden, 0)}, wantStatus: "inconclusive", wantReason: "generic", wantCalls: 1},
		{name: "quarantined", renewable: true, coord: codexCoordQuarantined, wantStatus: "inconclusive", wantReason: "generic"},
		{name: "refresh in progress", renewable: true, coord: codexCoordInProgress, wantStatus: "inconclusive", wantReason: "generic"},
		{name: "authority moved during read", renewable: true, moveGeneration: true, result: usageResult{reading: codexauth.UsageReading{}}, wantStatus: "inconclusive", wantReason: "superseded", wantCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := setupCodexLiveDB(t)
			fake := &fakeUsageClient{usageResults: []usageResult{tc.result}}
			fx := newUsageFixture(t, env, fake, tc.renewable)
			if tc.coord != "" {
				env.exec(`UPDATE codex_provider_account SET coord_state = $2 WHERE id = $1`, fx.accountID, tc.coord)
			}
			if tc.moveGeneration {
				fake.beforeReturn = func(int) {
					env.exec(`UPDATE codex_provider_account SET generation = generation + 1 WHERE id = $1`, fx.accountID)
				}
			}
			status, reason := fx.svc.TestCodexLinkedAccount(env.ctx, fx.userID, fx.accountID, fake)
			if status != tc.wantStatus || reason != tc.wantReason {
				t.Fatalf("result = %q/%q, want %q/%q", status, reason, tc.wantStatus, tc.wantReason)
			}
			if fake.usageCalls != tc.wantCalls || fake.refreshCalls != 0 {
				t.Fatalf("provider calls: usage=%d refresh=%d", fake.usageCalls, fake.refreshCalls)
			}
			acct, err := env.q.GetCodexProviderAccountByID(env.ctx, store.GetCodexProviderAccountByIDParams{UserID: fx.userID, ID: fx.accountID})
			if err != nil || acct.ReauthRequired {
				t.Fatalf("read-only test changed reauth: %v, err=%v", acct.ReauthRequired, err)
			}
		})
	}
}

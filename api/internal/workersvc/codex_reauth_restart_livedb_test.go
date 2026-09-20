package workersvc

import (
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/codexauth"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1209 M4, DELIVERABLE C: the zero-worker reauth-restart REGRESSION, end to end. Begin
// at canonical generation 0; drive CollectCodexAccountUsage down the proven-expired-with-no-
// refresh-token path so MarkCodexReauthRequired fires (reauth set, generation NOT advanced).
// SIMULATE AN API RESTART — a fresh store handle + Service against the SAME database, all
// in-memory state discarded — and prove the flag SURVIVED. Then save a VERIFIED same-account
// replacement through the REAL reconciler path (RefreshCodexAccountLogin's reauth arm, which
// now handles quarantined OR reauth_required), and prove reauth clears atomically with the
// generation advance and collection succeeds again. Finally, a TRANSIENT failure on the
// recovered account must NOT re-raise reauth.
//
// The per-milestone tests cover the pieces (M2 sets reauth on a no-refresh-token 401; the
// reconcile widening clears it) but never tie them across a restart with a fresh Service, and
// never prove the flag is DURABLE across one — which is this deliverable's whole point.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.

func TestCodexReauthSurvivesRestartThenReplacementClearsLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	// A linked account at generation 0 whose login carries NO refresh token — the material
	// that makes a 401 a PROVEN expiry (ErrCodexRefreshNoToken) rather than a rotatable one.
	fake := &fakeUsageClient{}
	fx := newUsageFixture(t, env, fake, false)

	// (1) Drive the proven-expired path: a 401 on a login with no renewal material flags
	// reauth_required and does NOT advance the generation.
	fake.usageResults = []usageResult{{err: usageAuthError(http.StatusUnauthorized, 0)}}
	_, err := fx.svc.CollectCodexAccountUsage(env.ctx, fx.userID, fx.accountID)
	if got := mustUsageFailure(t, err).Kind; got != CodexUsageFailReauthRequired {
		t.Fatalf("kind = %v, want reauth_required", got)
	}
	acct := env.mustAccount(t, fx.userID, fx.accountID)
	if !acct.ReauthRequired {
		t.Fatal("a proven no-renewal expiry must set reauth_required")
	}
	if acct.Generation != 0 {
		t.Fatalf("generation = %d, want 0 (a failed no-token rotation must not advance)", acct.Generation)
	}

	// (2) SIMULATE AN API RESTART: a fresh pool + store handle + Service (and a fresh master
	// box under the SAME key) against the SAME database — every in-memory poll/backoff/vault
	// state from before the restart is gone.
	dsn := os.Getenv("UZI_TEST_DATABASE_URL") // non-empty: setupCodexLiveDB skipped otherwise.
	freshPool, err := store.OpenPool(env.ctx, dsn)
	if err != nil {
		t.Fatalf("reopen pool (simulated restart): %v", err)
	}
	t.Cleanup(freshPool.Close)
	freshBox, err := secretbox.New([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("fresh secretbox: %v", err)
	}
	freshQ := store.New(freshPool)

	// The flag SURVIVED the restart: a fresh read of the account still shows reauth_required.
	restarted, err := freshQ.GetCodexProviderAccountByID(env.ctx, store.GetCodexProviderAccountByIDParams{UserID: fx.userID, ID: fx.accountID})
	if err != nil {
		t.Fatalf("read account after restart: %v", err)
	}
	if !restarted.ReauthRequired {
		t.Fatal("reauth_required did not survive the simulated API restart")
	}
	if restarted.Generation != 0 {
		t.Fatalf("post-restart generation = %d, want 0", restarted.Generation)
	}

	// A collect on the restarted Service, while still flagged, is disabled (not a poll target)
	// — the flag stops collection until a verified replacement lands.
	fake2 := &fakeUsageClient{}
	freshSvc := &Service{q: freshQ, box: freshBox, codexRefresh: fake2}
	if _, cerr := freshSvc.CollectCodexAccountUsage(env.ctx, fx.userID, fx.accountID); mustUsageFailure(t, cerr).Kind != CodexUsageFailDisabled {
		t.Fatalf("a reauth-flagged account must be disabled for polling, got %v", cerr)
	}

	// (3) Save a VERIFIED same-account replacement through the REAL path: re-import the SAME
	// identity under a new alias and reconcile it with a FRESH reconciler. The reconciler's
	// restore branch (quarantined OR reauth_required) fires RefreshCodexAccountLogin's reauth
	// arm — installing the login, advancing the generation, clearing the flag, linking the
	// alias, all in one transaction.
	newAccess := codexToken("access-reimport")
	newAlias := env.seedStagingAlias(t, fx.userID, "codex-restart-reimport-"+uuid.NewString(),
		codexLoginBlob{AccessToken: newAccess, RefreshToken: codexToken("refresh-reimport")})
	fid := newFakeCodexIdentity()
	fid.idByToken[newAccess] = codexauth.Identity{ProviderUserID: fx.providerUserID, WorkspaceAccountID: fx.workspaceID}
	r := NewCodexReconciler(freshQ, nil, freshBox, fid, freshPool)
	if rerr := r.ReconcileCodexAuthIdentity(env.ctx, fx.userID, newAlias); rerr != nil {
		t.Fatalf("reconcile verified replacement: %v", rerr)
	}

	recovered, err := freshQ.GetCodexProviderAccountByID(env.ctx, store.GetCodexProviderAccountByIDParams{UserID: fx.userID, ID: fx.accountID})
	if err != nil {
		t.Fatalf("read account after replacement: %v", err)
	}
	if recovered.ReauthRequired {
		t.Fatal("a verified same-account replacement must clear reauth_required")
	}
	if recovered.ReauthGeneration.Valid || recovered.ReauthCredentialRevision.Valid {
		t.Fatalf("reauth observation counters must be cleared atomically: gen=%v rev=%v", recovered.ReauthGeneration, recovered.ReauthCredentialRevision)
	}
	if recovered.Generation != 1 {
		t.Fatalf("generation = %d, want 1 (the verified restore advanced it)", recovered.Generation)
	}
	st, err := freshQ.GetCodexCredentialState(env.ctx, store.GetCodexCredentialStateParams{UserSecretID: newAlias, UserID: fx.userID})
	if err != nil {
		t.Fatalf("read replacement alias state: %v", err)
	}
	if st.Status != codexStatusLinked || !st.ProviderAccountID.Valid || uuid.UUID(st.ProviderAccountID.Bytes) != fx.accountID {
		t.Fatalf("replacement alias not linked to the account: status=%q account=%v", st.Status, st.ProviderAccountID)
	}

	// (4) Collection succeeds again on the recovered account, and the reading is persisted.
	fake2.usageResults = []usageResult{
		{reading: codexauth.UsageReading{UserID: fx.providerUserID, Buckets: []codexauth.UsageBucket{{ID: "codex"}}}},
		{err: usageAuthError(http.StatusTooManyRequests, 30*time.Second)},
	}
	reading, err := freshSvc.CollectCodexAccountUsage(env.ctx, fx.userID, fx.accountID)
	if err != nil {
		t.Fatalf("collect after recovery: %v", err)
	}
	if reading.ObservedGeneration != 1 {
		t.Fatalf("post-recovery reading observed generation = %d, want 1", reading.ObservedGeneration)
	}
	if len(reading.Buckets) != 1 || reading.Buckets[0].ID != "codex" {
		t.Fatalf("post-recovery buckets = %+v", reading.Buckets)
	}
	n, err := freshQ.UpsertCodexAccountRateLimits(env.ctx, store.UpsertCodexAccountRateLimitsParams{
		UserID:                     fx.userID,
		ProviderAccountID:          fx.accountID,
		Buckets:                    []byte(`[{"id":"codex","allowed":null,"limit_reached":null,"primary":null,"secondary":null}]`),
		ObservedGeneration:         reading.ObservedGeneration,
		ObservedCredentialRevision: reading.ObservedCredentialRevision,
		AttemptStatus:              "ok",
	})
	if err != nil || n != 1 {
		t.Fatalf("persist recovered reading: n=%d err=%v", n, err)
	}

	// (5) A TRANSIENT failure (a 429) on the recovered, healthy account must NOT re-raise
	// reauth — reauth is reserved for a proven no-renewal expiry, never a transient fault.
	_, terr := freshSvc.CollectCodexAccountUsage(env.ctx, fx.userID, fx.accountID)
	if got := mustUsageFailure(t, terr).Kind; got != CodexUsageFailRateLimited {
		t.Fatalf("kind = %v, want rate_limited (a transient fault)", got)
	}
	afterTransient := env.mustAccount(t, fx.userID, fx.accountID)
	if afterTransient.ReauthRequired {
		t.Fatal("a transient (429) failure must never set reauth_required on a recovered account")
	}
	if afterTransient.Generation != 1 {
		t.Fatalf("generation = %d, want 1 (a transient must not advance)", afterTransient.Generation)
	}
}

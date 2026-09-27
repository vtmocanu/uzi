package workersvc

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/codexauth"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1732 M3b on the real schema: Codex liveness per account, the claim-fenced in-flight
// refresh exception (D3), durable completion of a refresh already in progress (D7), and the
// enabled-alias gate on background refresh and recovery (D6).
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.

// codexEnablementSig is the account's enablement list as the poke listing captures it (the
// value the poller's writes are fenced on), or "" when the account is not listed.
func codexEnablementSig(t *testing.T, env codexTestEnv, userID, accountID uuid.UUID) string {
	t.Helper()
	rows, err := env.q.ListLinkedCodexAccountsForUser(env.ctx, userID)
	if err != nil {
		t.Fatalf("ListLinkedCodexAccountsForUser: %v", err)
	}
	for _, r := range rows {
		if r.ProviderAccountID == accountID {
			return r.EnablementSig
		}
	}
	return ""
}

// setAliasEnabled runs the enablement transition the PATCH handler commits.
func setAliasEnabled(t *testing.T, env codexTestEnv, userID, aliasID uuid.UUID, enabled bool) {
	t.Helper()
	if _, err := env.q.SetSecretEnablement(env.ctx, store.SetSecretEnablementParams{ID: aliasID, UserID: userID, Enabled: enabled}); err != nil {
		t.Fatalf("set alias enabled=%v: %v", enabled, err)
	}
}

// accountPolled reports whether the poke listing returns the account (the poll worklist).
func accountPolled(t *testing.T, env codexTestEnv, userID, accountID uuid.UUID) bool {
	t.Helper()
	return codexEnablementSig(t, env, userID, accountID) != ""
}

// TestCodexNewAliasLinksWhileSiblingDisabledLiveDB: reconciliation is per alias (D6). With the
// account's only linked alias disabled, the account is not polled; a newly saved enabled
// alias for the same login is listed for reconciliation, links to that account, and the
// account is polled again, without re-enabling the disabled sibling.
func TestCodexNewAliasLinksWhileSiblingDisabledLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fake := &fakeUsageClient{}
	fx := newUsageFixture(t, env, fake, true)
	setAliasEnabled(t, env, fx.userID, fx.aliasID, false)
	if accountPolled(t, env, fx.userID, fx.accountID) {
		t.Fatal("an account whose only alias is disabled is still polled")
	}

	fresh := env.seedStagingAlias(t, fx.userID, "codex-new-"+uuid.NewString(), codexLoginBlob{AccessToken: fx.access, RefreshToken: codexToken("refresh")})
	staged, err := env.q.ListStagedCodexAliasesForUser(env.ctx, fx.userID)
	if err != nil {
		t.Fatal(err)
	}
	if len(staged) != 1 || staged[0].UserSecretID != fresh {
		t.Fatalf("staged worklist = %+v, want exactly the new enabled alias %s", staged, fresh)
	}

	fid := newFakeCodexIdentity()
	fid.idByToken[fx.access] = codexauth.Identity{ProviderUserID: fx.providerUserID, WorkspaceAccountID: fx.workspaceID}
	if err := NewCodexReconciler(env.q, nil, env.box, fid, env.pool).ReconcileCodexAuthIdentity(env.ctx, fx.userID, fresh); err != nil {
		t.Fatalf("reconcile the new alias: %v", err)
	}
	st, err := env.q.GetCodexCredentialState(env.ctx, store.GetCodexCredentialStateParams{UserSecretID: fresh, UserID: fx.userID})
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != codexStatusLinked || !st.ProviderAccountID.Valid || st.ProviderAccountID.Bytes != fx.accountID {
		t.Fatalf("new alias state = %q linked to %v, want linked to the sibling's account %s", st.Status, st.ProviderAccountID, fx.accountID)
	}
	if !accountPolled(t, env, fx.userID, fx.accountID) {
		t.Fatal("the account is not polled after an enabled alias linked to it")
	}
	sibling, err := env.q.GetSecretEnablement(env.ctx, store.GetSecretEnablementParams{ID: fx.aliasID, UserID: fx.userID})
	if err != nil || !sibling.DisabledAt.Valid {
		t.Fatalf("the disabled sibling changed: %+v err=%v", sibling, err)
	}
}

// TestCollectCodexUsageNeedsEnabledAliasLiveDB: the collector (the poll and its background
// refresh) reads an account with a disabled alias while an enabled sibling remains, and
// refuses one with no enabled alias before any provider call.
func TestCollectCodexUsageNeedsEnabledAliasLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fake := &fakeUsageClient{}
	fx := newUsageFixture(t, env, fake, true)
	sibling := env.seedStagingAlias(t, fx.userID, "codex-sibling-"+uuid.NewString(), codexLoginBlob{AccessToken: fx.access, RefreshToken: codexToken("refresh")})
	fid := newFakeCodexIdentity()
	fid.idByToken[fx.access] = codexauth.Identity{ProviderUserID: fx.providerUserID, WorkspaceAccountID: fx.workspaceID}
	if err := NewCodexReconciler(env.q, nil, env.box, fid, env.pool).ReconcileCodexAuthIdentity(env.ctx, fx.userID, sibling); err != nil {
		t.Fatalf("link sibling: %v", err)
	}
	reading := usageResult{reading: codexauth.UsageReading{UserID: fx.providerUserID, Buckets: []codexauth.UsageBucket{{ID: "codex"}}}}

	setAliasEnabled(t, env, fx.userID, fx.aliasID, false)
	fake.usageResults = []usageResult{reading}
	if _, err := fx.svc.CollectCodexAccountUsage(env.ctx, fx.userID, fx.accountID); err != nil {
		t.Fatalf("collect with an enabled sibling: %v", err)
	}
	if !accountPolled(t, env, fx.userID, fx.accountID) {
		t.Fatal("an account with an enabled sibling left the poll worklist")
	}

	setAliasEnabled(t, env, fx.userID, sibling, false)
	calls := fake.usageCalls
	_, err := fx.svc.CollectCodexAccountUsage(env.ctx, fx.userID, fx.accountID)
	if got := mustUsageFailure(t, err).Kind; got != CodexUsageFailNotLinked {
		t.Fatalf("kind = %v, want not_linked for an account with no enabled alias", got)
	}
	if fake.usageCalls != calls || fake.refreshCalls != 0 {
		t.Fatalf("provider calls with no enabled alias: usage %d->%d refresh %d, want none", calls, fake.usageCalls, fake.refreshCalls)
	}
	if accountPolled(t, env, fx.userID, fx.accountID) {
		t.Fatal("an account with no enabled alias is still on the poll worklist")
	}
}

// TestCollectCodexUsageDisableBeforeRotationSpendsNoRefreshLiveDB: a background poll whose
// first read gets a 401 re-proves the account is live before it rotates. When the last
// enabled alias was disabled during that read, no refresh token is spent and the account is
// not flagged for a new login.
func TestCollectCodexUsageDisableBeforeRotationSpendsNoRefreshLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fake := &fakeUsageClient{}
	fx := newUsageFixture(t, env, fake, true)
	fake.usageResults = []usageResult{{err: usageAuthError(http.StatusUnauthorized, 0)}}
	fake.beforeReturn = func(call int) {
		if call == 0 {
			setAliasEnabled(t, env, fx.userID, fx.aliasID, false)
		}
	}

	_, err := fx.svc.CollectCodexAccountUsage(env.ctx, fx.userID, fx.accountID)
	if got := mustUsageFailure(t, err).Kind; got != CodexUsageFailNotLinked {
		t.Fatalf("kind = %v, want not_linked (the account went dark before the rotation)", got)
	}
	if fake.refreshCalls != 0 {
		t.Fatalf("refresh calls = %d, want 0: a disabled account must not start a background refresh", fake.refreshCalls)
	}
	acct := env.mustAccount(t, fx.userID, fx.accountID)
	if acct.Generation != 0 || acct.ReauthRequired || acct.CoordState != "idle" {
		t.Fatalf("account = gen %d reauth %v state %q, want untouched (0, false, idle)", acct.Generation, acct.ReauthRequired, acct.CoordState)
	}
}

// refreshHookUsageClient runs onRefresh inside the provider refresh exchange, after the
// single-use refresh token was handed over, so a test can land a transition mid-exchange.
type refreshHookUsageClient struct {
	*fakeUsageClient
	onRefresh func()
}

func (c *refreshHookUsageClient) Refresh(ctx context.Context, token string) (codexauth.RefreshResult, error) {
	if c.onRefresh != nil {
		c.onRefresh()
	}
	return c.fakeUsageClient.Refresh(ctx, token)
}

// TestCodexBackgroundRefreshInProgressCompletesAcrossDisableLiveDB (D7): a background
// rotation already in the provider exchange when the account's last alias is disabled
// completes and persists (the spent refresh token's successor is committed), and its reading
// is discarded: the account is no longer live, so the poll reports not_linked and the
// poller's enablement-fenced write would refuse it anyway.
func TestCodexBackgroundRefreshInProgressCompletesAcrossDisableLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	base := &fakeUsageClient{}
	fx := newUsageFixture(t, env, base, true)
	hooked := &refreshHookUsageClient{fakeUsageClient: base}
	fx.svc.codexRefresh = hooked
	started := codexEnablementSig(t, env, fx.userID, fx.accountID)
	rotatedAccess, rotatedRefresh := codexToken("access-rotated"), codexToken("refresh-rotated")
	base.refreshResult = codexauth.RefreshResult{
		AccessToken:    rotatedAccess,
		RefreshToken:   &rotatedRefresh,
		IdentityClaims: freshClaimsForIdentity(codexauth.Identity{ProviderUserID: fx.providerUserID, WorkspaceAccountID: fx.workspaceID}),
	}
	base.usageResults = []usageResult{
		{err: usageAuthError(http.StatusUnauthorized, 0)},
		{reading: codexauth.UsageReading{UserID: fx.providerUserID, Buckets: []codexauth.UsageBucket{{ID: "codex"}}}},
	}
	hooked.onRefresh = func() { setAliasEnabled(t, env, fx.userID, fx.aliasID, false) }

	_, err := fx.svc.CollectCodexAccountUsage(env.ctx, fx.userID, fx.accountID)
	if got := mustUsageFailure(t, err).Kind; got != CodexUsageFailNotLinked {
		t.Fatalf("kind = %v, want not_linked (reading discarded after the disable)", got)
	}
	if base.refreshCalls != 1 {
		t.Fatalf("refresh calls = %d, want exactly 1", base.refreshCalls)
	}
	acct := env.mustAccount(t, fx.userID, fx.accountID)
	if acct.Generation != 1 || acct.CoordState != "idle" {
		t.Fatalf("account = gen %d state %q, want the rotation committed (1, idle)", acct.Generation, acct.CoordState)
	}
	if blob := env.accountBlob(t, fx.userID, fx.accountID); blob.AccessToken != rotatedAccess || blob.RefreshToken != rotatedRefresh {
		t.Fatal("the refreshed login was not persisted")
	}
	n, werr := env.q.UpsertCodexAccountRateLimits(env.ctx, store.UpsertCodexAccountRateLimitsParams{
		UserID: fx.userID, ProviderAccountID: fx.accountID, Buckets: []byte(`[]`),
		ObservedGeneration: 1, ObservedCredentialRevision: acct.CredentialRevision, AttemptStatus: "ok",
		EnablementSig: started,
	})
	if werr != nil || n != 0 {
		t.Fatalf("fenced write of the in-flight poll = (%d, %v), want (0, nil)", n, werr)
	}
}

// TestCodexFlightOnDisabledAliasRefreshesUntilClaimEndsLiveDB (D3): a worker flight holding
// the live claim on a run frozen to the account's last, now disabled, alias can still run
// the coordinated refresh; the exception is fenced by claim authority, so once the claim is
// superseded (a newer capability epoch) or lost (the run no longer on this worker) the same
// flight is refused and spends nothing.
func TestCodexFlightOnDisabledAliasRefreshesUntilClaimEndsLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	newAccess := codexToken("access-new")
	fake := &fakeRefreshClient{result: codexauth.RefreshResult{AccessToken: newAccess}}
	f := newRefreshFixture(t, env, fake)
	capw := env.mintCap(t, f.runID, f.workerID)
	setAliasEnabled(t, env, f.userID, f.aliasID, false)
	if accountPolled(t, env, f.userID, f.accountID) {
		t.Fatal("precondition: the account must have no enabled alias")
	}

	res, err := f.svc.CoordinatedCodexRefresh(env.ctx, f.wkr, f.runID, capw, uuid.New(), 0)
	if err != nil {
		t.Fatalf("refresh under a live claim on a disabled alias: %v", err)
	}
	if res.Outcome != CodexRefreshAdvanced || res.AccessToken != newAccess || fake.calls != 1 {
		t.Fatalf("result = %+v calls=%d, want an advanced refresh releasing %q", res, fake.calls, newAccess)
	}

	// A newer claim supersedes this flight's capability.
	env.mintCap(t, f.runID, f.workerID)
	if _, err := f.svc.CoordinatedCodexRefresh(env.ctx, f.wkr, f.runID, capw, uuid.New(), 1); !errors.Is(err, ErrCodexCapabilityEpoch) {
		t.Fatalf("superseded claim: err = %v, want ErrCodexCapabilityEpoch", err)
	}
	// The run left this worker: the exception is gone too.
	current := env.mintCap(t, f.runID, f.workerID)
	env.exec(`UPDATE runs SET worker_id = NULL WHERE id = $1`, f.runID)
	if _, err := f.svc.CoordinatedCodexRefresh(env.ctx, f.wkr, f.runID, current, uuid.New(), 1); !errors.Is(err, ErrCodexWorkerMismatch) {
		t.Fatalf("lost claim: err = %v, want ErrCodexWorkerMismatch", err)
	}
	if fake.calls != 1 {
		t.Fatalf("provider calls = %d, want 1: a refused flight spends nothing", fake.calls)
	}
}

// TestCodexWorkerRefreshInProgressCompletesAcrossDisableLiveDB (D7): a worker-mediated
// refresh whose provider exchange is in flight when the alias is disabled commits and
// persists the rotated login, and the flight, still holding its claim, receives the token.
func TestCodexWorkerRefreshInProgressCompletesAcrossDisableLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	newAccess, rotatedRefresh := codexToken("access-new"), codexToken("refresh-rotated")
	fake := &fakeRefreshClient{result: codexauth.RefreshResult{AccessToken: newAccess, RefreshToken: &rotatedRefresh}}
	f := newRefreshFixture(t, env, fake)
	capw := env.mintCap(t, f.runID, f.workerID)
	fake.onRefresh = func() { setAliasEnabled(t, env, f.userID, f.aliasID, false) }

	res, err := f.svc.CoordinatedCodexRefresh(env.ctx, f.wkr, f.runID, capw, uuid.New(), 0)
	if err != nil || res.Outcome != CodexRefreshAdvanced || res.AccessToken != newAccess {
		t.Fatalf("refresh = %+v err=%v, want an advanced refresh", res, err)
	}
	if acct := env.mustAccount(t, f.userID, f.accountID); acct.Generation != 1 || acct.CoordState != "idle" {
		t.Fatalf("account = gen %d state %q, want committed (1, idle)", acct.Generation, acct.CoordState)
	}
	if blob := env.accountBlob(t, f.userID, f.accountID); blob.AccessToken != newAccess || blob.RefreshToken != rotatedRefresh {
		t.Fatal("the refreshed login was not persisted")
	}
}

// TestCodexRecoveryGatedOnEnabledAliasLiveDB (D6/D7): with the account's only alias disabled,
// the recovery pass still resolves the interrupted refresh's intent (durable, local) but does
// not promote the protected recovery material (an upstream identity call); the material is
// retained. After re-enable the same pass promotes it.
func TestCodexRecoveryGatedOnEnabledAliasLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	newAccess := codexToken("access-new")
	fake := &fakeRefreshClient{result: codexauth.RefreshResult{AccessToken: newAccess}}
	f := newRefreshFixture(t, env, fake)
	capw := env.mintCap(t, f.runID, f.workerID)
	f.svc.q = refreshHookStore{Queries: env.q, commitErr: errors.New("commit exploded")}
	op := uuid.New()
	if _, err := f.svc.CoordinatedCodexRefresh(env.ctx, f.wkr, f.runID, capw, op, 0); !errors.Is(err, ErrCodexRefreshQuarantined) {
		t.Fatalf("seed commit-failure: err = %v, want ErrCodexRefreshQuarantined", err)
	}
	f.svc.q = env.q
	setAliasEnabled(t, env, f.userID, f.aliasID, false)

	n, err := f.svc.ReconcileUnresolvedCodexRefresh(env.ctx, f.userID, f.accountID)
	if err != nil {
		t.Fatalf("reconcile while disabled: %v", err)
	}
	if n != 1 {
		t.Fatalf("resolved intents while disabled = %d, want 1 (durable reconciliation still runs)", n)
	}
	if fake.discoverCalls != 0 {
		t.Fatalf("recovery discovery calls while disabled = %d, want 0", fake.discoverCalls)
	}
	held := env.mustAccount(t, f.userID, f.accountID)
	if held.CoordState != codexCoordQuarantined || len(held.RecoverySealed) == 0 || held.Generation != 0 {
		t.Fatalf("account while disabled = state %q slot %d gen %d, want quarantined with the material retained at 0",
			held.CoordState, len(held.RecoverySealed), held.Generation)
	}

	setAliasEnabled(t, env, f.userID, f.aliasID, true)
	if _, err := f.svc.ReconcileUnresolvedCodexRefresh(env.ctx, f.userID, f.accountID); err != nil {
		t.Fatalf("reconcile after re-enable: %v", err)
	}
	if fake.discoverCalls != 1 {
		t.Fatalf("recovery discovery calls after re-enable = %d, want 1", fake.discoverCalls)
	}
	acct := env.mustAccount(t, f.userID, f.accountID)
	if acct.CoordState != "idle" || acct.Generation != 1 || len(acct.RecoverySealed) != 0 {
		t.Fatalf("account after re-enable = state %q gen %d slot %d, want promoted (idle, 1, empty)", acct.CoordState, acct.Generation, len(acct.RecoverySealed))
	}
	if blob := env.accountBlob(t, f.userID, f.accountID); blob.AccessToken != newAccess {
		t.Fatal("the promoted login is not the recovered material")
	}
}

// TestCodexExpiredLeaseReapedWhileDisabledLiveDB (D7): an account wedged by an interrupted
// refresh is reaped into quarantine and its orphaned intent resolved even when no alias is
// enabled: that is durable completion of a refresh that already started, not new background
// work.
func TestCodexExpiredLeaseReapedWhileDisabledLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	f := newRefreshFixture(t, env, &fakeRefreshClient{})
	op := wedgeCodexAccount(t, env, f, time.Now().Add(-time.Hour))
	setAliasEnabled(t, env, f.userID, f.aliasID, false)
	if !survivorScanContains(t, env, f.accountID) {
		t.Fatal("the wedged account left the survivor scan when its alias was disabled")
	}
	if _, err := f.svc.ReconcileUnresolvedCodexRefresh(env.ctx, f.userID, f.accountID); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if acct := env.mustAccount(t, f.userID, f.accountID); acct.CoordState != codexCoordQuarantined {
		t.Fatalf("coord_state = %q, want quarantined (lease reaped)", acct.CoordState)
	}
	if it := mustIntent(t, env, op, f.userID); it.State == codexIntentRotating {
		t.Fatal("the orphaned intent is still rotating")
	}
}

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

// These tests exercise CollectCodexAccountUsage end to end against a REAL Postgres with an
// IN-PROCESS fake codexauth client — no HTTP (PRD #1209 M2). They prove the token-boundary and
// authority contract on the real schema: the raw access token never leaves the package (the
// return type carries only a bucket set), a 401 drives at most one bounded rotation + retry,
// a proven no-renewal expiry flags reauth, and a 403 never refreshes or reauths.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.

// fakeUsageClient is the in-process usage-read + refresh seam for the Service. It serves a
// queue of ReadUsage results (so a test can script first-401-then-success), records the
// ChatGPT-Account-Id header it was handed, and counts Refresh calls (proving a 403/mismatch
// spends none). It satisfies CodexRefreshClient (DiscoverIdentity + Refresh) AND
// CodexUsageReader (ReadUsage), so the ONE seam backs both the usage read and the coordinated
// refresh, exactly as the production shared codexauth.Client does.
type fakeUsageClient struct {
	usageResults  []usageResult
	usageCalls    int
	lastAccountID string

	refreshResult codexauth.RefreshResult
	refreshErr    error
	refreshCalls  int
}

type usageResult struct {
	reading codexauth.UsageReading
	err     error
}

func (f *fakeUsageClient) ReadUsage(_ context.Context, _ string, workspaceAccountID string) (codexauth.UsageReading, error) {
	i := f.usageCalls
	f.usageCalls++
	f.lastAccountID = workspaceAccountID
	if i < len(f.usageResults) {
		return f.usageResults[i].reading, f.usageResults[i].err
	}
	return codexauth.UsageReading{}, errors.New("fake: no usage result configured for call")
}

func (f *fakeUsageClient) Refresh(_ context.Context, _ string) (codexauth.RefreshResult, error) {
	f.refreshCalls++
	if f.refreshErr != nil {
		return codexauth.RefreshResult{}, f.refreshErr
	}
	return f.refreshResult, nil
}

func (f *fakeUsageClient) DiscoverIdentity(_ context.Context, _ string) (codexauth.Identity, error) {
	// Not reached on the usage/refresh paths under test (recovery promotion is the only caller).
	return codexauth.Identity{}, errors.New("fake: DiscoverIdentity not configured")
}

// usageFixture is a linked subscription account plus a Service wired with the given fake.
type usageFixture struct {
	userID         uuid.UUID
	aliasID        uuid.UUID
	accountID      uuid.UUID
	svc            *Service
	access         string
	providerUserID string
	workspaceID    string
}

// newUsageFixture seeds and reconciles one linked account. withRefreshToken controls whether
// the login carries a refresh token (false drives the ErrCodexRefreshNoToken → reauth path).
func newUsageFixture(t *testing.T, env codexTestEnv, fake *fakeUsageClient, withRefreshToken bool) usageFixture {
	t.Helper()
	userID := env.seedUser(t)
	access := codexToken("access")
	refresh := ""
	if withRefreshToken {
		refresh = codexToken("refresh")
	}
	aliasID := env.seedStagingAlias(t, userID, "codex-usage-"+uuid.NewString(), codexLoginBlob{AccessToken: access, RefreshToken: refresh})

	providerUser := "user-" + uuid.NewString()
	workspace := "acct-" + uuid.NewString()
	fid := newFakeCodexIdentity()
	fid.idByToken[access] = codexauth.Identity{ProviderUserID: providerUser, WorkspaceAccountID: workspace}
	r := NewCodexReconciler(env.q, nil, env.box, fid, env.pool)
	if err := r.ReconcileCodexAuthIdentity(env.ctx, userID, aliasID); err != nil {
		t.Fatalf("reconcile usage alias: %v", err)
	}
	st, err := env.q.GetCodexCredentialState(env.ctx, store.GetCodexCredentialStateParams{UserSecretID: aliasID, UserID: userID})
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if !st.ProviderAccountID.Valid {
		t.Fatal("usage alias must link an account")
	}
	accountID := uuid.UUID(st.ProviderAccountID.Bytes)

	svc := &Service{q: env.q, box: env.box, codexRefresh: fake}
	return usageFixture{
		userID:         userID,
		aliasID:        aliasID,
		accountID:      accountID,
		svc:            svc,
		access:         access,
		providerUserID: providerUser,
		workspaceID:    workspace,
	}
}

func usageAuthError(status int, retryAfter time.Duration) *codexauth.AuthError {
	return &codexauth.AuthError{Op: "read_usage", StatusCode: status, RetryAfter: retryAfter}
}

func mustUsageFailure(t *testing.T, err error) *CodexUsageFailure {
	t.Helper()
	var f *CodexUsageFailure
	if !errors.As(err, &f) {
		t.Fatalf("want *CodexUsageFailure, got %T: %v", err, err)
	}
	return f
}

func TestCollectCodexAccountUsageHappyLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fake := &fakeUsageClient{}
	fx := newUsageFixture(t, env, fake, true)
	pct := 42.0
	fake.usageResults = []usageResult{{reading: codexauth.UsageReading{
		UserID: fx.providerUserID,
		Buckets: []codexauth.UsageBucket{{
			ID:      "codex",
			Primary: &codexauth.UsageWindow{UsedPercent: &pct},
		}},
	}}}

	reading, err := fx.svc.CollectCodexAccountUsage(env.ctx, fx.userID, fx.accountID)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if reading.ObservedGeneration != 0 || reading.ObservedCredentialRevision != 0 {
		t.Fatalf("observed = (%d,%d), want (0,0)", reading.ObservedGeneration, reading.ObservedCredentialRevision)
	}
	if len(reading.Buckets) != 1 || reading.Buckets[0].ID != "codex" {
		t.Fatalf("buckets = %+v", reading.Buckets)
	}
	if reading.Buckets[0].Primary == nil || reading.Buckets[0].Primary.UsedPercent == nil || *reading.Buckets[0].Primary.UsedPercent != 42.0 {
		t.Fatalf("bucket window not mapped: %+v", reading.Buckets[0])
	}
	if fake.lastAccountID != fx.workspaceID {
		t.Fatalf("ChatGPT-Account-Id = %q, want the persisted workspace id %q", fake.lastAccountID, fx.workspaceID)
	}
	if fake.refreshCalls != 0 {
		t.Fatalf("a 2xx usage read must spend no refresh, got %d", fake.refreshCalls)
	}
}

func TestCollectCodexAccountUsageNotLinkedLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fake := &fakeUsageClient{}
	fx := newUsageFixture(t, env, fake, true)
	// Drop the linked alias: no subscription to meter.
	env.exec(`UPDATE codex_credential_state SET status='failed', provider_account_id=NULL WHERE user_secret_id=$1`, fx.aliasID)

	_, err := fx.svc.CollectCodexAccountUsage(env.ctx, fx.userID, fx.accountID)
	if got := mustUsageFailure(t, err).Kind; got != CodexUsageFailNotLinked {
		t.Fatalf("kind = %v, want not_linked", got)
	}
	if fake.usageCalls != 0 {
		t.Fatalf("a not-linked account must never reach the provider, got %d calls", fake.usageCalls)
	}
}

func TestCollectCodexAccountUsageProviderMismatchLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fake := &fakeUsageClient{}
	fx := newUsageFixture(t, env, fake, true)
	// The response names a DIFFERENT user than the account's frozen provider_user_id.
	fake.usageResults = []usageResult{{reading: codexauth.UsageReading{UserID: "someone-else"}}}

	_, err := fx.svc.CollectCodexAccountUsage(env.ctx, fx.userID, fx.accountID)
	if got := mustUsageFailure(t, err).Kind; got != CodexUsageFailProviderMismatch {
		t.Fatalf("kind = %v, want provider_mismatch", got)
	}
	if fake.refreshCalls != 0 {
		t.Fatalf("a mismatch must never refresh, got %d", fake.refreshCalls)
	}
}

func TestCollectCodexAccountUsageForbiddenIsTransientLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fake := &fakeUsageClient{}
	fx := newUsageFixture(t, env, fake, true)
	fake.usageResults = []usageResult{{err: usageAuthError(http.StatusForbidden, 0)}}

	_, err := fx.svc.CollectCodexAccountUsage(env.ctx, fx.userID, fx.accountID)
	if got := mustUsageFailure(t, err).Kind; got != CodexUsageFailTransient {
		t.Fatalf("kind = %v, want transient", got)
	}
	if fake.refreshCalls != 0 {
		t.Fatalf("a bare 403 must never trigger a refresh, got %d", fake.refreshCalls)
	}
	acct := env.mustAccount(t, fx.userID, fx.accountID)
	if acct.ReauthRequired {
		t.Fatal("a bare 403 must never set reauth_required")
	}
}

func TestCollectCodexAccountUsageRateLimitedLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fake := &fakeUsageClient{}
	fx := newUsageFixture(t, env, fake, true)
	fake.usageResults = []usageResult{{err: usageAuthError(http.StatusTooManyRequests, 45*time.Second)}}

	_, err := fx.svc.CollectCodexAccountUsage(env.ctx, fx.userID, fx.accountID)
	f := mustUsageFailure(t, err)
	if f.Kind != CodexUsageFailRateLimited {
		t.Fatalf("kind = %v, want rate_limited", f.Kind)
	}
	if f.RetryAfter != 45*time.Second {
		t.Fatalf("RetryAfter = %v, want 45s", f.RetryAfter)
	}
	if fake.refreshCalls != 0 {
		t.Fatalf("a 429 must never refresh, got %d", fake.refreshCalls)
	}
}

func TestCollectCodexAccountUsageQuarantinedIsDisabledLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fake := &fakeUsageClient{}
	fx := newUsageFixture(t, env, fake, true)
	env.exec(`UPDATE codex_provider_account SET coord_state='quarantined' WHERE id=$1`, fx.accountID)

	_, err := fx.svc.CollectCodexAccountUsage(env.ctx, fx.userID, fx.accountID)
	if got := mustUsageFailure(t, err).Kind; got != CodexUsageFailDisabled {
		t.Fatalf("kind = %v, want disabled", got)
	}
	if fake.usageCalls != 0 {
		t.Fatalf("a quarantined account must never reach the provider, got %d", fake.usageCalls)
	}
}

// TestCollectCodexAccountUsageUnauthorizedRefreshesLiveDB proves the 401 rotation path: the
// first ReadUsage is rejected, a single coordinated refresh advances the generation, and the
// retry against the freshly-committed token succeeds — captured under the ADVANCED generation.
func TestCollectCodexAccountUsageUnauthorizedRefreshesLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fake := &fakeUsageClient{}
	fx := newUsageFixture(t, env, fake, true)
	rotatedRefresh := codexToken("refresh-rotated")
	fake.refreshResult = codexauth.RefreshResult{
		AccessToken:    codexToken("access-rotated"),
		RefreshToken:   &rotatedRefresh,
		IdentityClaims: freshClaimsForIdentity(codexauth.Identity{ProviderUserID: fx.providerUserID, WorkspaceAccountID: fx.workspaceID}),
	}
	fake.usageResults = []usageResult{
		{err: usageAuthError(http.StatusUnauthorized, 0)},
		{reading: codexauth.UsageReading{UserID: fx.providerUserID, Buckets: []codexauth.UsageBucket{{ID: "codex"}}}},
	}

	reading, err := fx.svc.CollectCodexAccountUsage(env.ctx, fx.userID, fx.accountID)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if fake.refreshCalls != 1 {
		t.Fatalf("want exactly one refresh, got %d", fake.refreshCalls)
	}
	if fake.usageCalls != 2 {
		t.Fatalf("want two usage reads (401 then retry), got %d", fake.usageCalls)
	}
	if reading.ObservedGeneration != 1 {
		t.Fatalf("observed generation = %d, want 1 (advanced by the rotation)", reading.ObservedGeneration)
	}
	acct := env.mustAccount(t, fx.userID, fx.accountID)
	if acct.Generation != 1 {
		t.Fatalf("account generation = %d, want 1", acct.Generation)
	}
	if acct.ReauthRequired {
		t.Fatal("a successful refresh must not set reauth_required")
	}
}

// TestCollectCodexAccountUsageNoRefreshTokenReauthsLiveDB proves the proven-expiry path: a 401
// on an account whose login carries NO refresh token flags reauth_required and returns the
// reauth failure, spending no successful rotation.
func TestCollectCodexAccountUsageNoRefreshTokenReauthsLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fake := &fakeUsageClient{}
	fx := newUsageFixture(t, env, fake, false) // no refresh token
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
		t.Fatalf("a failed no-token rotation must not advance the generation, got %d", acct.Generation)
	}
}

// TestReconcileClearsReauthFlagLiveDB proves the deliverable-B reconcile widening: a
// re-imported alias whose tuple resolves to a REAUTH-flagged (non-quarantined) account fires
// RefreshCodexAccountLogin's reauth arm — installing the fresh login, advancing the generation,
// clearing the flag, and linking the new alias.
func TestReconcileClearsReauthFlagLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fake := &fakeUsageClient{}
	fx := newUsageFixture(t, env, fake, true)

	// Flag the linked account reauth_required at its live (generation, credential_revision).
	acct := env.mustAccount(t, fx.userID, fx.accountID)
	n, err := env.q.MarkCodexReauthRequired(env.ctx, store.MarkCodexReauthRequiredParams{
		ObservedGeneration:         acct.Generation,
		ObservedCredentialRevision: acct.CredentialRevision,
		ID:                         fx.accountID,
		UserID:                     fx.userID,
	})
	if err != nil || n != 1 {
		t.Fatalf("mark reauth: n=%d err=%v", n, err)
	}

	// Re-import the SAME identity under a new alias and reconcile it.
	newAccess := codexToken("access-reimport")
	newAlias := env.seedStagingAlias(t, fx.userID, "codex-reimport-"+uuid.NewString(), codexLoginBlob{AccessToken: newAccess, RefreshToken: codexToken("refresh-reimport")})
	fid := newFakeCodexIdentity()
	fid.idByToken[newAccess] = codexauth.Identity{ProviderUserID: fx.providerUserID, WorkspaceAccountID: fx.workspaceID}
	r := NewCodexReconciler(env.q, nil, env.box, fid, env.pool)
	if err := r.ReconcileCodexAuthIdentity(env.ctx, fx.userID, newAlias); err != nil {
		t.Fatalf("reconcile re-import: %v", err)
	}

	got := env.mustAccount(t, fx.userID, fx.accountID)
	if got.ReauthRequired {
		t.Fatal("reconcile must clear the reauth flag")
	}
	if got.Generation != acct.Generation+1 {
		t.Fatalf("generation = %d, want %d (advanced by the reauth-arm restore)", got.Generation, acct.Generation+1)
	}
	st, err := env.q.GetCodexCredentialState(env.ctx, store.GetCodexCredentialStateParams{UserSecretID: newAlias, UserID: fx.userID})
	if err != nil {
		t.Fatalf("read new alias state: %v", err)
	}
	if st.Status != codexStatusLinked || !st.ProviderAccountID.Valid || uuid.UUID(st.ProviderAccountID.Bytes) != fx.accountID {
		t.Fatalf("re-imported alias not linked to the account: status=%q account=%v", st.Status, st.ProviderAccountID)
	}
}

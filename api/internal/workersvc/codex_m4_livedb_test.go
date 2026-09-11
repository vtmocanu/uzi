package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/codexauth"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1147 M4 (ships DARK): the exhaustive PROOF of the M2 coordinated-refresh + authority
// mechanism, driven with an in-process fake provider and, where the property demands it, TWO
// concurrent clients against a REAL Postgres. These tests are TEST-ONLY: they add no
// production code and only stress the M2 seams already committed. They fill the gaps the
// representative M2 tests (codexrefresh_livedb_test.go / codexauthz_livedb_test.go) leave:
//
//	A  two clients burst the SAME stale generation → exactly ONE rotation, both get the one
//	   committed token (the loser reconciles, never a second exchange);
//	B  two successive generations N→N+1→N+2 mint DISTINCT fresh tokens, durable across a
//	   simulated restart (fresh readback of the sealed login);
//	C  a same-account alias replace revokes ONLY that alias's run, not a sibling run bound to
//	   another alias of the same account (per-alias vs account-wide revocation);
//	D  a generation bump is orthogonal to capability validity — the same claim capability
//	   still authorizes after the account advanced;
//	E  a refresh cancelled mid-exchange (before the durable commit) returns NO token, does
//	   NOT commit, and leaves the account recoverable (the lease is reaped to quarantine, not
//	   stranded in_progress forever);
//	F  the dark guarantee — an ORDINARY Claude run has no Codex binding and emits no `codex`
//	   block on the wire.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres. All fixtures use fresh
// UUIDs so a single-process `-run 'LiveDB$'` sweep never collides.

// mintingRefreshClient is the m4 fake oauth-exchange seam. Unlike fakeRefreshClient (which
// returns a fixed result), it mints a DISTINCT access token on EVERY call so "fresh vs
// stale" is observable, and models SINGLE-USE rotating refresh tokens (each call returns a
// new refresh token, so a later exchange must be handed the one the prior call minted). It
// is safe for concurrent use — test A drives it from two goroutines. onExchange, when set,
// runs inside Refresh AFTER the call is counted and BEFORE the token is minted; returning a
// non-nil error from it makes the exchange fail (test E blocks there until the context is
// cancelled).
type mintingRefreshClient struct {
	mu         sync.Mutex
	calls      int
	handed     []string // the refresh token handed to Refresh, in call order
	access     []string // the access token minted by Refresh, in call order
	refresh    []string // the rotated refresh token minted by Refresh, in call order
	onExchange func(ctx context.Context, call int) error

	// matchIdentity supplies every minted token's matching claim tuple and remains the
	// DiscoverIdentity answer for recovery promotion. A rotation never changes this tuple;
	// seedTwoRunsSameAccount / newRefreshFixture pin it to the fixture account.
	matchIdentity codexauth.Identity
}

func (c *mintingRefreshClient) Refresh(ctx context.Context, refreshToken string) (codexauth.RefreshResult, error) {
	c.mu.Lock()
	c.calls++
	call := c.calls
	c.handed = append(c.handed, refreshToken)
	matchIdentity := c.matchIdentity
	c.mu.Unlock()

	if c.onExchange != nil {
		if err := c.onExchange(ctx, call); err != nil {
			return codexauth.RefreshResult{}, err
		}
	}

	access := codexToken(fmt.Sprintf("access-gen%d", call))
	rotated := codexToken(fmt.Sprintf("refresh-gen%d", call))
	c.mu.Lock()
	c.access = append(c.access, access)
	c.refresh = append(c.refresh, rotated)
	c.mu.Unlock()
	return codexauth.RefreshResult{
		AccessToken:    access,
		RefreshToken:   &rotated,
		IdentityClaims: freshClaimsForIdentity(matchIdentity),
	}, nil
}

// DiscoverIdentity returns the pinned account tuple for recovery promotion. It remains
// independently observable from the fresh-token claim path and is safe for concurrent use.
func (c *mintingRefreshClient) DiscoverIdentity(_ context.Context, _ string) (codexauth.Identity, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.matchIdentity, nil
}

func (c *mintingRefreshClient) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// twoRunSameAccountFixture is two claimed subscription runs bound to TWO DIFFERENT aliases of
// the SAME provider account, sharing one Service (so a single fake provider counts the total
// rotations across both). It is the substrate for the two-client concurrency (A) and the
// per-alias revocation (C) proofs.
type twoRunSameAccountFixture struct {
	userID    uuid.UUID
	workerID  uuid.UUID
	accountID uuid.UUID
	alias1    uuid.UUID
	alias2    uuid.UUID
	run1      uuid.UUID
	run2      uuid.UUID
	svc       *Service
	wkr       store.Worker
}

// seedTwoRunsSameAccount seeds two staging aliases whose discovery yields the SAME identity
// tuple (so the second links to the first's account rather than creating a second), reconciles
// both, then freezes a claimed subscription run on each. The shared Service is wired with the
// given refresh client. accessToken1 is installed as the account's generation-0 login (the
// first alias reconciled owns the sealed_login), so a test can assert nobody hands back the
// pre-rotation token.
func seedTwoRunsSameAccount(t *testing.T, env codexTestEnv, refresh CodexRefreshClient) (twoRunSameAccountFixture, string) {
	t.Helper()
	userID, workerID, repoID := env.seedCodexInfra(t)

	providerUser := "user-" + uuid.NewString()
	workspace := "acct-" + uuid.NewString()

	access1 := codexToken("access-1")
	access2 := codexToken("access-2")
	alias1 := env.seedStagingAlias(t, userID, "codex-m4a-"+uuid.NewString(), codexLoginBlob{AccessToken: access1, RefreshToken: codexToken("refresh-1")})
	alias2 := env.seedStagingAlias(t, userID, "codex-m4b-"+uuid.NewString(), codexLoginBlob{AccessToken: access2, RefreshToken: codexToken("refresh-2")})

	fid := newFakeCodexIdentity()
	sameID := codexauth.Identity{ProviderUserID: providerUser, WorkspaceAccountID: workspace}
	fid.idByToken[access1] = sameID
	fid.idByToken[access2] = sameID
	r := NewCodexReconciler(env.q, nil, env.box, fid, env.pool)
	if err := r.ReconcileCodexAuthIdentity(env.ctx, userID, alias1); err != nil {
		t.Fatalf("reconcile alias1: %v", err)
	}
	if err := r.ReconcileCodexAuthIdentity(env.ctx, userID, alias2); err != nil {
		t.Fatalf("reconcile alias2: %v", err)
	}

	st1 := mustState(t, env, userID, alias1)
	st2 := mustState(t, env, userID, alias2)
	if !st1.ProviderAccountID.Valid || !st2.ProviderAccountID.Valid {
		t.Fatal("both aliases must link a provider account")
	}
	if st1.ProviderAccountID.Bytes != st2.ProviderAccountID.Bytes {
		t.Fatal("aliases must converge to the SAME account for the two-client proof")
	}
	if n := env.countProviderAccounts(t, userID); n != 1 {
		t.Fatalf("provider accounts = %d, want 1 (two aliases, one account)", n)
	}
	accountID := uuid.UUID(st1.ProviderAccountID.Bytes)

	// Pin both the fake's fresh-token claims and recovery DiscoverIdentity answer to the
	// shared account tuple across every rotation.
	setFakeMatchIdentity(refresh, codexauth.Identity{ProviderUserID: providerUser, WorkspaceAccountID: workspace})

	svc := &Service{q: env.q, box: env.box, codexRefresh: refresh}
	// Two runs on the SAME account but DISTINCT repos: uq_runs_one_active_per_issue is
	// (repo_id, issue_iid), and both seeded runs use issue_iid=1, so they must not share a
	// repo. The shared thing under test is the account, not the repo.
	repoID2 := env.seedCodexRepo(t, userID)
	run1 := env.seedCodexRun(t, userID, workerID, repoID)
	run2 := env.seedCodexRun(t, userID, workerID, repoID2)
	if err := svc.FreezeCodexBinding(env.ctx, userID, run1, alias1, codexAuthModeSubscription); err != nil {
		t.Fatalf("freeze run1: %v", err)
	}
	if err := svc.FreezeCodexBinding(env.ctx, userID, run2, alias2, codexAuthModeSubscription); err != nil {
		t.Fatalf("freeze run2: %v", err)
	}

	// The account's committed generation-0 access token is alias1's (the first reconcile
	// sealed its blob into the new account).
	preRotationAccess := access1
	return twoRunSameAccountFixture{
		userID: userID, workerID: workerID, accountID: accountID,
		alias1: alias1, alias2: alias2, run1: run1, run2: run2,
		svc: svc, wkr: store.Worker{ID: workerID, UserID: userID},
	}, preRotationAccess
}

// seedCodexRepo seeds a fresh forge connection + repo for an existing user and returns the
// repo id — used to place a second run on a DISTINCT repo so two runs bound to the same
// account do not collide on uq_runs_one_active_per_issue (repo_id, issue_iid).
func (e codexTestEnv) seedCodexRepo(t *testing.T, userID uuid.UUID) uuid.UUID {
	t.Helper()
	connID, repoID := uuid.New(), uuid.New()
	// base_url must be unique per (user_id, forge_type, base_url); seedCodexInfra already
	// used https://forge.e2e for this user, so derive a distinct one from the connection id.
	baseURL := "https://forge.e2e/" + connID.String()
	e.exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	        VALUES ($1, $2, 'gitlab', $3, 'bot', 1, $4)`, connID, userID, baseURL, []byte("x"))
	e.exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	        VALUES ($1, $2, 1, $3, $4, 'main', true)`, repoID, connID, "g/"+repoID.String(), "https://forge.e2e/g/"+repoID.String())
	return repoID
}

// TestCoordinatedCodexRefreshTwoClientBurstSingleRotationLiveDB (m4 A) proves the core
// concurrency invariant: two runs bound to the SAME account, both observing the same current
// generation, fire CoordinatedCodexRefresh CONCURRENTLY with DIFFERENT operation ids, released
// together by a barrier. The lease serializes them so the provider rotates EXACTLY ONCE, the
// account advances EXACTLY ONE generation, and BOTH callers converge on the SAME committed
// access token — the loser reconciles/replays to it rather than exchanging a second time. No
// caller ever receives the pre-rotation (stale) token.
func TestCoordinatedCodexRefreshTwoClientBurstSingleRotationLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fake := &mintingRefreshClient{}
	f, preRotationAccess := seedTwoRunsSameAccount(t, env, fake)

	cap1 := env.mintCap(t, f.run1, f.workerID)
	cap2 := env.mintCap(t, f.run2, f.workerID)

	// A contended caller's contract is "retry and reconcile"; loop until it lands a token so
	// the barrier race (winner vs loser) resolves to the committed token rather than a
	// transient contended error. Only the winner ever exchanges, so this spends NO extra
	// provider call.
	refreshUntilToken := func(runID uuid.UUID, capability string, op uuid.UUID) (CodexRefreshResult, error) {
		for i := 0; i < 500; i++ {
			res, err := f.svc.CoordinatedCodexRefresh(env.ctx, f.wkr, runID, capability, op, 0)
			if err == nil {
				return res, nil
			}
			if errors.Is(err, ErrCodexRefreshContended) {
				time.Sleep(time.Millisecond)
				continue
			}
			return res, err
		}
		return CodexRefreshResult{}, fmt.Errorf("refresh did not converge for run %s", runID)
	}

	var (
		res1, res2 CodexRefreshResult
		err1, err2 error
		wg         sync.WaitGroup
	)
	release := make(chan struct{})
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-release
		res1, err1 = refreshUntilToken(f.run1, cap1, uuid.New())
	}()
	go func() {
		defer wg.Done()
		<-release
		res2, err2 = refreshUntilToken(f.run2, cap2, uuid.New())
	}()
	close(release) // barrier: both goroutines start observing generation 0 together
	wg.Wait()

	if err1 != nil {
		t.Fatalf("client 1: %v", err1)
	}
	if err2 != nil {
		t.Fatalf("client 2: %v", err2)
	}

	// Exactly ONE provider exchange across BOTH clients.
	if fake.callCount() != 1 {
		t.Fatalf("provider calls = %d, want exactly 1 (one rotation for the burst)", fake.callCount())
	}
	// The account advanced exactly one generation and settled to idle.
	acct := env.mustAccount(t, f.userID, f.accountID)
	if acct.Generation != 1 {
		t.Fatalf("generation = %d, want 1 (exactly one advance)", acct.Generation)
	}
	if acct.CoordState != "idle" {
		t.Fatalf("coord_state = %q, want idle after the single committed rotation", acct.CoordState)
	}

	// Both callers ended with a usable token, and it is the SINGLE committed generation's
	// token — the loser reconciled to it, not to a second rotation.
	committed := env.accountBlob(t, f.userID, f.accountID).AccessToken
	for i, res := range []CodexRefreshResult{res1, res2} {
		if res.AccessToken == "" {
			t.Fatalf("client %d got no access token", i+1)
		}
		if res.AccessToken == preRotationAccess {
			t.Fatalf("client %d got the pre-rotation (stale) token", i+1)
		}
		if res.AccessToken != committed {
			t.Fatalf("client %d token = %q, want the single committed token %q", i+1, res.AccessToken, committed)
		}
		if res.Generation != 1 {
			t.Fatalf("client %d generation = %d, want 1", i+1, res.Generation)
		}
		if res.ChatGPTAccountID != acct.WorkspaceAccountID {
			t.Fatalf("client %d chatgpt account id = %q, want verified %q", i+1, res.ChatGPTAccountID, acct.WorkspaceAccountID)
		}
	}
	if res1.AccessToken != res2.AccessToken {
		t.Fatalf("clients diverged: %q vs %q (both must hold the one committed token)", res1.AccessToken, res2.AccessToken)
	}
	// Exactly one of them advanced; the other reconciled/replayed (no second exchange).
	advanced := 0
	for _, res := range []CodexRefreshResult{res1, res2} {
		if res.Outcome == CodexRefreshAdvanced {
			advanced++
		}
	}
	if advanced != 1 {
		t.Fatalf("advanced outcomes = %d, want exactly 1 (the other reconciles/replays)", advanced)
	}
}

// TestCoordinatedCodexRefreshTwoGenerationsDistinctTokensLiveDB (m4 B) proves two successive
// rotations N→N+1→N+2 each mint a DISTINCT fresh access token (never returning the prior,
// now-expired one), that the second exchange is handed the refresh token the FIRST rotation
// minted (single-use rotation), and that each committed access token is DURABLE across a
// simulated restart: a fresh account read + sealed-login open (discarding all in-memory
// state) yields the just-committed token, proving the merged reseal persisted the new access
// token rather than only bumping the generation counter.
func TestCoordinatedCodexRefreshTwoGenerationsDistinctTokensLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fake := &mintingRefreshClient{}
	f := newRefreshFixture(t, env, fake)
	cap1 := env.mintCap(t, f.runID, f.workerID)

	// Cycle 1: advance 0→1.
	res1, err := f.svc.CoordinatedCodexRefresh(env.ctx, f.wkr, f.runID, cap1, uuid.New(), 0)
	if err != nil {
		t.Fatalf("cycle 1 advance: %v", err)
	}
	if res1.Outcome != CodexRefreshAdvanced || res1.Generation != 1 {
		t.Fatalf("cycle 1 = (%v, gen %d), want (advanced, gen 1)", res1.Outcome, res1.Generation)
	}
	t1 := res1.AccessToken
	if t1 == "" {
		t.Fatal("cycle 1 returned no token")
	}
	// Durable readback after commit 1 (simulate a restart: nothing in-memory carried over).
	if blob := env.accountBlob(t, f.userID, f.accountID); blob.AccessToken != t1 {
		t.Fatalf("readback after commit 1 = %q, want the committed %q", blob.AccessToken, t1)
	}

	// Cycle 2: the access token expired again, so a NEW operation advances 1→2 from the
	// now-current generation.
	res2, err := f.svc.CoordinatedCodexRefresh(env.ctx, f.wkr, f.runID, cap1, uuid.New(), 1)
	if err != nil {
		t.Fatalf("cycle 2 advance: %v", err)
	}
	if res2.Outcome != CodexRefreshAdvanced || res2.Generation != 2 {
		t.Fatalf("cycle 2 = (%v, gen %d), want (advanced, gen 2)", res2.Outcome, res2.Generation)
	}
	t2 := res2.AccessToken
	if t2 == t1 {
		t.Fatal("cycle 2 returned the SAME token as cycle 1 (a generation-advancing rotation must mint a fresh token)")
	}

	if fake.callCount() != 2 {
		t.Fatalf("provider calls = %d, want exactly 2 (one per generation)", fake.callCount())
	}
	// Single-use rotation: the second exchange was handed the refresh token the first rotation
	// minted, not the original.
	if len(fake.handed) != 2 || len(fake.refresh) < 1 {
		t.Fatalf("fake bookkeeping off: handed=%v refresh=%v", fake.handed, fake.refresh)
	}
	if fake.handed[1] != fake.refresh[0] {
		t.Fatalf("cycle 2 was handed %q, want the refresh token cycle 1 minted %q (single-use rotation)", fake.handed[1], fake.refresh[0])
	}
	if fake.handed[0] == fake.handed[1] {
		t.Fatal("the same refresh token was replayed across cycles (must rotate single-use)")
	}

	// Durable readback after commit 2 via a FRESH account read.
	acct := env.mustAccount(t, f.userID, f.accountID)
	if acct.Generation != 2 {
		t.Fatalf("generation = %d, want 2", acct.Generation)
	}
	if blob := env.accountBlob(t, f.userID, f.accountID); blob.AccessToken != t2 {
		t.Fatalf("readback after commit 2 = %q, want the committed %q (the reseal must persist the new access token)", blob.AccessToken, t2)
	}
}

// TestCodexAliasReplaceDoesNotRevokeSiblingRunLiveDB (m4 C) proves per-alias vs account-wide
// revocation separation. Two runs are bound to two DIFFERENT aliases of the SAME account.
// Bumping ONE alias's material_revision (a manual replace) revokes ONLY the run frozen on that
// alias (ErrCodexMaterialRevisionStale); the sibling run bound to the other alias of the same
// account still authorizes, because neither its own material_revision nor the shared account's
// credential_revision moved.
func TestCodexAliasReplaceDoesNotRevokeSiblingRunLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	f, _ := seedTwoRunsSameAccount(t, env, &mintingRefreshClient{})

	cap1 := env.mintCap(t, f.run1, f.workerID)
	cap2 := env.mintCap(t, f.run2, f.workerID)

	// Baseline: both runs authorize a release before any replace.
	if _, err := f.svc.AuthorizeCodexCredentialOp(env.ctx, f.wkr, f.run1, cap1, ScopeReleaseAccessToken); err != nil {
		t.Fatalf("run1 baseline authorize: %v", err)
	}
	if _, err := f.svc.AuthorizeCodexCredentialOp(env.ctx, f.wkr, f.run2, cap2, ScopeReleaseAccessToken); err != nil {
		t.Fatalf("run2 baseline authorize: %v", err)
	}

	// Manual replace of alias1 only: bump its material_revision (drops its account link too).
	if _, err := env.q.BumpCodexMaterialRevision(env.ctx, store.BumpCodexMaterialRevisionParams{
		Status:       "staging",
		UserSecretID: f.alias1,
		UserID:       f.userID,
	}); err != nil {
		t.Fatalf("bump alias1 material: %v", err)
	}

	// run1 (bound to alias1) is now revoked.
	if _, err := f.svc.AuthorizeCodexCredentialOp(env.ctx, f.wkr, f.run1, cap1, ScopeReleaseAccessToken); !errors.Is(err, ErrCodexMaterialRevisionStale) {
		t.Fatalf("run1 after alias1 replace: err = %v, want ErrCodexMaterialRevisionStale", err)
	}
	// run2 (bound to alias2 of the SAME account) is untouched — per-alias revocation did not
	// spill onto the account or the sibling alias.
	if _, err := f.svc.AuthorizeCodexCredentialOp(env.ctx, f.wkr, f.run2, cap2, ScopeReleaseAccessToken); err != nil {
		t.Fatalf("run2 after alias1 replace: err = %v, want it to STILL authorize", err)
	}
}

// TestCodexGenerationBumpDoesNotRevokeCapabilityLiveDB (m4 D) proves a generation advance is
// orthogonal to capability validity. A claimed run's capability, minted at epoch E, still
// authorizes a credential op after the account's generation is bumped: the authority gate is
// epoch/ownership/revisions/tuple, and generation is none of those, so a coordinated refresh
// advancing the account never invalidates an in-flight claim capability.
func TestCodexGenerationBumpDoesNotRevokeCapabilityLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	f := newSubscriptionFixture(t, env)
	cap1 := env.mintCap(t, f.runID, f.workerID)

	// Resolve the account behind the run's alias so we can advance its generation directly.
	st := mustState(t, env, f.userID, f.aliasID)
	if !st.ProviderAccountID.Valid {
		t.Fatal("subscription fixture must link an account")
	}
	accountID := uuid.UUID(st.ProviderAccountID.Bytes)

	// The capability authorizes before the bump.
	if _, err := f.svc.AuthorizeCodexCredentialOp(env.ctx, f.wkr, f.runID, cap1, ScopeReleaseAccessToken); err != nil {
		t.Fatalf("authorize before generation bump: %v", err)
	}

	// Advance the account's generation (as a committed refresh would), leaving
	// credential_revision and the identity tuple untouched.
	env.exec(`UPDATE codex_provider_account SET generation = generation + 1, committed_generation = generation + 1 WHERE id = $1`, accountID)
	if acct := env.mustAccount(t, f.userID, accountID); acct.Generation != 1 {
		t.Fatalf("generation = %d, want 1 after the bump", acct.Generation)
	}

	// The SAME capability (epoch unchanged) STILL authorizes — generation is orthogonal.
	authCtx, err := f.svc.AuthorizeCodexCredentialOp(env.ctx, f.wkr, f.runID, cap1, ScopeReleaseAccessToken)
	if err != nil {
		t.Fatalf("authorize after generation bump: err = %v, want nil (generation must not revoke the capability)", err)
	}
	if authCtx.AccountID != accountID {
		t.Fatalf("resolved account = %s, want %s", authCtx.AccountID, accountID)
	}
}

// TestCoordinatedCodexRefreshCancelledMidExchangeRecoverableLiveDB (m4 E) proves cancellation
// before the durable commit is safe: the context is cancelled while the provider exchange is
// in flight (a hook blocks the fake there until cancel), so the caller gets NO token, the
// account is NOT committed (generation unchanged), and the held lease is NOT stranded — a
// recovery pass whose clock is past the lease deadline reaps it to 'quarantined' and resolves
// the stranded intent, so the account is recoverable rather than permanently stuck holding a
// live lease.
func TestCoordinatedCodexRefreshCancelledMidExchangeRecoverableLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)

	reached := make(chan struct{})
	var once sync.Once
	fake := &mintingRefreshClient{
		onExchange: func(ctx context.Context, _ int) error {
			once.Do(func() { close(reached) })
			<-ctx.Done() // block the exchange until the caller cancels
			return ctx.Err()
		},
	}
	f := newRefreshFixture(t, env, fake)
	capw := env.mintCap(t, f.runID, f.workerID)

	ctx, cancel := context.WithCancel(env.ctx)
	var (
		res  CodexRefreshResult
		rerr error
		done = make(chan struct{})
	)
	go func() {
		res, rerr = f.svc.CoordinatedCodexRefresh(ctx, f.wkr, f.runID, capw, uuid.New(), 0)
		close(done)
	}()

	<-reached // the exchange is in flight, lease held, nothing committed
	cancel()
	<-done

	// No token escaped, and the outcome is the ambiguous-exchange (contended) shape.
	if rerr == nil {
		t.Fatal("cancelled refresh returned nil error, want a failure")
	}
	if res.AccessToken != "" {
		t.Fatalf("cancelled caller got token %q, want none", res.AccessToken)
	}
	if res.Outcome != CodexRefreshContended {
		t.Fatalf("outcome = %v, want contended (no durable commit)", res.Outcome)
	}
	if fake.callCount() != 1 {
		t.Fatalf("provider calls = %d, want exactly 1", fake.callCount())
	}

	// The account did NOT advance and the lease is held with a bounded deadline (in_progress,
	// not committed) — i.e. nothing was committed and the lease is time-limited, not eternal.
	acct := env.mustAccount(t, f.userID, f.accountID)
	if acct.Generation != 0 {
		t.Fatalf("generation = %d, want 0 (a cancelled exchange must not commit)", acct.Generation)
	}
	if acct.CommittedGeneration.Valid && acct.CommittedGeneration.Int64 != 0 {
		t.Fatalf("committed_generation = %d, want 0", acct.CommittedGeneration.Int64)
	}
	if acct.CoordState != "in_progress" {
		t.Fatalf("coord_state = %q, want in_progress (lease held pending reconcile)", acct.CoordState)
	}
	if !acct.LeaseDeadline.Valid {
		t.Fatal("lease_deadline must be set on a held lease")
	}

	// Recovery: a survivor whose clock is past the lease deadline reaps the expired lease to
	// quarantine and resolves the stranded 'rotating' intent — the account is NOT permanently
	// stuck holding a live lease.
	recSvc := &Service{q: env.q, box: env.box, now: func() time.Time { return time.Now().Add(codexRefreshLeaseTTL + time.Minute) }}
	n, err := recSvc.ReconcileUnresolvedCodexRefresh(env.ctx, f.userID, f.accountID)
	if err != nil {
		t.Fatalf("reconcile after cancel: %v", err)
	}
	if n != 1 {
		t.Fatalf("reconcile resolved %d intents, want 1", n)
	}
	if acct := env.mustAccount(t, f.userID, f.accountID); acct.CoordState != codexCoordQuarantined {
		t.Fatalf("coord_state = %q, want quarantined (the stranded lease was reaped)", acct.CoordState)
	}
}

// TestCodexDarkNoCodexOnNormalClaimPathLiveDB (m4 F) proves the dark guarantee: a run created
// through the ORDINARY (Claude) path carries no Codex binding — codex_secret_id and
// codex_auth_mode are NULL — so the claim-assembly Codex branch (guarded on
// run.CodexSecretID.Valid) is skipped, codexClaimSecrets refuses it as not-bound, and the
// emitted ClaimSecrets JSON carries NO `codex` key (byte-identical to today's wire).
func TestCodexDarkNoCodexOnNormalClaimPathLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, workerID, repoID := env.seedCodexInfra(t)
	runID := env.seedCodexRun(t, userID, workerID, repoID) // the ordinary create path, no codex binding
	// seedCodexInfra stores a placeholder forge token that box.Open cannot decrypt; the real
	// claim assembly opens the bot PAT, so reseal it with a genuine master-box blob.
	botPATSealed, err := env.box.Seal([]byte(codexToken("bot-pat")))
	if err != nil {
		t.Fatalf("seal bot PAT: %v", err)
	}
	env.exec(`UPDATE forge_connections SET token_ciphertext = $1 WHERE user_id = $2`, botPATSealed, userID)
	// A default Anthropic token so the real claim assembly (openAnthropic) resolves a
	// credential — an ordinary run always spends one.
	anthropicSealed, err := env.box.Seal([]byte(codexToken("anthropic")))
	if err != nil {
		t.Fatalf("seal anthropic token: %v", err)
	}
	env.exec(`INSERT INTO user_secrets (id, user_id, kind, label, is_default, ciphertext, sealed_with)
	          VALUES ($1, $2, 'anthropic_token', $3, true, $4, 'master')`,
		uuid.New(), userID, "anthropic-"+uuid.NewString(), anthropicSealed)
	svc := New(env.q, env.box, testParams())
	wkr := store.Worker{ID: workerID, UserID: userID}

	// The normal run has NO codex binding frozen.
	run := mustRun(t, env, runID)
	if run.CodexSecretID.Valid {
		t.Fatalf("ordinary run must have codex_secret_id NULL, got %v", run.CodexSecretID)
	}
	if run.CodexAuthMode.Valid {
		t.Fatalf("ordinary run must have codex_auth_mode NULL, got %v", run.CodexAuthMode)
	}

	// Even if the codex claim path were reached for a non-bound run, it refuses rather than
	// fabricating a codex block.
	if _, err := svc.codexClaimSecrets(env.ctx, wkr, run); !errors.Is(err, ErrCodexRunNotBound) {
		t.Fatalf("codexClaimSecrets on an unbound run: err = %v, want ErrCodexRunNotBound", err)
	}

	// Build the REAL claim payload via the production assembly path, exercising the
	// `run.CodexSecretID.Valid == false` branch (not a hand-built ClaimSecrets), then marshal
	// its Secrets: an ordinary run leaves ClaimSecrets.Codex nil and omitempty drops the key,
	// so no `codex` rides the wire.
	payload, err := svc.assembleClaim(env.ctx, wkr, run)
	if err != nil {
		t.Fatalf("assembleClaim: %v", err)
	}
	if payload.Secrets.Codex != nil {
		t.Fatalf("ordinary run's assembled claim carried a codex block: %+v", payload.Secrets.Codex)
	}
	raw, err := json.Marshal(payload.Secrets)
	if err != nil {
		t.Fatalf("marshal claim secrets: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal claim secrets: %v", err)
	}
	if _, ok := m["codex"]; ok {
		t.Fatalf("ordinary claim must emit NO codex block, got %v", m["codex"])
	}
}

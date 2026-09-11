package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/codexauth"
	"github.com/vtmocanu/uzi/api/internal/secretopen"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

// These tests exercise the coordinated Codex refresh state machine (PRD #1147 M2, B6,
// ships DARK) end to end against a REAL Postgres with an IN-PROCESS call-counting fake
// oauth client — no HTTP. They are REPRESENTATIVE: one client at a time, driving each
// arm of the state machine (advance, replay, reconcile, contended, quarantine, recovery).
// codex_m4_livedb_test.go supplies the exhaustive two-client concurrency matrix.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres. All fixtures use
// fresh UUIDs so a single-process `-run 'LiveDB$'` sweep never collides.

// fakeRefreshClient is the in-process oauth-exchange + recovery-identity seam. It counts
// Refresh calls (proving replay/reconcile spend zero), records the refresh token it was
// handed, and returns configured rotating material. The default refresh result carries a
// complete claim tuple derived from matchIdentity; tests can override it to exercise
// mismatch and unverified callback outcomes without a network identity call.
//
// DiscoverIdentity remains independently observable for recovery promotion, where full
// network verification is still required outside the callback budget. onRefresh, when set,
// fires after counting so tests can stale authority mid-exchange.
type fakeRefreshClient struct {
	calls            int
	lastRefreshToken string
	result           codexauth.RefreshResult
	err              error

	matchIdentity   codexauth.Identity            // default recovery DiscoverIdentity answer
	identityByToken map[string]codexauth.Identity // per-token recovery identity override
	errByToken      map[string]error              // per-token recovery DiscoverIdentity error
	discoverCalls   int
	freshClaims     codexauth.FreshAccessTokenIdentityClaims
	freshClaimsSet  bool
	onRefresh       func() // fires inside Refresh, after counting (audit #3b hook)
}

func (f *fakeRefreshClient) Refresh(_ context.Context, refreshToken string) (codexauth.RefreshResult, error) {
	f.calls++
	f.lastRefreshToken = refreshToken
	if f.onRefresh != nil {
		f.onRefresh()
	}
	if f.err != nil {
		return codexauth.RefreshResult{}, f.err
	}
	result := f.result
	if f.freshClaimsSet {
		result.IdentityClaims = f.freshClaims
	} else if result.IdentityClaims == (codexauth.FreshAccessTokenIdentityClaims{}) {
		result.IdentityClaims = freshClaimsForIdentity(f.matchIdentity)
	}
	return result, nil
}

func (f *fakeRefreshClient) DiscoverIdentity(_ context.Context, accessToken string) (codexauth.Identity, error) {
	f.discoverCalls++
	if err, ok := f.errByToken[accessToken]; ok {
		return codexauth.Identity{}, err
	}
	if id, ok := f.identityByToken[accessToken]; ok {
		return id, nil
	}
	return f.matchIdentity, nil
}

// refreshHookStore wraps *store.Queries to force a CommitCodexRefresh failure path: an
// onCommit hook runs a side effect just before the real commit (used to move the account
// under a live refresher), and commitErr, when set, replaces the commit outcome outright.
// Embedding *store.Queries keeps the wrapper a full Store (+ the codex surfaces).
type refreshHookStore struct {
	*store.Queries
	onCommit  func()
	commitErr error
}

func (h refreshHookStore) CommitCodexRefresh(ctx context.Context, arg store.CommitCodexRefreshParams) (store.CommitCodexRefreshRow, error) {
	if h.onCommit != nil {
		h.onCommit()
	}
	if h.commitErr != nil {
		return store.CommitCodexRefreshRow{}, h.commitErr
	}
	return h.Queries.CommitCodexRefresh(ctx, arg)
}

// intentReadHookStore wraps *store.Queries to fire a side effect right after the step-2
// GetCodexRefreshIntent read observes its result. It reproduces the same-operationID
// concurrent-retry race: the hook drives a rival attempt of the SAME op to full commit in
// the exact window after this attempt has read NoRows but before it advances.
type intentReadHookStore struct {
	*store.Queries
	onIntentRead func(store.CodexRefreshIntent, error)
}

func (h intentReadHookStore) GetCodexRefreshIntent(ctx context.Context, arg store.GetCodexRefreshIntentParams) (store.CodexRefreshIntent, error) {
	it, err := h.Queries.GetCodexRefreshIntent(ctx, arg)
	if h.onIntentRead != nil {
		h.onIntentRead(it, err)
	}
	return it, err
}

// releaseHookStore wraps *store.Queries to fire a side effect during the subscription
// token-read step of ReleaseCodexCredential, after the FIRST authorize has passed and
// before the recheck-before-release — so a test can stale authority mid-call and pin the
// recheck (deleting the recheck makes such a test leak the token and fail).
type releaseHookStore struct {
	*store.Queries
	onAccountRead func()
}

func (h releaseHookStore) GetCodexProviderAccountByID(ctx context.Context, arg store.GetCodexProviderAccountByIDParams) (store.CodexProviderAccount, error) {
	acct, err := h.Queries.GetCodexProviderAccountByID(ctx, arg)
	if h.onAccountRead != nil {
		h.onAccountRead()
	}
	return acct, err
}

// recoverySlotHookStore wraps *store.Queries to make the FIRST failFirst
// SetCodexRecoverySlot calls fail with a transient error, so a test can prove the resilient
// recovery-slot persistence (PRD #1147 M4, defect 4): a bounded synchronous retry followed
// by a background retry that eventually lands the write. Pointer receiver so `calls` counts
// across the whole flow (sync + background attempts).
type recoverySlotHookStore struct {
	*store.Queries
	failFirst int
	calls     int
}

func (h *recoverySlotHookStore) SetCodexRecoverySlot(ctx context.Context, arg store.SetCodexRecoverySlotParams) (int64, error) {
	h.calls++
	if h.calls <= h.failFirst {
		return 0, errors.New("recovery write blip")
	}
	return h.Queries.SetCodexRecoverySlot(ctx, arg)
}

// recoverySlotZeroStore simulates a recovery CAS whose operation fence moved before
// the write. The SQL call succeeds but affects zero rows, which must never be reported
// as persisted recovery material.
type recoverySlotZeroStore struct {
	*store.Queries
	calls int
}

func (h *recoverySlotZeroStore) SetCodexRecoverySlot(context.Context, store.SetCodexRecoverySlotParams) (int64, error) {
	h.calls++
	return 0, nil
}

// refreshFixture is a fully-seeded, linked subscription Codex run ready to refresh, with
// the ORIGINAL access/refresh tokens known so a test can assert the merged reseal.
type refreshFixture struct {
	userID           uuid.UUID
	workerID         uuid.UUID
	runID            uuid.UUID
	aliasID          uuid.UUID
	accountID        uuid.UUID
	svc              *Service
	wkr              store.Worker
	accessToken      string // the account's committed access token at generation 0
	prevRefreshToken string // the refresh token the first exchange must be handed
	providerUserID   string // frozen tuple used by claim and recovery checks
	workspaceAcctID  string
}

// newRefreshFixture seeds a linked subscription account at generation 0 with known tokens,
// a claimed run frozen to it, and a Service wired with the given fake refresh client.
func newRefreshFixture(t *testing.T, env codexTestEnv, fake CodexRefreshClient) refreshFixture {
	t.Helper()
	userID, workerID, repoID := env.seedCodexInfra(t)

	access := codexToken("access")
	refresh := codexToken("refresh")
	aliasID := env.seedStagingAlias(t, userID, "codex-refresh-"+uuid.NewString(), codexLoginBlob{AccessToken: access, RefreshToken: refresh})

	fid := newFakeCodexIdentity()
	fid.idByToken[access] = codexauth.Identity{
		ProviderUserID:     "user-" + uuid.NewString(),
		WorkspaceAccountID: "acct-" + uuid.NewString(),
	}
	r := NewCodexReconciler(env.q, nil, env.box, fid, env.pool)
	if err := r.ReconcileCodexAuthIdentity(env.ctx, userID, aliasID); err != nil {
		t.Fatalf("reconcile subscription alias: %v", err)
	}
	st, err := env.q.GetCodexCredentialState(env.ctx, store.GetCodexCredentialStateParams{UserSecretID: aliasID, UserID: userID})
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if !st.ProviderAccountID.Valid {
		t.Fatal("subscription alias must link an account")
	}
	accountID := uuid.UUID(st.ProviderAccountID.Bytes)

	runID := env.seedCodexRun(t, userID, workerID, repoID)
	svc := &Service{q: env.q, box: env.box, codexRefresh: fake}
	if err := svc.FreezeCodexBinding(env.ctx, userID, runID, aliasID, codexAuthModeSubscription); err != nil {
		t.Fatalf("FreezeCodexBinding: %v", err)
	}

	// Pin the fake's default fresh-token claims and recovery DiscoverIdentity answer to the
	// account's frozen tuple. Tests can override freshClaims for callback mismatch/unverified
	// outcomes or identityByToken/errByToken for recovery verification.
	acct := env.mustAccount(t, userID, accountID)
	setFakeMatchIdentity(fake, codexauth.Identity{ProviderUserID: acct.ProviderUserID, WorkspaceAccountID: acct.WorkspaceAccountID})

	return refreshFixture{
		userID:           userID,
		workerID:         workerID,
		runID:            runID,
		aliasID:          aliasID,
		accountID:        accountID,
		svc:              svc,
		wkr:              store.Worker{ID: workerID, UserID: userID},
		accessToken:      access,
		prevRefreshToken: refresh,
		providerUserID:   acct.ProviderUserID,
		workspaceAcctID:  acct.WorkspaceAccountID,
	}
}

// setFakeMatchIdentity pins the default fresh-token claims and recovery discovery answer on
// whichever concrete fake a fixture was handed.
func setFakeMatchIdentity(fake CodexRefreshClient, id codexauth.Identity) {
	switch c := fake.(type) {
	case *fakeRefreshClient:
		c.matchIdentity = id
	case *mintingRefreshClient:
		c.matchIdentity = id
	}
}

func freshClaimsForIdentity(id codexauth.Identity) codexauth.FreshAccessTokenIdentityClaims {
	return codexauth.FreshAccessTokenIdentityClaims{
		ChatGPTAccountID: id.WorkspaceAccountID,
		ChatGPTUserID:    id.ProviderUserID,
		AuthUserID:       id.ProviderUserID,
	}
}

// accountBlob opens and decodes an account's current committed sealed_login (master box).
func (e codexTestEnv) accountBlob(t *testing.T, userID, accountID uuid.UUID) codexLoginBlob {
	t.Helper()
	acct, err := e.q.GetCodexProviderAccountByID(e.ctx, store.GetCodexProviderAccountByIDParams{UserID: userID, ID: accountID})
	if err != nil {
		t.Fatalf("read account: %v", err)
	}
	plain, err := secretopen.OpenSealed(nil, e.box, userID, store.KindCodexAuth, acct.SealedWith, acct.SealedLogin)
	if err != nil {
		t.Fatalf("open account login: %v", err)
	}
	var b codexLoginBlob
	if err := json.Unmarshal(plain, &b); err != nil {
		t.Fatalf("decode account login: %v", err)
	}
	return b
}

func (e codexTestEnv) mustAccount(t *testing.T, userID, accountID uuid.UUID) store.CodexProviderAccount {
	t.Helper()
	acct, err := e.q.GetCodexProviderAccountByID(e.ctx, store.GetCodexProviderAccountByIDParams{UserID: userID, ID: accountID})
	if err != nil {
		t.Fatalf("read account: %v", err)
	}
	return acct
}

// TestCoordinatedCodexRefreshAdvancesLiveDB proves the ADVANCE arm: an op whose observed
// generation equals the account's rotates N→N+1, returns the freshly-committed access
// token, and reseals the merged blob (new access token + the provider-rotated refresh
// token), with exactly ONE provider call handed the previous refresh token.
func TestCoordinatedCodexRefreshAdvancesLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	rotatedRefresh := codexToken("refresh-rotated")
	newAccess := codexToken("access-new")
	fake := &fakeRefreshClient{result: codexauth.RefreshResult{AccessToken: newAccess, RefreshToken: &rotatedRefresh}}
	f := newRefreshFixture(t, env, fake)
	capw := env.mintCap(t, f.runID, f.workerID)

	res, err := f.svc.CoordinatedCodexRefresh(env.ctx, f.wkr, f.runID, capw, uuid.New(), 0)
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if res.Outcome != CodexRefreshAdvanced {
		t.Fatalf("outcome = %v, want advanced", res.Outcome)
	}
	if res.Generation != 1 {
		t.Fatalf("generation = %d, want 1", res.Generation)
	}
	if res.AccessToken != newAccess {
		t.Fatalf("access token = %q, want the freshly-exchanged %q", res.AccessToken, newAccess)
	}
	if fake.calls != 1 {
		t.Fatalf("provider calls = %d, want exactly 1", fake.calls)
	}
	if fake.lastRefreshToken != f.prevRefreshToken {
		t.Fatalf("exchange was handed %q, want the previous refresh token %q", fake.lastRefreshToken, f.prevRefreshToken)
	}

	// The resealed committed blob carries the new access token AND the rotated refresh token.
	blob := env.accountBlob(t, f.userID, f.accountID)
	if blob.AccessToken != newAccess || blob.RefreshToken != rotatedRefresh {
		t.Fatalf("committed blob = (%q,%q), want (%q,%q)", blob.AccessToken, blob.RefreshToken, newAccess, rotatedRefresh)
	}
	// The account settled back to idle so the next cycle can acquire.
	if acct := env.mustAccount(t, f.userID, f.accountID); acct.CoordState != "idle" {
		t.Fatalf("coord_state = %q, want idle after a committed refresh", acct.CoordState)
	}
}

// TestCoordinatedCodexRefreshAdvanceRetainsRefreshTokenLiveDB proves the merged reseal
// when the provider OMITS a rotated refresh token: the committed blob installs the new
// access token but RETAINS the previous refresh token.
func TestCoordinatedCodexRefreshAdvanceRetainsRefreshTokenLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	newAccess := codexToken("access-new")
	fake := &fakeRefreshClient{result: codexauth.RefreshResult{AccessToken: newAccess, RefreshToken: nil}}
	f := newRefreshFixture(t, env, fake)
	capw := env.mintCap(t, f.runID, f.workerID)

	if _, err := f.svc.CoordinatedCodexRefresh(env.ctx, f.wkr, f.runID, capw, uuid.New(), 0); err != nil {
		t.Fatalf("advance: %v", err)
	}
	blob := env.accountBlob(t, f.userID, f.accountID)
	if blob.AccessToken != newAccess {
		t.Fatalf("committed access token = %q, want %q", blob.AccessToken, newAccess)
	}
	if blob.RefreshToken != f.prevRefreshToken {
		t.Fatalf("committed refresh token = %q, want the retained previous %q", blob.RefreshToken, f.prevRefreshToken)
	}
}

// TestCoordinatedCodexRefreshReplaysLiveDB proves idempotent REPLAY: re-driving the SAME
// operation id returns the same committed token with the provider called exactly ONCE.
func TestCoordinatedCodexRefreshReplaysLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	newAccess := codexToken("access-new")
	fake := &fakeRefreshClient{result: codexauth.RefreshResult{AccessToken: newAccess}}
	f := newRefreshFixture(t, env, fake)
	capw := env.mintCap(t, f.runID, f.workerID)
	op := uuid.New()

	first, err := f.svc.CoordinatedCodexRefresh(env.ctx, f.wkr, f.runID, capw, op, 0)
	if err != nil {
		t.Fatalf("first (advance): %v", err)
	}
	second, err := f.svc.CoordinatedCodexRefresh(env.ctx, f.wkr, f.runID, capw, op, 0)
	if err != nil {
		t.Fatalf("second (replay): %v", err)
	}
	if second.Outcome != CodexRefreshReplayed {
		t.Fatalf("outcome = %v, want replayed", second.Outcome)
	}
	if second.AccessToken != first.AccessToken || second.Generation != first.Generation {
		t.Fatalf("replay returned (%q,%d), want the committed (%q,%d)", second.AccessToken, second.Generation, first.AccessToken, first.Generation)
	}
	if fake.calls != 1 {
		t.Fatalf("provider calls = %d, want exactly 1 (replay spends none)", fake.calls)
	}
}

// TestCoordinatedCodexRefreshReconcilesBehindLiveDB proves RECONCILE: an op whose observed
// generation is behind the current one returns the current committed token with ZERO
// provider calls.
func TestCoordinatedCodexRefreshReconcilesBehindLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	newAccess := codexToken("access-new")
	fake := &fakeRefreshClient{result: codexauth.RefreshResult{AccessToken: newAccess}}
	f := newRefreshFixture(t, env, fake)
	capw := env.mintCap(t, f.runID, f.workerID)

	if _, err := f.svc.CoordinatedCodexRefresh(env.ctx, f.wkr, f.runID, capw, uuid.New(), 0); err != nil {
		t.Fatalf("advance to gen 1: %v", err)
	}
	callsAfterAdvance := fake.calls

	res, err := f.svc.CoordinatedCodexRefresh(env.ctx, f.wkr, f.runID, capw, uuid.New(), 0)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.Outcome != CodexRefreshReconciled {
		t.Fatalf("outcome = %v, want reconciled", res.Outcome)
	}
	if res.Generation != 1 || res.AccessToken != newAccess {
		t.Fatalf("reconcile returned (%q,%d), want the current committed (%q,1)", res.AccessToken, res.Generation, newAccess)
	}
	if fake.calls != callsAfterAdvance {
		t.Fatalf("provider calls = %d, want unchanged %d (reconcile spends none)", fake.calls, callsAfterAdvance)
	}
}

// TestCoordinatedCodexRefreshContendedLiveDB proves a live lease held by another operation
// blocks a parallel rotation: acquire returns 0 rows → contended, NO provider call, NO
// token.
func TestCoordinatedCodexRefreshContendedLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fake := &fakeRefreshClient{result: codexauth.RefreshResult{AccessToken: codexToken("access-new")}}
	f := newRefreshFixture(t, env, fake)
	capw := env.mintCap(t, f.runID, f.workerID)

	// Another operation already holds a live lease on the account.
	env.exec(`UPDATE codex_provider_account SET coord_state='in_progress', coord_operation_id=$2, lease_deadline=now() + interval '1 hour' WHERE id=$1`,
		f.accountID, uuid.New())

	res, err := f.svc.CoordinatedCodexRefresh(env.ctx, f.wkr, f.runID, capw, uuid.New(), 0)
	if !errors.Is(err, ErrCodexRefreshContended) {
		t.Fatalf("err = %v, want ErrCodexRefreshContended", err)
	}
	if res.Outcome != CodexRefreshContended || res.AccessToken != "" {
		t.Fatalf("result = %+v, want a contended outcome with no token", res)
	}
	if fake.calls != 0 {
		t.Fatalf("provider calls = %d, want 0 (never rotate under a live lease)", fake.calls)
	}
}

// TestCoordinatedCodexRefreshSameOpConcurrentRetryDoesNotStrandLeaseLiveDB proves the
// finding-1 fix: with the durable intent recorded BEFORE the lease is acquired, a
// same-operationID concurrent retry can no longer strand the lease and quarantine the
// account. It reproduces the adversarial interleaving in-process: attempt B reads its
// intent as NoRows (step 2), then in that exact window a rival attempt A of the SAME op
// fully advances (commit + reset to idle); B continues into the advance path, hits the
// pre-lease duplicate insert (23505), and replays WITHOUT ever acquiring a lease. The
// account must end 'idle' (never stranded 'in_progress'/'quarantined'), B must get the
// committed token, and the provider must have been called exactly once (by A).
func TestCoordinatedCodexRefreshSameOpConcurrentRetryDoesNotStrandLeaseLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	newAccess := codexToken("access-new")
	fake := &fakeRefreshClient{result: codexauth.RefreshResult{AccessToken: newAccess}}
	f := newRefreshFixture(t, env, fake)
	capw := env.mintCap(t, f.runID, f.workerID)
	op := uuid.New()

	// Attempt A shares the fixture but runs on the un-hooked store, so its own step-2 read
	// does not recurse into the hook below.
	attemptA := &Service{q: env.q, box: env.box, codexRefresh: fake}

	var raced bool
	f.svc.q = intentReadHookStore{
		Queries: env.q,
		onIntentRead: func(_ store.CodexRefreshIntent, err error) {
			if raced || !errors.Is(err, pgx.ErrNoRows) {
				return
			}
			raced = true
			// The rival attempt A of the SAME op fully advances (commit + reset to idle) in
			// the window after B read NoRows and before B advances.
			if _, aerr := attemptA.CoordinatedCodexRefresh(env.ctx, f.wkr, f.runID, capw, op, 0); aerr != nil {
				t.Errorf("attempt A advance: %v", aerr)
			}
		},
	}

	res, err := f.svc.CoordinatedCodexRefresh(env.ctx, f.wkr, f.runID, capw, op, 0)
	if err != nil {
		t.Fatalf("attempt B: %v", err)
	}
	// B did not exchange: it replayed A's committed rotation (a reconcile is equally valid).
	if res.Outcome != CodexRefreshReplayed && res.Outcome != CodexRefreshReconciled {
		t.Fatalf("outcome = %v, want replayed/reconciled (no second exchange)", res.Outcome)
	}
	if res.AccessToken != newAccess || res.Generation != 1 {
		t.Fatalf("B returned (%q,%d), want the committed (%q,1)", res.AccessToken, res.Generation, newAccess)
	}
	if fake.calls != 1 {
		t.Fatalf("provider calls = %d, want exactly 1 (only attempt A exchanged)", fake.calls)
	}
	// The lease was NEVER stranded: the account settled to idle, not 'in_progress' (which
	// the reconcile pass would then quarantine, bricking future refreshes).
	if acct := env.mustAccount(t, f.userID, f.accountID); acct.CoordState != "idle" {
		t.Fatalf("coord_state = %q, want idle (a stranded lease would leave in_progress/quarantined)", acct.CoordState)
	}
}

// TestCoordinatedCodexRefreshStaleLoserCannotExchangeLiveDB proves the M4 generation-guard
// fix on AcquireCodexRefreshLease closes the stale-generation redundant-exchange window
// (invariant D5: exactly ONE provider rotation per stale-generation burst). It forces the
// adversarial interleaving deterministically: op B (a DISTINCT operation from A) reads the
// account at generation 0, then in the window right after B's step-2 intent read — before B
// takes the lease — a rival op A runs a FULL cycle (exchange → commit gen 1 → reset to
// idle). B then proceeds into the advance path with its snapshotted from_generation=0 and
// tries to acquire the now-freshly-idle lease. WITHOUT the guard, B would win the idle lease
// at gen 1 and perform a SECOND provider exchange with its stale gen-0 refresh token (its
// commit would then be CAS-rejected, but the provider was already spent twice). WITH the
// guard, B's acquire matches generation=0 against the account's current generation 1, wins 0
// rows, re-reads, sees the account advanced, and reconciles to A's committed token with NO
// provider call. The assertion that pins the fix: the provider is called EXACTLY ONCE (by A)
// — reverting the WHERE-clause generation guard makes B exchange a second time (fake.calls==2)
// and this test fail.
func TestCoordinatedCodexRefreshStaleLoserCannotExchangeLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	rotatedRefresh := codexToken("refresh-rotated")
	newAccess := codexToken("access-new")
	fake := &fakeRefreshClient{result: codexauth.RefreshResult{AccessToken: newAccess, RefreshToken: &rotatedRefresh}}
	f := newRefreshFixture(t, env, fake)
	capw := env.mintCap(t, f.runID, f.workerID)

	// The winner op A shares the fixture but runs on the un-hooked store, so its own step-2
	// read does not recurse into the hook below. It uses a DISTINCT operation id from B.
	attemptA := &Service{q: env.q, box: env.box, codexRefresh: fake}
	opA := uuid.New()
	opB := uuid.New()

	var raced bool
	f.svc.q = intentReadHookStore{
		Queries: env.q,
		onIntentRead: func(_ store.CodexRefreshIntent, err error) {
			// Fire only on B's own first (NoRows) intent read, after B has already read the
			// account at generation 0 (step 1) but before B acquires the lease (step 4).
			if raced || !errors.Is(err, pgx.ErrNoRows) {
				return
			}
			raced = true
			if _, aerr := attemptA.CoordinatedCodexRefresh(env.ctx, f.wkr, f.runID, capw, opA, 0); aerr != nil {
				t.Errorf("attempt A advance: %v", aerr)
			}
		},
	}

	res, err := f.svc.CoordinatedCodexRefresh(env.ctx, f.wkr, f.runID, capw, opB, 0)
	if err != nil {
		t.Fatalf("attempt B: %v", err)
	}
	// B did not exchange: the generation-guarded acquire failed, so B reconciled to A's
	// committed rotation rather than re-spending the stale gen-0 refresh token.
	if res.Outcome != CodexRefreshReconciled {
		t.Fatalf("outcome = %v, want reconciled (the stale loser must not exchange)", res.Outcome)
	}
	if res.AccessToken != newAccess || res.Generation != 1 {
		t.Fatalf("B returned (%q,%d), want A's committed (%q,1)", res.AccessToken, res.Generation, newAccess)
	}
	// The invariant the fix protects: exactly ONE provider rotation for this stale-generation
	// burst. Reverting the WHERE-clause guard lets B exchange a second time → fake.calls==2.
	if fake.calls != 1 {
		t.Fatalf("provider calls = %d, want exactly 1 (only the winner A exchanged; the stale loser must not)", fake.calls)
	}
	// The account settled to A's committed generation and back to idle; B stranded nothing.
	if acct := env.mustAccount(t, f.userID, f.accountID); acct.CoordState != "idle" || acct.Generation != 1 {
		t.Fatalf("account = (state=%q gen=%d), want (idle, 1)", acct.CoordState, acct.Generation)
	}
}

// TestCoordinatedCodexRefreshQuarantinedHolderCannotCommitLiveDB proves the store's
// quarantine guard end to end: a presumed-dead holder whose lease was quarantined mid-
// flight CANNOT commit — the CAS fails, the freshly-rotated material is protected into the
// recovery slot, and the service surfaces a quarantined outcome with NO token.
func TestCoordinatedCodexRefreshQuarantinedHolderCannotCommitLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	newAccess := codexToken("access-new")
	fake := &fakeRefreshClient{result: codexauth.RefreshResult{AccessToken: newAccess, RefreshToken: nil}}
	f := newRefreshFixture(t, env, fake)
	capw := env.mintCap(t, f.runID, f.workerID)

	// Between the provider exchange and the commit, the lease expires and is reaped to
	// 'quarantined' (simulated by the hook firing just before the real commit runs).
	f.svc.q = refreshHookStore{
		Queries: env.q,
		onCommit: func() {
			env.exec(`UPDATE codex_provider_account SET coord_state='quarantined' WHERE id=$1`, f.accountID)
		},
	}

	res, err := f.svc.CoordinatedCodexRefresh(env.ctx, f.wkr, f.runID, capw, uuid.New(), 0)
	if !errors.Is(err, ErrCodexRefreshQuarantined) {
		t.Fatalf("err = %v, want ErrCodexRefreshQuarantined", err)
	}
	if res.Outcome != CodexRefreshQuarantined || res.AccessToken != "" {
		t.Fatalf("result = %+v, want a quarantined outcome with no token", res)
	}
	if fake.calls != 1 {
		t.Fatalf("provider calls = %d, want exactly 1", fake.calls)
	}
	acct := env.mustAccount(t, f.userID, f.accountID)
	if acct.Generation != 0 {
		t.Fatalf("generation = %d, want 0 (a quarantined holder must not advance)", acct.Generation)
	}
	if len(acct.RecoverySealed) == 0 || !acct.RecoveryGeneration.Valid || acct.RecoveryGeneration.Int64 != 0 {
		t.Fatalf("recovery slot not protected: sealed=%d gen=%v", len(acct.RecoverySealed), acct.RecoveryGeneration)
	}
}

// TestCoordinatedCodexRefreshStaleCASReconcilesLiveDB proves a stale CAS (another op
// committed while ours was mid-flight, so from_generation no longer matches) does NOT
// double-advance: it reconciles to the now-current committed generation and re-spends
// nothing.
func TestCoordinatedCodexRefreshStaleCASReconcilesLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fake := &fakeRefreshClient{result: codexauth.RefreshResult{AccessToken: codexToken("access-ours")}}
	f := newRefreshFixture(t, env, fake)
	capw := env.mintCap(t, f.runID, f.workerID)

	// Simulate a rival op landing a commit (generation 0→1 with its own material) just
	// before our commit runs, so our CAS on from_generation=0 fails.
	jumpedAccess := codexToken("access-jumped")
	rivalBlob, err := json.Marshal(codexLoginBlob{AccessToken: jumpedAccess, RefreshToken: codexToken("refresh-jumped")}) //nolint:gosec // G117: synthetic rival login fixture, master-sealed below
	if err != nil {
		t.Fatalf("marshal rival blob: %v", err)
	}
	sealed, err := env.box.Seal(rivalBlob)
	if err != nil {
		t.Fatalf("seal rival blob: %v", err)
	}
	f.svc.q = refreshHookStore{
		Queries: env.q,
		onCommit: func() {
			env.exec(`UPDATE codex_provider_account
			          SET generation=1, committed_generation=1, sealed_login=$2, sealed_with='master', coord_state='idle', coord_operation_id=NULL
			          WHERE id=$1`, f.accountID, sealed)
		},
	}

	res, err := f.svc.CoordinatedCodexRefresh(env.ctx, f.wkr, f.runID, capw, uuid.New(), 0)
	if err != nil {
		t.Fatalf("stale-CAS refresh: %v", err)
	}
	if res.Outcome != CodexRefreshReconciled {
		t.Fatalf("outcome = %v, want reconciled", res.Outcome)
	}
	if res.Generation != 1 || res.AccessToken != jumpedAccess {
		t.Fatalf("reconcile returned (%q,%d), want the rival's committed (%q,1)", res.AccessToken, res.Generation, jumpedAccess)
	}
	if acct := env.mustAccount(t, f.userID, f.accountID); acct.Generation != 1 {
		t.Fatalf("generation = %d, want 1 (a stale CAS must not double-advance)", acct.Generation)
	}
}

// TestCoordinatedCodexRefreshPersistenceFailureLiveDB proves a provider-success-but-commit-
// failure (a DB error after the exchange) protects the new material in the recovery slot
// and returns NO token.
func TestCoordinatedCodexRefreshPersistenceFailureLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fake := &fakeRefreshClient{result: codexauth.RefreshResult{AccessToken: codexToken("access-new")}}
	f := newRefreshFixture(t, env, fake)
	capw := env.mintCap(t, f.runID, f.workerID)

	f.svc.q = refreshHookStore{Queries: env.q, commitErr: errors.New("commit exploded")}

	res, err := f.svc.CoordinatedCodexRefresh(env.ctx, f.wkr, f.runID, capw, uuid.New(), 0)
	if !errors.Is(err, ErrCodexRefreshQuarantined) {
		t.Fatalf("err = %v, want ErrCodexRefreshQuarantined", err)
	}
	if res.AccessToken != "" {
		t.Fatalf("token = %q, want NO token on a persistence failure", res.AccessToken)
	}
	if fake.calls != 1 {
		t.Fatalf("provider calls = %d, want exactly 1", fake.calls)
	}
	acct := env.mustAccount(t, f.userID, f.accountID)
	if len(acct.RecoverySealed) == 0 || acct.Generation != 0 {
		t.Fatalf("recovery not protected / generation advanced: sealed=%d gen=%d", len(acct.RecoverySealed), acct.Generation)
	}
}

// TestReconcileUnresolvedCodexRefreshLiveDB proves the crash-safe recovery scan resolves a
// landed-commit intent to 'reconciled' and a no-recoverable-copy intent to 'unrecoverable'.
func TestReconcileUnresolvedCodexRefreshLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)

	t.Run("landed commit reconciles", func(t *testing.T) {
		fake := &fakeRefreshClient{}
		f := newRefreshFixture(t, env, fake)
		op := uuid.New()
		if _, err := env.q.InsertCodexRefreshIntent(env.ctx, store.InsertCodexRefreshIntentParams{
			OperationID: op, UserID: f.userID, ProviderAccountID: f.accountID, FromGeneration: 0,
		}); err != nil {
			t.Fatalf("insert intent: %v", err)
		}
		// A commit landed after this intent started.
		env.exec(`UPDATE codex_provider_account SET generation=1, committed_generation=1 WHERE id=$1`, f.accountID)

		n, err := f.svc.ReconcileUnresolvedCodexRefresh(env.ctx, f.userID, f.accountID)
		if err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if n != 1 {
			t.Fatalf("resolved = %d, want 1", n)
		}
		it := mustIntent(t, env, op, f.userID)
		if it.State != codexIntentReconciled {
			t.Fatalf("intent state = %q, want reconciled", it.State)
		}
	})

	t.Run("no recoverable copy is unrecoverable", func(t *testing.T) {
		fake := &fakeRefreshClient{}
		f := newRefreshFixture(t, env, fake)
		op := uuid.New()
		if _, err := env.q.InsertCodexRefreshIntent(env.ctx, store.InsertCodexRefreshIntentParams{
			OperationID: op, UserID: f.userID, ProviderAccountID: f.accountID, FromGeneration: 0,
		}); err != nil {
			t.Fatalf("insert intent: %v", err)
		}
		// Generation unchanged, no recovery copy: total loss.
		n, err := f.svc.ReconcileUnresolvedCodexRefresh(env.ctx, f.userID, f.accountID)
		if err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if n != 1 {
			t.Fatalf("resolved = %d, want 1", n)
		}
		it := mustIntent(t, env, op, f.userID)
		if it.State != codexIntentUnrecoverable {
			t.Fatalf("intent state = %q, want unrecoverable", it.State)
		}
	})
}

func mustIntent(t *testing.T, env codexTestEnv, op, userID uuid.UUID) store.CodexRefreshIntent {
	t.Helper()
	it, err := env.q.GetCodexRefreshIntent(env.ctx, store.GetCodexRefreshIntentParams{OperationID: op, UserID: userID})
	if err != nil {
		t.Fatalf("read intent: %v", err)
	}
	return it
}

// TestCoordinatedCodexRefreshIdentityMatchCommitsLiveDB proves complete, internally
// consistent fresh-token claims matching the frozen tuple permit the commit with no
// DiscoverIdentity network call.
func TestCoordinatedCodexRefreshIdentityMatchCommitsLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	newAccess := codexToken("access-new")
	fake := &fakeRefreshClient{result: codexauth.RefreshResult{AccessToken: newAccess}}
	f := newRefreshFixture(t, env, fake)
	capw := env.mintCap(t, f.runID, f.workerID)

	res, err := f.svc.CoordinatedCodexRefresh(env.ctx, f.wkr, f.runID, capw, uuid.New(), 0)
	if err != nil {
		t.Fatalf("match advance: %v", err)
	}
	if res.Outcome != CodexRefreshAdvanced || res.Generation != 1 || res.AccessToken != newAccess {
		t.Fatalf("result = %+v, want advanced/gen1/%q", res, newAccess)
	}
	if fake.calls != 1 || fake.discoverCalls != 0 {
		t.Fatalf("provider calls = (refresh %d, discover %d), want (1, 0)", fake.calls, fake.discoverCalls)
	}
	if blob := env.accountBlob(t, f.userID, f.accountID); blob.AccessToken != newAccess {
		t.Fatalf("committed access token = %q, want %q", blob.AccessToken, newAccess)
	}
}

// TestCoordinatedCodexRefreshAccountClaimMismatchRetainsLiveDB proves a matching verified
// user plus a different workspace claim is not enough evidence for destructive quarantine.
// The material is retained for network recovery and no token is returned.
func TestCoordinatedCodexRefreshAccountClaimMismatchRetainsLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	newAccess := codexToken("access-new")
	fake := &fakeRefreshClient{result: codexauth.RefreshResult{AccessToken: newAccess}}
	f := newRefreshFixture(t, env, fake)
	fake.freshClaimsSet = true
	fake.freshClaims = freshClaimsForIdentity(codexauth.Identity{
		ProviderUserID:     f.providerUserID,
		WorkspaceAccountID: "other-acct-" + uuid.NewString(),
	})
	capw := env.mintCap(t, f.runID, f.workerID)
	op := uuid.New()

	res, err := f.svc.CoordinatedCodexRefresh(env.ctx, f.wkr, f.runID, capw, op, 0)
	if !errors.Is(err, ErrCodexRefreshQuarantined) {
		t.Fatalf("err = %v, want ErrCodexRefreshQuarantined", err)
	}
	if errors.Is(err, ErrCodexRefreshUnrecoverable) {
		t.Fatalf("err = %v, must not be unrecoverable for an unverified account claim", err)
	}
	if res.Outcome != CodexRefreshQuarantined || res.AccessToken != "" {
		t.Fatalf("result = %+v, want quarantined outcome with no token", res)
	}
	if fake.calls != 1 || fake.discoverCalls != 0 {
		t.Fatalf("provider calls = (refresh %d, discover %d), want (1, 0)", fake.calls, fake.discoverCalls)
	}
	acct := env.mustAccount(t, f.userID, f.accountID)
	if acct.Generation != 0 || acct.CoordState != codexCoordQuarantined {
		t.Fatalf("account state = generation %d coord %q, want generation 0 quarantined", acct.Generation, acct.CoordState)
	}
	if len(acct.RecoverySealed) == 0 || !acct.RecoveryGeneration.Valid || acct.RecoveryGeneration.Int64 != 0 {
		t.Fatalf("recovery slot not retained: sealed=%d gen=%v", len(acct.RecoverySealed), acct.RecoveryGeneration)
	}
	if blob := env.openSealedBlob(t, f.userID, acct.RecoverySealed, acct.RecoverySealedWith.String); blob.AccessToken != newAccess {
		t.Fatalf("recovery blob access token = %q, want %q", blob.AccessToken, newAccess)
	}
	if it := mustIntent(t, env, op, f.userID); it.State != codexIntentRotating {
		t.Fatalf("intent state = %q, want rotating for network recovery", it.State)
	}
}

// A verified user mismatch remains destructive even when the workspace claim also differs;
// account uncertainty must not hide a same-workspace/different-user rejection.
func TestCoordinatedCodexRefreshUserClaimMismatchQuarantinesLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	newAccess := codexToken("access-new")
	fake := &fakeRefreshClient{result: codexauth.RefreshResult{AccessToken: newAccess}}
	f := newRefreshFixture(t, env, fake)
	fake.freshClaimsSet = true
	fake.freshClaims = freshClaimsForIdentity(codexauth.Identity{
		ProviderUserID:     "other-user-" + uuid.NewString(),
		WorkspaceAccountID: "other-acct-" + uuid.NewString(),
	})
	capw := env.mintCap(t, f.runID, f.workerID)
	op := uuid.New()

	res, err := f.svc.CoordinatedCodexRefresh(env.ctx, f.wkr, f.runID, capw, op, 0)
	if !errors.Is(err, ErrCodexRefreshUnrecoverable) {
		t.Fatalf("err = %v, want ErrCodexRefreshUnrecoverable", err)
	}
	if res.Outcome != CodexRefreshQuarantined || res.AccessToken != "" {
		t.Fatalf("result = %+v, want quarantined outcome with no token", res)
	}
	if fake.calls != 1 || fake.discoverCalls != 0 {
		t.Fatalf("provider calls = (refresh %d, discover %d), want (1, 0)", fake.calls, fake.discoverCalls)
	}
	acct := env.mustAccount(t, f.userID, f.accountID)
	if acct.Generation != 0 || acct.CoordState != codexCoordQuarantined || len(acct.RecoverySealed) != 0 {
		t.Fatalf("mismatch account state = generation %d coord %q recovery %d bytes", acct.Generation, acct.CoordState, len(acct.RecoverySealed))
	}
	if it := mustIntent(t, env, op, f.userID); it.State != codexIntentUnrecoverable {
		t.Fatalf("intent state = %q, want unrecoverable", it.State)
	}
}

// TestCoordinatedCodexRefreshUnverifiedClaimsRetainsLiveDB proves malformed, missing, and
// internally inconsistent fresh-token claims release no token and retain the rotated login
// for a later full network verification.
func TestCoordinatedCodexRefreshUnverifiedClaimsRetainsLiveDB(t *testing.T) {
	tests := []struct {
		name   string
		claims func(refreshFixture) codexauth.FreshAccessTokenIdentityClaims
	}{
		{name: "malformed parser zero value", claims: func(refreshFixture) codexauth.FreshAccessTokenIdentityClaims {
			return codexauth.FreshAccessTokenIdentityClaims{}
		}},
		{name: "missing auth user", claims: func(f refreshFixture) codexauth.FreshAccessTokenIdentityClaims {
			return codexauth.FreshAccessTokenIdentityClaims{ChatGPTAccountID: f.workspaceAcctID, ChatGPTUserID: f.providerUserID}
		}},
		{name: "ambiguous user claims", claims: func(f refreshFixture) codexauth.FreshAccessTokenIdentityClaims {
			return codexauth.FreshAccessTokenIdentityClaims{ChatGPTAccountID: f.workspaceAcctID, ChatGPTUserID: f.providerUserID, AuthUserID: "other-user-" + uuid.NewString()}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := setupCodexLiveDB(t)
			newAccess := codexToken("access-new")
			fake := &fakeRefreshClient{result: codexauth.RefreshResult{AccessToken: newAccess}}
			f := newRefreshFixture(t, env, fake)
			fake.freshClaimsSet = true
			fake.freshClaims = tt.claims(f)
			capw := env.mintCap(t, f.runID, f.workerID)
			op := uuid.New()

			res, err := f.svc.CoordinatedCodexRefresh(env.ctx, f.wkr, f.runID, capw, op, 0)
			if !errors.Is(err, ErrCodexRefreshQuarantined) {
				t.Fatalf("err = %v, want ErrCodexRefreshQuarantined", err)
			}
			if res.Outcome != CodexRefreshQuarantined || res.AccessToken != "" {
				t.Fatalf("result = %+v, want quarantined outcome with no token", res)
			}
			if fake.calls != 1 || fake.discoverCalls != 0 {
				t.Fatalf("provider calls = (refresh %d, discover %d), want (1, 0)", fake.calls, fake.discoverCalls)
			}
			acct := env.mustAccount(t, f.userID, f.accountID)
			if acct.Generation != 0 {
				t.Fatalf("generation = %d, want 0", acct.Generation)
			}
			if len(acct.RecoverySealed) == 0 || !acct.RecoveryGeneration.Valid || acct.RecoveryGeneration.Int64 != 0 {
				t.Fatalf("recovery slot not retained: sealed=%d gen=%v", len(acct.RecoverySealed), acct.RecoveryGeneration)
			}
			if blob := env.openSealedBlob(t, f.userID, acct.RecoverySealed, acct.SealedWith); blob.AccessToken != newAccess {
				t.Fatalf("recovery blob access token = %q, want %q", blob.AccessToken, newAccess)
			}
			if it := mustIntent(t, env, op, f.userID); it.State != codexIntentRotating {
				t.Fatalf("intent state = %q, want rotating", it.State)
			}
		})
	}
}

// TestCoordinatedCodexRefreshRecheckDiscardsTokenLiveDB (audit #3b) proves the post-exchange
// release recheck: authority is staled DURING the exchange (a hook bumps the account's
// credential_revision inside Refresh), so the durable commit STILL lands (good for the
// account) but THIS run — whose authority was lost mid-IO — is refused the token.
//
// FAILS OLD: the old CoordinatedCodexRefresh authorized only before the network and returned
// the freshly-committed token unconditionally, leaking it to a run that no longer owned it.
func TestCoordinatedCodexRefreshRecheckDiscardsTokenLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	newAccess := codexToken("access-new")
	fake := &fakeRefreshClient{result: codexauth.RefreshResult{AccessToken: newAccess}}
	f := newRefreshFixture(t, env, fake)
	capw := env.mintCap(t, f.runID, f.workerID)

	// During the exchange (before the commit and the outer recheck), a genuine account-level
	// revoke bumps credential_revision, staling this run's frozen authority.
	fake.onRefresh = func() {
		env.exec(`UPDATE codex_provider_account SET credential_revision = credential_revision + 1 WHERE id=$1`, f.accountID)
	}

	res, err := f.svc.CoordinatedCodexRefresh(env.ctx, f.wkr, f.runID, capw, uuid.New(), 0)
	if !errors.Is(err, ErrCodexAccountRevisionStale) {
		t.Fatalf("err = %v, want ErrCodexAccountRevisionStale from the recheck", err)
	}
	if res.Outcome != CodexRefreshContended || res.AccessToken != "" {
		t.Fatalf("result = %+v, want a contended outcome with NO token", res)
	}
	if fake.calls != 1 {
		t.Fatalf("provider calls = %d, want exactly 1", fake.calls)
	}
	// The durable commit STANDS: the account advanced to generation 1 and settled to idle —
	// only THIS run was refused the token, the rotation is good for the next authorized run.
	acct := env.mustAccount(t, f.userID, f.accountID)
	if acct.Generation != 1 {
		t.Fatalf("generation = %d, want 1 (the durable commit must stand)", acct.Generation)
	}
	if blob := env.accountBlob(t, f.userID, f.accountID); blob.AccessToken != newAccess {
		t.Fatalf("committed access token = %q, want the rotated %q", blob.AccessToken, newAccess)
	}
}

// TestReconcileUnresolvedCodexRefreshPromotesRecoveryLiveDB (audit #5, recovery promotion)
// proves the reconcile roll-forward: a commit-failure into the recovery slot leaves the
// account quarantined on the OLD token; a reconcile pass with a MATCHING-identity fake
// promotes the recovery material, so the account is coord-idle, the generation advanced, the
// recovery slot cleared, and its sealed_login OPENS to the NEW access token (usable).
//
// FAILS OLD: the old reconcile only resolved intents and left the account quarantined on the
// pre-rotation token — never promoting the protected material.
func TestReconcileUnresolvedCodexRefreshPromotesRecoveryLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)

	t.Run("matching recovery is promoted usable", func(t *testing.T) {
		newAccess := codexToken("access-new")
		fake := &fakeRefreshClient{result: codexauth.RefreshResult{AccessToken: newAccess}}
		f := newRefreshFixture(t, env, fake)
		capw := env.mintCap(t, f.runID, f.workerID)

		// A commit-failure after a MATCHED exchange protects the new material into recovery.
		f.svc.q = refreshHookStore{Queries: env.q, commitErr: errors.New("commit exploded")}
		op := uuid.New()
		if _, err := f.svc.CoordinatedCodexRefresh(env.ctx, f.wkr, f.runID, capw, op, 0); !errors.Is(err, ErrCodexRefreshQuarantined) {
			t.Fatalf("seed commit-failure: err = %v, want ErrCodexRefreshQuarantined", err)
		}
		pre := env.mustAccount(t, f.userID, f.accountID)
		if pre.CoordState != codexCoordQuarantined || len(pre.RecoverySealed) == 0 {
			t.Fatalf("precondition: account not quarantined-with-recovery: state=%q sealed=%d", pre.CoordState, len(pre.RecoverySealed))
		}
		if fake.discoverCalls != 0 {
			t.Fatalf("refresh callback made %d discovery calls, want 0", fake.discoverCalls)
		}

		// Reconcile on the un-hooked store (the commit hook was only for the seed) with the
		// same MATCHING-identity fake → promotion installs the recovery material.
		f.svc.q = env.q
		n, err := f.svc.ReconcileUnresolvedCodexRefresh(env.ctx, f.userID, f.accountID)
		if err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if n != 1 {
			t.Fatalf("resolved = %d, want 1", n)
		}
		if fake.discoverCalls != 1 {
			t.Fatalf("recovery discovery calls = %d, want exactly 1 before promotion", fake.discoverCalls)
		}
		acct := env.mustAccount(t, f.userID, f.accountID)
		if acct.CoordState != "idle" {
			t.Fatalf("coord_state = %q, want idle after promotion", acct.CoordState)
		}
		if acct.Generation != 1 {
			t.Fatalf("generation = %d, want 1 (promotion advances)", acct.Generation)
		}
		if len(acct.RecoverySealed) != 0 || acct.RecoveryGeneration.Valid {
			t.Fatalf("recovery slot not cleared: sealed=%d gen=%v", len(acct.RecoverySealed), acct.RecoveryGeneration)
		}
		// The account's live login now OPENS to the NEW access token — usable, not the old one.
		if blob := env.accountBlob(t, f.userID, f.accountID); blob.AccessToken != newAccess {
			t.Fatalf("promoted sealed_login access token = %q, want the new %q", blob.AccessToken, newAccess)
		}
		if it := mustIntent(t, env, op, f.userID); it.State != codexIntentReconciled {
			t.Fatalf("intent state = %q, want reconciled", it.State)
		}

		// Idempotent: a second reconcile is a no-op (account already idle, nothing to promote).
		n2, err := f.svc.ReconcileUnresolvedCodexRefresh(env.ctx, f.userID, f.accountID)
		if err != nil {
			t.Fatalf("second reconcile: %v", err)
		}
		if n2 != 0 {
			t.Fatalf("second reconcile resolved %d, want 0 (no-op)", n2)
		}
		if fake.discoverCalls != 1 {
			t.Fatalf("recovery discovery calls after no-op = %d, want 1", fake.discoverCalls)
		}
		if a2 := env.mustAccount(t, f.userID, f.accountID); a2.Generation != 1 || a2.CoordState != "idle" {
			t.Fatalf("second reconcile mutated the account: gen=%d state=%q", a2.Generation, a2.CoordState)
		}
	})

	t.Run("mismatched recovery is not promoted, intent unrecoverable", func(t *testing.T) {
		fake := &fakeRefreshClient{}
		f := newRefreshFixture(t, env, fake)

		// Seed a recovery slot directly with a blob whose token discovers a DIFFERENT tuple.
		mismatchTok := codexToken("access-mismatch")
		raw, merr := json.Marshal(codexLoginBlob{AccessToken: mismatchTok, RefreshToken: codexToken("r")}) //nolint:gosec // G117: synthetic recovery fixture, master-sealed below
		if merr != nil {
			t.Fatalf("marshal recovery blob: %v", merr)
		}
		sealed, serr := env.box.Seal(raw)
		if serr != nil {
			t.Fatalf("seal recovery blob: %v", serr)
		}
		// SetCodexRecoverySlot now requires the live-lease owner's op (and the key that
		// sealed the blob), so acquire the lease under a known op first — this leaves the
		// account 'quarantined' with the recovery blob at generation 0, which is exactly the
		// unresolved state ReconcileUnresolvedCodexRefresh scans.
		op := uuid.New()
		future := pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true}
		if n, err := env.q.AcquireCodexRefreshLease(env.ctx, store.AcquireCodexRefreshLeaseParams{
			Op: op, Deadline: future, ID: f.accountID, UserID: f.userID, FromGeneration: 0,
		}); err != nil || n != 1 {
			t.Fatalf("AcquireCodexRefreshLease = (%d,%v), want (1,nil)", n, err)
		}
		if _, err := env.q.SetCodexRecoverySlot(env.ctx, store.SetCodexRecoverySlotParams{
			Sealed: sealed, Gen: 0, ID: f.accountID, UserID: f.userID,
			Op: op, RecoverySealedWith: pgtype.Text{String: store.SealedWithMaster, Valid: true},
		}); err != nil {
			t.Fatalf("seed recovery slot: %v", err)
		}
		if _, err := env.q.InsertCodexRefreshIntent(env.ctx, store.InsertCodexRefreshIntentParams{
			OperationID: op, UserID: f.userID, ProviderAccountID: f.accountID, FromGeneration: 0,
		}); err != nil {
			t.Fatalf("insert intent: %v", err)
		}
		// The recovery token verifies to a DIFFERENT account than the one it sits under.
		fake.identityByToken = map[string]codexauth.Identity{
			mismatchTok: {ProviderUserID: "other-" + uuid.NewString(), WorkspaceAccountID: "other-" + uuid.NewString()},
		}

		if _, err := f.svc.ReconcileUnresolvedCodexRefresh(env.ctx, f.userID, f.accountID); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if fake.discoverCalls != 1 {
			t.Fatalf("recovery discovery calls = %d, want exactly 1 before rejecting promotion", fake.discoverCalls)
		}
		acct := env.mustAccount(t, f.userID, f.accountID)
		if acct.CoordState != codexCoordQuarantined {
			t.Fatalf("coord_state = %q, want still quarantined (mismatch must not promote)", acct.CoordState)
		}
		if acct.Generation != 0 {
			t.Fatalf("generation = %d, want 0 (mismatch must not advance)", acct.Generation)
		}
		if it := mustIntent(t, env, op, f.userID); it.State != codexIntentUnrecoverable {
			t.Fatalf("intent state = %q, want unrecoverable (recovery copy is bad)", it.State)
		}
	})
}

// openSealedBlob opens and decodes an arbitrary sealed codex login blob (master box).
func (e codexTestEnv) openSealedBlob(t *testing.T, userID uuid.UUID, sealed []byte, sealedWith string) codexLoginBlob {
	t.Helper()
	plain, err := secretopen.OpenSealed(nil, e.box, userID, store.KindCodexAuth, sealedWith, sealed)
	if err != nil {
		t.Fatalf("open sealed blob: %v", err)
	}
	var b codexLoginBlob
	if err := json.Unmarshal(plain, &b); err != nil {
		t.Fatalf("decode sealed blob: %v", err)
	}
	return b
}

// TestReleaseCodexCredentialSubscriptionLiveDB proves release returns the subscription
// access token plus its server-owned account/generation metadata (never the login/refresh
// blob) and rejects when authority is stale.
func TestReleaseCodexCredentialSubscriptionLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)

	t.Run("returns access token only", func(t *testing.T) {
		f := newRefreshFixture(t, env, &fakeRefreshClient{})
		capw := env.mintCap(t, f.runID, f.workerID)
		released, err := f.svc.ReleaseCodexCredential(env.ctx, f.wkr, f.runID, capw)
		if err != nil {
			t.Fatalf("release: %v", err)
		}
		if released.AccessToken != f.accessToken {
			t.Fatalf("released %q, want the access token %q", released.AccessToken, f.accessToken)
		}
		if released.AccessToken == f.prevRefreshToken {
			t.Fatal("release must never return the refresh token")
		}
		if released.AuthMode != codexAuthModeSubscription || released.Generation == nil || *released.Generation != 0 {
			t.Fatalf("subscription release metadata = (%q,%v), want (subscription,0)", released.AuthMode, released.Generation)
		}
		if released.ChatGPTAccountID != f.workspaceAcctID {
			t.Fatalf("chatgpt account id = %q, want verified %q", released.ChatGPTAccountID, f.workspaceAcctID)
		}
	})

	t.Run("rejects stale authority", func(t *testing.T) {
		f := newRefreshFixture(t, env, &fakeRefreshClient{})
		capw := env.mintCap(t, f.runID, f.workerID)
		// A genuine account-level revoke bumps credential_revision, staling the run's
		// frozen authority.
		env.exec(`UPDATE codex_provider_account SET credential_revision = credential_revision + 1 WHERE id=$1`, f.accountID)
		released, err := f.svc.ReleaseCodexCredential(env.ctx, f.wkr, f.runID, capw)
		if !errors.Is(err, ErrCodexAccountRevisionStale) {
			t.Fatalf("err = %v, want ErrCodexAccountRevisionStale", err)
		}
		if released != (CodexReleaseResult{}) {
			t.Fatalf("release = %+v, want empty on stale authority", released)
		}
	})

	// Pins the recheck-before-release: authority is staled BETWEEN the initial authorize and
	// the recheck (the token-read step bumps credential_revision), so the FIRST authorize
	// passes and only the SECOND (recheck) rejects. Deleting the recheck would leak the
	// token here and fail this test — the existing "rejects stale authority" case cannot,
	// because its staleness is present before the first authorize.
	t.Run("rejects authority staled mid-call (pins recheck)", func(t *testing.T) {
		f := newRefreshFixture(t, env, &fakeRefreshClient{})
		capw := env.mintCap(t, f.runID, f.workerID)
		f.svc.q = releaseHookStore{
			Queries: env.q,
			onAccountRead: func() {
				// The token read has completed (first authorize already passed); bump the
				// account's credential_revision so only the recheck sees stale authority.
				env.exec(`UPDATE codex_provider_account SET credential_revision = credential_revision + 1 WHERE id=$1`, f.accountID)
			},
		}
		released, err := f.svc.ReleaseCodexCredential(env.ctx, f.wkr, f.runID, capw)
		if !errors.Is(err, ErrCodexAccountRevisionStale) {
			t.Fatalf("err = %v, want ErrCodexAccountRevisionStale from the recheck", err)
		}
		if released != (CodexReleaseResult{}) {
			t.Fatalf("release = %+v, want empty when authority stales mid-call", released)
		}
	})
}

// TestReleaseCodexCredentialMisboundKindNonDisclosureLiveDB pins the kind non-disclosure
// defense (PRD #1147 audit #6): a run FORCED into a mis-bound state — codex_auth_mode
// 'api_key' while its codex_secret_id points at a codex_auth alias (whose login blob
// carries a refresh_token) — must NEVER release that blob. Release refuses it with
// ErrCodexKindModeMismatch (the predicate's kind↔mode check is the first guard) and, even
// were that bypassed, the api_key branch now opens through OpenByIDOfKind, which returns
// the not-found sentinel for a non-openai_api_key row.
//
// FAIL-OLD / PASS-FIXED: the old release path had no kind↔mode predicate check and opened
// the api_key branch with OpenByID, which decrypts a row of ANY kind — so this run would
// have released the codex_auth login blob (refresh_token and all) as the "access token".
// The fix refuses it and discloses no bytes.
func TestReleaseCodexCredentialMisboundKindNonDisclosureLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, workerID, repoID := env.seedCodexInfra(t)

	// A codex_auth staging alias whose sealed blob carries a refresh token.
	refresh := codexToken("refresh")
	aliasID := env.seedStagingAlias(t, userID, "codex-"+uuid.NewString(), codexLoginBlob{AccessToken: codexToken("access"), RefreshToken: refresh})
	runID := env.seedCodexRun(t, userID, workerID, repoID)

	// FORCE the mis-bind directly (FreezeCodexBinding would now refuse it): api_key mode
	// bound to a codex_auth alias, material_revision matching the staging state so the old
	// per-alias check would have passed straight through to the open.
	env.exec(`UPDATE runs SET codex_secret_id=$1, codex_auth_mode='api_key', codex_secret_label='forced', codex_material_revision=0 WHERE id=$2`, aliasID, runID)

	svc := &Service{q: env.q, box: env.box}
	wkr := store.Worker{ID: workerID, UserID: userID}
	capw := env.mintCap(t, runID, workerID)

	released, err := svc.ReleaseCodexCredential(env.ctx, wkr, runID, capw)
	if !errors.Is(err, ErrCodexKindModeMismatch) {
		t.Fatalf("mis-bound release: want ErrCodexKindModeMismatch, got %v", err)
	}
	// Any non-empty token is a failure; report a LEAKED value specifically (folded into the
	// non-empty branch so the leak check is live, not dead code on an already-empty tok).
	if released != (CodexReleaseResult{}) {
		if strings.Contains(released.AccessToken, "refresh") || released.AccessToken == refresh {
			t.Fatalf("the codex_auth login blob leaked through the api_key release path: %q", released.AccessToken)
		}
		t.Fatalf("mis-bound release must disclose no credential, got %+v", released)
	}
}

// TestReleaseCodexCredentialAPIKeyLiveDB proves release on an api_key run returns the
// static key and no subscription-only metadata.
func TestReleaseCodexCredentialAPIKeyLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, workerID, repoID := env.seedCodexInfra(t)
	key := codexToken("sk")
	aliasID := env.seedStaticAPIKey(t, userID, "codex-key-"+uuid.NewString(), key)
	runID := env.seedCodexRun(t, userID, workerID, repoID)
	svc := &Service{q: env.q, box: env.box}
	if err := svc.FreezeCodexBinding(env.ctx, userID, runID, aliasID, codexAuthModeAPIKey); err != nil {
		t.Fatalf("FreezeCodexBinding: %v", err)
	}
	wkr := store.Worker{ID: workerID, UserID: userID}
	capw := env.mintCap(t, runID, workerID)

	released, err := svc.ReleaseCodexCredential(env.ctx, wkr, runID, capw)
	if err != nil {
		t.Fatalf("release api_key: %v", err)
	}
	if released.AccessToken != key {
		t.Fatalf("released %q, want the static key %q", released.AccessToken, key)
	}
	if released.AuthMode != codexAuthModeAPIKey || released.Generation != nil || released.ChatGPTAccountID != "" {
		t.Fatalf("api_key release carried subscription metadata: %+v", released)
	}
}

// ===========================================================================================
// PRD #1147 M4 defect coverage (defects 4, 5, 6). These are LiveDB tests; the fail-old/
// pass-fixed demonstrations revert each sub-fix in codexrefresh.go.
// ===========================================================================================

// TestCoordinatedCodexRefreshRecoverySlotRetryPersistsLiveDB (defect 4, sub-fix 2) proves the
// recovery-slot write is RESILIENT: SetCodexRecoverySlot fails the first calls (a transient
// blip) but the bounded synchronous retry, then the background retrier, eventually land it —
// so the freshly-rotated single-use material is NOT dropped and the intent is NOT marked
// unrecoverable. It drives the unverified fresh-token claim retain path.
//
// FAILS OLD: the old codexRetainUnverifiedMaterial wrote once on the request ctx and, on a
// single write error, marked the intent unrecoverable and dropped the material.
func TestCoordinatedCodexRefreshRecoverySlotRetryPersistsLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	newAccess := codexToken("access-new")
	fake := &fakeRefreshClient{result: codexauth.RefreshResult{AccessToken: newAccess}}
	f := newRefreshFixture(t, env, fake)
	// Unverified claims route the exchanged material through the recovery slot.
	fake.freshClaimsSet = true
	// The recovery write fails past the synchronous attempts, forcing the background retrier;
	// the test's synchronous s.background runs it inline so the slot is populated by return.
	hook := &recoverySlotHookStore{Queries: env.q, failFirst: codexRecoverySlotSyncAttempts + 1}
	f.svc.q = hook
	f.svc.SetBackground(func(fn func()) { fn() })
	capw := env.mintCap(t, f.runID, f.workerID)
	op := uuid.New()

	res, err := f.svc.CoordinatedCodexRefresh(env.ctx, f.wkr, f.runID, capw, op, 0)
	if !errors.Is(err, ErrCodexRefreshQuarantined) {
		t.Fatalf("err = %v, want ErrCodexRefreshQuarantined", err)
	}
	if errors.Is(err, ErrCodexRefreshUnrecoverable) {
		t.Fatalf("err = %v, must NOT be unrecoverable on a transient recovery-write blip", err)
	}
	if res.AccessToken != "" {
		t.Fatalf("token = %q, want NO token", res.AccessToken)
	}
	// The write really was retried past the initial failures (sync exhausted, background landed).
	if hook.calls <= codexRecoverySlotSyncAttempts {
		t.Fatalf("SetCodexRecoverySlot calls = %d, want > %d (background retried past the sync attempts)", hook.calls, codexRecoverySlotSyncAttempts)
	}
	// The recovery slot was eventually populated despite the initial write failures.
	acct := env.mustAccount(t, f.userID, f.accountID)
	if len(acct.RecoverySealed) == 0 || !acct.RecoveryGeneration.Valid || acct.RecoveryGeneration.Int64 != 0 {
		t.Fatalf("recovery slot not persisted after retry: sealed=%d gen=%v", len(acct.RecoverySealed), acct.RecoveryGeneration)
	}
	// The intent stays 'rotating' (retained for a later promotion), NOT unrecoverable.
	if it := mustIntent(t, env, op, f.userID); it.State != codexIntentRotating {
		t.Fatalf("intent state = %q, want rotating (retained, not unrecoverable)", it.State)
	}
}

// TestCoordinatedCodexRefreshRecoverySlotZeroRowsIsNotPersistedLiveDB proves that a
// successful SQL round trip is not enough: a zero-row operation-fence miss stored no
// recovery material and must be classified as unrecoverable rather than retained.
//
// FAILS OLD: writeCodexRecoverySlotOnce ignored the :execrows count, returned true on
// (0,nil), and the caller reported a protected quarantined outcome with an empty slot.
func TestCoordinatedCodexRefreshRecoverySlotZeroRowsIsNotPersistedLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	newAccess := codexToken("access-new")
	fake := &fakeRefreshClient{result: codexauth.RefreshResult{AccessToken: newAccess}}
	f := newRefreshFixture(t, env, fake)
	fake.freshClaimsSet = true
	hook := &recoverySlotZeroStore{Queries: env.q}
	f.svc.q = hook
	capw := env.mintCap(t, f.runID, f.workerID)
	op := uuid.New()

	res, err := f.svc.CoordinatedCodexRefresh(env.ctx, f.wkr, f.runID, capw, op, 0)
	if !errors.Is(err, ErrCodexRefreshUnrecoverable) {
		t.Fatalf("err = %v, want ErrCodexRefreshUnrecoverable after a fenced-out recovery write", err)
	}
	if res.AccessToken != "" {
		t.Fatalf("token = %q, want NO token", res.AccessToken)
	}
	if hook.calls != 1 {
		t.Fatalf("SetCodexRecoverySlot calls = %d, want 1 (a fence miss is permanent, not retryable)", hook.calls)
	}
	acct := env.mustAccount(t, f.userID, f.accountID)
	if len(acct.RecoverySealed) != 0 {
		t.Fatalf("recovery slot contains %d bytes after a zero-row write, want empty", len(acct.RecoverySealed))
	}
	if it := mustIntent(t, env, op, f.userID); it.State != codexIntentUnrecoverable {
		t.Fatalf("intent state = %q, want unrecoverable after the recovery fence rejected the only copy", it.State)
	}
}

// TestCoordinatedCodexRefreshRecoverySlotSurvivesCtxCancelLiveDB (defect 4, sub-fix 2) proves
// a CANCELLED request ctx no longer drops the freshly-rotated material: the recovery write
// runs on a DETACHED context (context.WithoutCancel), so it persists even though the request
// ctx is cancelled at the exact moment the commit fails. The intent is NOT unrecoverable.
//
// FAILS OLD: the old recovery write reused the request ctx, so a cancelled ctx failed the
// write and marked the intent unrecoverable, dropping the material.
func TestCoordinatedCodexRefreshRecoverySlotSurvivesCtxCancelLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	newAccess := codexToken("access-new")
	fake := &fakeRefreshClient{result: codexauth.RefreshResult{AccessToken: newAccess}}
	f := newRefreshFixture(t, env, fake)
	capw := env.mintCap(t, f.runID, f.workerID)
	op := uuid.New()

	ctx, cancel := context.WithCancel(env.ctx)
	defer cancel()
	f.svc.q = refreshHookStore{
		Queries:   env.q,
		commitErr: errors.New("commit exploded"),
		// The request ctx is cancelled as the commit fails and the recovery write is about to
		// run: only a detached write survives it.
		onCommit: func() { cancel() },
	}

	res, err := f.svc.CoordinatedCodexRefresh(ctx, f.wkr, f.runID, capw, op, 0)
	if !errors.Is(err, ErrCodexRefreshQuarantined) {
		t.Fatalf("err = %v, want ErrCodexRefreshQuarantined", err)
	}
	if errors.Is(err, ErrCodexRefreshUnrecoverable) {
		t.Fatalf("err = %v, must NOT be unrecoverable when only the request ctx was cancelled", err)
	}
	if res.AccessToken != "" {
		t.Fatalf("token = %q, want NO token", res.AccessToken)
	}
	// The detached recovery write persisted despite the cancelled request ctx.
	acct := env.mustAccount(t, f.userID, f.accountID)
	if len(acct.RecoverySealed) == 0 || !acct.RecoveryGeneration.Valid || acct.RecoveryGeneration.Int64 != 0 {
		t.Fatalf("recovery slot not persisted under a cancelled request ctx: sealed=%d gen=%v", len(acct.RecoverySealed), acct.RecoveryGeneration)
	}
	if it := mustIntent(t, env, op, f.userID); it.State != codexIntentRotating {
		t.Fatalf("intent state = %q, want rotating (retained, not unrecoverable)", it.State)
	}
}

// TestCoordinatedCodexRefreshVaultLockedSealRetainsLiveDB proves that a vault lock after
// provider rotation does not discard the only new login. The operation protects the
// material under the server master key, returns no token, then re-seals it under the user
// DEK and promotes it after unlock.
//
// FAILS OLD: the prior vault-locked branch quarantined an EMPTY recovery slot and later
// reconciliation marked the operation unrecoverable, so its "retained" claim was false.
func TestCoordinatedCodexRefreshVaultLockedSealRetainsLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	newAccess := codexToken("access-new")
	fake := &fakeRefreshClient{result: codexauth.RefreshResult{AccessToken: newAccess}}
	f := newRefreshFixture(t, env, fake)
	capw := env.mintCap(t, f.runID, f.workerID)
	op := uuid.New()

	// Wire a real vault that is LOCKED for this user. The account's sealed_login is
	// master-sealed (opens without an unlock), but the post-rotation canonical seal takes the
	// DEK path and returns vault.ErrLocked. The recovery path must still retain the new login.
	vlt := vault.New(env.box, env.q)
	f.svc.SetVault(vlt)

	res, err := f.svc.CoordinatedCodexRefresh(env.ctx, f.wkr, f.runID, capw, op, 0)
	if !errors.Is(err, ErrCodexRefreshQuarantined) {
		t.Fatalf("err = %v, want ErrCodexRefreshQuarantined (retained)", err)
	}
	if errors.Is(err, ErrCodexRefreshUnrecoverable) {
		t.Fatalf("err = %v, must NOT be unrecoverable on a transient vault-locked seal", err)
	}
	if res.Outcome != CodexRefreshQuarantined || res.AccessToken != "" {
		t.Fatalf("result = %+v, want a quarantined outcome with NO token", res)
	}
	if fake.calls != 1 {
		t.Fatalf("provider calls = %d, want exactly 1", fake.calls)
	}
	acct := env.mustAccount(t, f.userID, f.accountID)
	if acct.Generation != 0 {
		t.Fatalf("generation = %d, want 0 (a vault-locked seal must not commit)", acct.Generation)
	}
	if acct.CoordState != codexCoordQuarantined {
		t.Fatalf("coord_state = %q, want quarantined (retained for a survivor)", acct.CoordState)
	}
	if len(acct.RecoverySealed) == 0 || !acct.RecoveryGeneration.Valid || acct.RecoveryGeneration.Int64 != 0 {
		t.Fatalf("vault-locked refresh did not retain protected recovery: sealed=%d gen=%v", len(acct.RecoverySealed), acct.RecoveryGeneration)
	}
	if !acct.RecoverySealedWith.Valid || acct.RecoverySealedWith.String != store.SealedWithMaster {
		t.Fatalf("recovery sealed_with = %v, want temporary master protection", acct.RecoverySealedWith)
	}
	// The intent stays 'rotating' — retried by a later pass, NOT marked unrecoverable.
	if it := mustIntent(t, env, op, f.userID); it.State != codexIntentRotating {
		t.Fatalf("intent state = %q, want rotating (retained, not unrecoverable)", it.State)
	}

	// Once the user vault unlocks, reconciliation must re-seal the protected recovery under
	// the DEK before making it live, then return the account to an idle usable generation.
	if uerr := vlt.Unlock(env.ctx, f.userID, "vault-lock-recovery-password"); uerr != nil {
		t.Fatalf("unlock vault: %v", uerr)
	}
	if _, rerr := f.svc.ReconcileUnresolvedCodexRefresh(env.ctx, f.userID, f.accountID); rerr != nil {
		t.Fatalf("reconcile protected vault-lock recovery: %v", rerr)
	}
	acct = env.mustAccount(t, f.userID, f.accountID)
	if acct.CoordState != "idle" || acct.Generation != 1 {
		t.Fatalf("post-unlock account = (state=%q gen=%d), want (idle,1)", acct.CoordState, acct.Generation)
	}
	if acct.SealedWith != store.SealedWithDEK {
		t.Fatalf("promoted sealed_with = %q, want %q", acct.SealedWith, store.SealedWithDEK)
	}
	blob, oerr := f.svc.openCodexAccountLogin(f.userID, acct)
	if oerr != nil {
		t.Fatalf("open promoted login: %v", oerr)
	}
	if blob.AccessToken != newAccess {
		t.Fatalf("promoted access token = %q, want retained %q", blob.AccessToken, newAccess)
	}
}

// TestCoordinatedCodexRefreshIdentityIncompleteRetainsLiveDB proves an incomplete fresh-token
// claim tuple is retained rather than treated as a mismatch. A later reconcile performs the
// full network discovery and can promote it.
func TestCoordinatedCodexRefreshIdentityIncompleteRetainsLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	newAccess := codexToken("access-new")
	fake := &fakeRefreshClient{result: codexauth.RefreshResult{AccessToken: newAccess}}
	f := newRefreshFixture(t, env, fake)
	// The exchanged token omits one established user claim, so it cannot decide identity.
	fake.freshClaimsSet = true
	fake.freshClaims = codexauth.FreshAccessTokenIdentityClaims{
		ChatGPTAccountID: f.workspaceAcctID,
		ChatGPTUserID:    f.providerUserID,
	}
	capw := env.mintCap(t, f.runID, f.workerID)
	op := uuid.New()

	res, err := f.svc.CoordinatedCodexRefresh(env.ctx, f.wkr, f.runID, capw, op, 0)
	if !errors.Is(err, ErrCodexRefreshQuarantined) {
		t.Fatalf("err = %v, want ErrCodexRefreshQuarantined (retained)", err)
	}
	if errors.Is(err, ErrCodexRefreshUnrecoverable) {
		t.Fatalf("err = %v, must NOT be unrecoverable on an incomplete identity", err)
	}
	if res.Outcome != CodexRefreshQuarantined || res.AccessToken != "" {
		t.Fatalf("result = %+v, want quarantined outcome with NO token", res)
	}
	acct := env.mustAccount(t, f.userID, f.accountID)
	if acct.Generation != 0 {
		t.Fatalf("generation = %d, want 0 (an unverified exchange must not commit)", acct.Generation)
	}
	// The material was RETAINED in the recovery slot, NOT discarded as a mismatch would.
	if len(acct.RecoverySealed) == 0 || !acct.RecoveryGeneration.Valid || acct.RecoveryGeneration.Int64 != 0 {
		t.Fatalf("recovery slot not retained on an incomplete identity: sealed=%d gen=%v", len(acct.RecoverySealed), acct.RecoveryGeneration)
	}
	if it := mustIntent(t, env, op, f.userID); it.State != codexIntentRotating {
		t.Fatalf("intent state = %q, want rotating (retained for a later promotion)", it.State)
	}

	// A subsequent verification can still succeed: once DiscoverIdentity resolves the account's
	// frozen tuple, a reconcile pass promotes the retained material to the live login.
	fake.identityByToken = map[string]codexauth.Identity{
		newAccess: {ProviderUserID: f.providerUserID, WorkspaceAccountID: f.workspaceAcctID},
	}
	if _, rerr := f.svc.ReconcileUnresolvedCodexRefresh(env.ctx, f.userID, f.accountID); rerr != nil {
		t.Fatalf("reconcile after incomplete: %v", rerr)
	}
	acct = env.mustAccount(t, f.userID, f.accountID)
	if acct.CoordState != "idle" || acct.Generation != 1 {
		t.Fatalf("post-promotion account = (state=%q gen=%d), want (idle,1)", acct.CoordState, acct.Generation)
	}
	if blob := env.accountBlob(t, f.userID, f.accountID); blob.AccessToken != newAccess {
		t.Fatalf("promoted login access token = %q, want the retained new %q", blob.AccessToken, newAccess)
	}
}

// TestReconcileUnresolvedCodexRefreshIncompleteRecoveryRetainsLiveDB (defect 5, recovery
// promotion) proves the promotion path leaves an INCOMPLETE-identity recovery slot for a
// later pass instead of marking its recovery-dependent intents unrecoverable: the account
// stays quarantined with its material and the intent is NOT unrecoverable.
//
// FAILS OLD: the old promoteCodexRecovery bucketed ErrIdentityIncomplete with a verified
// mismatch and marked every recovery-dependent intent unrecoverable.
func TestReconcileUnresolvedCodexRefreshIncompleteRecoveryRetainsLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fake := &fakeRefreshClient{}
	f := newRefreshFixture(t, env, fake)

	// Seed a recovery slot whose token discovers an INCOMPLETE identity ("cannot tell").
	incompleteTok := codexToken("access-incomplete")
	raw, merr := json.Marshal(codexLoginBlob{AccessToken: incompleteTok, RefreshToken: codexToken("r")}) //nolint:gosec // G117: synthetic recovery fixture, master-sealed below
	if merr != nil {
		t.Fatalf("marshal recovery blob: %v", merr)
	}
	sealed, serr := env.box.Seal(raw)
	if serr != nil {
		t.Fatalf("seal recovery blob: %v", serr)
	}
	op := uuid.New()
	future := pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true}
	if n, err := env.q.AcquireCodexRefreshLease(env.ctx, store.AcquireCodexRefreshLeaseParams{
		Op: op, Deadline: future, ID: f.accountID, UserID: f.userID, FromGeneration: 0,
	}); err != nil || n != 1 {
		t.Fatalf("AcquireCodexRefreshLease = (%d,%v), want (1,nil)", n, err)
	}
	if _, err := env.q.SetCodexRecoverySlot(env.ctx, store.SetCodexRecoverySlotParams{
		Sealed: sealed, Gen: 0, ID: f.accountID, UserID: f.userID,
		Op: op, RecoverySealedWith: pgtype.Text{String: store.SealedWithMaster, Valid: true},
	}); err != nil {
		t.Fatalf("seed recovery slot: %v", err)
	}
	if _, err := env.q.InsertCodexRefreshIntent(env.ctx, store.InsertCodexRefreshIntentParams{
		OperationID: op, UserID: f.userID, ProviderAccountID: f.accountID, FromGeneration: 0,
	}); err != nil {
		t.Fatalf("insert intent: %v", err)
	}
	fake.errByToken = map[string]error{incompleteTok: codexauth.ErrIdentityIncomplete}

	if _, err := f.svc.ReconcileUnresolvedCodexRefresh(env.ctx, f.userID, f.accountID); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	acct := env.mustAccount(t, f.userID, f.accountID)
	// Incomplete ("cannot tell") must NOT promote AND must NOT mark unrecoverable: the slot is
	// left for a later pass, the account stays quarantined with its material.
	if acct.CoordState != codexCoordQuarantined {
		t.Fatalf("coord_state = %q, want still quarantined (incomplete must not promote)", acct.CoordState)
	}
	if acct.Generation != 0 {
		t.Fatalf("generation = %d, want 0 (incomplete must not advance)", acct.Generation)
	}
	if len(acct.RecoverySealed) == 0 {
		t.Fatalf("recovery slot must be RETAINED on an incomplete identity, got empty")
	}
	if it := mustIntent(t, env, op, f.userID); it.State == codexIntentUnrecoverable {
		t.Fatalf("intent state = %q, must NOT be unrecoverable on an incomplete identity", it.State)
	}
}

// TestReconcileUnresolvedCodexRefreshLeavesLiveLeaseLiveDB (defect 6) proves reconciliation
// does NOT declare a genuinely LIVE, validly-leased in-flight rotation unrecoverable: with a
// non-expired lease held under its own op (in_progress, generation unchanged, empty recovery
// slot), the intent is left 'rotating', nothing is counted resolved, and the original op can
// still COMMIT successfully.
//
// FAILS OLD: the old codexRefreshResolution fell through to unrecoverable for a live op
// (generation unchanged, no recovery slot), bricking a refresh that was about to commit.
func TestReconcileUnresolvedCodexRefreshLeavesLiveLeaseLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	newAccess := codexToken("access-new")
	rotatedRefresh := codexToken("refresh-rotated")
	fake := &fakeRefreshClient{result: codexauth.RefreshResult{AccessToken: newAccess, RefreshToken: &rotatedRefresh}}
	f := newRefreshFixture(t, env, fake)

	// A LIVE op holds a valid, non-expired lease: in_progress under opX, generation unchanged,
	// empty recovery slot — a rotation genuinely in flight.
	opX := uuid.New()
	future := pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true}
	if n, err := env.q.AcquireCodexRefreshLease(env.ctx, store.AcquireCodexRefreshLeaseParams{
		Op: opX, Deadline: future, ID: f.accountID, UserID: f.userID, FromGeneration: 0,
	}); err != nil || n != 1 {
		t.Fatalf("AcquireCodexRefreshLease = (%d,%v), want (1,nil)", n, err)
	}
	if _, err := env.q.InsertCodexRefreshIntent(env.ctx, store.InsertCodexRefreshIntentParams{
		OperationID: opX, UserID: f.userID, ProviderAccountID: f.accountID, FromGeneration: 0,
	}); err != nil {
		t.Fatalf("insert intent: %v", err)
	}

	// Reconcile must NOT declare the live op: the intent is left 'rotating' and nothing counts.
	n, err := f.svc.ReconcileUnresolvedCodexRefresh(env.ctx, f.userID, f.accountID)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if n != 0 {
		t.Fatalf("resolved = %d, want 0 (a live op's intent is left untouched)", n)
	}
	if it := mustIntent(t, env, opX, f.userID); it.State != codexIntentRotating {
		t.Fatalf("intent state = %q, want rotating (a live in-flight rotation must be left alone)", it.State)
	}
	acct := env.mustAccount(t, f.userID, f.accountID)
	if acct.CoordState != codexCoordInProgress {
		t.Fatalf("coord_state = %q, want in_progress (reconcile must not quarantine a live lease)", acct.CoordState)
	}

	// The original op can still COMMIT successfully: reconcile did not steal its lease nor
	// quarantine it (no competing rotation, no premature re-login).
	merged, mmerr := json.Marshal(codexLoginBlob{AccessToken: newAccess, RefreshToken: rotatedRefresh}) //nolint:gosec // G117: synthetic merged login fixture, master-sealed below
	if mmerr != nil {
		t.Fatalf("marshal merged blob: %v", mmerr)
	}
	sealedMerged, mserr := env.box.Seal(merged)
	if mserr != nil {
		t.Fatalf("seal merged blob: %v", mserr)
	}
	row, cerr := env.q.CommitCodexRefresh(env.ctx, store.CommitCodexRefreshParams{
		Sealed: sealedMerged, SealedWith: store.SealedWithMaster, Op: opX, ID: f.accountID, UserID: f.userID, FromGeneration: 0,
	})
	if cerr != nil {
		t.Fatalf("commit after reconcile: %v (the live op must still be able to commit)", cerr)
	}
	if row.Generation != 1 {
		t.Fatalf("committed generation = %d, want 1", row.Generation)
	}
}

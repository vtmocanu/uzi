package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/codexauth"
	"github.com/vtmocanu/uzi/api/internal/secretopen"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// These tests exercise the coordinated Codex refresh state machine (PRD #1147 M2, B6,
// ships DARK) end to end against a REAL Postgres with an IN-PROCESS call-counting fake
// oauth client — no HTTP. They are REPRESENTATIVE: one client at a time, driving each
// arm of the state machine (advance, replay, reconcile, contended, quarantine, recovery).
// m4 does the exhaustive two-client concurrency matrix.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres. All fixtures use
// fresh UUIDs so a single-process `-run 'LiveDB$'` sweep never collides.

// fakeRefreshClient is the in-process oauth-exchange seam. It counts Refresh calls
// (proving replay/reconcile spend ZERO), records the refresh token it was handed, and
// returns a configured result (single-use rotating tokens set by the test).
type fakeRefreshClient struct {
	calls            int
	lastRefreshToken string
	result           codexauth.RefreshResult
	err              error
}

func (f *fakeRefreshClient) Refresh(_ context.Context, refreshToken string) (codexauth.RefreshResult, error) {
	f.calls++
	f.lastRefreshToken = refreshToken
	if f.err != nil {
		return codexauth.RefreshResult{}, f.err
	}
	return f.result, nil
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
// token-read step of ReleaseCodexAccessToken — after the FIRST authorize has passed and
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
	r := NewCodexReconciler(env.q, nil, env.box, fid)
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

// TestReleaseCodexAccessTokenSubscriptionLiveDB proves release returns the subscription
// access token only (never the login/refresh blob) and rejects when authority is stale.
func TestReleaseCodexAccessTokenSubscriptionLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)

	t.Run("returns access token only", func(t *testing.T) {
		f := newRefreshFixture(t, env, &fakeRefreshClient{})
		capw := env.mintCap(t, f.runID, f.workerID)
		tok, err := f.svc.ReleaseCodexAccessToken(env.ctx, f.wkr, f.runID, capw)
		if err != nil {
			t.Fatalf("release: %v", err)
		}
		if tok != f.accessToken {
			t.Fatalf("released %q, want the access token %q", tok, f.accessToken)
		}
		if tok == f.prevRefreshToken {
			t.Fatal("release must never return the refresh token")
		}
	})

	t.Run("rejects stale authority", func(t *testing.T) {
		f := newRefreshFixture(t, env, &fakeRefreshClient{})
		capw := env.mintCap(t, f.runID, f.workerID)
		// A genuine account-level revoke bumps credential_revision, staling the run's
		// frozen authority.
		env.exec(`UPDATE codex_provider_account SET credential_revision = credential_revision + 1 WHERE id=$1`, f.accountID)
		tok, err := f.svc.ReleaseCodexAccessToken(env.ctx, f.wkr, f.runID, capw)
		if !errors.Is(err, ErrCodexAccountRevisionStale) {
			t.Fatalf("err = %v, want ErrCodexAccountRevisionStale", err)
		}
		if tok != "" {
			t.Fatalf("token = %q, want empty on stale authority", tok)
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
		tok, err := f.svc.ReleaseCodexAccessToken(env.ctx, f.wkr, f.runID, capw)
		if !errors.Is(err, ErrCodexAccountRevisionStale) {
			t.Fatalf("err = %v, want ErrCodexAccountRevisionStale from the recheck", err)
		}
		if tok != "" {
			t.Fatalf("token = %q, want empty when authority stales mid-call", tok)
		}
	})
}

// TestReleaseCodexAccessTokenAPIKeyLiveDB proves release on an api_key run returns the
// static key.
func TestReleaseCodexAccessTokenAPIKeyLiveDB(t *testing.T) {
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

	tok, err := svc.ReleaseCodexAccessToken(env.ctx, wkr, runID, capw)
	if err != nil {
		t.Fatalf("release api_key: %v", err)
	}
	if tok != key {
		t.Fatalf("released %q, want the static key %q", tok, key)
	}
}

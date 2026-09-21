package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/codexauth"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// M2 regression coverage for issue #1532 (Codex refresh survivor): SweepUnresolvedCodexRefresh
// existed in M1 but had NO production caller, so an interrupted refresh could wedge an
// account (in_progress + a lease that will never be released) forever. These tests exercise
// the always-on survivor against a REAL Postgres — TestSweepSurvivorRecoversWedgedViaServiceSweepLiveDB
// is the acceptance control: it proves the survivor runs THROUGH the production seam
// (Service.Sweep), not merely when called directly. codexrefresh_livedb_test.go and
// codex_m4_livedb_test.go supply the shared fixtures and helpers this file reuses.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres. All fixtures use
// fresh UUIDs so a single-process `-run 'LiveDB$'` sweep never collides.

// wedgeCodexAccount reproduces an account left wedged by an interrupted refresh: an
// 'in_progress' lease under a fresh op, with the given deadline, plus the durable 'rotating'
// intent coordinatedRefresh always inserts BEFORE acquiring the lease (mirrors
// codexrefresh_livedb_test.go's "mismatched recovery" fixture, generalized to any deadline so
// callers can wedge with an EXPIRED lease or leave a LIVE one in place).
func wedgeCodexAccount(t *testing.T, env codexTestEnv, f refreshFixture, deadline time.Time) uuid.UUID {
	t.Helper()
	op := uuid.New()
	if n, err := env.q.AcquireCodexRefreshLease(env.ctx, store.AcquireCodexRefreshLeaseParams{
		Op: op, Deadline: pgtype.Timestamptz{Time: deadline, Valid: true}, ID: f.accountID, UserID: f.userID, FromGeneration: 0,
	}); err != nil || n != 1 {
		t.Fatalf("AcquireCodexRefreshLease = (%d,%v), want (1,nil)", n, err)
	}
	if _, err := env.q.InsertCodexRefreshIntent(env.ctx, store.InsertCodexRefreshIntentParams{
		OperationID: op, UserID: f.userID, ProviderAccountID: f.accountID, FromGeneration: 0,
	}); err != nil {
		t.Fatalf("insert intent: %v", err)
	}
	return op
}

// TestSweepSurvivorRecoversWedgedViaServiceSweepLiveDB is the ACCEPTANCE CONTROL: it proves
// the survivor pass runs THROUGH Service.Sweep, the seam the sweeper Engine calls every tick
// and on Boot — not merely when SweepUnresolvedCodexRefresh is called directly. This is the
// exact defect issue #1532 fixes: the survivor method existed in M1 but had NO production
// caller, so a wedged account (in_progress + an expired lease that would never resolve on its
// own) stayed wedged forever in production even though the reconcile machine worked when
// invoked directly. Removing the `s.SweepUnresolvedCodexRefresh(ctx)` call from Service.Sweep
// (api/internal/workersvc/sweep.go) must make THIS test fail — proven by the mutation check
// described in the task that produced this file (sweep.go is reverted afterward).
func TestSweepSurvivorRecoversWedgedViaServiceSweepLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	f := newRefreshFixture(t, env, &fakeRefreshClient{})
	wedgeCodexAccount(t, env, f, time.Now().Add(-time.Hour))

	// Pre-assert the wedge: a fresh Codex run cannot acquire the lease while the account is
	// still (wrongly) in_progress under the stranded op.
	if n, err := env.q.AcquireCodexRefreshLease(env.ctx, store.AcquireCodexRefreshLeaseParams{
		Op: uuid.New(), Deadline: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
		ID: f.accountID, UserID: f.userID, FromGeneration: 0,
	}); err != nil {
		t.Fatalf("pre-assert AcquireCodexRefreshLease: %v", err)
	} else if n != 0 {
		t.Fatalf("pre-assert: a fresh op acquired the lease (n=%d), want 0 (account must still be wedged)", n)
	}

	// A FULL, Sweep-capable Service — not the bare f.svc fixture — exactly like the other
	// Sweep live-DB tests (wall_park_grace_livedb_test.go). Its clock is time.Now (New's
	// default), so codexNow() agrees with the already-expired lease deadline above.
	svc := New(env.q, env.box, testParams())
	svc.SetReadyAt(time.Now())

	res, err := svc.Sweep(env.ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if res.CodexRefreshRecovered < 1 {
		t.Fatalf("res.CodexRefreshRecovered = %d, want >= 1 (Sweep must run the codex refresh survivor)", res.CodexRefreshRecovered)
	}

	acct := env.mustAccount(t, f.userID, f.accountID)
	if acct.CoordState != codexCoordQuarantined {
		t.Fatalf("coord_state = %q, want quarantined (Sweep must reap the wedged lease)", acct.CoordState)
	}
	intents, err := env.q.ListUnresolvedCodexRefreshIntents(env.ctx, store.ListUnresolvedCodexRefreshIntentsParams{
		UserID: f.userID, ProviderAccountID: f.accountID,
	})
	if err != nil {
		t.Fatalf("list intents: %v", err)
	}
	if len(intents) != 0 {
		t.Fatalf("unresolved intents after Sweep = %d, want 0 (all resolved)", len(intents))
	}
}

// TestSweepUnresolvedCodexRefreshQuarantinesAndIsIdempotentLiveDB drives
// SweepUnresolvedCodexRefresh directly (no explicit account id), so the LIST query
// (ListUnresolvedCodexRefreshAccounts) itself must find the wedged candidate. It also proves
// a second sweep pass is a safe no-op: the intent is no longer 'rotating', so nothing new is
// resolved and the account stays exactly as the first pass left it.
func TestSweepUnresolvedCodexRefreshQuarantinesAndIsIdempotentLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	f := newRefreshFixture(t, env, &fakeRefreshClient{})
	op := wedgeCodexAccount(t, env, f, time.Now().Add(-time.Hour))

	resolved, err := f.svc.SweepUnresolvedCodexRefresh(env.ctx)
	if err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	if resolved < 1 {
		t.Fatalf("first sweep resolved = %d, want >= 1", resolved)
	}
	acct := env.mustAccount(t, f.userID, f.accountID)
	if acct.CoordState != codexCoordQuarantined {
		t.Fatalf("coord_state after first sweep = %q, want quarantined", acct.CoordState)
	}
	if it := mustIntent(t, env, op, f.userID); it.State != codexIntentUnrecoverable {
		t.Fatalf("intent state after first sweep = %q, want unrecoverable", it.State)
	}

	// Idempotence: a second sweep pass must not re-resolve or otherwise disturb the account.
	resolved2, err := f.svc.SweepUnresolvedCodexRefresh(env.ctx)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if resolved2 != 0 {
		t.Fatalf("second sweep resolved = %d, want 0 (the intent is no longer rotating)", resolved2)
	}
	acct2 := env.mustAccount(t, f.userID, f.accountID)
	if acct2.CoordState != codexCoordQuarantined {
		t.Fatalf("coord_state after second sweep = %q, want still quarantined", acct2.CoordState)
	}
	if it := mustIntent(t, env, op, f.userID); it.State != codexIntentUnrecoverable {
		t.Fatalf("intent state after second sweep = %q, want still unrecoverable", it.State)
	}
}

// TestSweepUnresolvedCodexRefreshSkipsLiveRotationLiveDB proves the survivor causes no churn
// and never wrongfully quarantines a LIVE op: an account with a future (non-expired) lease
// deadline and a rotating intent under the same op must be left exactly as-is.
func TestSweepUnresolvedCodexRefreshSkipsLiveRotationLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	f := newRefreshFixture(t, env, &fakeRefreshClient{})
	op := wedgeCodexAccount(t, env, f, time.Now().Add(time.Hour))

	resolved, err := f.svc.SweepUnresolvedCodexRefresh(env.ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if resolved != 0 {
		t.Fatalf("resolved = %d, want 0 (a live, validly-leased rotation must not be touched)", resolved)
	}
	acct := env.mustAccount(t, f.userID, f.accountID)
	if acct.CoordState != codexCoordInProgress {
		t.Fatalf("coord_state = %q, want still in_progress (must not quarantine a live op)", acct.CoordState)
	}
	if it := mustIntent(t, env, op, f.userID); it.State != codexIntentRotating {
		t.Fatalf("intent state = %q, want still rotating (must not touch a live op's intent)", it.State)
	}
}

// TestSweepThenRelinkRecoversCodexAccountLiveDB is the end-to-end recovery proof, and also
// proves "no silent self-recovery": a re-link performed BEFORE the survivor runs is a no-op
// (the account is in_progress, not quarantined, so reconcileTuple's healthy-account branch
// converges without restoring anything) — only the survivor's quarantine unlocks the
// reconcileTuple quarantined arm that actually restores the account.
func TestSweepThenRelinkRecoversCodexAccountLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	f := newRefreshFixture(t, env, &fakeRefreshClient{})
	wedgeCodexAccount(t, env, f, time.Now().Add(-time.Hour))

	// (a) Pre-sweep re-link: a NEW alias whose login maps (in a fresh fake identity) to the
	// SAME tuple as the wedged account. Each re-link needs its own fresh alias since a linked
	// alias will not re-reconcile.
	relinkAccess := codexToken("relink-access")
	preAlias := env.seedStagingAlias(t, f.userID, "codex-relink-pre-"+uuid.NewString(), codexLoginBlob{AccessToken: relinkAccess, RefreshToken: codexToken("relink-refresh")})
	preFid := newFakeCodexIdentity()
	preFid.idByToken[relinkAccess] = codexauth.Identity{ProviderUserID: f.providerUserID, WorkspaceAccountID: f.workspaceAcctID}
	preReconciler := NewCodexReconciler(env.q, nil, env.box, preFid, env.pool)
	if err := preReconciler.ReconcileCodexAuthIdentity(env.ctx, f.userID, preAlias); err != nil {
		t.Fatalf("pre-sweep reconcile: %v", err)
	}

	// The pre-sweep re-link must NOT recover the account: it is still in_progress, generation
	// is still 0, and a fresh op still cannot acquire the lease.
	acctPre := env.mustAccount(t, f.userID, f.accountID)
	if acctPre.CoordState != codexCoordInProgress {
		t.Fatalf("coord_state after pre-sweep re-link = %q, want still in_progress (re-link alone must not recover)", acctPre.CoordState)
	}
	if acctPre.Generation != 0 {
		t.Fatalf("generation after pre-sweep re-link = %d, want still 0", acctPre.Generation)
	}
	if n, err := env.q.AcquireCodexRefreshLease(env.ctx, store.AcquireCodexRefreshLeaseParams{
		Op: uuid.New(), Deadline: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
		ID: f.accountID, UserID: f.userID, FromGeneration: 0,
	}); err != nil {
		t.Fatalf("post-pre-relink AcquireCodexRefreshLease: %v", err)
	} else if n != 0 {
		t.Fatalf("post-pre-relink AcquireCodexRefreshLease = %d, want 0 (still wedged)", n)
	}

	// (b) Recover via the survivor.
	resolved, err := f.svc.SweepUnresolvedCodexRefresh(env.ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if resolved < 1 {
		t.Fatalf("sweep resolved = %d, want >= 1", resolved)
	}
	acctQuarantined := env.mustAccount(t, f.userID, f.accountID)
	if acctQuarantined.CoordState != codexCoordQuarantined {
		t.Fatalf("coord_state after sweep = %q, want quarantined", acctQuarantined.CoordState)
	}

	// (c) Re-link AGAIN with a fresh alias mapping to the same tuple — this now hits
	// reconcileTuple's quarantined arm (RefreshCodexAccountLogin), which restores the login,
	// advances the generation, clears the quarantine and clears reauth_required.
	relinkAccess2 := codexToken("relink-access-2")
	postAlias := env.seedStagingAlias(t, f.userID, "codex-relink-post-"+uuid.NewString(), codexLoginBlob{AccessToken: relinkAccess2, RefreshToken: codexToken("relink-refresh-2")})
	postFid := newFakeCodexIdentity()
	postFid.idByToken[relinkAccess2] = codexauth.Identity{ProviderUserID: f.providerUserID, WorkspaceAccountID: f.workspaceAcctID}
	postReconciler := NewCodexReconciler(env.q, nil, env.box, postFid, env.pool)
	if err := postReconciler.ReconcileCodexAuthIdentity(env.ctx, f.userID, postAlias); err != nil {
		t.Fatalf("post-sweep reconcile: %v", err)
	}

	acctRecovered := env.mustAccount(t, f.userID, f.accountID)
	if acctRecovered.CoordState != "idle" {
		t.Fatalf("coord_state after recovery re-link = %q, want idle", acctRecovered.CoordState)
	}
	if acctRecovered.Generation <= 0 {
		t.Fatalf("generation after recovery re-link = %d, want strictly > 0", acctRecovered.Generation)
	}
	if acctRecovered.ReauthRequired {
		t.Fatal("reauth_required after recovery re-link = true, want false")
	}

	// (d) The account is un-wedged: a later run can acquire the lease at the NEW generation.
	if n, err := env.q.AcquireCodexRefreshLease(env.ctx, store.AcquireCodexRefreshLeaseParams{
		Op: uuid.New(), Deadline: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
		ID: f.accountID, UserID: f.userID, FromGeneration: acctRecovered.Generation,
	}); err != nil {
		t.Fatalf("final AcquireCodexRefreshLease: %v", err)
	} else if n != 1 {
		t.Fatalf("final AcquireCodexRefreshLease = %d, want 1 (the account must be usable again)", n)
	}
}

// TestSweepSurvivorRetriesDeferredRecoveryViaServiceSweepLiveDB (issue #1532, race 1) is the
// two-tick regression for the always-on retriable recovery candidate + honest counting, driven
// THROUGH Service.Sweep. It seeds a quarantined account holding MATCHING recovery material at
// the current generation and NO rotating intent — the exact shape that a prior reconcile leaves
// behind once it has driven the account's intents terminal. Before the fix such an account was
// no longer selected by ListUnresolvedCodexRefreshAccounts (it has neither an expired in_progress
// lease nor a rotating intent), so a promotion that DEFERRED transiently (here: a transient
// DiscoverIdentity blip) was never retried and the account stayed quarantined forever.
//
// Tick 1 proves honest counting: the transient blip defers the promotion, so the sweep changes
// nothing about the account and does NOT count it as recovered (CodexRefreshRecovered == 0).
// Tick 2 proves the always-on retry: arm (c) of the scan re-lists the account, the discovery
// now matches, and the material is promoted (generation advanced, quarantine cleared, recovery
// slot emptied, live login usable) — and this time it IS counted.
//
// MUTATION PROOF (documented, run, and restored): removing arm (c) from
// ListUnresolvedCodexRefreshAccounts (codex_binding.sql) makes tick 2 no longer list this
// quarantined-with-recovery account, so it is never re-reconciled and never promoted — it stays
// quarantined at generation 0 and this test fails.
func TestSweepSurvivorRetriesDeferredRecoveryViaServiceSweepLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	recTok := codexToken("recovery-access")
	// Transient-first, token-specific fake: the FIRST discovery of recTok returns a transient
	// (non-ErrIdentityIncomplete) blip — a deferral — and later discoveries MATCH this account's
	// frozen tuple. discoverDefaultErr makes every OTHER user's leftover recovery blob defer, so
	// this GLOBAL survivor sweep's recovered count is attributable to this test's account alone.
	fake := &fakeRefreshClient{
		discoverTransientTok: recTok,
		discoverTransientErr: errors.New("transient discovery blip"),
		discoverDefaultErr:   codexauth.ErrIdentityIncomplete,
	}
	f := newRefreshFixture(t, env, fake)
	fake.identityByToken = map[string]codexauth.Identity{
		recTok: {ProviderUserID: f.providerUserID, WorkspaceAccountID: f.workspaceAcctID},
	}

	// Seed a quarantined account carrying matching recovery material at generation 0 and NO
	// rotating intent: acquire a lease under a known op, then protect the recovery blob (which
	// quarantines). No intent is inserted — this is the post-terminal shape race 1 must retry.
	op := uuid.New()
	future := pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true}
	if n, err := env.q.AcquireCodexRefreshLease(env.ctx, store.AcquireCodexRefreshLeaseParams{
		Op: op, Deadline: future, ID: f.accountID, UserID: f.userID, FromGeneration: 0,
	}); err != nil || n != 1 {
		t.Fatalf("AcquireCodexRefreshLease = (%d,%v), want (1,nil)", n, err)
	}
	recBlob, merr := json.Marshal(codexLoginBlob{AccessToken: recTok, RefreshToken: codexToken("recovery-refresh")}) //nolint:gosec // G117: synthetic recovery fixture, master-sealed below
	if merr != nil {
		t.Fatalf("marshal recovery blob: %v", merr)
	}
	sealedRec, serr := env.box.Seal(recBlob)
	if serr != nil {
		t.Fatalf("seal recovery blob: %v", serr)
	}
	if _, err := env.q.SetCodexRecoverySlot(env.ctx, store.SetCodexRecoverySlotParams{
		Sealed: sealedRec, Gen: 0, ID: f.accountID, UserID: f.userID,
		Op: op, RecoverySealedWith: pgtype.Text{String: store.SealedWithMaster, Valid: true},
	}); err != nil {
		t.Fatalf("seed recovery slot: %v", err)
	}

	// A FULL Sweep-capable Service (time.Now clock) with the transient-first fake as its
	// identity seam — like the acceptance control, plus the recovery-verification seam.
	svc := New(env.q, env.box, testParams())
	svc.SetReadyAt(time.Now())
	svc.SetCodexRefresh(fake)

	// Tick 1: the promotion DEFERS on the transient blip. The account is untouched and, the point
	// of honest counting, it is NOT counted as recovered.
	res, err := svc.Sweep(env.ctx)
	if err != nil {
		t.Fatalf("tick 1 Sweep: %v", err)
	}
	if res.CodexRefreshRecovered != 0 {
		t.Fatalf("tick 1 CodexRefreshRecovered = %d, want 0 (a deferred promotion is not a recovery)", res.CodexRefreshRecovered)
	}
	acct1 := env.mustAccount(t, f.userID, f.accountID)
	if acct1.CoordState != codexCoordQuarantined || acct1.Generation != 0 {
		t.Fatalf("tick 1 account = (state=%q gen=%d), want (quarantined, 0) — a deferral must not mutate it", acct1.CoordState, acct1.Generation)
	}
	if len(acct1.RecoverySealed) == 0 {
		t.Fatalf("tick 1 recovery slot emptied, want retained for the retry")
	}

	// Tick 2: arm (c) re-lists the quarantined-with-recovery account; the discovery now MATCHES,
	// so the material is promoted — generation advanced, quarantine cleared, recovery emptied, and
	// the live login opens to the recovered access token. This tick IS counted.
	res2, err := svc.Sweep(env.ctx)
	if err != nil {
		t.Fatalf("tick 2 Sweep: %v", err)
	}
	if res2.CodexRefreshRecovered < 1 {
		t.Fatalf("tick 2 CodexRefreshRecovered = %d, want >= 1 (the always-on retry promotes)", res2.CodexRefreshRecovered)
	}
	acct2 := env.mustAccount(t, f.userID, f.accountID)
	if acct2.CoordState != "idle" {
		t.Fatalf("tick 2 coord_state = %q, want idle after promotion", acct2.CoordState)
	}
	if acct2.Generation <= 0 {
		t.Fatalf("tick 2 generation = %d, want > 0 (promotion advances)", acct2.Generation)
	}
	if len(acct2.RecoverySealed) != 0 || acct2.RecoveryGeneration.Valid {
		t.Fatalf("tick 2 recovery slot not cleared: sealed=%d gen=%v", len(acct2.RecoverySealed), acct2.RecoveryGeneration)
	}
	if blob := env.accountBlob(t, f.userID, f.accountID); blob.AccessToken != recTok {
		t.Fatalf("tick 2 promoted login access token = %q, want the recovered %q", blob.AccessToken, recTok)
	}
}

// intentListBarrierStore wraps *store.Queries to fire a barrier exactly ONCE, right AFTER the
// reconcile's ListUnresolvedCodexRefreshIntents read returns — the precise window in which the
// account's durable snapshot (read before the list) has gone stale. The barrier drives the
// winning op to a full commit on the RAW store, so the survivor's subsequent intent-state write
// operates on a stale verdict. Pointer receiver so `fired` persists across the one call.
type intentListBarrierStore struct {
	*store.Queries
	onIntentList func()
	fired        bool
}

func (h *intentListBarrierStore) ListUnresolvedCodexRefreshIntents(ctx context.Context, arg store.ListUnresolvedCodexRefreshIntentsParams) ([]store.CodexRefreshIntent, error) {
	items, err := h.Queries.ListUnresolvedCodexRefreshIntents(ctx, arg)
	if h.onIntentList != nil && !h.fired {
		h.fired = true
		h.onIntentList()
	}
	return items, err
}

// TestReconcileFencedIntentWriteSpareCommittedBarrierLiveDB (issue #1532, race 2) is the barrier
// regression for the fenced survivor intent-state write. The survivor computes a terminal verdict
// from an account snapshot read BEFORE it lists the rotating intents; the durable intent is
// inserted BEFORE the lease, so the survivor can select an idle account mid-window. In that
// window the winning op can acquire the lease, commit, mark its intent 'committed' and reset the
// account to idle — after which the survivor's stale verdict ('unrecoverable', from the idle
// gen-0 snapshot) must NOT be written.
//
// The barrier drives that exact interleaving: right after ListUnresolvedCodexRefreshIntents
// returns the still-'rotating' intent, the winning op (the SAME op id) runs a full
// acquire → commit → intent 'committed' → reset-idle cycle on the raw store. The fenced write
// then finds the intent no longer 'rotating' (and the account no longer at gen 0), so it matches
// 0 rows and the committed intent survives.
//
// MUTATION PROOF (documented, run, and restored): reverting the survivor main-loop write to the
// UNCONDITIONAL SetCodexRefreshIntentState overwrites the concurrently-committed intent
// committed→unrecoverable, and this test fails.
func TestReconcileFencedIntentWriteSpareCommittedBarrierLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fake := &fakeRefreshClient{}
	f := newRefreshFixture(t, env, fake)

	// Seed a rotating intent at from_generation 0 under a fresh op, with NO lease held — the
	// account stays idle at generation 0, the mid-window shape the survivor can pick up.
	op := uuid.New()
	if _, err := env.q.InsertCodexRefreshIntent(env.ctx, store.InsertCodexRefreshIntentParams{
		OperationID: op, UserID: f.userID, ProviderAccountID: f.accountID, FromGeneration: 0,
	}); err != nil {
		t.Fatalf("insert intent: %v", err)
	}

	winTok := codexToken("winner-access")
	f.svc.q = &intentListBarrierStore{
		Queries: env.q,
		onIntentList: func() {
			// The winning op — the SAME op as the seeded intent — runs a full rotation cycle to
			// full commit on the RAW store, in the window after the survivor listed the intent.
			future := pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true}
			if n, aerr := env.q.AcquireCodexRefreshLease(env.ctx, store.AcquireCodexRefreshLeaseParams{
				Op: op, Deadline: future, ID: f.accountID, UserID: f.userID, FromGeneration: 0,
			}); aerr != nil || n != 1 {
				t.Errorf("barrier acquire lease = (%d,%v), want (1,nil)", n, aerr)
				return
			}
			winBlob, merr := json.Marshal(codexLoginBlob{AccessToken: winTok, RefreshToken: codexToken("winner-refresh")}) //nolint:gosec // G117: synthetic winner login fixture, master-sealed below
			if merr != nil {
				t.Errorf("barrier marshal winner blob: %v", merr)
				return
			}
			sealedWin, serr := env.box.Seal(winBlob)
			if serr != nil {
				t.Errorf("barrier seal winner blob: %v", serr)
				return
			}
			if _, cerr := env.q.CommitCodexRefresh(env.ctx, store.CommitCodexRefreshParams{
				Sealed: sealedWin, SealedWith: store.SealedWithMaster, Op: op, ID: f.accountID, UserID: f.userID, FromGeneration: 0,
			}); cerr != nil {
				t.Errorf("barrier commit: %v", cerr)
				return
			}
			if _, serr := env.q.SetCodexRefreshIntentState(env.ctx, store.SetCodexRefreshIntentStateParams{
				State: codexIntentCommitted, OperationID: op, UserID: f.userID,
			}); serr != nil {
				t.Errorf("barrier set intent committed: %v", serr)
				return
			}
			if _, rerr := env.q.ResetCodexCoordIdle(env.ctx, store.ResetCodexCoordIdleParams{
				ID: f.accountID, UserID: f.userID,
			}); rerr != nil {
				t.Errorf("barrier reset idle: %v", rerr)
			}
		},
	}

	if _, err := f.svc.ReconcileUnresolvedCodexRefresh(env.ctx, f.userID, f.accountID); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// The fence blocked the stale 'unrecoverable' write: the intent stays 'committed'.
	if it := mustIntent(t, env, op, f.userID); it.State != codexIntentCommitted {
		t.Fatalf("intent state = %q, want committed (the fence must not overwrite a concurrently-committed intent)", it.State)
	}
	acct := env.mustAccount(t, f.userID, f.accountID)
	if acct.CoordState != "idle" || acct.Generation != 1 {
		t.Fatalf("account = (state=%q gen=%d), want (idle, 1) — the winning op's commit must stand", acct.CoordState, acct.Generation)
	}
	// The survivor never rotated: it only reconciles. The one commit came from the barrier, which
	// used the store directly, not the provider seam — so the fake's Refresh counter stays 0.
	if fake.calls != 0 {
		t.Fatalf("provider Refresh calls = %d, want 0 (the survivor must never exchange)", fake.calls)
	}
}

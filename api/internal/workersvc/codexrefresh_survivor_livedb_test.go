package workersvc

import (
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

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// mkReauthAccount inserts a fresh codex_provider_account owned by `user` (gen 0, rev 0,
// coord 'idle', empty recovery slot, reauth clear) and returns its id, so the reauth
// tests can drive MarkCodexReauthRequired / RefreshCodexAccountLogin against it.
func mkReauthAccount(ctx context.Context, t *testing.T, q *store.Queries, user uuid.UUID) uuid.UUID {
	t.Helper()
	acc, err := q.InsertCodexProviderAccount(ctx, store.InsertCodexProviderAccountParams{
		UserID:             user,
		ProviderUserID:     "provider-" + uuid.NewString(),
		WorkspaceAccountID: "workspace-" + uuid.NewString(),
		SealedLogin:        []byte("sealed-login"),
		SealedWith:         store.SealedWithMaster,
	})
	if err != nil {
		t.Fatalf("insert account: %v", err)
	}
	return acc.ID
}

func getCodexAccount(ctx context.Context, t *testing.T, q *store.Queries, user, id uuid.UUID) store.CodexProviderAccount {
	t.Helper()
	acc, err := q.GetCodexProviderAccountByID(ctx, store.GetCodexProviderAccountByIDParams{UserID: user, ID: id})
	if err != nil {
		t.Fatalf("GetCodexProviderAccountByID: %v", err)
	}
	return acc
}

// TestMarkCodexReauthRequiredLiveDB pins the reauth flag write (PRD #1209 M1): it succeeds
// from BOTH 'idle' and 'committed' recording the observed counters, and is rejected (0
// rows) on a stale generation, a stale credential_revision, a populated recovery slot, or
// an 'in_progress' account.
func TestMarkCodexReauthRequiredLiveDB(t *testing.T) {
	ctx, pool, q, user := codexLiveDB(t)

	mark := func(id uuid.UUID, gen, rev int64) int64 {
		t.Helper()
		n, err := q.MarkCodexReauthRequired(ctx, store.MarkCodexReauthRequiredParams{
			ID: id, UserID: user, ObservedGeneration: gen, ObservedCredentialRevision: rev,
		})
		if err != nil {
			t.Fatalf("MarkCodexReauthRequired: %v", err)
		}
		return n
	}

	// idle → succeeds and records the observation counters.
	idle := mkReauthAccount(ctx, t, q, user)
	if n := mark(idle, 0, 0); n != 1 {
		t.Fatalf("mark from idle affected %d rows, want 1", n)
	}
	acc := getCodexAccount(ctx, t, q, user, idle)
	if !acc.ReauthRequired || !acc.ReauthGeneration.Valid || acc.ReauthGeneration.Int64 != 0 ||
		!acc.ReauthCredentialRevision.Valid || acc.ReauthCredentialRevision.Int64 != 0 {
		t.Fatalf("after mark: reauth_required=%v gen=%+v rev=%+v, want (true, 0, 0)",
			acc.ReauthRequired, acc.ReauthGeneration, acc.ReauthCredentialRevision)
	}

	// committed → succeeds too (the fence admits 'idle' and 'committed').
	committed := mkReauthAccount(ctx, t, q, user)
	mustExec(ctx, t, pool, `UPDATE codex_provider_account SET coord_state='committed' WHERE id=$1`, committed)
	if n := mark(committed, 0, 0); n != 1 {
		t.Fatalf("mark from committed affected %d rows, want 1", n)
	}

	// stale generation → 0 rows, and the account is left unflagged.
	staleGen := mkReauthAccount(ctx, t, q, user)
	if n := mark(staleGen, 1, 0); n != 0 {
		t.Fatalf("mark with stale generation affected %d rows, want 0", n)
	}
	if getCodexAccount(ctx, t, q, user, staleGen).ReauthRequired {
		t.Fatal("stale-generation mark flagged the account anyway")
	}

	// stale credential_revision → 0 rows.
	staleRev := mkReauthAccount(ctx, t, q, user)
	if n := mark(staleRev, 0, 1); n != 0 {
		t.Fatalf("mark with stale credential_revision affected %d rows, want 0", n)
	}

	// populated recovery slot → 0 rows (a quarantine/recovery target is not flaggable).
	withRecovery := mkReauthAccount(ctx, t, q, user)
	mustExec(ctx, t, pool,
		`UPDATE codex_provider_account SET recovery_sealed=$2, recovery_sealed_with='master', recovery_generation=0 WHERE id=$1`,
		withRecovery, []byte("recovery"))
	if n := mark(withRecovery, 0, 0); n != 0 {
		t.Fatalf("mark with a populated recovery slot affected %d rows, want 0", n)
	}

	// in_progress → 0 rows (a refresh is mid-flight).
	inProgress := mkReauthAccount(ctx, t, q, user)
	mustExec(ctx, t, pool, `UPDATE codex_provider_account SET coord_state='in_progress' WHERE id=$1`, inProgress)
	if n := mark(inProgress, 0, 0); n != 0 {
		t.Fatalf("mark on an in_progress account affected %d rows, want 0", n)
	}
}

// TestRefreshCodexAccountLoginReauthArmLiveDB pins the two-arm restore (PRD #1209 M1): the
// reauth arm restores + clears reauth atomically when gen/cred match and recovery is null;
// the quarantined arm still works unchanged; and the reauth arm refuses a stale
// from_generation, a stale reauth_generation, a stale reauth_credential_revision, or an
// in_progress account.
func TestRefreshCodexAccountLoginReauthArmLiveDB(t *testing.T) {
	ctx, pool, q, user := codexLiveDB(t)

	refresh := func(id uuid.UUID, from int64) int64 {
		t.Helper()
		n, err := q.RefreshCodexAccountLogin(ctx, store.RefreshCodexAccountLoginParams{
			Sealed: []byte("new-sealed"), SealedWith: store.SealedWithDEK,
			ID: id, UserID: user, FromGeneration: from,
		})
		if err != nil {
			t.Fatalf("RefreshCodexAccountLogin: %v", err)
		}
		return n
	}

	// --- reauth arm: an idle account flagged for reauth restores + clears atomically. ---
	a := mkReauthAccount(ctx, t, q, user)
	if n, err := q.MarkCodexReauthRequired(ctx, store.MarkCodexReauthRequiredParams{
		ID: a, UserID: user, ObservedGeneration: 0, ObservedCredentialRevision: 0,
	}); err != nil || n != 1 {
		t.Fatalf("mark reauth = (%d, %v), want (1, nil)", n, err)
	}
	if n := refresh(a, 0); n != 1 {
		t.Fatalf("reauth arm refresh affected %d rows, want 1", n)
	}
	acc := getCodexAccount(ctx, t, q, user, a)
	if acc.Generation != 1 || acc.CoordState != "idle" {
		t.Fatalf("after reauth-arm refresh: gen=%d coord=%q, want (1, idle)", acc.Generation, acc.CoordState)
	}
	if acc.ReauthRequired || acc.ReauthGeneration.Valid || acc.ReauthCredentialRevision.Valid {
		t.Fatalf("reauth-arm refresh did not clear reauth: required=%v gen=%+v rev=%+v",
			acc.ReauthRequired, acc.ReauthGeneration, acc.ReauthCredentialRevision)
	}
	if string(acc.SealedLogin) != "new-sealed" || acc.SealedWith != store.SealedWithDEK {
		t.Fatalf("reauth-arm refresh did not install the new login: sealed=%q with=%q", acc.SealedLogin, acc.SealedWith)
	}

	// --- quarantined arm: unchanged behaviour (restores from a quarantine). ---
	quar := mkReauthAccount(ctx, t, q, user)
	mustExec(ctx, t, pool, `UPDATE codex_provider_account SET coord_state='quarantined' WHERE id=$1`, quar)
	if n := refresh(quar, 0); n != 1 {
		t.Fatalf("quarantined arm refresh affected %d rows, want 1", n)
	}
	if qacc := getCodexAccount(ctx, t, q, user, quar); qacc.Generation != 1 || qacc.CoordState != "idle" {
		t.Fatalf("after quarantined-arm refresh: gen=%d coord=%q, want (1, idle)", qacc.Generation, qacc.CoordState)
	}

	// --- reauth arm refuses a stale from_generation (the top-level CAS). ---
	staleFrom := mkReauthAccount(ctx, t, q, user)
	if _, err := q.MarkCodexReauthRequired(ctx, store.MarkCodexReauthRequiredParams{
		ID: staleFrom, UserID: user, ObservedGeneration: 0, ObservedCredentialRevision: 0,
	}); err != nil {
		t.Fatalf("mark reauth: %v", err)
	}
	if n := refresh(staleFrom, 5); n != 0 {
		t.Fatalf("refresh with a stale from_generation affected %d rows, want 0", n)
	}
	if getCodexAccount(ctx, t, q, user, staleFrom).Generation != 0 {
		t.Fatal("stale-from_generation refresh advanced the generation anyway")
	}

	// --- reauth arm refuses when reauth_generation no longer matches the live generation. ---
	staleReauth := mkReauthAccount(ctx, t, q, user)
	// Flag reauth against a generation the account is NOT at (simulating the account having
	// moved after the flag), so the reauth arm's reauth_generation=generation guard fails
	// while the top-level from_generation still matches.
	mustExec(ctx, t, pool,
		`UPDATE codex_provider_account SET reauth_required=true, reauth_generation=5, reauth_credential_revision=0 WHERE id=$1`,
		staleReauth)
	if n := refresh(staleReauth, 0); n != 0 {
		t.Fatalf("refresh with a stale reauth_generation affected %d rows, want 0", n)
	}

	// --- reauth arm refuses when reauth_credential_revision no longer matches the live
	// credential_revision (the OTHER half of the reauth fence; reauth_generation matches). ---
	staleReauthCred := mkReauthAccount(ctx, t, q, user)
	// Advance the account's live credential_revision to 1, then flag reauth against the OLD
	// credential_revision (0) while reauth_generation MATCHES the live generation (0). The
	// top-level from_generation and the reauth arm's reauth_generation=generation guard both
	// still hold, so ONLY the reauth_credential_revision=credential_revision guard can reject
	// this — dropping that conjunct would let the refresh restore (see the mutation check).
	mustExec(ctx, t, pool,
		`UPDATE codex_provider_account SET credential_revision=1, reauth_required=true, reauth_generation=0, reauth_credential_revision=0 WHERE id=$1`,
		staleReauthCred)
	if n := refresh(staleReauthCred, 0); n != 0 {
		t.Fatalf("refresh with a stale reauth_credential_revision affected %d rows, want 0", n)
	}
	// No restore happened: the generation is untouched and the reauth flag still stands.
	if got := getCodexAccount(ctx, t, q, user, staleReauthCred); got.Generation != 0 || got.CoordState != "idle" || !got.ReauthRequired {
		t.Fatalf("stale-reauth_credential_revision refresh mutated the account: gen=%d coord=%q reauth_required=%v, want (0, idle, true)",
			got.Generation, got.CoordState, got.ReauthRequired)
	}

	// --- reauth arm refuses an in_progress account (reauth flagged, not quarantined). ---
	inProgress := mkReauthAccount(ctx, t, q, user)
	mustExec(ctx, t, pool,
		`UPDATE codex_provider_account SET coord_state='in_progress', reauth_required=true, reauth_generation=0, reauth_credential_revision=0 WHERE id=$1`,
		inProgress)
	if n := refresh(inProgress, 0); n != 0 {
		t.Fatalf("refresh on an in_progress reauth-flagged account affected %d rows, want 0", n)
	}
}

// TestCommitAndPromoteClearReauthLiveDB pins that CommitCodexRefresh and
// PromoteCodexRecovery clear the reauth columns as they advance the generation (PRD #1209
// M1) — a verified install is exactly the "healthy again" signal a pending reauth waits for.
func TestCommitAndPromoteClearReauthLiveDB(t *testing.T) {
	ctx, pool, q, user := codexLiveDB(t)

	// --- CommitCodexRefresh clears reauth. ---
	commitAcc := mkReauthAccount(ctx, t, q, user)
	op := uuid.New()
	if n, err := q.AcquireCodexRefreshLease(ctx, store.AcquireCodexRefreshLeaseParams{
		Op: op, Deadline: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
		ID: commitAcc, UserID: user, FromGeneration: 0,
	}); err != nil || n != 1 {
		t.Fatalf("AcquireCodexRefreshLease = (%d, %v), want (1, nil)", n, err)
	}
	// Simulate a reauth flag pending on the in-flight account (coherence holds: required
	// true with both counters set).
	mustExec(ctx, t, pool,
		`UPDATE codex_provider_account SET reauth_required=true, reauth_generation=0, reauth_credential_revision=0 WHERE id=$1`,
		commitAcc)
	row, err := q.CommitCodexRefresh(ctx, store.CommitCodexRefreshParams{
		Sealed: []byte("committed"), SealedWith: store.SealedWithDEK, Op: op,
		ID: commitAcc, UserID: user, FromGeneration: 0,
	})
	if err != nil {
		t.Fatalf("CommitCodexRefresh: %v", err)
	}
	if row.Generation != 1 || row.CoordState != "committed" {
		t.Fatalf("commit returned gen=%d coord=%q, want (1, committed)", row.Generation, row.CoordState)
	}
	if acc := getCodexAccount(ctx, t, q, user, commitAcc); acc.ReauthRequired || acc.ReauthGeneration.Valid || acc.ReauthCredentialRevision.Valid {
		t.Fatalf("commit did not clear reauth: required=%v gen=%+v rev=%+v",
			acc.ReauthRequired, acc.ReauthGeneration, acc.ReauthCredentialRevision)
	}

	// --- PromoteCodexRecovery clears reauth. ---
	promoteAcc := mkReauthAccount(ctx, t, q, user)
	mustExec(ctx, t, pool,
		`UPDATE codex_provider_account
		 SET coord_state='quarantined', recovery_sealed=$2, recovery_sealed_with='master', recovery_generation=0,
		     reauth_required=true, reauth_generation=0, reauth_credential_revision=0
		 WHERE id=$1`,
		promoteAcc, []byte("recovery"))
	gen, err := q.PromoteCodexRecovery(ctx, store.PromoteCodexRecoveryParams{
		Sealed: []byte("promoted"), SealedWith: store.SealedWithDEK,
		ID: promoteAcc, UserID: user, FromGeneration: 0,
	})
	if err != nil {
		t.Fatalf("PromoteCodexRecovery: %v", err)
	}
	if gen != 1 {
		t.Fatalf("promote returned gen=%d, want 1", gen)
	}
	if acc := getCodexAccount(ctx, t, q, user, promoteAcc); acc.ReauthRequired || acc.ReauthGeneration.Valid || acc.ReauthCredentialRevision.Valid {
		t.Fatalf("promote did not clear reauth: required=%v gen=%+v rev=%+v",
			acc.ReauthRequired, acc.ReauthGeneration, acc.ReauthCredentialRevision)
	}
}

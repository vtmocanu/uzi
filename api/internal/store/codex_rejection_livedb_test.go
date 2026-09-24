package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #1594: the provider-rejection primitive (store.QuarantineRejectedCodexRefresh),
// the reauth_reason column + CHECK (00247/00248), and the clear sites that null it.

const (
	rejectGen = int64(7)
	rejectRev = int64(3)
)

// rejectionFixture is one account held in_progress by op at generation rejectGen (with
// credential_revision rejectRev) plus that op's 'rotating' intent from rejectGen.
type rejectionFixture struct {
	user, account, op uuid.UUID
}

func (f rejectionFixture) params() store.QuarantineRejectedCodexRefreshParams {
	return store.QuarantineRejectedCodexRefreshParams{
		UserID: f.user, AccountID: f.account, OperationID: f.op, FromGeneration: rejectGen,
	}
}

func newRejectionFixture(ctx context.Context, t *testing.T, pool *pgxpool.Pool, q *store.Queries, user uuid.UUID) rejectionFixture {
	t.Helper()
	account, err := q.InsertCodexProviderAccount(ctx, store.InsertCodexProviderAccountParams{
		UserID: user, ProviderUserID: "provider-" + uuid.NewString(),
		WorkspaceAccountID: "workspace-" + uuid.NewString(),
		SealedLogin:        []byte("sealed-login"), SealedWith: store.SealedWithMaster,
	})
	if err != nil {
		t.Fatalf("insert account: %v", err)
	}
	mustExec(ctx, t, pool,
		`UPDATE codex_provider_account SET generation = $2, credential_revision = $3 WHERE id = $1`,
		account.ID, rejectGen, rejectRev)
	op := uuid.New()
	if n, err := q.AcquireCodexRefreshLease(ctx, store.AcquireCodexRefreshLeaseParams{
		Op: op, Deadline: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
		ID: account.ID, UserID: user, FromGeneration: rejectGen,
	}); err != nil || n != 1 {
		t.Fatalf("AcquireCodexRefreshLease = (%d,%v), want (1,nil)", n, err)
	}
	if _, err := q.InsertCodexRefreshIntent(ctx, store.InsertCodexRefreshIntentParams{
		OperationID: op, UserID: user, ProviderAccountID: account.ID, FromGeneration: rejectGen,
	}); err != nil {
		t.Fatalf("insert intent: %v", err)
	}
	return rejectionFixture{user: user, account: account.ID, op: op}
}

// accountSnap is every account column the primitive could touch, rendered comparable.
type accountSnap struct {
	CoordState, CoordOp, ReauthGen, ReauthRev, Reason string
	RecoverySealed, RecoveryGen, RecoverySealedWith   string
	UpdatedAt                                         string
	Generation, CredentialRevision                    int64
	ReauthRequired, RecoveryNull                      bool
}

func snapAccount(ctx context.Context, t *testing.T, pool *pgxpool.Pool, id uuid.UUID) accountSnap {
	t.Helper()
	var s accountSnap
	err := pool.QueryRow(ctx, `
		SELECT coord_state, COALESCE(coord_operation_id::text, ''),
		       COALESCE(reauth_generation::text, ''), COALESCE(reauth_credential_revision::text, ''),
		       COALESCE(reauth_reason, ''),
		       COALESCE(encode(recovery_sealed, 'hex'), ''), COALESCE(recovery_generation::text, ''),
		       COALESCE(recovery_sealed_with, ''), updated_at::text,
		       generation, credential_revision, reauth_required,
		       (recovery_sealed IS NULL AND recovery_generation IS NULL AND recovery_sealed_with IS NULL)
		FROM codex_provider_account WHERE id = $1`, id).Scan(
		&s.CoordState, &s.CoordOp, &s.ReauthGen, &s.ReauthRev, &s.Reason,
		&s.RecoverySealed, &s.RecoveryGen, &s.RecoverySealedWith, &s.UpdatedAt,
		&s.Generation, &s.CredentialRevision, &s.ReauthRequired, &s.RecoveryNull)
	if err != nil {
		t.Fatalf("snap account: %v", err)
	}
	return s
}

func intentState(ctx context.Context, t *testing.T, pool *pgxpool.Pool, op uuid.UUID) (state, updatedAt string) {
	t.Helper()
	if err := pool.QueryRow(ctx,
		`SELECT state, updated_at::text FROM codex_refresh_intent WHERE operation_id = $1`, op).Scan(&state, &updatedAt); err != nil {
		t.Fatalf("read intent: %v", err)
	}
	return state, updatedAt
}

func mustReject(ctx context.Context, t *testing.T, pool *pgxpool.Pool, arg store.QuarantineRejectedCodexRefreshParams) store.CodexRejectionOutcome {
	t.Helper()
	out, err := store.QuarantineRejectedCodexRefresh(ctx, pool, arg)
	if err != nil {
		t.Fatalf("QuarantineRejectedCodexRefresh: %v", err)
	}
	return out
}

func assertRejectionApplied(ctx context.Context, t *testing.T, pool *pgxpool.Pool, f rejectionFixture) {
	t.Helper()
	got := snapAccount(ctx, t, pool, f.account)
	if got.CoordState != "quarantined" || !got.ReauthRequired || got.Reason != "provider_rejected" {
		t.Fatalf("account = %+v, want quarantined + reauth_required + reason provider_rejected", got)
	}
	if got.ReauthGen != "7" || got.ReauthRev != "3" {
		t.Fatalf("reauth counters = (%s,%s), want (7,3) = the account's generation/credential_revision", got.ReauthGen, got.ReauthRev)
	}
	if got.CoordOp != f.op.String() || got.Generation != rejectGen {
		t.Fatalf("account op/gen = (%s,%d), want (%s,%d) untouched", got.CoordOp, got.Generation, f.op, rejectGen)
	}
	if !got.RecoveryNull {
		t.Fatalf("a recovery column was written: %+v", got)
	}
	if st, _ := intentState(ctx, t, pool, f.op); st != "unrecoverable" {
		t.Fatalf("intent state = %q, want unrecoverable", st)
	}
}

// (a) + (f) + clear site: the ordinary rejection applies atomically, a second delivery
// is a no-op, and a verified re-login on the quarantined arm clears the reason.
func TestQuarantineRejectedCodexRefreshAppliedLiveDB(t *testing.T) {
	ctx, pool, q, user := codexLiveDB(t)
	f := newRejectionFixture(ctx, t, pool, q, user)

	if out := mustReject(ctx, t, pool, f.params()); out != store.CodexRejectionApplied {
		t.Fatalf("outcome = %v, want Applied", out)
	}
	assertRejectionApplied(ctx, t, pool, f)

	// (f) idempotent second delivery: nothing changes, updated_at included.
	beforeAcct := snapAccount(ctx, t, pool, f.account)
	_, beforeIntentAt := intentState(ctx, t, pool, f.op)
	if out := mustReject(ctx, t, pool, f.params()); out != store.CodexRejectionNotApplied {
		t.Fatalf("second delivery outcome = %v, want NotApplied", out)
	}
	if after := snapAccount(ctx, t, pool, f.account); after != beforeAcct {
		t.Fatalf("second delivery changed the account:\n before %+v\n after  %+v", beforeAcct, after)
	}
	if st, at := intentState(ctx, t, pool, f.op); st != "unrecoverable" || at != beforeIntentAt {
		t.Fatalf("second delivery changed the intent: (%s,%s), want (unrecoverable,%s)", st, at, beforeIntentAt)
	}

	// Clear site: RefreshCodexAccountLogin's quarantined arm nulls the reason with the flag.
	if n, err := q.RefreshCodexAccountLogin(ctx, store.RefreshCodexAccountLoginParams{
		Sealed: []byte("relogin"), SealedWith: store.SealedWithMaster,
		ID: f.account, UserID: user, FromGeneration: rejectGen,
	}); err != nil || n != 1 {
		t.Fatalf("RefreshCodexAccountLogin = (%d,%v), want (1,nil)", n, err)
	}
	if got := snapAccount(ctx, t, pool, f.account); got.ReauthRequired || got.Reason != "" || got.ReauthGen != "" {
		t.Fatalf("re-login left reauth state behind: %+v", got)
	}
}

// (b) a survivor reap landed between the provider's 401 and the transaction: the account
// is already quarantined under the same op and generation, and the rejection still applies.
func TestQuarantineRejectedCodexRefreshAfterReapLiveDB(t *testing.T) {
	ctx, pool, q, user := codexLiveDB(t)
	f := newRejectionFixture(ctx, t, pool, q, user)
	mustExec(ctx, t, pool, `UPDATE codex_provider_account SET lease_deadline = now() - interval '1 minute' WHERE id = $1`, f.account)
	if n, err := q.QuarantineExpiredCodexLease(ctx, store.QuarantineExpiredCodexLeaseParams{
		ID: f.account, UserID: user, Now: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}); err != nil || n != 1 {
		t.Fatalf("QuarantineExpiredCodexLease = (%d,%v), want (1,nil)", n, err)
	}

	if out := mustReject(ctx, t, pool, f.params()); out != store.CodexRejectionApplied {
		t.Fatalf("outcome = %v, want Applied", out)
	}
	assertRejectionApplied(ctx, t, pool, f)
}

// (c) the intent moves (on another connection) after the account lock and before the
// intent lock: nothing is applied and the account row is untouched.
func TestQuarantineRejectedCodexRefreshIntentMovedLiveDB(t *testing.T) {
	ctx, pool, q, user := codexLiveDB(t)
	f := newRejectionFixture(ctx, t, pool, q, user)
	before := snapAccount(ctx, t, pool, f.account)

	fired := false
	t.Cleanup(store.SetCodexRejectionAfterAccountLockForTest(func(hookCtx context.Context) {
		fired = true
		// The pool, not the tx: a separate connection. The intent is not yet locked, so
		// this does not block.
		if _, err := pool.Exec(hookCtx,
			`UPDATE codex_refresh_intent SET state = 'reconciled', updated_at = now() WHERE operation_id = $1`, f.op); err != nil {
			t.Errorf("move intent in hook: %v", err)
		}
	}))

	if out := mustReject(ctx, t, pool, f.params()); out != store.CodexRejectionNotApplied {
		t.Fatalf("outcome = %v, want NotApplied", out)
	}
	if !fired {
		t.Fatal("the after-account-lock hook never ran")
	}
	if after := snapAccount(ctx, t, pool, f.account); after != before {
		t.Fatalf("account changed:\n before %+v\n after  %+v", before, after)
	}
	if st, _ := intentState(ctx, t, pool, f.op); st != "reconciled" {
		t.Fatalf("intent state = %q, want reconciled (the concurrent write)", st)
	}
}

// (d) (e) (g): each account fence refuses, leaving the intent rotating and the account
// byte-identical.
func TestQuarantineRejectedCodexRefreshFencesLiveDB(t *testing.T) {
	cases := []struct {
		name  string
		setup func(ctx context.Context, t *testing.T, pool *pgxpool.Pool, q *store.Queries, f rejectionFixture) store.QuarantineRejectedCodexRefreshParams
	}{
		{"generation_mismatch", func(_ context.Context, _ *testing.T, _ *pgxpool.Pool, _ *store.Queries, f rejectionFixture) store.QuarantineRejectedCodexRefreshParams {
			p := f.params()
			p.FromGeneration = rejectGen - 1
			return p
		}},
		{"operation_mismatch", func(ctx context.Context, t *testing.T, pool *pgxpool.Pool, _ *store.Queries, f rejectionFixture) store.QuarantineRejectedCodexRefreshParams {
			mustExec(ctx, t, pool, `UPDATE codex_provider_account SET coord_operation_id = $2 WHERE id = $1`, f.account, uuid.New())
			return f.params()
		}},
		{"recovery_sealed_present", func(ctx context.Context, t *testing.T, _ *pgxpool.Pool, q *store.Queries, f rejectionFixture) store.QuarantineRejectedCodexRefreshParams {
			if n, err := q.SetCodexRecoverySlot(ctx, store.SetCodexRecoverySlotParams{
				Sealed: []byte("protected-good-login"), Gen: rejectGen,
				RecoverySealedWith: pgtype.Text{String: store.SealedWithMaster, Valid: true},
				ID:                 f.account, UserID: f.user, Op: f.op,
			}); err != nil || n != 1 {
				t.Fatalf("SetCodexRecoverySlot = (%d,%v), want (1,nil)", n, err)
			}
			return f.params()
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, pool, q, user := codexLiveDB(t)
			f := newRejectionFixture(ctx, t, pool, q, user)
			arg := tc.setup(ctx, t, pool, q, f)
			before := snapAccount(ctx, t, pool, f.account)
			_, beforeIntentAt := intentState(ctx, t, pool, f.op)

			if out := mustReject(ctx, t, pool, arg); out != store.CodexRejectionNotApplied {
				t.Fatalf("outcome = %v, want NotApplied", out)
			}
			if after := snapAccount(ctx, t, pool, f.account); after != before {
				t.Fatalf("account changed:\n before %+v\n after  %+v", before, after)
			}
			if st, at := intentState(ctx, t, pool, f.op); st != "rotating" || at != beforeIntentAt {
				t.Fatalf("intent = (%s,%s), want (rotating,%s) untouched", st, at, beforeIntentAt)
			}
		})
	}
}

// 00247/00248: a reason needs a raised flag and must come from the closed set.
func TestCodexReauthReasonCheckLiveDB(t *testing.T) {
	ctx, pool, q, user := codexLiveDB(t)
	f := newRejectionFixture(ctx, t, pool, q, user)

	_, err := pool.Exec(ctx, `UPDATE codex_provider_account SET reauth_reason = 'provider_rejected' WHERE id = $1`, f.account)
	if code := pgCode(err); code != "23514" {
		t.Fatalf("reason without reauth_required: err = %v (code %q), want 23514", err, code)
	}
	_, err = pool.Exec(ctx, `UPDATE codex_provider_account
		SET reauth_required = true, reauth_generation = generation, reauth_credential_revision = credential_revision,
		    reauth_reason = 'bogus' WHERE id = $1`, f.account)
	if code := pgCode(err); code != "23514" {
		t.Fatalf("out-of-set reason: err = %v (code %q), want 23514", err, code)
	}
	var validated bool
	if err := pool.QueryRow(ctx, `SELECT convalidated FROM pg_constraint WHERE conname = 'codex_provider_account_reauth_reason_check'`).Scan(&validated); err != nil || !validated {
		t.Fatalf("reason CHECK validated = (%v,%v), want (true,nil) after 00248", validated, err)
	}
}

// raiseReasonDirect puts a reauth flag with reason provider_rejected on the account so a
// clear site can be observed nulling it.
func raiseReasonDirect(ctx context.Context, t *testing.T, pool *pgxpool.Pool, account uuid.UUID) {
	t.Helper()
	mustExec(ctx, t, pool, `UPDATE codex_provider_account
		SET reauth_required = true, reauth_generation = generation, reauth_credential_revision = credential_revision,
		    reauth_reason = 'provider_rejected' WHERE id = $1`, account)
}

// Every other statement that writes the reauth flag nulls the reason in the same write.
func TestCodexReauthReasonClearSitesLiveDB(t *testing.T) {
	t.Run("CommitCodexRefresh", func(t *testing.T) {
		ctx, pool, q, user := codexLiveDB(t)
		f := newRejectionFixture(ctx, t, pool, q, user)
		raiseReasonDirect(ctx, t, pool, f.account)
		if _, err := q.CommitCodexRefresh(ctx, store.CommitCodexRefreshParams{
			Sealed: []byte("new"), SealedWith: store.SealedWithMaster, Op: f.op,
			ID: f.account, UserID: user, FromGeneration: rejectGen,
		}); err != nil {
			t.Fatalf("CommitCodexRefresh: %v", err)
		}
		if got := snapAccount(ctx, t, pool, f.account); got.ReauthRequired || got.Reason != "" {
			t.Fatalf("commit left reauth state: %+v", got)
		}
	})
	t.Run("PromoteCodexRecovery", func(t *testing.T) {
		ctx, pool, q, user := codexLiveDB(t)
		f := newRejectionFixture(ctx, t, pool, q, user)
		if n, err := q.SetCodexRecoverySlot(ctx, store.SetCodexRecoverySlotParams{
			Sealed: []byte("good"), Gen: rejectGen,
			RecoverySealedWith: pgtype.Text{String: store.SealedWithMaster, Valid: true},
			ID:                 f.account, UserID: user, Op: f.op,
		}); err != nil || n != 1 {
			t.Fatalf("SetCodexRecoverySlot = (%d,%v)", n, err)
		}
		raiseReasonDirect(ctx, t, pool, f.account)
		if _, err := q.PromoteCodexRecovery(ctx, store.PromoteCodexRecoveryParams{
			Sealed: []byte("resealed"), SealedWith: store.SealedWithMaster,
			ID: f.account, UserID: user, FromGeneration: rejectGen,
		}); err != nil {
			t.Fatalf("PromoteCodexRecovery: %v", err)
		}
		if got := snapAccount(ctx, t, pool, f.account); got.ReauthRequired || got.Reason != "" {
			t.Fatalf("promotion left reauth state: %+v", got)
		}
	})
	t.Run("MarkCodexReauthRequired", func(t *testing.T) {
		ctx, pool, q, user := codexLiveDB(t)
		f := newRejectionFixture(ctx, t, pool, q, user)
		mustExec(ctx, t, pool, `UPDATE codex_provider_account SET coord_state = 'idle', coord_operation_id = NULL, lease_deadline = NULL WHERE id = $1`, f.account)
		raiseReasonDirect(ctx, t, pool, f.account)
		if n, err := q.MarkCodexReauthRequired(ctx, store.MarkCodexReauthRequiredParams{
			ObservedGeneration: rejectGen, ObservedCredentialRevision: rejectRev, ID: f.account, UserID: user,
		}); err != nil || n != 1 {
			t.Fatalf("MarkCodexReauthRequired = (%d,%v), want (1,nil)", n, err)
		}
		if got := snapAccount(ctx, t, pool, f.account); !got.ReauthRequired || got.Reason != "" {
			t.Fatalf("poll-path flag = %+v, want reauth_required with no reason", got)
		}
	})
}

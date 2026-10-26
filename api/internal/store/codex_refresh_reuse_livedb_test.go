package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

func reuseFixture(t *testing.T) (context.Context, *store.Queries, rejectionFixture, uuid.UUID, store.CodexRejectionTxBeginner) {
	t.Helper()
	ctx, pool, q, user := codexLiveDB(t)
	// These fixtures include deliberately unopenable recovery envelopes. Remove
	// their enabled accounts before a later package's global survivor scans the
	// shared LiveDB database. Registered after pool.Close, so cleanup runs first.
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := pool.Exec(cleanupCtx, "DELETE FROM users WHERE id=$1", user); err != nil {
			t.Errorf("delete reuse fixture user %s: %v", user, err)
		}
	})
	f := newRejectionFixture(ctx, t, pool, q, user)
	secret, err := insertSecret(ctx, pool, user, store.KindCodexAuth, "reuse-"+uuid.NewString(), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.InsertCodexCredentialState(ctx, store.InsertCodexCredentialStateParams{UserID: user, UserSecretID: secret, Status: "staging"}); err != nil {
		t.Fatal(err)
	}
	if n, err := q.LinkCodexCredentialState(ctx, store.LinkCodexCredentialStateParams{UserID: user, UserSecretID: secret, ProviderAccountID: pgtype.UUID{Bytes: f.account, Valid: true}}); err != nil || n != 1 {
		t.Fatalf("link=%d,%v", n, err)
	}
	if n, err := q.RecordCodexRefreshProviderError(ctx, store.RecordCodexRefreshProviderErrorParams{UserID: user, AccountID: f.account, OperationID: f.op, FromGeneration: rejectGen}); err != nil || n != 1 {
		t.Fatalf("receipt=%d,%v", n, err)
	}
	mustExec(ctx, t, pool, "UPDATE codex_provider_account SET coord_state='quarantined', lease_deadline=NULL WHERE id=$1", f.account)
	return ctx, q, f, secret, pool
}

func reuseArg(f rejectionFixture) store.CodexRefreshReuseParams {
	return store.CodexRefreshReuseParams{UserID: f.user, AccountID: f.account, OperationID: f.op, FromGeneration: rejectGen}
}

func TestCodexRefreshReuseAdmissionLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name, change string
		want         bool
	}{
		{"eligible", "", true},
		{"no_receipt", "UPDATE codex_refresh_intent SET provider_error_at=NULL WHERE operation_id=$1", false},
		{"consumed", "UPDATE codex_refresh_intent SET reuse_attempted_at=now() WHERE operation_id=$1", false},
		{"too_old", "UPDATE codex_refresh_intent SET created_at=now()-interval '30 minutes' WHERE operation_id=$1", false},
		{"terminal", "UPDATE codex_refresh_intent SET state='unrecoverable' WHERE operation_id=$1", false},
		{"owner_restored", "UPDATE codex_provider_account SET generation=generation+1 WHERE coord_operation_id=$1", false},
		{"other_op", "UPDATE codex_provider_account SET coord_operation_id=NULL WHERE coord_operation_id=$1", false},
		{"not_quarantined", "UPDATE codex_provider_account SET coord_state='idle',coord_operation_id=NULL WHERE coord_operation_id=$1", false},
		{"recovery", "UPDATE codex_provider_account SET recovery_sealed='x',recovery_generation=generation,recovery_sealed_with='master' WHERE coord_operation_id=$1", false},
		{"reauth", "UPDATE codex_provider_account SET reauth_required=true,reauth_generation=generation,reauth_credential_revision=credential_revision WHERE coord_operation_id=$1", false},
		{"disabled", "UPDATE user_secrets SET disabled_at=now() WHERE id=$1", false},
		{"unlinked", "UPDATE codex_credential_state SET status='staging',provider_account_id=NULL WHERE user_secret_id=$1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, q, f, secret, b := reuseFixture(t)
			// The pool implements both the transaction and execution surfaces.
			db := b.(store.DBTX)
			if tc.change != "" {
				id := f.op
				if tc.name == "disabled" || tc.name == "unlinked" {
					id = secret
				}
				if _, err := db.Exec(ctx, tc.change, id); err != nil {
					t.Fatal(err)
				}
			}
			ok, err := store.AdmitCodexRefreshReuse(ctx, b, reuseArg(f))
			if err != nil || ok != tc.want {
				t.Fatalf("admit=%v,%v want %v", ok, err, tc.want)
			}
			it, err := q.GetCodexRefreshIntent(ctx, store.GetCodexRefreshIntentParams{UserID: f.user, OperationID: f.op})
			if err != nil {
				t.Fatal(err)
			}
			if tc.want {
				if !it.ReuseAttemptedAt.Valid || it.State != "rotating" {
					t.Fatal("admission did not consume while preserving rotating")
				}
				ok, err = store.AdmitCodexRefreshReuse(ctx, b, reuseArg(f))
				if err != nil || ok {
					t.Fatalf("second admission=%v,%v", ok, err)
				}
			} else if tc.name != "consumed" && it.ReuseAttemptedAt.Valid {
				t.Fatal("denied admission consumed retry")
			}
		})
	}
}

// CI-owned execution: the admitted retry may terminate before its in-flight
// bound, but cannot reverse reconciliation, a restore, or protected recovery.
func TestCodexRefreshReuseTerminalFenceLiveDB(t *testing.T) {
	for _, kind := range []string{"current", "reconciled", "restored", "operation", "recovery", "unconsumed", "no_receipt", "foreign_owner"} {
		t.Run(kind, func(t *testing.T) {
			ctx, q, f, _, b := reuseFixture(t)
			if ok, err := store.AdmitCodexRefreshReuse(ctx, b, reuseArg(f)); err != nil || !ok {
				t.Fatalf("admit=%v,%v", ok, err)
			}
			var change string
			switch kind {
			case "reconciled":
				change = "UPDATE codex_refresh_intent SET state='reconciled' WHERE operation_id=$1"
			case "restored":
				change = "UPDATE codex_provider_account SET generation=generation+1,coord_state='idle',coord_operation_id=NULL WHERE coord_operation_id=$1"
			case "operation":
				change = "UPDATE codex_provider_account SET coord_operation_id=NULL WHERE coord_operation_id=$1"
			case "recovery":
				change = "UPDATE codex_provider_account SET recovery_sealed='protected',recovery_generation=generation,recovery_sealed_with='master' WHERE coord_operation_id=$1"
			case "unconsumed":
				change = "UPDATE codex_refresh_intent SET reuse_attempted_at=NULL WHERE operation_id=$1"
			case "no_receipt":
				change = "UPDATE codex_refresh_intent SET provider_error_at=NULL WHERE operation_id=$1"
			}
			if change != "" {
				if _, err := b.(store.DBTX).Exec(ctx, change, f.op); err != nil {
					t.Fatal(err)
				}
			}
			arg := store.SetCodexRefreshReuseUnrecoverableParams{
				UserID: f.user, AccountID: f.account, OperationID: f.op, FromGeneration: rejectGen,
			}
			if kind == "foreign_owner" {
				arg.UserID = uuid.New()
			}
			n, err := q.SetCodexRefreshReuseUnrecoverable(ctx, arg)
			want := int64(0)
			if kind == "current" {
				want = 1
			}
			if err != nil || n != want {
				t.Fatalf("terminate=%d,%v want %d", n, err, want)
			}
			it, err := q.GetCodexRefreshIntent(ctx, store.GetCodexRefreshIntentParams{UserID: f.user, OperationID: f.op})
			if err != nil {
				t.Fatal(err)
			}
			state := "rotating"
			switch kind {
			case "current":
				state = "unrecoverable"
			case "reconciled":
				state = "reconciled"
			}
			if it.State != state {
				t.Fatalf("state=%s want %s", it.State, state)
			}
		})
	}
}

func TestCodexRefreshReuseConcurrentAdmissionLiveDB(t *testing.T) {
	ctx, _, f, _, b := reuseFixture(t)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	start := make(chan struct{})
	type result struct {
		admitted bool
		err      error
	}
	results := make(chan result, 2)
	// Two independent bounded transactions compete; one failure does not stop its sibling.
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			ok, err := store.AdmitCodexRefreshReuse(ctx, b, reuseArg(f))
			results <- result{ok, err}
		}()
	}
	close(start)
	admitted := 0
	for i := 0; i < 2; i++ {
		select {
		case r := <-results:
			if r.err != nil {
				t.Errorf("admission: %v", r.err)
			}
			if r.admitted {
				admitted++
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if admitted != 1 {
		t.Fatalf("admitted %d retries, want exactly one", admitted)
	}
}

func TestCodexRefreshReuseResolutionFenceLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name, change, state string
		want                int64
	}{
		{"unconsumed", "", "unrecoverable", 0},
		{"lying_reconciled", "", "reconciled", 0},
		{"expired", "UPDATE codex_refresh_intent SET created_at=now()-interval '31 minutes' WHERE operation_id=$1", "unrecoverable", 1},
		{"inflight", "UPDATE codex_refresh_intent SET reuse_attempted_at=now(),created_at=now()-interval '1 hour' WHERE operation_id=$1", "unrecoverable", 0},
		{"spent", "UPDATE codex_refresh_intent SET reuse_attempted_at=now()-interval '2 minutes' WHERE operation_id=$1", "unrecoverable", 1},
		{"spent_lie", "UPDATE codex_refresh_intent SET reuse_attempted_at=now()-interval '2 minutes' WHERE operation_id=$1", "reconciled", 0},
		{"recovery", "UPDATE codex_provider_account SET recovery_sealed='x',recovery_generation=generation,recovery_sealed_with='master' WHERE coord_operation_id=$1", "reconciled", 1},
		{"advanced", "UPDATE codex_provider_account SET generation=generation+1 WHERE coord_operation_id=$1", "reconciled", 1},
		{"no_receipt", "UPDATE codex_refresh_intent SET provider_error_at=NULL WHERE operation_id=$1", "unrecoverable", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, q, f, _, b := reuseFixture(t)
			if tc.change != "" {
				if _, err := b.(store.DBTX).Exec(ctx, tc.change, f.op); err != nil {
					t.Fatal(err)
				}
			}
			gen := rejectGen
			if tc.name == "advanced" {
				gen++
			}
			n, err := q.SetCodexRefreshIntentStateFenced(ctx, store.SetCodexRefreshIntentStateFencedParams{
				UserID: f.user, OperationID: f.op, ExpectedGeneration: gen, State: tc.state, ReuseInFlightMicroseconds: time.Minute.Microseconds(),
			})
			if err != nil || n != tc.want {
				t.Fatalf("resolve=%d,%v want %d", n, err, tc.want)
			}
		})
	}
}

type clockStartReuseBeginner struct {
	store.CodexRejectionTxBeginner
	started chan time.Time
}

func (b clockStartReuseBeginner) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := b.CodexRejectionTxBeginner.Begin(ctx)
	if err != nil {
		return nil, err
	}
	var started time.Time
	if err := tx.QueryRow(ctx, "SELECT now()").Scan(&started); err != nil {
		_ = tx.Rollback(ctx)
		return nil, err
	}
	b.started <- started
	return tx, nil
}

// No sleeps: hold the user lock, observe admission's transaction-start instant,
// then release at a later DB instant. The expiry fixture straddles those instants.
func TestCodexRefreshReuseFreshAdmissionClockLiveDB(t *testing.T) {
	for _, kind := range []string{"deadline_crossed", "stamp_after_wait"} {
		t.Run(kind, func(t *testing.T) {
			ctx, q, f, _, b := reuseFixture(t)
			ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			owner, err := b.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = owner.Rollback(ctx) }()
			if err := store.LockSecretMutation(ctx, owner, f.user); err != nil {
				t.Fatal(err)
			}
			started := make(chan time.Time, 1)
			results := make(chan struct {
				ok  bool
				err error
			}, 1)
			go func() {
				ok, err := store.AdmitCodexRefreshReuse(ctx, clockStartReuseBeginner{b, started}, reuseArg(f))
				results <- struct {
					ok  bool
					err error
				}{ok, err}
			}()
			var transactionStart time.Time
			select {
			case transactionStart = <-started:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if kind == "deadline_crossed" {
				created := transactionStart.Add(-30 * time.Minute).Add(time.Microsecond)
				if _, err := owner.Exec(ctx, "UPDATE codex_refresh_intent SET created_at=$2 WHERE operation_id=$1", f.op, created); err != nil {
					t.Fatal(err)
				}
			}
			releasedAt, err := store.New(owner).LeaseClockNow(ctx)
			if err != nil || !releasedAt.Valid || !releasedAt.Time.After(transactionStart.Add(time.Microsecond)) {
				t.Fatalf("database clock did not advance across barrier: %v,%v", releasedAt, err)
			}
			if err := owner.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case result := <-results:
				if result.err != nil || result.ok != (kind == "stamp_after_wait") {
					t.Fatalf("admission after wait=%v,%v", result.ok, result.err)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			it, err := q.GetCodexRefreshIntent(ctx, store.GetCodexRefreshIntentParams{UserID: f.user, OperationID: f.op})
			if err != nil {
				t.Fatal(err)
			}
			if kind == "deadline_crossed" {
				if it.ReuseAttemptedAt.Valid {
					t.Fatal("expired retry consumed after lock wait")
				}
			} else if !it.ReuseAttemptedAt.Valid || it.ReuseAttemptedAt.Time.Before(releasedAt.Time) {
				t.Fatal("lock wait backdated reuse and spent its in-flight budget")
			}
		})
	}
}

// A verdict computed before the recovery write must not declare known surviving
// material lost, even after the retry's pending-write policy bound has expired.
func TestCodexRefreshReuseLateRecoveryFenceLiveDB(t *testing.T) {
	ctx, q, f, _, b := reuseFixture(t)
	if _, err := b.(store.DBTX).Exec(ctx,
		"UPDATE codex_refresh_intent SET reuse_attempted_at=now()-interval '2 minutes' WHERE operation_id=$1", f.op); err != nil {
		t.Fatal(err)
	}
	if n, err := q.SetCodexRecoverySlot(ctx, store.SetCodexRecoverySlotParams{
		ID: f.account, UserID: f.user, Op: f.op, Gen: rejectGen,
		Sealed: []byte("protected"), RecoverySealedWith: pgtype.Text{String: store.SealedWithMaster, Valid: true},
	}); err != nil || n != 1 {
		t.Fatalf("protect=%d,%v", n, err)
	}
	params := store.SetCodexRefreshIntentStateFencedParams{
		UserID: f.user, OperationID: f.op, ExpectedGeneration: rejectGen,
		State: "unrecoverable", ReuseInFlightMicroseconds: time.Minute.Microseconds(),
	}
	if n, err := q.SetCodexRefreshIntentStateFenced(ctx, params); err != nil || n != 0 {
		t.Fatalf("stale destructive verdict=%d,%v", n, err)
	}
	params.State = "reconciled"
	if n, err := q.SetCodexRefreshIntentStateFenced(ctx, params); err != nil || n != 1 {
		t.Fatalf("evidence-backed verdict=%d,%v", n, err)
	}
}

// observedReuseBeginner proves that the first statement is the user lock, then
// pauses before executing it so the owner change commits before admission proceeds.
type observedReuseBeginner struct {
	store.CodexRejectionTxBeginner
	entered chan string
	proceed chan struct{}
}
type observedReuseTx struct {
	pgx.Tx
	entered chan string
	proceed chan struct{}
}

func (b observedReuseBeginner) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := b.CodexRejectionTxBeginner.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &observedReuseTx{Tx: tx, entered: b.entered, proceed: b.proceed}, nil
}
func (tx *observedReuseTx) Exec(ctx context.Context, sql string, args ...interface{}) (pgconn.CommandTag, error) {
	if tx.entered != nil {
		tx.entered <- sql
		select {
		case <-tx.proceed:
		case <-ctx.Done():
			return pgconn.CommandTag{}, ctx.Err()
		}
		tx.entered = nil
	}
	return tx.Tx.Exec(ctx, sql, args...)
}

func TestCodexRefreshReuseOwnerWinsLiveDB(t *testing.T) {
	for _, kind := range []string{"restore", "disable", "operation"} {
		t.Run(kind, func(t *testing.T) {
			ctx, q, f, secret, b := reuseFixture(t)
			ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			owner, err := b.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = owner.Rollback(ctx) }()
			if err := store.LockSecretMutation(ctx, owner, f.user); err != nil {
				t.Fatal(err)
			}
			entered := make(chan string, 1)
			proceed := make(chan struct{})
			result := make(chan struct {
				ok  bool
				err error
			}, 1)
			go func() {
				ok, err := store.AdmitCodexRefreshReuse(ctx, observedReuseBeginner{b, entered, proceed}, reuseArg(f))
				result <- struct {
					ok  bool
					err error
				}{ok, err}
			}()
			select {
			case first := <-entered:
				if first != "SELECT pg_advisory_xact_lock($1, $2)" {
					close(proceed)
					t.Fatalf("first statement=%q", first)
				}
			case <-ctx.Done():
				close(proceed)
				t.Fatal(ctx.Err())
			}
			switch kind {
			case "restore":
				_, err = owner.Exec(ctx, "UPDATE codex_provider_account SET generation=generation+1 WHERE id=$1", f.account)
			case "disable":
				_, err = owner.Exec(ctx, "UPDATE user_secrets SET disabled_at=now() WHERE id=$1", secret)
			case "operation":
				_, err = owner.Exec(ctx, "UPDATE codex_provider_account SET coord_operation_id=$2 WHERE id=$1", f.account, uuid.New())
			}
			if err != nil {
				close(proceed)
				t.Fatal(err)
			}
			if err = owner.Commit(ctx); err != nil {
				close(proceed)
				t.Fatal(err)
			}
			close(proceed)
			select {
			case r := <-result:
				if r.err != nil || r.ok {
					t.Fatalf("owner lost: %+v", r)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			it, err := q.GetCodexRefreshIntent(ctx, store.GetCodexRefreshIntentParams{UserID: f.user, OperationID: f.op})
			if err != nil || it.ReuseAttemptedAt.Valid {
				t.Fatalf("owner race consumed: %+v,%v", it, err)
			}
		})
	}
}

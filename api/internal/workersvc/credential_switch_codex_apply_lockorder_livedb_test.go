package workersvc

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// codexLockOrderFixture holds a run on subscription alias A with a pending
// request for a distinct linked subscription alias B, both with accounts.
type codexLockOrderFixture struct {
	f               applyFx
	run             uuid.UUID
	aliasA, aliasB  uuid.UUID
	accountA, acctB uuid.UUID
	req             CredentialSwitchApplyRequest
	snapshot        func() string
}

func newCodexLockOrderFixture(t *testing.T) codexLockOrderFixture {
	t.Helper()
	f, run, accountA, _ := codexApplyFromSubscription(t)
	aliasA := uuid.UUID(mustRun(t, f.env, run).CodexSecretID.Bytes)
	aliasB := f.env.seedLinkedSubscription(t, f.o.userID, "lockorder-B", codexToken("lo-B-access"), codexToken("lo-B-refresh"))
	stateB, err := f.env.q.GetCodexCredentialState(f.env.ctx, store.GetCodexCredentialStateParams{UserSecretID: aliasB, UserID: f.o.userID})
	if err != nil || !stateB.ProviderAccountID.Valid {
		t.Fatal("B account unavailable")
	}
	accountB := uuid.UUID(stateB.ProviderAccountID.Bytes)
	if accountA == accountB || aliasA == aliasB {
		t.Fatal("lock order requires distinct aliases and accounts")
	}
	if _, err := f.svc.SetRunCredential(f.env.ctx, f.o.userID, run, BindModePinned, &aliasB); err != nil {
		t.Fatal("request B failed")
	}
	x := codexLockOrderFixture{f: f, run: run, aliasA: aliasA, aliasB: aliasB, accountA: accountA, acctB: accountB, req: f.req(f.opOf(t, run))}
	x.snapshot = func() string {
		t.Helper()
		var s string
		err := f.env.pool.QueryRow(f.env.ctx, `SELECT jsonb_build_object(
			'accounts',(SELECT coalesce(jsonb_agg(to_jsonb(a) ORDER BY a.id),'[]'::jsonb) FROM codex_provider_account a
				WHERE a.id IN ($1,$2) AND a.user_id=$5),
			'intents',(SELECT coalesce(jsonb_agg(to_jsonb(i) ORDER BY i.operation_id),'[]'::jsonb) FROM codex_refresh_intent i
				WHERE i.provider_account_id IN ($1,$2) AND i.user_id=$5),
			'aliases',(SELECT coalesce(jsonb_agg(to_jsonb(c) ORDER BY c.user_secret_id),'[]'::jsonb) FROM codex_credential_state c
				WHERE c.user_secret_id IN ($3,$4) AND c.user_id=$5))::text`,
			accountA, accountB, aliasA, aliasB, f.o.userID).Scan(&s)
		if err != nil {
			t.Fatal("alias/account/intent snapshot failed")
		}
		return s
	}
	return x
}

func (x codexLockOrderFixture) wantBusy(t *testing.T, ctx context.Context, leaf string) {
	t.Helper()
	worker := x.f.worker(t)
	x.f.noWrite(t, x.run, func() {
		got, err := x.f.svc.ApplyCredentialSwitch(ctx, worker, x.run, x.req)
		if err == nil && got.Outcome == ApplyOutcomeApplied {
			t.Fatalf("%s: apply was APPLIED; expected credential_busy", leaf)
		}
		if err != nil || got.Outcome != ApplyOutcomeNotApplied || got.Reason != applyReasonCredentialBusy || got.Codex != nil || got.Token != "" {
			t.Fatalf("%s: contended apply did not return an empty credential_busy decision (err=%v outcome=%q reason=%q)", leaf, err, got.Outcome, got.Reason)
		}
	})
}

// codexStatementTimeoutBeginner bounds every statement in apply's transaction
// with a test-only statement_timeout, set right after the service's isolation
// statement. A NOWAIT refusal (SQLSTATE 55P03) returns at once and maps to
// credential_busy. Any WAIT on a held row — at a preflight lock statement whose
// NOWAIT was removed, or at a later statement such as a foreign-key check —
// is cancelled with SQLSTATE 57014, which codexSwitchReadFailure does NOT map
// to busy, so it surfaces as an apply error at the busy-decision assertion.
// (lock_timeout would not do: its expiry is also 55P03 and would read as busy.)
type codexStatementTimeoutBeginner struct {
	TxBeginner
	armed *int
}

type codexStatementTimeoutTx struct {
	pgx.Tx
	armed *int
}

func (b codexStatementTimeoutBeginner) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := b.TxBeginner.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return codexStatementTimeoutTx{Tx: tx, armed: b.armed}, nil
}

func (tx codexStatementTimeoutTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	tag, err := tx.Tx.Exec(ctx, sql, args...)
	if err == nil && strings.HasPrefix(sql, "SET TRANSACTION ISOLATION LEVEL") {
		if _, err := tx.Tx.Exec(ctx, "SET LOCAL statement_timeout = '1s'"); err != nil {
			return tag, err
		}
		*tx.armed++
	}
	return tag, err
}

// A held alias row or enablement row, on either side, must make NOWAIT return
// the public busy decision with no writes; the same request then applies. A
// successful blocker Scan is the readiness barrier. Apply's statements are
// bounded by codexStatementTimeoutBeginner, so apply waiting on the held row
// anywhere (a blocking lock in place of NOWAIT, or a later statement such as the
// runs.codex_secret_id foreign-key check on B's user_secrets row) fails at the
// busy assertion with an error instead of reading as busy or hitting the deadline.
func TestCodexApplyAliasRowContentionLiveDB(t *testing.T) {
	const aliasSQL = "SELECT user_secret_id FROM codex_credential_state WHERE user_secret_id=$1 AND user_id=$2 FOR UPDATE"
	const enablementSQL = "SELECT id FROM user_secrets WHERE id=$1 AND user_id=$2 FOR UPDATE"
	for _, leaf := range []struct {
		name   string
		sql    string
		target bool
	}{
		{"old_alias", aliasSQL, false},
		{"target_alias", aliasSQL, true},
		{"old_enablement", enablementSQL, false},
		{"target_enablement", enablementSQL, true},
	} {
		t.Run(leaf.name, func(t *testing.T) {
			x := newCodexLockOrderFixture(t)
			armed := 0
			x.f.svc.SetTxBeginner(codexStatementTimeoutBeginner{TxBeginner: x.f.env.pool, armed: &armed})
			held := x.aliasA
			if leaf.target {
				held = x.aliasB
			}
			before := x.snapshot()
			ctx, cancel := context.WithTimeout(x.f.env.ctx, 10*time.Second)
			defer cancel()
			blocker, err := x.f.env.pool.Begin(ctx)
			if err != nil {
				t.Fatal("begin blocker failed")
			}
			defer func() { _ = blocker.Rollback(x.f.env.ctx) }()
			var locked uuid.UUID
			if err := blocker.QueryRow(ctx, leaf.sql, held, x.f.o.userID).Scan(&locked); err != nil || locked != held {
				t.Fatalf("%s: blocker not ready", leaf.name)
			}
			x.wantBusy(t, ctx, leaf.name)
			if armed == 0 {
				t.Fatalf("%s: statement_timeout was never armed in apply's transaction", leaf.name)
			}
			if x.snapshot() != before {
				t.Fatalf("%s: contended apply changed account, intent or alias state", leaf.name)
			}
			if err := blocker.Rollback(ctx); err != nil {
				t.Fatal("release blocker failed")
			}
			got := x.f.apply(t, x.run, x.req)
			if got.Outcome != ApplyOutcomeApplied || got.Codex == nil || got.Codex.AccessToken == "" || got.Token != "" {
				t.Fatalf("%s: same-request retry did not apply with material (outcome=%q reason=%q)", leaf.name, got.Outcome, got.Reason)
			}
		})
	}
}

// codexLockOrderObserver forwards all SQL and calls before ahead of each
// named account lock, so a test can interleave a writer at that exact point.
type codexLockOrderObserver struct {
	TxBeginner
	before func(kind string, id uuid.UUID)
}

type codexLockOrderTx struct {
	pgx.Tx
	observer *codexLockOrderObserver
}

func (o *codexLockOrderObserver) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := o.TxBeginner.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &codexLockOrderTx{Tx: tx, observer: o}, nil
}

func (tx *codexLockOrderTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if tx.observer.before != nil && strings.Contains(sql, "-- name: LockCodexAccountForUpdateNowait ") {
		id, _ := args[0].(uuid.UUID)
		tx.observer.before("LockCodexAccountForUpdateNowait", id)
	}
	return tx.Tx.QueryRow(ctx, sql, args...)
}

// Pins that a writer taking A's account first and then A's alias row cannot
// deadlock with apply, which holds the alias share locks and then NOWAIT-locks
// the account. Real writers in that order: the reconciler's quarantine/reauth
// restore (RefreshCodexAccountLogin, then LinkCodexCredentialState, in one
// installAndLink transaction, codexcred.go) and account deletion cascading
// ON DELETE SET NULL to the alias. (BumpCodexMaterialRevision and the healthy
// link path take the alias or user_secrets row first, the same order as apply.)
// The account step is a row lock in the test's writer tx (the real reconciler
// cannot be paused between its steps deterministically); the alias step is the
// real LinkCodexCredentialState query on A's current revision, rolled back.
// Not claimed: global lock sorting or behavior of other writers.
func TestCodexApplyReverseOrderReloginWriterLiveDB(t *testing.T) {
	x := newCodexLockOrderFixture(t)
	f := x.f
	stateA, err := f.env.q.GetCodexCredentialState(f.env.ctx, store.GetCodexCredentialStateParams{UserSecretID: x.aliasA, UserID: f.o.userID})
	if err != nil {
		t.Fatal(err)
	}
	before := x.snapshot()
	ctx, cancel := context.WithTimeout(f.env.ctx, 20*time.Second)
	defer cancel()

	var writer pgx.Tx
	writerDone := make(chan error, 1)
	fired, writerStarted, writerDrained := false, false, false
	observer := &codexLockOrderObserver{TxBeginner: f.env.pool}
	observer.before = func(_ string, id uuid.UUID) {
		if fired || id != x.accountA {
			return
		}
		fired = true
		var err error
		if writer, err = f.env.pool.Begin(ctx); err != nil {
			t.Fatal("begin writer failed")
		}
		var locked uuid.UUID
		if err := writer.QueryRow(ctx, "SELECT id FROM codex_provider_account WHERE id=$1 AND user_id=$2 FOR UPDATE", x.accountA, f.o.userID).Scan(&locked); err != nil || locked != x.accountA {
			t.Fatal("writer account step not ready")
		}
		var pid int
		if err := writer.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
			t.Fatal("writer pid unavailable")
		}
		writerStarted = true
		go func() {
			n, err := f.env.q.WithTx(writer).LinkCodexCredentialState(ctx, store.LinkCodexCredentialStateParams{
				ProviderAccountID: stateA.ProviderAccountID, UserSecretID: x.aliasA, UserID: f.o.userID, MaterialRevision: stateA.MaterialRevision,
			})
			if err == nil && n != 1 {
				err = errors.New("writer alias step matched no row")
			}
			writerDone <- err
		}()
		for {
			var blocked bool
			if err := f.env.pool.QueryRow(ctx, "SELECT cardinality(pg_blocking_pids($1)) > 0", pid).Scan(&blocked); err != nil {
				t.Fatal("writer blocked-state probe failed")
			}
			if blocked {
				return
			}
			select {
			case err := <-writerDone:
				writerDrained = true
				t.Fatalf("writer alias step finished before apply released its alias share lock: %v", err)
			case <-ctx.Done():
				t.Fatal("writer alias step never blocked on apply's alias lock")
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	f.svc.SetTxBeginner(observer)
	defer func() {
		if writer == nil {
			return
		}
		// Never touch the writer's connection while its goroutine may still use it.
		if writerStarted && !writerDrained {
			cancel()
			if err := <-writerDone; err != nil {
				t.Logf("writer alias step result on failure path: %v", err)
			}
		}
		_ = writer.Rollback(f.env.ctx)
	}()

	worker := f.worker(t)
	f.noWrite(t, x.run, func() {
		// An apply-side 40P01 is mapped to an opaque read error by
		// codexSwitchReadFailure and fails the busy assertion below.
		got, err := f.svc.ApplyCredentialSwitch(ctx, worker, x.run, x.req)
		if err == nil && got.Outcome == ApplyOutcomeApplied {
			t.Fatal("apply was APPLIED while a writer held A's account lock")
		}
		if err != nil || got.Outcome != ApplyOutcomeNotApplied || got.Reason != applyReasonCredentialBusy {
			t.Fatalf("apply did not refuse for retry with credential_busy (err=%v outcome=%q reason=%q)", err, got.Outcome, got.Reason)
		}
	})
	if !fired {
		t.Fatal("interleaving hook never ran")
	}
	select {
	case err := <-writerDone:
		writerDrained = true
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "40P01" {
			t.Fatal("DEADLOCK: the reverse-order writer was aborted with 40P01 (deadlock_detected)")
		}
		if err != nil {
			t.Fatalf("writer alias step failed after apply refused: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("writer alias step did not complete after apply released its locks")
	}
	if err := writer.Rollback(ctx); err != nil {
		t.Fatal("writer rollback failed")
	}
	if x.snapshot() != before {
		t.Fatal("rolled-back writer or refused apply left account, intent or alias state changed")
	}
	got := f.apply(t, x.run, x.req)
	if got.Outcome != ApplyOutcomeApplied || got.Codex == nil || got.Codex.AccessToken == "" || got.Token != "" {
		t.Fatalf("retry after writer rollback did not apply (outcome=%q reason=%q)", got.Outcome, got.Reason)
	}
}

// With A idle and one committed rotating intent, a held intent row makes the
// FOR SHARE NOWAIT intent lock fail; the committed intent alone is also busy.
// Redundant-fence control: either the lock or the count refuses.
func TestCodexApplyRotatingIntentRowLockLiveDB(t *testing.T) {
	x := newCodexLockOrderFixture(t)
	f := x.f
	operation := uuid.New()
	f.env.exec("INSERT INTO codex_refresh_intent(operation_id,user_id,provider_account_id,from_generation,state) VALUES($1,$2,$3,0,'rotating')", operation, f.o.userID, x.accountA)
	acct := f.env.mustAccount(t, f.o.userID, x.accountA)
	if acct.CoordState != "idle" || acct.CoordOperationID.Valid || acct.LeaseDeadline.Valid {
		t.Fatal("A must be idle with no operation or deadline")
	}
	before := x.snapshot()
	ctx, cancel := context.WithTimeout(f.env.ctx, 10*time.Second)
	defer cancel()
	blocker, err := f.env.pool.Begin(ctx)
	if err != nil {
		t.Fatal("begin blocker failed")
	}
	defer func() { _ = blocker.Rollback(f.env.ctx) }()
	var locked uuid.UUID
	if err := blocker.QueryRow(ctx, "SELECT operation_id FROM codex_refresh_intent WHERE operation_id=$1 AND user_id=$2 FOR UPDATE", operation, f.o.userID).Scan(&locked); err != nil || locked != operation {
		t.Fatal("intent blocker not ready")
	}
	x.wantBusy(t, ctx, "intent_row_lock")
	if x.snapshot() != before {
		t.Fatal("contended apply changed account, intent or alias state")
	}
	if err := blocker.Rollback(ctx); err != nil {
		t.Fatal("release intent blocker failed")
	}
	x.wantBusy(t, ctx, "intent_count")
}

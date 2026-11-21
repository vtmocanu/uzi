package workersvc

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Move A to a linked subscription through apply itself, then request B.
func codexApplyFromSubscription(t *testing.T) (applyFx, uuid.UUID, uuid.UUID, string) {
	t.Helper()
	f, run, alias := codexApplyFixture(t, true)
	applied := f.apply(t, run, f.req(f.opOf(t, run)))
	if applied.Outcome != ApplyOutcomeApplied || applied.Codex == nil {
		t.Fatal("subscription setup failed", applied)
	}
	state, err := f.env.q.GetCodexCredentialState(f.env.ctx, store.GetCodexCredentialStateParams{UserSecretID: alias, UserID: f.o.userID})
	if err != nil {
		t.Fatal(err)
	}
	next := f.env.seedStaticAPIKey(t, f.o.userID, "race-next", "race-next-key")
	if _, err := f.svc.SetRunCredential(f.env.ctx, f.o.userID, run, BindModePinned, &next); err != nil {
		t.Fatal(err)
	}
	return f, run, uuid.UUID(state.ProviderAccountID.Bytes), applied.Codex.Capability
}

// A successful blocker Scan is the readiness barrier; NOWAIT must return a
// public busy decision without changing account, intents, or run state.
func TestCodexApplyAccountRowContentionLiveDB(t *testing.T) {
	for _, side := range []string{"old", "target"} {
		t.Run(side, func(t *testing.T) {
			var f applyFx
			var run, account uuid.UUID
			if side == "old" {
				f, run, account, _ = codexApplyFromSubscription(t)
			} else {
				var alias uuid.UUID
				f, run, alias = codexApplyFixture(t, true)
				state, err := f.env.q.GetCodexCredentialState(f.env.ctx, store.GetCodexCredentialStateParams{UserSecretID: alias, UserID: f.o.userID})
				if err != nil || !state.ProviderAccountID.Valid {
					t.Fatal("target account unavailable")
				}
				account = uuid.UUID(state.ProviderAccountID.Bytes)
			}
			op := f.opOf(t, run)
			req := f.req(op)
			snapshot := func() string {
				t.Helper()
				var s string
				err := f.env.pool.QueryRow(f.env.ctx, `SELECT jsonb_build_object(
					'account',(SELECT to_jsonb(a) FROM codex_provider_account a WHERE a.id=$1 AND a.user_id=$2),
					'intents',(SELECT coalesce(jsonb_agg(to_jsonb(i) ORDER BY i.operation_id),'[]'::jsonb)
						FROM codex_refresh_intent i WHERE i.provider_account_id=$1 AND i.user_id=$2))::text`, account, f.o.userID).Scan(&s)
				if err != nil {
					t.Fatal("account snapshot failed")
				}
				return s
			}
			before := snapshot()
			ctx, cancel := context.WithTimeout(f.env.ctx, 10*time.Second)
			defer cancel()
			blocker, err := f.env.pool.Begin(ctx)
			if err != nil {
				t.Fatal("begin blocker failed")
			}
			defer func() { _ = blocker.Rollback(f.env.ctx) }()
			var locked uuid.UUID
			if err := blocker.QueryRow(ctx, "SELECT id FROM codex_provider_account WHERE id=$1 AND user_id=$2 FOR UPDATE", account, f.o.userID).Scan(&locked); err != nil || locked != account {
				t.Fatal("account blocker not ready")
			}
			worker := f.worker(t)
			f.noWrite(t, run, func() {
				got, err := f.svc.ApplyCredentialSwitch(ctx, worker, run, req)
				if err != nil || got.Outcome != ApplyOutcomeNotApplied || got.Reason != applyReasonCredentialBusy || got.Codex != nil || got.Token != "" {
					t.Fatal("contended account did not return an empty busy decision")
				}
			})
			if snapshot() != before {
				t.Fatal("contended apply changed account or intents")
			}
			if err := blocker.Rollback(ctx); err != nil {
				t.Fatal("release account blocker failed")
			}
			got := f.apply(t, run, req)
			if got.Outcome != ApplyOutcomeApplied || got.Codex == nil || got.Codex.AccessToken == "" || got.Codex.Capability == "" || got.Token != "" {
				t.Fatal("same-request retry did not apply with auth")
			}
			var journals, messages int
			err = f.env.pool.QueryRow(f.env.ctx, `SELECT
				(SELECT count(*) FROM run_credential_switch_epochs WHERE run_id=$1 AND op_id=$2),
				(SELECT count(*) FROM run_messages m JOIN run_credential_switch_epochs e
					ON e.run_id=m.run_id AND e.message_seq=m.seq
					WHERE e.run_id=$1 AND e.op_id=$2 AND m.kind='credential_switch')`, run, op).Scan(&journals, &messages)
			if err != nil || journals != 1 || messages != 1 {
				t.Fatal("retry did not produce exactly one operation journal and message")
			}
		})
	}
}

// The observer forwards all SQL to real transactions. Only successful lock
// Scans count; intent events are recorded before the delegated Query.
type codexApplyLockEvent struct {
	kind string
	id   uuid.UUID
}

type codexApplyLockObserver struct {
	TxBeginner
	events []codexApplyLockEvent
}

type codexApplyObservedTx struct {
	pgx.Tx
	observer *codexApplyLockObserver
}

type codexApplyObservedRow struct {
	pgx.Row
	observer *codexApplyLockObserver
	event    codexApplyLockEvent
}

func (o *codexApplyLockObserver) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := o.TxBeginner.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &codexApplyObservedTx{Tx: tx, observer: o}, nil
}

func (tx *codexApplyObservedTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	row := tx.Tx.QueryRow(ctx, sql, args...)
	for _, kind := range []string{"LockSecretEnablementForShareNowait", "LockCodexAliasForShareNowait", "LockCodexAccountForUpdateNowait"} {
		if strings.Contains(sql, "-- name: "+kind+" ") {
			id, _ := args[0].(uuid.UUID)
			return &codexApplyObservedRow{Row: row, observer: tx.observer, event: codexApplyLockEvent{kind: kind, id: id}}
		}
	}
	return row
}

func (r *codexApplyObservedRow) Scan(dest ...any) error {
	if err := r.Row.Scan(dest...); err != nil {
		return err
	}
	r.observer.events = append(r.observer.events, r.event)
	return nil
}

func (tx *codexApplyObservedTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if strings.Contains(sql, "-- name: LockUnresolvedCodexSwitchIntents ") {
		id, _ := args[0].(uuid.UUID)
		tx.observer.events = append(tx.observer.events, codexApplyLockEvent{kind: "LockUnresolvedCodexSwitchIntents", id: id})
	}
	return tx.Tx.Query(ctx, sql, args...)
}

// This pins old-first traversal and alias-before-account admission in both
// directions. It makes no claim of global sorting or deadlock freedom.
func TestCodexApplyAccountLockOrderLiveDB(t *testing.T) {
	f, run, accountA, _ := codexApplyFromSubscription(t)
	aRun := mustRun(t, f.env, run)
	aliasA := uuid.UUID(aRun.CodexSecretID.Bytes)
	accessTokenB := codexToken("lock-B-access")
	aliasB := f.env.seedLinkedSubscription(t, f.o.userID, "lock-order-B", accessTokenB, codexToken("lock-B-refresh"))
	stateB, err := f.env.q.GetCodexCredentialState(f.env.ctx, store.GetCodexCredentialStateParams{UserSecretID: aliasB, UserID: f.o.userID})
	if err != nil || !stateB.ProviderAccountID.Valid {
		t.Fatal("B account unavailable")
	}
	accountB := uuid.UUID(stateB.ProviderAccountID.Bytes)
	if accountA == accountB {
		t.Fatal("lock order requires distinct accounts")
	}
	if _, err := f.svc.SetRunCredential(f.env.ctx, f.o.userID, run, BindModePinned, &aliasB); err != nil {
		t.Fatal("request B failed")
	}
	pending := mustRun(t, f.env, run)
	if !aRun.CodexAccountKey.Valid || !pending.CodexSwitchTargetAccountKey.Valid || aRun.CodexAccountKey.String == pending.CodexSwitchTargetAccountKey.String {
		t.Fatal("lock order requires distinct account keys")
	}
	observer := &codexApplyLockObserver{TxBeginner: f.env.pool}
	f.svc.SetTxBeginner(observer)
	check := func(oldAlias, targetAlias, oldAccount, targetAccount uuid.UUID, expected string) {
		t.Helper()
		got := f.apply(t, run, f.req(f.opOf(t, run)))
		if got.Outcome != ApplyOutcomeApplied || got.Codex == nil || got.Codex.AccessToken != expected || got.Codex.Capability == "" {
			t.Fatal("ordered apply returned wrong target material")
		}
		var accounts []uuid.UUID
		seen := make(map[codexApplyLockEvent]int)
		intents := 0
		for _, event := range observer.events {
			switch event.kind {
			case "LockSecretEnablementForShareNowait", "LockCodexAliasForShareNowait":
				if len(accounts) != 0 {
					t.Fatal("alias lock completed after an account lock")
				}
				seen[event]++
			case "LockCodexAccountForUpdateNowait":
				for _, alias := range []uuid.UUID{oldAlias, targetAlias} {
					for _, kind := range []string{"LockSecretEnablementForShareNowait", "LockCodexAliasForShareNowait"} {
						if seen[codexApplyLockEvent{kind: kind, id: alias}] != 1 {
							t.Fatal("account lock preceded both alias admission scans")
						}
					}
				}
				accounts = append(accounts, event.id)
			case "LockUnresolvedCodexSwitchIntents":
				intents++
				if len(accounts) != 2 {
					t.Fatal("intent query preceded both account locks")
				}
			}
		}
		if len(accounts) != 2 || accounts[0] != oldAccount || accounts[1] != targetAccount || intents != 2 {
			t.Fatal("account locks did not follow exact old-to-target order")
		}
	}
	check(aliasA, aliasB, accountA, accountB, accessTokenB)
	if _, err := f.svc.SetRunCredential(f.env.ctx, f.o.userID, run, BindModePinned, &aliasA); err != nil {
		t.Fatal("request A failed")
	}
	observer.events = nil
	check(aliasB, aliasA, accountB, accountA, codexApplyTargetAccess)
}

func TestCodexApplyBusyOldAccountLiveDB(t *testing.T) {
	for _, provenance := range []string{"other run", "poller", "unknown", "intent only"} {
		t.Run(provenance, func(t *testing.T) {
			f, run, account, _ := codexApplyFromSubscription(t)
			operation := uuid.New()
			if provenance != "intent only" {
				f.env.exec("UPDATE codex_provider_account SET coord_state='in_progress',coord_operation_id=$2,lease_deadline=now()+interval '1 minute' WHERE id=$1", account, operation)
			}
			if provenance == "other run" || provenance == "intent only" {
				f.env.exec("INSERT INTO codex_refresh_intent(operation_id,user_id,provider_account_id,from_generation,state) VALUES($1,$2,$3,0,'rotating')", operation, f.o.userID, account)
			}
			before := f.env.mustAccount(t, f.o.userID, account)
			f.noWrite(t, run, func() {
				got := f.apply(t, run, f.req(f.opOf(t, run)))
				if got.Outcome != ApplyOutcomeNotApplied || got.Reason != applyReasonCredentialBusy {
					t.Fatalf("busy A accepted: %v", got)
				}
			})
			after := f.env.mustAccount(t, f.o.userID, account)
			if after.CoordState != before.CoordState || after.CoordOperationID != before.CoordOperationID || after.LeaseDeadline != before.LeaseDeadline {
				t.Fatal("apply cleared account-wide busy state")
			}
		})
	}
}

// The initial refresh read has passed. Before locked admission, a real apply
// changes authority. The locked refresh recheck must leave no intent or lease.
func TestCodexApplyWinsLateRefreshAdmissionLiveDB(t *testing.T) {
	f, run, account, capability := codexApplyFromSubscription(t)
	fake := &fakeRefreshClient{}
	f.svc.codexRefresh = fake
	changed := false
	f.svc.q = intentReadHookStore{
		Queries: f.env.q,
		onIntentRead: func(_ store.CodexRefreshIntent, readErr error) {
			if changed || !errors.Is(readErr, pgx.ErrNoRows) {
				return
			}
			changed = true
			result := f.apply(t, run, f.req(f.opOf(t, run)))
			if result.Outcome != ApplyOutcomeApplied {
				t.Fatal("switch did not win", result)
			}
		},
	}
	op := uuid.New()
	res, err := f.svc.CoordinatedCodexRefresh(f.env.ctx, f.worker(t), run, capability, op, 0)
	if !changed || err == nil || res.AccessToken != "" || fake.calls != 0 {
		t.Fatalf("late refresh escaped admission: changed=%t calls=%d err=%v", changed, fake.calls, err)
	}
	if _, err := f.env.q.GetCodexRefreshIntent(f.env.ctx, store.GetCodexRefreshIntentParams{OperationID: op, UserID: f.o.userID}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatal("late refresh left intent", err)
	}
	acct := f.env.mustAccount(t, f.o.userID, account)
	if acct.CoordState != "idle" || acct.CoordOperationID.Valid || acct.LeaseDeadline.Valid {
		t.Fatal("late refresh acquired lease")
	}
}

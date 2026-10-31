package workersvc

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// These tests belong to the full seven-package LiveDB$ lane. No-DSN runs
// compile them but do not prove their database behavior.
func TestScopedClaimAccountingLiveDB(t *testing.T) {
	for _, name := range []string{"history", "known owner lease plus ordinary", "three grouped plus ordinary", "three grouped plus two ordinary", "three ordinary", "ordinary wrong generation", "ordinary foreign", "ordinary released", "ordinary terminal", "group unknown", "group foreign", "group released", "group queued", "group terminal", "group wrong generation", "duplicate coverage", "unavailable coverage", "wire ceiling", "zero capacity", "malformed group", "equal epoch", "stale nonce", "capability shrink"} {
		t.Run(name, func(t *testing.T) {
			e := setupCodexLiveDB(t)
			user, unusedWorker, repo := e.seedCodexInfra(t)
			e.exec("UPDATE workers SET status='offline' WHERE id=$1", unusedWorker)
			enableClaimAssembly(t, e, user)
			p := testParams()
			p.WorkerOutboxMaxPending = 2
			if name == "wire ceiling" {
				p.ActiveSnapshotMaxEntries = 1
			}
			// New intentionally preserves a configured zero outbox cap.
			if name == "zero capacity" {
				p.WorkerOutboxMaxPending = 0
			}
			svc := snapshotSvc(e, p)
			wk := seedSnapshotWorker(t, e, user, "scoped")
			e.exec("UPDATE workers SET protocol_capabilities=$2,max_concurrent_runs=8 WHERE id=$1", wk, []string{"pending_outcome_scoped_claim_v1"})
			source := seedOutageRun(t, e, user, repo, wk, "running", "issue", 9, 0)
			ordinary := seedOutageRun(t, e, user, repo, wk, "running", "issue", 2, 0)
			candidate := seedUnassignedRun(t, e, user, repo, 0)
			pending := "[" + scopedTuple(source, 7) + "]"
			group := scopedGroup(source, 9)
			allow := name == "history" || name == "three grouped plus ordinary" || name == "known owner lease plus ordinary"
			invalid := name == "equal epoch" || name == "stale nonce"
			switch name {
			case "known owner lease plus ordinary", "three grouped plus ordinary", "three grouped plus two ordinary":
				pending = "[" + scopedTuple(source, 1) + "," + scopedTuple(source, 2) + "," + scopedTuple(source, 3) + "," + scopedTuple(ordinary, 2)
				if name == "three grouped plus two ordinary" {
					pending += "," + scopedTuple(ordinary, 1)
				}
				pending += "]"
			case "three ordinary":
				pending = "[" + scopedTuple(ordinary, 0) + "," + scopedTuple(ordinary, 1) + "," + scopedTuple(ordinary, 2) + "]"
			case "ordinary wrong generation", "ordinary foreign", "ordinary released", "ordinary terminal":
				pending = "[" + scopedTuple(source, 7) + "," + scopedTuple(ordinary, 2) + "]"
				switch name {
				case "ordinary wrong generation":
					e.exec("UPDATE runs SET claim_generation=3 WHERE id=$1", ordinary)
				case "ordinary foreign":
					foreign := seedSnapshotWorker(t, e, user, "foreign")
					e.exec("UPDATE runs SET worker_id=$2 WHERE id=$1", ordinary, foreign)
				case "ordinary released":
					e.exec("UPDATE runs SET claim_released_at=now() WHERE id=$1", ordinary)
				case "ordinary terminal":
					e.exec("UPDATE runs SET status='completed' WHERE id=$1", ordinary)
				}
			case "group unknown":
				group = scopedGroup(uuid.New(), 9)
			case "group foreign":
				foreign := seedSnapshotWorker(t, e, user, "foreign")
				e.exec("UPDATE runs SET worker_id=$2 WHERE id=$1", source, foreign)
			case "group released":
				e.exec("UPDATE runs SET claim_released_at=now() WHERE id=$1", source)
			case "group queued":
				e.exec("UPDATE runs SET status='queued' WHERE id=$1", source)
			case "group terminal":
				e.exec("UPDATE runs SET status='completed' WHERE id=$1", source)
			case "group wrong generation":
				group = scopedGroup(source, 8)
			case "duplicate coverage":
				pending = "[" + scopedTuple(source, 7) + "," + scopedTuple(source, 7) + "]"
			case "unavailable coverage":
				pending = "null"
			case "wire ceiling":
				pending = "[" + scopedTuple(source, 7) + "," + scopedTuple(source, 8) + "]"
			case "malformed group":
				group = `[{"run_id":7,"owner_generation":9}]`
				pending = "[" + scopedTuple(source, 9) + "]"
			}
			input := wkrRow(t, e, wk)
			if name == "capability shrink" {
				// Actual re-registration filters/replaces capabilities and rotates the nonce.
				_, nonce, err := svc.Register(e.ctx, input, "test", "base", nil, nil, []string{}, nil)
				if err != nil {
					t.Fatal(err)
				}
				e.exec("UPDATE runs SET status='running' WHERE id=$1", source)
				// Use stale caller capability evidence but the fresh registration nonce.
				input.SnapshotRegisterNonce.String = nonce
			}
			snap := scopedRawSnapshot(t, source, pending, group)
			snap.RegisterNonce = input.SnapshotRegisterNonce.String
			if name == "known owner lease plus ordinary" {
				snap.Active = []ActiveRunEntry{entry(source, 9, "running", true)}
			}
			if name == "equal epoch" {
				e.exec("UPDATE workers SET snapshot_epoch=1 WHERE id=$1", wk)
			}
			if name == "stale nonce" {
				snap.RegisterNonce = "old"
			}
			payload, err := svc.Claim(e.ctx, input, snap)
			if invalid {
				if !errors.Is(err, ErrActiveSnapshotInvalid) {
					t.Fatalf("stale Claim err=%v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if (payload != nil) != allow {
				t.Fatalf("payload present=%v want %v", payload != nil, allow)
			}
			if allow && payload.RunID != candidate.String() {
				t.Fatalf("claimed %s want unrelated %s", payload.RunID, candidate)
			}
			if got := wkrRow(t, e, wk); !got.PendingOverflowUntil.Valid || !got.PendingOverflowUntil.Time.After(svc.now()) {
				t.Fatal("snapshot overflow closure did not persist")
			}
			if name == "malformed group" && pendingIdentityCount(t, e, wk) != 1 {
				t.Fatal("malformed group discarded valid pending coverage")
			}
			if name == "history" && pendingIdentityCount(t, e, wk) != 0 {
				t.Fatal("historical source unexpectedly acquired current-generation identity authority")
			}
		})
	}
}

// An event hook fires after the actual target-lock result has been consumed,
// before the fresh row read. It changes only this transaction's row, exercising
// status/release fields absent from the frozen ledger without real-time waits.
type scopedTargetBeginner struct {
	TxBeginner
	source   uuid.UUID
	mutation string
}

func (b scopedTargetBeginner) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := b.TxBeginner.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &scopedTargetTx{Tx: tx, source: b.source, mutation: b.mutation}, nil
}

type scopedTargetTx struct {
	pgx.Tx
	source   uuid.UUID
	mutation string
}

func (tx *scopedTargetTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	rows, err := tx.Tx.Query(ctx, sql, args...)
	if err != nil || !strings.Contains(sql, "name: LockFrozenWorkerSnapshotRuns ") {
		return rows, err
	}
	return &scopedTargetRows{Rows: rows, after: func() error { _, err := tx.Exec(ctx, tx.mutation, tx.source); return err }}, nil
}

type scopedTargetRows struct {
	pgx.Rows
	after   func() error
	hookErr error
	closed  bool
}

func (r *scopedTargetRows) Close() {
	r.Rows.Close()
	if !r.closed {
		r.closed = true
		r.hookErr = r.after()
	}
}
func (r *scopedTargetRows) Next() bool {
	next := r.Rows.Next()
	if !next {
		r.Close()
	}
	return next
}
func (r *scopedTargetRows) Err() error {
	if r.hookErr != nil {
		return r.hookErr
	}
	return r.Rows.Err()
}

func TestScopedClaimPostTargetLockFencesLiveDB(t *testing.T) {
	for _, mutation := range []string{
		"UPDATE runs SET status='queued' WHERE id=$1",
		"UPDATE runs SET claim_released_at=now() WHERE id=$1",
		"UPDATE runs SET claim_generation=10 WHERE id=$1",
		"UPDATE runs SET worker_id=NULL WHERE id=$1",
	} {
		t.Run(mutation, func(t *testing.T) {
			e := setupCodexLiveDB(t)
			user, _, repo := e.seedCodexInfra(t)
			enableClaimAssembly(t, e, user)
			svc := snapshotSvc(e, testParams())
			wk := seedSnapshotWorker(t, e, user, "scoped")
			e.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", wk, []string{"pending_outcome_scoped_claim_v1"})
			source := seedOutageRun(t, e, user, repo, wk, "running", "issue", 9, 0)
			candidate := seedUnassignedRun(t, e, user, repo, 0)
			svc.SetTxBeginner(scopedTargetBeginner{TxBeginner: e.pool, source: source, mutation: mutation})
			snap := scopedRawSnapshot(t, source, "["+scopedTuple(source, 7)+"]", scopedGroup(source, 9))
			payload, err := svc.Claim(e.ctx, wkrRow(t, e, wk), snap)
			if err != nil || payload != nil || statusOf(t, e, candidate) != "queued" {
				t.Fatalf("post-lock drift claimed candidate: payload=%v err=%v", payload != nil, err)
			}
		})
	}
}

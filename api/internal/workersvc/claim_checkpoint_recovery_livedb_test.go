package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/pushbroker"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestClaimCheckpointRecoveryEncoding(t *testing.T) {
	for _, value := range []*bool{nil, new(bool), func() *bool { v := true; return &v }()} {
		data, err := json.Marshal(ClaimPayload{CheckpointWriteUnresolved: value})
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil {
			t.Fatal(err)
		}
		got, present := fields["checkpoint_write_unresolved"]
		if value == nil {
			if present {
				t.Fatal("legacy claim fabricated recovery evidence")
			}
		} else {
			want := "false"
			if *value {
				want = "true"
			}
			if string(got) != want {
				t.Fatalf("encoded evidence=%s want=%s", got, want)
			}
		}
	}
}

// Claim's real assembly seam lets confirmation commit before the final handoff.
func TestClaimCheckpointRecoveryProjectionLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	for _, mode := range []string{"none", "nonready", "ready", "matching tip", "confirmed", "null anchors"} {
		t.Run(mode, func(t *testing.T) {
			f := newCustodyContinuationFixture(t, env)
			id := seedCoordinateTask(f)
			env.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", f.workerID,
				[]string{capability.TaskCheckpointCoordinatesV1, capability.RecoveryArchiveV1})
			hookRan := false
			f.svc.claimHooks = &claimTestHooks{afterAssembly: func(ctx context.Context, run store.Run, payload *ClaimPayload, err error) {
				if err != nil || payload == nil || run.ID != id {
					t.Fatalf("assembly err=%v", err)
				}
				hookRan = true
				if mode == "null anchors" {
					// Poison only the assembly snapshot: projection must overwrite BOTH with NULL.
					old := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
					payload.CheckpointTip, payload.CheckpointAttestedSource = &old, &old
					return
				}
				if mode == "none" {
					return
				}
				pair, err := frozenCheckpointCoordinates(run)
				if err != nil {
					t.Fatal(err)
				}
				attemptID, err := env.q.RecordCheckpointPublishAttempt(ctx, store.RecordCheckpointPublishAttemptParams{
					RunID: id, Branch: pair.CheckpointKey, Ref: checkpointRefPrefix + pair.CheckpointKey,
					Tip:            "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
					AttestedSource: pgconv.Text("cccccccccccccccccccccccccccccccccccccccc"),
				})
				if err != nil {
					t.Fatal(err)
				}
				// Neither due scheduling, nor matching the current tip, may hide an attempt.
				env.exec("UPDATE checkpoint_publish_attempts SET next_check_at=now()+interval '1 day' WHERE id=$1", attemptID)
				if mode == "matching tip" {
					env.exec("UPDATE runs SET checkpoint_tip=$2 WHERE id=$1", id, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
					return
				}
				if mode == "nonready" {
					return
				}
				if n, err := env.q.MarkCheckpointPublishAttemptReady(ctx, attemptID); err != nil || n != 1 {
					t.Fatalf("ready n=%d err=%v", n, err)
				}
				if mode == "ready" {
					return
				}
				attempt, err := env.q.GetCheckpointPublishAttempt(ctx, attemptID)
				if err != nil {
					t.Fatal(err)
				}
				observed := mustRun(t, env, id)
				ok, err := f.svc.confirmLivePublishAttempt(ctx, attempt, observed, store.CheckpointRetention{}, false, func(context.Context) error { return nil })
				if err != nil || !ok {
					t.Fatalf("real confirmation ok=%v err=%v", ok, err)
				}
				if _, err := env.q.GetCheckpointPublishAttempt(ctx, attemptID); !errors.Is(err, pgx.ErrNoRows) {
					t.Fatalf("confirmed attempt not consumed: %v", err)
				}
			}}
			payload, err := f.svc.Claim(env.ctx, f.wkr, nil)
			if err != nil || payload == nil || !hookRan || payload.CheckpointCoordinates == nil || payload.CheckpointWriteUnresolved == nil {
				t.Fatalf("claim projection missing: payload=%v hook=%v err=%v", payload != nil, hookRan, err)
			}
			want := mode == "nonready" || mode == "ready" || mode == "matching tip"
			if *payload.CheckpointWriteUnresolved != want {
				t.Fatalf("unresolved=%v want=%v", *payload.CheckpointWriteUnresolved, want)
			}
			if mode == "confirmed" {
				if payload.CheckpointTip == nil || *payload.CheckpointTip != "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" ||
					payload.CheckpointAttestedSource == nil || *payload.CheckpointAttestedSource != "cccccccccccccccccccccccccccccccccccccccc" {
					t.Fatal("handoff retained pre-confirmation anchors")
				}
			} else if mode == "matching tip" {
				if payload.CheckpointTip == nil || *payload.CheckpointTip != "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" || payload.CheckpointAttestedSource != nil {
					t.Fatal("matching-tip unresolved evidence or nullable attestation was not refreshed")
				}
			} else if payload.CheckpointTip != nil || payload.CheckpointAttestedSource != nil {
				t.Fatal("NULL anchors were not refreshed")
			}
		})
	}
}

// A real pgx transaction remains behind the concrete enrolled-claim guard.
// Inject faults only at the projection/commit seam, leaving locks and rollback real.
type claimProjectionFaultBeginner struct {
	TxBeginner
	runID      uuid.UUID
	mode       string
	reached    bool
	rolledBack bool
	isolation  string
	failure    error
}

func (b *claimProjectionFaultBeginner) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := b.TxBeginner.Begin(ctx)
	if err != nil {
		return nil, err
	}
	// The production helper must override a stricter connection default.
	if _, err := tx.Exec(ctx, "SET TRANSACTION ISOLATION LEVEL REPEATABLE READ"); err != nil {
		_ = tx.Rollback(ctx)
		return nil, err
	}
	return &claimProjectionFaultTx{Tx: tx, b: b}, nil
}

type claimProjectionFaultTx struct {
	pgx.Tx
	b *claimProjectionFaultBeginner
}
type claimProjectionErrorRow struct{ err error }

func (r claimProjectionErrorRow) Scan(...any) error { return r.err }
func (tx *claimProjectionFaultTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if strings.Contains(sql, "-- name: GetClaimCheckpointRecovery :one") {
		tx.b.reached = true
		var isolation string
		if err := tx.Tx.QueryRow(ctx, "SHOW transaction_isolation").Scan(&isolation); err != nil {
			return claimProjectionErrorRow{err}
		}
		tx.b.isolation = isolation
		if isolation != "read committed" {
			return claimProjectionErrorRow{errors.New("claim finish did not force READ COMMITTED")}
		}
		if _, err := tx.Exec(ctx, "UPDATE runs SET issue_title='projection rollback marker' WHERE id=$1", tx.b.runID); err != nil {
			return claimProjectionErrorRow{err}
		}
		if tx.b.mode == "missing" {
			return claimProjectionErrorRow{tx.b.failure}
		}
		if tx.b.mode == "read" {
			return claimProjectionErrorRow{tx.b.failure}
		}
	}
	return tx.Tx.QueryRow(ctx, sql, args...)
}
func (tx *claimProjectionFaultTx) Commit(ctx context.Context) error {
	if tx.b.reached && tx.b.mode == "commit" {
		return tx.b.failure
	}
	return tx.Tx.Commit(ctx)
}
func (tx *claimProjectionFaultTx) Rollback(ctx context.Context) error {
	tx.b.rolledBack = true
	return tx.Tx.Rollback(ctx)
}

func TestClaimCheckpointRecoveryFailureLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	for _, mode := range []string{"missing", "read", "commit"} {
		t.Run(mode, func(t *testing.T) {
			f := newCustodyContinuationFixture(t, env)
			id := seedCoordinateTask(f)
			env.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", f.workerID,
				[]string{capability.TaskCheckpointCoordinatesV1, capability.RecoveryArchiveV1})
			failure := errors.New("projection " + mode + " fault")
			if mode == "missing" {
				failure = pgx.ErrNoRows
			}
			fault := &claimProjectionFaultBeginner{TxBeginner: env.pool, runID: id, mode: mode, failure: failure}
			f.svc.claimHooks = &claimTestHooks{afterAssembly: func(_ context.Context, _ store.Run, payload *ClaimPayload, err error) {
				if err != nil || payload == nil {
					t.Fatalf("assembly: %v", err)
				}
				f.svc.txBeginner = fault
			}}
			payload, err := f.svc.Claim(env.ctx, f.wkr, nil)
			if !errors.Is(err, failure) || payload != nil || !fault.reached || !fault.rolledBack || fault.isolation != "read committed" {
				t.Fatalf("failure delivered payload=%v err=%v reached=%v rollback=%v", payload != nil, err, fault.reached, fault.rolledBack)
			}
			persisted := mustRun(t, env, id)
			if persisted.IssueTitle != "task" || persisted.Status != "claimed" || f.ownOpenHolds(t, id) != 1 {
				t.Fatal("projection failure escaped rollback or changed durable claim/custody")
			}
		})
	}
}

// Exercise both serial orders at the production admission boundary. Attempts are
// generation fenced before IO; an older admitted attempt survives the next claim.
func TestClaimCheckpointAttemptReclaimOrderingLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	for _, attemptFirst := range []bool{true, false} {
		name := "reclaim first"
		if attemptFirst {
			name = "attempt first"
		}
		t.Run(name, func(t *testing.T) {
			f := newCustodyContinuationFixture(t, env)
			id := seedCoordinateTask(f)
			env.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", f.workerID,
				[]string{capability.TaskCheckpointCoordinatesV1, capability.RecoveryArchiveV1})
			first, err := f.svc.Claim(env.ctx, f.wkr, nil)
			if err != nil || first == nil || first.CheckpointCoordinates == nil {
				t.Fatalf("first claim: %v", err)
			}
			params := store.RecordLiveCheckpointPublishAttemptParams{
				RecordCheckpointPublishAttemptParams: store.RecordCheckpointPublishAttemptParams{
					RunID: id, Branch: first.CheckpointCoordinates.CheckpointKey,
					Ref: checkpointRefPrefix + first.CheckpointCoordinates.CheckpointKey, Tip: testCheckpointTip,
				},
				ExpectedWorkerID: pgconv.UUID(f.workerID), ExpectedClaimGeneration: first.ClaimGeneration,
			}
			if attemptFirst {
				if _, err := env.q.RecordLiveCheckpointPublishAttempt(env.ctx, params); err != nil {
					t.Fatal(err)
				}
			}
			// This is the existing nonterminal requeue/reclaim lane, not a terminal reset.
			env.exec("UPDATE runs SET status='queued',worker_id=NULL WHERE id=$1", id)
			second, err := f.svc.Claim(env.ctx, f.wkr, nil)
			if err != nil || second == nil || second.ClaimGeneration != first.ClaimGeneration+1 || second.CheckpointWriteUnresolved == nil {
				t.Fatalf("reclaim missing exact generation/evidence: %v", err)
			}
			if *second.CheckpointWriteUnresolved != attemptFirst {
				t.Fatalf("reclaim evidence=%v attemptFirst=%v", *second.CheckpointWriteUnresolved, attemptFirst)
			}
			if !attemptFirst {
				if got, err := env.q.RecordLiveCheckpointPublishAttempt(env.ctx, params); !errors.Is(err, pgx.ErrNoRows) || got != uuid.Nil {
					t.Fatalf("old generation admitted after reclaim: id=%s err=%v", got, err)
				}
				var attempts int
				if err := env.pool.QueryRow(env.ctx, "SELECT count(*) FROM checkpoint_publish_attempts WHERE run_id=$1", id).Scan(&attempts); err != nil || attempts != 0 {
					t.Fatalf("stale admission attempts=%d err=%v", attempts, err)
				}
			}
		})
	}
}

func TestPublishTaskCheckpointGenerationLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	f := newCustodyContinuationFixture(t, env)
	id := seedCoordinateTask(f)
	env.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", f.workerID,
		[]string{capability.TaskCheckpointCoordinatesV1, capability.RecoveryArchiveV1})
	payload, err := f.svc.Claim(env.ctx, f.wkr, nil)
	if err != nil || payload == nil {
		t.Fatalf("claim: %v", err)
	}
	calls := 0
	f.svc.SetPublishFn(func(context.Context, pushbroker.Options) (pushbroker.Result, error) {
		calls++
		return pushbroker.Result{}, nil
	})
	stale := payload.ClaimGeneration + 1
	for _, tc := range []struct {
		gen  *int64
		want error
	}{{nil, ErrMissingClaimGeneration}, {&stale, ErrStaleClaim}} {
		_, err := f.svc.Publish(env.ctx, f.wkr, id, testCheckpointTip, []byte("pack"), tc.gen)
		if !errors.Is(err, tc.want) || calls != 0 {
			t.Fatalf("generation refusal: err=%v calls=%d", err, calls)
		}
		var attempts int
		if err := env.pool.QueryRow(env.ctx, "SELECT count(*) FROM checkpoint_publish_attempts WHERE run_id=$1", id).Scan(&attempts); err != nil || attempts != 0 {
			t.Fatalf("attempts=%d err=%v", attempts, err)
		}
	}
}

package workersvc

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestPolicyStopSettledReceiptsLiveDB(t *testing.T) {
	e := setupUsageTail(t)
	e.wkr.ProtocolCapabilities = []string{capability.InputReceiptsV1, capability.InputInclusionV1}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := e.pool.Exec(e.ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec("UPDATE runs SET status='running' WHERE id=$1", e.runID)
	var scope, settled, ordinary, follow int64
	insert := func(kind, body string, dest *int64) {
		t.Helper()
		if err := e.pool.QueryRow(e.ctx, "INSERT INTO run_user_inputs(run_id,kind,body) VALUES($1,$2,$3) RETURNING id", e.runID, kind, body).Scan(dest); err != nil {
			t.Fatal(err)
		}
	}
	insert("scope", "ceiling", &scope)
	insert("revise_plan", "preserve exact refused revision", &settled)
	// Model an old ACK whose response or executor delivery is still in flight.
	if _, err := e.svc.AckInputs(e.ctx, e.wkr, e.runID, 1, []int64{settled}); err != nil {
		t.Fatal(err)
	}
	exec("UPDATE run_user_inputs SET disposition='superseded_by_stop',settled_at=now(),superseded_by_stop_input_id=$2 WHERE id=$1", settled, scope)
	// More than one replay window of unreceived settlements must not starve later input.
	exec("INSERT INTO run_user_inputs(run_id,kind,body,disposition,settled_at,superseded_by_stop_input_id) SELECT $1,'revise_plan','settled older feedback','superseded_by_stop',now(),$2 FROM generate_series(1,1001)", e.runID, scope)
	insert("revise_plan", "ordinary revision remains actionable", &ordinary)
	insert("follow_up", "normal sibling", &follow)
	for _, mode := range []string{"ack", "applied", "included"} {
		t.Run(mode+"_mixed_batch", func(t *testing.T) {
			var out InputReceiptResult
			var err error
			ids := []int64{ordinary, follow, settled}
			switch mode {
			case "ack":
				out, err = e.svc.AckInputs(e.ctx, e.wkr, e.runID, 1, ids)
			case "applied":
				out, err = e.svc.ApplyInputs(e.ctx, e.wkr, e.runID, 1, ids)
			case "included":
				out, err = e.svc.IncludeInputs(e.ctx, e.wkr, e.runID, 1, ids)
			}
			var conflict *InputReceiptConflictError
			if !errors.As(err, &conflict) || conflict.Reason != "superseded_by_stop" || len(out.Inputs) != 0 {
				t.Fatalf("settled input must return typed conflict without actionable body: out=%+v err=%v", out, err)
			}
			var changed int
			if err := e.pool.QueryRow(e.ctx, "SELECT count(*) FROM run_user_inputs WHERE id=ANY($1) AND (consumed_at IS NOT NULL OR applied_at IS NOT NULL OR included_at IS NOT NULL)", []int64{ordinary, follow}).Scan(&changed); err != nil || changed != 0 {
				t.Fatalf("mixed receipt mutated siblings: count=%d err=%v", changed, err)
			}
		})
	}
	replay, err := e.q.ListReplayRunInputs(e.ctx, e.runID)
	if err != nil || len(replay) != 2 || replay[0].ID != ordinary || replay[1].ID != follow {
		t.Fatalf("settlements filled replay window: count=%d err=%v", len(replay), err)
	}
	// SQL is independently guarded even if called without the service preflight.
	stamped, err := e.q.AckRunInputRows(e.ctx, store.AckRunInputRowsParams{
		RunID: e.runID, Ids: []int64{ordinary, settled}, ClaimGeneration: pgtype.Int8{Int64: 1, Valid: true}, WorkerID: pgconv.UUID(e.wkr.ID),
	})
	if err != nil || len(stamped) != 0 {
		t.Fatalf("SQL ACK revived a mixed batch: rows=%v err=%v", stamped, err)
	}
	if _, err := e.svc.AckInputs(e.ctx, e.wkr, e.runID, 1, []int64{ordinary, follow}); err != nil {
		t.Fatal(err)
	}
	changed, err := e.q.ApplyRunInputRows(e.ctx, store.ApplyRunInputRowsParams{
		RunID: e.runID, Ids: []int64{ordinary, settled}, ClaimGeneration: pgtype.Int8{Int64: 1, Valid: true}, WorkerID: pgconv.UUID(e.wkr.ID),
	})
	if err != nil || changed != 0 {
		t.Fatalf("SQL APPLY revived a mixed batch: changed=%d err=%v", changed, err)
	}
	// A poll started by an older, consume-on-read worker never returns settled bodies.
	consumed, err := e.q.ConsumeRunInputs(e.ctx, e.runID)
	if err != nil || len(consumed) != 2 || consumed[0].ID != ordinary || consumed[1].ID != follow {
		t.Fatalf("legacy poll returned settled feedback: count=%d err=%v", len(consumed), err)
	}
	var honest int
	if err := e.pool.QueryRow(e.ctx, "SELECT count(*) FROM run_user_inputs WHERE run_id=$1 AND settled_at IS NOT NULL AND applied_at IS NULL AND included_at IS NULL", e.runID).Scan(&honest); err != nil || honest != 1002 {
		t.Fatalf("settlement became an application receipt: count=%d err=%v", honest, err)
	}
	// Ordinary applied retries keep their established lost-response behavior.
	exec("UPDATE runs SET claim_released_at=now() WHERE id=$1", e.runID)
	if _, err := e.svc.ApplyInputs(e.ctx, e.wkr, e.runID, 1, []int64{ordinary}); err != nil {
		t.Fatalf("ordinary applied retry changed: %v", err)
	}
	var conflict *InputReceiptConflictError
	if _, err := e.svc.AckInputs(e.ctx, e.wkr, e.runID, 2, []int64{follow}); !errors.As(err, &conflict) {
		t.Fatalf("stale claim unexpectedly accepted: %v", err)
	}
}

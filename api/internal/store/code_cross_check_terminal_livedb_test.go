package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/store"
)

const terminalCodeFindings = `[{"id":"terminal-finding","description":"retain evidence"}]`
const terminalCodeDispositions = `[{"finding_id":"terminal-finding","disposition":"addressed","reason":"fixed before completion"}]`

func terminalCodeSnapshot(t *testing.T, fx *planCrossCheckDeletionFixture) map[string]json.RawMessage {
	t.Helper()
	var raw []byte
	if err := fx.f.pool.QueryRow(context.Background(), "SELECT to_jsonb(cc) FROM cross_checks cc WHERE id=$1", fx.crossCheckID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var row map[string]json.RawMessage
	if err := json.Unmarshal(raw, &row); err != nil {
		t.Fatal(err)
	}
	return row
}

func terminalCodeFinalize(t *testing.T, fx *planCrossCheckDeletionFixture) error {
	t.Helper()
	_, err := fx.f.q.FinalizeCodeCrossCheckDispositions(context.Background(), store.FinalizeCodeCrossCheckDispositionsParams{
		LeadRunID: fx.f.runID, WorkerID: pgU(fx.f.workerID), ClaimGeneration: 1, Dispositions: []byte(terminalCodeDispositions),
	})
	return err
}

func terminalCodeVerdict(t *testing.T, fx *planCrossCheckDeletionFixture) error {
	t.Helper()
	_, err := fx.f.q.DecideCodeCrossCheck(context.Background(), store.DecideCodeCrossCheckParams{
		ChildID: fx.checkerID, WorkerID: pgU(fx.f.workerID), ClaimGeneration: 1, Outcome: "completed", Findings: []byte(terminalCodeFindings),
	})
	return err
}

func terminalCodeFixture(t *testing.T, state string) *planCrossCheckDeletionFixture {
	t.Helper()
	fx := codeCrossCheckFixture(t)
	outcome := state
	var reason any
	if state == "finalized" {
		outcome = "completed"
	}
	if state == "failed" {
		reason = "confinement_failed"
	}
	if err := insertCodeFixture(t, fx, codeFixtureHead, codeFixtureBase, outcome, reason); err != nil {
		t.Fatal(err)
	}
	mustExec(context.Background(), t, fx.f.pool, "UPDATE runs SET status='running',claim_generation=1 WHERE id=$1", fx.checkerID)
	if outcome == "completed" {
		mustExec(context.Background(), t, fx.f.pool, "UPDATE cross_checks SET findings=$2 WHERE id=$1", fx.crossCheckID, terminalCodeFindings)
	}
	if state == "finalized" {
		// Finalize through the store API while the lead still has valid custody.
		if err := terminalCodeFinalize(t, fx); err != nil {
			t.Fatal(err)
		}
	}
	return fx
}

func TestCodeCrossCheckTerminalWorkerRemovalLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name, status, state string
		direct              bool
	}{
		{"completed-finalized-delete", "completed", "finalized", false},
		{"completed-confinement-failed-delete", "completed", "failed", false},
		{"completed-finalized-direct-null", "completed", "finalized", true},
		{"failed-delete", "failed", "completed", false},
		{"cancelled-delete", "cancelled", "completed", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			fx := terminalCodeFixture(t, tc.state)
			f := fx.f
			mustExec(ctx, t, f.pool, "UPDATE workers SET ephemeral=true,ephemeral_run_id=$2 WHERE id=$1", f.workerID, f.runID)
			mustExec(ctx, t, f.pool, "UPDATE runs SET status=$2 WHERE id=$1", f.runID, tc.status)
			// Failed/cancelled transitions already interrupt; snapshot their settled row.
			before := terminalCodeSnapshot(t, fx)
			if tc.status == "completed" && string(before["interrupted_at"]) != "null" {
				t.Fatal("completed evidence already interrupted")
			}
			if tc.direct {
				mustExec(ctx, t, f.pool, "UPDATE runs SET worker_id=NULL WHERE id=$1", f.runID)
			} else {
				tag, err := f.pool.Exec(ctx, "DELETE FROM workers WHERE id=$1", f.workerID)
				if err != nil {
					t.Fatal(err)
				}
				if tag.RowsAffected() != 1 {
					t.Fatalf("deleted workers = %d", tag.RowsAffected())
				}
			}
			var workerNull, workerExists bool
			if err := f.pool.QueryRow(ctx, "SELECT worker_id IS NULL,EXISTS(SELECT 1 FROM workers WHERE id=$2) FROM runs WHERE id=$1", f.runID, f.workerID).Scan(&workerNull, &workerExists); err != nil {
				t.Fatal(err)
			}
			if !workerNull || workerExists != tc.direct {
				t.Fatalf("worker null=%v exists=%v", workerNull, workerExists)
			}
			after := terminalCodeSnapshot(t, fx)
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("terminal worker removal changed complete cross_check row:\nbefore=%s\nafter=%s", mustTerminalJSON(t, before), mustTerminalJSON(t, after))
			}
		})
	}
}

func mustTerminalJSON(t *testing.T, row map[string]json.RawMessage) []byte {
	t.Helper()
	raw, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func assertTerminalCodeInterruption(t *testing.T, fx *planCrossCheckDeletionFixture, before map[string]json.RawMessage, reason string) {
	t.Helper()
	after := terminalCodeSnapshot(t, fx)
	if string(after["outcome"]) != `"failed"` || string(after["reason_class"]) != `"`+reason+`"` || string(after["interrupted_at"]) == "null" {
		t.Fatalf("expected failed/%s/interrupted, got %s", reason, mustTerminalJSON(t, after))
	}
	if string(before["outcome"]) != `"pending"` && !reflect.DeepEqual(before["decided_at"], after["decided_at"]) {
		t.Fatal("interruption changed existing decided_at")
	}
	// Settlement metadata may change; every other field, including findings,
	// dispositions and finalized_at, must survive.
	for _, key := range []string{"outcome", "reason_class", "interrupted_at", "decided_at", "wait_credited"} {
		delete(before, key)
		delete(after, key)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("interruption lost evidence: before=%s after=%s", mustTerminalJSON(t, before), mustTerminalJSON(t, after))
	}
	// These refusals exercise the full API guards: custody, outcome and child
	// status also change. They do not isolate the interrupted_at predicate.
	if err := terminalCodeVerdict(t, fx); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("late verdict: %v", err)
	}
	if err := terminalCodeFinalize(t, fx); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("late dispositions: %v", err)
	}
}

func TestCodeCrossCheckActiveCustodyEvidenceLiveDB(t *testing.T) {
	for _, state := range []string{"pending", "completed", "finalized"} {
		for _, exit := range []string{"worker-loss-reclaim", "release", "generation"} {
			t.Run(state+"/"+exit, func(t *testing.T) {
				ctx := context.Background()
				fx := terminalCodeFixture(t, state)
				before := terminalCodeSnapshot(t, fx)
				switch exit {
				case "worker-loss-reclaim":
					mustExec(ctx, t, fx.f.pool, "UPDATE runs SET worker_id=NULL WHERE id=$1", fx.f.runID)
					mustExec(ctx, t, fx.f.pool, "UPDATE runs SET worker_id=$2,claim_generation=claim_generation+1,claim_released_at=NULL WHERE id=$1", fx.f.runID, fx.f.workerID)
				case "release":
					mustExec(ctx, t, fx.f.pool, "UPDATE runs SET claim_released_at=now() WHERE id=$1", fx.f.runID)
				case "generation":
					mustExec(ctx, t, fx.f.pool, "UPDATE runs SET claim_generation=claim_generation+1 WHERE id=$1", fx.f.runID)
				}
				assertTerminalCodeInterruption(t, fx, before, "superseded")
			})
		}
	}
}

func TestCodeCrossCheckExpiredCustodyLiveDB(t *testing.T) {
	ctx := context.Background()
	fx := codeCrossCheckFixture(t)
	// The deadline is immutable: set the expired value on INSERT, not UPDATE.
	mustExec(ctx, t, fx.f.pool, `INSERT INTO cross_checks
 (id,lead_run_id,checker_run_id,stage,round,lead_claim_generation,verdict,
 head_commit,base_commit,candidate_digest,outcome,code_context,guidance_snapshot,
 guidance_text,guidance_digest,repo_instructions_text,repo_instructions_digest,
 checker_harness,created_at,deadline_at)
 VALUES($1,$2,$3,'code',1,1,'failed',$4,$5,$6,'pending','{}','',
 '',sha256(convert_to('','UTF8')),'',sha256(convert_to('','UTF8')),'codex',
 now()-interval '20 seconds',now()-interval '10 seconds')`,
		fx.crossCheckID, fx.f.runID, fx.checkerID, codeFixtureHead, codeFixtureBase, []byte("expired fixture"))
	before := terminalCodeSnapshot(t, fx)
	mustExec(ctx, t, fx.f.pool, "UPDATE runs SET worker_id=NULL WHERE id=$1", fx.f.runID)
	after := terminalCodeSnapshot(t, fx)
	if !reflect.DeepEqual(after["decided_at"], after["deadline_at"]) {
		t.Fatal("expired decision must use deadline")
	}
	assertTerminalCodeInterruption(t, fx, before, "timed_out")
}

func TestCodeCrossCheckTerminalGuardBoundariesLiveDB(t *testing.T) {
	for _, change := range []string{"generation", "status"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			fx := terminalCodeFixture(t, "finalized")
			mustExec(ctx, t, fx.f.pool, "UPDATE runs SET status='completed' WHERE id=$1", fx.f.runID)
			before := terminalCodeSnapshot(t, fx)
			if string(before["interrupted_at"]) != "null" {
				t.Fatal("terminal evidence already interrupted")
			}
			if change == "generation" {
				mustExec(ctx, t, fx.f.pool, "UPDATE runs SET worker_id=NULL,claim_generation=claim_generation+1 WHERE id=$1", fx.f.runID)
			} else {
				mustExec(ctx, t, fx.f.pool, "UPDATE runs SET worker_id=NULL,status='failed' WHERE id=$1", fx.f.runID)
			}
			assertTerminalCodeInterruption(t, fx, before, "superseded")
		})
	}
}

func TestCodeCrossCheckEvidenceWriteControlsLiveDB(t *testing.T) {
	t.Run("eligible-verdict", func(t *testing.T) {
		fx := terminalCodeFixture(t, "pending")
		if err := terminalCodeVerdict(t, fx); err != nil {
			t.Fatal(err)
		}
		after := terminalCodeSnapshot(t, fx)
		if string(after["outcome"]) != `"completed"` || string(after["decided_at"]) == "null" || string(after["interrupted_at"]) != "null" {
			t.Fatalf("verdict not recorded: %s", mustTerminalJSON(t, after))
		}
	})
	t.Run("eligible-dispositions", func(t *testing.T) {
		fx := terminalCodeFixture(t, "completed")
		if err := terminalCodeFinalize(t, fx); err != nil {
			t.Fatal(err)
		}
		after := terminalCodeSnapshot(t, fx)
		if string(after["finalized_at"]) == "null" || string(after["dispositions"]) == "null" {
			t.Fatal("dispositions not recorded")
		}
	})
}

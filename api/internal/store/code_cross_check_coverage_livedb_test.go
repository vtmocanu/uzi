package store_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/store"
)

const storedCodeCoverage = `{"version":1,"entries":[{"path":"a.go","status":"examined","reason":"checked","recorded_read":true}],"inventory_incomplete":false,"omitted_paths":0,"omitted_count_exact":true,"unknown_judgments":0,"invalid_judgments":0,"omitted_judgments":0,"activity_complete":true,"reasons":[]}`

func TestCodeCrossCheckCoverageAtomicImmutableLiveDB(t *testing.T) {
	ctx := context.Background()
	fx := terminalCodeFixture(t, "pending")
	for _, sql := range []string{
		"UPDATE cross_checks SET coverage=$2 WHERE id=$1",
		"UPDATE cross_checks SET coverage=$2,outcome='failed',reason_class='model_error',decided_at=now() WHERE id=$1",
	} {
		if _, err := fx.f.pool.Exec(ctx, sql, fx.crossCheckID, storedCodeCoverage); err == nil {
			t.Fatal("coverage admitted outside completed verdict")
		}
	}
	cc, err := fx.f.q.DecideCodeCrossCheck(ctx, store.DecideCodeCrossCheckParams{
		ChildID: fx.checkerID, WorkerID: pgU(fx.f.workerID), ClaimGeneration: 1,
		Outcome: "completed", Findings: []byte(terminalCodeFindings), Coverage: []byte(storedCodeCoverage),
	})
	if err != nil || len(cc.Coverage) == 0 || len(cc.Findings) == 0 || !cc.DecidedAt.Valid {
		t.Fatalf("atomic verdict: %+v %v", cc, err)
	}
	before := terminalCodeSnapshot(t, fx)
	for _, assignment := range []string{"coverage=NULL", "coverage='{}'", "coverage=coverage || '{\"activity_complete\":false}'"} {
		if _, err := fx.f.pool.Exec(ctx, "UPDATE cross_checks SET "+assignment+" WHERE id=$1", fx.crossCheckID); err == nil {
			t.Fatalf("coverage overwrite before dispositions: %s", assignment)
		}
	}
	if !reflect.DeepEqual(before, terminalCodeSnapshot(t, fx)) {
		t.Fatal("rejected overwrite changed evidence")
	}
	if err := terminalCodeFinalize(t, fx); err != nil {
		t.Fatal(err)
	}
	after := terminalCodeSnapshot(t, fx)
	if !reflect.DeepEqual(before["coverage"], after["coverage"]) {
		t.Fatal("finalization changed coverage")
	}
	if _, err := fx.f.q.DecideCodeCrossCheck(ctx, store.DecideCodeCrossCheckParams{
		ChildID: fx.checkerID, WorkerID: pgU(fx.f.workerID), ClaimGeneration: 1, Outcome: "completed", Findings: []byte("[]"),
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("second verdict: %v", err)
	}
	if _, err := fx.f.pool.Exec(ctx, "UPDATE cross_checks SET coverage=NULL WHERE id=$1", fx.crossCheckID); err == nil {
		t.Fatal("coverage erased after dispositions")
	}
}
func TestCodeCrossCheckCoverageLegacyAndPlanLiveDB(t *testing.T) {
	ctx := context.Background()
	t.Run("legacy", func(t *testing.T) {
		fx := terminalCodeFixture(t, "pending")
		if err := terminalCodeVerdict(t, fx); err != nil {
			t.Fatal(err)
		}
		if string(terminalCodeSnapshot(t, fx)["coverage"]) != "null" {
			t.Fatal("legacy verdict invented coverage")
		}
		if _, err := fx.f.pool.Exec(ctx, "UPDATE cross_checks SET coverage=$2 WHERE id=$1", fx.crossCheckID, storedCodeCoverage); err == nil {
			t.Fatal("late legacy backfill admitted")
		}
	})
	t.Run("plan", func(t *testing.T) {
		fx := setupPlanCrossCheckDeletion(ctx, t)
		var isNull bool
		if err := fx.f.pool.QueryRow(ctx, "SELECT coverage IS NULL FROM cross_checks WHERE id=$1", fx.crossCheckID).Scan(&isNull); err != nil || !isNull {
			t.Fatalf("plan coverage: %v", err)
		}
		if _, err := fx.f.pool.Exec(ctx, "UPDATE cross_checks SET coverage=$2 WHERE id=$1", fx.crossCheckID, storedCodeCoverage); err == nil {
			t.Fatal("plan acquired code coverage")
		}
	})
}
func TestCodeCrossCheckCoverageCustodyPreservationLiveDB(t *testing.T) {
	for _, exit := range []string{"release", "generation", "worker-loss", "active-worker-delete", "failed", "cancelled", "completed", "terminal-worker-delete"} {
		for _, finalize := range []bool{false, true} {
			t.Run(exit+map[bool]string{false: "/verdict", true: "/finalized"}[finalize], func(t *testing.T) {
				ctx := context.Background()
				fx := terminalCodeFixture(t, "pending")
				_, err := fx.f.q.DecideCodeCrossCheck(ctx, store.DecideCodeCrossCheckParams{
					ChildID: fx.checkerID, WorkerID: pgU(fx.f.workerID), ClaimGeneration: 1, Outcome: "completed",
					Findings: []byte(terminalCodeFindings), Coverage: []byte(storedCodeCoverage),
				})
				if err != nil {
					t.Fatal(err)
				}
				if finalize {
					if err := terminalCodeFinalize(t, fx); err != nil {
						t.Fatal(err)
					}
				}
				before := terminalCodeSnapshot(t, fx)
				switch exit {
				case "release":
					mustExec(ctx, t, fx.f.pool, "UPDATE runs SET claim_released_at=now() WHERE id=$1", fx.f.runID)
				case "generation":
					mustExec(ctx, t, fx.f.pool, "UPDATE runs SET claim_generation=claim_generation+1 WHERE id=$1", fx.f.runID)
				case "worker-loss":
					mustExec(ctx, t, fx.f.pool, "UPDATE runs SET worker_id=NULL WHERE id=$1", fx.f.runID)
				case "active-worker-delete":
					mustExec(ctx, t, fx.f.pool, "DELETE FROM workers WHERE id=$1", fx.f.workerID)
				case "terminal-worker-delete":
					mustExec(ctx, t, fx.f.pool, "UPDATE runs SET status='completed' WHERE id=$1", fx.f.runID)
					mustExec(ctx, t, fx.f.pool, "DELETE FROM workers WHERE id=$1", fx.f.workerID)
				default:
					mustExec(ctx, t, fx.f.pool, "UPDATE runs SET status=$2 WHERE id=$1", fx.f.runID, exit)
				}
				after := terminalCodeSnapshot(t, fx)
				for _, key := range []string{"coverage", "findings", "dispositions", "finalized_at"} {
					if !reflect.DeepEqual(before[key], after[key]) {
						t.Fatalf("%s lost on %s", key, exit)
					}
				}
				if strings.Contains(exit, "completed") || exit == "terminal-worker-delete" {
					if string(after["interrupted_at"]) != "null" {
						t.Fatal("terminal cleanup interrupted evidence")
					}
				} else if string(after["interrupted_at"]) == "null" {
					t.Fatal("custody loss did not interrupt")
				}
			})
		}
	}
}

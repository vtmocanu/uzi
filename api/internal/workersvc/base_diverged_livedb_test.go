package workersvc

import (
	"fmt"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestBaseDivergedSetStateJudgeLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.forgeParkService(t, 6)
	// Execute judge enqueue synchronously so the database assertion observes its result.
	svc.SetBackground(func(f func()) { f() })
	e.enableJudge(t, svc)
	w := e.seedWorker(t, nil)
	for _, iter := range []int32{0, 7} {
		for _, origin := range []string{"base_diverged", "agent_failure"} {
			t.Run(fmt.Sprintf("%s/%d", origin, iter), func(t *testing.T) {
				id := e.fpSeedRunning(t, w, 1)
				e.exec(t, "UPDATE runs SET iteration_count=$2 WHERE id=$1", id, iter)
				reason := "base refusal or control"
				run, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, id, StateRequest{
					State: "failed", ClaimGeneration: i64Ptr(1), FailOrigin: &origin, FailureReason: &reason,
				})
				if err != nil || !applied || run.Status != "failed" {
					t.Fatalf("SetState: applied=%v run=%+v err=%v", applied, run, err)
				}
				got, err := e.q.GetRunByID(e.ctx, id)
				if err != nil || !got.FailOrigin.Valid || got.FailOrigin.String != origin {
					t.Fatalf("stored origin: %+v err=%v", got.FailOrigin, err)
				}
				var count int
				if err := e.pool.QueryRow(e.ctx, "SELECT count(*) FROM runs WHERE kind='judge' AND target_run_id=$1", id).Scan(&count); err != nil {
					t.Fatal(err)
				}
				want := 0
				if origin == "agent_failure" {
					want = 1
				}
				if count != want {
					t.Fatalf("judge count=%d want %d", count, want)
				}
			})
		}
	}
}

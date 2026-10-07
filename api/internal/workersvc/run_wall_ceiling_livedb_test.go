package workersvc

import (
	"github.com/vtmocanu/uzi/api/internal/store"
	"testing"
	"time"
)

// Both freeze entry points must persist the operator ceiling, including the large-repo floor.
func TestConfiguredRunWallCeilingFreezeLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	wid := e.seedWorker(t, nil)
	for _, path := range []string{"approve", "running"} {
		for _, size := range []string{"s", "l"} {
			t.Run(path+"/"+size, func(t *testing.T) {
				id := e.seedActiveRunOwnedBy(t, wid)
				candidate := `[{"id":"m1","title":"first"},{"id":"m2","title":"second"},{"id":"m3","title":"third"}]`
				if size == "l" {
					candidate = `[{"id":"m1","title":"first"}]`
				}
				e.exec(t, `UPDATE runs SET size_class=$2, milestones_candidate=$3 WHERE id=$1`, id, size, candidate)
				params := testParams()
				params.RunTimeout = 6 * time.Hour
				params.RunWallCeiling = 13 * time.Hour
				svc := New(e.q, newBox(t), params)
				if path == "approve" {
					e.exec(t, `UPDATE runs SET status='awaiting_approval' WHERE id=$1`, id)
					if _, err := svc.SubmitInput(e.ctx, e.userID, id, "approve_plan", "", &AgentSelection{Source: AgentSourceOwn}); err != nil {
						t.Fatal(err)
					}
				} else {
					e.exec(t, `INSERT INTO run_user_inputs(run_id,kind,body,consumed_at,applied_at) VALUES($1,'approve_plan','{}',now(),now())`, id)
					if _, applied, err := svc.SetState(e.ctx, store.Worker{ID: wid}, id, StateRequest{State: "running"}); err != nil || !applied {
						t.Fatalf("running freeze: applied=%v err=%v", applied, err)
					}
				}
				var wall int32
				if err := e.pool.QueryRow(e.ctx, `SELECT budget_wall_seconds FROM runs WHERE id=$1`, id).Scan(&wall); err != nil {
					t.Fatal(err)
				}
				if wall != 13*3600 {
					t.Fatalf("frozen wall=%d, want 46800 (configured ceiling)", wall)
				}
			})
		}
	}
}

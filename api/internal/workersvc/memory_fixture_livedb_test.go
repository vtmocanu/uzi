package workersvc

import (
	"testing"

	"github.com/google/uuid"
)

// The parent fixture is a foreign-row control: subtest cleanup must remove its
// own health-active runs without deleting another fixture's active run.
func TestMemoryFixtureIsolationLiveDB(t *testing.T) {
	control := newMemoryFixture(t)
	for _, kind := range []string{"issue", "chat", "plan", "code", "foreign code"} {
		t.Run(kind, func(t *testing.T) {
			var owners, runs []uuid.UUID
			t.Run("fixture", func(t *testing.T) {
				var f memoryFixture
				var lead uuid.UUID
				switch kind {
				case "plan":
					f, lead = memoryCheckerFixture(t)
				case "code", "foreign code":
					f, lead = memoryCodeFixture(t, false)
				default:
					f = newMemoryFixture(t)
				}
				owners = append(owners, f.w.UserID)
				runs = append(runs, f.b.RunID)
				if lead != uuid.Nil {
					runs = append(runs, lead)
				}
				f.reserve(t, f.b) // Include the cascaded intervention record.
				if kind == "chat" {
					chat := seedOutageChatRun(t, f.e, f.w.UserID, f.w.ID, 2, 2)
					runs = append(runs, chat)
				}
				if kind == "foreign code" {
					user, _, repo := f.e.seedCodexInfra(t)
					f.ownUser(user)
					owners = append(owners, user)
					target := seedOutageRun(t, f.e, user, repo, f.w.ID, "running", "issue", 2, 0)
					runs = append(runs, target)
					f.e.exec("UPDATE runs SET target_run_id=$2 WHERE id=$1", f.b.RunID, target)
					f.e.exec("UPDATE runs SET user_id=$2 WHERE id=$1", lead, user)
				}
				if kind == "issue" {
					f.e.exec(`INSERT INTO recovery_custody_holds
						(id,user_id,repo_id,run_id,generation,state,original_worker_id,
						 original_worker_identity,live_worker_id,live_run_id)
						VALUES($1,$2,$3,$4,2,'open',$5,'memory-cleanup-fixture',$5,$4)`,
						uuid.New(), f.w.UserID, f.repo, f.b.RunID, f.w.ID)
				}
				active, err := control.e.q.ListActiveRunsForHealth(control.e.ctx, codexCuratedModelsSlice())
				if err != nil {
					t.Fatal(err)
				}
				seen := false
				for _, r := range active {
					if r.ID == f.b.RunID {
						seen = true
					}
				}
				if !seen {
					t.Fatal("fixture did not enter the global health scan")
				}
			}) // Fixture cleanup and pool.Close have both run when this returns.
			if len(owners) == 0 {
				t.Fatal("fixture did not seed an owner")
			}
			active, err := control.e.q.ListActiveRunsForHealth(control.e.ctx, codexCuratedModelsSlice())
			if err != nil {
				t.Fatal(err)
			}
			seenControl := false
			for _, r := range active {
				if r.ID == control.b.RunID {
					seenControl = true
				}
				for _, id := range runs {
					if r.ID == id {
						t.Fatalf("fixture run %s remains health-active after subtest cleanup", id)
					}
				}
			}
			if !seenControl {
				t.Fatal("cleanup removed the foreign fixture's health-active run")
			}
			var residual bool
			if err := control.e.pool.QueryRow(control.e.ctx, `SELECT
				EXISTS(SELECT 1 FROM users WHERE id=ANY($1))
				OR EXISTS(SELECT 1 FROM runs WHERE id=ANY($2))
				OR EXISTS(SELECT 1 FROM workers WHERE user_id=ANY($1))
				OR EXISTS(SELECT 1 FROM recovery_custody_holds WHERE user_id=ANY($1))
				OR EXISTS(SELECT 1 FROM cross_checks WHERE lead_run_id=ANY($2) OR checker_run_id=ANY($2))
				OR EXISTS(SELECT 1 FROM memory_interventions WHERE user_id=ANY($1))`,
				owners, runs).Scan(&residual); err != nil {
				t.Fatal(err)
			}
			if residual {
				t.Fatal("fixture-owned data remains after subtest cleanup")
			}
			if _, err := control.e.q.GetRunByID(control.e.ctx, control.b.RunID); err != nil {
				t.Fatalf("foreign fixture was changed: %v", err)
			}
		})
	}
}

package workersvc

import (
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestRetainedParkSweepersPreserveHeldRowLiveDB(t *testing.T) {
	for _, name := range []string{
		"health", "RequestWallParks", "ParkRunsAtWall",
		"FailRunsOfStaleWorkersOverCap", "RequeueRunsOfStaleWorkers",
		"FailWorkerRunsOverCap", "RequeueWorkerRuns",
		"FailRunsMissingFromSnapshot", "RequeueRunsMissingFromSnapshot",
		"RequeueAttestedFinalizeRuns",
	} {
		t.Run(name, func(t *testing.T) {
			env := setupCodexLiveDB(t)
			user, _, repo := env.seedCodexInfra(t)
			worker := seedSnapshotWorker(t, env, user, "retained-sweep")
			spent := int32(7)
			switch name {
			case "RequeueRunsOfStaleWorkers", "RequeueWorkerRuns", "RequeueRunsMissingFromSnapshot", "RequeueAttestedFinalizeRuns":
				spent = 4 // Within budget even before the held status and fence exclude it.
			}
			held := seedOutageRun(t, env, user, repo, worker, "running", "issue", 2, spent)
			env.exec("UPDATE runs SET worker_recovery_episode=2, requeue_episode_baseline=4, retained_park_count=3, recovery_wait_count=9, budget_wall_seconds=60 WHERE id=$1", held)
			hold := uuid.New()
			env.exec("INSERT INTO recovery_custody_holds(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id) VALUES($1,$2,$3,$4,2,'open',$5,'retained-sweep',$5,$4)", hold, user, repo, held, worker)
			capture := uuid.New()
			env.exec("INSERT INTO recovery_captures(id,hold_id,run_id,user_id,original_worker_identity,source_sha,idempotency_key,state,expires_at) VALUES($1,$2,$3,$4,'retained-sweep','aaaa1111','retained-sweep','available',now()+interval '1 hour')", capture, hold, held, user)
			svc := snapshotSvc(env, testParams())
			svc.p.RunRetainedRecoveryMaxParks = 0
			if _, applied, err := svc.SetState(env.ctx, store.Worker{ID: worker}, held, retainedReport(2)); err != nil || !applied {
				t.Fatalf("park applied=%v err=%v", applied, err)
			}
			// The held row has both an expired wall and a stale worker. Snapshot-based
			// writers additionally see no active-run evidence for its generation.
			env.exec("UPDATE workers SET last_heartbeat_at=now()-interval '1 hour' WHERE id=$1", worker)
			before := exhaustionRun(t, env, held)
			if before.Status != "recovery_wait" || before.RecoveryWaitCause.String != recoveryCauseRetainedRecoveryExhausted || before.RecoveryRetryNotBefore.Valid || !before.ClaimReleasedAt.Valid {
				t.Fatalf("not a fenced NULL-stamp retained hold: %+v", before)
			}
			custody := func() string {
				t.Helper()
				var snapshot string
				if err := env.pool.QueryRow(env.ctx, "SELECT json_build_array(row_to_json(h),row_to_json(c))::text FROM recovery_custody_holds h JOIN recovery_captures c ON c.hold_id=h.id WHERE h.id=$1 AND c.id=$2", hold, capture).Scan(&snapshot); err != nil {
					t.Fatal(err)
				}
				return snapshot
			}
			custodyBefore := custody()
			count := int32(0)
			switch name {
			case "FailRunsOfStaleWorkersOverCap", "FailWorkerRunsOverCap", "FailRunsMissingFromSnapshot":
				count = 2
			}
			control := seedOutageRun(t, env, user, repo, worker, "running", "issue", 2, count)
			now := time.Now()
			cutoff := pgconv.Time(now.Add(-time.Minute))
			wantStatus := "queued"
			switch name {
			case "health":
				rows, err := env.q.ListActiveRunsForHealth(env.ctx, codexCuratedModelsSlice())
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, row := range rows {
					if row.ID == held {
						t.Fatal("held row selected for health")
					}
					found = found || row.ID == control
				}
				if !found {
					t.Fatal("health did not select eligible control")
				}
				wantStatus = "running"
			case "RequestWallParks":
				// RequestWallParks requires a live capable worker; the server-side
				// ParkRunsAtWall scenario separately uses the stale incarnation.
				env.exec("UPDATE workers SET last_heartbeat_at=now(), protocol_capabilities=ARRAY['wall_park_v1']::text[] WHERE id=$1", worker)
				env.exec("UPDATE runs SET budget_wall_seconds=60 WHERE id=$1", control)
				rows, err := env.q.RequestWallParks(env.ctx, store.RequestWallParksParams{Now: pgconv.Time(now), GlobalTimeoutSeconds: 7200, WorkerStaleCutoff: cutoff})
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, row := range rows {
					if row.ID == held {
						t.Fatal("held row wall-requested")
					}
					found = found || row.ID == control
				}
				if !found || exhaustionRun(t, env, control).PauseMode.String != "wall" {
					t.Fatal("wall request did not affect control")
				}
				wantStatus = "running"
			case "FailRunsMissingFromSnapshot":
				if _, err := env.q.FailRunsMissingFromSnapshot(env.ctx, store.FailRunsMissingFromSnapshotParams{WorkerID: pgconv.UUID(worker), MissingCutoff: cutoff, MaxRequeues: 1, Now: pgconv.Time(now), GlobalTimeoutSeconds: 7200, FailureReason: pgconv.TextOrNull("lost")}); err != nil {
					t.Fatal(err)
				}
				wantStatus = "failed"
			case "RequeueRunsMissingFromSnapshot":
				if _, err := env.q.RequeueRunsMissingFromSnapshot(env.ctx, store.RequeueRunsMissingFromSnapshotParams{WorkerID: pgconv.UUID(worker), MissingCutoff: cutoff, MaxRequeues: 1, Now: pgconv.Time(now), GlobalTimeoutSeconds: 7200}); err != nil {
					t.Fatal(err)
				}
			case "RequeueAttestedFinalizeRuns":
				if _, err := env.q.RequeueAttestedFinalizeRuns(env.ctx, store.RequeueAttestedFinalizeRunsParams{WorkerID: pgconv.UUID(worker), RunIds: []uuid.UUID{held, control}, ClaimGenerations: []int64{2, 2}, MaxRequeues: 1}); err != nil {
					t.Fatal(err)
				}
			default:
				found := false
				for _, writer := range outageTerminalWriters() {
					if writer.name == name {
						writer.fire(t, env, worker, control)
						wantStatus = writer.sweptStatus
						found = true
						break
					}
				}
				if !found {
					t.Fatalf("missing writer %s", name)
				}
			}
			if got := exhaustionRun(t, env, control); got.Status != wantStatus {
				t.Fatalf("control status=%s want %s", got.Status, wantStatus)
			}
			after := exhaustionRun(t, env, held)
			if !reflect.DeepEqual(before, after) || custody() != custodyBefore {
				t.Fatalf("%s changed held run, counters, fence or custody: before=%+v after=%+v", name, before, after)
			}
		})
	}
}

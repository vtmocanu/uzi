package handler

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

func TestWorkerCodexStoppedLaneRouteLiveDB(t *testing.T) {
	for _, status := range []string{"running", "paused"} {
		for _, lane := range []string{"chat", "judge", "job", "cross_check", "reviewtarget"} {
			t.Run(status+"/"+lane, func(t *testing.T) {
				e, w := setupStoppedGuardsRoute(t)
				withdrawStoppedRoute(t, e, w)
				if status == "paused" {
					reportStoppedRoute(t, e, w, true, true)
				}

				// guard_checkpoint_coordinates permits identity changes only on
				// this unenrolled fixture; never clear coordinates or disable it.
				var unenrolled bool
				if err := e.pool.QueryRow(context.Background(),
					"SELECT checkpoint_coordinates IS NULL FROM runs WHERE id=$1", e.run).Scan(&unenrolled); err != nil {
					t.Fatal(err)
				}
				if !unenrolled {
					t.Fatal("lane fixture blocked by frozen checkpoint coordinates")
				}

				target := uuid.New()
				if lane == "judge" || lane == "cross_check" || lane == "reviewtarget" {
					// Fresh same-owner TASK target, on the existing repository.
					// No copied claim, credentials, tokens, or review relationship.
					stoppedRouteExec(t, e, `INSERT INTO runs
 (id,user_id,repo_id,kind,branch,issue_title,issue_description,status,harness)
 SELECT $2::uuid,user_id,repo_id,'task','uzi/task/' || $2::uuid::text,
 'stopped lane target','', 'queued',harness FROM runs WHERE id=$1`, e.run, target)
				}

				wantKind := lane
				switch lane {
				case "chat":
					stoppedRouteExec(t, e, `UPDATE runs SET kind='chat',
 repo_id=NULL,issue_iid=NULL,branch=NULL WHERE id=$1`, e.run)
				case "judge":
					stoppedRouteExec(t, e, `UPDATE runs SET kind='judge',
 repo_id=NULL,issue_iid=NULL,branch=NULL,target_run_id=$2 WHERE id=$1`, e.run, target)
				case "job":
					stoppedRouteExec(t, e, `UPDATE runs SET kind='job',
 repo_id=NULL,issue_iid=NULL,branch=NULL,job_type='research' WHERE id=$1`, e.run)
				case "cross_check":
					stoppedRouteExec(t, e, `UPDATE runs SET kind='cross_check',
 issue_iid=NULL,target_run_id=$2,report_only=true WHERE id=$1`, e.run, target)
				case "reviewtarget":
					wantKind = "task"
					stoppedRouteExec(t, e, `UPDATE runs SET kind='task',
 issue_iid=NULL,branch='uzi/task/' || $2::uuid::text,review_target_run_id=$2::uuid WHERE id=$1`, e.run, target)
				}

				// Check the actual lane and preserve the live Codex authority.
				var valid bool
				if err := e.pool.QueryRow(context.Background(), `SELECT
 kind=$2 AND status=$3 AND harness='codex'
 AND codex_secret_id IS NOT NULL AND codex_cap_hash IS NOT NULL
 AND codex_claim_epoch=$4 AND claim_generation=$5
 AND claim_released_at IS NULL AND worker_id=$6
 AND hold_reason IS NULL AND NOT policy_pending_fresh
 AND credential_switch_requested_at IS NULL AND credential_switch_generation IS NULL
 AND checkpoint_coordinates IS NULL
 AND CASE WHEN $7 THEN review_target_run_id=$8 ELSE review_target_run_id IS NULL END
 FROM runs WHERE id=$1`, e.run, wantKind, status, w.ExpectedEpoch, e.gen, e.worker,
					lane == "reviewtarget", target).Scan(&valid); err != nil {
					t.Fatal(err)
				}
				if !valid {
					t.Fatal("stored excluded lane lost its live Codex fixture authority")
				}
				reportStoppedRoute(t, e, w, false, false)
			})
		}
	}
}

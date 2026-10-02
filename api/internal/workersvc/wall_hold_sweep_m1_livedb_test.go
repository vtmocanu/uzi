package workersvc

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestWallSweepAfterEmptyHeartbeatLiveDB(t *testing.T) {
	cases := []struct {
		name         string
		overCap      bool
		extension    int
		postattempt  bool
		interactive  bool
		stale        bool
		graceExpired bool
		want         string
	}{
		{name: "stale-under-cap", stale: true, want: "paused"},
		{name: "stale-over-cap", stale: true, overCap: true, want: "paused"},
		{name: "live-within-grace", want: "running"},
		{name: "live-over-cap-within-grace", overCap: true, want: "running"},
		{name: "live-after-grace", graceExpired: true, want: "paused"},
		{name: "live-over-cap-after-grace", overCap: true, graceExpired: true, want: "paused"},
		{name: "extended", extension: 4 * 3600, want: "queued"},
		{name: "postattempt-completion-route", postattempt: true, want: "running"},
		{name: "interactive-exempt", interactive: true, want: "queued"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := setupCodexLiveDB(t)
			userID, repoID := seedWallOwner(t, env)
			workerID := uuid.New()
			env.exec(`INSERT INTO workers (id,user_id,name,token_hash,status,last_heartbeat_at,
                snapshot_register_nonce,protocol_capabilities)
                VALUES ($1,$2,'wall-empty',$3,'online',now(),'nonce-A',ARRAY['wall_park_v1'])`,
				workerID, userID, workerID[:])
			p := testParams()
			p.SweeperBootGrace = 0
			p.RunMaxRequeues = 3
			svc := snapshotSvc(env, p)
			wkr, err := env.q.GetWorkerByID(env.ctx, workerID)
			if err != nil {
				t.Fatal(err)
			}
			runID := uuid.New()
			requeues := 0
			if tc.overCap {
				requeues = p.RunMaxRequeues
			}
			attempts := 0
			if tc.postattempt {
				attempts = 1
			}
			var pauseMode any
			var pauseAt any
			var contract any
			if tc.postattempt {
				contract = 1
			}
			if tc.graceExpired {
				pauseMode = "wall"
				pauseAt = time.Now().Add(-11 * time.Minute)
			}
			env.exec(`INSERT INTO runs (id,user_id,repo_id,kind,issue_iid,issue_title,issue_description,
                status,worker_id,started_at,status_since,budget_wall_seconds,budget_extension_seconds,
                completion_attempts,interactive,claim_generation,requeue_count,pause_mode,pause_requested_at,completion_contract_version)
                VALUES ($1,$2,$3,'issue',987912,'t','d','running',$4,now()-interval '2 hours',
                now()-interval '5 minutes',60,$5,$6,$7,1,$8,$9,$10,$11)`,
				runID, userID, repoID, workerID, tc.extension, attempts, tc.interactive, requeues, pauseMode, pauseAt, contract)
			active := []ActiveRunEntry{}
			if tc.postattempt {
				active = []ActiveRunEntry{entry(runID, 1, "running", false)}
			}
			if _, err := svc.Heartbeat(env.ctx, wkr, nil, nil,
				&ActiveSnapshot{SnapshotEpoch: 1, RegisterNonce: "nonce-A", Active: active}); err != nil {
				t.Fatalf("valid empty heartbeat: %v", err)
			}
			if tc.want == "paused" || tc.want == "running" {
				got := mustRun(t, env, runID)
				if got.Status != "running" {
					t.Fatalf("heartbeat changed wall-eligible run to %q", got.Status)
				}
				if tc.overCap && !tc.stale && (got.FinishedAt.Valid || got.FailOrigin.Valid || got.FailureReason.Valid) {
					t.Fatalf("heartbeat terminalized live over-cap run: finished_at=%v fail_origin=%v failure_reason=%v",
						got.FinishedAt, got.FailOrigin, got.FailureReason)
				}
			}
			if tc.stale {
				env.exec(`UPDATE workers SET last_heartbeat_at=now()-interval '1 hour' WHERE id=$1`, workerID)
			}
			if _, err := svc.Sweep(env.ctx); err != nil {
				t.Fatalf("Sweep: %v", err)
			}
			got := mustRun(t, env, runID)
			if got.Status != tc.want {
				t.Fatalf("status = %q, want %q", got.Status, tc.want)
			}
			if tc.want == "paused" && (!got.HoldReason.Valid || got.HoldReason.String != "budget_exhausted") {
				t.Fatalf("parked hold = %q, want budget_exhausted", got.HoldReason.String)
			}
			if tc.overCap && !tc.stale {
				if got.FinishedAt.Valid || got.FailOrigin.Valid || got.FailureReason.Valid {
					t.Fatalf("sweep terminalized live over-cap run: finished_at=%v fail_origin=%v failure_reason=%v",
						got.FinishedAt, got.FailOrigin, got.FailureReason)
				}
				if got.ClaimReleasedAt.Valid != tc.graceExpired {
					t.Fatalf("claim_released_at valid = %v, want %v", got.ClaimReleasedAt.Valid, tc.graceExpired)
				}
			}
			if tc.postattempt && !got.CompletionBudgetExhaustedAt.Valid {
				t.Fatal("postattempt live run did not receive completion budget steer")
			}
		})
	}
}

package workersvc

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Issue 2009 M1: enabled regression, intentionally red until M2/M3.
// pending_all is independent of the capped/rotating active display. Exercise the
// existing wire decoder and Heartbeat, without naming future storage or Go types.
func TestIssue2009PendingAllSustainsHealthAcrossRotationLiveDB(t *testing.T) {
	for _, cap0 := range []bool{false, true} {
		name := "rotation"
		if cap0 {
			name = "cap0"
		}
		t.Run(name, func(t *testing.T) {
			e := setupCodexLiveDB(t)
			svc := healthSvcLive(e)
			o := seedReevalOwner(t, e, BindModeAuto, false)
			e.exec(`UPDATE workers SET snapshot_register_nonce = 'n1', snapshot_epoch = 0, last_heartbeat_at = now() WHERE id = $1`, o.workerID)
			a := seedRunningRun(t, e, o, nextOutageIID(), 0)
			b := seedRunningRun(t, e, o, nextOutageIID(), 0)
			for _, id := range []uuid.UUID{a, b} {
				setRunGeneration(t, e, id, 5)
			}
			// Establish first-seen age for both via the legacy-compatible list.
			heartbeatSnapshot(t, svc, e, o.workerID, entry(a, 5, "running", true), entry(b, 5, "running", true))
			// Age legacy rows without sleeps; recreating first-seen must not erase it.
			e.exec(`UPDATE worker_active_runs SET terminal_pending_since = now() - interval '1 hour' WHERE worker_id = $1`, o.workerID)
			svc.detectRunHealth(e.ctx, time.Now())
			for _, id := range []uuid.UUID{a, b} {
				if health, reason := runHealthOf(t, e, id); health != healthStalled || reason != reasonOutcomeUndelivered {
					t.Fatalf("initial pending health = %q / %q, want stalled / outcome_undelivered", health, reason)
				}
			}
			var lists [][]ActiveRunEntry
			if cap0 {
				lists = [][]ActiveRunEntry{{}, {}}
			} else {
				lists = [][]ActiveRunEntry{{entry(a, 5, "running", true)}, {entry(b, 5, "running", true)}}
			}
			// Two bounded heartbeats; check both runs after each rotation. A failure
			// on one run does not hide the sibling or the next heartbeat.
			for i, active := range lists {
				wire, err := json.Marshal(map[string]any{
					"snapshot_epoch": int64(i + 2), "register_nonce": "n1",
					"active": active, "pending_overflow": true,
					"pending_all": []map[string]any{
						{"run_id": a, "claim_generation": int64(5)},
						{"run_id": b, "claim_generation": int64(5)},
					},
				})
				if err != nil {
					t.Fatal(err)
				}
				var snap ActiveSnapshot
				if err := json.Unmarshal(wire, &snap); err != nil {
					t.Fatalf("decode snapshot JSON: %v", err)
				}
				wkr, err := e.q.GetWorkerByID(e.ctx, o.workerID)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := svc.Heartbeat(e.ctx, wkr, nil, nil, &snap); err != nil {
					t.Fatalf("Heartbeat: %v", err)
				}
				if got := workerEpoch(t, e, o.workerID); got != int64(i+2) {
					t.Fatalf("accepted snapshot epoch = %d, want %d", got, i+2)
				}
				svc.detectRunHealth(e.ctx, time.Now())
				for j, id := range []uuid.UUID{a, b} {
					if health, reason := runHealthOf(t, e, id); health != healthStalled || reason != reasonOutcomeUndelivered {
						t.Errorf("issue 2009: heartbeat %d run %d health=%q reason=%q; pending_all must sustain stalled / outcome_undelivered despite active omission", i+1, j+1, health, reason)
					}
				}
			}
		})
	}
}

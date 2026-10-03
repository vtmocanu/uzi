package workersvc

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// These cases use the real queued-row projection and composed query on a single
// worker, so separate workers cannot accidentally satisfy different gates.
func TestHealthRollSevenRungsLiveDB(t *testing.T) {
	for _, lane := range []string{"caps", "completion", "harness", "runtime", "codex completion", "custom", "job files"} {
		t.Run(lane, func(t *testing.T) {
			e := setupInterlockLiveDB(t)
			svc := e.permitService(t)
			svc.SetCapabilitySettings(fakeCapabilitySettings{on: true})
			var cv *int32
			v1 := int32(1)
			req := []string{}
			protocols := []string{}
			if lane == "caps" {
				req = []string{"gpu"}
			}
			if lane == "completion" || lane == "codex completion" {
				cv = &v1
				protocols = append(protocols, capability.CompletionInterlockV1)
			}
			runID := e.seedQueuedRun(t, cv, req)
			if lane == "harness" || lane == "runtime" || lane == "codex completion" || lane == "custom" {
				e.exec(t, "UPDATE runs SET harness='codex' WHERE id=$1", runID)
				protocols = append(protocols, capability.CodexHarnessV1, capability.CodexRuntimeV2)
			}
			if lane == "codex completion" {
				protocols = append(protocols, capability.CodexCompletionInterlockV1)
			}
			if lane == "custom" {
				e.exec(t, "UPDATE users SET default_codex_model='custom-fixture' WHERE id=$1", e.userID)
				protocols = append(protocols, capability.CodexCustomModelV1)
			}
			if lane == "job files" {
				e.exec(t, "UPDATE runs SET kind='job',job_type='research',repo_id=NULL,issue_iid=NULL,job_protocol=2 WHERE id=$1", runID)
				protocols = []string{capability.JobRunnerV1, capability.JobFilesV1}
			}
			w := e.seedHeartbeatWorker(t, protocols)
			now := time.Now().UTC().Truncate(time.Second)
			e.exec(t, "UPDATE workers SET capabilities=$2,draining_since=$3,max_concurrent_runs=1 WHERE id=$1", w, req, now.Add(-time.Hour))
			e.seedActiveRunOwnedBy(t, w) // saturation must not erase static suitability
			e.exec(t, "UPDATE runs SET created_at=$2,status_since=$2 WHERE id=$1", runID, now.Add(-time.Hour))
			row := e.healthRowFor(t, runID)
			if got := svc.queuedReason(e.ctx, now, row); got != ReasonWorkersUpgrading {
				t.Fatalf("queuedReason=%q", got)
			}
			eligibility, err := svc.WorkerEligibilityForHealth(e.ctx, now, runID)
			if err != nil || eligibility.Claimable != 0 || eligibility.DrainingEligible != 1 || eligibility.NonDrainingEligible != 0 || eligibility.SuitableOwnDraining != 0 || !eligibility.LatestSuitableDrainingSince.Valid || !eligibility.LatestSuitableDrainingSince.Time.Equal(now.Add(-time.Hour)) {
				t.Fatalf("eligibility=%+v err=%v", eligibility, err)
			}
			svc.healthSettings = defaultHealthSettings()
			svc.detectRunHealth(e.ctx, now)
			stored := e.healthRowFor(t, runID)
			if stored.Health != healthWaitingWorker || stored.HealthReason.String != ReasonWorkersUpgrading {
				t.Fatalf("stored reason=%+v", stored)
			}
			owners, err := e.q.ListOwnersWaitingNoCapacity(e.ctx, store.ListOwnersWaitingNoCapacityParams{HeartbeatCutoff: pgtype.Timestamptz{Time: now.Add(-45 * time.Second), Valid: true}, RollReason: ReasonWorkersUpgrading})
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, r := range owners {
				if r.RunID == runID {
					found = r.HasRollReason
				}
			}
			if !found {
				t.Fatal("stored roll reason absent from owner query")
			}
			// The reason remains stored, but a lost heartbeat invalidates confirmation.
			e.exec(t, "UPDATE workers SET last_heartbeat_at=$2 WHERE id=$1", w, now.Add(-time.Hour))
			stale, err := svc.WorkerEligibilityForHealth(e.ctx, now, runID)
			if err != nil || stale.DrainingEligible != 0 || stale.LatestSuitableDrainingSince.Valid {
				t.Fatalf("stale eligibility=%+v err=%v", stale, err)
			}
			if got := svc.queuedReason(e.ctx, now, row); got == ReasonWorkersUpgrading {
				t.Fatal("stale drainer retained upgrade diagnosis")
			}
			// Restore freshness, then remove precisely the capability under test.
			e.exec(t, "UPDATE workers SET last_heartbeat_at=$2 WHERE id=$1", w, now)
			missing := ""
			switch lane {
			case "caps":
				e.exec(t, "UPDATE workers SET capabilities='{}' WHERE id=$1", w)
			case "completion":
				missing = capability.CompletionInterlockV1
			case "harness":
				missing = capability.CodexHarnessV1
			case "runtime":
				missing = capability.CodexRuntimeV2
			case "codex completion":
				missing = capability.CodexCompletionInterlockV1
			case "custom":
				missing = capability.CodexCustomModelV1
			case "job files":
				missing = capability.JobFilesV1
			}
			if missing != "" {
				e.exec(t, "UPDATE workers SET protocol_capabilities=array_remove(protocol_capabilities,$2) WHERE id=$1", w, missing)
			}
			incomplete, err := svc.WorkerEligibilityForHealth(e.ctx, now, runID)
			if err != nil || incomplete.DrainingEligible != 0 || incomplete.LatestSuitableDrainingSince.Valid {
				t.Fatalf("missing gate=%+v err=%v", incomplete, err)
			}
			if got := svc.queuedReason(e.ctx, now, row); got == ReasonWorkersUpgrading {
				t.Fatalf("missing %s falsely diagnosed roll", lane)
			}
			if lane == "job files" {
				e.exec(t, "UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", w, []string{capability.JobFilesV1})
				incomplete, err = svc.WorkerEligibilityForHealth(e.ctx, now, runID)
				if err != nil || incomplete.DrainingEligible != 0 {
					t.Fatalf("missing job runner=%+v err=%v", incomplete, err)
				}
			}
		})
	}
}

func TestHealthRollWholeWorkerNegativesLiveDB(t *testing.T) {
	for _, name := range []string{"split gates", "missing caps", "missing protocol", "missing runtime", "allowlist", "released incarnation", "bound elsewhere", "offline", "stale", "own one peer", "own several peers", "unsuitable own"} {
		t.Run(name, func(t *testing.T) {
			e := setupInterlockLiveDB(t)
			svc := e.permitService(t)
			svc.SetCapabilitySettings(fakeCapabilitySettings{on: true})
			v1 := int32(1)
			runID := e.seedQueuedRun(t, &v1, []string{"gpu"})
			e.exec(t, "UPDATE runs SET harness='codex',updated_at=now() - interval '3 hours' WHERE id=$1", runID)
			caps := []string{capability.CompletionInterlockV1, capability.CodexHarnessV1, capability.CodexRuntimeV2, capability.CodexCompletionInterlockV1}
			w := e.seedHeartbeatWorker(t, caps)
			e.exec(t, "UPDATE workers SET capabilities='{gpu}',draining_since=now()-interval '1 hour' WHERE id=$1", w)
			wantRoll := false
			switch name {
			case "split gates":
				e.exec(t, "UPDATE workers SET protocol_capabilities='{}' WHERE id=$1", w)
				peer := e.seedHeartbeatWorker(t, caps)
				e.exec(t, "UPDATE workers SET draining_since=now() WHERE id=$1", peer)
			case "missing caps":
				e.exec(t, "UPDATE workers SET capabilities='{}' WHERE id=$1", w)
			case "missing protocol":
				e.exec(t, "UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", w, caps[1:])
			case "missing runtime":
				e.exec(t, "UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", w, []string{capability.CompletionInterlockV1, capability.CodexHarnessV1, capability.CodexCompletionInterlockV1})
			case "allowlist":
				e.exec(t, "UPDATE workers SET docker_enabled=true WHERE id=$1", w)
			case "released incarnation":
				nonce := uuid.New().String()
				e.exec(t, "UPDATE workers SET snapshot_register_nonce=$2 WHERE id=$1", w, nonce)
				e.exec(t, "UPDATE runs SET released_worker_id=$2,released_worker_nonce=$3 WHERE id=$1", runID, w, nonce)
			case "bound elsewhere":
				bound := e.seedQueuedRun(t, nil, nil)
				e.exec(t, "UPDATE workers SET ephemeral=true,ephemeral_run_id=$2 WHERE id=$1", w, bound)
			case "offline":
				e.exec(t, "UPDATE workers SET status='offline' WHERE id=$1", w)
			case "stale":
				e.exec(t, "UPDATE workers SET last_heartbeat_at=now()-interval '1 hour' WHERE id=$1", w)
			case "own one peer", "own several peers", "unsuitable own":
				e.exec(t, "UPDATE runs SET worker_id=$2 WHERE id=$1", runID, w)
				if name == "unsuitable own" {
					e.exec(t, "UPDATE workers SET capabilities='{}' WHERE id=$1", w)
					wantRoll = true
				}
				count := 1
				if name == "own several peers" {
					count = 3
				}
				for i := 0; i < count; i++ {
					peer := e.seedHeartbeatWorker(t, caps)
					e.exec(t, "UPDATE workers SET capabilities='{gpu}',draining_since=now() WHERE id=$1", peer)
				}
			}
			now := time.Now()
			got := svc.queuedReason(e.ctx, now, e.healthRowFor(t, runID))
			if (got == ReasonWorkersUpgrading) != wantRoll {
				t.Fatalf("reason=%q want roll=%v", got, wantRoll)
			}
			row, err := svc.WorkerEligibilityForHealth(e.ctx, now, runID)
			if err != nil {
				t.Fatal(err)
			}
			if name == "own one peer" || name == "own several peers" {
				if row.SuitableOwnDraining != 1 || row.DrainingEligible < 1 {
					t.Fatalf("own veto fields=%+v", row)
				}
			} else if wantRoll {
				if row.SuitableOwnDraining != 0 || row.DrainingEligible != 1 {
					t.Fatalf("unsuitable own=%+v", row)
				}
			} else if row.DrainingEligible != 0 {
				t.Fatalf("unsuitable drainer counted=%+v", row)
			}
			if name == "released incarnation" {
				e.exec(t, "UPDATE workers SET snapshot_register_nonce=$2 WHERE id=$1", w, uuid.New().String())
				changed, err := svc.WorkerEligibilityForHealth(e.ctx, now, runID)
				if err != nil || changed.DrainingEligible != 1 {
					t.Fatalf("new incarnation still excluded=%+v err=%v", changed, err)
				}
			}
			if name == "allowlist" {
				svc.SetDockerAllowlist(fakeAllowlistReader{ids: []uuid.UUID{e.repoID}})
				allowed, err := svc.WorkerEligibilityForHealth(e.ctx, now, runID)
				if err != nil || allowed.DrainingEligible != 1 {
					t.Fatalf("allowlisted drainer=%+v err=%v", allowed, err)
				}
			}
		})
	}
}

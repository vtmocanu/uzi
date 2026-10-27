package workersvc

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestRetainedParkLateExecutionFencesLiveDB(t *testing.T) {
	for _, timed := range []bool{false, true} {
		for _, endPark := range []string{"parked", "timer", "owner"} {
			if !timed && endPark == "timer" {
				continue
			}
			t.Run(fmt.Sprintf("timed=%v/%s", timed, endPark), func(t *testing.T) {
				e := setupInterlockLiveDB(t)
				svc := e.diskParkService(t, 3)
				svc.p.RunRetainedRecoveryMaxParks = 0
				if timed {
					svc.p.RunRetainedRecoveryMaxParks = 3
				}
				w := e.seedWorker(t, nil)
				id := e.fpSeedRunning(t, w, 1)
				if _, ok, err := svc.SetState(e.ctx, store.Worker{ID: w}, id, retainedReport(1)); err != nil || !ok {
					t.Fatalf("park: %v %v", ok, err)
				}
				if endPark == "timer" {
					e.exec(t, "UPDATE runs SET recovery_retry_not_before=now()-interval '1 second' WHERE id=$1", id)
					if _, err := e.q.PromoteRecoveryWaitRuns(e.ctx, pgconv.Time(time.Now())); err != nil {
						t.Fatal(err)
					}
				}
				if endPark == "owner" {
					if _, err := e.q.ResumeWorkerRecoveryEpisode(e.ctx, store.ResumeWorkerRecoveryEpisodeParams{ID: id, UserID: e.userID, GlobalTimeoutSeconds: 3600}); err != nil {
						t.Fatal(err)
					}
				}
				snapshot := func() string {
					t.Helper()
					r, err := e.q.GetRunByID(e.ctx, id)
					if err != nil {
						t.Fatal(err)
					}
					b, err := json.Marshal(r)
					if err != nil {
						t.Fatal(err)
					}
					return string(b)
				}
				before := snapshot()
				// Both generation-bearing and legacy reports must honor execution release.
				for _, gen := range []*int64{i64Ptr(1), nil} {
					for _, state := range []string{"running", "awaiting_approval", "awaiting_input", "awaiting_followup", "paused", "failed", "completed", "limit_wait", "recovery_wait", "credential_switch", "credential_switch_failed"} {
						req := StateRequest{State: state, ClaimGeneration: gen, PlanMd: strPtr("fixture plan"), OpenQuestionID: strPtr("question"), OpenFollowupID: i64Ptr(1)}
						_, ok, err := svc.SetState(e.ctx, store.Worker{ID: w}, id, req)
						// Credential release acknowledges an already released generation
						// idempotently. Its ACK is not a write; the full-row comparison
						// below still requires every field to remain unchanged.
						if ok && (state != "credential_switch" || gen == nil) {
							t.Fatalf("late %s applied", state)
						}
						if err != nil && !errors.Is(err, ErrStaleClaim) && !errors.Is(err, ErrMissingClaimGeneration) && !errors.Is(err, ErrInvalidState) {
							t.Fatalf("late %s: %v", state, err)
						}
						if snapshot() != before {
							t.Fatalf("late %s changed run", state)
						}
					}
				}
				n, err := e.q.UpsertWorkerActiveRun(e.ctx, store.UpsertWorkerActiveRunParams{WorkerID: w, RunID: id, ClaimGeneration: 1, Phase: "running", SnapshotEpoch: 1, LeaseSeconds: 60})
				if err != nil || n != 0 {
					t.Fatalf("released snapshot upsert=%d %v", n, err)
				}
				// A snapshot persisted before the park also cannot readopt during either queued interval.
				e.exec(t, "INSERT INTO worker_active_runs(worker_id,run_id,claim_generation,phase,snapshot_epoch,reported_at) VALUES($1,$2,1,'running',1,now())", w, id)
				rows, err := e.q.ReadoptRunsFromSnapshot(e.ctx, w)
				if err != nil || len(rows) != 0 {
					t.Fatalf("released readoption=%v %v", rows, err)
				}
				if snapshot() != before {
					t.Fatal("snapshot revived released run")
				}
				// Interlocked completion has its own transaction, outside the generic fence.
				e.exec(t, "UPDATE runs SET completion_contract_version=1,contract_revision=1 WHERE id=$1", id)
				before = snapshot()
				_, ok, err := svc.SetState(e.ctx, store.Worker{ID: w}, id, StateRequest{State: "completed", ClaimGeneration: i64Ptr(1), Head: strPtr("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")})
				if ok || !errors.Is(err, ErrStaleClaim) {
					t.Fatalf("interlocked late completion=%v %v", ok, err)
				}
				if snapshot() != before {
					t.Fatal("interlocked completion changed released run")
				}
			})
		}
	}
}

func TestRetainedParkOwnerCancelLiveDB(t *testing.T) {
	for _, cap := range []int{0, 3} {
		t.Run(fmt.Sprint(cap), func(t *testing.T) {
			e := setupInterlockLiveDB(t)
			svc := e.diskParkService(t, 3)
			svc.p.RunRetainedRecoveryMaxParks = cap
			w := e.seedWorker(t, nil)
			id := e.fpSeedRunning(t, w, 1)
			if _, ok, err := svc.SetState(e.ctx, store.Worker{ID: w}, id, retainedReport(1)); err != nil || !ok {
				t.Fatalf("park=%v %v", ok, err)
			}
			if _, err := e.q.CancelRunServerSide(e.ctx, store.CancelRunServerSideParams{ID: id, UserID: e.userID}); err != nil {
				t.Fatal(err)
			}
			r, err := e.q.GetRunByID(e.ctx, id)
			if err != nil || r.Status != "cancelled" {
				t.Fatalf("cancel status=%s %v", r.Status, err)
			}
		})
	}
}

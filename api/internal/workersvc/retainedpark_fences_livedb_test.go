package workersvc

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestRetainedParkLateExecutionFencesLiveDB(t *testing.T) {
	for _, cause := range []string{"retained_recovery_exhausted", "retained_recovery_adoption_failed"} {
		t.Run(cause, func(t *testing.T) {
			testRetainedParkLateExecutionFencesLiveDB(t, cause)
		})
	}
}

func testRetainedParkLateExecutionFencesLiveDB(t *testing.T, cause string) {
	for _, timed := range []bool{false, true} {
		for _, endPark := range []string{"parked", "timer", "owner"} {
			if endPark == "timer" && (!timed || cause == recoveryCauseRetainedRecoveryAdoptionFailed) {
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
				if _, ok, err := svc.SetState(e.ctx, store.Worker{ID: w}, id, retainedReportCause(1, cause)); err != nil || !ok {
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
	for _, cause := range []string{"retained_recovery_exhausted", "retained_recovery_adoption_failed"} {
		for _, cap := range []int{0, 3} {
			t.Run(fmt.Sprintf("%s/cap=%d", cause, cap), func(t *testing.T) {
				e := setupInterlockLiveDB(t)
				svc := e.diskParkService(t, 3)
				svc.p.RunRetainedRecoveryMaxParks = cap
				w := e.seedWorker(t, nil)
				// The retained worker stays live for its other runs after this flight parks.
				e.exec(t, "UPDATE workers SET last_heartbeat_at=now() WHERE id=$1", w)
				id := e.fpSeedRunning(t, w, 1)
				hold := mhOpenHold(t, e, id, 1, w)
				capture := uuid.New()
				const source = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
				e.exec(t, "INSERT INTO recovery_captures(id,hold_id,run_id,user_id,original_worker_identity,source_sha,idempotency_key,state,expires_at) VALUES($1,$2,$3,$4,'retained-cancel',$5,'retained-cancel','available',now()+interval '1 hour')", capture, hold, id, e.userID, source)
				e.exec(t, "UPDATE runs SET worker_recovery_episode=3, requeue_count=7, requeue_episode_baseline=5, recovery_wait_count=9, session_id='retained-session', checkpoint_tip=$2 WHERE id=$1", id, source)

				readRun := func() store.Run {
					t.Helper()
					r, err := e.q.GetRunByID(e.ctx, id)
					if err != nil {
						t.Fatal(err)
					}
					return r
				}
				runSnapshot := func() string {
					t.Helper()
					b, err := json.Marshal(readRun())
					if err != nil {
						t.Fatal(err)
					}
					return string(b)
				}
				custodySnapshot := func() string {
					t.Helper()
					var snapshot string
					if err := e.pool.QueryRow(e.ctx, `SELECT row_to_json(h)::text || '|' || row_to_json(c)::text
					    FROM recovery_custody_holds h JOIN recovery_captures c ON c.hold_id=h.id
					    WHERE h.id=$1 AND c.id=$2`, hold, capture).Scan(&snapshot); err != nil {
						t.Fatal(err)
					}
					return snapshot
				}
				assertCustody := func(want string) {
					t.Helper()
					if state, evidence := e.fpHoldEvidence(t, hold); state != "open" || evidence != nil {
						t.Fatalf("custody state=%s evidence=%v", state, evidence)
					}
					if custodySnapshot() != want {
						t.Fatal("cancel or stale report changed custody/source evidence")
					}
					var counted int64
					if err := e.pool.QueryRow(e.ctx, "SELECT fn_custody_admission_count($1, now()-interval '1 minute')", e.userID).Scan(&counted); err != nil || counted != 1 {
						t.Fatalf("counted custody=%d err=%v", counted, err)
					}
				}
				sourceBefore := custodySnapshot()
				if live, err := svc.hasLivePoller(e.ctx, readRun()); err != nil || !live {
					t.Fatalf("fixture must have a live poller before parking: live=%v err=%v", live, err)
				}
				if _, ok, err := svc.SetState(e.ctx, store.Worker{ID: w}, id, retainedReportCause(1, cause)); err != nil || !ok {
					t.Fatalf("park=%v %v", ok, err)
				}
				parked := readRun()
				wantParks := int32(0)
				timed := cause == "retained_recovery_exhausted" && cap > 0
				if timed {
					wantParks = 1
				}
				if parked.Status != "recovery_wait" || !parked.RecoveryWaitCause.Valid || parked.RecoveryWaitCause.String != cause ||
					!parked.ClaimReleasedAt.Valid || parked.FinishedAt.Valid || parked.RecoveryRetryNotBefore.Valid != timed ||
					parked.RetainedParkCount != wantParks || parked.RecoveryWaitCount != 10 {
					t.Fatalf("unexpected retained park: %+v", parked)
				}
				assertCustody(sourceBefore)
				before := runSnapshot()
				if _, err := svc.SubmitInput(e.ctx, uuid.New(), id, "cancel", "foreign cancel", nil); !errors.Is(err, ErrRunNotFound) {
					t.Fatalf("foreign cancel err=%v, want ErrRunNotFound", err)
				}
				if runSnapshot() != before {
					t.Fatal("foreign cancel changed run")
				}
				assertCustody(sourceBefore)

				const reason = "owner cancels retained recovery"
				res, err := svc.SubmitInput(e.ctx, e.userID, id, "cancel", reason, nil)
				if err != nil || !res.ServerSide {
					t.Fatalf("owner cancel server-side=%v err=%v", res.ServerSide, err)
				}
				cancelled := readRun()
				if cancelled.Status != "cancelled" || !cancelled.FinishedAt.Valid ||
					!cancelled.StopKind.Valid || cancelled.StopKind.String != "cancelled" ||
					!cancelled.StopReason.Valid || cancelled.StopReason.String != reason {
					t.Fatalf("cancel disposition: %+v", cancelled)
				}
				// CancelRunServerSide preserves recovery cause and allowance accounting.
				if cancelled.RecoveryWaitCause != parked.RecoveryWaitCause || cancelled.RecoveryRetryNotBefore != parked.RecoveryRetryNotBefore ||
					cancelled.WorkerRecoveryEpisode != parked.WorkerRecoveryEpisode || cancelled.RequeueCount != parked.RequeueCount ||
					cancelled.RequeueEpisodeBaseline != parked.RequeueEpisodeBaseline || cancelled.RetainedParkCount != parked.RetainedParkCount ||
					cancelled.RecoveryWaitCount != parked.RecoveryWaitCount || cancelled.ClaimGeneration != parked.ClaimGeneration ||
					cancelled.ClaimReleasedAt != parked.ClaimReleasedAt || cancelled.WorkerID != parked.WorkerID ||
					cancelled.SessionID != parked.SessionID || cancelled.CheckpointTip != parked.CheckpointTip {
					t.Fatal("cancel changed recovery accounting, execution fence or source tip")
				}
				assertCustody(sourceBefore)
				assertNoInputs := func() {
					t.Helper()
					var n int
					if err := e.pool.QueryRow(e.ctx, "SELECT count(*) FROM run_user_inputs WHERE run_id=$1", id).Scan(&n); err != nil || n != 0 {
						t.Fatalf("cancel inputs=%d err=%v, want no enqueued input", n, err)
					}
				}
				assertNoInputs()
				before = runSnapshot()
				if _, err := svc.SubmitInput(e.ctx, e.userID, id, "cancel", reason, nil); !errors.Is(err, ErrRunTerminal) {
					t.Fatalf("repeat cancel err=%v, want ErrRunTerminal", err)
				}
				if _, ok, err := svc.SetState(e.ctx, store.Worker{ID: w}, id, retainedReportCause(1, cause)); ok || !errors.Is(err, ErrStaleClaim) {
					t.Fatalf("stale retained report applied=%v err=%v", ok, err)
				}
				if runSnapshot() != before {
					t.Fatal("repeat cancel or stale report changed cancelled run")
				}
				assertCustody(sourceBefore)
				assertNoInputs()
			})
		}
	}
}

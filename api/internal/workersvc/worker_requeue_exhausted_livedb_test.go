package workersvc

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// These exercise existing SQL writers and service transactions. Recovery evidence
// is recorded on the server; no worker snapshot or forge lookup supplies it.
func TestWorkerRequeueExhaustedDispositionLiveDB(t *testing.T) {
	for _, path := range []string{"stale", "registration", "attested", "missing", "frozen_registration", "frozen_attested", "frozen_missing"} {
		t.Run(path, func(t *testing.T) {
			// The policy matrix is shared; other callers pin their lock/write seams.
			evidenceCases := []string{"empty", "checkpoint", "open_custody"}
			if path == "registration" {
				evidenceCases = append(evidenceCases, "available", "both", "preparing", "uploading", "needs_action", "unknown_capture", "expired", "discarded", "expired_open_custody", "publication_attempt", "invalid_checkpoint", "sql_read_error")
			}
			if path == "frozen_registration" {
				evidenceCases = append(evidenceCases, "available", "publication_attempt", "sql_read_error")
			}
			for _, evidence := range evidenceCases {
				t.Run(evidence, func(t *testing.T) {
					env := setupCodexLiveDB(t)
					user, _, repo := env.seedCodexInfra(t)
					worker := seedSnapshotWorker(t, env, user, "exhaustion-nonce")
					run := seedOutageRun(t, env, user, repo, worker, "running", "issue", 2, 2)
					// The one-shot finalize allowance was already consumed at generation 1.
					env.exec("UPDATE runs SET finalize_resume_generation=1 WHERE id=$1", run)
					tip := strings.Repeat("a", 40)
					switch evidence {
					case "checkpoint", "both":
						env.exec("UPDATE runs SET checkpoint_tip=$2,checkpoint_tip_at=now() WHERE id=$1", run, tip)
					case "open_custody":
						// An older unresolved generation still protects source custody.
						env.exec(`INSERT INTO recovery_custody_holds
       (user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity)
       VALUES ($1,$2,$3,1,'open',$4,'exhaustion-fixture')`, user, repo, run, worker)
					}
					if evidence == "invalid_checkpoint" {
						env.exec("UPDATE runs SET checkpoint_tip='invalid' WHERE id=$1", run)
					}
					if evidence == "publication_attempt" {
						env.exec("INSERT INTO checkpoint_publish_attempts(run_id,branch,ref,tip) VALUES($1,'branch','ref',$2)", run, strings.Repeat("b", 40))
					}
					captureStates := map[string]string{"available": "available", "both": "available", "preparing": "preparing", "uploading": "uploading", "needs_action": "needs_action", "unknown_capture": "unknown", "expired": "expired", "discarded": "discarded", "expired_open_custody": "expired"}
					if state, ok := captureStates[evidence]; ok {
						hold := uuid.New()
						holdState := "released"
						if evidence == "expired_open_custody" {
							holdState = "open"
						}
						env.exec(`INSERT INTO recovery_custody_holds(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity)
                            VALUES($1,$2,$3,$4,1,$5,$6,'fixture')`, hold, user, repo, run, holdState, worker)
						env.exec(`INSERT INTO recovery_captures(hold_id,run_id,user_id,original_worker_identity,source_sha,idempotency_key,state)
                            VALUES($1,$2,$3,'fixture',$4,'fixture',$5)`, hold, run, user, tip, state)
					}
					if evidence == "sql_read_error" {
						env.exec("ALTER TABLE checkpoint_publish_attempts RENAME TO exhaustion_read_error_attempts")
						defer env.exec("ALTER TABLE exhaustion_read_error_attempts RENAME TO checkpoint_publish_attempts")
					}
					now := time.Now()
					cutoff := pgtype.Timestamptz{Time: now.Add(-time.Minute), Valid: true}
					workerID := pgtype.UUID{Bytes: worker, Valid: true}
					reason := pgtype.Text{String: "worker requeue budget exhausted", Valid: true}
					var err error
					var changed []uuid.UUID
					svc := snapshotSvc(env, testParams())
					spy := &stateSpy{}
					svc.SetBroadcaster(spy)
					switch path {
					case "stale":
						env.exec("UPDATE workers SET last_heartbeat_at=now()-interval '10 minutes' WHERE id=$1", worker)
						var rows []store.FailRunsOfStaleWorkersOverCapRow
						rows, err = env.q.FailRunsOfStaleWorkersOverCap(env.ctx, store.FailRunsOfStaleWorkersOverCapParams{FailureReason: reason, MaxRequeues: 1, FailCutoff: cutoff})
						for _, row := range rows {
							changed = append(changed, row.ID)
						}
					case "registration":
						var rows []store.WorkerRecoveryDisposition
						rows, err = env.q.FailWorkerRunsOverCap(env.ctx, store.FailWorkerRunsOverCapParams{FailureReason: reason, WorkerID: workerID, MaxRequeues: 1})
						for _, row := range rows {
							changed = append(changed, row.ID)
						}
					case "attested":
						var rows []store.WorkerRecoveryDisposition
						rows, err = env.q.FailAttestedFinalizeRunsOverCap(env.ctx, store.FailAttestedFinalizeRunsOverCapParams{FailureReason: reason, WorkerID: workerID, MaxRequeues: 1, RunIds: []uuid.UUID{run}, ClaimGenerations: []int64{2}})
						for _, row := range rows {
							changed = append(changed, row.ID)
						}
					case "missing":
						var rows []store.FailRunsMissingFromSnapshotRow
						rows, err = env.q.FailRunsMissingFromSnapshot(env.ctx, store.FailRunsMissingFromSnapshotParams{FailureReason: reason, WorkerID: workerID, MaxRequeues: 1, MissingCutoff: cutoff, Now: pgtype.Timestamptz{Time: now, Valid: true}, GlobalTimeoutSeconds: 86400})
						for _, row := range rows {
							changed = append(changed, row.ID)
						}
					case "frozen_registration":
						_, _, err = svc.Register(env.ctx, hbWorker(t, env, worker), "fixture", "", nil, nil, nil, nil)
					case "frozen_attested":
						_, _, err = svc.Register(env.ctx, hbWorker(t, env, worker), "fixture", "", nil, nil, nil, finalizeSnap(false, fin(run, 2)))
					case "frozen_missing":
						_, err = svc.Heartbeat(env.ctx, hbWorker(t, env, worker), nil, nil, &ActiveSnapshot{SnapshotEpoch: 1, RegisterNonce: "exhaustion-nonce", Active: []ActiveRunEntry{}})
					}
					if err != nil {
						t.Fatalf("exhaustion transition: %v", err)
					}
					if !strings.HasPrefix(path, "frozen_") {
						found := false
						for _, id := range changed {
							if id == run {
								found = true
							}
						}
						if !found {
							t.Fatalf("writer did not return exhausted run %s: %v", run, changed)
						}
					}
					wantStatus, wantCause, wantOrigin := "recovery_wait", "worker_requeue_exhausted", ""
					if evidence == "empty" || evidence == "expired" || evidence == "discarded" {
						wantStatus, wantCause, wantOrigin = "failed", "", "worker_lost"
					}
					var status, cause, origin string
					var finished bool
					var persistedTip pgtype.Text
					var requeues int32
					if err := env.pool.QueryRow(env.ctx, `SELECT status,COALESCE(recovery_wait_cause,''),COALESCE(fail_origin,''),finished_at IS NOT NULL,checkpoint_tip,requeue_count FROM runs WHERE id=$1`, run).Scan(&status, &cause, &origin, &finished, &persistedTip, &requeues); err != nil {
						t.Fatal(err)
					}
					if status != wantStatus || cause != wantCause || origin != wantOrigin || finished != (wantStatus == "failed") {
						t.Errorf("exhaustion disposition = status=%q cause=%q fail_origin=%q finished=%v; want status=%q cause=%q fail_origin=%q finished=%v", status, cause, origin, finished, wantStatus, wantCause, wantOrigin, wantStatus == "failed")
					}
					var released bool
					var capRevoked bool
					var retryCleared bool
					var observed store.WorkerRecoveryEvidence
					var raw []byte
					if err := env.pool.QueryRow(env.ctx, "SELECT claim_released_at IS NOT NULL,codex_cap_hash IS NULL,recovery_retry_not_before IS NULL,worker_recovery_evidence FROM runs WHERE id=$1", run).Scan(&released, &capRevoked, &retryCleared, &raw); err != nil {
						t.Fatal(err)
					}
					if err := json.Unmarshal(raw, &observed); err != nil {
						t.Fatal(err)
					}
					if wantStatus == "recovery_wait" && (!released || !capRevoked || !retryCleared) {
						t.Fatalf("park fences: released=%v revoked=%v retry cleared=%v", released, capRevoked, retryCleared)
					}
					if observed.Unknown != (evidence == "sql_read_error" || evidence == "invalid_checkpoint" || evidence == "unknown_capture") {
						t.Fatalf("unknown evidence=%v for %s", observed.Unknown, evidence)
					}
					if requeues != 2 {
						t.Errorf("exhaustion spent more requeues: %d, want 2", requeues)
					}
					if (evidence == "checkpoint" || evidence == "both") && (!persistedTip.Valid || persistedTip.String != tip) {
						t.Errorf("persisted checkpoint lost: %v", persistedTip)
					}
					if evidence == "open_custody" {
						var state string
						if err := env.pool.QueryRow(env.ctx, "SELECT state FROM recovery_custody_holds WHERE run_id=$1 AND generation=1", run).Scan(&state); err != nil {
							t.Fatal(err)
						}
						if state != "open" {
							t.Errorf("unresolved custody changed to %q", state)
						}
					}
					if strings.HasPrefix(path, "frozen_") {
						if got, ok := spy.statusFor(run); !ok || got != wantStatus {
							t.Errorf("committed exhaustion broadcast=(%q,%v), want (%q,true)", got, ok, wantStatus)
						}
					}
				})
			}
		})
	}
}

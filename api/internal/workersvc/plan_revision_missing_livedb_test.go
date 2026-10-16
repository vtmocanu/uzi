package workersvc

import (
	"strings"
	"testing"
)

func TestHeartbeatOmittedHeldRevisionLiveDB(t *testing.T) {
	for _, phase := range []string{"awaiting_approval", "awaiting_input", "awaiting_followup"} {
		t.Run(phase, func(t *testing.T) {
			env := setupCodexLiveDB(t)
			user, _, repo := env.seedCodexInfra(t)
			for _, scenario := range []string{"omitted", "listed", "wrong generation", "no snapshot", "wrong nonce", "old epoch", "fresh fence", "released", "terminal lease", "overflow", "cap evidence", "cap no evidence", "cancel cap evidence", "cancel cap no evidence"} {
				t.Run(scenario, func(t *testing.T) {
					worker := seedSnapshotWorker(t, env, user, "m1-held")
					count := int32(0)
					if strings.Contains(scenario, "cap") {
						count = 2
					}
					run := seedOutageRun(t, env, user, repo, worker, phase, "issue", 2, count)
					// Approval/input waiting exceeds the wall budget, but consumed execution does not.
					// Followup deliberately retains its existing elapsed-time distinction.
					if phase != "awaiting_followup" {
						env.exec("UPDATE runs SET budget_wall_seconds=60 WHERE id=$1", run)
					}
					env.exec("UPDATE runs SET codex_cap_hash='held-cap', codex_claim_epoch=7 WHERE id=$1", run)
					snapshot := &ActiveSnapshot{SnapshotEpoch: 1, RegisterNonce: "m1-held", Active: []ActiveRunEntry{}}
					want := "queued"
					evidence := scenario == "cap evidence" || scenario == "cancel cap evidence"
					if evidence {
						env.exec("UPDATE runs SET checkpoint_tip=$2 WHERE id=$1", run, strings.Repeat("c", 40))
					}
					switch scenario {
					case "listed":
						snapshot.Active = []ActiveRunEntry{entry(run, 2, phase, false)}
						want = phase
					case "wrong generation":
						snapshot.Active = []ActiveRunEntry{entry(run, 1, phase, false)}
					case "no snapshot":
						snapshot = nil
						want = phase
					case "wrong nonce":
						snapshot.RegisterNonce = "retired"
						want = phase
					case "old epoch":
						snapshot.SnapshotEpoch = 0
						want = phase
					case "fresh fence":
						env.exec("UPDATE runs SET status_since=now() WHERE id=$1", run)
						want = phase
					case "released":
						env.exec("UPDATE runs SET claim_released_at=now() WHERE id=$1", run)
						want = phase
					case "terminal lease":
						snapshot.Active = []ActiveRunEntry{entry(run, 2, phase, true)}
						want = phase
					case "overflow":
						snapshot.PendingOverflow = true
						want = phase
					case "cap evidence":
						want = "recovery_wait"
					case "cap no evidence":
						want = "failed"
					case "cancel cap evidence", "cancel cap no evidence":
						env.exec("UPDATE runs SET stop_kind='cancelled', stop_reason='owner reason' WHERE id=$1", run)
						env.exec("INSERT INTO run_user_inputs(run_id,kind,body) VALUES($1,'cancel','owner reason')", run)
						want = "cancelled"
					}
					svc := snapshotSvc(env, testParams())
					spy := &stateSpy{}
					svc.SetBroadcaster(spy)
					if _, err := svc.Heartbeat(env.ctx, hbWorker(t, env, worker), nil, nil, snapshot); err != nil {
						t.Fatal(err)
					}
					got := exhaustionRun(t, env, run)
					if got.Status != want || got.ClaimGeneration != 2 {
						t.Fatalf("heartbeat: status=%s want=%s generation=%d", got.Status, want, got.ClaimGeneration)
					}
					if want == "queued" || want == "recovery_wait" {
						paused := budgetPausedOf(t, env, run)
						if phase == "awaiting_followup" {
							if paused != 0 {
								t.Fatalf("followup bank=%d", paused)
							}
						} else if paused < 10800 || paused > 10830 {
							t.Fatalf("held interval bank=%d", paused)
						}
						before := paused
						snapshot.SnapshotEpoch++
						if _, err := svc.Heartbeat(env.ctx, hbWorker(t, env, worker), nil, nil, snapshot); err != nil {
							t.Fatal(err)
						}
						if after := budgetPausedOf(t, env, run); after != before {
							t.Fatalf("interval banked twice: before=%d after=%d", before, after)
						}
					}
					switch want {
					case "queued":
						if got.RequeueCount != 1 || got.StaleRequeueGeneration.Valid || len(got.CodexCapHash) != 0 || got.CodexClaimEpoch != 8 {
							t.Fatalf("lost execution accounting/cap: %+v", got)
						}
					case "cancelled":
						var settled bool
						if err := env.pool.QueryRow(env.ctx, "SELECT bool_and(applied_at IS NOT NULL) FROM run_user_inputs WHERE run_id=$1 AND kind='cancel'", run).Scan(&settled); err != nil || !settled {
							t.Fatalf("cancel receipt=%v err=%v", settled, err)
						}
						if got.StopReason.String != "owner reason" || got.RequeueCount != 2 || got.ClaimReleasedAt.Valid || got.FailOrigin.Valid {
							t.Fatalf("cancel changed reason/claim/budget: %+v", got)
						}
						if published, ok := spy.statusFor(run); !ok || published != "cancelled" {
							t.Fatalf("cancel publication=%s %v", published, ok)
						}
					case "recovery_wait":
						if got.RecoveryWaitCause.String != "worker_requeue_exhausted" || !got.ClaimReleasedAt.Valid || got.RequeueCount != 2 {
							t.Fatalf("exhaustion: %+v", got)
						}
					}
				})
			}
		})
	}
}

func TestHeartbeatOmittedQueuedCancelLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	user, _, repo := env.seedCodexInfra(t)
	for _, scenario := range []string{"under cap", "cap evidence", "cap no evidence"} {
		t.Run(scenario, func(t *testing.T) {
			worker := seedSnapshotWorker(t, env, user, "m1-real-cancel")
			count := int32(0)
			if scenario != "under cap" {
				count = 2
			}
			run := seedOutageRun(t, env, user, repo, worker, "awaiting_approval", "issue", 2, count)
			if scenario == "cap evidence" {
				env.exec("UPDATE runs SET checkpoint_tip=$2 WHERE id=$1", run, strings.Repeat("d", 40))
			}
			svc := snapshotSvc(env, testParams())
			result, err := svc.SubmitInput(env.ctx, user, run, "cancel", "owner queued reason", nil)
			if err != nil || result.ServerSide {
				t.Fatalf("cancel must enqueue for fresh worker: %+v %v", result, err)
			}
			held := exhaustionRun(t, env, run)
			if held.Status != "awaiting_approval" || held.StopKind.String != "cancelled" {
				t.Fatalf("cancel did not stay held: %+v", held)
			}
			var pending bool
			if err := env.pool.QueryRow(env.ctx, "SELECT count(*)=1 AND bool_and(applied_at IS NULL) FROM run_user_inputs WHERE run_id=$1 AND kind='cancel'", run).Scan(&pending); err != nil || !pending {
				t.Fatalf("queued receipt=%v err=%v", pending, err)
			}
			snap := &ActiveSnapshot{SnapshotEpoch: 1, RegisterNonce: "m1-real-cancel", Active: []ActiveRunEntry{}}
			for epoch := int64(1); epoch <= 2; epoch++ {
				snap.SnapshotEpoch = epoch
				if _, err := svc.Heartbeat(env.ctx, hbWorker(t, env, worker), nil, nil, snap); err != nil {
					t.Fatal(err)
				}
			}
			got := exhaustionRun(t, env, run)
			if got.Status != "cancelled" || got.StopReason.String != "owner queued reason" || got.ClaimGeneration != 2 || got.RequeueCount != count || got.FailOrigin.Valid || got.ClaimReleasedAt.Valid {
				t.Fatalf("omitted queued cancel: %+v", got)
			}
			var settled bool
			if err := env.pool.QueryRow(env.ctx, "SELECT count(*)=1 AND bool_and(applied_at IS NOT NULL AND consumed_at IS NOT NULL) FROM run_user_inputs WHERE run_id=$1 AND kind='cancel'", run).Scan(&settled); err != nil || !settled {
				t.Fatalf("settled receipt=%v err=%v", settled, err)
			}
		})
	}
}

func TestHeartbeatOmittedCancelledCheckerLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	user, _, repo := env.seedCodexInfra(t)
	worker := seedSnapshotWorker(t, env, user, "m1-checker-cancel")
	parent := seedOutageRun(t, env, user, repo, worker, "running", "issue", 1, 0)
	child := seedRecoveryCheck(t, env, user, repo, worker, parent)
	svc := snapshotSvc(env, testParams())
	result, err := svc.SubmitInput(env.ctx, user, child, "cancel", "cancel checker", nil)
	if err != nil || result.ServerSide {
		t.Fatalf("checker cancel enqueue: %+v %v", result, err)
	}
	snap := &ActiveSnapshot{SnapshotEpoch: 1, RegisterNonce: "m1-checker-cancel", Active: []ActiveRunEntry{entry(parent, 1, "running", false)}}
	if _, err := svc.Heartbeat(env.ctx, hbWorker(t, env, worker), nil, nil, snap); err != nil {
		t.Fatal(err)
	}
	if got := exhaustionRun(t, env, child); got.Status != "cancelled" || got.StopReason.String != "cancel checker" || got.FailOrigin.Valid {
		t.Fatalf("checker cancel: %+v", got)
	}
	if got := exhaustionRun(t, env, parent); got.Status != "running" {
		t.Fatalf("listed parent changed: %+v", got)
	}
}

func TestHeartbeatOmittedHeldExcludedStatusesLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	user, _, repo := env.seedCodexInfra(t)
	for _, phase := range []string{"paused", "recovery_wait", "limit_wait", "pool_wait"} {
		t.Run(phase, func(t *testing.T) {
			worker := seedSnapshotWorker(t, env, user, "m1-excluded")
			run := seedOutageRun(t, env, user, repo, worker, phase, "issue", 2, 0)
			svc := snapshotSvc(env, testParams())
			if _, err := svc.Heartbeat(env.ctx, hbWorker(t, env, worker), nil, nil, &ActiveSnapshot{SnapshotEpoch: 1, RegisterNonce: "m1-excluded", Active: []ActiveRunEntry{}}); err != nil {
				t.Fatal(err)
			}
			got, err := env.q.GetRunByID(env.ctx, run)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != phase || got.RequeueCount != 0 {
				t.Fatalf("excluded state changed: %+v", got)
			}
		})
	}
}

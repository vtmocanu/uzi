package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/pushbroker"
	"github.com/vtmocanu/uzi/api/internal/recovery"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Use the production cache: a CustodyHoldLimit-only fake deliberately disables
// queuedCustodyAdmission and would make every negative control vacuous.
func queuedCustodyCache(t *testing.T, env codexTestEnv, svc *Service, limit int) {
	t.Helper()
	var old string
	err := env.pool.QueryRow(env.ctx, "SELECT value FROM app_settings WHERE key='custody_hold_limit'").Scan(&old)
	env.exec("INSERT INTO app_settings(key,value) VALUES('custody_hold_limit',$1) ON CONFLICT(key) DO UPDATE SET value=excluded.value", fmt.Sprint(limit))
	t.Cleanup(func() {
		if err == nil {
			env.exec("UPDATE app_settings SET value=$1 WHERE key='custody_hold_limit'", old)
		} else {
			env.exec("DELETE FROM app_settings WHERE key='custody_hold_limit'")
		}
	})
	cache := settings.New(env.q, time.Hour)
	svc.p.CustodyHoldLimit = cache
	if got, err := cache.CustodyHoldLimit(env.ctx); err != nil || int(got) != limit {
		t.Fatalf("DB-backed custody limit=%d err=%v, want %d", got, err, limit)
	}
}

type queuedCustodyObservation struct {
	run             map[string]json.RawMessage
	holds, attempts string
}

func observeQueuedCustody(t *testing.T, env codexTestEnv, id uuid.UUID) queuedCustodyObservation {
	t.Helper()
	var raw []byte
	var o queuedCustodyObservation
	if err := env.pool.QueryRow(env.ctx, "SELECT to_jsonb(r) FROM runs r WHERE id=$1", id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &o.run); err != nil {
		t.Fatal(err)
	}
	for _, q := range []struct {
		table string
		dest  *string
	}{
		{"recovery_custody_holds", &o.holds}, {"checkpoint_publish_attempts", &o.attempts},
	} {
		if err := env.pool.QueryRow(env.ctx, "SELECT COALESCE(jsonb_agg(to_jsonb(x) ORDER BY id),'[]'::jsonb)::text FROM "+q.table+" x WHERE run_id=$1", id).Scan(q.dest); err != nil {
			t.Fatal(err)
		}
	}
	return o
}

func requireQueuedCustodyPreserved(t *testing.T, before, after queuedCustodyObservation, parked bool) {
	t.Helper()
	allowed := map[string]bool{}
	if parked {
		for _, key := range []string{"status", "status_since", "recovery_wait_cause", "recovery_retry_not_before", "health", "health_reason", "health_since", "claim_released_at", "released_worker_id", "released_worker_nonce", "codex_cap_hash", "updated_at"} {
			allowed[key] = true
		}
	}
	for key, want := range before.run {
		if !allowed[key] && !reflect.DeepEqual(want, after.run[key]) {
			t.Fatalf("preservation: runs.%s changed: before=%s after=%s", key, want, after.run[key])
		}
	}
	if before.holds != after.holds {
		t.Fatal("preservation: custody hold rows changed")
	}
	if before.attempts != after.attempts {
		t.Fatal("preservation: checkpoint attempt rows changed")
	}
}

func queuedCustodyTrigger(t *testing.T, c *claimantExhaustion, path string) {
	t.Helper()
	switch path {
	case "heartbeat":
		w := hbWorker(t, c.env, c.workerID)
		_, err := c.svc.Heartbeat(c.env.ctx, w, nil, nil, &ActiveSnapshot{
			SnapshotEpoch: w.SnapshotEpoch + 1, RegisterNonce: w.SnapshotRegisterNonce.String, Active: []ActiveRunEntry{},
		})
		if err != nil {
			t.Fatalf("Heartbeat: %v", err)
		}
	case "register":
		if _, _, err := c.svc.Register(c.env.ctx, hbWorker(t, c.env, c.workerID), "fixture", "", nil, nil, c.wkr.ProtocolCapabilities, nil); err != nil {
			t.Fatalf("Register: %v", err)
		}
	case "sweep":
		c.env.exec("UPDATE workers SET last_heartbeat_at=now()-interval '10 minutes' WHERE id=$1", c.workerID)
		if _, err := c.svc.Sweep(c.env.ctx); err != nil {
			t.Fatalf("Sweep: %v", err)
		}
	default:
		t.Fatalf("unknown path %s", path)
	}
}

// The seeded queued tuple is a historical interrupted continuation, not a live
// execution. Distinct episode/lifetime/codex counters make accidental reuse of
// active exhaustion writers observable. Both released and unreleased postures
// must retain their evidence, holds and every publication-attempt column.
func TestQueuedCustodyCheckpointPreservationLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	for _, path := range []string{"heartbeat", "register", "sweep"} {
		for _, budget := range []struct {
			name  string
			count int
		}{{"belowcap", 6}, {"atcap", 7}, {"overcap", 8}} {
			for _, released := range []bool{false, true} {
				for _, ready := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/%s/released_%v/ready_%v", path, budget.name, released, ready), func(t *testing.T) {
						c := newClaimantExhaustion(t, env, true, 3)
						queuedCustodyCache(t, env, c.svc, 2)
						id := c.seedRun("running", 2)
						c.seedHold(c.userID, id, 1, "open")
						c.seedHold(c.userID, id, 2, "open")
						env.exec(`UPDATE runs SET worker_id=$2, claimed_at=now(), requeue_count=$3, requeue_episode_baseline=4,worker_recovery_episode=5,codex_claim_epoch=9,iteration_count=3,worker_recovery_evidence='{"checkpoint_tip":null,"available_capture":false,"publication_uncertain":true,"capture_uncertain":false,"custody_uncertain":true,"unknown":false,"recorded_at":"2026-11-01T00:00:00Z"}'::jsonb WHERE id=$1`, id, c.workerID, budget.count)
						attempt, err := env.q.RecordLiveCheckpointPublishAttempt(env.ctx, store.RecordLiveCheckpointPublishAttemptParams{
							RecordCheckpointPublishAttemptParams: store.RecordCheckpointPublishAttemptParams{RunID: id, Branch: "m1-branch", Ref: "refs/heads/m1-branch", Tip: lateTip},
							ExpectedWorkerID:                     pgconv.UUID(c.workerID), ExpectedClaimGeneration: 2,
						})
						if err != nil || attempt == uuid.Nil {
							t.Fatalf("production attempt admission: id=%s err=%v", attempt, err)
						}
						if ready {
							if n, err := env.q.MarkCheckpointPublishAttemptReady(env.ctx, attempt); err != nil || n != 1 {
								t.Fatalf("ready: n=%d err=%v", n, err)
							}
						}
						env.exec("UPDATE runs SET status='queued' WHERE id=$1", id)
						if released {
							env.exec("UPDATE runs SET claim_released_at=now()-interval '1 hour',released_worker_id=$2,released_worker_nonce='old-incarnation' WHERE id=$1", id, c.workerID)
						}
						before := observeQueuedCustody(t, env, id)
						parked := budget.name != "belowcap"
						for cycle := 0; cycle < 2; cycle++ {
							queuedCustodyTrigger(t, c, path)
							after := observeQueuedCustody(t, env, id)
							want := "queued"
							if parked {
								want = "recovery_wait"
							}
							if string(after.run["status"]) != fmt.Sprintf("%q", want) {
								t.Fatalf("cycle %d status=%s want %s", cycle, after.run["status"], want)
							}
							if parked && string(after.run["recovery_wait_cause"]) != `"worker_requeue_exhausted"` {
								t.Fatalf("cause=%s", after.run["recovery_wait_cause"])
							}
							requireQueuedCustodyPreserved(t, before, after, parked && cycle == 0)
							if parked && released && (!reflect.DeepEqual(before.run["claim_released_at"], after.run["claim_released_at"]) || !reflect.DeepEqual(before.run["released_worker_nonce"], after.run["released_worker_nonce"]) || !reflect.DeepEqual(before.run["released_worker_id"], after.run["released_worker_id"])) {
								t.Fatal("existing release fence changed")
							}
							before = after
						}
					})
				}
			}
		}
	}
}

// Real Publish supplies both sides of the broker boundary: committed pre-ready
// enrollment is observed while the broker is paused; unknown outcome makes the
// same attempt ready after it returns. Neither is execution authority.
func TestQueuedCustodyPublishBoundaryLiveDB(t *testing.T) {
	for _, budget := range []struct {
		name  string
		count int
	}{{"belowcap", 2}, {"atcap", 3}, {"overcap", 4}} {
		for _, ready := range []bool{false, true} {
			for _, live := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/ready_%v/live_%v", budget.name, ready, live), func(t *testing.T) {
					f := newLiveAttemptFix(t) // retains the real custody hold; do not discard it
					// Adapt the existing publication fixture without manufacturing a publish row.
					env := codexTestEnv{ctx: f.e.ctx, pool: f.e.pool, q: f.e.q, exec: func(sql string, args ...any) { f.e.exec(t, sql, args...) }}
					queuedCustodyCache(t, env, f.svc1, 1)
					f.svc1.p.RunMaxRequeues = 3
					r, err := f.e.q.GetRunByID(f.e.ctx, f.oldRun)
					if err != nil {
						t.Fatal(err)
					}
					w := f.rf.wkr(uuid.UUID(r.WorkerID.Bytes))
					env.exec("UPDATE workers SET snapshot_register_nonce=$2,snapshot_epoch=0,last_heartbeat_at=now() WHERE id=$1", w.ID, exhaustionNonce)
					entered := make(chan struct{})
					release := make(chan struct{})
					var once sync.Once
					unblock := func() { once.Do(func() { close(release) }) }
					ctx, cancel := context.WithTimeout(f.e.ctx, 20*time.Second)
					defer cancel()
					f.svc1.SetPublishFn(func(ctx context.Context, _ pushbroker.Options) (pushbroker.Result, error) {
						close(entered)
						select {
						case <-release:
						case <-ctx.Done():
						}
						return pushbroker.Result{Disposition: pushbroker.PublishOutcomeUnknown}, errors.New("M1 unknown forge outcome")
					})
					out := exhaustionPublishStart(ctx, f, w)
					joined := false
					defer func() {
						unblock()
						cancel()
						if !joined {
							<-out
						}
					}()
					select {
					case <-entered:
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					}
					if ready {
						unblock()
						result := recvPublish(t, out)
						joined = true
						if result.err == nil && result.res.Published {
							t.Fatalf("unknown publish unexpectedly succeeded: %+v", result)
						}
					}
					var n int
					var isReady bool
					if err := env.pool.QueryRow(ctx, "SELECT count(*),bool_and(reconcile_ready_at IS NOT NULL) FROM checkpoint_publish_attempts WHERE run_id=$1", f.oldRun).Scan(&n, &isReady); err != nil {
						t.Fatal(err)
					}
					if n != 1 || isReady != ready {
						t.Fatalf("committed boundary n=%d ready=%v want %v", n, isReady, ready)
					}
					if live {
						snap := &ActiveSnapshot{SnapshotEpoch: 1, RegisterNonce: exhaustionNonce, Active: []ActiveRunEntry{entry(f.oldRun, r.ClaimGeneration, "running", false)}}
						if applied, err := applySnapshot(t, f.svc1, env, w.ID, snap, snapshotModeHeartbeat); err != nil || !applied {
							t.Fatalf("publication lease=%v %v", applied, err)
						}
					}
					env.exec("UPDATE runs SET status='queued',requeue_count=$2,requeue_episode_baseline=0 WHERE id=$1", f.oldRun, budget.count)
					before := observeQueuedCustody(t, env, f.oldRun)
					c := &claimantExhaustion{custodyContinuationFixture: &custodyContinuationFixture{env: env, svc: f.svc1, workerID: w.ID, wkr: w}}
					if live {
						if n := runQueuedCustodyFrozen(t, c); n != 0 {
							t.Fatal("live publication authority was parked")
						}
					} else {
						queuedCustodyTrigger(t, c, "heartbeat")
					}
					after := observeQueuedCustody(t, env, f.oldRun)
					parked := budget.name != "belowcap" && !live
					want := `"queued"`
					if parked {
						want = `"recovery_wait"`
					}
					if string(after.run["status"]) != want {
						t.Fatalf("publish boundary status=%s want %s", after.run["status"], want)
					}
					requireQueuedCustodyPreserved(t, before, after, parked)
				})
			}
		}
	}
}

// A real claimant stop, rather than a seeded queued row, must not spend another
// requeue while custody prevents the next claim. Owner controls retain custody.
func TestQueuedCustodyClaimantControlsLiveDB(t *testing.T) {
	for _, action := range []string{"discard", "cancel", "exhausted_resume", "exhausted_cancel"} {
		t.Run(action, func(t *testing.T) {
			env := setupCodexLiveDB(t)
			max := 3
			exhausted := action == "exhausted_resume" || action == "exhausted_cancel"
			if exhausted {
				max = 1
			}
			c := newClaimantExhaustion(t, env, true, max)
			queuedCustodyCache(t, env, c.svc, 1)
			id := c.seedKind(t, "issue")
			c.claimAndRun(t, c.wkr, id, 1, true)
			c.omitHeartbeat(t, c.workerID)
			before := observeQueuedCustody(t, env, id)
			want := `"queued"`
			if exhausted {
				want = `"recovery_wait"`
			}
			if string(before.run["status"]) != want {
				t.Fatalf("real claimant transition status=%s want %s", before.run["status"], want)
			}
			for cycle := 0; cycle < 2; cycle++ {
				for _, path := range []string{"heartbeat", "sweep", "register", "heartbeat"} {
					queuedCustodyTrigger(t, c, path)
				}
				p, err := c.svc.Claim(env.ctx, hbWorker(t, env, c.workerID), nil)
				if err != nil || p != nil {
					t.Fatalf("blocked Claim=%+v err=%v", p, err)
				}
				after := observeQueuedCustody(t, env, id)
				requireQueuedCustodyPreserved(t, before, after, false)
			}
			switch action {
			case "discard":
				var hold uuid.UUID
				if err := env.pool.QueryRow(env.ctx, "SELECT id FROM recovery_custody_holds WHERE run_id=$1 AND generation=1", id).Scan(&hold); err != nil {
					t.Fatal(err)
				}
				rs := recovery.New(env.q, env.pool, env.box, recovery.Limits{}, time.Now)
				if ok, err := rs.DiscardHold(env.ctx, uuid.New(), id, hold); err != nil || ok {
					t.Fatalf("foreign discard=%v %v", ok, err)
				}
				if ok, err := rs.DiscardHold(env.ctx, c.userID, id, hold); err != nil || !ok {
					t.Fatalf("owner discard=%v %v", ok, err)
				}
				p, err := c.svc.Claim(env.ctx, hbWorker(t, env, c.workerID), nil)
				if err != nil || p == nil || p.RunID != id.String() || p.ClaimGeneration != 2 {
					t.Fatalf("automatic claim after discard=%+v err=%v", p, err)
				}
				var state string
				if err := env.pool.QueryRow(env.ctx, "SELECT state FROM recovery_custody_holds WHERE id=$1", hold).Scan(&state); err != nil || state != "discarded" {
					t.Fatalf("discarded hold=%s %v", state, err)
				}
			case "exhausted_resume":
				if _, err := env.q.ResumeWorkerRecoveryEpisode(env.ctx, store.ResumeWorkerRecoveryEpisodeParams{ID: id, UserID: c.userID, GlobalTimeoutSeconds: 86400}); err != nil {
					t.Fatal(err)
				}
				after := observeQueuedCustody(t, env, id)
				if string(after.run["status"]) != `"queued"` || string(after.run["worker_recovery_episode"]) != "1" || !reflect.DeepEqual(before.run["requeue_count"], after.run["requeue_count"]) || before.holds != after.holds {
					t.Fatal("existing Resume did not renew episode while retaining custody and lifetime usage")
				}
				p, err := c.svc.Claim(env.ctx, hbWorker(t, env, c.workerID), nil)
				if err != nil || p != nil {
					t.Fatalf("Resume must not waive custody: %+v %v", p, err)
				}
			default:
				result, err := c.svc.SubmitInputWithOptions(env.ctx, c.userID, id, "cancel", "owner cancel", nil, SubmitInputOptions{})
				if err != nil || !result.ServerSide {
					t.Fatalf("owner Cancel=%+v %v", result, err)
				}
				after := observeQueuedCustody(t, env, id)
				if string(after.run["status"]) != `"cancelled"` || before.holds != after.holds || before.attempts != after.attempts {
					t.Fatal("Cancel did not preserve custody/attempts")
				}
			}
		})
	}
}

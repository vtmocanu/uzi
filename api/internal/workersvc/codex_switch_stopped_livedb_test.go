package workersvc

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Capture READY before any withdrawal or competing mutation. Never reconstruct
// authority from the run after a failure.
func stoppedFixture(t *testing.T) (applyFx, uuid.UUID, uuid.UUID, StateRequest) {
	t.Helper()
	f, id, target := codexApplyFixtureWithRegistration(t, false, true)
	nonce := f.worker(t).SnapshotRegisterNonce.String
	applyReq := f.req(f.opOf(t, id))
	applyReq.RegisterNonce = nonce
	ready, err := f.svc.PreflightCodexCredentialSwitch(f.env.ctx, f.worker(t), id, CodexSwitchPreflightRequest{
		CredentialSwitchApplyRequest: applyReq, TargetSecretID: target,
	})
	if err != nil || ready.Outcome != "ready" || ready.ExpectedEpoch != 41 {
		t.Fatal("negotiated preflight must be ready at baseline epoch", ready.Outcome, err)
	}
	epoch, generation := ready.ExpectedEpoch, ready.ClaimGeneration
	return f, id, target, StateRequest{State: stateCodexSwitchStopped, ClaimGeneration: &generation, RegisterNonce: ready.RegisterNonce, ExpectedEpoch: &epoch}
}

func withdrawStoppedSwitch(t *testing.T, f applyFx, id uuid.UUID) {
	t.Helper()
	op := f.opOf(t, id).String()
	w := f.worker(t)
	_, applied, err := f.svc.SetState(f.env.ctx, w, id, StateRequest{State: "credential_switch_failed", ClaimGeneration: &f.gen, RegisterNonce: w.SnapshotRegisterNonce.String, OpID: &op})
	if err != nil || !applied {
		t.Fatalf("withdraw: applied=%v err=%v", applied, err)
	}
}

func TestCodexSwitchStoppedPreservationReplayLostReplyLiveDB(t *testing.T) {
	f, id, _, req := stoppedFixture(t)
	withdrawStoppedSwitch(t, f, id)
	before := mustRun(t, f.env, id)
	// A previously admitted switch settles even after advertisement is withdrawn.
	f.env.exec("UPDATE workers SET protocol_capabilities='{}' WHERE id=$1", f.o.workerID)
	after, applied, err := f.svc.SetState(f.env.ctx, f.worker(t), id, req)
	if err != nil || !applied || after.Status != "paused" {
		t.Fatalf("settle: applied=%v status=%s err=%v", applied, after.Status, err)
	}
	want := before
	want.Status, want.StatusSince, want.UpdatedAt = after.Status, after.StatusSince, after.UpdatedAt
	want.Health, want.HealthReason, want.HealthSince = after.Health, after.HealthReason, after.HealthSince
	if !reflect.DeepEqual(want, after) {
		t.Fatal("stopped settlement changed fields beyond status, timestamps and health")
	}
	// Simulate loss of the first reply by resending the exact request. Compare the
	// complete run and journals/messages, including timestamps, on every replay.
	f.noWrite(t, id, func() {
		replayed, accepted, err := f.svc.SetState(f.env.ctx, f.worker(t), id, req)
		if err != nil || !accepted || !reflect.DeepEqual(after, replayed) {
			t.Fatal("lost-reply replay did not preserve the complete row", err)
		}
	})
}

func TestCodexSwitchStoppedWithdrawnXYLiveDB(t *testing.T) {
	f, id, target, req := stoppedFixture(t)
	withdrawStoppedSwitch(t, f, id)
	if _, err := f.svc.SetRunCredential(f.env.ctx, f.o.userID, id, BindModePinned, &target); err != nil {
		t.Fatal(err)
	}
	withdrawStoppedSwitch(t, f, id)
	// Neither withdrawn operation changes binding/claim/incarnation/epoch.
	_, applied, err := f.svc.SetState(f.env.ctx, f.worker(t), id, req)
	if err != nil || !applied {
		t.Fatal("withdrawn X then Y must settle", err)
	}
}

func TestCodexSwitchStoppedEpochAdvanceAppliedSuccessorLiveDB(t *testing.T) {
	f, id, _, req := stoppedFixture(t)
	applyReq := f.req(f.opOf(t, id))
	applyReq.RegisterNonce = req.RegisterNonce
	res := f.apply(t, id, applyReq)
	if res.Outcome != ApplyOutcomeApplied {
		t.Fatal(res.Outcome)
	}
	f.noWrite(t, id, func() {
		_, applied, err := f.svc.SetState(f.env.ctx, f.worker(t), id, req)
		if err != nil || applied {
			t.Fatal("old epoch settled over applied successor", err)
		}
	})
}

func TestCodexSwitchStoppedCompetingControlsDelayedReportLiveDB(t *testing.T) {
	for _, mutation := range []string{
		"pause_requested_at=now(), pause_mode='wall'",
		"pause_requested_at=now(), pause_mode='reserve'",
		"pause_requested_at=now(), pause_mode='now'",
		"pause_requested_at=now(), pause_mode='milestone'",
		"hold_reason='credential_switch_unsettled'",
		"budget_wall_seconds=1, started_at=now()-interval '1 hour'",
		"stop_kind='cancelled'",
		"reserve_hold_reason='weekly_reserve'",
		"credential_switch_requested_at=now(), credential_switch_generation=7",
		"claim_generation=8",
		"claim_released_at=now()",
		"codex_claim_epoch=42",
		"status='queued'",
		"status='completed'",
	} {
		t.Run(mutation, func(t *testing.T) {
			f, id, _, req := stoppedFixture(t)
			withdrawStoppedSwitch(t, f, id)
			f.env.exec("UPDATE runs SET "+mutation+" WHERE id=$1", id)
			f.noWrite(t, id, func() {
				_, applied, _ := f.svc.SetState(f.env.ctx, f.worker(t), id, req)
				if applied {
					t.Fatal("competing control/delayed report accepted")
				}
			})
		})
	}
}

func TestCodexSwitchStoppedPendingControlAndReplayLiveDB(t *testing.T) {
	for _, status := range []string{"running", "paused"} {
		for _, kind := range []string{"pause", "pause_cancel", "cancel", "stop", "scope", "revise_plan", "reject_plan", "resume"} {
			t.Run(status+"/"+kind, func(t *testing.T) {
				f, id, _, req := stoppedFixture(t)
				withdrawStoppedSwitch(t, f, id)
				if status == "paused" {
					if _, accepted, err := f.svc.SetState(f.env.ctx, f.worker(t), id, req); err != nil || !accepted {
						t.Fatal("initial stop", err)
					}
				}
				f.env.exec("INSERT INTO run_user_inputs(run_id,kind,body) VALUES($1,$2,'competing control')", id, kind)
				f.noWrite(t, id, func() {
					_, accepted, err := f.svc.SetState(f.env.ctx, f.worker(t), id, req)
					if err != nil || accepted {
						t.Fatal("pending control must refuse mutation and replay", err)
					}
				})
			})
		}
	}
}

func TestCodexSwitchStoppedResumeReclaimReregistrationLiveDB(t *testing.T) {
	for _, change := range []string{"resume", "reclaim", "registration", "advertisement"} {
		t.Run(change, func(t *testing.T) {
			f, id, _, req := stoppedFixture(t)
			withdrawStoppedSwitch(t, f, id)
			if _, applied, err := f.svc.SetState(f.env.ctx, f.worker(t), id, req); err != nil || !applied {
				t.Fatal("initial stop refused", err)
			}
			switch change {
			case "resume":
				_, err := f.env.q.ResumePausedRun(f.env.ctx, store.ResumePausedRunParams{ID: id, UserID: f.o.userID, GlobalTimeoutSeconds: 3600})
				if err != nil {
					t.Fatal(err)
				}
			case "reclaim":
				f.env.exec("UPDATE runs SET claim_generation=claim_generation+1 WHERE id=$1", id)
			case "registration":
				f.env.exec("UPDATE workers SET snapshot_register_nonce='new-incarnation' WHERE id=$1", f.o.workerID)
			case "advertisement":
				f.env.exec("UPDATE workers SET protocol_capabilities='{}' WHERE id=$1", f.o.workerID)
			}
			f.noWrite(t, id, func() {
				_, applied, _ := f.svc.SetState(f.env.ctx, f.worker(t), id, req)
				if applied != (change == "advertisement") {
					t.Fatal("delayed report/replay disposition mismatch")
				}
			})
		})
	}
}

// Cutoffs only retire stop/cancel/reject_plan. Pause and revise remain deliverable.
func TestCodexSwitchStoppedDeliverableBelowCutoffLiveDB(t *testing.T) {
	for _, paused := range []bool{false, true} {
		for _, kind := range []string{"pause", "revise_plan", "stop", "cancel", "reject_plan"} {
			t.Run(fmt.Sprintf("%t/%s", paused, kind), func(t *testing.T) {
				f, id, _, req := stoppedFixture(t)
				withdrawStoppedSwitch(t, f, id)
				if paused {
					if _, ok, err := f.svc.SetState(f.env.ctx, f.worker(t), id, req); err != nil || !ok {
						t.Fatal("initial settlement refused", err)
					}
				}
				var input int64
				if err := f.env.pool.QueryRow(f.env.ctx, "INSERT INTO run_user_inputs(run_id,kind,body) VALUES($1,$2,'control') RETURNING id", id, kind).Scan(&input); err != nil {
					t.Fatal(err)
				}
				f.env.exec(`INSERT INTO run_milestone_reconciliations(id,run_id,owner_id,gate_revision,presentation_id,
					gate_digest,reset_state_digest,worker_id,claim_generation,reason,consent,before_state,after_state,applied_input_id,stop_cutoff)
					VALUES($1,$2,$3,1,$4,$5,'fixture',$6,$7,'fixture','{}','{}','{}',$8,$8)`,
					uuid.New(), id, f.o.userID, uuid.New(), []byte{1}, f.o.workerID, f.gen, input)
				check := func() {
					_, accepted, err := f.svc.SetState(f.env.ctx, f.worker(t), id, req)
					if err != nil || accepted != (kind == "stop" || kind == "cancel" || kind == "reject_plan") {
						t.Fatal("cutoff disposition mismatch", err)
					}
				}
				if paused || kind == "pause" || kind == "revise_plan" {
					f.noWrite(t, id, check)
				} else {
					check()
				}
			})
		}
	}
}

func TestCodexSwitchStoppedRevokedAuthorityLiveDB(t *testing.T) {
	for _, paused := range []bool{false, true} {
		for _, revoke := range []string{"alias", "capability", "policy", "policy_fresh", "policy_hold"} {
			t.Run(fmt.Sprintf("%t/%s", paused, revoke), func(t *testing.T) {
				f, id, _, req := stoppedFixture(t)
				withdrawStoppedSwitch(t, f, id)
				if paused {
					if _, ok, err := f.svc.SetState(f.env.ctx, f.worker(t), id, req); err != nil || !ok {
						t.Fatal("initial settlement refused", err)
					}
				}
				switch revoke {
				case "alias":
					f.env.exec("DELETE FROM user_secrets WHERE id=(SELECT codex_secret_id FROM runs WHERE id=$1)", id)
				case "capability":
					f.env.exec("UPDATE runs SET codex_cap_hash=NULL WHERE id=$1", id)
				case "policy", "policy_fresh", "policy_hold":
					setCodexSwitchPolicyEpisode(f, id, revoke == "policy_fresh", revoke == "policy_hold")
				}
				f.noWrite(t, id, func() {
					_, accepted, err := f.svc.SetState(f.env.ctx, f.worker(t), id, req)
					if err != nil || accepted {
						t.Fatal("revoked authority accepted", err)
					}
				})
			})
		}
	}
}

// The held worker lock orders the late control before the report's locked read.
// Each case starts one report, bounded by the service timeout and this context.
func TestCodexSwitchStoppedConcurrentLateControlLiveDB(t *testing.T) {
	for _, control := range []string{"policy", "registration", "pause"} {
		t.Run(control, func(t *testing.T) {
			f, id, _, req := stoppedFixture(t)
			withdrawStoppedSwitch(t, f, id)
			worker := f.worker(t)
			ctx, cancel := context.WithTimeout(f.env.ctx, 5*time.Second)
			defer cancel()
			tx, err := f.env.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if _, err := store.New(tx).GetWorkerForUpdate(ctx, worker.ID); err != nil {
				t.Fatal(err)
			}
			type report struct {
				accepted bool
				err      error
			}
			done := make(chan report, 1)
			go func() {
				_, accepted, err := f.svc.SetState(ctx, worker, id, req)
				done <- report{accepted, err}
			}()
			switch control {
			case "policy":
				_, err = tx.Exec(ctx, "UPDATE runs SET policy_outcome_id=$2, policy_preserve_required=true, policy_continuation_mode='same_conversation', policy_applying_worker_id=worker_id, policy_applying_generation=claim_generation, policy_source_generation=claim_generation WHERE id=$1", id, uuid.New())
			case "registration":
				_, err = tx.Exec(ctx, "UPDATE workers SET snapshot_register_nonce='late-registration' WHERE id=$1", worker.ID)
			case "pause":
				_, err = tx.Exec(ctx, "UPDATE runs SET pause_requested_at=now(),pause_mode='wall' WHERE id=$1", id)
			}
			if err != nil {
				_ = tx.Rollback(ctx)
				<-done
				t.Fatal(err)
			}
			err = tx.Commit(ctx)
			result := <-done
			if err != nil || result.err != nil || result.accepted {
				t.Fatal("late control failed to fence report", err, result.err)
			}
		})
	}
}

func TestCodexSwitchStoppedOldWorkerPreflightRefusedLiveDB(t *testing.T) {
	f, id, target, req := stoppedFixture(t)
	f.env.exec("UPDATE workers SET protocol_capabilities=ARRAY[$2]::text[] WHERE id=$1", f.o.workerID, capability.CodexCredentialSwitchV1)
	nonce := req.RegisterNonce
	applyReq := f.req(f.opOf(t, id))
	applyReq.RegisterNonce = nonce
	f.noWrite(t, id, func() {
		result, err := f.svc.PreflightCodexCredentialSwitch(f.env.ctx, f.worker(t), id, CodexSwitchPreflightRequest{
			CredentialSwitchApplyRequest: applyReq, TargetSecretID: target,
		})
		if err != nil || result.Outcome == "ready" || result.ExpectedEpoch != 0 {
			t.Fatal("old worker admitted teardown", err)
		}
	})
	// Settlement of an admitted claim ignores the current advertisement.
	withdrawStoppedSwitch(t, f, id)
	if run, accepted, err := f.svc.SetState(f.env.ctx, f.worker(t), id, req); err != nil || !accepted || run.Status != "paused" {
		t.Fatal("advertisement downgrade blocked settlement", err)
	}
}

func TestCodexSwitchStoppedReplayUsesOriginalPauseClockLiveDB(t *testing.T) {
	f, id, _, req := stoppedFixture(t)
	withdrawStoppedSwitch(t, f, id)
	// An ordinary paused row with the same complete predicate is equivalent.
	// Its running interval was 10 seconds, while the elapsed wall time is an hour.
	f.env.exec("UPDATE runs SET status='paused',started_at=now()-interval '1 hour',status_since=now()-interval '3590 seconds',budget_wall_seconds=60,budget_paused_seconds=0 WHERE id=$1", id)
	f.noWrite(t, id, func() {
		_, accepted, err := f.svc.SetState(f.env.ctx, f.worker(t), id, req)
		if err != nil || !accepted {
			t.Fatal("paused wall time charged again", err)
		}
	})
}

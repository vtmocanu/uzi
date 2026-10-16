package workersvc

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func latePolicyExec(t *testing.T, e usageTailEnv, sql string, args ...any) {
	t.Helper()
	if _, err := e.pool.Exec(e.ctx, sql, args...); err != nil {
		t.Fatal(err)
	}
}

func latePolicyHold(t *testing.T, e usageTailEnv) uuid.UUID {
	t.Helper()
	id := uuid.New()
	latePolicyExec(t, e, `INSERT INTO recovery_custody_holds(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity)
 VALUES($1,$2,$3,$4,1,'open',$5,'policy-late-fixture')`, id, e.userID, e.repoID, e.runID, e.wkr.ID)
	return id
}

// Only policy metadata may change; the returned row supplies the unchanged live contract.
func latePolicyLive(r store.Run) store.Run {
	r.PolicyRefusalCount = 0
	r.PolicyLastTag = store.Run{}.PolicyLastTag
	r.PolicyLastPhase = store.Run{}.PolicyLastPhase
	r.PolicyLastCorrelationID = store.Run{}.PolicyLastCorrelationID
	r.PolicyLastRefusedAt = store.Run{}.PolicyLastRefusedAt
	r.PolicyLastOutcomeID = store.Run{}.PolicyLastOutcomeID
	r.PolicyLastSourceGeneration = store.Run{}.PolicyLastSourceGeneration
	r.PolicyHoldDetail = store.Run{}.PolicyHoldDetail
	r.PolicyHoldProvenance = store.Run{}.PolicyHoldProvenance
	r.PolicyHoldPublished = store.Run{}.PolicyHoldPublished
	r.UpdatedAt = store.Run{}.UpdatedAt
	return r
}

func TestPolicyLateAfterRealClaimLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fx := newCodexClaimFix(t, env, true) // Seeds exact G=1 original-worker custody via claimRecoveryHold.
	if fx.holdA == uuid.Nil {
		t.Fatal("missing original custody fixture")
	}
	env.exec("UPDATE runs SET worker_id=NULL,claim_released_at=now(),released_worker_id=$2,released_worker_nonce='old-process' WHERE id=$1", fx.runID, fx.workerA)
	claimed, err := env.q.ClaimRun(env.ctx, claimRunParamsFor(fx.claimant(t, true)))
	if err != nil || claimed.ID != fx.runID || claimed.ClaimGeneration != 2 || claimed.ReleasedWorkerID.Valid || claimed.ReleasedWorkerNonce.Valid {
		t.Fatalf("real claim: id=%s generation=%d release=%v/%v err=%v", claimed.ID, claimed.ClaimGeneration, claimed.ReleasedWorkerID, claimed.ReleasedWorkerNonce, err)
	}
	worker := wkrRow(t, env, fx.workerA)
	worker.ProtocolCapabilities = []string{capability.ProviderPolicyHoldV1, capability.CredentialSwitchV1}
	fx.svc.p.PolicyHoldNegotiated = true
	fx.svc.p.PolicyHoldSnapshotEnabled = true
	fx.svc.p.TerminalPendingLease = time.Hour
	before := mustRun(t, env, fx.runID)
	r, d, err := fx.svc.ReportPolicyHold(env.ctx, worker, fx.runID, policyRequest())
	if err != nil || string(d) != "recorded" || r.PolicyRefusalCount != 1 {
		t.Fatalf("after real claim: %s count=%d err=%v", d, r.PolicyRefusalCount, err)
	}
	if !reflect.DeepEqual(latePolicyLive(before), latePolicyLive(r)) {
		t.Fatal("late G altered G+1")
	}
	// A later reclaim must enforce the newly recorded constraint even though
	// this already-started G+1 was left untouched by the historical report.
	env.exec("UPDATE runs SET status='queued' WHERE id=$1", fx.runID)
	nextWorker := fx.claimant(t, true)
	if _, err := env.q.ClaimRun(env.ctx, claimRunParamsFor(nextWorker)); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("incapable future claimant: %v", err)
	}
	nextWorker.ProtocolCapabilities = append(nextWorker.ProtocolCapabilities, capability.ProviderPolicyHoldV1, capability.CredentialSwitchV1)
	env.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", nextWorker.ID, nextWorker.ProtocolCapabilities)
	next, err := env.q.ClaimRun(env.ctx, claimRunParamsFor(nextWorker))
	if err != nil || next.ClaimGeneration != 3 || next.PolicyRefusalCount != 1 || next.PolicyFreshClaimGeneration.Valid {
		t.Fatalf("future claim lost policy constraint: generation=%d count=%d fresh=%v err=%v", next.ClaimGeneration, next.PolicyRefusalCount, next.PolicyFreshClaimGeneration, err)
	}
}

func TestPolicyLateOrdinaryWritersLiveDB(t *testing.T) {
	for _, holdReason := range []string{"provider_policy_refusal", "provider_unavailable", ""} {
		for _, queued := range []bool{false, true} {
			for _, state := range []string{"running", "completed", "completed_query", "reconcile_mr_query", "credential_switch", "failed", "awaiting_input", "awaiting_followup", "plan_rejected", "superseded"} {
				t.Run(state+map[bool]string{false: "_paused", true: "_queued"}[queued]+"/"+holdReason, func(t *testing.T) {
					e := setupPolicyHold(t)
					held, d, err := e.svc.ReportPolicyHold(e.ctx, e.wkr, e.runID, policyRequest())
					if err != nil || d != PolicyHoldApplied {
						t.Fatalf("hold=%s err=%v", d, err)
					}
					latePolicyExec(t, e, "UPDATE runs SET hold_reason=NULLIF($2, '') WHERE id=$1", e.runID, holdReason)
					if queued {
						if _, err := e.svc.ResumePolicyHold(e.ctx, e.userID, e.runID, PolicyHoldActionToken(held), PolicyResumeRequest{Mode: "fresh", Guidance: "owner choice"}); err != nil {
							t.Fatal(err)
						}
					}
					before := mustRun(t, e.codexTestEnv, e.runID)
					branch := "agent/late-policy"
					mr := int64(123)
					reason := "late"
					origin := "provider_policy_refusal"
					var applied bool
					switch state {
					case "completed_query":
						n, err := e.q.SetRunCompleted(e.ctx, store.SetRunCompletedParams{ID: e.runID, WorkerID: pgconv.UUID(e.wkr.ID)})
						if err != nil {
							t.Fatal(err)
						}
						applied = n != 0
					case "reconcile_mr_query":
						n, err := e.q.ReconcileRunMR(e.ctx, store.ReconcileRunMRParams{ID: e.runID, WorkerID: pgconv.UUID(e.wkr.ID), Branch: pgconv.TextOrNull(branch), MrIid: pgconv.Int8Ptr(&mr)})
						if err != nil {
							t.Fatal(err)
						}
						applied = n != 0
					case "plan_rejected":
						n, err := e.q.SetRunFailedPlanRejected(e.ctx, store.SetRunFailedPlanRejectedParams{
							ID: e.runID, WorkerID: pgconv.UUID(e.wkr.ID),
							FailureReason: pgconv.TextOrNull(reason), FailOrigin: pgconv.TextOrNull("plan_rejected"),
						})
						if err != nil {
							t.Fatal(err)
						}
						applied = n != 0
					case "superseded":
						n, err := e.q.SupersedeRunByWorker(e.ctx, store.SupersedeRunByWorkerParams{
							ID: e.runID, WorkerID: pgconv.UUID(e.wkr.ID), StopReason: reason,
						})
						if err != nil {
							t.Fatal(err)
						}
						applied = n != 0
					default:
						_, applied, err = e.svc.SetState(e.ctx, e.wkr, e.runID, StateRequest{State: state, ClaimGeneration: i64p(1), Branch: &branch, MrIID: &mr, FailureReason: &reason, FailOrigin: &origin})
					}
					after := mustRun(t, e.codexTestEnv, e.runID)
					if !reflect.DeepEqual(before, after) {
						bv, av := reflect.ValueOf(before), reflect.ValueOf(after)
						for i := 0; i < bv.NumField(); i++ {
							if !reflect.DeepEqual(bv.Field(i).Interface(), av.Field(i).Interface()) {
								t.Logf("changed %s: before=%+v after=%+v", bv.Type().Field(i).Name, bv.Field(i).Interface(), av.Field(i).Interface())
							}
						}
						t.Error("late ordinary writer changed owner contract")
					}
					if applied {
						t.Fatalf("late %s applied: %v", state, err)
					}
				})
			}
		}
	}
}

func TestPolicyLateFreshOrdinaryContinuationLiveDB(t *testing.T) {
	for _, status := range []string{"claimed", "running"} {
		t.Run(status, func(t *testing.T) {
			e := setupPolicyHold(t)
			held, d, err := e.svc.ReportPolicyHold(e.ctx, e.wkr, e.runID, policyRequest())
			if err != nil || d != PolicyHoldApplied {
				t.Fatalf("hold=%s err=%v", d, err)
			}
			if _, err := e.svc.ResumePolicyHold(e.ctx, e.userID, e.runID, PolicyHoldActionToken(held), PolicyResumeRequest{Mode: "fresh"}); err != nil {
				t.Fatal(err)
			}
			latePolicyExec(t, e, "UPDATE runs SET status=$2,claim_generation=2,policy_fresh_claim_generation=2 WHERE id=$1", e.runID, status)
			r, applied, err := e.svc.SetState(e.ctx, e.wkr, e.runID, StateRequest{State: "running", ClaimGeneration: i64p(2)})
			if err != nil || !applied || r.Status != "running" {
				t.Fatalf("fresh continuation: status=%s applied=%v err=%v", r.Status, applied, err)
			}
		})
	}
}

func TestPolicyLateDeliveryAuthorityLiveDB(t *testing.T) {
	for _, delivery := range []int64{1, 2, 3} {
		t.Run(fmt.Sprintf("delivery_%d", delivery), func(t *testing.T) {
			e := setupPolicyHold(t)
			// This worker owns G+1, but has no custody authorizing old delivery G.
			latePolicyExec(t, e, "UPDATE runs SET claim_generation=2,status='running' WHERE id=$1", e.runID)
			req := policyRequest()
			req.ClaimGeneration = delivery
			before := mustRun(t, e.codexTestEnv, e.runID)
			r, d, err := e.svc.ReportPolicyHold(e.ctx, e.wkr, e.runID, req)
			switch delivery {
			case 1:
				if err != nil || d != PolicyHoldUnauthenticatedHistoricalOutcome {
					t.Fatalf("old delivery: %s %v", d, err)
				}
			case 2:
				if err != nil || d != PolicyHoldApplied || r.Status != "paused" || r.PolicyRefusalCount != 1 {
					t.Fatalf("local conversion: %s %+v %v", d, r, err)
				}
			case 3:
				if !errors.Is(err, ErrRunNotOwned) {
					t.Fatalf("future delivery: %s %v", d, err)
				}
			}
			if delivery != 2 && !reflect.DeepEqual(before, mustRun(t, e.codexTestEnv, e.runID)) {
				t.Fatal("unauthorized delivery mutated run")
			}
		})
	}
}

func TestPolicyLateBudgetMetadataLiveDB(t *testing.T) {
	e := setupPolicyHold(t)
	latePolicyExec(t, e, `UPDATE runs SET status='paused',hold_reason='budget_exhausted',claim_released_at=now(),
 released_worker_id=worker_id,hold_captured_head=repeat('b',40),checkpoint_contains_latest=false WHERE id=$1`, e.runID)
	before := mustRun(t, e.codexTestEnv, e.runID)
	req := policyRequest()
	latest := true
	req.CheckpointContainsLatest = &latest
	r, d, err := e.svc.ReportPolicyHold(e.ctx, e.wkr, e.runID, req)
	if err != nil || string(d) != "recorded" || r.PolicyRefusalCount != 1 {
		t.Fatalf("budget metadata: disposition=%s count=%d err=%v", d, r.PolicyRefusalCount, err)
	}
	if r.HoldCapturedHead.String != req.Head || !r.CheckpointContainsLatest.Valid || !r.CheckpointContainsLatest.Bool {
		t.Fatal("budget policy preservation does not identify the reported source")
	}
	before.HoldCapturedHead = r.HoldCapturedHead
	before.CheckpointContainsLatest = r.CheckpointContainsLatest
	if !reflect.DeepEqual(latePolicyLive(before), latePolicyLive(r)) {
		t.Fatal("budget metadata changed live contract")
	}
	r, d, err = e.svc.ReportPolicyHold(e.ctx, e.wkr, e.runID, req)
	if err != nil || d != PolicyHoldAlreadyApplied || r.PolicyRefusalCount != 1 {
		t.Fatalf("duplicate=%s count=%d err=%v", d, r.PolicyRefusalCount, err)
	}
}

func TestPolicyLateHistoricalAuthorizationLiveDB(t *testing.T) {
	for _, mode := range []string{"valid", "missing", "discarded", "wrong_worker", "wrong_user", "dedupe_discarded", "newer", "equal_source", "resume_rejected", "resume_rejected_empty", "stop_pending", "queued", "cancelled", "completed", "failed"} {
		t.Run(mode, func(t *testing.T) {
			e := setupPolicyHold(t)
			req := policyRequest()
			hold := latePolicyHold(t, e)
			switch mode {
			case "missing":
				latePolicyExec(t, e, "DELETE FROM recovery_custody_holds WHERE id=$1", hold)
			case "discarded", "dedupe_discarded":
				latePolicyExec(t, e, "UPDATE recovery_custody_holds SET state='discarded' WHERE id=$1", hold)
			case "wrong_worker":
				latePolicyExec(t, e, "UPDATE recovery_custody_holds SET original_worker_id=$2 WHERE id=$1", hold, uuid.New())
			case "wrong_user":
				other := uuid.New()
				latePolicyExec(t, e, "INSERT INTO users(id,email,password_hash) VALUES($1,$2,'x')", other, other.String()+"@policy.test")
				latePolicyExec(t, e, "UPDATE recovery_custody_holds SET user_id=$2 WHERE id=$1", hold, other)
			}
			latePolicyExec(t, e, `UPDATE runs SET claim_generation=2,status='running',released_worker_id=NULL,released_worker_nonce=NULL,
 session_id='live-new-session',policy_fresh_claim_generation=2,scope_ceiling=3 WHERE id=$1`, e.runID)
			if mode == "queued" {
				latePolicyExec(t, e, "UPDATE runs SET status='queued',claim_generation=1,claim_released_at=now(),worker_id=NULL WHERE id=$1", e.runID)
			}
			if mode == "cancelled" || mode == "completed" || mode == "failed" {
				latePolicyExec(t, e, "UPDATE runs SET status=$2 WHERE id=$1", e.runID, mode)
			}
			if mode == "dedupe_discarded" {
				latePolicyExec(t, e, "UPDATE runs SET policy_last_outcome_id=$2,policy_last_source_generation=1,policy_refusal_count=1 WHERE id=$1", e.runID, req.OutcomeID)
			}
			if mode == "equal_source" {
				latePolicyExec(t, e, "UPDATE runs SET policy_last_outcome_id=$2,policy_last_source_generation=1,policy_refusal_count=1 WHERE id=$1", e.runID, uuid.New())
			}
			if mode == "resume_rejected" || mode == "resume_rejected_empty" {
				req.Detail = "resume_rejected"
				if mode == "resume_rejected" {
					latePolicyExec(t, e, "UPDATE runs SET policy_refusal_count=1,policy_last_tag='misalignmentPolicyViolation',policy_last_phase='implementation' WHERE id=$1", e.runID)
				}
			}
			if mode == "stop_pending" {
				latePolicyExec(t, e, "UPDATE runs SET stop_kind='cancelled' WHERE id=$1", e.runID)
			}
			if mode == "newer" {
				latePolicyExec(t, e, "UPDATE runs SET policy_last_outcome_id=$2,policy_last_source_generation=2,policy_refusal_count=1 WHERE id=$1", e.runID, uuid.New())
			}
			latePolicyExec(t, e, "UPDATE runs SET policy_hold_provenance='verified',policy_hold_detail='refused',policy_hold_published=true WHERE id=$1", e.runID)
			before := mustRun(t, e.codexTestEnv, e.runID)
			r, d, err := e.svc.ReportPolicyHold(e.ctx, e.wkr, e.runID, req)
			want := "unauthenticated_historical_outcome"
			switch mode {
			case "valid", "queued", "resume_rejected":
				want = "recorded"
			case "equal_source", "stop_pending", "resume_rejected_empty":
				want = "pending"
			case "newer":
				want = "resolved_newer_policy"
			case "cancelled", "completed", "failed":
				want = "terminal"
			}
			if mode == "resume_rejected_empty" {
				if !errors.Is(err, ErrInvalidState) {
					t.Fatalf("empty rejection: %v", err)
				}
				err = nil
			}
			if err != nil || string(d) != want {
				t.Fatalf("history: disposition=%s want=%s err=%v", d, want, err)
			}
			after := mustRun(t, e.codexTestEnv, e.runID)
			if !reflect.DeepEqual(latePolicyLive(before), latePolicyLive(after)) {
				t.Fatal("history overwrote current execution/owner contract")
			}
			if after.PolicyHoldProvenance != before.PolicyHoldProvenance || after.PolicyHoldDetail != before.PolicyHoldDetail || after.PolicyHoldPublished != before.PolicyHoldPublished {
				t.Fatal("historical report replaced current hold provenance")
			}
			if mode == "resume_rejected" && (after.PolicyLastTag != before.PolicyLastTag || after.PolicyLastPhase != before.PolicyLastPhase || after.PolicyLastCorrelationID != before.PolicyLastCorrelationID || after.PolicyLastRefusedAt != before.PolicyLastRefusedAt) {
				t.Fatal("resume rejection replaced actual refusal metadata")
			}
			if want == "recorded" {
				if r.PolicyRefusalCount != 1 || r.PolicyLastSourceGeneration.Int64 != 1 {
					t.Fatal("history not recorded")
				}
				_, d, err = e.svc.ReportPolicyHold(e.ctx, e.wkr, e.runID, req)
				if err != nil || d != PolicyHoldAlreadyApplied {
					t.Fatalf("duplicate=%s %v", d, err)
				}
			} else if !reflect.DeepEqual(before, after) {
				t.Fatal("refused/resolved/terminal history mutated policy")
			}
		})
	}
}

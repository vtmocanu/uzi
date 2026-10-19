package workersvc

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// V1 is seeded for these service/SQL component proofs. This is not a runtime
// source-writer, running/completion/ACK or zero-root worker composition proof.
func associationStopFixture(t *testing.T, crossCheck bool) (usageTailEnv, uuid.UUID) {
	t.Helper()
	e := seedPolicyRevisionApprovalClaim(t, crossCheck, 2, true)
	ctx, cancel := context.WithTimeout(e.ctx, 20*time.Second)
	t.Cleanup(cancel)
	e.ctx = ctx
	e.svc.p.PolicyStopNegotiated = true
	// A seeded running revision context is admitted by the real refusal service;
	// this does not exercise a running transition or planning-frame source writer.
	e.exec("UPDATE runs SET kind='issue',issue_iid=101,size_class='s',status='running',required_tools='{go}' WHERE id=$1", e.runID)
	r := mustRun(t, e.codexTestEnv, e.runID)
	source, err := apitypes.ParsePolicyRevisionSourceV1(r.PolicyRevisionSource)
	if err != nil {
		t.Fatal(err)
	}
	payload, digest, err := gatePayloadFromRow(r).digest()
	if err != nil {
		t.Fatal(err)
	}
	source.Gate.PayloadDigest = hex.EncodeToString(digest)
	ccID := uuid.Nil
	if crossCheck {
		ccID = uuid.New()
		candidate := PlanCrossCheckCandidate{PlanMd: r.PlanMd.String, Milestones: r.MilestonesCandidate,
			RequiredCapabilities: r.RequiredCapabilities, RequiredTools: r.RequiredTools,
			SizeClass: r.SizeClass, BaseCommit: strings.Repeat("a", 40), PlanningDiff: "original automatic feedback diff\n"}
		ccDigest, err := candidate.Digest()
		if err != nil {
			t.Fatal(err)
		}
		e.exec(`INSERT INTO cross_checks(id,lead_run_id,stage,round,lead_claim_generation,plan_md,milestones,
			required_capabilities,required_tools,size_class,base_commit,planning_diff,candidate_digest,
			verdict,reason_class,findings,created_at,decided_at,deadline_at,checker_harness)
			VALUES($1,$2,'plan',1,$11,$3,$4,$5,$6,$7,$8,$9,$10,'revise','revise',
			'[{"body":"  original automatic feedback\n"}]',now(),now(),now()+interval '1 hour','claude')`,
			ccID, e.runID, candidate.PlanMd, candidate.Milestones, candidate.RequiredCapabilities, candidate.RequiredTools,
			candidate.SizeClass, candidate.BaseCommit, candidate.PlanningDiff, ccDigest, r.ClaimGeneration)
		child := uuid.New()
		e.exec("INSERT INTO runs(id,user_id,repo_id,kind,harness,status,target_run_id,report_only,budget_wall_seconds,issue_title,issue_description,finished_at) VALUES($1,$2,$3,'cross_check','claude','completed',$4,true,60,'seeded checker','seeded checker provenance',now())", child, e.userID, e.repoID, e.runID)
		e.exec("UPDATE cross_checks SET checker_run_id=$2 WHERE id=$1", ccID, child)
		ref := ccID.String()
		source.AssociatedCrossChecks = []apitypes.PolicyRevisionRef{{Source: "plan_cross_check", CrossCheckID: &ref}}
		e.exec("UPDATE runs SET policy_candidate_cross_check_id=$2 WHERE id=$1", e.runID, ccID)
	}
	raw, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	e.exec("UPDATE runs SET gate_presented_payload=$2,gate_payload_digest=$3,policy_revision_source=$4 WHERE id=$1",
		e.runID, payload, digest, raw)
	held, disposition, err := e.svc.ReportPolicyHold(ctx, e.wkr, e.runID, policyRequest())
	if err != nil || disposition != PolicyHoldApplied || held.Status != "paused" {
		t.Fatalf("fixture policy hold: status=%s disposition=%s err=%v", held.Status, disposition, err)
	}
	return e, ccID
}

func TestCrossCheckPolicyAssociationControlsLiveDB(t *testing.T) {
	for _, mode := range []string{"sql_null", "unknown_v1", "associated_null", "primary_null", "selection_outside_v1", "dangling", "wrong_lead", "wrong_stage", "wrong_content", "wrong_projection", "wrong_digest"} {
		t.Run(mode, func(t *testing.T) {
			e, ccID := associationStopFixture(t, true)
			review := policyOwnerStopReview(t, e) // actual displayed consent before context changes
			switch mode {
			case "sql_null":
				e.exec("UPDATE runs SET policy_candidate_cross_check_id=NULL,policy_revision_source=NULL WHERE id=$1", e.runID)
			case "unknown_v1":
				e.exec("UPDATE runs SET policy_candidate_cross_check_id=NULL,policy_revision_source=jsonb_set(policy_revision_source,'{version}','99') WHERE id=$1", e.runID)
			case "associated_null":
				e.exec("UPDATE runs SET policy_candidate_cross_check_id=NULL WHERE id=$1", e.runID)
			case "primary_null":
				e.exec(`UPDATE runs SET policy_candidate_cross_check_id=NULL,policy_revision_source=jsonb_set(
					jsonb_set(policy_revision_source,'{primary_revision}',policy_revision_source->'associated_cross_checks'->0),
					'{associated_cross_checks}','[]') WHERE id=$1`, e.runID)
			case "selection_outside_v1":
				e.exec("UPDATE runs SET policy_candidate_cross_check_id=$2 WHERE id=$1", e.runID, uuid.New())
			case "dangling":
				e.exec("DELETE FROM cross_checks WHERE id=$1", ccID)
			case "wrong_lead":
				other := newPolicyOwnerStopFixture(t, `["m1"]`)
				e.exec("UPDATE cross_checks SET lead_run_id=$2 WHERE id=$1", ccID, other.runID)
			case "wrong_stage":
				// Point V1 and selection at an actual code-stage row, not an SQL failure.
				id := uuid.New()
				e.exec(`INSERT INTO cross_checks(id,lead_run_id,stage,round,lead_claim_generation,verdict,outcome,reason_class,
					code_context,guidance_snapshot,guidance_text,guidance_digest,repo_instructions_text,repo_instructions_digest,created_at,deadline_at)
					VALUES($1,$2,'code',1,1,'failed','failed','snapshot_failed','{}','','',sha256(''::bytea),'',sha256(''::bytea),now(),now())`, id, e.runID)
				e.exec(`UPDATE runs SET policy_candidate_cross_check_id=$2::uuid,policy_revision_source=jsonb_set(policy_revision_source,
					'{associated_cross_checks,0,cross_check_id}',to_jsonb($2::uuid::text)) WHERE id=$1`, e.runID, id)
			case "wrong_content":
				e.exec("UPDATE cross_checks SET plan_md='different candidate' WHERE id=$1", ccID)
			case "wrong_projection":
				proof := review.Manifest.CrossCheckFeedback[0].Candidate
				candidate := PlanCrossCheckCandidate{PlanMd: "different candidate", Milestones: proof.Milestones,
					RequiredCapabilities: proof.RequiredCapabilities, RequiredTools: proof.RequiredTools,
					SizeClass: *proof.SizeClass, BaseCommit: *proof.BaseCommit, PlanningDiff: *proof.PlanningDiff}
				digest, err := candidate.Digest()
				if err != nil {
					t.Fatal(err)
				}
				e.exec("UPDATE cross_checks SET plan_md=$2,candidate_digest=$3 WHERE id=$1", ccID, candidate.PlanMd, digest)
			case "wrong_digest":
				e.exec("UPDATE cross_checks SET candidate_digest=$2 WHERE id=$1", ccID, make([]byte, 32))
			}
			before := policyOwnerStopSnapshot(t, e.codexTestEnv, e.runID)
			r := mustRun(t, e.codexTestEnv, e.runID)
			_, err := e.svc.PolicyStopReviewForOwner(e.ctx, e.userID, e.runID, PolicyHoldActionToken(r))
			requirePolicyOwnerStopConflict(t, err, "policy_stop_review_unverifiable")
			_, err = e.svc.SubmitInputWithOptions(e.ctx, e.userID, e.runID, "stop", "", nil, SubmitInputOptions{ActionIfMatch: review.StopIfMatch})
			requirePolicyOwnerStopConflict(t, err, "policy_stop_review_unverifiable")
			if policyOwnerStopSnapshot(t, e.codexTestEnv, e.runID) != before {
				t.Fatal("refused review/confirmation changed full run/input/check snapshot")
			}
		})
	}
}

func TestCrossCheckPolicyAssociationAcceptedStopLiveDB(t *testing.T) {
	for _, cc := range []bool{false, true} {
		t.Run(map[bool]string{false: "human_only_null", true: "human_and_cc"}[cc], func(t *testing.T) {
			e, ccID := associationStopFixture(t, cc)
			review := policyOwnerStopReview(t, e)
			before := mustRun(t, e.codexTestEnv, e.runID)
			if (review.Manifest.CandidateCrossCheckID != nil) != cc {
				t.Fatal("selection nullability mismatch")
			}
			res, err := e.svc.SubmitInputWithOptions(e.ctx, e.userID, e.runID, "stop", "finish only", nil, SubmitInputOptions{ActionIfMatch: review.StopIfMatch})
			if err != nil || res.PolicyStop == nil || !res.Resumed || res.PolicyStop.Disposition != "accepted" || res.PolicyStop.ScopeCeiling != 1 {
				t.Fatalf("accepted Stop=%+v err=%v", res, err)
			}
			if res.PolicyStop.ReviewGeneration != review.Manifest.ReviewGeneration || len(res.PolicyStop.SupersededRevisions) != 1+len(review.Manifest.CrossCheckFeedback) {
				t.Fatal("receipt differs from displayed feedback")
			}
			after := mustRun(t, e.codexTestEnv, e.runID)
			if after.AutoApprove || after.PolicyFreshClaimGeneration.Valid || after.SessionID != before.SessionID ||
				!bytes.Equal(after.MilestonesFrozen, before.MilestonesFrozen) || !bytes.Equal(after.MilestonesCompleted, before.MilestonesCompleted) ||
				!bytes.Equal(after.MilestonesCandidate, before.MilestonesCandidate) || after.PlanMd != before.PlanMd ||
				!bytes.Equal(after.CompletionContract, before.CompletionContract) || after.CompletionContractVersion != before.CompletionContractVersion ||
				after.ContractRevision != before.ContractRevision || after.GateRevision != before.GateRevision || after.GatePresentationID != before.GatePresentationID {
				t.Fatal("Stop changed candidate approval, session, fresh grant or execution accounting")
			}
			for _, h := range review.Manifest.HumanFeedback {
				var body, disposition string
				var applied bool
				inputID, err := strconv.ParseInt(string(h.InputID), 10, 64)
				if err != nil {
					t.Fatal(err)
				}
				if err := e.pool.QueryRow(e.ctx, "SELECT body,disposition,applied_at IS NOT NULL FROM run_user_inputs WHERE id=$1", inputID).Scan(&body, &disposition, &applied); err != nil || body != *h.Body || disposition != "superseded_by_stop" || applied {
					t.Fatalf("human provenance lost: %v", err)
				}
			}
			if cc {
				var disposition string
				var findings []byte
				if err := e.pool.QueryRow(e.ctx, "SELECT revision_disposition,findings FROM cross_checks WHERE id=$1", ccID).Scan(&disposition, &findings); err != nil || disposition != "superseded_by_stop" {
					t.Fatalf("automatic settlement: %v", err)
				}
				canonical, err := canonicalRawJSON(findings)
				if err != nil || !bytes.Equal(canonical, review.Manifest.CrossCheckFeedback[0].Findings) {
					t.Fatal("automatic feedback body changed")
				}
				// Accepted retry uses the immutable row ID even after checker pointer loss.
				if review.Manifest.CrossCheckFeedback[0].CheckerRunID == nil {
					t.Fatal("missing checker deletion control")
				}
				e.exec("DELETE FROM runs WHERE id=$1", *review.Manifest.CrossCheckFeedback[0].CheckerRunID)
			}
			if policyAssociationApproval(t, e) {
				t.Fatal("accepted Stop minted candidate approval")
			}
			baseline := policyOwnerStopSnapshot(t, e.codexTestEnv, e.runID)
			retry, err := e.svc.SubmitInputWithOptions(e.ctx, e.userID, e.runID, "stop", "ignored retry body", nil, SubmitInputOptions{ActionIfMatch: review.StopIfMatch})
			if err != nil || retry.PolicyStop == nil || retry.Resumed || retry.PolicyStop.Disposition != "already_accepted" ||
				retry.PolicyStop.ResolutionID != res.PolicyStop.ResolutionID || retry.PolicyStop.ScopeInputID != res.PolicyStop.ScopeInputID ||
				!reflect.DeepEqual(retry.PolicyStop.SupersededRevisions, res.PolicyStop.SupersededRevisions) {
				t.Fatalf("retry receipt=%+v err=%v", retry, err)
			}
			if policyOwnerStopSnapshot(t, e.codexTestEnv, e.runID) != baseline {
				t.Fatal("retry created another scope/resume/settlement")
			}
		})
	}
}

func TestCrossCheckPolicyAssociationSelectionTokenLiveDB(t *testing.T) {
	e, first := associationStopFixture(t, true)
	second := uuid.New()
	e.exec(`INSERT INTO cross_checks(id,lead_run_id,stage,round,lead_claim_generation,plan_md,milestones,
		required_capabilities,required_tools,size_class,base_commit,planning_diff,candidate_digest,verdict,
		findings,created_at,decided_at,deadline_at,checker_harness)
		SELECT $2,lead_run_id,stage,2,lead_claim_generation,plan_md,milestones,required_capabilities,
		required_tools,size_class,base_commit,planning_diff,candidate_digest,verdict,findings,created_at,
		decided_at,deadline_at,checker_harness FROM cross_checks WHERE id=$1`, first, second)
	e.exec(`UPDATE runs SET policy_revision_source=jsonb_set(policy_revision_source,'{associated_cross_checks}',
		(policy_revision_source->'associated_cross_checks') || jsonb_build_array(jsonb_build_object(
		'source','plan_cross_check','cross_check_id',$2::text))) WHERE id=$1`, e.runID, second.String())
	displayed := policyOwnerStopReview(t, e)
	e.exec("UPDATE runs SET policy_candidate_cross_check_id=$2 WHERE id=$1", e.runID, second)
	current := policyOwnerStopReview(t, e)
	if displayed.StopIfMatch == current.StopIfMatch {
		t.Fatal("changed selected row reused token")
	}
	baseline := policyOwnerStopSnapshot(t, e.codexTestEnv, e.runID)
	_, err := e.svc.SubmitInputWithOptions(e.ctx, e.userID, e.runID, "stop", "", nil, SubmitInputOptions{ActionIfMatch: displayed.StopIfMatch})
	requirePolicyOwnerStopConflict(t, err, "policy_stop_token_stale")
	if policyOwnerStopSnapshot(t, e.codexTestEnv, e.runID) != baseline {
		t.Fatal("stale selection consent mutated database")
	}
}

func TestCrossCheckPolicyAssociationOrdinaryNullLiveDB(t *testing.T) {
	e := newPolicyOwnerStopFixture(t, `["m1"]`)
	review := policyOwnerStopReview(t, e)
	if review.Manifest.CandidateCrossCheckID != nil || review.Manifest.SourceContext.PrimaryRevision != nil ||
		len(review.Manifest.SourceContext.AssociatedCrossChecks) != 0 {
		t.Fatal("ordinary absence control not established")
	}
	res, err := e.svc.SubmitInputWithOptions(e.ctx, e.userID, e.runID, "stop", "", nil, SubmitInputOptions{ActionIfMatch: review.StopIfMatch})
	if err != nil || res.PolicyStop == nil {
		t.Fatalf("ordinary NULL selection refused: %v", err)
	}
}

func TestCrossCheckPolicyPendingApprovalInertLiveDB(t *testing.T) {
	for _, mode := range []string{"pending_revision", "pending_approval", "ordinary_approval"} {
		t.Run(mode, func(t *testing.T) {
			constrained := mode != "ordinary_approval"
			e := seedPolicyRevisionApprovalClaim(t, false, 2, constrained)
			if mode != "pending_revision" {
				// Seed a finished revision and complete no-revision coverage only for
				// this component fixture. This is not a runtime source-writer proof.
				e.exec("UPDATE run_user_inputs SET applied_at=now() WHERE run_id=$1 AND kind='revise_plan'", e.runID)
				// Keep the proposed execution definitions identical to the frozen
				// contract. This case requests approval, not owner reconciliation.
				e.exec("UPDATE runs SET milestones_candidate=milestones_frozen WHERE id=$1", e.runID)
				seedPolicyOwnerStopSource(t, e.codexTestEnv, e.runID)
			}
			e.exec("UPDATE runs SET status='awaiting_approval' WHERE id=$1", e.runID)
			before := mustRun(t, e.codexTestEnv, e.runID)
			snapshot := policyOwnerStopSnapshot(t, e.codexTestEnv, e.runID)
			_, err := e.svc.SubmitInputWithOptions(e.ctx, e.userID, e.runID, "approve_plan", "", nil, SubmitInputOptions{ExpectedGateRevision: i64p(3)})
			if mode == "pending_revision" {
				if !errors.Is(err, ErrPlanRevisionPending) {
					t.Fatalf("pending revision admission=%v", err)
				}
				if policyOwnerStopSnapshot(t, e.codexTestEnv, e.runID) != snapshot {
					t.Fatal("refused approval mutated pending revision")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var historical int64
			if err := e.pool.QueryRow(e.ctx, "SELECT min(id) FROM run_user_inputs WHERE run_id=$1 AND kind='approve_plan' AND applied_at IS NOT NULL", e.runID).Scan(&historical); err != nil {
				t.Fatal(err)
			}
			if _, err := e.svc.ApplyInputs(e.ctx, e.wkr, e.runID, 1, []int64{historical}); err != nil {
				t.Fatal(err)
			}
			if _, err := e.svc.Heartbeat(e.ctx, e.wkr, nil, nil, nil); err != nil {
				t.Fatal(err)
			}
			after := mustRun(t, e.codexTestEnv, e.runID)
			if !bytes.Equal(before.PolicyRevisionSource, after.PolicyRevisionSource) || !bytes.Equal(before.MilestonesCandidate, after.MilestonesCandidate) || before.PlanMd != after.PlanMd {
				t.Fatal("enqueue/heartbeat/historical retry changed source or candidate")
			}
			var pending int
			if err := e.pool.QueryRow(e.ctx, "SELECT count(*) FROM run_user_inputs WHERE run_id=$1 AND kind='approve_plan' AND applied_at IS NULL", e.runID).Scan(&pending); err != nil || pending != 1 {
				t.Fatalf("approval enqueue became APPLIED: %d %v", pending, err)
			}
			if approved := policyAssociationApproval(t, e); approved == constrained {
				t.Fatalf("approval authority changed: approved=%v constrained=%v", approved, constrained)
			}
		})
	}
}

// The source context remains seeded, but the independent approval is produced
// through owner Submit plus actual worker ACK/APPLIED and public Claim/running.
// This component does not exercise stopped running/completion SQL or worker turns.
func TestCrossCheckPolicyAcceptedStopPreservesAppliedApprovalLiveDB(t *testing.T) {
	e := policyOwnerRunningContinuation(t)
	run := mustRun(t, e.codexTestEnv, e.runID)
	if !policyAssociationApproval(t, e) {
		t.Fatal("missing independently applied approval")
	}
	req := policyRequest()
	req.OutcomeID = uuid.NewString()
	correlation := "pr-" + strings.ReplaceAll(uuid.NewString(), "-", "") + "-1"
	req.CorrelationID = &correlation
	req.SourceGeneration, req.ClaimGeneration = run.ClaimGeneration, run.ClaimGeneration
	phase := "implementation"
	req.Phase = &phase
	if _, _, err := e.svc.ReportPolicyHold(e.ctx, e.wkr, e.runID, req); err != nil {
		t.Fatal(err)
	}
	seedPolicyOwnerStopSource(t, e.codexTestEnv, e.runID)
	before := mustRun(t, e.codexTestEnv, e.runID)
	review := policyOwnerStopReview(t, e)
	result, err := e.svc.SubmitInputWithOptions(e.ctx, e.userID, e.runID, "stop", "finish completed scope", nil, SubmitInputOptions{ActionIfMatch: review.StopIfMatch})
	if err != nil || result.PolicyStop == nil || !result.Resumed || result.PolicyStop.Disposition != "accepted" {
		t.Fatalf("ordinary accepted Stop=%+v err=%v", result, err)
	}
	after := mustRun(t, e.codexTestEnv, e.runID)
	if !policyAssociationApproval(t, e) {
		t.Fatal("Stop erased independent approval")
	}
	if after.AutoApprove || after.PolicyFreshClaimGeneration.Valid || after.SessionID != before.SessionID ||
		!bytes.Equal(after.MilestonesFrozen, before.MilestonesFrozen) || !bytes.Equal(after.MilestonesCandidate, before.MilestonesCandidate) ||
		!bytes.Equal(after.MilestonesCompleted, before.MilestonesCompleted) || !bytes.Equal(after.CompletionContract, before.CompletionContract) ||
		after.CompletionContractVersion != before.CompletionContractVersion || after.ContractRevision != before.ContractRevision ||
		after.GateRevision != before.GateRevision || after.GatePresentationID != before.GatePresentationID || after.PlanMd != before.PlanMd {
		t.Fatal("accepted Stop changed independent approval or execution contract")
	}
	baseline := policyOwnerStopSnapshot(t, e.codexTestEnv, e.runID)
	retry, err := e.svc.SubmitInputWithOptions(e.ctx, e.userID, e.runID, "stop", "retry", nil, SubmitInputOptions{ActionIfMatch: review.StopIfMatch})
	if err != nil || retry.PolicyStop == nil || retry.PolicyStop.Disposition != "already_accepted" || retry.PolicyStop.ResolutionID != result.PolicyStop.ResolutionID ||
		policyOwnerStopSnapshot(t, e.codexTestEnv, e.runID) != baseline {
		t.Fatalf("ordinary accepted retry changed evidence: %+v %v", retry, err)
	}
}

// Exercise the same approval decision used by claim assembly, under a run lock
// with its transaction-bound reader. Queued owner Stop is not an active Codex
// credential lease; this helper neither fabricates a claim nor spends one.
func policyAssociationApproval(t *testing.T, e usageTailEnv) bool {
	t.Helper()
	tx, err := e.pool.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(e.ctx) }()
	q := store.New(tx)
	run, err := q.GetRunByIDForUpdate(e.ctx, e.runID)
	if err != nil {
		t.Fatal(err)
	}
	claimContext, err := q.GetRunClaimContext(e.ctx, e.runID)
	if err != nil {
		t.Fatal(err)
	}
	return policyRevisionPlanApproved(e.ctx, q, run, run.AutoApprove || claimContext.HumanPlanApproved || run.PlanSource == planSourceSeeded)
}

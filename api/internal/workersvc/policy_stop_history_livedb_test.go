package workersvc

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/settings"
)

// Public issue birth, running plan/progress and publication establish the scope
// and source. Ownership/admission are fixture bridges. Go emits SIMULATED worker
// carriers: this does not exercise worker composition or worker delivery.
func stopHistoryRunningFixture(t *testing.T) (codexTestEnv, *gateFix, uuid.UUID) {
	t.Helper()
	e, f, old, _ := acceptanceUnpublishedFixture(t)
	f.svc.p.PolicyStopNegotiated = true
	f.svc.p.PlanMaxRevisions = 10
	resetInterlockSetting(t, e)
	f.svc.SetCompletionInterlockSettings(settings.New(e.q, time.Minute))
	repo := uuid.UUID(mustRun(t, e, old).RepoID.Bytes)
	seedEligibleIssue(t, e, repo, 9472)
	r, err := f.svc.CreateRun(e.ctx, f.userID, repo, 9472, "history Stop", nil, nil, false, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	e.exec("UPDATE runs SET worker_id=$2,status='claimed',claim_generation=1,auto_approve=true,plan_cross_check_required=false WHERE id=$1", r.ID, f.wkr.ID)
	ms := []Milestone{{ID: "m1", Title: "one"}, {ID: "m2", Title: "two"}}
	plan := "# Execution plan"
	autopilotRevisionReport(t, f, r.ID, StateRequest{State: "running", ClaimGeneration: i64(1), PlanMd: &plan, Milestones: &ms, SessionID: strPtr("history-session")})
	completed := []string{"m1"}
	running := autopilotRevisionReport(t, f, r.ID, StateRequest{State: "running", ClaimGeneration: i64(1), MilestonesCompleted: &completed})
	if !running.CompletionContractVersion.Valid || len(running.MilestonesFrozen) == 0 {
		t.Fatal("public running failed to freeze execution scope")
	}
	// The subsequent candidate is deliberately unapproved.
	e.exec("UPDATE runs SET auto_approve=false WHERE id=$1", r.ID)
	return e, f, r.ID
}

func stopHistoryFixture(t *testing.T) (codexTestEnv, *gateFix, uuid.UUID) {
	t.Helper()
	e, f, run := stopHistoryRunningFixture(t)
	f.mustPublish(run, acceptanceGateRequest(), 1)
	return e, f, run
}

func stopHistoryInput(t *testing.T, e codexTestEnv, f *gateFix, run uuid.UUID, body string, take bool) int64 {
	t.Helper()
	r := mustRun(t, e, run)
	input, err := f.svc.SubmitInputWithOptions(e.ctx, f.userID, run, "revise_plan", body, nil, SubmitInputOptions{ExpectedGateRevision: &r.GateRevision})
	if err != nil {
		t.Fatal(err)
	}
	acceptanceAck(t, f, run, input.ID)
	if take {
		source := acceptanceSource(t, e, run)
		err := f.svc.AppendMessagesForClaim(e.ctx, f.wkr, run, []IncomingMessage{{Seq: mustRun(t, e, run).LastSeq + 1, Kind: "plan_revising", Payload: revisionWriterPayload(t, source, revisionWriterHumanRef(input.ID))}}, i64(1))
		if err != nil {
			t.Fatal(err)
		}
		if got := acceptanceSource(t, e, run).PrimaryRevision; got == nil || got.InputID == nil || *got.InputID != stopDecimal(input.ID) {
			t.Fatal("real carrier writer lost human primary")
		}
	}
	return input.ID
}

func stopHistoryPublish(t *testing.T, e codexTestEnv, f *gateFix, run uuid.UUID, active int64, predecessors ...int64) {
	t.Helper()
	gate := mustRun(t, e, run).GateRevision
	req := acceptanceGateRequest()
	req.PlanMd = strPtr("# Replacement candidate")
	req.ExpectedGateRevision, req.RevisionInputID, req.SupersededRevisionInputIDs = &gate, &active, predecessors
	f.mustPublish(run, req, gate+1)
	if acceptanceSource(t, e, run).PrimaryRevision == nil {
		t.Fatal("publication accepted the revision")
	}
}

func stopHistoryHold(t *testing.T, e codexTestEnv, f *gateFix, run uuid.UUID) apitypes.PolicyStopReview {
	t.Helper()
	req := policyRequest()
	req.Phase = strPtr("implementation")
	req.ExpectedGateRevision = i64(mustRun(t, e, run).GateRevision)
	if _, disposition, err := f.svc.ReportPolicyHold(e.ctx, f.wkr, run, req); err != nil || disposition != PolicyHoldApplied {
		t.Fatalf("hold %s: %v", disposition, err)
	}
	before := policyOwnerStopSnapshot(t, e, run)
	review, err := f.svc.PolicyStopReviewForOwner(e.ctx, f.userID, run, PolicyHoldActionToken(mustRun(t, e, run)))
	if err != nil {
		t.Fatal(err)
	}
	if policyOwnerStopSnapshot(t, e, run) != before {
		t.Fatal("preview wrote state")
	}
	return review
}

func stopHistoryRows(t *testing.T, e codexTestEnv, run uuid.UUID, ids ...int64) string {
	t.Helper()
	var result string
	if err := e.pool.QueryRow(e.ctx, "SELECT jsonb_agg(to_jsonb(i) ORDER BY id)::text FROM run_user_inputs i WHERE run_id=$1 AND id=ANY($2::bigint[])", run, ids).Scan(&result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestPolicyStopHistoryPublicationLiveDB(t *testing.T) {
	for _, mode := range []string{"pending", "published", "second_carrier"} {
		t.Run(mode, func(t *testing.T) {
			e, f, run := stopHistoryFixture(t)
			var predecessor, spare int64
			if mode != "pending" {
				spare = stopHistoryInput(t, e, f, run, "older nonprimary still pending", false)
				predecessor = stopHistoryInput(t, e, f, run, "predecessor body", false)
			}
			active := stopHistoryInput(t, e, f, run, "active body", true)
			pending := active
			var history string
			if mode != "pending" {
				stopHistoryPublish(t, e, f, run, active, predecessor)
				history = stopHistoryRows(t, e, run, spare, predecessor, active)
				pending = stopHistoryInput(t, e, f, run, "current unrelated feedback", mode == "second_carrier")
			}
			review := stopHistoryHold(t, e, f, run)
			want := 1
			if mode != "pending" {
				want = 3
			}
			if len(review.Manifest.HumanFeedback) != want {
				t.Fatalf("feedback=%+v", review.Manifest.HumanFeedback)
			}
			if mode != "pending" {
				if review.Manifest.HumanFeedback[0].Disposition == nil || *review.Manifest.HumanFeedback[0].Disposition != "superseded" ||
					review.Manifest.HumanFeedback[1].AppliedAt == nil || review.Manifest.HumanFeedback[1].Body == nil || *review.Manifest.HumanFeedback[1].Body != "active body" {
					t.Fatal("history disclosure lost provenance/body")
				}
			}
			before := mustRun(t, e, run)
			if policyRevisionPlanApproved(e.ctx, e.q, before, true) {
				t.Fatal("unapproved candidate gained approval")
			}
			result, err := f.svc.SubmitInputWithOptions(e.ctx, f.userID, run, "stop", "finalize", nil, SubmitInputOptions{ActionIfMatch: review.StopIfMatch})
			if err != nil || result.PolicyStop == nil {
				t.Fatalf("Stop=%+v err=%v", result, err)
			}
			refs := result.PolicyStop.SupersededRevisions
			if len(refs) != 1 || refs[0].InputID == nil || *refs[0].InputID != stopDecimal(pending) {
				t.Fatalf("waived history: %+v", refs)
			}
			if mode != "pending" && stopHistoryRows(t, e, run, spare, predecessor, active) != history {
				t.Fatal("Stop rewrote #3001 history")
			}
			var disposition string
			var applied bool
			if err := e.pool.QueryRow(e.ctx, "SELECT disposition,applied_at IS NOT NULL FROM run_user_inputs WHERE id=$1", pending).Scan(&disposition, &applied); err != nil || disposition != "superseded_by_stop" || applied {
				t.Fatalf("pending settlement=%s applied=%v: %v", disposition, applied, err)
			}
			after := mustRun(t, e, run)
			if !bytes.Equal(after.PolicyRevisionSource, before.PolicyRevisionSource) || after.PolicyCandidateCrossCheckID != before.PolicyCandidateCrossCheckID ||
				after.GateRevision != before.GateRevision || !bytes.Equal(after.GatePresentedPayload, before.GatePresentedPayload) ||
				!bytes.Equal(after.MilestonesFrozen, before.MilestonesFrozen) || !bytes.Equal(after.CompletionContract, before.CompletionContract) {
				t.Fatal("Stop changed source/gate/selection/execution")
			}
			if policyRevisionPlanApproved(e.ctx, e.q, after, true) {
				t.Fatal("Stop approved candidate")
			}
			// Exact retry reads the immutable cohort even after terminal/mutable changes.
			e.exec("UPDATE runs SET status='failed',plan_md=NULL,gate_revision=99 WHERE id=$1", run)
			snapshot := policyOwnerStopSnapshot(t, e, run)
			retry, err := f.svc.SubmitInputWithOptions(e.ctx, f.userID, run, "stop", "", nil, SubmitInputOptions{ActionIfMatch: review.StopIfMatch})
			if err != nil || retry.PolicyStop == nil || retry.PolicyStop.Disposition != "already_accepted" ||
				len(retry.PolicyStop.SupersededRevisions) != 1 || *retry.PolicyStop.SupersededRevisions[0].InputID != stopDecimal(pending) {
				t.Fatalf("retry=%+v %v", retry, err)
			}
			if snapshot != policyOwnerStopSnapshot(t, e, run) {
				t.Fatal("retry wrote a grant or cohort")
			}
		})
	}
}

// This control uses actual running plan/progress and ordinary approval receipts.
// It stops at API acceptance; it does not execute SQL Stop or compose a worker.
func TestPolicyStopHistoryOrdinaryApprovalLiveDB(t *testing.T) {
	e, f, run := stopHistoryRunningFixture(t)
	execution := mustRun(t, e, run)
	ms := []Milestone{{ID: "m1", Title: "one"}, {ID: "m2", Title: "two"}}
	first := acceptanceGateRequest()
	first.Milestones = &ms
	f.mustPublish(run, first, 1)
	active := stopHistoryInput(t, e, f, run, "ordinary revised candidate", true)
	replacement := acceptanceGateRequest()
	replacement.PlanMd = strPtr("# Ordinary replacement")
	replacement.Milestones = &ms
	replacement.ExpectedGateRevision, replacement.RevisionInputID = i64(1), &active
	f.mustPublish(run, replacement, 2)
	acceptanceApply(t, f, run, active)
	approval, err := f.svc.SubmitInputWithOptions(e.ctx, f.userID, run, "approve_plan", "", nil,
		SubmitInputOptions{ExpectedGateRevision: i64(2)})
	if err != nil {
		t.Fatal(err)
	}
	acceptanceAck(t, f, run, approval.ID)
	acceptanceApply(t, f, run, approval.ID)
	if acceptanceSource(t, e, run).PrimaryRevision != nil {
		t.Fatal("ordinary applied approval did not clear human primary")
	}
	history := acceptanceHistory(t, e, run)
	autopilotRevisionReport(t, f, run, StateRequest{State: "running", ClaimGeneration: i64(1)})
	review := stopHistoryHold(t, e, f, run)
	before := mustRun(t, e, run)
	if len(review.Manifest.HumanFeedback) != 1 || review.Manifest.HumanFeedback[0].AppliedAt == nil ||
		review.Manifest.SourceContext.PrimaryRevision != nil {
		t.Fatalf("ordinary applied history not disclosed: %+v", review.Manifest)
	}
	if !policyRevisionPlanApproved(e.ctx, e.q, before, true) {
		t.Fatal("current ordinary approval lost before Stop")
	}
	result, err := f.svc.SubmitInputWithOptions(e.ctx, f.userID, run, "stop", "", nil,
		SubmitInputOptions{ActionIfMatch: review.StopIfMatch})
	if err != nil || result.PolicyStop == nil {
		t.Fatalf("Stop=%+v err=%v", result, err)
	}
	if len(result.PolicyStop.SupersededRevisions) != 0 {
		t.Fatalf("applied history became a waiver: %+v", result.PolicyStop)
	}
	after := mustRun(t, e, run)
	if !policyRevisionPlanApproved(e.ctx, e.q, after, true) {
		t.Fatal("accepted applied history erased independent current approval")
	}
	if acceptanceHistory(t, e, run) != history ||
		!bytes.Equal(after.PolicyRevisionSource, before.PolicyRevisionSource) ||
		after.PlanMd != before.PlanMd || after.GateRevision != before.GateRevision ||
		!bytes.Equal(after.GatePresentedPayload, before.GatePresentedPayload) ||
		!bytes.Equal(after.MilestonesFrozen, execution.MilestonesFrozen) ||
		!bytes.Equal(after.MilestonesCompleted, execution.MilestonesCompleted) ||
		!bytes.Equal(after.CompletionContract, execution.CompletionContract) ||
		after.CompletionContractVersion != execution.CompletionContractVersion ||
		after.ContractRevision != execution.ContractRevision {
		t.Fatal("ordinary Stop changed history, approval provenance or frozen execution")
	}
}

func TestPolicyStopHistoryPrimaryDeliveryLiveDB(t *testing.T) {
	e, f, run := stopHistoryFixture(t)
	active := stopHistoryInput(t, e, f, run, "taken primary", true)
	e.exec("UPDATE run_user_inputs SET consumed_at=NULL,consumed_worker_id=NULL,consumed_claim_generation=NULL WHERE id=$1", active)
	f.svc.p.PolicyStopNegotiated = true
	req := policyRequest()
	req.ExpectedGateRevision = i64(mustRun(t, e, run).GateRevision)
	_, disposition, err := f.svc.ReportPolicyHold(e.ctx, f.wkr, run, req)
	if err != nil || disposition != PolicyHoldApplied {
		t.Fatalf("hold=%s err=%v", disposition, err)
	}
	before := policyOwnerStopSnapshot(t, e, run)
	_, err = f.svc.PolicyStopReviewForOwner(e.ctx, f.userID, run, PolicyHoldActionToken(mustRun(t, e, run)))
	if err == nil || policyOwnerStopSnapshot(t, e, run) != before {
		t.Fatalf("undelivered primary review err=%v", err)
	}
}

func TestPolicyStopHistoryRefusesCorruptionLiveDB(t *testing.T) {
	for _, mode := range []string{"source", "receipt_active", "receipt_origin", "receipt_unknown", "receipt_order", "missing_predecessor", "foreign_predecessor", "future_binding", "unbound_nonprimary", "included_history"} {
		t.Run(mode, func(t *testing.T) {
			e, f, run := stopHistoryFixture(t)
			predecessor := stopHistoryInput(t, e, f, run, "predecessor", false)
			active := stopHistoryInput(t, e, f, run, "active", true)
			stopHistoryPublish(t, e, f, run, active, predecessor)
			pending := stopHistoryInput(t, e, f, run, "pending", false)
			_ = stopHistoryHold(t, e, f, run)
			switch mode {
			case "source":
				e.exec("UPDATE runs SET policy_revision_source=NULL WHERE id=$1", run)
			case "receipt_active":
				e.exec("UPDATE runs SET gate_presented_payload=jsonb_set(gate_presented_payload,'{revision_retry_receipt,revision_input_id}',to_jsonb($2::bigint)) WHERE id=$1", run, active+100000)
			case "receipt_origin":
				e.exec("UPDATE runs SET gate_presented_payload=jsonb_set(gate_presented_payload,'{revision_retry_receipt,source_revision}','2') WHERE id=$1", run)
			case "receipt_unknown":
				e.exec(`UPDATE runs SET gate_presented_payload=jsonb_set(gate_presented_payload,'{revision_retry_receipt,unknown}','true') WHERE id=$1`, run)
			case "receipt_order":
				e.exec("UPDATE runs SET gate_presented_payload=jsonb_set(gate_presented_payload,'{revision_retry_receipt,superseded_revision_input_ids}',to_jsonb($2::bigint[])) WHERE id=$1", run, []int64{predecessor, predecessor})
			case "missing_predecessor":
				e.exec("DELETE FROM run_user_inputs WHERE id=$1", predecessor)
			case "foreign_predecessor":
				other, err := f.svc.CreateTaskRun(e.ctx, f.userID, uuid.UUID(mustRun(t, e, run).RepoID.Bytes), "foreign", "", false, false, false, false, nil)
				if err != nil {
					t.Fatal(err)
				}
				e.exec("UPDATE run_user_inputs SET run_id=$2 WHERE id=$1", predecessor, other.ID)
			case "future_binding":
				e.exec("UPDATE run_user_inputs SET gate_revision=99 WHERE id=$1", pending)
			case "unbound_nonprimary":
				e.exec("UPDATE run_user_inputs SET gate_revision=NULL,gate_binding='unbound' WHERE id=$1", pending)
			case "included_history":
				e.exec("UPDATE run_user_inputs SET included_at=now(),inclusion_reported=true WHERE id=$1", active)
			}
			before := policyOwnerStopSnapshot(t, e, run)
			_, err := f.svc.PolicyStopReviewForOwner(e.ctx, f.userID, run, PolicyHoldActionToken(mustRun(t, e, run)))
			requirePolicyOwnerStopConflict(t, err, "policy_stop_review_unverifiable")
			if policyOwnerStopSnapshot(t, e, run) != before {
				t.Fatal("failed proof mutated state")
			}
		})
	}
}

func TestPolicyStopHistoryDistinctCandidatesLiveDB(t *testing.T) {
	for _, verdict := range []string{"revise", "approve"} {
		t.Run(verdict, func(t *testing.T) {
			e, f, old, _ := acceptanceUnpublishedFixture(t)
			f.svc.p.PolicyStopNegotiated = true
			f.svc.p.PlanMaxRevisions = 10
			repo := uuid.UUID(mustRun(t, e, old).RepoID.Bytes)
			seedEligibleIssue(t, e, repo, 9473)
			created, err := f.svc.CreateRun(e.ctx, f.userID, repo, 9473, "checker history component", nil, nil, false, nil, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			run := created.ID
			// Explicit legacy execution-scope precondition for this checker-history
			// component. No plan has been persisted: both Submits obey initial
			// admission. This is not a composed worker initial-planning trace.
			e.exec(`UPDATE runs SET worker_id=$2,status='running',claim_generation=1,
 auto_approve=true,plan_cross_check_required=true,session_id='checker-history-session',
 milestones_frozen='[{"id":"m1","title":"one"},{"id":"m2","title":"two"}]',
 milestones_completed='["m1"]',completion_contract_version=NULL,contract_revision=NULL,completion_contract=NULL
 WHERE id=$1`, run, f.wkr.ID)
			if initial := mustRun(t, e, run); initial.PlanMd.Valid || initial.GateRevision != 0 {
				t.Fatal("checker fixture persisted a plan before initial Submit")
			}
			c := PlanCrossCheckCandidate{PlanMd: "# Historical candidate A", Milestones: []byte("[]"), RequiredCapabilities: []string{}, RequiredTools: []string{}, SizeClass: "s", BaseCommit: strings.Repeat("a", 40)}
			first, err := f.svc.SubmitPlanCrossCheck(e.ctx, f.wkr, run, 1, c, 1)
			if err != nil {
				t.Fatal(err)
			}
			autopilotDecide(t, e, f, first, "revise")
			firstID := first.ID.String()
			source := acceptanceSource(t, e, run)
			if err := f.svc.AppendMessagesForClaim(e.ctx, f.wkr, run, []IncomingMessage{{Seq: mustRun(t, e, run).LastSeq + 1, Kind: "plan_revising", Payload: revisionWriterPayload(t, source, apitypes.PolicyRevisionRef{Source: "plan_cross_check", CrossCheckID: &firstID})}}, i64(1)); err != nil {
				t.Fatal(err)
			}
			c.PlanMd = "# Selected candidate B"
			second, err := f.svc.SubmitPlanCrossCheck(e.ctx, f.wkr, run, 1, c, 2)
			if err != nil {
				t.Fatal(err)
			}
			autopilotDecide(t, e, f, second, verdict)
			req := acceptanceGateRequest()
			req.PlanMd = &c.PlanMd
			f.mustPublish(run, req, 1)
			if verdict == "revise" {
				source = acceptanceSource(t, e, run)
				secondID := second.ID.String()
				if err := f.svc.AppendMessagesForClaim(e.ctx, f.wkr, run, []IncomingMessage{{Seq: mustRun(t, e, run).LastSeq + 1, Kind: "plan_revising", Payload: revisionWriterPayload(t, source, apitypes.PolicyRevisionRef{Source: "plan_cross_check", CrossCheckID: &secondID})}}, i64(1)); err != nil {
					t.Fatal(err)
				}
				got := acceptanceSource(t, e, run)
				if got.PrimaryRevision == nil || got.PrimaryRevision.CrossCheckID == nil || *got.PrimaryRevision.CrossCheckID != secondID {
					t.Fatal("historical A invalidated B carrier")
				}
			} else {
				stopHistoryInput(t, e, f, run, "human revision over approved witness", true)
			}
			review := stopHistoryHold(t, e, f, run)
			for _, mode := range []string{"historical_digest", "selected_generation", "historical_future", "foreign", "dangling_selection"} {
				t.Run(mode, func(t *testing.T) {
					firstRow, err := e.q.GetPolicyStopCrossCheck(e.ctx, first.ID)
					if err != nil {
						t.Fatal(err)
					}
					secondRow, err := e.q.GetPolicyStopCrossCheck(e.ctx, second.ID)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() {
						e.exec("UPDATE cross_checks SET candidate_digest=$2,lead_claim_generation=$3,lead_run_id=$4 WHERE id=$1", first.ID, firstRow.CandidateDigest, firstRow.LeadClaimGeneration, firstRow.LeadRunID)
						e.exec("UPDATE cross_checks SET lead_claim_generation=$2 WHERE id=$1", second.ID, secondRow.LeadClaimGeneration)
						e.exec("UPDATE runs SET policy_candidate_cross_check_id=$2 WHERE id=$1", run, second.ID)
					})
					switch mode {
					case "historical_digest":
						e.exec("UPDATE cross_checks SET candidate_digest=$2 WHERE id=$1", first.ID, bytes.Repeat([]byte{42}, 32))
					case "selected_generation":
						e.exec("UPDATE cross_checks SET lead_claim_generation=0 WHERE id=$1", second.ID)
					case "historical_future":
						e.exec("UPDATE cross_checks SET lead_claim_generation=2 WHERE id=$1", first.ID)
					case "foreign":
						foreign, err := f.svc.CreateTaskRun(e.ctx, f.userID, uuid.UUID(mustRun(t, e, run).RepoID.Bytes), "foreign checker evidence", "", false, false, false, false, nil)
						if err != nil {
							t.Fatal(err)
						}
						e.exec("UPDATE cross_checks SET lead_run_id=$2 WHERE id=$1", first.ID, foreign.ID)
					case "dangling_selection":
						e.exec("UPDATE runs SET policy_candidate_cross_check_id=$2 WHERE id=$1", run, uuid.New())
					}
					before := policyOwnerStopSnapshot(t, e, run)
					_, err = f.svc.PolicyStopReviewForOwner(e.ctx, f.userID, run, PolicyHoldActionToken(mustRun(t, e, run)))
					requirePolicyOwnerStopConflict(t, err, "policy_stop_review_unverifiable")
					if before != policyOwnerStopSnapshot(t, e, run) {
						t.Fatal("corrupt checker proof wrote state")
					}
				})
			}
			if len(review.Manifest.CrossCheckFeedback) != 2 {
				t.Fatal("finite history missing")
			}
			for _, cc := range review.Manifest.CrossCheckFeedback {
				want := "# Historical candidate A"
				if cc.CrossCheckID == second.ID.String() {
					want = "# Selected candidate B"
				}
				if cc.Candidate.PlanMd == nil || *cc.Candidate.PlanMd != want {
					t.Fatal("substituted candidate body")
				}
				if cc.Supersede != (cc.Verdict == "revise") {
					t.Fatal("incorrect checker waiver")
				}
			}
			// Generation advancement is an admission bridge, not a public Claim proof.
			// The original selected/checker provenance must survive under the new claim.
			e.exec("UPDATE runs SET claim_generation=2 WHERE id=$1", run)
			beforeReclaimReview := policyOwnerStopSnapshot(t, e, run)
			_, err = f.svc.SubmitInputWithOptions(e.ctx, f.userID, run, "stop", "", nil, SubmitInputOptions{ActionIfMatch: review.StopIfMatch})
			requirePolicyOwnerStopConflict(t, err, "policy_stop_token_stale")
			if beforeReclaimReview != policyOwnerStopSnapshot(t, e, run) {
				t.Fatal("stale pre-reclaim consent wrote state")
			}
			review, err = f.svc.PolicyStopReviewForOwner(e.ctx, f.userID, run, PolicyHoldActionToken(mustRun(t, e, run)))
			if err != nil || review.Manifest.ReviewGeneration != "2" {
				t.Fatalf("original provenance under new claim: %+v %v", review, err)
			}
			for _, cc := range review.Manifest.CrossCheckFeedback {
				if cc.CandidateGeneration != "1" {
					t.Fatal("reclaim rewrote original checker provenance")
				}
			}
			result, err := f.svc.SubmitInputWithOptions(e.ctx, f.userID, run, "stop", "", nil, SubmitInputOptions{ActionIfMatch: review.StopIfMatch})
			if err != nil || result.PolicyStop == nil {
				t.Fatalf("Stop %+v: %v", result, err)
			}
			var waived int
			if err := e.pool.QueryRow(e.ctx, "SELECT count(*) FROM cross_checks WHERE lead_run_id=$1 AND revision_disposition='superseded_by_stop'", run).Scan(&waived); err != nil {
				t.Fatal(err)
			}
			want := 2
			if verdict == "approve" {
				want = 1
			}
			if waived != want {
				t.Fatalf("waived=%d want=%d", waived, want)
			}
		})
	}
}

func TestPolicyStopHistoryStaleConfirmationLiveDB(t *testing.T) {
	for _, mode := range []string{"cohort", "source", "gate", "selection", "history_body"} {
		t.Run(mode, func(t *testing.T) {
			e, f, run := stopHistoryFixture(t)
			active := stopHistoryInput(t, e, f, run, "active", true)
			stopHistoryPublish(t, e, f, run, active)
			review := stopHistoryHold(t, e, f, run)
			switch mode {
			case "cohort":
				// Like the persisted-context mutations below, inject this component
				// precondition explicitly: bound public verdicts correctly refuse
				// a paused run. This is not proof of a live feedback delivery path.
				gate := mustRun(t, e, run).GateRevision
				e.exec(`WITH bumped AS (
 UPDATE runs SET revise_count=revise_count+1 WHERE id=$1 RETURNING id
) INSERT INTO run_user_inputs(run_id,kind,body,gate_binding,gate_revision)
 SELECT id,'revise_plan','new current cohort','bound',$2 FROM bumped`, run, gate)
				renewed, err := f.svc.PolicyStopReviewForOwner(e.ctx, f.userID, run, PolicyHoldActionToken(mustRun(t, e, run)))
				if err != nil || renewed.StopIfMatch == review.StopIfMatch || len(renewed.Manifest.HumanFeedback) != len(review.Manifest.HumanFeedback)+1 {
					t.Fatalf("new bound cohort did not produce a changed complete review: %v", err)
				}
			case "source":
				e.exec("UPDATE runs SET policy_revision_source=NULL WHERE id=$1", run)
			case "gate":
				e.exec("UPDATE runs SET gate_revision=gate_revision+1 WHERE id=$1", run)
			case "selection":
				e.exec("UPDATE runs SET policy_candidate_cross_check_id=$2 WHERE id=$1", run, uuid.New())
			case "history_body":
				e.exec("UPDATE run_user_inputs SET body='changed history' WHERE id=$1", active)
			}
			before := policyOwnerStopSnapshot(t, e, run)
			_, err := f.svc.SubmitInputWithOptions(e.ctx, f.userID, run, "stop", "", nil, SubmitInputOptions{ActionIfMatch: review.StopIfMatch})
			var conflict *PolicyStopReviewConflict
			if !errors.As(err, &conflict) {
				t.Fatalf("changed context accepted: %v", err)
			}
			if mode == "cohort" && conflict.Reason != "policy_stop_token_stale" {
				t.Fatalf("changed bound cohort did not reject the old review token: %v", err)
			}
			if before != policyOwnerStopSnapshot(t, e, run) {
				t.Fatal("stale confirmation wrote state")
			}
		})
	}
}

// Record actual generated-query identities without assertions in the callback:
// a callback failure can be swallowed when the writer invalidates coverage.
type stopHistoryCountBegin struct {
	pool *pgxpool.Pool
	ids  []int64
}

func (b *stopHistoryCountBegin) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return stopHistoryCountTx{Tx: tx, counter: b}, nil
}

type stopHistoryCountTx struct {
	pgx.Tx
	counter *stopHistoryCountBegin
}

func (tx stopHistoryCountTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if strings.HasPrefix(sql, "-- name: GetPolicyStopRevisionInput :one") {
		tx.counter.ids = append(tx.counter.ids, args[1].(int64))
	}
	return tx.Tx.QueryRow(ctx, sql, args...)
}

func TestPolicyStopHistoryFiniteReadsLiveDB(t *testing.T) {
	for _, mode := range []string{"pending", "published", "invalid_active"} {
		t.Run(mode, func(t *testing.T) {
			e, f, run := stopHistoryFixture(t)
			var predecessor int64
			if mode != "pending" {
				predecessor = stopHistoryInput(t, e, f, run, "exact predecessor", false)
			}
			active := stopHistoryInput(t, e, f, run, "previous human primary", true)
			if mode != "pending" {
				stopHistoryPublish(t, e, f, run, active, predecessor)
			}
			current := stopHistoryInput(t, e, f, run, "second human revision", false)
			if mode == "invalid_active" {
				e.exec("UPDATE run_user_inputs SET included_at=now() WHERE id=$1", active)
			}
			counter := &stopHistoryCountBegin{pool: e.pool}
			f.svc.txBeginner = counter
			source := acceptanceSource(t, e, run)
			err := f.svc.AppendMessagesForClaim(e.ctx, f.wkr, run, []IncomingMessage{{
				Seq: mustRun(t, e, run).LastSeq + 1, Kind: "plan_revising",
				Payload: revisionWriterPayload(t, source, revisionWriterHumanRef(current)),
			}}, i64(1))
			if err != nil {
				t.Fatal(err)
			}
			assertIDs := func(want ...int64) {
				t.Helper()
				counts := map[int64]int{}
				for _, id := range counter.ids {
					counts[id]++
				}
				if len(counter.ids) != len(want) {
					t.Fatalf("exact reads=%v want identities=%v once each", counter.ids, want)
				}
				for i, id := range want {
					if counts[id] != 1 || counter.ids[i] != id {
						t.Fatalf("identity %d read %d times; ordered reads=%v want=%v", id, counts[id], counter.ids, want)
					}
				}
			}
			if mode == "invalid_active" {
				assertIDs(active)
				if len(mustRun(t, e, run).PolicyRevisionSource) != 0 {
					t.Fatal("invalid active proof preserved source coverage")
				}
				return
			}
			if mode == "published" {
				assertIDs(active, predecessor, current)
			} else {
				assertIDs(active, current)
			}
			taken := acceptanceSource(t, e, run).PrimaryRevision
			if taken == nil || taken.InputID == nil || *taken.InputID != stopDecimal(current) {
				t.Fatal("second human revision lost source coverage")
			}
			if !policyRevisionPlanApproved(e.ctx, e.q, mustRun(t, e, run), true) {
				t.Fatal("pre-hold reader lost legacy approval")
			}
			counter.ids = nil
			review := stopHistoryHold(t, e, f, run)
			if policyRevisionPlanApproved(e.ctx, e.q, mustRun(t, e, run), true) {
				t.Fatal("human history approved an unapproved candidate after hold")
			}
			if mode == "published" {
				assertIDs(active, predecessor)
				if len(review.Manifest.HumanFeedback) != 3 {
					t.Fatalf("history disclosure=%+v", review.Manifest.HumanFeedback)
				}
			} else {
				assertIDs()
			}
		})
	}
}

var errStopHistoryReadFailure = errors.New("injected exact history read")

type stopHistoryReadFaultBegin struct{ pool *pgxpool.Pool }

func (b stopHistoryReadFaultBegin) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return stopHistoryReadFaultTx{Tx: tx}, nil
}

type stopHistoryReadFaultTx struct{ pgx.Tx }

func (tx stopHistoryReadFaultTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if strings.HasPrefix(sql, "-- name: GetPolicyStopRevisionInput :one") {
		return stopHistoryReadFaultRow{}
	}
	return tx.Tx.QueryRow(ctx, sql, args...)
}

type stopHistoryReadFaultRow struct{}

func (stopHistoryReadFaultRow) Scan(...any) error { return errStopHistoryReadFailure }

func TestPolicyStopHistoryReadErrorLiveDB(t *testing.T) {
	e, f, run := stopHistoryFixture(t)
	active := stopHistoryInput(t, e, f, run, "active", true)
	stopHistoryPublish(t, e, f, run, active)
	_ = stopHistoryHold(t, e, f, run)
	before := policyOwnerStopSnapshot(t, e, run)
	f.svc.txBeginner = stopHistoryReadFaultBegin{pool: e.pool}
	_, err := f.svc.PolicyStopReviewForOwner(e.ctx, f.userID, run, PolicyHoldActionToken(mustRun(t, e, run)))
	if !errors.Is(err, errStopHistoryReadFailure) {
		t.Fatalf("database error became absence: %v", err)
	}
	if before != policyOwnerStopSnapshot(t, e, run) {
		t.Fatal("failed read mutated snapshot")
	}
}

// Faults wrap the real transaction: a settlement failure happens after the first
// pending row has been updated; commit failure occurs after all writes.
type stopHistoryFaultBegin struct {
	pool   *pgxpool.Pool
	commit bool
}

func (b stopHistoryFaultBegin) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &stopHistoryFaultTx{Tx: tx, commit: b.commit}, nil
}

type stopHistoryFaultTx struct {
	pgx.Tx
	commit      bool
	settlements int
}

func (tx *stopHistoryFaultTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if strings.Contains(sql, "SET disposition = 'superseded_by_stop'") {
		tx.settlements++
		if !tx.commit && tx.settlements == 2 {
			return pgconn.CommandTag{}, errors.New("injected mid-settlement")
		}
	}
	return tx.Tx.Exec(ctx, sql, args...)
}
func (tx *stopHistoryFaultTx) Commit(ctx context.Context) error {
	if tx.commit {
		return errors.New("injected commit")
	}
	return tx.Tx.Commit(ctx)
}

func TestPolicyStopHistoryRollbackLiveDB(t *testing.T) {
	for _, commit := range []bool{false, true} {
		t.Run(map[bool]string{false: "settlement", true: "commit"}[commit], func(t *testing.T) {
			e, f, run := stopHistoryFixture(t)
			stopHistoryInput(t, e, f, run, "first", false)
			stopHistoryInput(t, e, f, run, "second", true)
			review := stopHistoryHold(t, e, f, run)
			before := policyOwnerStopSnapshot(t, e, run)
			f.svc.txBeginner = stopHistoryFaultBegin{pool: e.pool, commit: commit}
			_, err := f.svc.SubmitInputWithOptions(e.ctx, f.userID, run, "stop", "", nil, SubmitInputOptions{ActionIfMatch: review.StopIfMatch})
			if err == nil || !strings.Contains(err.Error(), "injected") {
				t.Fatalf("fault did not reach writes: %v", err)
			}
			if policyOwnerStopSnapshot(t, e, run) != before {
				t.Fatal("failed transaction leaked scope/settlement/acceptance")
			}
		})
	}
}

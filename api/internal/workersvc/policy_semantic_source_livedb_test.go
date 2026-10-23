package workersvc

import (
	"bytes"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/settings"
)

// Public issue creation supplies birth coverage. SQL bridges enrollment, claim
// and checker configuration, plus prior-refusal preconditions in selected cases.
// Candidate, source, plan, approval and acceptance use service APIs; this is not
// worker runtime composition or activation.
func semanticPolicySourceLive(t *testing.T, auto bool) (codexTestEnv, *gateFix, *coverageLiveFixture) {
	t.Helper()
	e, f, old, checked := acceptanceUnpublishedFixture(t)
	resetInterlockSetting(t, e)
	f.svc.SetCompletionInterlockSettings(settings.New(e.q, time.Minute))
	f.svc.p.PlanMaxRevisions = 10
	repo := uuid.UUID(mustRun(t, e, old).RepoID.Bytes)
	seedEligibleIssue(t, e, repo, 9481)
	run, err := f.svc.CreateRun(e.ctx, f.userID, repo, 9481, "semantic source", nil, nil, false, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	structural, proposal, sources := semanticFixture()
	// Checker admission requires these fields, and they must participate in
	// semantic compilation rather than being patched into its result.
	structural.BaseCommit = checked.BaseCommit
	structural.PlanningDiff = checked.PlanningDiff
	structural.SizeClass = checked.SizeClass
	sources[0].Bytes = []byte(run.IssueDescription)
	proposal.Criteria[0].Citations[0].Hash = semanticHash(sources[0].Bytes)
	// The cached eligible issue supplies the immutable description.
	proposal.Criteria[0].Citations[0].Start = 0
	proposal.Criteria[0].Citations[0].End = len(sources[0].Bytes)
	e.exec("UPDATE runs SET worker_id=$2,status='running',claim_generation=7,auto_approve=$3,plan_cross_check_required=false,completion_contract_version=1 WHERE id=$1", run.ID, f.wkr.ID, auto)
	e.exec(`INSERT INTO semantic_planning(run_id,enrollment) VALUES($1,'{"profile":"semantic","policy":{"version":1,"model":"sonnet","max_reviews":3,"max_invocations":2,"max_tokens":120000,"max_seconds":900}}')`, run.ID)
	c := &coverageLiveFixture{e: interlockLiveDB{ctx: e.ctx, pool: e.pool, q: e.q, userID: f.userID, repoID: repo}, svc: f.svc, worker: f.wkr.ID, run: run.ID, structural: structural, proposal: proposal, sources: sources}
	c.submit(t)
	return e, f, c
}

func TestPolicySemanticSourcePublicationReceiptLiveDB(t *testing.T) {
	e, f, c := semanticPolicySourceLive(t, false)
	publishGateCoverage(t, c)
	req := gateReportRequest(t, c)
	firstGate := presentSemanticGate(t, c, req)
	first := mustRun(t, e, c.run)
	source := assertRevisionGateBound(t, e, c.run, first.GateRevision)
	revisionID := submitSemanticRevision(t, c, firstGate, "Revise the semantic evidence requirement", true)
	if err := f.svc.AppendMessagesForClaim(e.ctx, f.wkr, c.run, []IncomingMessage{{
		Seq: first.LastSeq + 1, Kind: "plan_revising", Payload: revisionWriterPayload(t, source, revisionWriterHumanRef(revisionID)),
	}}, i64(7)); err != nil {
		t.Fatal(err)
	}
	// A genuine semantic revision with unchanged structural fields.
	a := c.candidate
	c.proposal.Criteria[0].DoneWhen += " with independently checked evidence"
	c.submit(t)
	if c.candidate.Digest == a.Digest || c.candidate.PlanFieldsDigest != a.PlanFieldsDigest {
		t.Fatal("fixture did not separate semantic and structural identity")
	}
	publishGateCoverage(t, c)
	req = gateReportRequest(t, c)
	req.ExpectedGateRevision = &firstGate.Approval.PresentationRevision
	req.RevisionInputID = &revisionID
	gate := presentSemanticGate(t, c, req)
	assertRevisionGateBound(t, e, c.run, gate.Approval.PresentationRevision)
	before := mustRun(t, e, c.run)
	// No-checker approval must still consume the actual current gate receipt.
	input, err := semanticGateApprove(c, f.userID, gate, nil)
	if err != nil {
		t.Fatal(err)
	}
	e.exec("UPDATE runs SET policy_refusal_count=1 WHERE id=$1", c.run)
	assertAutopilotApproval(t, e, c.run, false)
	if _, err := f.svc.AckInputs(e.ctx, f.wkr, c.run, 7, []int64{input.ID}); err != nil {
		t.Fatal(err)
	}
	assertAutopilotApproval(t, e, c.run, false)
	if _, err := f.svc.ApplyInputs(e.ctx, f.wkr, c.run, 7, []int64{input.ID}); err != nil {
		t.Fatal(err)
	}
	assertAutopilotApproval(t, e, c.run, true)
	after := mustRun(t, e, c.run)
	if !bytes.Equal(before.GatePresentedPayload, after.GatePresentedPayload) || !bytes.Equal(before.GatePayloadDigest, after.GatePayloadDigest) {
		t.Fatal("acceptance rewrote stored gate")
	}
}

func TestPolicySemanticSourceNoCheckerAutoStopLiveDB(t *testing.T) {
	e, f, c := semanticPolicySourceLive(t, true)
	f.svc.p.PolicyStopNegotiated = true
	publishGateCoverage(t, c)
	req := gateReportRequest(t, c)
	req.State, req.PresentationID = "running", nil
	autopilotRevisionReport(t, f, c.run, req)
	accepted := mustRun(t, e, c.run)
	target, valid, err := policyRevisionAcceptedTarget(e.ctx, e.q, accepted)
	if err != nil || !valid || target == nil || target.Digest != c.candidate.Digest {
		t.Fatalf("accepted target valid=%v err=%v", valid, err)
	}
	if hex.EncodeToString(accepted.GatePayloadDigest) != "" {
		t.Fatal("automatic plan published a gate")
	}
	completed := []string{"m1"}
	autopilotRevisionReport(t, f, c.run, StateRequest{State: "running", ClaimGeneration: i64(7), MilestonesCompleted: &completed})
	hold := policyRequest()
	hold.ClaimGeneration, hold.SourceGeneration = 7, 7
	hold.Phase = strPtr("implementation")
	_, disposition, err := f.svc.ReportPolicyHold(e.ctx, f.wkr, c.run, hold)
	if err != nil || disposition != PolicyHoldApplied {
		t.Fatalf("hold=%s err=%v", disposition, err)
	}
	policyStopNoGateReview(t, e, f, c.run)
	e.exec("UPDATE runs SET completion_contract=jsonb_set(completion_contract,'{revision}','99') WHERE id=$1", c.run)
	if _, err := f.svc.PolicyStopReviewForOwner(e.ctx, f.userID, c.run, PolicyHoldActionToken(mustRun(t, e, c.run))); err == nil {
		t.Fatal("corrupt frozen contract authorized Stop")
	}
}

// Both paths take real automatic feedback A, then accept the distinct semantic
// candidate B through either the normal automatic write or a fresh human receipt.
func TestPolicySemanticSourceCheckedAcceptanceLiveDB(t *testing.T) {
	for _, human := range []bool{false, true} {
		t.Run(map[bool]string{false: "automatic", true: "human"}[human], func(t *testing.T) {
			e, f, c := semanticPolicySourceLive(t, true)
			e.exec("UPDATE runs SET plan_cross_check_required=true WHERE id=$1", c.run)
			publishGateCoverage(t, c)
			firstCandidate := c.structural
			firstCandidate.CandidateIdentityVersion, firstCandidate.SemanticCandidate = semanticIdentityVersion, &c.candidate
			first, err := f.svc.SubmitPlanCrossCheck(e.ctx, f.wkr, c.run, 7, firstCandidate, 1)
			if err != nil {
				t.Fatal(err)
			}
			autopilotDecide(t, e, f, first, "revise")
			firstID := first.ID.String()
			source := inventorySource(t, e, c.run)
			if err := f.svc.AppendMessagesForClaim(e.ctx, f.wkr, c.run, []IncomingMessage{{
				Seq: mustRun(t, e, c.run).LastSeq + 1, Kind: "plan_revising",
				Payload: revisionWriterPayload(t, source, apitypes.PolicyRevisionRef{Source: "plan_cross_check", CrossCheckID: &firstID}),
			}}, i64(7)); err != nil {
				t.Fatal(err)
			}
			taken := inventorySource(t, e, c.run)
			if taken.PrimaryRevision == nil || *taken.PrimaryRevision.CrossCheckID != firstID {
				t.Fatal("automatic feedback was not taken")
			}
			oldDigest, oldFields := c.candidate.Digest, c.candidate.PlanFieldsDigest
			c.proposal.Criteria[0].DoneWhen += " with a different semantic obligation"
			if human {
				c.proposal.Criteria[0].Kind = "maintainer"
				c.proposal.Criteria[0].Maintainer = &apitypes.SemanticMaintainer{
					Reason: "Owner observation", Conditions: []string{"environment available"},
					VerificationSteps: []string{"observe the accepted behavior"},
				}
			}
			c.submit(t)
			if c.candidate.Digest == oldDigest || c.candidate.PlanFieldsDigest != oldFields {
				t.Fatal("fixture did not distinguish semantic identity from unchanged displayed fields")
			}
			publishGateCoverage(t, c)
			nextCandidate := c.structural
			nextCandidate.CandidateIdentityVersion, nextCandidate.SemanticCandidate = semanticIdentityVersion, &c.candidate
			next, err := f.svc.SubmitPlanCrossCheck(e.ctx, f.wkr, c.run, 7, nextCandidate, 2)
			if err != nil {
				t.Fatal(err)
			}
			autopilotDecide(t, e, f, next, "approve")
			history := acceptanceHistory(t, e, c.run)
			e.exec("UPDATE runs SET policy_refusal_count=1 WHERE id=$1", c.run)
			assertAutopilotApproval(t, e, c.run, false)
			req := gateReportRequest(t, c)
			if human {
				req.SemanticPark = strPtr(semanticParkMaintainerAuth)
				gate := presentSemanticGate(t, c, req)
				approval, err := semanticGateApprove(c, f.userID, gate, nil)
				if err != nil {
					t.Fatal(err)
				}
				assertAutopilotApproval(t, e, c.run, false)
				if _, err := f.svc.AckInputs(e.ctx, f.wkr, c.run, 7, []int64{approval.ID}); err != nil {
					t.Fatal(err)
				}
				assertAutopilotApproval(t, e, c.run, false)
				if _, err := f.svc.ApplyInputs(e.ctx, f.wkr, c.run, 7, []int64{approval.ID}); err != nil {
					t.Fatal(err)
				}
			} else {
				req.State, req.PresentationID = "running", nil
				autopilotRevisionReport(t, f, c.run, req)
			}
			accepted := inventorySource(t, e, c.run)
			row := mustRun(t, e, c.run)
			if accepted.PrimaryRevision != nil || len(accepted.AssociatedCrossChecks) != 1 ||
				*accepted.AssociatedCrossChecks[0].CrossCheckID != next.ID.String() ||
				!row.PolicyCandidateCrossCheckID.Valid || uuid.UUID(row.PolicyCandidateCrossCheckID.Bytes) != next.ID {
				t.Fatal("acceptance did not retain exactly the current checker")
			}
			assertAutopilotApproval(t, e, c.run, true)
			if acceptanceHistory(t, e, c.run) != history {
				t.Fatal("acceptance rewrote automatic feedback provenance")
			}
		})
	}
}

func TestPolicySemanticSourceTakenAndRecoveryLiveDB(t *testing.T) {
	for _, drift := range []bool{false, true} {
		t.Run(map[bool]string{false: "exact", true: "same_text_new_semantic"}[drift], func(t *testing.T) {
			e, f, c := semanticPolicySourceLive(t, true)
			e.exec("UPDATE runs SET plan_cross_check_required=true WHERE id=$1", c.run)
			candidate := c.structural
			candidate.CandidateIdentityVersion, candidate.SemanticCandidate = semanticIdentityVersion, &c.candidate
			cc, err := f.svc.SubmitPlanCrossCheck(e.ctx, f.wkr, c.run, 7, candidate, 1)
			if err != nil {
				t.Fatal(err)
			}
			child := uuid.UUID(cc.CheckerRunID.Bytes)
			// Explicit checker active-claim bridge, followed by the real verdict writer.
			e.exec("UPDATE runs SET worker_id=$2,status='running',claim_generation=1 WHERE id=$1", child, f.wkr.ID)
			if _, err := f.svc.DecidePlanCrossCheck(e.ctx, f.wkr, child, 1, "revise", "revise", []byte(`{"summary":"revise","items":[]}`)); err != nil {
				t.Fatal(err)
			}
			a := c.candidate
			if drift {
				c.proposal.Criteria[0].DoneWhen += " with additional semantic evidence"
				c.submit(t)
				if c.candidate.Digest == a.Digest || c.candidate.PlanFieldsDigest != a.PlanFieldsDigest {
					t.Fatal("candidate drift not discriminating")
				}
			}
			source := inventorySource(t, e, c.run)
			id := cc.ID.String()
			ref := apitypes.PolicyRevisionRef{Source: "plan_cross_check", CrossCheckID: &id}
			if err := f.svc.AppendMessagesForClaim(e.ctx, f.wkr, c.run, []IncomingMessage{{Seq: mustRun(t, e, c.run).LastSeq + 1, Kind: "plan_revising", Payload: revisionWriterPayload(t, source, ref)}}, i64(7)); err != nil {
				t.Fatal(err)
			}
			r := mustRun(t, e, c.run)
			valid, err := policyRevisionIdentityValid(e.ctx, e.q, cc)
			if err != nil || !valid {
				t.Fatal("historical witness was damaged")
			}
			if drift {
				if len(r.PolicyRevisionSource) != 0 {
					t.Fatal("old same-text semantic candidate taken as current")
				}
			} else {
				got := inventorySource(t, e, c.run)
				if got.PrimaryRevision == nil || *got.PrimaryRevision.CrossCheckID != id {
					t.Fatal("exact current semantic revision not taken")
				}
				before := acceptancePair(t, e, c.run)
				autopilotRevisionReport(t, f, c.run, StateRequest{State: "running", ClaimGeneration: i64(7)})
				if acceptancePair(t, e, c.run) != before {
					t.Fatal("heartbeat cleared primary")
				}
			}
			before := policyOwnerStopSnapshot(t, e, c.run)
			replay, err := f.svc.SubmitPlanCrossCheck(e.ctx, f.wkr, c.run, 7, candidate, 1)
			if policyOwnerStopSnapshot(t, e, c.run) != before {
				t.Fatal("recovery changed source or selection")
			}
			if drift {
				// Submit binds current immutable planning before its exact-attempt
				// lookup; after candidate drift the original round is stale.
				if !errors.Is(err, ErrCrossCheckInterrupted) {
					t.Fatalf("changed current candidate recovery: %v", err)
				}
			} else if err != nil || replay.ID != cc.ID {
				t.Fatalf("exact recovery changed identity: %v", err)
			}
		})
	}
}

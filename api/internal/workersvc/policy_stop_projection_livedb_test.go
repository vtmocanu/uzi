package workersvc

import (
	"bytes"
	"context"
	"strconv"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Credential setup and cross-check configuration are fixtures. Both ownership
// handoffs below use actual Service.Claim; stopExecutionBeginClaim is not used.
// These service calls supply authenticated identities, not HTTP authentication.
// Immutable receipt corruption is exercised by unit input seams; database tests
// retain the trigger that prevents overwriting an accepted receipt.
func TestPolicyStopProjectionActualClaimLiveDB(t *testing.T) {
	for _, mode := range []string{"gated", "no_gate", "switch_off", "claim_commit_failure", "ack_commit_failure", "ack_switch_off", "terminal"} {
		t.Run(mode, func(t *testing.T) {
			e := setupCodexLiveDB(t)
			f := newCodexClaimFix(t, e, false)
			w := f.claimant(t, true)
			enableRevisionProjection(f.svc, &w)
			f.svc.p.PolicyStopNegotiated = true
			w.ProtocolCapabilities = append(w.ProtocolCapabilities, capability.GateRevisionV1, capability.InputReceiptsV1)
			e.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", w.ID, w.ProtocolCapabilities)
			e.exec("UPDATE runs SET status='cancelled' WHERE id=$1", f.runID)
			setDefaultHarness(t, e, f.userID, "codex")
			makeCodexDefault(t, e, f.aliasID)
			seedEligibleIssue(t, e, f.repoID, 9473)
			created, err := f.svc.CreateRun(e.ctx, f.userID, f.repoID, 9473, "Stop handoff", nil, nil, false, nil, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			// Automatic approval is established before the first plan; Stop never
			// seeds approval of the later gated candidate.
			e.exec("UPDATE runs SET auto_approve=true,plan_cross_check_required=false WHERE id=$1", created.ID)
			t.Cleanup(func() { dropCustodyHolds(t, e, f.userID) })
			first, err := f.svc.Claim(e.ctx, w, nil)
			if err != nil || first == nil || first.RunID != created.ID.String() {
				t.Fatalf("initial real Claim: %v", err)
			}
			generation := mustRun(t, e, created.ID).ClaimGeneration
			g := &gateFix{t: t, ctx: e.ctx, pool: e.pool, q: e.q, svc: f.svc, wkr: w, userID: f.userID, repoID: f.repoID}
			plan := "# Approved execution A"
			milestones := []Milestone{{ID: "m1", Title: "one"}, {ID: "m2", Title: "two"}}
			report := StateRequest{State: "running", ClaimGeneration: &generation, PlanMd: &plan, Milestones: &milestones, SessionID: strPtr("stop-projection-session")}
			_, applied, err := f.svc.SetState(e.ctx, w, created.ID, report)
			if err != nil || !applied {
				t.Fatalf("initial approved running: %v", err)
			}
			completed := []string{"m1"}
			_, applied, err = f.svc.SetState(e.ctx, w, created.ID, StateRequest{State: "running", ClaimGeneration: &generation, MilestonesCompleted: &completed})
			if err != nil || !applied {
				t.Fatalf("progress: %v", err)
			}
			gated := mode == "gated"
			if gated {
				e.exec("UPDATE runs SET auto_approve=false WHERE id=$1", created.ID)
				req := acceptanceGateRequest()
				req.ClaimGeneration = &generation
				req.PlanMd = strPtr("# Unapproved candidate B")
				req.Milestones = &[]Milestone{{ID: "b1", Title: "candidate"}}
				g.mustPublish(created.ID, req, 1)
			}
			before := mustRun(t, e, created.ID)
			hold := policyRequest()
			hold.ClaimGeneration = generation
			hold.SourceGeneration = generation
			hold.Phase = strPtr("implementation")
			if gated {
				hold.ExpectedGateRevision = &before.GateRevision
			}
			if _, disposition, err := f.svc.ReportPolicyHold(e.ctx, w, created.ID, hold); err != nil || disposition != PolicyHoldApplied {
				t.Fatalf("hold: %s %v", disposition, err)
			}
			review, err := f.svc.PolicyStopReviewForOwner(e.ctx, f.userID, created.ID, PolicyHoldActionToken(mustRun(t, e, created.ID)))
			if err != nil {
				t.Fatal(err)
			}
			record := stopExecutionConfirm(t, e, g, created.ID, review)
			assembled := false
			f.svc.claimHooks = &claimTestHooks{afterAssembly: func(_ context.Context, r store.Run, payload *ClaimPayload, err error) {
				if err != nil || payload == nil || r.ID != created.ID {
					t.Fatalf("assembly: %v", err)
				}
				assembled = true
				payload.PolicyStopExecution = &PolicyStopExecution{ResolutionID: uuid.NewString(), ClaimGeneration: "1"}
				switch mode {
				case "switch_off":
					f.svc.p.PolicyStopNegotiated = false
				case "claim_commit_failure":
					f.svc.SetTxBeginner(&revisionGateBeginner{TxBeginner: e.pool, point: "commit"})
				}
			}}
			claim, err := f.svc.Claim(e.ctx, w, nil)
			if !assembled {
				t.Fatalf("real Claim did not assemble: %v", err)
			}
			if mode == "switch_off" || mode == "claim_commit_failure" {
				if err == nil || claim != nil {
					t.Fatalf("refused handoff=%+v err=%v", claim, err)
				}
				return
			}
			if err != nil || claim == nil || claim.PolicyStopExecution == nil {
				t.Fatalf("real reclaim: %v", err)
			}
			current := mustRun(t, e, created.ID)
			generation = current.ClaimGeneration
			if generation <= 1 || claim.PolicyStopExecution.ResolutionID != record.ResolutionID.String() ||
				claim.PolicyStopExecution.ClaimGeneration != strconv.FormatInt(generation, 10) {
				t.Fatalf("current claim authority=%+v", claim.PolicyStopExecution)
			}
			frozen, err := DecodeMilestones(before.MilestonesFrozen)
			if err != nil || claim.ExecutionMilestones == nil || !bytes.Equal(mustStopJSON(t, *claim.ExecutionMilestones), mustStopJSON(t, frozen)) {
				t.Fatal("frozen execution changed")
			}
			if gated && (claim.PlanApproved || claim.ResumePhase != "awaiting_approval" || claim.PlanMd == nil ||
				*claim.PlanMd != "# Unapproved candidate B" || len(claim.Milestones) != 1 || claim.Milestones[0].ID != "b1") {
				t.Fatal("gated candidate replaced or approved")
			}
			if mode == "ack_commit_failure" {
				f.svc.SetTxBeginner(&revisionGateBeginner{TxBeginner: e.pool, point: "commit"})
			}
			if mode == "ack_switch_off" {
				f.svc.p.PolicyStopNegotiated = false
			}
			state := "running"
			if mode == "terminal" {
				state = "failed"
			}
			ack, err := f.svc.SetStateReportWithReconciliation(e.ctx, w, created.ID, StateRequest{State: state, ClaimGeneration: &generation})
			if mode == "terminal" {
				if err != nil || !ack.Applied || ack.PolicyStopExecution != nil {
					t.Fatalf("terminal ACK: %+v %v", ack, err)
				}
				return
			}
			if gated || mode == "ack_commit_failure" || mode == "ack_switch_off" {
				// Expect independent SQL admission to refuse gated running.
				// This is deliberately not a positive gated ACK proof.
				if ack.Applied || ack.PolicyStopExecution != nil {
					t.Fatalf("refused ACK leaked authority: %+v %v", ack, err)
				}
				if !gated && err == nil {
					t.Fatal("fault did not fail report")
				}
				return
			}
			if err != nil || !ack.Applied || ack.PolicyStopExecution == nil || *ack.PolicyStopExecution != *claim.PolicyStopExecution || string(ack.PolicyRevisionGate) != "null" {
				t.Fatalf("actual no-gate ACK: %+v %v", ack, err)
			}
		})
	}
}

package workersvc

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Credential setup and cross-check configuration are fixtures. Both ownership
// handoffs below use actual Service.Claim; stopExecutionBeginClaim is not used.
// These service calls supply authenticated identities, not HTTP authentication.
// Immutable receipt corruption is exercised by unit input seams; database tests
// retain the trigger that prevents overwriting an accepted receipt.
// Scalar checks below exercise the Go service preflight only. Independent SQL K
// authorization remains required; this is not SQL completion or seam readiness.
func TestPolicyStopProjectionActualClaimLiveDB(t *testing.T) {
	for _, mode := range []string{"gated", "no_gate", "ordinary_question", "completion_question", "switch_off", "claim_commit_failure", "ack_commit_failure", "ack_switch_off", "terminal"} {
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
			var revisionID int64
			if gated {
				e.exec("UPDATE runs SET auto_approve=false WHERE id=$1", created.ID)
				req := acceptanceGateRequest()
				req.ClaimGeneration = &generation
				req.PlanMd = strPtr("# Unapproved candidate B")
				req.Milestones = &[]Milestone{{ID: "b1", Title: "candidate"}}
				g.mustPublish(created.ID, req, 1)
				gate := mustRun(t, e, created.ID).GateRevision
				revision, err := f.svc.SubmitInputWithOptions(e.ctx, f.userID, created.ID, "revise_plan", "revise candidate B", nil, SubmitInputOptions{ExpectedGateRevision: &gate})
				if err != nil {
					t.Fatal(err)
				}
				if receipt, err := f.svc.AckInputs(e.ctx, w, created.ID, generation, []int64{revision.ID}); err != nil || !receipt.Active {
					t.Fatalf("revision ACK: %+v %v", receipt, err)
				}
				revisionID = revision.ID
				source := acceptanceSource(t, e, created.ID)
				// This Go frame exercises the service writer, not the Node worker.
				if err := f.svc.AppendMessagesForClaim(e.ctx, w, created.ID, []IncomingMessage{{
					Seq: mustRun(t, e, created.ID).LastSeq + 1, Kind: "plan_revising",
					Payload: revisionWriterPayload(t, source, revisionWriterHumanRef(revision.ID)),
				}}, &generation); err != nil {
					t.Fatal(err)
				}
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
			if gated {
				revision, err := e.q.GetPolicyStopRevisionInput(e.ctx, store.GetPolicyStopRevisionInputParams{RunID: created.ID, InputID: revisionID})
				if err != nil || !revision.Disposition.Valid || revision.Disposition.String != "superseded_by_stop" ||
					!revision.SettledAt.Valid || !revision.SettledAt.Time.Equal(record.AcceptedAt) ||
					!revision.SupersededByStopInputID.Valid || revision.SupersededByStopInputID.Int64 != record.ScopeInputID || revision.AppliedAt.Valid {
					t.Fatalf("owner Stop did not supersede pending revision: %+v %v", revision, err)
				}
			}
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
			resolutionID := claim.PolicyStopExecution.ResolutionID
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
			// Snapshot only persistent evidence: status, clocks and banked time
			// legitimately change during ACK and question transitions.
			snapshot := func() map[string][]byte {
				t.Helper()
				r := mustRun(t, e, created.ID)
				scope, err := e.q.GetPolicyStopScopeInput(e.ctx, store.GetPolicyStopScopeInputParams{RunID: created.ID, InputID: record.ScopeInputID})
				if err != nil {
					t.Fatal(err)
				}
				if scope.Disposition.Valid || scope.SettledAt.Valid || scope.ConsumedAt.Valid ||
					scope.ConsumedWorkerID.Valid || scope.ConsumedClaimGeneration.Valid || scope.AppliedAt.Valid ||
					scope.IncludedAt.Valid || scope.InclusionReported {
					t.Fatal("scope audit no longer pending and undelivered")
				}
				fields := map[string]any{
					"accepted receipt": r.PolicyStopResolution, "scope audit": scope,
					"source context": r.PolicyRevisionSource, "selected cross-check": r.PolicyCandidateCrossCheckID,
					"scope ceiling": r.ScopeCeiling, "frozen execution": r.MilestonesFrozen,
					"completed IDs": r.MilestonesCompleted, "completion contract": r.CompletionContract,
					"contract version": r.CompletionContractVersion, "contract revision": r.ContractRevision,
					"candidate plan": r.PlanMd, "candidate milestones": r.MilestonesCandidate,
					"gate revision": r.GateRevision, "gate presentation": r.GatePresentationID,
					"gate payload": r.GatePresentedPayload, "gate digest": r.GatePayloadDigest,
					"worker": r.WorkerID, "claim generation": r.ClaimGeneration,
					"refusal count": r.PolicyRefusalCount, "gate refusal count": r.GateRefusalCount,
					"gate refusal generation": r.GateRefusalGeneration,
					"original outcome":        r.PolicyLastOutcomeID, "source generation": r.PolicyLastSourceGeneration,
					"session": r.SessionID,
				}
				if revisionID > 0 {
					revision, err := e.q.GetPolicyStopRevisionInput(e.ctx, store.GetPolicyStopRevisionInputParams{RunID: created.ID, InputID: revisionID})
					if err != nil {
						t.Fatal(err)
					}
					fields["superseded human revision"] = revision
				}
				encoded := make(map[string][]byte, len(fields))
				for name, value := range fields {
					encoded[name] = mustStopJSON(t, value)
				}
				return encoded
			}
			saved := snapshot()
			assertPreserved := func() {
				t.Helper()
				current := snapshot()
				if len(saved) != len(current) {
					t.Fatal("Stop evidence field inventory changed")
				}
				names := make([]string, 0, len(current))
				for name := range current {
					names = append(names, name)
				}
				slices.Sort(names)
				for _, name := range names {
					value, present := saved[name]
					if !present || !bytes.Equal(value, current[name]) {
						t.Fatalf("Stop flow changed persisted %s", name)
					}
				}
			}
			// Each refusal compares the full run/input/check snapshot, including
			// status, clocks and accounting excluded from the transition snapshot.
			refuseScalar := func(state string, resolution *string) {
				t.Helper()
				before := policyOwnerStopSnapshot(t, e, created.ID)
				ack, err := f.svc.SetStateReportWithReconciliation(e.ctx, w, created.ID, StateRequest{
					State: state, ClaimGeneration: &generation, PolicyStopResolutionID: resolution,
				})
				requirePolicyOwnerStopConflict(t, err, "policy_stop_execution_unverifiable")
				if ack.Applied || ack.PolicyStopExecution != nil {
					t.Fatal("scalar refusal issued authority")
				}
				if before != policyOwnerStopSnapshot(t, e, created.ID) {
					t.Fatal("scalar refusal changed full run/input/check snapshot")
				}
			}
			if mode == "gated" || mode == "no_gate" {
				for _, state := range []string{"running", "awaiting_approval", "awaiting_input"} {
					refuseScalar(state, nil)
					refuseScalar(state, strPtr(uuid.NewString()))
				}
			}
			if mode == "ordinary_question" || mode == "completion_question" {
				qid := "stop-resume-question"
				park, err := f.svc.SetStateReportWithReconciliation(e.ctx, w, created.ID, StateRequest{
					State: "awaiting_input", ClaimGeneration: &generation, PolicyStopResolutionID: &resolutionID, OpenQuestionID: &qid,
					CompletionQuestion: mode == "completion_question",
				})
				if err != nil || !park.Applied || park.PolicyStopExecution != nil {
					t.Fatalf("question park: %+v %v", park, err)
				}
				parked := mustRun(t, e, created.ID)
				if parked.Status != "awaiting_input" || !parked.OpenQuestionID.Valid || parked.OpenQuestionID.String != qid ||
					parked.CompletionQuestionAt.Valid != (mode == "completion_question") {
					t.Fatal("question park identity or completion marker lost")
				}
				assertPreserved()
				refuseRunning := func(stage string) {
					t.Helper()
					ack, err := f.svc.SetStateReportWithReconciliation(e.ctx, w, created.ID, StateRequest{State: "running", ClaimGeneration: &generation, PolicyStopResolutionID: &resolutionID})
					// An unanswered question must not issue authority.
					if ack.Applied || ack.PolicyStopExecution != nil {
						t.Fatalf("%s running bypassed answer guard: %+v %v", stage, ack, err)
					}
					if mustRun(t, e, created.ID).Status != "awaiting_input" {
						t.Fatalf("%s refusal changed park", stage)
					}
					assertPreserved()
				}
				refuseRunning("before_answer")
				answer, err := f.svc.SubmitInput(e.ctx, f.userID, created.ID, "answer", answerBody(t, qid, "continue"), nil)
				if err != nil {
					t.Fatal(err)
				}
				refuseRunning("before_ack")
				if receipt, err := f.svc.AckInputs(e.ctx, w, created.ID, generation, []int64{answer.ID}); err != nil || !receipt.Active {
					t.Fatalf("answer ACK: %+v %v", receipt, err)
				}
				refuseRunning("before_applied")
				if receipt, err := f.svc.ApplyInputs(e.ctx, w, created.ID, generation, []int64{answer.ID}); err != nil || !receipt.Active {
					t.Fatalf("answer APPLIED: %+v %v", receipt, err)
				}
				assertPreserved()
				resume, err := f.svc.SetStateReportWithReconciliation(e.ctx, w, created.ID, StateRequest{State: "running", ClaimGeneration: &generation, PolicyStopResolutionID: &resolutionID})
				if err != nil || !resume.Applied || resume.PolicyStopExecution == nil || *resume.PolicyStopExecution != *claim.PolicyStopExecution {
					t.Fatalf("answered resume refused or lost current-generation authority: %+v %v", resume, err)
				}
				resumed := mustRun(t, e, created.ID)
				if resumed.Status != "running" || resumed.OpenQuestionID.Valid || resumed.CompletionQuestionAt.Valid {
					t.Fatal("answered resume retained question park")
				}
				assertPreserved()
				return
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
			ack, err := f.svc.SetStateReportWithReconciliation(e.ctx, w, created.ID, StateRequest{State: state, ClaimGeneration: &generation, PolicyStopResolutionID: &resolutionID})
			if mode == "terminal" {
				if err != nil || !ack.Applied || ack.PolicyStopExecution != nil {
					t.Fatalf("terminal ACK: %+v %v", ack, err)
				}
				return
			}
			if mode == "ack_commit_failure" || mode == "ack_switch_off" {
				if ack.Applied || ack.PolicyStopExecution != nil {
					t.Fatalf("refused ACK leaked authority: %+v %v", ack, err)
				}
				if err == nil {
					t.Fatal("fault did not fail report")
				}
				assertPreserved()
				return
			}
			if err != nil || !ack.Applied || ack.Run.Status != "running" || ack.PolicyStopExecution == nil || *ack.PolicyStopExecution != *claim.PolicyStopExecution {
				t.Fatalf("actual running carrier ACK: %+v %v", ack, err)
			}
			if mustRun(t, e, created.ID).Status != "running" {
				t.Fatal("running ACK was not committed")
			}
			if gated {
				if ack.PlanApproved == nil || *ack.PlanApproved {
					t.Fatal("gated ACK must explicitly leave candidate unapproved")
				}
				var gate apitypes.PolicyStopGateContext
				if record.Manifest.SourceContext.Gate == nil || json.Unmarshal(ack.PolicyRevisionGate, &gate) != nil || gate != *record.Manifest.SourceContext.Gate {
					t.Fatal("gated ACK did not preserve accepted source gate context")
				}
			} else if string(ack.PolicyRevisionGate) != "null" {
				t.Fatal("no-gate ACK must carry JSON null")
			}
			assertPreserved()
		})
	}
}

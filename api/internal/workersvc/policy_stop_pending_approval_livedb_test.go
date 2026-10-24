package workersvc

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Complete no-revision V1 context is seeded, as in
// TestCrossCheckPolicyPendingApprovalInertLiveDB. This is reader/receipt proof,
// not a runtime source-writer, running/completion or worker-turn proof.
func policyStopPendingApprovalFixture(t *testing.T) (usageTailEnv, int64) {
	t.Helper()
	e := seedPolicyRevisionApprovalClaim(t, false, 2, true)
	e.svc.p.PolicyStopNegotiated = true
	e.svc.p.RunTimeout = 16 * time.Hour
	e.wkr.ProtocolCapabilities = append(e.wkr.ProtocolCapabilities,
		capability.CodexHarnessV1, capability.CodexRuntimeV2, capability.RecoveryArchiveV1)
	e.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", e.wkr.ID, e.wkr.ProtocolCapabilities)
	// Finish the seeded revision context before the owner submits approval.
	// Keep the candidate identical to frozen execution; no reconciliation needed.
	e.exec("UPDATE run_user_inputs SET applied_at=now() WHERE run_id=$1 AND kind='revise_plan'", e.runID)
	e.exec("UPDATE runs SET status='awaiting_approval',kind='issue',issue_iid=101,size_class='s',required_tools='{go}',milestones_candidate=milestones_frozen WHERE id=$1", e.runID)
	seedPolicyOwnerStopSource(t, e.codexTestEnv, e.runID)
	if _, err := e.svc.SubmitInputWithOptions(e.ctx, e.userID, e.runID, "approve_plan", "approval before policy Stop", nil,
		SubmitInputOptions{ExpectedGateRevision: i64p(3)}); err != nil {
		t.Fatalf("owner approval Submit: %v", err)
	}
	var id int64
	if err := e.pool.QueryRow(e.ctx, "SELECT id FROM run_user_inputs WHERE run_id=$1 AND kind='approve_plan' AND applied_at IS NULL", e.runID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	delivery, err := e.svc.ConsumeInputs(e.ctx, e.wkr, e.runID)
	if err != nil || !delivery.Receipts || !slices.ContainsFunc(delivery.Inputs, func(i InputDTO) bool { return i.ID == id && i.Kind == "approve_plan" }) {
		t.Fatalf("ordinary pre-Stop delivery=%+v err=%v", delivery, err)
	}
	if policyAssociationApproval(t, e) {
		t.Fatal("enqueue authorized the current unapproved candidate")
	}
	return e, id
}

func policyStopPendingApprovalAccept(t *testing.T, e usageTailEnv) {
	t.Helper()
	// This frame simulates awaiting-approval revision admission, not a taken
	// source. Its missing carrier must invalidate coverage. The receipt/Stop
	// component proof restores its explicitly seeded V1 precondition below;
	// this is not a source-writer-to-Stop composition proof.
	if err := e.svc.AppendMessagesForClaim(e.ctx, e.wkr, e.runID, []IncomingMessage{{Seq: 1, Kind: "plan_revising", Payload: []byte("{}")}}, i64p(1)); err != nil {
		t.Fatal(err)
	}
	req := policyRequest()
	req.ExpectedGateRevision = i64p(3)
	held, disposition, err := e.svc.ReportPolicyHold(e.ctx, e.wkr, e.runID, req)
	if err != nil || disposition != PolicyHoldApplied || held.Status != "paused" {
		t.Fatalf("real policy hold: status=%s disposition=%s err=%v", held.Status, disposition, err)
	}
	if len(held.PolicyRevisionSource) != 0 {
		t.Fatal("metadata-only revision admission certified source coverage")
	}
	seedPolicyOwnerStopSource(t, e.codexTestEnv, e.runID)
	// Closed full V1 plus no cross-check and NULL selection must still allow Stop.
	review := policyOwnerStopReview(t, e)
	result, err := e.svc.SubmitInputWithOptions(e.ctx, e.userID, e.runID, "stop", "finish completed work", nil,
		SubmitInputOptions{ActionIfMatch: review.StopIfMatch})
	if err != nil || result.PolicyStop == nil || result.PolicyStop.Disposition != "accepted" || !result.Resumed || result.PolicyStop.ScopeCeiling != 1 {
		t.Fatalf("confirmed Stop=%+v err=%v", result, err)
	}
	accepted := mustRun(t, e.codexTestEnv, e.runID)
	if len(accepted.PolicyStopResolution) == 0 || accepted.PolicyFreshClaimGeneration.Valid || accepted.AutoApprove {
		t.Fatal("Stop lost its resolution or granted fresh/automatic execution")
	}
	before := policyOwnerStopSnapshot(t, e.codexTestEnv, e.runID)
	retry, err := e.svc.SubmitInputWithOptions(e.ctx, e.userID, e.runID, "stop", "lost response retry", nil,
		SubmitInputOptions{ActionIfMatch: review.StopIfMatch})
	if err != nil || retry.PolicyStop == nil || retry.PolicyStop.Disposition != "already_accepted" ||
		retry.PolicyStop.ResolutionID != result.PolicyStop.ResolutionID || policyOwnerStopSnapshot(t, e.codexTestEnv, e.runID) != before {
		t.Fatalf("Stop retry changed immutable evidence: %+v err=%v", retry, err)
	}
	_, err = e.svc.SubmitInputWithOptions(e.ctx, e.userID, e.runID, "approve_plan", "new approval after Stop", nil,
		SubmitInputOptions{ExpectedGateRevision: i64p(3)})
	requirePolicyOwnerStopConflict(t, err, "policy_stop_gate_resolved")
	if policyOwnerStopSnapshot(t, e.codexTestEnv, e.runID) != before {
		t.Fatal("refused new approval changed full run/input snapshot")
	}
}

func policyStopPendingApprovalReaders(t *testing.T, e usageTailEnv, approval int64) {
	t.Helper()
	for _, receipts := range []bool{true, false} {
		w := e.wkr
		w.ProtocolCapabilities = slices.Clone(w.ProtocolCapabilities)
		if !receipts {
			w.ProtocolCapabilities = slices.DeleteFunc(w.ProtocolCapabilities, func(c string) bool { return c == capability.InputReceiptsV1 })
		}
		gen := mustRun(t, e.codexTestEnv, e.runID).ClaimGeneration
		result, err := e.svc.ConsumeInputs(e.ctx, w, e.runID, &gen)
		if err != nil || result.Receipts != receipts || slices.ContainsFunc(result.Inputs, func(i InputDTO) bool { return i.ID == approval || i.Kind == "approve_plan" }) {
			t.Fatalf("service reader receipts=%v: %+v err=%v", receipts, result, err)
		}
	}
	replay, err := e.q.ListReplayRunInputs(e.ctx, e.runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range replay {
		if row.ID == approval || row.Kind == "approve_plan" {
			t.Fatalf("direct replay exposed stopped approval: %+v", row)
		}
	}
	consumed, err := e.q.ConsumeRunInputs(e.ctx, e.runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range consumed {
		if row.ID == approval || row.Kind == "approve_plan" {
			t.Fatalf("direct legacy consume exposed stopped approval: %+v", row)
		}
	}
}

func policyStopPendingApprovalRow(t *testing.T, e usageTailEnv, id int64) string {
	t.Helper()
	var row string
	if err := e.pool.QueryRow(e.ctx, "SELECT to_jsonb(i)::text FROM run_user_inputs i WHERE run_id=$1 AND id=$2", e.runID, id).Scan(&row); err != nil {
		t.Fatal(err)
	}
	return row
}

func TestPolicyStopPendingApprovalInertLiveDB(t *testing.T) {
	for _, preACK := range []bool{false, true} {
		name := "pending"
		if preACK {
			name = "ACK_response_lost"
		}
		t.Run(name, func(t *testing.T) {
			e, approval := policyStopPendingApprovalFixture(t)
			if preACK {
				receipt, err := e.svc.AckInputs(e.ctx, e.wkr, e.runID, 1, []int64{approval})
				if err != nil || !receipt.Active || len(receipt.Inputs) != 1 || receipt.Inputs[0].ID != approval {
					t.Fatalf("ordinary ACK=%+v err=%v", receipt, err)
				}
				// Simulate losing the ACK response: GET must replay before Stop.
				replay, err := e.svc.ConsumeInputs(e.ctx, e.wkr, e.runID)
				if err != nil || !slices.ContainsFunc(replay.Inputs, func(i InputDTO) bool { return i.ID == approval }) {
					t.Fatalf("pre-Stop ACK replay=%+v err=%v", replay, err)
				}
			}
			approvalBefore := policyStopPendingApprovalRow(t, e, approval)
			policyStopPendingApprovalAccept(t, e)
			before := policyOwnerStopSnapshot(t, e.codexTestEnv, e.runID)
			policyStopPendingApprovalReaders(t, e, approval)
			if policyOwnerStopSnapshot(t, e.codexTestEnv, e.runID) != before {
				t.Fatal("queued stopped readers changed approval/history")
			}
			claim, err := e.svc.Claim(e.ctx, e.wkr, nil)
			if err != nil || claim == nil {
				t.Fatalf("public Stop reclaim=%+v err=%v", claim, err)
			}
			if claim.ClaimGeneration != 2 || claim.PlanApproved || policyAssociationApproval(t, e) {
				t.Fatalf("pending approval authorized stopped claim: gen=%d approved=%v", claim.ClaimGeneration, claim.PlanApproved)
			}
			before = policyOwnerStopSnapshot(t, e.codexTestEnv, e.runID)
			policyStopPendingApprovalReaders(t, e, approval)
			if policyOwnerStopSnapshot(t, e.codexTestEnv, e.runID) != before {
				t.Fatal("claimed stopped readers changed approval/history")
			}
			// Seed unrelated actionable siblings via the ordinary input store.
			// One is ACKed so a partial APPLIED write is observable; one is fresh
			// so a partial ACK write is observable.
			acked, err := e.q.CreateRunInput(e.ctx, store.CreateRunInputParams{RunID: e.runID, Kind: "answer"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := e.svc.AckInputs(e.ctx, e.wkr, e.runID, claim.ClaimGeneration, []int64{acked.ID}); err != nil {
				t.Fatal(err)
			}
			fresh, err := e.q.CreateRunInput(e.ctx, store.CreateRunInputParams{RunID: e.runID, Kind: "answer"})
			if err != nil {
				t.Fatal(err)
			}
			for _, ids := range [][]int64{{approval}, {acked.ID, approval, fresh.ID}} {
				for _, gen := range []int64{1, claim.ClaimGeneration} {
					before = policyOwnerStopSnapshot(t, e.codexTestEnv, e.runID)
					for _, apply := range []bool{false, true} {
						var result InputReceiptResult
						if apply {
							result, err = e.svc.ApplyInputs(e.ctx, e.wkr, e.runID, gen, ids)
						} else {
							result, err = e.svc.AckInputs(e.ctx, e.wkr, e.runID, gen, ids)
						}
						var conflict *InputReceiptConflictError
						if !errors.As(err, &conflict) || conflict.Reason != "superseded" || len(result.Inputs) != 0 {
							t.Fatalf("receipt apply=%v gen=%d ids=%v result=%+v err=%v", apply, gen, ids, result, err)
						}
						if policyOwnerStopSnapshot(t, e.codexTestEnv, e.runID) != before {
							t.Fatal("refused receipt changed full run/input snapshot")
						}
					}
					direct, err := e.q.AckRunInputRows(e.ctx, store.AckRunInputRowsParams{RunID: e.runID, Ids: ids,
						ClaimGeneration: pgtype.Int8{Int64: gen, Valid: true}, WorkerID: pgconv.UUID(e.wkr.ID), InclusionReported: true})
					if err != nil || len(direct) != 0 {
						t.Fatalf("direct ACK returned rows=%+v err=%v", direct, err)
					}
					n, err := e.q.ApplyRunInputRows(e.ctx, store.ApplyRunInputRowsParams{RunID: e.runID, Ids: ids,
						ClaimGeneration: pgtype.Int8{Int64: gen, Valid: true}, WorkerID: pgconv.UUID(e.wkr.ID)})
					if err != nil || n != 0 {
						t.Fatalf("direct APPLIED changed rows=%d err=%v", n, err)
					}
					if policyOwnerStopSnapshot(t, e.codexTestEnv, e.runID) != before {
						t.Fatal("direct mixed writer changed full run/input snapshot")
					}
				}
			}
			// Retry the independently APPLIED historical approval under its original
			// receipt ownership. Stop must not rewrite it into a discarded approval.
			var historical int64
			if err := e.pool.QueryRow(e.ctx, "SELECT min(id) FROM run_user_inputs WHERE run_id=$1 AND kind='approve_plan' AND applied_at IS NOT NULL", e.runID).Scan(&historical); err != nil {
				t.Fatal(err)
			}
			before = policyOwnerStopSnapshot(t, e.codexTestEnv, e.runID)
			if _, err := e.svc.ApplyInputs(e.ctx, e.wkr, e.runID, 1, []int64{historical}); err != nil {
				t.Fatalf("exact historical APPLIED retry: %v", err)
			}
			if policyOwnerStopSnapshot(t, e.codexTestEnv, e.runID) != before {
				t.Fatal("historical retry changed persisted history")
			}
			// Explicit terminal seed is only reader-fence proof. No product
			// running/completion transition or accepted-Stop authority is inferred.
			e.exec("UPDATE runs SET status='completed',finished_at=now() WHERE id=$1", e.runID)
			policyStopPendingApprovalReaders(t, e, approval)
			var pending bool
			if policyStopPendingApprovalRow(t, e, approval) != approvalBefore {
				t.Fatal("Stop/readers/receipts changed the full pending approval row")
			}
			if err := e.pool.QueryRow(e.ctx, "SELECT applied_at IS NULL AND disposition IS NULL AND settled_at IS NULL FROM run_user_inputs WHERE id=$1", approval).Scan(&pending); err != nil || !pending {
				t.Fatalf("readers settled/approved the inert row: pending=%v err=%v", pending, err)
			}
		})
	}
}

// A legitimate current-gate approval applied before Stop retains authority and
// its exact APPLIED retry remains immutable after acceptance.
func TestPolicyStopPendingApprovalAppliedHistoryLiveDB(t *testing.T) {
	e, approval := policyStopPendingApprovalFixture(t)
	if _, err := e.svc.AckInputs(e.ctx, e.wkr, e.runID, 1, []int64{approval}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.ApplyInputs(e.ctx, e.wkr, e.runID, 1, []int64{approval}); err != nil {
		t.Fatal(err)
	}
	if !policyAssociationApproval(t, e) {
		t.Fatal("missing legitimate applied approval before Stop")
	}
	history := policyStopPendingApprovalRow(t, e, approval)
	policyStopPendingApprovalAccept(t, e)
	before := policyOwnerStopSnapshot(t, e.codexTestEnv, e.runID)
	result, err := e.svc.ApplyInputs(e.ctx, e.wkr, e.runID, 1, []int64{approval})
	if err != nil || len(result.Inputs) != 1 || result.Inputs[0].ID != approval {
		t.Fatalf("exact APPLIED retry after Stop=%+v err=%v", result, err)
	}
	if !policyAssociationApproval(t, e) || policyStopPendingApprovalRow(t, e, approval) != history || policyOwnerStopSnapshot(t, e.codexTestEnv, e.runID) != before {
		t.Fatal("Stop/retry erased independent approval or changed persisted history")
	}
}

// Positive control through the same public Submit/delivery/receipt seams without
// Stop: the newly applied current-gate approval must still authorize the candidate.
func TestPolicyStopPendingApprovalOrdinaryControlLiveDB(t *testing.T) {
	e, approval := policyStopPendingApprovalFixture(t)
	if _, err := e.svc.AckInputs(e.ctx, e.wkr, e.runID, 1, []int64{approval}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.ApplyInputs(e.ctx, e.wkr, e.runID, 1, []int64{approval}); err != nil {
		t.Fatal(err)
	}
	if !policyAssociationApproval(t, e) {
		t.Fatal("ordinary independently applied current approval lost authority")
	}
}

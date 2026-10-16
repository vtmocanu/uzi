package workersvc

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Start with a real legacy publication and selection approval: the selection path
// freezes immediately, before the worker reports running. No completion interlock is seeded.
func legacyReconciliationGate(t *testing.T) (*gateFix, uuid.UUID) {
	t.Helper()
	f := newGateFix(t, 8)
	id := f.newRun("issue")
	f.mustPublish(id, gateReq(i64(1), uid()), 1)
	_, err := f.submit(id, "approve_plan", &AgentSelection{Source: AgentSourceOwn}, SubmitInputOptions{})
	if err != nil {
		t.Fatal(err)
	}
	frozen := reconciliationRow(t, f, id)
	if frozen.CompletionContractVersion.Valid || len(frozen.MilestonesFrozen) == 0 ||
		!jsonEqual(t, frozen.MilestonesCandidate, string(frozen.MilestonesFrozen)) {
		t.Fatalf("legacy approve-time freeze: version=%v frozen=%s candidate=%s",
			frozen.CompletionContractVersion, frozen.MilestonesFrozen, frozen.MilestonesCandidate)
	}
	// Model the worker applying the queued approval before its running report.
	f.exec(`UPDATE run_user_inputs SET consumed_at=now(), applied_at=now()
		WHERE run_id=$1 AND kind='approve_plan' AND gate_revision=1`, id)
	_, applied, _, err := f.report(id, StateRequest{State: "running", ClaimGeneration: i64(1),
		MilestonesCompleted: &[]string{"M1"}, MilestonesInProgress: &[]string{"M1"}})
	if err != nil || !applied || reconciliationRow(t, f, id).Status != "running" {
		t.Fatalf("approved running report: applied=%v err=%v", applied, err)
	}
	f.exec("UPDATE runs SET scope_ceiling=1 WHERE id=$1", id)
	return f, id
}

func assertLegacyReconciliationPreserved(t *testing.T, before, after store.Run) {
	t.Helper()
	if after.CompletionContractVersion.Valid || after.ContractRevision != before.ContractRevision ||
		!jsonEqual(t, after.MilestonesFrozen, string(before.MilestonesFrozen)) ||
		!jsonEqual(t, after.MilestonesCompleted, string(before.MilestonesCompleted)) ||
		!jsonEqual(t, after.MilestonesInProgress, string(before.MilestonesInProgress)) ||
		after.ScopeCeiling != before.ScopeCeiling ||
		after.BudgetMaxIterations != before.BudgetMaxIterations || after.BudgetWallSeconds != before.BudgetWallSeconds {
		t.Fatalf("legacy progress/scope/budget changed: before=%+v after=%+v", before, after)
	}
}

func TestReconciliationLegacySamePlanOmittedMilestonesLiveDB(t *testing.T) {
	for _, selected := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "selected"}[selected], func(t *testing.T) {
			f, id := legacyReconciliationGate(t)
			before := reconciliationRow(t, f, id)
			req := gateReq(i64(1), uid())
			req.Milestones = nil
			f.mustPublish(id, req, 2)
			presented := reconciliationRow(t, f, id)
			payload, ok, err := decodeGatePresentedPayload(presented.GatePresentedPayload)
			if err != nil || !ok || !jsonEqual(t, presented.MilestonesCandidate, string(before.MilestonesFrozen)) ||
				!jsonEqual(t, payload.Milestones, string(before.MilestonesFrozen)) {
				t.Fatalf("same-plan gate lost definitions: candidate=%s payload=%s err=%v",
					presented.MilestonesCandidate, presented.GatePresentedPayload, err)
			}
			var sel *AgentSelection
			if selected {
				sel = &AgentSelection{Source: AgentSourceOwn}
			}
			if _, err := f.submit(id, "approve_plan", sel, SubmitInputOptions{ExpectedGateRevision: i64(2)}); err != nil {
				t.Fatalf("ordinary approval: %v", err)
			}
			assertLegacyReconciliationPreserved(t, before, reconciliationRow(t, f, id))
		})
	}
}

func TestReconciliationLegacyNULLRequiresFreshValidatedCandidateLiveDB(t *testing.T) {
	for _, rejected := range []bool{false, true} {
		t.Run(map[bool]string{false: "preexisting NULL", true: "rejected list"}[rejected], func(t *testing.T) {
			f, id := legacyReconciliationGate(t)
			before := reconciliationRow(t, f, id)
			revision := int64(2)
			if rejected {
				req := gateReq(i64(1), uid())
				req.Milestones = &[]Milestone{{ID: "", Title: "invalid"}}
				f.mustPublish(id, req, revision)
				revision++
			} else {
				f.exec("UPDATE runs SET milestones_candidate=NULL WHERE id=$1", id)
			}
			req := gateReq(i64(1), uid())
			req.Milestones = nil
			f.mustPublish(id, req, revision)
			held := reconciliationRow(t, f, id)
			payload, ok, err := decodeGatePresentedPayload(held.GatePresentedPayload)
			if len(held.MilestonesCandidate) != 0 || err != nil || !ok || string(payload.Milestones) != "null" {
				t.Fatalf("NULL was repaired: candidate=%s payload=%s err=%v", held.MilestonesCandidate, held.GatePresentedPayload, err)
			}
			for _, sel := range []*AgentSelection{nil, {Source: AgentSourceOwn}} {
				if _, err := f.submit(id, "approve_plan", sel, SubmitInputOptions{ExpectedGateRevision: &revision}); !errors.Is(err, ErrReconciliationRequired) {
					t.Fatalf("NULL ordinary approval: %v", err)
				}
			}
			digest, err := ResetStateDigest(held)
			if err != nil {
				t.Fatal(err)
			}
			_, err = f.submit(id, "approve_plan", nil, SubmitInputOptions{
				ExpectedGateRevision: &revision, ExpectedResetStateDigest: digest,
				Reconciliation: &apitypes.MilestoneReconciliation{Mode: "reset", Reason: "cannot infer NULL"},
			})
			if !errors.Is(err, ErrReconciliationConflict) {
				t.Fatalf("NULL reset refusal: %v", err)
			}
			assertLegacyReconciliationPreserved(t, before, reconciliationRow(t, f, id))
			// Only a fresh report with validated definitions restores ordinary approval.
			revision++
			f.mustPublish(id, gateReq(i64(1), uid()), revision)
			if _, err := f.submit(id, "approve_plan", nil, SubmitInputOptions{ExpectedGateRevision: &revision}); err != nil {
				t.Fatalf("fresh validated candidate recovery: %v", err)
			}
			assertLegacyReconciliationPreserved(t, before, reconciliationRow(t, f, id))
		})
	}
}

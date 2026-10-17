package workersvc

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strconv"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
)

// This exercises the real query and assembleClaim against persisted receipt rows.
// It does not exercise Claim admission/reclaim or prove the source writer: the
// complete source is seeded explicitly. A vertical Claim/ACK test is still needed.
func TestPolicyRevisionApprovalClaimLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name         string
		crossCheck   bool
		approvalGate int64
		constrained  bool
	}{
		{"non_cc_old_gate", false, 2, true},
		{"non_cc_same_gate_before_revision", false, 3, true},
		{"cc_old_gate_negative_control", true, 2, true},
		{"cc_same_gate_before_revision", true, 3, true},
		{"ordinary_historical_approval_control", false, 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := seedPolicyRevisionApprovalClaim(t, tc.crossCheck, tc.approvalGate, tc.constrained)
			before := mustRun(t, e.codexTestEnv, e.runID)
			claim, err := e.svc.assembleClaim(e.ctx, e.wkr, before)
			if err != nil {
				t.Fatalf("assembleClaim fixture blocked: %v", err)
			}
			if claim == nil {
				t.Fatal("assembleClaim returned no claim")
			}
			if claim.PlanApproved != !tc.constrained {
				t.Errorf("PlanApproved = %v, want %v: historical approval must not authorize a pending constrained revision", claim.PlanApproved, !tc.constrained)
			}
			wantPhase := "awaiting_approval"
			if !tc.constrained {
				wantPhase = "implementing"
			}
			if claim.ResumePhase != wantPhase {
				t.Errorf("ResumePhase = %q, want %q", claim.ResumePhase, wantPhase)
			}
			if claim.SessionID == nil || *claim.SessionID != before.SessionID.String {
				t.Errorf("session was not preserved: %v", claim.SessionID)
			}
			if claim.PlanMd == nil || *claim.PlanMd != before.PlanMd.String {
				t.Errorf("current candidate plan was not retained: %v", claim.PlanMd)
			}
			frozen := []Milestone{{ID: "m1", Title: "Completed work"}, {ID: "m2", Title: "Earlier execution work"}}
			if claim.ExecutionMilestones == nil || !reflect.DeepEqual(*claim.ExecutionMilestones, frozen) {
				t.Errorf("execution milestones = %v, want frozen work %+v", claim.ExecutionMilestones, frozen)
			}
			if tc.constrained {
				candidate := frozen
				if tc.approvalGate != 3 {
					candidate = []Milestone{{ID: "m3", Title: "Unapproved revision"}}
				}
				if !reflect.DeepEqual(claim.Milestones, candidate) {
					t.Errorf("gate milestones = %+v, want current candidate %+v", claim.Milestones, candidate)
				}
				if claim.ResumeGateRevision != 3 || claim.ResumeGatePresentationID == nil ||
					*claim.ResumeGatePresentationID != uuid.UUID(before.GatePresentationID.Bytes) ||
					claim.ResumeGatePresented == nil {
					t.Errorf("current readable gate was lost: revision=%d presentation=%v requirements=%+v",
						claim.ResumeGateRevision, claim.ResumeGatePresentationID, claim.ResumeGatePresented)
				} else if !reflect.DeepEqual(claim.ResumeGatePresented.RequiredTools, []string{"git"}) {
					t.Errorf("presented tools = %v, want [git]", claim.ResumeGatePresented.RequiredTools)
				}
			}
			after := mustRun(t, e.codexTestEnv, e.runID)
			if !bytes.Equal(after.MilestonesCompleted, before.MilestonesCompleted) ||
				!bytes.Equal(after.MilestonesFrozen, before.MilestonesFrozen) ||
				!bytes.Equal(after.CompletionContract, before.CompletionContract) ||
				after.CompletionContractVersion != before.CompletionContractVersion ||
				after.ContractRevision != before.ContractRevision || after.ScopeCeiling != before.ScopeCeiling {
				t.Error("claim assembly changed execution completion accounting")
			}
		})
	}
}

// These no-revision contexts are seeded, not source-writer proof. The first two
// cases execute HasPolicyCurrentGateApproval through assembleClaim against real
// applied receipts; the remaining cases check that the flags cannot skip proof.
func TestPolicyRevisionApprovalNoRevisionClaimLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name         string
		approvalGate int64
		pending      bool
		auto         bool
		seeded       bool
		unknown      bool
		approved     bool
	}{
		{"current_applied_approval", 3, false, false, false, false, true},
		{"old_applied_approval", 2, false, false, false, false, false},
		{"known_auto", 2, false, true, false, false, true},
		{"known_seeded", 2, false, false, true, false, true},
		{"pending_blocks_auto", 3, true, true, false, false, false},
		{"pending_blocks_seeded", 3, true, false, true, false, false},
		{"unknown_blocks_current_approval", 3, false, false, false, true, false},
		{"unknown_blocks_auto", 3, false, true, false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := seedPolicyRevisionApprovalClaim(t, false, tc.approvalGate, true)
			run := mustRun(t, e.codexTestEnv, e.runID)
			source, err := apitypes.ParsePolicyRevisionSourceV1(run.PolicyRevisionSource)
			if err != nil {
				t.Fatal(err)
			}
			source.PrimaryRevision = nil
			raw, err := json.Marshal(source)
			if err != nil {
				t.Fatal(err)
			}
			if tc.unknown {
				raw = nil
			}
			planSource := "agent"
			if tc.seeded {
				planSource = planSourceSeeded
			}
			if !tc.pending {
				// Seed a completed revision before the explicit no-revision source.
				// This deliberately does not claim to exercise source acceptance.
				if _, err := e.pool.Exec(e.ctx, "UPDATE run_user_inputs SET applied_at=now() WHERE run_id=$1 AND kind='revise_plan'", e.runID); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := e.pool.Exec(e.ctx, "UPDATE runs SET policy_revision_source=$2,auto_approve=$3,plan_source=$4 WHERE id=$1", e.runID, raw, tc.auto, planSource); err != nil {
				t.Fatal(err)
			}
			run = mustRun(t, e.codexTestEnv, e.runID)
			claim, err := e.svc.assembleClaim(e.ctx, e.wkr, run)
			if err != nil || claim == nil {
				t.Fatalf("assembleClaim: claim=%v err=%v", claim, err)
			}
			if claim.PlanApproved != tc.approved {
				t.Errorf("PlanApproved = %v, want %v", claim.PlanApproved, tc.approved)
			}
			wantPhase := "awaiting_approval"
			if tc.approved {
				wantPhase = "implementing"
			}
			if claim.ResumePhase != wantPhase {
				t.Errorf("ResumePhase = %q, want %q", claim.ResumePhase, wantPhase)
			}
		})
	}
}

// The fixture intentionally seeds PolicyRevisionSourceV1 directly; this is not
// evidence that a runtime writer records the primary revision or its associations.
func seedPolicyRevisionApprovalClaim(t *testing.T, crossCheck bool, approvalGate int64, constrained bool) usageTailEnv {
	t.Helper()
	e := setupPolicyHold(t)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := e.pool.Exec(e.ctx, sql, args...); err != nil {
			t.Fatalf("fixture SQL: %v", err)
		}
	}
	e.sealBotPAT(t, e.userID)
	alias := e.seedStaticAPIKey(t, e.userID, "revision-claim-"+uuid.NewString(), codexToken("sk"))
	if err := e.svc.FreezeCodexBinding(e.ctx, e.userID, e.runID, alias, codexAuthModeAPIKey); err != nil {
		t.Fatalf("FreezeCodexBinding: %v", err)
	}
	e.wkr.ProtocolCapabilities = append(e.wkr.ProtocolCapabilities, capability.InputReceiptsV1, capability.InputInclusionV1)
	exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", e.wkr.ID, e.wkr.ProtocolCapabilities)

	// Present the actual gate before applying its approval. Same-gate cases
	// retain this presentation: the later revision has started but has not yet
	// published a replacement candidate. Old-gate cases publish revision 3 below.
	presentation := uuid.New()
	exec(`UPDATE runs SET auto_approve=false,plan_source='agent',
 plan_md='# Previously approved execution',gate_revision=$2,gate_presentation_id=$3,
 plan_cross_check_required=$4,required_tools='{git}',
 milestones_candidate='[{"id":"m1","title":"Completed work"},{"id":"m2","title":"Earlier execution work"}]',
 milestones_frozen='[{"id":"m1","title":"Completed work"},{"id":"m2","title":"Earlier execution work"}]'
 WHERE id=$1`, e.runID, approvalGate, presentation, crossCheck)
	initial := mustRun(t, e.codexTestEnv, e.runID)
	initialPayload, initialDigest, err := gatePayloadFromRow(initial).digest()
	if err != nil {
		t.Fatal(err)
	}
	exec("UPDATE runs SET gate_presented_payload=$2,gate_payload_digest=$3 WHERE id=$1", e.runID, initialPayload, initialDigest)
	// APPLIED is a real receipt preceding the new revise_plan. No approval
	// receipt is sent after the pending revision is created.
	var approval int64
	if err := e.pool.QueryRow(e.ctx, `INSERT INTO run_user_inputs
		(run_id,kind,gate_binding,gate_revision) VALUES ($1,'approve_plan','bound',$2) RETURNING id`,
		e.runID, approvalGate).Scan(&approval); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.AckInputs(e.ctx, e.wkr, e.runID, 1, []int64{approval}); err != nil {
		t.Fatalf("ACK historical approval: %v", err)
	}
	if _, err := e.svc.ApplyInputs(e.ctx, e.wkr, e.runID, 1, []int64{approval}); err != nil {
		t.Fatalf("APPLIED historical approval: %v", err)
	}

	count := 0
	if constrained {
		count = 1
	}
	plan := "# Previously approved execution"
	candidate := `[{"id":"m1","title":"Completed work"},{"id":"m2","title":"Earlier execution work"}]`
	if approvalGate != 3 {
		presentation = uuid.New()
		plan = "# Current unapproved revision"
		candidate = `[{"id":"m3","title":"Unapproved revision"}]`
	}
	exec(`UPDATE runs SET status='claimed',auto_approve=false,plan_source='agent',
		plan_md=$5,gate_revision=3,gate_presentation_id=$2,
		plan_cross_check_required=$3,policy_refusal_count=$4,policy_revision_source=NULL,
		milestones_candidate=$6::jsonb,milestones_completed='["m1"]'
		WHERE id=$1`, e.runID, presentation, crossCheck, count, plan, candidate)
	run := mustRun(t, e.codexTestEnv, e.runID)
	payload, digest, err := gatePayloadFromRow(run).digest()
	if err != nil {
		t.Fatalf("canonical gate payload: %v", err)
	}
	exec("UPDATE runs SET gate_presented_payload=$2,gate_payload_digest=$3 WHERE id=$1", e.runID, payload, digest)

	var revision int64
	if err := e.pool.QueryRow(e.ctx, `INSERT INTO run_user_inputs
		(run_id,kind,body,gate_binding,gate_revision)
		VALUES ($1,'revise_plan','Revise only the remaining work','bound',3) RETURNING id`,
		e.runID).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.AckInputs(e.ctx, e.wkr, e.runID, 1, []int64{revision}); err != nil {
		t.Fatalf("ACK pending revision: %v", err)
	}
	if constrained {
		inputID := apitypes.PolicyStopDecimal(strconv.FormatInt(revision, 10))
		source, err := json.Marshal(apitypes.PolicyRevisionSourceV1{
			Version: 1,
			Gate: &apitypes.PolicyStopGateContext{
				Revision: "3", PresentationID: uuid.UUID(run.GatePresentationID.Bytes).String(),
				PayloadDigest: hex.EncodeToString(digest),
			},
			PrimaryRevision:       &apitypes.PolicyRevisionRef{Source: "run_user_input", InputID: &inputID},
			AssociatedCrossChecks: []apitypes.PolicyRevisionRef{},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := apitypes.ParsePolicyRevisionSourceV1(source); err != nil {
			t.Fatalf("fixture source is not known full V1: %v", err)
		}
		exec("UPDATE runs SET policy_revision_source=$2 WHERE id=$1", e.runID, source)
	}
	var pending, applied bool
	if err := e.pool.QueryRow(e.ctx, `SELECT
		EXISTS(SELECT 1 FROM run_user_inputs WHERE id=$1 AND gate_binding='bound' AND gate_revision=3
			AND consumed_at IS NOT NULL AND consumed_worker_id=$3 AND consumed_claim_generation=1
			AND applied_at IS NULL AND included_at IS NULL AND inclusion_reported),
		EXISTS(SELECT 1 FROM run_user_inputs WHERE id=$2 AND applied_at IS NOT NULL AND id<$1)`,
		revision, approval, e.wkr.ID).Scan(&pending, &applied); err != nil || !pending || !applied {
		t.Fatalf("receipt fixture: pending=%v historical applied=%v err=%v", pending, applied, err)
	}
	return e
}

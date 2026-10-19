package workersvc

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
)

// This joins the production append writer to the transaction-bound approval
// reader used by claim assembly. Initial complete V1 is deliberately seeded;
// this does not prove bootstrap, an actual reclaim, or stopped execution.
func TestPolicyRevisionWriterUnknownApprovalLiveDB(t *testing.T) {
	for _, mode := range []string{"missing", "null", "wrong_identity"} {
		t.Run(mode, func(t *testing.T) {
			env, svc, w, lead, candidate := newAutomaticRoundsHarnessFixture(t, 2, "codex")
			svc.p.PolicyHoldNegotiated = true
			svc.p.PolicyHoldSnapshotEnabled = true
			svc.p.TerminalPendingLease = time.Hour
			w.ProtocolCapabilities = []string{capability.ProviderPolicyHoldV1, capability.CredentialSwitchV1, capability.CrossCheckV1, capability.CrossCheckRoundsV1, capability.CrossCheckCodexLeadV1}
			env.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", w.ID, w.ProtocolCapabilities)
			cc, err := svc.SubmitPlanCrossCheck(env.ctx, w, lead, 1, candidate, 1)
			if err != nil {
				t.Fatal(err)
			}
			child := uuid.UUID(cc.CheckerRunID.Bytes)
			env.exec("UPDATE runs SET worker_id=$2,status='running',claim_generation=1 WHERE id=$1", child, w.ID)
			if _, err := svc.DecidePlanCrossCheck(env.ctx, w, child, 1, "revise", "revise", []byte(`{"summary":"revise","items":[]}`)); err != nil {
				t.Fatal(err)
			}
			id := cc.ID.String()
			ref := apitypes.PolicyRevisionRef{Source: "plan_cross_check", CrossCheckID: &id}
			source := apitypes.PolicyRevisionSourceV1{Version: 1, AssociatedCrossChecks: []apitypes.PolicyRevisionRef{ref}}
			raw, err := json.Marshal(source)
			if err != nil {
				t.Fatal(err)
			}
			env.exec("UPDATE runs SET policy_refusal_count=1,policy_revision_source=$2 WHERE id=$1", lead, raw)
			e := usageTailEnv{codexTestEnv: env, svc: svc, wkr: w, runID: lead}
			if !mustRun(t, env, lead).AutoApprove {
				t.Fatal("fixture did not retain legacy auto-approval")
			}
			if policyAssociationApproval(t, e) {
				t.Fatal("a verified revision was approved")
			}
			seq := mustRun(t, env, lead).LastSeq + 1
			if err := svc.AppendMessagesForClaim(env.ctx, w, lead, []IncomingMessage{{Seq: seq, Kind: "plan_revising", Payload: revisionWriterPayload(t, source, ref)}}, i64p(1)); err != nil {
				t.Fatal(err)
			}
			taken, err := apitypes.ParsePolicyRevisionSourceV1(mustRun(t, env, lead).PolicyRevisionSource)
			if err != nil || taken.PrimaryRevision == nil || taken.PrimaryRevision.CrossCheckID == nil || *taken.PrimaryRevision.CrossCheckID != id {
				t.Fatalf("taken revision was not recorded: %+v %v", taken, err)
			}
			payload := []byte("{}")
			if mode == "null" {
				payload = []byte(`{"policy_revision":null}`)
			} else if mode == "wrong_identity" {
				wrong := uuid.NewString()
				payload = revisionWriterPayload(t, source, apitypes.PolicyRevisionRef{Source: "plan_cross_check", CrossCheckID: &wrong})
			}
			if err := svc.AppendMessagesForClaim(env.ctx, w, lead, []IncomingMessage{{Seq: seq + 1, Kind: "plan_revising", Payload: payload}}, i64p(1)); err != nil {
				t.Fatal(err)
			}
			if len(mustRun(t, env, lead).PolicyRevisionSource) != 0 {
				t.Fatal("unsupported revision retained verified coverage")
			}
			if policyAssociationApproval(t, e) {
				t.Fatal("unknown revision restored historical approval")
			}
			// Neither a later plan frame nor generation advancement establishes
			// current-source acceptance. The latter is fixture-seeded here.
			if err := svc.AppendMessagesForClaim(env.ctx, w, lead, []IncomingMessage{{Seq: seq + 2, Kind: "plan", Payload: []byte("{}")}}, i64p(1)); err != nil {
				t.Fatal(err)
			}
			if policyAssociationApproval(t, e) {
				t.Fatal("a later plan frame restored historical approval")
			}
			env.exec("UPDATE runs SET claim_generation=2 WHERE id=$1", lead)
			if policyAssociationApproval(t, e) {
				t.Fatal("generation advancement restored historical approval")
			}
		})
	}
}

func TestPolicyRevisionWriterLegacyApprovalControlLiveDB(t *testing.T) {
	t.Run("ordinary_implementation", func(t *testing.T) {
		e := setupPolicyHold(t)
		e.exec("UPDATE runs SET policy_refusal_count=1,auto_approve=true WHERE id=$1", e.runID)
		if !policyAssociationApproval(t, e) {
			t.Fatal("ordinary legacy implementation lost approval")
		}
		revisionWriterAppend(t, e, IncomingMessage{Seq: 1, Kind: "status", Payload: []byte("{}")})
		if !policyAssociationApproval(t, e) {
			t.Fatal("ordinary message erased legacy implementation approval")
		}
	})
	t.Run("first_refusal", func(t *testing.T) {
		e := setupPolicyHold(t)
		e.exec("UPDATE runs SET policy_refusal_count=0,auto_approve=true WHERE id=$1", e.runID)
		// Unknown revision capture before the first refusal must constrain later
		// approval once the run becomes subject to policy continuation.
		revisionWriterAppend(t, e, IncomingMessage{Seq: 1, Kind: "plan_revising", Payload: []byte("{}")})
		if !policyAssociationApproval(t, e) {
			t.Fatal("unconstrained legacy behavior changed")
		}
		e.exec("UPDATE runs SET policy_refusal_count=1 WHERE id=$1", e.runID)
		if policyAssociationApproval(t, e) {
			t.Fatal("first refusal revived unverified revision approval")
		}
	})
}

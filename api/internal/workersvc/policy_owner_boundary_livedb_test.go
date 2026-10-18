package workersvc

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/capability"
)

// Exercise an actual approved plan, refusal, preserving resume, public Claim and
// generation-fenced running report. Only the no-revision source witness is seeded;
// this component proof does not establish the runtime source writer.
func policyOwnerRunningContinuation(t *testing.T) usageTailEnv {
	t.Helper()
	env := setupCodexLiveDB(t)
	fx := newCodexClaimFix(t, env, false)
	fx.svc.p.RunTimeout = 16 * time.Hour
	fx.svc.p.PolicyHoldNegotiated = true
	fx.svc.p.PolicyHoldSnapshotEnabled = true
	fx.svc.p.PolicyStopNegotiated = true
	w := fx.claimant(t, true)
	w.ProtocolCapabilities = append(w.ProtocolCapabilities, capability.ProviderPolicyHoldV1, capability.CredentialSwitchV1, capability.InputReceiptsV1, capability.InputInclusionV1)
	env.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", w.ID, w.ProtocolCapabilities)
	p, err := fx.svc.Claim(env.ctx, w, nil)
	if err != nil || p == nil {
		t.Fatalf("initial public Claim=%v err=%v", p != nil, err)
	}
	gen := p.ClaimGeneration
	plan, session := "Approved ordinary implementation", "ordinary-policy-thread"
	presentation := uuid.New()
	milestones := []Milestone{{ID: "m1", Title: "one"}, {ID: "m2", Title: "two"}}
	if _, applied, err := fx.svc.SetState(env.ctx, w, fx.runID, StateRequest{State: "awaiting_approval", ClaimGeneration: &gen, PresentationID: &presentation, PlanMd: &plan, SessionID: &session, Milestones: &milestones}); err != nil || !applied {
		t.Fatalf("present plan: applied=%v err=%v", applied, err)
	}
	gate := mustRun(t, env, fx.runID)
	if _, err := fx.svc.SubmitInputWithOptions(env.ctx, fx.userID, fx.runID, "approve_plan", "", nil, SubmitInputOptions{ExpectedGateRevision: &gate.GateRevision}); err != nil {
		t.Fatalf("ordinary human approval: %v", err)
	}
	var approval int64
	if err := env.pool.QueryRow(env.ctx, "SELECT id FROM run_user_inputs WHERE run_id=$1 AND kind='approve_plan'", fx.runID).Scan(&approval); err != nil {
		t.Fatal(err)
	}
	if receipt, err := fx.svc.AckInputs(env.ctx, w, fx.runID, gen, []int64{approval}); err != nil || !receipt.Active {
		t.Fatalf("approval ACK=%+v err=%v", receipt, err)
	}
	if receipt, err := fx.svc.ApplyInputs(env.ctx, w, fx.runID, gen, []int64{approval}); err != nil || !receipt.Active {
		t.Fatalf("approval APPLIED=%+v err=%v", receipt, err)
	}
	completed := []string{"m1"}
	if _, applied, err := fx.svc.SetState(env.ctx, w, fx.runID, StateRequest{State: "running", ClaimGeneration: &gen, MilestonesCompleted: &completed}); err != nil || !applied {
		t.Fatalf("approved running: applied=%v err=%v", applied, err)
	}
	req := policyRequest()
	phase := "implementation"
	req.Phase = &phase
	req.SourceGeneration, req.ClaimGeneration = gen, gen
	held, _, err := fx.svc.ReportPolicyHold(env.ctx, w, fx.runID, req)
	if err != nil {
		t.Fatal(err)
	}
	seedPolicyOwnerStopSource(t, env, fx.runID)
	resumed, err := fx.svc.ResumePolicyHold(env.ctx, fx.userID, fx.runID, PolicyHoldActionToken(held), PolicyResumeRequest{})
	if err != nil || resumed.Status != "queued" || resumed.SessionID != held.SessionID || resumed.PolicyFreshClaimGeneration.Valid {
		t.Fatalf("preserving resume: status=%s err=%v", resumed.Status, err)
	}
	p, err = fx.svc.Claim(env.ctx, w, nil)
	if err != nil || p == nil {
		t.Fatalf("continuation public Claim=%v err=%v", p != nil, err)
	}
	if p.ClaimGeneration != gen+1 || !p.PlanApproved || p.ResumePhase != "implementing" || p.SessionID == nil || *p.SessionID != session {
		t.Fatalf("continuation lost applied approval/session: generation=%d approved=%v phase=%s session=%v", p.ClaimGeneration, p.PlanApproved, p.ResumePhase, p.SessionID)
	}
	gen = p.ClaimGeneration
	if _, applied, err := fx.svc.SetState(env.ctx, w, fx.runID, StateRequest{State: "running", ClaimGeneration: &gen}); err != nil || !applied {
		t.Fatalf("continuation running: applied=%v err=%v", applied, err)
	}
	r := mustRun(t, env, fx.runID)
	if r.Status != "running" || r.SessionID.String != session || r.HoldReason.Valid || r.PolicyRefusalCount <= 0 || r.ClaimGeneration != gen || !r.WorkerID.Valid || uuid.UUID(r.WorkerID.Bytes) != w.ID || r.PolicyFreshClaimGeneration.Valid || r.AutoApprove || r.PlanSource != "agent" || len(r.PolicyStopResolution) != 0 {
		t.Fatal("invalid ordinary constrained continuation")
	}
	return usageTailEnv{codexTestEnv: env, svc: fx.svc, wkr: w, runID: fx.runID, userID: fx.userID, repoID: fx.repoID}
}

// Only the ordinary scope ceiling and its updated_at may change. Compare every
// other run field and every preexisting input/check, including settlement fields.
func policyOwnerOrdinaryStopSnapshot(t *testing.T, e usageTailEnv) string {
	t.Helper()
	var snapshot string
	if err := e.pool.QueryRow(e.ctx, `SELECT jsonb_build_object(
 'run',to_jsonb(r)-ARRAY['scope_ceiling','updated_at'],
 'inputs',(SELECT coalesce(jsonb_agg(to_jsonb(i) ORDER BY i.id),'[]'::jsonb) FROM run_user_inputs i WHERE i.run_id=r.id AND i.kind<>'scope'),
 'checks',(SELECT coalesce(jsonb_agg(to_jsonb(c) ORDER BY c.id),'[]'::jsonb) FROM cross_checks c WHERE c.lead_run_id=r.id))::text
 FROM runs r WHERE r.id=$1`, e.runID).Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestPolicyOwnerRunningStopAfterContinuationLiveDB(t *testing.T) {
	e := policyOwnerRunningContinuation(t)
	r := mustRun(t, e.codexTestEnv, e.runID)
	// Applicable feedback arriving after continuation must remain actionable.
	var feedback int64
	if err := e.pool.QueryRow(e.ctx, `INSERT INTO run_user_inputs(run_id,kind,body,gate_binding,gate_revision)
 VALUES($1,'revise_plan','Keep this applicable feedback','bound',$2) RETURNING id`, e.runID, r.GateRevision).Scan(&feedback); err != nil {
		t.Fatal(err)
	}
	if receipt, err := e.svc.AckInputs(e.ctx, e.wkr, e.runID, r.ClaimGeneration, []int64{feedback}); err != nil || !receipt.Active {
		t.Fatalf("feedback ACK=%+v err=%v", receipt, err)
	}
	var baseline string
	e.svc.q = policyOwnerStopReadRace{Store: e.svc.q, afterRead: func() {
		// A later completed count must be recomputed by the locked ordinary writer.
		if _, err := e.pool.Exec(e.ctx, `UPDATE runs SET milestones_completed='["m1","m2"]' WHERE id=$1`, e.runID); err != nil {
			t.Fatal(err)
		}
		baseline = policyOwnerOrdinaryStopSnapshot(t, e)
	}}
	result, err := e.svc.SubmitInput(e.ctx, e.userID, e.runID, "stop", "", nil)
	if err != nil {
		t.Fatalf("tokenless ordinary running Stop: %v", err)
	}
	got := mustRun(t, e.codexTestEnv, e.runID)
	if result.Resumed || result.ScopeCeiling == nil || *result.ScopeCeiling != 2 || !got.ScopeCeiling.Valid || got.ScopeCeiling.Int32 != 2 || got.Status != "running" || result.PolicyStop != nil || len(got.PolicyStopResolution) != 0 {
		t.Fatalf("ordinary Stop changed continuation: result=%+v status=%s", result, got.Status)
	}
	if baseline == "" || policyOwnerOrdinaryStopSnapshot(t, e) != baseline {
		t.Fatal("ordinary Stop changed candidate/frozen/source/session/approval/grant/feedback")
	}
	var scope int
	if err := e.pool.QueryRow(e.ctx, "SELECT count(*) FROM run_user_inputs WHERE run_id=$1 AND kind='scope'", e.runID).Scan(&scope); err != nil || scope != 1 {
		t.Fatalf("scope audits=%d err=%v", scope, err)
	}
}

func TestPolicyOwnerStopRacingResumedRefusalLiveDB(t *testing.T) {
	e := policyOwnerRunningContinuation(t)
	r := mustRun(t, e.codexTestEnv, e.runID)
	var baseline string
	e.svc.q = policyOwnerStopReadRace{Store: e.svc.q, afterRead: func() {
		req := policyRequest()
		phase := "implementation"
		req.Phase = &phase
		req.SourceGeneration, req.ClaimGeneration = r.ClaimGeneration, r.ClaimGeneration
		held, _, err := e.svc.ReportPolicyHold(e.ctx, e.wkr, e.runID, req)
		if err != nil {
			t.Fatal(err)
		}
		resumed, err := e.svc.ResumePolicyHold(e.ctx, e.userID, e.runID, PolicyHoldActionToken(held), PolicyResumeRequest{})
		if err != nil || resumed.Status != "queued" {
			t.Fatalf("race preserving resume: status=%s err=%v", resumed.Status, err)
		}
		baseline = policyOwnerStopSnapshot(t, e.codexTestEnv, e.runID)
	}}
	_, err := e.svc.SubmitInput(e.ctx, e.userID, e.runID, "stop", "", nil)
	if !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("Stop crossed refusal/resume lock boundary: %v", err)
	}
	if baseline == "" || policyOwnerStopSnapshot(t, e.codexTestEnv, e.runID) != baseline {
		t.Fatal("stale ordinary Stop wrote scope or audits after external resume")
	}
}

func TestPolicyOwnerStopConstrainedRoutingLiveDB(t *testing.T) {
	for _, status := range []string{"queued", "claimed", "paused", "running"} {
		t.Run(status, func(t *testing.T) {
			e := newPolicyOwnerStopFixture(t, `["m1"]`)
			review := policyOwnerStopReview(t, e)
			holdToken := PolicyHoldActionToken(mustRun(t, e.codexTestEnv, e.runID))
			if _, err := e.pool.Exec(e.ctx, "UPDATE runs SET status=$2 WHERE id=$1", e.runID, status); err != nil {
				t.Fatal(err)
			}
			tokens := []string{holdToken}
			if status != "paused" {
				tokens = append(tokens, review.StopIfMatch)
			}
			if status != "running" {
				tokens = append(tokens, "")
			}
			for _, token := range tokens {
				baseline := policyOwnerStopSnapshot(t, e.codexTestEnv, e.runID)
				_, err := e.svc.SubmitInputWithOptions(e.ctx, e.userID, e.runID, "stop", "", nil, SubmitInputOptions{ActionIfMatch: token})
				reason := "policy_stop_review_unverifiable"
				switch token {
				case "":
					reason = "policy_stop_token_required"
				case holdToken:
					reason = "policy_stop_token_stale"
				}
				requirePolicyOwnerStopConflict(t, err, reason)
				if policyOwnerStopSnapshot(t, e.codexTestEnv, e.runID) != baseline {
					t.Fatal("constrained routing fell through to ordinary scope writer")
				}
			}
		})
	}
}

func TestPolicyOwnerStopRacingFirstRefusalLiveDB(t *testing.T) {
	e := setupPolicyHold(t)
	if _, err := e.pool.Exec(e.ctx, `UPDATE runs SET kind='issue',issue_iid=102,
 milestones_frozen='[{"id":"m1","title":"one"},{"id":"m2","title":"two"}]',
 milestones_completed='["m1"]' WHERE id=$1`, e.runID); err != nil {
		t.Fatal(err)
	}
	e.svc.q = policyOwnerStopReadRace{Store: e.svc.q, afterRead: func() {
		if _, _, err := e.svc.ReportPolicyHold(e.ctx, e.wkr, e.runID, policyRequest()); err != nil {
			t.Fatal(err)
		}
	}}
	_, err := e.svc.SubmitInput(e.ctx, e.userID, e.runID, "stop", "", nil)
	if !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("unpresented new policy hold must conflict: %v", err)
	}
	got := mustRun(t, e.codexTestEnv, e.runID)
	var inputs int
	if err := e.pool.QueryRow(e.ctx, "SELECT count(*) FROM run_user_inputs WHERE run_id=$1 AND kind IN ('scope','resume')", e.runID).Scan(&inputs); err != nil {
		t.Fatal(err)
	}
	if got.Status != "paused" || got.ScopeCeiling.Valid || inputs != 0 {
		t.Fatalf("stale Stop mutated hold: status=%s ceiling=%v inputs=%d", got.Status, got.ScopeCeiling, inputs)
	}
}

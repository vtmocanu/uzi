package workersvc

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func setupPolicyHold(t *testing.T) usageTailEnv {
	t.Helper()
	e := setupUsageTail(t)
	if _, err := e.pool.Exec(e.ctx, "UPDATE runs SET harness='codex' WHERE id=$1", e.runID); err != nil {
		t.Fatal(err)
	}
	e.svc.p.PolicyHoldNegotiated = true
	e.svc.p.PolicyHoldSnapshotEnabled = true
	e.svc.p.TerminalPendingLease = time.Hour
	e.wkr.ProtocolCapabilities = []string{capability.ProviderPolicyHoldV1, capability.CredentialSwitchV1}
	return e
}

func policyRequest() PolicyHoldRequest {
	tag, phase, correlation := "cyberPolicy", "planning", "pr-00000000000000000000000000000000-1"
	return PolicyHoldRequest{OutcomeID: uuid.NewString(), SourceGeneration: 1, ClaimGeneration: 1,
		Detail: "refused", Tag: &tag, Phase: &phase, CorrelationID: &correlation, Head: strings.Repeat("a", 40)}
}

func TestPolicyHoldResumeAndIdentityLiveDB(t *testing.T) {
	e := setupPolicyHold(t)
	req := policyRequest()
	before := mustRun(t, e.codexTestEnv, e.runID)
	if _, err := e.q.CreateRunInput(e.ctx, store.CreateRunInputParams{RunID: e.runID, Kind: "pause"}); err != nil {
		t.Fatal(err)
	}
	r, d, err := e.svc.ReportPolicyHold(e.ctx, e.wkr, e.runID, req)
	if err != nil || d != PolicyHoldApplied || r.Status != "paused" || r.PolicyRefusalCount != 1 {
		t.Fatalf("hold: status=%s count=%d disposition=%s err=%v", r.Status, r.PolicyRefusalCount, d, err)
	}
	if r.SessionID != before.SessionID || r.WorkerID != before.WorkerID || r.ClaimReleasedAt.Valid || r.Health != "ok" {
		t.Fatal("hold lost identity/affinity/health")
	}
	var pending int
	if err := e.pool.QueryRow(e.ctx, "SELECT count(*) FROM run_user_inputs WHERE run_id=$1 AND kind='pause' AND applied_at IS NULL", e.runID).Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("unsettled pause=%d err=%v", pending, err)
	}
	token := PolicyHoldActionToken(r)
	if _, err := e.svc.ResumePolicyHold(e.ctx, e.userID, e.runID, "", PolicyResumeRequest{Guidance: "must not insert"}); !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("missing precondition=%v", err)
	}
	if _, err := e.pool.Exec(e.ctx, "UPDATE runs SET status_since=now()-interval '1 minute' WHERE id=$1", e.runID); err != nil {
		t.Fatal(err)
	}
	resumed, err := e.svc.ResumePolicyHold(e.ctx, e.userID, e.runID, token, PolicyResumeRequest{Guidance: "Narrow the scope"})
	if err != nil || resumed.Status != "queued" || resumed.SessionID != before.SessionID || resumed.BudgetPausedSeconds < 59 || resumed.PolicyFreshClaimGeneration.Valid {
		t.Fatalf("resume: %+v err=%v", resumed, err)
	}
	if _, err := e.svc.ResumePolicyHold(e.ctx, e.userID, e.runID, token, PolicyResumeRequest{Guidance: "duplicate"}); !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("repeated resume=%v", err)
	}
	var guidance string
	if err := e.pool.QueryRow(e.ctx, "SELECT string_agg(body,',') FROM run_user_inputs WHERE run_id=$1 AND kind='follow_up'", e.runID).Scan(&guidance); err != nil || guidance != "Narrow the scope" {
		t.Fatalf("guidance=%q err=%v", guidance, err)
	}
	// Lost ACK replay after an owner action must preserve that action.
	r, d, err = e.svc.ReportPolicyHold(e.ctx, e.wkr, e.runID, req)
	if err != nil || d != PolicyHoldAlreadyApplied || r.Status != "queued" || r.PolicyRefusalCount != 1 {
		t.Fatalf("duplicate undid owner resume: %s %s %v", r.Status, d, err)
	}
	heldMeta := r
	// Reclaim without old custody; older evidence must convert under current ownership.
	if _, err := e.pool.Exec(e.ctx, "UPDATE runs SET status='claimed',claim_generation=2 WHERE id=$1", e.runID); err != nil {
		t.Fatal(err)
	}
	rejection := PolicyHoldRequest{OutcomeID: uuid.NewString(), SourceGeneration: 2, ClaimGeneration: 2, Detail: "resume_rejected"}
	r, d, err = e.svc.ReportPolicyHold(e.ctx, e.wkr, e.runID, rejection)
	if err != nil || d != PolicyHoldApplied || r.PolicyRefusalCount != 1 || r.PolicyLastTag != heldMeta.PolicyLastTag || r.PolicyLastPhase != heldMeta.PolicyLastPhase || r.PolicyLastCorrelationID != heldMeta.PolicyLastCorrelationID || !r.PolicyLastRefusedAt.Time.Equal(heldMeta.PolicyLastRefusedAt.Time) {
		t.Fatalf("metadata-free rejection: %s %+v %v", d, r, err)
	}
	if _, err := e.svc.ResumePolicyHold(e.ctx, e.userID, e.runID, token, PolicyResumeRequest{}); !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("old hold token accepted: %v", err)
	}
	req.ClaimGeneration = 2
	_, d, err = e.svc.ReportPolicyHold(e.ctx, e.wkr, e.runID, req)
	if err != nil || d != PolicyHoldResolvedNewer {
		t.Fatalf("older source=%s %v", d, err)
	}
}

func TestPolicyHoldAnyGenerationWithoutCustodyLiveDB(t *testing.T) {
	e := setupPolicyHold(t)
	if _, err := e.pool.Exec(e.ctx, "UPDATE runs SET status='claimed',claim_generation=3 WHERE id=$1", e.runID); err != nil {
		t.Fatal(err)
	}
	req := policyRequest()
	req.ClaimGeneration = 3
	r, d, err := e.svc.ReportPolicyHold(e.ctx, e.wkr, e.runID, req)
	if err != nil || d != PolicyHoldApplied || r.Status != "paused" || r.PolicyLastSourceGeneration.Int64 != 1 || r.ClaimGeneration != 3 {
		t.Fatalf("older-source conversion: %s %+v %v", d, r, err)
	}
	r, d, err = e.svc.ReportPolicyHold(e.ctx, e.wkr, e.runID, req)
	if err != nil || d != PolicyHoldAlreadyApplied || r.PolicyRefusalCount != 1 {
		t.Fatalf("conversion duplicate=%s count=%d err=%v", d, r.PolicyRefusalCount, err)
	}
}

func TestPolicyHoldAdmissionLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name, kind, harness, status string
		interactive                 bool
		gen                         int64
		want                        bool
	}{
		{"issue", "issue", "codex", "running", false, 1, true},
		{"task", "task", "codex", "running", false, 1, true},
		{"prompt", "prompt", "codex", "running", false, 1, true},
		{"self", "self_improve", "codex", "running", false, 1, true},
		{"chat", "chat", "codex", "running", false, 1, false},
		{"judge", "judge", "codex", "running", false, 1, false},
		{"job", "job", "codex", "running", false, 1, false},
		{"cross", "cross_check", "codex", "running", false, 1, false},
		{"interactive", "task", "codex", "running", true, 1, false},
		{"claude", "issue", "claude", "running", false, 1, false},
		{"stale", "issue", "codex", "running", false, 2, false},
		{"claimed_refusal", "issue", "codex", "claimed", false, 1, false},
		{"unverified_gate", "issue", "codex", "awaiting_approval", false, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := setupPolicyHold(t)
			if _, err := e.pool.Exec(e.ctx, "UPDATE runs SET kind=$2,harness=$3,status=$4,interactive=$5,issue_iid=CASE WHEN $2 IN ('issue','self_improve') THEN 101 END,repo_id=CASE WHEN $2 IN ('chat','judge','job') THEN NULL ELSE $6::uuid END,branch=CASE WHEN $2='task' THEN 'agent/policy-fixture' END,target_run_id=CASE WHEN $2 IN ('judge','cross_check') THEN id END,job_type=CASE WHEN $2='job' THEN 'research' END,report_only=($2='cross_check'),budget_wall_seconds=120 WHERE id=$1", e.runID, tc.kind, tc.harness, tc.status, tc.interactive, e.repoID); err != nil {
				t.Fatal(err)
			}
			req := policyRequest()
			req.ClaimGeneration = tc.gen
			_, d, _ := e.svc.ReportPolicyHold(e.ctx, e.wkr, e.runID, req)
			if (d == PolicyHoldApplied) != tc.want {
				t.Fatalf("disposition=%s want applied=%v", d, tc.want)
			}
			if !tc.want && mustRun(t, e.codexTestEnv, e.runID).PolicyRefusalCount != 0 {
				t.Fatal("rejected report mutated count")
			}
		})
	}
}

func TestPolicyHoldVerifiedRevisionLiveDB(t *testing.T) {
	e := setupPolicyHold(t)
	if _, err := e.pool.Exec(e.ctx, "UPDATE runs SET status='awaiting_approval',plan_md='Original plan',gate_revision=7,gate_presentation_id='00000000-0000-4000-8000-000000000007',milestones_candidate='[]' WHERE id=$1", e.runID); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.AppendMessagesForClaim(e.ctx, e.wkr, e.runID, []IncomingMessage{{Seq: 1, Kind: "plan_revising", Payload: []byte("{}")}}, i64p(1)); err != nil {
		t.Fatal(err)
	}
	req := policyRequest()
	req.ExpectedGateRevision = i64p(6)
	_, d, err := e.svc.ReportPolicyHold(e.ctx, e.wkr, e.runID, req)
	if err != nil || d != PolicyHoldPending {
		t.Fatalf("stale gate=%s %v", d, err)
	}
	req.ExpectedGateRevision = i64p(7)
	r, d, err := e.svc.ReportPolicyHold(e.ctx, e.wkr, e.runID, req)
	if err != nil || d != PolicyHoldApplied || r.GateRevision != 7 || r.PlanMd.String != "Original plan" || r.GatePresentationID.String() != "00000000-0000-4000-8000-000000000007" {
		t.Fatalf("source gate hold=%s %+v %v", d, r, err)
	}
	if _, err := e.svc.ResumePolicyHold(e.ctx, e.userID, e.runID, PolicyHoldActionToken(r), PolicyResumeRequest{Guidance: "Revise narrowly"}); err != nil {
		t.Fatal(err)
	}
}

func TestPolicyHoldQueuedLateWritersLiveDB(t *testing.T) {
	e := setupPolicyHold(t)
	r, d, err := e.svc.ReportPolicyHold(e.ctx, e.wkr, e.runID, policyRequest())
	if err != nil || d != PolicyHoldApplied {
		t.Fatalf("hold=%s %v", d, err)
	}
	if _, err := e.svc.ResumePolicyHold(e.ctx, e.userID, e.runID, PolicyHoldActionToken(r), PolicyResumeRequest{}); err != nil {
		t.Fatal(err)
	}
	reason, origin := "late", "provider_policy_refusal"
	for _, state := range []string{"failed", "awaiting_input", "awaiting_followup", "plan_rejected", "superseded"} {
		_, applied, err := e.svc.SetState(e.ctx, e.wkr, e.runID, StateRequest{State: state, ClaimGeneration: i64p(1), FailureReason: &reason, FailOrigin: &origin})
		if applied {
			t.Fatalf("late %s applied (err=%v)", state, err)
		}
	}
	if got := mustRun(t, e.codexTestEnv, e.runID); got.Status != "queued" || got.PolicyRefusalCount != 1 {
		t.Fatalf("late writer changed hold: %+v", got)
	}
}

func TestPolicyHoldCancelAndLostOwnershipLiveDB(t *testing.T) {
	e := setupPolicyHold(t)
	req := policyRequest()
	if _, err := e.pool.Exec(e.ctx, "UPDATE runs SET claim_released_at=now() WHERE id=$1", e.runID); err != nil {
		t.Fatal(err)
	}
	_, d, err := e.svc.ReportPolicyHold(e.ctx, e.wkr, e.runID, req)
	if err != nil || d != PolicyHoldUnauthenticatedHistoricalOutcome {
		t.Fatalf("released report=%s %v", d, err)
	}
	if _, err := e.pool.Exec(e.ctx, "UPDATE runs SET status='cancelled' WHERE id=$1", e.runID); err != nil {
		t.Fatal(err)
	}
	_, d, err = e.svc.ReportPolicyHold(e.ctx, e.wkr, e.runID, req)
	if err != nil || d != PolicyHoldUnauthenticatedHistoricalOutcome {
		t.Fatalf("released cancel without custody disposition=%s %v", d, err)
	}
}

func TestPolicyHoldClaimPlacementLiveDB(t *testing.T) {
	e := setupPolicyHold(t)
	if _, err := e.pool.Exec(e.ctx, "UPDATE runs SET status='queued',policy_refusal_count=1 WHERE id=$1", e.runID); err != nil {
		t.Fatal(err)
	}
	caps := []string{capability.CodexHarnessV1, capability.CodexRuntimeV2}
	if _, err := e.pool.Exec(e.ctx, "UPDATE workers SET protocol_capabilities=$2,last_heartbeat_at=now(),ephemeral=true,ephemeral_run_id=$3 WHERE id=$1", e.wkr.ID, caps, e.runID); err != nil {
		t.Fatal(err)
	}
	eligibility, err := e.svc.WorkerEligibilityForHealth(e.ctx, time.Now(), e.runID)
	if err != nil || eligibility.StrictEligible != 0 {
		t.Fatalf("incapable ephemeral eligible=%+v err=%v", eligibility, err)
	}
	caps = append(caps, capability.ProviderPolicyHoldV1)
	if _, err := e.pool.Exec(e.ctx, "UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", e.wkr.ID, caps); err != nil {
		t.Fatal(err)
	}
	eligibility, err = e.svc.WorkerEligibilityForHealth(e.ctx, time.Now(), e.runID)
	if err != nil || eligibility.StrictEligible != 1 {
		t.Fatalf("capable bound ephemeral eligible=%+v err=%v", eligibility, err)
	}
	n, err := e.q.CountOnlineWorkersSatisfyingPolicyHold(e.ctx, store.CountOnlineWorkersSatisfyingPolicyHoldParams{
		RunID: e.runID, HeartbeatCutoff: pgconv.Time(time.Now().Add(-time.Hour)), AffinityCutoff: pgconv.Time(time.Now().Add(-time.Hour)),
		CrossCheckEvaluatedAt: pgconv.Time(time.Now()), CrossCheckAffinityCutoff: pgconv.Time(time.Now().Add(-time.Hour)),
		StaleRequeueCutoff: pgconv.Time(time.Now().Add(-time.Hour)), DockerRepoAllowlist: []uuid.UUID{}, CodexCuratedModels: codexCuratedModelsSlice(),
	})
	if err != nil || n != 1 {
		t.Fatalf("policy diagnostic bound ephemeral=%d err=%v", n, err)
	}
}

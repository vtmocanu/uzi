package workersvc

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestPlanCrossCheckClaimInputLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	f := newSubscriptionFixture(t, env)
	resealBotPAT(t, env, f.userID)
	original := mustRun(t, env, f.runID)
	leadID := uuid.New()
	env.exec(`INSERT INTO runs (id,user_id,repo_id,worker_id,issue_iid,issue_title,issue_description,
   status,harness,auto_approve,plan_cross_check_required,claim_generation)
   VALUES ($1,$2,$3,$4,42,'lead issue','lead body','running','claude',true,true,1)`,
		leadID, f.userID, original.RepoID, f.workerID)
	env.exec(`UPDATE runs SET kind='cross_check',issue_iid=NULL,target_run_id=$2,report_only=true,
   budget_wall_seconds=1800,status='claimed',claim_generation=1,issue_title='lead issue',issue_description='lead body' WHERE id=$1`, f.runID, leadID)
	candidate := PlanCrossCheckCandidate{PlanMd: "Review this plan", Milestones: json.RawMessage(`[]`),
		RequiredCapabilities: []string{}, RequiredTools: []string{}, SizeClass: "s",
		BaseCommit: strings.Repeat("a", 40), PlanningDiff: "diff --git a/file b/file"}
	digest, err := candidate.Digest()
	if err != nil {
		t.Fatal(err)
	}
	env.exec(`INSERT INTO cross_checks (lead_run_id,stage,round,lead_claim_generation,plan_md,milestones,
   size_class,base_commit,planning_diff,candidate_digest,checker_run_id,checker_harness,deadline_at)
   VALUES ($1,'plan',1,1,$2,$3,'s',$4,$5,$6,$7,'codex',now()+interval '30 minutes')`,
		leadID, candidate.PlanMd, candidate.Milestones, candidate.BaseCommit, candidate.PlanningDiff, digest, f.runID)
	env.exec(`UPDATE users SET default_codex_model=$2, default_claude_model='claude-opus-4-8',default_codex_effort='high' WHERE id=$1`, f.userID, customCodexModel)
	svc := New(env.q, env.box, testParams())
	svc.SetTxBeginner(env.pool)
	child := mustRun(t, env, f.runID)
	worker := store.Worker{ID: f.workerID, UserID: f.userID, ProtocolCapabilities: []string{
		capability.CrossCheckV1, capability.CodexHarnessV1, capability.CodexRuntimeV2, capability.CodexCustomModelV1}}
	payload, err := svc.assembleClaim(env.ctx, worker, child)
	if err != nil {
		t.Fatal(err)
	}
	if payload.CrossCheck == nil || payload.CrossCheck.PlanMd != candidate.PlanMd || payload.CrossCheck.BaseCommit != candidate.BaseCommit ||
		payload.CrossCheck.PlanningDiff != candidate.PlanningDiff || payload.CrossCheck.LeadRunID != leadID.String() {
		t.Fatalf("stored candidate missing or changed: %+v", payload.CrossCheck)
	}
	if payload.Config.DefaultModel == nil || *payload.Config.DefaultModel != customCodexModel ||
		payload.Config.DefaultEffort == nil || *payload.Config.DefaultEffort != "high" {
		t.Fatal("child bypassed ordinary Codex model/effort lane")
	}
	var model, effort string
	if err := env.pool.QueryRow(env.ctx, `SELECT checker_model,checker_effort FROM cross_checks WHERE lead_run_id=$1`, leadID).Scan(&model, &effort); err != nil {
		t.Fatal(err)
	}
	if model != *payload.Config.DefaultModel || effort != *payload.Config.DefaultEffort {
		t.Fatal("recorded model/effort differ from delivered claim")
	}
	if payload.Secrets.Codex == nil || payload.Secrets.AnthropicOAuthToken != "" {
		t.Fatal("child claim mixed harness credentials")
	}
	inputJSON, err := json.Marshal(payload.CrossCheck)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(inputJSON), f.accessToken) || strings.Contains(string(inputJSON), payload.Secrets.ForgePAT) {
		t.Fatal("candidate DTO leaked worker-owned credential")
	}
	for _, tc := range []struct{ name, sql string }{
		{"lead generation", `UPDATE runs SET claim_generation=2 WHERE id=$1`},
		{"lead exit", `UPDATE runs SET status='awaiting_approval' WHERE id=$1`},
		{"decided", `UPDATE cross_checks SET verdict='approve' WHERE lead_run_id=$1`},
		{"expired", `UPDATE cross_checks SET deadline_at=now()-interval '1 second' WHERE lead_run_id=$1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env.exec(tc.sql, leadID)
			if err := svc.assemblePlanCrossCheckInput(env.ctx, worker, child, payload); !errors.Is(err, ErrCrossCheckRefused) {
				t.Fatalf("stale candidate accepted: %v", err)
			}
			env.exec(`UPDATE runs SET claim_generation=1,status='running' WHERE id=$1`, leadID)
			// Reset this synthetic refusal fixture after the synchronous lead-exit trigger.
			env.exec(`UPDATE cross_checks SET verdict='pending',reason_class=NULL,decided_at=NULL,deadline_at=now()+interval '30 minutes' WHERE lead_run_id=$1`, leadID)
			env.exec(`UPDATE runs SET status='claimed',claim_released_at=NULL WHERE id=$1`, f.runID)
		})
	}
	t.Run("child generation", func(t *testing.T) {
		stale := child
		stale.ClaimGeneration++
		if err := svc.assemblePlanCrossCheckInput(env.ctx, worker, stale, payload); !errors.Is(err, ErrCrossCheckRefused) {
			t.Fatalf("stale child accepted: %v", err)
		}
	})
	t.Run("wrong worker", func(t *testing.T) {
		wrong := worker
		wrong.ID = uuid.New()
		if err := svc.assemblePlanCrossCheckInput(env.ctx, wrong, child, payload); !errors.Is(err, ErrCrossCheckRefused) {
			t.Fatalf("foreign worker accepted: %v", err)
		}
	})
}

func TestPlanCrossCheckClaimBounds(t *testing.T) {
	c := PlanCrossCheckCandidate{PlanMd: "plan", Milestones: json.RawMessage(`[]`), RequiredCapabilities: []string{}, RequiredTools: []string{}, SizeClass: "s", BaseCommit: strings.Repeat("a", 40)}
	digest, err := c.Digest()
	if err != nil {
		t.Fatal(err)
	}
	row := store.CrossCheck{Stage: "plan", Round: 1, LeadRunID: uuid.New(), PlanMd: pgtype.Text{String: c.PlanMd, Valid: true}, Milestones: c.Milestones,
		RequiredCapabilities: c.RequiredCapabilities, RequiredTools: c.RequiredTools, SizeClass: pgtype.Text{String: c.SizeClass, Valid: true},
		BaseCommit: pgtype.Text{String: c.BaseCommit, Valid: true}, CandidateDigest: digest, DeadlineAt: pgtype.Timestamptz{Time: time.Now().Add(time.Minute), Valid: true}}
	if _, err := crossCheckClaimInput(row); err != nil {
		t.Fatal(err)
	}
	roundTwo := row
	roundTwo.Round, roundTwo.AutomaticRoundsEnabled, roundTwo.AutomaticRevisionLimit = 2, true, 2
	if _, err := crossCheckClaimInput(roundTwo); err != nil {
		t.Fatalf("admitted round 2 refused: %v", err)
	}
	roundTwo.AutomaticRoundsEnabled = false
	if _, err := crossCheckClaimInput(roundTwo); !errors.Is(err, ErrCrossCheckRefused) {
		t.Fatalf("round 2 without snapshot accepted: %v", err)
	}
	roundTwo.AutomaticRoundsEnabled, roundTwo.AutomaticRevisionLimit = true, 0
	if _, err := crossCheckClaimInput(roundTwo); !errors.Is(err, ErrCrossCheckRefused) {
		t.Fatalf("round 2 beyond snapshot accepted: %v", err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*store.CrossCheck)
	}{
		{"plan bytes", func(r *store.CrossCheck) { r.PlanMd.String = strings.Repeat("x", 256*1024+1) }},
		{"diff bytes", func(r *store.CrossCheck) { r.PlanningDiff.String = strings.Repeat("x", 512*1024+1) }},
		{"milestone count", func(r *store.CrossCheck) { r.Milestones = []byte("[" + strings.Repeat("{},", 64) + "{}]") }},
		{"base sha", func(r *store.CrossCheck) { r.BaseCommit.String = "main" }},
		{"digest mismatch", func(r *store.CrossCheck) { r.CandidateDigest = []byte("other") }},
		{"null milestones", func(r *store.CrossCheck) { r.Milestones = []byte("null") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := row
			tc.mutate(&bad)
			if _, err := crossCheckClaimInput(bad); !errors.Is(err, ErrCrossCheckRefused) {
				t.Fatalf("unbounded/mismatched candidate accepted: %v", err)
			}
		})
	}
}

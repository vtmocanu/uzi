package store_test

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestPlanCrossCheckApprovalBindingLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run e2e/run-store-it.sh")
	}
	cases := []struct {
		name     string
		mutate   func(*store.SetRunAutopilotPlanParams)
		checkSQL string
	}{
		{name: "approved exact candidate"},
		{name: "digest", mutate: func(p *store.SetRunAutopilotPlanParams) { p.CandidateDigest = []byte("other") }},
		{name: "plan", mutate: func(p *store.SetRunAutopilotPlanParams) { p.PlanMd = pgT("other") }},
		{name: "milestones", mutate: func(p *store.SetRunAutopilotPlanParams) { p.MilestonesFrozen = []byte(`[{"id":"other"}]`) }},
		{name: "capabilities", mutate: func(p *store.SetRunAutopilotPlanParams) { p.InferredCapabilities = []string{"docker"} }},
		{name: "tools", mutate: func(p *store.SetRunAutopilotPlanParams) { p.InferredTools = []string{"go"} }},
		{name: "size", mutate: func(p *store.SetRunAutopilotPlanParams) { p.SizeClass = pgT("m") }},
		{name: "non approval", checkSQL: `UPDATE cross_checks SET verdict = 'revise' WHERE lead_run_id = $1`},
		{name: "lead generation", checkSQL: `UPDATE cross_checks SET lead_claim_generation = 2 WHERE lead_run_id = $1`},
		{name: "same harness", checkSQL: `UPDATE runs SET harness = 'codex' WHERE id = $1`},
		{name: "child harness", checkSQL: `UPDATE cross_checks SET checker_harness = 'other' WHERE lead_run_id = $1`},
		{name: "child kind", checkSQL: `UPDATE runs SET kind = 'task', branch = 'test', target_run_id = NULL WHERE id = (SELECT checker_run_id FROM cross_checks WHERE lead_run_id = $1)`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f, done := setupAwaitingInput(ctx, t, dsn)
			defer done()
			mustExec(ctx, t, f.pool, `UPDATE runs SET plan_cross_check_required = true, auto_approve = true,
    plan_source = 'agent', harness = 'claude', claim_generation = 1 WHERE id = $1`, f.runID)
			childID := uuid.New()
			mustExec(ctx, t, f.pool, `INSERT INTO runs (id, user_id, repo_id, kind, target_run_id, harness, report_only, budget_wall_seconds,
    issue_title, issue_description, status) VALUES ($1,$2,$3,'cross_check',$4,'codex',true,1800,'check','check','running')`,
				childID, f.userID, f.repoID, f.runID)
			mustExec(ctx, t, f.pool, `INSERT INTO cross_checks (lead_run_id, stage, round, lead_claim_generation, plan_md,
    milestones, required_capabilities, required_tools, size_class, base_commit, candidate_digest,
    checker_run_id, checker_harness, verdict, deadline_at)
    VALUES ($1,'plan',1,1,'approved','[]','{}','{}','s',$2,$3,$4,'codex','approve',now()+interval '30 minutes')`,
				f.runID, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", []byte("digest"), childID)
			p := store.SetRunAutopilotPlanParams{ID: f.runID, WorkerID: pgU(f.workerID), PlanMd: pgT("approved"),
				CandidateDigest: []byte("digest"), InferredCapabilities: []string{}, InferredTools: []string{},
				SizeClass: pgT("s"), MilestonesFrozen: []byte(`[]`)}
			if tc.mutate != nil {
				tc.mutate(&p)
			}
			if tc.checkSQL != "" {
				mustExec(ctx, t, f.pool, tc.checkSQL, f.runID)
			}
			rows, err := f.q.SetRunAutopilotPlan(ctx, p)
			if err != nil {
				t.Fatal(err)
			}
			want := int64(0)
			if tc.name == "approved exact candidate" {
				want = 1
			}
			if rows != want {
				t.Fatalf("candidate binding wrote %d rows, want %d", rows, want)
			}
			if want == 0 {
				var untouched bool
				if err := f.pool.QueryRow(ctx, `SELECT plan_md IS NULL AND milestones_frozen IS NULL
      AND required_tools = '{}' AND size_class = '' FROM runs WHERE id=$1`, f.runID).Scan(&untouched); err != nil {
					t.Fatal(err)
				}
				if !untouched {
					t.Fatal("refused plan mutated approval-bearing fields")
				}
			}
		})
	}
}

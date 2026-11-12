package store_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4158: SetRunRunning's cross-check guard against a REAL Postgres, driven from the shared
// fixture fixtures/cross-check-running-guard/cases.json. The same file drives the agent fake's
// running rule (agent/test/fake-api-cross-check-running.test.ts), so for the lane, approval-binding,
// status and plan_md-payload axes the fixture names, the fake cannot accept a running report the
// api refuses without one of the two suites going red. The fake does not model the payload's
// milestones, required_tools or size_class.

type runningGuardCase struct {
	Name            string `json:"name"`
	Lane            string `json:"lane"`
	Approval        string `json:"approval"`
	Status          string `json:"status"`
	PayloadMismatch bool   `json:"payload_mismatch"`
	ExpectApplied   bool   `json:"expect_applied"`
}

func loadRunningGuardCases(t *testing.T) []runningGuardCase {
	t.Helper()
	raw, err := os.ReadFile("../../../fixtures/cross-check-running-guard/cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		Cases []runningGuardCase `json:"cases"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	if len(fx.Cases) == 0 {
		t.Fatal("fixture has no cases")
	}
	return fx.Cases
}

// runningGuardApproveInput records an approve_plan input bound to `revision` WITHOUT applying it.
func runningGuardApproveInput(ctx context.Context, t *testing.T, f *awaitingInputFixture, revision int64) int64 {
	t.Helper()
	input, err := f.q.CreateApprovePlanInput(ctx, store.CreateApprovePlanInputParams{
		RunID: f.runID, Body: pgT("{}"), AgentSource: pgT("own"),
		AgentExclusions: []byte("[]"), ExpectedGateRevision: pgtype.Int8{Int64: revision, Valid: true},
		ContractSource: []byte("[]"),
	})
	if err != nil {
		t.Fatalf("approve revision %d: %v", revision, err)
	}
	return input.ID
}

// runningGuardAutopilotPlan stores `plan` on an auto-approved required run the way the api does:
// SetRunAutopilotPlan, bound to an opposite-harness approve cross-check of the current generation.
func runningGuardAutopilotPlan(ctx context.Context, t *testing.T, f *awaitingInputFixture, plan string) {
	t.Helper()
	mustExec(ctx, t, f.pool, `UPDATE runs SET status = 'claimed' WHERE id = $1`, f.runID)
	childID := uuid.New()
	mustExec(ctx, t, f.pool, `INSERT INTO runs (id, user_id, repo_id, kind, target_run_id, harness, report_only, budget_wall_seconds,
		issue_title, issue_description, status) VALUES ($1,$2,$3,'cross_check',$4,'codex',true,1800,'check','check','running')`,
		childID, f.userID, f.repoID, f.runID)
	mustExec(ctx, t, f.pool, `INSERT INTO cross_checks (lead_run_id, stage, round, lead_claim_generation, plan_md,
		milestones, required_capabilities, required_tools, size_class, base_commit, candidate_digest,
		checker_run_id, checker_harness, verdict, deadline_at)
		VALUES ($1,'plan',1,1,$2,'[]','{}','{}','s',$3,$4,$5,'codex','approve',now()+interval '30 minutes')`,
		f.runID, plan, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", []byte("digest"), childID)
	rows, err := f.q.SetRunAutopilotPlan(ctx, store.SetRunAutopilotPlanParams{ID: f.runID, WorkerID: pgU(f.workerID),
		PlanMd: pgT(plan), CandidateDigest: []byte("digest"), InferredCapabilities: []string{}, InferredTools: []string{},
		SizeClass: pgT("s"), MilestonesFrozen: []byte(`[]`)})
	if err != nil || rows != 1 {
		t.Fatalf("SetRunAutopilotPlan: rows=%d err=%v", rows, err)
	}
}

func TestCrossCheckRunningGuardLiveDB(t *testing.T) {
	cases := loadRunningGuardCases(t)
	// Rows set after the park. required_auto and required_auto_parked are set up BEFORE any park
	// (auto_approve = true from the start), because SetRunAwaitingApproval clears auto_approve.
	lanes := map[string]string{
		"gated":                 "",
		"required_forced_human": "plan_cross_check_gated = false, plan_cross_check_required = true",
		"required_auto":         "",
		"required_auto_parked":  "",
		"ordinary":              "plan_cross_check_gated = false",
	}
	approvals := map[string]bool{"applied_bound_current": true, "unapplied": true, "stale_revision": true,
		"unbound": true, "superseded": true, "none": true}
	statuses := map[string]bool{"claimed": true, "awaiting_approval": true, "running": true}
	for _, c := range cases {
		if _, ok := lanes[c.Lane]; !ok {
			t.Fatalf("case %q: unknown lane %q", c.Name, c.Lane)
		}
		if !approvals[c.Approval] {
			t.Fatalf("case %q: unknown approval %q", c.Name, c.Approval)
		}
		if !statuses[c.Status] {
			t.Fatalf("case %q: unknown status %q", c.Name, c.Status)
		}
	}
	if os.Getenv("UZI_TEST_DATABASE_URL") == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}

	ctx := context.Background()
	ran := 0
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			f := setupGatedRun(ctx, t)
			autoStart := c.Lane == "required_auto" || c.Lane == "required_auto_parked"
			if autoStart {
				mustExec(ctx, t, f.pool, `UPDATE runs SET plan_cross_check_gated = false, plan_cross_check_required = true,
					auto_approve = true, plan_source = 'agent' WHERE id = $1`, f.runID)
			}
			var rev1 int64
			if c.Lane == "required_auto" {
				// An auto-approved required run stores its plan through SetRunAutopilotPlan (no park, no gate).
				if c.Approval != "none" || c.PayloadMismatch || c.Status == "awaiting_approval" {
					t.Fatalf("case %q: required_auto has no gate and is never awaiting_approval", c.Name)
				}
				runningGuardAutopilotPlan(ctx, t, f, "plan A")
			} else {
				// A plan is stored and presented in every other case, through the real park.
				gatedPark(ctx, t, f, "plan A", 1, nil)
				rev1 = publishPlanCrossCheckGate(ctx, t, f, "plan A")
			}
			if autoStart && c.Lane == "required_auto_parked" {
				var auto bool
				if err := f.pool.QueryRow(ctx, `SELECT auto_approve FROM runs WHERE id = $1`, f.runID).Scan(&auto); err != nil {
					t.Fatal(err)
				}
				if auto {
					t.Fatal("SetRunAwaitingApproval left auto_approve = true")
				}
			}
			switch c.Approval {
			case "applied_bound_current":
				gatedApprove(ctx, t, f, rev1)
			case "unapplied":
				runningGuardApproveInput(ctx, t, f, rev1)
			case "stale_revision":
				gatedApprove(ctx, t, f, rev1)
				gatedPark(ctx, t, f, "plan B", 1, nil)
				if rev2 := publishPlanCrossCheckGate(ctx, t, f, "plan B"); rev2 <= rev1 {
					t.Fatalf("revision did not advance: %d -> %d", rev1, rev2)
				}
			case "unbound":
				mustExec(ctx, t, f.pool, `INSERT INTO run_user_inputs (run_id, kind, body, consumed_at, applied_at, disposition)
					VALUES ($1, 'approve_plan', '{}', now(), now(), 'applied')`, f.runID)
			case "superseded":
				gatedApprove(ctx, t, f, rev1)
				mustExec(ctx, t, f.pool, `UPDATE run_user_inputs SET disposition = 'superseded'
					WHERE run_id = $1 AND kind = 'approve_plan'`, f.runID)
			case "none":
			}
			if c.PayloadMismatch {
				mustExec(ctx, t, f.pool, `UPDATE runs SET gate_presented_payload = jsonb_set(gate_presented_payload, '{plan_md}', '"other"') WHERE id = $1`, f.runID)
			}
			if set := lanes[c.Lane]; set != "" {
				mustExec(ctx, t, f.pool, "UPDATE runs SET "+set+" WHERE id = $1", f.runID)
			}
			switch c.Status {
			case "claimed":
				mustExec(ctx, t, f.pool, `UPDATE runs SET status = 'claimed' WHERE id = $1`, f.runID)
			case "running":
				mustExec(ctx, t, f.pool, `UPDATE runs SET status = 'claimed' WHERE id = $1`, f.runID)
				mustExec(ctx, t, f.pool, `UPDATE runs SET status = 'running' WHERE id = $1`, f.runID)
			}

			rows, err := f.q.SetRunRunning(ctx, store.SetRunRunningParams{ID: f.runID, WorkerID: pgU(f.workerID)})
			if err != nil {
				t.Fatalf("SetRunRunning: %v", err)
			}
			want := int64(0)
			if c.ExpectApplied {
				want = 1
			}
			if rows != want {
				t.Fatalf("SetRunRunning rows = %d, want %d", rows, want)
			}
			// Assert the persisted status, not only the row count: an accepted report must leave
			// the row running, and a refused one must leave the case's starting status in place.
			var got string
			if err := f.pool.QueryRow(ctx, `SELECT status FROM runs WHERE id = $1`, f.runID).Scan(&got); err != nil {
				t.Fatalf("read run status: %v", err)
			}
			wantStatus := c.Status
			if c.ExpectApplied {
				wantStatus = "running"
			}
			if got != wantStatus {
				t.Fatalf("run status after SetRunRunning = %q, want %q", got, wantStatus)
			}
			ran++
		})
	}
	if ran != len(cases) {
		t.Fatalf("ran %d of %d cases", ran, len(cases))
	}
}

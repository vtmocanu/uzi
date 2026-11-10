package store_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4158: SetRunRunning's cross-check guard against a REAL Postgres, driven from the shared
// fixture fixtures/cross-check-running-guard/cases.json. The same file drives the agent fake's
// running rule (agent/test/fake-api-cross-check-running.test.ts), so the fake cannot accept a
// running report the api refuses without one of the two suites going red.

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

func TestCrossCheckRunningGuardLiveDB(t *testing.T) {
	cases := loadRunningGuardCases(t)
	lanes := map[string]string{
		"gated":                 "",
		"required_forced_human": "plan_cross_check_gated = false, plan_cross_check_required = true",
		"required_auto":         "plan_cross_check_gated = false, plan_cross_check_required = true, auto_approve = true",
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
			// A plan is stored and presented in every case.
			gatedPark(ctx, t, f, "plan A", 1, nil)
			rev1 := publishPlanCrossCheckGate(ctx, t, f, "plan A")
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
			ran++
		})
	}
	if ran != len(cases) {
		t.Fatalf("ran %d of %d cases", ran, len(cases))
	}
}

package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestPlanCrossCheckIssueByteBoundsLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name     string
		title    string
		body     string
		accepted bool
	}{
		{"exact ASCII", strings.Repeat("t", 4096), strings.Repeat("b", 262144), true},
		{"exact UTF8", strings.Repeat("😀", 1024), strings.Repeat("😀", 65536), true},
		{"title UTF8 overflow", strings.Repeat("😀", 1024) + "x", "body", false},
		{"body UTF8 overflow", "title", strings.Repeat("😀", 65536) + "x", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			fx := setupPlanCrossCheckDeletion(ctx, t)
			f := fx.f
			mustExec(ctx, t, f.pool, `UPDATE runs SET auto_approve = true,
                issue_title = $2, issue_description = $3 WHERE id = $1`, f.runID, tc.title, tc.body)
			childID := uuid.New()
			_, err := f.q.CreatePlanCrossCheckChild(ctx, store.CreatePlanCrossCheckChildParams{
				ChildID: childID, ChildHarness: "codex", LeadRunID: f.runID, UserID: f.userID,
				WorkerID: pgU(f.workerID), ClaimGeneration: 1, BudgetWallSeconds: 300,
			})
			if tc.accepted {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("oversized child insert: %v, want no rows", err)
			}
			var present bool
			if err = f.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM runs WHERE id = $1)`, childID).Scan(&present); err != nil {
				t.Fatal(err)
			}
			if present != tc.accepted {
				t.Fatalf("inserted=%v want=%v", present, tc.accepted)
			}
			if present {
				var title, body string
				if err = f.pool.QueryRow(ctx, `SELECT issue_title, issue_description FROM runs WHERE id = $1`, childID).
					Scan(&title, &body); err != nil {
					t.Fatal(err)
				}
				if title != tc.title || body != tc.body {
					t.Fatal("accepted child issue text was truncated")
				}
			}
		})
	}
}

func TestPlanCrossCheckPresenceHelpersLiveDB(t *testing.T) {
	cases := []struct {
		name     string
		mutation string
		live     bool
		pending  bool
	}{
		{"live", "", true, true},
		{"default off forced row", "UPDATE runs SET plan_cross_check_required = false WHERE id = $1", false, true},
		{"expired", "UPDATE cross_checks SET deadline_at = now() - interval '1 second' WHERE lead_run_id = $1", false, true},
		{"historical generation", "UPDATE cross_checks SET lead_claim_generation = 0 WHERE lead_run_id = $1", false, true},
		{"settled", "UPDATE cross_checks SET verdict = 'approve', reason_class = 'approve', decided_at = now() WHERE lead_run_id = $1", false, false},
		{"lead parked", "UPDATE runs SET status = 'paused' WHERE id = $1", false, false},
		{"released", "UPDATE runs SET claim_released_at = now() WHERE id = $1", false, false},
		{"malformed legacy round", "UPDATE cross_checks SET round = 2 WHERE lead_run_id = $1", false, true},
		{"capable round 2", "", true, true},
		{"no check", "DELETE FROM cross_checks WHERE lead_run_id = $1", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			fx := setupPlanCrossCheckDeletion(ctx, t)
			f := fx.f
			if tc.name == "capable round 2" {
				// Recreate the fixture with its immutable automatic-round snapshot.
				mustExec(ctx, t, f.pool, `DELETE FROM cross_checks WHERE lead_run_id=$1`, f.runID)
				mustExec(ctx, t, f.pool, `INSERT INTO cross_checks
                    (id,lead_run_id,checker_run_id,stage,round,lead_claim_generation,
                     plan_md,milestones,size_class,base_commit,candidate_digest,checker_harness,
                     deadline_at,automatic_rounds_enabled,automatic_revision_limit)
                    VALUES ($1,$2,$3,'plan',2,1,'plan','[]','s',repeat('a',40),$4,'codex',
                            now()+interval '5 minutes',true,2)`,
					fx.crossCheckID, f.runID, fx.checkerID, []byte("test-digest"))
			}
			if tc.mutation != "" {
				mustExec(ctx, t, f.pool, tc.mutation, f.runID)
			}
			for _, helper := range []struct {
				name string
				want bool
				call func(context.Context, uuid.UUID) (bool, error)
			}{
				{"HasLivePlanCrossCheck", tc.live, f.q.HasLivePlanCrossCheck},
				{"HasPendingPlanCrossCheck", tc.pending, f.q.HasPendingPlanCrossCheck},
			} {
				got, err := helper.call(ctx, f.runID)
				if err != nil {
					t.Fatal(err)
				}
				if got != helper.want {
					t.Fatalf("%s=%v want=%v", helper.name, got, helper.want)
				}
				// A neighbouring lead has no check and must not inherit this one.
				got, err = helper.call(ctx, uuid.New())
				if err != nil {
					t.Fatal(err)
				}
				if got {
					t.Fatalf("%s matched unrelated lead", helper.name)
				}
			}
		})
	}
}

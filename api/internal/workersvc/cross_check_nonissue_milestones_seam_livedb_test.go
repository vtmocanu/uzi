package workersvc

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4555: the second prior lookup (inside the checker-child creation transaction) must
// also accept a row keyed on the legacy milestones. A prior row that appears only after the
// preflight transaction committed is the only way to reach it, so the test-only
// planCrossCheckAfterPreflightHook inserts one at that point.
func TestCrossCheckNonIssueMilestonesSecondLookupLegacyLiveDB(t *testing.T) {
	const raw = `[{"id":"m1","title":"  Title  "}]`
	for _, kind := range ccNINonIssueKinds {
		t.Run(kind, func(t *testing.T) {
			f := ccNIFixture(t, kind, "claude", false)
			c := f.c
			c.Milestones = json.RawMessage(raw)
			digest, ms := ccNIDigest(t, f, c, raw, 2)
			if string(ms) == raw {
				t.Fatalf("fixture does not exercise normalisation: %s", ms)
			}
			var inserted store.CrossCheck
			f.svc.planCrossCheckAfterPreflightHook = func() {
				child, err := f.env.q.CreatePlanCrossCheckChild(f.env.ctx, store.CreatePlanCrossCheckChildParams{
					ChildID: uuid.New(), ChildHarness: "codex", LeadRunID: f.lead, UserID: f.w.UserID,
					WorkerID: pgconv.UUID(f.w.ID), ClaimGeneration: 1, BudgetWallSeconds: 1800,
				})
				if err != nil {
					t.Fatalf("seed checker child: %v", err)
				}
				inserted, err = f.env.q.InsertPlanCrossCheck(f.env.ctx, store.InsertPlanCrossCheckParams{
					LeadRunID: f.lead, LeadClaimGeneration: 1, Round: 1, AutomaticRoundsEnabled: true, AutomaticRevisionLimit: 2,
					PlanMd: pgtype.Text{String: c.PlanMd, Valid: true}, Milestones: ms,
					RequiredCapabilities: []string{}, RequiredTools: []string{},
					SizeClass: pgtype.Text{String: "s", Valid: true}, BaseCommit: pgtype.Text{String: c.BaseCommit, Valid: true},
					PlanningDiff: pgtype.Text{String: c.PlanningDiff, Valid: true}, CandidateDigest: digest,
					CheckerRunID: child.ID, DeadlineAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
				})
				if err != nil {
					t.Fatalf("seed legacy row: %v", err)
				}
			}
			got, err := f.svc.SubmitPlanCrossCheck(f.env.ctx, f.w, f.lead, 1, c, 1)
			if inserted.ID == uuid.Nil {
				t.Fatal("hook did not run: the seam was not reached")
			}
			if err != nil {
				t.Fatalf("second-lookup legacy replay: %v", err)
			}
			if got.ID != inserted.ID || got.CheckerRunID != inserted.CheckerRunID {
				t.Fatalf("returned %v, want the legacy row %v", got.ID, inserted.ID)
			}
			if rows, children := f.counts(t); rows != 1 || children != 1 {
				t.Fatalf("rows=%d children=%d, want 1/1 (no second row or checker)", rows, children)
			}
		})
	}
}

// The same seam, but the submit is at lead generation 2 while the row that appears after the
// preflight carries lead generation 1 and the legacy digest: the second lookup must still
// refuse it as interrupted rather than replay it across generations.
func TestCrossCheckNonIssueMilestonesSecondLookupLegacyCrossGenerationLiveDB(t *testing.T) {
	const raw = `[{"id":"m1","title":"  Title  "}]`
	for _, kind := range ccNINonIssueKinds {
		t.Run(kind, func(t *testing.T) {
			f := ccNIFixture(t, kind, "claude", false)
			c := f.c
			c.Milestones = json.RawMessage(raw)
			digest, ms := ccNIDigest(t, f, c, raw, 2)
			f.setGeneration(2)
			// The checker child is created at the live lead generation (2); only the row is stale (1).
			var inserted store.CrossCheck
			f.svc.planCrossCheckAfterPreflightHook = func() {
				child, err := f.env.q.CreatePlanCrossCheckChild(f.env.ctx, store.CreatePlanCrossCheckChildParams{
					ChildID: uuid.New(), ChildHarness: "codex", LeadRunID: f.lead, UserID: f.w.UserID,
					WorkerID: pgconv.UUID(f.w.ID), ClaimGeneration: 2, BudgetWallSeconds: 1800,
				})
				if err != nil {
					t.Fatalf("seed checker child: %v", err)
				}
				inserted, err = f.env.q.InsertPlanCrossCheck(f.env.ctx, store.InsertPlanCrossCheckParams{
					LeadRunID: f.lead, LeadClaimGeneration: 1, Round: 1, AutomaticRoundsEnabled: true, AutomaticRevisionLimit: 2,
					PlanMd: pgtype.Text{String: c.PlanMd, Valid: true}, Milestones: ms,
					RequiredCapabilities: []string{}, RequiredTools: []string{},
					SizeClass: pgtype.Text{String: "s", Valid: true}, BaseCommit: pgtype.Text{String: c.BaseCommit, Valid: true},
					PlanningDiff: pgtype.Text{String: c.PlanningDiff, Valid: true}, CandidateDigest: digest,
					CheckerRunID: child.ID, DeadlineAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
				})
				if err != nil {
					t.Fatalf("seed legacy row: %v", err)
				}
			}
			_, err := f.svc.SubmitPlanCrossCheck(f.env.ctx, f.w, f.lead, 2, c, 1)
			if inserted.ID == uuid.Nil {
				t.Fatal("hook did not run: the seam was not reached")
			}
			if !errors.Is(err, ErrCrossCheckInterrupted) {
				t.Fatalf("second-lookup cross-generation legacy replay: want ErrCrossCheckInterrupted, got %v", err)
			}
			if rows, children := f.counts(t); rows != 1 || children != 1 {
				t.Fatalf("rows=%d children=%d, want 1/1", rows, children)
			}
			if got := ccNIStoredRow(t, f, 1); got.ID != inserted.ID || got.LeadClaimGeneration != 1 {
				t.Fatalf("stored row changed: id=%v gen=%d", got.ID, got.LeadClaimGeneration)
			}
		})
	}
}

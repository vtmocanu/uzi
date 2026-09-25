package workersvc

import (
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestPlanMilestonesParam pins issue #1626's server-side reading of a milestone-less plan: only
// an INTERLOCKED issue run's FIRST PLAN-BEARING report (non-blank plan_md, no stored plan_md)
// with ABSENT milestones becomes the explicit `[]`; a stored candidate is kept on a later
// milestone-less report of the SAME plan and reset to `[]` by a milestone-less DIFFERENT plan
// (F2), a rejected list stays sticky (NULL), and every other shape is milestonesParam unchanged.
func TestPlanMilestonesParam(t *testing.T) {
	interlocked := store.Run{Kind: runkind.Issue, CompletionContractVersion: pgtype.Int4{Int32: 1, Valid: true}}
	legacy := store.Run{Kind: runkind.Issue}
	plan := strPtr("# Plan")
	one := &[]Milestone{{ID: "m1", Title: "First"}}
	bad := &[]Milestone{{ID: "", Title: "no id"}}
	empty := &[]Milestone{}
	const twoJSON = `[{"id": "m1", "title": "First"}, {"id": "m2", "title": "Second"}]`
	withCandidate := interlocked
	withCandidate.MilestonesCandidate = []byte(twoJSON)
	withEmptyCandidate := interlocked
	withEmptyCandidate.MilestonesCandidate = []byte("[]")
	samePlanNullCandidate := interlocked
	samePlanNullCandidate.PlanMd = pgtype.Text{String: *plan, Valid: true}
	// B-a: an earlier plan-bearing report stored "# Plan A" and its milestone list was REJECTED
	// (candidate NULL). A DIFFERENT plan with no milestones must not infer `[]`.
	rejectedEarlierPlan := interlocked
	rejectedEarlierPlan.PlanMd = pgtype.Text{String: "# Plan A", Valid: true}
	planB := strPtr("# Plan B (revised)")
	emptyCandidateStoredPlan := withEmptyCandidate
	emptyCandidateStoredPlan.PlanMd = pgtype.Text{String: "# Plan A", Valid: true}
	corruptCandidateStoredPlan := interlocked
	corruptCandidateStoredPlan.MilestonesCandidate = []byte("{not json")
	corruptCandidateStoredPlan.PlanMd = pgtype.Text{String: "# Plan A", Valid: true}
	candidatePlanA := withCandidate
	candidatePlanA.PlanMd = pgtype.Text{String: "# Plan A", Valid: true}
	legacyWithCandidate := legacy
	legacyWithCandidate.MilestonesCandidate = []byte(twoJSON)

	cases := []struct {
		name string
		run  store.Run
		plan *string
		ms   *[]Milestone
		want string // "" = nil (SQL NULL)
	}{
		{"interlocked plan, absent milestones", interlocked, plan, nil, "[]"},
		{"interlocked plan, explicit empty", interlocked, plan, empty, "[]"},
		{"interlocked plan, real milestones", interlocked, plan, one, `[{"id":"m1","title":"First"}]`},
		{"interlocked plan, invalid list stays NULL", interlocked, plan, bad, ""},
		{"interlocked, no plan_md (claim-time report)", interlocked, nil, nil, ""},
		{"interlocked, blank plan_md", interlocked, strPtr(" \n\x00 "), nil, ""},
		{"legacy plan, absent milestones", legacy, plan, nil, ""},
		{"interlocked non-issue kind", store.Run{Kind: runkind.Chat, CompletionContractVersion: interlocked.CompletionContractVersion}, plan, nil, ""},
		// B1: a re-presented gate whose report carries no milestones never downgrades a stored
		// NON-EMPTY candidate: it is passed through unchanged (no stored plan_md to differ from).
		{"interlocked, absent milestones, non-empty candidate kept", withCandidate, plan, nil, twoJSON},
		{"interlocked, explicit empty, non-empty candidate kept", withCandidate, plan, empty, twoJSON},
		{"interlocked, absent milestones, stored [] stays []", withEmptyCandidate, plan, nil, "[]"},
		{"interlocked, real milestones replace the candidate", withCandidate, plan, one, `[{"id":"m1","title":"First"}]`},
		// A re-report of the SAME plan whose milestones previously resolved to NULL (a rejected
		// list) stays NULL rather than inferring the vacuous `[]`.
		{"interlocked, same plan re-reported after a NULL candidate", samePlanNullCandidate, plan, nil, ""},
		{"legacy, absent milestones, candidate not carried", legacyWithCandidate, plan, nil, ""},
		// B-a (sticky rejection): a NULL stored candidate beside a non-NULL stored plan_md means an
		// earlier list was rejected; only the FIRST plan-bearing report (stored plan_md NULL) infers.
		{"interlocked, different plan after a rejected list stays NULL", rejectedEarlierPlan, planB, nil, ""},
		// N2: an EXPLICIT `[]` after a rejected list is read like an absent list (sticky NULL), so
		// the rejection holds whichever worker sent the report; only the first report's `[]` counts.
		{"interlocked, different plan after a rejected list, explicit [] stays NULL", rejectedEarlierPlan, planB, empty, ""},
		{"interlocked, same plan after a rejected list, explicit [] stays NULL", samePlanNullCandidate, plan, empty, ""},
		{"interlocked, stored [] candidate, explicit [] stays []", emptyCandidateStoredPlan, planB, empty, "[]"},
		{"interlocked, different plan after a rejected list, valid list wins", rejectedEarlierPlan, planB, one, `[{"id":"m1","title":"First"}]`},
		{"interlocked, different plan after a rejected list, rejected again", rejectedEarlierPlan, planB, bad, ""},
		{"interlocked, revise without milestones keeps a stored []", emptyCandidateStoredPlan, planB, nil, "[]"},
		// F2 (issue #1626 review): a revise with a DIFFERENT plan and no milestones resets a stored
		// non-empty candidate to `[]`, so the superseded plan's criteria never freeze; the SAME plan
		// re-presented (a gate reclaim, compared NUL-stripped as plan_md is stored) keeps it (B1).
		{"interlocked, revise without milestones resets a stored non-empty candidate", candidatePlanA, planB, nil, "[]"},
		{"interlocked, new plan B absent milestones resets candidate", candidatePlanA, strPtr("# Plan B"), nil, "[]"},
		{"interlocked, new plan B explicit empty resets candidate", candidatePlanA, strPtr("# Plan B"), empty, "[]"},
		{"interlocked, plan A re-presented absent milestones keeps candidate", candidatePlanA, strPtr("# Plan A"), nil, twoJSON},
		{"interlocked, plan A re-presented explicit empty keeps candidate", candidatePlanA, strPtr("# Plan A"), empty, twoJSON},
		{"interlocked, plan A re-presented with a NUL keeps candidate", candidatePlanA, strPtr("# Pl\x00an A"), nil, twoJSON},
		{"interlocked, plan A re-presented with a NUL, explicit empty keeps candidate", candidatePlanA, strPtr("# Plan A\x00"), empty, twoJSON},
		{"interlocked, corrupt stored candidate beside a stored plan stays NULL", corruptCandidateStoredPlan, planB, nil, ""},
		{"interlocked, rejected list replaces a stored candidate with NULL", withCandidate, plan, bad, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := planMilestonesParam(tc.run, tc.plan, tc.ms)
			if tc.want == "" {
				if got != nil {
					t.Fatalf("got %q, want nil (SQL NULL)", got)
				}
				return
			}
			if string(got) != tc.want {
				t.Fatalf("got %q, want %s", got, tc.want)
			}
		})
	}
}

// TestEmptyContractIsVacuouslyMet: the `[]` source builds criteria:[] and computeUnmetCriteria
// reads it as (empty, verifiable) — the permit's grant precondition (issue #1626).
func TestEmptyContractIsVacuouslyMet(t *testing.T) {
	contract, err := buildCompletionContract([]byte("[]"))
	if err != nil {
		t.Fatalf("buildCompletionContract([]): %v", err)
	}
	unmet, ok := computeUnmetCriteria(store.Run{CompletionContract: contract, MilestonesFrozen: []byte("[]")})
	if !ok || len(unmet) != 0 {
		t.Fatalf("computeUnmetCriteria = (%v, %v), want (empty, true)", unmet, ok)
	}
}

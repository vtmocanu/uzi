package workersvc

import (
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestPlanMilestonesParam pins issue #1626's server-side reading of a milestone-less plan: only
// an INTERLOCKED issue run's PLAN-BEARING report (non-blank plan_md) with ABSENT milestones
// becomes the explicit `[]`; every other shape is milestonesParam unchanged.
func TestPlanMilestonesParam(t *testing.T) {
	interlocked := store.Run{Kind: runkind.Issue, CompletionContractVersion: pgtype.Int4{Int32: 1, Valid: true}}
	legacy := store.Run{Kind: runkind.Issue}
	plan := strPtr("# Plan")
	one := &[]Milestone{{ID: "m1", Title: "First"}}
	bad := &[]Milestone{{ID: "", Title: "no id"}}
	empty := &[]Milestone{}

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

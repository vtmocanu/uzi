package handler

import (
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestIsPlanningPhase pins the single shared predicate issue #321 exposes on both the
// run read model and the board card. The rule is deliberately a conjunction of FOUR
// inputs (kind guard, running status, iteration_count 0, no persisted plan): each row
// below isolates a factor whose omission a weaker rule would get wrong, so the table is
// the specification, not merely coverage.
func TestIsPlanningPhase(t *testing.T) {
	cases := []struct {
		name           string
		kind           string
		status         string
		iterationCount int32
		planMdPresent  bool
		want           bool
	}{
		{"issue running iter0 no-plan is planning", "issue", "running", 0, false, true},
		{"issue running iter1 with-plan is implementing", "issue", "running", 1, true, false},
		// Seeded startup: a plan is already persisted at iter 0, so a plan_md-empty-only
		// rule would wrongly call this planning.
		{"issue running iter0 with-plan (seeded) not planning", "issue", "running", 0, true, false},
		// Autopilot implementing with no plan persisted: an iteration-only rule keyed on
		// iter==0 handles this, but a plan_md-only rule would get it wrong.
		{"issue running iter1 no-plan (autopilot impl) not planning", "issue", "running", 1, false, false},
		// Revise re-plans at the gate — status is awaiting_approval, never running, so the
		// status guard excludes it.
		{"issue awaiting_approval iter0 no-plan not planning", "issue", "awaiting_approval", 0, false, false},
		// Kind guard — the discriminating control: a kind-less rule would call chat planning.
		{"chat running iter0 no-plan not planning", "chat", "running", 0, false, false},
		{"judge running iter0 no-plan not planning", "judge", "running", 0, false, false},
		// self_improve is genuinely planning-capable (matches the NOT IN ('chat','judge') set).
		{"self_improve running iter0 no-plan is planning", "self_improve", "running", 0, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isPlanningPhase(tc.kind, tc.status, tc.iterationCount, tc.planMdPresent); got != tc.want {
				t.Fatalf("isPlanningPhase(%q, %q, %d, %v) = %v, want %v",
					tc.kind, tc.status, tc.iterationCount, tc.planMdPresent, got, tc.want)
			}
		})
	}
}

// TestRunToDTOIsPlanning asserts the field is wired through the real store→DTO mapper,
// not just the predicate. runToDTO is a pure mapping, so a hand-built store.Run is enough
// to prove the serialized is_planning matches the predicate on the run's own columns —
// including that a blank/invalid plan_md counts as "no plan" (the TrimSpace path).
func TestRunToDTOIsPlanning(t *testing.T) {
	cases := []struct {
		name string
		run  store.Run
		want bool
	}{
		{
			name: "issue running iter0 blank plan is planning",
			run:  store.Run{Kind: "issue", Status: "running", IterationCount: 0, PlanMd: pgtype.Text{Valid: false}},
			want: true,
		},
		{
			name: "chat running iter0 not planning",
			run:  store.Run{Kind: "chat", Status: "running", IterationCount: 0, PlanMd: pgtype.Text{Valid: false}},
			want: false,
		},
		{
			name: "issue running iter1 with plan is implementing",
			run:  store.Run{Kind: "issue", Status: "running", IterationCount: 1, PlanMd: pgtype.Text{String: "# plan", Valid: true}},
			want: false,
		},
		{
			name: "issue running iter0 whitespace-only plan is planning",
			run:  store.Run{Kind: "issue", Status: "running", IterationCount: 0, PlanMd: pgtype.Text{String: "   \n\t", Valid: true}},
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := runToDTO(tc.run, "normal").IsPlanning; got != tc.want {
				t.Fatalf("runToDTO(%+v).IsPlanning = %v, want %v", tc.run, got, tc.want)
			}
		})
	}
}

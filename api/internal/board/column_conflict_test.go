package board

import "testing"

func TestResolveColumnConflictOrder(t *testing.T) {
	tests := []struct {
		name       string
		labels     []string
		position   map[string]int
		wantColumn string
	}{
		{
			name:       "Later wins when listed last",
			labels:     []string{"PRD", "In Progress", "Later"},
			position:   columns("In Progress", "Later"),
			wantColumn: "Later",
		},
		{
			name:       "Later wins when listed first",
			labels:     []string{"PRD", "Later", "In Progress"},
			position:   columns("In Progress", "Later"),
			wantColumn: "Later",
		},
		{
			name:       "In Progress wins when listed first",
			labels:     []string{"PRD", "In Progress", "Later"},
			position:   columns("Later", "In Progress"),
			wantColumn: "In Progress",
		},
		{
			name:       "In Progress wins when listed last",
			labels:     []string{"PRD", "Later", "In Progress"},
			position:   columns("Later", "In Progress"),
			wantColumn: "In Progress",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			column, closed, conflict := ResolveColumn(tc.labels, "opened", tc.position)
			if column != tc.wantColumn || closed || !conflict {
				t.Errorf(
					"ResolveColumn(%v, %q, %v) = (%q, %v, %v), want (%q, false, true)",
					tc.labels, "opened", tc.position,
					column, closed, conflict, tc.wantColumn,
				)
			}
		})
	}
}

func TestPlanLabelMoveExistingTarget(t *testing.T) {
	current := []string{"PRD", "In Progress", "Later"}
	cols := set("In Progress", "Later")

	tests := []struct {
		name          string
		target        string
		wantRemove    []string
		wantNewLabels []string
	}{
		{
			name:          "keep In Progress and remove Later",
			target:        "In Progress",
			wantRemove:    []string{"Later"},
			wantNewLabels: []string{"PRD", "In Progress"},
		},
		{
			name:          "keep Later and remove In Progress",
			target:        "Later",
			wantRemove:    []string{"In Progress"},
			wantNewLabels: []string{"PRD", "Later"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			add, remove, newLabels := PlanLabelMove(current, cols, tc.target)
			if len(add) != 0 {
				t.Errorf(
					"PlanLabelMove(%v, target=%q): add = %v, want empty",
					current, tc.target, add,
				)
			}
			if !sortedEqual(remove, tc.wantRemove) {
				t.Errorf(
					"PlanLabelMove(%v, target=%q): remove = %v, want %v",
					current, tc.target, remove, tc.wantRemove,
				)
			}
			if !sortedEqual(newLabels, tc.wantNewLabels) {
				t.Errorf(
					"PlanLabelMove(%v, target=%q): newLabels = %v, want %v",
					current, tc.target, newLabels, tc.wantNewLabels,
				)
			}
		})
	}
}

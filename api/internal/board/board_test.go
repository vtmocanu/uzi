package board

import (
	"reflect"
	"sort"
	"testing"
)

func columns(names ...string) map[string]int {
	m := make(map[string]int, len(names))
	for i, n := range names {
		m[n] = i
	}
	return m
}

func TestResolveColumn(t *testing.T) {
	pos := columns("In Progress", "Upcoming", "Later") // positions 0,1,2

	tests := []struct {
		name         string
		labels       []string
		state        string
		wantColumn   string
		wantClosed   bool
		wantConflict bool
	}{
		{"closed goes to Closed regardless of labels", []string{"PRD", "In Progress"}, "closed", "", true, false},
		{"no column label is Open", []string{"PRD"}, "opened", "", false, false},
		{"single column label", []string{"PRD", "Upcoming"}, "opened", "Upcoming", false, false},
		{"multi-label shows highest position with conflict", []string{"PRD", "In Progress", "Later"}, "opened", "Later", false, true},
		{"non-column labels ignored", []string{"PRD", "bug"}, "opened", "", false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			col, closed, conflict := ResolveColumn(tc.labels, tc.state, pos)
			if col != tc.wantColumn || closed != tc.wantClosed || conflict != tc.wantConflict {
				t.Errorf("ResolveColumn(%v,%q) = (%q,%v,%v), want (%q,%v,%v)",
					tc.labels, tc.state, col, closed, conflict, tc.wantColumn, tc.wantClosed, tc.wantConflict)
			}
		})
	}
}

func set(names ...string) map[string]struct{} {
	m := make(map[string]struct{}, len(names))
	for _, n := range names {
		m[n] = struct{}{}
	}
	return m
}

func sortedEqual(a, b []string) bool {
	ac := append([]string(nil), a...)
	bc := append([]string(nil), b...)
	sort.Strings(ac)
	sort.Strings(bc)
	if len(ac) == 0 && len(bc) == 0 {
		return true
	}
	return reflect.DeepEqual(ac, bc)
}

func TestPlanLabelMove(t *testing.T) {
	cols := set("In Progress", "Upcoming", "Later")

	tests := []struct {
		name          string
		current       []string
		target        string
		wantAdd       []string
		wantRemove    []string
		wantNewLabels []string
	}{
		{
			name:          "move to a different column swaps atomically",
			current:       []string{"PRD", "In Progress"},
			target:        "Upcoming",
			wantAdd:       []string{"Upcoming"},
			wantRemove:    []string{"In Progress"},
			wantNewLabels: []string{"PRD", "Upcoming"},
		},
		{
			name:          "move to Open removes all column labels, adds none",
			current:       []string{"PRD", "In Progress"},
			target:        "",
			wantAdd:       nil,
			wantRemove:    []string{"In Progress"},
			wantNewLabels: []string{"PRD"},
		},
		{
			name:          "move to the same column is a no-op",
			current:       []string{"PRD", "Upcoming"},
			target:        "Upcoming",
			wantAdd:       nil,
			wantRemove:    nil,
			wantNewLabels: []string{"PRD", "Upcoming"},
		},
		{
			name:          "multi-column arrival is normalized to the single target",
			current:       []string{"PRD", "In Progress", "Later"},
			target:        "Upcoming",
			wantAdd:       []string{"Upcoming"},
			wantRemove:    []string{"In Progress", "Later"},
			wantNewLabels: []string{"PRD", "Upcoming"},
		},
		{
			name:          "non-column labels are preserved",
			current:       []string{"PRD", "bug", "Later"},
			target:        "In Progress",
			wantAdd:       []string{"In Progress"},
			wantRemove:    []string{"Later"},
			wantNewLabels: []string{"PRD", "bug", "In Progress"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			add, remove, newLabels := PlanLabelMove(tc.current, cols, tc.target)
			if !sortedEqual(add, tc.wantAdd) {
				t.Errorf("add = %v, want %v", add, tc.wantAdd)
			}
			if !sortedEqual(remove, tc.wantRemove) {
				t.Errorf("remove = %v, want %v", remove, tc.wantRemove)
			}
			if !sortedEqual(newLabels, tc.wantNewLabels) {
				t.Errorf("newLabels = %v, want %v", newLabels, tc.wantNewLabels)
			}
		})
	}
}

// Package board holds the pure kanban-column helpers shared across the API: which
// column a cached issue resolves to, and the atomic add/remove plan for a
// single-column move. They are intentionally dependency-free (plain slices and
// maps, no store or forge types) so the HTTP handler, the forge-sync service, the
// run service, and the lifecycle automation can all agree on one implementation
// without importing each other.
package board

// Column names the run-lifecycle automation targets. The rest of a repo's
// columns are operator-defined; only these two carry workflow meaning the server
// applies on its own (In Progress while a run works, Human Review once its MR is
// open).
const (
	ColumnInProgress  = "In Progress"
	ColumnHumanReview = "Human Review"
)

// ResolveColumn decides which column a card belongs to. Closed issues go to the
// implicit Closed column regardless of labels. Otherwise the card sits in its
// single column label, or Open ("") if it has none. An issue carrying more than
// one column label is shown in the highest-positioned one and flagged as a
// conflict until the next move normalizes it.
func ResolveColumn(labels []string, state string, position map[string]int) (column string, closed bool, conflict bool) {
	if state == "closed" {
		return "", true, false
	}
	best := ""
	bestPos := -1
	matches := 0
	for _, l := range labels {
		pos, isCol := position[l]
		if !isCol {
			continue
		}
		matches++
		if pos > bestPos {
			bestPos = pos
			best = l
		}
	}
	return best, false, matches > 1
}

// PlanLabelMove computes the atomic add/remove sets for a single-column move and
// the resulting label set for the cache. add is the target column (if not
// already present); remove is every OTHER column label the issue currently
// carries. Moving to Open (target == "") removes all column labels and adds none.
func PlanLabelMove(current []string, columnSet map[string]struct{}, target string) (add, remove, newLabels []string) {
	has := map[string]struct{}{}
	for _, l := range current {
		has[l] = struct{}{}
	}
	// remove: current column labels that are not the target.
	for _, l := range current {
		if _, isCol := columnSet[l]; isCol && l != target {
			remove = append(remove, l)
		}
	}
	// add: the target, if set and not already present.
	if target != "" {
		if _, present := has[target]; !present {
			add = append(add, target)
		}
	}
	// newLabels: current minus removed, plus target.
	removeSet := map[string]struct{}{}
	for _, l := range remove {
		removeSet[l] = struct{}{}
	}
	for _, l := range current {
		if _, dropped := removeSet[l]; dropped {
			continue
		}
		newLabels = append(newLabels, l)
	}
	if target != "" {
		if _, present := has[target]; !present {
			newLabels = append(newLabels, target)
		}
	}
	return add, remove, newLabels
}

package forgesvc

import (
	"testing"

	"gitlab.example.com/vtmocanu/uzi/api/internal/board"
)

// PRD #102 M2 / Decision 2. Nothing asserted on DefaultColumns before this, so the
// seeded set and its order were a silent contract: a reorder or a color change
// would have shipped green and only shown up on the next repo somebody connected.
//
// These assert the DECISIONS, not the literal slice — which of them is being broken
// is readable from the failure rather than from a diff of two slices.
func TestDefaultColumnsSeedInFlowOrder(t *testing.T) {
	names := make([]string, len(DefaultColumns))
	for i, c := range DefaultColumns {
		names[i] = c.Name
	}

	// Decision 2: reading order is flow order. The implicit Backlog lane is intake
	// (it has no label, so it is not in this slice), then selected, working, review,
	// and finally the deliberately-deferred bucket.
	want := []string{"Planned", board.ColumnInProgress, board.ColumnHumanReview, "Later"}
	if len(names) != len(want) {
		t.Fatalf("DefaultColumns = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("DefaultColumns[%d] = %q, want %q (full: %v)", i, names[i], want[i], names)
		}
	}

	// Planned leading is the whole point of the reorder: a column meaning "selected,
	// not yet started" must not render past the review lane, which is where the old
	// "workflow columns lead, backlog buckets follow" convention put it.
	if names[0] != "Planned" {
		t.Errorf("Planned must seed first, got %q at position 0", names[0])
	}
	// Human Review sits directly after In Progress; handler.humanReviewPlacement
	// retrofits older boards to that same relative position.
	for i, n := range names {
		if n == board.ColumnHumanReview && (i == 0 || names[i-1] != board.ColumnInProgress) {
			t.Errorf("Human Review at %d must follow In Progress, got %v", i, names)
		}
	}
}

func TestDefaultColumnsKeepUpcomingsColor(t *testing.T) {
	// Planned replaces Upcoming and inherits its color so an operator's existing
	// palette does not shift under them (PRD #102 M2). A bare presence check would
	// pass on any color, so the literal is the assertion.
	const upcomingColor = "#6699cc"
	var found bool
	for _, c := range DefaultColumns {
		if c.Name == "Planned" {
			found = true
			if c.Color != upcomingColor {
				t.Errorf("Planned color = %q, want %q (the color Upcoming shipped with)", c.Color, upcomingColor)
			}
		}
		if c.Name == "Upcoming" {
			t.Error("Upcoming must no longer be seeded; it was renamed to Planned")
		}
		if c.Color == "" {
			t.Errorf("column %q has no color; GitLab's label-create API requires one", c.Name)
		}
	}
	if !found {
		t.Error("Planned is not in DefaultColumns")
	}
}

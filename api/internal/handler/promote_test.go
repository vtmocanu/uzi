package handler

import (
	"testing"

	"github.com/vtmocanu/uzi/api/internal/forgesvc"
	"github.com/vtmocanu/uzi/api/internal/selfimprove"
)

// Promote's eligibility (PRD #102 Decisions 13a and 15).

// TestPromotableExcludesTheSelfImproveTracker: uzi's own self-improvement tracking
// issue is open on uzi's own repo and carries selfimprove.TrackingLabel
// deliberately INSTEAD of a PRD or autopilot label, so M6's additive fetch caches
// it and the toggle renders it as an ordinary non-PRD card. Promoting it would put
// the PRD label on internal machinery and make a self-improve run startable by hand
// from a board card.
//
// The check is server-side rather than only a hidden button: the endpoint is
// reachable whatever the web renders.
func TestPromotableExcludesTheSelfImproveTracker(t *testing.T) {
	cases := []struct {
		name   string
		labels []string
		want   bool
	}{
		{"an ordinary non-PRD issue", []string{"bug"}, true},
		{"an issue with no labels", []string{}, true},
		{"the self-improve tracker", []string{selfimprove.TrackingLabel}, false},
		{"the tracker alongside others", []string{"bug", selfimprove.TrackingLabel}, false},
		// Exact matching, like every other label comparison in this codebase.
		{"a different-cased near-miss is not the tracker", []string{"UZI-SELF-IMPROVE"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := promotable(tc.labels); got != tc.want {
				t.Fatalf("promotable(%v) = %v, want %v", tc.labels, got, tc.want)
			}
		})
	}
}

// TestPromoteDoesNotUseThePrdlessColor is the "naive reuse" trap Decision 15
// names, pinned as a value rather than as prose. SetIssueLabel auto-creates the
// label it applies, so promoting on a repo whose PRD label is somehow missing
// would create it in the escape hatch's amber if the constant were shared.
func TestPromoteDoesNotUseThePrdlessColor(t *testing.T) {
	if forgesvc.PrdLabelColor == forgesvc.PrdlessLabelColor {
		t.Fatalf("PrdLabelColor == PrdlessLabelColor (%s): a missing PRD label would be auto-created in the PRDLESS color",
			forgesvc.PrdLabelColor)
	}
	if forgesvc.PrdLabelColor == "" {
		t.Fatal("PrdLabelColor is empty; GitLab's label-create API requires a color")
	}
}

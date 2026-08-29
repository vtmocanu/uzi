package handler

import (
	"testing"

	"github.com/vtmocanu/uzi/api/internal/forgesvc"
	"github.com/vtmocanu/uzi/api/internal/schedsvc"
)

// Promote's eligibility (PRD #102 Decisions 13a and 15).

// TestPromotableExcludesTheSelfImproveTracker: uzi's own self-improvement tracking
// issue is open on uzi's own repo and carries schedsvc.SelfImproveTrackingLabel
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
		{"the self-improve tracker", []string{schedsvc.SelfImproveTrackingLabel}, false},
		{"the tracker alongside others", []string{"bug", schedsvc.SelfImproveTrackingLabel}, false},
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

// TestPromoteLabelColorIsSet pins that Promote carries a non-empty color for the
// uzi label it applies (PRD #764): SetIssueLabel auto-creates the label it applies,
// and GitLab's label-create API requires a color, so an empty constant would fail
// the promote on a repo whose uzi label is somehow missing.
func TestPromoteLabelColorIsSet(t *testing.T) {
	if forgesvc.PromoteLabelColor == "" {
		t.Fatal("PromoteLabelColor is empty; GitLab's label-create API requires a color")
	}
}

package forge

import (
	"testing"
	"time"
)

// TestFoldReviewDecision is the D6 table test: it exercises the one pure fold with
// the shapes each forge feeds it — GitHub (raw APPROVED/CHANGES_REQUESTED/COMMENTED/
// DISMISSED states + a requested-reviewer count), Forgejo (states the driver has
// already normalized to APPROVED/CHANGES_REQUESTED/COMMENTED + a requested count) —
// so the GraphQL reviewDecision semantics hold identically for both.
func TestFoldReviewDecision(t *testing.T) {
	ts := func(day int) time.Time { return time.Date(2024, 1, day, 0, 0, 0, 0, time.UTC) }

	cases := []struct {
		name      string
		reviews   []reviewInput
		requested int
		author    string
		want      ReviewDecision
	}{
		{
			name: "no reviews, none requested → none",
			want: ReviewNone,
		},
		{
			name:      "no reviews, one requested → review_required",
			requested: 1,
			want:      ReviewRequired,
		},
		{
			name:    "single approval → approved",
			reviews: []reviewInput{{ReviewerLogin: "alice", State: "APPROVED", SubmittedAt: ts(1)}},
			want:    ReviewApproved,
		},
		{
			name:    "single changes-requested → changes_requested",
			reviews: []reviewInput{{ReviewerLogin: "bob", State: "CHANGES_REQUESTED", SubmittedAt: ts(1)}},
			want:    ReviewChangesRequested,
		},
		{
			name: "changes-requested wins over another reviewer's approval",
			reviews: []reviewInput{
				{ReviewerLogin: "alice", State: "APPROVED", SubmittedAt: ts(1)},
				{ReviewerLogin: "bob", State: "CHANGES_REQUESTED", SubmittedAt: ts(1)},
			},
			want: ReviewChangesRequested,
		},
		{
			name: "latest per reviewer: approve then request changes → changes_requested",
			reviews: []reviewInput{
				{ReviewerLogin: "alice", State: "APPROVED", SubmittedAt: ts(1)},
				{ReviewerLogin: "alice", State: "CHANGES_REQUESTED", SubmittedAt: ts(2)},
			},
			want: ReviewChangesRequested,
		},
		{
			name: "latest per reviewer: request changes then approve → approved",
			reviews: []reviewInput{
				{ReviewerLogin: "alice", State: "CHANGES_REQUESTED", SubmittedAt: ts(1)},
				{ReviewerLogin: "alice", State: "APPROVED", SubmittedAt: ts(2)},
			},
			want: ReviewApproved,
		},
		{
			name: "a later COMMENTED review does not clear an earlier decision",
			reviews: []reviewInput{
				{ReviewerLogin: "alice", State: "APPROVED", SubmittedAt: ts(1)},
				{ReviewerLogin: "alice", State: "COMMENTED", SubmittedAt: ts(3)},
			},
			want: ReviewApproved,
		},
		{
			name: "comment-only reviews carry no decision → none (or required)",
			reviews: []reviewInput{
				{ReviewerLogin: "alice", State: "COMMENTED", SubmittedAt: ts(1)},
				{ReviewerLogin: "bob", State: "DISMISSED", SubmittedAt: ts(1)},
			},
			want: ReviewNone,
		},
		{
			name: "comment-only reviews but a reviewer requested → review_required",
			reviews: []reviewInput{
				{ReviewerLogin: "alice", State: "COMMENTED", SubmittedAt: ts(1)},
			},
			requested: 1,
			want:      ReviewRequired,
		},
		{
			name:   "the PR author's own approval is excluded → none",
			author: "octo",
			reviews: []reviewInput{
				{ReviewerLogin: "octo", State: "APPROVED", SubmittedAt: ts(1)},
			},
			want: ReviewNone,
		},
		{
			name: "lowercase / mixed-case states are normalized",
			reviews: []reviewInput{
				{ReviewerLogin: "alice", State: "changes_requested", SubmittedAt: ts(1)},
			},
			want: ReviewChangesRequested,
		},
		{
			name: "an empty reviewer login is ignored",
			reviews: []reviewInput{
				{ReviewerLogin: "", State: "CHANGES_REQUESTED", SubmittedAt: ts(1)},
			},
			want: ReviewNone,
		},
		{
			name: "two reviewers both approve → approved",
			reviews: []reviewInput{
				{ReviewerLogin: "alice", State: "APPROVED", SubmittedAt: ts(1)},
				{ReviewerLogin: "bob", State: "APPROVED", SubmittedAt: ts(2)},
			},
			want: ReviewApproved,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := foldReviewDecision(tc.reviews, tc.requested, tc.author)
			if got != tc.want {
				t.Fatalf("foldReviewDecision() = %q, want %q", got, tc.want)
			}
		})
	}
}

package forge

import (
	"strings"
	"time"
)

// reviewInput is the forge-neutral per-review record foldReviewDecision consumes
// (PRD #1255 D6). The GitHub and Forgejo drivers each translate their own review
// payloads into a []reviewInput and call the one fold; GitLab derives its decision
// separately (no per-reviewer review stream on the free tier). State is normalized
// UPPERCASE: "APPROVED", "CHANGES_REQUESTED", "COMMENTED", "DISMISSED", or any
// other value (all non-decision states are ignored by the fold).
type reviewInput struct {
	ReviewerLogin string
	State         string
	SubmittedAt   time.Time
}

// foldReviewDecision derives GitHub's GraphQL reviewDecision semantics from a flat
// list of reviews (PRD #1255 D6): keep the LATEST decision-bearing review per
// reviewer (excluding the PR author, and ignoring COMMENTED/DISMISSED and any
// non-decision state), then — any CHANGES_REQUESTED ⇒ changes_requested; else any
// APPROVED ⇒ approved; else if a review is still requested (requestedReviewers > 0)
// ⇒ review_required; else none. It is pure and side-effect-free so the drivers can
// share it and it can be table-tested against all three forges' inputs.
func foldReviewDecision(reviews []reviewInput, requestedReviewers int, authorLogin string) ReviewDecision {
	// latest holds each reviewer's newest decision-bearing state (APPROVED or
	// CHANGES_REQUESTED). A COMMENTED or DISMISSED review does not overwrite an
	// earlier decision — it simply carries none — so those are skipped entirely,
	// matching GitHub's model where a comment-only review neither approves nor blocks.
	latest := make(map[string]reviewInput, len(reviews))
	for _, r := range reviews {
		login := strings.TrimSpace(r.ReviewerLogin)
		if login == "" || login == strings.TrimSpace(authorLogin) {
			continue // the author's own review never counts toward the decision
		}
		state := strings.ToUpper(strings.TrimSpace(r.State))
		if state != "APPROVED" && state != "CHANGES_REQUESTED" {
			continue // COMMENTED / DISMISSED / PENDING / unknown carry no decision
		}
		prev, ok := latest[login]
		// Keep r when it is the first seen for this reviewer or is at/after the one
		// already kept (ties resolve to the later-in-slice review, which the drivers
		// order oldest→newest, so the freshest decision wins).
		if !ok || !r.SubmittedAt.Before(prev.SubmittedAt) {
			latest[login] = reviewInput{ReviewerLogin: login, State: state, SubmittedAt: r.SubmittedAt}
		}
	}
	anyApproved := false
	for _, r := range latest {
		if r.State == "CHANGES_REQUESTED" {
			return ReviewChangesRequested
		}
		if r.State == "APPROVED" {
			anyApproved = true
		}
	}
	if anyApproved {
		return ReviewApproved
	}
	if requestedReviewers > 0 {
		return ReviewRequired
	}
	return ReviewNone
}

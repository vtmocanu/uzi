// Package issuesource is the seam the services that read or write a repo's issues
// use to reach a linked external issue tracker (PRD #4173 DL-5). A service keeps
// its forge factory for the code-host side and for a repo whose link is NULL; for a
// linked repo it asks Sources, and a service with no Sources wired fails closed
// instead of falling back to the forge.
package issuesource

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/forge"
)

// Sources resolves the issue side of a repo, bound by repo row and owner.
// *forgesvc.Service satisfies it.
type Sources interface {
	IssueTrackerForRepo(ctx context.Context, userID, repoID uuid.UUID) (forge.IssueTracker, error)
}

// ErrNotWired is returned for a linked repo when the caller has no Sources: there
// is deliberately no fallback to the forge, whose issue numbers mean nothing there.
var ErrNotWired = errors.New("repo is linked to an issue tracker but no issue tracker source is wired")

// ErrIssueWriteNotOffered is returned for the one write to a linked external issue
// tracker that is not offered: a label write that removes the eligibility name
// (PRD #4173 M5).
// forgesvc.ErrIssueWriteNotOffered is this same value; it lives here so a package that
// cannot import forgesvc can wrap and match it.
var ErrIssueWriteNotOffered = errors.New("this issue write is not offered on a repo linked to an issue tracker")

// For returns the issue side a caller should use for one repo row. linked is
// whether the row's issue_tracker_connection_id is set. An unlinked row gets
// unlinked untouched (the caller's own forge driver, byte-identical to before M4);
// a linked row gets src's resolver, or ErrNotWired when src is nil.
func For(ctx context.Context, src Sources, linked bool, userID, repoID uuid.UUID, unlinked forge.IssueTracker) (forge.IssueTracker, error) {
	if !linked {
		return unlinked, nil
	}
	if src == nil {
		return nil, ErrNotWired
	}
	return src.IssueTrackerForRepo(ctx, userID, repoID)
}

// LinkedLabels is the label set an issue uzi creates on a repo linked to an issue tracker
// carries (PRD #4173 D12, D14): the configured uzi label becomes the link's stored
// eligibility name, the autopilot label is dropped because autopilot is not offered on a
// linked repo, and each name appears once, in the order first seen. A blank name is
// dropped. Only a linked repo's create site calls it; an unlinked repo keeps its labels
// untouched.
func LinkedLabels(labels []string, uziLabel, autopilotLabel, eligibilityName string) []string {
	out := make([]string, 0, len(labels))
	seen := make(map[string]struct{}, len(labels))
	for _, l := range labels {
		if l == autopilotLabel {
			continue
		}
		if l == uziLabel {
			l = eligibilityName
		}
		if l == "" {
			continue
		}
		if _, dup := seen[l]; dup {
			continue
		}
		seen[l] = struct{}{}
		out = append(out, l)
	}
	return out
}

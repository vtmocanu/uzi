// Package issuesource is the seam the services that read or write a repo's issues
// use to reach a linked external issue tracker (PRD #4173 DL-5). A service keeps
// its forge factory for the code-host side and for a repo whose link is NULL; for a
// linked repo it asks Sources, and a service with no Sources wired fails closed
// instead of falling back to the forge.
package issuesource

import (
	"context"
	"errors"
	"fmt"

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

// ErrMarkerConfersEligibility is returned when a label uzi adds to an issue on its own (a
// bookkeeping marker such as the finding marker, the self-improve tracking label or a
// proposal label) has the same name as the linked repo's stored eligibility tag. Creating
// the issue would tag it eligible for runs without anyone opting in, because the issue
// tracker driver resolves that name to the eligibility tag id (PRD #4173 D12, D14).
var ErrMarkerConfersEligibility = errors.New("a label uzi adds on its own would make this issue eligible for runs on the linked repo")

// ErrMarkerNotWritable is returned when a label uzi must add to an issue on its own cannot
// be written on the linked repo under its own name: the linked label set drops it (it is
// also the autopilot label) or rewrites it (it is also the uzi label). The issue would be
// created without its mandatory marker, so the create is refused instead.
var ErrMarkerNotWritable = errors.New("a label uzi adds on its own cannot be written on the linked repo")

// RequireMarker returns an error wrapping ErrMarkerNotWritable when marker is not in
// linkedLabels, the set LinkedLabels built for the create, and nil when it is. A blank
// marker is never required.
func RequireMarker(linkedLabels []string, marker string) error {
	if marker == "" {
		return nil
	}
	for _, l := range linkedLabels {
		if l == marker {
			return nil
		}
	}
	return fmt.Errorf("%w: the label %q is also a run label, which a repo linked to an issue tracker does not write; rename one of them", ErrMarkerNotWritable, marker)
}

// RefuseEligibilityMarkers returns an error wrapping ErrMarkerConfersEligibility naming the
// first of markers whose name equals eligibilityName, and nil when none does. The match is
// exact, as the driver's tag resolution is. A create site on a linked repo calls it with
// the markers it supplies implicitly, before any write, and refuses on an error instead of
// dropping the marker. A site that has an explicit opt-in (a label the owner chose, or
// send-to-uzi) does not call it. A blank name never matches, so an unlinked repo (no
// eligibility name) is a no-op.
func RefuseEligibilityMarkers(eligibilityName string, markers ...string) error {
	if eligibilityName == "" {
		return nil
	}
	for _, m := range markers {
		if m == eligibilityName {
			return fmt.Errorf("%w: the label %q is also this repo's issue tracker eligibility tag; rename one of them", ErrMarkerConfersEligibility, m)
		}
	}
	return nil
}

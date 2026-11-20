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

// ErrIssueWriteNotOffered is returned for every write to a linked external issue
// tracker (YouTrack writes arrive in PRD #4173 M5). forgesvc.ErrIssueWriteNotOffered
// is this same value; it lives here so a package that cannot import forgesvc can
// wrap and match it.
var ErrIssueWriteNotOffered = errors.New("issue writes on a repo linked to an issue tracker are not offered yet")

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

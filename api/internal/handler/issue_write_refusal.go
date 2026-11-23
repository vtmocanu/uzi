package handler

import (
	"context"
	"errors"
	"net/http"

	"github.com/vtmocanu/uzi/api/internal/forgesvc"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	"github.com/vtmocanu/uzi/api/internal/issuesource"
	"github.com/vtmocanu/uzi/api/internal/issuetracker"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// issueWriteNotOfferedMessage is the 409 body for a write a linked repo does not offer.
const issueWriteNotOfferedMessage = "this issue write is not offered on a repo linked to an issue tracker"

// linkedEligibility returns the stored eligibility tag name of a repo linked to an issue
// tracker (PRD #4173 D14), and whether the repo is linked at all.
func linkedEligibility(repo store.GetRepoForUserRow) (string, bool) {
	if !repo.IssueTrackerConnectionID.Valid {
		return "", false
	}
	return repo.EligibilityTagName.String, true
}

// createLabels returns the labels an issue uzi creates on repo carries: the configured
// uzi run-eligibility label (PRD #764). For a linked repo the uzi and autopilot labels come
// from one settings snapshot, the uzi label is replaced by the stored eligibility name and
// the autopilot label is dropped (PRD #4173 D12, D14). A settings read error is returned
// for any repo, since the default names it comes with cannot say which label the owner
// saved; the caller refuses before the claim and before any write (PRD #4173 M5 review).
func (h *Handler) createLabels(ctx context.Context, repo store.GetRepoForUserRow) ([]string, error) {
	eligibility, linked := linkedEligibility(repo)
	if !linked {
		uzi, err := h.settings.UziLabel(ctx)
		if err != nil {
			return nil, err
		}
		return []string{uzi}, nil
	}
	names, err := h.settings.FilingLabels(ctx)
	if err != nil {
		return nil, err
	}
	return issuesource.LinkedLabels([]string{names.Uzi}, names.Uzi, names.Autopilot, eligibility), nil
}

// writeIssueWriteNotOffered answers the typed refusal the linked-repo write guard
// returns (PRD #4173 M4; after M5 only the writes no unit has lifted) as a 409 instead of a 502/500,
// and reports whether it did. Callers that claimed something before the write have
// already released it by the time they call this.
func writeIssueWriteNotOffered(w http.ResponseWriter, err error) bool {
	if !errors.Is(err, forgesvc.ErrIssueWriteNotOffered) {
		return false
	}
	httpx.Error(w, http.StatusConflict, issueWriteNotOfferedMessage)
	return true
}

// writeLinkedMoveError answers a failed card move or State write on a repo linked
// to an issue tracker (PRD #4173 M5) and reports whether it did. A column or State
// the driver refused before any request is a 409, and a moved-away issue is a 404
// (D10); an issue the issue tracker shows as resolved is a 409 too, refused after the
// reads and before any write. In each the cache is unchanged, so the card snaps back. Any other error
// is left to the caller's 502.
func writeLinkedMoveError(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, issuetracker.ErrStateNotAllowed):
		httpx.Error(w, http.StatusConflict, "the target issue tracker State is not allowed for this move")
	case errors.Is(err, issuetracker.ErrStateMappingUnset):
		httpx.Error(w, http.StatusConflict, "the open and closed issue tracker States are not set for this repo")
	case forgesvc.LinkedIssueResolved(err):
		httpx.Error(w, http.StatusConflict, "the issue was resolved in the issue tracker; reopen it from the Closed lane after the next sync")
	case forgesvc.ColumnMoveRefused(err):
		httpx.Error(w, http.StatusConflict, "map this column to an issue tracker State first")
	case forgesvc.LinkedIssueGone(err):
		httpx.Error(w, http.StatusNotFound, "the issue is no longer in the linked project")
	default:
		return false
	}
	return true
}

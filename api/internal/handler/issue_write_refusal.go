package handler

import (
	"errors"
	"net/http"

	"github.com/vtmocanu/uzi/api/internal/forgesvc"
	"github.com/vtmocanu/uzi/api/internal/httpx"
)

// writeIssueWriteNotOffered answers the typed refusal the linked-repo write guard
// returns (PRD #4173 M4: YouTrack writes arrive in M5) as a 409 instead of a 502/500,
// and reports whether it did. Callers that claimed something before the write have
// already released it by the time they call this.
func writeIssueWriteNotOffered(w http.ResponseWriter, err error) bool {
	if !errors.Is(err, forgesvc.ErrIssueWriteNotOffered) {
		return false
	}
	httpx.Error(w, http.StatusConflict, "issue writes on a repo linked to an issue tracker are not offered yet")
	return true
}

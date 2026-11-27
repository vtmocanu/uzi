package handler

import (
	"net/http"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	"github.com/vtmocanu/uzi/api/internal/issuetrackersvc"
)

type issueTrackerColumnStateDTO struct {
	LabelName string `json:"label_name"`
	State     string `json:"state"`
}

// issueTrackerStateMappingDTO is the wire form of a repo's state mapping, for both the
// PUT body and the responses.
type issueTrackerStateMappingDTO struct {
	OpenState   string                       `json:"open_state"`
	ClosedState string                       `json:"closed_state"`
	Columns     []issueTrackerColumnStateDTO `json:"columns"`
}

// issueTrackerStateDTO is one State of the linked project with what uzi may do with it.
type issueTrackerStateDTO struct {
	Name     string `json:"name"`
	Resolved bool   `json:"resolved"`
	Archived bool   `json:"archived"`
	Closable bool   `json:"closable"`
	Openable bool   `json:"openable"`
}

func toIssueTrackerStateMappingDTO(m issuetrackersvc.StateMapping) issueTrackerStateMappingDTO {
	out := issueTrackerStateMappingDTO{
		OpenState: m.OpenState, ClosedState: m.ClosedState,
		Columns: make([]issueTrackerColumnStateDTO, 0, len(m.Columns)),
	}
	for _, c := range m.Columns {
		out.Columns = append(out.Columns, issueTrackerColumnStateDTO{LabelName: cleanTrackerText(c.LabelName), State: c.State})
	}
	return out
}

// GetIssueTrackerStateMapping returns the saved state mapping of a linked repo (null
// until one is saved) and the States of its project, each with whether uzi may write it as
// the closed state (closable) or as the open or a column state (openable)
// (GET /api/repos/{id}/issue-tracker-link/states). 404 for a repo that is not the user's,
// 409 for one that is not linked or whose project's State field is a state machine or
// cannot be read as a plain one.
func (h *Handler) GetIssueTrackerStateMapping(w http.ResponseWriter, r *http.Request) {
	userID, svc, ok := h.issueTrackerRequest(w, r)
	if !ok {
		return
	}
	repoID, ok := httpx.PathUUID(w, r, "id", "repo")
	if !ok {
		return
	}
	view, err := svc.GetStateMapping(r.Context(), userID, repoID)
	if err != nil {
		writeIssueTrackerError(w, err)
		return
	}
	states := make([]issueTrackerStateDTO, 0, len(view.States))
	for _, s := range view.States {
		states = append(states, issueTrackerStateDTO{
			Name: cleanTrackerText(s.Name), Resolved: s.Resolved, Archived: s.Archived,
			Closable: s.Closable, Openable: s.Openable,
		})
	}
	var mapping *issueTrackerStateMappingDTO
	if view.Mapping != nil {
		m := toIssueTrackerStateMappingDTO(*view.Mapping)
		mapping = &m
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"mapping": mapping, "states": states})
}

// PutIssueTrackerStateMapping replaces the whole state mapping of a linked repo
// (PUT /api/repos/{id}/issue-tracker-link/states) and returns what it saved. Refusals: 404
// for a repo that is not the user's, 409 for one that is not linked or whose project's State
// field is a state machine or cannot be read as a plain one, 400 for a name that is
// out of bounds, not a State of the linked project, or not allowed in its role (a closed
// state must mean done, never "Won't fix"), for a column label that is not a board column
// and for a State that is mapped twice.
func (h *Handler) PutIssueTrackerStateMapping(w http.ResponseWriter, r *http.Request) {
	userID, svc, ok := h.issueTrackerRequest(w, r)
	if !ok {
		return
	}
	repoID, ok := httpx.PathUUID(w, r, "id", "repo")
	if !ok {
		return
	}
	var req issueTrackerStateMappingDTO
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	in := issuetrackersvc.StateMapping{OpenState: req.OpenState, ClosedState: req.ClosedState}
	for _, c := range req.Columns {
		in.Columns = append(in.Columns, issuetrackersvc.ColumnState{LabelName: c.LabelName, State: c.State})
	}
	saved, err := svc.SetStateMapping(r.Context(), userID, repoID, in)
	if err != nil {
		writeIssueTrackerError(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"mapping": toIssueTrackerStateMappingDTO(saved)})
}

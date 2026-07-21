package handler

import (
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"gitlab.example.com/vtmocanu/uzi/api/internal/apitypes"
	"gitlab.example.com/vtmocanu/uzi/api/internal/httpx"
	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
)

// runListItemDTO (apitypes.RunListItemDTO) and adminWorkerDTO
// (apitypes.AdminWorkerDTO) moved to the stdlib-only apitypes leaf (PRD #64 M1);
// the mappers below stay here.

// ListRuns returns the current user's runs, newest first. Optional ?repo_id= and
// ?issue_iid= narrow the list (repo scope for the board attention strip, repo +
// issue for the in-app issue history); a malformed value is a 400.
func (h *Handler) ListRuns(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var repoID *uuid.UUID
	if s := r.URL.Query().Get("repo_id"); s != "" {
		id, err := uuid.Parse(s)
		if err != nil {
			httpx.Error(w, http.StatusBadRequest, "invalid repo_id")
			return
		}
		repoID = &id
	}
	var issueIID *int64
	if s := r.URL.Query().Get("issue_iid"); s != "" {
		iid, err := parseInt64(s)
		if err != nil {
			httpx.Error(w, http.StatusBadRequest, "invalid issue_iid")
			return
		}
		issueIID = &iid
	}
	rows, err := h.wsvc.ListRunsForUser(r.Context(), user.ID, repoID, issueIID)
	if err != nil {
		slog.Error("list runs", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]apitypes.RunListItemDTO, 0, len(rows))
	for _, row := range rows {
		item := apitypes.RunListItemDTO{
			RunDTO:     runToDTO(row.Run),
			RepoPath:   row.RepoPath,
			WorkerName: textPtrValue(row.WorkerName.Valid, row.WorkerName.String),
		}
		item.ForgeType = row.ForgeType // per-run MR/PR noun (PRD #65 D2)
		item.Usage = usageFromListRow(row) // nil when the run has no usage rows (PRD #40)
		out = append(out, item)
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"runs": out})
}

// AdminListRuns returns every non-terminal run across all users (admin-only,
// gated by RequireAdmin on the route). Powers the Agents-status overview.
func (h *Handler) AdminListRuns(w http.ResponseWriter, r *http.Request) {
	rows, err := h.wsvc.ListActiveRunsAll(r.Context())
	if err != nil {
		slog.Error("admin list runs", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]apitypes.RunListItemDTO, 0, len(rows))
	for _, row := range rows {
		email := row.OwnerEmail
		item := apitypes.RunListItemDTO{
			RunDTO:     runToDTO(row.Run),
			RepoPath:   row.RepoPath,
			WorkerName: textPtrValue(row.WorkerName.Valid, row.WorkerName.String),
			OwnerEmail: &email,
		}
		item.ForgeType = row.ForgeType // per-run MR/PR noun (PRD #65 D2)
		out = append(out, item)
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"runs": out})
}

// AdminListWorkers returns every worker with its owner email and busy status
// (admin-only, gated by RequireAdmin on the route).
func (h *Handler) AdminListWorkers(w http.ResponseWriter, r *http.Request) {
	rows, err := h.wsvc.ListAllWorkers(r.Context())
	if err != nil {
		slog.Error("admin list workers", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]apitypes.AdminWorkerDTO, 0, len(rows))
	for _, row := range rows {
		out = append(out, apitypes.AdminWorkerDTO{
			WorkerDTO:  workerDTOFromWorker(row.Worker, int(row.ActiveRuns), row.Busy, ""),
			OwnerEmail: row.OwnerEmail,
		})
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"workers": out})
}

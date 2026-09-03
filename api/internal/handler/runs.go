package handler

import (
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
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
	// Judge to-triage counts for the runs ON THIS PAGE (PRD #98 M4). Deliberately a
	// second query bucketed in Go rather than a join: see queries/runtime.sql. Best
	// effort — the judge badge is decoration, so a failure here logs and leaves every
	// count at 0 rather than failing the whole run list.
	runIDs := make([]uuid.UUID, 0, len(rows))
	for _, row := range rows {
		runIDs = append(runIDs, row.Run.ID)
	}
	todo, err := h.wsvc.JudgeTodoCountsForRuns(r.Context(), user.ID, runIDs)
	if err != nil {
		slog.Error("judge todo counts", "error", err)
		todo = nil
	}
	// Plan-revise display flag for the runs on this page (issue #750). Same best-effort
	// contract as the judge badge above: it is decoration, so a failure logs and leaves
	// every flag false (nil map indexes to false) rather than failing the run list.
	revising, err := h.wsvc.PlanRevisingForRuns(r.Context(), runIDs)
	if err != nil {
		slog.Error("plan revising states", "error", err)
		revising = nil
	}
	// PRD #1064 M2: the server-derived "now" line for the NON-TERMINAL runs on this
	// page (a finished run has no "now"). Same best-effort contract as the
	// judge/revising decoration above — a failure logs and leaves every current_activity
	// null rather than failing the run list.
	activeIDs := make([]uuid.UUID, 0, len(rows))
	for _, row := range rows {
		if !apitypes.IsTerminalRunStatus(row.Run.Status) {
			activeIDs = append(activeIDs, row.Run.ID)
		}
	}
	activity, err := h.wsvc.CurrentActivityForRuns(r.Context(), activeIDs)
	if err != nil {
		slog.Error("current activity", "error", err)
		activity = nil
	}

	out := make([]apitypes.RunListItemDTO, 0, len(rows))
	for _, row := range rows {
		item := apitypes.RunListItemDTO{
			RunDTO:     runToDTO(row.Run, row.PriorityClass),
			RepoPath:   row.RepoPath,
			WorkerName: textPtrValue(row.WorkerName.Valid, row.WorkerName.String),
		}
		item.ForgeType = row.ForgeType // per-run MR/PR noun (PRD #65 D2)
		// PRD #411: the joined forge issue web URL, nil for issue-less/uncached runs.
		item.IssueWebURL = textPtrValue(row.IssueWebUrl.Valid, row.IssueWebUrl.String)
		item.Usage = usageFromListRow(row) // nil when the run has no usage rows (PRD #40)
		// nil stays nil for an unjudged run — absent, not a neutral verdict.
		item.JudgeVerdict = textPtrValue(row.JudgeVerdict.Valid, row.JudgeVerdict.String)
		item.JudgeTodoCount = todo[row.Run.ID]
		item.IsRevising = revising[row.Run.ID]      // nil map ⇒ false (issue #750)
		item.CurrentActivity = activity[row.Run.ID] // nil map ⇒ null (PRD #1064)
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
	runIDs := make([]uuid.UUID, 0, len(rows))
	for _, row := range rows {
		runIDs = append(runIDs, row.Run.ID)
	}
	// Plan-revise display flag, same best-effort contract as ListRuns (issue #750): the
	// admin CLI renders status through effectiveRunStatus(..., IsRevising), so without this
	// a revising run would show as awaiting_approval here while the owner's own list says
	// "revising". Decoration only — a failure logs and leaves every flag false.
	revising, err := h.wsvc.PlanRevisingForRuns(r.Context(), runIDs)
	if err != nil {
		slog.Error("plan revising states", "error", err)
		revising = nil
	}
	// PRD #1064 M2: the "now" line for the non-terminal runs on this page. AdminListRuns
	// (ListActiveRunsAll) already excludes terminal runs, so every id here is active; the
	// IsTerminalRunStatus guard is kept for parity with ListRuns and to stay correct if
	// that query ever widens. Best-effort like the revising decoration above.
	activeIDs := make([]uuid.UUID, 0, len(rows))
	for _, row := range rows {
		if !apitypes.IsTerminalRunStatus(row.Run.Status) {
			activeIDs = append(activeIDs, row.Run.ID)
		}
	}
	activity, err := h.wsvc.CurrentActivityForRuns(r.Context(), activeIDs)
	if err != nil {
		slog.Error("current activity", "error", err)
		activity = nil
	}

	out := make([]apitypes.RunListItemDTO, 0, len(rows))
	for _, row := range rows {
		email := row.OwnerEmail
		item := apitypes.RunListItemDTO{
			RunDTO:     runToDTO(row.Run, row.PriorityClass),
			RepoPath:   row.RepoPath,
			WorkerName: textPtrValue(row.WorkerName.Valid, row.WorkerName.String),
			OwnerEmail: &email,
		}
		item.ForgeType = row.ForgeType // per-run MR/PR noun (PRD #65 D2)
		// PRD #411: the joined forge issue web URL, nil for issue-less/uncached runs.
		item.IssueWebURL = textPtrValue(row.IssueWebUrl.Valid, row.IssueWebUrl.String)
		item.IsRevising = revising[row.Run.ID]      // nil map ⇒ false (issue #750)
		item.CurrentActivity = activity[row.Run.ID] // nil map ⇒ null (PRD #1064)
		out = append(out, item)
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"runs": out})
}

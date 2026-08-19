package handler

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/slacksvc"
	"github.com/vtmocanu/uzi/api/internal/termsafe"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// The task-review surface (PRD #400 M4a). A review run (a task carrying
// review_target_run_id) posts its structured diff findings to POST
// /worker/runs/{id}/task-review, where {id} is the reviewed TARGET run. The read side,
// GET /runs/{id}/task-review, serves them to the owner-or-admin. Both are scoped in the
// service exactly like the judge's review surface.

// workerTaskReviewRequest is the review run's POST (PRD #400 M4a). Every free-text field is
// UNTRUSTED (a worker is a user-controlled container): the handler enum-validates,
// length-caps + control-strips, and secret-scrubs before it reaches the DB.
type workerTaskReviewRequest struct {
	Status   string                 `json:"status"`
	Summary  string                 `json:"summary"`
	Findings []workerTaskReviewFind `json:"findings"`
}

type workerTaskReviewFind struct {
	File      string `json:"file"`
	Symbol    string `json:"symbol"`
	Line      int32  `json:"line"`
	Severity  string `json:"severity"`
	Summary   string `json:"summary"`
	Rationale string `json:"rationale"`
}

// WorkerTaskReview persists a review run's structured diff findings for the reviewed task
// (PRD #400 M4a). Review-run-scoped authz + the atomic UPSERT (replace semantics) live in
// the service; here the request is validated and scrubbed at ingest. {id} is the TARGET
// run id (the reviewed task), not the review run.
func (h *Handler) WorkerTaskReview(w http.ResponseWriter, r *http.Request) {
	wkr, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	targetID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid run id")
		return
	}
	var req workerTaskReviewRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	sub, err := validateAndScrubTaskReview(req)
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.wsvc.PostTaskReview(r.Context(), wkr, targetID, sub); err != nil {
		if errors.Is(err, workersvc.ErrRunNotFound) {
			httpx.Error(w, http.StatusNotFound, "run not found")
			return
		}
		slog.Error("worker task review", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// validateAndScrubTaskReview is the task-review ingest gate (PRD #400 M4a, mirrors
// validateAndScrubReview): reject a bad status/severity enum, cap the free text and strip
// control chars (preserving markdown newlines in the multi-line fields via
// termsafe.SanitizeBounded), bound line to a sane non-negative int, cap the findings
// count, and scrub every free field through the secret-family redactor before it is
// persisted or ever rendered.
func validateAndScrubTaskReview(req workerTaskReviewRequest) (workersvc.TaskReviewSubmission, error) {
	status := req.Status
	if status == "" {
		status = "complete"
	}
	if !workersvc.TaskReviewStatuses[status] {
		return workersvc.TaskReviewSubmission{}, errors.New("status must be complete|failed")
	}
	if len(req.Findings) > workersvc.TaskReviewMaxFindings {
		return workersvc.TaskReviewSubmission{}, fmt.Errorf("at most %d findings", workersvc.TaskReviewMaxFindings)
	}
	sub := workersvc.TaskReviewSubmission{
		Status:    status,
		SummaryMd: slacksvc.ScrubSecrets(termsafe.SanitizeBounded(req.Summary, workersvc.TaskReviewSummaryMaxBytes)),
	}
	for _, f := range req.Findings {
		if !workersvc.TaskReviewSeverities[f.Severity] {
			return workersvc.TaskReviewSubmission{}, fmt.Errorf("invalid finding severity: %q", f.Severity)
		}
		sub.Findings = append(sub.Findings, workersvc.TaskReviewFinding{
			// file/symbol are single-line self-reported identifiers: control/Cf-strip + cap
			// via sanitizeSelfReported, then secret-scrub. summary/rationale keep markdown
			// newlines via termsafe.SanitizeBounded.
			File:        slacksvc.ScrubSecrets(sanitizeSelfReported(f.File, workersvc.TaskReviewFileMaxBytes)),
			Symbol:      slacksvc.ScrubSecrets(sanitizeSelfReported(f.Symbol, workersvc.TaskReviewSymbolMaxBytes)),
			Line:        boundTaskReviewLine(f.Line),
			Severity:    f.Severity,
			SummaryMd:   slacksvc.ScrubSecrets(termsafe.SanitizeBounded(f.Summary, workersvc.TaskReviewFindingSummaryMax)),
			RationaleMd: slacksvc.ScrubSecrets(termsafe.SanitizeBounded(f.Rationale, workersvc.TaskReviewRationaleMaxBytes)),
		})
	}
	return sub, nil
}

// boundTaskReviewLine clamps a worker-reported line number to [0, TaskReviewMaxLine]: a
// negative value (unanchored / malformed) collapses to 0, and a pathological large value is
// capped, so the int column never takes an absurd or negative number.
func boundTaskReviewLine(line int32) int32 {
	if line < 0 {
		return 0
	}
	if line > workersvc.TaskReviewMaxLine {
		return workersvc.TaskReviewMaxLine
	}
	return line
}

// GetTaskReview serves a handoff task's diff-review as JSON (PRD #400 M4a): the header +
// findings, or null when the task has no review yet. Visibility is owner-or-admin via
// GetTaskReviewPanel (GetRunForViewer-scoped): a run the caller can't see is 404; a
// visible-but-unreviewed run is 200 with task_review:null. {id} is the TARGET (task) run.
func (h *Handler) GetTaskReview(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid run id")
		return
	}
	res, err := h.wsvc.GetTaskReviewPanel(r.Context(), user.ID, user.IsAdmin, id)
	if err != nil {
		if errors.Is(err, workersvc.ErrRunNotFound) {
			httpx.Error(w, http.StatusNotFound, "run not found")
			return
		}
		slog.Error("get task review", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	body := map[string]any{"task_review": nil}
	if res != nil {
		body["task_review"] = taskReviewToDTO(*res)
	}
	httpx.JSON(w, http.StatusOK, body)
}

// taskReviewToDTO maps the service read struct to the wire DTO. review_run_id is nil when
// the review run row was deleted (SET NULL); the findings are already ordered
// most-severe-first by the query.
func taskReviewToDTO(rw workersvc.TaskReviewWithFindings) apitypes.TaskReviewDTO {
	findings := make([]apitypes.TaskReviewFindingDTO, 0, len(rw.Findings))
	for _, f := range rw.Findings {
		findings = append(findings, apitypes.TaskReviewFindingDTO{
			File:      f.File,
			Symbol:    f.Symbol,
			Line:      f.Line,
			Severity:  f.Severity,
			Summary:   f.SummaryMd,
			Rationale: f.RationaleMd,
		})
	}
	var reviewRunID *string
	if rw.Review.ReviewRunID.Valid {
		s := uuid.UUID(rw.Review.ReviewRunID.Bytes).String()
		reviewRunID = &s
	}
	return apitypes.TaskReviewDTO{
		TargetRunID: rw.Review.TargetRunID.String(),
		ReviewRunID: reviewRunID,
		Status:      rw.Review.Status,
		SummaryMd:   rw.Review.SummaryMd,
		Findings:    findings,
		CreatedAt:   rw.Review.CreatedAt.Time,
	}
}

package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"gitlab.example.com/vtmocanu/uzi/api/internal/httpx"
	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/slacksvc"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
	"gitlab.example.com/vtmocanu/uzi/api/internal/workersvc"
)

// The worker's judge surface (PRD #46 M3). A judge run fetches the run it reviews
// through GET /worker/runs/{id}/trace and posts its verdict to POST
// /worker/runs/{id}/review. Both are JUDGE-RUN-SCOPED, not user-scoped (Decision 3,
// audit H1): the worker must own the active judge run reviewing {id}, verified in the
// service, so a foreign {id} — or one the worker's judge run isn't reviewing — is 404.

// judgeTraceTargetDTO is the reviewed run's metadata: enough for the judge to reason
// about agents, plan, review cycles, and delivery (Decision 3). repo_agents is the
// run's agent-roster snapshot (raw jsonb).
type judgeTraceTargetDTO struct {
	ID               string          `json:"id"`
	Kind             string          `json:"kind"`
	Status           string          `json:"status"`
	IssueTitle       string          `json:"issue_title"`
	IssueDescription string          `json:"issue_description"`
	Branch           *string         `json:"branch"`
	MrIID            *int64          `json:"mr_iid"`
	FailureReason    *string         `json:"failure_reason"`
	FixVerdict       *string         `json:"fix_verdict"`
	PlanMd           *string         `json:"plan_md"`
	IterationCount   int32           `json:"iteration_count"`
	RepoAgents       json.RawMessage `json:"repo_agents"`
}

// judgeTraceInputDTO is one steering-log entry (a follow-up, plan verdict, or cancel).
type judgeTraceInputDTO struct {
	Kind      string    `json:"kind"`
	Body      *string   `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

// WorkerRunTrace returns one page of the reviewed run's trace for the caller's judge
// run (Decision 3): the target metadata + steering log + a bounded page of messages
// after ?after=<seq> (default 0), the page size capped server-side. Judge-run-scoped
// authz lives in the service; a foreign/unreviewed id is 404.
func (h *Handler) WorkerRunTrace(w http.ResponseWriter, r *http.Request) {
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
	after := int32(0)
	if raw := r.URL.Query().Get("after"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 32)
		if err != nil || n < 0 {
			httpx.Error(w, http.StatusBadRequest, "after must be a non-negative integer")
			return
		}
		after = int32(n)
	}
	limit := int32(0)
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 32)
		if err != nil || n < 0 {
			httpx.Error(w, http.StatusBadRequest, "limit must be a non-negative integer")
			return
		}
		limit = int32(n)
	}

	res, err := h.wsvc.JudgeTrace(r.Context(), wkr, targetID, after, limit)
	if err != nil {
		if errors.Is(err, workersvc.ErrRunNotFound) {
			httpx.Error(w, http.StatusNotFound, "run not found")
			return
		}
		slog.Error("worker run trace", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}

	msgs := make([]messageDTO, 0, len(res.Messages))
	for _, m := range res.Messages {
		msgs = append(msgs, messageToDTO(m))
	}
	inputs := make([]judgeTraceInputDTO, 0, len(res.Inputs))
	for _, in := range res.Inputs {
		inputs = append(inputs, judgeTraceInputDTO{
			Kind:      in.Kind,
			Body:      textPtrValue(in.Body.Valid, in.Body.String),
			CreatedAt: in.CreatedAt.Time,
		})
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"target":   traceTargetDTO(res.Target),
		"messages": msgs,
		"inputs":   inputs,
	})
}

func traceTargetDTO(run store.Run) judgeTraceTargetDTO {
	repoAgents := json.RawMessage(run.RepoAgents)
	if len(repoAgents) == 0 {
		repoAgents = json.RawMessage("null")
	}
	return judgeTraceTargetDTO{
		ID:               run.ID.String(),
		Kind:             run.Kind,
		Status:           run.Status,
		IssueTitle:       run.IssueTitle,
		IssueDescription: run.IssueDescription,
		Branch:           textPtrValue(run.Branch.Valid, run.Branch.String),
		MrIID:            int64Ptr(run.MrIid),
		FailureReason:    textPtrValue(run.FailureReason.Valid, run.FailureReason.String),
		FixVerdict:       textPtrValue(run.FixVerdict.Valid, run.FixVerdict.String),
		PlanMd:           textPtrValue(run.PlanMd.Valid, run.PlanMd.String),
		IterationCount:   run.IterationCount,
		RepoAgents:       repoAgents,
	}
}

// workerReviewRequest is the judge's review POST (Decision 5). Every free-text field
// is UNTRUSTED (a worker is a user-controlled container): the handler enum-validates,
// length-caps + control-strips, and secret-scrubs before it reaches the DB.
type workerReviewRequest struct {
	Verdict         string            `json:"verdict"`
	Summary         string            `json:"summary"`
	Model           string            `json:"model"`
	Status          string            `json:"status"`
	Recommendations []workerReviewRec `json:"recommendations"`
}

type workerReviewRec struct {
	Category   string `json:"category"`
	Target     string `json:"target"`
	Rationale  string `json:"rationale"`
	Confidence string `json:"confidence"`
}

// WorkerRunReview persists the judge's verdict + recommendations for the reviewed run
// (Decision 5). Judge-run-scoped authz + the atomic UPSERT (replace semantics) live in
// the service; here the request is validated and scrubbed at ingest.
func (h *Handler) WorkerRunReview(w http.ResponseWriter, r *http.Request) {
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
	var req workerReviewRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	sub, err := validateAndScrubReview(req)
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.wsvc.PostReview(r.Context(), wkr, targetID, sub); err != nil {
		if errors.Is(err, workersvc.ErrRunNotFound) {
			httpx.Error(w, http.StatusNotFound, "run not found")
			return
		}
		slog.Error("worker run review", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// validateAndScrubReview is the review ingest gate (Decision 5, audit C1/L4): reject a
// bad verdict/status/category/confidence enum, cap the free text and strip control
// chars (preserving markdown newlines in the multi-line fields), and scrub every free
// field through the secret-family redactor before it is persisted or ever rendered.
func validateAndScrubReview(req workerReviewRequest) (workersvc.ReviewSubmission, error) {
	if !workersvc.ReviewVerdicts[req.Verdict] {
		return workersvc.ReviewSubmission{}, errors.New("verdict must be one of ideal|ok|issues")
	}
	status := req.Status
	if status == "" {
		status = "complete"
	}
	if !workersvc.ReviewStatuses[status] {
		return workersvc.ReviewSubmission{}, errors.New("status must be complete|failed")
	}
	if len(req.Recommendations) > workersvc.ReviewMaxRecommendations {
		return workersvc.ReviewSubmission{}, fmt.Errorf("at most %d recommendations", workersvc.ReviewMaxRecommendations)
	}
	sub := workersvc.ReviewSubmission{
		Verdict:    req.Verdict,
		Status:     status,
		SummaryMd:  slacksvc.ScrubSecrets(sanitizeReviewText(req.Summary, workersvc.ReviewSummaryMaxBytes)),
		JudgeModel: slacksvc.ScrubSecrets(sanitizeSelfReported(req.Model, workersvc.ReviewModelMaxBytes)),
	}
	for _, rec := range req.Recommendations {
		if !workersvc.RecommendationCategories[rec.Category] {
			return workersvc.ReviewSubmission{}, fmt.Errorf("invalid recommendation category: %q", rec.Category)
		}
		if !workersvc.RecommendationConfidences[rec.Confidence] {
			return workersvc.ReviewSubmission{}, errors.New("confidence must be empty|low|medium|high")
		}
		sub.Recommendations = append(sub.Recommendations, workersvc.ReviewRecommendation{
			Category:    rec.Category,
			Target:      slacksvc.ScrubSecrets(sanitizeSelfReported(rec.Target, workersvc.ReviewTargetMaxBytes)),
			RationaleMd: slacksvc.ScrubSecrets(sanitizeReviewText(rec.Rationale, workersvc.ReviewRationaleMaxBytes)),
			Confidence:  rec.Confidence,
		})
	}
	return sub, nil
}

// sanitizeReviewText bounds an untrusted multi-line markdown field: trim, cap to max
// bytes, and drop control characters EXCEPT newline and tab (so the markdown structure
// survives while terminal escapes do not). The byte check runs after each whole rune
// is written, so it never splits a multi-byte rune.
func sanitizeReviewText(s string, max int) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range s {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			continue
		}
		b.WriteRune(r)
		if b.Len() >= max {
			break
		}
	}
	return b.String()
}

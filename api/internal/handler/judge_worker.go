package handler

import (
	"context"
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

	"gitlab.example.com/vtmocanu/uzi/api/internal/apitypes"
	"gitlab.example.com/vtmocanu/uzi/api/internal/httpx"
	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/notifysvc"
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

	msgs := make([]apitypes.MessageDTO, 0, len(res.Messages))
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
	res, err := h.wsvc.PostReview(r.Context(), wkr, targetID, sub)
	if err != nil {
		if errors.Is(err, workersvc.ErrRunNotFound) {
			httpx.Error(w, http.StatusNotFound, "run not found")
			return
		}
		slog.Error("worker run review", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	// The review is now durably persisted (persist-first). Surface it best-effort as a
	// "review ready" notification to the run's OWNER (never cross-user): an inbox row
	// plus an optional Slack DM. A notify failure never fails the worker POST — the
	// review is the source of truth and re-running is cheap.
	h.notifyReviewReady(r.Context(), targetID, res, sub)
	httpx.JSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// judgeReviewNotificationKind is the inbox notification kind the judge produces at
// review completion (PRD #46 M4). The inbox + Slack renderers key on the { title,
// body } payload convention; this kind lets a reader recognize a judge review row.
const judgeReviewNotificationKind = "judge_review"

// notifyReviewReady fires the "review ready" notification for a just-persisted review
// (PRD #46 M4). Best-effort and nil-safe: no notifier wired ⇒ no-op; a delivery error
// is logged, never returned (the worker POST already succeeded). The deep link's base
// is the operator-set public base URL (server-side, never LLM text); a lookup failure
// simply drops the link.
func (h *Handler) notifyReviewReady(ctx context.Context, targetID uuid.UUID, res workersvc.ReviewResult, sub workersvc.ReviewSubmission) {
	if h.notifier == nil {
		return
	}
	base := ""
	if h.settings != nil {
		if b, err := h.settings.PublicBaseURL(ctx); err == nil {
			base = b
		}
	}
	if _, err := h.notifier.Notify(ctx, buildReviewNotification(base, targetID, res, sub)); err != nil {
		slog.Error("notify judge review ready", "error", err)
	}
}

// buildReviewNotification assembles the "review ready" notification (PRD #46 M4,
// Decision 6). It is PURE (no I/O) so the security-critical shape is unit-testable.
// The inbox payload and the Slack body carry the verdict, the SCRUBBED summary, and
// the recommendation count + categories (the full recommendation detail lives on the
// run page behind the deep link). The summary is untrusted judge/worker text: it was
// already validated + capped + secret-scrubbed at the review POST, and the producer
// re-scrubs + length-caps it here (belt and suspenders — the notifysvc payload path
// is stored/served VERBATIM, incl. the admin all-view). Recommendation TARGET and
// RATIONALE free text are NOT copied here — only the closed category enum is. The deep
// link is server-built from the operator-set base URL + the target run UUID (never any
// LLM-supplied text); an empty/unset base yields no link. The notification goes only
// to the reviewed run's owner (res.OwnerID), anchored to both the run and review.
func buildReviewNotification(baseURL string, targetID uuid.UUID, res workersvc.ReviewResult, sub workersvc.ReviewSubmission) notifysvc.Notification {
	summary := reviewSummaryPreview(sub.SummaryMd)
	body := reviewNotificationBody(sub.Verdict, len(sub.Recommendations), summary)
	runID, reviewID := targetID, res.ReviewID
	return notifysvc.Notification{
		UserID: res.OwnerID,
		Kind:   judgeReviewNotificationKind,
		Payload: map[string]any{
			"title":                     "Run review ready",
			"body":                      body,
			"verdict":                   sub.Verdict,
			"status":                    sub.Status,
			"summary":                   summary,
			"recommendation_count":      len(sub.Recommendations),
			"recommendation_categories": recommendationCategories(sub.Recommendations),
		},
		RunID:    &runID,
		ReviewID: &reviewID,
		Slack: &notifysvc.SlackRender{
			Title: "Run review ready",
			Body:  body,
			Link:  reviewDeepLink(baseURL, targetID),
		},
	}
}

// reviewNotificationBody renders the one-line summary shown in the inbox row and the
// Slack DM: the verdict, how many recommendations came with it, and (when present) the
// scrubbed summary preview.
func reviewNotificationBody(verdict string, recCount int, summary string) string {
	line := "verdict: " + verdict
	if recCount == 1 {
		line += " — 1 recommendation"
	} else {
		line += fmt.Sprintf(" — %d recommendations", recCount)
	}
	if summary != "" {
		line += ": " + summary
	}
	return line
}

// reviewSummaryPreviewMaxRunes caps the summary preview carried in the notification —
// tight for a one-line inbox/DM row; the run page holds the full text.
const reviewSummaryPreviewMaxRunes = 280

// reviewSummaryPreview renders the judge's summary for the notification: whitespace
// (incl. newlines) collapsed to a single space, secret-scrubbed (belt and suspenders —
// it is already scrubbed at ingest, but the producer contract re-scrubs every free
// field copied onto the verbatim payload path), then rune-capped. Scrub before the cap
// so a redaction marker can't be split.
func reviewSummaryPreview(summaryMd string) string {
	oneLine := slacksvc.ScrubSecrets(strings.Join(strings.Fields(summaryMd), " "))
	if oneLine == "" {
		return ""
	}
	runes := []rune(oneLine)
	if len(runes) > reviewSummaryPreviewMaxRunes {
		return string(runes[:reviewSummaryPreviewMaxRunes]) + "…"
	}
	return oneLine
}

// recommendationCategories is the list of recommendation category enums (closed set,
// safe to surface raw) carried in the notification payload — count + categories, not
// the free-text target/rationale (those stay on the run page).
func recommendationCategories(recs []workersvc.ReviewRecommendation) []string {
	cats := make([]string, 0, len(recs))
	for _, r := range recs {
		cats = append(cats, r.Category)
	}
	return cats
}

// reviewDeepLink builds the run-page deep link from the operator-set public base URL
// and the target run UUID. Both are server-controlled; no LLM text is ever
// interpolated. An empty base (unset, or the settings lookup failed) yields "" so the
// notification simply carries no link.
func reviewDeepLink(baseURL string, targetID uuid.UUID) string {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return ""
	}
	return baseURL + "/runs/" + targetID.String()
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

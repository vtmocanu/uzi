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

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/notifysvc"
	"github.com/vtmocanu/uzi/api/internal/slacksvc"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/termsafe"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
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
	// PRD #634 M4: the terminal stop disposition (server-computed, a closed CHECK enum),
	// null on a normal run. Carries 'scope_capped' so the judge does not score an
	// operator-directed partial as an incomplete or defective implementation.
	StopKind *string `json:"stop_kind"`
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
	targetID, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
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
		StopKind:         textPtrValue(run.StopKind.Valid, run.StopKind.String),
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
	targetID, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
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
// the recommendation count + categories (the full recommendation detail lives on the Judge
// workbench behind the deep link — `/judge?run={target}` since PRD #98 M5, not the run
// page). The summary is untrusted judge/worker text: it was
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
		// PRD #268 M3: the Slack DM is Block Kit (family D). The Facts are TRUSTED,
		// closed-enum strings carrying intentional markup — the verdict glyph + bold
		// verdict word, the recommendation count, and one `code` chip per DISTINCT
		// recommendation category (the closed workersvc.RecommendationCategories enum,
		// safe raw). The notifier scrubs but does NOT escape them. Body is the scrubbed
		// summary preview alone (the untrusted model text, whole-blob escaped in the
		// blockquote), NOT the verdict/count one-liner — those now ride the Facts.
		Slack: &notifysvc.SlackRender{
			Emoji: "🔎",
			Title: "Run review ready",
			Facts: reviewSlackFacts(sub),
			Body:  summary,
			Link:  reviewDeepLink(baseURL, targetID),
		},
	}
}

// reviewSlackFacts builds the trusted Fact chips for the judge review DM (PRD #268 M3):
// the verdict (glyph + bold word), the recommendation count, then one `code` chip per
// DISTINCT recommendation category, capped at the enum size so a pathological review
// cannot flood the context block. Every part is drawn from the closed verdict/category
// enums or an int, so the markup is intentional and safe to leave un-escaped.
func reviewSlackFacts(sub workersvc.ReviewSubmission) []string {
	facts := []string{
		"Verdict " + verdictGlyph(sub.Verdict) + " *" + sub.Verdict + "*",
		recCountFact(len(sub.Recommendations)),
	}
	return append(facts, recommendationCategoryChips(sub.Recommendations)...)
}

// verdictGlyph maps a review verdict to its canonical DM glyph, "" for an unknown value
// (byte-honest degrade — the fact then reads "Verdict *x*" with no glyph). The live
// enum (workersvc.ReviewVerdicts) is ideal|ok|issues; the needs_changes/needs-changes
// and bad keys are carried per the PRD #268 M3 spec so a later verdict rename lands
// glyph-ready.
func verdictGlyph(verdict string) string {
	switch verdict {
	case "ideal", "ok":
		return "✅"
	case "issues", "needs_changes", "needs-changes":
		return "⚠️"
	case "bad":
		return "❌"
	default:
		return ""
	}
}

// recCountFact is the singular/plural recommendation-count Fact.
func recCountFact(n int) string {
	if n == 1 {
		return "1 recommendation"
	}
	return fmt.Sprintf("%d recommendations", n)
}

// recommendationCategoryChips renders one `code` chip per DISTINCT recommendation
// category, in first-seen order, capped at the RecommendationCategories enum size so
// the context block stays bounded even for a many-recommendation review. The category
// is the closed enum (validated at ingest), so it is safe to surface raw as a chip.
func recommendationCategoryChips(recs []workersvc.ReviewRecommendation) []string {
	const maxChips = 7 // len(workersvc.RecommendationCategories)
	seen := make(map[string]struct{}, maxChips)
	chips := make([]string, 0, maxChips)
	for _, r := range recs {
		if _, dup := seen[r.Category]; dup {
			continue
		}
		seen[r.Category] = struct{}{}
		chips = append(chips, "`"+r.Category+"`")
		if len(chips) >= maxChips {
			break
		}
	}
	return chips
}

// reviewNotificationBody renders the one-line summary shown in the web inbox row
// (Payload["body"]): the verdict, how many recommendations came with it, and (when
// present) the scrubbed summary preview. The summary preview is now MULTI-LINE (PRD #292
// M4 preserves its newlines for the Slack blockquote), so its whitespace/newlines are
// collapsed to single spaces HERE, at the append, to keep the inbox row a one-liner
// (Decision 7 / SC4). The web inbox reads only payload.body — never payload.summary — so
// the multi-line summary never reaches it.
func reviewNotificationBody(verdict string, recCount int, summary string) string {
	line := "verdict: " + verdict
	if recCount == 1 {
		line += " — 1 recommendation"
	} else {
		line += fmt.Sprintf(" — %d recommendations", recCount)
	}
	if summary != "" {
		line += ": " + strings.Join(strings.Fields(summary), " ")
	}
	return line
}

// reviewSummaryPreviewMaxRunes caps the summary preview carried in the notification.
// PRD #268 M3 raised it from 280 to 600: the fuller excerpt is for the Slack DM's
// blockquote (the Body of the Block Kit render), which has room for it; the inbox
// one-liner (Payload.body via reviewNotificationBody) is still capped, just to the same
// longer preview. The run page still holds the full text.
const reviewSummaryPreviewMaxRunes = 600

// reviewSummaryPreview renders the judge's summary for the notification: trimmed but
// with its NEWLINES PRESERVED (PRD #292 M4), secret-scrubbed (belt and suspenders — it
// is already scrubbed at ingest, but the producer contract re-scrubs every free field
// copied onto the verbatim payload path), then rune-capped. Newlines are kept so the
// Slack blockquote/list structure survives when the body is rendered as mrkdwn (the
// output feeds SlackRender.Body); the web inbox one-liner collapses them itself in
// reviewNotificationBody (Decision 7). Scrub the FULL text before the cap so no secret
// byte can survive the cut regardless of where it lands — the cap may split a redaction
// marker, but a split marker leaks nothing; only unscrubbed secret bytes would.
func reviewSummaryPreview(summaryMd string) string {
	text := slacksvc.ScrubSecrets(strings.TrimSpace(summaryMd))
	if text == "" {
		return ""
	}
	runes := []rune(text)
	if len(runes) > reviewSummaryPreviewMaxRunes {
		return string(runes[:reviewSummaryPreviewMaxRunes]) + "…"
	}
	return text
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

// reviewDeepLink builds the Slack DM's deep link from the operator-set public base URL
// and the target run UUID. Both are server-controlled; no LLM text is ever
// interpolated. An empty base (unset, or the settings lookup failed) yields "" so the
// notification simply carries no link.
//
// It points at the Judge workbench anchored to this run (PRD #98 M5, Decision 4), not at
// the run page. The DM's CADENCE is deliberately unchanged — one per review, un-batched,
// no Slack digest (Decision 5, user-decided); only the destination moves.
//
// This function is judge-only by construction — it is called from exactly one place, the
// judge review notification — which is why it is a plain URL change here and a
// kind-conditional guard in the web inbox (see web/src/lib/notifications.ts, where the
// same link is computed for a surface that renders EVERY kind).
func reviewDeepLink(baseURL string, targetID uuid.UUID) string {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return ""
	}
	return baseURL + "/judge?run=" + targetID.String()
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
		SummaryMd:  slacksvc.ScrubSecrets(termsafe.SanitizeBounded(req.Summary, workersvc.ReviewSummaryMaxBytes)),
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
			Category: rec.Category,
			// canonicalizeTarget runs LAST, AFTER control/Cf stripping (sanitizeSelfReported)
			// and secret scrubbing (ScrubSecrets), so those still see the raw bytes and this
			// only folds the already-clean result's cosmetic casing/whitespace/punctuation
			// drift (issue #232). Re-bounded to the same ReviewTargetMaxBytes — the ASCII-only
			// fold never grows a string (it only lowercases 1:1 and shortens runs/edges), so
			// the cap is a formality here, but keeping it makes the byte bound hold no matter
			// which order a future edit reshuffles these into.
			Target:      canonicalizeTarget(slacksvc.ScrubSecrets(sanitizeSelfReported(rec.Target, workersvc.ReviewTargetMaxBytes)), workersvc.ReviewTargetMaxBytes),
			RationaleMd: slacksvc.ScrubSecrets(termsafe.SanitizeBounded(rec.Rationale, workersvc.ReviewRationaleMaxBytes)),
			Confidence:  rec.Confidence,
		})
	}
	return sub, nil
}

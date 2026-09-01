package handler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

type setJudgeRequest struct {
	Enabled bool `json:"enabled"`
	// AnthropicToken is the LABEL of the credential this user's retrospectives should
	// spend (PRD #104 M4, D1). Three-way — omitted leaves the binding alone, null or
	// "" clears it back to the user's default, a label binds it — which is why it is
	// a json.RawMessage and not a *string: a nil *string cannot tell "clear" from
	// "don't touch", and every pre-M4 client sends {"enabled":true} with no token
	// key at all. Collapsing those two would make enabling the judge silently unbind
	// the user's judge credential. See parseTokenField.
	//
	// This EXTENDS the existing enabled-only body. PRD #69 has since landed a per-user
	// judge_model, but it does NOT live here: it is written through PUT /me/settings
	// (userSettingsDTO, PRD #69 M2), alongside the user's default_model, not on this
	// judge opt-in body. The instance-wide judge_model (settings.KeyJudgeModel) remains
	// the admin fallback the per-user value overrides.
	AnthropicToken json.RawMessage `json:"anthropic_token"`
}

// SetJudgeEnabled flips the CURRENT user's run-judge opt-in (PRD #46 Decision 7).
// Enabling it lets every one of this user's finished runs be reviewed by an LLM on
// THEIR own Anthropic token, so — exactly like the autopilot opt-in — the target is
// taken from the session, NEVER the body (audit H3): this WRITE path never lets one
// user opt another into spending their tokens. Returns the updated user.
//
// The instance-level exception is enforced mode (PRD #69): when an admin turns on
// judge_enforce_all, every user's OWN finished runs are judged on each user's OWN
// token without their per-user opt-in. That is the deliberate, documented tradeoff of
// enforced mode — the admin still cannot redirect the spend onto anyone ELSE's token
// (the judge always bills the run owner), so audit H3's core property (nobody spends a
// DIFFERENT user's tokens) holds; only the self-opt-in gate is what enforced mode
// bypasses. This write path is untouched by that: it still stamps the session user.
func (h *Handler) SetJudgeEnabled(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var req setJudgeRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	updated, err := h.q.SetUserJudgeEnabled(r.Context(), store.SetUserJudgeEnabledParams{
		ID:           user.ID,
		JudgeEnabled: req.Enabled,
	})
	if err != nil {
		slog.Error("set judge enabled", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}

	// The token binding is a SEPARATE statement, and only runs when the field was
	// present: an absent anthropic_token must leave an existing binding alone, or
	// every existing client that PUTs {"enabled":true} would silently unbind the
	// user's judge credential. The two writes are not transactional, and deliberately
	// not: they are independent settings, and the worst a half-applied pair does is
	// leave the opt-in flipped without the rebind, which the user can see and redo.
	token, ok := parseTokenField(req.AnthropicToken)
	if !ok {
		httpx.Error(w, http.StatusBadRequest, "anthropic_token must be a token label, null, or omitted")
		return
	}
	if token.present {
		var secretID *uuid.UUID
		if l := token.label; l != "" {
			resolved, rerr := h.wsvc.ResolveTokenLabel(r.Context(), user.ID, l)
			if rerr != nil {
				if errors.Is(rerr, workersvc.ErrUnknownSecretLabel) {
					httpx.Error(w, http.StatusBadRequest, "no Anthropic token with that label")
					return
				}
				slog.Error("resolve judge token label", "error", rerr)
				httpx.Error(w, http.StatusInternalServerError, "internal error")
				return
			}
			secretID = &resolved
		}
		bound, berr := h.wsvc.SetUserJudgeToken(r.Context(), user.ID, secretID)
		if berr != nil {
			if errors.Is(berr, workersvc.ErrSecretNotOwned) {
				// 404, not 403: a 403 would confirm the id names a real credential
				// belonging to someone else.
				httpx.Error(w, http.StatusNotFound, "anthropic token not found")
				return
			}
			slog.Error("set judge anthropic token", "error", berr)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
		updated = bound
	}

	dto := toDTO(updated)
	if token.label != "" {
		l := token.label
		dto.JudgeAnthropicSecretLabel = &l
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"user": dto})
}

// SetUserJudgeEnabled is the admin per-user toggle (PRD #46 Decision 7): it
// force-toggles ANY user's run-judge opt-in — the "force-disable per user" control.
// The actor is authorized by RequireAdmin (the /admin group gate); the TARGET is
// taken from the path, never the body, and is a distinct user id from the actor's.
// It sets the flag on the target user's OWN account, so the judge still only ever
// spends that user's tokens — an admin cannot redirect the spend elsewhere.
//
// It shares setJudgeRequest with the self-service route but deliberately IGNORES
// anthropic_token (PRD #104 M4): "an admin cannot redirect the spend elsewhere" is
// the property above, and honoring a binding here would let an admin choose WHICH of
// a user's credentials burns — a narrower version of the same thing. An admin who
// needs that asks the user. The field being silently ignored is safe precisely
// because the only reachable effect would be the one we are refusing.
func (h *Handler) SetUserJudgeEnabled(w http.ResponseWriter, r *http.Request) {
	id, ok := httpx.PathUUID(w, r, "id", "user")
	if !ok {
		return
	}
	var req setJudgeRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	updated, err := h.q.SetUserJudgeEnabled(r.Context(), store.SetUserJudgeEnabledParams{
		ID:           id,
		JudgeEnabled: req.Enabled,
	})
	if err != nil {
		// A no-op UPDATE (unknown id) returns no row → 404; anything else is a real
		// DB failure → 500 (don't mask it as "user not found").
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Error(w, http.StatusNotFound, "user not found")
			return
		}
		slog.Error("admin set judge enabled", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"user": toDTO(updated)})
}

// reviewDTO (apitypes.ReviewDTO) and recommendationDTO (apitypes.RecommendationDTO)
// moved to the stdlib-only apitypes leaf (PRD #64 M1); reviewToDTO stays here.
func reviewToDTO(rw workersvc.ReviewWithRecommendations) apitypes.ReviewDTO {
	recs := make([]apitypes.RecommendationDTO, 0, len(rw.Recommendations))
	for _, rc := range rw.Recommendations {
		recs = append(recs, apitypes.RecommendationDTO{
			ID:          rc.ID.String(),
			Category:    rc.Category,
			Target:      rc.Target,
			RationaleMd: rc.RationaleMd,
			Confidence:  rc.Confidence,
			CreatedAt:   rc.CreatedAt.Time,
		})
	}
	// coord keys both side-tables to a recommendation (the stable (category, target)
	// coordinate PRD #68 introduced). The target='' collapse means several recs can share a
	// coordinate — accepted, identical to the filed case.
	type coord struct{ category, target string }

	// Only SETTLED links (filed_at valid) surface to the panel; an in-flight claim is
	// transient. The panel matches them to recommendations by (category, target).
	filed := make([]apitypes.FiledIssueDTO, 0, len(rw.FiledIssues))
	filedByCoord := make(map[coord]bool, len(rw.FiledIssues))
	for _, f := range rw.FiledIssues {
		if !f.FiledAt.Valid {
			continue
		}
		filedByCoord[coord{f.Category, f.Target}] = true
		filed = append(filed, apitypes.FiledIssueDTO{
			Category: f.Category,
			Target:   f.Target,
			IssueIID: f.FiledIssueIid.Int64,
			IssueURL: f.FiledIssueUrl,
			FiledAt:  f.FiledAt.Time,
		})
	}

	// dispByCoord indexes the review's dispositions by coordinate (PRD #94). A disposition
	// with no matching CURRENT recommendation is an orphan (re-judged away) — inert: it is
	// NOT emitted and does NOT count (both keyed off the current recommendation rows).
	dispByCoord := make(map[coord]store.RecommendationDisposition, len(rw.Dispositions))
	for _, d := range rw.Dispositions {
		dispByCoord[coord{d.Category, d.Target}] = d
	}

	// Emit one DispositionDTO per (category, target) that has BOTH a current recommendation
	// and a disposition, deduping the target='' collapse via an emitted set; stale is the
	// server-side rationale-hash compare against the first current rec on the coordinate.
	// Triage buckets one row per current recommendation through the shared ladder, so the
	// bar matches the on-screen rows and the global strip (filed = settled only).
	dispositions := make([]apitypes.DispositionDTO, 0, len(rw.Dispositions))
	emitted := make(map[coord]bool, len(rw.Dispositions))
	triageRows := make([]workersvc.TriageRow, 0, len(rw.Recommendations))
	for _, rc := range rw.Recommendations {
		c := coord{rc.Category, rc.Target}
		d, disposed := dispByCoord[c]
		triageRows = append(triageRows, workersvc.TriageRow{
			Status:       dispStatus(disposed, d),
			Reason:       dispReason(disposed, d),
			FiledSettled: filedByCoord[c],
		})
		if disposed && !emitted[c] {
			emitted[c] = true
			dispositions = append(dispositions, apitypes.DispositionDTO{
				Category: d.Category,
				Target:   d.Target,
				Status:   d.Status,
				Reason:   d.DismissReason.String, // "" when not dismissed
				SetAt:    d.SetAt.Time,
				Stale:    workersvc.RationaleHash(rc.RationaleMd) != d.RationaleHash,
			})
		}
	}

	return apitypes.ReviewDTO{
		ID:              rw.Review.ID.String(),
		TargetRunID:     rw.Review.TargetRunID.String(),
		Verdict:         rw.Review.Verdict,
		SummaryMd:       rw.Review.SummaryMd,
		JudgeModel:      rw.Review.JudgeModel,
		Status:          rw.Review.Status,
		CreatedAt:       rw.Review.CreatedAt.Time,
		UpdatedAt:       rw.Review.UpdatedAt.Time,
		Recommendations: recs,
		FiledIssues:     filed,
		Dispositions:    dispositions,
		Triage:          workersvc.BucketTriage(triageRows),
		JudgeRun:        judgeRunToDTO(rw.JudgeRun),
	}
}

// judgeRunToDTO renders the judge run's timing + usage for the review panel (PRD #69
// M6). nil in → nil out (no judge-run detail). Usage is attached ONLY when the judge
// posted a result frame (its run_usage row exists, so cost_usd is non-null) — a
// pre-feature judge has valid timings but NULL usage, which stays nil so the panel
// renders no cost/time strip rather than a fabricated 0.
func judgeRunToDTO(jr *store.GetJudgeRunUsageForTargetRow) *apitypes.JudgeRunDTO {
	if jr == nil {
		return nil
	}
	dto := &apitypes.JudgeRunDTO{
		JudgeRunID: uuid.UUID(jr.JudgeRunID.Bytes).String(),
		ClaimedAt:  timePtr(jr.ClaimedAt.Valid, jr.ClaimedAt.Time),
		StartedAt:  timePtr(jr.StartedAt.Valid, jr.StartedAt.Time),
		FinishedAt: timePtr(jr.FinishedAt.Valid, jr.FinishedAt.Time),
	}
	if jr.CostUsd.Valid {
		dto.Usage = &apitypes.UsageDTO{
			InputTokens:         jr.InputTokens.Int64,
			CacheReadTokens:     jr.CacheReadTokens.Int64,
			CacheCreationTokens: jr.CacheCreationTokens.Int64,
			OutputTokens:        jr.OutputTokens.Int64,
			CostUSD:             numericToFloat(jr.CostUsd),
		}
	}
	return dto
}

// pendingJudgeState normalizes a judge run's RAW runs.status into the two-value display
// union the clients consume ("scheduled" | "running"), for PRD #119's pending-judge
// signal. queued — enqueued, not yet claimed — is "scheduled"; EVERYTHING else is
// "running".
//
// The else is the point of this function, and it must never become an enumerated switch
// over queued/claimed/running. The rows that reach here come from
// GetActiveJudgeRunForTarget, whose predicate carries the
// uq_runs_one_active_judge_per_target index's active set: status NOT IN
// ('completed','failed','cancelled'). That set is defined by SUBTRACTION, so it contains
// every status runs.status legally admits minus three. Against the LIVE constraint
// (runs_status_check, last rewritten by 00146, ten values) the whole set is:
//
//	queued, claimed, running, awaiting_approval, awaiting_input, limit_wait, awaiting_followup
//
// — and it will silently include any status a future migration adds. ALL SEVEN are
// enumerated, because the subtraction is the whole argument and naming some members
// while dropping others makes it read as a guess:
//   - queued, claimed, running: where a judge run actually lives.
//   - awaiting_approval (00020): out of reach today — the judge runner has no approval
//     flow and auto_approve is autopilot-only.
//   - awaiting_input (00092): no server-side kind guard at all — SetRunAwaitingInput is
//     `WHERE id AND worker_id AND status NOT IN (terminal)`, so it is reachable the
//     moment any runner asks a question under a judge run.
//   - limit_wait (00091): out of reach behind TWO independent guards — CreateJudgeRun
//     never stamps wait_on_limit, leaving the column's DEFAULT false, and SetRunLimitWait
//     carries `AND kind <> 'judge'` (PRD #35 Decision 14). That statement is the only
//     writer of the status.
//   - awaiting_followup (00146): PRD #517's interactive-task park; out of reach for a
//     judge run (its writer guards `kind = 'task' AND interactive`), but the schema
//     permits the status on any row, so the query can still return it.
//
// Out of reach is not impossible: the schema permits every one of these on a judge row
// (runs_status_check does not condition on kind), so the query CAN return them, and a
// switch that fell through to "" would ship state:"" and break the web union
// "scheduled" | "running" — a blank chip in the one place the panel exists to
// explain. Defaulting to "running" degrades an unknown active status to "a
// judge is working on it", which is true of every member of that set by construction.
//
// TestPendingJudgeState asserts totality, including for a status this code has never
// seen.
func pendingJudgeState(status string) string {
	if status == "queued" {
		return "scheduled"
	}
	return "running"
}

// pendingJudgeToDTO renders the service's PendingJudge (raw status + enqueue time) as the
// wire DTO, applying the normalization above. The service deliberately reports the raw
// runs.status and this boundary owns the display vocabulary.
func pendingJudgeToDTO(p workersvc.PendingJudge) apitypes.PendingJudgeDTO {
	return apitypes.PendingJudgeDTO{
		State:      pendingJudgeState(p.Status),
		EnqueuedAt: p.EnqueuedAt,
	}
}

// dispStatus / dispReason read a disposition's fields for the triage bucketer, yielding the
// "" undisposed sentinels when the coordinate carries no disposition.
func dispStatus(disposed bool, d store.RecommendationDisposition) string {
	if !disposed {
		return ""
	}
	return d.Status
}

func dispReason(disposed bool, d store.RecommendationDisposition) string {
	if !disposed {
		return ""
	}
	return d.DismissReason.String
}

// GetRunReview serves the run-page review panel: the judge's verdict + recommendations,
// AND the active judge run for the target (PRD #46 M4, PRD #119 M1). Visibility is
// owner-or-admin via GetRunReviewPanel (GetRunForViewer-scoped, applied once before
// either read): a run the caller can't see is 404, and no pending-judge query is issued
// for it.
//
// The response is {"review": …|null, "pending_judge": …|null} with BOTH keys always
// present and either one nullable. They are independent: an unjudged run with an
// auto-judge in flight is review:null + pending_judge set, which is precisely the state
// the panel could not previously distinguish from "never judged" — it showed "not judged
// yet" next to a live button whose only possible outcome was a 409 from the
// one-active-judge-per-target index. A visible-but-unjudged run is still 200, never an
// error.
func (h *Handler) GetRunReview(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	id, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	res, pending, err := h.wsvc.GetRunReviewPanel(r.Context(), user.ID, user.IsAdmin, id)
	if err != nil {
		if errors.Is(err, workersvc.ErrRunNotFound) {
			httpx.Error(w, http.StatusNotFound, "run not found")
			return
		}
		slog.Error("get run review", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	// Both keys on EVERY success path. The nil branches emit an explicit null rather than
	// omitting the key, so a client can always read the pair — an absent key and a null
	// one are different claims, and "there is no judge coming" is a claim this endpoint
	// makes, not something a client should infer from silence.
	body := map[string]any{"review": nil, "pending_judge": nil}
	if res != nil {
		body["review"] = reviewToDTO(*res)
	}
	if pending != nil {
		body["pending_judge"] = pendingJudgeToDTO(*pending)
	}
	httpx.JSON(w, http.StatusOK, body)
}

// RerunJudge enqueues a fresh judge run for a terminal run at the owner's request
// (PRD #46 Decision 8). Owner-only (audit H3 — spends the owner's token), behind the
// per-user spend limiter. Maps the service's typed gate errors to specific statuses.
func (h *Handler) RerunJudge(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	id, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	judge, err := h.wsvc.RerunJudge(r.Context(), user.ID, user.IsAdmin, id)
	if err != nil {
		switch {
		case errors.Is(err, workersvc.ErrRunNotFound):
			httpx.Error(w, http.StatusNotFound, "run not found")
		case errors.Is(err, workersvc.ErrNotRunOwner):
			httpx.Error(w, http.StatusForbidden, "only the run owner can run the judge")
		case errors.Is(err, workersvc.ErrRunNotJudgeable):
			httpx.Error(w, http.StatusUnprocessableEntity, "this run cannot be judged")
		case errors.Is(err, workersvc.ErrJudgeDisabled):
			httpx.Error(w, http.StatusConflict, "run judging is disabled")
		case errors.Is(err, workersvc.ErrNoAnthropicToken):
			httpx.Error(w, http.StatusUnprocessableEntity, "add an Anthropic token before running the judge")
		case errors.Is(err, workersvc.ErrJudgeAlreadyActive):
			httpx.Error(w, http.StatusConflict, "a judge run is already in progress for this run")
		default:
			slog.Error("rerun judge", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	httpx.JSON(w, http.StatusAccepted, map[string]any{"run": runToDTO(judge, h.runPriorityClass(r.Context(), judge))})
}

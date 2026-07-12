package handler

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"gitlab.example.com/vtmocanu/uzi/api/internal/httpx"
	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
	"gitlab.example.com/vtmocanu/uzi/api/internal/workersvc"
)

type setJudgeRequest struct {
	Enabled bool `json:"enabled"`
}

// SetJudgeEnabled flips the CURRENT user's run-judge opt-in (PRD #46 Decision 7).
// Enabling it lets every one of this user's finished runs be reviewed by an LLM on
// THEIR own Anthropic token, so — exactly like the autopilot opt-in — the target is
// taken from the session, NEVER the body (audit H3): nobody can opt another user
// into spending their tokens. Returns the updated user.
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
	httpx.JSON(w, http.StatusOK, map[string]any{"user": toDTO(updated)})
}

// SetUserJudgeEnabled is the admin per-user toggle (PRD #46 Decision 7): it
// force-toggles ANY user's run-judge opt-in — the "force-disable per user" control.
// The actor is authorized by RequireAdmin (the /admin group gate); the TARGET is
// taken from the path, never the body, and is a distinct user id from the actor's.
// It sets the flag on the target user's OWN account, so the judge still only ever
// spends that user's tokens — an admin cannot redirect the spend elsewhere.
func (h *Handler) SetUserJudgeEnabled(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid user id")
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

// recommendationDTO is one structured judge recommendation for the run-page panel
// (PRD #46 M4). category is the taxonomy enum; target/rationale are the scrubbed
// free-text fields (validated + capped + secret-scrubbed at the review POST), which
// the SPA renders as escaped text (never markdown/HTML).
type recommendationDTO struct {
	ID          string    `json:"id"`
	Category    string    `json:"category"`
	Target      string    `json:"target"`
	RationaleMd string    `json:"rationale_md"`
	Confidence  string    `json:"confidence"`
	CreatedAt   time.Time `json:"created_at"`
}

// reviewDTO is the run's judge verdict + recommendations for the run page. summary_md
// and each rationale_md were scrubbed at ingest; the SPA renders them as escaped text.
type reviewDTO struct {
	ID              string              `json:"id"`
	TargetRunID     string              `json:"target_run_id"`
	Verdict         string              `json:"verdict"`
	SummaryMd       string              `json:"summary_md"`
	JudgeModel      string              `json:"judge_model"`
	Status          string              `json:"status"`
	CreatedAt       time.Time           `json:"created_at"`
	UpdatedAt       time.Time           `json:"updated_at"`
	Recommendations []recommendationDTO `json:"recommendations"`
}

func reviewToDTO(rw workersvc.ReviewWithRecommendations) reviewDTO {
	recs := make([]recommendationDTO, 0, len(rw.Recommendations))
	for _, rc := range rw.Recommendations {
		recs = append(recs, recommendationDTO{
			ID:          rc.ID.String(),
			Category:    rc.Category,
			Target:      rc.Target,
			RationaleMd: rc.RationaleMd,
			Confidence:  rc.Confidence,
			CreatedAt:   rc.CreatedAt.Time,
		})
	}
	return reviewDTO{
		ID:              rw.Review.ID.String(),
		TargetRunID:     rw.Review.TargetRunID.String(),
		Verdict:         rw.Review.Verdict,
		SummaryMd:       rw.Review.SummaryMd,
		JudgeModel:      rw.Review.JudgeModel,
		Status:          rw.Review.Status,
		CreatedAt:       rw.Review.CreatedAt.Time,
		UpdatedAt:       rw.Review.UpdatedAt.Time,
		Recommendations: recs,
	}
}

// GetRunReview serves the judge's verdict + recommendations for a run, for the
// run-page panel (PRD #46 M4). Visibility is owner-or-admin via GetReviewForTarget
// (GetRunForViewer-scoped): a run the caller can't see is 404. A visible but unjudged
// run returns 200 with review:null so the panel can render "not judged yet" without
// treating it as an error.
func (h *Handler) GetRunReview(w http.ResponseWriter, r *http.Request) {
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
	res, err := h.wsvc.GetReviewForTarget(r.Context(), user.ID, user.IsAdmin, id)
	if err != nil {
		if errors.Is(err, workersvc.ErrRunNotFound) {
			httpx.Error(w, http.StatusNotFound, "run not found")
			return
		}
		slog.Error("get run review", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if res == nil {
		httpx.JSON(w, http.StatusOK, map[string]any{"review": nil})
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"review": reviewToDTO(*res)})
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
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid run id")
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
	httpx.JSON(w, http.StatusAccepted, map[string]any{"run": runToDTO(judge)})
}

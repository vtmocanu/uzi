package handler

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// setMrReworkEnabledRequest is the PUT /api/runs/{id}/mr-rework body (PRD #841 M2). The
// field is NULLABLE, unlike wait-on-limit's non-nullable bool: mr_rework_enabled is a
// tri-state (nil = inherit the owner default, true/false = explicit override), so a
// null/absent "enabled" is a deliberate CLEAR back to inherit (D2), passed straight
// through as a *bool. A plain bool would collapse "inherit" into an explicit false.
type setMrReworkEnabledRequest struct {
	Enabled *bool `json:"enabled"`
}

// SetRunMrReworkEnabled flips ONE run's MR-rework override, the per-run surface for the
// MR review watcher (PRD #841 M2, Decision D2).
//
// 🔴 NO STATUS GUARD, and it must not have one. Unlike SetRunWaitOnLimit — which governs
// an in-flight run and so refuses a terminal run — the MR-rework watcher acts AFTER the
// run completes, during Human Review while its MR still has open comments. A terminal
// guard would lock the toggle exactly when it matters. The write is inert once the MR is
// no longer open, because the candidate query already excludes any run whose MR has left
// the opened state, so no explicit terminal guard is needed.
//
// The body's "enabled" is a *bool: null/absent clears the override back to inherit, and
// true/false set an explicit override. Ownership is the SQL predicate, not a pre-read: a
// foreign run yields 0 rows and 404 — never 403, which would confirm the run exists to
// someone who cannot see it.
func (h *Handler) SetRunMrReworkEnabled(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	runID, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	var req setMrReworkEnabledRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if _, err := h.q.SetRunMrReworkEnabled(r.Context(), store.SetRunMrReworkEnabledParams{
		ID: runID, UserID: user.ID, MrReworkEnabled: optBoolToPgtype(req.Enabled),
	}); err != nil {
		slog.Error("set run mr-rework", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	// Re-read owner-scoped rather than trusting the write's row count: 0 rows means the
	// run is not the caller's or does not exist, which is a 404. A successful owned write
	// re-reads and returns the run, mirroring SetRunWaitOnLimit's 404-vs-200 shape.
	run, err := h.q.GetRunByIDForUser(r.Context(), store.GetRunByIDForUserParams{ID: runID, UserID: user.ID})
	if err != nil {
		httpx.Error(w, http.StatusNotFound, "run not found")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"run": runToDTO(run, h.runPriorityClass(r.Context(), run), h.cfg.RunTimeout)})
}

// startRunReworkRequest is the POST /api/runs/{id}/rework body (PRD #1202): optional owner
// guidance steering WHAT to fix in the on-demand rework cycle. Absent/empty is a bare
// trigger, valid only when there is a genuinely-new review comment (the service 409s a bare
// trigger with nothing new).
type startRunReworkRequest struct {
	Guidance string `json:"guidance"`
}

// StartRunRework mints an ON-DEMAND mr_rework run for a completed run's still-open MR, past
// the automatic cap, with optional guidance (PRD #1202). It is the owner's escape hatch
// after the watcher halts, the sibling of the manual Fix CI button (CreateCIFixRun): it does
// the reads only the handler can do — the admin kill-switch (settings cache) and the forge
// review-comment snapshot — then hands the run-state checks + create to
// workersvc.StartMRReworkForRun so the web and CLI (both on this one endpoint) cannot drift.
//
// It is mounted RequireUser behind the per-user forge limiter (it reads the forge on every
// call, and the `uzi run rework` CLI verb needs a uzc_ Bearer). A foreign/missing run is 404
// (never 403); the service maps its own typed reasons to 409s naming why.
func (h *Handler) StartRunRework(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	runID, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	var req startRunReworkRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(req.Guidance) > MaxGuidanceBytes {
		httpx.Error(w, http.StatusBadRequest, "guidance is too long")
		return
	}

	// Admin kill-switch (D2): fail CLOSED exactly as the detector does — a read error OR a
	// false value refuses the feature. The per-user and per-run opt-outs are DELIBERATELY
	// ignored: a manual trigger is asked-for, so they do not apply. The settings cache is out
	// of workersvc's reach, so this read lives in the handler.
	enabled, err := h.settings.MrReworkEnabled(r.Context())
	if err != nil || !enabled {
		if err != nil {
			slog.Warn("mr-rework kill-switch read", "error", err)
		}
		httpx.Error(w, http.StatusConflict, "MR rework is disabled on this instance")
		return
	}

	// Read the run owner-scoped (404, never 403), then the repo keyed on the CALLER (a run
	// the caller owns on a repo they no longer own reads 404 — the createCIFixRun shape).
	run, err := h.q.GetRunByIDForUser(r.Context(), store.GetRunByIDForUserParams{ID: runID, UserID: user.ID})
	if err != nil {
		httpx.Error(w, http.StatusNotFound, "run not found")
		return
	}
	if !run.RepoID.Valid {
		// A repo-less run (chat) can never carry an open MR; the service would 409, but there
		// is nothing to read a forge for. Treat as not found — no MR-rework surface exists.
		httpx.Error(w, http.StatusNotFound, "run not found")
		return
	}
	repo, err := h.q.GetRepoForUser(r.Context(), store.GetRepoForUserParams{ID: uuid.UUID(run.RepoID.Bytes), UserID: user.ID})
	if err != nil {
		httpx.Error(w, http.StatusNotFound, "run not found")
		return
	}

	// Snapshot the MR's review comments from the forge (the FULL kept set rides the run,
	// Decision 3). Guarded on a real MR: a run with no mr_iid gets a nil snapshot and the
	// service returns the precise "this run has no merge request" 409 rather than a spurious
	// forge error. A forge read failure is 502 with the already-redacted error, as Fix CI is.
	var snapshot *workersvc.ReviewCommentsSnapshot
	if run.MrIid.Valid {
		f, err := h.svc.ForgeForConnection(repo.ForgeType, repo.BaseUrl, repo.TokenCiphertext)
		if err != nil {
			slog.Error("mr-rework: build forge for connection", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
		comments, err := f.ListMergeRequestComments(r.Context(), repo.ForgeProjectID, run.MrIid.Int64)
		if err != nil {
			// err is already PAT-redacted by the driver.
			httpx.Error(w, http.StatusBadGateway, "could not read the merge request comments: "+err.Error())
			return
		}
		snapshot = workersvc.BuildReviewCommentsSnapshot(comments, repo.BotForgeUserID)
	}

	run, err = h.wsvc.StartMRReworkForRun(r.Context(), user.ID, runID, req.Guidance, snapshot)
	if err != nil {
		switch {
		case errors.Is(err, workersvc.ErrRunNotFound):
			httpx.Error(w, http.StatusNotFound, "run not found")
		case errors.Is(err, workersvc.ErrReworkKindUnsupported),
			errors.Is(err, workersvc.ErrReworkRunNotCompleted),
			errors.Is(err, workersvc.ErrReworkNoMR),
			errors.Is(err, workersvc.ErrReworkMRNotOpen),
			errors.Is(err, workersvc.ErrReworkNoToken),
			errors.Is(err, workersvc.ErrReworkNothingNew):
			// Each carries its own user-facing reason (the service's sentinel message).
			httpx.Error(w, http.StatusConflict, err.Error())
		case errors.Is(err, workersvc.ErrBranchInUse):
			httpx.Error(w, http.StatusConflict, "a CI fix is working this branch")
		case errors.Is(err, workersvc.ErrActiveMRReworkExists):
			httpx.Error(w, http.StatusConflict, "a rework is already running for this merge request")
		default:
			slog.Error("start run rework", "run_id", runID, "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	httpx.JSON(w, http.StatusCreated, map[string]any{"run": runToDTO(run, h.runPriorityClass(r.Context(), run), h.cfg.RunTimeout)})
}

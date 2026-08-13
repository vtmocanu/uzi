package handler

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"gitlab.example.com/vtmocanu/uzi/api/internal/httpx"
	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/pipelinestatus"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
	"gitlab.example.com/vtmocanu/uzi/api/internal/workersvc"
)

// CreateCIFixRun queues a ci_fix run for a failed pipeline on a watched ref (PRD
// #6). It re-validates the Fix CI preconditions server-side — the pipeline cache
// must show the ref FAILED — captures a self-contained snapshot of the failed
// pipeline's jobs + log tails, and hands off to workersvc.CreateCIFixRun (which
// enforces the cross-kind same-branch exclusion and the one-active-fix-per-ref
// index). The plan gate still keeps a human in the loop before any fix lands.
func (h *Handler) CreateCIFixRun(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.repoForRequest(w, r)
	if !ok {
		return
	}
	user, _ := mw.UserFromContext(r.Context())
	var req struct {
		Ref string `json:"ref"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	ref := strings.TrimSpace(req.Ref)
	if ref == "" {
		httpx.Error(w, http.StatusBadRequest, "ref is required")
		return
	}

	// Precondition: the pipeline cache must show this ref FAILED — the same gate the
	// UI's Fix CI button reads, re-checked here since the cache identifies which
	// pipeline to snapshot.
	ps, err := h.q.GetPipelineStatusByRef(r.Context(), store.GetPipelineStatusByRefParams{RepoID: repo.ID, Ref: ref})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Error(w, http.StatusConflict, "no cached pipeline for this ref")
			return
		}
		slog.Error("ci-fix: get pipeline status", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !pipelinestatus.IsFailed(ps.Status) {
		httpx.Error(w, http.StatusConflict, "the latest pipeline for this ref is not failed")
		return
	}

	f, err := h.svc.ForgeForConnection(repo.ForgeType, repo.BaseUrl, repo.TokenCiphertext)
	if err != nil {
		slog.Error("build forge for connection", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	snapshot, err := workersvc.BuildFailureSnapshot(r.Context(), f, repo.ForgeProjectID, ps, h.cfg.CIFixMaxJobs, h.cfg.CIFixLogTailBytes)
	if err != nil {
		// err is already PAT-redacted by the driver.
		httpx.Error(w, http.StatusBadGateway, "could not snapshot the failed pipeline: "+err.Error())
		return
	}

	// Resolve the guard's watch set (PRD #71 M2): the configured default globs unioned
	// with the project's own ci_config_path. A fetch failure degrades to defaults —
	// manual Fix CI runs are human-approved, so the guard never reads this set for them.
	projectPath, err := f.ProjectCIConfigPath(r.Context(), repo.ForgeProjectID)
	if err != nil {
		slog.Warn("ci-fix: fetch ci_config_path", "error", err)
		projectPath = "" // degrade to defaults; manual runs are human-approved so the guard never reads this
	}
	ciConfigPaths := workersvc.MergeCIConfigPaths(h.cfg.CIAutofixConfigPaths, projectPath)

	title := fmt.Sprintf("Fix CI: %s pipeline #%d", ref, ps.PipelineID)
	description := fmt.Sprintf("Diagnose and fix the failed pipeline for `%s`.\n\nFailing pipeline: %s", ref, ps.WebUrl)

	run, err := h.wsvc.CreateCIFixRun(r.Context(), user.ID, repo.ID, ref, title, description, snapshot, ciConfigPaths)
	if err != nil {
		// #66 D1 layer 2: the service-layer guardrail refused. 422 with the forge.go:191
		// body shape (error + violations), matching the issue-lane gate.
		var ge *workersvc.GuardrailBlockedError
		if errors.As(err, &ge) {
			httpx.JSON(w, http.StatusUnprocessableEntity, map[string]any{
				"error":      "this CI-fix run was refused: the bot can push or merge to the repo's default branch, or that could not be verified (main is never touched). Fix branch protection on the forge, then retry.",
				"violations": ge.Findings,
			})
			return
		}
		switch {
		case errors.Is(err, workersvc.ErrRepoNotFound):
			httpx.Error(w, http.StatusNotFound, "repo not found")
		case errors.Is(err, workersvc.ErrActiveFixExists):
			httpx.Error(w, http.StatusConflict, "an active CI-fix run already exists for this ref")
		case errors.Is(err, workersvc.ErrBranchInUse):
			httpx.Error(w, http.StatusConflict, "an active run already occupies this branch")
		default:
			slog.Error("create ci-fix run", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	httpx.JSON(w, http.StatusCreated, map[string]any{"run": runToDTO(run)})
}

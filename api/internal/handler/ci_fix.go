package handler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"

	"gitlab.example.com/vtmocanu/uzi/api/internal/forge"
	"gitlab.example.com/vtmocanu/uzi/api/internal/httpx"
	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/pipelinestatus"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
	"gitlab.example.com/vtmocanu/uzi/api/internal/workersvc"
)

// snapshotSecretPatterns is the dedicated log-tail scrubber for the failure
// snapshot (PRD #6). A job trace is a SUCCESS body, so the forge driver's
// error-only, connection-PAT-only redactor does not cover it — this second,
// pattern-based pass runs BEFORE any tail is frozen into failure_snapshot or a
// claim payload. It targets the token SHAPES a teammate's pipeline might print:
//   - GitLab token families (glpat-, gloas-, glrt-, glcbt-, glptt-, glsoat-,
//     glimt-, glagent-, gldt- deploy tokens) — a long base62/underscore/dash body.
//   - Anthropic keys (sk-ant-...), the shape of a printed per-user token.
//   - Auth header lines a `curl -v` / `set -x` echo would emit: PRIVATE-TOKEN,
//     Authorization (Bearer ...), and a bare "Bearer <token>".
//   - uzi's own Bearer credentials (uzw_/uzc_/uza_, PRD #64 Risk 14). The CLI PRD
//     tells users to put UZI_TOKEN in a GitLab CI variable, so a `uzi ...` invocation
//     that echoes its token into a trace is exactly the path this snapshot ingests —
//     a uzc_/uza_ minted by this API must never freeze into a failure_snapshot.
//
// Arbitrary third-party secrets with no recognizable shape remain the documented
// residual risk (docs/configuration.md); the snapshot is owner/admin-visible only.
var snapshotSecretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`gl(pat|oas|rt|cbt|ptt|soat|imt|agent|dt)-[A-Za-z0-9_\-]{16,}`),
	regexp.MustCompile(`sk-ant-[A-Za-z0-9_\-]{16,}`),
	regexp.MustCompile(`uz[caw]_[A-Za-z0-9_\-]{16,}`),
	// Header lines a `curl -v` / `set -x` echoes — redact the WHOLE value to EOL
	// (`.` excludes newline, so `.*` stops at the line end), never just the first word.
	regexp.MustCompile(`(?i)(private-token|authorization)\s*[:=].*`),
	regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._\-]{16,}`),
}

// scrubKnownTokens redacts known token shapes from snapshot log-tail content.
func scrubKnownTokens(s string) string {
	for _, re := range snapshotSecretPatterns {
		s = re.ReplaceAllString(s, "[REDACTED]")
	}
	return s
}

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
	snapshot, err := h.snapshotFailedPipeline(r.Context(), f, repo.ForgeProjectID, ps)
	if err != nil {
		// err is already PAT-redacted by the driver.
		httpx.Error(w, http.StatusBadGateway, "could not snapshot the failed pipeline: "+err.Error())
		return
	}

	title := fmt.Sprintf("Fix CI: %s pipeline #%d", ref, ps.PipelineID)
	description := fmt.Sprintf("Diagnose and fix the failed pipeline for `%s`.\n\nFailing pipeline: %s", ref, ps.WebUrl)

	run, err := h.wsvc.CreateCIFixRun(r.Context(), user.ID, repo.ID, ref, title, description, snapshot)
	if err != nil {
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

// snapshotFailedPipeline freezes the failed pipeline into a self-contained
// FailureSnapshot: up to CIFixMaxJobs failed jobs, each with a CIFixLogTailBytes
// tail of its trace. Log tails pass the driver's connection-PAT scrub (M1) plus a
// known-token scrub here. A single unreadable trace degrades to an empty tail, not
// a whole-snapshot failure.
func (h *Handler) snapshotFailedPipeline(ctx context.Context, f forge.Forge, projectID int64, ps store.PipelineStatus) (workersvc.FailureSnapshot, error) {
	snap := workersvc.FailureSnapshot{
		PipelineID: ps.PipelineID,
		Ref:        ps.Ref,
		SHA:        ps.Sha,
		WebURL:     ps.WebUrl,
	}
	jobs, err := f.ListPipelineJobs(ctx, projectID, ps.PipelineID)
	if err != nil {
		return workersvc.FailureSnapshot{}, err
	}
	for _, j := range jobs {
		if !pipelinestatus.IsFailed(j.Status) {
			continue
		}
		if len(snap.FailedJobs) >= h.cfg.CIFixMaxJobs {
			break
		}
		tail, err := f.JobLogTail(ctx, projectID, j.ID, h.cfg.CIFixLogTailBytes)
		if err != nil {
			slog.Warn("ci-fix: job log tail", "job", j.ID, "error", err)
			tail = ""
		}
		snap.FailedJobs = append(snap.FailedJobs, workersvc.SnapshotJob{
			Name:    j.Name,
			Stage:   j.Stage,
			WebURL:  j.WebURL,
			LogTail: scrubKnownTokens(tail),
		})
	}
	return snap, nil
}

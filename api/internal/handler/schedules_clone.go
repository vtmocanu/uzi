package handler

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	"github.com/vtmocanu/uzi/api/internal/schedsvc"
	"github.com/vtmocanu/uzi/api/internal/schedtmpl"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// schedules_clone.go holds the derive-a-row handlers: clone a schedule and add a
// repo to a sibling group, plus their next-fire helper (PRD #1022 file split).

// CloneSchedule copies a schedule into a new fully-editable user-origin row (POST
// /api/schedules/{id}/clone, PRD #589 M3). The source is owner-scoped (404 if not owned).
// An optional body {"repo_id": "<uuid>"} clones into a DIFFERENT repo the caller owns (the
// replication path); an absent/empty body clones into the source's own repo.
//
// The key behaviour is that cloning a DEFAULT-origin schedule LIFTS the catalog prompt lock:
// a default row stores NULL prompt/labels/guidance and resolves them from the builtin catalog
// by catalog_slug at read time (Decision 2), so it is not directly editable. The clone
// resolves the catalog job and COPIES its baked Prompt (prompt target) or Labels+Guidance
// (sweep target) into the new row's columns, then inserts via CreateRunSchedule — which yields
// origin='user', catalog_slug=NULL, customized=false. The clone is therefore a normal user
// schedule the owner can freely edit. Cloning a user-origin source copies its stored
// prompt/labels/guidance/target/issue_iid as-is. Either way the editable fields (cron_expr,
// timezone, model, auto_approve, wait_on_limit, max_issues, override_subagent_model, timing,
// enabled) are copied and next_fire_at is recomputed from the copied timing.
func (h *Handler) CloneSchedule(w http.ResponseWriter, r *http.Request) {
	user, id, ok := h.scheduleParam(w, r)
	if !ok {
		return
	}
	cur, err := h.q.GetRunScheduleForUser(r.Context(), store.GetRunScheduleForUserParams{ID: id, UserID: user.ID})
	if err != nil {
		httpx.Error(w, http.StatusNotFound, "schedule not found")
		return
	}

	// Optional body. An empty body decodes to io.EOF, which is the "clone into the source
	// repo" case, not a malformed request; any other decode error is a 400.
	var req apitypes.ScheduleCloneRequest
	if derr := httpx.DecodeJSONLimited(w, r, &req); derr != nil && !errors.Is(derr, io.EOF) {
		httpx.RespondDecodeError(w, derr, "invalid request body")
		return
	}

	targetRepo := cur.RepoID
	if req.RepoID != nil && strings.TrimSpace(*req.RepoID) != "" {
		rid, perr := uuid.Parse(strings.TrimSpace(*req.RepoID))
		if perr != nil {
			httpx.Error(w, http.StatusBadRequest, "invalid repo id")
			return
		}
		// Ownership mirror (create parity): 404 on a foreign/absent target repo.
		if _, gerr := h.q.GetRepoForUser(r.Context(), store.GetRepoForUserParams{ID: rid, UserID: user.ID}); gerr != nil {
			httpx.Error(w, http.StatusNotFound, "repo not found")
			return
		}
		targetRepo = rid
	}

	// The target/prompt columns: copied straight from the source for a user row, or lifted
	// from the builtin catalog for a default row (the lock-lift described above).
	issueIID := cur.IssueIid
	labels := cur.Labels
	prompt := cur.Prompt
	guidance := cur.Guidance
	if cur.Origin == "default" {
		job, found := schedtmpl.BySlug(cur.CatalogSlug.String)
		if !found {
			httpx.Error(w, http.StatusUnprocessableEntity, "this schedule's catalog entry no longer exists")
			return
		}
		issueIID = pgtype.Int8{}
		labels = nil
		prompt = pgtype.Text{}
		guidance = pgtype.Text{}
		switch cur.Target {
		case "prompt":
			prompt = pgtype.Text{String: job.Prompt, Valid: true}
		case "sweep":
			labels = marshalLabels(job.Labels)
			// Baked-only clone (issue #675): the cloned user row carries the BAKED catalog
			// guidance; the source row's stored owner OVERLAY (cur.Guidance, reset to empty
			// above) is intentionally discarded, matching the prompt-clone in #662.
			if strings.TrimSpace(job.Guidance) != "" {
				guidance = pgtype.Text{String: job.Guidance, Valid: true}
			}
		case "self_improve":
			// A self_improve row carries no prompt/labels/guidance to bake — the whole directive
			// is worker-side (PRD #590 M1). The reset to NULL/empty above already holds, so the
			// clone is a user-origin self_improve row carrying only cadence/model. The shape
			// CHECK's self_improve arm is origin-agnostic, so a user-origin clone is valid.
		}
	}

	nextFire, err := cloneNextFire(cur, h.clock())
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "could not compute the next fire time")
		return
	}

	s, err := h.q.CreateRunSchedule(r.Context(), store.CreateRunScheduleParams{
		UserID:                user.ID,
		RepoID:                targetRepo,
		Target:                cur.Target,
		IssueIid:              issueIID,
		Labels:                labels,
		Prompt:                prompt,
		Timing:                cur.Timing,
		CronExpr:              cur.CronExpr,
		RunAt:                 cur.RunAt,
		Timezone:              cur.Timezone,
		NextFireAt:            nextFire,
		AutoApprove:           cur.AutoApprove,
		WaitOnLimit:           cur.WaitOnLimit,
		Enabled:               cur.Enabled,
		MaxIssues:             cur.MaxIssues,
		Guidance:              guidance,
		Model:                 cur.Model,
		OutputMode:            cur.OutputMode,
		OverrideSubagentModel: cur.OverrideSubagentModel,
	})
	if err != nil {
		slog.Error("clone run schedule", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusCreated, h.scheduleDTO(s, h.repoPathFor(r.Context(), s)))
}

// AddScheduleRepo replicates a custom schedule's current config onto another repo the caller
// owns as a new sibling in the source's group (POST /api/schedules/{id}/add-repo, PRD #636 M1,
// Decision 5). Unlike /clone (which is frozen and never groups), this endpoint stamps a shared
// sibling_group_id so the web can render the source and the new row as one expandable summary.
//
// It is owner-scoped (404 for a foreign source or target repo) and origin='user' only (a
// default-origin source is catalog-owned; 409, mirroring ResetSchedule's origin gate). In ONE
// transaction it (a) ensures the source has a group id via the coalescing, race-safe UPDATE —
// two concurrent add-repo calls both settle on one id under the row lock, so no split group —
// and (b) copies the source's config into a new row on the target repo carrying that group id.
// A duplicate target repo already in the group conflicts on the partial unique index and is a
// clean 409 (no second row), so add-repo is idempotent-safe.
func (h *Handler) AddScheduleRepo(w http.ResponseWriter, r *http.Request) {
	user, id, ok := h.scheduleParam(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	cur, err := h.q.GetRunScheduleForUser(ctx, store.GetRunScheduleForUserParams{ID: id, UserID: user.ID})
	if err != nil {
		httpx.Error(w, http.StatusNotFound, "schedule not found")
		return
	}
	if cur.Origin != "user" {
		// A default-origin row's config is catalog-owned; clone it first to get an editable
		// user row (mirrors ResetSchedule's 409 for an origin mismatch).
		httpx.Error(w, http.StatusConflict, "only a custom schedule can add a repo; clone a default schedule first")
		return
	}
	if cur.Target == "issue" {
		// issue_iid is repo-relative, so copying it onto a sibling repo points at a
		// different, unrelated issue (mirrors scheduleRepoChange's repoint rejection).
		httpx.Error(w, http.StatusUnprocessableEntity,
			"adding a repo to an issue-target schedule is not supported; issue numbers are repo-relative, so delete and recreate it against the new repo")
		return
	}

	var req apitypes.AddScheduleRepoRequest
	if derr := httpx.DecodeJSONLimited(w, r, &req); derr != nil {
		httpx.RespondDecodeError(w, derr, "invalid request body")
		return
	}
	if strings.TrimSpace(req.RepoID) == "" {
		httpx.Error(w, http.StatusBadRequest, "repo_id is required")
		return
	}
	targetRepo, perr := uuid.Parse(strings.TrimSpace(req.RepoID))
	if perr != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid repo id")
		return
	}
	// Ownership mirror (clone parity): 404 on a foreign/absent target repo.
	if _, gerr := h.q.GetRepoForUser(ctx, store.GetRepoForUserParams{ID: targetRepo, UserID: user.ID}); gerr != nil {
		httpx.Error(w, http.StatusNotFound, "repo not found")
		return
	}

	nextFire, err := cloneNextFire(cur, h.clock())
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "could not compute the next fire time")
		return
	}

	tx, err := h.pool.Begin(ctx)
	if err != nil {
		slog.Error("add schedule repo: begin", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after a successful Commit
	qtx := h.q.WithTx(tx)

	// (a) Ensure the source carries a group id, race-safely (COALESCE under the row lock).
	newGroup := pgtype.UUID{Bytes: uuid.New(), Valid: true}
	group, err := qtx.CoalesceScheduleSiblingGroup(ctx, store.CoalesceScheduleSiblingGroupParams{
		NewGroup: newGroup,
		ID:       id,
		UserID:   user.ID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// The source is no longer a user-origin row the caller owns (raced with a delete).
			httpx.Error(w, http.StatusNotFound, "schedule not found")
			return
		}
		slog.Error("add schedule repo: coalesce group", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}

	// (b) Copy the source's config onto the target repo as a new sibling carrying that group.
	s, err := qtx.CreateRunSchedule(ctx, store.CreateRunScheduleParams{
		UserID:                user.ID,
		RepoID:                targetRepo,
		Target:                cur.Target,
		IssueIid:              cur.IssueIid,
		Labels:                cur.Labels,
		Prompt:                cur.Prompt,
		Timing:                cur.Timing,
		CronExpr:              cur.CronExpr,
		RunAt:                 cur.RunAt,
		Timezone:              cur.Timezone,
		NextFireAt:            nextFire,
		AutoApprove:           cur.AutoApprove,
		WaitOnLimit:           cur.WaitOnLimit,
		Enabled:               cur.Enabled,
		MaxIssues:             cur.MaxIssues,
		Guidance:              cur.Guidance,
		Model:                 cur.Model,
		OutputMode:            cur.OutputMode,
		OverrideSubagentModel: cur.OverrideSubagentModel,
		SiblingGroupID:        group,
	})
	if err != nil {
		if isUniqueViolation(err) {
			// The target repo is already a sibling in this group: idempotent-safe, no second
			// row created (the tx rolls back the coalesce too, which is a harmless no-op when
			// the source already had the group id).
			httpx.Error(w, http.StatusConflict, "this schedule already has a sibling on that repo")
			return
		}
		slog.Error("add schedule repo: create sibling", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		slog.Error("add schedule repo: commit", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusCreated, h.scheduleDTO(s, h.repoPathFor(ctx, s)))
}

// cloneNextFire recomputes the durable next_fire_at for a clone from the source's copied
// timing: the next cron fire for a recurring schedule, or the (future) run_at for a once
// schedule. A once schedule whose run_at has already passed cannot be cloned to a valid
// future fire and surfaces as a 400 in the caller.
func cloneNextFire(cur store.RunSchedule, now time.Time) (pgtype.Timestamptz, error) {
	switch cur.Timing {
	case "recurring":
		t, err := schedsvc.NextFire(cur.CronExpr.String, cur.Timezone, now)
		if err != nil {
			return pgtype.Timestamptz{}, err
		}
		return pgtype.Timestamptz{Time: t, Valid: true}, nil
	case "once":
		if !cur.RunAt.Valid {
			return pgtype.Timestamptz{}, fmt.Errorf("once schedule missing run_at")
		}
		t, err := schedsvc.OnceFire(cur.RunAt.Time, now)
		if err != nil {
			return pgtype.Timestamptz{}, err
		}
		return pgtype.Timestamptz{Time: t, Valid: true}, nil
	default:
		return pgtype.Timestamptz{}, fmt.Errorf("unknown timing %q", cur.Timing)
	}
}

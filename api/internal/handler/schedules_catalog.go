package handler

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/agenttmpl"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/schedsvc"
	"github.com/vtmocanu/uzi/api/internal/schedtmpl"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// schedules_catalog.go holds the default-schedule catalog surface: catalog listing,
// enable/reset, the default-config patch path and its DTO/divergence helpers (PRD #1022 file split).

// ScheduleCatalog returns the builtin default-schedule catalog plus the caller's per-repo
// enablement state (GET /api/schedule-catalog, PRD #589 M2). The catalog is the same 6
// shipped entries on every request; the enablement list is owner-scoped.
func (h *Handler) ScheduleCatalog(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	entries := schedtmpl.Catalog()
	out := apitypes.ScheduleCatalogResponse{
		Entries:     make([]apitypes.CatalogEntryDTO, 0, len(entries)),
		Enablements: []apitypes.CatalogEnablementDTO{},
	}
	for _, j := range entries {
		out.Entries = append(out.Entries, catalogEntryDTO(j))
	}
	rows, err := h.q.ListEnabledDefaultsForUser(r.Context(), user.ID)
	if err != nil {
		slog.Error("list enabled defaults", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	for _, row := range rows {
		out.Enablements = append(out.Enablements, apitypes.CatalogEnablementDTO{
			RepoID:     row.RepoID.String(),
			Slug:       row.CatalogSlug.String,
			ScheduleID: row.ID.String(),
			Enabled:    row.Enabled,
		})
	}
	httpx.JSON(w, http.StatusOK, out)
}

// EnableCatalogSchedule enables a builtin default scheduled job on a repo the caller owns
// (POST /api/repos/{id}/schedule-catalog/{slug}, PRD #589 M2). It resolves the catalog job
// by {slug} (404 if unknown), computes next_fire_at from the job's cron+timezone, and
// inserts a default-origin row. The insert is idempotent per (user, repo, slug): a repeat
// enable inserts nothing and returns the existing row with 200 (the partial unique index
// backs the ON CONFLICT DO NOTHING).
func (h *Handler) EnableCatalogSchedule(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.repoForRequest(w, r)
	if !ok {
		return
	}
	user, _ := mw.UserFromContext(r.Context())
	slug := chi.URLParam(r, "slug")
	job, ok := schedtmpl.BySlug(slug)
	if !ok {
		httpx.Error(w, http.StatusNotFound, "unknown catalog slug")
		return
	}
	// Optional body: a timezone override (issue #660). An empty/absent body decodes to
	// io.EOF and keeps the catalog zone (CLI/headless and older clients send none); a
	// present, valid IANA name overrides it so the first fire lands in the caller's detected
	// zone. Any other decode error is a malformed request (400).
	var req apitypes.EnableCatalogRequest
	if derr := httpx.DecodeJSONLimited(w, r, &req); derr != nil && !errors.Is(derr, io.EOF) {
		httpx.RespondDecodeError(w, derr, "invalid request body")
		return
	}
	tz := catalogTimezone(job)
	if override := strings.TrimSpace(req.Timezone); override != "" {
		// Reject the "Local" sentinel: time.LoadLocation("Local") succeeds and resolves to
		// the server's time.Local, which would make the schedule fire in the deployment's
		// zone instead of a real IANA one. Any other invalid name fails LoadLocation below.
		if override == "Local" {
			httpx.Error(w, http.StatusBadRequest, "invalid timezone")
			return
		}
		if _, lerr := time.LoadLocation(override); lerr != nil {
			httpx.Error(w, http.StatusBadRequest, "invalid timezone")
			return
		}
		tz = override
	}
	next, err := schedsvc.NextFire(job.Cron, tz, h.clock())
	if err != nil {
		slog.Error("enable default schedule: next fire", "slug", slug, "error", err)
		httpx.Error(w, http.StatusInternalServerError, "could not compute the next fire time")
		return
	}
	slugText := pgtype.Text{String: slug, Valid: true}
	s, err := h.q.CreateDefaultSchedule(r.Context(), store.CreateDefaultScheduleParams{
		UserID:      user.ID,
		RepoID:      repo.ID,
		Target:      job.Target,
		CatalogSlug: slugText,
		CronExpr:    pgtype.Text{String: job.Cron, Valid: true},
		Timezone:    tz,
		NextFireAt:  pgtype.Timestamptz{Time: next, Valid: true},
		AutoApprove: schedtmpl.AutoApprove,
		WaitOnLimit: schedtmpl.WaitOnLimit,
		MaxIssues:   catalogMaxIssues(job),
		Model:       catalogModel(job),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// ON CONFLICT DO NOTHING inserted nothing: the owner already enabled this job on
			// this repo. Return the existing row (idempotent enable, 200).
			existing, gerr := h.q.GetDefaultScheduleForRepoSlug(r.Context(), store.GetDefaultScheduleForRepoSlugParams{
				UserID:      user.ID,
				RepoID:      repo.ID,
				CatalogSlug: slugText,
			})
			if gerr != nil {
				slog.Error("enable default schedule: fetch existing", "slug", slug, "error", gerr)
				httpx.Error(w, http.StatusInternalServerError, "internal error")
				return
			}
			httpx.JSON(w, http.StatusOK, h.scheduleDTO(existing, repo.PathWithNamespace))
			return
		}
		slog.Error("enable default schedule", "slug", slug, "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusCreated, h.scheduleDTO(s, repo.PathWithNamespace))
}

// ResetSchedule restores a default-origin schedule's editable fields to the builtin
// catalog defaults and clears its customized flag (POST /api/schedules/{id}/reset, PRD
// #589 M2). Owner-scoped; a user-origin row is a 409 (nothing to reset to). A schedule
// whose catalog entry has since been removed is a 422.
func (h *Handler) ResetSchedule(w http.ResponseWriter, r *http.Request) {
	user, id, ok := h.scheduleParam(w, r)
	if !ok {
		return
	}
	cur, err := h.q.GetRunScheduleForUser(r.Context(), store.GetRunScheduleForUserParams{ID: id, UserID: user.ID})
	if err != nil {
		httpx.Error(w, http.StatusNotFound, "schedule not found")
		return
	}
	if cur.Origin != "default" {
		httpx.Error(w, http.StatusConflict, "only a default schedule can be reset")
		return
	}
	job, ok := schedtmpl.BySlug(cur.CatalogSlug.String)
	if !ok {
		httpx.Error(w, http.StatusUnprocessableEntity, "this schedule's catalog entry no longer exists")
		return
	}
	tz := catalogTimezone(job)
	next, err := schedsvc.NextFire(job.Cron, tz, h.clock())
	if err != nil {
		slog.Error("reset default schedule: next fire", "schedule", id.String(), "error", err)
		httpx.Error(w, http.StatusInternalServerError, "could not compute the next fire time")
		return
	}
	s, err := h.q.ResetDefaultSchedule(r.Context(), store.ResetDefaultScheduleParams{
		CronExpr:    pgtype.Text{String: job.Cron, Valid: true},
		Timezone:    tz,
		Model:       catalogModel(job),
		AutoApprove: schedtmpl.AutoApprove,
		WaitOnLimit: schedtmpl.WaitOnLimit,
		MaxIssues:   catalogMaxIssues(job),
		NextFireAt:  pgtype.Timestamptz{Time: next, Valid: true},
		ID:          id,
		UserID:      user.ID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Error(w, http.StatusNotFound, "schedule not found")
			return
		}
		slog.Error("reset default schedule", "schedule", id.String(), "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, h.scheduleDTO(s, h.repoPathFor(r.Context(), s)))
}

// patchDefaultScheduleConfig applies a config PATCH to a default-origin schedule (PRD #589
// M2). Only the catalog-inherited editable fields (cron_expr, timezone, model, auto_approve,
// wait_on_limit, and — for a sweep — max_issues) may be edited; prompt/labels/target/repo/
// timing are catalog-owned and a request touching them is a 400. Guidance is catalog-owned
// for issue/self_improve defaults but owner-editable for a PROMPT default (issue #662: it
// overlays the catalog prompt at fire time) and for a SWEEP default (issue #675: an owner
// OVERLAY composed onto the baked catalog guidance at fire time — the baked value stays
// catalog-owned and is surfaced read-only as BakedGuidance). The editable overlay takes
// replace-semantics under an 8 KiB cap. It recomputes next_fire_at, recomputes the customized flag (any editable field
// diverging from the catalog default OR the row was already customized), and persists via
// UpdateRunSchedule keeping the catalog-owned columns NULL. It writes the HTTP error itself
// and returns done=false on any failure; on success it returns the updated row and done=true.
func (h *Handler) patchDefaultScheduleConfig(w http.ResponseWriter, r *http.Request, user store.User, id uuid.UUID, cur store.RunSchedule, req apitypes.ScheduleRequest) (store.RunSchedule, bool) {
	// Owner-guidance overlay (issue #662, extended by issue #675): a DEFAULT PROMPT job
	// carries owner-editable guidance appended to the catalog-resolved prompt at fire time,
	// and a DEFAULT SWEEP job carries an owner OVERLAY composed onto the baked catalog
	// guidance at fire time — so guidance is NOT catalog-owned for a prompt or sweep default.
	// Issue/self_improve defaults keep guidance catalog-owned (locked). All the other fields
	// stay catalog-owned for every target.
	guidanceEditable := cur.Target == "prompt" || cur.Target == "sweep"
	if req.Prompt != "" || req.Labels != nil || (!guidanceEditable && req.Guidance != nil) || req.Target != "" ||
		req.RepoID != "" || req.IssueIID != nil || req.Timing != "" || req.RunAt != nil {
		locked := "prompt, labels, guidance, target, timing and repo"
		if guidanceEditable {
			locked = "prompt, labels, target, timing and repo"
		}
		httpx.Error(w, http.StatusBadRequest, "a default schedule's "+locked+" are catalog-owned and cannot be edited")
		return store.RunSchedule{}, false
	}
	if guidanceEditable && req.Guidance != nil && len(*req.Guidance) > MaxGuidanceBytes {
		httpx.Error(w, http.StatusUnprocessableEntity, "guidance is too large")
		return store.RunSchedule{}, false
	}

	cron := cur.CronExpr.String
	if req.CronExpr != "" {
		cron = req.CronExpr
	}
	tz := cur.Timezone
	if req.Timezone != "" {
		tz = strings.TrimSpace(req.Timezone)
	}
	if tz == "" {
		tz = "UTC"
	}
	if err := schedsvc.ValidateCron(cron); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid cron expression")
		return store.RunSchedule{}, false
	}
	if _, err := time.LoadLocation(tz); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid timezone")
		return store.RunSchedule{}, false
	}

	model := cur.Model
	if req.Model != nil {
		normalized, err := agenttmpl.ValidateModel(*req.Model)
		if err != nil {
			httpx.Error(w, http.StatusBadRequest, "model: "+err.Error())
			return store.RunSchedule{}, false
		}
		if normalized == "" {
			model = pgtype.Text{}
		} else {
			model = pgtype.Text{String: normalized, Valid: true}
		}
	}

	autoApprove := cur.AutoApprove
	if req.AutoApprove != nil {
		autoApprove = *req.AutoApprove
	}
	// Item 2 (PRD #590 follow-up): a self_improve run is always auto-approved
	// (CreateSelfImproveRun hardcodes auto_approve=true, selfimprove.sql); force the schedule's
	// stored flag true regardless of the request so the DTO/modal never misrepresent it as
	// manual-approve. The catalog default is auto_approve=true, so this keeps the row from
	// spuriously flagging customized.
	if cur.Target == "self_improve" {
		autoApprove = true
	}
	waitOnLimit := cur.WaitOnLimit
	if req.WaitOnLimit != nil {
		waitOnLimit = *req.WaitOnLimit
	}

	// mr_rework (PRD #841) uses replace-semantics like mergeSchedule's mr_rework and
	// max_issues/guidance/model: the config PATCH rewrites the whole row (enabled-only is
	// short-circuited by onlyEnabled), so a null mr_rework_enabled must reach the DB as
	// NULL = inherit (D5). It is the tri-state *bool, so nil clears to inherit (unlike
	// wait_on_limit, a plain bool that keeps-on-nil). defaultEditableDiverges then
	// recomputes customized = false when the override returns to the nil/inherit baseline.
	mrRework := optBoolToPgtype(req.MrReworkEnabled)

	// max_issues is meaningful only for a sweep default; a prompt default keeps it NULL. For
	// a sweep it takes replace-semantics from the request (the web sends the full editable
	// config), same as the user path — an omitted value clears it to unlimited.
	maxIssues := pgtype.Int4{}
	if cur.Target == "sweep" {
		if req.MaxIssues != nil {
			if *req.MaxIssues <= 0 {
				httpx.Error(w, http.StatusBadRequest, "max_issues must be a positive integer")
				return store.RunSchedule{}, false
			}
			if *req.MaxIssues > MaxSweepIssues {
				httpx.Error(w, http.StatusBadRequest, fmt.Sprintf("max_issues must not exceed %d (leave it unset for unlimited)", MaxSweepIssues))
				return store.RunSchedule{}, false
			}
			maxIssues = pgtype.Int4{Int32: int32(*req.MaxIssues), Valid: true}
		}
	}

	next, err := schedsvc.NextFire(cron, tz, h.clock())
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "could not compute the next fire time")
		return store.RunSchedule{}, false
	}

	// Guidance replace-semantics (issue #662, #675), scoped to a prompt or sweep default. The
	// persisted column holds the owner OVERLAY (a prompt catalog job carries no guidance; a
	// sweep default's baked guidance stays catalog-owned and is never stored here), so any
	// persisted non-empty owner guidance is a divergence. Empty/whitespace clears it back to
	// NULL (an exact-restore un-customizes). For issue/self_improve defaults guidance stays
	// NULL — catalog-owned.
	guidance := pgtype.Text{}
	if guidanceEditable && req.Guidance != nil && strings.TrimSpace(*req.Guidance) != "" {
		guidance = pgtype.Text{String: *req.Guidance, Valid: true}
	}

	// override_subagent_model is a run option (not a catalog field), owner-editable on a
	// default (issue #691). It takes replace-semantics from the request like the other run
	// options — an omitted value keeps the stored one. Its catalog baseline is always false,
	// so any toggled-on value OR-s into customized (see the recompute below).
	ov := cur.OverrideSubagentModel
	if req.OverrideSubagentModel != nil {
		ov = *req.OverrideSubagentModel
	}

	// customized latches on divergence but the reset endpoint clears it; a patch that puts
	// every editable field back to the catalog default also clears it (recomputed fresh, not
	// OR-ed with a stale true — Reset and an exact-restore patch both un-customize).
	customized := false
	if job, ok := schedtmpl.BySlug(cur.CatalogSlug.String); ok {
		customized = defaultEditableDiverges(job, cron, tz, model, autoApprove, waitOnLimit, mrRework, maxIssues)
	} else {
		// Catalog entry gone: cannot compare, so preserve the stored flag rather than guess.
		customized = cur.Customized
	}
	// Owner guidance is not one of defaultEditableDiverges' inputs (its signature is
	// unchanged); OR-in its divergence for a prompt or sweep default. The stored column holds
	// only the owner overlay (the baked value is never compared), so any persisted non-empty
	// overlay diverges — and clearing it back to empty leaves guidance.Valid false, so an
	// exact-restore still un-customizes. A sweep default with a NULL overlay is therefore not
	// falsely "customized".
	if guidanceEditable {
		customized = customized || guidance.Valid
	}
	// override_subagent_model is a run option (not a catalog field, so not in
	// defaultEditableDiverges' inputs); its catalog baseline is always false, so any
	// toggled-on value diverges (issue #691). Mirrors the guidance precedent above.
	customized = customized || ov

	final, err := h.q.UpdateRunSchedule(r.Context(), store.UpdateRunScheduleParams{
		Target:                cur.Target,
		RepoID:                cur.RepoID,
		IssueIid:              pgtype.Int8{},
		Labels:                nil,
		Prompt:                pgtype.Text{},
		Timing:                "recurring",
		CronExpr:              pgtype.Text{String: cron, Valid: true},
		RunAt:                 pgtype.Timestamptz{},
		Timezone:              tz,
		NextFireAt:            pgtype.Timestamptz{Time: next, Valid: true},
		AutoApprove:           autoApprove,
		WaitOnLimit:           waitOnLimit,
		MrReworkEnabled:       mrRework,
		MaxIssues:             maxIssues,
		Guidance:              guidance,
		Model:                 model,
		OverrideSubagentModel: ov,
		Customized:            customized,
		ID:                    id,
		UserID:                user.ID,
	})
	if err != nil {
		slog.Error("update default run schedule", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return store.RunSchedule{}, false
	}
	return final, true
}

// catalogEntryDTO maps a builtin default job to its wire view (PRD #589 M2). auto_approve
// and wait_on_limit are the fixed schedtmpl run flags, not per-entry.
func catalogEntryDTO(j schedtmpl.DefaultJob) apitypes.CatalogEntryDTO {
	return apitypes.CatalogEntryDTO{
		Slug:         j.Slug,
		Name:         j.Name,
		Description:  j.Description,
		Target:       j.Target,
		Cron:         j.Cron,
		Timezone:     catalogTimezone(j),
		Model:        j.Model,
		Prompt:       j.Prompt,
		SelectorKind: j.SelectorKind,
		Labels:       j.Labels,
		Guidance:     j.Guidance,
		MaxIssues:    j.MaxIssues,
		AutoApprove:  schedtmpl.AutoApprove,
		WaitOnLimit:  schedtmpl.WaitOnLimit,
	}
}

// catalogTimezone returns a job's timezone, defaulting a blank to the catalog default so a
// stored/enabled row never carries an empty timezone.
func catalogTimezone(j schedtmpl.DefaultJob) string {
	if strings.TrimSpace(j.Timezone) == "" {
		return schedtmpl.DefaultTimezone
	}
	return j.Timezone
}

// catalogMaxIssues maps a job's max_issues to the nullable store column: set only for a
// sweep with a positive value, NULL (unlimited) otherwise — mirroring maxIssuesColumn.
func catalogMaxIssues(j schedtmpl.DefaultJob) pgtype.Int4 {
	// MaxIssues is bounded to (0, math.MaxInt32] just above so the int32 narrowing is
	// provably in range for both integer-conversion analyzers (gosec G115 / CodeQL
	// go/incorrect-integer-conversion); no nolint is needed once the bound is explicit.
	if j.Target == "sweep" && j.MaxIssues > 0 && j.MaxIssues <= math.MaxInt32 {
		return pgtype.Int4{Int32: int32(j.MaxIssues), Valid: true}
	}
	return pgtype.Int4{}
}

// catalogModel maps a job's model override to the nullable store column: NULL (inherit the
// owner default) when the catalog leaves it blank.
func catalogModel(j schedtmpl.DefaultJob) pgtype.Text {
	if strings.TrimSpace(j.Model) != "" {
		return pgtype.Text{String: j.Model, Valid: true}
	}
	return pgtype.Text{}
}

// defaultEditableDiverges reports whether a default row's editable fields differ from the
// catalog defaults (PRD #589 M2). It compares the editable run fields; the catalog-owned
// prompt/labels/guidance are excluded (they are never stored on the row). A blank catalog
// model and a NULL row model both mean "inherit", so they compare equal; a 0 catalog
// max_issues and a NULL row max_issues both mean "unlimited".
func defaultEditableDiverges(job schedtmpl.DefaultJob, cron, tz string, model pgtype.Text, autoApprove, waitOnLimit bool, mrRework pgtype.Bool, maxIssues pgtype.Int4) bool {
	if cron != job.Cron {
		return true
	}
	jtz := job.Timezone
	if strings.TrimSpace(jtz) == "" {
		jtz = schedtmpl.DefaultTimezone
	}
	if tz != jtz {
		return true
	}
	rModel := ""
	if model.Valid {
		rModel = model.String
	}
	if strings.TrimSpace(rModel) != strings.TrimSpace(job.Model) {
		return true
	}
	if autoApprove != schedtmpl.AutoApprove || waitOnLimit != schedtmpl.WaitOnLimit {
		return true
	}
	// mr_rework_enabled (PRD #841 M2, D5): the schedtmpl/catalog baseline is inherit
	// (nil), which for a nullable column is Valid=false. Nil-safe *bool comparison — an
	// unset row column equals the nil default, and ANY explicit override (Valid) diverges.
	if mrRework.Valid {
		return true
	}
	jMax := int32(0)
	// Bounded to (0, math.MaxInt32] so the int32 narrowing is provably in range for both
	// integer-conversion analyzers (gosec G115 / CodeQL go/incorrect-integer-conversion).
	if job.Target == "sweep" && job.MaxIssues > 0 && job.MaxIssues <= math.MaxInt32 {
		jMax = int32(job.MaxIssues)
	}
	rMax := int32(0)
	if maxIssues.Valid {
		rMax = maxIssues.Int32
	}
	return rMax != jMax
}

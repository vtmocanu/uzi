package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
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
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// -------------------------------------------------------------------------
// Scheduled runs (PRD #241 M4): owner-scoped CRUD + preview + run-now.
// -------------------------------------------------------------------------

// schedulePreviewCap bounds the "next fires" preview N so a client cannot ask for an
// unbounded fire computation (closes the M3-audit unbounded-N concern).
const schedulePreviewCap = 10

// MaxGuidanceBytes caps the optional owner guidance field (PRD #274 M3). It is kept well
// below workersvc.MaxIssueDescriptionBytes (256 KiB) so guidance is a few-KB steer, not a
// second body: the composition in schedsvc truncates guidance rather than skip an issue,
// but a small hard cap keeps a fat guidance value from crowding out the actual task.
const MaxGuidanceBytes = 8 * 1024

// MaxSweepIssues is the upper bound on a sweep's per-fire max_issues cap (PRD #274 M2).
// A sweep fans out one run per oldest issue, so a cap in the thousands is already far past
// any sane unattended batch; the real "no bound" intent is expressed by leaving max_issues
// NULL (unlimited), not by a giant number. The ceiling also guards the column write:
// max_issues persists into an int32 column (pgtype.Int4), so an unbounded *int would wrap
// negative past math.MaxInt32 and later render `LIMIT -N`, which Postgres rejects — wedging
// the sweep in a transient-retry loop. Rejecting above this ceiling closes that off.
const MaxSweepIssues = 10000

// validateScheduleConfig is the PURE (no store, no clock beyond the passed-in `now`)
// validator behind create/patch/preview, extracted so it is unit-testable without a
// DB. It normalizes the request — a blank timezone becomes "UTC", sweep labels are
// stripped of blanks — and returns the normalized request plus an HTTP status/message
// (status 0 == valid). It never touches the forge or the store; the per-target field
// invariants it enforces mirror the DB CHECK the row would otherwise fail on insert.
//
// allowSelfImprove gates the self_improve target (PRD #590 follow-up, item 1): a
// self_improve schedule is catalog-enable-only, so a DIRECT create (POST /schedules) must
// keep returning the same "target must be one of: issue, sweep, prompt" 400 (pass false),
// while a user-origin CLONE reconfiguring its own row routes its config PATCH through this
// validator and must be accepted. The patch/merge caller passes true ONLY when the row is
// already self_improve (cur.Target == "self_improve"), so editing an existing self_improve
// row is allowed but CONVERTING an issue/sweep/prompt row into one via PATCH stays a 400 —
// a create-by-patch the direct POST path also blocks.
func validateScheduleConfig(req apitypes.ScheduleRequest, now time.Time, allowSelfImprove bool) (apitypes.ScheduleRequest, int, string) {
	n := req
	n.Timezone = strings.TrimSpace(n.Timezone)
	if n.Timezone == "" {
		n.Timezone = "UTC"
	}

	// max_issues (PRD #274 M2), when present, must be a positive integer regardless of
	// target — a zero/negative cap is a nonsense fan-out bound. Its target scoping (sweep
	// only) is enforced per-target below.
	if n.MaxIssues != nil && *n.MaxIssues <= 0 {
		return n, http.StatusBadRequest, "max_issues must be a positive integer"
	}
	if n.MaxIssues != nil && *n.MaxIssues > MaxSweepIssues {
		return n, http.StatusBadRequest, fmt.Sprintf("max_issues must not exceed %d (leave it unset for unlimited)", MaxSweepIssues)
	}

	// guidance (PRD #274 M3): a blank/whitespace value is "none" — normalize it to nil so a
	// cleared web textarea clears the stored guidance rather than persisting whitespace.
	// The size cap applies regardless of target; the per-target scoping (issue/sweep only,
	// rejected on prompt) is enforced in the switch below.
	if n.Guidance != nil && strings.TrimSpace(*n.Guidance) == "" {
		n.Guidance = nil
	}
	if n.Guidance != nil && len(*n.Guidance) > MaxGuidanceBytes {
		return n, http.StatusUnprocessableEntity, "guidance is too large"
	}

	// model (PRD #300): validated with the shared Decision-4 gate (agenttmpl.ValidateModel),
	// the same single-token/≤100-char rule the per-user Worker model, the judge model, and
	// template models use. Applies to EVERY target (a run's model is orthogonal to what it
	// works on), so it is validated here, above the per-target switch, and rejected in no
	// case arm. A blank/whitespace value means inherit — normalized to nil so a cleared
	// control clears the stored model; a malformed token is a 400.
	if n.Model != nil {
		normalized, err := agenttmpl.ValidateModel(*n.Model)
		if err != nil {
			return n, http.StatusBadRequest, "model: " + err.Error()
		}
		if normalized == "" {
			n.Model = nil
		} else {
			n.Model = &normalized
		}
	}

	switch n.Target {
	case "issue":
		if n.IssueIID == nil || *n.IssueIID <= 0 {
			return n, http.StatusBadRequest, "issue_iid is required and must be a positive integer for an issue schedule"
		}
		if n.MaxIssues != nil {
			return n, http.StatusBadRequest, "max_issues is only valid for a sweep schedule"
		}
	case "sweep":
		// Labels are optional; an empty selector defaults to the PRD label at fire time
		// (Decisions 7/9). Drop blanks so a stored [""] cannot defeat that.
		n.Labels = nonBlankLabels(n.Labels)
	case "prompt":
		if strings.TrimSpace(n.Prompt) == "" {
			return n, http.StatusBadRequest, "prompt is required for a prompt schedule"
		}
		if len(n.Prompt) > workersvc.MaxIssueDescriptionBytes {
			return n, http.StatusUnprocessableEntity, "prompt is too large to run"
		}
		if n.MaxIssues != nil {
			return n, http.StatusBadRequest, "max_issues is only valid for a sweep schedule"
		}
		// Guidance steers HOW an issue/sweep run approaches a task; a prompt schedule
		// carries its own free-form text, so guidance is out of scope there (PRD #274 M3,
		// Out of scope). A blank was already normalized to nil above, so only a real value
		// reaches here.
		if n.Guidance != nil {
			return n, http.StatusBadRequest, "guidance is not valid for a prompt schedule"
		}
	case "self_improve":
		// A self_improve schedule is catalog-enable-only: a DIRECT create (POST /schedules)
		// stays rejected (defense in depth), but a user-origin CLONE (CloneSchedule) is a valid
		// row the owner may reconfigure, so its config PATCH (which routes through this validator)
		// must be accepted. allowSelfImprove is true only on the patch/merge caller.
		// (PRD #590 follow-up, item 1.)
		if !allowSelfImprove {
			return n, http.StatusBadRequest, "target must be one of: issue, sweep, prompt"
		}
		// A self_improve row carries only cadence/model — no issue_iid/labels/prompt and no
		// sweep cap (mirrors the issue/prompt arms rejecting a stray max_issues).
		if n.MaxIssues != nil {
			return n, http.StatusBadRequest, "max_issues is only valid for a sweep schedule"
		}
		// Item 2: a self_improve run is ALWAYS auto-approved (CreateSelfImproveRun hardcodes
		// auto_approve=true, selfimprove.sql). Force the stored flag true so it can never
		// misrepresent as manual-approve; do not honor a user-set false.
		approve := true
		n.AutoApprove = &approve
	default:
		return n, http.StatusBadRequest, "target must be one of: issue, sweep, prompt"
	}

	switch n.Timing {
	case "recurring":
		if err := schedsvc.ValidateCron(n.CronExpr); err != nil {
			return n, http.StatusBadRequest, "invalid cron expression"
		}
		if _, err := time.LoadLocation(n.Timezone); err != nil {
			return n, http.StatusBadRequest, "invalid timezone"
		}
	case "once":
		if n.RunAt == nil {
			return n, http.StatusBadRequest, "run_at is required for a one-time schedule"
		}
		if _, err := schedsvc.OnceFire(*n.RunAt, now); err != nil {
			return n, http.StatusUnprocessableEntity, "run_at must be in the future"
		}
	default:
		return n, http.StatusBadRequest, "timing must be one of: once, recurring"
	}
	return n, 0, ""
}

// CreateSchedule creates an owner-scoped schedule on a repo the caller owns
// (POST /api/repos/{id}/schedules). The repo ownership check is repoForRequest's
// GetRepoForUser (404 for a foreign/absent repo); the config is validated by the pure
// validateScheduleConfig, and next_fire_at is computed from the timing before insert.
func (h *Handler) CreateSchedule(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.repoForRequest(w, r)
	if !ok {
		return
	}
	user, _ := mw.UserFromContext(r.Context())

	var req apitypes.ScheduleRequest
	if err := httpx.DecodeJSONLimited(w, r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	applyCreateDefaults(&req)

	m, status, msg := validateScheduleConfig(req, h.clock(), false)
	if status != 0 {
		httpx.Error(w, status, msg)
		return
	}
	// sibling_group_id is create-only (PRD #636 Decision 4): a multi-repo fan-out passes the
	// same client-generated uuid in each of its N creates so the rows share a group; a
	// single-repo create sends none. A malformed value is a 400 (not a 500 from the insert).
	siblingGroup, ok := parseSiblingGroupColumn(w, req.SiblingGroupID)
	if !ok {
		return
	}
	nextFire, err := nextFireFor(m, h.clock())
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "could not compute the next fire time")
		return
	}
	issueIID, labels, prompt, cronExpr, runAt := scheduleColumns(m)
	s, err := h.q.CreateRunSchedule(r.Context(), store.CreateRunScheduleParams{
		UserID:                user.ID,
		RepoID:                repo.ID,
		Target:                m.Target,
		IssueIid:              issueIID,
		Labels:                labels,
		Prompt:                prompt,
		Timing:                m.Timing,
		CronExpr:              cronExpr,
		RunAt:                 runAt,
		Timezone:              m.Timezone,
		NextFireAt:            nextFire,
		AutoApprove:           *m.AutoApprove,
		WaitOnLimit:           *m.WaitOnLimit,
		Enabled:               *m.Enabled,
		MaxIssues:             maxIssuesColumn(m),
		Guidance:              guidanceColumn(m),
		Model:                 modelColumn(m),
		OverrideSubagentModel: overrideSubagentModelColumn(m),
		SiblingGroupID:        siblingGroup,
	})
	if err != nil {
		slog.Error("create run schedule", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusCreated, h.scheduleDTO(s, repo.PathWithNamespace))
}

// ListMySchedules lists the caller's schedules, newest first
// (GET /api/me/schedules). repo_path is resolved best-effort per row (an N+1 over a
// small owner-scoped list) and left "" when the repo can no longer be resolved.
func (h *Handler) ListMySchedules(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	rows, err := h.q.ListRunSchedulesForUser(r.Context(), user.ID)
	if err != nil {
		slog.Error("list run schedules", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]apitypes.ScheduleDTO, 0, len(rows))
	for _, s := range rows {
		out = append(out, h.scheduleDTO(s, h.repoPathFor(r.Context(), s)))
	}
	httpx.JSON(w, http.StatusOK, out)
}

// GetSchedule returns one owner-scoped schedule (GET /api/schedules/{id}); a foreign
// or absent id is a 404.
func (h *Handler) GetSchedule(w http.ResponseWriter, r *http.Request) {
	user, id, ok := h.scheduleParam(w, r)
	if !ok {
		return
	}
	s, err := h.q.GetRunScheduleForUser(r.Context(), store.GetRunScheduleForUserParams{ID: id, UserID: user.ID})
	if err != nil {
		httpx.Error(w, http.StatusNotFound, "schedule not found")
		return
	}
	httpx.JSON(w, http.StatusOK, h.scheduleDTO(s, h.repoPathFor(r.Context(), s)))
}

// PatchSchedule merges the provided fields over the current schedule (pointer/empty =
// keep), re-validates the merged config, recomputes next_fire_at, and persists
// (PATCH /api/schedules/{id}). When only `enabled` is sent the config UPDATE is
// skipped and just the pause/resume flag flips.
func (h *Handler) PatchSchedule(w http.ResponseWriter, r *http.Request) {
	user, id, ok := h.scheduleParam(w, r)
	if !ok {
		return
	}
	var req apitypes.ScheduleRequest
	if err := httpx.DecodeJSONLimited(w, r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	cur, err := h.q.GetRunScheduleForUser(r.Context(), store.GetRunScheduleForUserParams{ID: id, UserID: user.ID})
	if err != nil {
		httpx.Error(w, http.StatusNotFound, "schedule not found")
		return
	}

	final := cur
	if cur.Origin == "default" && !onlyEnabled(req) {
		// A default-origin row (PRD #589 M2) is editable ONLY in its catalog-inherited
		// editable fields; its prompt/labels/guidance/target/repo/timing are catalog-owned.
		// A distinct branch keeps the user-row path below byte-for-byte unchanged.
		updated, done := h.patchDefaultScheduleConfig(w, r, user, id, cur, req)
		if !done {
			return
		}
		final = updated
	} else if !onlyEnabled(req) {
		merged := mergeSchedule(cur, req)
		repoID, repoint, status, msg := scheduleRepoChange(cur, merged)
		if status != 0 {
			httpx.Error(w, status, msg)
			return
		}
		if repoint {
			// Ownership mirror (create parity, PRD #344 D3): 404 on a foreign/absent repo.
			// No enabled/guardrail/blocked gate is added here — those run at fire time.
			if _, rerr := h.q.GetRepoForUser(r.Context(), store.GetRepoForUserParams{ID: repoID, UserID: user.ID}); rerr != nil {
				httpx.Error(w, http.StatusNotFound, "repo not found")
				return
			}
		}
		// self_improve is catalog-enable-only (defense in depth): a config PATCH may edit an
		// EXISTING self_improve row (a catalog-enabled default or a clone of one) but must not
		// CONVERT an issue/sweep/prompt row into one — that would be a create-by-patch the
		// direct POST path deliberately blocks. So allow the self_improve arm only when the row
		// is already self_improve (PRD #590 follow-up, item 1).
		m, status, msg := validateScheduleConfig(merged, h.clock(), cur.Target == "self_improve")
		if status != 0 {
			httpx.Error(w, status, msg)
			return
		}
		nextFire, err := nextFireFor(m, h.clock())
		if err != nil {
			httpx.Error(w, http.StatusBadRequest, "could not compute the next fire time")
			return
		}
		issueIID, labels, prompt, cronExpr, runAt := scheduleColumns(m)
		final, err = h.q.UpdateRunSchedule(r.Context(), store.UpdateRunScheduleParams{
			Target:                m.Target,
			RepoID:                repoID,
			IssueIid:              issueIID,
			Labels:                labels,
			Prompt:                prompt,
			Timing:                m.Timing,
			CronExpr:              cronExpr,
			RunAt:                 runAt,
			Timezone:              m.Timezone,
			NextFireAt:            nextFire,
			AutoApprove:           *m.AutoApprove,
			WaitOnLimit:           *m.WaitOnLimit,
			MaxIssues:             maxIssuesColumn(m),
			Guidance:              guidanceColumn(m),
			Model:                 modelColumn(m),
			OverrideSubagentModel: overrideSubagentModelColumn(m),
			// A user-origin row is never customized (that flag is default-only); preserve
			// the stored value (always false here) so this write never sets it.
			Customized: cur.Customized,
			ID:         id,
			UserID:     user.ID,
		})
		if err != nil {
			if isUniqueViolation(err) {
				// Repoint onto a repo already occupied by another sibling in the same group
				// hits the partial unique index (PRD #636 Decision 10). Mirror AddScheduleRepo's
				// 23505→409 mapping rather than surface it as a generic 500.
				httpx.Error(w, http.StatusConflict, "another repo in this schedule's group already uses that repo")
				return
			}
			slog.Error("update run schedule", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
	}

	if req.Enabled != nil {
		// On an enabled-only resume of a recurring schedule (issue #396), re-arm next_fire_at
		// to the next FUTURE cron occurrence in the SAME write that flips enabled, so a
		// schedule paused past one or more fire times does not immediately replay the missed
		// window on resume. rearmed gates the atomic path; every other case (combined
		// config+enabled PATCH, a `once` resume, or any pause) keeps the plain flip below.
		rearmed := false
		if onlyEnabled(req) && *req.Enabled && cur.Timing == "recurring" {
			next, nfErr := schedsvc.NextFire(cur.CronExpr.String, cur.Timezone, h.clock())
			if nfErr != nil {
				// Defense-in-depth: a valid recurring cron should never fail here. Degrade
				// rather than crash (mirrors scheduler.go's compute-next-fire path) — log and
				// fall through to the plain enabled flip so the resume still succeeds.
				slog.Error("resume recompute next fire", "error", nfErr)
			} else {
				final, err = h.q.ResumeRecurringSchedule(r.Context(), store.ResumeRecurringScheduleParams{
					Enabled:    true,
					NextFireAt: pgtype.Timestamptz{Time: next, Valid: true},
					ID:         id,
					UserID:     user.ID,
				})
				if err != nil {
					slog.Error("resume recurring schedule", "error", err)
					httpx.Error(w, http.StatusInternalServerError, "internal error")
					return
				}
				rearmed = true
			}
		}
		if !rearmed {
			final, err = h.q.SetRunScheduleEnabled(r.Context(), store.SetRunScheduleEnabledParams{Enabled: *req.Enabled, ID: id, UserID: user.ID})
			if err != nil {
				slog.Error("set run schedule enabled", "error", err)
				httpx.Error(w, http.StatusInternalServerError, "internal error")
				return
			}
		}
	}
	httpx.JSON(w, http.StatusOK, h.scheduleDTO(final, h.repoPathFor(r.Context(), final)))
}

// DeleteSchedule removes an owner-scoped schedule (DELETE /api/schedules/{id}); 0 rows
// affected (foreign/absent id) is a 404, otherwise 204.
func (h *Handler) DeleteSchedule(w http.ResponseWriter, r *http.Request) {
	user, id, ok := h.scheduleParam(w, r)
	if !ok {
		return
	}
	// Read the row first so a successful delete can run the group hygiene below (DeleteRunSchedule
	// is execrows and returns no row). A miss here is the same 404 the delete would report.
	cur, gerr := h.q.GetRunScheduleForUser(r.Context(), store.GetRunScheduleForUserParams{ID: id, UserID: user.ID})
	if gerr != nil {
		httpx.Error(w, http.StatusNotFound, "schedule not found")
		return
	}
	n, err := h.q.DeleteRunSchedule(r.Context(), store.DeleteRunScheduleParams{ID: id, UserID: user.ID})
	if err != nil {
		slog.Error("delete run schedule", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if n == 0 {
		httpx.Error(w, http.StatusNotFound, "schedule not found")
		return
	}
	// Delete hygiene (PRD #636 Decision 3): if the deleted row was a grouped sibling and the
	// group now has exactly one live member, clear that survivor's group id so it renders as a
	// standalone row. Best-effort — the load-bearing guarantee is M3's view collapse — so a
	// failure here is logged, never surfaced.
	if cur.SiblingGroupID.Valid {
		if _, cerr := h.q.ClearSingletonSiblingGroup(r.Context(), store.ClearSingletonSiblingGroupParams{
			UserID:  user.ID,
			GroupID: cur.SiblingGroupID,
		}); cerr != nil {
			slog.Error("clear singleton sibling group", "group", uuid.UUID(cur.SiblingGroupID.Bytes).String(), "error", cerr)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// RunScheduleNow fires an owner-scoped schedule immediately through the shared seam
// (POST /api/schedules/{id}/run-now), WITHOUT advancing or parking it — a manual extra
// fire must not disturb the recurring cadence. It maps the scheduler's error sentinels
// to HTTP codes: repo gone → 404, malformed config → 400, transient → 502.
func (h *Handler) RunScheduleNow(w http.ResponseWriter, r *http.Request) {
	user, id, ok := h.scheduleParam(w, r)
	if !ok {
		return
	}
	if h.scheduler == nil {
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	s, err := h.q.GetRunScheduleForUser(r.Context(), store.GetRunScheduleForUserParams{ID: id, UserID: user.ID})
	if err != nil {
		httpx.Error(w, http.StatusNotFound, "schedule not found")
		return
	}
	out, err := h.scheduler.RunNow(r.Context(), s)
	if err != nil {
		switch {
		case errors.Is(err, workersvc.ErrRepoNotFound):
			httpx.Error(w, http.StatusNotFound, "the schedule's repo is disconnected or no longer owned by you")
		case errors.Is(err, schedsvc.ErrBadConfig):
			httpx.Error(w, http.StatusBadRequest, "the schedule's configuration is invalid")
		default:
			slog.Error("run schedule now", "schedule", s.ID.String(), "error", err)
			httpx.Error(w, http.StatusBadGateway, "could not fire the schedule")
		}
		return
	}
	httpx.JSON(w, http.StatusAccepted, runNowResponse(out))
}

// runNowResponse maps a FireOutcome onto the run-now wire shape (PRD #308 M3). It is pure
// (no clock, no DB) so it is unit-tested directly against a constructed outcome. Created /
// RunIDs are retained for back-compat and derived from Started; Started/Skips are non-nil
// empty slices, matching the persisted last_fire convention. RunNow does NOT persist the
// outcome — that invariant is pinned in schedsvc's TestRunNowDoesNotPersistLastFire.
func runNowResponse(out schedsvc.FireOutcome) apitypes.RunNowResponse {
	started := make([]apitypes.LastFireStarted, 0, len(out.Started))
	runIDs := make([]string, 0, len(out.Started))
	for _, s := range out.Started {
		id := s.RunID.String()
		runIDs = append(runIDs, id)
		started = append(started, apitypes.LastFireStarted{
			IssueIID: s.IssueIID,
			RunID:    id,
			Title:    s.Title,
		})
	}
	skips := make([]apitypes.LastFireSkip, 0, len(out.Skips))
	for _, s := range out.Skips {
		skips = append(skips, apitypes.LastFireSkip{
			IssueIID: s.IssueIID,
			Title:    s.Title,
			Reason:   string(s.Reason),
		})
	}
	return apitypes.RunNowResponse{
		Created: len(started),
		RunIDs:  runIDs,
		Matched: out.Matched,
		Capped:  out.Capped,
		Started: started,
		Skips:   skips,
	}
}

// PreviewSchedule computes a live "next fires" preview from a timing spec that need not
// correspond to a stored schedule (POST /api/schedules/preview). N is clamped to
// [1,schedulePreviewCap] (default 3). Invalid cron/tz is a 400.
func (h *Handler) PreviewSchedule(w http.ResponseWriter, r *http.Request) {
	var req apitypes.SchedulePreviewRequest
	if err := httpx.DecodeJSONLimited(w, r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	n := req.N
	if n <= 0 {
		n = 3
	}
	if n > schedulePreviewCap {
		n = schedulePreviewCap
	}
	tz := strings.TrimSpace(req.Timezone)
	if tz == "" {
		tz = "UTC"
	}
	switch req.Timing {
	case "recurring":
		fires, err := schedsvc.NextFires(req.CronExpr, tz, h.clock(), n)
		if err != nil {
			httpx.Error(w, http.StatusBadRequest, "invalid cron expression or timezone")
			return
		}
		httpx.JSON(w, http.StatusOK, apitypes.SchedulePreviewResponse{Fires: fires})
	case "once":
		if req.RunAt == nil {
			httpx.Error(w, http.StatusBadRequest, "run_at is required for a one-time schedule")
			return
		}
		httpx.JSON(w, http.StatusOK, apitypes.SchedulePreviewResponse{Fires: []time.Time{req.RunAt.UTC()}})
	default:
		httpx.Error(w, http.StatusBadRequest, "timing must be one of: once, recurring")
	}
}

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
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	tz := catalogTimezone(job)
	if override := strings.TrimSpace(req.Timezone); override != "" {
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
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
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
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
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
		customized = defaultEditableDiverges(job, cron, tz, model, autoApprove, waitOnLimit, maxIssues)
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

// ── helpers ──────────────────────────────────────────────────────────────────

// scheduleParam pulls the authenticated user and the {id} path param, answering the
// standard 401/400 before a handler touches the store.
func (h *Handler) scheduleParam(w http.ResponseWriter, r *http.Request) (store.User, uuid.UUID, bool) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return store.User{}, uuid.Nil, false
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid schedule id")
		return store.User{}, uuid.Nil, false
	}
	return user, id, true
}

// applyCreateDefaults fills the create-time defaults for the three tri-state flags:
// auto_approve ON (PRD #241 Decision 4), wait_on_limit ON (PRD #274 Decision 1a — a
// schedule is unattended, so a fired run should park on the usage limit rather than die),
// enabled on. A nil pointer means the caller omitted the field; a present pointer (even to
// false) is respected. Create-time only: existing rows are not rewritten (PRD #274
// Decision 4).
//
// max_issues (PRD #274 M2) defaults to 10 for a NEW SWEEP with no explicit value — a
// bounded fan-out is the safer default (Decision 2/4). It is left nil for issue/prompt
// targets, where max_issues is not a valid field (validateScheduleConfig rejects it there).
func applyCreateDefaults(req *apitypes.ScheduleRequest) {
	if req.AutoApprove == nil {
		v := true
		req.AutoApprove = &v
	}
	if req.WaitOnLimit == nil {
		v := true
		req.WaitOnLimit = &v
	}
	if req.Enabled == nil {
		v := true
		req.Enabled = &v
	}
	if req.Target == "sweep" && req.MaxIssues == nil {
		v := 10
		req.MaxIssues = &v
	}
}

// onlyEnabled reports whether a PATCH body carries ONLY the enabled flag, so the
// handler can flip pause/resume without re-validating (and re-writing) the whole config.
func onlyEnabled(req apitypes.ScheduleRequest) bool {
	return req.Enabled != nil &&
		req.Target == "" && req.Timing == "" &&
		req.RepoID == "" &&
		req.IssueIID == nil && req.Labels == nil && req.Prompt == "" &&
		req.CronExpr == "" && req.RunAt == nil && req.Timezone == "" &&
		req.AutoApprove == nil && req.WaitOnLimit == nil && req.MaxIssues == nil &&
		req.Guidance == nil && req.Model == nil && req.OverrideSubagentModel == nil
}

// mergeSchedule overlays the provided PATCH fields onto the current stored schedule,
// producing the full ScheduleRequest to re-validate. Pointer/empty provided fields keep
// the current value; the three flags always come back non-nil (seeded from cur) so the
// caller can dereference them.
func mergeSchedule(cur store.RunSchedule, req apitypes.ScheduleRequest) apitypes.ScheduleRequest {
	autoApprove := cur.AutoApprove
	waitOnLimit := cur.WaitOnLimit
	enabled := cur.Enabled
	m := apitypes.ScheduleRequest{
		Target:      cur.Target,
		Labels:      scheduleLabelsToSlice(cur.Labels),
		Timing:      cur.Timing,
		Timezone:    cur.Timezone,
		AutoApprove: &autoApprove,
		WaitOnLimit: &waitOnLimit,
		Enabled:     &enabled,
	}
	if cur.IssueIid.Valid {
		v := cur.IssueIid.Int64
		m.IssueIID = &v
	}
	if cur.Prompt.Valid {
		m.Prompt = cur.Prompt.String
	}
	if cur.CronExpr.Valid {
		m.CronExpr = cur.CronExpr.String
	}
	if cur.RunAt.Valid {
		t := cur.RunAt.Time
		m.RunAt = &t
	}

	if req.Target != "" {
		m.Target = req.Target
	}
	if req.Timing != "" {
		m.Timing = req.Timing
	}
	if req.IssueIID != nil {
		m.IssueIID = req.IssueIID
	}
	if req.Labels != nil {
		m.Labels = req.Labels
	}
	if req.Prompt != "" {
		m.Prompt = req.Prompt
	}
	if req.CronExpr != "" {
		m.CronExpr = req.CronExpr
	}
	if req.RunAt != nil {
		m.RunAt = req.RunAt
	}
	if req.Timezone != "" {
		m.Timezone = req.Timezone
	}
	if req.AutoApprove != nil {
		m.AutoApprove = req.AutoApprove
	}
	if req.WaitOnLimit != nil {
		m.WaitOnLimit = req.WaitOnLimit
	}
	// max_issues (PRD #274 M2) takes the request value DIRECTLY — it is NOT seeded from
	// cur and kept-on-empty like the fields above. RATIONALE: a config PATCH rewrites the
	// whole row, and the only sparse PATCH (enabled-only) is short-circuited by onlyEnabled
	// before this merge, so the web always sends the full config. Replace-semantics is what
	// makes clear-to-unlimited work: a `max_issues: null` in the PATCH must reach the DB as
	// NULL. A seed-and-keep would make an explicit null indistinguishable from "omitted",
	// leaving unlimited unreachable once a sweep has any cap set. This is the deliberate
	// difference from the keep-on-empty fields — the *int tri-state carries "cleared".
	m.MaxIssues = req.MaxIssues
	// guidance (PRD #274 M3) takes the request value DIRECTLY for the same reason as
	// max_issues above: a config PATCH rewrites the whole row (the enabled-only sparse PATCH
	// is short-circuited by onlyEnabled), so the web sends the full config. Replace-semantics
	// is what makes clear-to-none work — a `guidance: null` (or a cleared textarea, which
	// validateScheduleConfig normalizes to nil) must reach the DB as NULL. A seed-and-keep
	// would make an explicit clear indistinguishable from "omitted".
	m.Guidance = req.Guidance
	// model (PRD #300) takes the request value DIRECTLY, same replace-semantics as
	// max_issues/guidance above: the config PATCH rewrites the whole row (enabled-only is
	// short-circuited by onlyEnabled), so a `model: null` (or a cleared control) must reach
	// the DB as NULL = inherit. A seed-and-keep would make an explicit clear indistinguishable
	// from omitted.
	m.Model = req.Model
	// override_subagent_model (PRD #305) takes the request value DIRECTLY, same
	// replace-semantics as model/guidance/max_issues above: the config PATCH rewrites the
	// whole row (enabled-only is short-circuited by onlyEnabled), so nil ≡ false (Decision 5)
	// means an omitted value turns it off — which the web avoids by sending the full config.
	m.OverrideSubagentModel = req.OverrideSubagentModel
	// repo_id (Feature A, PRD #344) is seeded from the CURRENT row and overridden only by
	// a non-empty request value — keep-on-empty, deliberately NOT the replace-semantics of
	// max_issues/guidance/model above. RATIONALE: run_schedules.repo_id is NOT NULL + FK, and
	// every config edit flows through UpdateRunSchedule. If a config PATCH that omits --repo
	// let repo_id fall to "" -> uuid.Nil, the UPDATE would SET repo_id = '00000000-...' and
	// violate the FK -> 500 on EVERY config edit, not just repoints (S1). So keep the stored id.
	m.RepoID = cur.RepoID.String()
	if req.RepoID != "" {
		m.RepoID = req.RepoID
	}
	return m
}

// scheduleRepoChange classifies the merged PATCH's repo_id against the current row's repo.
// It is PURE (no store) so it unit-tests the 400/422 grounds directly; the caller performs
// the store-backed ownership check (404) only when repoint is true. merged.RepoID is always
// non-empty (mergeSchedule seeds it from cur), so uuid.Parse fails only when the caller sent
// a malformed repo_id -> 400. A parsed id equal to the current repo is not a repoint. A repoint
// of an issue-target schedule is REJECTED 422 (PRD #344 D4): issue_iid is repo-relative, so the
// same IID in the new repo is a different, unrelated issue that auto_approve would run to an MR.
func scheduleRepoChange(cur store.RunSchedule, merged apitypes.ScheduleRequest) (repoID uuid.UUID, repoint bool, status int, msg string) {
	id, err := uuid.Parse(merged.RepoID)
	if err != nil {
		return uuid.Nil, false, http.StatusBadRequest, "invalid repo id"
	}
	if id == cur.RepoID {
		return id, false, 0, ""
	}
	if merged.Target == "issue" {
		return uuid.Nil, false, http.StatusUnprocessableEntity,
			"repointing an issue-target schedule is not supported; delete and recreate it against the new repo"
	}
	return id, true, 0, ""
}

// scheduleColumns maps a normalized request to the per-target nullable store columns.
// Only the columns the target/timing actually uses are set Valid; the rest stay SQL
// NULL, matching the DB's field-presence CHECK.
func scheduleColumns(m apitypes.ScheduleRequest) (issueIID pgtype.Int8, labels []byte, prompt pgtype.Text, cronExpr pgtype.Text, runAt pgtype.Timestamptz) {
	if m.Target == "issue" && m.IssueIID != nil {
		issueIID = pgtype.Int8{Int64: *m.IssueIID, Valid: true}
	}
	if m.Target == "sweep" {
		labels = marshalLabels(m.Labels)
	}
	if m.Target == "prompt" {
		prompt = pgtype.Text{String: m.Prompt, Valid: true}
	}
	if m.Timing == "recurring" {
		cronExpr = pgtype.Text{String: m.CronExpr, Valid: true}
	}
	if m.Timing == "once" && m.RunAt != nil {
		runAt = pgtype.Timestamptz{Time: m.RunAt.UTC(), Valid: true}
	}
	return
}

// maxIssuesColumn maps the request's *int max_issues to the nullable store column (PRD
// #274 M2). It is set Valid ONLY for the sweep target (mirroring labels): a non-sweep
// row must carry SQL NULL, so an issue/prompt schedule never persists a stray cap even
// if one slipped past validation. A nil pointer is SQL NULL = unlimited.
func maxIssuesColumn(m apitypes.ScheduleRequest) pgtype.Int4 {
	if m.Target == "sweep" && m.MaxIssues != nil {
		return pgtype.Int4{Int32: int32(*m.MaxIssues), Valid: true}
	}
	return pgtype.Int4{}
}

// guidanceColumn maps the request's *string guidance to the nullable store column (PRD
// #274 M3). It is set Valid ONLY for the issue/sweep targets and only for a non-blank
// value: the prompt target carries its own text (validateScheduleConfig rejects guidance
// there), and a blank was already normalized to nil, so a non-issue/sweep row never
// persists a stray guidance value. A nil pointer is SQL NULL = no guidance.
func guidanceColumn(m apitypes.ScheduleRequest) pgtype.Text {
	if (m.Target == "issue" || m.Target == "sweep") && m.Guidance != nil && strings.TrimSpace(*m.Guidance) != "" {
		return pgtype.Text{String: *m.Guidance, Valid: true}
	}
	return pgtype.Text{}
}

// modelColumn maps the request's *string model to the nullable store column (PRD #300).
// Unlike maxIssuesColumn/guidanceColumn it is NOT target-scoped — a per-schedule model
// applies to every target. validateScheduleConfig already normalized a blank to nil and
// rejected a malformed token, so a nil pointer here is SQL NULL = inherit.
func modelColumn(m apitypes.ScheduleRequest) pgtype.Text {
	if m.Model != nil && strings.TrimSpace(*m.Model) != "" {
		return pgtype.Text{String: *m.Model, Valid: true}
	}
	return pgtype.Text{}
}

// overrideSubagentModelColumn maps the request's *bool to the NOT NULL DEFAULT false
// column (PRD #305): nil (absent) ≡ false. Unlike modelColumn there is no tri-state —
// the flag is off or on (Decision 5), not target-scoped (applies to every target).
func overrideSubagentModelColumn(m apitypes.ScheduleRequest) bool {
	return m.OverrideSubagentModel != nil && *m.OverrideSubagentModel
}

// parseSiblingGroupColumn validates the optional, create-only sibling_group_id (PRD #636
// Decision 4) into the nullable store column. A nil/blank value is SQL NULL (a standalone
// row, the common single-repo case); a malformed uuid writes a 400 and returns ok=false so
// the caller stops before the insert (a bad value is a client error, not a 500).
func parseSiblingGroupColumn(w http.ResponseWriter, s *string) (pgtype.UUID, bool) {
	if s == nil || strings.TrimSpace(*s) == "" {
		return pgtype.UUID{}, true
	}
	id, err := uuid.Parse(strings.TrimSpace(*s))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid sibling_group_id")
		return pgtype.UUID{}, false
	}
	return pgtype.UUID{Bytes: id, Valid: true}, true
}

// nextFireFor computes the durable next_fire_at from a validated request: the next cron
// fire for a recurring schedule, or the normalized run_at for a once schedule.
func nextFireFor(m apitypes.ScheduleRequest, now time.Time) (pgtype.Timestamptz, error) {
	switch m.Timing {
	case "recurring":
		t, err := schedsvc.NextFire(m.CronExpr, m.Timezone, now)
		if err != nil {
			return pgtype.Timestamptz{}, err
		}
		return pgtype.Timestamptz{Time: t, Valid: true}, nil
	case "once":
		if m.RunAt == nil {
			return pgtype.Timestamptz{}, fmt.Errorf("once schedule missing run_at")
		}
		t, err := schedsvc.OnceFire(*m.RunAt, now)
		if err != nil {
			return pgtype.Timestamptz{}, err
		}
		return pgtype.Timestamptz{Time: t, Valid: true}, nil
	default:
		return pgtype.Timestamptz{}, fmt.Errorf("unknown timing %q", m.Timing)
	}
}

// repoPathFor resolves a schedule's repo path best-effort (owner-scoped), returning ""
// when the repo is gone or no longer owned so the DTO can still render.
func (h *Handler) repoPathFor(ctx context.Context, s store.RunSchedule) string {
	repo, err := h.q.GetRepoForUser(ctx, store.GetRepoForUserParams{ID: s.RepoID, UserID: s.UserID})
	if err != nil {
		return ""
	}
	return repo.PathWithNamespace
}

// scheduleDTO builds the response view, computing the next-fires preview (up to 3) for
// a recurring schedule from the same cron logic the modal preview uses.
func (h *Handler) scheduleDTO(s store.RunSchedule, repoPath string) apitypes.ScheduleDTO {
	dto := apitypes.ScheduleDTO{
		ID:          s.ID.String(),
		RepoID:      s.RepoID.String(),
		RepoPath:    repoPath,
		Target:      s.Target,
		Labels:      scheduleLabelsToSlice(s.Labels),
		Prompt:      s.Prompt.String,
		Timing:      s.Timing,
		CronExpr:    s.CronExpr.String,
		Timezone:    s.Timezone,
		AutoApprove: s.AutoApprove,
		WaitOnLimit: s.WaitOnLimit,
		Enabled:     s.Enabled,
		Status:      s.Status,
		Origin:      s.Origin,
		Customized:  s.Customized,
		CreatedAt:   s.CreatedAt.Time,
		UpdatedAt:   s.UpdatedAt.Time,
	}
	if s.CatalogSlug.Valid && s.CatalogSlug.String != "" {
		v := s.CatalogSlug.String
		dto.CatalogSlug = &v
	}
	// sibling_group_id (PRD #636): a display-only group tag, surfaced as a uuid string only
	// when the row is grouped (nil for a standalone row).
	if s.SiblingGroupID.Valid {
		v := uuid.UUID(s.SiblingGroupID.Bytes).String()
		dto.SiblingGroupID = &v
	}
	// Default-origin resolution (PRD #589 Decision 2): a default row stores NULL
	// prompt/labels/guidance and carries them in the builtin catalog, resolved by
	// catalog_slug. Surface the RESOLVED values here (never persisted) so the modal can show
	// the read-only baked prompt / selector. A gone slug leaves the (empty) columns as-is.
	if s.Origin == "default" && s.CatalogSlug.Valid {
		if job, ok := schedtmpl.BySlug(s.CatalogSlug.String); ok {
			dto.Prompt = job.Prompt
			dto.Labels = job.Labels
			g := job.Guidance
			// Owner-guidance overlay (issue #675): for a SWEEP default the catalog guidance is
			// the BAKED value shown read-only; Guidance is reserved for the owner overlay,
			// populated below from the stored column. For a prompt/other default the baked value
			// still travels through Guidance as before.
			if s.Target == "sweep" {
				dto.BakedGuidance = &g
			} else {
				dto.Guidance = &g
			}
		}
	}
	if s.IssueIid.Valid {
		v := s.IssueIid.Int64
		dto.IssueIID = &v
	}
	if s.RunAt.Valid {
		t := s.RunAt.Time
		dto.RunAt = &t
	}
	if s.NextFireAt.Valid {
		t := s.NextFireAt.Time
		dto.NextFireAt = &t
	}
	if s.LastFiredAt.Valid {
		t := s.LastFiredAt.Time
		dto.LastFiredAt = &t
	}
	if s.MaxIssues.Valid {
		v := int(s.MaxIssues.Int32)
		dto.MaxIssues = &v
	}
	if s.Guidance.Valid && s.Guidance.String != "" {
		v := s.Guidance.String
		dto.Guidance = &v
	}
	if s.Model.Valid && s.Model.String != "" {
		v := s.Model.String
		dto.Model = &v
	}
	// override_subagent_model is a plain bool column (never NULL, PRD #305), so always set it.
	ov := s.OverrideSubagentModel
	dto.OverrideSubagentModel = &ov
	// last_fire is the persisted jsonb summary of the most recent fire (PRD #308 M3). NULL
	// or empty ⇒ never fired, leave nil. A malformed payload is logged and left nil — a
	// bad summary must never fail the whole DTO.
	if len(s.LastFire) > 0 {
		var lf apitypes.LastFire
		if err := json.Unmarshal(s.LastFire, &lf); err != nil {
			slog.Error("unmarshal schedule last_fire", "schedule", s.ID.String(), "error", err)
		} else {
			dto.LastFire = &lf
		}
	}
	if s.Timing == "recurring" && s.CronExpr.Valid {
		if fires, err := schedsvc.NextFires(s.CronExpr.String, s.Timezone, h.clock(), 3); err == nil {
			dto.NextFires = fires
		}
	}
	return dto
}

// scheduleLabelsToSlice decodes the stored jsonb label selector into a string slice,
// nil on empty or a non-array value (which the writer never produces).
func scheduleLabelsToSlice(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	var out []string
	if err := json.Unmarshal(b, &out); err != nil {
		return nil
	}
	return out
}

// marshalLabels encodes a label selector to jsonb bytes, or nil (SQL NULL) when empty
// so the fire-time default-to-PRD-label path applies.
func marshalLabels(labels []string) []byte {
	if len(labels) == 0 {
		return nil
	}
	b, _ := json.Marshal(labels)
	return b
}

// catalogEntryDTO maps a builtin default job to its wire view (PRD #589 M2). auto_approve
// and wait_on_limit are the fixed schedtmpl run flags, not per-entry.
func catalogEntryDTO(j schedtmpl.DefaultJob) apitypes.CatalogEntryDTO {
	return apitypes.CatalogEntryDTO{
		Slug:        j.Slug,
		Name:        j.Name,
		Description: j.Description,
		Target:      j.Target,
		Cron:        j.Cron,
		Timezone:    catalogTimezone(j),
		Model:       j.Model,
		Prompt:      j.Prompt,
		Labels:      j.Labels,
		Guidance:    j.Guidance,
		MaxIssues:   j.MaxIssues,
		AutoApprove: schedtmpl.AutoApprove,
		WaitOnLimit: schedtmpl.WaitOnLimit,
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
	if j.Target == "sweep" && j.MaxIssues > 0 {
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
// catalog defaults (PRD #589 M2). It compares the six editable fields; the catalog-owned
// prompt/labels/guidance are excluded (they are never stored on the row). A blank catalog
// model and a NULL row model both mean "inherit", so they compare equal; a 0 catalog
// max_issues and a NULL row max_issues both mean "unlimited".
func defaultEditableDiverges(job schedtmpl.DefaultJob, cron, tz string, model pgtype.Text, autoApprove, waitOnLimit bool, maxIssues pgtype.Int4) bool {
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
	jMax := int32(0)
	if job.Target == "sweep" && job.MaxIssues > 0 {
		jMax = int32(job.MaxIssues)
	}
	rMax := int32(0)
	if maxIssues.Valid {
		rMax = maxIssues.Int32
	}
	return rMax != jMax
}

// nonBlankLabels trims and drops blank entries from a label selector.
func nonBlankLabels(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if t := strings.TrimSpace(s); t != "" {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

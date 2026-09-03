package handler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/schedsvc"
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
		httpx.RespondDecodeError(w, err, "invalid request body")
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
		MrReworkEnabled:       optBoolToPgtype(m.MrReworkEnabled),
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
		httpx.RespondDecodeError(w, err, "invalid request body")
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
			MrReworkEnabled:       optBoolToPgtype(m.MrReworkEnabled),
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
		httpx.RespondDecodeError(w, err, "invalid request body")
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

// ── helpers ──────────────────────────────────────────────────────────────────

// scheduleParam pulls the authenticated user and the {id} path param, answering the
// standard 401/400 before a handler touches the store.
func (h *Handler) scheduleParam(w http.ResponseWriter, r *http.Request) (store.User, uuid.UUID, bool) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return store.User{}, uuid.Nil, false
	}
	id, ok := httpx.PathUUID(w, r, "id", "schedule")
	if !ok {
		return store.User{}, uuid.Nil, false
	}
	return user, id, true
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

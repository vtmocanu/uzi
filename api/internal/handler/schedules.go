package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/agenttmpl"
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

// validateScheduleConfig is the PURE (no store, no clock beyond the passed-in `now`)
// validator behind create/patch/preview, extracted so it is unit-testable without a
// DB. It normalizes the request — a blank timezone becomes "UTC", sweep labels are
// stripped of blanks — and returns the normalized request plus an HTTP status/message
// (status 0 == valid). It never touches the forge or the store; the per-target field
// invariants it enforces mirror the DB CHECK the row would otherwise fail on insert.
func validateScheduleConfig(req apitypes.ScheduleRequest, now time.Time) (apitypes.ScheduleRequest, int, string) {
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

	m, status, msg := validateScheduleConfig(req, h.clock())
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
	if !onlyEnabled(req) {
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
		m, status, msg := validateScheduleConfig(merged, h.clock())
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
			ID:                    id,
			UserID:                user.ID,
		})
		if err != nil {
			slog.Error("update run schedule", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
	}

	if req.Enabled != nil {
		final, err = h.q.SetRunScheduleEnabled(r.Context(), store.SetRunScheduleEnabledParams{Enabled: *req.Enabled, ID: id, UserID: user.ID})
		if err != nil {
			slog.Error("set run schedule enabled", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
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
		CreatedAt:   s.CreatedAt.Time,
		UpdatedAt:   s.UpdatedAt.Time,
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

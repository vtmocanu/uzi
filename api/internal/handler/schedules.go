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

	"gitlab.example.com/vtmocanu/uzi/api/internal/apitypes"
	"gitlab.example.com/vtmocanu/uzi/api/internal/httpx"
	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/schedsvc"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
	"gitlab.example.com/vtmocanu/uzi/api/internal/workersvc"
)

// -------------------------------------------------------------------------
// Scheduled runs (PRD #241 M4): owner-scoped CRUD + preview + run-now.
// -------------------------------------------------------------------------

// schedulePreviewCap bounds the "next fires" preview N so a client cannot ask for an
// unbounded fire computation (closes the M3-audit unbounded-N concern).
const schedulePreviewCap = 10

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

	switch n.Target {
	case "issue":
		if n.IssueIID == nil || *n.IssueIID <= 0 {
			return n, http.StatusBadRequest, "issue_iid is required and must be a positive integer for an issue schedule"
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
		UserID:      user.ID,
		RepoID:      repo.ID,
		Target:      m.Target,
		IssueIid:    issueIID,
		Labels:      labels,
		Prompt:      prompt,
		Timing:      m.Timing,
		CronExpr:    cronExpr,
		RunAt:       runAt,
		Timezone:    m.Timezone,
		NextFireAt:  nextFire,
		AutoApprove: *m.AutoApprove,
		WaitOnLimit: *m.WaitOnLimit,
		Enabled:     *m.Enabled,
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
			Target:      m.Target,
			IssueIid:    issueIID,
			Labels:      labels,
			Prompt:      prompt,
			Timing:      m.Timing,
			CronExpr:    cronExpr,
			RunAt:       runAt,
			Timezone:    m.Timezone,
			NextFireAt:  nextFire,
			AutoApprove: *m.AutoApprove,
			WaitOnLimit: *m.WaitOnLimit,
			ID:          id,
			UserID:      user.ID,
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
	ids, err := h.scheduler.RunNow(r.Context(), s)
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
	runIDs := make([]string, 0, len(ids))
	for _, x := range ids {
		runIDs = append(runIDs, x.String())
	}
	httpx.JSON(w, http.StatusAccepted, apitypes.RunNowResponse{Created: len(runIDs), RunIDs: runIDs})
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
}

// onlyEnabled reports whether a PATCH body carries ONLY the enabled flag, so the
// handler can flip pause/resume without re-validating (and re-writing) the whole config.
func onlyEnabled(req apitypes.ScheduleRequest) bool {
	return req.Enabled != nil &&
		req.Target == "" && req.Timing == "" &&
		req.IssueIID == nil && req.Labels == nil && req.Prompt == "" &&
		req.CronExpr == "" && req.RunAt == nil && req.Timezone == "" &&
		req.AutoApprove == nil && req.WaitOnLimit == nil
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
	return m
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

// This file is the single-instance background scheduler actor for PRD #241
// (scheduled runs). It is modeled on selfimprove/engine.go: a Boot() immediate
// tick plus an interval ticker, with the DURABLE due-gate being the persisted
// run_schedules.next_fire_at column (Decision 1), not an in-memory timer that
// resets on restart. A tick claims every due schedule (ClaimDueSchedules), fires
// each through the shared run-creation seam autopilot uses, then advances it.
//
// Skip-vs-advance discipline (mirrors selfimprove): a fire SKIPPED for a benign
// reason — a prior run still active, a per-fire gate the seam rejects — STILL
// advances the schedule, so there is no tick-storm and no double-fire. Only a
// TRANSIENT failure (forge/DB) leaves next_fire_at in the past for the next tick to
// retry; only a PERMANENT failure (the repo/owner is gone) parks the schedule at
// status='error', dropping it from the active claim set.
package schedsvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/forge"
	"gitlab.example.com/vtmocanu/uzi/api/internal/notifysvc"
	"gitlab.example.com/vtmocanu/uzi/api/internal/settings"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
	"gitlab.example.com/vtmocanu/uzi/api/internal/workersvc"
)

// promptTitleCap bounds the derived title of a prompt run so the run-view header is
// a short label, not the whole prompt body. The full prompt still rides into the
// run as its issue_description.
const promptTitleCap = 60

// sweepPacing is a small pause between per-issue seam calls inside one sweep fan-out
// so a wide sweep does not burst the forge/DB in a tight loop. The sweep fan-out is
// deliberately NOT behind the per-user run limiter (review N1); this pacing is the
// only backpressure on it, so keep it.
const sweepPacing = 50 * time.Millisecond

// Store is the DB surface the scheduler reads and writes. *store.Queries satisfies it.
type Store interface {
	ClaimDueSchedules(ctx context.Context) ([]store.RunSchedule, error)
	AdvanceSchedule(ctx context.Context, arg store.AdvanceScheduleParams) (store.RunSchedule, error)
	SetRunScheduleStatus(ctx context.Context, arg store.SetRunScheduleStatusParams) (store.RunSchedule, error)
	ListSweepCandidateIssues(ctx context.Context, arg store.ListSweepCandidateIssuesParams) ([]store.ListSweepCandidateIssuesRow, error)
	HasActiveRunForIssue(ctx context.Context, arg store.HasActiveRunForIssueParams) (bool, error)
	HasActiveRunForSchedule(ctx context.Context, scheduleID pgtype.UUID) (bool, error)
	GetRepoForUser(ctx context.Context, arg store.GetRepoForUserParams) (store.GetRepoForUserRow, error)
}

// RunCreator is the shared run-creation seam the scheduler fires through — the SAME
// seam autopilot and the manual board use. *workersvc.Service satisfies it.
type RunCreator interface {
	CreateRun(ctx context.Context, userID, repoID uuid.UUID, issueIID int64, description string, allowWithoutPRD bool, waitOnLimit *bool, seed *workersvc.SeededPlan) (store.Run, error)
	CreateAutopilotRun(ctx context.Context, userID, repoID uuid.UUID, issueIID int64, description string, allowWithoutPRD bool) (store.Run, error)
	CreatePromptRun(ctx context.Context, userID, repoID, scheduleID uuid.UUID, title, prompt string, autoApprove, waitOnLimit bool) (store.Run, error)
}

// ForgeBuilder builds a forge driver from a stored (encrypted) connection — the same
// seam selfimprove/autopilot use. *forgesvc.Service satisfies it.
type ForgeBuilder interface {
	ForgeForConnection(forgeType, baseURL string, tokenCiphertext []byte) (forge.Forge, error)
}

// SettingsReader is the typed settings surface the scheduler reads to compute the
// PRDLESS bypass (mirroring the poller/handler) and to default an empty sweep
// selector to the configured PRD label. *settings.Cache satisfies it.
type SettingsReader interface {
	PrdlessEnabled(ctx context.Context) (bool, error)
	PrdlessLabel(ctx context.Context) (string, error)
	PRDLabel(ctx context.Context) (string, error)
}

// Notifier is the notifysvc write seam (persist-first, best-effort Slack).
// *notifysvc.Service satisfies it — the same seam selfimprove uses. Optional: a nil
// notifier skips notifications. The scheduler uses it only to surface a permanent
// park (repo/owner gone) so a schedule going to status='error' is not silent.
type Notifier interface {
	Notify(ctx context.Context, n notifysvc.Notification) (store.Notification, error)
}

// Scheduler is the single-instance scheduled-runs actor.
type Scheduler struct {
	store    Store
	runs     RunCreator
	forge    ForgeBuilder
	settings SettingsReader
	notifier Notifier
	interval time.Duration
	now      func() time.Time
	logger   *slog.Logger
}

// New builds a Scheduler. interval is the wake cadence (how often the due-gate is
// polled), not any per-schedule cadence; a non-positive value is the caller's cue not
// to start it. notifier may be nil.
func New(st Store, runs RunCreator, fb ForgeBuilder, set SettingsReader, notifier Notifier, interval time.Duration, logger *slog.Logger) *Scheduler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Scheduler{
		store: st, runs: runs, forge: fb, settings: set, notifier: notifier,
		interval: interval, now: time.Now, logger: logger,
	}
}

// Boot runs one immediate tick at API start so schedules that came due while the API
// was down fire promptly instead of one wake-cadence later. Failures are logged per
// schedule, never fatal.
func (e *Scheduler) Boot(ctx context.Context) { e.tick(ctx) }

// Run blocks until ctx is cancelled, polling the due-gate every wake interval.
func (e *Scheduler) Run(ctx context.Context) {
	ticker := time.NewTicker(e.interval)
	defer ticker.Stop()
	e.logger.Info("scheduler started", "check_interval", e.interval.String())
	for {
		select {
		case <-ctx.Done():
			e.logger.Info("scheduler stopped")
			return
		case <-ticker.C:
			e.tick(ctx)
		}
	}
}

// tick claims every due schedule and processes each. One schedule's failure — a
// panic in a driver or a bad row — must not kill the whole tick, so each is handled
// in its own recovered closure.
func (e *Scheduler) tick(ctx context.Context) {
	scheds, err := e.store.ClaimDueSchedules(ctx)
	if err != nil {
		e.logger.Error("scheduler: claim due schedules", "error", err)
		return
	}
	for _, sched := range scheds {
		e.process(ctx, sched)
	}
}

// process fires one schedule and advances it, isolating panics so a single bad
// schedule cannot abort the tick.
func (e *Scheduler) process(ctx context.Context, sched store.RunSchedule) {
	defer func() {
		if r := recover(); r != nil {
			e.logger.Error("scheduler: panic firing schedule", "schedule", sched.ID.String(), "panic", r)
		}
	}()
	fireErr := e.fireOne(ctx, sched)
	e.advance(ctx, sched, fireErr)
}

// fireOne dispatches on the schedule target and returns:
//   - nil                        → success or a benign per-fire skip (advance the schedule)
//   - workersvc.ErrRepoNotFound  → permanent (park the schedule at status='error')
//   - any other error            → transient (do NOT advance; retry next tick)
func (e *Scheduler) fireOne(ctx context.Context, sched store.RunSchedule) error {
	switch sched.Target {
	case "issue":
		return e.fireIssue(ctx, sched)
	case "sweep":
		return e.fireSweep(ctx, sched)
	case "prompt":
		return e.firePrompt(ctx, sched)
	default:
		// A target the DB CHECK should have rejected. Park it rather than retry forever.
		e.logger.Error("scheduler: unknown target", "schedule", sched.ID.String(), "target", sched.Target)
		return workersvc.ErrRepoNotFound
	}
}

// fireIssue fires a single-issue schedule through the same seam a manual/autopilot
// start uses, computing the PRDLESS bypass from a fresh GetIssue snapshot exactly
// like the poller/handler.
func (e *Scheduler) fireIssue(ctx context.Context, sched store.RunSchedule) error {
	repo, f, err := e.resolveRepoForge(ctx, sched)
	if err != nil {
		return err
	}
	iid := sched.IssueIid.Int64

	// Benign dedup: a prior run for this issue is still live. Swallow WITHOUT firing,
	// but the schedule still advances (Decision 5-style: no queued re-runs).
	active, err := e.store.HasActiveRunForIssue(ctx, store.HasActiveRunForIssueParams{
		RepoID: repo.ID, IssueIid: pgtype.Int8{Int64: iid, Valid: true},
	})
	if err != nil {
		return err // transient DB error: retry next tick
	}
	if active {
		e.logger.Info("scheduler: issue has active run, skipping fire", "schedule", sched.ID.String(), "issue", iid)
		return nil
	}

	issue, err := f.GetIssue(ctx, repo.ForgeProjectID, iid)
	if err != nil {
		return fmt.Errorf("get issue %d: %w", iid, err) // transient forge error
	}
	return e.createIssueRun(ctx, sched, repo.ID, iid, issue.Description, issue.Labels)
}

// fireSweep resolves the label selector (an empty/NULL selector defaults to the PRD
// label — NEVER an empty jsonb array, which `labels @> '[]'` would match against
// every open issue, Decisions 7/9), lists the matching open issues, and fires each
// through the same per-issue flow as fireIssue. Per-issue failures are logged and
// skipped so one bad issue does not abort the fan-out; only a failure to resolve the
// repo/forge or to LIST is treated as transient for the whole schedule.
func (e *Scheduler) fireSweep(ctx context.Context, sched store.RunSchedule) error {
	repo, f, err := e.resolveRepoForge(ctx, sched)
	if err != nil {
		return err
	}
	labelsJSON, err := e.resolveSweepLabels(ctx, sched.Labels)
	if err != nil {
		return err
	}
	candidates, err := e.store.ListSweepCandidateIssues(ctx, store.ListSweepCandidateIssuesParams{
		RepoID: repo.ID, Labels: labelsJSON,
	})
	if err != nil {
		return err // transient DB error
	}
	e.logger.Info("scheduler: sweep fan-out (not behind the per-user run limiter, review N1)",
		"schedule", sched.ID.String(), "candidates", len(candidates))
	for _, c := range candidates {
		iid := c.ForgeIssueIid
		active, err := e.store.HasActiveRunForIssue(ctx, store.HasActiveRunForIssueParams{
			RepoID: repo.ID, IssueIid: pgtype.Int8{Int64: iid, Valid: true},
		})
		if err != nil {
			e.logger.Warn("scheduler: sweep active-run check", "schedule", sched.ID.String(), "issue", iid, "error", err)
			continue
		}
		if active {
			continue
		}
		issue, err := f.GetIssue(ctx, repo.ForgeProjectID, iid)
		if err != nil {
			e.logger.Warn("scheduler: sweep fetch issue", "schedule", sched.ID.String(), "issue", iid, "error", err)
			continue
		}
		if err := e.createIssueRun(ctx, sched, repo.ID, iid, issue.Description, issue.Labels); err != nil {
			// A permanent repo error mid-sweep is unexpected (the repo just resolved);
			// log and keep going rather than aborting the whole fan-out.
			e.logger.Warn("scheduler: sweep create run", "schedule", sched.ID.String(), "issue", iid, "error", err)
		}
		// Light pacing between seam calls (review N1: no per-user limiter guards this).
		e.sleep(sweepPacing)
	}
	return nil
}

// firePrompt fires a free-form prompt schedule: a repo-ful, issue-less prompt run
// keyed to the schedule. HasActiveRunForSchedule is the benign dedup pre-check
// alongside the uq_runs_one_active_prompt_per_schedule structural backstop.
func (e *Scheduler) firePrompt(ctx context.Context, sched store.RunSchedule) error {
	active, err := e.store.HasActiveRunForSchedule(ctx, pgUUID(sched.ID))
	if err != nil {
		return err // transient DB error
	}
	if active {
		e.logger.Info("scheduler: prompt schedule has active run, skipping fire", "schedule", sched.ID.String())
		return nil
	}
	prompt := sched.Prompt.String
	title := promptTitle(prompt)
	_, err = e.runs.CreatePromptRun(ctx, sched.UserID, sched.RepoID, sched.ID, title, prompt, sched.AutoApprove, sched.WaitOnLimit)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, workersvc.ErrActivePromptExists):
		// Raced the pre-check; the unique index did its job. Benign — advance.
		e.logger.Info("scheduler: prompt run already active (race)", "schedule", sched.ID.String())
		return nil
	case errors.Is(err, workersvc.ErrRepoNotFound):
		return workersvc.ErrRepoNotFound // permanent
	default:
		return err // transient
	}
}

// createIssueRun fires one issue through the shared seam. auto_approve schedules go
// through CreateAutopilotRun; interactive ones through CreateRun threading the
// schedule's wait_on_limit. The benign per-fire seam rejects (active run, not a PRD
// issue, no PRD link, description too large) are swallowed so the schedule still
// advances; ErrRepoNotFound is permanent; anything else is transient.
//
// Decision 2: CreateAutopilotRun uses the OWNER's wait-on-limit default (an
// auto-approve run has no human in the loop), so the schedule's wait_on_limit is
// threaded ONLY on the non-auto-approve CreateRun path. This is intentional — do not
// change the seam to thread it through the autopilot path.
func (e *Scheduler) createIssueRun(ctx context.Context, sched store.RunSchedule, repoID uuid.UUID, iid int64, description string, labels []string) error {
	allowWithoutPRD := e.allowWithoutPRD(ctx, labels)

	var err error
	if sched.AutoApprove {
		_, err = e.runs.CreateAutopilotRun(ctx, sched.UserID, repoID, iid, description, allowWithoutPRD)
	} else {
		waitOnLimit := sched.WaitOnLimit
		_, err = e.runs.CreateRun(ctx, sched.UserID, repoID, iid, description, allowWithoutPRD, &waitOnLimit, nil)
	}

	switch {
	case err == nil:
		return nil
	case errors.Is(err, workersvc.ErrActiveRunExists),
		errors.Is(err, workersvc.ErrNotPRDIssue),
		errors.Is(err, workersvc.ErrNoPRDLink),
		errors.Is(err, workersvc.ErrDescriptionTooLarge):
		// Benign per-fire skip: the schedule still advances (no tick-storm).
		e.logger.Info("scheduler: issue fire skipped", "schedule", sched.ID.String(), "issue", iid, "reason", err)
		return nil
	case errors.Is(err, workersvc.ErrRepoNotFound):
		return workersvc.ErrRepoNotFound // permanent
	default:
		return err // transient
	}
}

// resolveRepoForge loads the owner's repo and builds its forge driver. A missing repo
// (the owner disconnected it or lost ownership) is PERMANENT — the schedule can never
// fire again — so it maps to ErrRepoNotFound; any other lookup error is transient.
func (e *Scheduler) resolveRepoForge(ctx context.Context, sched store.RunSchedule) (store.GetRepoForUserRow, forge.Forge, error) {
	repo, err := e.store.GetRepoForUser(ctx, store.GetRepoForUserParams{ID: sched.RepoID, UserID: sched.UserID})
	if err != nil {
		// GetRepoForUser returns no row when the repo is gone or not owned; treat the
		// no-row case as permanent. A transient DB error retries next tick.
		if isNoRows(err) {
			return store.GetRepoForUserRow{}, nil, workersvc.ErrRepoNotFound
		}
		return store.GetRepoForUserRow{}, nil, err
	}
	f, err := e.forge.ForgeForConnection(repo.ForgeType, repo.BaseUrl, repo.TokenCiphertext)
	if err != nil {
		return store.GetRepoForUserRow{}, nil, fmt.Errorf("build forge driver: %w", err) // transient
	}
	return repo, f, nil
}

// advance applies the skip-vs-advance rule after a fire:
//   - permanent error → park at status='error' (drops it from the active claim set)
//   - transient error → log, do NOT advance (next_fire_at stays in the past → retry)
//   - success/benign  → recurring: next next_fire_at, status stays 'active';
//     once: next_fire_at NULL, status='fired'
func (e *Scheduler) advance(ctx context.Context, sched store.RunSchedule, fireErr error) {
	if fireErr != nil {
		if errors.Is(fireErr, workersvc.ErrRepoNotFound) {
			e.park(ctx, sched, "the schedule's repo is disconnected or no longer owned by you")
			return
		}
		// Transient: leave next_fire_at in the past so the next tick retries.
		e.logger.Warn("scheduler: transient fire error, will retry", "schedule", sched.ID.String(), "error", fireErr)
		return
	}

	now := e.now()
	switch sched.Timing {
	case "recurring":
		next, err := NextFire(sched.CronExpr.String, sched.Timezone, now)
		if err != nil {
			// A recurring schedule whose stored cron no longer computes a next fire can
			// never advance sanely; park it rather than tick-storm.
			e.logger.Error("scheduler: compute next fire", "schedule", sched.ID.String(), "error", err)
			e.park(ctx, sched, "the schedule's cron expression has no next fire")
			return
		}
		if _, err := e.store.AdvanceSchedule(ctx, store.AdvanceScheduleParams{
			LastFiredAt: pgTime(now),
			NextFireAt:  pgTime(next),
			Status:      "active",
			ID:          sched.ID,
		}); err != nil {
			e.logger.Error("scheduler: advance recurring", "schedule", sched.ID.String(), "error", err)
		}
	case "once":
		if _, err := e.store.AdvanceSchedule(ctx, store.AdvanceScheduleParams{
			LastFiredAt: pgTime(now),
			NextFireAt:  pgtype.Timestamptz{}, // NULL: a once schedule never fires again
			Status:      "fired",
			ID:          sched.ID,
		}); err != nil {
			e.logger.Error("scheduler: advance once", "schedule", sched.ID.String(), "error", err)
		}
	default:
		e.logger.Error("scheduler: unknown timing", "schedule", sched.ID.String(), "timing", sched.Timing)
	}
}

// park moves a schedule to status='error' (dropping it from the active claim set) and
// notifies the owner, if a notifier is wired, so a parked schedule is not silent.
func (e *Scheduler) park(ctx context.Context, sched store.RunSchedule, reason string) {
	if _, err := e.store.SetRunScheduleStatus(ctx, store.SetRunScheduleStatusParams{
		Status: "error", ID: sched.ID,
	}); err != nil {
		e.logger.Error("scheduler: park schedule", "schedule", sched.ID.String(), "error", err)
		return
	}
	e.logger.Warn("scheduler: schedule parked at error", "schedule", sched.ID.String(), "reason", reason)
	if e.notifier != nil {
		body := "A scheduled run was paused because " + reason + ". Reconnect the repo or update the schedule to resume it."
		if _, err := e.notifier.Notify(ctx, notifysvc.Notification{
			UserID:  sched.UserID,
			Kind:    "schedule_error",
			Payload: map[string]any{"title": "Scheduled run paused", "body": body, "schedule_id": sched.ID.String()},
			Slack:   &notifysvc.SlackRender{Title: "Scheduled run paused", Body: body},
		}); err != nil {
			e.logger.Warn("scheduler: notify schedule error", "schedule", sched.ID.String(), "error", err)
		}
	}
}

// allowWithoutPRD computes the PRDLESS bypass from a fresh label snapshot, mirroring
// the poller/handler exactly (PRD #22 Decision 3): enabled AND the issue carries the
// configured PRDLESS label.
func (e *Scheduler) allowWithoutPRD(ctx context.Context, labels []string) bool {
	enabled, _ := e.settings.PrdlessEnabled(ctx)
	if !enabled {
		return false
	}
	label, _ := e.settings.PrdlessLabel(ctx)
	if label == "" {
		label = settings.DefaultPrdlessLabel
	}
	return contains(labels, label)
}

// resolveSweepLabels turns the schedule's stored jsonb label selector into the jsonb
// param ListSweepCandidateIssues expects. An empty/NULL/`[]` selector defaults to a
// SINGLE-element [PRD label] — never an empty array, whose `@> '[]'` containment
// matches every open issue (Decisions 7/9). The non-empty invariant is enforced HERE.
func (e *Scheduler) resolveSweepLabels(ctx context.Context, stored []byte) ([]byte, error) {
	var sel []string
	if len(stored) > 0 {
		if err := json.Unmarshal(stored, &sel); err != nil {
			return nil, fmt.Errorf("parse sweep labels: %w", err)
		}
	}
	// Drop blanks so a stored `[""]` does not defeat the non-empty invariant.
	sel = nonBlank(sel)
	if len(sel) == 0 {
		prd, _ := e.settings.PRDLabel(ctx)
		if strings.TrimSpace(prd) == "" {
			prd = settings.DefaultPRDLabel
		}
		sel = []string{prd}
	}
	out, err := json.Marshal(sel)
	if err != nil {
		return nil, fmt.Errorf("marshal sweep labels: %w", err)
	}
	return out, nil
}

// sleep pauses for d unless ctx-less; extracted so the test can drive it to a no-op.
func (e *Scheduler) sleep(d time.Duration) {
	if d > 0 {
		time.Sleep(d)
	}
}

// ── small helpers ────────────────────────────────────────────────────────────

// promptTitle derives a short run title from the prompt body so the run-view header
// is not blank. It takes the first line, trimmed and capped to promptTitleCap runes.
func promptTitle(prompt string) string {
	t := strings.TrimSpace(prompt)
	if i := strings.IndexByte(t, '\n'); i >= 0 {
		t = strings.TrimSpace(t[:i])
	}
	if t == "" {
		return "Scheduled prompt run"
	}
	r := []rune(t)
	if len(r) > promptTitleCap {
		return strings.TrimSpace(string(r[:promptTitleCap])) + "…"
	}
	return t
}

func nonBlank(in []string) []string {
	out := in[:0]
	for _, s := range in {
		if strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}

func contains(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}

// isNoRows reports whether err signals "no such row" from the store — the marker
// that a repo lookup found nothing, i.e. the repo is gone or no longer owned.
func isNoRows(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}

// pgTime wraps a known-present time as a valid pgtype.Timestamptz.
func pgTime(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

// pgUUID wraps a known-present uuid as a valid pgtype.UUID.
func pgUUID(id uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: id, Valid: true} }

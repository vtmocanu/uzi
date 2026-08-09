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

// ErrBadConfig marks a fire failure caused by a schedule's own stored config being
// malformed (e.g. a labels selector that is valid jsonb but not a string array). Such
// a row can NEVER succeed, so advance() treats it as PERMANENT and parks the schedule
// at status='error' rather than re-failing it every tick forever (a self-inflicted
// log-storm on the owner's schedule). It is distinct from a transient forge/DB error,
// which must keep retrying. The create/edit handler (M4) validates config up front, so
// this is defense-in-depth for a row that somehow bypassed that validation.
//
// Exported so the M4 run-now handler can classify a malformed-config fire as a 400
// (a bad request against the schedule's own config), distinct from a repo-gone 404 or
// a transient 502. errBadConfig is the unexported alias the rest of this package uses.
var ErrBadConfig = errors.New("schedule config is malformed")

// errBadConfig is the package-internal name for ErrBadConfig; it is the SAME sentinel,
// so errors.Is(err, errBadConfig) and errors.Is(err, ErrBadConfig) are equivalent.
var errBadConfig = ErrBadConfig

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
	// CreateScheduledRun is the non-auto-approve scheduled path: like CreateRun but
	// without PRD #196's interactive PRD-link waiver, so a timer-fired sweep never
	// starts a link-less non-primary-eligible run (see workersvc.CreateScheduledRun).
	CreateScheduledRun(ctx context.Context, userID, repoID uuid.UUID, issueIID int64, description string, allowWithoutPRD bool, waitOnLimit *bool, seed *workersvc.SeededPlan) (store.Run, error)
	CreateAutopilotRun(ctx context.Context, userID, repoID uuid.UUID, issueIID int64, description string, allowWithoutPRD bool) (store.Run, error)
	// CreateScheduledAutopilotRun is the auto-approve scheduled path (PRD #274 Decision
	// 1a): like CreateAutopilotRun but it HONOURS the schedule's persisted wait_on_limit
	// instead of the owner default. It is a distinct method so the poller's
	// CreateAutopilotRun seam stays untouched (see workersvc.CreateScheduledAutopilotRun).
	CreateScheduledAutopilotRun(ctx context.Context, userID, repoID uuid.UUID, issueIID int64, description string, allowWithoutPRD bool, waitOnLimit *bool) (store.Run, error)
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
	// The tick path ignores the created run ids; only RunNow surfaces them.
	_, fireErr := e.fireOne(ctx, sched)
	e.advance(ctx, sched, fireErr)
}

// RunNow fires a schedule ONCE, manually, and returns the ids of the runs it created
// (issue→0/1, sweep→N, prompt→0/1; a benign dedup skip → empty). It is the seam behind
// POST /api/schedules/{id}/run-now.
//
// Unlike the tick path it does NOT advance or park the schedule: a manual extra fire
// must not disturb the recurring cadence (next_fire_at stays where the tick left it) nor
// terminate a once schedule. Errors ride up UNCHANGED — workersvc.ErrRepoNotFound (repo
// gone / not owned), ErrBadConfig (malformed stored config), or a transient forge/DB
// error — so the handler can map each to the right HTTP code.
func (e *Scheduler) RunNow(ctx context.Context, sched store.RunSchedule) ([]uuid.UUID, error) {
	return e.fireOne(ctx, sched)
}

// fireOne dispatches on the schedule target and returns the ids of the runs it created
// this fire, plus:
//   - nil                        → success or a benign per-fire skip (advance the schedule)
//   - workersvc.ErrRepoNotFound  → permanent (park the schedule at status='error')
//   - ErrBadConfig               → permanent (malformed stored config)
//   - any other error            → transient (do NOT advance; retry next tick)
func (e *Scheduler) fireOne(ctx context.Context, sched store.RunSchedule) ([]uuid.UUID, error) {
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
		return nil, workersvc.ErrRepoNotFound
	}
}

// fireIssue fires a single-issue schedule through the same seam a manual/autopilot
// start uses, computing the PRDLESS bypass from a fresh GetIssue snapshot exactly
// like the poller/handler.
func (e *Scheduler) fireIssue(ctx context.Context, sched store.RunSchedule) ([]uuid.UUID, error) {
	repo, f, err := e.resolveRepoForge(ctx, sched)
	if err != nil {
		return nil, err
	}
	iid := sched.IssueIid.Int64

	// Benign dedup: a prior run for this issue is still live. Swallow WITHOUT firing,
	// but the schedule still advances (Decision 5-style: no queued re-runs).
	active, err := e.store.HasActiveRunForIssue(ctx, store.HasActiveRunForIssueParams{
		RepoID: repo.ID, IssueIid: pgtype.Int8{Int64: iid, Valid: true},
	})
	if err != nil {
		return nil, err // transient DB error: retry next tick
	}
	if active {
		e.logger.Info("scheduler: issue has active run, skipping fire", "schedule", sched.ID.String(), "issue", iid)
		return nil, nil
	}

	issue, err := f.GetIssue(ctx, repo.ForgeProjectID, iid)
	if err != nil {
		return nil, fmt.Errorf("get issue %d: %w", iid, err) // transient forge error
	}
	return e.createIssueRun(ctx, sched, repo.ID, iid, issue.Description, issue.Labels)
}

// fireSweep resolves the label selector (an empty/NULL selector defaults to the PRD
// label — NEVER an empty jsonb array, which `labels @> '[]'` would match against
// every open issue, Decisions 7/9), lists the matching open issues, and fires each
// through the same per-issue flow as fireIssue. Per-issue failures are logged and
// skipped so one bad issue does not abort the fan-out; only a failure to resolve the
// repo/forge or to LIST is treated as transient for the whole schedule.
//
// The schedule's max_issues cap (PRD #274 M2) rides straight into the query as
// ListSweepCandidateIssuesParams.MaxIssues: a NULL/invalid cap renders an unlimited
// LIMIT (today's unbounded behaviour), and a set cap N returns the N OLDEST candidates
// (the query's ORDER BY forge_issue_iid ASC), so the fan-out is an oldest-first batch.
func (e *Scheduler) fireSweep(ctx context.Context, sched store.RunSchedule) ([]uuid.UUID, error) {
	repo, f, err := e.resolveRepoForge(ctx, sched)
	if err != nil {
		return nil, err
	}
	labelsJSON, err := e.resolveSweepLabels(ctx, sched.Labels)
	if err != nil {
		return nil, err
	}
	candidates, err := e.store.ListSweepCandidateIssues(ctx, store.ListSweepCandidateIssuesParams{
		RepoID: repo.ID, Labels: labelsJSON, MaxIssues: sched.MaxIssues,
	})
	if err != nil {
		return nil, err // transient DB error
	}
	e.logger.Info("scheduler: sweep fan-out (not behind the per-user run limiter, review N1)",
		"schedule", sched.ID.String(), "candidates", len(candidates))
	var created []uuid.UUID
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
		ids, err := e.createIssueRun(ctx, sched, repo.ID, iid, issue.Description, issue.Labels)
		if err != nil {
			// A permanent repo error mid-sweep is unexpected (the repo just resolved);
			// log and keep going rather than aborting the whole fan-out.
			e.logger.Warn("scheduler: sweep create run", "schedule", sched.ID.String(), "issue", iid, "error", err)
		}
		created = append(created, ids...)
		// Light pacing between seam calls (review N1: no per-user limiter guards this).
		e.sleep(sweepPacing)
	}
	return created, nil
}

// firePrompt fires a free-form prompt schedule: a repo-ful, issue-less prompt run
// keyed to the schedule. HasActiveRunForSchedule is the benign dedup pre-check
// alongside the uq_runs_one_active_prompt_per_schedule structural backstop.
func (e *Scheduler) firePrompt(ctx context.Context, sched store.RunSchedule) ([]uuid.UUID, error) {
	active, err := e.store.HasActiveRunForSchedule(ctx, pgUUID(sched.ID))
	if err != nil {
		return nil, err // transient DB error
	}
	if active {
		e.logger.Info("scheduler: prompt schedule has active run, skipping fire", "schedule", sched.ID.String())
		return nil, nil
	}
	prompt := sched.Prompt.String
	title := promptTitle(prompt)
	run, err := e.runs.CreatePromptRun(ctx, sched.UserID, sched.RepoID, sched.ID, title, prompt, sched.AutoApprove, sched.WaitOnLimit)
	switch {
	case err == nil:
		return []uuid.UUID{run.ID}, nil
	case errors.Is(err, workersvc.ErrActivePromptExists):
		// Raced the pre-check; the unique index did its job. Benign — advance.
		e.logger.Info("scheduler: prompt run already active (race)", "schedule", sched.ID.String())
		return nil, nil
	case errors.Is(err, workersvc.ErrRepoNotFound):
		return nil, workersvc.ErrRepoNotFound // permanent
	default:
		return nil, err // transient
	}
}

// createIssueRun fires one issue through the shared seam. auto_approve schedules go
// through CreateAutopilotRun; non-auto-approve ones through CreateScheduledRun (NOT
// the interactive CreateRun — a timer/sweep has no human at fire time, so it must not
// receive PRD #196's interactive PRD-link waiver) threading the schedule's
// wait_on_limit. The benign per-fire seam rejects (active run, not eligible, no PRD
// link, description too large) are swallowed so the schedule still advances;
// ErrRepoNotFound is permanent; anything else is transient.
//
// Wait-on-limit (PRD #274 Decision 1a, revising PRD #241 Decision 2): BOTH branches now
// thread the schedule's persisted wait_on_limit. The auto-approve branch fires through
// CreateScheduledAutopilotRun (NOT the poller's CreateAutopilotRun, whose seam stays
// untouched so label-driven autopilot keeps taking the owner default); the non-auto
// branch fires through CreateScheduledRun. So a schedule's explicit wait_on_limit takes
// effect regardless of auto_approve.
func (e *Scheduler) createIssueRun(ctx context.Context, sched store.RunSchedule, repoID uuid.UUID, iid int64, description string, labels []string) ([]uuid.UUID, error) {
	allowWithoutPRD := e.allowWithoutPRD(ctx, labels)

	waitOnLimit := sched.WaitOnLimit
	var run store.Run
	var err error
	if sched.AutoApprove {
		// CreateScheduledAutopilotRun, NOT the poller's CreateAutopilotRun: a scheduled
		// auto-approve run carries a persisted per-schedule wait_on_limit that must take
		// effect (PRD #274 Decision 1a). Leaving the poller seam untouched is a structural
		// guarantee that label-driven autopilot behaviour is unchanged.
		run, err = e.runs.CreateScheduledAutopilotRun(ctx, sched.UserID, repoID, iid, description, allowWithoutPRD, &waitOnLimit)
	} else {
		// CreateScheduledRun, NOT CreateRun: a timer-fired sweep is not the interactive
		// human click PRD #196's PRD-link waiver is scoped to, so a link-less
		// non-primary-eligible issue swept in here still needs a link or PRDLESS. Using
		// CreateRun would silently widen the scheduler past the PRD's stated invariant.
		run, err = e.runs.CreateScheduledRun(ctx, sched.UserID, repoID, iid, description, allowWithoutPRD, &waitOnLimit, nil)
	}

	switch {
	case err == nil:
		return []uuid.UUID{run.ID}, nil
	case errors.Is(err, workersvc.ErrActiveRunExists),
		errors.Is(err, workersvc.ErrNotPRDIssue),
		errors.Is(err, workersvc.ErrNoPRDLink),
		errors.Is(err, workersvc.ErrDescriptionTooLarge):
		// Benign per-fire skip: the schedule still advances (no tick-storm).
		e.logger.Info("scheduler: issue fire skipped", "schedule", sched.ID.String(), "issue", iid, "reason", err)
		return nil, nil
	case errors.Is(err, workersvc.ErrRepoNotFound):
		return nil, workersvc.ErrRepoNotFound // permanent
	default:
		return nil, err // transient
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
		if errors.Is(fireErr, errBadConfig) {
			e.park(ctx, sched, "the schedule's configuration is invalid and cannot fire")
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
		// A timing the DB CHECK should have rejected. Park it rather than leave
		// next_fire_at in the past to re-claim every tick (symmetry with fireOne's
		// unknown-target default).
		e.logger.Error("scheduler: unknown timing", "schedule", sched.ID.String(), "timing", sched.Timing)
		e.park(ctx, sched, "the schedule has an unrecognized timing mode")
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
			// Permanent: a stored labels value that is not a string array will never
			// parse. Park the schedule instead of retrying it every tick forever.
			return nil, fmt.Errorf("parse sweep labels: %w: %w", errBadConfig, err)
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

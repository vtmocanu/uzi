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
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/notifysvc"
	"github.com/vtmocanu/uzi/api/internal/schedtmpl"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
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

// backfillHeadroom is how many EXTRA candidates past max_issues one sweep fire may
// examine to backfill slots lost to skipped (ineligible/already-running/transient)
// candidates (issue #416). The cap counts runs STARTED, not candidates matched, so the
// fan-out walks oldest-first past a skip and starts the next eligible candidate until
// max_issues runs fire. This constant bounds that walk: a fire fetches at most
// max_issues + backfillHeadroom candidates and so spends at most that many forge
// GetIssue / DB HasActiveRunForIssue calls, even when the head of the backlog is a wall
// of ineligible issues. 10 is a round, generous headroom for realistic backlogs; it is
// additive (not a multiplier) so per-fire cost stays predictable for small caps. Runs
// STARTED are still capped at max_issues, so backfill adds no load on the run limiter.
const backfillHeadroom = 10

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
	CountSweepCandidateIssues(ctx context.Context, arg store.CountSweepCandidateIssuesParams) (int64, error)
	HasActiveRunForIssue(ctx context.Context, arg store.HasActiveRunForIssueParams) (bool, error)
	HasActiveRunForSchedule(ctx context.Context, scheduleID pgtype.UUID) (bool, error)
	GetRepoForUser(ctx context.Context, arg store.GetRepoForUserParams) (store.GetRepoForUserRow, error)
	// Self-improvement fire path (PRD #590 M1): the per-repo active-run pre-check, the
	// owner-scoped improve_uzi backlog, and the addressed-stamp write, relocated from the
	// bespoke selfimprove engine's Store surface.
	CountActiveSelfImproveRunsForRepo(ctx context.Context, repoID uuid.UUID) (int64, error)
	// RecentSelfImproveMRRunsForRepo feeds the forge-sourced open-MR cap (PRD #686 D10/D12):
	// the small window of recent MR-bearing self_improve runs whose live open-state the fire
	// path checks against the forge before firing a new cycle.
	RecentSelfImproveMRRunsForRepo(ctx context.Context, arg store.RecentSelfImproveMRRunsForRepoParams) ([]store.RecentSelfImproveMRRunsForRepoRow, error)
	ListOpenImproveUziRecommendationsForUser(ctx context.Context, arg store.ListOpenImproveUziRecommendationsForUserParams) ([]store.ListOpenImproveUziRecommendationsForUserRow, error)
	MarkImproveUziRecommendationsAddressed(ctx context.Context, arg store.MarkImproveUziRecommendationsAddressedParams) (int64, error)
}

// RunCreator is the shared run-creation seam the scheduler fires through — the SAME
// seam autopilot and the manual board use. *workersvc.Service satisfies it.
type RunCreator interface {
	CreateRun(ctx context.Context, userID, repoID uuid.UUID, issueIID int64, description string, waitOnLimit *bool, seed *workersvc.SeededPlan) (store.Run, error)
	// CreateScheduledRun is the non-auto-approve scheduled path: like CreateRun but the
	// plan gate still requires a human (see workersvc.CreateScheduledRun). Eligibility is
	// the single uzi_label gate (PRD #764 M1); a PRD link is no longer required.
	CreateScheduledRun(ctx context.Context, userID, repoID uuid.UUID, issueIID int64, description string, waitOnLimit *bool, model *string, overrideSubagentModel bool, seed *workersvc.SeededPlan) (store.Run, error)
	CreateAutopilotRun(ctx context.Context, userID, repoID uuid.UUID, issueIID int64, description string) (store.Run, error)
	// CreateScheduledAutopilotRun is the auto-approve scheduled path (PRD #274 Decision
	// 1a): like CreateAutopilotRun but it HONOURS the schedule's persisted wait_on_limit
	// instead of the owner default. It is a distinct method so the poller's
	// CreateAutopilotRun seam stays untouched (see workersvc.CreateScheduledAutopilotRun).
	CreateScheduledAutopilotRun(ctx context.Context, userID, repoID uuid.UUID, issueIID int64, description string, waitOnLimit *bool, model *string, overrideSubagentModel bool) (store.Run, error)
	CreatePromptRun(ctx context.Context, userID, repoID, scheduleID uuid.UUID, title, prompt string, autoApprove, waitOnLimit bool, model *string, overrideSubagentModel bool) (store.Run, error)
	// CreateSelfImproveRun is the self-improvement fire path's insert (PRD #590 M1): a
	// dedicated issue-shaped, auto_approve, kind='self_improve' run against the tracking
	// issue, threading the schedule's per-schedule model override. The per-repo unique index
	// backs ErrActiveSelfImproveExists on a lost race.
	CreateSelfImproveRun(ctx context.Context, userID, repoID uuid.UUID, issueIID int64, title, description string, model *string, overrideSubagentModel bool) (store.Run, error)
}

// ForgeBuilder builds a forge driver from a stored (encrypted) connection — the same
// seam selfimprove/autopilot use. *forgesvc.Service satisfies it.
type ForgeBuilder interface {
	ForgeForConnection(forgeType, baseURL string, tokenCiphertext []byte) (forge.Forge, error)
}

// SettingsReader is the typed settings surface the scheduler reads to default an
// empty sweep selector to the configured uzi label. *settings.Cache satisfies it.
type SettingsReader interface {
	UziLabel(ctx context.Context) (string, error)
}

// Notifier is the notifysvc write seam (persist-first, best-effort Slack).
// *notifysvc.Service satisfies it — the same seam selfimprove uses. Optional: a nil
// notifier skips notifications. The scheduler uses it only to surface a permanent
// park (repo/owner gone) so a schedule going to status='error' is not silent.
type Notifier interface {
	Notify(ctx context.Context, n notifysvc.Notification) (store.Notification, error)
}

// VaultGate reports whether an owner's vault DEK is cached (unlocked) — the same gate the
// bespoke self-improve engine uses (PRD #590 M1). *vault.Vault satisfies it. Optional: a
// nil gate is treated as "always unlocked" (a deployment without the vault), so a
// self_improve fire never wedges on a missing vault.
type VaultGate interface {
	Unlocked(userID uuid.UUID) bool
}

// Scheduler is the single-instance scheduled-runs actor.
type Scheduler struct {
	store    Store
	runs     RunCreator
	forge    ForgeBuilder
	settings SettingsReader
	notifier Notifier
	// vault gates the self_improve fire path (PRD #590 M1): a locked owner vault yields a
	// benign skip so the run never tries to spend a token it cannot unwrap. nil ⇒ always
	// unlocked.
	vault    VaultGate
	interval time.Duration
	now      func() time.Time
	// catalog resolves a default-schedule catalog slug to its builtin DefaultJob at fire
	// time (PRD #589 M2, Decision 2). A default-origin row stores only its editable fields
	// plus catalog_slug; its prompt (prompt target) and labels+guidance (sweep target) are
	// resolved from HERE by catalog_slug so a shipped catalog change reaches every enabled
	// default automatically. It is an injectable func field (defaulted to schedtmpl.BySlug
	// in New) purely so a test can substitute a fake catalog; the discriminating fire tests
	// use the real one. A user-origin row NEVER consults it — the resolver engages only for
	// origin='default'.
	catalog func(slug string) (schedtmpl.DefaultJob, bool)
	logger  *slog.Logger
}

// New builds a Scheduler. interval is the wake cadence (how often the due-gate is
// polled), not any per-schedule cadence; a non-positive value is the caller's cue not
// to start it. notifier may be nil.
func New(st Store, runs RunCreator, fb ForgeBuilder, set SettingsReader, notifier Notifier, vault VaultGate, interval time.Duration, logger *slog.Logger) *Scheduler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Scheduler{
		store: st, runs: runs, forge: fb, settings: set, notifier: notifier, vault: vault,
		interval: interval, now: time.Now, catalog: schedtmpl.BySlug, logger: logger,
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
	// The tick path threads the FireOutcome into advance, which persists it into
	// last_fire on the success/benign path (PRD #308 M2). RunNow does NOT reach here, so
	// a manual fire never persists a last_fire (Decision 3).
	out, fireErr := e.fireOne(ctx, sched)
	e.advance(ctx, sched, out, fireErr)
}

// RunNow fires a schedule ONCE, manually, and returns the FireOutcome (matched/started/
// skips; the number started is issue→0/1, sweep→N, prompt→0/1, a benign dedup skip → 0
// started). It is the seam behind POST /api/schedules/{id}/run-now.
//
// Unlike the tick path it does NOT advance or park the schedule: a manual extra fire
// must not disturb the recurring cadence (next_fire_at stays where the tick left it) nor
// terminate a once schedule. Errors ride up UNCHANGED — workersvc.ErrRepoNotFound (repo
// gone / not owned), ErrBadConfig (malformed stored config), or a transient forge/DB
// error — so the handler can map each to the right HTTP code.
func (e *Scheduler) RunNow(ctx context.Context, sched store.RunSchedule) (FireOutcome, error) {
	return e.fireOne(ctx, sched)
}

// fireOne dispatches on the schedule target and returns the FireOutcome for this fire,
// plus:
//   - nil                          → success or a benign per-fire skip (advance the schedule)
//   - workersvc.ErrRepoNotFound    → permanent (park the schedule at status='error')
//   - ErrBadConfig                 → permanent (malformed stored config)
//   - workersvc.ErrGuardrailBlocked → permanent (#66: the bot can reach the default
//     branch, or it can't be verified — will not change tick-to-tick, so park rather
//     than tick-storm; the owner fixes forge protection or an admin allows the repo)
//   - any other error              → transient (do NOT advance; retry next tick)
//
// On any non-nil error the returned FireOutcome is the zero value: the outcome is only
// meaningful on the success/benign advance path (M2 persists it there).
func (e *Scheduler) fireOne(ctx context.Context, sched store.RunSchedule) (FireOutcome, error) {
	switch sched.Target {
	case "issue":
		return e.fireIssue(ctx, sched)
	case "sweep":
		return e.fireSweep(ctx, sched)
	case "prompt":
		return e.firePrompt(ctx, sched)
	case "self_improve":
		return e.fireSelfImprove(ctx, sched)
	default:
		// A target the DB CHECK should have rejected. Park it rather than retry forever.
		e.logger.Error("scheduler: unknown target", "schedule", sched.ID.String(), "target", sched.Target)
		return FireOutcome{}, workersvc.ErrRepoNotFound
	}
}

// fireIssue fires a single-issue schedule through the same seam a manual/autopilot
// start uses. Eligibility is the single uzi_label gate that seam applies (PRD #764 M1),
// read from the cached issue labels; the fresh GetIssue snapshot here supplies the run's
// title/description/web URL, not an eligibility decision.
func (e *Scheduler) fireIssue(ctx context.Context, sched store.RunSchedule) (FireOutcome, error) {
	repo, f, err := e.resolveRepoForge(ctx, sched)
	if err != nil {
		return FireOutcome{}, err
	}
	iid := sched.IssueIid.Int64

	// Benign dedup: a prior run for this issue is still live. Swallow WITHOUT firing,
	// but the schedule still advances (Decision 5-style: no queued re-runs). Matched=1:
	// the pinned issue was considered; it lands in the Skips bucket as already_running.
	// Title is left empty — the pre-check does not pay for a forge GetIssue just to label
	// a skip (PRD #308 M1).
	active, err := e.store.HasActiveRunForIssue(ctx, store.HasActiveRunForIssueParams{
		RepoID: repo.ID, IssueIid: pgtype.Int8{Int64: iid, Valid: true},
	})
	if err != nil {
		return FireOutcome{}, err // transient DB error: retry next tick
	}
	if active {
		e.logger.Info("scheduler: issue has active run, skipping fire", "schedule", sched.ID.String(), "issue", iid)
		iidCopy := iid
		return FireOutcome{Matched: 1, Skips: []Skip{{IssueIID: &iidCopy, Reason: SkipAlreadyRunning}}}, nil
	}

	issue, err := f.GetIssue(ctx, repo.ForgeProjectID, iid)
	if err != nil {
		// A forge error on an ISSUE target is transient (retry next tick), NOT a recorded
		// fetch_failed skip — that bucketing is a sweep-only choice (PRD #308). Return the
		// zero outcome so nothing is recorded for this fire.
		return FireOutcome{}, fmt.Errorf("get issue %d: %w", iid, err) // transient forge error
	}
	return e.createIssueRun(ctx, sched, repo.ID, iid, issue.Title, issue.Description, issue.WebURL)
}

// fireSweep resolves the label selector (an empty/NULL selector defaults to the PRD
// label — NEVER an empty jsonb array, which `labels @> '[]'` would match against
// every open issue, Decisions 7/9), lists the matching open issues, and fires each
// through the same per-issue flow as fireIssue. Per-issue failures are logged and
// skipped so one bad issue does not abort the fan-out; only a failure to resolve the
// repo/forge or to LIST is treated as transient for the whole schedule.
//
// The schedule's max_issues cap (PRD #274 M2) counts runs STARTED, not candidates
// matched (issue #416): the fan-out fetches a bounded scan window of the OLDEST
// max_issues + backfillHeadroom candidates (the query's ORDER BY forge_issue_iid ASC),
// walks it oldest-first flagging each skip exactly as before, and stops once max_issues
// runs have started — so a slot lost to a skip is refilled from the next eligible
// candidate rather than wasted. A NULL/invalid cap renders an unlimited LIMIT (today's
// unbounded behaviour) and has no started ceiling, draining the candidate set as before.
func (e *Scheduler) fireSweep(ctx context.Context, sched store.RunSchedule) (FireOutcome, error) {
	// Catalog overlay (PRD #589 M2, Decision 2): a default-origin sweep row stores NULL
	// labels/guidance and carries them in the builtin catalog, resolved HERE by catalog_slug
	// so a shipped selector/guidance change reaches every enabled default automatically. The
	// catalog values are overlaid onto the LOCAL sched copy (sched is a by-value parameter,
	// so this mutates nothing persisted) BEFORE the existing logic runs, so resolveSweepLabels
	// and createIssueRun's composeRunDescription transparently use the catalog values. A gone
	// catalog entry can never resolve → permanent park (errBadConfig), not a tick-storm. A
	// user-origin row is byte-for-byte unchanged — the resolver engages ONLY for origin='default'.
	if sched.Origin == "default" {
		job, ok := e.catalog(sched.CatalogSlug.String)
		if !ok {
			return FireOutcome{}, errBadConfig // catalog entry gone → permanent park
		}
		labelsJSON, mErr := json.Marshal(job.Labels)
		if mErr != nil {
			return FireOutcome{}, fmt.Errorf("marshal catalog labels: %w", mErr)
		}
		sched.Labels = labelsJSON
		// Owner-guidance overlay (issue #675): a default sweep's guidance column holds the
		// owner OVERLAY (NULL today until one is set). The catalog value is the BAKED guidance.
		// Compose baked + overlay into the single effective guidance so the one
		// composeRunDescription call in createIssueRun emits exactly ONE guidance header. An
		// empty overlay yields effective == baked, i.e. byte-for-byte the prior baked-only
		// description. An over-long overlay is truncated (tail-first) by composeRunDescription,
		// so the baked catalog text is preserved before the overlay.
		overlay := guidanceOf(sched)
		baked := job.Guidance
		effective := baked
		if strings.TrimSpace(overlay) != "" {
			if strings.TrimSpace(baked) == "" {
				effective = overlay
			} else {
				effective = baked + "\n\n" + overlay
			}
		}
		sched.Guidance = pgtype.Text{String: effective, Valid: effective != ""}
	}
	repo, f, err := e.resolveRepoForge(ctx, sched)
	if err != nil {
		return FireOutcome{}, err
	}
	labelsJSON, err := e.resolveSweepLabels(ctx, sched.Labels)
	if err != nil {
		return FireOutcome{}, err
	}
	// Backfill scan window (issue #416): fetch max_issues + backfillHeadroom candidates so
	// the loop below can walk past skipped slots and still start up to max_issues runs. The
	// widened limit is computed here in Go and threaded into the SAME MaxIssues query param
	// (no SQL change). A NULL/invalid cap stays unlimited exactly as before.
	scanLimit := sched.MaxIssues
	if sched.MaxIssues.Valid {
		scanLimit = pgtype.Int4{Int32: sched.MaxIssues.Int32 + backfillHeadroom, Valid: true}
	}
	candidates, err := e.store.ListSweepCandidateIssues(ctx, store.ListSweepCandidateIssuesParams{
		RepoID: repo.ID, Labels: labelsJSON, MaxIssues: scanLimit,
	})
	if err != nil {
		return FireOutcome{}, err // transient DB error
	}
	e.logger.Info("scheduler: sweep fan-out (not behind the per-user run limiter, review N1)",
		"schedule", sched.ID.String(), "candidates", len(candidates))

	// Capped probe: only a set max_issues can truncate, so count the full matching set
	// only when the cap is present (a NULL cap can never truncate → Capped=false, no count).
	// Since the fetch widened to the scan window (issue #416), Capped now means "more
	// matching open issues than the scan window (max_issues + backfillHeadroom) reached",
	// i.e. eligible issues may exist beyond backfill's reach — it still drives the
	// started-nothing hint.
	capped := false
	if sched.MaxIssues.Valid {
		total, err := e.store.CountSweepCandidateIssues(ctx, store.CountSweepCandidateIssuesParams{
			RepoID: repo.ID, Labels: labelsJSON,
		})
		if err != nil {
			return FireOutcome{}, err // transient DB error
		}
		capped = total > int64(len(candidates))
	}

	// Matched is set at the END to len(Started)+len(Skips) — the candidates actually
	// EXAMINED this fire (issue #416), which preserves the matched == started + skipped
	// invariant (PRD #308 Decision 4) by construction. Every candidate the loop reaches
	// lands in exactly one of Started/Skips; the loop stops early once max_issues have
	// started, so Matched counts attempts (may exceed max_issues), not the whole window.
	out := FireOutcome{Capped: capped}
	for _, c := range candidates {
		iid := c.ForgeIssueIid
		iidCopy := iid
		active, err := e.store.HasActiveRunForIssue(ctx, store.HasActiveRunForIssueParams{
			RepoID: repo.ID, IssueIid: pgtype.Int8{Int64: iid, Valid: true},
		})
		if err != nil {
			// Transient per-candidate DB error: recorded as fetch_failed so the candidate
			// is not silently dropped. Title is unavailable (not fetched) → left empty.
			e.logger.Warn("scheduler: sweep active-run check", "schedule", sched.ID.String(), "issue", iid, "error", err)
			out.Skips = append(out.Skips, Skip{IssueIID: &iidCopy, Reason: SkipFetchFailed})
			continue
		}
		if active {
			out.Skips = append(out.Skips, Skip{IssueIID: &iidCopy, Reason: SkipAlreadyRunning})
			continue
		}
		issue, err := f.GetIssue(ctx, repo.ForgeProjectID, iid)
		if err != nil {
			e.logger.Warn("scheduler: sweep fetch issue", "schedule", sched.ID.String(), "issue", iid, "error", err)
			out.Skips = append(out.Skips, Skip{IssueIID: &iidCopy, Reason: SkipFetchFailed})
			continue
		}
		res, err := e.createIssueRun(ctx, sched, repo.ID, iid, issue.Title, issue.Description, issue.WebURL)
		if err != nil {
			// A permanent/transient repo error mid-sweep is unexpected (the repo just
			// resolved); record it as fetch_failed and keep going rather than aborting the
			// whole fan-out or dropping the candidate.
			e.logger.Warn("scheduler: sweep create run", "schedule", sched.ID.String(), "issue", iid, "error", err)
			out.Skips = append(out.Skips, Skip{IssueIID: &iidCopy, Title: issue.Title, Reason: SkipFetchFailed, WebURL: issue.WebURL})
		} else {
			// createIssueRun returns a single-issue FireOutcome (one Started or one Skip);
			// fold it into the sweep's buckets.
			out.Started = append(out.Started, res.Started...)
			out.Skips = append(out.Skips, res.Skips...)
		}
		// Light pacing between seam calls (review N1: no per-user limiter guards this).
		e.sleep(sweepPacing)
		// Backfill early break (issue #416): the cap bounds runs STARTED, not candidates
		// examined. Stop once max_issues have started so backfill refills lost slots but
		// never overshoots the cap. A NULL cap has no ceiling and drains the (unlimited)
		// candidate set exactly as before.
		if sched.MaxIssues.Valid && len(out.Started) >= int(sched.MaxIssues.Int32) {
			break
		}
	}
	// Matched = candidates examined this fire (== started + skipped by construction).
	out.Matched = len(out.Started) + len(out.Skips)
	return out, nil
}

// firePrompt fires a free-form prompt schedule: a repo-ful, issue-less prompt run
// keyed to the schedule. HasActiveRunForSchedule is the benign dedup pre-check
// alongside the uq_runs_one_active_prompt_per_schedule structural backstop.
func (e *Scheduler) firePrompt(ctx context.Context, sched store.RunSchedule) (FireOutcome, error) {
	// Prompt resolution (PRD #589 M2, Decision 2): a default-origin row stores NULL prompt
	// and carries its text in the builtin catalog, resolved HERE by catalog_slug at fire
	// time so a shipped prompt change reaches every enabled default automatically. A gone
	// catalog entry can never resolve, so it is a PERMANENT park (ErrBadConfig), never a
	// tick-storm. A user-origin row takes the identical old path (prompt straight off the
	// column) — the resolver engages ONLY for origin='default', so existing schedules are
	// byte-for-byte unchanged. Resolution runs BEFORE deriving the title so the run-view
	// header labels the resolved prompt, not the (NULL) column.
	prompt := sched.Prompt.String
	if sched.Origin == "default" {
		job, ok := e.catalog(sched.CatalogSlug.String)
		if !ok {
			return FireOutcome{}, errBadConfig // catalog entry gone → permanent park
		}
		prompt = job.Prompt
	}
	// Matched=1 always: a prompt schedule is its own single candidate whenever a fire is
	// attempted. The prompt title labels its Started/Skip; IssueIID is nil (issue-less).
	// The title derives from the RAW resolved prompt (not the composed instruction) so the
	// run-view header labels the catalog prompt, unaffected by owner guidance.
	title := promptTitle(prompt)
	// Owner-guidance overlay (issue #662): a DEFAULT prompt job can carry owner-editable
	// guidance that is appended to the catalog-resolved prompt here at fire time. This is
	// applied unconditionally, but is a NO-OP for user-origin rows: guidance is persisted
	// only for default-origin prompt schedules, so guidanceOf(sched) returns "" for a
	// user-origin row, and composeRunDescription returns the body unchanged when guidance
	// trims to empty — so the user path stays byte-for-byte identical to before.
	instruction := composeRunDescription(prompt, guidanceOf(sched))
	active, err := e.store.HasActiveRunForSchedule(ctx, pgUUID(sched.ID))
	if err != nil {
		return FireOutcome{}, err // transient DB error
	}
	if active {
		e.logger.Info("scheduler: prompt schedule has active run, skipping fire", "schedule", sched.ID.String())
		return FireOutcome{Matched: 1, Skips: []Skip{{Title: title, Reason: SkipAlreadyRunning}}}, nil
	}
	run, err := e.runs.CreatePromptRun(ctx, sched.UserID, sched.RepoID, sched.ID, title, instruction, sched.AutoApprove, sched.WaitOnLimit, scheduleModel(sched), scheduleOverrideSubagentModel(sched))
	switch {
	case err == nil:
		return FireOutcome{Matched: 1, Started: []Started{{RunID: run.ID, Title: title}}}, nil
	case errors.Is(err, workersvc.ErrActivePromptExists):
		// Raced the pre-check; the unique index did its job. Benign — advance. Recorded as
		// already_running, the same reason the pre-check bool synthesizes.
		e.logger.Info("scheduler: prompt run already active (race)", "schedule", sched.ID.String())
		return FireOutcome{Matched: 1, Skips: []Skip{{Title: title, Reason: SkipAlreadyRunning}}}, nil
	case errors.Is(err, workersvc.ErrRepoNotFound):
		return FireOutcome{}, workersvc.ErrRepoNotFound // permanent
	default:
		return FireOutcome{}, err // transient
	}
}

// createIssueRun fires one issue through the shared seam. auto_approve schedules go
// through CreateScheduledAutopilotRun; non-auto-approve ones through CreateScheduledRun,
// both threading the schedule's wait_on_limit. Post-PRD #764 M1 every create path shares
// the single uzi_label eligibility gate — there is no PRD-link requirement or escape-hatch
// waiver left for the two branches to differ on — so they differ
// only in auto-approve, not in what they will run. The benign per-fire seam rejects
// (active run, not eligible, description too large) are swallowed so the schedule still
// advances; ErrRepoNotFound is permanent; anything else is transient.
//
// Wait-on-limit (PRD #274 Decision 1a, revising PRD #241 Decision 2): BOTH branches now
// thread the schedule's persisted wait_on_limit. The auto-approve branch fires through
// CreateScheduledAutopilotRun (NOT the poller's CreateAutopilotRun, whose seam stays
// untouched so label-driven autopilot keeps taking the owner default); the non-auto
// branch fires through CreateScheduledRun. So a schedule's explicit wait_on_limit takes
// effect regardless of auto_approve.
//
// Guidance (PRD #274 M3): before the seam call the issue body is composed with the
// schedule's optional owner guidance into a single description via composeRunDescription
// — a clearly delineated "how" section after the body (the untrusted forge task).
// composeRunDescription truncates the guidance rather than let the composed total exceed
// MaxIssueDescriptionBytes, so guidance never turns a runnable issue into a silent
// ErrDescriptionTooLarge skip. It is purely additive: the eligibility gate reads the
// cached issue's uzi_label (PRD #764 M1) and never sees the composed text.
func (e *Scheduler) createIssueRun(ctx context.Context, sched store.RunSchedule, repoID uuid.UUID, iid int64, title, description string, webURL string) (FireOutcome, error) {
	iidCopy := iid

	desc := composeRunDescription(description, guidanceOf(sched))

	waitOnLimit := sched.WaitOnLimit
	var run store.Run
	var err error
	if sched.AutoApprove {
		// CreateScheduledAutopilotRun, NOT the poller's CreateAutopilotRun: a scheduled
		// auto-approve run carries a persisted per-schedule wait_on_limit that must take
		// effect (PRD #274 Decision 1a). Leaving the poller seam untouched is a structural
		// guarantee that label-driven autopilot behaviour is unchanged.
		run, err = e.runs.CreateScheduledAutopilotRun(ctx, sched.UserID, repoID, iid, desc, &waitOnLimit, scheduleModel(sched), scheduleOverrideSubagentModel(sched))
	} else {
		// CreateScheduledRun is the non-auto-approve scheduled seam. Post-PRD #764 M1 it
		// and the interactive CreateRun apply the SAME single uzi_label eligibility gate —
		// no PRD link or escape-hatch waiver enters into it — so a swept uzi-labelled issue
		// with no PRD link runs here. The only thing separating this branch from the
		// CreateScheduledAutopilotRun branch above is auto-approve (the plan gate still
		// needs a human here), not eligibility.
		run, err = e.runs.CreateScheduledRun(ctx, sched.UserID, repoID, iid, desc, &waitOnLimit, scheduleModel(sched), scheduleOverrideSubagentModel(sched), nil)
	}

	if err == nil {
		return FireOutcome{Matched: 1, Started: []Started{{IssueIID: &iidCopy, RunID: run.ID, Title: title, WebURL: webURL}}}, nil
	}
	// Benign per-fire seam sentinel → a typed Skip; the schedule still advances (no
	// tick-storm). skipReasonForErr maps the benign sentinels a scheduled fire can still
	// return (ErrActiveRunExists → already_running, ErrNotPRDIssue → not_eligible,
	// ErrDescriptionTooLarge → description_too_large). The old link-less skip reason was
	// retired with the PRD-link gate (PRD #764).
	if reason, ok := skipReasonForErr(err); ok {
		e.logger.Info("scheduler: issue fire skipped", "schedule", sched.ID.String(), "issue", iid, "reason", err)
		return FireOutcome{Matched: 1, Skips: []Skip{{IssueIID: &iidCopy, Title: title, Reason: reason, WebURL: webURL}}}, nil
	}
	if errors.Is(err, workersvc.ErrRepoNotFound) {
		return FireOutcome{}, workersvc.ErrRepoNotFound // permanent
	}
	return FireOutcome{}, err // transient
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
//
// On the success/benign path it also serializes the FireOutcome into last_fire (PRD #308
// M2). A marshal error is LOGGED and last_fire falls back to nil (SQL NULL): a
// serialization hiccup must never wedge the cadence — the schedule still advances. The
// park and transient branches never touch last_fire, so a parked/transient fire keeps the
// prior last_fire or none (Decision 5).
func (e *Scheduler) advance(ctx context.Context, sched store.RunSchedule, out FireOutcome, fireErr error) {
	if fireErr != nil {
		if errors.Is(fireErr, workersvc.ErrRepoNotFound) {
			e.park(ctx, sched, "the schedule's repo is disconnected or no longer owned by you")
			return
		}
		if errors.Is(fireErr, errBadConfig) {
			e.park(ctx, sched, "the schedule's configuration is invalid and cannot fire")
			return
		}
		if errors.Is(fireErr, workersvc.ErrGuardrailBlocked) {
			// #66: the bot can push or merge to this repo's default branch, or that
			// could not be verified. This will not resolve tick-to-tick, so park the
			// schedule rather than refire (and re-block) every tick — a self-inflicted
			// tick-storm. The owner fixes branch protection on the forge, or an admin
			// allows the repo, then re-enables the schedule.
			e.park(ctx, sched, "runs are blocked: the bot can push or merge to this repo's default branch (or it could not be verified); fix branch protection on the forge, or ask an admin to allow this repo, then re-enable the schedule")
			return
		}
		// Transient: leave next_fire_at in the past so the next tick retries.
		e.logger.Warn("scheduler: transient fire error, will retry", "schedule", sched.ID.String(), "error", fireErr)
		return
	}

	now := e.now()
	lastFireJSON, err := marshalLastFire(out, now)
	if err != nil {
		// Never let a serialization hiccup wedge the cadence: log it and advance with a
		// NULL last_fire rather than skipping the advance.
		e.logger.Error("scheduler: marshal last_fire", "schedule", sched.ID.String(), "error", err)
		lastFireJSON = nil
	}
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
			LastFire:    lastFireJSON,
			ID:          sched.ID,
		}); err != nil {
			e.logger.Error("scheduler: advance recurring", "schedule", sched.ID.String(), "error", err)
		}
	case "once":
		if _, err := e.store.AdvanceSchedule(ctx, store.AdvanceScheduleParams{
			LastFiredAt: pgTime(now),
			NextFireAt:  pgtype.Timestamptz{}, // NULL: a once schedule never fires again
			Status:      "fired",
			LastFire:    lastFireJSON,
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
			Slack:   &notifysvc.SlackRender{Emoji: "⏸️", Title: "Scheduled run paused", Body: body},
		}); err != nil {
			e.logger.Warn("scheduler: notify schedule error", "schedule", sched.ID.String(), "error", err)
		}
	}
}

// resolveSweepLabels turns the schedule's stored jsonb label selector into the jsonb
// param ListSweepCandidateIssues expects. An empty/NULL/`[]` selector defaults to a
// SINGLE-element [uzi label] — never an empty array, whose `@> '[]'` containment
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
		uzi, _ := e.settings.UziLabel(ctx)
		if strings.TrimSpace(uzi) == "" {
			uzi = settings.DefaultUziLabel
		}
		sel = []string{uzi}
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

// guidanceHeader is the fixed delimiter + framing prepended to owner guidance when it is
// composed into a run instruction (PRD #274 M3). The wording is deliberately fixed: it
// tells the model the section is HOW-steering authored by the schedule owner, distinct
// from the issue body above it (the WHAT, and untrusted forge content). Do not template
// user text into this header.
const guidanceHeader = "\n\n---\n\n" +
	"The guidance below was provided by the schedule owner to steer HOW this task is " +
	"approached. It does not change WHAT the task is (described above); apply it where " +
	"relevant.\n\n"

// guidanceTruncMarker is appended when the guidance had to be truncated to keep the
// composed description under MaxIssueDescriptionBytes, so the model (and a human reading
// the run) can tell the guidance was cut rather than authored short.
const guidanceTruncMarker = "\n\n…[guidance truncated]"

// composeRunDescription joins an issue body (the task) with optional owner guidance (the
// "how") into the single description passed to the run-creation seam (PRD #274 M3).
//
// When guidance is empty/whitespace it returns the body UNCHANGED — no delimiter — so a
// schedule without guidance produces a byte-identical description to the pre-M3 behaviour.
//
// Otherwise it appends guidanceHeader + guidance. Because createRun rejects a description
// over workersvc.MaxIssueDescriptionBytes with ErrDescriptionTooLarge — which the
// scheduler treats as a benign skip — appending guidance must never push a runnable issue
// over the cap. So when body + full section would exceed the cap, the GUIDANCE is
// truncated (on a UTF-8 rune boundary) with a marker, keeping the header when there is
// room for it, rather than the issue being silently dropped. If the body alone already
// meets/exceeds the cap there is no room for guidance and the body is returned unchanged
// (createRun handles the oversized body exactly as before — guidance does not make it
// worse).
func composeRunDescription(body, guidance string) string {
	g := strings.TrimSpace(guidance)
	if g == "" {
		return body
	}
	const max = workersvc.MaxIssueDescriptionBytes

	section := guidanceHeader + g
	if len(body)+len(section) <= max {
		return body + section
	}

	// Truncation path. If the body alone leaves no room, do not touch it.
	room := max - len(body)
	if room <= 0 {
		return body
	}
	// Reserve the header and the truncation marker; whatever remains is the guidance
	// budget. If the header + marker alone will not fit, guidance cannot be included
	// meaningfully without risking the cap — run the body alone (it still runs).
	avail := room - len(guidanceHeader) - len(guidanceTruncMarker)
	if avail <= 0 {
		return body
	}
	return body + guidanceHeader + truncateUTF8(g, avail) + guidanceTruncMarker
}

// guidanceOf reads a schedule's stored guidance as a plain string, "" when NULL/invalid.
func guidanceOf(sched store.RunSchedule) string {
	if sched.Guidance.Valid {
		return sched.Guidance.String
	}
	return ""
}

// scheduleModel returns the schedule's per-schedule model override (PRD #300) as a
// *string for the run-insert seams: nil when the column is NULL, so a schedule with no
// override freezes NULL onto the run and the run inherits the owner's per-user Worker
// default at claim assembly (today's behaviour).
func scheduleModel(s store.RunSchedule) *string {
	if !s.Model.Valid {
		return nil
	}
	m := s.Model.String
	return &m
}

// scheduleOverrideSubagentModel returns the schedule's "apply model also to agents"
// opt-in (PRD #305) for the run-insert seams: false when off, so a schedule freezes
// false onto the run and it stays in the default lane where subagent pins win (today's
// behaviour). It is a plain bool (NOT NULL DEFAULT false column), not a tri-state.
func scheduleOverrideSubagentModel(s store.RunSchedule) bool {
	return s.OverrideSubagentModel
}

// truncateUTF8 returns the longest prefix of s that is at most n bytes AND does not split
// a multibyte rune. s is assumed valid UTF-8, so backing the cut up to the nearest rune
// start yields a valid string.
func truncateUTF8(s string, n int) string {
	if n >= len(s) {
		return s
	}
	if n <= 0 {
		return ""
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
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

// isNoRows reports whether err signals "no such row" from the store — the marker
// that a repo lookup found nothing, i.e. the repo is gone or no longer owned.
func isNoRows(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}

// pgTime wraps a known-present time as a valid pgtype.Timestamptz.
func pgTime(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

// pgUUID wraps a known-present uuid as a valid pgtype.UUID.
func pgUUID(id uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: id, Valid: true} }

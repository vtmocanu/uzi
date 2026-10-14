package schedsvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/pipelinestatus"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// CIRepairConfig carries the operator bounds a ci_watch fire shares with the CI auto-fix lane
// (PRD #2865): the same CI_FIX_MAX_JOBS, CI_FIX_LOG_TAIL_BYTES, CI_AUTOFIX_MAX_ATTEMPTS and
// CI_AUTOFIX_CONFIG_PATHS values the poller's CI auto-fix is built from. The server-side repair
// floor (workersvc.CIRepairConfigFloor) is unioned into ConfigPaths by the run creation seam.
type CIRepairConfig struct {
	MaxJobs      int
	LogTailBytes int
	MaxAttempts  int
	ConfigPaths  []string
}

// SetCIRepair wires the ci_watch bounds. Call once at startup. A zero field falls back to the
// same default the matching environment variable has, so a scheduler that never wires it (the
// run-now-only handler scheduler in a test) still fires with sane bounds.
func (e *Scheduler) SetCIRepair(c CIRepairConfig) { e.ciRepair = c }

const (
	ciDefaultMaxJobs      = 10
	ciDefaultLogTailBytes = 32768
	ciDefaultMaxAttempts  = 2
	// ciListLimit is the one bounded page a fire reads per repo (Forgejo caps itself at 50).
	ciListLimit = 100
	// ciMaxSnapshots bounds the evidence builds (job listing plus log tails) per fire.
	ciMaxSnapshots = 3
)

func (c CIRepairConfig) withDefaults() CIRepairConfig {
	if c.MaxJobs <= 0 {
		c.MaxJobs = ciDefaultMaxJobs
	}
	if c.LogTailBytes <= 0 {
		c.LogTailBytes = ciDefaultLogTailBytes
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = ciDefaultMaxAttempts
	}
	return c
}

// ciPullRequestEvents are the executions a default-branch watch ignores: a fork pull request
// from a branch that happens to be named like the default branch reports that branch name.
var ciPullRequestEvents = map[string]struct{}{
	"pull_request": {}, "pull_request_target": {},
	"merge_request_event": {}, "external_pull_request_event": {},
}

// ciCompleted reports whether a workflow execution has finished. GitHub splits phase from
// outcome (Status "completed" plus a Conclusion); GitLab and Forgejo fold everything into
// Status, so a terminal status counts.
func ciCompleted(r forge.WorkflowRun) bool {
	if r.Status == "completed" || r.Conclusion != "" {
		return true
	}
	switch pipelinestatus.Tone(r.Status) {
	case "passed", "failed":
		return true
	}
	switch r.Status {
	case "canceled", "cancelled", "skipped", "neutral", "stale":
		return true
	}
	return false
}

type ciClass int

const (
	ciClassRed ciClass = iota
	ciClassGreen
	ciClassOther
)

// ciClassify collapses the status/conclusion pair (the conclusion once completed) and
// classifies it with the shared pipelinestatus helpers: failure and timed_out are red;
// cancelled, skipped, neutral and action_required are neither red nor green.
func ciClassify(r forge.WorkflowRun) ciClass {
	collapsed := r.Status
	if r.Conclusion != "" {
		collapsed = r.Conclusion
	}
	switch {
	case pipelinestatus.IsFailed(collapsed):
		return ciClassRed
	case pipelinestatus.IsSuccess(collapsed):
		return ciClassGreen
	default:
		return ciClassOther
	}
}

// ciKey is one watched workflow's newest completed default-branch execution.
type ciKey struct {
	key   string
	run   forge.WorkflowRun
	obs   workersvc.CIObservation
	class ciClass
	ep    store.CiWatchEpisode
	hasEp bool
}

// parseCIWorkflows reads the schedule's selector list: NULL (or null) means every workflow.
func parseCIWorkflows(raw []byte) ([]string, error) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return nil, nil
	}
	var sel []string
	if err := json.Unmarshal(raw, &sel); err != nil {
		return nil, fmt.Errorf("parse ci_workflows: %w: %w", errBadConfig, err)
	}
	return nonBlank(sel), nil
}

func ciSkip(k *ciKey, reason SkipReason) Skip {
	return Skip{Title: capField(k.run.Name, ciFieldCap), Reason: reason, WebURL: capField(k.run.WebURL, ciURLCap)}
}

func ciHaltSkip(reason string) SkipReason {
	if reason == workersvc.CIHaltAttemptCap {
		return SkipCIHaltedAttemptCap
	}
	return SkipCIHaltedNoProgress
}

// ciResolveBody returns the base instruction a ci_watch fire runs with: the schedule's own
// prompt, or the catalog body of a default job (resolved at fire time like a default prompt).
func (e *Scheduler) ciResolveBody(sched store.RunSchedule) (string, error) {
	if sched.Origin == "default" {
		job, ok := e.catalog(sched.CatalogSlug.String)
		if !ok {
			return "", errBadConfig // catalog entry gone: permanent park
		}
		return job.Prompt, nil
	}
	return sched.Prompt.String, nil
}

// fireCIWatch fires a ci_watch schedule (PRD #2865): it inspects the newest completed
// default-branch execution of each watched workflow and starts at most one guarded repair run.
//
// Forge discovery, snapshots and signatures run OUTSIDE any database lock. Every write (a
// started run, a halt, a green reset) goes through workersvc, which re-takes the repo and
// schedule locks, rechecks the configuration fingerprint taken here, and rechecks the episode
// row before writing; a recheck that fails writes nothing and surfaces as a benign skip.
func (e *Scheduler) fireCIWatch(ctx context.Context, sched store.RunSchedule) (FireOutcome, error) {
	cfg := e.ciRepair.withDefaults()
	body, err := e.ciResolveBody(sched)
	if err != nil {
		return FireOutcome{}, err
	}
	selectors, err := parseCIWorkflows(sched.CiWorkflows)
	if err != nil {
		return FireOutcome{}, err
	}
	repo, f, err := e.resolveRepoForge(ctx, sched)
	if err != nil {
		return FireOutcome{}, err
	}
	branch := strings.TrimSpace(repo.DefaultBranch.String)
	if !repo.DefaultBranch.Valid || branch == "" {
		return FireOutcome{Matched: 1, Skips: []Skip{{Reason: SkipNoDefaultBranch}}}, nil
	}
	// The configuration this fire decides on; every write rechecks it under the locks.
	fingerprint := workersvc.CIWatchFingerprint(sched, repo.DefaultBranch, body)
	target := workersvc.CIWatchTarget{
		UserID: sched.UserID, RepoID: sched.RepoID, ScheduleID: sched.ID, ForgeType: repo.ForgeType,
		Fingerprint: fingerprint, ResolvedBody: body,
	}

	// One budget for the whole fire: the listing and every snapshot's job listing draw on it,
	// sequentially (the compressed per-fire reservation is not designed for concurrent reads).
	ctx = forge.WithCIRepairReadLimits(ctx)
	runs, err := f.ListWorkflowRuns(ctx, repo.ForgeProjectID, forge.ListWorkflowRunsOptions{Branch: branch, Limit: ciListLimit})
	if err != nil {
		return ciForgeErrOutcome(err)
	}

	keys := ciNewestPerKey(repo.ForgeType, branch, runs)
	keys, unseen := ciApplySelectors(keys, selectors)

	var out FireOutcome
	configChanged := func() (FireOutcome, error) {
		e.logger.Info("scheduler: ci_watch configuration changed during the fire, skipping", "schedule", sched.ID.String())
		return FireOutcome{Matched: 1, Skips: []Skip{{Reason: SkipCIConfigChanged}}}, nil
	}

	// Pass 1: classify every key against its (unlocked) episode, perform fresh green resets,
	// and collect the red candidates.
	var candidates, handled []*ciKey
	greens := 0
	for _, k := range keys {
		ep, gerr := e.store.GetCIWatchEpisode(ctx, store.GetCIWatchEpisodeParams{ScheduleID: sched.ID, UserID: sched.UserID, WorkflowKey: k.key})
		switch {
		case gerr == nil:
			k.ep, k.hasEp = ep, true
		case errors.Is(gerr, pgx.ErrNoRows):
		default:
			return FireOutcome{}, gerr // transient DB error
		}
		fresh := workersvc.ClassifyCIObservation(repo.ForgeType, k.obs, k.ep) == workersvc.CIFresh
		switch k.class {
		case ciClassOther:
			out.Skips = append(out.Skips, ciSkip(k, SkipCINotFailed))
		case ciClassGreen:
			greens++
			if !k.hasEp || !fresh {
				continue // nothing to reset, or a stale/identical green: leave the episode alone
			}
			tr := workersvc.CIWatchTransition{CIWatchTarget: ciTargetFor(target, k), Reset: true}
			switch terr := e.runs.TransitionCIWatchEpisode(ctx, tr); {
			case terr == nil, errors.Is(terr, workersvc.ErrCIWatchStale), errors.Is(terr, workersvc.ErrCIWatchAlreadyHandled):
			case errors.Is(terr, workersvc.ErrCIWatchConfigChanged):
				return configChanged()
			default:
				return FireOutcome{}, terr
			}
		case ciClassRed:
			switch {
			case k.hasEp && k.ep.HaltedReason.Valid:
				out.Skips = append(out.Skips, ciSkip(k, ciHaltSkip(k.ep.HaltedReason.String)))
			case !fresh:
				handled = append(handled, k)
			default:
				candidates = append(candidates, k)
			}
		}
	}
	for _, name := range unseen {
		out.Skips = append(out.Skips, Skip{Title: capField(name, ciFieldCap), Reason: SkipCIWatchedNotSeen})
	}

	// A run of this schedule still in flight explains every red that is not halted: it is
	// reported as already_running, handled or not (once the run ends, an execution that is
	// still the newest is reported as already handled).
	if len(candidates)+len(handled) > 0 {
		active, aerr := e.store.HasActiveRunForSchedule(ctx, pgconv.UUID(sched.ID))
		if aerr != nil {
			return FireOutcome{}, aerr // transient DB error
		}
		if active {
			e.logger.Info("scheduler: ci_watch schedule has an active run, skipping fire", "schedule", sched.ID.String())
			for _, k := range append(append([]*ciKey{}, handled...), candidates...) {
				out.Skips = append(out.Skips, ciSkip(k, SkipAlreadyRunning))
			}
			candidates, handled = nil, nil
		}
	}
	for _, k := range handled {
		out.Skips = append(out.Skips, ciSkip(k, SkipCIAlreadyHandled))
	}

	// Pass 2: at most one run. Oldest newest-red execution first, ties by key.
	sort.SliceStable(candidates, func(i, j int) bool {
		if c := workersvc.CIObservationOrder(repo.ForgeType, candidates[i].obs, candidates[j].obs); c != 0 {
			return c < 0
		}
		return candidates[i].key < candidates[j].key
	})
	if len(candidates) > 0 {
		o, ferr := e.ciStartOne(ctx, sched, repo, f, cfg, target, body, candidates)
		if ferr != nil {
			return FireOutcome{}, ferr
		}
		if o.configChanged {
			return configChanged()
		}
		out.Started = append(out.Started, o.started...)
		out.Skips = append(out.Skips, o.skips...)
	}

	if len(out.Started) == 0 && len(out.Skips) == 0 {
		// Nothing red and nothing unseen. Green only when something completed and was green;
		// an empty window is never reported as green.
		reason := SkipCIWatchedNotSeen
		if greens > 0 {
			reason = SkipCIGreen
		}
		out.Skips = append(out.Skips, Skip{Reason: reason})
	}
	out.Matched = len(out.Started) + len(out.Skips)
	return out, nil
}

// ciForgeErrOutcome maps a discovery or evidence read error: an unsupported forge and an
// exceeded read budget are benign skips; anything else is transient.
func ciForgeErrOutcome(err error) (FireOutcome, error) {
	if reason, ok := ciForgeErrOutcomeBenign(err); ok {
		return FireOutcome{Matched: 1, Skips: []Skip{{Reason: reason}}}, nil
	}
	return FireOutcome{}, err
}

func ciTargetFor(base workersvc.CIWatchTarget, k *ciKey) workersvc.CIWatchTarget {
	t := base
	t.WorkflowKey = k.key
	t.Observation = k.obs
	return t
}

// ciNewestPerKey keeps, per workflow key, the newest completed non-pull-request execution on
// the default branch, by the forge-specific ordering the lock-time freshness check also uses
// (so an older execution completing late never outranks a newer one).
func ciNewestPerKey(forgeType, branch string, runs []forge.WorkflowRun) []*ciKey {
	best := map[string]*ciKey{}
	for _, r := range runs {
		if _, pr := ciPullRequestEvents[r.Event]; pr || !ciCompleted(r) || r.WorkflowKey == "" {
			continue
		}
		if r.Branch != "" && r.Branch != branch {
			continue
		}
		obs := workersvc.CIObservation{RunID: r.ID, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
		if cur, ok := best[r.WorkflowKey]; ok && !workersvc.CIObservationNewer(forgeType, obs, cur.obs) {
			continue
		}
		best[r.WorkflowKey] = &ciKey{key: r.WorkflowKey, run: r, obs: obs, class: ciClassify(r)}
	}
	keys := make([]*ciKey, 0, len(best))
	for _, k := range best {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].key < keys[j].key })
	return keys
}

// ciApplySelectors restricts keys to the watched set (an entry matches a key or a display name
// exactly) and returns the entries that matched no completed execution in the window.
func ciApplySelectors(keys []*ciKey, selectors []string) (kept []*ciKey, unseen []string) {
	if len(selectors) == 0 {
		return keys, nil
	}
	matched := make([]bool, len(selectors))
	for _, k := range keys {
		hit := false
		for i, s := range selectors {
			if s == k.key || s == k.run.Name {
				matched[i], hit = true, true
			}
		}
		if hit {
			kept = append(kept, k)
		}
	}
	for i, s := range selectors {
		if !matched[i] {
			unseen = append(unseen, s)
		}
	}
	return kept, unseen
}

type ciStartResult struct {
	started       []Started
	skips         []Skip
	configChanged bool
}

// ciStartOne walks the candidates in order, building at most ciMaxSnapshots failure snapshots,
// and starts the first run the loop guard and the locked recheck allow.
func (e *Scheduler) ciStartOne(ctx context.Context, sched store.RunSchedule, repo store.GetRepoForUserRow, f forge.Forge, cfg CIRepairConfig, target workersvc.CIWatchTarget, body string, candidates []*ciKey) (ciStartResult, error) {
	var res ciStartResult
	if conflict, cerr := e.codexOverrideConflict(ctx, sched); cerr != nil {
		return res, cerr
	} else if conflict {
		for _, k := range candidates {
			res.skips = append(res.skips, ciSkip(k, SkipCodexOverrideConflict))
		}
		return res, nil
	}

	for i, k := range candidates {
		if i >= ciMaxSnapshots {
			break
		}
		ps := store.PipelineStatus{PipelineID: k.run.ID, Ref: k.run.Branch, Sha: k.run.SHA, WebUrl: k.run.WebURL}
		snap, serr := workersvc.BuildFailureSnapshot(ctx, f, repo.ForgeProjectID, ps, cfg.MaxJobs, cfg.LogTailBytes)
		if serr != nil {
			if o, benign := ciForgeErrOutcomeBenign(serr); benign {
				res.skips = append(res.skips, Skip{Title: capField(k.run.Name, ciFieldCap), Reason: o, WebURL: capField(k.run.WebURL, ciURLCap)})
				return res, nil // the read budget is per fire: later candidates would hit it too
			}
			return res, serr
		}
		// The signature is taken from the original bounded snapshot, never from the
		// presentation-shortened description text.
		sig := workersvc.FailureSignature(snap)
		tgt := ciTargetFor(target, k)

		if reason := workersvc.CILoopGuard(k.ep, sig, cfg.MaxAttempts); reason != "" {
			switch terr := e.runs.TransitionCIWatchEpisode(ctx, workersvc.CIWatchTransition{
				CIWatchTarget: tgt, HaltReason: reason, Signature: sig, MaxAttempts: cfg.MaxAttempts,
			}); {
			case terr == nil:
				res.skips = append(res.skips, ciSkip(k, ciHaltSkip(reason)))
			case errors.Is(terr, workersvc.ErrCIWatchConfigChanged):
				res.configChanged = true
				return res, nil
			case errors.Is(terr, workersvc.ErrCIWatchCapacity):
				res.skips = append(res.skips, ciSkip(k, SkipCICapacity))
			case errors.Is(terr, workersvc.ErrCIWatchStale), errors.Is(terr, workersvc.ErrCIWatchAlreadyHandled):
				res.skips = append(res.skips, ciSkip(k, SkipCIAlreadyHandled))
			default:
				return res, terr
			}
			continue
		}

		desc, tooLarge := composeCIRepairDescription(body, guidanceOf(sched), ciEvidence{
			Workflow: k.run.Name, URL: k.run.WebURL, SHA: k.run.SHA, Jobs: snap.FailedJobs,
		}, cfg.MaxJobs)
		if tooLarge {
			res.skips = append(res.skips, ciSkip(k, SkipDescriptionTooLarge))
			return res, nil // the trusted prefix is shared by every candidate
		}
		projectPath, perr := f.ProjectCIConfigPath(ctx, repo.ForgeProjectID)
		if perr != nil {
			e.logger.Warn("scheduler: ci_watch ci_config_path, using the configured defaults", "schedule", sched.ID.String(), "error", perr)
			projectPath = ""
		}
		title := ciRepairTitle(k.run.Name, k.run.Number)
		run, cerr := e.runs.CreateCIRepairRun(ctx, workersvc.CIRepairRunParams{
			UserID: sched.UserID, RepoID: sched.RepoID, ScheduleID: sched.ID,
			Title: title, Description: desc, AutoApprove: sched.AutoApprove, WaitOnLimit: sched.WaitOnLimit,
			MrReworkEnabled: scheduleMrRework(sched), Model: scheduleModel(sched),
			OverrideSubagentModel: scheduleOverrideSubagentModel(sched),
			CredOverride:          scheduleCredentialOverride(sched), Explicit: scheduleHarness(sched),
			CIConfigPaths: workersvc.MergeCIConfigPaths(cfg.ConfigPaths, projectPath),
			Episode:       workersvc.CIWatchStart{CIWatchTarget: tgt, Signature: sig, MaxAttempts: cfg.MaxAttempts},
		})
		var halted *workersvc.CIWatchHaltedError
		switch {
		case cerr == nil:
			res.started = append(res.started, Started{RunID: run.ID, Title: title, WebURL: k.run.WebURL})
			return res, nil
		case errors.Is(cerr, workersvc.ErrActivePromptExists):
			res.skips = append(res.skips, ciSkip(k, SkipAlreadyRunning))
			return res, nil
		case errors.Is(cerr, workersvc.ErrCIWatchConfigChanged):
			res.configChanged = true
			return res, nil
		case errors.Is(cerr, workersvc.ErrCIWatchCapacity):
			res.skips = append(res.skips, ciSkip(k, SkipCICapacity))
		case errors.Is(cerr, workersvc.ErrCIWatchStale), errors.Is(cerr, workersvc.ErrCIWatchAlreadyHandled):
			res.skips = append(res.skips, ciSkip(k, SkipCIAlreadyHandled))
		case errors.As(cerr, &halted):
			// The locked row halted, or tripped the guard, between the unlocked read and the
			// lock. The create rolled back; record the halt in its own transaction.
			switch terr := e.runs.TransitionCIWatchEpisode(ctx, workersvc.CIWatchTransition{
				CIWatchTarget: tgt, HaltReason: halted.Reason, Signature: sig, MaxAttempts: cfg.MaxAttempts,
			}); {
			case terr == nil, errors.Is(terr, workersvc.ErrCIWatchStale), errors.Is(terr, workersvc.ErrCIWatchAlreadyHandled), errors.Is(terr, workersvc.ErrCIWatchCapacity):
			case errors.Is(terr, workersvc.ErrCIWatchConfigChanged):
				res.configChanged = true
				return res, nil
			default:
				return res, terr
			}
			res.skips = append(res.skips, ciSkip(k, ciHaltSkip(halted.Reason)))
		case errors.Is(cerr, workersvc.ErrRepoNotFound):
			return res, workersvc.ErrRepoNotFound
		case holdsOnceCredentialDisabled(sched, cerr):
			return res, cerr
		default:
			if reason, ok := skipReasonForErr(cerr); ok {
				e.logger.Info("scheduler: ci_watch fire skipped", "schedule", sched.ID.String(), "reason", cerr)
				res.skips = append(res.skips, ciSkip(k, reason))
				return res, nil
			}
			return res, cerr // transient (or a permanent guardrail error advance() classifies)
		}
	}
	return res, nil
}

// ciForgeErrOutcomeBenign is the evidence-read half of ciForgeErrOutcome.
func ciForgeErrOutcomeBenign(err error) (SkipReason, bool) {
	switch {
	case errors.Is(err, forge.ErrCIReadBudget):
		return SkipCIEvidenceLimited, true
	case errors.Is(err, forge.ErrForgeVersionUnsupported):
		return SkipCIUnsupported, true
	}
	return "", false
}

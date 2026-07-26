// Package poller runs the background sync engine: on a fixed interval it pulls
// forge changes into uzi's issue cache for every enabled repo. Most ticks are
// cheap incremental pulls bounded by a per-repo high-water-mark; every Nth tick
// is a full reconcile that also evicts issues deleted or de-labeled forge-side
// (the incremental filter structurally cannot see those).
//
// This is the ChangeSource seam: a webhook receiver could feed the same
// FullSync/IncrementalSync methods later without touching the cache logic.
package poller

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"gitlab.example.com/vtmocanu/uzi/api/internal/forgesvc"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// defaultMaxConcurrency bounds how many repos are synced in parallel per tick.
const defaultMaxConcurrency = 4

// Engine polls all enabled repos. A single goroutine drives every repo each
// tick, holding per-repo state (high-water-mark + poll counter) in memory. The
// state map self-heals: repos that get disabled drop out of the enabled set and
// their state is discarded; newly enabled repos start with a full reconcile.
type Engine struct {
	svc            *forgesvc.Service
	q              *store.Queries
	interval       time.Duration
	reconcileEvery int
	maxConcurrency int

	states map[uuid.UUID]*repoState

	// forceReconcile carries an admin's "labels changed, resync everything" signal
	// from the settings PUT handler to the Run loop. Capacity 1 + a non-blocking
	// send (ForceReconcile) means the handler never blocks and rapid successive
	// PUTs coalesce into a single pending reconcile. Only the Run goroutine reads
	// it, and it mutates states in that same goroutine, so states needs no lock.
	forceReconcile chan struct{}

	// autopilot runs the post-sync autopilot detection (PRD #19 M4) as a sibling of
	// the MR-close watcher. Optional (nil-safe): nil disables detection, so tests
	// and any deployment without autopilot wiring keep the plain sync behaviour.
	// Set via SetAutopilot, mirroring workersvc's SetBroadcaster/SetLifecycle so
	// New's signature — and its existing callers — stay unchanged.
	autopilot *Autopilot

	// Pipeline-status watch (PRD #6). Set via SetPipelineWatch, same optional-
	// collaborator pattern as autopilot: pipelineMaxRefs <= 0 (the zero value when
	// unset, or CI_WATCH_MAX_REFS=0) disables the per-tick pipeline sync entirely,
	// so a poller built without SetPipelineWatch behaves exactly as before PRD #6.
	pipelineWindow  time.Duration
	pipelineMaxRefs int
}

type repoState struct {
	hwm       time.Time
	pollCount int
}

// New constructs an Engine. reconcileEvery < 1 is clamped to 1 (every poll is a
// full reconcile), so a misconfigured value degrades to "always correct, less
// efficient" rather than "never reconciles".
func New(svc *forgesvc.Service, q *store.Queries, interval time.Duration, reconcileEvery int) *Engine {
	if interval <= 0 {
		interval = time.Minute
	}
	if reconcileEvery < 1 {
		reconcileEvery = 1
	}
	return &Engine{
		svc:            svc,
		q:              q,
		interval:       interval,
		reconcileEvery: reconcileEvery,
		maxConcurrency: defaultMaxConcurrency,
		states:         make(map[uuid.UUID]*repoState),
		forceReconcile: make(chan struct{}, 1),
	}
}

// SetAutopilot wires the post-sync autopilot detector (PRD #19 M4). Call once at
// startup, before Run. A nil detector (the default) disables autopilot: the sync
// still runs, no runs are auto-created and no autopilot comments are posted.
func (e *Engine) SetAutopilot(a *Autopilot) { e.autopilot = a }

// SetPipelineWatch wires the PRD #6 pipeline-status sync into each tick. Call once
// at startup, before Run. maxRefs <= 0 (the default when unset, or an operator's
// CI_WATCH_MAX_REFS=0) disables it: the per-tick pipeline step is skipped and no
// badge cache is produced, reproducing pre-PRD-6 behaviour bit-for-bit. window is
// CI_WATCH_RUN_WINDOW, maxRefs is CI_WATCH_MAX_REFS.
func (e *Engine) SetPipelineWatch(window time.Duration, maxRefs int) {
	e.pipelineWindow = window
	e.pipelineMaxRefs = maxRefs
}

// ForceReconcile requests that the next tick full-syncs every enabled repo,
// dropping the incremental fast-path so a changed prd_label immediately re-filters
// each board (PRD #19 M2). It is non-blocking: it drops the signal when one is
// already pending (the Run loop will reconcile once regardless), so a caller — the
// settings PUT handler — never blocks on the poller. A no-op if the poller is not
// running (the buffered send simply fills or is dropped).
func (e *Engine) ForceReconcile() {
	select {
	case e.forceReconcile <- struct{}{}:
	default:
	}
}

// resetReconcileState makes the next tick a full reconcile for every repo by
// zeroing each known repo's poll counter (reconcileDue treats pollCount==1 — the
// value after the next tick's increment — as a reconcile). Runs only in the Run
// goroutine, the sole mutator of states, so no lock is needed.
func (e *Engine) resetReconcileState() {
	for _, st := range e.states {
		st.pollCount = 0
	}
}

// reconcileDue reports whether the given poll should be a full reconcile (with
// eviction) rather than an incremental pull: the first poll after (re)enable,
// then every reconcileEvery-th poll thereafter.
func reconcileDue(pollCount, reconcileEvery int) bool {
	return pollCount == 1 || pollCount%reconcileEvery == 0
}

// Run blocks until ctx is cancelled, ticking every interval. It does not run an
// immediate tick on start; the first pass happens one interval in (the compose
// stack is up by then and the board's first-open path seeds initial data).
func (e *Engine) Run(ctx context.Context) {
	ticker := time.NewTicker(e.interval)
	defer ticker.Stop()
	slog.Info("poller started", "interval", e.interval.String(), "reconcile_every", e.reconcileEvery)
	for {
		select {
		case <-ctx.Done():
			slog.Info("poller stopped")
			return
		case <-e.forceReconcile:
			// A settings change asked for a resync: forget the incremental state so
			// the next tick full-syncs every repo. Cheap and in-goroutine; the tick
			// itself does the forge work.
			slog.Info("poller: force reconcile requested")
			e.resetReconcileState()
		case <-ticker.C:
			e.tick(ctx)
		}
	}
}

// tick syncs every enabled repo once, with bounded concurrency and a hard
// per-tick deadline so one slow forge call can neither stall the cycle nor let
// a tick run longer than the poll interval. Errors on one repo are logged and
// skipped so a single bad connection (revoked PAT, forge down) never stalls the
// others.
func (e *Engine) tick(ctx context.Context) {
	// Cap the whole tick at one interval: even under bounded concurrency, a
	// pile-up of slow forges can't run past the next scheduled tick.
	tickCtx, cancel := context.WithTimeout(ctx, e.interval)
	defer cancel()

	repos, err := e.q.ListEnabledReposWithConnections(tickCtx)
	if err != nil {
		slog.Error("poller: list enabled repos", "error", err)
		return
	}

	// Pre-create every repo's state entry in this goroutine so the workers below
	// only ever touch their own (distinct) *repoState — no map writes race, no
	// per-state lock needed. Ticks never overlap (the ticker loop calls tick
	// synchronously and we wait for all workers before returning).
	seen := make(map[uuid.UUID]struct{}, len(repos))
	for _, r := range repos {
		seen[r.ID] = struct{}{}
		if e.states[r.ID] == nil {
			e.states[r.ID] = &repoState{}
		}
	}

	sem := make(chan struct{}, e.maxConcurrency)
	var wg sync.WaitGroup
	for _, r := range repos {
		wg.Add(1)
		sem <- struct{}{}
		go func(r store.ListEnabledReposWithConnectionsRow, st *repoState) {
			defer wg.Done()
			defer func() { <-sem }()
			e.syncRepo(tickCtx, r, st)
		}(r, e.states[r.ID])
	}
	wg.Wait()

	// Drop state for repos no longer enabled so a re-enable starts fresh (full
	// reconcile) and the map does not grow without bound.
	for id := range e.states {
		if _, ok := seen[id]; !ok {
			delete(e.states, id)
		}
	}
}

// syncRepo runs one repo's sync using the caller-provided state (which only this
// worker touches this tick).
func (e *Engine) syncRepo(ctx context.Context, r store.ListEnabledReposWithConnectionsRow, st *repoState) {
	f, err := e.svc.ForgeForConnection(r.ForgeType, r.BaseUrl, r.TokenCiphertext)
	if err != nil {
		slog.Error("poller: build forge client", "repo", r.PathWithNamespace, "error", err)
		return
	}

	st.pollCount++

	if reconcileDue(st.pollCount, e.reconcileEvery) {
		maxUpdated, err := e.svc.FullSync(ctx, r.ID, r.ForgeProjectID, f)
		if err != nil {
			slog.Error("poller: full sync", "repo", r.PathWithNamespace, "error", err)
			return
		}
		if maxUpdated.After(st.hwm) {
			st.hwm = maxUpdated
		}
	} else {
		newHWM, err := e.svc.IncrementalSync(ctx, r.ID, r.ForgeProjectID, f, st.hwm)
		if err != nil {
			slog.Error("poller: incremental sync", "repo", r.PathWithNamespace, "error", err)
			return
		}
		st.hwm = newHWM
	}

	// MR-close watcher (PRD #24): after the issue cache is fresh, check each
	// watched card's MR for an opened↔closed edge and move the card accordingly.
	// Runs only after a successful issue sync (an early return above skips it when
	// the forge is unreachable). Per-candidate errors are log-and-skipped inside;
	// only a candidate-enumeration failure surfaces here.
	if err := e.svc.SyncMRStates(ctx, r.ID, r.ForgeProjectID, f); err != nil {
		slog.Error("poller: sync MR states", "repo", r.PathWithNamespace, "error", err)
	}

	// PRD-link patch (PRD #72 M5): once a run's MR has merged, rewrite the issue's
	// own `prds/*.md` link to the path the run moved the file to. Placed HERE — after
	// SyncMRStates, before the filed→close sync — because it needs a live forge
	// client and must not run when the forge is unreachable, which the early returns
	// above already guarantee. It makes NO use of the issue cache (it reads the
	// description live via GetIssue, since the cache has no description column), so
	// unlike SyncFiledIssueCloses it does not depend on the sync ordering.
	if err := e.svc.SyncPRDLinkPatches(ctx, r.ID, r.ForgeProjectID, f); err != nil {
		slog.Error("poller: sync PRD link patches", "repo", r.PathWithNamespace, "error", err)
	}

	// Filed→Done judge sync (PRD #98 M6): with the issue cache fresh, move any
	// recommendation whose filed issue (#68) has just been observed CLOSED to Done —
	// once, on the open→closed edge, never overwriting a human's own verdict. Reads the
	// cache the sync above just wrote and makes NO forge call, so it costs nothing on the
	// wire and, like the MR watcher, is skipped entirely by the early returns when the
	// forge is unreachable (a stale cache must not manufacture edges).
	if err := e.svc.SyncFiledIssueCloses(ctx, r.ID); err != nil {
		slog.Error("poller: sync filed-issue closes", "repo", r.PathWithNamespace, "error", err)
	}

	// Pipeline-status sync (PRD #6): refresh the CI-badge cache for the repo's
	// default branch + its watched agent run branches. Rides this tick (no second
	// loop, no new interval); disabled when SetPipelineWatch was not called or
	// CI_WATCH_MAX_REFS=0. Eviction aligns with the issue reconcile (same pollCount).
	if e.pipelineMaxRefs > 0 {
		defaultBranch := ""
		if r.DefaultBranch.Valid {
			defaultBranch = r.DefaultBranch.String
		}
		if err := e.svc.SyncPipelines(ctx, r.ID, r.ForgeProjectID, f, forgesvc.PipelineSyncOptions{
			DefaultBranch: defaultBranch,
			Window:        e.pipelineWindow,
			MaxRefs:       e.pipelineMaxRefs,
			Evict:         reconcileDue(st.pollCount, e.reconcileEvery),
		}); err != nil {
			slog.Error("poller: sync pipelines", "repo", r.PathWithNamespace, "error", err)
		}
	}

	// Autopilot detection (PRD #19 M4): also post-sync, a sibling of the MR-close
	// watcher, reading the same fresh issue cache. It turns an autopilot-label
	// application on a PRD issue into an auto_approve run for the mapped consenting
	// user (or one explanatory issue comment). All per-issue errors are handled
	// inside; nothing surfaces here.
	if e.autopilot != nil {
		e.autopilot.detect(ctx, r, f)
	}
}

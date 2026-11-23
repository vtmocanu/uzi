// Package poller runs the background sync engine: on a fixed interval it pulls
// forge changes into uzi's issue cache for every enabled repo. Most ticks are
// cheap incremental pulls, each of the sync's two fetches bounded by its own
// per-repo high-water mark (forgesvc.Marks); every Nth tick is a full reconcile
// that also evicts issues deleted or de-labeled forge-side (the incremental
// filter structurally cannot see those).
//
// This is the ChangeSource seam: a webhook receiver could feed the same
// FullSync/IncrementalSync methods later without touching the cache logic.
package poller

import (
	"context"
	"errors"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forgesvc"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// projectReverseSyncer is the narrow reverse (Status → label) sync collaborator the
// per-tick sibling calls (PRD #364 M6). *forgesvc.ProjectSyncService satisfies it.
// Kept as an interface so the poller's tests can inject a fake without the full
// Projects v2 dependency surface.
type projectReverseSyncer interface {
	ReverseSync(ctx context.Context, repoID uuid.UUID) error
}

// RepoStore is the poller's read-only enabled-repository enumeration seam.
type RepoStore interface {
	ListEnabledReposWithConnections(context.Context) ([]store.ListEnabledReposWithConnectionsRow, error)
}

// ImportStateStore is the poller's write seam for a repo's durable first-import
// status (issue #4399). Enabling a repo records a 'waiting' import; the poller
// claims it at the generation it read, runs a FullSync, and finishes it. Both
// statements are generation-guarded in SQL, so a claim or finish from a stale
// generation (the repo was disabled and re-enabled meanwhile) writes zero rows.
// *store.Queries satisfies it. Kept apart from RepoStore so that seam stays
// read-only and its fakes unchanged.
type ImportStateStore interface {
	ClaimRepoInitialImport(ctx context.Context, arg store.ClaimRepoInitialImportParams) (int64, error)
	FinishRepoInitialImport(ctx context.Context, arg store.FinishRepoInitialImportParams) (int64, error)
}

// Initial-import states, mirroring the repos_initial_import_state_check constraint.
const (
	importWaiting   = "waiting"
	importRunning   = "running"
	importSucceeded = "succeeded"
	importFailed    = "failed"
)

// importWriteTimeout bounds each claim/finish write. The writes use a context
// detached from the tick so a tick deadline that already fired (the very failure
// being recorded) cannot stop the row from leaving 'running'.
const importWriteTimeout = 5 * time.Second

// defaultMaxConcurrency bounds how many repos are synced in parallel per tick.
const defaultMaxConcurrency = 4

// Engine polls all enabled repos. A single goroutine drives every repo each
// tick, holding per-repo state (high-water marks + poll counter) in memory. The
// state map self-heals: repos that get disabled drop out of the enabled set and
// their state is discarded; newly enabled repos start with a full reconcile.
type Engine struct {
	svc            *forgesvc.Service
	q              RepoStore
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

	// ciAutoFix runs the post-pipeline-sync CI-autofix detection (PRD #71 M6) as a
	// sibling of the autopilot detector. Optional (nil-safe): nil disables detection —
	// the instance kill-switch — so tests and any deployment without CI-autofix wiring
	// keep the plain sync behaviour. Set via SetCIAutoFix. It also fires only when the
	// pipeline watch is on (pipelineMaxRefs > 0), since it reads the pipeline cache the
	// same tick populates.
	ciAutoFix *CIAutoFix

	// mrReviewWatch runs the post-SyncMRStates MR-review-rework detection (PRD #700 M3)
	// as a sibling of the CI-autofix detector. Optional (nil-safe): nil disables
	// detection — the instance kill-switch — so tests and any deployment without
	// MR-review-watcher wiring keep the plain sync behaviour. Set via SetMRReviewWatch.
	// Like ciAutoFix it fires only when the pipeline watch is on (pipelineMaxRefs > 0),
	// since it reads the same pipeline cache for the green-head-pipeline gate.
	mrReviewWatch *MRReviewWatch

	// projectReverse runs the per-tick GitHub Projects v2 reverse (Status → label)
	// sync (PRD #364 M6) as a sibling of the other post-sync steps. Optional
	// (nil-safe): nil disables reverse sync — a deployment or test without
	// SetProjectReverseSync keeps forward-only behaviour. Set via
	// SetProjectReverseSync, the same optional-collaborator pattern as autopilot/
	// ciAutoFix. Unlike those, it runs EVERY tick (not gated on reconcileDue): a
	// GitHub-side drag must be picked up on the poll cadence regardless of whether the
	// tick is a full reconcile.
	projectReverse projectReverseSyncer

	// forgeTimeout is the per-forge-call HTTP timeout (config.ForgeHTTPTimeout). It
	// is used ONLY to floor the per-tick deadline (tickBudget, at 3x) so a poll
	// interval shorter than the forge calls a tick makes — the e2e harness pins 2s,
	// well under the 15s default — cannot preempt an in-flight sync call. Optional
	// (zero => no floor, tick budget stays exactly interval), set via SetForgeTimeout,
	// the same optional-collaborator pattern as autopilot/pipeline/ciAutoFix above
	// (issue #139).
	forgeTimeout time.Duration

	// beat is the admin-health loop-beat callback (PRD #1484 M2): main.go injects a
	// func(){ registry.Beat("poller") } so the `loops` health check can see this loop is
	// still ticking. Optional and nil-safe (nil = no beat), so existing tests that
	// construct the Engine with no beat keep working. It is a plain func — this package
	// must NOT import healthsvc (the registry is a leaf; the loop only receives a callback).
	beat        func()
	syncAttempt func(uuid.UUID) func(bool, forge.ErrorClass)

	// Initial-import seam (issue #4399). RequestImport (any goroutine) adds a repo to
	// importReq under importMu and pokes importWake (capacity 1, non-blocking send,
	// so requests coalesce). Only the Run goroutine drains it (importRequested), so
	// the import runs serialized with ticks and states still needs no lock; never
	// call PollOnce from elsewhere to get this effect. importStore nil disables all
	// state writes (set via SetImportStateStore before Run).
	importWake  chan struct{}
	importMu    sync.Mutex
	importReq   map[uuid.UUID]struct{}
	importStore ImportStateStore

	// itSyncAttempt observes a linked repo's issue sync result (the issue_tracker.sync
	// health check); the forge.sync observation of a linked repo is a code-host probe.
	itSyncAttempt func(uuid.UUID) func(bool, forge.ErrorClass)
}

// repoState is one repo's in-memory sync state. marks holds a SEPARATE
// high-water mark per sync fetch (issue #177): a single shared one stalled
// permanently on any repo with open issues but no PRD-labelled ones, so every
// incremental pull re-read that repo's whole open set. Process-local by design —
// nothing persists it. On restart the map is empty and pollCount is 0, so the
// first tick reconciles and FullSync re-seeds both marks from its own result;
// there is no stale mark to migrate or misread.
type repoState struct {
	marks     forgesvc.Marks
	pollCount int
	// autopilotSkipLogged: the linked-repo autopilot skip is logged once per repo.
	autopilotSkipLogged bool
}

// New constructs an Engine. reconcileEvery < 1 is clamped to 1 (every poll is a
// full reconcile), so a misconfigured value degrades to "always correct, less
// efficient" rather than "never reconciles".
func New(svc *forgesvc.Service, q RepoStore, interval time.Duration, reconcileEvery int) *Engine {
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
		importWake:     make(chan struct{}, 1),
		importReq:      make(map[uuid.UUID]struct{}),
	}
}

// SetAutopilot wires the post-sync autopilot detector (PRD #19 M4). Call once at
// startup, before Run. A nil detector (the default) disables autopilot: the sync
// still runs, no runs are auto-created and no autopilot comments are posted.
func (e *Engine) SetAutopilot(a *Autopilot) { e.autopilot = a }

// SetCIAutoFix wires the post-pipeline-sync CI-autofix detector (PRD #71 M6). Call
// once at startup, before Run. A nil detector (the default) disables CI-autofix: the
// sync still runs, no automatic ci_fix runs are created and no autofix comments are
// posted. NOT wiring it is the instance kill-switch, exactly like SetAutopilot.
func (e *Engine) SetCIAutoFix(d *CIAutoFix) { e.ciAutoFix = d }

// SetMRReviewWatch wires the post-SyncMRStates MR-review-rework detector (PRD #700
// M3). Call once at startup, before Run. A nil detector (the default) disables the
// MR-review watcher: the sync still runs, no automatic mr_rework runs are created and
// no rework comments are posted. NOT wiring it is the instance kill-switch, exactly
// like SetCIAutoFix.
func (e *Engine) SetMRReviewWatch(d *MRReviewWatch) { e.mrReviewWatch = d }

// SetProjectReverseSync wires the per-tick GitHub Projects v2 reverse (Status →
// label) sync (PRD #364 M6). Call once at startup, before Run. A nil syncer (the
// default) disables reverse sync: the tick's forward and issue-sync behaviour is
// unchanged. NOT wiring it is the reverse kill-switch, exactly like SetCIAutoFix.
func (e *Engine) SetProjectReverseSync(s projectReverseSyncer) { e.projectReverse = s }

// SetPipelineWatch wires the PRD #6 pipeline-status sync into each tick. Call once
// at startup, before Run. maxRefs <= 0 (the default when unset, or an operator's
// CI_WATCH_MAX_REFS=0) disables it: the per-tick pipeline step is skipped and no
// badge cache is produced, reproducing pre-PRD-6 behaviour bit-for-bit. window is
// CI_WATCH_RUN_WINDOW, maxRefs is CI_WATCH_MAX_REFS.
func (e *Engine) SetPipelineWatch(window time.Duration, maxRefs int) {
	e.pipelineWindow = window
	e.pipelineMaxRefs = maxRefs
}

// SetForgeTimeout records the per-forge-call HTTP timeout so tickBudget can floor
// the per-tick deadline at 2x it. Call once at startup, before Run. Without it (the
// zero value) the tick budget is exactly the poll interval — the pre-#139
// behaviour. This decouples the tick DEADLINE from the poll CADENCE: a short
// interval keeps its fast cadence, but a single tick is always granted enough time
// for the forge round-trips it makes, so a healthy forge is never cancelled mid-call.
func (e *Engine) SetForgeTimeout(d time.Duration) { e.forgeTimeout = d }

// SetBeat wires the admin-health loop-beat callback (PRD #1484 M2). Call once at startup,
// before Run. A nil beat (the default) disables it, so tests that never wire one behave
// exactly as before. The callback fires once per tick.
func (e *Engine) SetBeat(beat func()) { e.beat = beat }

// SetSyncAttempt wires issue-sync observations before Run or PollOnce.
func (e *Engine) SetSyncAttempt(begin func(uuid.UUID) func(bool, forge.ErrorClass)) {
	e.syncAttempt = begin
}

// SetImportStateStore wires the durable first-import status writes (issue #4399).
// Call once at startup, before Run. A nil store (the default) disables them: the
// poller then never claims or finishes an import and syncs exactly as before.
func (e *Engine) SetImportStateStore(s ImportStateStore) { e.importStore = s }

// RequestImport asks the Run loop to run repoID's owed first import now instead of
// waiting for the next tick. It is non-blocking and coalescing: repeated requests
// for the same repo collapse to one entry, and a pending wake absorbs the rest, so a
// request handler never waits on the poller. It is only a hint: the repo is synced
// only if its row still says an import is owed, and the regular ticks pick up any
// owed import whose hint was lost (or when Run is not running).
func (e *Engine) RequestImport(repoID uuid.UUID) {
	e.importMu.Lock()
	e.importReq[repoID] = struct{}{}
	e.importMu.Unlock()
	select {
	case e.importWake <- struct{}{}:
	default:
	}
}

// SetIssueTrackerSyncAttempt wires linked-repo issue sync observations before Run or
// PollOnce. A linked repo's forge sync observation is a code-host probe instead.
func (e *Engine) SetIssueTrackerSyncAttempt(begin func(uuid.UUID) func(bool, forge.ErrorClass)) {
	e.itSyncAttempt = begin
}

// Interval reports the poll cadence the loop actually ticks at (after New's clamp), so
// main.go registers the loop-beat with the effective interval rather than duplicating the
// clamp (PRD #1484 M2).
func (e *Engine) Interval() time.Duration { return e.interval }

// ForceReconcile requests that the next tick full-syncs every enabled repo,
// dropping the incremental fast-path so a changed uzi_label immediately re-filters
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

// tickBudget is the deadline for one tick: the poll interval, floored at 3x the
// forge HTTP timeout. Without the floor, an interval shorter than the forge calls a
// tick makes (the e2e harness pins 2s, well under the 15s FORGE_HTTP_TIMEOUT
// default) makes the tick's context deadline fire before the forge client's own
// per-call timeout, cancelling an in-flight ListIssues with a spurious "context
// deadline exceeded" (issue #139) and, worse, risking interruption of the post-sync
// autopilot/ci-fix detectors mid-operation. A zero forgeTimeout disables the floor
// (budget == interval), preserving the pre-#139 behaviour for callers that never
// wire it.
//
// The floor is 3x, not 1x: FullSync makes THREE sequential ListIssues calls — the
// uzi-labeled fetch, the open fetch, and the finding-labeled fetch (PRD #1183 Child B,
// added so a CLOSED finding issue reaches the cache for the filed→Done sync) — and the
// tick opens with a ListEnabledReposWithConnections DB query before any forge call, so
// a smaller budget could cancel the third call mid-flight. config.go clamps the sweep
// timeouts to 2x for the same kind of reason, but sweep clears only two forge calls;
// the poller's tick now clears three, so its floor is one multiple wider.
//
// This floors the DEADLINE only, not the cadence: the Run ticker still fires every
// interval, and because ticks never overlap (the Run loop is single-goroutine), a
// budget larger than the interval only lets Go's ticker coalesce — it never runs two
// ticks at once, and a tick that overruns the interval merely delays the next one.
// The common fast path (a responsive forge answering in ms) still completes far
// inside the interval, so a short poll cadence is preserved.
func tickBudget(interval, forgeTimeout time.Duration) time.Duration {
	if floor := 3 * forgeTimeout; forgeTimeout > 0 && interval < floor {
		return floor
	}
	return interval
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
		case <-e.importWake:
			e.importRequested(ctx)
		case <-ticker.C:
			if e.beat != nil {
				e.beat() // admin-health loop-beat: this loop ticked (PRD #1484 M2)
			}
			e.PollOnce(ctx)
		}
	}
}

// guardPanic runs fn and converts a panic into a logged error, so one repo's
// hostile-forge response (e.g. a nil-pointer panic that slips past a driver
// guard) cannot crash the shared poller process — it degrades to that one
// repo's sync being skipped this tick. The repo path is logged for diagnosis.
func guardPanic(repo string, fn func()) {
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("poller: repo sync panicked", "repo", repo, "panic", rec, "stack", string(debug.Stack()))
		}
	}()
	fn()
}

// PollOnce syncs every enabled repo once, with bounded concurrency and a hard per-tick
// deadline (tickBudget) so a dead or wedged forge can neither stall the cycle nor
// let a tick run unbounded. Errors on one repo are logged and skipped so a single
// bad connection (revoked PAT, forge down) never stalls the others. Call serially;
// do not call PollOnce concurrently with Run or another PollOnce.
func (e *Engine) PollOnce(ctx context.Context) {
	// Bound the whole tick at tickBudget — the poll interval, but floored at 3x the
	// forge HTTP timeout so a poll interval shorter than the tick's forge calls (the
	// e2e 2s cadence) can't preempt an in-flight sync call (issue #139). When the
	// floor applies, a slow-but-alive forge lets the tick run past the interval; that
	// is fine — ticks never overlap (see below), so it only delays the next tick.
	tickCtx, cancel := context.WithTimeout(ctx, tickBudget(e.interval, e.forgeTimeout))
	defer cancel()

	repos, err := e.q.ListEnabledReposWithConnections(tickCtx)
	if err != nil {
		slog.Error("poller: list enabled repos", "error", err)
		return
	}

	e.pollRepos(tickCtx, repos)

	// Pruning is driven by the COMPLETE enabled set, never by a subset run through
	// pollRepos (see importRequested).
	seen := make(map[uuid.UUID]struct{}, len(repos))
	for _, r := range repos {
		seen[r.ID] = struct{}{}
	}
	e.pruneStates(seen)
	if e.svc != nil {
		repoIDs := make([]uuid.UUID, 0, len(repos))
		for _, r := range repos {
			repoIDs = append(repoIDs, r.ID)
		}
		e.svc.ReconcileMergedPredecessors(ctx, repoIDs)
	}
}

// pollRepos syncs the given repos once with bounded concurrency under ctx (the tick
// context). It pre-creates every repo's state entry in this goroutine so the workers
// only ever touch their own (distinct) *repoState: no map writes race, no per-state
// lock needed. Only the Run goroutine (or a serial PollOnce) calls it, and it waits
// for all workers before returning, so syncs never overlap. It never prunes: the
// caller may pass a subset of the enabled repos.
func (e *Engine) pollRepos(ctx context.Context, repos []store.ListEnabledReposWithConnectionsRow) {
	for _, r := range repos {
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
			guardPanic(r.PathWithNamespace, func() { e.syncRepo(ctx, r, st) })
		}(r, e.states[r.ID])
	}
	wg.Wait()
}

// importRequested runs the first import for the repos RequestImport named, inside
// the Run goroutine (so it is serialized with ticks and the states map needs no
// lock). It drains the request set, lists the enabled repos under a tick-budget
// deadline, keeps those that were requested AND still owe an import (waiting or
// running; a failed one is retried by the regular ticks, a succeeded or legacy NULL
// one is skipped), and syncs just those. It does not prune and does not run the
// per-tick ReconcileMergedPredecessors: other repos' state is left intact.
func (e *Engine) importRequested(ctx context.Context) {
	e.importMu.Lock()
	req := e.importReq
	e.importReq = make(map[uuid.UUID]struct{})
	e.importMu.Unlock()
	if len(req) == 0 {
		return
	}

	tickCtx, cancel := context.WithTimeout(ctx, tickBudget(e.interval, e.forgeTimeout))
	defer cancel()
	repos, err := e.q.ListEnabledReposWithConnections(tickCtx)
	if err != nil {
		slog.Error("poller: list enabled repos for requested import", "error", err)
		return
	}
	owed := make([]store.ListEnabledReposWithConnectionsRow, 0, len(req))
	for _, r := range repos {
		if _, ok := req[r.ID]; !ok {
			continue
		}
		if s := r.InitialImportState; s.Valid && (s.String == importWaiting || s.String == importRunning) {
			owed = append(owed, r)
		}
	}
	e.pollRepos(tickCtx, owed)
}

// importOwed reports whether the row owes a first import the poller should run.
// A NULL state is a legacy repo and is never owed.
func importOwed(r store.ListEnabledReposWithConnectionsRow) bool {
	if !r.InitialImportState.Valid {
		return false
	}
	switch r.InitialImportState.String {
	case importWaiting, importRunning, importFailed:
		return true
	}
	return false
}

// importErrorClass classifies an import failure for the persisted error class: a
// deadline (the error's own or the tick context's) is a timeout, otherwise the
// forge's classification, which is other for anything unclassified.
func importErrorClass(ctx context.Context, err error) forge.ErrorClass {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return forge.ErrorClassTimeout
	}
	return forge.Class(err)
}

// claimImport claims the row's owed import at the generation it read. A write error
// is logged and treated as not claimed (the row stays as it was and a later tick
// retries); zero rows means the generation moved, so another enable owns the import.
func (e *Engine) claimImport(ctx context.Context, r store.ListEnabledReposWithConnectionsRow) bool {
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), importWriteTimeout)
	defer cancel()
	n, err := e.importStore.ClaimRepoInitialImport(wctx, store.ClaimRepoInitialImportParams{ID: r.ID, InitialImportGeneration: r.InitialImportGeneration})
	if err != nil {
		slog.Error("poller: claim initial import", "repo", r.PathWithNamespace, "error", err)
		return false
	}
	return n == 1
}

// finishImport settles a claimed import at its generation. The class is mapped into
// the closed set the column's CHECK accepts, so a future forge.ErrorClass value can
// never fail the write and leave the row 'running'. A stale generation writes zero
// rows, which is the intended no-op.
func (e *Engine) finishImport(ctx context.Context, r store.ListEnabledReposWithConnectionsRow, ok bool, class forge.ErrorClass) {
	arg := store.FinishRepoInitialImportParams{ID: r.ID, Generation: r.InitialImportGeneration, State: importSucceeded}
	if !ok {
		arg.State = importFailed
		switch class {
		case forge.ErrorClassTimeout, forge.ErrorClassAuth, forge.ErrorClassServerError, forge.ErrorClassRateLimited, forge.ErrorClassOther:
		default:
			class = forge.ErrorClassOther
		}
		arg.ErrorClass = pgtype.Text{String: string(class), Valid: true}
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), importWriteTimeout)
	defer cancel()
	if _, err := e.importStore.FinishRepoInitialImport(wctx, arg); err != nil {
		slog.Error("poller: finish initial import", "repo", r.PathWithNamespace, "error", err)
	}
}

// pruneStates drops per-repo state for repos no longer enabled (not in seen), so a
// re-enable starts fresh (full reconcile) and no map grows without bound: the poller's
// own states and the sync service's pipeline ref-cap state alike.
func (e *Engine) pruneStates(seen map[uuid.UUID]struct{}) {
	for id := range e.states {
		if _, ok := seen[id]; !ok {
			delete(e.states, id)
		}
	}
	if e.svc != nil {
		e.svc.PrunePipelineCapState(seen)
	}
}

// syncRepo runs one repo's sync using the caller-provided state (which only this
// worker touches this tick).
func (e *Engine) syncRepo(ctx context.Context, r store.ListEnabledReposWithConnectionsRow, st *repoState) {
	var complete func(bool, forge.ErrorClass)
	if e.syncAttempt != nil {
		complete = e.syncAttempt(r.ID)
	}
	var itComplete func(bool, forge.ErrorClass)
	finished := false
	report := func(ok bool, class forge.ErrorClass) {
		if !finished {
			finished = true
			if complete != nil {
				complete(ok, class)
			}
		}
	}
	// itReport is the issue tracker attempt of a linked repo, begun only once the linked
	// issue sync is about to run; itBegun/itFinished make it complete exactly once.
	itBegun, itFinished := false, false
	itReport := func(ok bool, class forge.ErrorClass) {
		if itBegun && !itFinished {
			itFinished = true
			if itComplete != nil {
				itComplete(ok, class)
			}
		}
	}
	// A panic before issue-sync succeeds reports Other; guardPanic still owns recovery.
	defer func() {
		if !finished {
			report(false, forge.ErrorClassOther)
		}
		itReport(false, forge.ErrorClassOther)
	}()

	// Owed first import (issue #4399): claim BEFORE anything can fail, so the row
	// reads 'running' for the whole attempt, then force a full sync below. The
	// deferred finisher guarantees a claimed row never stays 'running': any exit not
	// explicitly settled (including a panic guardPanic recovers above) writes
	// failed/other. A lost claim (the generation moved) just syncs normally.
	claimed := e.importStore != nil && importOwed(r) && e.claimImport(ctx, r)
	importSettled := false
	settleImport := func(ok bool, class forge.ErrorClass) {
		if claimed && !importSettled {
			importSettled = true
			e.finishImport(ctx, r, ok, class)
		}
	}
	defer func() { settleImport(false, forge.ErrorClassOther) }()

	f, err := e.svc.ForgeForConnection(r.ForgeType, r.BaseUrl, r.TokenCiphertext)
	if err != nil {
		slog.Error("poller: build forge client", "repo", r.PathWithNamespace, "error", err)
		settleImport(false, importErrorClass(ctx, err))
		report(false, forge.Class(err))
		return
	}

	// The issue side. An unlinked repo's issue side IS the forge driver (the same value
	// as the code-host side, so every forge call stays byte-identical). A linked repo
	// (PRD #4173 M4) goes through the resolver, which binds repo, owner and connection
	// and wraps the driver in the write guard.
	linked := r.IssueTrackerConnectionID.Valid
	var tr forge.IssueTracker = f
	var trErr error
	if linked {
		tr, trErr = e.svc.IssueTrackerForRepo(ctx, r.UserID, r.ID)
		if trErr != nil {
			// Never hand a nil tracker to the issue-side steps below: they read through
			// it per candidate and would panic, and guardPanic would then skip every
			// remaining code-host step on every tick.
			tr = forgesvc.UnavailableIssueTracker(trErr)
		}
	}

	st.pollCount++

	// Target verification reads the bound MR directly and does not need a fresh
	// issue cache. Run it even when unlinked issue sync fails.
	syncActiveMRTargets := func() {
		if err := e.svc.SyncActiveMRTargets(ctx, r.ID, r.ForgeProjectID, f); err != nil {
			slog.Error("poller: sync active MR targets", "repo", r.PathWithNamespace, "error", err)
		}
	}

	if linked {
		// A linked repo's issue sync failing (issue tracker unreachable or its link
		// unresolvable) must never skip the code-host steps below: its MRs, pipelines
		// and CI do not depend on the issue tracker. The linked sync result feeds the
		// issue_tracker.sync attempt (class from forge.Class: its health action names the
		// issue tracker connection, so an auth class is right there), and the forge.sync
		// attempt is a code-host probe instead, so a revoked forge token turns forge.sync
		// red and an issue tracker outage does not. Unlike the unlinked branch this
		// carries on whatever either result. Cost: one extra GetProject per linked repo
		// per tick.
		if e.itSyncAttempt != nil {
			itComplete = e.itSyncAttempt(r.ID)
		}
		// Boundary: when the forge client cannot be built (the early return above) this
		// attempt is never started for the tick, and forge.sync is red then.
		itBegun = true
		//
		// An owed first import (issue #4399) forces the full form here as it does on an
		// unlinked repo, and settles on the linked sync's result alone: the import is
		// the issue cache's first fill, which the code-host probe below does not touch.
		if trErr != nil {
			slog.Error("poller: build linked issue tracker", "repo", r.PathWithNamespace, "error", trErr)
			settleImport(false, importErrorClass(ctx, trErr))
			itReport(false, forge.Class(trErr))
		} else if err := e.syncLinkedIssues(ctx, r, tr, st, claimed); err != nil {
			slog.Error("poller: linked issue sync", "repo", r.PathWithNamespace, "error", err)
			settleImport(false, importErrorClass(ctx, err))
			itReport(false, forge.Class(err))
		} else {
			settleImport(true, forge.ErrorClassOther)
			itReport(true, forge.ErrorClassOther)
		}
		_, perr := f.GetProject(ctx, r.ForgeProjectID)
		if perr != nil {
			slog.Error("poller: code host probe", "repo", r.PathWithNamespace, "error", perr)
		}
		report(perr == nil, forge.Class(perr))
	} else {
		if claimed || reconcileDue(st.pollCount, e.reconcileEvery) {
			observed, err := e.svc.FullSync(ctx, r.ID, r.ForgeProjectID, f)
			if err != nil {
				slog.Error("poller: full sync", "repo", r.PathWithNamespace, "error", err)
				settleImport(false, importErrorClass(ctx, err))
				report(false, forge.Class(err))
				syncActiveMRTargets()
				return
			}
			settleImport(true, forge.ErrorClassOther)
			// FullSync is unbounded and reports what it observed, not an advance; the
			// clamp is the caller's. Field-wise, so a fetch that legitimately came back
			// empty leaves its own mark alone rather than dragging the other's down.
			st.marks = st.marks.Advance(observed)
		} else {
			next, err := e.svc.IncrementalSync(ctx, r.ID, r.ForgeProjectID, f, st.marks)
			if err != nil {
				slog.Error("poller: incremental sync", "repo", r.PathWithNamespace, "error", err)
				report(false, forge.Class(err))
				syncActiveMRTargets()
				return
			}
			// Already clamped inside, and held at the caller's pair on any fetch error.
			st.marks = next
		}

		report(true, forge.ErrorClassOther)
	}

	// MR-close watcher (PRD #24): after the issue cache is fresh, check each
	// watched card's MR for an opened↔closed edge and move the card accordingly.
	// Runs after the issue sync: on an unlinked repo only after a successful one (an early
	// return above skips it when the forge is unreachable); on a linked repo regardless,
	// since an issue-side failure says nothing about the code host. Per-candidate errors are log-and-skipped inside;
	// only a candidate-enumeration failure surfaces here.
	if err := e.svc.SyncMRStates(ctx, r.ID, r.ForgeProjectID, f, tr); err != nil {
		slog.Error("poller: sync MR states", "repo", r.PathWithNamespace, "error", err)
	}

	// Board-free MR-state recorder (PRD #908, widened in issue #1253): board-free sibling of
	// SyncMRStates for every issue-less MR-bearing run (prompt/self_improve plus issue-less
	// helper runs like mr_rework/ci_fix/chat/task), so their runs.mr_state is fresh for the
	// mr_rework detector reading candidates later this same tick. Reads live forge state
	// (needs the forge reachable: guaranteed by the early returns above on an unlinked repo,
	// and by the forge client having been built on a linked one); records mr_state
	// and cancels an in-flight rework on close/merge. It moves NO board card, except that on
	// a linked repo an issue-lane run's MR close/reopen moves the card through the issue
	// tracker (PRD #4173 M5), which is why it takes the issue side. Per-candidate
	// errors are log-and-skipped inside; only a candidate-enumeration failure surfaces here.
	if err := e.svc.SyncBoardFreeMRStatesWithIssueSide(ctx, r.ID, r.ForgeProjectID, f, tr); err != nil {
		slog.Error("poller: sync board-free MR states", "repo", r.PathWithNamespace, "error", err)
	}

	syncActiveMRTargets()

	// PRD-link patch (PRD #72 M5): once a run's MR has merged, rewrite the issue's
	// own `prds/*.md` link to the path the run moved the file to. Placed HERE — after
	// SyncMRStates, before the filed→close sync — because it needs a live forge
	// client and must not run when the forge is unreachable, which the early returns
	// above guarantee for an unlinked repo (a linked repo runs it regardless of its
	// issue side). It makes NO use of the issue cache (it reads the
	// description live via GetIssue, since the cache has no description column), so
	// unlike SyncFiledIssueCloses it does not depend on the sync ordering.
	if err := e.svc.SyncPRDLinkPatches(ctx, r.ID, r.ForgeProjectID, f, tr); err != nil {
		slog.Error("poller: sync PRD link patches", "repo", r.PathWithNamespace, "error", err)
	}

	// Filed→Done judge sync (PRD #98 M6): with the issue cache fresh, move any
	// recommendation whose filed issue (#68) has just been observed CLOSED to Done —
	// once, on the open→closed edge, never overwriting a human's own verdict. Reads the
	// cache the sync above just wrote and makes NO forge call, so it costs nothing on the
	// wire and, like the MR watcher, is skipped entirely by the early returns when the
	// forge is unreachable on an unlinked repo (a stale cache must not manufacture edges;
	// a linked repo's cache holds only the eligibility tag's issues, which the linked sync wrote).
	if err := e.svc.SyncFiledIssueCloses(ctx, r.ID); err != nil {
		slog.Error("poller: sync filed-issue closes", "repo", r.PathWithNamespace, "error", err)
	}

	// Filed→Done findings sync (PRD #1183 Child B, M3): the finding twin of the judge sync above,
	// on the SAME fresh issue cache and with no forge call of its own. A filed incidental finding
	// whose issue was just observed CLOSED moves to Done, once, on the open→closed edge, and reopens
	// if the bug reappears. Placed immediately after the judge sync so both consume the same tick's
	// cache; skipped by the same early returns when the forge is unreachable (unlinked repos).
	if err := e.svc.SyncFindingIssueCloses(ctx, r.ID); err != nil {
		slog.Error("poller: sync finding-issue closes", "repo", r.PathWithNamespace, "error", err)
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
	// Skipped on a linked repo (PRD #4173 D12): the autopilot label is a forge label.
	if e.autopilot != nil {
		if linked {
			if !st.autopilotSkipLogged {
				st.autopilotSkipLogged = true
				slog.Info("poller: autopilot detection skipped for a repo linked to an issue tracker", "repo", r.PathWithNamespace)
			}
		} else {
			e.autopilot.detect(ctx, r, f, f)
		}
	}

	// CI-autofix detection (PRD #71 M6): also post-sync, a sibling of the autopilot
	// detector, reading the pipeline-status cache the SyncPipelines step above just
	// wrote. Gated on the pipeline watch being on (pipelineMaxRefs > 0) — with no fresh
	// pipeline cache there are no candidates and the ledger would read stale — and on the
	// detector being wired (the instance kill-switch). It turns an eligible failing agent
	// MR branch into an automatic ci_fix run (or one halt comment). All per-candidate
	// errors are handled inside; nothing surfaces here.
	if e.pipelineMaxRefs > 0 && e.ciAutoFix != nil {
		e.ciAutoFix.detect(ctx, r, f, tr)
	}

	// MR-review-rework detection (PRD #700 M3): also post-sync, a sibling of the
	// CI-autofix detector. It runs AFTER SyncMRStates above (Decision 10 — a fresh
	// close/merge is authoritative for the card move, and the detector gates on the
	// watcher-owned mr_state, so it never reworks a just-closed MR) and beside the
	// CI-autofix detect, reading the same fresh pipeline-status cache for the
	// green-head-pipeline gate. Gated on the pipeline watch being on and the detector
	// being wired (the instance kill-switch); the admin kill-switch + per-user opt-in +
	// per-MR cap gate activation inside it. All per-candidate errors are handled inside.
	if e.pipelineMaxRefs > 0 && e.mrReviewWatch != nil {
		e.mrReviewWatch.detect(ctx, r, f, tr)
	}

	// Reverse Status→label sync (PRD #364 M6): a genuine per-tick sibling (NOT gated
	// on reconcileDue) so a GitHub-side board drag is picked up on the poll cadence
	// regardless of whether this tick is a full reconcile. Only for GitHub repos and
	// only when a reverse syncer is wired (the instance kill-switch). ReverseSync is
	// best-effort and returns nil in practice; the error log is defensive. Ticks never
	// overlap (tick does wg.Wait()), but the forward path in the HTTP handler CAN
	// interleave with this read — the stored-marker no-op tolerates it (worst case a
	// redundant same-value label write).
	// Skipped on a linked repo: a project's items are forge issue numbers (PRD #4173 M4).
	if e.reverseSyncEligible(r.ForgeType) && !linked {
		if err := e.projectReverse.ReverseSync(ctx, r.ID); err != nil {
			slog.Error("poller: reverse project sync", "repo", r.PathWithNamespace, "error", err)
		}
	}
}

// syncLinkedIssues runs the linked repo's issue sync through the forgesvc
// dispatchers on the same full/incremental cadence as an unlinked repo, folding the
// marks only on success.
func (e *Engine) syncLinkedIssues(ctx context.Context, r store.ListEnabledReposWithConnectionsRow, tr forge.IssueTracker, st *repoState, forceFull bool) error {
	if forceFull || reconcileDue(st.pollCount, e.reconcileEvery) {
		observed, err := e.svc.SyncIssues(ctx, r.UserID, r.ID, r.ForgeProjectID, tr)
		if err != nil {
			return err
		}
		st.marks = st.marks.Advance(observed)
		return nil
	}
	next, err := e.svc.SyncIssuesIncremental(ctx, r.UserID, r.ID, r.ForgeProjectID, tr, st.marks)
	if err != nil {
		return err
	}
	st.marks = next
	return nil
}

// reverseSyncEligible reports whether the per-tick reverse (Status → label) sibling
// should fire for a repo of forgeType: only GitHub repos, and only when a reverse
// syncer is wired (the instance kill-switch by wiring). Extracted so the fire/skip
// gate can be exercised as production code rather than re-implemented in a test.
func (e *Engine) reverseSyncEligible(forgeType string) bool {
	return forgeType == string(forge.TypeGitHub) && e.projectReverse != nil
}

// Package runlifecycle drives the board's column automation off run status
// changes (PRD #12 M1). It is the subscriber workersvc notifies after every
// status write: for the four statuses the board reacts to it performs a
// forge-first label move (queued → In Progress, completed → Human Review,
// failed/cancelled → the origin column), guarded so it never fights a manual
// drag and never moves a closed issue.
//
// Two things make it robust to a down forge without ever blocking a run
// transition. First, the status write itself stamps runs.move_pending_since in
// the same statement (see the store queries), so a crash between persisting the
// status and moving the card leaves a durable "move owed" marker. Second, the
// inline Notify move is best-effort and off the request path; a background
// reconcile loop (RunReconciler) retries any run whose marker is still set,
// re-deriving the target from the run's CURRENT status via a total status→column
// map: every tick for the first 30 minutes, then at a slow cadence until a 24-hour
// give-up. The reconcile loop is deliberately separate from the liveness sweeper
// so a stalled forge can never delay worker-loss recovery.
package runlifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/board"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forgesvc"
	"github.com/vtmocanu/uzi/api/internal/issuesource"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Reconcile-loop tuning. None are PRD env vars: they only bound retry latency and
// per-pass work, and the loop is idempotent, so a misconfiguration degrades to
// "slower/faster retries", never to incorrectness.
const (
	// defaultInterval is the reconcile cadence. Distinct from (and looser than)
	// the 15s liveness sweep it must never share a goroutine with.
	defaultInterval = 30 * time.Second
	// defaultGrace keeps the loop from racing the inline Notify move: a marker is
	// only retried once it is older than this.
	defaultGrace = 15 * time.Second
	// defaultFastWindow is the fast-lane window: a marker younger than this (and
	// older than defaultGrace) is retried every tick.
	defaultFastWindow = 30 * time.Minute
	// defaultSlowInterval is the minimum spacing of the slow lane, which retries
	// markers between defaultFastWindow and defaultGiveup old.
	defaultSlowInterval = 10 * time.Minute
	// defaultGiveup is the end of retries. A marker older than this is left set
	// (visible drift, not a silent correct-looking badge), never retried, and only
	// cleared by the next transition or a manual drag.
	defaultGiveup = 24 * time.Hour
	// defaultSlowBatch caps runs reconciled per slow pass.
	defaultSlowBatch = 25
	// defaultSlowTimeout bounds one slow pass so a stalled forge cannot spend more
	// than this of the fast lane's pass budget.
	defaultSlowTimeout = 10 * time.Second
	// defaultWarnTimeout bounds the crossing-warning scans after the fast lane.
	defaultWarnTimeout = 2 * time.Second
	// warnScanCap mirrors the LIMIT 100 of ListGaveUpColumnMoves: a scan returning
	// this many rows may have been truncated.
	warnScanCap = 100
	// defaultBatch caps runs reconciled per pass.
	defaultBatch = 100
	// notifyTimeout bounds one inline move (DB reads + one forge call, itself
	// already capped by FORGE_HTTP_TIMEOUT).
	notifyTimeout = 30 * time.Second
)

// Store is the narrow set of generated queries the automation uses. *store.Queries
// satisfies it; tests supply a fake.
type Store interface {
	GetRunMoveContext(ctx context.Context, runID uuid.UUID) (store.GetRunMoveContextRow, error)
	GetIssueByIID(ctx context.Context, arg store.GetIssueByIIDParams) (store.Issue, error)
	ListBoardColumns(ctx context.Context, repoID uuid.UUID) ([]store.BoardColumn, error)
	RecordRunColumnMove(ctx context.Context, arg store.RecordRunColumnMoveParams) (int64, error)
	ClearRunMovePending(ctx context.Context, id uuid.UUID) (int64, error)
	ListPendingColumnMoves(ctx context.Context, arg store.ListPendingColumnMovesParams) ([]uuid.UUID, error)
	ListGaveUpColumnMoves(ctx context.Context, arg store.ListGaveUpColumnMovesParams) ([]store.ListGaveUpColumnMovesRow, error)
	ClaimAutopilotTerminalComment(ctx context.Context, id uuid.UUID) (int64, error)
}

// Mover builds a forge client from a stored connection and applies the shared
// forge-first single-column move. *forgesvc.Service satisfies it.
type Mover interface {
	ForgeForConnection(forgeType, baseURL string, tokenCiphertext []byte) (forge.Forge, error)
	AutoMove(ctx context.Context, f forge.IssueTracker, forgeProjectID int64, issue store.Issue, columns []store.BoardColumn, target string) (store.Issue, error)
}

// Projector is an OPTIONAL collaborator that projects a uzi-originated column move
// onto a linked GitHub Projects v2 Status (PRD #364 M5). *forgesvc.ProjectSyncService
// satisfies it. It is nil unless wired via SetProjector; when nil the lifecycle
// simply skips the projection. Best-effort: a returned error is logged, never
// propagated (a failed projection must not fail a run's column move).
type Projector interface {
	ForwardMove(ctx context.Context, repoID uuid.UUID, issueIID int64, targetColumn string) error
}

// Lifecycle is the automation subscriber and reconcile loop.
type Lifecycle struct {
	q     Store
	mover Mover
	now   func() time.Time

	// projector projects a successful uzi-originated move onto a linked GitHub
	// Projects v2 Status (PRD #364 M5). Optional; nil skips the projection.
	projector Projector

	// issueSources resolves the issue side of a repo linked to an external issue
	// tracker (PRD #4173 DL-5); SetIssueSources. Nil ⇒ a linked repo fails closed
	// (issuesource.ErrNotWired) and an unlinked one keeps using the mover's forge.
	issueSources issuesource.Sources

	// frontendOrigin is the user-facing origin (config.FrontendOrigin) the
	// terminal-comment hook builds run links from. Empty omits the link.
	frontendOrigin string

	interval time.Duration
	grace    time.Duration
	giveup   time.Duration
	batch    int32

	// Two-tier retry tuning (see Reconcile). fastWindow is the age at which a marker
	// moves from the fast lane to the slow lane; giveup is where retries end.
	fastWindow   time.Duration
	slowInterval time.Duration
	slowBatch    int32
	slowTimeout  time.Duration
	warnTimeout  time.Duration

	// Reconcile state. Reconcile is called from a single goroutine (RunReconciler),
	// so these fields are unsynchronized. lastSlow is when the slow lane last ran
	// (zero: never). warnFrom holds the per-threshold warning cursors: index 0 is
	// the fast-window threshold, index 1 the give-up threshold.
	lastSlow time.Time
	warnFrom [2]time.Time

	// beat is the admin-health loop-beat callback (PRD #1484 M2): main.go injects a
	// func(){ registry.Beat("lifecycle") } so the `loops` health check can see this loop is
	// still ticking. Optional and nil-safe. A plain func — this package must NOT import
	// healthsvc.
	beat func()

	// wg tracks in-flight async Notify goroutines so shutdown can drain them
	// before the pool closes.
	wg sync.WaitGroup
}

// New constructs a Lifecycle with the default tuning. frontendOrigin is the
// user-facing origin (config.FrontendOrigin) used to build run links in autopilot
// terminal comments; "" omits the link.
func New(q Store, mover Mover, frontendOrigin string) *Lifecycle {
	return &Lifecycle{
		q:              q,
		mover:          mover,
		now:            time.Now,
		frontendOrigin: strings.TrimRight(frontendOrigin, "/"),
		interval:       defaultInterval,
		grace:          defaultGrace,
		giveup:         defaultGiveup,
		batch:          defaultBatch,
		fastWindow:     defaultFastWindow,
		slowInterval:   defaultSlowInterval,
		slowBatch:      defaultSlowBatch,
		slowTimeout:    defaultSlowTimeout,
		warnTimeout:    defaultWarnTimeout,
	}
}

// SetProjector wires the optional GitHub Projects v2 projector in after
// construction (built in main alongside the other forge collaborators), mirroring
// the repo's optional-collaborator pattern. Safe to leave unset — the lifecycle
// then performs no Status projection.
func (l *Lifecycle) SetProjector(p Projector) { l.projector = p }

// SetIssueSources wires the resolver of a linked repo's issue side (PRD #4173 DL-5).
func (l *Lifecycle) SetIssueSources(is issuesource.Sources) { l.issueSources = is }

// SetBeat wires the admin-health loop-beat callback (PRD #1484 M2). Call once at startup,
// before RunReconciler. A nil beat (the default) disables it, so tests that never wire one
// behave exactly as before. The callback fires once per reconcile tick.
func (l *Lifecycle) SetBeat(beat func()) { l.beat = beat }

// Interval reports the reconcile cadence the loop ticks at, so main.go registers the
// loop-beat with the effective interval (PRD #1484 M2).
func (l *Lifecycle) Interval() time.Duration { return l.interval }

// moveContext is the run + connection facts one move needs, sourced identically
// by the notifier (GetRunMoveContext) and the reconciler (re-read per run).
type moveContext struct {
	repoID          uuid.UUID
	userID          uuid.UUID
	linked          bool   // the repo's issue_tracker_connection_id is set (PRD #4173)
	eligibilityName string // the link's eligibility tag name; "" on an unlinked repo
	issueIID        int64
	forgeProjectID  int64
	forgeType       string
	baseURL         string
	tokenCiphertext []byte
	originColumn    pgtype.Text
	boardColumn     pgtype.Text
	superseded      bool // a strictly later run exists on the same repo and issue (#4704)
}

// decision is the outcome of mapping a status to a target column: whether to move
// at all, and if not, whether the skip is definitive (clear the marker) or should
// leave the marker for the reconcile loop.
type decision struct {
	act    bool
	clear  bool
	target string
}

// notifierDecision maps the JUST-WRITTEN status to a target column. It ignores
// claimed/running/awaiting_approval on purpose: a running report cannot be
// distinguished from a resume, so re-dragging on it would fight manual placement
// (PRD Decision #1). Those statuses leave any pending marker for the reconciler,
// which uses the total map.
func notifierDecision(status string, origin pgtype.Text) decision {
	switch status {
	case "queued":
		return decision{act: true, target: board.ColumnInProgress}
	case "completed":
		return decision{act: true, target: board.ColumnHumanReview}
	case "failed", "cancelled":
		return restoreDecision(origin)
	default:
		return decision{act: false, clear: false}
	}
}

// reconcilerDecision maps a run's CURRENT status to a target column using the
// total map: every non-terminal working status is In Progress, because by retry
// time a queued run has usually advanced to claimed/running and reusing the
// notifier's partial map would no-op the most common retry forever (PRD
// Decision #3).
func reconcilerDecision(status string, origin pgtype.Text) decision {
	switch status {
	// limit_wait is In Progress (PRD #35): a parked run is working — it holds its
	// issue, its session and its branch, and it resumes on its own. It is here rather
	// than in the default arm because that arm is {act:false, clear:false}, which
	// leaves a pending move marker UNHEALED and trips the 30-minute pending warn — and a
	// park routinely outlasts 30 minutes by design, so the warn would be the normal
	// case rather than an exception. Notify's partial map is deliberately NOT
	// extended: a park moves no card, so there is nothing to notify about.
	//
	// awaiting_input (PRD #88) is here for exactly the same reasons, and a
	// clarification park outlasts 30 minutes just as routinely.
	//
	// awaiting_followup (PRD #517) is the interactive-task park and belongs here for
	// the same reasons: a run parked after signal_done still holds its issue, session
	// and branch, resumes on its own follow-up, and routinely idles past 30 minutes —
	// so the default {act:false} arm would leave its pending move marker UNHEALED and
	// trip the 30-minute pending warn as the normal case.
	//
	// pool_wait (PRD #754) is the empty-token-pool hold and belongs here for the very
	// same reasons: a held auto run keeps its issue and worker affinity, resumes when a
	// token is pooled (M5), and routinely outlasts 30 minutes — so leaving it in the
	// default arm would trip the 30-minute pending warn on every hold. It is In Progress (a held
	// run is still working), and Notify's partial map is deliberately NOT extended: a
	// hold moves no card, so there is nothing to notify about.
	// recovery_wait (issue #1197) is the transient-recovery park and belongs here for the
	// very same reasons as limit_wait/pool_wait: a recovery-parked run keeps its issue and
	// worker affinity, retries timed causes on capped backoff or awaits owner action for
	// held causes (including Codex idle stalls, #3509), and routinely
	// outlasts 30 minutes — so leaving it in the default arm would trip the 30-minute pending warn on
	// every park. It is In Progress (a recovering run is still working), and Notify's partial
	// map is deliberately NOT extended: a park moves no card, so there is nothing to notify.
	//
	// paused (PRD #1190) is the owner-requested park and belongs here for the same reasons:
	// a paused run keeps its issue, session and worker affinity, resumes on demand, and
	// routinely outlasts 30 minutes by design — so the default arm would trip the 30-minute pending
	// warn as the normal case. It is In Progress (a paused run is still an active piece of
	// work), and Notify's partial map is deliberately NOT extended: a pause moves no card.
	case "queued", "claimed", "running", "awaiting_approval", "awaiting_input", "awaiting_followup", "limit_wait", "pool_wait", "paused", "recovery_wait":
		return decision{act: true, target: board.ColumnInProgress}
	case "completed":
		return decision{act: true, target: board.ColumnHumanReview}
	case "failed", "cancelled":
		return restoreDecision(origin)
	default:
		return decision{act: false, clear: false}
	}
}

// restoreDecision restores the origin column, unless the origin is unknown (NULL,
// e.g. a pre-migration run): an unknown snapshot is never Open, and the guess is
// never worth stripping a human's label over — skip definitively.
func restoreDecision(origin pgtype.Text) decision {
	if !origin.Valid {
		return decision{act: false, clear: true}
	}
	return decision{act: true, target: origin.String}
}

// Notify is the workersvc hook, fired best-effort right after a run's status is
// written. It performs the column move off the request path so forge latency or a
// down forge never delays or fails the status write (the same-transaction marker
// is already durable and the reconcile loop is the backstop). Statuses the board
// does not react to are dropped without spawning a goroutine.
func (l *Lifecycle) Notify(runID uuid.UUID, status string) {
	switch status {
	case "queued", "completed", "failed", "cancelled":
	default:
		return
	}
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		defer func() {
			if p := recover(); p != nil {
				slog.Error("run lifecycle: notify panicked", "run", runID, "status", status, "panic", p)
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), notifyTimeout)
		defer cancel()
		l.notifyOnce(ctx, runID, status)
	}()
}

// Wait blocks until all in-flight Notify goroutines finish. Called on shutdown,
// after the HTTP server has drained (so no new Notify can arrive), before the DB
// pool closes.
func (l *Lifecycle) Wait() { l.wg.Wait() }

// notifyOnce performs one inline move for the given (runID, just-written status).
// The decision uses the PASSED status, not the run's possibly-advanced current
// status: the passed status is the trigger, and its target stays correct even if
// the run has moved on (a queued→In Progress target is still right once running).
func (l *Lifecycle) notifyOnce(ctx context.Context, runID uuid.UUID, status string) {
	mc, err := l.q.GetRunMoveContext(ctx, runID)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.Warn("run lifecycle: load move context", "run", runID, "error", err)
		}
		return
	}
	// A ci_fix run (PRD #6) carries no issue_iid and has no board card, so neither
	// the column automation nor the terminal issue-comment applies — skip both.
	if !mc.IssueIid.Valid {
		return
	}
	l.apply(ctx, runID, contextFromRow(mc), notifierDecision(status, mc.OriginColumn))
	// The worker's Set('completed'|'failed') funnels the terminal comment here; the
	// passed status is the just-written one (see notifyOnce's doc).
	l.maybeTerminalComment(ctx, runID, status, mc)
}

func contextFromRow(mc store.GetRunMoveContextRow) moveContext {
	return moveContext{
		// GetRunMoveContext INNER-JOINs repos, so a row is returned only for a run
		// with a non-NULL repo_id (a chat run yields ErrNoRows and never reaches here).
		repoID:          uuid.UUID(mc.RepoID.Bytes),
		userID:          mc.UserID,
		linked:          mc.IssueTrackerConnectionID.Valid,
		eligibilityName: mc.EligibilityTagName.String,
		issueIID:        mc.IssueIid.Int64, // valid: notifyOnce and reconcileOne skip card-less runs (NULL issue_iid)
		forgeProjectID:  mc.ForgeProjectID,
		forgeType:       mc.ForgeType,
		baseURL:         mc.BaseUrl,
		tokenCiphertext: mc.TokenCiphertext,
		originColumn:    mc.OriginColumn,
		boardColumn:     mc.BoardColumn,
		superseded:      mc.Superseded,
	}
}

// apply performs one guarded column move. It enforces what the raw AutoMove does
// not: never move a closed issue, and never override a manual drag (proceed only
// if the card still sits where the automation last left it —
// current == COALESCE(board_column, origin_column)). Outcomes:
//   - a strictly later run on the same issue exists (superseded, #4704) → clear the
//     marker and move nothing, before any other check;
//   - forge move succeeds → record board_column and clear the marker;
//   - manual drag / closed issue / unknown baseline → clear the marker (a
//     definitive skip that retrying cannot fix);
//   - an issue a person resolved in the linked issue tracker since the last sync
//     (the cache still shows it open) → the driver refuses the write and the marker
//     is cleared, the same outcome as the cached closed issue;
//   - forge error, or a move the linked issue tracker refused (no State mapped for
//     the column) → leave the marker for a later reconcile retry;
//   - a linked issue missing from the cache → probeUncachedLinkedIssue decides.
func (l *Lifecycle) apply(ctx context.Context, runID uuid.UUID, mc moveContext, dec decision) {
	if mc.superseded {
		// #4704: a strictly later run on the same issue owns the card; this run's
		// move would clobber it (both may expect In Progress, so the manual-drag
		// guard cannot tell). Clear the marker and move nothing. Known limit: a
		// successor that becomes visible after the move-context read is not fenced.
		slog.Info("run lifecycle: run superseded by a newer run on the same issue, skipping move", "run", runID, "issue", mc.issueIID)
		l.clearPending(ctx, runID)
		return
	}
	if !dec.act {
		if dec.clear {
			l.clearPending(ctx, runID)
		}
		return
	}

	issue, err := l.q.GetIssueByIID(ctx, store.GetIssueByIIDParams{RepoID: mc.repoID, ForgeIssueIid: mc.issueIID})
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.Warn("run lifecycle: load issue for move", "run", runID, "error", err)
		} else if mc.linked {
			// The cache has no row for this linked issue. It may be gone (moved to another
			// project, D10), never eligible (it will never be cached), or only not cached
			// yet; the issue tracker itself says which (PRD #4173 M5).
			l.probeUncachedLinkedIssue(ctx, runID, mc)
			return
		}
		// No cached card to move (evicted/de-labeled forge-side): leave the marker;
		// the reconcile loop gives up after the window.
		return
	}

	// Never auto-move a closed issue: closed is a terminal placement, not a
	// workflow column. The marker is cleared (nothing to retry).
	if issue.State == "closed" {
		l.clearPending(ctx, runID)
		return
	}

	cols, err := l.q.ListBoardColumns(ctx, mc.repoID)
	if err != nil {
		slog.Warn("run lifecycle: list columns for move", "run", runID, "error", err)
		return
	}
	position := make(map[string]int, len(cols))
	for _, c := range cols {
		position[c.LabelName] = int(c.Position)
	}
	var labels []string
	if err := json.Unmarshal(issue.Labels, &labels); err != nil {
		labels = []string{}
	}
	// A linked repo's card sits in its cached State column, never in a label (PRD #4173 M5).
	current, _, _ := board.ResolveColumn(board.CardColumnLabels(mc.linked, labels, issue.IssueTrackerColumn.String), issue.State, position)

	baseline, haveBaseline := coalesceColumn(mc.boardColumn, mc.originColumn)
	if !haveBaseline {
		// Unknown baseline (pre-migration run): cannot tell a manual drag from the
		// intended state, so never move on a guess.
		l.clearPending(ctx, runID)
		return
	}
	if current != baseline {
		// A human placed the card since the automation last touched it — manual
		// drags win. The guard compares against COALESCE(board_column,
		// origin_column), NOT the move's target, so a legit retry after a failed
		// first write (board_column still NULL) proceeds when the card is untouched.
		l.clearPending(ctx, runID)
		return
	}

	tr, ok := l.issueSide(ctx, runID, mc)
	if !ok {
		return
	}
	if _, err := l.mover.AutoMove(ctx, tr, mc.forgeProjectID, issue, cols, dec.target); err != nil {
		if forgesvc.IssueWriteSkippable(err) {
			// PRD #4173 D10: the write is not offered on a linked repo, or the linked issue
			// is gone from its binding. Retrying cannot help, so clear the marker (permanent
			// skip, logged once) instead of retrying to the give-up window. The run's
			// code-host side is unaffected.
			slog.Info("run lifecycle: issue move skipped on linked repo", "run", runID, "target", dec.target, "reason", err)
			l.clearPending(ctx, runID)
			return
		}
		if forgesvc.LinkedIssueResolved(err) {
			// A person resolved the issue in the issue tracker and the cache has not caught
			// up. Their resolution wins: no State was written, and retrying cannot help
			// (same outcome as the cached closed-issue guard above).
			slog.Info("run lifecycle: issue move skipped, the issue is resolved in the issue tracker", "run", runID, "target", dec.target, "reason", err)
			l.clearPending(ctx, runID)
			return
		}
		if forgesvc.ColumnMoveRefused(err) {
			// The issue tracker refused the move before any write: the column has no State
			// mapped, the target State is not allowed, the issue's State field is run by a
			// state machine, or it cannot be read as a plain State field. Unlike a skip this
			// can heal (the owner saves the mapping, or the YouTrack workflow or the
			// connection's visibility changes), so the marker stays and reconciliation
			// retries inside its window; a retry costs the State bundle read and one
			// issue GET and never a write (PRD #4173 M5; specs/human.md, failed column
			// moves retry).
			slog.Warn("run lifecycle: issue tracker refused the column move, will retry", "run", runID, "target", dec.target, "error", err)
			return
		}
		// Forge error: the transition already succeeded; leave the marker so the
		// reconcile loop retries once the forge is reachable again.
		slog.Warn("run lifecycle: forge move failed, will retry", "run", runID, "target", dec.target, "error", err)
		return
	}
	// Success: record the applied column (board_column, stored even when "" = Open)
	// and clear the marker in one statement.
	if _, err := l.q.RecordRunColumnMove(ctx, store.RecordRunColumnMoveParams{
		BoardColumn: pgtype.Text{String: dec.target, Valid: true},
		ID:          runID,
	}); err != nil {
		slog.Warn("run lifecycle: record column move", "run", runID, "error", err)
	}

	// Project the move onto a linked GitHub Projects v2 Status (PRD #364 M5),
	// best-effort. Hooked here (a uzi-originated move) rather than inside AutoMove,
	// which the reverse poller also calls. A projection error never fails the move.
	if l.projector != nil {
		if err := l.projector.ForwardMove(ctx, mc.repoID, mc.issueIID, dec.target); err != nil {
			slog.Warn("run lifecycle: forward project sync", "run", runID, "repo", mc.repoID, "issue", mc.issueIID, "error", err)
		}
	}
}

// issueSide builds the forge client and resolves the repo's issue side for a move or a
// probe. On failure it logs and reports false, leaving the marker for the reconcile
// loop like any other outage.
func (l *Lifecycle) issueSide(ctx context.Context, runID uuid.UUID, mc moveContext) (forge.IssueTracker, bool) {
	f, err := l.mover.ForgeForConnection(mc.forgeType, mc.baseURL, mc.tokenCiphertext)
	if err != nil {
		// Likely an undecryptable token; leave the marker (reconcile will retry,
		// then give up — visible drift).
		slog.Warn("run lifecycle: build forge client for move", "run", runID, "error", err)
		return nil, false
	}
	tr, err := issuesource.For(ctx, l.issueSources, mc.linked, mc.userID, mc.repoID, f)
	if err != nil {
		// A linked repo with no resolver, or one that cannot be built: never the forge.
		// Left for the reconcile loop like any other outage.
		slog.Warn("run lifecycle: resolve issue tracker for move", "run", runID, "error", err)
		return nil, false
	}
	return tr, true
}

// probeUncachedLinkedIssue settles the pending move of a linked run whose issue is not
// in the cache (PRD #4173 M5). It reads the issue from the linked project:
//   - not found (moved away, D10): clear the marker, the move can never happen;
//   - found but without the link's eligibility tag: it will never be cached (a
//     self-improve tracking issue, for example), so clear the marker;
//   - found and eligible, or any read error: leave the marker, so reconciliation moves
//     the card once the next sync caches it, as on an unlinked repo.
func (l *Lifecycle) probeUncachedLinkedIssue(ctx context.Context, runID uuid.UUID, mc moveContext) {
	tr, ok := l.issueSide(ctx, runID, mc)
	if !ok {
		return
	}
	issue, err := tr.GetIssue(ctx, mc.forgeProjectID, mc.issueIID)
	if err != nil {
		if forgesvc.LinkedIssueGone(err) {
			slog.Info("run lifecycle: linked issue is no longer in the linked project, skipping move", "run", runID, "issue", mc.issueIID)
			l.clearPending(ctx, runID)
			return
		}
		slog.Warn("run lifecycle: probe uncached linked issue", "run", runID, "issue", mc.issueIID, "error", err)
		return
	}
	if mc.eligibilityName == "" {
		return
	}
	if !slices.Contains(issue.Labels, mc.eligibilityName) {
		slog.Info("run lifecycle: linked issue does not carry the eligibility tag and will never be cached, skipping move", "run", runID, "issue", mc.issueIID)
		l.clearPending(ctx, runID)
	}
}

func (l *Lifecycle) clearPending(ctx context.Context, runID uuid.UUID) {
	if _, err := l.q.ClearRunMovePending(ctx, runID); err != nil {
		slog.Warn("run lifecycle: clear pending marker", "run", runID, "error", err)
	}
}

// coalesceColumn implements COALESCE(board_column, origin_column): the automation
// last-written column if any, else the origin snapshot, else unknown. "" (Open)
// is a real value distinct from unknown (NULL).
func coalesceColumn(boardCol, originCol pgtype.Text) (string, bool) {
	if boardCol.Valid {
		return boardCol.String, true
	}
	if originCol.Valid {
		return originCol.String, true
	}
	return "", false
}

// maybeTerminalComment is the SINGLE run-lifecycle hook that posts an autopilot
// run's outcome as a GitLab issue comment (PRD #19 Decision 6). It fires for BOTH
// terminal paths through the one place they converge — the lifecycle notifier: the
// worker's Set('failed'|'completed') reaches it via the inline notifyOnce, and the
// sweeper's bulk terminal writes reach it via the reconcile loop (they stamp
// move_pending_since but never call the inline notify). It is a no-op for anything
// but a just-terminal autopilot run.
//
// Ordering is record-then-comment: ClaimAutopilotTerminalComment atomically marks
// the run FIRST (and doubles as the concurrency claim — of the possibly-racing
// invocations exactly one wins the row), then the comment is posted. A crash or a
// forge error after the claim loses that one comment rather than ever
// double-posting — the same trade the pre-run comments make.
//
// Bodies are FIXED templates plus links built only from trusted values (the run
// uuid, the numeric mr_iid, the config origin, and the repo's own web_url). Issue
// title/description and the free-text failure_reason are NEVER interpolated: issue
// text is untrusted input and the reason is not guaranteed content-free, so the
// failure comment points at the run for detail instead of echoing it (audit
// requirement).
func (l *Lifecycle) maybeTerminalComment(ctx context.Context, runID uuid.UUID, status string, mc store.GetRunMoveContextRow) {
	switch status {
	case "completed", "failed":
	default:
		return // non-terminal, or a cancel (a human stop, not an autopilot outcome)
	}
	if !mc.AutoApprove {
		return // only autopilot runs comment; a manual run's outcome is the user's own
	}

	// Record-then-comment: claim the one comment for this run. 0 rows means either
	// a manual run (already excluded) or a peer invocation already claimed it.
	claimed, err := l.q.ClaimAutopilotTerminalComment(ctx, runID)
	if err != nil {
		slog.Warn("run lifecycle: claim autopilot terminal comment", "run", runID, "error", err)
		return
	}
	if claimed == 0 {
		return
	}

	f, err := l.mover.ForgeForConnection(mc.ForgeType, mc.BaseUrl, mc.TokenCiphertext)
	if err != nil {
		// The marker is recorded, so this comment is now lost (never retried) — the
		// accepted record-then-comment trade. Likely an undecryptable token.
		slog.Warn("run lifecycle: build forge for terminal comment", "run", runID, "error", err)
		return
	}
	// Only the issue side of the driver is used here. PRD #4173: a linked repo's
	// comment goes through the resolver and lands on the linked issue (M5); like any
	// terminal-comment failure it is logged and the lifecycle continues.
	tracker, err := issuesource.For(ctx, l.issueSources, mc.IssueTrackerConnectionID.Valid, mc.UserID, uuid.UUID(mc.RepoID.Bytes), f)
	if err != nil {
		slog.Warn("run lifecycle: resolve issue tracker for terminal comment", "run", runID, "error", err)
		return
	}
	body := l.terminalCommentBody(status, runID, mc)
	if _, err := tracker.CreateIssueNote(ctx, mc.ForgeProjectID, mc.IssueIid.Int64, body); err != nil {
		// Already PAT-redacted by the driver. Recorded but lost (never retried).
		if forgesvc.IssueWriteSkippable(err) {
			slog.Info("run lifecycle: autopilot terminal comment skipped, the linked issue is gone or moved", "run", runID, "reason", err)
			return
		}
		slog.Warn("run lifecycle: post autopilot terminal comment", "run", runID, "error", err)
	}
}

// terminalCommentBody renders the fixed comment for a terminal autopilot run. Only
// trusted values are interpolated (see maybeTerminalComment): the run uuid, the
// numeric mr_iid, and URLs derived from the config origin and the repo's web_url.
func (l *Lifecycle) terminalCommentBody(status string, runID uuid.UUID, mc store.GetRunMoveContextRow) string {
	if status == "completed" {
		if mc.MrIid.Valid {
			return fmt.Sprintf(
				"**Autopilot opened a merge request.**\n\n"+
					"Autopilot finished this issue and opened merge request !%d for your review: %s",
				mc.MrIid.Int64, mergeRequestURL(mc.RepoWebUrl, mc.MrIid.Int64))
		}
		// Completed without an MR is not expected on the normal success path; still
		// close the loop with the run link rather than going silent.
		return "**Autopilot finished this issue.**\n\n" +
			"Autopilot completed without opening a merge request." + l.runLinkSuffix(runID)
	}
	return "**Autopilot could not complete this issue.**\n\n" +
		"The autopilot run failed." + l.runLinkSuffix(runID) +
		"\n\nRemove and re-add the label to retry."
}

// runLinkSuffix appends " See the run: <url>" when the frontend origin is known,
// else nothing (a bare, unroutable path is worse than no link).
func (l *Lifecycle) runLinkSuffix(runID uuid.UUID) string {
	if l.frontendOrigin == "" {
		return ""
	}
	return fmt.Sprintf(" See the run: %s/runs/%s", l.frontendOrigin, runID)
}

// mergeRequestURL builds the GitLab merge-request web URL from the repo's web_url.
func mergeRequestURL(repoWebURL string, mrIID int64) string {
	return fmt.Sprintf("%s/-/merge_requests/%d", strings.TrimRight(repoWebURL, "/"), mrIID)
}

// RunReconciler blocks until ctx is cancelled, retrying pending moves every
// interval. It is the isolated sibling of the liveness sweeper: its forge I/O
// must never sit in the sweep path, and each pass is bounded so a slow forge
// cannot overrun into the next tick.
func (l *Lifecycle) RunReconciler(ctx context.Context) {
	ticker := time.NewTicker(l.interval)
	defer ticker.Stop()
	slog.Info("run-lifecycle reconciler started", "interval", l.interval.String())
	for {
		select {
		case <-ctx.Done():
			slog.Info("run-lifecycle reconciler stopped")
			return
		case <-ticker.C:
			if l.beat != nil {
				l.beat() // admin-health loop-beat: this loop ticked (PRD #1484 M2)
			}
			passCtx, cancel := context.WithTimeout(ctx, l.interval)
			l.Reconcile(passCtx)
			cancel()
		}
	}
}

// Reconcile runs one retry pass in two tiers. The fast lane lists markers older
// than the grace and younger than the fast window (30 minutes) and runs every
// tick. The slow lane lists markers between the fast window and the give-up (24
// hours), oldest first, at most every slowInterval and at most slowBatch per pass;
// it runs first, under its own slowTimeout, so a stalled forge there cannot spend
// more than slowTimeout of the fast lane's pass budget. Markers older than the give-up are left set (visible
// drift) and never retried. Each run's context and status are re-read immediately
// before the write (narrowing the clobber window against a concurrent manual
// drag) and the total-map target is applied. After the lanes, warnGivenUp logs the
// runs that crossed the 30-minute and give-up thresholds, under its own warnTimeout.
//
// Known limits:
//   - Both lanes list oldest first with a LIMIT, so rows that keep failing can
//     leave later rows unattempted in that lane (in the slow lane, for up to 24
//     hours: 25 stuck markers can hold every slow slot).
//   - Cadence and caps are per reconciler process (per api replica).
//   - A restart runs the slow pass early, and the first post-deploy tick retries
//     markers up to 24 hours old, behind the existing guards and the superseded
//     fence in apply.
//   - Crossing warnings are best effort: downtime, a restart (the cursors reset)
//     or same-timestamp ties beyond a capped scan can miss one.
//   - The superseded fence races: a successor that becomes visible after the
//     context read is not fenced.
//   - Late-success record race: the RecordRunColumnMove write stays on the lane
//     context, so a forge move that succeeds just before a lane deadline can leave
//     it unwritten. The stale board_column then makes the next retry or transition
//     read the moved card as a manual drag, clear the marker and not move it. The
//     same race exists at the 30s pass deadline and is likelier in the 10s slow lane.
//   - The autopilot terminal comment (claim, then post) also runs under the lane
//     context: a deadline between the claim and the post loses that comment, and a
//     comment an earlier tick failed to claim can now post up to 24 hours late.
func (l *Lifecycle) Reconcile(ctx context.Context) {
	now := l.now()
	for i := range l.warnFrom {
		if l.warnFrom[i].IsZero() {
			l.warnFrom[i] = now.Add(-l.interval)
		}
	}
	if l.lastSlow.IsZero() || now.Sub(l.lastSlow) >= l.slowInterval {
		l.lastSlow = now
		l.slowPass(ctx, now)
	}
	ids, err := l.q.ListPendingColumnMoves(ctx, store.ListPendingColumnMovesParams{
		GraceCutoff:  pgtype.Timestamptz{Time: now.Add(-l.grace), Valid: true},
		GiveupCutoff: pgtype.Timestamptz{Time: now.Add(-l.fastWindow), Valid: true},
		MaxBatch:     l.batch,
	})
	if err != nil {
		slog.Warn("run lifecycle: list pending moves", "error", err)
	} else {
		for _, id := range ids {
			if ctx.Err() != nil {
				return // shutdown / per-pass deadline: leave the rest for the next tick
			}
			l.reconcileOne(ctx, id)
		}
	}
	if ctx.Err() != nil {
		return // expired parent: skip the scans; the retained cursors recover next tick
	}
	wctx, cancel := context.WithTimeout(ctx, l.warnTimeout)
	defer cancel()
	l.warnGivenUp(wctx, now)
}

// slowPass retries markers between the fast window and the give-up, bounded by
// slowTimeout.
func (l *Lifecycle) slowPass(ctx context.Context, now time.Time) {
	sctx, cancel := context.WithTimeout(ctx, l.slowTimeout)
	defer cancel()
	ids, err := l.q.ListPendingColumnMoves(sctx, store.ListPendingColumnMovesParams{
		GraceCutoff:  pgtype.Timestamptz{Time: now.Add(-l.fastWindow), Valid: true},
		GiveupCutoff: pgtype.Timestamptz{Time: now.Add(-l.giveup), Valid: true},
		MaxBatch:     l.slowBatch,
	})
	if err != nil {
		slog.Warn("run lifecycle: list slow-lane pending moves", "error", err)
		return
	}
	for _, id := range ids {
		if sctx.Err() != nil {
			return
		}
		l.reconcileOne(sctx, id)
	}
}

func (l *Lifecycle) reconcileOne(ctx context.Context, runID uuid.UUID) {
	mc, err := l.q.GetRunMoveContext(ctx, runID)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.Warn("run lifecycle: reload move context", "run", runID, "error", err)
		}
		return
	}
	if !mc.MovePendingSince.Valid {
		return // healed by an inline move or manual drag since the candidate scan
	}
	// A card-less run (repo but no issue: ci_fix, prompt, task) has nothing to move and
	// no issue to comment on; mirror notifyOnce's skip and clear a stale marker instead
	// of posting an autopilot comment to issue 0 (issue #1482).
	if !mc.IssueIid.Valid {
		l.clearPending(ctx, runID)
		return
	}
	l.apply(ctx, runID, contextFromRow(mc), reconcilerDecision(mc.Status, mc.OriginColumn))
	// The sweeper's bulk terminal writes never call the inline notify — they only
	// stamp move_pending_since — so the reconcile loop is the terminal comment's
	// funnel for that path. mc.Status is the run's current (terminal) status.
	l.maybeTerminalComment(ctx, runID, mc.Status, mc)
}

// warnGivenUp emits a one-shot warning for each run whose marker crossed a
// threshold (the fast window, then the give-up) since the previous scan. The
// marker is deliberately NOT cleared: a silent clear would hide the drift behind
// a correct-looking badge.
//
// For threshold X the scan covers markers whose pending_since lies in
// (warnFrom-X, now-X]. The cursor then advances to now, so consecutive windows
// abut and a row warns exactly once. When a scan returns warnScanCap rows it may
// have been truncated, so the cursor advances only to the last returned row's
// crossing time (pending_since+X) and the rest is picked up next tick. A failed
// scan keeps its cursor so the window is retried.
func (l *Lifecycle) warnGivenUp(ctx context.Context, now time.Time) {
	thresholds := [2]struct {
		X   time.Duration
		msg string
	}{
		{l.fastWindow, "run lifecycle: column move still pending after the fast retry window; now eligible for slow retries every 10 minutes until the 24h give-up (batch-limited, oldest first)"},
		{l.giveup, "run lifecycle: column move gave up after the retry window; marker left for manual heal"},
	}
	for i, th := range thresholds {
		rows, err := l.q.ListGaveUpColumnMoves(ctx, store.ListGaveUpColumnMovesParams{
			GiveupCutoff: pgtype.Timestamptz{Time: now.Add(-th.X), Valid: true},
			PriorCutoff:  pgtype.Timestamptz{Time: l.warnFrom[i].Add(-th.X), Valid: true},
		})
		if err != nil {
			slog.Warn("run lifecycle: list pending-move crossings", "error", err, "threshold", th.X.String())
			continue
		}
		for _, row := range rows {
			attrs := []any{
				"run", row.ID, "repo", row.RepoID, "issue", row.IssueIid, "status", row.Status,
				"pending_since", row.MovePendingSince.Time,
			}
			if i == 0 {
				attrs = append(attrs, "fast_window", l.fastWindow.String(), "slow_interval", l.slowInterval.String(), "giveup", l.giveup.String())
			}
			slog.Warn(th.msg, attrs...)
		}
		if len(rows) < warnScanCap {
			l.warnFrom[i] = now
		} else {
			l.warnFrom[i] = rows[len(rows)-1].MovePendingSince.Time.Add(th.X)
		}
	}
}

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
// map, within a 30-minute window. The reconcile loop is deliberately separate
// from the liveness sweeper so a stalled forge can never delay worker-loss
// recovery.
package runlifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/board"
	"gitlab.example.com/vtmocanu/uzi/api/internal/forge"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
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
	// defaultGiveup is the retry window. After it the marker is left set (visible
	// drift, not a silent correct-looking badge) and only cleared by the next
	// transition or a manual drag.
	defaultGiveup = 30 * time.Minute
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
	AutoMove(ctx context.Context, f forge.Forge, forgeProjectID int64, issue store.Issue, columns []store.BoardColumn, target string) (store.Issue, error)
}

// Lifecycle is the automation subscriber and reconcile loop.
type Lifecycle struct {
	q     Store
	mover Mover
	now   func() time.Time

	// frontendOrigin is the user-facing origin (config.FrontendOrigin) the
	// terminal-comment hook builds run links from. Empty omits the link.
	frontendOrigin string

	interval time.Duration
	grace    time.Duration
	giveup   time.Duration
	batch    int32

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
	}
}

// moveContext is the run + connection facts one move needs, sourced identically
// by the notifier (GetRunMoveContext) and the reconciler (re-read per run).
type moveContext struct {
	repoID          uuid.UUID
	issueIID        int64
	forgeProjectID  int64
	forgeType       string
	baseURL         string
	tokenCiphertext []byte
	originColumn    pgtype.Text
	boardColumn     pgtype.Text
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
	case "queued", "claimed", "running", "awaiting_approval":
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
		issueIID:        mc.IssueIid.Int64, // valid: notifyOnce skips ci_fix runs (NULL issue_iid)
		forgeProjectID:  mc.ForgeProjectID,
		forgeType:       mc.ForgeType,
		baseURL:         mc.BaseUrl,
		tokenCiphertext: mc.TokenCiphertext,
		originColumn:    mc.OriginColumn,
		boardColumn:     mc.BoardColumn,
	}
}

// apply performs one guarded column move. It enforces what the raw AutoMove does
// not: never move a closed issue, and never override a manual drag (proceed only
// if the card still sits where the automation last left it —
// current == COALESCE(board_column, origin_column)). Outcomes:
//   - forge move succeeds → record board_column and clear the marker;
//   - manual drag / closed issue / unknown baseline → clear the marker (a
//     definitive skip that retrying cannot fix);
//   - forge error → leave the marker for a later reconcile retry.
func (l *Lifecycle) apply(ctx context.Context, runID uuid.UUID, mc moveContext, dec decision) {
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
	current, _, _ := board.ResolveColumn(labels, issue.State, position)

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

	f, err := l.mover.ForgeForConnection(mc.forgeType, mc.baseURL, mc.tokenCiphertext)
	if err != nil {
		// Likely an undecryptable token; leave the marker (reconcile will retry,
		// then give up — visible drift).
		slog.Warn("run lifecycle: build forge client for move", "run", runID, "error", err)
		return
	}
	if _, err := l.mover.AutoMove(ctx, f, mc.forgeProjectID, issue, cols, dec.target); err != nil {
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
	body := l.terminalCommentBody(status, runID, mc)
	if _, err := f.CreateIssueNote(ctx, mc.ForgeProjectID, mc.IssueIid.Int64, body); err != nil {
		// Already PAT-redacted by the driver. Recorded but lost (never retried).
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
			passCtx, cancel := context.WithTimeout(ctx, l.interval)
			l.Reconcile(passCtx)
			cancel()
		}
	}
}

// Reconcile runs one retry pass: it re-reads each pending run's current status and
// context immediately before the write (narrowing the clobber window against a
// concurrent manual drag), applies the total-map target, and warn-logs runs that
// just crossed the give-up boundary.
func (l *Lifecycle) Reconcile(ctx context.Context) {
	now := l.now()
	ids, err := l.q.ListPendingColumnMoves(ctx, store.ListPendingColumnMovesParams{
		GraceCutoff:  pgtype.Timestamptz{Time: now.Add(-l.grace), Valid: true},
		GiveupCutoff: pgtype.Timestamptz{Time: now.Add(-l.giveup), Valid: true},
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
	l.warnGivenUp(ctx, now)
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
	l.apply(ctx, runID, contextFromRow(mc), reconcilerDecision(mc.Status, mc.OriginColumn))
	// The sweeper's bulk terminal writes never call the inline notify — they only
	// stamp move_pending_since — so the reconcile loop is the terminal comment's
	// funnel for that path. mc.Status is the run's current (terminal) status.
	l.maybeTerminalComment(ctx, runID, mc.Status, mc)
}

// warnGivenUp emits a one-shot warning for each run whose marker crossed the
// give-up boundary during the last interval. The marker is deliberately NOT
// cleared — a silent clear would hide the drift behind a correct-looking badge.
func (l *Lifecycle) warnGivenUp(ctx context.Context, now time.Time) {
	rows, err := l.q.ListGaveUpColumnMoves(ctx, store.ListGaveUpColumnMovesParams{
		GiveupCutoff: pgtype.Timestamptz{Time: now.Add(-l.giveup), Valid: true},
		PriorCutoff:  pgtype.Timestamptz{Time: now.Add(-l.giveup - l.interval), Valid: true},
	})
	if err != nil {
		slog.Warn("run lifecycle: list given-up moves", "error", err)
		return
	}
	for _, row := range rows {
		slog.Warn("run lifecycle: column move gave up after the retry window; marker left for manual heal",
			"run", row.ID, "repo", row.RepoID, "issue", row.IssueIid, "status", row.Status,
			"pending_since", row.MovePendingSince.Time)
	}
}

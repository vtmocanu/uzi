package forgesvc

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/board"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// SyncMRStates is the MR-close watcher (PRD #24 M3). The poller calls it once per
// repo per tick, AFTER the issue sync, so it reads the freshest issue cache. For
// each watch candidate (the issue's latest run, watched only while completed with
// a known MR, card in Human Review or already recorded closed — see
// ListMRWatchCandidates) it reads the MR's current state and, on an
// opened→closed / closed→opened TRANSITION, moves the card forge-first under the
// same guards the run-lifecycle automation enforces.
//
// It never returns per-candidate errors: one bad MR read or move must not stall
// the tick (the poller's log-and-skip convention). Only a failure to enumerate
// candidates is returned, so the poller can log it once for the repo.
func (s *Service) SyncMRStates(ctx context.Context, repoID uuid.UUID, forgeProjectID int64, ch forge.CodeHost, tr forge.IssueTracker) error {
	candidates, err := s.q.ListMRWatchCandidates(ctx, repoID)
	if err != nil {
		return err
	}
	for _, c := range candidates {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.syncOneMRState(ctx, repoID, forgeProjectID, ch, tr, mrWatchCandidate{
			RunID: c.ID, IssueIID: c.IssueIid.Int64, MrIID: c.MrIid.Int64, MrState: c.MrState,
		}, mrLane{missingRow: moveDeferred})
	}
	return nil
}

// mrWatchCandidate is the part of a watch candidate syncOneMRState reads, so the
// board lane (ListMRWatchCandidates) and the linked issue-lane of the board-free
// recorder (ListBoardFreeMRStateWatchCandidates) share one edge switch.
type mrWatchCandidate struct {
	RunID    uuid.UUID
	IssueIID int64
	MrIID    int64
	MrState  pgtype.Text
}

// mrLane is how a lane differs in the card moves syncOneMRState asks for. The edge
// logic itself is identical across lanes.
type mrLane struct {
	// missingRow is what a move reports when the issue has no cached row. The board
	// lane defers (its candidates JOIN the issue cache, so a vanished row self-heals
	// next tick); the linked board-free lane skips, because its candidate set does
	// not require the row and an evicted issue's MR must not be polled forever.
	missingRow moveOutcome
	// noMove records the edge without moving the card: a superseded run of the issue
	// (a newer issue-lane run owns the card) never moves it.
	noMove bool
}

// laneMRMove runs one guarded card move for the candidate, or skips it on a lane that does
// not move (the edge is consumed).
func (s *Service) laneMRMove(ctx context.Context, lane mrLane, repoID uuid.UUID, forgeProjectID, issueIID int64, tr forge.IssueTracker, wantSource, target string) moveOutcome {
	if lane.noMove {
		return moveSkipped
	}
	return s.guardedMRMove(ctx, repoID, forgeProjectID, issueIID, tr, wantSource, target, lane.missingRow)
}

// syncOneMRState reconciles one candidate's MR state against the stored value.
//
// As of #527 the candidate set (Lane B of ListMRWatchCandidates) also includes
// CLOSED-issue runs, so a merged/closed terminal state can be recorded after the
// merge closed the issue. These are handled move-free by the paths already here:
// the NULL bootstrap records without moving, the merged/locked `default` arm
// records without moving, and guardedMRMove skips a closed issue — no behavioral
// or signature change is needed for the new lane.
//
// State-persistence contract (PRD #24 TD §3, review finding 4): mr_state advances
// only on (a) a successful move, (b) a deliberate guard-skip (manual drag / closed
// issue — the edge is consumed, we never re-fight a human), or (c) a no-op
// observation (merged / locked / NULL bootstrap). On a forge-side FAILURE (the MR
// read, or the move itself) mr_state is LEFT as-is, so the next poller tick
// re-observes the same edge and retries — the poller cadence is the retry loop.
// Consuming the edge on failure would re-create the stuck-card bug this PRD fixes.
func (s *Service) syncOneMRState(ctx context.Context, repoID uuid.UUID, forgeProjectID int64, ch forge.CodeHost, tr forge.IssueTracker, c mrWatchCandidate, lane mrLane) {
	mr, err := ch.GetMergeRequest(ctx, forgeProjectID, c.MrIID)
	if err != nil {
		// A read failure is not an edge: log-and-skip, leave mr_state for next tick.
		slog.Warn("forgesvc: MR-state read failed", "repo", repoID, "issue", c.IssueIID, "mr", c.MrIID, "error", err)
		return
	}
	observed := mr.State

	// Record and act on KNOWN states only (reviewer hardening, PRD #24 Decision
	// Log). An unrecognized or empty state is ignored ENTIRELY — no write — in
	// both the bootstrap and transition paths, leaving the prior baseline (or
	// NULL) intact. Because edges fire on exact string compares, recording garbage
	// would mask the next real close until a full reopen cycle re-synced the
	// baseline; ignoring it instead lets a transient glitch self-heal
	// (stored="opened" → glitch skipped → a later real "closed" still fires).
	if !forge.IsKnownMRState(observed) {
		slog.Warn("forgesvc: ignoring unknown MR state", "repo", repoID, "issue", c.IssueIID, "mr", c.MrIID, "state", observed)
		return
	}

	if !c.MrState.Valid {
		// Bootstrap (Decision 9): first observation records state WITHOUT moving.
		// Acting on NULL→closed cannot distinguish "closed and stuck" from "closed
		// yesterday, already triaged elsewhere", so pre-existing closed MRs at
		// upgrade time never cause a wave of moves — a manual drag fixes those.
		s.recordMRState(ctx, c.RunID, observed)
		return
	}
	stored := c.MrState.String
	if observed == stored {
		return // no transition, nothing to do
	}

	switch {
	case stored == forge.MRStateOpened && observed == forge.MRStateClosed:
		// close-edge: reviewer rejected the MR → rework needed → In Progress,
		// guarded from Human Review (where the completed run parked the card).
		//
		// A confirmed close means any in-flight rework can never land — abort it
		// INDEPENDENTLY of the board move (#853 review finding). Cancellation is
		// keyed on the MR, not the card, so it must NOT be gated behind a move that
		// can defer: a forge move error, or an evicted card that stops being a
		// candidate next tick, would otherwise leave the rework spending forever.
		// Attempt it every tick, ahead of the move. Re-attempts converge: once the
		// run is terminal (server-side path) GetActiveMRReworkRunForMR returns no
		// rows → no-op nil; while a live worker's run is still non-terminal a
		// re-entry is also a no-op: the fenced cancel writes only when stop_kind IS
		// NULL, and the first stamp already set it.
		cancelErr := s.cancelReworkOnClosedMR(ctx, repoID, c.MrIID, observed)
		if cancelErr != nil {
			slog.Warn("forgesvc: cancel rework on MR close failed, will retry", "repo", repoID, "issue", c.IssueIID, "mr", c.MrIID, "error", cancelErr)
		}
		moved := s.laneMRMove(ctx, lane, repoID, forgeProjectID, c.IssueIID, tr, board.ColumnHumanReview, board.ColumnInProgress)
		if moved == moveDeferred || cancelErr != nil {
			// forge failure / vanished card, or a cancel that must be retried: leave
			// mr_state unadvanced so the next tick re-observes the edge and retries.
			return
		}
		s.recordMRState(ctx, c.RunID, observed)
	case stored == forge.MRStateClosed && observed == forge.MRStateOpened:
		// reopen-edge (Decision 6): restore the card Human Review-ward,
		// symmetrically, guarded from In Progress (where the close-edge left it).
		if s.laneMRMove(ctx, lane, repoID, forgeProjectID, c.IssueIID, tr, board.ColumnInProgress, board.ColumnHumanReview) == moveDeferred {
			return
		}
		s.recordMRState(ctx, c.RunID, observed)
	default:
		// A KNOWN non-edge transition: record, never move (this arm makes no board
		// move). Unknown states never reach here — they are ignored before the
		// bootstrap check above. A merge closes the issue via `Closes #N`, which the
		// existing issue-close sync owns.
		//
		// A confirmed TERMINAL state (merged OR closed) means the branch is gone / the
		// MR can never land, so any in-flight rework must be aborted (#853). This must
		// fire for `locked`->`closed` (and `locked`->`merged`) too: `locked` is the
		// transient mid-merge state, so an `opened`->`locked`->`closed` sequence reaches
		// this arm on the `locked`->`closed` tick — the `opened`->`closed` close arm
		// above never sees it, so without cancelling here the run self-evicts at the
		// terminal `closed` state with the rework still spending (issue #1072, the
		// issue-lane analogue of the board-free-lane fix in recordBoardFreeMRState). The
		// cancel is keyed on the MR, not the card; no board move happens here because
		// `locked` was mid-merge. A `locked`->`opened` settle is a non-terminal
		// transition and must NOT cancel — the rework gate stays open.
		if observed == forge.MRStateMerged || observed == forge.MRStateClosed {
			if err := s.cancelReworkOnClosedMR(ctx, repoID, c.MrIID, observed); err != nil {
				slog.Warn("forgesvc: cancel rework on MR terminal state failed, will retry", "repo", repoID, "issue", c.IssueIID, "mr", c.MrIID, "state", observed, "error", err)
				return // leave mr_state unadvanced so the next tick retries
			}
		}
		s.recordMRState(ctx, c.RunID, observed)
	}
}

// cancelReworkOnClosedMR aborts an in-flight mr_rework when its MR has merged/closed
// (issue #853). Returns nil when no canceller is wired or no active rework exists.
// A non-nil error means the caller must LEAVE mr_state unadvanced (don't consume the
// edge) so the next poller tick retries — same contract as a forge-side failure.
func (s *Service) cancelReworkOnClosedMR(ctx context.Context, repoID uuid.UUID, mrIID int64, observed string) error {
	if s.reworkCanceller == nil {
		return nil
	}
	return s.reworkCanceller.CancelReworkForMR(ctx, repoID, mrIID, workersvc.TerminalTargetStopReason(observed))
}

// moveOutcome is the result of a guarded MR-state move, distinguishing the two
// edge-consuming outcomes (applied / skipped) from the one that must LEAVE the
// edge unconsumed for a next-tick retry (deferred).
type moveOutcome int

const (
	moveApplied  moveOutcome = iota // forge move succeeded → consume the edge
	moveSkipped                     // a guard pre-empted the move (manual drag / closed issue) → consume the edge
	moveDeferred                    // forge error or vanished cache row → leave the edge, retry next tick
)

// guardedMRMove applies a forge-first move to target, but only if the issue is
// open and the card currently sits in wantSource. This mirrors the guard shape of
// runlifecycle.apply (lifecycle.go:240-317) — load issue → closed-skip →
// resolve-column → source-column compare → AutoMove — but persists nothing here:
// the caller owns mr_state, and this watcher's durable retry marker is mr_state
// itself (left unadvanced on moveDeferred), not runs.move_pending_since.
//
// Write ordering is forge-move-FIRST, then the caller records mr_state (review
// finding 5). A crash in between re-fires the edge next tick, and the
// source-column guard makes the retry a no-op (the card already moved) — except
// the narrow case where a human dragged the card back to wantSource in the crash
// window, which then gets re-moved once; accepted (crash-window-sized exposure).
func (s *Service) guardedMRMove(ctx context.Context, repoID uuid.UUID, forgeProjectID, issueIID int64, tr forge.IssueTracker, wantSource, target string, missingRow moveOutcome) moveOutcome {
	issue, err := s.q.GetIssueByIID(ctx, store.GetIssueByIIDParams{RepoID: repoID, ForgeIssueIid: issueIID})
	if err != nil {
		// No cached card (evicted/de-labeled since the candidate scan): the caller's
		// lane decides. The board lane leaves the edge: if the issue is genuinely gone
		// it stops being a candidate next tick (the query JOINs issues), so this does
		// not retry forever. The linked board-free lane skips (its query does not JOIN
		// issues) so the MR state is still recorded and the run stops being polled.
		if errors.Is(err, pgx.ErrNoRows) {
			return missingRow
		}
		slog.Warn("forgesvc: MR-state load issue", "repo", repoID, "issue", issueIID, "error", err)
		return moveDeferred
	}
	// Never auto-move a closed issue: Closed is a terminal placement, not a
	// workflow column. The edge is consumed (nothing to retry) — mirrors
	// runlifecycle.apply's closed-issue skip.
	if issue.State == "closed" {
		return moveSkipped
	}

	cols, err := s.q.ListBoardColumns(ctx, repoID)
	if err != nil {
		slog.Warn("forgesvc: MR-state list columns", "repo", repoID, "issue", issueIID, "error", err)
		return moveDeferred
	}
	position := make(map[string]int, len(cols))
	for _, col := range cols {
		position[col.LabelName] = int(col.Position)
	}
	var labels []string
	if err := json.Unmarshal(issue.Labels, &labels); err != nil {
		labels = []string{}
	}
	current, _, _ := board.ResolveColumn(board.CardColumnLabels(isLinkedTracker(tr), labels, issue.IssueTrackerColumn.String), issue.State, position)
	if current != wantSource {
		// A human placed the card elsewhere since the run parked it — manual drags
		// win (same baseline discipline as runlifecycle.apply). Consume the edge;
		// we never re-fight a move a human pre-empted.
		return moveSkipped
	}

	if _, err := s.AutoMove(ctx, tr, forgeProjectID, issue, cols, target); err != nil {
		if IssueWriteSkippable(err) {
			// The linked issue is gone or the write is not offered:
			// consume the edge so recordMRState still records the MR state, and never
			// retry forever (PRD #4173 D10).
			slog.Info("forgesvc: MR-state move skipped for a linked repo", "repo", repoID, "issue", issueIID, "target", target, "reason", err)
			return moveSkipped
		}
		if LinkedIssueResolved(err) {
			// A person resolved the issue in the issue tracker since the last sync, so the
			// cache still shows it open. Their resolution wins: consume the edge without a
			// move, as for a closed issue in the cache.
			slog.Info("forgesvc: MR-state move skipped, the issue is resolved in the issue tracker", "repo", repoID, "issue", issueIID, "target", target, "reason", err)
			return moveSkipped
		}
		if ColumnMoveRefused(err) {
			// The linked issue tracker refused the move before any request: the target
			// column has no State mapped, the mapping is unset, or the State is not
			// allowed. Unlike the run lifecycle, the MR watch does not replay it: the
			// MR state is recorded and the card stays where it is (a manual drag
			// fixes it). Accepted exception, PRD #4173 M5.
			slog.Info("forgesvc: MR-state move refused by the issue tracker, edge consumed without a move", "repo", repoID, "issue", issueIID, "target", target, "reason", err)
			return moveSkipped
		}
		// Forge error: leave mr_state unadvanced so the next tick re-observes this
		// edge and retries once the forge is reachable again.
		slog.Warn("forgesvc: MR-state move failed, will retry", "repo", repoID, "issue", issueIID, "target", target, "error", err)
		return moveDeferred
	}
	return moveApplied
}

// recordMRState persists the observed MR state (runs.mr_state is written only by
// forgesvc MR-state sync — SyncMRStates and SyncBoardFreeMRStates, PRD #908 — both
// through the single SetRunMRState statement). The two syncs cover DISJOINT run sets,
// so no run is ever recorded by both on one tick: ListMRWatchCandidates requires a
// non-null issue_iid (via its issues JOIN) AND kind NOT IN ('prompt','self_improve'),
// while ListBoardFreeMRStateWatchCandidates owns exactly the complement
// (issue_iid IS NULL OR kind IN ('prompt','self_improve'), plus EVERY run of a repo linked
// to an external issue tracker, which the board lane excludes — PRD #4173 M4) — a run
// cannot satisfy both predicates at once. A linked repo's issue-lane runs are still
// reconciled by syncOneMRState, through SyncBoardFreeMRStatesWithIssueSide (PRD #4173 M5). Best-effort: a write failure only means the same edge is
// re-observed next tick, and the guards make the retry a no-op if the card has
// already moved.
func (s *Service) recordMRState(ctx context.Context, runID uuid.UUID, state string) {
	if _, err := s.q.SetRunMRState(ctx, store.SetRunMRStateParams{
		ID:      runID,
		MrState: pgtype.Text{String: state, Valid: true},
	}); err != nil {
		slog.Warn("forgesvc: record MR state", "run", runID, "state", state, "error", err)
	}
}

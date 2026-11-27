package forgesvc

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// SyncBoardFreeMRStates is the board-FREE MR-state recorder for every issue-less
// MR-bearing run — the prompt/self_improve lanes plus issue-less helper runs (mr_rework,
// ci_fix, and issue-less chat/task) — PRD #908, widened in issue #1253. The poller calls it
// once per repo per tick, right after SyncMRStates. Unlike SyncMRStates it moves NO board
// card: these runs are issue-less (no card) or share a non-board-promoted tracking issue. It
// only records runs.mr_state (via the existing recordMRState, the sole SQL writer — invariant
// preserved) and, on the opened->closed / ->merged edge, cancels an in-flight rework via the
// existing cancelReworkOnClosedMR (issue #853 contract, reused verbatim). It is
// SyncBoardFreeMRStatesWithIssueSide with no issue tracker, which is the whole of its
// behaviour on an unlinked repo; it is the tr=nil seam kept for the board-free tests and
// equals the pre-M5 behaviour. Reads the run set
// from ListBoardFreeMRStateWatchCandidates, which self-evicts a run once its mr_state is
// terminal, except a linked repo's closed issue-lane run (below).
//
// SCOPE BOUNDARY (PRD #908 design, "self-bounding like Lane B"): for issue-less and
// scheduled-lane runs closed AND merged are terminal — the candidate query drops such a run at
// mr_state='closed', so a closed-then-reopened board-free MR is not re-watched. The exception
// is a repo linked to an external issue tracker (PRD #4173 M4): its issue-lane runs also stay
// candidates at mr_state='closed' (latest run of the issue only, and only while the cached
// issue is opened), so the reopen is recorded
// back to 'opened' by the default arm below and mr_rework regains eligibility. A closed MR
// that is still closed is a no-transition and fires no side effect.
//
// Like SyncMRStates it never returns a per-candidate error (log-and-skip); only a failure
// to enumerate candidates surfaces to the poller.
func (s *Service) SyncBoardFreeMRStates(ctx context.Context, repoID uuid.UUID, forgeProjectID int64, f forge.CodeHost) error {
	return s.SyncBoardFreeMRStatesWithIssueSide(ctx, repoID, forgeProjectID, f, nil)
}

// SyncBoardFreeMRStatesWithIssueSide is SyncBoardFreeMRStates for a repo whose issue side
// may be a linked issue tracker (PRD #4173 M5). When tr is a real linked issue tracker, a candidate
// that is an issue-lane run (issue_iid set, kind not prompt/self_improve) is reconciled
// through syncOneMRState, the board lane's edge switch, so a linked card moves Human Review
// -> In Progress when its MR closes and back when it reopens, exactly as an unlinked card
// does. The card moves only for the issue's latest such run (the board lane's DISTINCT ON
// semantics): a superseded run records its MR state with no move. A transient error from the
// real linked driver defers the edge and retries it next tick, like the board lane.
//
// Every other candidate goes through recordBoardFreeMRState unchanged: that is every
// candidate when tr is nil or is not a linked issue tracker (an unlinked repo's forge driver, whose
// issue-lane runs the candidate query keeps out anyway), and every candidate when tr is the
// UnavailableIssueTracker the poller hands a linked repo whose resolver failed. Recording
// consumes the close edge (mr_state advances, so no later tick sees it and the card is
// never moved for it). That is the trade-off: the cause can be permanent (a connection
// removed from the allowlist), and deferring would leave mr_state opened and the rework gate
// open on a closed MR forever, so the edge is recorded without a move. The one exception is a
// failed repo-source database read (errRepoSourceRead), which is transient: the linked
// issue-lane candidates are skipped this tick, neither recorded nor moved, and retried next
// tick. Candidates that are not linked issue-lane runs are recorded as usual.
//
// A missing cached issue row skips the move (the MR state is still recorded) rather than
// deferring it: this lane's candidates do not JOIN the issue cache, so deferring would poll
// an evicted issue's MR forever. A move the issue tracker refuses (ColumnMoveRefused) is
// likewise consumed without a move; see guardedMRMove.
func (s *Service) SyncBoardFreeMRStatesWithIssueSide(ctx context.Context, repoID uuid.UUID, forgeProjectID int64, f forge.CodeHost, tr forge.IssueTracker) error {
	candidates, err := s.q.ListBoardFreeMRStateWatchCandidates(ctx, repoID)
	if err != nil {
		return err
	}
	movesCard := tr != nil && isLinkedTracker(tr) && !isUnavailableTracker(tr)
	deferLinked := tr != nil && isTransientUnavailableTracker(tr)
	deferLogged := false
	for _, c := range candidates {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if movesCard && c.IssueIid.Valid && c.Kind != "prompt" && c.Kind != "self_improve" {
			s.syncOneMRState(ctx, repoID, forgeProjectID, f, tr, mrWatchCandidate{
				RunID: c.ID, IssueIID: c.IssueIid.Int64, MrIID: c.MrIid.Int64, MrState: c.MrState,
			}, mrLane{missingRow: moveSkipped, noMove: !c.IsLatest})
			continue
		}
		if deferLinked && c.IssueIid.Valid && c.Kind != "prompt" && c.Kind != "self_improve" {
			if !deferLogged {
				slog.Warn("forgesvc: repo issue source unreadable, deferring linked MR edges to the next tick", "repo", repoID)
				deferLogged = true
			}
			continue
		}
		s.recordBoardFreeMRState(ctx, repoID, forgeProjectID, f, c)
	}
	return nil
}

// recordBoardFreeMRState reconciles one board-free run's MR state against the stored value,
// board-free. It is a second implementation of syncOneMRState's observe->persist->cancel
// contract WITHOUT the board move (a linked repo's issue-lane runs use syncOneMRState itself,
// see SyncBoardFreeMRStatesWithIssueSide); the two must agree on: unknown-state -> no write; the
// NULL bootstrap records without acting; opened->closed and ->merged cancel exactly once;
// locked is transient (no cancel); a forge read failure or a cancel failure leaves mr_state
// unadvanced so the next tick retries.
func (s *Service) recordBoardFreeMRState(ctx context.Context, repoID uuid.UUID, forgeProjectID int64, f forge.CodeHost, c store.ListBoardFreeMRStateWatchCandidatesRow) {
	mr, err := f.GetMergeRequest(ctx, forgeProjectID, c.MrIid.Int64)
	if err != nil {
		slog.Warn("forgesvc: board-free MR-state read failed", "repo", repoID, "run", c.ID, "mr", c.MrIid.Int64, "error", err)
		return
	}
	// Observation time, captured right after the fetch and before the rework cancel
	// (PRD #4691; see syncOneMRState).
	observedAt := time.Now()
	observed := mr.State
	if !forge.IsKnownMRState(observed) {
		slog.Warn("forgesvc: ignoring unknown board-free MR state", "repo", repoID, "run", c.ID, "mr", c.MrIid.Int64, "state", observed)
		return
	}
	if !c.MrState.Valid {
		// Bootstrap: first observation records without acting. No rework was ever gated in
		// for a run whose mr_state was NULL, so there is nothing to cancel; recording an
		// `opened` here is exactly what opens the rework gate for a scheduled MR.
		s.recordMRState(ctx, c.ID, mr, observedAt)
		return
	}
	stored := c.MrState.String
	if observed == stored {
		return // no transition
	}
	switch observed {
	case forge.MRStateClosed, forge.MRStateMerged:
		// terminal edge from a valid non-terminal stored state (opened OR locked): a
		// confirmed close/merge means an in-flight rework can never land -> abort it
		// (issue #853). This MUST fire for stored=='locked' too — a locked->closed (or
		// ->merged) transition is still a terminal edge. The earlier stored=='opened'-only
		// close arm leaked a rework when the MR passed through `locked` before closing
		// (review finding, PRD #908): `locked`->`closed` fell to the default arm, which
		// records `closed` (self-evicting the run from the candidate set) without ever
		// cancelling. No board move (board-free). On cancel failure leave mr_state
		// unadvanced so the next tick re-observes the edge and retries.
		if err := s.cancelReworkOnClosedMR(ctx, repoID, c.MrIid.Int64, observed); err != nil {
			slog.Warn("forgesvc: cancel board-free rework on MR terminal state failed, will retry", "repo", repoID, "run", c.ID, "mr", c.MrIid.Int64, "state", observed, "error", err)
			return
		}
		s.recordMRState(ctx, c.ID, mr, observedAt)
	default:
		// KNOWN non-terminal transition: `locked` (transient during merge processing) or a
		// `locked`->`opened` settle. Record, never cancel — the gate stays open.
		s.recordMRState(ctx, c.ID, mr, observedAt)
	}
}

package forgesvc

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/board"
	"gitlab.example.com/vtmocanu/uzi/api/internal/forge"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
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
func (s *Service) SyncMRStates(ctx context.Context, repoID uuid.UUID, forgeProjectID int64, f forge.Forge) error {
	candidates, err := s.q.ListMRWatchCandidates(ctx, repoID)
	if err != nil {
		return err
	}
	for _, c := range candidates {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.syncOneMRState(ctx, repoID, forgeProjectID, f, c)
	}
	return nil
}

// syncOneMRState reconciles one candidate's MR state against the stored value.
//
// State-persistence contract (PRD #24 TD §3, review finding 4): mr_state advances
// only on (a) a successful move, (b) a deliberate guard-skip (manual drag / closed
// issue — the edge is consumed, we never re-fight a human), or (c) a no-op
// observation (merged / locked / NULL bootstrap). On a forge-side FAILURE (the MR
// read, or the move itself) mr_state is LEFT as-is, so the next poller tick
// re-observes the same edge and retries — the poller cadence is the retry loop.
// Consuming the edge on failure would re-create the stuck-card bug this PRD fixes.
func (s *Service) syncOneMRState(ctx context.Context, repoID uuid.UUID, forgeProjectID int64, f forge.Forge, c store.ListMRWatchCandidatesRow) {
	mr, err := f.GetMergeRequest(ctx, forgeProjectID, c.MrIid.Int64)
	if err != nil {
		// A read failure is not an edge: log-and-skip, leave mr_state for next tick.
		slog.Warn("forgesvc: MR-state read failed", "repo", repoID, "issue", c.IssueIid, "mr", c.MrIid.Int64, "error", err)
		return
	}
	observed := mr.State

	if !c.MrState.Valid {
		// Bootstrap (Decision 9): first observation records state WITHOUT moving.
		// Acting on NULL→closed cannot distinguish "closed and stuck" from "closed
		// yesterday, already triaged elsewhere", so pre-existing closed MRs at
		// upgrade time never cause a wave of moves — a manual drag fixes those.
		s.recordMRState(ctx, c.ID, observed)
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
		if s.guardedMRMove(ctx, repoID, forgeProjectID, c.IssueIid, f, board.ColumnHumanReview, board.ColumnInProgress) == moveDeferred {
			return // forge failure / vanished card: leave the edge for next-tick retry
		}
		s.recordMRState(ctx, c.ID, observed)
	case stored == forge.MRStateClosed && observed == forge.MRStateOpened:
		// reopen-edge (Decision 6): restore the card Human Review-ward,
		// symmetrically, guarded from In Progress (where the close-edge left it).
		if s.guardedMRMove(ctx, repoID, forgeProjectID, c.IssueIid, f, board.ColumnInProgress, board.ColumnHumanReview) == moveDeferred {
			return
		}
		s.recordMRState(ctx, c.ID, observed)
	default:
		// merged / locked / any non-actionable transition: record, never move. A
		// merge closes the issue via `Closes #N`, which the existing issue-close
		// sync owns; locked is transient during merge processing.
		s.recordMRState(ctx, c.ID, observed)
	}
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
func (s *Service) guardedMRMove(ctx context.Context, repoID uuid.UUID, forgeProjectID, issueIID int64, f forge.Forge, wantSource, target string) moveOutcome {
	issue, err := s.q.GetIssueByIID(ctx, store.GetIssueByIIDParams{RepoID: repoID, ForgeIssueIid: issueIID})
	if err != nil {
		// No cached card (evicted/de-labeled since the candidate scan): leave the
		// edge. If the issue is genuinely gone it stops being a candidate next tick
		// (the query JOINs issues), so this does not retry forever.
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.Warn("forgesvc: MR-state load issue", "repo", repoID, "issue", issueIID, "error", err)
		}
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
	current, _, _ := board.ResolveColumn(labels, issue.State, position)
	if current != wantSource {
		// A human placed the card elsewhere since the run parked it — manual drags
		// win (same baseline discipline as runlifecycle.apply). Consume the edge;
		// we never re-fight a move a human pre-empted.
		return moveSkipped
	}

	if _, err := s.AutoMove(ctx, f, forgeProjectID, issue, cols, target); err != nil {
		// Forge error: leave mr_state unadvanced so the next tick re-observes this
		// edge and retries once the forge is reachable again.
		slog.Warn("forgesvc: MR-state move failed, will retry", "repo", repoID, "issue", issueIID, "target", target, "error", err)
		return moveDeferred
	}
	return moveApplied
}

// recordMRState persists the observed MR state (SetRunMRState is the sole writer
// of runs.mr_state). Best-effort: a write failure only means the same edge is
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

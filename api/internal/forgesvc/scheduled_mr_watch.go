package forgesvc

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// SyncScheduledMRStates is the board-FREE MR-state recorder for the scheduled lanes
// (prompt, self_improve) — PRD #908. The poller calls it once per repo per tick, right
// after SyncMRStates. Unlike SyncMRStates it moves NO board card: prompt runs are
// issue-less and the self_improve tracking issue is not board-promoted. It only records
// runs.mr_state (via the existing recordMRState, the sole SQL writer — invariant preserved)
// and, on the opened->closed / ->merged edge, cancels an in-flight rework via the existing
// cancelReworkOnClosedMR (issue #853 contract, reused verbatim). Reads the run set from
// ListScheduledMRStateWatchCandidates, which self-evicts a run once its mr_state is terminal.
//
// SCOPE BOUNDARY (PRD #908 design, "self-bounding like Lane B"): closed AND merged are
// terminal for this lane — the candidate query drops a run at mr_state='closed', so a
// scheduled MR that a reviewer closes then REOPENS is not re-watched and does not regain
// mr_rework eligibility. That is deliberate: unlike the issue lane's board-coupled Lane A
// (which keeps watching a closed-issue run for the reopen edge), a board-free recorder that
// kept polling closed MRs forever would reintroduce the unbounded-growth cost the design
// rejected. There is therefore no closed->opened arm below (unlike syncOneMRState).
//
// Like SyncMRStates it never returns a per-candidate error (log-and-skip); only a failure
// to enumerate candidates surfaces to the poller.
func (s *Service) SyncScheduledMRStates(ctx context.Context, repoID uuid.UUID, forgeProjectID int64, f forge.Forge) error {
	candidates, err := s.q.ListScheduledMRStateWatchCandidates(ctx, repoID)
	if err != nil {
		return err
	}
	for _, c := range candidates {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.recordScheduledMRState(ctx, repoID, forgeProjectID, f, c)
	}
	return nil
}

// recordScheduledMRState reconciles one scheduled run's MR state against the stored value,
// board-free. It is a second implementation of syncOneMRState's observe->persist->cancel
// contract WITHOUT the board move; the two must agree on: unknown-state -> no write; the
// NULL bootstrap records without acting; opened->closed and ->merged cancel exactly once;
// locked is transient (no cancel); a forge read failure or a cancel failure leaves mr_state
// unadvanced so the next tick retries.
func (s *Service) recordScheduledMRState(ctx context.Context, repoID uuid.UUID, forgeProjectID int64, f forge.Forge, c store.ListScheduledMRStateWatchCandidatesRow) {
	mr, err := f.GetMergeRequest(ctx, forgeProjectID, c.MrIid.Int64)
	if err != nil {
		slog.Warn("forgesvc: scheduled MR-state read failed", "repo", repoID, "run", c.ID, "mr", c.MrIid.Int64, "error", err)
		return
	}
	observed := mr.State
	if !forge.IsKnownMRState(observed) {
		slog.Warn("forgesvc: ignoring unknown scheduled MR state", "repo", repoID, "run", c.ID, "mr", c.MrIid.Int64, "state", observed)
		return
	}
	if !c.MrState.Valid {
		// Bootstrap: first observation records without acting. No rework was ever gated in
		// for a run whose mr_state was NULL, so there is nothing to cancel; recording an
		// `opened` here is exactly what opens the rework gate for a scheduled MR.
		s.recordMRState(ctx, c.ID, observed)
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
		if err := s.cancelReworkOnClosedMR(ctx, repoID, c.MrIid.Int64); err != nil {
			slog.Warn("forgesvc: cancel scheduled rework on MR terminal state failed, will retry", "repo", repoID, "run", c.ID, "mr", c.MrIid.Int64, "state", observed, "error", err)
			return
		}
		s.recordMRState(ctx, c.ID, observed)
	default:
		// KNOWN non-terminal transition: `locked` (transient during merge processing) or a
		// `locked`->`opened` settle. Record, never cancel — the gate stays open.
		s.recordMRState(ctx, c.ID, observed)
	}
}

package forgesvc

import (
	"context"
	"errors"
	"log/slog"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// activeMRTargetPageSize bounds how many active ci_fix / mr_rework runs one repo pass
// inspects. The store query returns one more row (LIMIT 33) so a 33rd row signals that
// another page exists.
const activeMRTargetPageSize = 32

// SyncActiveMRTargets stops active ci_fix / mr_rework runs whose bound target MR has merged
// or closed (issue #4616). It is stateless apart from a per-repo rotation cursor: each pass
// enumerates at most activeMRTargetPageSize runs from ListActiveMRTargetRuns, reads each
// distinct target MR once, and asks the canceller to stop a run only when the live answer
// proves the MR is the run's own: same iid, a safe source branch equal to the run's
// pipeline_ref, a 40-hex head SHA, and a merged/closed state. Any read error, 404,
// mismatched or malformed answer and any opened/locked/unknown state leaves the run alone.
//
// A canceller error is logged and the pass continues; the next pass retries naturally
// because enumeration re-reads the runs. Only an enumeration failure is returned.
func (s *Service) SyncActiveMRTargets(ctx context.Context, repoID uuid.UUID, forgeProjectID int64, f forge.CodeHost) error {
	if s.terminalTargetCanceller == nil {
		return nil
	}
	after := s.activeTargetCursorFor(repoID)
	rows, err := s.q.ListActiveMRTargetRuns(ctx, store.ListActiveMRTargetRunsParams{RepoID: repoID, AfterID: after})
	if err != nil {
		return err
	}
	if len(rows) > activeMRTargetPageSize {
		rows = rows[:activeMRTargetPageSize]
		s.setActiveTargetCursor(repoID, rows[len(rows)-1].ID)
		slog.Info("forgesvc: active MR-target runs exceed one pass; continuing next pass",
			"event", "active_mr_targets_overflow", "repo", repoID, "page", activeMRTargetPageSize)
	} else {
		s.setActiveTargetCursor(repoID, uuid.Nil)
	}

	summaries := make(map[int64]*forge.MergeRequestSummary, len(rows))
	for _, row := range rows {
		// The cursor already moved past this page, so a deadline hit mid-page leaves the rest of
		// it for the next wrap-around. Accepted: the next page is always reached, and a skipped
		// run is re-enumerated within one full rotation.
		if ctx.Err() != nil {
			return ctx.Err()
		}
		sum, seen := summaries[row.TargetMrIid]
		if !seen {
			got, rerr := f.GetMergeRequestSummary(ctx, forgeProjectID, row.TargetMrIid)
			if rerr != nil {
				if !errors.Is(rerr, forge.ErrMergeRequestNotFound) {
					slog.Warn("forgesvc: active MR-target read failed", "repo", repoID, "mr", row.TargetMrIid, "error", rerr)
				}
			} else {
				sum = &got
			}
			summaries[row.TargetMrIid] = sum // nil caches a failed read for this pass
		}
		if sum == nil {
			continue
		}
		obs, verified := verifiedTerminalTarget(row, *sum)
		if !verified {
			continue
		}
		if err := s.terminalTargetCanceller.CancelRunForTerminalTarget(ctx, repoID, obs); err != nil {
			slog.Warn("forgesvc: terminal-target cancel failed, will retry", "repo", repoID, "run", row.ID, "mr", row.TargetMrIid, "state", obs.State, "error", err)
		}
	}
	return nil
}

// verifiedTerminalTarget builds the cancel observation for row from a live MR summary, or
// reports false when the summary is not provably the run's own merged/closed MR.
func verifiedTerminalTarget(row store.ListActiveMRTargetRunsRow, sum forge.MergeRequestSummary) (workersvc.TerminalTargetObservation, bool) {
	if sum.State != forge.MRStateMerged && sum.State != forge.MRStateClosed {
		return workersvc.TerminalTargetObservation{}, false
	}
	if sum.IID != row.TargetMrIid {
		return workersvc.TerminalTargetObservation{}, false
	}
	if !row.PipelineRef.Valid || !workersvc.IsSafeTerminalTargetBranch(sum.SourceBranch) || sum.SourceBranch != row.PipelineRef.String {
		return workersvc.TerminalTargetObservation{}, false
	}
	if !workersvc.IsFullCommitSHA(sum.HeadSHA) {
		return workersvc.TerminalTargetObservation{}, false
	}
	return workersvc.TerminalTargetObservation{
		RunID:           row.ID,
		Kind:            row.Kind,
		Status:          row.Status,
		WorkerID:        row.WorkerID,
		ClaimGeneration: row.ClaimGeneration,
		TargetMRIID:     row.TargetMrIid,
		SourceBranch:    row.PipelineRef.String,
		State:           sum.State,
	}, true
}

func (s *Service) activeTargetCursorFor(repoID uuid.UUID) uuid.UUID {
	s.activeTargetMu.Lock()
	defer s.activeTargetMu.Unlock()
	return s.activeTargetCursor[repoID]
}

func (s *Service) setActiveTargetCursor(repoID, id uuid.UUID) {
	s.activeTargetMu.Lock()
	defer s.activeTargetMu.Unlock()
	if id == uuid.Nil {
		delete(s.activeTargetCursor, repoID)
		return
	}
	if s.activeTargetCursor == nil {
		s.activeTargetCursor = map[uuid.UUID]uuid.UUID{}
	}
	s.activeTargetCursor[repoID] = id
}

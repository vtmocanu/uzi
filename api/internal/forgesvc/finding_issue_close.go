package forgesvc

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// SyncFindingIssueCloses moves an incidental-finding coordinate to Done when the issue it was
// filed as (#333) has been observed CLOSED — PRD #1183 Child B, M3, the finding twin of
// SyncFiledIssueCloses (judge_issue_close.go).
//
// It rides the existing poller tick, right after the repo's issue cache is refreshed, and makes
// NO forge call of its own: it reads the cache the sync just wrote. The poller holds a synced
// SNAPSHOT of issue state, not a stream of transitions, so "write done while the linked issue is
// closed" would be LEVEL-triggered and re-fire on every tick — silently re-applying after a human
// Undo. The pass therefore acts only on the open→closed EDGE (cached state closed AND
// close_synced_at IS NULL) and consumes it, so it fires EXACTLY ONCE per close and never
// overwrites a human verdict (the apply is guarded status='filed').
//
// Errors are per-repo and non-fatal: an enumeration failure returns (the poller logs it and
// carries on with the next repo), while a per-edge failure is logged and skipped WITHOUT stamping,
// so the unconsumed edge is simply retried on the next tick — the same retry-through-the-poller-
// cadence contract as the judge sync.
func (s *Service) SyncFindingIssueCloses(ctx context.Context, repoID uuid.UUID) error {
	edges, err := s.q.ListFindingIssueCloseEdges(ctx, repoID)
	if err != nil {
		return err
	}
	for _, e := range edges {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.syncOneFindingIssueClose(ctx, repoID, e)
	}
	return nil
}

// syncOneFindingIssueClose applies one close edge: the automatic Done and the edge stamp together,
// in the single guarded statement ApplyFindingIssueCloseEdge runs. rows-affected is 1 on a real
// apply and 0 when the guard already failed (the coordinate raced or a human superseded it) — both
// are success; only a query error is logged and left for the next tick to retry.
func (s *Service) syncOneFindingIssueClose(ctx context.Context, repoID uuid.UUID, e store.ListFindingIssueCloseEdgesRow) {
	rows, err := s.q.ApplyFindingIssueCloseEdge(ctx, e.ID)
	if err != nil {
		// The statement is atomic, so nothing was applied and the edge is still open: the next
		// tick retries it.
		slog.Warn("forgesvc: finding-issue close edge failed",
			"repo", repoID, "disposition", e.ID, "error", err)
		return
	}
	if rows > 0 {
		slog.Info("forgesvc: finding auto-resolved by issue close",
			"repo", repoID, "disposition", e.ID, "issue_iid", e.FiledIssueIid.Int64)
	}
}

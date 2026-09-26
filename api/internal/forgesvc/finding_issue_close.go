package forgesvc

import (
	"context"
	"errors"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

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
// close_synced_at IS NULL) and consumes it, so it fires EXACTLY ONCE per close. It consumes the
// edge without changing a human verdict: a coordinate a human already marked done (issue #1723)
// has its edge stamped but keeps its status, set_via NULL and resolved_at, so a later human Undo
// back to filed is not auto-resolved over by the close this pass already observed.
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

// syncOneFindingIssueClose applies one close edge in the single guarded statement
// ApplyFindingIssueCloseEdge runs: the edge stamp always, and the automatic Done only on a still-
// filed coordinate. The returned set_via separates the outcomes: 'issue_close' is a real
// auto-resolve (logged), NULL is an edge consumed under a human done (verdict untouched), and
// pgx.ErrNoRows means the guard already failed (raced, or the coordinate moved meanwhile). All
// three are success; only a query error is logged and left for the next tick to retry.
func (s *Service) syncOneFindingIssueClose(ctx context.Context, repoID uuid.UUID, e store.ListFindingIssueCloseEdgesRow) {
	setVia, err := s.q.ApplyFindingIssueCloseEdge(ctx, e.ID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return
		}
		// The statement is atomic, so nothing was applied and the edge is still open: the next
		// tick retries it.
		slog.Warn("forgesvc: finding-issue close edge failed",
			"repo", repoID, "disposition", e.ID, "error", err)
		return
	}
	if setVia.Valid && setVia.String == "issue_close" {
		slog.Info("forgesvc: finding auto-resolved by issue close",
			"repo", repoID, "disposition", e.ID, "issue_iid", e.FiledIssueIid.Int64)
		return
	}
	slog.Debug("forgesvc: finding close edge consumed under a human verdict",
		"repo", repoID, "disposition", e.ID, "issue_iid", e.FiledIssueIid.Int64)
}

package forgesvc

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
	"gitlab.example.com/vtmocanu/uzi/api/internal/workersvc"
)

// FiledIssueCloseBatch bounds how many close edges one repo's tick applies. Each edge is a
// single statement, so this caps the serial work a repo can impose on the shared poller
// tick; leftovers ride the next tick.
const FiledIssueCloseBatch = 200

// SyncFiledIssueCloses moves a judge recommendation to Done when the issue it was filed as
// (#68) has been observed CLOSED — PRD #98 Decision 6, the Filed→Done sync.
//
// It rides the existing poller tick, right after the repo's issue cache is refreshed, and
// makes NO forge call of its own: it reads the cache the sync just wrote. That is the whole
// reason the edge marker exists. The poller holds a synced SNAPSHOT of issue state, not a
// stream of transitions, so "write done while the linked issue is closed" would be
// LEVEL-triggered and re-fire on every tick — silently re-applying after a human Undo. The
// pass therefore acts only on the open→closed EDGE (cached state closed AND
// close_synced_at IS NULL) and consumes it.
//
// Two guarantees, each enforced in SQL rather than here (see queries/judge_issue_close.sql):
//
//   - EXACTLY ONCE per close — the edge predicate plus the stamp.
//   - NEVER overwrites a human verdict — the write is INSERT … ON CONFLICT DO NOTHING, not
//     #94's DO-UPDATE upsert, so a coordinate the user already dismissed keeps their
//     verdict.
//
// Errors are per-repo and non-fatal: enumeration failure returns (the poller logs it and
// carries on with the next repo), while a per-edge failure is logged and skipped WITHOUT
// stamping, so the unconsumed edge is simply retried on the next tick — the same
// retry-through-the-poller-cadence contract as the MR-close watcher (mr_watch.go).
//
// The per-tick batch is BOUNDED (FiledIssueCloseBatch). Edges are near-zero in practice, but
// a mass issue closure would otherwise make one repo's tick run a statement per edge
// serially and delay every repo behind it. The remainder is picked up next tick — an
// unconsumed edge stays unconsumed, so nothing is lost by deferring it.
func (s *Service) SyncFiledIssueCloses(ctx context.Context, repoID uuid.UUID) error {
	edges, err := s.q.ListFiledIssueCloseEdges(ctx, store.ListFiledIssueCloseEdgesParams{
		RepoID: repoID,
		Lim:    FiledIssueCloseBatch,
	})
	if err != nil {
		return err
	}
	for _, e := range edges {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.syncOneFiledIssueClose(ctx, repoID, e)
	}
	return nil
}

// syncOneFiledIssueClose applies one close edge in a SINGLE atomic statement: the automatic
// Done and the edge stamp together.
//
// This used to be two calls, insert-then-stamp. That was the right ordering of the two
// (stamping first would lose the disposition outright if the process died in between), but
// it was not sufficient: if the process died or the stamp errored inside the window, the
// edge stayed open, and a user who hit Undo BEFORE the next tick would have it silently
// re-applied — the retry's insert is NOT a no-op once the row has been deleted, which
// defeats exactly the guarantee M6 exists to provide. One statement removes the window
// rather than narrowing it (audit B7).
//
// The edge is consumed even when the insert wrote nothing (the user already had a verdict).
// That is deliberate: leaving it open would make the pass re-examine the same row on every
// future tick forever.
func (s *Service) syncOneFiledIssueClose(ctx context.Context, repoID uuid.UUID, e store.ListFiledIssueCloseEdgesRow) {
	res, err := s.q.ApplyFiledIssueCloseEdge(ctx, store.ApplyFiledIssueCloseEdgeParams{
		ReviewID: e.ReviewID,
		Category: e.Category,
		Target:   e.Target,
		// Re-stamped from the CURRENT rationale, like every other disposition write
		// (#94 Decision 3) — so the panel's stale flag compares against the same key.
		// Empty when the recommendation was re-judged away under a surviving filed link.
		RationaleHash: workersvc.RationaleHash(e.RationaleMd),
		FiledID:       e.FiledID,
	})
	if err != nil {
		// Atomic: nothing was applied, so the edge is still open and the next tick retries.
		slog.Warn("forgesvc: filed-issue close edge failed",
			"repo", repoID, "review", e.ReviewID, "category", e.Category, "error", err)
		return
	}
	if res.Disposed > 0 {
		slog.Info("forgesvc: recommendation auto-resolved by issue close",
			"repo", repoID, "review", e.ReviewID, "category", e.Category, "target", e.Target,
			"issue_iid", e.FiledIssueIid.Int64)
	}
}

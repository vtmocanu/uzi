package forgesvc

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
	"gitlab.example.com/vtmocanu/uzi/api/internal/workersvc"
)

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
func (s *Service) SyncFiledIssueCloses(ctx context.Context, repoID uuid.UUID) error {
	edges, err := s.q.ListFiledIssueCloseEdges(ctx, repoID)
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

// syncOneFiledIssueClose applies one close edge: write the automatic Done, then consume the
// edge. The ORDER IS LOAD-BEARING. Insert-then-stamp means a crash in between leaves the
// edge unconsumed, so the next tick retries the insert (idempotent — DO NOTHING) and
// stamps. Stamping first would lose the disposition outright if the process died before the
// insert, with nothing left to indicate it should have happened.
//
// A stamp still happens when the insert wrote NOTHING (rows == 0, i.e. the user already had
// a verdict on this coordinate). That is deliberate: the close has been accounted for, and
// leaving the edge open would make the pass re-examine the same row on every future tick
// forever.
func (s *Service) syncOneFiledIssueClose(ctx context.Context, repoID uuid.UUID, e store.ListFiledIssueCloseEdgesRow) {
	rows, err := s.q.InsertIssueCloseDisposition(ctx, store.InsertIssueCloseDispositionParams{
		ReviewID: e.ReviewID,
		Category: e.Category,
		Target:   e.Target,
		// Re-stamped from the CURRENT rationale, like every other disposition write
		// (#94 Decision 3) — so the panel's stale flag compares against the same key.
		// Empty when the recommendation was re-judged away under a surviving filed link.
		RationaleHash: workersvc.RationaleHash(e.RationaleMd),
	})
	if err != nil {
		// Not an edge: leave close_synced_at NULL so the next tick retries.
		slog.Warn("forgesvc: filed-issue close disposition failed",
			"repo", repoID, "review", e.ReviewID, "category", e.Category, "error", err)
		return
	}
	if _, err := s.q.MarkFiledIssueCloseSynced(ctx, e.FiledID); err != nil {
		// The disposition landed but the edge is unconsumed. Harmless: the retry's insert
		// is a DO-NOTHING no-op and only the stamp is re-attempted.
		slog.Warn("forgesvc: filed-issue close edge stamp failed",
			"repo", repoID, "filed", e.FiledID, "error", err)
		return
	}
	if rows > 0 {
		slog.Info("forgesvc: recommendation auto-resolved by issue close",
			"repo", repoID, "review", e.ReviewID, "category", e.Category, "target", e.Target,
			"issue_iid", e.FiledIssueIid.Int64)
	}
}

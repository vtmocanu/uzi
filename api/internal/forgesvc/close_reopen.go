package forgesvc

import (
	"context"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// CloseIssue closes an issue forge-first (PRD #1034 M2), then flips the cache
// state to closed. It mirrors SetIssueLabel's contract: diff-first idempotency
// (if the cache already shows the issue closed, NO forge call is made and the row
// is returned unchanged, success criterion 5), forge write next, and cache write
// only on forge success — so a forge error leaves the cache untouched and the card
// snaps back (the AutoMove/SetIssueLabel snap-back contract).
//
// Close is state-only: labels, assignee_ids and board_position are deliberately
// left as the last sync/move stored them (a closed card renders in the Closed lane
// via board.ResolveColumn regardless of its column labels), which is why this uses
// the narrow UpdateIssueState query rather than UpsertIssueLabels.
func (s *Service) CloseIssue(ctx context.Context, f forge.Forge, forgeProjectID int64, issue store.Issue) (store.Issue, error) {
	// Diff-first: if the cache already reflects the desired state, skip the forge
	// entirely and return the row unchanged (idempotent close, no forge call).
	if issue.State == string(forge.StateClosed) {
		return issue, nil
	}
	// Forge-first: close remotely before touching the cache. On failure the cache is
	// untouched and the caller surfaces a 502 / snaps the card back.
	if err := f.SetIssueState(ctx, forgeProjectID, issue.ForgeIssueIid, forge.StateClosed); err != nil {
		return store.Issue{}, err
	}
	return s.q.UpdateIssueState(ctx, store.UpdateIssueStateParams{
		RepoID:        issue.RepoID,
		ForgeIssueIid: issue.ForgeIssueIid,
		State:         string(forge.StateClosed),
	})
}

// ReopenIssue reopens an issue forge-first (PRD #1034 M2) and then moves it to the
// drop-target column. Ordering matters: state flips FIRST (both forge and cache, and
// board_position is nulled by ReopenIssueState) so the reopened card lands at the
// bottom of its lane, and only then does the label move run.
//
// 🔴 Cache-clobber trap: AutoMove re-writes State into UpsertIssueLabels
// (state = EXCLUDED.state, forge.sql), so the move MUST run against a struct whose
// State is already "opened" — passing the stale "closed" struct would clobber the
// cache back to closed (forge opened, cache closed, desync until the next poll). This
// is why AutoMove is called with the row ReopenIssueState returns (issue = reopened),
// never with the original closed struct.
//
// Idempotency: like CloseIssue, the forge reopen + cache flip is skipped when the
// cache already shows the issue opened (no forge call), but the label move still runs
// so a reopen-onto-a-column of an already-open issue is still a plain move.
//
// Partial failure: if the move half fails, the reopened-but-unmoved row (state opened,
// board_position nulled) is returned with the error so the handler can surface a 502
// for the move while the card still renders open — never lost.
func (s *Service) ReopenIssue(ctx context.Context, f forge.Forge, forgeProjectID int64, issue store.Issue, columns []store.BoardColumn, target string) (store.Issue, error) {
	if issue.State != string(forge.StateOpened) {
		// Forge-first: reopen remotely before touching the cache. On failure the cache
		// is untouched (no ReopenIssueState, no UpsertIssueLabels) and the card snaps back.
		if err := f.SetIssueState(ctx, forgeProjectID, issue.ForgeIssueIid, forge.StateOpened); err != nil {
			return store.Issue{}, err
		}
		reopened, err := s.q.ReopenIssueState(ctx, store.ReopenIssueStateParams{
			RepoID:        issue.RepoID,
			ForgeIssueIid: issue.ForgeIssueIid,
		})
		if err != nil {
			return store.Issue{}, err
		}
		// State now "opened", board_position nulled — feeding THIS row to AutoMove is
		// what stops the EXCLUDED.state write from clobbering the flip back to closed.
		issue = reopened
	}
	moved, err := s.AutoMove(ctx, f, forgeProjectID, issue, columns, target)
	if err != nil {
		// Reopened-but-unmoved: return the opened row so the handler 502s the move half
		// while the card still renders open.
		return issue, err
	}
	return moved, nil
}

package forgesvc

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// SyncIssues is the one entry point for a full issue sync into the cache. tr is the
// repo's issue side as IssueTrackerForRepo returned it: a linked repo's tracker runs
// the linked sync (the eligibility tag's issues, evicting the rest), anything else is
// a forge driver and runs FullSync. The choice is derived from tr, so it cannot
// disagree with it. userID is the repo's owner: the linked sync reads the link
// through the owner-scoped repo row, never by repo id alone.
func (s *Service) SyncIssues(ctx context.Context, userID, repoID uuid.UUID, forgeProjectID int64, tr forge.IssueTracker) (Marks, error) {
	if isLinkedTracker(tr) {
		return s.syncLinkedIssues(ctx, userID, repoID, forgeProjectID, tr, Marks{}, true)
	}
	return s.FullSync(ctx, repoID, forgeProjectID, tr)
}

// SyncIssuesIncremental is SyncIssues for the poller's between-reconcile tick. For a
// linked repo it walks the same listing as the full form and upserts what it
// returns, but never evicts and never filters by m (see syncLinkedIssues).
func (s *Service) SyncIssuesIncremental(ctx context.Context, userID, repoID uuid.UUID, forgeProjectID int64, tr forge.IssueTracker, m Marks) (Marks, error) {
	if isLinkedTracker(tr) {
		return s.syncLinkedIssues(ctx, userID, repoID, forgeProjectID, tr, m, false)
	}
	return s.IncrementalSync(ctx, repoID, forgeProjectID, tr, m)
}

// syncLinkedIssues caches the issues of a linked repo's eligibility tag (D10, D14).
// It reads the link from the owner-scoped repo row (the row repoSource reads), lists
// the tag's issues, and upserts every one with issue_key = short + "-" + number. No
// open-issue and no finding fetch: only tagged issues are cached.
//
// The listing never carries UpdatedAfter, in either form: the id-scoped listing has
// no server-side date filter, and offset paging can miss an issue that shifts during
// a walk, so a mark that filtered it could hide the missed issue. Every later sync of
// either form walks the whole listing again and caches it as soon as a walk does not
// miss it; marks are bookkeeping only and are advanced by the newest updated time
// returned.
//
// Only the full form evicts, and only after the listing and every upsert succeeded;
// an error anywhere returns before the eviction with marks unchanged, so a failed
// listing is never read as an empty set.
func (s *Service) syncLinkedIssues(ctx context.Context, userID, repoID uuid.UUID, forgeProjectID int64, tr forge.IssueTracker, m Marks, full bool) (Marks, error) {
	src, err := s.repoSource(ctx, userID, repoID)
	if err != nil {
		return m, fmt.Errorf("read repo issue source: %w", err)
	}
	if !src.IssueTrackerProjectShort.Valid || !src.EligibilityTagName.Valid {
		return m, ErrLinkedRepoUnresolvable
	}
	issues, err := tr.ListIssues(ctx, forgeProjectID, forge.ListIssuesOptions{Labels: []string{src.EligibilityTagName.String}})
	if err != nil {
		return m, err
	}
	if err := s.upsertIssues(ctx, repoID, issues, src.IssueTrackerProjectShort.String); err != nil {
		return m, err
	}
	if full {
		keep := make([]int64, 0, len(issues))
		for _, is := range issues {
			keep = append(keep, is.IID)
		}
		if _, err := s.q.DeleteIssuesNotIn(ctx, store.DeleteIssuesNotInParams{RepoID: repoID, KeepIids: keep}); err != nil {
			return m, err
		}
	}
	return m.Advance(Marks{PRD: maxUpdatedAt(issues)}), nil
}

package forgesvc

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/issuesource"
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
// the tag's issues, and upserts every one with issue_key = short + "-" + number and the board column its
// State maps to. No
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
	// The listing carries each issue's State so the board column can be cached (PRD
	// #4173 M5). The write guard of a linked repo always offers it; a linked side that
	// cannot is a fault, never an empty listing, which the full form would read as
	// "evict everything".
	lister, ok := tr.(issuesource.IssueStateLister)
	if !ok {
		return m, fmt.Errorf("linked issue sync: %w", errNoStateListing)
	}
	stated, err := lister.ListIssuesWithState(ctx, forgeProjectID, forge.ListIssuesOptions{Labels: []string{src.EligibilityTagName.String}})
	if err != nil {
		return m, err
	}
	stateColumns, err := reverseColumnStates(src.ColumnStates)
	if err != nil {
		return m, err
	}
	issues := make([]forge.Issue, 0, len(stated))
	columns := make(map[int64]string, len(stated))
	for _, si := range stated {
		issues = append(issues, si.Issue)
		// A resolved issue, the open state and a State no column maps to are all the Open
		// lane (empty column, stored as NULL); closed is decided by issues.state.
		if si.Issue.State == string(forge.StateClosed) {
			continue
		}
		if col, mapped := stateColumns[si.State]; mapped {
			columns[si.Issue.IID] = col
		}
	}
	if err := s.upsertIssues(ctx, repoID, issues, src.IssueTrackerProjectShort.String, columns); err != nil {
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

// reverseColumnStates inverts the stored column-to-State mapping (a JSON object of
// board column label to State name) into State name to column label. The stored
// States are distinct per repo, so the inverse is a function.
func reverseColumnStates(raw []byte) (map[string]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var columns map[string]string
	if err := json.Unmarshal(raw, &columns); err != nil {
		return nil, fmt.Errorf("decode stored column states: %w", err)
	}
	out := make(map[string]string, len(columns))
	for col, state := range columns {
		out[state] = col
	}
	return out, nil
}

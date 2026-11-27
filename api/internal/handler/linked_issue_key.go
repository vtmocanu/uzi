package handler

import (
	"github.com/vtmocanu/uzi/api/internal/issuetracker"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// linkedIssueKey is the issue tracker key ("ABC-12") of issue iid on repo: the project
// short name stored at link time, a dash and the issue number. It is empty for a repo not
// linked to an issue tracker, so every unlinked payload stays as it was. It is derived, not
// read from issues.issue_key, so a row a create path wrote without a key still shows one
// (PRD #4173 M6). The format matches the claim assembly's key for a run.
func linkedIssueKey(repo store.GetRepoForUserRow, iid int64) string {
	if !repo.IssueTrackerConnectionID.Valid || !repo.IssueTrackerProjectShort.Valid || repo.IssueTrackerProjectShort.String == "" {
		return ""
	}
	return issuetracker.IssueKey(repo.IssueTrackerProjectShort.String, iid)
}

// stampLinkedIssueKeys sets each card's issue key to the derived one on a linked repo and
// leaves the cards of an unlinked repo untouched.
func stampLinkedIssueKeys(cards []cardDTO, repo store.GetRepoForUserRow) {
	for i := range cards {
		if k := linkedIssueKey(repo, cards[i].IID); k != "" {
			cards[i].IssueKey = k
		}
	}
}

// linkedEligibilityLabel is the stored eligibility tag name of a linked repo, "" for an
// unlinked one: what the board and issue payloads report as eligibility_label.
func linkedEligibilityLabel(repo store.GetRepoForUserRow) string {
	name, _ := linkedEligibility(repo)
	return name
}

package workersvc

import (
	"testing"

	"github.com/vtmocanu/uzi/api/internal/capability"
)

// PRD #4173 M4 acceptance 12: the claim's issue key comes from the repo's project short name
// and the run's iid, so it survives the cached issues row being evicted (deleted).
func TestIssueKeyClaimSurvivesADeletedIssueRowLiveDB(t *testing.T) {
	f := newCRFix(t)
	f.linkFixtureRepo(t)
	f.env.exec(`INSERT INTO issues (repo_id, forge_issue_iid, title, state, labels, web_url, forge_updated_at, synced_at, issue_key)
	            VALUES ($1, 12, 't', 'opened', '[]', 'https://yt.test/i', now(), now(), 'ABC-12')`, f.repo)
	run := f.startQueuedIssueRun(t, 12)
	f.env.exec(`DELETE FROM issues WHERE repo_id = $1 AND forge_issue_iid = 12`, f.repo)

	capable := f.protocolWorker(t, 0, capability.IssueKeyV1)
	p := f.acceptClaim(t, capable, run, "linked run whose cached issue row is gone")
	if p.IssueKey != "ABC-12" {
		t.Fatalf("issue_key = %q, want ABC-12", p.IssueKey)
	}
}

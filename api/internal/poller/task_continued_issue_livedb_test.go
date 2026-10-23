package poller

import (
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Run creation is permissive so a missed precheck cannot hide behind a second
// guard. Candidate reads, active identity and transition receipts are real SQL.
func TestLinkedTaskAutopilotTransitionLiveDB(t *testing.T) {
	for _, status := range []string{"running", "pool_wait"} {
		t.Run(status, func(t *testing.T) {
			e := newMergedPollEnv(t)
			e.exec("UPDATE users SET autopilot_enabled=true WHERE id=$1", e.worker.UserID)
			e.exec("UPDATE forge_connections SET base_url=$2,human_username='owner' WHERE id=$1", e.conn, "https://"+uuid.NewString()+".forge.test")
			e.exec(`INSERT INTO user_secrets(id,user_id,kind,label,is_default,ciphertext,sealed_with)
    VALUES($1,$2,'anthropic_token',$3,true,$4,'master')`, uuid.New(), e.worker.UserID, uuid.NewString(), []byte("fixture"))
			e.exec(`INSERT INTO issues(repo_id,forge_issue_iid,title,state,labels,author,web_url,has_prd_link,forge_updated_at,synced_at)
    VALUES($1,17,'candidate','opened','["uzi","autopilot"]','owner','https://forge.test/17',false,now(),now())`, e.repo)
			linkedID := uuid.New()
			e.exec(`INSERT INTO runs(id,user_id,repo_id,kind,continued_issue_iid,issue_title,issue_description,branch,status)
    VALUES($1,$2,$3,'task',17,'linked','body',$4,$5)`, linkedID, e.worker.UserID, e.repo, "uzi/task/"+uuid.NewString(), status)
			runs := &apRuns{}
			f := &apForge{events: map[int64][]forge.LabelEvent{17: {addEvt(10, "owner")}}, issue: forge.Issue{IID: 17, Description: "task body"}}
			ap := NewAutopilot(e.q, runs, apLabeler{label: apLabel})
			repo := store.ListEnabledReposWithConnectionsRow{ID: e.repo, ConnectionID: e.conn, ForgeProjectID: 42}
			receipt := func(want int64) {
				t.Helper()
				got, err := e.q.GetAutopilotTrigger(e.ctx, store.GetAutopilotTriggerParams{RepoID: e.repo, IssueIid: 17})
				if err != nil || got.LastEventID != want {
					t.Fatalf("receipt=%+v error=%v want event=%d", got, err, want)
				}
			}
			quiet := func() {
				t.Helper()
				if len(runs.calls) != 0 || len(f.noteAttempts) != 0 || f.fetches != 0 {
					t.Fatalf("swallowed event caused create=%d comments=%d issue fetches=%d", len(runs.calls), len(f.noteAttempts), f.fetches)
				}
			}
			ap.detect(e.ctx, repo, f)
			receipt(10)
			quiet()
			e.exec("UPDATE runs SET status='completed',finished_at=now() WHERE id=$1", linkedID)
			ap.detect(e.ctx, repo, f) // consumed application never replays after terminal
			receipt(10)
			quiet()
			f.events[17] = append(f.events[17], remEvt(11, "owner"))
			ap.detect(e.ctx, repo, f)
			receipt(10)
			quiet()
			f.events[17] = append(f.events[17], addEvt(12, "owner"))
			ap.detect(e.ctx, repo, f)
			receipt(12)
			if len(runs.calls) != 1 || runs.calls[0].iid != 17 || runs.calls[0].repoID != e.repo || len(f.noteAttempts) != 0 || f.fetches != 1 {
				t.Fatalf("new application must create once: calls=%+v comments=%v fetches=%d", runs.calls, f.noteAttempts, f.fetches)
			}
			ap.detect(e.ctx, repo, f)
			if len(runs.calls) != 1 || f.fetches != 1 || len(f.noteAttempts) != 0 {
				t.Fatal("new event replayed")
			}
		})
	}
}

package workersvc

import (
	"encoding/json"
	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/issueinput"
	"strings"
	"testing"
)

func TestM2IssueCreateClaimPersistedDowngradeLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	user, worker, repo := env.seedCodexInfra(t)
	enableClaimAssembly(t, env, user)
	env.exec(`UPDATE forge_connections SET bot_forge_user_id=1 WHERE id=(SELECT connection_id FROM repos WHERE id=$1)`, repo)
	env.exec(`INSERT INTO issues(id,repo_id,forge_issue_iid,title,state,labels,web_url,has_prd_link,forge_updated_at,synced_at)
 VALUES($1,$2,4,'cached stale title','opened','["uzi"]','https://forge.e2e/i',false,now(),now())`, uuid.New(), repo)
	svc := New(env.q, env.box, testParams())
	svc.SetForges(m2Builder{&m2Forge{disposition: forge.AuthorNotEligible, comments: []forge.IssueComment{{AuthorForgeUserID: 3, Body: "WITHHELD_RAW_CREATE"}, {AuthorForgeUserID: 4, Body: "UNKNOWN_RAW_CREATE"}}}})
	run, err := svc.CreateScheduledAutopilotRun(env.ctx, user, repo, 4, "raw argument", nil, nil, nil, false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if run.AutoApprove || strings.Join(run.AutoApproveBlockedReasons, ",") != "author_not_eligible,permission_unknown" || run.IssueTitle != "captured title" || run.IssueSavedBody.String != "captured body" || !run.IssueRawDigest.Valid {
		t.Fatalf("persisted row=%+v", run)
	}
	// Drive the real claim query and service payload assembly over the committed row.
	env.exec(`UPDATE runs SET created_at=now()-interval '3 hours',updated_at=now()-interval '3 hours' WHERE id=$1`, run.ID)
	claimed, err := env.q.ClaimRun(env.ctx, claimRunParams(wkrRow(t, env, worker)))
	if err != nil {
		t.Fatal(err)
	}
	if claimed.ID != run.ID || claimed.AutoApprove || len(claimed.AutoApproveBlockedReasons) != 2 {
		t.Fatalf("claim row=%+v", claimed)
	}
	payload, err := svc.assembleClaim(env.ctx, wkrRow(t, env, worker), claimed)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if payload.IssueDescription != "captured body" {
		t.Fatalf("capture missing: %+v", payload)
	}
	if payload.PlanApproved || payload.IssueComments == nil || payload.IssueComments.Comments[0].Reason != issueinput.NotEligible {
		t.Fatalf("claim=%+v", payload)
	}
	for _, secret := range []string{"WITHHELD_RAW_CREATE", "UNKNOWN_RAW_CREATE", "raw argument"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("claim leaked %s", secret)
		}
	}
	conn, err := svc.ForgeConnForRun(env.ctx, wkrRow(t, env, worker), run.ID)
	if err != nil || conn.IssueIID == nil || *conn.IssueIID != 4 || conn.SavedTitle != "captured title" || conn.SavedBody != "captured body" || conn.RawDigest == nil {
		t.Fatalf("owned conn=%+v err=%v", conn, err)
	}
	// Requeue and reclaim the same persisted run after the forge author is promoted.
	svc.SetForges(m2Builder{&m2Forge{disposition: forge.AuthorEligible}})
	env.exec("UPDATE runs SET status='queued',worker_id=NULL,updated_at=now()-interval '3 hours' WHERE id=$1", run.ID)
	reclaimed, err := env.q.ClaimRun(env.ctx, claimRunParams(wkrRow(t, env, worker)))
	if err != nil {
		t.Fatal(err)
	}
	if reclaimed.ID != run.ID || reclaimed.IssueTitle != run.IssueTitle || reclaimed.IssueSavedBody != run.IssueSavedBody || reclaimed.IssueRawDigest != run.IssueRawDigest || strings.Join(reclaimed.AutoApproveBlockedReasons, ",") != "author_not_eligible,permission_unknown" || reclaimed.AutoApprove {
		t.Fatalf("reclaim changed saved input: %+v", reclaimed)
	}
	resumed, err := svc.assembleClaim(env.ctx, wkrRow(t, env, worker), reclaimed)
	if err != nil {
		t.Fatal(err)
	}
	resumedRaw, err := json.Marshal(resumed)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.IssueDescription != "captured body" || resumed.IssueComments == nil || resumed.IssueComments.Comments[0].Body != issueinput.Placeholder || resumed.IssueComments.Comments[0].Reason != issueinput.NotEligible || strings.Contains(string(resumedRaw), "WITHHELD_RAW_CREATE") || strings.Contains(string(resumedRaw), "UNKNOWN_RAW_CREATE") {
		t.Fatalf("resumed projection=%s", resumedRaw)
	}
	// A legacy thread cannot regain access on a resumed claim.
	claimed.IssueComments = []byte(`{"comments":[{"author_username":"legacy","body":"LEGACY_RAW"}],"truncated":false}`)
	legacy, err := svc.assembleClaim(env.ctx, wkrRow(t, env, worker), claimed)
	if err != nil {
		t.Fatal(err)
	}
	if legacy.IssueComments == nil || legacy.IssueComments.Comments[0].Body != issueinput.Placeholder || legacy.IssueComments.Comments[0].Reason != issueinput.Unknown {
		t.Fatal("legacy body not withheld")
	}
	claimed.IssueComments = []byte("malformed")
	malformed, err := svc.assembleClaim(env.ctx, wkrRow(t, env, worker), claimed)
	if err != nil || malformed.IssueComments != nil {
		t.Fatalf("malformed=%+v err=%v", malformed, err)
	}
}

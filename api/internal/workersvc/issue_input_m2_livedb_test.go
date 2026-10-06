package workersvc

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/issueinput"
)

func TestM2IssueCreateClaimPersistedDowngradeLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	user, worker, repo := env.seedCodexInfra(t)
	enableClaimAssembly(t, env, user)
	env.exec(`UPDATE forge_connections SET bot_forge_user_id=1 WHERE id=(SELECT connection_id FROM repos WHERE id=$1)`, repo)
	env.exec(`INSERT INTO issues(id,repo_id,forge_issue_iid,title,state,labels,web_url,has_prd_link,forge_updated_at,synced_at)
 VALUES($1,$2,4,'cached stale title','opened','["uzi"]','https://forge.e2e/i',false,now(),now())`, uuid.New(), repo)
	svc := New(env.q, env.box, testParams())
	svc.SetTxBeginner(env.pool)
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
	claimed = reclaimed
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
	// Persist a legacy thread, then publish a real gate before the owner's approval.
	legacyThread := claimed.IssueComments
	env.exec("UPDATE runs SET issue_comments=$2, status='running' WHERE id=$1", run.ID, legacyThread)
	baseline, err := svc.GetRun(env.ctx, user, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	env.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", worker, []string{capability.InputReceiptsV1, capability.GateRevisionV1})
	wkr := wkrRow(t, env, worker)
	req := gateReq(&reclaimed.ClaimGeneration, uid())
	// This plan needs no capabilities; derive the requirement through the gate report.
	req.RequiredCapabilities = &[]string{}
	gated, applied, err := svc.SetState(env.ctx, wkr, run.ID, req)
	if err != nil || !applied || gated.Status != "awaiting_approval" || gated.GateRevision != 1 {
		t.Fatalf("publish gate: applied=%v row=%+v err=%v", applied, gated, err)
	}
	approval, err := svc.SubmitInput(env.ctx, user, run.ID, "approve_plan", "", nil)
	if err != nil || approval.ServerSide {
		t.Fatalf("submit approval=%+v err=%v", approval, err)
	}
	var binding string
	var revision int64
	if err := env.pool.QueryRow(env.ctx, "SELECT gate_binding, gate_revision FROM run_user_inputs WHERE id=$1 AND run_id=$2 AND kind='approve_plan'", approval.ID, run.ID).Scan(&binding, &revision); err != nil || binding != "bound" || revision != gated.GateRevision {
		t.Fatalf("approval binding=%s revision=%d err=%v", binding, revision, err)
	}
	ids := []int64{approval.ID}
	acked, err := svc.AckInputs(env.ctx, wkr, run.ID, reclaimed.ClaimGeneration, ids)
	if err != nil || !acked.Active {
		t.Fatalf("approval ACK=%+v err=%v", acked, err)
	}
	receipt, err := svc.ApplyInputs(env.ctx, wkr, run.ID, reclaimed.ClaimGeneration, ids)
	if err != nil || !receipt.Active {
		t.Fatalf("approval APPLIED=%+v err=%v", receipt, err)
	}
	var received, acted bool
	if err := env.pool.QueryRow(env.ctx, "SELECT consumed_at IS NOT NULL, applied_at IS NOT NULL FROM run_user_inputs WHERE id=$1 AND run_id=$2", approval.ID, run.ID).Scan(&received, &acted); err != nil || !received || !acted {
		t.Fatalf("persisted approval receipt: received=%v applied=%v err=%v", received, acted, err)
	}
	afterApproval, err := svc.GetRun(env.ctx, user, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if afterApproval.IssueTitle != baseline.IssueTitle || afterApproval.IssueSavedBody != baseline.IssueSavedBody || afterApproval.IssueRawDigest != baseline.IssueRawDigest || !slices.Equal(afterApproval.AutoApproveBlockedReasons, baseline.AutoApproveBlockedReasons) || afterApproval.AutoApprove != baseline.AutoApprove || !bytes.Equal(afterApproval.IssueComments, baseline.IssueComments) {
		t.Fatalf("approval changed saved input: %+v", afterApproval)
	}
	approvedPayload, err := svc.assembleClaim(env.ctx, wkr, afterApproval)
	if err != nil || approvedPayload.IssueComments == nil || len(approvedPayload.IssueComments.Comments) != 1 || approvedPayload.IssueComments.Comments[0].Body != issueinput.Placeholder || approvedPayload.IssueComments.Comments[0].Reason != issueinput.Unknown {
		t.Fatalf("approval legacy projection=%+v err=%v", approvedPayload, err)
	}
	claimed = afterApproval
	claimed.IssueComments = []byte("malformed")
	malformed, err := svc.assembleClaim(env.ctx, wkrRow(t, env, worker), claimed)
	if err != nil || malformed.IssueComments != nil {
		t.Fatalf("malformed=%+v err=%v", malformed, err)
	}
}

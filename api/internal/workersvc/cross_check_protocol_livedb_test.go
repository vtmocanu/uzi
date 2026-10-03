package workersvc

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestPlanCrossCheckProtocolLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	f := newSubscriptionFixture(t, env)
	env.exec(`UPDATE user_secrets SET is_default=true WHERE id=$1`, f.aliasID)
	original := mustRun(t, env, f.runID)
	leadID := uuid.New()
	env.exec(`INSERT INTO runs (id,user_id,repo_id,worker_id,issue_iid,issue_title,issue_description,status,harness,
   auto_approve,plan_cross_check_required,plan_source,claim_generation)
   VALUES ($1,$2,$3,$4,42,'issue title','issue body','running','claude',true,true,'agent',1)`,
		leadID, f.userID, original.RepoID, f.workerID)
	svc := New(env.q, env.box, testParams())
	svc.SetTxBeginner(env.pool)
	candidate := PlanCrossCheckCandidate{PlanMd: "  Plan \u202e", Milestones: []byte(`[]`), RequiredCapabilities: []string{},
		RequiredTools: []string{}, SizeClass: "s", BaseCommit: strings.Repeat("a", 40), PlanningDiff: "diff"}
	cc, err := svc.SubmitPlanCrossCheck(env.ctx, f.wkr, leadID, 1, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if cc.PlanMd.String != "Plan" || !cc.CheckerRunID.Valid {
		t.Fatal("canonical candidate/child not stored atomically")
	}
	childID := uuid.UUID(cc.CheckerRunID.Bytes)
	child := mustRun(t, env, childID)
	if child.Harness != "codex" || child.Kind != "cross_check" || !child.ReportOnly || !child.Priority.Valid || child.Priority.Int16 != 2 || !child.CodexSecretID.Valid || child.AnthropicSecretID.Valid {
		t.Fatalf("child did not inherit the dedicated Codex custody path: kind=%s harness=%s report_only=%v", child.Kind, child.Harness, child.ReportOnly)
	}
	if child.IssueTitle != "issue title" || child.IssueDescription != "issue body" {
		t.Fatal("child lost lead issue input")
	}
	retry, err := svc.SubmitPlanCrossCheck(env.ctx, f.wkr, leadID, 1, candidate)
	if err != nil || retry.ID != cc.ID || retry.CheckerRunID != cc.CheckerRunID {
		t.Fatalf("submit retry created another attempt: %v", err)
	}
	var children int
	if err := env.pool.QueryRow(env.ctx, `SELECT count(*) FROM runs WHERE target_run_id=$1 AND kind='cross_check'`, leadID).Scan(&children); err != nil || children != 1 {
		t.Fatalf("children=%d err=%v", children, err)
	}
	changed := candidate
	changed.PlanMd = "another plan"
	if _, err := svc.SubmitPlanCrossCheck(env.ctx, f.wkr, leadID, 1, changed); !errors.Is(err, ErrCrossCheckInterrupted) {
		t.Fatalf("changed candidate resubmitted: %v", err)
	}
	// Immutable same-generation retries precede current credential availability.
	env.exec(`UPDATE user_secrets SET disabled_at=now(),enablement_rev=enablement_rev+1 WHERE id=$1`, f.aliasID)
	if retry, err := svc.SubmitPlanCrossCheck(env.ctx, f.wkr, leadID, 1, candidate); err != nil || retry.ID != cc.ID {
		t.Fatalf("disabled credential prevented immutable retry: %v", err)
	}
	env.exec(`UPDATE user_secrets SET disabled_at=NULL,enablement_rev=enablement_rev+1 WHERE id=$1`, f.aliasID)
	env.exec(`UPDATE runs SET status='running',worker_id=$2,claim_generation=1 WHERE id=$1`, childID, f.workerID)
	env.exec(`UPDATE cross_checks SET created_at=now()-interval '120 seconds' WHERE id=$1`, cc.ID)
	findings := []byte(`{"summary":"ok","items":[]}`)
	if _, err := svc.DecidePlanCrossCheck(env.ctx, f.wkr, childID, 2, "approve", "approve", findings); !errors.Is(err, ErrCrossCheckRefused) {
		t.Fatalf("stale child verdict accepted: %v", err)
	}
	decided, err := svc.DecidePlanCrossCheck(env.ctx, f.wkr, childID, 1, "approve", "approve", findings)
	if err != nil || decided.Verdict != "approve" {
		t.Fatalf("valid verdict refused: %v", err)
	}
	lead := mustRun(t, env, leadID)
	if lead.BudgetPausedSeconds < 120 || lead.BudgetPausedSeconds > 122 {
		t.Fatalf("wait credited incorrectly: %d", lead.BudgetPausedSeconds)
	}
	if lead.LastSeq != 1 {
		t.Fatalf("server verdict message sequence=%d, want 1", lead.LastSeq)
	}
	var persistedVerdict string
	if err := env.pool.QueryRow(env.ctx, `SELECT payload->>'verdict' FROM run_messages WHERE run_id=$1 AND seq=1 AND kind='cross_check'`, leadID).Scan(&persistedVerdict); err != nil || persistedVerdict != "approve" {
		t.Fatalf("verdict message was not persisted: %v", err)
	}
	if _, err := svc.DecidePlanCrossCheck(env.ctx, f.wkr, childID, 1, "approve", "approve", findings); !errors.Is(err, ErrCrossCheckRefused) {
		t.Fatalf("duplicate verdict accepted: %v", err)
	}
	after := mustRun(t, env, leadID)
	if after.BudgetPausedSeconds != lead.BudgetPausedSeconds || after.LastSeq != lead.LastSeq {
		t.Fatal("duplicate verdict double-banked wait or duplicated message")
	}
	status, seq, err := svc.PlanCrossCheckStatus(env.ctx, f.wkr, leadID, 1, 1)
	if err != nil || status.Verdict != "approve" || seq != 1 {
		t.Fatalf("owner status incorrect: %s seq=%d err=%v", status.Verdict, seq, err)
	}
	if _, _, err := svc.PlanCrossCheckStatus(env.ctx, f.wkr, leadID, 2, 1); !errors.Is(err, ErrCrossCheckRefused) {
		t.Fatalf("stale owner status accepted: %v", err)
	}
	if retry, err := svc.SubmitPlanCrossCheck(env.ctx, f.wkr, leadID, 1, candidate); err != nil || retry.ID != cc.ID || retry.Verdict != "approve" {
		t.Fatalf("decided lost-ACK retry not reused: %v", err)
	}
}

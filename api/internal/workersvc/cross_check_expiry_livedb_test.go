package workersvc

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestPlanCrossCheckExpiryEventAndSequenceLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, workerID, repoID := env.seedCodexInfra(t)
	leadID := env.seedCodexRun(t, userID, workerID, repoID)
	env.exec(`UPDATE runs SET harness='claude',auto_approve=true,plan_cross_check_required=true,
		claim_generation=1,last_seq=7,budget_paused_seconds=5 WHERE id=$1`, leadID)
	env.exec(`INSERT INTO run_messages (run_id,seq,kind,payload,claim_generation)
		VALUES ($1,7,'status','{"text":"before timeout"}',1)`, leadID)
	childID := uuid.New()
	env.exec(`INSERT INTO runs (id,user_id,repo_id,worker_id,kind,target_run_id,harness,report_only,
		budget_wall_seconds,status,claim_generation,issue_title,issue_description)
		VALUES ($1,$2,$3,$4,'cross_check',$5,'codex',true,1800,'running',1,'check','check')`,
		childID, userID, repoID, workerID, leadID)
	env.exec(`INSERT INTO cross_checks (lead_run_id,stage,round,lead_claim_generation,plan_md,milestones,
		required_capabilities,required_tools,size_class,base_commit,planning_diff,candidate_digest,
		checker_run_id,checker_harness,created_at,deadline_at)
		VALUES ($1,'plan',1,1,'plan','[]','{}','{}','s',$2,'',$3,$4,'codex',
		now()-interval '120 seconds',now()-interval '1 second')`,
		leadID, strings.Repeat("a", 40), []byte("digest"), childID)
	var credit int
	if err := env.pool.QueryRow(env.ctx, `SELECT CEIL(EXTRACT(EPOCH FROM (deadline_at-created_at)))::int
		FROM cross_checks WHERE lead_run_id=$1`, leadID).Scan(&credit); err != nil {
		t.Fatal(err)
	}
	svc := New(env.q, env.box, testParams())
	svc.SetTxBeginner(env.pool)
	worker := store.Worker{ID: workerID, UserID: userID}
	cc, seq, err := svc.PlanCrossCheckStatus(env.ctx, worker, leadID, 1, 1)
	if err != nil || cc.Verdict != "failed" || cc.ReasonClass.String != "timed_out" || seq != 8 {
		t.Fatalf("expiry status: verdict=%s reason=%s seq=%d err=%v", cc.Verdict, cc.ReasonClass.String, seq, err)
	}
	var eventSeq int32
	var generation int64
	var verdict, reason, author string
	if err := env.pool.QueryRow(env.ctx, `SELECT seq,claim_generation,payload->>'verdict',
		payload->>'reason_class',payload->>'findings_author' FROM run_messages
		WHERE run_id=$1 AND kind='cross_check'`, leadID).Scan(&eventSeq, &generation, &verdict, &reason, &author); err != nil {
		t.Fatal(err)
	}
	if eventSeq != seq || generation != 1 || verdict != "failed" || reason != "timed_out" || author != "server" {
		t.Fatalf("persisted expiry event: seq=%d generation=%d verdict=%s reason=%s author=%s", eventSeq, generation, verdict, reason, author)
	}
	lead := mustRun(t, env, leadID)
	child := mustRun(t, env, childID)
	if lead.LastSeq != seq || int64(lead.BudgetPausedSeconds) != 5+int64(credit) ||
		child.Status != "cancelled" || !child.ClaimReleasedAt.Valid {
		t.Fatalf("expiry settlement: seq=%d budget=%d child=%s released=%v", lead.LastSeq, lead.BudgetPausedSeconds, child.Status, child.ClaimReleasedAt.Valid)
	}
	repeated, repeatedSeq, err := svc.PlanCrossCheckStatus(env.ctx, worker, leadID, 1, 1)
	if err != nil || repeated.ID != cc.ID || repeatedSeq != seq {
		t.Fatalf("repeat expiry status: seq=%d err=%v", repeatedSeq, err)
	}
	after := mustRun(t, env, leadID)
	var events int
	if err := env.pool.QueryRow(env.ctx, `SELECT count(*) FROM run_messages WHERE run_id=$1 AND kind='cross_check'`, leadID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 1 || after.LastSeq != lead.LastSeq || after.BudgetPausedSeconds != lead.BudgetPausedSeconds {
		t.Fatal("repeat expiry emitted another event or double-banked wait")
	}
}

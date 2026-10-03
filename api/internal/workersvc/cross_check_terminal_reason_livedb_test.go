package workersvc

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestPlanCrossCheckHumanRejectionClearsReasonLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, workerID, repoID := env.seedCodexInfra(t)
	leadID := env.seedCodexRun(t, userID, workerID, repoID)
	env.exec(`UPDATE runs SET status='awaiting_approval',harness='claude',plan_source='agent',
  auto_approve=false,plan_cross_check_required=true,plan_md='Human candidate',
  plan_cross_check_gate_reason='block',gate_revision=3 WHERE id=$1`, leadID)
	params := store.RejectRunServerSideParams{ID: leadID, UserID: uuid.New(),
		FailureReason:        pgtype.Text{String: "Human rejected", Valid: true},
		ExpectedGateRevision: pgtype.Int8{Int64: 3, Valid: true}}
	if rows, err := env.q.RejectRunServerSide(env.ctx, params); err != nil || rows != 0 {
		t.Fatalf("foreign rejection rows=%d err=%v", rows, err)
	}
	params.UserID = userID
	params.ExpectedGateRevision.Int64 = 2
	if rows, err := env.q.RejectRunServerSide(env.ctx, params); err != nil || rows != 0 {
		t.Fatalf("stale presentation rejection rows=%d err=%v", rows, err)
	}
	if run := mustRun(t, env, leadID); run.Status != "awaiting_approval" || run.PlanCrossCheckGateReason.String != "block" {
		t.Fatalf("refused rejection mutated gate: status=%s reason=%+v", run.Status, run.PlanCrossCheckGateReason)
	}
	params.ExpectedGateRevision.Int64 = 3
	if rows, err := env.q.RejectRunServerSide(env.ctx, params); err != nil || rows != 1 {
		t.Fatalf("current owner rejection rows=%d err=%v", rows, err)
	}
	if run := mustRun(t, env, leadID); run.Status != "failed" || run.PlanCrossCheckGateReason.Valid {
		t.Fatalf("terminal rejection retained gate: status=%s reason=%+v", run.Status, run.PlanCrossCheckGateReason)
	}
}

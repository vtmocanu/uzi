package workersvc

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestPolicyOwnerCompletionDecisionLiveDB(t *testing.T) {
	for _, decision := range []string{"continue", "partial", "accept"} {
		t.Run(decision, func(t *testing.T) {
			e := setupInterlockLiveDB(t)
			svc := e.permitService(t)
			wid := e.seedWorker(t, []string{"completion_interlock_v1"})
			id := e.seedFrozenRun(t, wid, []string{"m1", "m2"}, []string{"m1"}, false)
			e.parkCompletionBlocked(t, id)
			e.exec(t, "UPDATE runs SET policy_refusal_count=1,policy_last_outcome_id=$2,policy_fresh_claim_generation=1,session_id='keep-completion-session',completion_budget_exhausted_at=now() WHERE id=$1", id, uuid.New())
			e.seedPermit(t, id, wid, 1, "priorhead")
			before, err := svc.GetRun(e.ctx, e.userID, id)
			if err != nil {
				t.Fatal(err)
			}
			req := CompletionDecisionInput{Decision: decision, Guidance: "narrow guidance", Keep: []string{"m1"}, Criteria: []string{"m2.c1"}, Reason: "owner decision", ContractRevision: 1}
			for _, token := range []string{"", "\"stale\""} {
				req.ActionIfMatch = token
				if _, err := svc.DecideCompletion(e.ctx, e.userID, id, req); !errors.Is(err, ErrStaleClaim) {
					t.Fatalf("token=%q err=%v", token, err)
				}
				after, err := svc.GetRun(e.ctx, e.userID, id)
				if err != nil {
					t.Fatal(err)
				}
				if after.Status != "paused" || after.ContractRevision != before.ContractRevision || string(after.CompletionContract) != string(before.CompletionContract) || after.PolicyFreshClaimGeneration != before.PolicyFreshClaimGeneration || !e.exhaustedSet(t, id) || e.permitConsumed(t, id, 1, "priorhead") {
					t.Fatal("rejected decision changed state, contract, grant, steer or permit")
				}
				for _, kind := range []string{"follow_up", "completion_decision", "resume"} {
					if e.countInputs(t, id, kind) != 0 {
						t.Fatalf("rejected decision wrote %s", kind)
					}
				}
			}
			req.ActionIfMatch = PolicyHoldActionToken(before)
			got, err := svc.DecideCompletion(e.ctx, e.userID, id, req)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != "queued" || got.SessionID.String != "keep-completion-session" || got.PolicyFreshClaimGeneration.Valid || got.PolicyRefusalCount != 1 || got.HoldReason.String != "completion_blocked" || e.exhaustedSet(t, id) {
				t.Fatalf("preserving decision: %+v", got)
			}
			if e.countInputs(t, id, "follow_up") != 1 || e.countInputs(t, id, "completion_decision") != 1 || e.countInputs(t, id, "resume") != 1 {
				t.Fatal("missing or duplicate guidance/audit")
			}
		})
	}
}

func TestPolicyOwnerGenericResumeRefusedLiveDB(t *testing.T) {
	for _, reason := range []string{"budget_exhausted", "completion_blocked", ""} {
		t.Run(reason, func(t *testing.T) {
			e := setupPolicyHold(t)
			if _, _, err := e.svc.ReportPolicyHold(e.ctx, e.wkr, e.runID, policyRequest()); err != nil {
				t.Fatal(err)
			}
			if _, err := e.pool.Exec(e.ctx, "UPDATE runs SET hold_reason=NULLIF($2,''),budget_extension_seconds=10000 WHERE id=$1", e.runID, reason); err != nil {
				t.Fatal(err)
			}
			if _, err := e.q.ResumePausedRun(e.ctx, store.ResumePausedRunParams{ID: e.runID, UserID: e.userID, AllowCompletionBlockedHold: true, GlobalTimeoutSeconds: 3600}); err == nil {
				t.Fatal("generic resume bypassed policy consent")
			}
			if got := mustRun(t, e.codexTestEnv, e.runID); got.Status != "paused" {
				t.Fatal("generic resume changed state")
			}
		})
	}
}

package workersvc

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// One lifecycle per authority source keeps the matrix small: old completion,
// owner deferral of M2, and owner acceptance of M1.c1 all reuse the same IDs
// with different titles after a fresh-context replan.
func TestMilestoneReconciliationFreshCompletionLiveDB(t *testing.T) {
	for _, decision := range []string{"completed", "partial", "accept"} {
		t.Run(decision, func(t *testing.T) {
			f, id := seedMilestoneReconciliationFix(t)
			// Arrange a persisted predecessor with no resumable transcript. Resume
			// and Claim below are the real writers, rather than a fabricated payload.
			f.exec("UPDATE runs SET status='paused', session_id=NULL, plan_md=$2 WHERE id=$1", id, gatePlanA)
			revision := 1
			var decisionBody string
			if decision != "completed" {
				f.exec(`UPDATE runs SET milestones_completed='["M3","M4"]',
 hold_reason='completion_blocked', hold_captured_head=$2, completion_attempts=1 WHERE id=$1`, id, retentionTestTip)
				req := CompletionDecisionInput{
					Decision: decision, ContractRevision: 1, Reason: "predecessor delivery decision",
				}
				if decision == "partial" {
					req.Keep = []string{"M1", "M3", "M4"}
				} else {
					req.Criteria = []string{"M1.c1"}
				}
				run, err := f.svc.DecideCompletion(f.ctx, f.userID, id, req)
				if err != nil || run.Status != "queued" || run.ContractRevision.Int32 != 2 {
					t.Fatalf("real %s decision: status=%s revision=%v err=%v", decision, run.Status, run.ContractRevision, err)
				}
				var contract completionContract
				if err := json.Unmarshal(run.CompletionContract, &contract); err != nil {
					t.Fatal(err)
				}
				if decision == "partial" {
					if contract.Scope == nil || len(contract.Scope.Out) != 1 || contract.Scope.Out[0].MilestoneID != "M2" {
						t.Fatalf("partial did not defer M2: %s", run.CompletionContract)
					}
				} else if len(contract.Accepted) != 1 || contract.Accepted[0].ID != "M1.c1" || contract.Accepted[0].Text != "Old one" {
					t.Fatalf("accept did not retain original criterion: %s", run.CompletionContract)
				}
				if err := f.pool.QueryRow(f.ctx, "SELECT body FROM run_user_inputs WHERE run_id=$1 AND kind='completion_decision'", id).Scan(&decisionBody); err != nil {
					t.Fatal(err)
				}
				var audit struct {
					Decision string `json:"decision"`
					Reason   string `json:"reason"`
					Revision int    `json:"revision"`
				}
				if err := json.Unmarshal([]byte(decisionBody), &audit); err != nil {
					t.Fatal(err)
				}
				if audit.Decision != decision || audit.Reason != req.Reason || audit.Revision != 2 {
					t.Fatalf("decision audit=%s", decisionBody)
				}
				revision = 2
			} else if _, err := f.q.ResumePausedRun(f.ctx, store.ResumePausedRunParams{ID: id, UserID: f.userID}); err != nil {
				t.Fatalf("resume: %v", err)
			}

			claim, err := f.svc.Claim(f.ctx, f.wkr, nil)
			if err != nil || claim == nil {
				t.Fatalf("fresh resume: %+v %v", claim, err)
			}
			if claim.RunID != id.String() || claim.ClaimGeneration != 2 || claim.SessionID != nil || claim.PlanApproved || claim.ResumePhase != "" {
				t.Fatalf("resume must replan without predecessor context: %+v", claim)
			}
			oldGen := claim.ClaimGeneration
			session := "fresh-replan-transcript"
			if _, applied, err := f.svc.SetState(f.ctx, f.wkr, id, StateRequest{
				State: "running", ClaimGeneration: &oldGen, SessionID: &session,
			}); err != nil || !applied {
				t.Fatalf("fresh running: applied=%v err=%v", applied, err)
			}
			req := gateReq(&oldGen, uid())
			req.SessionID = &session
			plan := "Replacement plan: deliver Replacement one, then Replacement two."
			req.PlanMd = &plan
			req.Milestones = &[]Milestone{{ID: "M1", Title: "Replacement one"}, {ID: "M2", Title: "Replacement two"}}
			req.GateCheckpointWitness = &apitypes.GateCheckpointWitness{
				SourceHead: "1111111111111111111111111111111111111111", PublishedCheckpoint: retentionTestTip,
			}
			f.mustPublish(id, req, 1)
			before := reconciliationRow(t, f, id)
			if !jsonEqual(t, before.MilestonesFrozen, reconciliationOldList) || !jsonEqual(t, before.MilestonesCandidate, reconciliationNewList) {
				t.Fatalf("replan replaced frozen authority before consent: frozen=%s candidate=%s", before.MilestonesFrozen, before.MilestonesCandidate)
			}
			digest, err := ResetStateDigest(before)
			if err != nil {
				t.Fatal(err)
			}
			opts := SubmitInputOptions{
				ExpectedGateRevision: i64(1), ExpectedResetStateDigest: digest,
				Reconciliation: &apitypes.MilestoneReconciliation{
					Mode: "reset", Reason: "require fresh replacement delivery", ExpectedCompletionRevision: &revision,
				},
			}
			if _, err := f.svc.SubmitInputWithOptions(f.ctx, f.userID, id, "approve_plan", "", nil, opts); err != nil {
				t.Fatal(err)
			}
			revision++
			assertReconciliationProgressEmpty(t, reconciliationRow(t, f, id))
			claim, err = f.svc.Claim(f.ctx, f.wkr, nil)
			if err != nil || claim == nil {
				t.Fatalf("replacement reclaim: %+v %v", claim, err)
			}
			if claim.ClaimGeneration != 3 || claim.SessionID != nil || !claim.PlanApproved || claim.ResumePhase != "implementing" ||
				claim.Config.ContractRevision == nil || *claim.Config.ContractRevision != revision ||
				!reflect.DeepEqual(claim.Milestones, *req.Milestones) {
				t.Fatalf("replacement claim lost fresh context or titles: %+v", claim)
			}
			gen := claim.ClaimGeneration
			current := reconciliationRow(t, f, id)
			assertReconciliationProgressEmpty(t, current)
			if !jsonEqual(t, current.MilestonesFrozen, reconciliationNewList) {
				t.Fatalf("replacement titles: %s", current.MilestonesFrozen)
			}
			var contract completionContract
			if err := json.Unmarshal(current.CompletionContract, &contract); err != nil {
				t.Fatal(err)
			}
			if contract.Revision != revision || contract.Scope != nil || len(contract.Accepted) != 0 ||
				len(contract.Criteria) != 2 ||
				contract.Criteria[0].ID != "M1.c1" || contract.Criteria[0].MilestoneID != "M1" || contract.Criteria[0].Text != "Replacement one" ||
				contract.Criteria[1].ID != "M2.c1" || contract.Criteria[1].MilestoneID != "M2" || contract.Criteria[1].Text != "Replacement two" {
				t.Fatalf("replacement contract retained old authority/text: %s", current.CompletionContract)
			}
			// A report from the actual pre-reset generation carries both old
			// contract titles and old completed IDs. It must change neither.
			stale := gateReq(&oldGen, uid())
			stale.Milestones = &[]Milestone{{ID: "M1", Title: "Old one"}, {ID: "M2", Title: "Old two"}, {ID: "M3", Title: "Old three"}, {ID: "M4", Title: "Old four"}}
			stale.MilestonesCompleted = &[]string{"M1", "M2", "M3", "M4"}
			if _, applied, _, err := f.report(id, stale); applied || !errors.Is(err, ErrStaleClaim) {
				t.Fatalf("pre-reset report: applied=%v err=%v", applied, err)
			}
			if !reflect.DeepEqual(current, reconciliationRow(t, f, id)) {
				t.Fatal("pre-reset report overwrote replacement contract/progress")
			}

			permitReq := CompletionPermitRequest{
				ContractRevision: revision, Branch: "agent/replacement", Head: retentionTestTip, ClaimGeneration: &gen,
			}
			requireDenied := func(want []string) {
				t.Helper()
				result, err := f.svc.RequestCompletionPermit(f.ctx, f.wkr, id, permitReq)
				if err != nil || result.Granted || result.Permit != nil || result.DenyReason != CompletionDenyMissingMilestones || !reflect.DeepEqual(result.Unmet, want) {
					t.Fatalf("permit must require fresh %v: %+v err=%v", want, result, err)
				}
			}
			if _, applied, err := f.svc.SetState(f.ctx, f.wkr, id, StateRequest{State: "running", ClaimGeneration: &gen}); err != nil || !applied {
				t.Fatalf("replacement running: applied=%v err=%v", applied, err)
			}
			requireDenied([]string{"M1", "M2"})
			for _, milestone := range []string{"M1", "M2"} {
				if _, applied, err := f.svc.SetState(f.ctx, f.wkr, id, StateRequest{
					State: "running", ClaimGeneration: &gen, MilestonesCompleted: &[]string{milestone},
				}); err != nil || !applied {
					t.Fatalf("fresh %s report: applied=%v err=%v", milestone, applied, err)
				}
				if milestone == "M1" {
					requireDenied([]string{"M2"})
				}
			}
			permit, err := f.svc.RequestCompletionPermit(f.ctx, f.wkr, id, permitReq)
			if err != nil || !permit.Granted || permit.Permit == nil || permit.Permit.ContractRevision != revision {
				t.Fatalf("fresh replacement permit: %+v err=%v", permit, err)
			}
			run, applied, err := f.svc.SetState(f.ctx, f.wkr, id, StateRequest{
				State: "completed", ClaimGeneration: &gen, Branch: &permitReq.Branch, Head: &permitReq.Head,
			})
			if err != nil || !applied || run.Status != "completed" || !jsonEqual(t, run.MilestonesCompleted, `["M1","M2"]`) {
				t.Fatalf("permit-backed completion: status=%s completed=%s applied=%v err=%v", run.Status, run.MilestonesCompleted, applied, err)
			}
			var consumed bool
			if err := f.pool.QueryRow(f.ctx, "SELECT consumed_at IS NOT NULL FROM run_completion_permits WHERE id=$1", permit.Permit.ID).Scan(&consumed); err != nil || !consumed {
				t.Fatalf("completion did not consume permit: consumed=%v err=%v", consumed, err)
			}

			audit, err := f.svc.MilestoneReconciliationAudit(f.ctx, f.userID, id)
			if err != nil || len(audit) != 1 {
				t.Fatalf("reconciliation audit: %s err=%v", audit, err)
			}
			var record struct {
				Before struct {
					Contract  json.RawMessage `json:"completion_contract"`
					Completed json.RawMessage `json:"milestones_completed"`
				} `json:"before_state"`
			}
			if err := json.Unmarshal(audit[0], &record); err != nil {
				t.Fatal(err)
			}
			if !jsonEqual(t, record.Before.Contract, string(before.CompletionContract)) ||
				!jsonEqual(t, record.Before.Completed, string(before.MilestonesCompleted)) {
				t.Fatalf("predecessor contract/progress no longer auditable: %s", audit[0])
			}
			if decision != "completed" {
				var retained string
				if err := f.pool.QueryRow(f.ctx, "SELECT body FROM run_user_inputs WHERE run_id=$1 AND kind='completion_decision'", id).Scan(&retained); err != nil {
					t.Fatal(err)
				}
				if !jsonEqual(t, []byte(retained), decisionBody) {
					t.Fatalf("prior decision audit changed: %s -> %s", decisionBody, retained)
				}
			}
		})
	}
}

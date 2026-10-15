package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pushbroker"
	"github.com/vtmocanu/uzi/api/internal/store"
)

const reconciliationOldList = `[{"id":"M1","title":"Old one"},{"id":"M2","title":"Old two"},{"id":"M3","title":"Old three"},{"id":"M4","title":"Old four"}]`
const reconciliationNewList = `[{"id":"M1","title":"Replacement one"},{"id":"M2","title":"Replacement two"}]`

// Seed predecessor authority; only the forge ref read is fake.
func seedMilestoneReconciliationFix(t *testing.T, historicalStop ...string) (*gateFix, uuid.UUID) {
	t.Helper()
	f := newGateFix(t, 8)
	f.wkr.ProtocolCapabilities = append(f.wkr.ProtocolCapabilities, capability.CompletionInterlockV1, capability.CredentialSwitchV1, capability.MilestoneReconciliationClaimV1)
	f.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", f.wkr.ID, f.wkr.ProtocolCapabilities)
	f.svc.SetBackground(func(func()) {})
	f.svc.SetRetentionLockPool(f.pool)
	f.svc.SetForgeBaseURLAllowed(func(u string) bool { return u == "https://forge.e2e" })
	f.svc.SetListRefTipsFn(func(_ context.Context, _ pushbroker.ListRefsOptions, refs ...string) (map[string]string, error) {
		tips := map[string]string{}
		for _, ref := range refs {
			tips[ref] = retentionTestTip
		}
		return tips, nil
	})
	id := f.newRun("issue")
	contract, err := buildCompletionContract([]byte(reconciliationOldList))
	if err != nil {
		t.Fatal(err)
	}
	f.exec(`UPDATE runs SET milestones_frozen=$2, milestones_completed='["M1","M2","M3","M4"]',
 milestones_in_progress='["M4"]', milestones_agents='[{"milestone_id":"M1","agent":"old-agent"}]',
 completion_contract_version=1, completion_contract=$3, contract_revision=1, scope_ceiling=4,
 stop_kind='scope_capped', stop_reason='old scope', checkpoint_tip=$4, checkpoint_tip_at=now(),
 budget_max_iterations=17, budget_wall_seconds=123456 WHERE id=$1`, id, []byte(reconciliationOldList), contract, retentionTestTip)
	if len(historicalStop) > 0 {
		f.exec("UPDATE runs SET stop_kind=$2, stop_reason='historical decision' WHERE id=$1", id, historicalStop[0])
	}
	return f, id
}

// Publish the replacement through SetStateReport, including the canonical H/P witness.
func newMilestoneReconciliationFix(t *testing.T, historicalStop ...string) (*gateFix, uuid.UUID, SubmitInputOptions) {
	t.Helper()
	f, id := seedMilestoneReconciliationFix(t, historicalStop...)
	req := gateReq(i64(1), uid())
	req.Milestones = &[]Milestone{{ID: "M1", Title: "Replacement one"}, {ID: "M2", Title: "Replacement two"}}
	req.GateCheckpointWitness = &apitypes.GateCheckpointWitness{SourceHead: "1111111111111111111111111111111111111111", PublishedCheckpoint: retentionTestTip}
	f.mustPublish(id, req, 1)
	r, err := f.q.GetRunByID(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := ResetStateDigest(r)
	if err != nil {
		t.Fatal(err)
	}
	revision := 1
	return f, id, SubmitInputOptions{ExpectedGateRevision: i64(1), ExpectedResetStateDigest: digest,
		Reconciliation: &apitypes.MilestoneReconciliation{Mode: "reset", Reason: "replace completed contract", ExpectedCompletionRevision: &revision}}
}

func reconciliationRow(t *testing.T, f *gateFix, id uuid.UUID) store.Run {
	t.Helper()
	r, err := f.q.GetRunByID(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func assertReconciliationProgressEmpty(t *testing.T, r store.Run) {
	t.Helper()
	if !jsonEqual(t, r.MilestonesCompleted, "[]") || !jsonEqual(t, r.MilestonesInProgress, "[]") ||
		!jsonEqual(t, r.MilestonesAgents, "[]") || r.ScopeCeiling.Valid || r.StopKind.Valid || r.StopReason.Valid {
		t.Fatalf("old progress or scope survived: completed=%s in_progress=%s agents=%s scope=%v stop=%v/%v",
			r.MilestonesCompleted, r.MilestonesInProgress, r.MilestonesAgents, r.ScopeCeiling, r.StopKind, r.StopReason)
	}
}

func TestMilestoneReconciliationAtomicReclaimLiveDB(t *testing.T) {
	for _, different := range []bool{false, true} {
		name := "same worker"
		if different {
			name = "different worker"
		}
		t.Run(name, func(t *testing.T) {
			f, id, opts := newMilestoneReconciliationFix(t)
			f.exec("UPDATE runs SET session_id='predecessor-transcript' WHERE id=$1", id)
			runWithSession := reconciliationRow(t, f, id)
			opts.ExpectedResetStateDigest, _ = ResetStateDigest(runWithSession)
			before := runWithSession
			f.exec(`INSERT INTO run_completion_permits(run_id,contract_revision,branch,head,issued_by_worker_id)
    VALUES($1,1,'agent/old',$2,$3)`, id, retentionTestTip, f.wkr.ID)
			f.exec(`INSERT INTO run_user_inputs(run_id,kind,body) VALUES($1,'stop','old stop'),($1,'cancel','old cancel')`, id)
			result, err := f.svc.SubmitInputWithOptions(f.ctx, f.userID, id, "approve_plan", "", nil, opts)
			if err != nil {
				t.Fatal(err)
			}
			after := reconciliationRow(t, f, id)
			assertReconciliationProgressEmpty(t, after)
			if after.Status != "queued" || !after.ClaimReleasedAt.Valid || after.ClaimGeneration != 1 ||
				!jsonEqual(t, after.MilestonesFrozen, reconciliationNewList) || after.ContractRevision.Int32 != 2 {
				t.Fatalf("replacement/release not atomic: status=%s released=%v gen=%d frozen=%s revision=%v",
					after.Status, after.ClaimReleasedAt, after.ClaimGeneration, after.MilestonesFrozen, after.ContractRevision)
			}
			var contract struct {
				Revision int `json:"revision"`
				Criteria []struct {
					MilestoneID string `json:"milestone_id"`
				} `json:"criteria"`
			}
			if err := json.Unmarshal(after.CompletionContract, &contract); err != nil {
				t.Fatal(err)
			}
			if contract.Revision != 2 || len(contract.Criteria) != 2 || contract.Criteria[0].MilestoneID != "M1" || contract.Criteria[1].MilestoneID != "M2" {
				t.Fatalf("new contract: %s", after.CompletionContract)
			}
			if after.CheckpointTip != before.CheckpointTip || after.CheckpointTipAt != before.CheckpointTipAt ||
				after.BudgetMaxIterations != before.BudgetMaxIterations || after.BudgetWallSeconds != before.BudgetWallSeconds ||
				after.HoldReason != before.HoldReason || after.HoldCapturedHead != before.HoldCapturedHead {
				t.Fatal("checkpoint, budget or hold changed")
			}
			var applied, bound, invalidated bool
			if err := f.pool.QueryRow(f.ctx, `SELECT applied_at IS NOT NULL AND consumed_at IS NOT NULL AND disposition='applied',
    gate_binding='bound' AND gate_revision=1,
    (SELECT bool_and(consumed_at IS NOT NULL) FROM run_completion_permits WHERE run_id=$2)
    FROM run_user_inputs WHERE id=$1`, result.ID, id).Scan(&applied, &bound, &invalidated); err != nil {
				t.Fatal(err)
			}
			if !applied || !bound || !invalidated {
				t.Fatalf("approval/permit: %v %v %v", applied, bound, invalidated)
			}
			assertReplay := func() {
				t.Helper()
				retry, err := f.svc.SubmitInputWithOptions(f.ctx, f.userID, id, "approve_plan", "", nil, opts)
				if err != nil || retry.ID != result.ID {
					t.Fatalf("retry=%+v err=%v", retry, err)
				}
				audit, err := f.svc.MilestoneReconciliationAudit(f.ctx, f.userID, id)
				if err != nil || len(audit) != 1 {
					t.Fatalf("audit count=%d err=%v", len(audit), err)
				}
				var record struct {
					BeforeState    json.RawMessage `json:"before_state"`
					AfterState     json.RawMessage `json:"after_state"`
					AppliedInputID int64           `json:"applied_input_id"`
				}
				if err := json.Unmarshal(audit[0], &record); err != nil {
					t.Fatal(err)
				}
				var old, new struct {
					SessionID           *string         `json:"session_id"`
					MilestonesCompleted json.RawMessage `json:"milestones_completed"`
					MilestonesFrozen    json.RawMessage `json:"milestones_frozen"`
				}
				if err := json.Unmarshal(record.BeforeState, &old); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(record.AfterState, &new); err != nil {
					t.Fatal(err)
				}
				if old.SessionID == nil || *old.SessionID != "predecessor-transcript" || new.SessionID == nil || *new.SessionID != *old.SessionID {
					t.Fatal("audit session_id must be JSON string and retain predecessor custody")
				}
				if !jsonEqual(t, old.MilestonesCompleted, `["M1","M2","M3","M4"]`) || !jsonEqual(t, new.MilestonesFrozen, reconciliationNewList) || record.AppliedInputID != result.ID {
					t.Fatal("incorrect audit snapshots")
				}
				foreign, err := f.svc.MilestoneReconciliationAudit(f.ctx, uuid.New(), id)
				if err != nil || len(foreign) != 0 {
					t.Fatal("audit exposed to foreign owner")
				}
				rows, err := f.q.ListReplayRunInputs(f.ctx, id)
				if err != nil || len(rows) != 0 {
					t.Fatalf("retired input replay: %+v %v", rows, err)
				}
				var approvals int
				if err := f.pool.QueryRow(f.ctx, "SELECT count(*) FROM run_user_inputs WHERE run_id=$1 AND kind='approve_plan'", id).Scan(&approvals); err != nil {
					t.Fatal(err)
				}
				if approvals != 1 {
					t.Fatalf("duplicate approval banking: %d", approvals)
				}
			}
			assertReplay()
			claimant := f.wkr
			if different {
				claimant.ID = uuid.New()
				f.exec(`INSERT INTO workers(id,user_id,name,token_hash,status,protocol_capabilities,capabilities)
     VALUES($1,$2,'replacement',$3,'online',$4,$5)`, claimant.ID, f.userID, claimant.ID[:], claimant.ProtocolCapabilities, claimant.Capabilities)
			}
			// Disable affinity to exercise an immediate transfer to the different worker.
			f.svc.p.WorkerAffinityCeiling = 0
			claim, err := f.svc.Claim(f.ctx, claimant, nil)
			if err != nil || claim == nil {
				t.Fatalf("reclaim=%+v err=%v", claim, err)
			}
			if claim.SessionID != nil || claim.CheckpointTip == nil || *claim.CheckpointTip != before.CheckpointTip.String {
				t.Fatalf("replacement reused predecessor transcript or lost checkpoint: %+v", claim)
			}
			if claim.RunID != id.String() || claim.ClaimGeneration != 2 || !claim.PlanApproved || claim.ResumePhase != "implementing" {
				t.Fatalf("reclaim regated or wrong generation: %+v", claim)
			}
			r := reconciliationRow(t, f, id)
			if r.ClaimReleasedAt.Valid || r.WorkerID.Bytes != claimant.ID || !jsonEqual(t, r.MilestonesFrozen, reconciliationNewList) {
				t.Fatal("claim did not acquire replacement")
			}
			assertReconciliationProgressEmpty(t, r)
			if r.SessionID != before.SessionID {
				t.Fatal("persisted predecessor session cleared")
			}
			// Reassembly after recovery before the first new session stays fresh.
			fresh, err := f.svc.assembleClaim(f.ctx, claimant, r)
			if err != nil || fresh.SessionID != nil {
				t.Fatalf("recovery transcript: %+v %v", fresh, err)
			}
			f.exec("UPDATE runs SET session_id='replacement-transcript' WHERE id=$1", id)
			fresh, err = f.svc.assembleClaim(f.ctx, claimant, reconciliationRow(t, f, id))
			if err != nil || fresh.SessionID == nil || *fresh.SessionID != "replacement-transcript" {
				t.Fatalf("new transcript: %+v %v", fresh, err)
			}
			assertReplay()
		})
	}
}

func TestMilestoneReconciliationOrdinaryApprovalLiveDB(t *testing.T) {
	for _, selected := range []bool{false, true} {
		for _, changed := range []bool{false, true} {
			t.Run(map[bool]string{false: "plain", true: "selected"}[selected]+"/"+map[bool]string{false: "unchanged", true: "changed"}[changed], func(t *testing.T) {
				f, id, _ := newMilestoneReconciliationFix(t)
				if !changed {
					f.exec("UPDATE runs SET milestones_candidate=milestones_frozen WHERE id=$1", id)
				}
				before := reconciliationRow(t, f, id)
				var sel *AgentSelection
				if selected {
					sel = &AgentSelection{Source: AgentSourceOwn}
				}
				_, err := f.svc.SubmitInput(f.ctx, f.userID, id, "approve_plan", "", sel)
				if changed && !errors.Is(err, ErrReconciliationRequired) {
					t.Fatalf("changed approval: %v", err)
				}
				if !changed && err != nil {
					t.Fatalf("unchanged approval: %v", err)
				}
				after := reconciliationRow(t, f, id)
				if !jsonEqual(t, after.MilestonesCompleted, string(before.MilestonesCompleted)) || after.ScopeCeiling != before.ScopeCeiling ||
					after.ContractRevision != before.ContractRevision || !jsonEqual(t, after.CompletionContract, string(before.CompletionContract)) {
					t.Fatal("ordinary approval reset progress/contract")
				}
			})
		}
	}
}

func TestMilestoneReconciliationStaleProgressAndAttemptUnionLiveDB(t *testing.T) {
	f, id, opts := newMilestoneReconciliationFix(t)
	if _, err := f.svc.SubmitInputWithOptions(f.ctx, f.userID, id, "approve_plan", "", nil, opts); err != nil {
		t.Fatal(err)
	}
	assertStale := func() {
		t.Helper()
		for _, gen := range []*int64{nil, i64(1)} {
			_, applied, err := f.svc.SetState(f.ctx, f.wkr, id, StateRequest{State: "running", ClaimGeneration: gen, MilestonesCompleted: &[]string{"M1", "M2"}})
			if applied || (!errors.Is(err, ErrMissingClaimGeneration) && !errors.Is(err, ErrStaleClaim)) {
				t.Fatalf("stale report applied=%v err=%v", applied, err)
			}
			_, err = f.svc.RecordCompletionAttempt(f.ctx, f.wkr, id, CompletionAttemptRequest{Head: retentionTestTip, ClaimGeneration: gen, MilestonesCompleted: []string{"M1", "M2"}})
			if !errors.Is(err, ErrMissingClaimGeneration) && !errors.Is(err, ErrStaleClaim) {
				t.Fatalf("stale attempt: %v", err)
			}
		}
		assertReconciliationProgressEmpty(t, reconciliationRow(t, f, id))
	}
	assertStale()
	claim, err := f.svc.Claim(f.ctx, f.wkr, nil)
	if err != nil || claim == nil {
		t.Fatalf("reclaim: %+v %v", claim, err)
	}
	assertStale()
	gen := claim.ClaimGeneration
	if _, applied, err := f.svc.SetState(f.ctx, f.wkr, id, StateRequest{State: "running", ClaimGeneration: &gen, MilestonesCompleted: &[]string{"M1"}}); err != nil || !applied {
		t.Fatalf("current report applied=%v err=%v", applied, err)
	}
	result, err := f.svc.RecordCompletionAttempt(f.ctx, f.wkr, id, CompletionAttemptRequest{Head: retentionTestTip, ClaimGeneration: &gen, MilestonesCompleted: []string{"M2"}})
	if err != nil || result.AttemptCount != 1 || len(result.Unmet) != 0 {
		t.Fatalf("current union: %+v %v", result, err)
	}
	r := reconciliationRow(t, f, id)
	if !jsonEqual(t, r.MilestonesCompleted, `["M1","M2"]`) {
		t.Fatalf("union=%s", r.MilestonesCompleted)
	}
}

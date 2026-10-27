package workersvc

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pushbroker"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func taskWritableLifecycleWorker(t *testing.T, x *taskLifecycleFixture) {
	t.Helper()
	f := x.f
	f.wkr.ProtocolCapabilities = []string{
		capability.TaskCheckpointCoordinatesV1, capability.RecoveryArchiveV1, capability.RecoveryInventoryV1,
		capability.RecoveryHeldPublicationV1, capability.InputReceiptsV1,
		capability.GateRevisionV1, capability.CompletionInterlockV1,
		capability.CredentialSwitchV1, capability.MilestoneReconciliationClaimV1,
	}
	f.env.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", f.workerID, f.wkr.ProtocolCapabilities)
	var err error
	f.wkr, err = f.env.q.GetWorkerByID(f.env.ctx, f.workerID)
	if err != nil {
		t.Fatal(err)
	}
	if x.child.ReviewTargetRunID.Valid || !x.child.ThenFixOfRunID.Valid ||
		uuid.UUID(x.child.ThenFixOfRunID.Bytes) != x.root.ID {
		t.Fatal("proof requires a writable THEN-FIX child from a fresh enrolled claim")
	}
	f.svc.SetBackground(func(func()) {})
}

// Read the actual claim-created, guarded hold; no seed hold or trigger bypass.
func taskLifecycleHold(t *testing.T, x *taskLifecycleFixture, generation int64) (uuid.UUID, string) {
	t.Helper()
	f := x.f
	var id uuid.UUID
	var snapshot string
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT id,to_jsonb(h)::text
 FROM recovery_custody_holds h WHERE run_id=$1 AND generation=$2
 AND state='open' AND inventory_guarded AND live_run_id=$1
 AND live_worker_id=$3 AND original_worker_id=$3 AND user_id=$4 AND repo_id=$5`,
		x.child.ID, generation, f.workerID, f.userID, f.repoID).Scan(&id, &snapshot); err != nil {
		t.Fatalf("claim-created guarded hold: %v", err)
	}
	return id, snapshot
}

func TestTaskCheckpointWritableDescendantHeldPublicationLiveDB(t *testing.T) {
	x := newTaskLifecycleFixtureWithChild(t, true)
	f := x.f
	taskWritableLifecycleWorker(t, x)
	f.svc.p.HeldPublication = true
	g := newTaskHeldGitFixture(t, x)
	// Dispose only this synthetic remote after the assertions. The DB protects
	// live receipts from DELETE, so CAS-delete our ref and record that outcome
	// through the existing guarded query before the fixture removes its holds.
	t.Cleanup(func() {
		row, err := f.env.q.GetHeldPublicationByRunGeneration(f.env.ctx, store.GetHeldPublicationByRunGenerationParams{
			RunID: x.child.ID, Generation: x.child.ClaimGeneration,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return
		}
		if err != nil {
			t.Errorf("cleanup held lookup: %v", err)
			return
		}
		if row.State == "created" {
			taskHeldGit(t, g.origin, "update-ref", "-d", row.Ref, row.Tip)
			n, err := f.env.q.RecordHeldDeleteOutcome(f.env.ctx, store.RecordHeldDeleteOutcomeParams{
				ID: row.ID, State: "deleted", FromStates: []string{"created"},
			})
			if err != nil || n != 1 {
				t.Errorf("cleanup held deletion receipt: rows=%d err=%v", n, err)
				return
			}
		}
		if _, err := f.env.pool.Exec(f.env.ctx, "DELETE FROM run_held_publications WHERE run_id=$1", x.child.ID); err != nil {
			t.Errorf("cleanup child held receipt: %v", err)
		}
	})
	// Establish independent SQL checkpoint evidence matching the native origin.
	// Only this setup uses the existing fake; held prepare/send below are real.
	x.publish(t, x.root, g.decoy)
	x.publish(t, x.child, g.checkpoint)
	f.env.exec("UPDATE runs SET status='failed',fail_origin='agent_failure',finished_at=now() WHERE id=$1", x.child.ID)
	before := mustRun(t, f.env, x.child.ID)
	holdID, holdBefore := taskLifecycleHold(t, x, before.ClaimGeneration)
	rootBefore := x.rootEvidence(t)
	retainedBefore := x.retention(t, x.child.ID, x.childKey, x.childRef, g.checkpoint, "retained")
	prepares, sends := 0, 0
	f.svc.heldPrepareFn = func(ctx context.Context, o pushbroker.HeldPackOptions) (*pushbroker.PreparedHeldPack, error) {
		prepares++
		if o.WorkBranch != "uzi/task/"+x.root.ID.String() ||
			o.CheckpointKey != "uzi/task/"+x.child.ID.String() ||
			o.RunID != x.child.ID || o.Generation != before.ClaimGeneration ||
			o.Tip != g.tip || o.DefaultBranch != "main" || len(o.BaseRefs) != 0 {
			t.Fatal("held server options lost root branch / current-child key / generation")
		}
		o.CloneURL = g.url // Only transport destination is substituted.
		wrong := o
		wrong.CheckpointKey = x.rootKey
		if _, err := pushbroker.PrepareHeldPack(ctx, wrong); err == nil {
			t.Fatal("thin-pack control: root decoy unexpectedly supplied the child-only base")
		} else {
			if !errors.Is(err, plumbing.ErrObjectNotFound) || !strings.HasPrefix(err.Error(), "pushbroker: apply pack:") {
				t.Fatalf("thin-pack control failed before the missing child base was checked: %v", err)
			}
			t.Logf("PrepareHeldPack thin-pack control key=%s (root decoy, child-only base excluded): %v", wrong.CheckpointKey, err)
		}
		prepared, err := pushbroker.PrepareHeldPack(ctx, o)
		if err == nil && (prepared.Ref() != pushbroker.HeldRef(x.child.ID, before.ClaimGeneration) || prepared.Tip() != g.tip) {
			t.Fatal("checkpoint key changed held destination or declared tip")
		}
		return prepared, err
	}
	f.svc.heldSendFn = func(ctx context.Context, p *pushbroker.PreparedHeldPack) (pushbroker.HeldCreateOutcome, error) {
		sends++
		return pushbroker.SendHeldCreate(ctx, p)
	}
	res, err := f.svc.PublishHeld(f.env.ctx, f.wkr, x.child.ID, HeldPublishRequest{
		Tip: g.tip, Generation: before.ClaimGeneration, Coverage: heldTestCov,
	}, g.pack)
	wantRef := pushbroker.HeldRef(x.child.ID, before.ClaimGeneration)
	if err != nil || res.State != "created" || res.Ref != wantRef || res.Tip != g.tip || res.Reason != "" || prepares != 1 || sends != 1 {
		t.Fatalf("real held create=%+v err=%v prepares=%d sends=%d", res, err, prepares, sends)
	}
	row, err := f.env.q.GetHeldPublicationByRunGeneration(f.env.ctx, store.GetHeldPublicationByRunGenerationParams{
		RunID: x.child.ID, Generation: before.ClaimGeneration,
	})
	if err != nil || row.ID != res.PublicationID || row.RunID != x.child.ID || row.Generation != before.ClaimGeneration ||
		row.State != "created" || row.Ref != wantRef || row.Tip != g.tip || row.CoverageDigest != heldTestCov ||
		row.HoldID != holdID || row.WorkerID != f.workerID || row.UserID != f.userID || row.RepoID != f.repoID ||
		!row.LiveRunID.Valid || uuid.UUID(row.LiveRunID.Bytes) != x.child.ID ||
		!row.CreateInvokedAt.Valid || !row.RefCreatedAt.Valid {
		t.Fatalf("held publication SQL identity=%+v err=%v", row, err)
	}
	g.ref(t, wantRef, g.tip)
	g.ref(t, x.childRef, g.checkpoint)
	g.ref(t, x.rootRef, g.decoy)
	g.ref(t, "refs/heads/"+x.rootKey, g.base)
	_, holdAfter := taskLifecycleHold(t, x, before.ClaimGeneration)
	retainedAfter := x.retention(t, x.child.ID, x.childKey, x.childRef, g.checkpoint, "retained")
	if holdAfter != holdBefore || !reflect.DeepEqual(before, mustRun(t, f.env, x.child.ID)) ||
		!reflect.DeepEqual(retainedBefore, retainedAfter) || x.rootEvidence(t) != rootBefore {
		t.Fatal("held creation changed custody, checkpoint evidence, frozen coordinates or root")
	}
}

// Milestone reports remain issue-only. Refusing a task reset must preserve
// its checkpoint coordinates and custody; this does not prove a positive reset.
func TestTaskCheckpointWritableDescendantMilestoneGatePolicyLiveDB(t *testing.T) {
	x := newTaskLifecycleFixtureWithChild(t, true)
	f := x.f
	taskWritableLifecycleWorker(t, x)
	x.publish(t, x.root, taskLifecycleAdvancedTip)
	x.publish(t, x.child, taskLifecycleTip)
	rootBefore := x.rootEvidence(t)
	oldGeneration := x.child.ClaimGeneration
	_, holdBefore := taskLifecycleHold(t, x, oldGeneration)
	contract, err := buildCompletionContract([]byte(reconciliationOldList))
	if err != nil {
		t.Fatal(err)
	}
	// Even prior structural metadata cannot make a task report acquire the
	// issue-only milestone ingestion policy. Coordinates and holds are real.
	f.env.exec(`UPDATE runs SET status='running',auto_approve=false,
 milestones_frozen=$2,milestones_completed='["M1","M2","M3","M4"]',
 completion_contract_version=1,completion_contract=$3,contract_revision=1
 WHERE id=$1`, x.child.ID, []byte(reconciliationOldList), contract)
	req := gateReq(&oldGeneration, uid())
	req.RequiredCapabilities = &[]string{}
	req.Milestones = &[]Milestone{{ID: "M1", Title: "Replacement one"}, {ID: "M2", Title: "Replacement two"}}
	req.GateCheckpointWitness = &apitypes.GateCheckpointWitness{
		SourceHead: taskLifecycleTip, PublishedCheckpoint: taskLifecycleTip,
	}
	gated, applied, revision, err := f.svc.SetStateReport(f.env.ctx, f.wkr, x.child.ID, req)
	if err != nil || !applied || revision != 1 || gated.Status != "awaiting_approval" {
		t.Fatalf("real child gate status=%s applied=%v revision=%d err=%v", gated.Status, applied, revision, err)
	}
	before := mustRun(t, f.env, x.child.ID)
	if len(before.MilestonesCandidate) != 0 && string(before.MilestonesCandidate) != "null" {
		t.Fatalf("task unexpectedly accepted issue-only milestone candidate: %s", before.MilestonesCandidate)
	}
	payload, ok, err := decodeGatePresentedPayload(before.GatePresentedPayload)
	if err != nil || !ok || len(payload.Milestones) != 0 && string(payload.Milestones) != "null" ||
		payload.CheckpointWitness == nil || payload.CheckpointWitness.PublishedCheckpoint != taskLifecycleTip {
		t.Fatalf("task gate payload=%s err=%v", before.GatePresentedPayload, err)
	}
	digest, err := ResetStateDigest(before)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	f.svc.SetListRefTipsFn(func(ctx context.Context, o pushbroker.ListRefsOptions, refs ...string) (map[string]string, error) {
		calls++
		return x.forge.listRefTips(ctx, o, refs...)
	})
	authority := coordinateAuthoritySnapshot(t, f.env, x.child.ID)
	priorRevision := 1
	approval, err := f.svc.SubmitInputWithOptions(f.env.ctx, f.userID, x.child.ID, "approve_plan", "", nil, SubmitInputOptions{
		ExpectedGateRevision: i64(1), ExpectedResetStateDigest: digest,
		Reconciliation: &apitypes.MilestoneReconciliation{
			Mode: "reset", Reason: "replace descendant contract", ExpectedCompletionRevision: &priorRevision,
		},
	})
	if !errors.Is(err, ErrReconciliationConflict) || approval.ServerSide || approval.ID != 0 || calls != 0 {
		t.Fatalf("task milestone policy refusal=%+v err=%v remote reads=%d", approval, err, calls)
	}
	_, holdAfter := taskLifecycleHold(t, x, oldGeneration)
	if coordinateAuthoritySnapshot(t, f.env, x.child.ID) != authority || holdAfter != holdBefore ||
		x.rootEvidence(t) != rootBefore {
		t.Fatal("rejected task milestone reset changed authority, custody or root evidence")
	}
	x.ref(t, x.childRef, taskLifecycleTip)
	x.ref(t, x.rootRef, taskLifecycleAdvancedTip)
}

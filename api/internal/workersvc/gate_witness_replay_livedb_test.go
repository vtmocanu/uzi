package workersvc

import (
	"bytes"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

func TestPlanRevisionWitnessRestorationLiveDB(t *testing.T) {
	f, id, opts := newMilestoneReconciliationFix(t)
	original := reconciliationRow(t, f, id)
	pid := uuid.UUID(original.GatePresentationID.Bytes)
	restore := gateReq(i64(1), &pid)
	restore.Milestones = &[]Milestone{{ID: "M1", Title: "Replacement one"}, {ID: "M2", Title: "Replacement two"}}
	restore.GateCheckpointWitness = &apitypes.GateCheckpointWitness{
		SourceHead: "1111111111111111111111111111111111111111", PublishedCheckpoint: retentionTestTip,
	}
	restore.ExpectedGateRevision = i64(1)
	if _, err := f.svc.SubmitInputWithOptions(f.ctx, f.userID, id, "revise_plan", "revise the replacement", nil,
		SubmitInputOptions{ExpectedGateRevision: i64(1)}); err != nil {
		t.Fatal(err)
	}
	var inputID int64
	if err := f.pool.QueryRow(f.ctx, "SELECT id FROM run_user_inputs WHERE run_id=$1 AND kind='revise_plan'", id).Scan(&inputID); err != nil {
		t.Fatal(err)
	}
	// Model delivery by this claim, followed by a clarification during revision.
	f.exec("UPDATE run_user_inputs SET consumed_at=now(), consumed_worker_id=$2, consumed_claim_generation=1 WHERE id=$1", inputID, f.wkr.ID)
	if _, applied, _, err := f.report(id, StateRequest{State: "awaiting_input", ClaimGeneration: i64(1), OpenQuestionID: strp("revision-question")}); err != nil || !applied {
		t.Fatalf("clarification: applied=%v err=%v", applied, err)
	}
	f.mustPublish(id, restore, 1)
	restored := reconciliationRow(t, f, id)
	if restored.Status != "awaiting_approval" || restored.GateRevision != 1 || restored.ReviseCount != 1 ||
		restored.GatePresentationID != original.GatePresentationID ||
		!bytes.Equal(restored.GatePresentedPayload, original.GatePresentedPayload) ||
		!bytes.Equal(restored.GatePayloadDigest, original.GatePayloadDigest) ||
		!bytes.Equal(restored.MilestonesFrozen, original.MilestonesFrozen) ||
		!bytes.Equal(restored.CompletionContract, original.CompletionContract) {
		t.Fatal("restoration changed the original presentation, witness or frozen contract")
	}
	var pending bool
	if err := f.pool.QueryRow(f.ctx, "SELECT applied_at IS NULL FROM run_user_inputs WHERE id=$1", inputID).Scan(&pending); err != nil || !pending {
		t.Fatalf("restoration settled unanswered feedback: pending=%v err=%v", pending, err)
	}
	// Fresh consent cannot renew the restored witness's old captured revise count.
	var err error
	opts.ExpectedResetStateDigest, err = ResetStateDigest(restored)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.SubmitInputWithOptions(f.ctx, f.userID, id, "approve_plan", "", nil, opts); !errors.Is(err, ErrReconciliationNotReady) {
		t.Fatalf("restored stale witness accepted for reset: %v", err)
	}
	if reconciliationRow(t, f, id).ClaimReleasedAt.Valid {
		t.Fatal("stale witness released the claim")
	}
	changed := restore
	changed.ExpectedGateRevision = nil // ordinary replay exposes the presentation conflict
	changed.GateCheckpointWitness = &apitypes.GateCheckpointWitness{
		SourceHead: "3333333333333333333333333333333333333333", PublishedCheckpoint: retentionTestTip,
	}
	if _, applied, _, err := f.report(id, changed); !errors.Is(err, ErrGatePresentationConflict) || applied {
		t.Fatalf("changed witness under current id: applied=%v err=%v", applied, err)
	}
	fresh := changed
	fresh.PresentationID, fresh.ExpectedGateRevision, fresh.RevisionInputID = uid(), i64(1), &inputID
	fresh.PlanMd = strp("# Revised plan answering the feedback")
	f.mustPublish(id, fresh, 2)
	published := reconciliationRow(t, f, id)
	payload, ok, err := decodeGatePresentedPayload(published.GatePresentedPayload)
	if err != nil || !ok || payload.ReviseCount == nil || *payload.ReviseCount != 1 {
		t.Fatalf("fresh publication did not capture current count: %+v %v", payload.ReviseCount, err)
	}
	// The former presentation must not retain against the newer presentation's snapshot.
	if _, applied, _, err := f.report(id, restore); !errors.Is(err, ErrStaleRevisionSource) || applied {
		t.Fatalf("old source replay after fresh publication: applied=%v err=%v", applied, err)
	}
	if after := reconciliationRow(t, f, id); !bytes.Equal(after.GatePresentedPayload, published.GatePresentedPayload) || after.GateRevision != 2 {
		t.Fatal("old source replay changed the newer presentation")
	}
	opts.ExpectedGateRevision = i64(2)
	opts.ExpectedResetStateDigest, err = ResetStateDigest(published)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.SubmitInputWithOptions(f.ctx, f.userID, id, "approve_plan", "", nil, opts); err != nil {
		t.Fatalf("fresh witnessed publication not ready for explicit reset: %v", err)
	}
	assertReconciliationProgressEmpty(t, reconciliationRow(t, f, id))
}

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
	if _, err := f.svc.SubmitInput(f.ctx, f.userID, id, "answer", answerBody(t, "revision-question", "continue"), nil); err != nil {
		t.Fatal(err)
	}
	f.exec("UPDATE run_user_inputs SET consumed_at=now(), applied_at=now(), consumed_worker_id=$2, consumed_claim_generation=1 WHERE run_id=$1 AND kind='answer'", id, f.wkr.ID)
	if _, applied, _, err := f.report(id, StateRequest{State: "running", ClaimGeneration: i64(1)}); err != nil || !applied {
		t.Fatalf("answered clarification: applied=%v err=%v", applied, err)
	}
	for _, changedFence := range []string{"generation", "source revision"} {
		t.Run(changedFence, func(t *testing.T) {
			stale := restore
			want := ErrStaleClaim
			if changedFence == "generation" {
				stale.ClaimGeneration = i64(2)
			} else {
				stale.ExpectedGateRevision = i64(2)
				want = ErrStaleRevisionSource
			}
			if _, applied, _, err := f.report(id, stale); applied || !errors.Is(err, want) {
				t.Fatalf("changed %s: applied=%v err=%v want=%v", changedFence, applied, err, want)
			}
			if row := reconciliationRow(t, f, id); row.Status != "running" || row.GateRevision != 1 ||
				!bytes.Equal(row.GatePresentedPayload, original.GatePresentedPayload) {
				t.Fatal("stale restoration changed the source")
			}
		})
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
	// Ordinary approval must wait for unanswered feedback.
	if _, err := f.svc.SubmitInputWithOptions(f.ctx, f.userID, id, "approve_plan", "", nil,
		SubmitInputOptions{ExpectedGateRevision: i64(1)}); !errors.Is(err, ErrPlanRevisionPending) {
		t.Fatalf("ordinary approval with pending feedback: %v", err)
	}
	// Old consent fails the state CAS before the witness-readiness check.
	if _, err := f.svc.SubmitInputWithOptions(f.ctx, f.userID, id, "approve_plan", "", nil, opts); !errors.Is(err, ErrReconciliationConflict) {
		t.Fatalf("stale reset consent accepted: %v", err)
	}
	// Fresh consent still cannot approve unanswered feedback. The standalone
	// stale-witness test settles feedback before checking witness readiness.
	var err error
	opts.ExpectedResetStateDigest, err = ResetStateDigest(restored)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.SubmitInputWithOptions(f.ctx, f.userID, id, "approve_plan", "", nil, opts); !errors.Is(err, ErrPlanRevisionPending) {
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
	// The expired source accepts an ordinary bound replacement J. I stays pending
	// until J actually publishes its revised plan under the same source fence.
	if _, err := f.svc.SubmitInputWithOptions(f.ctx, f.userID, id, "revise_plan", "replacement feedback", nil,
		SubmitInputOptions{ExpectedGateRevision: i64(1)}); err != nil {
		t.Fatalf("source-bound replacement revise: %v", err)
	}
	var replacementID int64
	if err := f.pool.QueryRow(f.ctx, "SELECT max(id) FROM run_user_inputs WHERE run_id=$1 AND kind='revise_plan'", id).Scan(&replacementID); err != nil {
		t.Fatal(err)
	}
	if replacementID <= inputID {
		t.Fatal("replacement did not create a newer feedback input")
	}
	f.exec("UPDATE run_user_inputs SET consumed_at=now(), consumed_worker_id=$2, consumed_claim_generation=1 WHERE id=$1", replacementID, f.wkr.ID)
	if err := f.pool.QueryRow(f.ctx, "SELECT applied_at IS NULL FROM run_user_inputs WHERE id=$1", inputID).Scan(&pending); err != nil || !pending {
		t.Fatalf("replacement settled predecessor before publication: pending=%v err=%v", pending, err)
	}
	fresh := changed
	fresh.PresentationID, fresh.ExpectedGateRevision, fresh.RevisionInputID = uid(), i64(1), &replacementID
	fresh.SupersededRevisionInputIDs = []int64{inputID}
	fresh.PlanMd = strp("# Revised plan answering the feedback")
	f.mustPublish(id, fresh, 2)
	published := reconciliationRow(t, f, id)
	payload, ok, err := decodeGatePresentedPayload(published.GatePresentedPayload)
	if err != nil || !ok || payload.ReviseCount == nil || *payload.ReviseCount != 2 {
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

func TestPlanRevisionWitnessReceiptReplayLiveDB(t *testing.T) {
	// Independent fixture: unlike the restoration test, revision 2 stays held and
	// unapproved while newer owner feedback invalidates its checkpoint readiness.
	f, id, opts := newMilestoneReconciliationFix(t)
	if _, err := f.svc.SubmitInputWithOptions(f.ctx, f.userID, id, "revise_plan", "first revision", nil,
		SubmitInputOptions{ExpectedGateRevision: i64(1)}); err != nil {
		t.Fatal(err)
	}
	var inputID int64
	if err := f.pool.QueryRow(f.ctx, "SELECT id FROM run_user_inputs WHERE run_id=$1 AND kind='revise_plan'", id).Scan(&inputID); err != nil {
		t.Fatal(err)
	}
	f.exec("UPDATE run_user_inputs SET consumed_at=now(), consumed_worker_id=$2, consumed_claim_generation=1 WHERE id=$1", inputID, f.wkr.ID)
	fresh := gateReq(i64(1), uid())
	fresh.ExpectedGateRevision, fresh.RevisionInputID = i64(1), &inputID
	fresh.PlanMd = strp("# Persisted revised plan")
	fresh.Milestones = &[]Milestone{{ID: "M1", Title: "Replacement one"}, {ID: "M2", Title: "Replacement two"}}
	fresh.GateCheckpointWitness = &apitypes.GateCheckpointWitness{
		SourceHead: "3333333333333333333333333333333333333333", PublishedCheckpoint: retentionTestTip,
	}
	initial, err := f.svc.SetStateReportWithReconciliation(f.ctx, f.wkr, id, fresh)
	if err != nil || !initial.Applied || initial.GateRevision != 2 || initial.RevisionInputID == nil || *initial.RevisionInputID != inputID {
		t.Fatalf("fresh revision publication: %+v err=%v", initial, err)
	}
	var settledBefore string
	if err := f.pool.QueryRow(f.ctx, "SELECT row_to_json(i)::text FROM run_user_inputs i WHERE id=$1", inputID).Scan(&settledBefore); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.SubmitInputWithOptions(f.ctx, f.userID, id, "revise_plan", "K after publication", nil,
		SubmitInputOptions{ExpectedGateRevision: i64(2)}); err != nil {
		t.Fatal(err)
	}
	beforeReplay := reconciliationRow(t, f, id)
	if beforeReplay.ReviseCount != 2 || !revisionReceiptReplay(beforeReplay, fresh) {
		t.Fatal("fixture lacks the newer revision request or committed receipt")
	}
	replay, err := f.svc.SetStateReportWithReconciliation(f.ctx, f.wkr, id, fresh)
	if err != nil || !replay.Applied || replay.GateRevision != 2 || replay.RevisionInputID == nil ||
		*replay.RevisionInputID != inputID || len(replay.SupersededRevisionInputIDs) != 0 {
		t.Fatalf("identical lost-ACK receipt replay: %+v err=%v", replay, err)
	}
	afterReplay := reconciliationRow(t, f, id)
	if !bytes.Equal(afterReplay.GatePresentedPayload, beforeReplay.GatePresentedPayload) ||
		!bytes.Equal(afterReplay.GatePayloadDigest, beforeReplay.GatePayloadDigest) ||
		afterReplay.GateRevision != 2 || afterReplay.GateRefusalCount != beforeReplay.GateRefusalCount ||
		!revisionReceiptReplay(afterReplay, fresh) {
		t.Fatal("receipt replay changed presentation, digest, receipt or refusal accounting")
	}
	var settledAfter string
	if err := f.pool.QueryRow(f.ctx, "SELECT row_to_json(i)::text FROM run_user_inputs i WHERE id=$1", inputID).Scan(&settledAfter); err != nil || settledAfter != settledBefore {
		t.Fatalf("receipt replay settled the original input twice: err=%v", err)
	}
	opts.ExpectedGateRevision = i64(2)
	opts.ExpectedResetStateDigest, err = ResetStateDigest(afterReplay)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.SubmitInputWithOptions(f.ctx, f.userID, id, "approve_plan", "", nil, opts); !errors.Is(err, ErrPlanRevisionPending) {
		t.Fatalf("receipt replay renewed stale witness readiness: %v", err)
	}
	var pending bool
	if err := f.pool.QueryRow(f.ctx, "SELECT count(*)=1 AND bool_and(applied_at IS NULL AND consumed_at IS NULL) FROM run_user_inputs WHERE run_id=$1 AND kind='revise_plan' AND id<>$2", id, inputID).Scan(&pending); err != nil || !pending {
		t.Fatalf("receipt replay consumed or applied K: pending=%v err=%v", pending, err)
	}
	// Once K is settled, pending feedback must no longer mask stale-witness
	// readiness. A lost-ACK replay cannot renew the captured revision count.
	f.exec("UPDATE run_user_inputs SET applied_at=now() WHERE run_id=$1 AND kind='revise_plan' AND id<>$2", id, inputID)
	opts.ExpectedResetStateDigest, err = ResetStateDigest(reconciliationRow(t, f, id))
	if err != nil {
		t.Fatal(err)
	}
	beforeApproval := reconciliationRaceSnapshot(t, f, id)
	if _, err := f.svc.SubmitInputWithOptions(f.ctx, f.userID, id, "approve_plan", "", nil, opts); !errors.Is(err, ErrReconciliationNotReady) {
		t.Fatalf("receipt replay renewed stale witness readiness after settlement: %v", err)
	}
	reconciliationRaceUnchanged(t, f, id, beforeApproval)
}

func TestGateWitnessOrdinaryReplayAfterReviseLiveDB(t *testing.T) {
	f, id, _ := newMilestoneReconciliationFix(t)
	original := reconciliationRow(t, f, id)
	pid := uuid.UUID(original.GatePresentationID.Bytes)
	ordinary := gateReq(i64(1), &pid) // deliberately no expected_gate_revision or revision receipt
	ordinary.Milestones = &[]Milestone{{ID: "M1", Title: "Replacement one"}, {ID: "M2", Title: "Replacement two"}}
	ordinary.GateCheckpointWitness = &apitypes.GateCheckpointWitness{
		SourceHead: "1111111111111111111111111111111111111111", PublishedCheckpoint: retentionTestTip,
	}
	if _, err := f.svc.SubmitInputWithOptions(f.ctx, f.userID, id, "revise_plan", "ordinary revise", nil,
		SubmitInputOptions{ExpectedGateRevision: i64(1)}); err != nil {
		t.Fatal(err)
	}
	before := reconciliationRow(t, f, id)
	if before.GateRefusalCount != 0 || before.ReviseCount != 1 {
		t.Fatal("fixture must have an uncharged accepted revision")
	}
	f.mustPublish(id, ordinary, 1)
	after := reconciliationRow(t, f, id)
	if after.GateRefusalCount != before.GateRefusalCount ||
		!bytes.Equal(after.GatePresentedPayload, before.GatePresentedPayload) ||
		!bytes.Equal(after.GatePayloadDigest, before.GatePayloadDigest) || after.GateRevision != before.GateRevision {
		t.Fatal("ordinary exact replay changed the witnessed snapshot or charged a refusal")
	}
}

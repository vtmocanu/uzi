package recovery

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/capability"
)

func TestExactHoldMergedPolicyObservationLiveDB(t *testing.T) {
	e, snapshot := newMergedPredecessorEnv(t)
	e.w.ProtocolCapabilities = append(e.w.ProtocolCapabilities, capability.RecoveryHeldPublicationV2)
	n, err := e.svc.DiscardMergedPredecessorHolds(e.ctx, snapshot)
	if err != nil || n != 1 {
		t.Fatalf("policy settlement = %d, %v", n, err)
	}
	out, err := e.svc.ObserveExactHold(e.ctx, e.w, e.run, 1)
	if err != nil || out == nil || out.State != "discarded" || out.ReleaseEvidence != "merged_predecessor_discard" {
		t.Fatalf("known policy settlement must be observed: %+v, %v", out, err)
	}
	if out.FinalReceipt != nil {
		t.Fatal("policy observation fabricated FINAL")
	}
}

func TestExactHoldCrossRunPolicyObservationLiveDB(t *testing.T) {
	e, w, b := newCrossRunBindingEnv(t, 3, 1)
	a, s := crossRunInputs(t, e, w, b)
	e.w.ProtocolCapabilities = append(e.w.ProtocolCapabilities, capability.RecoveryHeldPublicationV2)
	if n, err := e.svc.DiscardCrossRunHolds(e.ctx, a, s); err != nil || n != 1 {
		t.Fatalf("cross-run settlement = %d, %v", n, err)
	}
	var audit []byte
	if err := e.pool.QueryRow(e.ctx, "SELECT merged_predecessor_discard_binding FROM recovery_custody_holds WHERE id=$1", e.hold).Scan(&audit); err != nil {
		t.Fatal(err)
	}
	var got, want map[string]any
	if err := json.Unmarshal(audit, &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(crossRunJSON(t, b), &want); err != nil {
		t.Fatal(err)
	}
	if len(got) != 19 || !reflect.DeepEqual(got, want) {
		t.Fatalf("service must persist the exact 19-key witness binding: %s", audit)
	}
	out, err := e.svc.ObserveExactHold(e.ctx, e.w, e.run, 1)
	if err != nil || out == nil || out.HoldID != e.hold.String() || out.State != "discarded" || out.ReleaseEvidence != "merged_predecessor_discard" || out.FinalReceipt != nil || out.CompletedPublicationReceipt != nil {
		t.Fatalf("cross-run settlement must be observed without FINAL: %+v, %v", out, err)
	}
	before := mergedWorkSnapshot(t, e)
	// Ignore the first settlement response as a lost ACK, then retry the real
	// service and the worker's existing exact-observation reader.
	for i := 0; i < 2; i++ {
		if n, err := e.svc.DiscardCrossRunHolds(e.ctx, a, s); err != nil || n != 0 {
			t.Fatalf("lost-ACK retry = %d, %v", n, err)
		}
		retry, err := e.svc.ObserveExactHold(e.ctx, e.w, e.run, 1)
		if err != nil || !reflect.DeepEqual(retry, out) {
			t.Fatalf("lost-ACK observation changed: %+v, %v", retry, err)
		}
	}
	oldWorker := e.w
	oldWorker.ProtocolCapabilities = nil
	if out, err := e.svc.ObserveExactHold(e.ctx, oldWorker, e.run, 1); err != nil || out != nil {
		t.Fatalf("old worker must retain unknown exact-observation semantics: %+v, %v", out, err)
	}
	if !bytes.Equal(before, mergedWorkSnapshot(t, e)) {
		t.Fatal("observation and retries must leave closed custody unchanged")
	}
}

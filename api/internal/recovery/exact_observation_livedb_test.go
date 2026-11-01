package recovery

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/capability"
)

func TestExactHoldMergedPolicyObservationLiveDB(t *testing.T) {
	e, snapshot := newMergedPredecessorEnv(t)
	e.w.ProtocolCapabilities = append(e.w.ProtocolCapabilities, capability.RecoveryBindinglessClosedV1)
	n, err := e.svc.DiscardMergedPredecessorHolds(e.ctx, snapshot)
	if err != nil || n != 1 {
		t.Fatalf("policy settlement = %d, %v", n, err)
	}
	successor := uuid.New()
	tag, setupErr := e.pool.Exec(e.ctx, "INSERT INTO workers(id,user_id,name,token_hash,status) VALUES($1,$2,'later custodian',$3,'online')", successor, e.w.UserID, successor[:])
	if setupErr != nil || tag.RowsAffected() != 1 {
		t.Fatalf("successor fixture: %v %v", tag, setupErr)
	}
	tag, setupErr = e.pool.Exec(e.ctx, "UPDATE runs SET claim_generation=9,worker_id=$2 WHERE id=$1", e.run, successor)
	if setupErr != nil || tag.RowsAffected() != 1 {
		t.Fatalf("historical claim fixture: %v %v", tag, setupErr)
	}
	var historical bool
	if err := e.pool.QueryRow(e.ctx, "SELECT claim_generation=9 AND worker_id=$2 FROM runs WHERE id=$1", e.run, successor).Scan(&historical); err != nil || !historical {
		t.Fatalf("claim readback=%t %v", historical, err)
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
	e.w.ProtocolCapabilities = append(e.w.ProtocolCapabilities, capability.RecoveryBindinglessClosedV1)
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

// exactClosureFixtureChange deliberately models legacy corruption after genuine release.
// The caller supplies a readback predicate, since custody guards may silently refuse writes.
func exactClosureFixtureChange(t *testing.T, fx *closureFinalFx, table, set, check string, id uuid.UUID) {
	t.Helper()
	if table != "recovery_captures" && table != "recovery_prerequisite_closures" && table != "recovery_custody_holds" {
		t.Fatal("unsupported fixture table")
	}
	tx, err := fx.pool.Begin(fx.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(fx.ctx) }()
	if _, err = tx.Exec(fx.ctx, "ALTER TABLE "+table+" DISABLE TRIGGER USER"); err != nil {
		t.Fatal(err)
	}
	tag, err := tx.Exec(fx.ctx, "UPDATE "+table+" SET "+set+" WHERE id=$1", id)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("fixture rows=%d err=%v", tag.RowsAffected(), err)
	}
	var ok bool
	if err = tx.QueryRow(fx.ctx, "SELECT "+check+" FROM "+table+" WHERE id=$1", id).Scan(&ok); err != nil || !ok {
		t.Fatalf("fixture readback=%t err=%v", ok, err)
	}
	if _, err = tx.Exec(fx.ctx, "ALTER TABLE "+table+" ENABLE TRIGGER USER"); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(fx.ctx); err != nil {
		t.Fatal(err)
	}
}

func TestExactHoldClosureImmutableEvidenceLiveDB(t *testing.T) {
	isolateClosureUploadDatabase(t)
	cases := []struct {
		name, table, set, check string
		known                   bool
	}{
		{"historical pin lost", "recovery_prerequisite_closures", "state='lost',live_run_id=NULL", "state='lost'", true},
		{"historical pin deleted", "recovery_prerequisite_closures", "state='deleted',live_run_id=NULL", "state='deleted'", true},
		{"historical pin absent", "recovery_prerequisite_closures", "absent_since=now(),absence_pass_id=gen_random_uuid()", "absent_since IS NOT NULL", true},
		{"historical capture expired", "recovery_captures", "expires_at=now()-interval '1 day'", "expires_at<now()", true},
		{"historical capture discarded", "recovery_captures", "state='discarded'", "state='discarded'", true},
		{"historical capture expired state", "recovery_captures", "state='expired'", "state='expired'", true},
		{"capture manifest unbound", "recovery_captures", "manifest_bound=false", "NOT manifest_bound", false},
		{"capture label mismatch", "recovery_captures", "original_worker_identity='different saved label'", "original_worker_identity='different saved label'", false},
		{"capture worker mismatch", "recovery_captures", "original_worker_id=gen_random_uuid()", "original_worker_id IS NOT NULL", false},
		{"capture missing worker provenance", "recovery_captures", "original_worker_id=NULL", "original_worker_id IS NULL", false},
		{"capture owner mismatch", "recovery_captures", "user_id=gen_random_uuid()", "user_id IS NOT NULL", false},
		{"capture run mismatch", "recovery_captures", "run_id=gen_random_uuid()", "run_id IS NOT NULL", false},
		{"capture source mismatch", "recovery_captures", "source_sha='bbbb2222'", "source_sha='bbbb2222'", false},
		{"capture digest mismatch", "recovery_captures", "coverage_digest=repeat('e',64)", "coverage_digest=repeat('e',64)", false},
		{"closure checksum mismatch", "recovery_prerequisite_closures", "checksum=repeat('e',64)", "checksum=repeat('e',64)", false},
		{"capture checksum mismatch", "recovery_captures", "checksum=repeat('e',64)", "checksum=repeat('e',64)", false},
		{"closure prerequisites mismatch", "recovery_prerequisite_closures", "prerequisite_shas=ARRAY[repeat('e',40)]", "prerequisite_shas=ARRAY[repeat('e',40)]", false},
		{"capture prerequisites mismatch", "recovery_captures", "prerequisite_shas=ARRAY[repeat('e',40)]", "prerequisite_shas=ARRAY[repeat('e',40)]", false},
		{"closure owner mismatch", "recovery_prerequisite_closures", "user_id=gen_random_uuid()", "user_id IS NOT NULL", false},
		{"closure repo mismatch", "recovery_prerequisite_closures", "repo_id=gen_random_uuid()", "repo_id IS NOT NULL", false},
		{"closure run mismatch", "recovery_prerequisite_closures", "run_id=gen_random_uuid(),state='lost',live_run_id=NULL", "state='lost'", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newClosureFinalFx(t, "pinned")
			ack, err := fx.release(fx.w, fx.request(fx.capture))
			if err != nil || !ack.Released {
				t.Fatalf("genuine release: %+v %v", ack, err)
			}
			fx.w.ProtocolCapabilities = []string{capability.RecoveryBindinglessClosedV1}
			tag, err := fx.pool.Exec(fx.ctx, "UPDATE runs SET claim_generation=9,worker_id=NULL WHERE id=$1", fx.run)
			if err != nil || tag.RowsAffected() != 1 {
				t.Fatalf("historical run setup: %v %v", tag, err)
			}
			id := fx.capture
			if tc.table == "recovery_prerequisite_closures" {
				id = fx.closure().ID
			}
			if tc.table == "recovery_custody_holds" {
				id = fx.hold
			}
			beforeMutation := fx.snapshot(tc.table, id)
			exactClosureFixtureChange(t, fx, tc.table, tc.set, tc.check, id)
			if beforeMutation == fx.snapshot(tc.table, id) {
				t.Fatal("fixture did not change")
			}
			before := fx.snapshot("recovery_custody_holds", fx.hold) + fx.snapshot("recovery_captures", fx.capture) + fx.snapshot("recovery_prerequisite_closures", fx.closure().ID)
			r0, o0, a0, f0 := fx.port.counts()
			g0 := fx.gate.total()
			out, err := fx.svc.ObserveExactHold(fx.ctx, fx.w, fx.run, fx.gen)
			if err != nil || (out != nil) != tc.known {
				t.Fatalf("known=%t want=%t observation=%+v err=%v", out != nil, tc.known, out, err)
			}
			after := fx.snapshot("recovery_custody_holds", fx.hold) + fx.snapshot("recovery_captures", fx.capture) + fx.snapshot("recovery_prerequisite_closures", fx.closure().ID)
			if before != after {
				t.Fatal("historical read mutated evidence")
			}
			r, o, a, f := fx.port.counts()
			if r != r0 || o != o0 || a != a0 || f != f0 || fx.gate.total() != g0 {
				t.Fatal("historical read invoked FINAL forge admission")
			}
		})
	}
}

func TestExactHoldClosureOrderedPrerequisitesLiveDB(t *testing.T) {
	isolateClosureUploadDatabase(t)
	fx := newClosureFinalFx(t, "pinned")
	ack, err := fx.release(fx.w, fx.request(fx.capture))
	if err != nil || !ack.Released {
		t.Fatalf("release: %+v %v", ack, err)
	}
	// Model an older immutable two-prerequisite receipt and then change only its order.
	ordered := "ARRAY[repeat('b',40),repeat('c',40)]"
	for _, table := range []string{"recovery_captures", "recovery_prerequisite_closures"} {
		id := fx.capture
		if table == "recovery_prerequisite_closures" {
			id = fx.closure().ID
		}
		exactClosureFixtureChange(t, fx, table, "prerequisite_shas="+ordered, "prerequisite_shas="+ordered, id)
	}
	if out, err := fx.svc.ObserveExactHold(fx.ctx, fx.w, fx.run, fx.gen); err != nil || out == nil {
		t.Fatalf("ordered positive control: %+v %v", out, err)
	}
	reversed := "ARRAY[repeat('c',40),repeat('b',40)]"
	exactClosureFixtureChange(t, fx, "recovery_captures", "prerequisite_shas="+reversed, "prerequisite_shas="+reversed, fx.capture)
	if out, err := fx.svc.ObserveExactHold(fx.ctx, fx.w, fx.run, fx.gen); err != nil || out != nil {
		t.Fatalf("reordered prerequisites accepted: %+v %v", out, err)
	}
}

func TestExactHoldArchiveHistoricalEvidenceLiveDB(t *testing.T) {
	isolateClosureUploadDatabase(t)
	for _, bad := range []bool{false, true} {
		t.Run(fmt.Sprintf("bad_manifest_%t", bad), func(t *testing.T) {
			fx := newClosureFinalFx(t, "pinned")
			ack, err := fx.release(fx.w, fx.request(fx.capture))
			if err != nil || !ack.Released {
				t.Fatalf("release: %+v %v", ack, err)
			}
			exactClosureFixtureChange(t, fx, "recovery_custody_holds", "release_evidence='archive',final_closure_id=NULL", "release_evidence='archive' AND final_closure_id IS NULL", fx.hold)
			exactClosureFixtureChange(t, fx, "recovery_captures", "state='expired',expires_at=now()-interval '1 day'", "state='expired' AND expires_at<now()", fx.capture)
			if bad {
				exactClosureFixtureChange(t, fx, "recovery_captures", "manifest_bound=false", "NOT manifest_bound", fx.capture)
			}
			if out, err := fx.svc.ObserveExactHold(fx.ctx, fx.w, fx.run, fx.gen); err != nil || (out != nil) == bad {
				t.Fatalf("historical archive: %+v %v", out, err)
			}
		})
	}
}

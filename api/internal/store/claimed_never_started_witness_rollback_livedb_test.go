package store_test

import "testing"

// TestClaimedNeverStartedWitnessRollbackLiveDB runs 00401's Down inside a rolled-back
// transaction: a run parked at retained_continuation_unwitnessed moves to the owner-held
// retained_recovery_exhausted park (still recovery_wait, custody and claim fields untouched),
// the pre-00401 CHECK and fn_custody_attention are restored, and the witness columns go away.
func TestClaimedNeverStartedWitnessRollbackLiveDB(t *testing.T) {
	f := newAdmissionFixture(t, 1)
	tx, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(f.ctx) }()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := tx.Exec(f.ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	attention := func(cause string) string {
		t.Helper()
		var got string
		if err := tx.QueryRow(f.ctx, "SELECT fn_custody_attention('open',true,false,'available','recovery_wait',$1)", cause).Scan(&got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	exec("UPDATE runs SET status='recovery_wait', recovery_wait_cause='retained_continuation_unwitnessed', recovery_retry_not_before=now()+interval '1 hour' WHERE id=$1", f.runs[0])
	var claimBefore string
	if err := tx.QueryRow(f.ctx, "SELECT concat_ws('|', worker_id, claim_generation, claim_released_at, released_worker_id) FROM runs WHERE id=$1", f.runs[0]).Scan(&claimBefore); err != nil {
		t.Fatal(err)
	}
	if got := attention("retained_continuation_unwitnessed"); got != "source_only" {
		t.Fatalf("Up attention = %s, want source_only", got)
	}
	for _, stmt := range migrationDownStatements(t, "00401_claimed_never_started_witness.sql") {
		exec(stmt)
	}
	var ok bool
	if err := tx.QueryRow(f.ctx, "SELECT status='recovery_wait' AND recovery_wait_cause='retained_recovery_exhausted' AND recovery_retry_not_before IS NULL AND finished_at IS NULL AND fail_origin IS NULL FROM runs WHERE id=$1", f.runs[0]).Scan(&ok); err != nil || !ok {
		t.Fatalf("Down did not move the run to the non-terminal owner-held park: ok=%v err=%v", ok, err)
	}
	var claimAfter string
	if err := tx.QueryRow(f.ctx, "SELECT concat_ws('|', worker_id, claim_generation, claim_released_at, released_worker_id) FROM runs WHERE id=$1", f.runs[0]).Scan(&claimAfter); err != nil || claimAfter != claimBefore {
		t.Fatalf("Down changed claim fields: before=%q after=%q err=%v", claimBefore, claimAfter, err)
	}
	if got := attention("retained_continuation_unwitnessed"); got != "archive_ready" {
		t.Fatalf("Down attention for the new cause = %s, want archive_ready (function restored)", got)
	}
	exec("SAVEPOINT narrow")
	if _, err := tx.Exec(f.ctx, "UPDATE runs SET recovery_wait_cause='retained_continuation_unwitnessed' WHERE id=$1", f.runs[0]); err == nil {
		t.Fatal("Down CHECK still admits the new cause")
	}
	exec("ROLLBACK TO SAVEPOINT narrow")
	var cols int
	if err := tx.QueryRow(f.ctx, "SELECT count(*) FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='runs' AND column_name IN ('registration_evidence_generation','registration_evidence_nonce','registration_evidence_epoch','snapshot_departed_generation','snapshot_departed_nonce','snapshot_departed_epoch')").Scan(&cols); err != nil || cols != 0 {
		t.Fatalf("witness columns after Down = %d (%v), want 0", cols, err)
	}
}

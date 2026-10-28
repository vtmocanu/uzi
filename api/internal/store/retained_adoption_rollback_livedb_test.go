package store_test

import (
	"testing"

	"github.com/google/uuid"
)

func TestRetainedAdoptionRollbackLiveDB(t *testing.T) {
	f := newAdmissionFixture(t, 3)
	f.capture(f.holds[0], f.runs[0], f.owner, "available")
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
	snapshot := func(sql string, id uuid.UUID) string {
		t.Helper()
		var v string
		if err := tx.QueryRow(f.ctx, sql, id).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	exec("UPDATE runs SET status='recovery_wait', claim_released_at=now(), retained_park_count=2, recovery_wait_cause='retained_recovery_adoption_failed', recovery_retry_not_before=now()+interval '1 hour' WHERE id=$1", f.runs[0])
	exec("UPDATE runs SET status='recovery_wait', recovery_wait_cause='retained_recovery_exhausted', recovery_retry_not_before=now()+interval '1 hour' WHERE id=$1", f.runs[1])
	// The neighboring idle hold must survive adoption Up/Down without acquiring
	// an owner-grant or custody-retention policy from the adoption cause.
	exec("UPDATE runs SET status='recovery_wait', recovery_wait_cause='codex_idle_stall', recovery_retry_not_before=NULL, idle_stall_park_count=4 WHERE id=$1", f.runs[2])
	const whole = "SELECT to_jsonb(r)::text FROM runs r WHERE id=$1"
	const preserved = "SELECT (to_jsonb(r)-ARRAY['status','status_since','updated_at','finished_at','fail_origin','failure_reason','recovery_wait_cause','recovery_retry_not_before'])::text FROM runs r WHERE id=$1"
	const custody = "SELECT json_build_array(row_to_json(h),(SELECT json_agg(row_to_json(c)) FROM recovery_captures c WHERE c.hold_id=h.id))::text FROM recovery_custody_holds h WHERE id=$1"
	idle := snapshot(whole, f.runs[2])
	old, keep, source := snapshot(whole, f.runs[1]), snapshot(preserved, f.runs[0]), snapshot(custody, f.holds[0])
	for _, stmt := range migrationDownStatements(t, "00372_retained_recovery_adoption_failed.sql") {
		exec(stmt)
	}
	if snapshot(whole, f.runs[2]) != idle {
		t.Fatal("adoption Down changed the idle-stall hold")
	}
	var terminal bool
	if err := tx.QueryRow(f.ctx, "SELECT status='failed' AND fail_origin='worker_lost' AND finished_at IS NOT NULL AND recovery_wait_cause IS NULL AND recovery_retry_not_before IS NULL FROM runs WHERE id=$1", f.runs[0]).Scan(&terminal); err != nil || !terminal {
		t.Fatalf("Down terminalized=%v err=%v", terminal, err)
	}
	if snapshot(whole, f.runs[1]) != old || snapshot(preserved, f.runs[0]) != keep || snapshot(custody, f.holds[0]) != source {
		t.Fatal("Down changed other cause, retained state or source inventory")
	}
	exec("SAVEPOINT narrow")
	if _, err := tx.Exec(f.ctx, "UPDATE runs SET recovery_wait_cause='retained_recovery_adoption_failed' WHERE id=$1", f.runs[0]); err == nil {
		t.Fatal("Down CHECK still admits new cause")
	}
	exec("ROLLBACK TO SAVEPOINT narrow")
	for _, stmt := range migrationUpStatements(t, "00372_retained_recovery_adoption_failed.sql") {
		exec(stmt)
	}
	if snapshot(whole, f.runs[2]) != idle {
		t.Fatal("adoption Up changed the idle-stall hold")
	}
	var idleAttention string
	if err := tx.QueryRow(f.ctx, "SELECT fn_custody_attention('open',true,false,'available','recovery_wait','codex_idle_stall')").Scan(&idleAttention); err != nil || idleAttention != "archive_ready" {
		t.Fatalf("idle custody settlement changed: %s %v", idleAttention, err)
	}
	exec("UPDATE runs SET status='recovery_wait', recovery_wait_cause='retained_recovery_adoption_failed' WHERE id=$1", f.runs[0])
	for _, state := range []string{"open", "discarded", "released"} {
		want := state
		if state == "open" {
			want = "source_only"
		}
		var got string
		if err := tx.QueryRow(f.ctx, "SELECT fn_custody_attention($1,true,false,'available','recovery_wait','retained_recovery_adoption_failed')", state).Scan(&got); err != nil || got != want {
			t.Fatalf("Up attention=%s want=%s err=%v", got, want, err)
		}
	}
}

package recovery

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// Blocking the last row proves the actual release transaction already owns every
// earlier row. Relation locks alone would also pass with the row order reversed.
func TestRecoveryClosureFinalClosureLastLockOrderLiveDB(t *testing.T) {
	isolateClosureUploadDatabase(t)
	fx := newClosureFinalFx(t, "pinned")
	blocker := fx.observer()
	tx, err := blocker.Begin(fx.ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	closureID := fx.closure().ID
	obs := fx.observer()
	// Probe all earlier row locks in one server round trip, before the real
	// 500ms closure wait expires. Four client round trips would test host load.
	_, err = obs.Exec(fx.ctx, `CREATE FUNCTION pg_temp.closure_lock_probe(w uuid,r uuid,h uuid,c uuid)
 RETURNS boolean[] LANGUAGE plpgsql AS $$
 DECLARE result boolean[] := '{}'; blocked boolean;
 BEGIN
  blocked:=false;
  BEGIN PERFORM id FROM workers WHERE id=w FOR UPDATE NOWAIT;
   EXCEPTION WHEN lock_not_available THEN blocked:=true; END;
  result:=array_append(result,blocked); blocked:=false;
  BEGIN PERFORM id FROM runs WHERE id=r FOR UPDATE NOWAIT;
   EXCEPTION WHEN lock_not_available THEN blocked:=true; END;
  result:=array_append(result,blocked); blocked:=false;
  BEGIN PERFORM id FROM recovery_custody_holds WHERE id=h FOR UPDATE NOWAIT;
   EXCEPTION WHEN lock_not_available THEN blocked:=true; END;
  result:=array_append(result,blocked); blocked:=false;
  BEGIN PERFORM id FROM recovery_captures WHERE id=c FOR UPDATE NOWAIT;
   EXCEPTION WHEN lock_not_available THEN blocked:=true; END;
  RETURN array_append(result,blocked);
 END $$`)
	if err != nil {
		t.Fatal(err)
	}
	fx.svc.finalClosureHook = func(stage string) {
		if stage == "observed" {
			_, err := tx.Exec(fx.ctx, "SELECT id FROM recovery_prerequisite_closures WHERE id=$1 FOR UPDATE", closureID)
			if err != nil {
				t.Error(err)
			}
		}
	}
	ctx, cancel := context.WithCancel(fx.ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := fx.svc.Release(ctx, fx.w, fx.run, fx.request(fx.capture))
		done <- err
	}()
	// Always join before fixture cleanup, including a failed lock-order assertion.
	t.Cleanup(func() {
		cancel()
		_ = tx.Rollback(context.Background())
		select {
		case err := <-done:
			if err != nil {
				t.Logf("FINAL request settled after cleanup cancel: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("FINAL request did not settle")
		}
	})
	var pid int32
	deadline := time.Now().Add(2 * time.Second)
	for pid == 0 {
		err := obs.QueryRow(fx.ctx, `SELECT pid FROM pg_stat_activity
 WHERE datname=current_database() AND $1=ANY(pg_blocking_pids(pid))
 AND wait_event_type='Lock' AND strpos(query,'recovery_prerequisite_closures')>0`, blocker.PgConn().PID()).Scan(&pid)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			t.Fatal(err)
		}
		if pid == 0 {
			if time.Now().After(deadline) {
				t.Fatal("FINAL did not wait on the closure row")
			}
			time.Sleep(time.Millisecond)
		}
	}
	var relations int
	var blocked []bool
	if err := obs.QueryRow(fx.ctx, `SELECT (SELECT count(DISTINCT relation) FROM pg_locks
 WHERE pid=$1 AND granted AND locktype='relation' AND mode='RowShareLock'
 AND relation IN ('workers'::regclass,'runs'::regclass,'recovery_custody_holds'::regclass,
 'recovery_captures'::regclass,'recovery_prerequisite_closures'::regclass)),
 pg_temp.closure_lock_probe($2,$3,$4,$5)`, pid, fx.w.ID, fx.run, fx.hold, fx.capture).Scan(&relations, &blocked); err != nil || relations != 5 {
		t.Fatalf("release relation locks: %d, %v", relations, err)
	}
	if len(blocked) != 4 {
		t.Fatalf("row-lock probe returned %v", blocked)
	}
	for i, table := range []string{"workers", "runs", "recovery_custody_holds", "recovery_captures"} {
		if !blocked[i] {
			t.Fatalf("FINAL did not own %s before waiting on closure", table)
		}
	}
	fx.assertOpen()
}

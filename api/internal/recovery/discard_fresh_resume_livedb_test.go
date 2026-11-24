package recovery

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// #4628: a fresh-resume Start holds the capture FOR SHARE while it inserts its pending row. An owner
// Discard that begins in that window must wait for the Start to commit and then REFUSE: the capture
// stays available with its chunks. (The EXISTS guard inside one FOR UPDATE statement reads the
// pre-wait snapshot and would miss the row; the service locks first and checks in a second statement.)
func TestDiscardRefusesCaptureReservedByConcurrentFreshResumeStartLiveDB(t *testing.T) {
	e := newInventoryEnv(t)
	id := e.reserve("fresh-resume-barrier")
	e.upload(id)
	t.Cleanup(func() { e.exec("DELETE FROM run_fresh_resumes WHERE capture_id=$1", id) })

	tx, err := e.pool.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(e.ctx) }()
	if _, err := tx.Exec(e.ctx, `SELECT 1 FROM recovery_captures WHERE id=$1 FOR SHARE`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(e.ctx, `INSERT INTO run_fresh_resumes (run_id, user_id, request_id, evidence_generation, evidence_kind, capture_id, sha, candidate_token)
 VALUES ($1,$2,$3,1,'capture',$4,'aaaa1111','tok')`, e.run, e.w.UserID, uuid.New(), id); err != nil {
		t.Fatal(err)
	}

	type result struct {
		ok  bool
		err error
	}
	done := make(chan result, 1)
	go func() {
		ok, err := e.svc.Discard(e.ctx, e.w.UserID, e.run, id)
		done <- result{ok, err}
	}()

	// The Discard must be blocked on the capture row lock, not finished, while the Start is uncommitted.
	deadline := time.Now().Add(10 * time.Second)
	for blocked := false; !blocked; {
		select {
		case r := <-done:
			t.Fatalf("Discard finished while the Start was uncommitted: %+v", r)
		default:
		}
		var n int
		if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock' AND query ILIKE '%recovery_captures%FOR UPDATE%'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		blocked = n > 0
		if !blocked {
			if time.Now().After(deadline) {
				t.Fatal("Discard never blocked on the capture lock")
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	if err := tx.Commit(e.ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if r.ok || !errors.Is(r.err, ErrCaptureInFreshResume) {
			t.Fatalf("Discard after the Start committed: ok=%v err=%v, want ErrCaptureInFreshResume", r.ok, r.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Discard did not finish after the Start committed")
	}

	var state string
	if err := e.pool.QueryRow(e.ctx, `SELECT state FROM recovery_captures WHERE id=$1`, id).Scan(&state); err != nil || state != "available" {
		t.Fatalf("capture state = %q (%v), want available", state, err)
	}
	chunks, err := e.q.ListCaptureChunks(e.ctx, id)
	if err != nil || len(chunks) != 1 {
		t.Fatalf("chunks after the refused Discard: %d (%v), want 1", len(chunks), err)
	}

	// Once the transfer closes the same Discard goes through.
	e.exec("UPDATE run_fresh_resumes SET state='refused', closed_at=now() WHERE capture_id=$1", id)
	if ok, err := e.svc.Discard(e.ctx, e.w.UserID, e.run, id); err != nil || !ok {
		t.Fatalf("Discard after the transfer closed: ok=%v err=%v", ok, err)
	}
	if chunks, err := e.q.ListCaptureChunks(e.ctx, id); err != nil || len(chunks) != 0 {
		t.Fatalf("chunks after the allowed Discard: %d (%v)", len(chunks), err)
	}
}

// A foreign or absent capture still reports false with no error (the handler answers 404).
func TestDiscardForeignCaptureReportsFalseLiveDB(t *testing.T) {
	e := newInventoryEnv(t)
	ok, err := e.svc.Discard(e.ctx, e.w.UserID, e.run, uuid.New())
	if err != nil || ok {
		t.Fatalf("Discard of an absent capture: ok=%v err=%v", ok, err)
	}
}

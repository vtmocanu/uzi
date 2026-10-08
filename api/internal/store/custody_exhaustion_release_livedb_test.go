package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func assertExhaustionCustodyRetained(t *testing.T, f *admissionFixture) {
	t.Helper()
	var state string
	var liveWorker, liveRun bool
	var released, evidence bool
	if err := f.pool.QueryRow(f.ctx, `SELECT state, live_worker_id=$2, live_run_id=$3,
 released_at IS NOT NULL, release_evidence IS NOT NULL FROM recovery_custody_holds WHERE id=$1`,
		f.holds[0], f.worker, f.runs[0]).Scan(&state, &liveWorker, &liveRun, &released, &evidence); err != nil {
		t.Fatal(err)
	}
	if state != "open" || !liveWorker || !liveRun || released || evidence {
		t.Fatalf("custody changed: state=%s liveWorker=%v liveRun=%v released=%v evidence=%v",
			state, liveWorker, liveRun, released, evidence)
	}
	holds, err := f.q.ListCustodyHoldsForOwner(f.ctx, store.ListCustodyHoldsForOwnerParams{UserID: f.owner, RunID: pgtype.UUID{Bytes: f.runs[0], Valid: true}})
	if err != nil {
		t.Fatal(err)
	}
	if len(holds) != 1 || holds[0].ID != f.holds[0] || !holds[0].DecisionNeeded || !holds[0].HasAvailableCapture {
		t.Fatalf("owner decision/archive lost: %+v", holds)
	}
}

func TestExhaustionArchiveRetainsCustodyDecisionLiveDB(t *testing.T) {
	for _, newerUpload := range []bool{false, true} {
		name := "available archive"
		if newerUpload {
			name = "older available plus newer uploading"
		}
		t.Run(name, func(t *testing.T) {
			f := newAdmissionFixture(t, 2)
			f.exec("UPDATE runs SET status='recovery_wait', recovery_wait_cause='worker_requeue_exhausted' WHERE id=$1", f.runs[0])
			f.capture(f.holds[0], f.runs[0], f.owner, "available")
			if newerUpload {
				f.exec("UPDATE recovery_captures SET created_at=now()-interval '1 hour' WHERE hold_id=$1", f.holds[0])
				f.capture(f.holds[0], f.runs[0], f.owner, "uploading")
			}
			// A ready archive on an ordinary run must still qualify and release.
			f.capture(f.holds[1], f.runs[1], f.owner, "available")
			holds, err := f.q.ListReleasableCustodyHolds(f.ctx)
			if err != nil {
				t.Fatal(err)
			}
			control := false
			for _, h := range holds {
				if h.ID == f.holds[0] {
					t.Fatal("exhaustion decision selected for implicit archive release")
				}
				if h.ID == f.holds[1] {
					control = true
					if h.Reason != "archive" {
						t.Fatalf("ordinary archive reason=%s", h.Reason)
					}
					n, err := f.q.ReleaseCustodyHold(f.ctx, store.ReleaseCustodyHoldParams{
						ID: h.ID, ReleaseEvidence: pgtype.Text{String: h.Reason, Valid: true}})
					if err != nil || n != 1 {
						t.Fatalf("ordinary archive release rows=%d err=%v", n, err)
					}
				}
			}
			if !control {
				t.Fatal("ordinary archive control was not selected")
			}
			assertExhaustionCustodyRetained(t, f)
		})
	}
}

func TestExhaustionDelayedSelectionRetainsCustodyLiveDB(t *testing.T) {
	f := newAdmissionFixture(t, 1)
	f.capture(f.holds[0], f.runs[0], f.owner, "available")
	holds, err := f.q.ListReleasableCustodyHolds(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	selected := false
	for _, h := range holds {
		if h.ID == f.holds[0] && h.Reason == "archive" {
			selected = true
		}
	}
	if !selected {
		t.Fatal("archive must qualify before retry exhaustion")
	}
	f.exec("UPDATE runs SET status='recovery_wait', recovery_wait_cause='worker_requeue_exhausted' WHERE id=$1", f.runs[0])
	n, err := f.q.ReleaseCustodyHold(f.ctx, store.ReleaseCustodyHoldParams{
		ID: f.holds[0], ReleaseEvidence: pgtype.Text{String: "archive", Valid: true}})
	if err != nil || n != 0 {
		t.Fatalf("delayed archive release rows=%d err=%v, want zero", n, err)
	}
	assertExhaustionCustodyRetained(t, f)
}

func TestExhaustionConcurrentReleaseRechecksLockedRunLiveDB(t *testing.T) {
	f := newAdmissionFixture(t, 1)
	f.capture(f.holds[0], f.runs[0], f.owner, "available")
	ctx, cancel := context.WithTimeout(f.ctx, 10*time.Second)
	defer cancel()
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := tx.Rollback(f.ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Errorf("rollback exhaustion transaction: %v", err)
		}
	}()
	if _, err := tx.Exec(ctx, "UPDATE runs SET status='recovery_wait', recovery_wait_cause='worker_requeue_exhausted' WHERE id=$1", f.runs[0]); err != nil {
		t.Fatal(err)
	}
	conn, err := f.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	pid := conn.Conn().PgConn().PID()
	type result struct {
		n   int64
		err error
	}
	done := make(chan result, 1)
	go func() {
		n, err := store.New(conn).ReleaseCustodyHold(ctx, store.ReleaseCustodyHoldParams{
			ID: f.holds[0], ReleaseEvidence: pgtype.Text{String: "archive", Valid: true}})
		done <- result{n, err}
	}()
	// Always cancel and join the one bounded query before releasing its connection.
	defer func() {
		cancel()
		if done != nil {
			<-done
		}
	}()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var blocked bool
		if err := f.pool.QueryRow(ctx, "SELECT COALESCE((SELECT wait_event_type='Lock' FROM pg_stat_activity WHERE pid=$1),false)", pid).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		select {
		case r := <-done:
			done = nil
			t.Fatalf("release did not wait for the lifecycle run lock: rows=%d err=%v", r.n, r.err)
		case <-ctx.Done():
			t.Fatal("release never waited for the lifecycle run lock")
		case <-ticker.C:
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	r := <-done
	done = nil
	if r.err != nil || r.n != 0 {
		t.Fatalf("release after concurrent exhaustion rows=%d err=%v, want zero", r.n, r.err)
	}
	assertExhaustionCustodyRetained(t, f)
}

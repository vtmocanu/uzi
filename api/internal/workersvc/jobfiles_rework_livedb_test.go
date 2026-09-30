package workersvc

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1909 M1 rework, at the job-file store: Reserve refuses a run that is not the reserving
// owner's job, and the sweep never waits on a reservation a live Write holds. Skipped unless
// UZI_TEST_DATABASE_URL points at a throwaway database.

// TestJobFilesReserveRefusesForeignRunLiveDB: an output reservation names a run, and that run must
// be a job of the same owner. SumRunJobFiles counts across owners, so without the check another
// owner's job would be charged (and its caps consumed) by a stranger's reservation.
//
// CALIBRATION: delete the GetOwnedJobRun check from JobFiles.Reserve; the foreign reservation then
// inserts and this test goes red.
func TestJobFilesReserveRefusesForeignRunLiveDB(t *testing.T) {
	e := newJFEnv(t, wide())
	owner, stranger := e.seedUser(t), e.seedUser(t)
	run := e.jobFor(t, owner)

	if _, err := e.jf.Reserve(e.ctx, ReserveParams{UserID: stranger, RunID: &run, Direction: JobFileOutput, DisplayName: "o.txt", DeclaredSize: 10}); !errors.Is(err, ErrJobRunNotFound) {
		t.Fatalf("a stranger's reservation against another owner's job = %v, want ErrJobRunNotFound", err)
	}
	missing := uuid.New()
	if _, err := e.jf.Reserve(e.ctx, ReserveParams{UserID: owner, RunID: &missing, Direction: JobFileOutput, DisplayName: "o.txt", DeclaredSize: 10}); !errors.Is(err, ErrJobRunNotFound) {
		t.Fatalf("a reservation against a run that does not exist = %v, want ErrJobRunNotFound", err)
	}
	var n int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM job_files WHERE run_id = $1`, run).Scan(&n); err != nil || n != 0 {
		t.Fatalf("refused reservations left %d rows on the run (err %v), want 0", n, err)
	}
	if _, err := e.jf.Reserve(e.ctx, ReserveParams{UserID: owner, RunID: &run, Direction: JobFileOutput, DisplayName: "o.txt", DeclaredSize: 10}); err != nil {
		t.Fatalf("the owner's own reservation: %v", err)
	}
}

// TestJobFilesSweepSkipsWriterHeldReservationLiveDB: the sweep holds the shared advisory key, so a
// stale-looking reservation that a slow Write holds FOR UPDATE must be skipped (picked up next
// tick), never waited on, or every admission would stall behind the writer.
//
// CALIBRATION: turn ReleaseStaleJobFileReservations back into a plain DELETE ... WHERE state =
// 'reserved' AND created_at < @cutoff; the sweep then blocks on the held row and this test goes red.
func TestJobFilesSweepSkipsWriterHeldReservationLiveDB(t *testing.T) {
	l := wide()
	l.RequestDeadline = time.Minute
	e := newJFEnv(t, l)
	u := e.seedUser(t)
	held, err := e.jf.Reserve(e.ctx, ReserveParams{UserID: u, Direction: JobFileInput, DisplayName: "held.txt", DeclaredSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	e.exec(`UPDATE job_files SET created_at = now() - interval '1 hour' WHERE id = $1`, held.ID)

	writer, err := e.pool.Begin(e.ctx) // stands in for a Write that has locked its reservation.
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Rollback(e.ctx) }()
	if _, err := writer.Exec(e.ctx, `SELECT 1 FROM job_files WHERE id = $1 FOR UPDATE`, held.ID); err != nil {
		t.Fatal(err)
	}

	type out struct {
		res JobFilesSweepResult
		err error
	}
	done := make(chan out, 1)
	go func() {
		r, err := e.jf.Sweep(e.ctx)
		done <- out{r, err}
	}()
	select {
	case o := <-done:
		if o.err != nil || o.res.ReleasedReservations != 0 {
			t.Fatalf("sweep beside a writer-held reservation = %+v, %v; want it skipped (0 released)", o.res, o.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the sweep is blocked on a reservation a writer holds; it must skip locked rows")
	}
	if !e.fileExists(t, held.ID) {
		t.Fatal("the writer's reservation was deleted under it")
	}

	// Once the writer is gone the next tick releases it.
	if err := writer.Rollback(e.ctx); err != nil {
		t.Fatal(err)
	}
	if r, err := e.jf.Sweep(e.ctx); err != nil || r.ReleasedReservations != 1 {
		t.Fatalf("sweep after the writer finished = %+v, %v; want the stale reservation released", r, err)
	}
}

// TestJobFilesRunDeletedUnderInsertMapsToNotFoundLiveDB: a run deleted between Reserve's ownership
// read and its insert surfaces as the job_files.run_id foreign-key violation from the insert. The
// insert here goes straight at the query with a run id that no longer exists (the deleted-run
// state), and the resulting error must be recognised by isRunFKViolation, which Reserve maps to
// ErrJobRunNotFound.
//
// CALIBRATION: rename the constraint name compared in isRunFKViolation; this test goes red.
func TestJobFilesRunDeletedUnderInsertMapsToNotFoundLiveDB(t *testing.T) {
	e := newJFEnv(t, wide())
	owner := e.seedUser(t)
	gone := uuid.New()
	_, err := store.New(e.pool).ReserveJobFile(e.ctx, store.ReserveJobFileParams{
		UserID: owner, RunID: pgconv.UUIDPtr(&gone), Direction: JobFileOutput, DisplayName: "o.txt", ByteSize: 10,
	})
	if err == nil {
		t.Fatal("insert against a missing run succeeded")
	}
	if !isRunFKViolation(err) {
		t.Fatalf("insert error = %v, want the job_files_run_id_fkey violation recognised", err)
	}
}

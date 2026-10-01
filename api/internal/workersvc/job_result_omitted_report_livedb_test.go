package workersvc

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
	"strings"
	"testing"
	"time"
)

func TestEmptyReportRepostRemovesGeneratedReportLiveDB(t *testing.T) {
	e := newJFEnv(t, wide())
	e.svc.SetJobFiles(e.jf)
	u := e.seedJobUser(t)
	worker, run := e.heldJob(t, u)
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO job_origins(run_id) VALUES($1)`, run); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.SubmitJobResult(e.ctx, worker, run, 1, JobResultSubmission{Status: "completed", ReportMD: "# old report"}); err != nil {
		t.Fatal(err)
	}
	e.svc.WaitForGeneratedOutputs()
	ids := e.outputsNamed(t, run, JobOutputReportName)
	if len(ids) != 1 {
		t.Fatalf("initial reports=%d, want 1", len(ids))
	}
	oldID := ids[0]
	if _, _, err := e.jf.OpenForCaller(e.ctx, oldID, cliCaller(u)); err != nil {
		t.Fatalf("initial report is not downloadable: %v", err)
	}
	if err := e.svc.SubmitJobResult(e.ctx, worker, run, 1, JobResultSubmission{Status: "completed"}); err != nil {
		t.Fatal(err)
	}
	e.svc.WaitForGeneratedOutputs()
	rows, err := e.q.ListJobFilesForRun(e.ctx, store.ListJobFilesForRunParams{RunID: pgconv.UUID(run), MaxRows: 100})
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range rows {
		if file.DisplayName == JobOutputReportName {
			t.Fatal("empty current result still lists the old report.md")
		}
	}
	if _, _, err := e.jf.OpenForCaller(e.ctx, oldID, cliCaller(u)); !errors.Is(err, ErrJobFileNotFound) {
		t.Fatalf("old report download=%v, want not found", err)
	}
	if n := len(e.outputsNamed(t, run, JobOutputFindingsName)); n != 1 {
		t.Fatalf("findings files=%d, want 1", n)
	}
}

func TestOmittedReportCleanupPreservesNewerPostLiveDB(t *testing.T) {
	for _, generation := range []int64{1, 2} {
		t.Run(map[int64]string{1: "same generation", 2: "new claim generation"}[generation], func(t *testing.T) {
			e := newJFEnv(t, wide())
			e.svc.SetJobFiles(e.jf)
			u := e.seedJobUser(t)
			worker, run := e.heldJob(t, u)
			observed := make(chan int64, 1)
			release := make(chan struct{})
			done := make(chan error, 1)
			e.svc.genPostHook = func(sub JobResultSubmission) {
				if sub.ReportMD != "" {
					return
				}
				var seq int64
				if err := e.pool.QueryRow(e.ctx, `SELECT post_id FROM job_output_refusals WHERE run_id=$1 AND display_name='findings.json'`, run).Scan(&seq); err != nil {
					t.Error(err)
				}
				observed <- seq
				<-release
			}
			go func() { done <- e.svc.SubmitJobResult(e.ctx, worker, run, 1, JobResultSubmission{Status: "completed"}) }()
			seq := <-observed
			defer func() { close(release); <-done; e.svc.WaitForGeneratedOutputs() }()
			if generation == 2 {
				if _, err := e.pool.Exec(e.ctx, `UPDATE runs SET claim_generation=2 WHERE id=$1`, run); err != nil {
					t.Fatal(err)
				}
			}
			if err := e.svc.SubmitJobResult(e.ctx, worker, run, generation, JobResultSubmission{Status: "completed", ReportMD: "# latest"}); err != nil {
				t.Fatal(err)
			}
			e.svc.WaitForGeneratedOutputs()
			if err := e.svc.removeOmittedGeneratedReport(e.ctx, worker, run, 1, seq); err != nil {
				t.Fatal(err)
			}
			if got := e.reportContent(t, run, u); got != "# latest" {
				t.Fatalf("stale cleanup changed latest report to %q", got)
			}
		})
	}
}

func TestEmptyReportRepostClearsLatePredecessorLiveDB(t *testing.T) {
	e := newJFEnv(t, wide())
	e.svc.SetJobFiles(e.jf)
	e.svc.genReplyBound = 5 * time.Millisecond
	u := e.seedJobUser(t)
	worker, run := e.heldJob(t, u)
	release := holdStoredFiles(t, e, u)
	defer release()
	if err := e.svc.SubmitJobResult(e.ctx, worker, run, 1, JobResultSubmission{Status: "completed", ReportMD: "# late old writer"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		var waiting int
		if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event='advisory'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("old generator did not reach its reservation after checking its marker")
		}
		time.Sleep(time.Millisecond)
	}
	if err := e.svc.SubmitJobResult(e.ctx, worker, run, 1, JobResultSubmission{Status: "completed"}); err != nil {
		t.Fatal(err)
	}
	release()
	e.svc.WaitForGeneratedOutputs()
	if n := len(e.outputsNamed(t, run, JobOutputReportName)); n != 0 {
		t.Fatalf("late old writer left %d reports after the empty post", n)
	}
}

func TestOmittedReportCleanupReadsPostAfterRunLockLiveDB(t *testing.T) {
	e := newJFEnv(t, wide())
	e.svc.SetJobFiles(e.jf)
	u := e.seedJobUser(t)
	worker, run := e.heldJob(t, u)
	sub := JobResultSubmission{Status: "completed", ReportMD: "# keep"}
	if err := e.svc.SubmitJobResult(e.ctx, worker, run, 1, sub); err != nil {
		t.Fatal(err)
	}
	e.svc.WaitForGeneratedOutputs()
	// An empty committed post can temporarily coexist with a predecessor's late file.
	seq, err := e.q.NextJobOutputPostID(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.q.UpsertJobResult(e.ctx, store.UpsertJobResultParams{RunID: run, Status: "completed", ReportMd: ""}); err != nil {
		t.Fatal(err)
	}
	if _, err = e.q.InsertGeneratedOutputMarker(e.ctx, store.InsertGeneratedOutputMarkerParams{RunID: run, DisplayName: JobOutputFindingsName, Reason: RefusalGenerationPending, PostID: seq, ClaimGeneration: 1}); err != nil {
		t.Fatal(err)
	}
	tx, err := e.pool.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(e.ctx) }()
	q := store.New(tx)
	if _, err = q.GetRunOwnedByWorkerForUpdate(e.ctx, store.GetRunOwnedByWorkerForUpdateParams{ID: run, WorkerID: pgconv.UUID(worker.ID)}); err != nil {
		t.Fatal(err)
	}
	// A newer nonempty post commits while the old cleanup is waiting for the run lock.
	if err = q.UpsertJobResult(e.ctx, store.UpsertJobResultParams{RunID: run, Status: "completed", ReportMd: sub.ReportMD}); err != nil {
		t.Fatal(err)
	}
	newer, err := q.NextJobOutputPostID(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.jf.markGenerationPending(e.ctx, q, run, 1, newer, sub); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- e.svc.removeOmittedGeneratedReport(e.ctx, worker, run, 1, seq) }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		var waiting int
		if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND state='active' AND wait_event_type='Lock'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cleanup did not wait for the run lock")
		}
		time.Sleep(time.Millisecond)
	}
	if err = tx.Commit(e.ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	if got := e.reportContent(t, run, u); got != sub.ReportMD {
		t.Fatalf("cleanup used a pre-lock snapshot and changed the report to %q", got)
	}
}

type omittedReportFailureDB struct{ JobFilesDB }

func (db omittedReportFailureDB) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := db.JobFilesDB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return omittedReportFailureTx{Tx: tx}, nil
}

type omittedReportFailureTx struct{ pgx.Tx }

func (tx omittedReportFailureTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if strings.Contains(sql, "DeleteOmittedGeneratedJobReport") {
		return pgconn.CommandTag{}, errors.New("fixture report cleanup failure")
	}
	return tx.Tx.Exec(ctx, sql, args...)
}

func TestOmittedReportCleanupFailureRemainsVisibleLiveDB(t *testing.T) {
	e := newJFEnv(t, wide())
	e.svc.SetJobFiles(e.jf)
	e.jf.db = omittedReportFailureDB{JobFilesDB: e.pool}
	u := e.seedJobUser(t)
	worker, run := e.heldJob(t, u)
	if err := e.svc.SubmitJobResult(e.ctx, worker, run, 1, JobResultSubmission{Status: "completed"}); err != nil {
		t.Fatal(err)
	}
	e.svc.WaitForGeneratedOutputs()
	e.wantReasons(t, run, JobOutputFindingsName, RefusalGenerationFailed)
	if n := len(e.outputsNamed(t, run, JobOutputFindingsName)); n != 0 {
		t.Fatalf("cleanup failure was masked by storing %d findings files", n)
	}
}

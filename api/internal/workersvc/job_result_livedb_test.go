package workersvc

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// job_result_livedb_test.go covers PRD #1908's job-result ingest (SubmitJobResult) and the
// no-result invariant (a job reported completed without a result is failed job_no_result) against
// a live Postgres. Skipped unless UZI_TEST_DATABASE_URL points at a throwaway database; run via
// ./e2e/run-store-it.sh.

func i32p(v int32) *int32 { return &v }

func (e jobEnv) countRows(t *testing.T, table string, runID uuid.UUID) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM `+table+` WHERE run_id = $1`, runID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (e jobEnv) findingMessages(t *testing.T, runID uuid.UUID) []string {
	t.Helper()
	rows, err := e.pool.Query(e.ctx, `SELECT message_md FROM job_findings WHERE run_id = $1 ORDER BY ordinal`, runID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

func jobWorker(id, user uuid.UUID) store.Worker { return store.Worker{ID: id, UserID: user} }

// TestSubmitJobResultLiveDB: the ingest fences on ownership, kind, claim generation and terminality,
// and its write is one transaction that replaces the run's earlier result and findings.
func TestSubmitJobResultLiveDB(t *testing.T) {
	e := setupJobLiveDB(t, 0)
	u := e.seedJobUser(t)
	workerID := e.seedWorkerRow(t, u, false, nil, jobCap, jobFilesCap)
	wkr := jobWorker(workerID, u)
	runID := e.seedRawJob(t, u, "running", &workerID, time.Minute, 600) // claim_generation 1

	first := JobResultSubmission{Status: "completed", ReportMD: "first", Findings: []JobFindingSubmission{
		{Severity: "info", MessageMD: "a"},
		{Severity: "error", MessageMD: "b", File: strp("x.go"), Line: i32p(3)},
	}}
	if err := e.svc.SubmitJobResult(e.ctx, wkr, runID, 1, first); err != nil {
		t.Fatalf("first submit: %v", err)
	}
	if got := e.findingMessages(t, runID); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("findings after first submit = %v", got)
	}

	// Idempotent replace: a second POST swaps the result and the findings.
	second := JobResultSubmission{Status: "partial", ReportMD: "second", Findings: []JobFindingSubmission{
		{Severity: "warning", MessageMD: "c", URL: strp("https://example.com/x")},
	}}
	if err := e.svc.SubmitJobResult(e.ctx, wkr, runID, 1, second); err != nil {
		t.Fatalf("second submit: %v", err)
	}
	if got := e.findingMessages(t, runID); len(got) != 1 || got[0] != "c" {
		t.Fatalf("findings after replace = %v, want [c]", got)
	}
	var status, report string
	if err := e.pool.QueryRow(e.ctx, `SELECT status, report_md FROM job_results WHERE run_id = $1`, runID).Scan(&status, &report); err != nil {
		t.Fatal(err)
	}
	if status != "partial" || report != "second" || e.countRows(t, "job_results", runID) != 1 {
		t.Fatalf("result = %q/%q, want partial/second (one row)", status, report)
	}

	// Atomicity: a body that fails part-way (a finding breaking the url-xor-file CHECK) rolls the
	// whole write back, leaving the previous result and findings intact.
	bad := JobResultSubmission{Status: "completed", ReportMD: "bad", Findings: []JobFindingSubmission{
		{Severity: "info", MessageMD: "ok"},
		{Severity: "info", MessageMD: "both", URL: strp("https://example.com"), File: strp("f")},
	}}
	if err := e.svc.SubmitJobResult(e.ctx, wkr, runID, 1, bad); err == nil {
		t.Fatal("a finding violating the location CHECK must fail the submit")
	}
	if got := e.findingMessages(t, runID); len(got) != 1 || got[0] != "c" {
		t.Fatalf("findings after a failed submit = %v, want the previous [c]", got)
	}
	if err := e.pool.QueryRow(e.ctx, `SELECT report_md FROM job_results WHERE run_id = $1`, runID).Scan(&report); err != nil || report != "second" {
		t.Fatalf("report after a failed submit = %q, %v; want the previous %q", report, err, "second")
	}

	// Fences.
	if err := e.svc.SubmitJobResult(e.ctx, wkr, runID, 2, first); !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("stale generation: err = %v, want ErrStaleClaim", err)
	}
	other := e.seedWorkerRow(t, u, false, nil, jobCap, jobFilesCap)
	if err := e.svc.SubmitJobResult(e.ctx, jobWorker(other, u), runID, 1, first); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("a worker not holding the run: err = %v, want ErrRunNotFound", err)
	}
	chatID := uuid.New()
	e.exec(`INSERT INTO runs (id, user_id, kind, issue_title, issue_description, status, worker_id, claim_generation)
	        VALUES ($1, $2, 'chat', 't', 'd', 'running', $3, 1)`, chatID, u, workerID)
	if err := e.svc.SubmitJobResult(e.ctx, wkr, chatID, 1, first); !errors.Is(err, ErrNotJobRun) {
		t.Fatalf("a non-job run: err = %v, want ErrNotJobRun", err)
	}
	if n := e.countRows(t, "job_results", chatID); n != 0 {
		t.Fatalf("a refused non-job submit wrote %d result rows", n)
	}
	e.exec(`UPDATE runs SET status = 'failed', finished_at = now() WHERE id = $1`, runID)
	if err := e.svc.SubmitJobResult(e.ctx, wkr, runID, 1, first); !errors.Is(err, ErrRunTerminal) {
		t.Fatalf("a terminal run: err = %v, want ErrRunTerminal", err)
	}
	released := e.seedRawJob(t, u, "running", &workerID, time.Minute, 600)
	e.exec(`UPDATE runs SET claim_released_at = now() WHERE id = $1`, released)
	if err := e.svc.SubmitJobResult(e.ctx, wkr, released, 1, first); !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("a released claim: err = %v, want ErrStaleClaim", err)
	}
}

// TestJobNoResultInvariantLiveDB: a job the worker reports `completed` without having submitted a
// result is failed with fail_origin job_no_result, on both the generation-fenced and the legacy
// (no generation) report; with a result it completes; another kind is unchanged.
//
// MUTATION CHECK: removing the runkind.Job branch of SetState's completed arm makes the two
// no-result cases complete instead of fail, and this test goes red.
func TestJobNoResultInvariantLiveDB(t *testing.T) {
	e := setupJobLiveDB(t, 0)
	u := e.seedJobUser(t)
	workerID := e.seedWorkerRow(t, u, false, nil, jobCap, jobFilesCap)
	wkr := jobWorker(workerID, u)

	complete := func(t *testing.T, runID uuid.UUID, gen *int64) {
		t.Helper()
		if _, _, err := e.svc.SetState(e.ctx, wkr, runID, StateRequest{State: "completed", ClaimGeneration: gen}); err != nil {
			t.Fatalf("SetState completed: %v", err)
		}
	}
	t.Run("no result fails/generation-fenced", func(t *testing.T) {
		runID := e.seedRawJob(t, u, "running", &workerID, time.Minute, 600)
		complete(t, runID, i64Ptr(1))
		run := mustRun(t, e.codexTestEnv, runID)
		if run.Status != "failed" || !run.FailOrigin.Valid || run.FailOrigin.String != "job_no_result" {
			t.Fatalf("status = %q fail_origin = %+v, want failed / job_no_result", run.Status, run.FailOrigin)
		}
		if !run.FailureReason.Valid || run.FailureReason.String == "" || !run.FinishedAt.Valid {
			t.Fatalf("failure_reason = %+v finished_at = %+v, want a reason and a finish time", run.FailureReason, run.FinishedAt)
		}
	})
	// The no-result check is race-free only on the FOR UPDATE fenced path, which needs the
	// generation: a job state report that omits it is refused, and changes nothing.
	t.Run("a job report without claim_generation is refused", func(t *testing.T) {
		for _, state := range []string{"completed", "running", "failed"} {
			runID := e.seedRawJob(t, u, "running", &workerID, time.Minute, 600)
			_, _, err := e.svc.SetState(e.ctx, wkr, runID, StateRequest{State: state})
			if !errors.Is(err, ErrMissingClaimGeneration) {
				t.Fatalf("%s without generation: err = %v, want ErrMissingClaimGeneration", state, err)
			}
			if run := mustRun(t, e.codexTestEnv, runID); run.Status != "running" || run.FailOrigin.Valid {
				t.Fatalf("%s without generation changed the run: %s / %+v", state, run.Status, run.FailOrigin)
			}
		}
	})
	t.Run("with a result completes", func(t *testing.T) {
		runID := e.seedRawJob(t, u, "running", &workerID, time.Minute, 600)
		if err := e.svc.SubmitJobResult(e.ctx, wkr, runID, 1, JobResultSubmission{Status: "completed", ReportMD: "done"}); err != nil {
			t.Fatal(err)
		}
		complete(t, runID, i64Ptr(1))
		run := mustRun(t, e.codexTestEnv, runID)
		if run.Status != "completed" || run.FailOrigin.Valid {
			t.Fatalf("status = %q fail_origin = %+v, want completed with no fail_origin", run.Status, run.FailOrigin)
		}
	})
	t.Run("another kind is unchanged", func(t *testing.T) {
		chatID := uuid.New()
		e.exec(`INSERT INTO runs (id, user_id, kind, issue_title, issue_description, status, worker_id, claim_generation)
		        VALUES ($1, $2, 'chat', 't', 'd', 'running', $3, 0)`, chatID, u, workerID)
		complete(t, chatID, nil)
		if run := mustRun(t, e.codexTestEnv, chatID); run.Status != "completed" || run.FailOrigin.Valid {
			t.Fatalf("chat status = %q fail_origin = %+v, want completed", run.Status, run.FailOrigin)
		}
	})
}

// TestTimedOutJobNeverCompletesLiveDB: a job the wall backstop failed (run_timeout, claim kept) must
// not be flipped to completed by a late `completed` report. The runner's result POST is refused as
// terminal, so the issue #329 supersede arm of SetRunCompleted would otherwise yield a completed
// job with no result.
//
// MUTATION CHECK: removing the `kind <> 'job' OR status IN ('claimed', 'running')` conjunct from
// SetRunCompleted turns the run completed, and this test goes red.
func TestTimedOutJobNeverCompletesLiveDB(t *testing.T) {
	e := setupJobLiveDB(t, 0)
	u := e.seedJobUser(t)
	workerID := e.seedWorkerRow(t, u, false, nil, jobCap, jobFilesCap)
	wkr := jobWorker(workerID, u)
	runID := e.seedRawJob(t, u, "running", &workerID, time.Hour, 60)
	if n, err := e.svc.FailJobsPastWallDeadline(e.ctx, time.Hour); err != nil || n != 1 {
		t.Fatalf("FailJobsPastWallDeadline = %d, %v; want 1", n, err)
	}
	if err := e.svc.SubmitJobResult(e.ctx, wkr, runID, 1, JobResultSubmission{Status: "completed", ReportMD: "late"}); !errors.Is(err, ErrRunTerminal) {
		t.Fatalf("late result: err = %v, want ErrRunTerminal", err)
	}
	if _, _, err := e.svc.SetState(e.ctx, wkr, runID, StateRequest{State: "completed", ClaimGeneration: i64Ptr(1)}); err != nil {
		t.Fatalf("late completed report: %v", err)
	}
	run := mustRun(t, e.codexTestEnv, runID)
	if run.Status != "failed" || run.FailOrigin.String != "run_timeout" {
		t.Fatalf("timed-out job = %s / %q; want it to stay failed / run_timeout", run.Status, run.FailOrigin.String)
	}
	if n := e.countRows(t, "job_results", runID); n != 0 {
		t.Fatalf("job_results rows = %d, want 0", n)
	}
}

// TestJobLiveStatusGuardLiveDB: FailJobRunWithoutResult and SetRunCompleted agree on which job
// rows a worker report can move: only claimed or running, never queued (a requeued job whose old
// flight reports late) or paused.
func TestJobLiveStatusGuardLiveDB(t *testing.T) {
	e := setupJobLiveDB(t, 0)
	u := e.seedJobUser(t)
	workerID := e.seedWorkerRow(t, u, false, nil, jobCap, jobFilesCap)
	for _, status := range []string{"queued", "paused"} {
		runID := e.seedRawJob(t, u, status, &workerID, 0, 600)
		n, err := e.q.FailJobRunWithoutResult(e.ctx, store.FailJobRunWithoutResultParams{ID: runID, WorkerID: pgconv.UUID(workerID)})
		if err != nil || n != 0 {
			t.Fatalf("FailJobRunWithoutResult on a %s job = %d, %v; want 0 rows", status, n, err)
		}
		n, err = e.q.SetRunCompleted(e.ctx, store.SetRunCompletedParams{ID: runID, WorkerID: pgconv.UUID(workerID)})
		if err != nil || n != 0 {
			t.Fatalf("SetRunCompleted on a %s job = %d, %v; want 0 rows", status, n, err)
		}
		if got := e.status(t, runID); got != status {
			t.Fatalf("a %s job moved to %s", status, got)
		}
	}
}

// TestJobReclaimClearsEarlierResultLiveDB: a requeued job that is claimed again starts with no
// result, so a result an earlier flight posted cannot satisfy the no-result invariant for a later
// flight that posts nothing.
//
// MUTATION CHECK: removing ClearJobResultForRun from assembleJobClaim leaves the earlier result in
// place, the second flight's bare `completed` succeeds, and this test goes red.
func TestJobReclaimClearsEarlierResultLiveDB(t *testing.T) {
	e := setupJobLiveDB(t, 0)
	u := e.seedJobUser(t)
	e.makeTokenDefault(t, u)
	v, err := e.svc.CreateJobRun(e.ctx, jobReq(cliCaller(u)))
	if err != nil {
		t.Fatal(err)
	}
	workerID := e.seedWorkerRow(t, u, false, nil, jobCap, jobFilesCap)
	wkr := store.Worker{ID: workerID, UserID: u, Name: "w", Status: "online", ProtocolCapabilities: []string{jobCap, jobFilesCap}}

	pl, err := e.svc.Claim(e.ctx, wkr, nil)
	if err != nil || pl == nil || pl.ClaimGeneration != 1 {
		t.Fatalf("first claim = %+v, %v", pl, err)
	}
	e.exec(`UPDATE runs SET status = 'running', started_at = now() WHERE id = $1`, v.ID)
	if err := e.svc.SubmitJobResult(e.ctx, wkr, v.ID, 1, JobResultSubmission{Status: "completed", ReportMD: "flight one", Findings: []JobFindingSubmission{{Severity: "info", MessageMD: "f"}}}); err != nil {
		t.Fatal(err)
	}
	// The worker dies: the run is requeued (RequeueWorkerRuns keeps worker affinity) and claimed again.
	e.exec(`UPDATE runs SET status = 'queued', claim_released_at = NULL WHERE id = $1`, v.ID)
	pl, err = e.svc.Claim(e.ctx, wkr, nil)
	if err != nil || pl == nil || pl.ClaimGeneration != 2 {
		t.Fatalf("second claim = %+v, %v; want generation 2", pl, err)
	}
	if n, f := e.countRows(t, "job_results", v.ID), e.countRows(t, "job_findings", v.ID); n != 0 || f != 0 {
		t.Fatalf("after the re-claim job_results = %d, job_findings = %d; want both cleared", n, f)
	}
	e.exec(`UPDATE runs SET status = 'running' WHERE id = $1`, v.ID)
	if _, _, err := e.svc.SetState(e.ctx, wkr, v.ID, StateRequest{State: "completed", ClaimGeneration: i64Ptr(2)}); err != nil {
		t.Fatal(err)
	}
	if run := mustRun(t, e.codexTestEnv, v.ID); run.Status != "failed" || run.FailOrigin.String != "job_no_result" {
		t.Fatalf("second flight without a result = %s / %q; want failed / job_no_result", run.Status, run.FailOrigin.String)
	}
}

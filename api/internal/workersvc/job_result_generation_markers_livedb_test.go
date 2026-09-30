package workersvc

import (
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
)

// PRD #1909 M4 rework 4: SubmitJobResult commits a generation_pending marker (a job_output_refusals
// row) for each file the server will generate, so a generation that never finishes is visible, not
// silent. Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.

func (e jfEnv) wantReasons(t *testing.T, run uuid.UUID, name string, want ...string) {
	t.Helper()
	got := e.refusalReasons(t, run, name)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("%s refusal rows = %v, want %v", name, got, want)
	}
}

// TestGenerationMarkersStoredClearsThemLiveDB: with the storage blocked the markers exist; once it
// completes they are gone and the files exist.
//
// MUTATION CHECK: skipping the marker insert in markGenerationPending turns the pending assertions
// red; skipping clearGenerated after the write leaves the pending rows and turns the last
// assertions red.
func TestGenerationMarkersStoredClearsThemLiveDB(t *testing.T) {
	e := newJFEnv(t, wide())
	e.svc.SetJobFiles(e.jf)
	e.svc.genReplyBound = 50 * time.Millisecond
	e.svc.genTimeout = 30 * time.Second
	u := e.seedJobUser(t)
	wkr, run := e.heldJob(t, u)
	release := holdStoredFiles(t, e, u)
	defer release()

	if err := e.svc.SubmitJobResult(e.ctx, wkr, run, 1, JobResultSubmission{Status: "completed", ReportMD: "# r\n"}); err != nil {
		t.Fatal(err)
	}
	e.wantReasons(t, run, "report.md", RefusalGenerationPending)
	e.wantReasons(t, run, "findings.json", RefusalGenerationPending)

	release()
	e.svc.WaitForGeneratedOutputs()
	if n := len(e.outputsNamed(t, run, "report.md")); n != 1 {
		t.Fatalf("report.md files = %d, want 1", n)
	}
	if n := len(e.outputsNamed(t, run, "findings.json")); n != 1 {
		t.Fatalf("findings.json files = %d, want 1", n)
	}
	if n := e.refusalCount(t, run); n != 0 {
		t.Fatalf("%d refusal rows after the files were stored, want none", n)
	}
}

// TestGenerationFailureReplacesMarkerLiveDB: a generation that fails turns its markers into
// generation_failed (one row per name, not a marker plus a failure).
func TestGenerationFailureReplacesMarkerLiveDB(t *testing.T) {
	e := newJFEnv(t, wide())
	e.svc.SetJobFiles(e.jf)
	// The hook runs on the generation goroutine, where t.Fatalf must not be called: it reports what
	// it saw through a channel.
	seen := make(chan []string, 1)
	e.svc.genHook = func(j *genJob) {
		var got []string
		rows, err := e.pool.Query(e.ctx, `SELECT reason FROM job_output_refusals WHERE run_id = $1 AND display_name = 'report.md'`, j.run.ID)
		if err != nil {
			got = []string{"query error: " + err.Error()}
		} else {
			for rows.Next() {
				var r string
				if err := rows.Scan(&r); err != nil {
					got = append(got, "scan error: "+err.Error())
					break
				}
				got = append(got, r)
			}
			rows.Close()
		}
		seen <- got
		panic("boom")
	}
	u := e.seedJobUser(t)
	wkr, run := e.heldJob(t, u)
	if err := e.svc.SubmitJobResult(e.ctx, wkr, run, 1, JobResultSubmission{Status: "completed", ReportMD: "# r\n"}); err != nil {
		t.Fatal(err)
	}
	e.svc.WaitForGeneratedOutputs()
	if got := <-seen; !slices.Equal(got, []string{RefusalGenerationPending}) {
		t.Fatalf("report.md rows when the generation started = %v, want the pending marker committed before it", got)
	}
	e.wantReasons(t, run, "report.md", RefusalGenerationFailed)
	e.wantReasons(t, run, "findings.json", RefusalGenerationFailed)
}

// TestAbandonedGenerationLeavesPendingMarkerLiveDB: a generation the drain gives up on (here one
// that never gets to run) leaves generation_pending behind, which is what a SIGKILL or crash leaves
// too: the marker was committed with the result.
//
// MUTATION CHECK: skipping the marker insert in markGenerationPending leaves no row here.
func TestAbandonedGenerationLeavesPendingMarkerLiveDB(t *testing.T) {
	e := newJFEnv(t, wide())
	e.svc.SetJobFiles(e.jf)
	e.svc.genReplyBound = 20 * time.Millisecond
	e.svc.genDrainBound = 50 * time.Millisecond
	e.svc.genCancelGrace = 20 * time.Millisecond
	stall := make(chan struct{})
	e.svc.genHook = func(*genJob) { <-stall }
	u := e.seedJobUser(t)
	wkr, run := e.heldJob(t, u)
	if err := e.svc.SubmitJobResult(e.ctx, wkr, run, 1, JobResultSubmission{Status: "completed", ReportMD: "# r\n"}); err != nil {
		t.Fatal(err)
	}
	if e.svc.DrainGeneratedOutputs() {
		t.Fatal("drain reported done with a generation still stalled")
	}
	e.wantReasons(t, run, "report.md", RefusalGenerationPending)
	e.wantReasons(t, run, "findings.json", RefusalGenerationPending)
	if n := len(e.outputsNamed(t, run, "report.md")); n != 0 {
		t.Fatal("a file was stored by the abandoned generation")
	}
	close(stall)
	e.svc.WaitForGeneratedOutputs()
}

// TestRepostRewritesMarkersLiveDB: a re-post clears the generation-owned rows of the earlier post
// (a stale failure here) and writes its own markers, only for the names it generates, while the
// worker's own rows under a reserved name are left alone.
func TestRepostRewritesMarkersLiveDB(t *testing.T) {
	e := newJFEnv(t, wide())
	e.svc.SetJobFiles(e.jf)
	e.svc.genReplyBound = 20 * time.Millisecond
	stall := make(chan struct{})
	e.svc.genHook = func(*genJob) { <-stall }
	u := e.seedJobUser(t)
	wkr, run := e.heldJob(t, u)
	e.exec(`INSERT INTO job_output_refusals (run_id, display_name, byte_size, reason, post_id) VALUES ($1, 'report.md', 0, 'generation_failed', 1)`, run)
	e.exec(`INSERT INTO job_output_refusals (run_id, display_name, byte_size, reason) VALUES ($1, 'report.md', 7, 'worker_too_large')`, run)

	if err := e.svc.SubmitJobResult(e.ctx, wkr, run, 1, JobResultSubmission{Status: "completed", ReportMD: "# r\n"}); err != nil {
		t.Fatal(err)
	}
	e.wantReasons(t, run, "report.md", RefusalGenerationPending, WorkerRefusalTooLarge)
	// A second post with an empty report no longer generates report.md: its marker goes.
	if err := e.svc.SubmitJobResult(e.ctx, wkr, run, 1, JobResultSubmission{Status: "completed"}); err != nil {
		t.Fatal(err)
	}
	e.wantReasons(t, run, "report.md", WorkerRefusalTooLarge)
	e.wantReasons(t, run, "findings.json", RefusalGenerationPending)
	close(stall)
	e.svc.WaitForGeneratedOutputs()
}

// TestGenerationMarkersSurviveFullRefusalCapLiveDB: the markers are written even when the run's
// refusal rows already fill OutputsMaxFiles, and they do not take a slot of the cap.
//
// MUTATION CHECK (run): bounding the marker insert by the refusal count again
// (InsertGeneratedOutputMarker guarded by a count of every row of the run below 3) inserts no
// marker, so SubmitJobResult fails with ErrStaleClaim and this goes red.
func TestGenerationMarkersSurviveFullRefusalCapLiveDB(t *testing.T) {
	l := wide()
	l.OutputsMaxFiles = 3
	e := newJFEnv(t, l)
	e.svc.SetJobFiles(e.jf)
	e.svc.genReplyBound = 20 * time.Millisecond
	stall := make(chan struct{})
	e.svc.genHook = func(*genJob) { <-stall }
	u := e.seedJobUser(t)
	wkr, run := e.heldJob(t, u)
	e.exec(`INSERT INTO job_output_refusals (run_id, display_name, byte_size, reason) VALUES ($1, 'a.bin', 1, 'file_too_large'), ($1, 'b.bin', 1, 'file_too_large'), ($1, 'c.bin', 1, 'file_too_large')`, run)

	if err := e.svc.SubmitJobResult(e.ctx, wkr, run, 1, JobResultSubmission{Status: "completed", ReportMD: "# r\n"}); err != nil {
		t.Fatal(err)
	}
	e.wantReasons(t, run, "report.md", RefusalGenerationPending)
	e.wantReasons(t, run, "findings.json", RefusalGenerationPending)
	if n := e.refusalCount(t, run); n != 5 {
		t.Fatalf("refusal rows = %d, want the 3 earlier refusals plus the 2 markers", n)
	}
	close(stall)
	e.svc.WaitForGeneratedOutputs()
	if n := e.refusalCount(t, run); n != 3 {
		t.Fatalf("refusal rows after the files were stored = %d, want the 3 earlier refusals", n)
	}
}

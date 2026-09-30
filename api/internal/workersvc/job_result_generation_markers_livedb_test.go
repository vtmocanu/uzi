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
	sawPending := false
	e.svc.genHook = func(j *genJob) {
		got := e.refusalReasons(t, j.run.ID, "report.md")
		sawPending = slices.Equal(got, []string{RefusalGenerationPending})
		panic("boom")
	}
	u := e.seedJobUser(t)
	wkr, run := e.heldJob(t, u)
	if err := e.svc.SubmitJobResult(e.ctx, wkr, run, 1, JobResultSubmission{Status: "completed", ReportMD: "# r\n"}); err != nil {
		t.Fatal(err)
	}
	e.svc.WaitForGeneratedOutputs()
	if !sawPending {
		t.Fatal("the marker was not committed before the generation started")
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
	e.exec(`INSERT INTO job_output_refusals (run_id, display_name, byte_size, reason) VALUES ($1, 'report.md', 0, 'generation_failed'), ($1, 'report.md', 7, 'worker_too_large')`, run)

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

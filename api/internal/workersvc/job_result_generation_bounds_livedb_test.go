package workersvc

import (
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1909 M4 rework 3: the generated-output storage is bounded per run and process-wide, drained
// at shutdown, panic-safe, and the worker-reported refusals do not depend on it. Skipped unless
// UZI_TEST_DATABASE_URL points at a throwaway Postgres.

func holdStoredFiles(t *testing.T, e jfEnv, owner uuid.UUID) func() {
	t.Helper()
	tx, err := e.pool.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.LockStoredFiles(e.ctx, tx, owner); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	return func() { once.Do(func() { _ = tx.Rollback(e.ctx) }) }
}

func (e jfEnv) reportContent(t *testing.T, run, owner uuid.UUID) string {
	t.Helper()
	ids := e.outputsNamed(t, run, "report.md")
	if len(ids) != 1 {
		t.Fatalf("report.md rows = %d, want exactly 1", len(ids))
	}
	return string(e.outputBytes(t, ids[0], owner))
}

// TestGenerationPerRunLatestWinsLiveDB: with the first generation of a run blocked, further posts
// with different content queue as ONE pending slot and each replaces the previous one; once the
// lock clears the run has exactly one report.md and it holds the latest content. The superseded
// posts leave no refusal rows.
//
// MUTATION CHECK: replacing the pending slot with a goroutine per post (no per-run bound) lets the
// superseded content be stored last or leaves two report.md rows, and turns this red.
func TestGenerationPerRunLatestWinsLiveDB(t *testing.T) {
	e := newJFEnv(t, wide())
	e.svc.SetJobFiles(e.jf)
	e.svc.genReplyBound = 50 * time.Millisecond
	e.svc.genTimeout = 30 * time.Second
	u := e.seedJobUser(t)
	wkr, run := e.heldJob(t, u)
	release := holdStoredFiles(t, e, u)
	defer release()

	for _, body := range []string{"# A\n", "# B\n", "# C\n"} {
		if err := e.svc.SubmitJobResult(e.ctx, wkr, run, 1, JobResultSubmission{Status: "completed", ReportMD: body}); err != nil {
			t.Fatal(err)
		}
	}
	e.svc.genMu.Lock()
	st := e.svc.genRuns[run]
	bounded := st != nil && st.pending != nil && len(e.svc.genRuns) == 1
	e.svc.genMu.Unlock()
	if !bounded {
		t.Fatal("want one running generation with one pending slot for the run")
	}
	release()
	e.svc.WaitForGeneratedOutputs()
	if got := e.reportContent(t, run, u); got != "# C\n" {
		t.Fatalf("report.md = %q, want the latest post", got)
	}
	if n := e.refusalCount(t, run); n != 0 {
		t.Fatalf("%d refusal rows, want none (superseded posts are not refusals)", n)
	}
	e.svc.genMu.Lock()
	left := len(e.svc.genRuns)
	e.svc.genMu.Unlock()
	if left != 0 {
		t.Fatalf("%d runs still tracked after the drain", left)
	}
}

// TestGenerationProcessWideCapLiveDB: with the process-wide cap full, a post for another run is
// recorded generation_failed for every name it would have generated, and nothing is spawned.
func TestGenerationProcessWideCapLiveDB(t *testing.T) {
	e := newJFEnv(t, wide())
	e.svc.SetJobFiles(e.jf)
	e.svc.genMax = 1
	e.svc.genReplyBound = 50 * time.Millisecond
	e.svc.genTimeout = 30 * time.Second
	u := e.seedJobUser(t)
	w1, r1 := e.heldJob(t, u)
	w2, r2 := e.heldJob(t, u)
	release := holdStoredFiles(t, e, u)
	defer release()

	sub := JobResultSubmission{Status: "completed", ReportMD: "# r\n"}
	if err := e.svc.SubmitJobResult(e.ctx, w1, r1, 1, sub); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.SubmitJobResult(e.ctx, w2, r2, 1, sub); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"report.md", "findings.json"} {
		if got := e.refusalReasons(t, r2, name); len(got) != 1 || got[0] != RefusalGenerationFailed {
			t.Fatalf("%s refusals of the capped run = %v, want [%s]", name, got, RefusalGenerationFailed)
		}
	}
	release()
	e.svc.WaitForGeneratedOutputs()
	if n := len(e.outputsNamed(t, r1, "report.md")); n != 1 {
		t.Fatalf("the admitted run stored %d report.md, want 1", n)
	}
}

// TestDrainGeneratedOutputsOrderingLiveDB: DrainGeneratedOutputs returns only after the in-flight
// generation finished (its files are stored), and after it a new post records generation_failed
// instead of spawning anything. This is the ordering main.go relies on (drain after the HTTP
// server, before the pool closes).
//
// MUTATION CHECK: dropping the DrainGeneratedOutputs call from shutdown, or making it not wait,
// turns the in-flight assertion red; dropping the genClosed refusal turns the second half red.
func TestDrainGeneratedOutputsOrderingLiveDB(t *testing.T) {
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
	go func() { time.Sleep(300 * time.Millisecond); release() }()
	if !e.svc.DrainGeneratedOutputs() {
		t.Fatal("drain gave up with a generation that finishes well inside its bound")
	}
	if n := len(e.outputsNamed(t, run, "report.md")); n != 1 {
		t.Fatalf("report.md files after the drain = %d, want 1 (the drain must wait for the in-flight storage)", n)
	}

	// After the drain a new post is refused and recorded, not started.
	wkr2, run2 := e.heldJob(t, u)
	if err := e.svc.SubmitJobResult(e.ctx, wkr2, run2, 1, JobResultSubmission{Status: "completed", ReportMD: "# r\n"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"report.md", "findings.json"} {
		if got := e.refusalReasons(t, run2, name); len(got) != 1 || got[0] != RefusalGenerationFailed {
			t.Fatalf("%s refusals after shutdown = %v, want [%s]", name, got, RefusalGenerationFailed)
		}
	}
	if n := len(e.outputsNamed(t, run2, "report.md")); n != 0 {
		t.Fatal("a generation started after shutdown began")
	}
}

// TestDrainGeneratedOutputsGivesUpAtItsBoundLiveDB: a generation stalled past the drain bound makes
// the drain return false instead of hanging shutdown.
func TestDrainGeneratedOutputsGivesUpAtItsBoundLiveDB(t *testing.T) {
	e := newJFEnv(t, wide())
	e.svc.SetJobFiles(e.jf)
	e.svc.genReplyBound = 20 * time.Millisecond
	e.svc.genTimeout = 50 * time.Millisecond
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
	close(stall)
	e.svc.WaitForGeneratedOutputs()
}

// TestGenerationPanicIsRecoveredAndRecordedLiveDB: a panic in a detached generation is recovered
// (the process survives), logged and recorded generation_failed for every generated name, and the
// run's state is released.
//
// MUTATION CHECK: removing the recover in runGeneration crashes the test binary.
func TestGenerationPanicIsRecoveredAndRecordedLiveDB(t *testing.T) {
	e := newJFEnv(t, wide())
	e.svc.SetJobFiles(e.jf)
	e.svc.genHook = func(*genJob) { panic("boom") }
	u := e.seedJobUser(t)
	wkr, run := e.heldJob(t, u)
	if err := e.svc.SubmitJobResult(e.ctx, wkr, run, 1, JobResultSubmission{Status: "completed", ReportMD: "# r\n"}); err != nil {
		t.Fatal(err)
	}
	e.svc.WaitForGeneratedOutputs()
	for _, name := range []string{"report.md", "findings.json"} {
		if got := e.refusalReasons(t, run, name); len(got) != 1 || got[0] != RefusalGenerationFailed {
			t.Fatalf("%s refusals = %v, want [%s]", name, got, RefusalGenerationFailed)
		}
	}
	e.svc.genMu.Lock()
	left := len(e.svc.genRuns)
	e.svc.genMu.Unlock()
	if left != 0 {
		t.Fatalf("%d runs still tracked after the panic", left)
	}
}

// TestRefusedOutputsRecordedIndependentlyOfGenerationLiveDB: the worker-reported drops are recorded
// by the result transaction even when no generation runs (shutdown), and no refusal is recorded for
// a report.md that would not have been generated (empty report).
//
// MUTATION CHECK: moving the RefusedOutputs loop back into the detached generation leaves the
// worker-reported row missing here.
func TestRefusedOutputsRecordedIndependentlyOfGenerationLiveDB(t *testing.T) {
	e := newJFEnv(t, wide())
	e.svc.SetJobFiles(e.jf)
	e.svc.DrainGeneratedOutputs()
	u := e.seedJobUser(t)
	wkr, run := e.heldJob(t, u)
	sub := JobResultSubmission{Status: "completed", RefusedOutputs: []JobRefusedOutput{{DisplayName: "big.bin", Reason: WorkerRefusalTooLarge}}}
	if err := e.svc.SubmitJobResult(e.ctx, wkr, run, 1, sub); err != nil {
		t.Fatal(err)
	}
	if got := e.refusalReasons(t, run, "big.bin"); len(got) != 1 || got[0] != WorkerRefusalTooLarge {
		t.Fatalf("big.bin refusals = %v, want [%s]", got, WorkerRefusalTooLarge)
	}
	if got := e.refusalReasons(t, run, "report.md"); len(got) != 0 {
		t.Fatalf("report.md refusals = %v, want none for an empty report", got)
	}
	if got := e.refusalReasons(t, run, "findings.json"); len(got) != 1 || got[0] != RefusalGenerationFailed {
		t.Fatalf("findings.json refusals = %v, want [%s]", got, RefusalGenerationFailed)
	}
}

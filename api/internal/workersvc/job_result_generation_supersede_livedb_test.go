package workersvc

import (
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1909 M4 review rework: a superseded post must neither clear or settle a newer post's
// markers nor store its own file after the newer post took over. Skipped unless
// UZI_TEST_DATABASE_URL points at a throwaway Postgres.

// TestOlderGenerationDoesNotClearNewerPostMarkersLiveDB: post A's storage is blocked on the
// stored-files lock, post B commits and waits behind it with B's own generation stalled, then A
// finishes. A's clear must not remove B's generation_pending markers: when the drain then gives up
// on B, both markers are still there, so the missing files stay visible.
//
// Run against the pre-fix logic (markers matched by run, name and claim generation only) this
// fails: A's clearGenerated deleted B's markers and the wantReasons assertions below see no rows.
// Run against the fix (markers owned by post_id) it passes.
func TestOlderGenerationDoesNotClearNewerPostMarkersLiveDB(t *testing.T) {
	e := newJFEnv(t, wide())
	e.svc.SetJobFiles(e.jf)
	e.svc.genReplyBound = 50 * time.Millisecond
	e.svc.genTimeout = 30 * time.Second
	e.svc.genDrainBound = 50 * time.Millisecond
	e.svc.genCancelGrace = 20 * time.Millisecond
	stallB := make(chan struct{})
	bStarted := make(chan struct{})
	e.svc.genHook = func(j *genJob) {
		if j.sub.ReportMD == "# B\n" {
			close(bStarted)
			<-stallB
		}
	}
	u := e.seedJobUser(t)
	wkr, run := e.heldJob(t, u)
	release := holdStoredFiles(t, e, u)
	defer release()

	for _, body := range []string{"# A\n", "# B\n"} {
		if err := e.svc.SubmitJobResult(e.ctx, wkr, run, 1, JobResultSubmission{Status: "completed", ReportMD: body}); err != nil {
			t.Fatal(err)
		}
	}
	release() // A now stores; B's generation starts right after A's returns, and stalls.
	select {
	case <-bStarted:
	case <-time.After(20 * time.Second):
		t.Fatal("B's generation never started after A finished")
	}
	if e.svc.DrainGeneratedOutputs() {
		t.Fatal("drain reported done with B's generation still stalled")
	}
	e.wantReasons(t, run, "report.md", RefusalGenerationPending)
	e.wantReasons(t, run, "findings.json", RefusalGenerationPending)
	close(stallB)
	e.svc.WaitForGeneratedOutputs()
}

// TestOlderPostCommittedFirstDoesNotStoreAfterNewerLiveDB: "latest wins" follows the COMMIT order,
// not the order the posts reach the generation queue. Post X commits first but reaches the queue
// only after the newer post Y has been stored: X's generation finds its markers gone (Y's commit
// deleted them) and stores nothing, so report.md keeps Y's content.
//
// MUTATION CHECK (run): removing the ownsGenerated check at the top of storeGeneratedOutput makes
// X's generation replace Y's report.md with X's and turns the content assertion red.
func TestOlderPostCommittedFirstDoesNotStoreAfterNewerLiveDB(t *testing.T) {
	e := newJFEnv(t, wide())
	e.svc.SetJobFiles(e.jf)
	e.svc.genReplyBound = 5 * time.Second
	e.svc.genTimeout = 30 * time.Second
	xCommitted := make(chan struct{})
	releaseX := make(chan struct{})
	e.svc.genPostHook = func(sub JobResultSubmission) {
		if sub.ReportMD == "# X\n" {
			close(xCommitted)
			<-releaseX
		}
	}
	u := e.seedJobUser(t)
	wkr, run := e.heldJob(t, u)

	xDone := make(chan error, 1)
	go func() {
		xDone <- e.svc.SubmitJobResult(e.ctx, wkr, run, 1, JobResultSubmission{Status: "completed", ReportMD: "# X\n"})
	}()
	<-xCommitted
	if err := e.svc.SubmitJobResult(e.ctx, wkr, run, 1, JobResultSubmission{Status: "completed", ReportMD: "# Y\n"}); err != nil {
		t.Fatal(err)
	}
	e.svc.WaitForGeneratedOutputs()
	if got := e.reportContent(t, run, u); got != "# Y\n" {
		t.Fatalf("report.md = %q before X queued, want Y's", got)
	}
	close(releaseX)
	if err := <-xDone; err != nil {
		t.Fatal(err)
	}
	e.svc.WaitForGeneratedOutputs()
	if got := e.reportContent(t, run, u); got != "# Y\n" {
		t.Fatalf("report.md = %q after the older post's generation ran, want the newer post's content", got)
	}
	if n := e.refusalCount(t, run); n != 0 {
		t.Fatalf("%d refusal rows, want none", n)
	}
}

// TestPendingGenerationAfterDrainStartedIsRecordedNotRunLiveDB: a job waiting behind a running
// generation when the drain starts is recorded generation_failed without running.
//
// MUTATION CHECK (run): making runGenerations ignore its closed flag (always calling runGeneration)
// runs the second post's generation, which the hook records, and turns the assertions red.
func TestPendingGenerationAfterDrainStartedIsRecordedNotRunLiveDB(t *testing.T) {
	e := newJFEnv(t, wide())
	e.svc.SetJobFiles(e.jf)
	e.svc.genReplyBound = 20 * time.Millisecond
	e.svc.genTimeout = 30 * time.Second
	var mu sync.Mutex
	var ran []string
	stallA := make(chan struct{})
	e.svc.genHook = func(j *genJob) {
		mu.Lock()
		ran = append(ran, j.sub.ReportMD)
		mu.Unlock()
		if j.sub.ReportMD == "# A\n" {
			<-stallA
		}
	}
	u := e.seedJobUser(t)
	wkr, run := e.heldJob(t, u)
	for _, body := range []string{"# A\n", "# B\n"} {
		if err := e.svc.SubmitJobResult(e.ctx, wkr, run, 1, JobResultSubmission{Status: "completed", ReportMD: body}); err != nil {
			t.Fatal(err)
		}
	}
	drained := make(chan bool, 1)
	go func() { drained <- e.svc.DrainGeneratedOutputs() }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		e.svc.genMu.Lock()
		closed := e.svc.genClosed
		e.svc.genMu.Unlock()
		if closed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the drain never started")
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(stallA)
	select {
	case ok := <-drained:
		if !ok {
			t.Fatal("drain gave up")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("drain did not return")
	}
	mu.Lock()
	got := append([]string(nil), ran...)
	mu.Unlock()
	if len(got) != 1 || got[0] != "# A\n" {
		t.Fatalf("generations that ran = %q, want only the first (the pending one is refused after the drain started)", got)
	}
	e.wantReasons(t, run, "report.md", RefusalGenerationFailed)
	e.wantReasons(t, run, "findings.json", RefusalGenerationFailed)
}

// TestSupersededPostReturnsPromptlyLiveDB: the caller of a post that a newer post replaced in the
// pending slot does not wait out the reply bound.
//
// MUTATION CHECK (run): removing close(st.pending.done) in startJobResultOutputs leaves the
// replaced post's caller waiting the full reply bound and turns this red.
func TestSupersededPostReturnsPromptlyLiveDB(t *testing.T) {
	e := newJFEnv(t, wide())
	e.svc.SetJobFiles(e.jf)
	e.svc.genReplyBound = 20 * time.Second
	e.svc.genTimeout = 30 * time.Second
	stallA := make(chan struct{})
	e.svc.genHook = func(j *genJob) {
		if j.sub.ReportMD == "# A\n" {
			<-stallA
		}
	}
	u := e.seedJobUser(t)
	wkr, run := e.heldJob(t, u)
	post := func(body string) chan time.Duration {
		d := make(chan time.Duration, 1)
		start := time.Now()
		go func() {
			_ = e.svc.SubmitJobResult(e.ctx, wkr, run, 1, JobResultSubmission{Status: "completed", ReportMD: body})
			d <- time.Since(start)
		}()
		return d
	}
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		for i := 0; i < 2000; i++ {
			e.svc.genMu.Lock()
			ok := cond()
			e.svc.genMu.Unlock()
			if ok {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s", what)
	}
	aDone := post("# A\n")
	waitFor("A running", func() bool { return e.svc.genRuns[run] != nil })
	bDone := post("# B\n")
	waitFor("B pending", func() bool { st := e.svc.genRuns[run]; return st != nil && st.pending != nil })
	cDone := post("# C\n")
	select {
	case d := <-bDone:
		if d > 5*time.Second {
			t.Fatalf("the superseded post returned after %s, want promptly", d)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the superseded post's caller is still waiting for its reply bound")
	}
	close(stallA)
	<-aDone
	<-cDone
	e.svc.WaitForGeneratedOutputs()
}

// TestGenerationPerOwnerShareLiveDB: one owner's runs cannot hold every slot of the process-wide
// cap. With a share of 1, the owner's second run is recorded generation_failed while another
// owner's run is still admitted under the same cap.
//
// MUTATION CHECK (run): removing the genOwners case in startJobResultOutputs admits the second run
// of the first owner and turns the failed assertion red.
func TestGenerationPerOwnerShareLiveDB(t *testing.T) {
	l := wide()
	l.MaxConcurrentWritesPerOwner = 1
	e := newJFEnv(t, l)
	e.svc.SetJobFiles(e.jf)
	e.svc.genMax = 3
	e.svc.genReplyBound = 20 * time.Millisecond
	e.svc.genTimeout = 30 * time.Second
	stall := make(chan struct{})
	e.svc.genHook = func(*genJob) { <-stall }
	u1 := e.seedJobUser(t)
	u2 := e.seedJobUser(t)
	w1, r1 := e.heldJob(t, u1)
	w2, r2 := e.heldJob(t, u1)
	w3, r3 := e.heldJob(t, u2)
	sub := JobResultSubmission{Status: "completed", ReportMD: "# r\n"}
	for _, c := range []struct {
		w store.Worker
		r uuid.UUID
	}{{w1, r1}, {w2, r2}, {w3, r3}} {
		if err := e.svc.SubmitJobResult(e.ctx, c.w, c.r, 1, sub); err != nil {
			t.Fatal(err)
		}
	}
	e.wantReasons(t, r1, "report.md", RefusalGenerationPending)
	e.wantReasons(t, r2, "report.md", RefusalGenerationFailed)
	e.wantReasons(t, r2, "findings.json", RefusalGenerationFailed)
	e.wantReasons(t, r3, "report.md", RefusalGenerationPending)
	close(stall)
	e.svc.WaitForGeneratedOutputs()
	e.svc.genMu.Lock()
	left := len(e.svc.genOwners)
	e.svc.genMu.Unlock()
	if left != 0 {
		t.Fatalf("%d owners still counted after the generations finished", left)
	}
}

// TestDrainCancelsGenerationsAtItsBoundLiveDB: a generation blocked on the stored-files lock is
// cancelled when the drain bound elapses, so shutdown waits the drain bound plus the cancel grace
// and not the storage deadline. The cut-off file is recorded generation_failed (shutdown, not its
// own deadline).
//
// MUTATION CHECK (run): deriving the generation context from context.Background() instead of
// genBaseCtx leaves the generation blocked for the 60 s storage deadline and turns the wait
// assertion red.
func TestDrainCancelsGenerationsAtItsBoundLiveDB(t *testing.T) {
	e := newJFEnv(t, wide())
	e.svc.SetJobFiles(e.jf)
	e.svc.genReplyBound = 20 * time.Millisecond
	e.svc.genTimeout = 60 * time.Second
	e.svc.genDrainBound = 100 * time.Millisecond
	e.svc.genCancelGrace = 5 * time.Second
	u := e.seedJobUser(t)
	wkr, run := e.heldJob(t, u)
	release := holdStoredFiles(t, e, u)
	defer release()
	if err := e.svc.SubmitJobResult(e.ctx, wkr, run, 1, JobResultSubmission{Status: "completed", ReportMD: "# r\n"}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if e.svc.DrainGeneratedOutputs() {
		t.Fatal("drain reported a clean finish with the storage blocked")
	}
	if d := time.Since(start); d > 4*time.Second {
		t.Fatalf("drain took %s, want the drain bound plus a short wait for the cancelled generation", d)
	}
	e.wantReasons(t, run, "report.md", RefusalGenerationFailed)
	e.wantReasons(t, run, "findings.json", RefusalGenerationFailed)
}

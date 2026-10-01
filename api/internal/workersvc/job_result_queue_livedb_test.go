package workersvc

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestThirdOwnerGenerationQueuesAndStoresLiveDB(t *testing.T) {
	l := wide()
	l.MaxConcurrentWrites = 8
	l.MaxConcurrentWritesPerOwner = 2
	e := newJFEnv(t, l)
	e.svc.SetJobFiles(e.jf)
	e.svc.genReplyBound = 5 * time.Millisecond
	u := e.seedJobUser(t)
	workers := make([]store.Worker, 3)
	runs := make([]uuid.UUID, 3)
	for i := range runs {
		workers[i], runs[i] = e.heldJob(t, u)
	}
	gate := make(chan struct{})
	started := make(chan uuid.UUID, 3)
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	defer func() { release(); e.svc.WaitForGeneratedOutputs() }()
	e.svc.genHook = func(job *genJob) {
		started <- job.run.ID
		if job.run.ID == runs[0] || job.run.ID == runs[1] {
			<-gate
		}
	}
	for i := 0; i < 2; i++ {
		if err := e.svc.SubmitJobResult(e.ctx, workers[i], runs[i], 1,
			JobResultSubmission{Status: "completed", ReportMD: "# report"}); err != nil {
			t.Fatal(err)
		}
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatal("the first two owner writes did not start")
		}
	}
	if err := e.svc.SubmitJobResult(e.ctx, workers[2], runs[2], 1,
		JobResultSubmission{Status: "completed", ReportMD: "# third"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range generatedOutputNames {
		e.wantReasons(t, runs[2], name, RefusalGenerationPending)
	}
	select {
	case id := <-started:
		t.Fatalf("queued generation %s started before an owner slot freed", id)
	default:
	}
	// The terminal can arrive before a write slot frees: completing the run must not
	// strand the committed result's queued files.
	if _, err := e.pool.Exec(e.ctx, `UPDATE runs SET status='completed',finished_at=now() WHERE id=$1`, runs[2]); err != nil {
		t.Fatal(err)
	}
	release()
	e.svc.WaitForGeneratedOutputs()
	if got := e.reportContent(t, runs[2], u); got != "# third" {
		t.Fatalf("third report = %q, want stored third result", got)
	}
	if got := len(e.outputsNamed(t, runs[2], JobOutputFindingsName)); got != 1 {
		t.Fatalf("third findings files = %d, want one", got)
	}
	if n := e.refusalCount(t, runs[2]); n != 0 {
		t.Fatalf("third job has %d refusal rows after storage", n)
	}
}

func TestFirstGenerationCannotEscapeShutdownCancellationLiveDB(t *testing.T) {
	e := newJFEnv(t, wide())
	e.svc.SetJobFiles(e.jf)
	e.svc.genReplyBound = 5 * time.Millisecond
	e.svc.genDrainBound = 10 * time.Millisecond
	e.svc.genCancelGrace = 10 * time.Millisecond
	entered := make(chan struct{})
	gate := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	defer func() { release(); e.svc.WaitForGeneratedOutputs() }()
	e.svc.genStartHook = func(*genJob) { close(entered); <-gate }
	u := e.seedJobUser(t)
	wkr, run := e.heldJob(t, u)
	if err := e.svc.SubmitJobResult(e.ctx, wkr, run, 1,
		JobResultSubmission{Status: "completed", ReportMD: "# late first write"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("first generation did not reach the pre-start seam")
	}
	if e.svc.DrainGeneratedOutputs() {
		t.Fatal("drain must time out while the first goroutine is held before starting")
	}
	e.svc.genMu.Lock()
	base := e.svc.genBase
	e.svc.genMu.Unlock()
	if base == nil || !errors.Is(context.Cause(base), errGenShutdown) {
		t.Fatal("shutdown did not cancel the context of the admitted first generation")
	}
	release()
	e.svc.WaitForGeneratedOutputs()
	if got := len(e.outputsNamed(t, run, JobOutputReportName)); got != 0 {
		t.Fatalf("cancelled first generation stored %d reports", got)
	}
}

func TestWaitingGenerationLatestWinsAndCapacityLiveDB(t *testing.T) {
	l := wide()
	l.MaxConcurrentWritesPerOwner = 1
	e := newJFEnv(t, l)
	e.svc.SetJobFiles(e.jf)
	e.svc.genMax = 1
	e.svc.genReplyBound = 5 * time.Millisecond
	u := e.seedJobUser(t)
	w1, r1 := e.heldJob(t, u)
	w2, r2 := e.heldJob(t, u)
	w3, r3 := e.heldJob(t, u)
	gate := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	defer func() { release(); e.svc.WaitForGeneratedOutputs() }()
	e.svc.genHook = func(j *genJob) {
		if j.run.ID == r1 {
			<-gate
		}
	}
	if err := e.svc.SubmitJobResult(e.ctx, w1, r1, 1, JobResultSubmission{Status: "completed", ReportMD: "# active"}); err != nil {
		t.Fatal(err)
	}
	for _, report := range []string{"# old", "# replaced", "# latest"} {
		if err := e.svc.SubmitJobResult(e.ctx, w2, r2, 1, JobResultSubmission{Status: "completed", ReportMD: report}); err != nil {
			t.Fatal(err)
		}
	}
	e.svc.genMu.Lock()
	bounded := len(e.svc.genQueue) == 1 && len(e.svc.genRuns) == 2 && e.svc.genRuns[r2].pending != nil && e.svc.genRuns[r2].pending.sub.ReportMD == ""
	e.svc.genMu.Unlock()
	if !bounded {
		t.Fatal("waiting posts must collapse to one metadata-only pending entry")
	}
	for _, name := range generatedOutputNames {
		e.wantReasons(t, r2, name, RefusalGenerationPending)
	}
	if err := e.svc.SubmitJobResult(e.ctx, w3, r3, 1, JobResultSubmission{Status: "completed", ReportMD: "# over capacity"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range generatedOutputNames {
		e.wantReasons(t, r3, name, RefusalGenerationFailed)
	}
	release()
	e.svc.WaitForGeneratedOutputs()
	if got := e.reportContent(t, r2, u); got != "# latest" {
		t.Fatalf("waiting report = %q, want latest", got)
	}
	if n := e.refusalCount(t, r2); n != 0 {
		t.Fatalf("waiting run retained %d markers", n)
	}
}

func TestWaitingGenerationOwnerFairnessLiveDB(t *testing.T) {
	l := wide()
	l.MaxConcurrentWritesPerOwner = 1
	e := newJFEnv(t, l)
	e.svc.SetJobFiles(e.jf)
	e.svc.genMax = 3
	e.svc.genReplyBound = 5 * time.Millisecond
	u1, u2 := e.seedJobUser(t), e.seedJobUser(t)
	gate := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	defer func() { release(); e.svc.WaitForGeneratedOutputs() }()
	e.svc.genHook = func(j *genJob) {
		if j.run.UserID == u1 {
			<-gate
		}
	}
	var run uuid.UUID
	for i := 0; i < 3; i++ {
		w, r := e.heldJob(t, u1)
		if err := e.svc.SubmitJobResult(e.ctx, w, r, 1, JobResultSubmission{Status: "completed", ReportMD: "# owner one"}); err != nil {
			t.Fatal(err)
		}
		if i == 1 {
			for _, name := range generatedOutputNames {
				e.wantReasons(t, r, name, RefusalGenerationPending)
			}
		}
		if i == 2 {
			for _, name := range generatedOutputNames {
				e.wantReasons(t, r, name, RefusalGenerationFailed)
			}
		}
	}
	w, run := e.heldJob(t, u2)
	if err := e.svc.SubmitJobResult(e.ctx, w, run, 1, JobResultSubmission{Status: "completed", ReportMD: "# owner two"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		var n int
		if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM job_files WHERE run_id=$1 AND display_name='report.md' AND state IN ('attached','available')`, run).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("waiting owner monopolized another owner's free active slot")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestWaitingGenerationActivationFenceLiveDB(t *testing.T) {
	for _, state := range []string{"cancelled", "deleted", "superseded"} {
		t.Run(state, func(t *testing.T) {
			l := wide()
			l.MaxConcurrentWritesPerOwner = 1
			e := newJFEnv(t, l)
			e.svc.SetJobFiles(e.jf)
			e.svc.genMax = 1
			e.svc.genReplyBound = 5 * time.Millisecond
			u := e.seedJobUser(t)
			w1, r1 := e.heldJob(t, u)
			w2, r2 := e.heldJob(t, u)
			gate := make(chan struct{})
			var once sync.Once
			release := func() { once.Do(func() { close(gate) }) }
			defer func() { release(); e.svc.WaitForGeneratedOutputs() }()
			e.svc.genHook = func(j *genJob) {
				if j.run.ID == r1 {
					<-gate
				}
			}
			for _, p := range []struct {
				w store.Worker
				r uuid.UUID
			}{{w1, r1}, {w2, r2}} {
				if err := e.svc.SubmitJobResult(e.ctx, p.w, p.r, 1, JobResultSubmission{Status: "completed", ReportMD: "# waiting"}); err != nil {
					t.Fatal(err)
				}
			}
			var err error
			switch state {
			case "cancelled":
				_, err = e.pool.Exec(e.ctx, `UPDATE runs SET status='cancelled',finished_at=now() WHERE id=$1`, r2)
			case "deleted":
				_, err = e.pool.Exec(e.ctx, `DELETE FROM runs WHERE id=$1`, r2)
			case "superseded":
				_, err = e.pool.Exec(e.ctx, `UPDATE runs SET claim_generation=2 WHERE id=$1`, r2)
			}
			if err != nil {
				t.Fatal(err)
			}
			release()
			e.svc.WaitForGeneratedOutputs()
			if n := len(e.outputsNamed(t, r2, JobOutputReportName)); n != 0 {
				t.Fatalf("%s waiting generation stored %d reports", state, n)
			}
		})
	}
}

func TestWaitingGenerationShutdownIsVisibleLiveDB(t *testing.T) {
	l := wide()
	l.MaxConcurrentWritesPerOwner = 1
	e := newJFEnv(t, l)
	e.svc.SetJobFiles(e.jf)
	e.svc.genMax = 1
	e.svc.genReplyBound = 5 * time.Millisecond
	e.svc.genDrainBound = 10 * time.Millisecond
	e.svc.genCancelGrace = 10 * time.Millisecond
	u := e.seedJobUser(t)
	w1, r1 := e.heldJob(t, u)
	w2, r2 := e.heldJob(t, u)
	gate := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	defer func() { release(); e.svc.WaitForGeneratedOutputs() }()
	e.svc.genHook = func(j *genJob) {
		if j.run.ID == r1 {
			<-gate
		}
		if j.run.ID == r2 {
			t.Error("waiting generation started after shutdown")
		}
	}
	for _, post := range []struct {
		w store.Worker
		r uuid.UUID
	}{{w1, r1}, {w2, r2}} {
		if err := e.svc.SubmitJobResult(e.ctx, post.w, post.r, 1, JobResultSubmission{Status: "completed", ReportMD: "# queued"}); err != nil {
			t.Fatal(err)
		}
	}
	if e.svc.DrainGeneratedOutputs() {
		t.Fatal("held active generation should exceed drain bound")
	}
	release()
	e.svc.WaitForGeneratedOutputs()
	for _, name := range generatedOutputNames {
		e.wantReasons(t, r2, name, RefusalGenerationShutdown)
	}
	if n := len(e.outputsNamed(t, r2, JobOutputReportName)); n != 0 {
		t.Fatalf("shutdown wrote %d queued reports", n)
	}
}

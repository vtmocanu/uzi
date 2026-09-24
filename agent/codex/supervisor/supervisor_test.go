package main

import (
	"bytes"
	"strings"
	"syscall"
	"testing"
	"time"

	"uzi.local/codex-supervisor/internal/safetree"
)

// scriptedReaper yields the scripted results in order, then fallback forever.
func scriptedReaper(fallback reapStep, steps ...reapStep) childReaper {
	i := 0
	return func() (int, error) {
		if i < len(steps) {
			s := steps[i]
			i++
			return s.pid, s.err
		}
		return fallback.pid, fallback.err
	}
}

var (
	reapEmpty   = reapStep{0, syscall.ECHILD} // tree empty: drained
	reapPending = reapStep{0, nil}            // nothing ready: never drains
)

// cleanupRecorder is a fake tmpCleanup seam. It records how many times it ran
// and how many evidence lines had been written when it did.
type cleanupRecorder struct {
	buf         *bytes.Buffer
	result      *tmpCleanupResult
	calls       int
	linesAtCall int
}

func (c *cleanupRecorder) seam() func() *tmpCleanupResult {
	return func() *tmpCleanupResult {
		c.calls++
		c.linesAtCall = strings.Count(c.buf.String(), "\n")
		return c.result
	}
}

// newTestSupervisor builds a supervisor over scripted control frames and a fake
// clock that advances 50ms per drain sleep, so an undrainable tree reaches its
// deadline without real waiting. The primary child never becomes ready.
func newTestSupervisor(frames string, reap childReaper) (*supervisor, *bytes.Buffer) {
	var buf bytes.Buffer
	now := time.Unix(0, 0)
	return &supervisor{
		ev:      &evidence{w: &buf},
		control: &controlReader{r: strings.NewReader(frames)},
		seams: seams{
			supervisorPid:  1,
			directChildren: func() []int { return nil },
			kill:           func(int) error { return nil },
			reap:           reap,
			reapChild:      func(int) (int, error) { return 0, nil },
			snapshot:       func() ([]procRow, error) { return nil, nil },
			now:            func() time.Time { return now },
			sleep:          func() { now = now.Add(50 * time.Millisecond) },
		},
	}, &buf
}

func tmpCleanupField(t *testing.T, m map[string]any) (map[string]any, bool) {
	t.Helper()
	raw, ok := m["tmpCleanup"]
	if !ok {
		return nil, false
	}
	tc, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("tmpCleanup = %v, want an object", raw)
	}
	return tc, true
}

func TestRunDrainedDisposeRemovesTmpBeforeReporting(t *testing.T) {
	sup, buf := newTestSupervisor(`{"op":"dispose","id":1}`+"\n", scriptedReaper(reapEmpty))
	rec := &cleanupRecorder{buf: buf, result: &tmpCleanupResult{State: tmpCleanupRemoved}}
	sup.seams.tmpCleanup = rec.seam()

	if code := sup.run(9, procStatus{}); code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	if rec.calls != 1 {
		t.Fatalf("cleanup calls = %d, want 1", rec.calls)
	}
	if rec.linesAtCall != 1 {
		t.Fatalf("cleanup ran after %d evidence lines, want 1 (started only, before dispose)", rec.linesAtCall)
	}
	lines := decodeLines(t, buf)
	last := lines[len(lines)-1]
	if last["event"] != "dispose" || last["state"] != stateDrained {
		t.Fatalf("last evidence = %v", last)
	}
	tc, ok := tmpCleanupField(t, last)
	if !ok || tc["state"] != "removed" || tc["reason"] != "" {
		t.Fatalf("tmpCleanup = %v, want removed with empty reason", tc)
	}
}

func TestRunDrainedDisposeRetainedReasonKeepsExitCode(t *testing.T) {
	sup, buf := newTestSupervisor(`{"op":"dispose","id":3}`+"\n", scriptedReaper(reapEmpty))
	ct := &commandTmp{remove: func(int, string, safetree.Pin) error { return safetree.ErrOwner }}
	sup.seams.tmpCleanup = tmpCleanupFor(ct)

	if code := sup.run(9, procStatus{}); code != 0 {
		t.Fatalf("code = %d, want 0 (cleanup never changes the exit code)", code)
	}
	lines := decodeLines(t, buf)
	tc, ok := tmpCleanupField(t, lines[len(lines)-1])
	if !ok || tc["state"] != "retained" || tc["reason"] != "owner" {
		t.Fatalf("tmpCleanup = %v, want retained/owner", tc)
	}
}

func TestRunNoTokenMeansNoTmpCleanupField(t *testing.T) {
	sup, buf := newTestSupervisor(`{"op":"dispose","id":1}`+"\n", scriptedReaper(reapEmpty))
	if code := sup.run(9, procStatus{}); code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	for _, m := range decodeLines(t, buf) {
		if _, ok := m["tmpCleanup"]; ok {
			t.Fatalf("event %v carries tmpCleanup without a token", m)
		}
	}
}

func TestRunAbnormalNotDrainedLeavesTmp(t *testing.T) {
	// Control EOF with a tree that never drains: the best-effort cleanup is
	// unconfirmed, so the tmp must not be touched.
	sup, buf := newTestSupervisor("", scriptedReaper(reapPending))
	rec := &cleanupRecorder{buf: buf, result: &tmpCleanupResult{State: tmpCleanupRemoved}}
	sup.seams.tmpCleanup = rec.seam()

	if code := sup.run(9, procStatus{}); code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if rec.calls != 0 {
		t.Fatalf("cleanup ran %d times after an unconfirmed drain", rec.calls)
	}
	lines := decodeLines(t, buf)
	last := lines[len(lines)-1]
	if last["event"] != "abnormal" {
		t.Fatalf("last evidence = %v", last)
	}
	if c, _ := last["cleanup"].(map[string]any); c["state"] != stateUnconfirmed {
		t.Fatalf("cleanup = %v, want unconfirmed", last["cleanup"])
	}
	if _, ok := tmpCleanupField(t, last); ok {
		t.Fatal("abnormal after an unconfirmed drain carries tmpCleanup")
	}
}

func TestRunAbnormalDrainedCleansTmp(t *testing.T) {
	sup, buf := newTestSupervisor("", scriptedReaper(reapEmpty))
	rec := &cleanupRecorder{buf: buf, result: &tmpCleanupResult{State: tmpCleanupRetained, Reason: "mismatch"}}
	sup.seams.tmpCleanup = rec.seam()

	if code := sup.run(9, procStatus{}); code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if rec.calls != 1 {
		t.Fatalf("cleanup calls = %d, want 1", rec.calls)
	}
	lines := decodeLines(t, buf)
	last := lines[len(lines)-1]
	if last["event"] != "abnormal" || last["reason"] != "control EOF" {
		t.Fatalf("last evidence = %v", last)
	}
	tc, ok := tmpCleanupField(t, last)
	if !ok || tc["state"] != "retained" || tc["reason"] != "mismatch" {
		t.Fatalf("tmpCleanup = %v, want retained/mismatch", tc)
	}
}

func TestRunUnconfirmedDisposeDoesNotCleanThenDrainedCleansOnce(t *testing.T) {
	// The first dispose (timeoutMs 0, nothing reapable) is unconfirmed; the
	// repeat reaches drained. Cleanup runs exactly once, on the drained one.
	frames := `{"op":"dispose","id":1,"timeoutMs":0}` + "\n" + `{"op":"dispose","id":2}` + "\n"
	sup, buf := newTestSupervisor(frames, scriptedReaper(reapEmpty, reapPending))
	rec := &cleanupRecorder{buf: buf, result: &tmpCleanupResult{State: tmpCleanupRemoved}}
	sup.seams.tmpCleanup = rec.seam()

	if code := sup.run(9, procStatus{}); code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	if rec.calls != 1 {
		t.Fatalf("cleanup calls = %d, want 1", rec.calls)
	}
	lines := decodeLines(t, buf)
	if len(lines) != 3 {
		t.Fatalf("evidence = %v", lines)
	}
	if lines[1]["state"] != stateUnconfirmed {
		t.Fatalf("first dispose = %v, want unconfirmed", lines[1])
	}
	if _, ok := tmpCleanupField(t, lines[1]); ok {
		t.Fatal("an unconfirmed dispose carries tmpCleanup")
	}
	if _, ok := tmpCleanupField(t, lines[2]); !ok {
		t.Fatal("the drained dispose lacks tmpCleanup")
	}
}

func TestCleanupTmpRunsAtMostOnce(t *testing.T) {
	sup, buf := newTestSupervisor("", scriptedReaper(reapEmpty))
	rec := &cleanupRecorder{buf: buf, result: &tmpCleanupResult{State: tmpCleanupRemoved}}
	sup.seams.tmpCleanup = rec.seam()
	if got := sup.cleanupTmp(); got == nil || got.State != tmpCleanupRemoved {
		t.Fatalf("first cleanupTmp = %+v", got)
	}
	if got := sup.cleanupTmp(); got != nil {
		t.Fatalf("repeat cleanupTmp = %+v, want nil (no re-clean, no field)", got)
	}
	if rec.calls != 1 {
		t.Fatalf("cleanup calls = %d, want 1", rec.calls)
	}
}

func failingWatch(int) (<-chan error, error) { return nil, syscall.EMFILE }

// TestRunWatchedWatchFailureNotDrainedLeavesTmp: the child-watch failure path
// goes through abnormal, so an unconfirmed drain never touches the tmp.
func TestRunWatchedWatchFailureNotDrainedLeavesTmp(t *testing.T) {
	sup, buf := newTestSupervisor("", scriptedReaper(reapPending))
	rec := &cleanupRecorder{buf: buf, result: &tmpCleanupResult{State: tmpCleanupRemoved}}
	sup.seams.tmpCleanup = rec.seam()

	if code := sup.runWatched(9, procStatus{}, failingWatch); code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if rec.calls != 0 {
		t.Fatalf("cleanup ran %d times after an unconfirmed drain", rec.calls)
	}
	lines := decodeLines(t, buf)
	if len(lines) != 1 || lines[0]["event"] != "abnormal" || lines[0]["reason"] != "child watch failed" {
		t.Fatalf("evidence = %v", lines)
	}
	if c, _ := lines[0]["cleanup"].(map[string]any); c["state"] != stateUnconfirmed {
		t.Fatalf("cleanup = %v, want unconfirmed", lines[0]["cleanup"])
	}
	if _, ok := tmpCleanupField(t, lines[0]); ok {
		t.Fatal("watch-failure abnormal after an unconfirmed drain carries tmpCleanup")
	}
}

// TestRunWatchedWatchFailureDrainedCleansTmpOnce: a drained watch-failure
// abnormal removes the tmp and reports it; the at-most-once guard then keeps a
// later abnormal from running it again.
func TestRunWatchedWatchFailureDrainedCleansTmpOnce(t *testing.T) {
	sup, buf := newTestSupervisor("", scriptedReaper(reapEmpty))
	rec := &cleanupRecorder{buf: buf, result: &tmpCleanupResult{State: tmpCleanupRetained, Reason: "owner"}}
	sup.seams.tmpCleanup = rec.seam()

	if code := sup.runWatched(9, procStatus{}, failingWatch); code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if rec.calls != 1 {
		t.Fatalf("cleanup calls = %d, want 1", rec.calls)
	}
	lines := decodeLines(t, buf)
	if len(lines) != 1 || lines[0]["event"] != "abnormal" || lines[0]["reason"] != "child watch failed" {
		t.Fatalf("evidence = %v", lines)
	}
	tc, ok := tmpCleanupField(t, lines[0])
	if !ok || tc["state"] != "retained" || tc["reason"] != "owner" {
		t.Fatalf("tmpCleanup = %v, want retained/owner", tc)
	}

	sup.abnormal("control EOF")
	if rec.calls != 1 {
		t.Fatalf("cleanup calls = %d after a second abnormal, want 1", rec.calls)
	}
	if _, ok := tmpCleanupField(t, decodeLines(t, buf)[1]); ok {
		t.Fatal("a second abnormal carries tmpCleanup")
	}
}

// TestRunWatchedRunsTheLoopAfterAWatch: a successful watch of the child's pid
// hands over to the control loop.
func TestRunWatchedRunsTheLoopAfterAWatch(t *testing.T) {
	sup, _ := newTestSupervisor(`{"op":"dispose","id":1}`+"\n", scriptedReaper(reapEmpty))
	watched := 0
	watch := func(pid int) (<-chan error, error) {
		watched = pid
		return make(chan error), nil
	}
	if code := sup.runWatched(9, procStatus{}, watch); code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	if watched != 9 {
		t.Fatalf("watched pid %d, want 9", watched)
	}
}

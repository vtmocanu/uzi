package main

import (
	"bytes"
	"reflect"
	"syscall"
	"testing"
	"time"
)

// reapStep is one scripted childReaper result.
type reapStep struct {
	pid int
	err error
}

// seqReaper returns a childReaper that yields the scripted steps in order, then
// ECHILD (no children remain) once exhausted.
func seqReaper(steps ...reapStep) childReaper {
	i := 0
	return func() (int, error) {
		if i < len(steps) {
			s := steps[i]
			i++
			return s.pid, s.err
		}
		return 0, syscall.ECHILD
	}
}

// seqChildren returns a childLister that yields the scripted slices in order,
// then empty once exhausted.
func seqChildren(slices ...[]int) childLister {
	i := 0
	return func() ([]observationRow, error) {
		if i < len(slices) {
			s := slices[i]
			i++
			return testChildRows(s), nil
		}
		return nil, nil
	}
}

func recordingKiller() (childKiller, *[]int) {
	var killed []int
	return func(child processIdentity) error {
		killed = append(killed, child.Pid)
		return nil
	}, &killed
}

// farDeadline never trips; clock stays before it.
func farClock() (time.Time, func() time.Time, func()) {
	deadline := time.Unix(1000, 0)
	clock := func() time.Time { return time.Unix(0, 0) }
	return deadline, clock, func() {}
}

func TestDrainDrained(t *testing.T) {
	deadline, clock, sleep := farClock()
	kill, killed := recordingKiller()
	children := seqChildren([]int{10}, []int{}) // top of iter1: [10]; ECHILD branch: []
	reap := seqReaper(reapStep{10, nil}, reapStep{0, syscall.ECHILD})

	got := drain(deadline, clock, sleep, children, kill, reap)
	if got.State != stateDrained {
		t.Fatalf("state = %q, want drained (%+v)", got.State, got)
	}
	if got.Authority != authorityECHILD {
		t.Errorf("authority = %q", got.Authority)
	}
	if !reflect.DeepEqual(got.Killed, []int{10}) {
		t.Errorf("killed = %v, want [10]", got.Killed)
	}
	if !reflect.DeepEqual(got.Reaped, []int{10}) {
		t.Errorf("reaped = %v, want [10]", got.Reaped)
	}
	if !reflect.DeepEqual(*killed, []int{10}) {
		t.Errorf("kill calls = %v, want [10]", *killed)
	}
}

func TestDrainDeadlineUnconfirmed(t *testing.T) {
	deadline := time.Unix(100, 0)
	// clock: before the deadline once (so it sleeps), then past it.
	times := []time.Time{time.Unix(99, 0), time.Unix(101, 0)}
	ci := 0
	clock := func() time.Time {
		tm := times[ci%len(times)]
		ci++
		return tm
	}
	kill, _ := recordingKiller()
	children := func() ([]observationRow, error) { return testChildRows([]int{10}), nil } // never drains
	reap := func() (int, error) { return 0, nil }                                         // WNOHANG, never ready

	got := drain(deadline, clock, func() {}, children, kill, reap)
	if got.State != stateUnconfirmed || got.Reason != reasonDeadline {
		t.Fatalf("got %+v, want unconfirmed/deadline", got)
	}
	if !reflect.DeepEqual(got.Children, []int{10}) {
		t.Errorf("children = %v, want [10]", got.Children)
	}
}

func TestDrainEchildContradicted(t *testing.T) {
	deadline, clock, sleep := farClock()
	kill, _ := recordingKiller()
	children := func() ([]observationRow, error) { return testChildRows([]int{11}), nil } // still present when ECHILD claimed
	reap := seqReaper(reapStep{0, syscall.ECHILD})

	got := drain(deadline, clock, sleep, children, kill, reap)
	if got.State != stateUnconfirmed || got.Reason != reasonEchildContradicted {
		t.Fatalf("got %+v, want unconfirmed/echild-contradicted", got)
	}
	if !reflect.DeepEqual(got.Children, []int{11}) {
		t.Errorf("children = %v, want [11]", got.Children)
	}
}

func TestDrainAdoptsReparentedDescendant(t *testing.T) {
	// iter1 reaps 10; a differently-grouped descendant 11 reparents onto the
	// supervisor and is only visible/reapable on iter2. drained must include both.
	deadline, clock, sleep := farClock()
	kill, killed := recordingKiller()
	children := seqChildren([]int{10}, []int{11}, []int{}) // iter1 top, iter2 top, ECHILD branch
	reap := seqReaper(
		reapStep{10, nil}, reapStep{0, nil}, // iter1 inner: reap 10, then not-ready
		reapStep{11, nil}, reapStep{0, syscall.ECHILD}, // iter2 inner: reap 11, then ECHILD
	)

	got := drain(deadline, clock, sleep, children, kill, reap)
	if got.State != stateDrained {
		t.Fatalf("state = %q, want drained (%+v)", got.State, got)
	}
	if !reflect.DeepEqual(got.Killed, []int{10, 11}) {
		t.Errorf("killed = %v, want [10 11]", got.Killed)
	}
	if !reflect.DeepEqual(got.Reaped, []int{10, 11}) {
		t.Errorf("reaped = %v, want [10 11]", got.Reaped)
	}
	if !reflect.DeepEqual(*killed, []int{10, 11}) {
		t.Errorf("kill calls = %v, want [10 11]", *killed)
	}
}

func TestDrainIdempotentOnEmptyTree(t *testing.T) {
	// A repeat dispose over an already-empty tree observes ECHILD immediately.
	deadline, clock, sleep := farClock()
	kill, killed := recordingKiller()
	children := seqChildren([]int{}, []int{})
	reap := seqReaper(reapStep{0, syscall.ECHILD})

	got := drain(deadline, clock, sleep, children, kill, reap)
	if got.State != stateDrained {
		t.Fatalf("state = %q, want drained", got.State)
	}
	if len(got.Killed) != 0 || len(got.Reaped) != 0 {
		t.Errorf("killed/reaped = %v/%v, want empty", got.Killed, got.Reaped)
	}
	if len(*killed) != 0 {
		t.Errorf("kill calls = %v, want none", *killed)
	}
}

func TestDrainContinuousReapsDeadline(t *testing.T) {
	now := time.Unix(0, 0)
	deadline := now.Add(time.Second)
	waits := 0
	got := drain(deadline, func() time.Time { return now }, func() {},
		seqChildren(nil), func(processIdentity) error { return nil },
		func() (int, error) {
			waits++
			if waits > 10000 {
				return 0, syscall.ECHILD
			}
			now = now.Add(250 * time.Millisecond)
			return waits, nil
		})
	if got.State != stateUnconfirmed || got.Reason != reasonDeadline || waits != 4 {
		t.Fatalf("waits=%d result=%+v", waits, got)
	}
	t.Logf("continuous reaps: waits=%d retained=%d state=%s reason=%s", waits, len(got.Reaped), got.State, got.Reason)
}

func TestDrainCumulativeChurnBound(t *testing.T) {
	deadline, clock, sleep := farClock()
	passes, kills, waits := 0, 0, 0
	got := drain(deadline, clock, sleep,
		func() ([]observationRow, error) { passes++; return testChildRows([]int{passes}), nil },
		func(processIdentity) error { kills++; return nil },
		func() (int, error) {
			waits++
			if waits%2 == 0 {
				return 0, nil
			}
			return (waits + 1) / 2, nil
		})
	if got.State != stateUnconfirmed || got.Reason != "oversize" || kills != 256 || len(got.Killed) != 256 || len(got.Reaped) != 256 {
		t.Fatalf("passes=%d kills=%d waits=%d result=%+v", passes, kills, waits, got)
	}
	var output bytes.Buffer
	if err := (&evidence{w: &output}).writeJSON(disposeEvidence(1, got)); err != nil {
		t.Fatal(err)
	}
	if output.Len() >= maxEvidenceBytes {
		t.Fatalf("output bytes=%d", output.Len())
	}
	t.Logf("cumulative churn: passes=%d kills=%d waits=%d killed=%d reaped=%d serialized=%d", passes, kills, waits, len(got.Killed), len(got.Reaped), output.Len())
}

func TestDrainKillLoopDeadline(t *testing.T) {
	now := time.Unix(0, 0)
	kills, waits := 0, 0
	got := drain(now.Add(time.Second), func() time.Time { return now }, func() {},
		seqChildren([]int{1, 2, 3}),
		func(processIdentity) error { kills++; now = now.Add(time.Second); return nil },
		func() (int, error) { waits++; return 0, syscall.ECHILD })
	if got.State != stateUnconfirmed || got.Reason != reasonDeadline || kills != 1 || waits != 0 {
		t.Fatalf("kills=%d waits=%d result=%+v", kills, waits, got)
	}
}

func TestDrainDeadlineAfterEmptyVerification(t *testing.T) {
	now := time.Unix(0, 0)
	lists := 0
	got := drain(now.Add(time.Second), func() time.Time { return now }, func() {},
		func() ([]observationRow, error) {
			lists++
			if lists == 2 {
				now = now.Add(time.Second)
			}
			return nil, nil
		}, func(processIdentity) error { t.Fatal("unexpected kill"); return nil }, seqReaper())
	if got.State != stateUnconfirmed || got.Reason != reasonDeadline || got.Authority != "" {
		t.Fatalf("result=%+v", got)
	}
}

func TestDrainExpiredAuditorProbe(t *testing.T) {
	now := time.Unix(0, 0)
	waits, kills := 0, 0
	got := drain(now.Add(-time.Second), func() time.Time { return now }, func() {},
		seqChildren([]int{10001}),
		func(processIdentity) error { kills++; return nil },
		func() (int, error) {
			waits++
			if waits <= 10000 {
				return waits, nil
			}
			return 0, syscall.ECHILD
		})
	if got.State != stateUnconfirmed || got.Reason != reasonDeadline || waits != 0 || kills != 0 {
		t.Fatalf("waits=%d kills=%d result=%+v", waits, kills, got)
	}
	t.Logf("auditor expired deadline: waits=%d kills=%d retained=%d state=%s", waits, kills, len(got.Reaped), got.State)
}

// testChildRows adapts scripted PID lists to the identity-carrying seam.
func testChildRows(pids []int) []observationRow {
	rows := make([]observationRow, 0, len(pids))
	for _, pid := range pids {
		rows = append(rows, observationRow{processIdentity: processIdentity{pid, "1"}})
	}
	return rows
}

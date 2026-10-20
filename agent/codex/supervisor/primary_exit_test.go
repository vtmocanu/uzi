package main

import (
	"errors"
	"io"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func statusReaper(steps ...childWaitResult) childReaper {
	i := 0
	return func() (childWaitResult, error) {
		if i == len(steps) {
			return childWaitResult{}, syscall.ECHILD
		}
		got := steps[i]
		i++
		return got, nil
	}
}

func assertPrimaryEvents(t *testing.T, events []map[string]any, code int, disposeStates ...string) {
	t.Helper()
	exits, disposals := 0, 0
	for _, event := range events {
		switch event["event"] {
		case "child_exit":
			exits++
			if event["code"] != float64(code) || disposals != 0 {
				t.Fatalf("incorrect or late primary status: %v", event)
			}
		case "dispose":
			if disposals >= len(disposeStates) || event["state"] != disposeStates[disposals] {
				t.Fatalf("unexpected disposal: %v", event)
			}
			disposals++
		case "started":
		default:
			t.Fatalf("unexpected lifecycle event: %v", event)
		}
	}
	if exits != 1 || disposals != len(disposeStates) {
		t.Fatalf("exits=%d disposals=%d events=%v", exits, disposals, events)
	}
}

func TestPrimaryStatusReportedFromControlFirstDrain(t *testing.T) {
	// The adopted child's status is unrelated. Even a primary's ordinary exit23
	// consumed by disposal must retain23, never a fabricated SIGKILL status.
	sup, out := newTestSupervisor("{\"op\":\"dispose\",\"id\":1}\n", statusReaper(
		childWaitResult{pid: 10, code: 55}, childWaitResult{pid: 9, code: 23}))
	if code := sup.run(9, procStatus{}); code != 0 {
		t.Fatalf("helper exit=%d", code)
	}
	assertPrimaryEvents(t, decodeLines(t, out), 23, stateDrained)
}

type exitGatedControl struct {
	gate <-chan struct{}
	r    io.Reader
}

func (r *exitGatedControl) Read(p []byte) (int, error) {
	<-r.gate
	return r.r.Read(p)
}

func TestPrimaryReadinessThenDisposeReportsOnce(t *testing.T) {
	sup, out := newTestSupervisor("", statusReaper(childWaitResult{pid: 9, code: 88}))
	gate := make(chan struct{})
	ready := make(chan error, 1)
	ready <- nil
	sup.seams.childReady = ready
	sup.seams.reapChild = func(pid int) (int, error) {
		if pid != 9 {
			t.Fatalf("primary pid=%d", pid)
		}
		close(gate)
		return 7, nil
	}
	sup.control = &controlReader{r: &exitGatedControl{gate, strings.NewReader("{\"op\":\"dispose\",\"id\":1}\n")}}
	if code := sup.run(9, procStatus{}); code != 0 {
		t.Fatalf("helper exit=%d", code)
	}
	// A later same-numeric-PID adopted reap must not become a second primary.
	assertPrimaryEvents(t, decodeLines(t, out), 7, stateDrained)
}

func TestPrimaryConsumedDuringUnconfirmedDrainStaysSettledOnRetry(t *testing.T) {
	ready := make(chan error, 1)
	step := 0
	reap := func() (childWaitResult, error) {
		step++
		switch step {
		case 1:
			ready <- errors.New("stale readiness after primary was consumed")
			return childWaitResult{pid: 9, code: 137}, nil
		case 3:
			return childWaitResult{pid: 10, code: 42}, nil
		default:
			return childWaitResult{}, syscall.ECHILD
		}
	}
	sup, out := newTestSupervisor("{\"op\":\"dispose\",\"id\":1}\n{\"op\":\"dispose\",\"id\":2}\n", reap)
	sup.seams.childReady = ready
	sup.seams.reapChild = func(int) (int, error) { t.Fatal("primary must never be waited twice"); return 0, nil }
	sup.seams.directChildren = seqChildren([]int{9, 10}, []int{10}, []int{10}, nil)
	if code := sup.run(9, procStatus{}); code != 0 {
		t.Fatalf("helper exit=%d", code)
	}
	assertPrimaryEvents(t, decodeLines(t, out), 137, stateUnconfirmed, stateDrained)
}

func TestTerminalWaitCodeRejectsNonterminalStatus(t *testing.T) {
	for _, tc := range []struct {
		status unix.WaitStatus
		code   int
	}{
		{unix.WaitStatus(23 << 8), 23}, {unix.WaitStatus(unix.SIGKILL), 137},
	} {
		if code, err := terminalWaitCode(tc.status); err != nil || code != tc.code {
			t.Fatalf("status=%d code=%d error=%v", tc.status, code, err)
		}
	}
	for _, status := range []unix.WaitStatus{unix.WaitStatus(int(unix.SIGSTOP)<<8 | 0x7f), unix.WaitStatus(0xffff)} {
		if code, err := terminalWaitCode(status); err == nil {
			t.Fatalf("nonterminal status=%d invented exit=%d", status, code)
		}
	}
}

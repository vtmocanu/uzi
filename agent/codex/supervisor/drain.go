package main

import (
	"errors"
	"sort"
	"syscall"
	"time"
)

// The drain seams. Factoring the drain as a pure state machine over these three
// functions (plus a clock and sleep) lets the drained / unconfirmed-deadline /
// echild-contradicted / adopt-and-reap branches be driven deterministically in
// tests, with no root and no fork. The real implementations live in syscalls.go.
type (
	// childLister returns the supervisor's CURRENT direct children. As a
	// subreaper, differently-grouped descendants reparent onto the supervisor as
	// their intermediates die, so re-reading this each pass is what lets the
	// drain adopt a code-mode host that setsid'd into its own group — a
	// process-group kill would miss it.
	childLister func() []int

	// childKiller pidfd-opens the pid and sends SIGKILL. A vanished child
	// (ESRCH) is not an error the drain records as killed.
	childKiller func(pid int) error

	// childReaper is one Wait4(-1, WNOHANG|__WALL) step: (pid>0, nil) reaped a
	// child; (0, nil) no child is ready right now; (0, ECHILD) no children
	// remain — the drain authority.
	childReaper func() (pid int, err error)
)

// drain kills and reaps every current descendant until it observes ECHILD, or a
// bound is hit. drained REQUIRES an observed ECHILD with no remaining children;
// an ECHILD contradicted by a still-present child, or the deadline, yields
// unconfirmed. It is idempotent: a second call over an already-empty tree
// observes ECHILD immediately and reports drained with empty sets.
func drain(
	deadline time.Time,
	clock func() time.Time,
	sleep func(),
	children childLister,
	kill childKiller,
	reap childReaper,
) drainResult {
	killed := map[int]bool{}
	reaped := map[int]bool{}
	for {
		for _, pid := range children() {
			if err := kill(pid); err == nil {
				killed[pid] = true
			}
		}
		for {
			pid, err := reap()
			if err != nil {
				if errors.Is(err, syscall.ECHILD) {
					if remaining := children(); len(remaining) > 0 {
						return drainResult{
							State:    stateUnconfirmed,
							Reason:   reasonEchildContradicted,
							Killed:   sortedKeys(killed),
							Reaped:   sortedKeys(reaped),
							Children: remaining,
						}
					}
					return drainResult{
						State:     stateDrained,
						Authority: authorityECHILD,
						Killed:    sortedKeys(killed),
						Reaped:    sortedKeys(reaped),
					}
				}
				// A transient reap error (e.g. EINTR) is not authority; stop
				// reaping this pass and let the deadline gate decide.
				break
			}
			if pid == 0 {
				break
			}
			reaped[pid] = true
		}
		if !clock().Before(deadline) {
			return drainResult{
				State:    stateUnconfirmed,
				Reason:   reasonDeadline,
				Killed:   sortedKeys(killed),
				Reaped:   sortedKeys(reaped),
				Children: children(),
			}
		}
		sleep()
	}
}

func sortedKeys(m map[int]bool) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}

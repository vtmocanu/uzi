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
// tests, with no root and no fork. The real implementations live in main.go and
// cleanupwalk.go.
type (
	// childVisitor streams the supervisor's CURRENT direct children. As a
	// subreaper, differently-grouped descendants reparent onto the supervisor as
	// their intermediates die, so re-reading this each pass is what lets the
	// drain adopt a code-mode host that setsid'd into its own group — a
	// process-group kill would miss it.
	childVisitor func(deadline time.Time, visit func(observationRow) error) error

	// childKiller pidfd-opens the pid and sends SIGKILL. A vanished child
	// (ESRCH) is not an error the drain records as killed.
	childKiller func(child processIdentity) error

	// childReaper is one Wait4(-1, WNOHANG|__WALL) step: (pid>0, nil) reaped a
	// child; (0, nil) no child is ready right now; (0, ECHILD) no children
	// remain — the drain authority.
	childReaper func() (childWaitResult, error)
)

// A positive pid carries its actual normalized terminal wait status. The drain
// needs the pid; its owning supervisor also owes the primary's exit evidence.
type childWaitResult struct {
	pid  int
	code int
}

// drain kills and reaps every current descendant until it observes ECHILD, or a
// bound is hit. drained REQUIRES an observed ECHILD with no remaining children;
// an ECHILD contradicted by a still-present child, or the deadline, yields
// unconfirmed. Reaped contains only children waited by this drain call; children
// already reaped by the periodic control loop are absent. It is idempotent:
// a second call over an already-empty tree observes ECHILD immediately and
// reports drained with empty sets.
func drain(
	deadline time.Time,
	clock func() time.Time,
	sleep func(),
	children childVisitor,
	kill childKiller,
	reap childReaper,
) drainResult {
	killed := map[int]bool{}
	reaped := map[int]bool{}
	truncated := false
	retain := func(ids map[int]bool, pid int) {
		if ids[pid] {
			return
		}
		if len(ids) < maxCleanupDiagnosticItems {
			ids[pid] = true
		} else {
			truncated = true
		}
	}
	result := func(state, reason string, rows []observationRow) drainResult {
		return drainResult{State: state, Reason: reason, EvidenceTruncated: truncated,
			Killed: sortedKeys(killed), Reaped: sortedKeys(reaped), Children: identities(rows)}
	}
	unconfirmed := func(reason string, rows []observationRow) drainResult {
		return result(stateUnconfirmed, reason, rows)
	}
	appendRow := func(rows *[]observationRow, row observationRow) {
		for _, retained := range *rows {
			if retained.Pid == row.Pid {
				if retained.processIdentity != row.processIdentity {
					truncated = true // A later incarnation cannot replace historical evidence.
				}
				return
			}
		}
		if len(*rows) < maxCleanupDiagnosticItems {
			*rows = append(*rows, row)
		} else {
			truncated = true
		}
	}
	// Diagnostic storage never limits cleanup progress. The enclosing deadline
	// bounds enumeration, every signal, and even an uninterrupted reap stream.
	for {
		var listed []observationRow
		listErr := children(deadline, func(child observationRow) error {
			appendRow(&listed, child)
			if !clock().Before(deadline) {
				return errObservationTimeout
			}
			if err := kill(child.processIdentity); err == nil {
				retain(killed, child.Pid)
			}
			return nil
		})
		if errors.Is(listErr, errObservationTimeout) || !clock().Before(deadline) {
			return unconfirmed(reasonDeadline, listed)
		}
		if listErr != nil {
			return unconfirmed("enumeration", nil)
		}
		for {
			if !clock().Before(deadline) {
				return unconfirmed(reasonDeadline, listed)
			}
			waited, err := reap()
			if err != nil {
				if errors.Is(err, syscall.ECHILD) {
					var remaining []observationRow
					err := children(deadline, func(child observationRow) error {
						appendRow(&remaining, child)
						if !clock().Before(deadline) {
							return errObservationTimeout
						}
						return nil
					})
					if errors.Is(err, errObservationTimeout) || !clock().Before(deadline) {
						return unconfirmed(reasonDeadline, remaining)
					}
					if err != nil {
						return unconfirmed("enumeration", nil)
					}
					if len(remaining) > 0 {
						return unconfirmed(reasonEchildContradicted, remaining)
					}
					drained := result(stateDrained, "", nil)
					drained.Authority = authorityECHILD
					return drained
				}
				// A transient reap error (e.g. EINTR) is not authority; stop
				// reaping this pass and let the deadline gate decide.
				break
			}
			if waited.pid == 0 {
				break
			}
			retain(reaped, waited.pid)
		}
		if !clock().Before(deadline) {
			return unconfirmed(reasonDeadline, listed)
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

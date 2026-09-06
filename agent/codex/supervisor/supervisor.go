package main

import (
	"errors"
	"time"
)

// seams bundles every runtime dependency of the control loop behind small
// function values. main.go wires the real syscall implementations; tests can
// substitute fakes to drive the loop without root or fork.
type seams struct {
	supervisorPid  int
	directChildren childLister
	kill           childKiller
	reap           childReaper
	snapshot       func() ([]procRow, error)
	now            func() time.Time
	sleep          func()
}

// supervisor runs the fd 3 control loop and emits fd 4 evidence.
type supervisor struct {
	ev      *evidence
	control *controlReader
	seams   seams
}

// drainWith runs one bounded drain using the given timeout.
func (s *supervisor) drainWith(timeoutMs int) drainResult {
	deadline := s.seams.now().Add(time.Duration(timeoutMs) * time.Millisecond)
	return drain(deadline, s.seams.now, s.seams.sleep, s.seams.directChildren, s.seams.kill, s.seams.reap)
}

// run emits the started evidence, then services control frames until a dispose
// reaches drained (exit 0) or an abnormal condition ends the run (non-zero).
// There is NO fixed lifetime: the loop lives on the control channel, bounded
// only by per-op deadlines.
func (s *supervisor) run(childPid, uid int) int {
	_ = s.ev.writeJSON(startedEvidence{
		Event:         "started",
		SupervisorPid: s.seams.supervisorPid,
		ChildPid:      childPid,
		Subreaper:     true,
		Dumpable:      true,
		UID:           uid,
		CapsZero:      true,
		NoNewPrivs:    true,
	})

	for {
		line, err := s.control.readLine()
		if err != nil {
			// Controller loss / oversized frame: a best-effort drain, reported
			// as cleanup SEPARATE from the abnormal reason. Never clean success.
			reason := "control EOF"
			if errors.Is(err, errControlOversized) {
				reason = errControlOversized.Error()
			}
			return s.abnormal(reason)
		}

		op, perr := parseControlLine(line)
		if perr != nil {
			return s.abnormal(perr.Error())
		}

		switch op.Op {
		case opSnapshot:
			rows, serr := s.seams.snapshot()
			if serr != nil {
				return s.abnormal(serr.Error())
			}
			_ = s.ev.writeJSON(snapshotEvidence{Event: opSnapshot, ID: op.ID, Processes: rows})
		case opDispose:
			result := s.drainWith(op.TimeoutMs)
			_ = s.ev.writeJSON(disposeEvidence(op.ID, result))
			if result.State == stateDrained {
				return 0
			}
			// unconfirmed dispose is retained state, not success; keep serving
			// (a repeat dispose is safe/idempotent).
		}
	}
}

// abnormal emits a best-effort-drained abnormal event and returns the non-zero
// exit code. The reason is already a short, sanitized sentinel string.
func (s *supervisor) abnormal(reason string) int {
	cleanup := s.drainWith(defaultDisposeTimeoutMs)
	_ = s.ev.writeJSON(abnormalEvidence(reason, &cleanup))
	return 2
}

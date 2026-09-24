package main

import (
	"errors"
	"strconv"
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
	childReady     <-chan error
	reapChild      func(pid int) (code int, err error)
	snapshot       func() ([]procRow, error)
	now            func() time.Time
	sleep          func()
	// tmpCleanup removes the command tmp and reports the outcome. nil when no
	// --cleanup-token was given; the evidence then carries no tmpCleanup field.
	tmpCleanup func() *tmpCleanupResult
}

// supervisor runs the fd 3 control loop and emits fd 4 evidence.
type supervisor struct {
	ev      *evidence
	control *controlReader
	seams   seams
	// tmpCleaned records that tmpCleanup already ran, so it runs at most once.
	tmpCleaned bool
}

// drainWith runs one bounded drain using the given timeout.
func (s *supervisor) drainWith(timeoutMs int) drainResult {
	deadline := s.seams.now().Add(time.Duration(timeoutMs) * time.Millisecond)
	return drain(deadline, s.seams.now, s.seams.sleep, s.seams.directChildren, s.seams.kill, s.seams.reap)
}

// runWatched watches the primary child through watch and then runs the control
// loop. A watch failure ends the run through abnormal, the one path that
// gates the command tmp cleanup on a drained drain and runs it at most once.
func (s *supervisor) runWatched(childPid int, st procStatus, watch func(pid int) (<-chan error, error)) int {
	ready, err := watch(childPid)
	if err != nil {
		return s.abnormal("child watch failed")
	}
	s.seams.childReady = ready
	return s.run(childPid, st)
}

// run emits the started evidence, then services control frames until a dispose
// reaches drained (exit 0) or an abnormal condition ends the run (non-zero).
// There is NO fixed lifetime: the loop lives on the control channel, bounded
// only by per-op deadlines.
func (s *supervisor) run(childPid int, st procStatus) int {
	_ = s.ev.writeJSON(startedEvidence{
		Event:          "started",
		SupervisorPid:  s.seams.supervisorPid,
		ChildPid:       childPid,
		Subreaper:      true,
		Nondumpable:    true,
		UID:            st.UID,
		LiveCapsZero:   st.CapInh == 0 && st.CapPrm == 0 && st.CapEff == 0 && st.CapAmb == 0,
		CapBoundingSet: "0x" + strconv.FormatUint(st.CapBnd, 16),
		NoNewPrivs:     st.NoNewPrivs == 1,
	})

	type controlResult struct {
		line []byte
		err  error
	}
	controls := make(chan controlResult, 1)
	go func() {
		for {
			line, err := s.control.readLine()
			controls <- controlResult{line: line, err: err}
			if err != nil {
				return
			}
		}
	}()

	childReady := s.seams.childReady
	for {
		select {
		case readyErr := <-childReady:
			if readyErr != nil {
				return s.abnormal("child watch failed")
			}
			code, err := s.seams.reapChild(childPid)
			if err != nil {
				return s.abnormal("child wait failed")
			}
			_ = s.ev.writeJSON(childExitEvidence{Event: "child_exit", Code: code})
			// A nil channel disables this select arm. The root remains alive on fd3
			// so the controller can drain adopted/background descendants explicitly.
			childReady = nil
		case result := <-controls:
			line, err := result.line, result.err
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
				drained := s.drainWith(op.TimeoutMs)
				m := disposeEvidence(op.ID, drained)
				if drained.State == stateDrained {
					// Remove the tmp BEFORE reporting, so the dispose line carries
					// the outcome. It never changes the exit code.
					withTmpCleanup(m, s.cleanupTmp())
					_ = s.ev.writeJSON(m)
					return 0
				}
				_ = s.ev.writeJSON(m)
				// unconfirmed dispose is retained state, not success; keep serving
				// (a repeat dispose is safe/idempotent).
			}
		}
	}
}

// abnormal emits a best-effort-drained abnormal event and returns the non-zero
// exit code. The reason is already a short, sanitized sentinel string.
//
// The command tmp is removed only when that drain reached drained. An
// unconfirmed drain may leave a live descendant still using the tmp, so it is
// left untouched for the startup orphan reaper (--reap-orphans).
func (s *supervisor) abnormal(reason string) int {
	cleanup := s.drainWith(defaultDisposeTimeoutMs)
	m := abnormalEvidence(reason, &cleanup)
	if cleanup.State == stateDrained {
		withTmpCleanup(m, s.cleanupTmp())
	}
	_ = s.ev.writeJSON(m)
	return 2
}

// cleanupTmp runs the tmpCleanup seam at most once. It returns nil (no
// evidence field) when there is no seam or it already ran.
func (s *supervisor) cleanupTmp() *tmpCleanupResult {
	if s.seams.tmpCleanup == nil || s.tmpCleaned {
		return nil
	}
	s.tmpCleaned = true
	return s.seams.tmpCleanup()
}

package main

import (
	"bufio"
	"errors"
	"io"
	"os"
	"strconv"
	"time"
)

// cleanupReader bounds each streaming read by the enclosing operation's clock,
// including Scanner reads which consume whitespace without returning a token.
type cleanupReader struct {
	io.Reader
	deadline time.Time
}

func (r cleanupReader) Read(data []byte) (int, error) {
	if !time.Now().Before(r.deadline) {
		return 0, errObservationTimeout
	}
	return r.Reader.Read(data)
}

// visitCleanupChildren has constant retained state, independent of the number
// of threads or children. Observation's aggregate sample limits do not govern
// ordinary cleanup. Duplicate appearances are safely revalidated and revisited.
func visitCleanupChildren(root string, pid int, deadline time.Time, visit func(observationRow) error) error {
	deadline = minDeadline(deadline, time.Now().Add(time.Second))
	p := &observationProc{root: root, deadline: deadline}
	if err := p.check(); err != nil {
		return err
	}
	name := strconv.Itoa(pid) + "/task"
	dir, err := os.Open(root + "/" + name)
	if err != nil {
		return procReadError(err)
	}
	defer dir.Close()
	for {
		if err := p.check(); err != nil {
			return err
		}
		entries, err := dir.ReadDir(1)
		if err != nil && !errors.Is(err, io.EOF) {
			return procReadError(err)
		}
		if len(entries) == 0 {
			return p.check()
		}
		tid, err := strconv.Atoi(entries[0].Name())
		if err != nil || tid <= 0 || strconv.Itoa(tid) != entries[0].Name() {
			return errObservationUnreadable
		}
		if err := visitCleanupThread(p, pid, name+"/"+entries[0].Name()+"/children", visit); err != nil {
			return err
		}
	}
}

func minDeadline(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func visitCleanupThread(p *observationProc, parent int, name string, visit func(observationRow) error) error {
	f, err := os.Open(p.root + "/" + name)
	if err != nil {
		if errors.Is(procReadError(err), errObservationGone) {
			return nil // A vanished thread may have reparented children; rescan after waits.
		}
		return procReadError(err)
	}
	defer f.Close()
	scan := bufio.NewScanner(cleanupReader{Reader: f, deadline: p.deadline})
	scan.Split(bufio.ScanWords)
	scan.Buffer(make([]byte, 32), 32) // A decimal Linux PID is at most ten digits.
	for scan.Scan() {
		if err := p.check(); err != nil {
			return err
		}
		pid, err := strconv.Atoi(scan.Text())
		if err != nil || pid <= 0 || strconv.Itoa(pid) != scan.Text() {
			return errObservationUnreadable
		}
		// Each stat retains its per-record limit, with a fresh aggregate budget.
		row, err := (&observationProc{root: p.root, deadline: p.deadline}).row(pid)
		if errors.Is(err, errObservationGone) {
			continue
		}
		if err != nil {
			return err
		}
		if row.PPid != parent {
			return errObservationStale
		}
		if err := visit(row); err != nil {
			return err
		}
	}
	if err := scan.Err(); err != nil {
		if errors.Is(err, errObservationTimeout) {
			return err
		}
		return errObservationUnreadable
	}
	return p.check()
}

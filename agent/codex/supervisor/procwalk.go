package main

import (
	"errors"
	"strconv"
	"strings"
)

// errSnapshotTooLarge is returned when the descendant walk exceeds its bound.
var errSnapshotTooLarge = errors.New("snapshot bounds exceeded")

// parseStatPPidPgid extracts ppid and pgid from a /proc/<pid>/stat line. It is
// pure and comm-safe: the comm field can itself contain spaces and parentheses,
// so it splits on the LAST ") " and reads the state/ppid/pgid fields after it —
// and it never returns the comm, which is read separately from /proc/<pid>/comm.
func parseStatPPidPgid(stat string) (ppid, pgid int, err error) {
	idx := strings.LastIndex(stat, ") ")
	if idx < 0 {
		return 0, 0, errors.New("malformed stat: no comm terminator")
	}
	fields := strings.Fields(stat[idx+2:])
	// fields[0]=state, fields[1]=ppid, fields[2]=pgid
	if len(fields) < 3 {
		return 0, 0, errors.New("malformed stat: too few fields")
	}
	if ppid, err = strconv.Atoi(fields[1]); err != nil {
		return 0, 0, err
	}
	if pgid, err = strconv.Atoi(fields[2]); err != nil {
		return 0, 0, err
	}
	return ppid, pgid, nil
}

// sanitizeComm trims the trailing newline and caps the value at the kernel's
// 15-char comm length. This is the ONLY process-name detail exported — never
// argv/cmdline.
func sanitizeComm(raw string) string {
	c := strings.TrimRight(raw, "\n")
	if len(c) > 15 {
		c = c[:15]
	}
	return c
}

// walkDescendants retains the legacy snapshot shape and destructive error path,
// but uses the same bounded, identity-validated proc traversal as observation.
func walkDescendants(rootPid int) ([]procRow, error) {
	p := &observationProc{root: "/proc"}
	root, err := p.row(rootPid)
	if err != nil {
		return nil, err
	}
	observed, err := p.walk(root.processIdentity, root.processIdentity)
	if err != nil {
		return nil, err
	}
	if len(observed) > maxSnapshotDescendants {
		return nil, errSnapshotTooLarge
	}
	rows := make([]procRow, 0, len(observed))
	for _, row := range observed {
		stat, err := p.read(strconv.Itoa(row.Pid) + "/stat")
		if err != nil {
			return nil, err
		}
		ppid, pgid, err := parseStatPPidPgid(string(stat))
		if err != nil {
			return nil, err
		}
		comm, err := p.read(strconv.Itoa(row.Pid) + "/comm")
		if err != nil {
			return nil, err
		}
		rows = append(rows, procRow{Pid: row.Pid, PPid: ppid, Pgid: pgid, Comm: sanitizeComm(string(comm))})
	}
	return rows, nil
}

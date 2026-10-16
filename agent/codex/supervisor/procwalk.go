package main

import (
	"errors"
	"os"
	"sort"
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

// walkDescendants reports the legacy best-effort snapshot. These rows are not
// intervention evidence: strict identity/consistency budgets live in observation.
func walkDescendants(rootPid int) ([]procRow, error) {
	return walkDescendantsFrom("/proc", rootPid)
}

// snapshotChildrenOf retains the reporting lane's thread-child deduplication.
// Unlike observation, legacy snapshots have no aggregate thread/byte budget.
func snapshotChildrenOf(procRoot string, pid int) []int {
	set := map[int]bool{}
	taskDir := procRoot + "/" + strconv.Itoa(pid) + "/task"
	entries, err := os.ReadDir(taskDir)
	if err != nil {
		return nil
	}
	for _, entry := range entries {
		data, err := os.ReadFile(taskDir + "/" + entry.Name() + "/children")
		if err != nil {
			continue
		}
		for _, field := range strings.Fields(string(data)) {
			if child, err := strconv.Atoi(field); err == nil {
				set[child] = true
			}
		}
	}
	children := make([]int, 0, len(set))
	for child := range set {
		children = append(children, child)
	}
	sort.Ints(children)
	return children
}

// readSnapshotProcRow drops vanished/unreadable/malformed rows and tolerates a
// missing comm, as the reporting contract did before memory observation existed.
func readSnapshotProcRow(procRoot string, pid int) (procRow, bool) {
	dir := procRoot + "/" + strconv.Itoa(pid)
	stat, err := os.ReadFile(dir + "/stat")
	if err != nil {
		return procRow{}, false
	}
	ppid, pgid, err := parseStatPPidPgid(string(stat))
	if err != nil {
		return procRow{}, false
	}
	comm := ""
	if data, err := os.ReadFile(dir + "/comm"); err == nil {
		comm = sanitizeComm(string(data))
	}
	return procRow{Pid: pid, PPid: ppid, Pgid: pgid, Comm: comm}, true
}

// walkDescendantsFrom is a package-only proc-root seam for deterministic tests.
func walkDescendantsFrom(procRoot string, rootPid int) ([]procRow, error) {
	pending := snapshotChildrenOf(procRoot, rootPid)
	visited := map[int]bool{}
	rows := []procRow{}
	for len(pending) > 0 {
		pid := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if visited[pid] {
			continue
		}
		visited[pid] = true
		if len(visited) > maxSnapshotDescendants {
			return nil, errSnapshotTooLarge
		}
		row, ok := readSnapshotProcRow(procRoot, pid)
		if !ok {
			continue
		}
		rows = append(rows, row)
		pending = append(pending, snapshotChildrenOf(procRoot, pid)...)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Pid < rows[j].Pid })
	return rows, nil
}

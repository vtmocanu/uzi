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

// childrenOf reads the direct children of pid via /proc/<pid>/task/*/children.
func childrenOf(pid int) []int {
	set := map[int]bool{}
	taskDir := "/proc/" + strconv.Itoa(pid) + "/task"
	entries, err := os.ReadDir(taskDir)
	if err != nil {
		return nil
	}
	for _, e := range entries {
		data, err := os.ReadFile(taskDir + "/" + e.Name() + "/children")
		if err != nil {
			continue
		}
		for _, f := range strings.Fields(string(data)) {
			if n, err := strconv.Atoi(f); err == nil {
				set[n] = true
			}
		}
	}
	out := make([]int, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}

// readProcRow builds one sanitized snapshot row from /proc. ppid/pgid come from
// stat; comm comes from the dedicated comm file. A row is dropped (ok=false) if
// the process vanished mid-walk.
func readProcRow(pid int) (procRow, bool) {
	statData, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return procRow{}, false
	}
	ppid, pgid, err := parseStatPPidPgid(string(statData))
	if err != nil {
		return procRow{}, false
	}
	comm := ""
	if commData, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/comm"); err == nil {
		comm = sanitizeComm(string(commData))
	}
	return procRow{Pid: pid, PPid: ppid, Pgid: pgid, Comm: comm}, true
}

// walkDescendants returns the sanitized snapshot of every descendant of rootPid,
// bounded at maxSnapshotDescendants to reject a hostile/large tree.
func walkDescendants(rootPid int) ([]procRow, error) {
	pending := childrenOf(rootPid)
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
		row, ok := readProcRow(pid)
		if !ok {
			continue
		}
		rows = append(rows, row)
		pending = append(pending, childrenOf(pid)...)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Pid < rows[j].Pid })
	return rows, nil
}

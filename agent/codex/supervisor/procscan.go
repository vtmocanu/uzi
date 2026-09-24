package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// The no-user proof: the orphan reaper deletes a released directory only when
// the kernel process table shows that no process in this PID namespace, other
// than the reaper and its ancestor chain, has the command uid in any of its
// real/effective/saved/filesystem uids. Every command user runs as that uid
// with zero capabilities and no_new_privs, so it cannot leave it; a process
// that could still use a released directory therefore shows up here.
const (
	proofHeld      = "held"
	proofUnknown   = "unknown"
	proofUserAlive = "user_alive"
)

// procUserRow is one process's identity for the no-user proof.
type procUserRow struct {
	pid  int
	ppid int
	// uids are the real, effective, saved and filesystem uids of the Uid line.
	uids [4]int
}

// procTable lists every process of the reaper's PID namespace. Any error
// makes the proof unknown.
type procTable interface {
	rows() ([]procUserRow, error)
}

// Bounds of the production scan.
const (
	// procRootPath is the production proc root.
	procRootPath = "/proc"
	// maxProcScan is the most pids one scan lists; more is an error.
	maxProcScan = 100000
	// maxStatusBytes is the most of one status file read. Uid and PPid come
	// long before the one unbounded line (Groups), so a longer file is parsed
	// up to its last complete line.
	maxStatusBytes = 64 << 10
	// maxMountinfoBytes is the most of self/mountinfo read; a longer file is
	// an error, because a truncated one could hide the proc mount's line.
	maxMountinfoBytes = 4 << 20
)

var (
	errProcHidden   = errors.New("proc mount hides processes")
	errProcMount    = errors.New("proc mount not found")
	errProcBound    = errors.New("proc scan bound exceeded")
	errProcSelf     = errors.New("proc self is not this process")
	errProcTooLarge = errors.New("proc file too large")
	errStatusParse  = errors.New("malformed status")
)

// procFS is the production procTable: it reads the proc filesystem mounted at
// root through fd-relative opens. self is this process's pid, which root's
// "self" link must name (so root is this PID namespace's proc).
type procFS struct {
	root    string
	self    int
	maxPids int
}

// rows opens the proc root without following a symlink, refuses a mount that
// can hide processes (checkProcMount), checks "self", then lists the numeric
// entries (at most maxPids) and parses each one's status. A pid whose status
// is gone (ENOENT/ESRCH: it exited mid-scan) is skipped; any other read or
// parse failure is an error.
func (p procFS) rows() ([]procUserRow, error) {
	fd, err := unix.Open(p.root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open proc root: %w", err)
	}
	defer func() { _ = unix.Close(fd) }()

	mi, truncated, err := readFileAt(fd, "self/mountinfo", maxMountinfoBytes)
	if err != nil {
		return nil, fmt.Errorf("mountinfo: %w", err)
	}
	if truncated {
		return nil, fmt.Errorf("mountinfo: %w", errProcTooLarge)
	}
	if err := checkProcMount(string(mi), p.root); err != nil {
		return nil, err
	}
	link := make([]byte, 64)
	n, err := unix.Readlinkat(fd, "self", link)
	if err != nil {
		return nil, fmt.Errorf("self: %w", err)
	}
	if string(link[:n]) != strconv.Itoa(p.self) {
		return nil, errProcSelf
	}

	limit := p.maxPids
	if limit <= 0 {
		limit = maxProcScan
	}
	pids, err := listPids(fd, limit)
	if err != nil {
		return nil, err
	}
	rows := make([]procUserRow, 0, len(pids))
	for _, pid := range pids {
		data, truncated, err := readFileAt(fd, strconv.Itoa(pid)+"/status", maxStatusBytes)
		if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ESRCH) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("status of %d: %w", pid, err)
		}
		text := string(data)
		if truncated {
			// Parse only complete lines.
			if i := strings.LastIndexByte(text, '\n'); i >= 0 {
				text = text[:i+1]
			}
		}
		row, err := parseStatusIDs(text)
		if err != nil {
			return nil, fmt.Errorf("status of %d: %w", pid, err)
		}
		row.pid = pid
		rows = append(rows, row)
	}
	return rows, nil
}

// readFileAt reads at most limit bytes of name under dirfd (no final-symlink
// follow). truncated reports that the file had more.
func readFileAt(dirfd int, name string, limit int) (data []byte, truncated bool, err error) {
	fd, err := unix.Openat(dirfd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = unix.Close(fd) }()
	buf := make([]byte, 0, 4096)
	chunk := make([]byte, 4096)
	for {
		n, err := unix.Read(fd, chunk)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return nil, false, err
		}
		if n <= 0 {
			return buf, false, nil
		}
		if len(buf)+n > limit {
			return append(buf, chunk[:limit-len(buf)]...), true, nil
		}
		buf = append(buf, chunk[:n]...)
	}
}

// listPids returns the numeric entries of the proc root fd, failing with
// errProcBound once there are more than limit.
func listPids(fd int, limit int) ([]int, error) {
	buf := make([]byte, 32<<10)
	var names []string
	var pids []int
	for {
		n, err := unix.Getdents(fd, buf)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("list proc: %w", err)
		}
		if n <= 0 {
			return pids, nil
		}
		_, _, names = unix.ParseDirent(buf[:n], -1, names[:0])
		for _, name := range names {
			pid, ok := parsePid(name)
			if !ok {
				continue
			}
			if len(pids) >= limit {
				return nil, errProcBound
			}
			pids = append(pids, pid)
		}
	}
}

// parsePid accepts a proc entry name made only of decimal digits.
func parsePid(name string) (int, bool) {
	if name == "" || len(name) > 10 {
		return 0, false
	}
	for _, c := range name {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	pid, err := strconv.Atoi(name)
	return pid, err == nil && pid > 0
}

// parseStatusIDs reads the PPid line (one number) and the Uid line (exactly
// four numbers) of a status file. A missing, repeated or malformed line is an
// error, never a zero.
func parseStatusIDs(text string) (procUserRow, error) {
	var row procUserRow
	sawUID, sawPPid := false, false
	for _, line := range strings.Split(text, "\n") {
		key, val, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		switch key {
		case "Uid":
			fields := strings.Fields(val)
			if sawUID || len(fields) != 4 {
				return row, errStatusParse
			}
			for i, f := range fields {
				n, err := strconv.Atoi(f)
				if err != nil || n < 0 {
					return row, errStatusParse
				}
				row.uids[i] = n
			}
			sawUID = true
		case "PPid":
			fields := strings.Fields(val)
			if sawPPid || len(fields) != 1 {
				return row, errStatusParse
			}
			n, err := strconv.Atoi(fields[0])
			if err != nil || n < 0 {
				return row, errStatusParse
			}
			row.ppid, sawPPid = n, true
		}
	}
	if !sawUID || !sawPPid {
		return row, errStatusParse
	}
	return row, nil
}

// checkProcMount finds every mountinfo line whose mount point is root. There
// must be at least one, and each must be a proc mount with neither a hidepid
// other than 0/off nor any subset= option, in either its per-mount or its
// super-block options: hidepid hides a nondumpable same-uid process from the
// scan, so its presence makes the proof unknown.
func checkProcMount(mountinfo, root string) error {
	found := false
	for _, line := range strings.Split(mountinfo, "\n") {
		if line == "" {
			continue
		}
		left, right, ok := strings.Cut(line, " - ")
		if !ok {
			return fmt.Errorf("%w: malformed mountinfo line", errProcMount)
		}
		lf := strings.Fields(left)
		rf := strings.Fields(right)
		if len(lf) < 6 || len(rf) < 3 {
			return fmt.Errorf("%w: malformed mountinfo line", errProcMount)
		}
		if unescapeMountField(lf[4]) != root {
			continue
		}
		found = true
		if rf[0] != "proc" {
			return fmt.Errorf("%w: %s is not a proc mount", errProcMount, root)
		}
		for _, opts := range []string{lf[5], rf[2]} {
			for _, opt := range strings.Split(opts, ",") {
				k, v, _ := strings.Cut(opt, "=")
				switch {
				case k == "hidepid" && v != "0" && v != "off":
					return errProcHidden
				case k == "subset":
					return errProcHidden
				}
			}
		}
	}
	if !found {
		return errProcMount
	}
	return nil
}

// unescapeMountField undoes mountinfo's \ooo octal escapes (space, tab,
// newline, backslash).
func unescapeMountField(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+4 <= len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// proveNoUser takes one proof from table: "held" when no process other than
// selfPid and its ancestor chain (followed through the rows' PPid) has uid in
// any uid field, "user_alive" when one does, and "unknown" when the table
// errs or does not list selfPid itself.
func proveNoUser(table procTable, uid, selfPid int) string {
	rows, err := table.rows()
	if err != nil {
		return proofUnknown
	}
	parent := make(map[int]int, len(rows))
	for _, r := range rows {
		parent[r.pid] = r.ppid
	}
	if _, ok := parent[selfPid]; !ok {
		return proofUnknown
	}
	exempt := map[int]bool{}
	for pid := selfPid; pid > 0 && !exempt[pid]; {
		exempt[pid] = true
		pp, ok := parent[pid]
		if !ok {
			break
		}
		pid = pp
	}
	for _, r := range rows {
		if exempt[r.pid] {
			continue
		}
		for _, u := range r.uids {
			if u == uid {
				return proofUserAlive
			}
		}
	}
	return proofHeld
}

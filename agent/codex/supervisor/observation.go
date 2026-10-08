package main

import (
	"errors"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const observationItems = 256
const observationBytes = 64 << 10

type processIdentity struct {
	Pid       int    `json:"pid"`
	StartTime string `json:"startTime"`
}
type observationRow struct {
	processIdentity
	PPid int    `json:"ppid"`
	RSS  uint64 `json:"rss"`
}
type observationEvidence struct {
	Event      string           `json:"event"`
	ID         int              `json:"id"`
	State      string           `json:"state"`
	Reason     string           `json:"reason,omitempty"`
	Supervisor processIdentity  `json:"supervisor"`
	Root       processIdentity  `json:"root"`
	Processes  []observationRow `json:"processes"`
}

var (
	errObservationOversize   = errors.New("oversize")
	errObservationStale      = errors.New("stale")
	errObservationGone       = errors.New("gone")
	errObservationUnreadable = errors.New("unreadable")
	errObservationTimeout    = errors.New("timeout")
)

type observationProc struct {
	root           string
	bytes, threads int
	deadline       time.Time
}

func (p *observationProc) check() error {
	if !p.deadline.IsZero() && !time.Now().Before(p.deadline) {
		return errObservationTimeout
	}
	return nil
}
func procReadError(err error) error {
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ESRCH) {
		return errObservationGone
	}
	return errObservationUnreadable
}

// read reserves an overflow byte within the aggregate budget before reading.
// Each file is also capped at 4 KiB; parsing never sees an unbounded file.
func (p *observationProc) read(name string) ([]byte, error) {
	if err := p.check(); err != nil {
		return nil, err
	}
	remaining := observationBytes - p.bytes
	if remaining < 2 {
		return nil, errObservationOversize
	}
	limit := min(4096, remaining-1)
	f, err := os.Open(p.root + "/" + name)
	if err != nil {
		return nil, procReadError(err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, int64(limit+1)))
	p.bytes += len(data)
	if err != nil {
		return nil, procReadError(err)
	}
	if len(data) > limit {
		return nil, errObservationOversize
	}
	return data, nil
}
func (p *observationProc) row(pid int) (observationRow, error) {
	data, err := p.read(strconv.Itoa(pid) + "/stat")
	if err != nil {
		return observationRow{}, err
	}
	text := string(data)
	end := strings.LastIndex(text, ") ")
	if end < 0 {
		return observationRow{}, errObservationUnreadable
	}
	space := strings.IndexByte(text, ' ')
	if space < 0 {
		return observationRow{}, errObservationUnreadable
	}
	actual, err := strconv.Atoi(strings.TrimSpace(text[:space]))
	if err != nil || actual != pid {
		return observationRow{}, errObservationStale
	}
	fields := strings.Fields(text[end+2:])
	if len(fields) < 22 {
		return observationRow{}, errObservationUnreadable
	}
	ppid, e1 := strconv.Atoi(fields[1])
	start, e2 := strconv.ParseUint(fields[19], 10, 64)
	pages, e3 := strconv.ParseInt(fields[21], 10, 64)
	if e1 != nil || e2 != nil || e3 != nil || start == 0 || pages < 0 || uint64(pages) > ^uint64(0)/uint64(os.Getpagesize()) {
		return observationRow{}, errObservationUnreadable
	}
	return observationRow{processIdentity{pid, strconv.FormatUint(start, 10)}, ppid, uint64(pages) * uint64(os.Getpagesize())}, nil
}

// children streams thread directory entries one at a time. A vanished thread
// invalidates the sample too: it may have forked a now-adopted child.
func (p *observationProc) children(pid int, limit int) ([]int, error) {
	if err := p.check(); err != nil {
		return nil, err
	}
	name := strconv.Itoa(pid) + "/task"
	dir, err := os.Open(p.root + "/" + name)
	if err != nil {
		return nil, procReadError(err)
	}
	defer dir.Close()
	seen := map[int]bool{}
	for {
		if err := p.check(); err != nil {
			return nil, err
		}
		entries, err := dir.ReadDir(1)
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, procReadError(err)
		}
		if len(entries) == 0 {
			break
		}
		if p.threads >= observationItems {
			return nil, errObservationOversize
		}
		p.threads++
		tid, e := strconv.Atoi(entries[0].Name())
		if e != nil || tid <= 0 {
			return nil, errObservationUnreadable
		}
		data, e := p.read(name + "/" + entries[0].Name() + "/children")
		if e != nil {
			return nil, e
		}
		text := strings.TrimSpace(string(data))
		for len(text) > 0 {
			end := strings.IndexAny(text, " \t\n")
			if end < 0 {
				end = len(text)
			}
			if len(seen) >= limit {
				return nil, errObservationOversize
			}
			child, e := strconv.Atoi(text[:end])
			if e != nil || child <= 0 {
				return nil, errObservationUnreadable
			}
			seen[child] = true
			text = strings.TrimSpace(text[end:])
		}
	}
	out := make([]int, 0, len(seen))
	for pid := range seen {
		out = append(out, pid)
	}
	sort.Ints(out)
	return out, nil
}
func identities(rows []observationRow) []int {
	out := make([]int, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.Pid)
	}
	return out
}
func (p *observationProc) direct(pid int) ([]observationRow, error) {
	return p.directLimit(pid, observationItems)
}

func (p *observationProc) directLimit(pid, limit int) ([]observationRow, error) {
	children, err := p.children(pid, limit)
	if err != nil {
		return nil, err
	}
	rows := make([]observationRow, 0, len(children))
	for _, child := range children {
		row, err := p.row(child)
		if err != nil {
			return nil, err
		}
		if row.PPid != pid {
			return nil, errObservationStale
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// observe takes one bounded pass then validates every identity and every parent
// edge in the overlapping second pass. Any inconsistency rejects the whole
// sample. There are no retries, signals, waits, or cleanup operations here.
func observe(root string, supervisor, primary processIdentity, deadline time.Time) observationEvidence {
	result := observationEvidence{Event: "observe", State: "unavailable", Supervisor: supervisor, Root: primary, Processes: []observationRow{}}
	p := &observationProc{root: root, deadline: deadline}
	rows, err := p.walk(supervisor, primary)
	if err != nil {
		result.Reason = err.Error()
		if errors.Is(err, errObservationGone) {
			result.Reason = "stale"
		}
		return result
	}
	result.State = "complete"
	result.Processes = rows
	return result
}
func (p *observationProc) walk(supervisor, primary processIdentity) ([]observationRow, error) {
	self, err := p.row(supervisor.Pid)
	if err != nil {
		return nil, err
	}
	if self.processIdentity != supervisor || primary.StartTime == "" {
		return nil, errObservationStale
	}
	pending := []observationRow{self}
	seen := map[int]observationRow{self.Pid: self}
	edges := map[int][]int{}
	rows := []observationRow{}
	for len(pending) > 0 {
		parent := pending[0]
		pending = pending[1:]
		children, err := p.directLimit(parent.Pid, observationItems-len(rows))
		if err != nil {
			return nil, err
		}
		edges[parent.Pid] = identities(children)
		for _, row := range children {
			if old, ok := seen[row.Pid]; ok {
				if old.processIdentity != row.processIdentity || old.PPid != row.PPid {
					return nil, errObservationStale
				}
				continue
			}
			if len(rows) >= observationItems || len(pending) >= observationItems {
				return nil, errObservationOversize
			}
			seen[row.Pid] = row
			rows = append(rows, row)
			pending = append(pending, row)
		}
	}
	if row, ok := seen[primary.Pid]; ok {
		if row.processIdentity != primary {
			return nil, errObservationStale
		}
	} else {
		_, err := p.row(primary.Pid)
		if !errors.Is(err, errObservationGone) {
			if err != nil {
				return nil, err
			}
			return nil, errObservationStale
		}
	}
	for _, parent := range seen {
		live, err := p.row(parent.Pid)
		if err != nil {
			return nil, err
		}
		if live.processIdentity != parent.processIdentity || live.PPid != parent.PPid {
			return nil, errObservationStale
		}
		children, err := p.children(parent.Pid, observationItems)
		if err != nil {
			return nil, err
		}
		old := edges[parent.Pid]
		if len(old) != len(children) {
			return nil, errObservationStale
		}
		for i := range old {
			if old[i] != children[i] {
				return nil, errObservationStale
			}
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Pid < rows[j].Pid })
	return rows, nil
}
func readIdentity(pid int) (processIdentity, error) {
	row, err := (&observationProc{root: "/proc"}).row(pid)
	return row.processIdentity, err
}

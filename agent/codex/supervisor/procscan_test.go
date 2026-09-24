package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// statusText is a status file shaped like the kernel's, with the given ids.
func statusText(ppid int, uids [4]int) string {
	return fmt.Sprintf("Name:\tsleep\nUmask:\t0022\nState:\tS (sleeping)\nTgid:\t77\nNgid:\t0\nPid:\t77\nPPid:\t%d\nTracerPid:\t0\nUid:\t%d\t%d\t%d\t%d\nGid:\t10002\t10002\t10002\t10002\nFDSize:\t64\nGroups:\t10002 10004\n",
		ppid, uids[0], uids[1], uids[2], uids[3])
}

func TestParseStatusIDs(t *testing.T) {
	row, err := parseStatusIDs(statusText(12, [4]int{1, 2, 3, 4}))
	if err != nil || row.ppid != 12 || row.uids != [4]int{1, 2, 3, 4} {
		t.Fatalf("parse = %+v %v", row, err)
	}
	good := statusText(12, [4]int{1, 2, 3, 4})
	for name, text := range map[string]string{
		"no Uid":          strings.Replace(good, "Uid:", "Xid:", 1),
		"no PPid":         strings.Replace(good, "PPid:", "XPid:", 1),
		"three uids":      strings.Replace(good, "\t4\n", "\n", 1),
		"five uids":       strings.Replace(good, "\t4\n", "\t4\t5\n", 1),
		"non-numeric uid": strings.Replace(good, "\t3\t", "\tx\t", 1),
		"negative uid":    strings.Replace(good, "\t3\t", "\t-3\t", 1),
		"bad PPid":        strings.Replace(good, "PPid:\t12", "PPid:\t1 2", 1),
		"repeated Uid":    good + "Uid:\t0\t0\t0\t0\n",
		"repeated PPid":   good + "PPid:\t1\n",
		"empty":           "",
		"garbage":         "\x00\x01not a status file",
	} {
		if _, err := parseStatusIDs(text); !errors.Is(err, errStatusParse) {
			t.Errorf("%s: err = %v, want errStatusParse", name, err)
		}
	}
}

// mountLine is one mountinfo line for a mount at point.
func mountLine(point, fstype, mountOpts, superOpts string) string {
	return fmt.Sprintf("24 1 0:22 / %s %s shared:13 - %s %s %s", point, mountOpts, fstype, fstype, superOpts)
}

func TestCheckProcMount(t *testing.T) {
	other := mountLine("/", "overlay", "rw,relatime", "rw,lowerdir=/l")
	tests := []struct {
		name  string
		lines []string
		want  error
	}{
		{"plain", []string{other, mountLine("/proc", "proc", "rw,nosuid,nodev,noexec,relatime", "rw")}, nil},
		{"hidepid=0", []string{mountLine("/proc", "proc", "rw", "rw,hidepid=0")}, nil},
		{"hidepid=off", []string{mountLine("/proc", "proc", "rw", "rw,hidepid=off")}, nil},
		{"hidepid=2", []string{mountLine("/proc", "proc", "rw", "rw,hidepid=2")}, errProcHidden},
		{"hidepid=1", []string{mountLine("/proc", "proc", "rw", "rw,hidepid=1")}, errProcHidden},
		{"hidepid=invisible", []string{mountLine("/proc", "proc", "rw", "rw,hidepid=invisible")}, errProcHidden},
		{"hidepid=ptraceable", []string{mountLine("/proc", "proc", "rw", "rw,hidepid=ptraceable")}, errProcHidden},
		{"hidepid per-mount", []string{mountLine("/proc", "proc", "rw,hidepid=2", "rw")}, errProcHidden},
		{"subset=pid", []string{mountLine("/proc", "proc", "rw", "rw,subset=pid")}, errProcHidden},
		{"stacked, one hidden", []string{mountLine("/proc", "proc", "rw", "rw"), mountLine("/proc", "proc", "rw", "rw,hidepid=2")}, errProcHidden},
		{"no proc line", []string{other}, errProcMount},
		{"not proc", []string{mountLine("/proc", "tmpfs", "rw", "rw")}, errProcMount},
		{"proc elsewhere only", []string{mountLine("/host/proc", "proc", "rw", "rw")}, errProcMount},
		{"malformed", []string{"garbage"}, errProcMount},
	}
	for _, tc := range tests {
		err := checkProcMount(strings.Join(tc.lines, "\n")+"\n", "/proc")
		if (tc.want == nil && err != nil) || (tc.want != nil && !errors.Is(err, tc.want)) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
	// An escaped mount point (a space is \040) is compared unescaped.
	if err := checkProcMount(mountLine(`/my\040proc`, "proc", "rw", "rw")+"\n", "/my proc"); err != nil {
		t.Fatalf("escaped mount point: %v", err)
	}
}

// fakeProcRoot builds a proc-shaped tree: self -> <self>, <self>/mountinfo
// (a proc mount at the tree's own path with superOpts), and one status per
// entry of statuses ("" means a pid dir without a status file: exited).
func fakeProcRoot(t *testing.T, self int, superOpts string, statuses map[int]string) string {
	t.Helper()
	root := t.TempDir()
	for pid, text := range statuses {
		dir := filepath.Join(root, strconv.Itoa(pid))
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if text != "" {
			if err := os.WriteFile(filepath.Join(dir, "status"), []byte(text), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	selfDir := filepath.Join(root, strconv.Itoa(self))
	if err := os.MkdirAll(selfDir, 0o700); err != nil {
		t.Fatal(err)
	}
	mi := mountLine("/", "overlay", "rw", "rw") + "\n" + mountLine(root, "proc", "rw,nosuid", superOpts) + "\n"
	if err := os.WriteFile(filepath.Join(selfDir, "mountinfo"), []byte(mi), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(strconv.Itoa(self), filepath.Join(root, "self")); err != nil {
		t.Fatal(err)
	}
	// Non-pid entries are ignored.
	if err := os.Mkdir(filepath.Join(root, "sys"), 0o700); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestProcFSRowsOnAFakeRoot(t *testing.T) {
	const self = 4242
	root := fakeProcRoot(t, self, "rw", map[int]string{
		self: statusText(1, [4]int{10003, 10003, 10003, 10003}),
		1:    statusText(0, [4]int{0, 0, 0, 0}),
		77:   statusText(1, [4]int{10002, 10002, 10003, 10002}),
		88:   "", // exited mid-scan: skipped
	})
	rows, err := procFS{root: root, self: self}.rows()
	if err != nil {
		t.Fatalf("rows: %v", err)
	}
	got := map[int]procUserRow{}
	for _, r := range rows {
		got[r.pid] = r
	}
	if len(got) != 3 || got[77].uids[2] != 10003 || got[77].ppid != 1 || got[self].ppid != 1 {
		t.Fatalf("rows = %+v", rows)
	}
	if _, ok := got[88]; ok {
		t.Fatal("a pid without a status must be skipped")
	}
}

func TestProcFSRowsFailClosed(t *testing.T) {
	const self = 4242
	selfStatus := statusText(1, [4]int{10003, 10003, 10003, 10003})
	t.Run("hidepid", func(t *testing.T) {
		root := fakeProcRoot(t, self, "rw,hidepid=2", map[int]string{self: selfStatus})
		if _, err := (procFS{root: root, self: self}).rows(); !errors.Is(err, errProcHidden) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("unparsable status", func(t *testing.T) {
		root := fakeProcRoot(t, self, "rw", map[int]string{self: selfStatus, 9: "Uid:\tnope\n"})
		if _, err := (procFS{root: root, self: self}).rows(); !errors.Is(err, errStatusParse) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("scan bound", func(t *testing.T) {
		root := fakeProcRoot(t, self, "rw", map[int]string{self: selfStatus, 9: selfStatus, 10: selfStatus})
		if _, err := (procFS{root: root, self: self, maxPids: 2}).rows(); !errors.Is(err, errProcBound) {
			t.Fatalf("err = %v", err)
		}
		if _, err := (procFS{root: root, self: self, maxPids: 3}).rows(); err != nil {
			t.Fatalf("at the bound: %v", err)
		}
	})
	t.Run("self is another pid", func(t *testing.T) {
		root := fakeProcRoot(t, self, "rw", map[int]string{self: selfStatus})
		if _, err := (procFS{root: root, self: self + 1}).rows(); !errors.Is(err, errProcSelf) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("unlistable root", func(t *testing.T) {
		if _, err := (procFS{root: filepath.Join(t.TempDir(), "absent"), self: self}).rows(); err == nil {
			t.Fatal("a missing proc root was listed")
		}
	})
	t.Run("symlinked root", func(t *testing.T) {
		root := fakeProcRoot(t, self, "rw", map[int]string{self: selfStatus})
		link := filepath.Join(t.TempDir(), "proc")
		if err := os.Symlink(root, link); err != nil {
			t.Fatal(err)
		}
		if _, err := (procFS{root: link, self: self}).rows(); err == nil {
			t.Fatal("a symlinked proc root was followed")
		}
	})
}

// TestProcFSRowsReal runs the real scan on this host: it must list this
// process with its own uid and parent.
func TestProcFSRowsReal(t *testing.T) {
	rows, err := procFS{root: procRootPath, self: os.Getpid()}.rows()
	if errors.Is(err, errProcHidden) {
		t.Skip("the proc mount here hides processes")
	}
	if err != nil {
		t.Fatalf("real scan: %v", err)
	}
	for _, r := range rows {
		if r.pid == os.Getpid() {
			if r.ppid != os.Getppid() || r.uids[0] != os.Getuid() || r.uids[1] != os.Geteuid() {
				t.Fatalf("own row = %+v", r)
			}
			return
		}
	}
	t.Fatalf("own pid %d not among %d rows", os.Getpid(), len(rows))
}

// staticTable is a procTable with fixed rows (or a fixed error).
type staticTable struct {
	list []procUserRow
	err  error
}

func (s staticTable) rows() ([]procUserRow, error) { return s.list, s.err }

func uids4(u int) [4]int { return [4]int{u, u, u, u} }

func TestProveNoUser(t *testing.T) {
	const uid, self = 10003, 500
	base := []procUserRow{
		{pid: 1, ppid: 0, uids: uids4(0)},
		{pid: 400, ppid: 1, uids: uids4(uid)}, // an ancestor with the uid: exempt
		{pid: self, ppid: 400, uids: uids4(uid)},
		{pid: 600, ppid: 1, uids: uids4(10002)},
	}
	with := func(r procUserRow) []procUserRow { return append(append([]procUserRow{}, base...), r) }
	tests := []struct {
		name  string
		table procTable
		want  string
	}{
		{"only self and ancestors", staticTable{list: base}, proofHeld},
		{"real uid", staticTable{list: with(procUserRow{pid: 700, ppid: 1, uids: [4]int{uid, 1, 1, 1}})}, proofUserAlive},
		{"effective uid", staticTable{list: with(procUserRow{pid: 700, ppid: 1, uids: [4]int{1, uid, 1, 1}})}, proofUserAlive},
		{"saved uid", staticTable{list: with(procUserRow{pid: 700, ppid: 1, uids: [4]int{1, 1, uid, 1}})}, proofUserAlive},
		{"fs uid", staticTable{list: with(procUserRow{pid: 700, ppid: 1, uids: [4]int{1, 1, 1, uid}})}, proofUserAlive},
		{"child of self", staticTable{list: with(procUserRow{pid: 701, ppid: self, uids: uids4(uid)})}, proofUserAlive},
		{"table error", staticTable{err: errProcHidden}, proofUnknown},
		{"self not listed", staticTable{list: base[:2]}, proofUnknown},
	}
	for _, tc := range tests {
		if got := proveNoUser(tc.table, uid, self); got != tc.want {
			t.Errorf("%s: proof = %q, want %q", tc.name, got, tc.want)
		}
	}
}

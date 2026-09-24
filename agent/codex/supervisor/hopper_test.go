package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// hopScript is one generation of a shell pid hopper: unless $D/stop exists or
// the generation budget $N is spent, it starts the next generation in the
// background (a NEW process: dash forks and execs sh), appends that child's
// pid to $D/pids, and exits at once, so the live hopper's pid changes every
// generation.
const hopScript = `[ -e "$D/stop" ] && exit 0
N=$((N - 1)); export N
[ "$N" -le 0 ] && exit 0
sh -c "$S" </dev/null >/dev/null 2>&1 &
echo $! >> "$D/pids"
exit 0`

// hopperTable is the REAL proc scan restricted to this process and the
// hopper's generations: the first generation's pid, every pid a generation
// wrote to the pids file (read AFTER the scan), and, within the same scan,
// every row whose parent is one of those (a child whose parent has not yet
// written its pid is still that parent's child). Every other process on the
// host is filtered out. Unlike filteredTable it never retakes an invalid
// scan: proveNoUser's own rescan rule is what is under test.
type hopperTable struct {
	first int
	dir   string
}

func (h hopperTable) rows() ([]procUserRow, error) {
	all, err := procFS{root: procRootPath, self: os.Getpid()}.rows()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(h.dir, "pids"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	hopper := map[int]bool{h.first: true}
	for _, f := range strings.Fields(string(data)) {
		if pid, err := strconv.Atoi(f); err == nil {
			hopper[pid] = true
		}
	}
	for grew := true; grew; {
		grew = false
		for _, r := range all {
			if !hopper[r.pid] && hopper[r.ppid] {
				hopper[r.pid], grew = true, true
			}
		}
	}
	var out []procUserRow
	for _, r := range all {
		if hopper[r.pid] || r.pid == os.Getpid() {
			out = append(out, r)
		}
	}
	return out, nil
}

// A live same-uid pid hopper (fork a new process, exit at once, repeat) must
// never leave the proof "held": a listed pid that vanishes before its status
// is read invalidates the scan (the rescan rule), because the hopper's next
// generation, created after the listing, is absent from it.
func TestProveNoUserNeverHeldWhileAHopperLives(t *testing.T) {
	if _, err := (filteredTable{}).rows(); errors.Is(err, errProcHidden) {
		t.Skip("the proc mount here hides processes")
	}
	dir := t.TempDir()
	cmd := exec.Command("/bin/sh", "-c", hopScript)
	cmd.Env = append(os.Environ(), "D="+dir, "N=200000", "S="+hopScript)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// The first generation exits at once; reap it.
	firstDone := make(chan error, 1)
	go func() { firstDone <- cmd.Wait() }()
	stop := func() {
		if err := os.WriteFile(filepath.Join(dir, "stop"), nil, 0o600); err != nil {
			t.Error(err)
		}
		// Every generation now exits without a successor; wait for the pids
		// file to stop growing.
		last := -1
		for range 200 {
			fi, err := os.Stat(filepath.Join(dir, "pids"))
			if err == nil && fi.Size() == int64(last) {
				return
			}
			if err == nil {
				last = int(fi.Size())
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Error("the hopper did not stop")
	}
	t.Cleanup(stop)
	if err := <-firstDone; err != nil {
		t.Fatalf("first generation: %v", err)
	}

	generations := func() int {
		data, _ := os.ReadFile(filepath.Join(dir, "pids"))
		return len(strings.Fields(string(data)))
	}
	for start := time.Now(); generations() < 5; time.Sleep(10 * time.Millisecond) {
		if time.Since(start) > 30*time.Second {
			t.Fatalf("the hopper made %d generations in 30s", generations())
		}
	}
	table := hopperTable{first: cmd.Process.Pid, dir: dir}
	uid := os.Geteuid()
	counts := map[string]int{}
	before := generations()
	for i := range 50 {
		if got := proveNoUser(table, uid, os.Getpid()); got == proofHeld {
			t.Fatalf("proof %d is held while the hopper lives (generation %d)", i, generations())
		} else {
			counts[got]++
		}
	}
	after := generations()
	t.Logf("proofs: %v; generations during them: %d", counts, after-before)
	if after-before < 2 {
		t.Fatalf("the hopper did not hop during the proofs (%d generations)", after-before)
	}
	if counts[proofUserAlive] == 0 {
		t.Fatalf("no proof saw the hopper: %v", counts)
	}
}

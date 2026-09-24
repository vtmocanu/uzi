package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
// hopper's generations, so unrelated processes on the host do not decide the
// proof. Unlike filteredTable it never retakes an invalid scan: proveNoUser's
// own rescan rule is what is under test.
//
// A row of the scan is a hopper generation only if, read right after the
// scan, its stat names the hopper's command ("sh") AND either its pid is one
// a generation recorded (the first generation's pid, or one written to the
// pids file) or its parent is a hopper generation (a child whose parent has
// not yet written its pid). So a recorded pid that the kernel has since
// reused for an unrelated process is not counted. The stats are read BEFORE
// the pids file: a child whose parent exited before its stat read (so its
// ppid no longer names it) had its pid written by that parent first.
//
// A row with the test's uid whose stat is already gone cannot be identified,
// so the scan is undecidable and rows returns errHopperUndecided: the proof is
// then "unknown", never "held", which keeps the held assertion exact.
type hopperTable struct {
	first int
	dir   string
}

var errHopperUndecided = errors.New("a same-uid row exited before it could be identified")

// hopperComm is the comm of every generation: the command hopScript runs.
const hopperComm = "sh"

func (h hopperTable) rows() ([]procUserRow, error) {
	self, uid := os.Getpid(), os.Geteuid()
	all, err := procFS{root: procRootPath, self: self}.rows()
	if err != nil {
		return nil, err
	}
	type ident struct {
		comm string
		ppid int
	}
	idents := map[int]ident{}
	for _, r := range all {
		if r.pid == self || !slices.Contains(r.uids[:], uid) {
			continue // another uid never decides the proof
		}
		stat, err := os.ReadFile(filepath.Join(procRootPath, strconv.Itoa(r.pid), "stat"))
		if err != nil {
			return nil, fmt.Errorf("stat of %d: %w", r.pid, errHopperUndecided)
		}
		open, closing := strings.IndexByte(string(stat), '('), strings.LastIndexByte(string(stat), ')')
		ppid, _, err := parseStatPPidPgid(string(stat))
		if open < 0 || closing < open || err != nil {
			return nil, fmt.Errorf("stat of %d: malformed", r.pid)
		}
		idents[r.pid] = ident{comm: string(stat[open+1 : closing]), ppid: ppid}
	}
	data, err := os.ReadFile(filepath.Join(h.dir, "pids"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	recorded := map[int]bool{h.first: true}
	for _, f := range strings.Fields(string(data)) {
		if pid, err := strconv.Atoi(f); err == nil {
			recorded[pid] = true
		}
	}
	hopper := map[int]bool{}
	for grew := true; grew; {
		grew = false
		for pid, id := range idents {
			if !hopper[pid] && id.comm == hopperComm && (recorded[pid] || hopper[id.ppid]) {
				hopper[pid], grew = true, true
			}
		}
	}
	var out []procUserRow
	for _, r := range all {
		if hopper[r.pid] || r.pid == self {
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
	// At least minProofs proofs, then more until one sees the hopper or the
	// budget is spent. Every one of them must not be "held"; "unknown" (an
	// invalid or undecidable scan, likelier on a busy host) is allowed, so the
	// liveness check waits for a "user_alive" instead of demanding it within
	// a fixed count.
	const minProofs, budget = 50, 10 * time.Second
	deadline := time.Now().Add(budget)
	for i := 0; i < minProofs || (counts[proofUserAlive] == 0 && time.Now().Before(deadline)); i++ {
		got := proveNoUser(table, uid, os.Getpid())
		if got == proofHeld {
			t.Fatalf("proof %d is held while the hopper lives (generation %d)", i, generations())
		}
		counts[got]++
	}
	after := generations()
	t.Logf("proofs: %v; generations during them: %d", counts, after-before)
	if after-before < 2 {
		t.Fatalf("the hopper did not hop during the proofs (%d generations)", after-before)
	}
	if counts[proofUserAlive] == 0 {
		t.Fatalf("no proof saw the hopper within %v: %v", budget, counts)
	}
}

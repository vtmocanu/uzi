package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func observationFixture(t *testing.T) string {
	t.Helper()
	if err := os.MkdirAll("../../../.uzi/scratch", 0755); err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp("../../../.uzi/scratch", "observation-proc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	for _, row := range []struct {
		pid, parent int
		start       string
		children    string
	}{
		{1, 0, "10", "2"}, {2, 1, "20", "3"}, {3, 2, "30", ""},
	} {
		dir := filepath.Join(root, strconv.Itoa(row.pid))
		if err := os.MkdirAll(filepath.Join(dir, "task", strconv.Itoa(row.pid)), 0700); err != nil {
			t.Fatal(err)
		}
		stat := fmt.Sprintf("%d (hostile ) comm) S %d 1 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 %s 0 2", row.pid, row.parent, row.start)
		if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "task", strconv.Itoa(row.pid), "children"), []byte(row.children), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}
func TestObservationContract(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		mutate       func(string)
	}{
		{"complete", "", func(string) {}},
		{"missing-link", "stale", func(root string) { _ = os.Remove(filepath.Join(root, "2", "stat")) }},
		{"unreadable", "unreadable", func(root string) {
			name := filepath.Join(root, "2", "stat")
			_ = os.Remove(name)
			_ = os.Mkdir(name, 0700)
		}},
		{"oversize", "oversize", func(root string) {
			_ = os.WriteFile(filepath.Join(root, "1", "task", "1", "children"), []byte(strings.Repeat("2 ", 3000)), 0600)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := observationFixture(t)
			tc.mutate(root)
			got := observe(root, processIdentity{1, "10"}, processIdentity{2, "20"}, time.Now().Add(time.Second))
			if got.Reason != tc.reason {
				t.Fatalf("%+v", got)
			}
			if tc.reason != "" {
				if got.State != "unavailable" || len(got.Processes) != 0 {
					t.Fatalf("%+v", got)
				}
				return
			}
			if got.State != "complete" || len(got.Processes) != 2 || got.Processes[0].StartTime != "20" || got.Processes[1].PPid != 2 || got.Processes[0].RSS != 2*uint64(os.Getpagesize()) {
				t.Fatalf("%+v", got)
			}
		})
	}
	root := observationFixture(t)
	if got := observe(root, processIdentity{1, "11"}, processIdentity{2, "20"}, time.Now().Add(time.Second)); got.Reason != "stale" {
		t.Fatalf("%+v", got)
	}
	if got := observe(root, processIdentity{1, "10"}, processIdentity{2, "21"}, time.Now().Add(time.Second)); got.Reason != "stale" {
		t.Fatalf("%+v", got)
	}
	if got := observe(root, processIdentity{1, "10"}, processIdentity{2, "20"}, time.Now().Add(-time.Second)); got.Reason != "timeout" {
		t.Fatalf("%+v", got)
	}
}

// The public observation seam must reject bounds before malformed rows beyond
// them can be read, and must never return a truncated successful sample.
func TestObservationBounds(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		children, padding, threads int
	}{
		{"file-bytes", 1, 4097, 0},
		{"aggregate-bytes", 20, 2000, 0},
		{"child-rows-and-queue", 257, 0, 0},
		{"thread-entries", 1, 0, 257},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := observationFixture(t)
			var children strings.Builder
			for pid := 2; pid < 2+tc.children; pid++ {
				fmt.Fprintf(&children, "%d ", pid)
				dir := filepath.Join(root, strconv.Itoa(pid))
				if err := os.MkdirAll(filepath.Join(dir, "task", strconv.Itoa(pid)), 0700); err != nil {
					t.Fatal(err)
				}
				stat := fmt.Sprintf("%d (fixture) S 1 1 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 %d 0 2", pid, pid*10)
				// A malformed first child proves the ID/row/queue limit fires
				// before child-row materialization when the list is too large.
				if tc.children > 256 {
					stat = "malformed"
				}
				if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat+strings.Repeat(" ", tc.padding)), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "task", strconv.Itoa(pid), "children"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(root, "1", "task", "1", "children"), []byte(children.String()), 0600); err != nil {
				t.Fatal(err)
			}
			for tid := 2; tid <= tc.threads; tid++ {
				dir := filepath.Join(root, "1", "task", strconv.Itoa(tid))
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "children"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			got := observe(root, processIdentity{1, "10"}, processIdentity{2, "20"}, time.Now().Add(time.Second))
			if got.State != "unavailable" || got.Reason != "oversize" || len(got.Processes) != 0 {
				t.Fatalf("%+v", got)
			}
		})
	}
	t.Run("remaining-row-budget-before-stat", func(t *testing.T) {
		root := observationFixture(t)
		var children strings.Builder
		for pid := 2; pid <= 257; pid++ {
			fmt.Fprintf(&children, "%d ", pid)
			dir := filepath.Join(root, strconv.Itoa(pid))
			if err := os.MkdirAll(filepath.Join(dir, "task", strconv.Itoa(pid)), 0700); err != nil {
				t.Fatal(err)
			}
			stat := fmt.Sprintf("%d (fixture) S 1 1 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 %d 0 2", pid, pid*10)
			if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "task", strconv.Itoa(pid), "children"), nil, 0600); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(root, "1", "task", "1", "children"), []byte(children.String()), 0600); err != nil {
			t.Fatal(err)
		}
		// All 256 rows are queued. This extra child has no stat: the zero
		// remaining budget must reject its ID before a read can report stale.
		if err := os.WriteFile(filepath.Join(root, "2", "task", "2", "children"), []byte("258"), 0600); err != nil {
			t.Fatal(err)
		}
		got := observe(root, processIdentity{1, "10"}, processIdentity{2, "20"}, time.Now().Add(time.Second))
		if got.Reason != "oversize" || len(got.Processes) != 0 {
			t.Fatalf("%+v", got)
		}
	})
}

func TestObservationEvidenceBounds(t *testing.T) {
	var buf bytes.Buffer
	ev := evidence{w: &buf}
	rows := make([]observationRow, 256)
	for i := range rows {
		rows[i] = observationRow{processIdentity{i + 2, "18446744073709551615"}, 1, ^uint64(0)}
	}
	for i := 1; i <= 300; i++ {
		start := buf.Len()
		if err := ev.writeObservation(observationEvidence{Event: "observe", ID: i, State: "complete", Supervisor: processIdentity{1, "10"}, Root: processIdentity{2, "20"}, Processes: rows}); err != nil {
			t.Fatal(err)
		}
		line := buf.Bytes()[start:]
		if len(line) > 65536 || len(line) == 0 {
			t.Fatalf("record bytes=%d", len(line))
		}
		var got observationEvidence
		if err := json.Unmarshal(line, &got); err != nil || len(got.Processes) != 256 {
			t.Fatalf("rows=%d err=%v", len(got.Processes), err)
		}
	}
	if ev.count != 0 {
		t.Fatalf("lifecycle count=%d", ev.count)
	}
	buf.Reset()
	if err := ev.writeObservation(observationEvidence{Event: "observe", State: "complete", Processes: append(rows, rows[0])}); err != nil {
		t.Fatal(err)
	}
	var got observationEvidence
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.State != "unavailable" || got.Reason != "oversize" || len(got.Processes) != 0 {
		t.Fatalf("%+v", got)
	}
}

func TestObservationControlDoesNotDrainOrSpendLifecycleBudget(t *testing.T) {
	var control strings.Builder
	for i := 1; i <= 300; i++ {
		fmt.Fprintf(&control, "{\"op\":\"observe\",\"id\":%d}\n", i)
	}
	control.WriteString("{\"op\":\"dispose\",\"id\":301}\n")
	sup, buf := newTestSupervisor(control.String(), scriptedReaper(reapEmpty))
	root := observationFixture(t)
	sup.seams.observe = func(deadline time.Time) observationEvidence {
		return observe(root, processIdentity{1, "10"}, processIdentity{2, "20"}, deadline)
	}
	calls := 0
	sup.seams.kill = func(processIdentity) error { calls++; return nil }
	if code := sup.run(2, procStatus{}); code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if calls != 0 {
		t.Fatalf("observation signaled %d children", calls)
	}
	decoder := json.NewDecoder(bytes.NewReader(buf.Bytes()))
	count := 0
	disposed := false
	for decoder.More() {
		var event map[string]any
		if err := decoder.Decode(&event); err != nil {
			t.Fatal(err)
		}
		if event["event"] == "observe" {
			count++
		}
		if event["event"] == "dispose" {
			disposed = true
		}
	}
	if count != 300 || !disposed {
		t.Fatalf("count=%d dispose=%v", count, disposed)
	}
}
func TestDrainEnumerationFailureCannotConfirmECHILD(t *testing.T) {
	deadline, clock, sleep := farClock()
	kill, calls := recordingKiller()
	got := drain(deadline, clock, sleep, listedChildren(func() ([]observationRow, error) { return nil, errObservationUnreadable }), kill, seqReaper())
	if got.State != stateUnconfirmed || got.Reason != "enumeration" || len(*calls) != 0 {
		t.Fatalf("%+v calls=%v", got, *calls)
	}
}
func TestDrainSecondEnumerationFailureCannotConfirmECHILD(t *testing.T) {
	for _, failure := range []error{errObservationUnreadable, errObservationOversize, errObservationGone} {
		deadline, clock, sleep := farClock()
		calls := 0
		children := listedChildren(func() ([]observationRow, error) {
			calls++
			if calls == 1 {
				return nil, nil
			}
			return nil, failure
		})
		kill, signals := recordingKiller()
		got := drain(deadline, clock, sleep, children, kill, seqReaper())
		if calls != 2 || got.State != stateUnconfirmed || got.Reason != "enumeration" || got.Authority != "" || len(*signals) != 0 {
			t.Fatalf("error=%v result=%+v enumerations=%d signals=%v", failure, got, calls, *signals)
		}
	}
}

func TestRealKillStartMismatchDoesNotSignal(t *testing.T) {
	cmd := exec.Command("/bin/sleep", "10")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	identity, err := readIdentity(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	wrong := identity
	wrong.StartTime = "1"
	if err := realKill(wrong); !errors.Is(err, errObservationStale) {
		t.Fatalf("mismatch=%v", err)
	}
	again, err := readIdentity(cmd.Process.Pid)
	if err != nil || again != identity {
		t.Fatalf("live identity=%+v err=%v", again, err)
	}
	if err := realKill(identity); err != nil {
		t.Fatal(err)
	}
}

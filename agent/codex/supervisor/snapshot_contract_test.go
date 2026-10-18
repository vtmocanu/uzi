package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func snapshotFixture(t *testing.T, count, padding int) string {
	t.Helper()
	root := observationFixture(t)
	write := func(name, data string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	children := []string{}
	for pid := count + 1; pid >= 1; pid-- {
		dir := strconv.Itoa(pid)
		if err := os.MkdirAll(filepath.Join(root, dir, "task", dir), 0700); err != nil {
			t.Fatal(err)
		}
		parent := 1
		if pid == 1 {
			parent = 0
		}
		write(dir+"/stat", fmt.Sprintf("%d (fixture) S %d 1 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 %d 0 2%s", pid, parent, pid*10, strings.Repeat(" ", padding)))
		write(dir+"/comm", "fixture\n")
		write(dir+"/task/"+dir+"/children", "")
		if pid != 1 {
			children = append(children, dir)
		}
	}
	write("1/task/1/children", strings.Join(children, " "))
	return root
}

func TestSnapshotSeparatesReportingFromStrictObservationLimits(t *testing.T) {
	for _, tc := range []struct{ count, padding int }{{128, 0}, {200, 0}, {100, 900}} {
		t.Run(fmt.Sprintf("children-%d-padding-%d", tc.count, tc.padding), func(t *testing.T) {
			root := snapshotFixture(t, tc.count, tc.padding)
			rows, err := walkDescendantsFrom(root, 1)
			if err != nil || len(rows) != tc.count {
				t.Fatalf("snapshot: rows=%d error=%v; want %d", len(rows), err, tc.count)
			}
			for i, row := range rows {
				if row != (procRow{Pid: i + 2, PPid: 1, Pgid: 1, Comm: "fixture"}) {
					t.Fatalf("sorted sanitized row %d: %+v", i, row)
				}
			}
			strict := observe(root, processIdentity{1, "10"}, processIdentity{2, "20"}, time.Now().Add(time.Second))
			if strict.State != "unavailable" || strict.Reason != "oversize" || len(strict.Processes) != 0 {
				t.Fatalf("same fixture must still refuse strict observation: %+v", strict)
			}
		})
	}
}

func TestSnapshotRetainsTwoHundredUniqueDescendantBound(t *testing.T) {
	root := snapshotFixture(t, 201, 0)
	if _, err := walkDescendantsFrom(root, 1); !errors.Is(err, errSnapshotTooLarge) {
		t.Fatalf("201 descendants error=%v; want snapshot bound", err)
	}
}

func TestSnapshotRetainsLegacyBestEffortRowsAndThreadDedup(t *testing.T) {
	root := snapshotFixture(t, 6, 0)
	// PID 7 is a grandchild, discovered only through a surviving reporting row.
	for name, value := range map[string]string{
		"1/task/1/children": "6 5 4 3 2",
		"4/task/4/children": "7",
		"7/stat":            "7 (fixture) S 4 1 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 70 0 2",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"2/stat", "4/comm"} {
		if err := os.Remove(filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "3/stat"), []byte("malformed"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "1/task/99"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "1/task/99/children"), []byte("6 5 4 6"), 0600); err != nil {
		t.Fatal(err)
	}
	rows, err := walkDescendantsFrom(root, 1)
	want := []procRow{{Pid: 4, PPid: 1, Pgid: 1, Comm: ""}, {Pid: 5, PPid: 1, Pgid: 1, Comm: "fixture"}, {Pid: 6, PPid: 1, Pgid: 1, Comm: "fixture"}, {Pid: 7, PPid: 4, Pgid: 1, Comm: "fixture"}}
	if err != nil || !reflect.DeepEqual(rows, want) {
		t.Fatalf("rows=%+v error=%v; want %+v", rows, err, want)
	}
	strict := observe(root, processIdentity{1, "10"}, processIdentity{2, "20"}, time.Now().Add(time.Second))
	if strict.State != "unavailable" || len(strict.Processes) != 0 {
		t.Fatalf("legacy best-effort rows never authorize observation: %+v", strict)
	}
}

func TestSnapshotWithinLegacyBoundDoesNotAbnormallyDispose(t *testing.T) {
	root := snapshotFixture(t, 128, 0)
	sup, buf := newTestSupervisor("{\"op\":\"snapshot\",\"id\":7}\n{\"op\":\"dispose\",\"id\":8}\n", scriptedReaper(reapEmpty))
	sup.seams.snapshot = func() ([]procRow, error) { return walkDescendantsFrom(root, 1) }
	if code := sup.run(2, procStatus{}); code != 0 {
		t.Fatalf("snapshot must not cause abnormal cleanup: code=%d evidence=%s", code, buf.String())
	}
	events := []string{}
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var event struct {
			Event     string    `json:"event"`
			Processes []procRow `json:"processes"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event.Event)
		if event.Event == "snapshot" && len(event.Processes) != 128 {
			t.Fatalf("snapshot rows=%d", len(event.Processes))
		}
	}
	if !reflect.DeepEqual(events, []string{"started", "snapshot", "dispose"}) {
		t.Fatalf("unexpected cleanup or missing snapshot: %v", events)
	}
}

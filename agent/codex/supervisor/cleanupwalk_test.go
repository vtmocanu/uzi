package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func cleanupFixture(t *testing.T, count int) string {
	t.Helper()
	root := t.TempDir()
	task := filepath.Join(root, "1", "task", "1")
	if err := os.MkdirAll(task, 0700); err != nil {
		t.Fatal(err)
	}
	var children strings.Builder
	for pid := 10; pid < 10+count; pid++ {
		fmt.Fprintf(&children, "%d ", pid)
		dir := filepath.Join(root, strconv.Itoa(pid))
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		stat := fmt.Sprintf("%d (child) S 1 1 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 %d 0 2", pid, pid)
		if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(task, "children"), []byte(children.String()), 0600); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestCleanupTraversalBeyondObservationLimit(t *testing.T) {
	root := cleanupFixture(t, 600)
	seen := 0
	err := visitCleanupChildren(root, 1, time.Now().Add(time.Second), func(row observationRow) error {
		want := 10 + seen
		if row.Pid != want || row.StartTime != strconv.Itoa(want) || row.PPid != 1 {
			t.Fatalf("identity=%+v want=%d", row, want)
		}
		seen++
		return nil
	})
	if err != nil || seen != 600 {
		t.Fatalf("visited=%d err=%v", seen, err)
	}
	// The strict read-only sampling limit is still enforced independently.
	if _, err := (&observationProc{root: root}).directLimit(1, observationItems); !errors.Is(err, errObservationOversize) {
		t.Fatalf("memory observation unexpectedly widened: %v", err)
	}
}

func TestCleanupTraversalStreamsLargeChildFile(t *testing.T) {
	root := cleanupFixture(t, 1200)
	data, err := os.ReadFile(filepath.Join(root, "1", "task", "1", "children"))
	if err != nil || len(data) <= 4096 {
		t.Fatalf("fixture must exceed the sampling per-file cap: bytes=%d err=%v", len(data), err)
	}
	seen := 0
	err = visitCleanupChildren(root, 1, time.Now().Add(time.Second), func(row observationRow) error {
		if row.Pid != 10+seen {
			t.Fatalf("lost or reordered child: %+v after %d", row, seen)
		}
		seen++
		return nil
	})
	if err != nil || seen != 1200 {
		t.Fatalf("visited=%d err=%v", seen, err)
	}
}

func TestCleanupTraversalStreamsAllThreadDirectories(t *testing.T) {
	root := cleanupFixture(t, 1)
	for tid := 1; tid <= 300; tid++ {
		dir := filepath.Join(root, "1", "task", strconv.Itoa(tid))
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		data := ""
		if tid == 300 {
			data = "10 "
		}
		if err := os.WriteFile(filepath.Join(dir, "children"), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	seen := []processIdentity{}
	err := visitCleanupChildren(root, 1, time.Now().Add(time.Second), func(row observationRow) error {
		seen = append(seen, row.processIdentity)
		return nil
	})
	if err != nil || len(seen) != 1 || seen[0] != (processIdentity{10, "10"}) {
		t.Fatalf("last-thread child=%+v err=%v", seen, err)
	}
	if _, err := (&observationProc{root: root}).children(1, observationItems); !errors.Is(err, errObservationOversize) {
		t.Fatalf("memory observation thread limit changed: %v", err)
	}
}

func TestCleanupTraversalRefusesBadIdentityAndMalformedInput(t *testing.T) {
	for _, mode := range []string{"parent", "malformed", "unreadable", "gone", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			root := cleanupFixture(t, 1)
			deadline := time.Now().Add(time.Second)
			switch mode {
			case "parent":
				p := filepath.Join(root, "10", "stat")
				data, _ := os.ReadFile(p)
				_ = os.WriteFile(p, []byte(strings.Replace(string(data), " S 1 1 ", " S 99 1 ", 1)), 0600)
			case "malformed":
				_ = os.WriteFile(filepath.Join(root, "1", "task", "1", "children"), []byte("not-a-pid"), 0600)
			case "unreadable":
				_ = os.Remove(filepath.Join(root, "10", "stat"))
				_ = os.Mkdir(filepath.Join(root, "10", "stat"), 0700)
			case "gone":
				_ = os.Remove(filepath.Join(root, "10", "stat"))
			case "deadline":
				deadline = time.Now().Add(-time.Second)
			}
			calls := 0
			err := visitCleanupChildren(root, 1, deadline, func(observationRow) error { calls++; return nil })
			if calls != 0 || (mode == "gone") != (err == nil) {
				t.Fatalf("mode=%s callbacks=%d err=%v", mode, calls, err)
			}
		})
	}
}

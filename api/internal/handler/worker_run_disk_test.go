package handler

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// PRD #1809 M6 (D8): the heartbeat's run_disk list and data-volume inode pair are parsed
// defensively inside the stats object. A junk list or entry must never drop the cpu/mem gauge,
// an absent list must read as "no report" (nil), and an array (even empty) as a real report.

const statsBase = `"mem_bytes":100,"source":"cgroup"`

func parseStatsJSON(t *testing.T, extra string) (json.RawMessage, uuid.UUID) {
	t.Helper()
	raw := json.RawMessage(`{` + statsBase + extra + `}`)
	return raw, uuid.New()
}

func TestParseWorkerStatsRunDiskAbsentIsNil(t *testing.T) {
	for _, extra := range []string{"", `,"run_disk":null`, `,"run_disk":"junk"`, `,"run_disk":{"run_id":"x"}`, `,"run_disk":42`} {
		raw, wid := parseStatsJSON(t, extra)
		st := parseWorkerStats(raw, wid)
		if st == nil {
			t.Fatalf("stats %s dropped whole; a bad run_disk must leave the gauge", raw)
		}
		if st.RunDisk != nil {
			t.Fatalf("stats %s: RunDisk = %#v, want nil (no report, rows untouched)", raw, st.RunDisk)
		}
		if st.MemBytes != 100 {
			t.Fatalf("stats %s: mem_bytes = %d, want 100", raw, st.MemBytes)
		}
	}
}

func TestParseWorkerStatsRunDiskEmptyArrayIsReport(t *testing.T) {
	raw, wid := parseStatsJSON(t, `,"run_disk":[]`)
	st := parseWorkerStats(raw, wid)
	if st == nil || st.RunDisk == nil || len(st.RunDisk) != 0 {
		t.Fatalf("empty run_disk = %#v, want a non-nil empty slice (a report that clears)", st)
	}
}

func TestParseWorkerStatsRunDiskValidAndJunkEntries(t *testing.T) {
	a, b, c := uuid.New(), uuid.New(), uuid.New()
	list := fmt.Sprintf(`[
		{"run_id":%q,"home_bytes":1000,"cache_bytes":400,"truncated":true},
		{"run_id":"not-a-uuid","home_bytes":1,"cache_bytes":1},
		{"run_id":%q,"home_bytes":-1,"cache_bytes":1},
		{"run_id":%q,"home_bytes":5,"cache_bytes":99999999999999999999},
		{"run_id":%q,"home_bytes":1.5,"cache_bytes":1},
		{"run_id":%q,"cache_bytes":1},
		"junk",
		{"run_id":%q,"home_bytes":2000,"cache_bytes":0,"truncated":"yes"},
		{"run_id":%q,"home_bytes":7,"cache_bytes":7},
		{"run_id":%q,"home_bytes":3000,"cache_bytes":1}
	]`, a, b, b, b, b, c, a, uuid.Nil)
	raw, wid := parseStatsJSON(t, `,"run_disk":`+list)
	st := parseWorkerStats(raw, wid)
	if st == nil {
		t.Fatal("stats dropped whole on junk run_disk entries")
	}
	if len(st.RunDisk) != 3 {
		t.Fatalf("RunDisk = %#v, want 3 valid entries (a, c, nil uuid)", st.RunDisk)
	}
	if st.RunDisk[0].RunID != a || st.RunDisk[0].HomeBytes != 1000 || st.RunDisk[0].CacheBytes != 400 || !st.RunDisk[0].Truncated {
		t.Fatalf("entry 0 = %#v, want a/1000/400/truncated", st.RunDisk[0])
	}
	// A non-boolean truncated reads as false, never as a reject.
	if st.RunDisk[1].RunID != c || st.RunDisk[1].HomeBytes != 2000 || st.RunDisk[1].Truncated {
		t.Fatalf("entry 1 = %#v, want c/2000/not truncated", st.RunDisk[1])
	}
	// The repeated run id a keeps its FIRST entry: the 7/7 duplicate is dropped.
	for _, e := range st.RunDisk {
		if e.RunID == a && e.HomeBytes != 1000 {
			t.Fatalf("duplicate run id replaced the first entry: %#v", e)
		}
	}
}

func TestParseWorkerStatsRunDiskCapped(t *testing.T) {
	var parts []string
	for i := 0; i < maxRunDiskEntries+25; i++ {
		parts = append(parts, fmt.Sprintf(`{"run_id":%q,"home_bytes":%d,"cache_bytes":0}`, uuid.New(), i))
	}
	raw, wid := parseStatsJSON(t, `,"run_disk":[`+strings.Join(parts, ",")+`]`)
	st := parseWorkerStats(raw, wid)
	if st == nil || len(st.RunDisk) != maxRunDiskEntries {
		t.Fatalf("RunDisk len = %d, want the cap %d", len(st.RunDisk), maxRunDiskEntries)
	}
	if st.RunDisk[0].HomeBytes != 0 || st.RunDisk[maxRunDiskEntries-1].HomeBytes != int64(maxRunDiskEntries-1) {
		t.Fatalf("cap kept the wrong entries: first %d last %d", st.RunDisk[0].HomeBytes, st.RunDisk[maxRunDiskEntries-1].HomeBytes)
	}
}

func TestParseWorkerStatsDataInodes(t *testing.T) {
	raw, wid := parseStatsJSON(t, `,"disk_data_inodes":7001,"disk_data_total_inodes":65536`)
	st := parseWorkerStats(raw, wid)
	if st == nil || st.DiskDataInodes == nil || *st.DiskDataInodes != 7001 || st.DiskDataTotalInodes == nil || *st.DiskDataTotalInodes != 65536 {
		t.Fatalf("data inodes = %#v, want 7001/65536", st)
	}
	// A bad inode value drops only that field.
	raw, wid = parseStatsJSON(t, `,"disk_data_inodes":-3,"disk_data_total_inodes":99999999999999999999`)
	st = parseWorkerStats(raw, wid)
	if st == nil {
		t.Fatal("bad inode fields dropped the whole stats object")
	}
	if st.DiskDataInodes != nil || st.DiskDataTotalInodes != nil {
		t.Fatalf("bad inode fields kept: %v / %v", st.DiskDataInodes, st.DiskDataTotalInodes)
	}
}

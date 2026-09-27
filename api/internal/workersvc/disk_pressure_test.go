package workersvc

import (
	"context"
	"testing"
)

// diskOverThreshold is the per-heartbeat "any volume at/above the disk-pressure
// threshold" predicate that drives the debounced stats_disk_pressure_streak (PRD #837
// M4). These cases pin the comparator boundary (>= fires, just-under does not), that
// EITHER volume can trip it, and the no-sample / div-by-zero guards that keep a nil or
// zero-total report from firing (or panicking).
func TestDiskOverThreshold(t *testing.T) {
	i := func(v int64) *int64 { return &v }
	const threshold = 0.90

	cases := []struct {
		name  string
		stats *WorkerStats
		want  bool
	}{
		{
			name:  "nix exactly at threshold fires (>= is inclusive)",
			stats: &WorkerStats{DiskNixBytes: i(900), DiskNixTotalBytes: i(1000)},
			want:  true,
		},
		{
			name:  "nix just under threshold does not fire",
			stats: &WorkerStats{DiskNixBytes: i(899), DiskNixTotalBytes: i(1000)},
			want:  false,
		},
		{
			name:  "data volume alone can trip it",
			stats: &WorkerStats{DiskDataBytes: i(950), DiskDataTotalBytes: i(1000)},
			want:  true,
		},
		{
			name:  "nix under but data over => over (any volume)",
			stats: &WorkerStats{DiskNixBytes: i(100), DiskNixTotalBytes: i(1000), DiskDataBytes: i(999), DiskDataTotalBytes: i(1000)},
			want:  true,
		},
		{
			name:  "nil used pointer => not over",
			stats: &WorkerStats{DiskNixBytes: nil, DiskNixTotalBytes: i(1000)},
			want:  false,
		},
		{
			name:  "nil total pointer => not over",
			stats: &WorkerStats{DiskNixBytes: i(900), DiskNixTotalBytes: nil},
			want:  false,
		},
		{
			name:  "zero total => not over (no div-by-zero)",
			stats: &WorkerStats{DiskNixBytes: i(900), DiskNixTotalBytes: i(0)},
			want:  false,
		},
		{
			// Issue #1759: dind is display-only, NEVER a disk_pressure input. A dind
			// volume at 99% bytes AND 99% inodes with nix/data low must not fire.
			name: "dind at 99% bytes and inodes, nix/data low => not over (display-only)",
			stats: &WorkerStats{
				DiskNixBytes: i(10), DiskNixTotalBytes: i(100),
				DiskDataBytes: i(10), DiskDataTotalBytes: i(100),
				DiskDindBytes: i(99), DiskDindTotalBytes: i(100),
				DiskDindInodes: i(99), DiskDindTotalInodes: i(100),
			},
			want: false,
		},
		{
			name: "dind full with nix/data absent => not over",
			stats: &WorkerStats{
				DiskDindBytes: i(100), DiskDindTotalBytes: i(100),
				DiskDindInodes: i(100), DiskDindTotalInodes: i(100),
			},
			want: false,
		},
		{
			name:  "both volumes absent => not over",
			stats: &WorkerStats{},
			want:  false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := diskOverThreshold(c.stats, threshold); got != c.want {
				t.Fatalf("diskOverThreshold = %v, want %v", got, c.want)
			}
		})
	}
}

// TestHeartbeatDindFullDoesNotIncrementPressureStreak drives the real Service.Heartbeat
// path (issue #1759): a sample whose dind volume is at 99% bytes and 99% inodes, with
// nix/data low, must reach HeartbeatWorker with the dind columns set AND
// DiskOverThreshold=false, which the SQL CASE turns into a streak RESET (never an
// increment). The positive control proves the same wiring does fire for /nix.
func TestHeartbeatDindFullDoesNotIncrementPressureStreak(t *testing.T) {
	i := func(v int64) *int64 { return &v }
	p := testParams()
	p.DiskPressureThreshold = 0.90

	fs := &fakeStore{}
	svc := New(fs, newBox(t), p)
	stats := &WorkerStats{
		MemBytes: 1, Source: "cgroup",
		DiskNixBytes: i(10), DiskNixTotalBytes: i(100),
		DiskDataBytes: i(10), DiskDataTotalBytes: i(100),
		DiskDindBytes: i(99), DiskDindTotalBytes: i(100),
		DiskDindInodes: i(99), DiskDindTotalInodes: i(100),
	}
	if _, err := svc.Heartbeat(context.Background(), worker(), stats, nil, nil); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	arg := fs.heartbeatArg
	if arg == nil {
		t.Fatal("HeartbeatWorker was not called")
	}
	if arg.StatsDiskDindBytes.Int64 != 99 || arg.StatsDiskDindTotalBytes.Int64 != 100 ||
		arg.StatsDiskDindInodes.Int64 != 99 || arg.StatsDiskDindTotalInodes.Int64 != 100 ||
		!arg.StatsDiskDindInodes.Valid || !arg.StatsDiskDindTotalInodes.Valid {
		t.Fatalf("dind sample not threaded into HeartbeatWorkerParams: %+v", arg)
	}
	if arg.DiskOverThreshold {
		t.Fatal("DiskOverThreshold = true for a dind-only full sample; dind must never feed disk_pressure")
	}

	// Positive control: the same path with /nix over threshold does fire.
	stats.DiskNixBytes = i(95)
	if _, err := svc.Heartbeat(context.Background(), worker(), stats, nil, nil); err != nil {
		t.Fatalf("heartbeat (control): %v", err)
	}
	if !fs.heartbeatArg.DiskOverThreshold {
		t.Fatal("control: /nix at 95% must set DiskOverThreshold")
	}
}

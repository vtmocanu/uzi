package workersvc

import "testing"

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

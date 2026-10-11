package workersvc

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// TestExtensionRoomSeconds pins the pure room arithmetic (#2735): room = MaxRunWallSeconds -
// wallUpper - max(finalize, WallParkFinalizeSeconds) - extension, clamped at 0, where wallUpper
// is the largest wall the run may be served (persisted wall, RunTimeout or the ceiling).
func TestExtensionRoomSeconds(t *testing.T) {
	null := pgtype.Int4{}
	set := func(v int32) pgtype.Int4 { return pgtype.Int4{Int32: v, Valid: true} }
	for _, tc := range []struct {
		name         string
		wall         pgtype.Int4
		ext, fin     int32
		timeout, cei time.Duration
		want         int
	}{
		{"defaults: 6h timeout, 24h ceiling, NULL wall", null, 0, 0, 6 * time.Hour, 24 * time.Hour, 2059283},
		{"72h ceiling", null, 0, 0, 6 * time.Hour, 72 * time.Hour, 1886483},
		{"persisted wall above config (72h wall, 24h ceiling)", set(259200), 0, 0, 6 * time.Hour, 24 * time.Hour, 1886483},
		{"persisted wall below config is ignored", set(3600), 0, 0, 6 * time.Hour, 24 * time.Hour, 2059283},
		{"existing extension reduces room", null, 57600, 0, 6 * time.Hour, 24 * time.Hour, 2059283 - 57600},
		{"finalize allowance already granted equals the reserve", null, 0, 1800, 6 * time.Hour, 24 * time.Hour, 2059283},
		{"finalize above the reserve", null, 0, 3600, 6 * time.Hour, 24 * time.Hour, 2059283 - 1800},
		{"timeout above ceiling", null, 0, 0, 30 * time.Hour, 24 * time.Hour, 2147483 - 108000 - 1800},
		{"clamps at zero", null, 2100000, 0, 6 * time.Hour, 24 * time.Hour, 0},
		{"clamps at zero when ext exceeds everything", null, 2147483, 0, 6 * time.Hour, 24 * time.Hour, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExtensionRoomSeconds(tc.wall, tc.ext, tc.fin, tc.timeout, tc.cei); got != tc.want {
				t.Fatalf("ExtensionRoomSeconds = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestMaxRunWallSecondsKeepsTimerInRange: the constant is the largest whole-second wall whose
// millisecond value still fits the worker's setTimeout range (2^31-1 ms).
func TestMaxRunWallSecondsKeepsTimerInRange(t *testing.T) {
	if MaxRunWallSeconds != 2147483 {
		t.Fatalf("MaxRunWallSeconds = %d, want 2147483", MaxRunWallSeconds)
	}
	if int64(MaxRunWallSeconds)*1000 > 2147483647 || int64(MaxRunWallSeconds+1)*1000 <= 2147483647 {
		t.Fatalf("MaxRunWallSeconds %d is not the largest second count with ms <= 2^31-1", MaxRunWallSeconds)
	}
}

func TestWallLimitLabel(t *testing.T) {
	for in, want := range map[int]string{2147483: "24d 20h 31m 23s", 3600: "1h", 0: "0s", -4: "0s", 90061: "1d 1h 1m 1s"} {
		if got := wallLimitLabel(in); got != want {
			t.Errorf("wallLimitLabel(%d) = %q, want %q", in, got, want)
		}
	}
}

package workersvc

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
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
		{"defaults: 6h timeout, 24h ceiling, NULL wall (reserves the 72h maximum ceiling)", null, 0, 0, 6 * time.Hour, 24 * time.Hour, 1886483},
		{"72h ceiling", null, 0, 0, 6 * time.Hour, 72 * time.Hour, 1886483},
		{"persisted wall above config (72h wall, 24h ceiling)", set(259200), 0, 0, 6 * time.Hour, 24 * time.Hour, 1886483},
		{"persisted wall below config is ignored", set(3600), 0, 0, 6 * time.Hour, 24 * time.Hour, 1886483},
		{"existing extension reduces room", null, 57600, 0, 6 * time.Hour, 24 * time.Hour, 1886483 - 57600},
		{"finalize allowance already granted equals the reserve", null, 0, 1800, 6 * time.Hour, 24 * time.Hour, 1886483},
		{"finalize above the reserve", null, 0, 3600, 6 * time.Hour, 24 * time.Hour, 1886483 - 1800},
		{"timeout above ceiling but under 72h", null, 0, 0, 30 * time.Hour, 24 * time.Hour, 1886483},
		{"timeout above 72h is the uncovered case", null, 0, 0, 100 * time.Hour, 100 * time.Hour, 2147483 - 360000 - 1800},
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

// TestMaxRunWallSecondsKeepsTimerInRange: the extension-admission limit keeps its value, the largest
// whole-second wall whose millisecond value fits 2^31-1 ms (the native timer range the worker now
// re-arms within in chunks, #2761).
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

// TestMaxRunWallCeilingMatchesConfigValidator: MaxRunWallCeiling must equal the largest
// RUN_WALL_CEILING config.Load accepts. workersvc imports config (so config's own tests cannot
// import workersvc), hence the equality lives here. A raised validator bound without this constant
// would let an operator-raised ceiling outgrow the room the extend bound reserved.
func TestMaxRunWallCeilingMatchesConfigValidator(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://uzi:pw@db:5432/uzi?sslmode=disable")
	t.Setenv("JWT_SECRET", "unit-test-jwt-signing-key-not-a-real-secret")
	key := make([]byte, secretbox.KeySize)
	for i := range key {
		key[i] = byte(i + 1)
	}
	t.Setenv("UZI_SECRET_KEY", base64.StdEncoding.EncodeToString(key))
	t.Setenv("RUN_TIMEOUT", "4h")
	t.Setenv("RUN_WALL_CEILING", MaxRunWallCeiling.String())
	if _, err := config.Load(); err != nil {
		t.Fatalf("config.Load refuses RUN_WALL_CEILING=%v (MaxRunWallCeiling): %v", MaxRunWallCeiling, err)
	}
	t.Setenv("RUN_WALL_CEILING", (MaxRunWallCeiling + time.Second).String())
	if _, err := config.Load(); err == nil || !strings.Contains(err.Error(), "RUN_WALL_CEILING") {
		t.Fatalf("config.Load accepts RUN_WALL_CEILING above MaxRunWallCeiling (err=%v): raise the constant with the validator", err)
	}
}

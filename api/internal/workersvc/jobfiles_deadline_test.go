package workersvc

import (
	"testing"
	"time"
)

// TestJobFileLimitsUploadDeadline: the read deadline of an upload route scales with the declared
// size (100 KiB/s), never drops below RequestDeadline and never exceeds what the per-file cap needs
// (256 s at the default 25 MiB), so a maximum-size upload is not cut off at the plain 120 s.
func TestJobFileLimitsUploadDeadline(t *testing.T) {
	l := JobFileLimits{}.withDefaults()
	const mib = 1 << 20
	for _, c := range []struct {
		name     string
		declared int64
		fileMax  int64
		want     time.Duration
	}{
		{"empty keeps the request deadline", 0, 25 * mib, 120 * time.Second},
		{"negative counts as zero", -5, 25 * mib, 120 * time.Second},
		{"1 MiB is under the floor", mib, 25 * mib, 120 * time.Second},
		{"10 MiB is 103 s, still under the floor", 10 * mib, 25 * mib, 120 * time.Second},
		{"12 MiB scales past the floor", 12 * mib, 25 * mib, 123 * time.Second},
		{"25 MiB is 256 s", 25 * mib, 25 * mib, 256 * time.Second},
		{"a declared size over the cap is bounded by the cap", 500 * mib, 25 * mib, 256 * time.Second},
	} {
		if got := l.UploadDeadline(c.declared, c.fileMax); got != c.want {
			t.Errorf("%s: UploadDeadline(%d, %d) = %s, want %s", c.name, c.declared, c.fileMax, got, c.want)
		}
	}
	// The scaled deadline is what Write honours: never shorter than the plain one.
	short := JobFileLimits{RequestDeadline: 10 * time.Second}.withDefaults()
	if got := short.UploadDeadline(1, 25*mib); got != 10*time.Second {
		t.Errorf("small file under a 10 s RequestDeadline = %s, want 10s", got)
	}
}

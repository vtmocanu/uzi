package schedsvc

import "testing"

// TestSelfImproveCronFromInterval pins the legacy selfimprove_interval -> cron mapping
// the boot migration uses: whole-day multiples 1..31 become "0 4 */N * *", everything
// else (sub-day, >31 days, unparseable, non-positive) falls back to the catalog default.
func TestSelfImproveCronFromInterval(t *testing.T) {
	const fallback = "0 4 */2 * *"
	cases := []struct {
		interval string
		want     string
	}{
		{"48h", "0 4 */2 * *"}, // 2 days
		{"24h", "0 4 */1 * *"}, // 1 day
		{"72h", "0 4 */3 * *"}, // 3 days
		{"90m", fallback},      // sub-day, not a whole-day multiple
		{"garbage", fallback},  // unparseable
		{"0s", fallback},       // non-positive
		{"800h", fallback},     // >31 days (33.3 days)
	}
	for _, c := range cases {
		if got := selfImproveCronFromInterval(c.interval, fallback); got != c.want {
			t.Errorf("selfImproveCronFromInterval(%q) = %q, want %q", c.interval, got, c.want)
		}
	}
}

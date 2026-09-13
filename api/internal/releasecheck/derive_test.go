package releasecheck

import (
	"testing"
	"time"
)

// TestUpdateAvailable pins the read-time compare through the re-prefix + IsValid
// guard (PRD #836 M1): the running version is served BARE, the tag is v-prefixed, and
// the guard handles both. The genuinely-behind pair (0.11.0 vs v0.11.7) is the
// DISCRIMINATING fixture — an all-current pair passes even against a broken compare,
// so it cannot stand alone.
func TestUpdateAvailable(t *testing.T) {
	cases := []struct {
		name    string
		running string
		latest  string
		want    bool
	}{
		{"newer upstream", "0.14.0", "v0.15.0", true},
		{"genuinely behind (discriminating)", "0.11.0", "v0.11.7", true},
		{"equal versions", "0.14.0", "v0.14.0", false},
		{"running ahead", "0.15.0", "v0.14.0", false},
		{"malformed running (dev)", "dev", "v0.15.0", false},
		{"empty running", "", "v0.15.0", false},
		{"malformed latest", "0.14.0", "not-a-version", false},
		{"both malformed", "dev", "nightly", false},
		// Prerelease running versions (PRD 1265 M5). Under the RC-first train the running
		// version is routinely an RC while a candidate is deployed; latest stays a stable
		// (releases/latest never returns a prerelease). SemVer precedence orders
		// X.Y.Z-rc.N < X.Y.Z, and these pin that no surface can be misled.
		{"running an RC ahead of latest stable", "0.84.0-rc.1", "v0.83.0", false},
		{"running an RC of the just-promoted version (update)", "0.83.0-rc.2", "v0.83.0", true},
		{"stable running is newer than its own RC", "0.83.0", "v0.83.0-rc.9", false},
		{"running an earlier RC than the latest stable RC-less", "0.83.0-rc.1", "v0.84.0", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := UpdateAvailable(tc.running, tc.latest); got != tc.want {
				t.Errorf("UpdateAvailable(%q, %q) = %v, want %v", tc.running, tc.latest, got, tc.want)
			}
		})
	}
}

// TestFarBehind pins D4's server-side heuristic: far_behind requires update_available
// AND (majorGap>=1 OR minorGap>=3 OR published_at >= 30 days ago); an unparseable
// published_at makes only the age clause false (never fail-open); a near-current
// update never raises it.
func TestFarBehind(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	recent := now.Add(-2 * 24 * time.Hour).Format(time.RFC3339) // 2 days old
	old := now.Add(-45 * 24 * time.Hour).Format(time.RFC3339)   // 45 days old
	dayOld := now.Add(-1 * 24 * time.Hour).Format(time.RFC3339) // published "today-ish"

	cases := []struct {
		name        string
		running     string
		latest      string
		publishedAt string
		want        bool
	}{
		{"not behind at all", "0.14.0", "v0.14.0", recent, false},
		{"one patch ahead, published recently", "0.14.0", "v0.14.1", dayOld, false},
		{"major gap", "0.14.0", "v1.0.0", recent, true},
		{"minor gap >= 3", "0.14.0", "v0.17.0", recent, true},
		{"minor gap == 2 (under threshold), recent", "0.14.0", "v0.16.0", recent, false},
		{"small gap but 30+ days old", "0.14.0", "v0.14.1", old, true},
		{"behind but published_at unparseable → age clause false", "0.14.0", "v0.14.1", "garbage", false},
		{"behind, empty published_at → age clause false", "0.14.0", "v0.14.1", "", false},
		{"up to date is never far behind even if old string present", "0.14.0", "v0.14.0", old, false},
		// Prerelease running versions (PRD 1265 M5): the major/minor math uses the base
		// (semver.MajorMinor strips the -rc.N), so an RC of the same minor as latest is a
		// small gap, and an RC several minors behind is far behind.
		{"RC of the just-promoted version is a small, recent gap", "0.14.0-rc.2", "v0.14.0", recent, false},
		{"RC several minors behind is far behind", "0.14.0-rc.1", "v0.17.0", recent, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := FarBehind(tc.running, tc.latest, tc.publishedAt, now); got != tc.want {
				t.Errorf("FarBehind(%q, %q, %q) = %v, want %v", tc.running, tc.latest, tc.publishedAt, got, tc.want)
			}
		})
	}
}

// TestSecurity pins the D5 heuristic: a line beginning "### Security" (tolerant of
// trailing text) → true; a body without one → false.
func TestSecurity(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"has ### Security heading", "### Added\n- x\n\n### Security\n- fix CVE", true},
		{"heading with trailing text", "### Security fixes\n- patched", true},
		{"heading with leading whitespace", "  ### Security\n- x", true},
		{"no security section", "### Added\n- feature\n\n### Fixed\n- bug", false},
		{"mention in prose is not a heading", "This release has security relevant notes.", false},
		{"empty body", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Security(tc.body); got != tc.want {
				t.Errorf("Security(%q) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}

// Package releasecheck polls the GitHub Releases API for the newest uzi release,
// persists the remote facts to app_settings, and derives "update available" / "far
// behind" / "security" at READ time with zero egress. It mirrors the agent-source
// poll→persist→derive core (api/internal/agentsource), a different target: a
// compile-time-constant, instance-global, unauthenticated (by default) endpoint
// rather than a per-user configured git source.
package releasecheck

import (
	"strconv"
	"strings"
	"time"

	"golang.org/x/mod/semver"
)

// farBehindMinorGap is the minor-version distance at or above which an update is
// treated as "far behind" (PRD #836 D4). farBehindAge is the published-age threshold
// with the same effect. Both are v1 defaults, tunable here.
const (
	farBehindMinorGap = 3
	farBehindAge      = 30 * 24 * time.Hour
)

// UpdateAvailable reports whether latestTag is a strictly-newer release than the
// running version (PRD #836 M1). It is PURE (no egress, no DB): the running version
// is served BARE ("0.14.0") and the tag is v-prefixed ("v0.14.0"); semverNewer
// re-prefixes and IsValid-guards BOTH operands, so a malformed or "dev" running
// version reads as "unknown" (false), never "behind". This is the ONLY place the
// semver compare lives — every surface renders from this boolean, never a client-side
// string compare (PRD D4).
func UpdateAvailable(runningVersion, latestTag string) bool {
	return semverNewer(latestTag, runningVersion)
}

// FarBehind reports whether the running version is far enough behind latestTag to
// warrant the escalation banner (PRD #836 D4). It is PURE and side-effect-free.
//
// far_behind == update_available AND (majorGap >= 1 OR minorGap >= farBehindMinorGap
// OR published_at is at least farBehindAge before now). majorGap/minorGap come from
// semver.Major / semver.MajorMinor on the re-prefixed strings; an unparseable
// published_at makes the age clause FALSE (fail-closed, never fail-open). A running
// version that is not strictly behind is never far behind.
func FarBehind(runningVersion, latestTag, publishedAt string, now time.Time) bool {
	if !UpdateAvailable(runningVersion, latestTag) {
		return false
	}
	// UpdateAvailable being true guarantees both operands are valid semver, so the
	// re-prefixed forms below are safe to feed to semver.Major / MajorMinor.
	cr := "v" + strings.TrimPrefix(runningVersion, "v")
	cl := "v" + strings.TrimPrefix(latestTag, "v")

	// majorGap >= 1: latest is strictly newer, so differing majors means latest's
	// major is the greater one.
	if semver.Major(cl) != semver.Major(cr) {
		return true
	}

	// minorGap >= farBehindMinorGap, computed within the shared major.
	if rMinor, rok := minorOf(cr); rok {
		if lMinor, lok := minorOf(cl); lok {
			if lMinor-rMinor >= farBehindMinorGap {
				return true
			}
		}
	}

	// published_at at least farBehindAge before now. Unparseable → clause is false.
	if published, err := time.Parse(time.RFC3339, strings.TrimSpace(publishedAt)); err == nil {
		if now.Sub(published) >= farBehindAge {
			return true
		}
	}
	return false
}

// Security reports whether the persisted release body advertises a security fix (PRD
// #836 D5): a line beginning with "### Security" after trimming surrounding
// whitespace. It is deliberately tolerant of trailing text ("### Security fixes")
// by matching the prefix rather than the exact heading, mirroring the Keep-a-Changelog
// `### Security` subsection the release pipeline emits verbatim into each release
// body. PURE — a plain scan of the persisted body, no egress.
func Security(body string) bool {
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "### Security") {
			return true
		}
	}
	return false
}

// semverNewer reports whether tag a is a strictly-greater semver than tag b, guarding
// BOTH operands with the re-prefix + IsValid discipline (PRD #836, mirroring
// agentsource.semverNewer): x/mod/semver treats a malformed version as equal (Compare
// returns 0), so an unguarded compare fails silently OPEN — it reports "up to date"
// for a pair that is actually behind. Either operand not being valid semver → false.
func semverNewer(a, b string) bool {
	ca := "v" + strings.TrimPrefix(a, "v")
	cb := "v" + strings.TrimPrefix(b, "v")
	if !semver.IsValid(ca) || !semver.IsValid(cb) {
		return false
	}
	return semver.Compare(ca, cb) > 0
}

// minorOf extracts the numeric minor component of a valid, v-prefixed semver string
// via semver.MajorMinor ("v0.14.0" → 14). It returns ok=false if the value is not
// valid semver or the minor is not an integer, so a caller treats an unparseable
// input as "no minor gap" rather than a spurious zero.
func minorOf(v string) (int, bool) {
	mm := strings.TrimPrefix(semver.MajorMinor(v), "v") // "0.14"
	parts := strings.SplitN(mm, ".", 2)
	if len(parts) != 2 {
		return 0, false
	}
	n, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, false
	}
	return n, true
}

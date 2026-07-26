package workersvc

import (
	"fmt"
	"strings"

	"golang.org/x/mod/semver"
)

// Derived per-worker upgrade status (PRD #113). One closed enum on the worker DTO,
// computed at read time from the worker's self-reported version against the control
// plane's own release — nothing here is persisted.
//
// M2 emits three of the five: up_to_date, outdated, unknown. The other two are
// roll-health states that only a controller report can justify, so they are declared
// here (the enum is the wire contract the web and CLI switch on) and produced by M4's
// fold, not by version comparison. A version compare alone can never see a stuck pod.
const (
	UpgradeStatusUpToDate      = "up_to_date"
	UpgradeStatusOutdated      = "outdated"
	UpgradeStatusUnknown       = "unknown"
	UpgradeStatusUpgrading     = "upgrading"      // M4 (controller roll-health)
	UpgradeStatusUpgradeFailed = "upgrade_failed" // M4 (controller roll-health)
)

// legacyFrozenAgentVersion is the hardcoded default every agent image reported before
// PRD #113 M1 retired it. It is treated as NO REPORT, and that is a correctness
// requirement rather than politeness: it is VALID semver that sorts below every
// release (measured: semver.IsValid("v0.1.0-m4") is true and
// semver.Compare("v0.1.0-m4","v0.11.7") is -1), so it slips past the unparseable
// guard and classifies `outdated`. Every worker still running a pre-M1 image reports
// it, so without this case the first release carrying M2 would mark the entire fleet
// outdated — the fleet-wide false alert this PRD exists to prevent, delivered by the
// PRD itself.
//
// Sunset: this can go once no pre-M1 image can still be running. It cannot be dated
// from here (an external worker upgrades whenever its owner chooses), so it stays
// until someone can show the fleet is clear.
const legacyFrozenAgentVersion = "0.1.0-m4"

// normSemver puts a bare release coordinate into the form golang.org/x/mod/semver
// requires. Everything uzi stamps is BARE — api/Dockerfile strips the leading v
// deliberately, deploy/chart/Chart.yaml has none, and PRD #113 M1 stamps the agent
// bare to match — while x/mod/semver requires the v and, crucially, does NOT error
// without it: it reports the version invalid, and Compare treats two invalid
// versions as EQUAL. So an un-normalized comparison of the real production strings
// returns 0 for every pair (measured: Compare("0.11.0","0.11.7") == 0), every worker
// reads up_to_date forever, and the feature fails silently OPEN. Never call
// semver.Compare on a raw stored string.
//
// Same wrinkle, same fix as forge/forgejo.go's checkForgejoVersion — see its comment.
func normSemver(v string) string {
	return "v" + strings.TrimPrefix(strings.TrimSpace(v), "v")
}

// ClassifyUpgrade derives a worker's upgrade status from the version it reported at
// register against target, the release the control plane is running. Both are the
// bare coordinate; normalization happens here.
//
// The rule order is PRD #113 Decision 8 and the design's decision table, first match
// wins. The two guards that look defensive and are not:
//
//   - An unparseable TARGET (a "dev" control plane, i.e. any non-release build) turns
//     classification off fleet-wide rather than comparing against garbage. IsValid("vdev")
//     is false, so this is the compose and local-build path.
//   - An unparseable REPORTED version is `unknown`, never `outdated`. Dropping this
//     guard does not merely lose precision, it inverts the answer: semver.Compare
//     sorts an INVALID operand BELOW a valid one (measured: Compare("v0.11.7.1","v0.11.7")
//     is -1 while IsValid("v0.11.7.1") is false), so garbage would confidently
//     classify as behind.
//
// Build metadata needs no handling: SemVer section 10 excludes it from precedence and
// x/mod/semver implements that, so M1's `<release>+g<short-sha>` stamp compares equal
// to the bare release it was built from.
//
// M4 extends this with the controller roll-health rows (R1/R2/R3/R8), which need the
// persisted signal, the worker kind and a clock. It will take a struct then; the two
// arguments here are what the version-compare rows actually read, and an input
// carrying fields no rule consults would be a claim that they are honoured.
func ClassifyUpgrade(reported, target string) (status string, detail string) {
	reported = strings.TrimSpace(reported)
	target = strings.TrimSpace(target)

	// R4 — the control plane has no release coordinate to compare against.
	if !semver.IsValid(normSemver(target)) {
		return UpgradeStatusUnknown, "control plane has no version stamp"
	}
	// R5 — the worker reported nothing usable. The legacy literal is folded in here
	// because it IS parsable; see legacyFrozenAgentVersion.
	if reported == legacyFrozenAgentVersion {
		return UpgradeStatusUnknown, "worker predates version stamping; upgrade the image to report one"
	}
	if reported == "" || !semver.IsValid(normSemver(reported)) {
		return UpgradeStatusUnknown, "worker reports no usable version"
	}

	switch cmp := semver.Compare(normSemver(reported), normSemver(target)); {
	case cmp > 0:
		// R6 — ahead of the control plane (a pinned tag, or a hand-built image) is
		// never an alert. Decision 8.
		return UpgradeStatusUpToDate, fmt.Sprintf("running %s, ahead of %s", reported, target)
	case cmp == 0:
		// R7 — the expected steady state, and the one that carries no detail.
		return UpgradeStatusUpToDate, ""
	default:
		// R9 — genuinely behind.
		return UpgradeStatusOutdated, fmt.Sprintf("running %s, target %s", reported, target)
	}
}

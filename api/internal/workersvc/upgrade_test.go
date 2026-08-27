package workersvc

import (
	"strings"
	"testing"
	"time"
)

// PRD #113 M2. The version-compare classification, driven ONLY by the bare forms the
// system actually stores.
//
// EVERY fixture here is bare on purpose, and a `v`-prefixed one would be a defect
// rather than extra coverage: api/Dockerfile strips the leading v, Chart.yaml has
// none, and M1 stamps the agent bare. A v-prefixed fixture passes against an
// implementation that never normalizes, i.e. against a production-dead feature.
//
// The discriminating row is BEHIND, not equal. semver.Compare returns 0 for two
// INVALID versions, and 0 is also the right answer for two equal ones, so an
// all-equal fixture set is green against a classifier that never normalizes at all.
// ("0.11.0", "0.11.7") is the row that separates them.
func TestClassifyUpgrade(t *testing.T) {
	cases := []struct {
		name       string
		reported   string
		target     string
		wantStatus string
		wantDetail string // substring; "" means the detail must be empty
	}{
		{
			name: "behind is outdated — THE discriminating row: without normalization this " +
				"compares 0 and reads up_to_date",
			reported: "0.11.0", target: "0.11.7",
			wantStatus: UpgradeStatusOutdated, wantDetail: "running 0.11.0, target 0.11.7",
		},
		{
			name:     "equal is up_to_date, and carries no detail",
			reported: "0.11.7", target: "0.11.7",
			wantStatus: UpgradeStatusUpToDate, wantDetail: "",
		},
		{
			name:     "ahead is up_to_date, never outdated (Decision 8)",
			reported: "0.12.0", target: "0.11.7",
			wantStatus: UpgradeStatusUpToDate, wantDetail: "ahead of 0.11.7",
		},
		{
			name:     "the retired frozen default is treated as no report, not as outdated",
			reported: legacyFrozenAgentVersion, target: "0.11.7",
			wantStatus: UpgradeStatusUnknown, wantDetail: "predates version stamping",
		},
		{
			name:     "a dev control plane disables classification fleet-wide",
			reported: "0.11.7", target: "dev",
			wantStatus: UpgradeStatusUnknown, wantDetail: "control plane has no version stamp",
		},
		{
			name:     "an unstamped worker (M1's empty default) is unknown",
			reported: "", target: "0.11.7",
			wantStatus: UpgradeStatusUnknown, wantDetail: "worker reports no usable version",
		},
		{
			name:     "garbage is unknown, NOT outdated — an invalid operand sorts below a valid one",
			reported: "0.11.7.1", target: "0.11.7",
			wantStatus: UpgradeStatusUnknown, wantDetail: "worker reports no usable version",
		},
		{
			name:     "a non-version string is unknown",
			reported: "not-a-version", target: "0.11.7",
			wantStatus: UpgradeStatusUnknown, wantDetail: "worker reports no usable version",
		},
		{
			// M1 stamps <release>+g<short-sha>. SemVer section 10 excludes build metadata
			// from precedence, so this must read up_to_date against the bare release it
			// was built from. This is the row that ties M1's stamp format to M2's compare.
			name:     "M1's +g<sha> build metadata compares equal to the bare release",
			reported: "0.11.7+g1a2b3c4", target: "0.11.7",
			wantStatus: UpgradeStatusUpToDate, wantDetail: "",
		},
		{
			name:     "build metadata does not rescue a behind version",
			reported: "0.11.0+g1a2b3c4", target: "0.11.7",
			wantStatus: UpgradeStatusOutdated, wantDetail: "target 0.11.7",
		},
		{
			name:     "both sides unstamped is unknown via the target guard, not a 0 compare",
			reported: "", target: "dev",
			wantStatus: UpgradeStatusUnknown, wantDetail: "control plane has no version stamp",
		},
		{
			name:     "a prerelease sorts below its release",
			reported: "0.11.7-rc.1", target: "0.11.7",
			wantStatus: UpgradeStatusOutdated, wantDetail: "running 0.11.7-rc.1, target 0.11.7",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, detail := classifyNoSignal(tc.reported, tc.target)
			if status != tc.wantStatus {
				t.Errorf("ClassifyUpgrade(%q, %q) status = %q, want %q (detail %q)",
					tc.reported, tc.target, status, tc.wantStatus, detail)
			}
			if tc.wantDetail == "" {
				if detail != "" {
					t.Errorf("ClassifyUpgrade(%q, %q) detail = %q, want empty",
						tc.reported, tc.target, detail)
				}
				return
			}
			if !strings.Contains(detail, tc.wantDetail) {
				t.Errorf("ClassifyUpgrade(%q, %q) detail = %q, want it to contain %q",
					tc.reported, tc.target, detail, tc.wantDetail)
			}
		})
	}
}

// The normalizer is the difference between a working feature and one that silently
// answers up_to_date for every worker forever, so it gets its own assertion against
// the literal production strings rather than being covered only in aggregate.
func TestNormSemverIsWhatMakesTheBareFormsComparable(t *testing.T) {
	// Guard the guard: if this ever stops holding, the table above stops discriminating.
	if got := normSemver("0.11.0"); got != "v0.11.0" {
		t.Fatalf("normSemver(\"0.11.0\") = %q, want \"v0.11.0\"", got)
	}
	if got := normSemver("v0.11.0"); got != "v0.11.0" {
		t.Fatalf("normSemver is not idempotent on an already-prefixed value: %q", got)
	}
	if got := normSemver("  0.11.0  "); got != "v0.11.0" {
		t.Fatalf("normSemver does not trim: %q", got)
	}
}

// A worker's reported version is untrusted input: any holder of a join token can send
// any 64 bytes. The invariant is narrow and exact — an UNPARSEABLE version must never
// reach a comparison, because semver.Compare sorts an invalid operand below a valid
// one and would report it `outdated` with full confidence.
func TestClassifyUpgradeUnparseableReportedVersionIsNeverCompared(t *testing.T) {
	for _, reported := range []string{
		"v", "vdev", "dev", "-1.0.0", "0.11.7\n0.11.8", "%s", "'; DROP TABLE workers; --",
	} {
		status, detail := classifyNoSignal(reported, "0.11.7")
		if status != UpgradeStatusUnknown {
			t.Errorf("ClassifyUpgrade(%q, \"0.11.7\") = %q (%q), want unknown: an unparseable version "+
				"must not be compared, since semver.Compare sorts an invalid operand BELOW a valid one",
				reported, status, detail)
		}
	}
}

// A PARTIAL version is a different thing from an unparseable one, and conflating them
// is the easy mistake here. x/mod/semver accepts "v0.11" and canonicalizes it to
// "v0.11.0" (measured: IsValid true, Canonical "v0.11.0", Compare against "v0.11.7"
// is -1), so a worker reporting "0.11" really is behind and `outdated` is the correct
// answer, not a false alert. Pinned because it looks like a gap in the unparseable
// guard and is not: a future reader who "fixes" it turns a true signal into `unknown`.
//
// Note the deliberate asymmetry with the release-tag gate in .github/workflows/release.yml, which
// REJECTS a partial vX.Y. That gate validates a tag we produce, where the full triple
// must equal the chart version; this classifies a string a worker reports, where the
// only question is whether it can be ordered.
func TestClassifyUpgradePartialVersionsAreValidAndOrdered(t *testing.T) {
	for _, tc := range []struct{ reported, want string }{
		{"0.11", UpgradeStatusOutdated}, // == 0.11.0, behind 0.11.7
		{"0", UpgradeStatusOutdated},    // == 0.0.0
		{"999", UpgradeStatusUpToDate},  // == 999.0.0, ahead
	} {
		if status, detail := classifyNoSignal(tc.reported, "0.11.7"); status != tc.want {
			t.Errorf("ClassifyUpgrade(%q, \"0.11.7\") = %q (%q), want %q",
				tc.reported, status, detail, tc.want)
		}
	}
}

// Surrounding whitespace is trimmed rather than rejected: the register path already
// trims, so a value differing only by padding is the same version, and rejecting it
// would report `unknown` for a worker that is demonstrably current.
func TestClassifyUpgradeTrimsPadding(t *testing.T) {
	for _, reported := range []string{"0.11.7 ", " 0.11.7", "\t0.11.7\n"} {
		if status, detail := classifyNoSignal(reported, "0.11.7"); status != UpgradeStatusUpToDate {
			t.Errorf("ClassifyUpgrade(%q, \"0.11.7\") = %q (%q), want up_to_date", reported, status, detail)
		}
	}
}

// classifyNoSignal drives the full decision table with NO controller report, which is
// what every version-compare assertion above is about: an external worker, a
// compose deployment with no controller, or a hosted worker the controller has not
// reached. Kind is deliberately "external" so the hosted-only no-signal grace (R8)
// cannot soften an `outdated` into `upgrading` and quietly change these answers.
func classifyNoSignal(reported, target string) (string, string) {
	return ClassifyUpgrade(UpgradeInput{
		Reported:  reported,
		Kind:      "external",
		CPVersion: target,
		Now:       time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC),
		// APIStartedAt long ago, so no grace is in play even if Kind changed.
		APIStartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}, UpgradeParams{})
}

package workersvc

import (
	"testing"
	"time"
)

// PRD #422 M2 — a hosted worker's upgrade target is the PINNED worker image tag
// (workers.image.tag), not the api's own release (appVersion).
//
// After M1 the worker image tag is pinned INDEPENDENTLY of appVersion, so a hosted
// worker deliberately kept on the pin while the api's CPVersion advances must NOT read
// `outdated` — that is the fleet-wide false alert Decision 1 forbids and the exact
// regression Decision 9 exists to prevent on the dev cluster (where the controller is
// denied `list pods` and so emits no fresh RolledTag). These cases pin the classifier's
// no-fresh-signal behaviour against the new PinnedWorkerVersion input.
//
// APIStartedAt is 24h in the past in every case so R8's no-signal hosted grace can never
// be the reason for an answer — otherwise a `behind` case would soften to `upgrading`
// and the assertions would be measuring the grace instead of the pin.
func TestHostedTargetIsThePinnedWorkerVersion(t *testing.T) {
	now := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	base := func(reported, pinned string) UpgradeInput {
		return UpgradeInput{
			Reported:            reported,
			Kind:                "hosted",
			CPVersion:           "0.50.0", // the api has advanced past the pin
			PinnedWorkerVersion: pinned,
			Signal:              nil, // NO fresh controller signal (dev-cluster RBAC denial)
			Now:                 now,
			APIStartedAt:        now.Add(-24 * time.Hour), // R8 grace never in play
		}
	}

	for _, tc := range []struct {
		name       string
		in         UpgradeInput
		wantStatus string
	}{
		{
			// (a) THE core M2 assertion: pinned == reported, api ahead, no signal ⇒
			// up_to_date. Before M2 this classified `outdated` (target was CPVersion).
			name:       "pinned worker on its pin is up_to_date though appVersion moved ahead",
			in:         base("0.48.0", "0.48.0"),
			wantStatus: UpgradeStatusUpToDate,
		},
		{
			// (b) back-compat: with no pin configured the target falls back to CPVersion,
			// so the same worker reads `outdated` exactly as it did before M2. This proves
			// an unconfigured deploy is not regressed.
			name:       "empty pin falls back to CPVersion (no regression when unconfigured)",
			in:         base("0.48.0", ""),
			wantStatus: UpgradeStatusOutdated,
		},
		{
			// (c) the pin must not BLIND real drift: a worker genuinely behind the pin is
			// still outdated. Reported 0.47.0 < pin 0.48.0.
			name:       "worker behind the pin is still outdated (the pin does not blind drift)",
			in:         base("0.47.0", "0.48.0"),
			wantStatus: UpgradeStatusOutdated,
		},
		{
			// An unparseable pin falls back to CPVersion rather than blinding the fleet on
			// one bad string — same fail-safe as an unparseable RolledTag.
			name:       "unparseable pin falls back to CPVersion",
			in:         base("0.48.0", "not-a-version"),
			wantStatus: UpgradeStatusOutdated,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, detail := ClassifyUpgrade(tc.in, ceilingParams())
			if status != tc.wantStatus {
				t.Errorf("ClassifyUpgrade = %q (%q), want %q", status, detail, tc.wantStatus)
			}
		})
	}
}

// (d) A FRESH controller RolledTag stays authoritative ABOVE the static pin (Decision 9):
// it is what the controller actually rolls to, confirmed live, so it GOVERNS THE TARGET
// even when a pin is configured — here target resolves to 0.49.0, not the pin 0.48.0.
// The STATUS, though, is the reuse/mis-stamp case (issue #738): the worker is `settled`
// on the 0.49.0-rendered pod with a parseable RolledTag one step ahead of its reported
// 0.48.0, past R3's convergence grace. `settled` proves the pod runs the 0.49.0 image, so
// a reported version below it is the baked stamp trailing a re-tagged image (PRD #422),
// not real drift — R7.5 reads it `up_to_date`. Target governance (Decision 9) and status
// softening (R7.5) are independent: target stays 0.49.0 while the status clears.
func TestFreshRolledTagOverridesTheStaticPin(t *testing.T) {
	now := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	anchor := now.Add(-time.Minute)
	settledAgesAgo := now.Add(-time.Hour) // past R3's convergence grace, so R3 does not fire

	in := UpgradeInput{
		Reported:            "0.48.0",
		Kind:                "hosted",
		CPVersion:           "0.50.0",
		PinnedWorkerVersion: "0.48.0", // pin alone ⇒ up_to_date
		Signal: &RollSignal{
			Phase:          PhaseSettled,
			ObservedAt:     now, // fresh
			UpgradingSince: &anchor,
			RolledTag:      "0.49.0", // controller actually rolls to 0.49.0, ahead of the worker
			PhaseSince:     &settledAgesAgo,
		},
		Now:          now,
		APIStartedAt: now.Add(-24 * time.Hour),
	}

	status, _, target := ClassifyUpgradeWithTarget(in, ceilingParams())
	if target != "0.49.0" {
		t.Errorf("target = %q, want the fresh RolledTag 0.49.0 to govern over the static pin 0.48.0", target)
	}
	if status != UpgradeStatusUpToDate {
		t.Errorf("status = %q, want %q: a fresh settled worker whose parseable RolledTag is one step ahead, "+
			"past the R3 grace, is the reuse/mis-stamp case (issue #738) — settled proves it is on the "+
			"RolledTag image and the trailing baked version is the lie",
			status, UpgradeStatusUpToDate)
	}
}

// (e) The nav badge's attention set (Decision 1) over the M2 cases: the pinned-current
// worker (a) is NOT attention-worthy, while the genuinely-behind worker (c) is.
func TestPinnedClassificationDrivesTheAttentionSet(t *testing.T) {
	now := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	pinnedCurrent := UpgradeInput{
		Reported: "0.48.0", Kind: "hosted", CPVersion: "0.50.0", PinnedWorkerVersion: "0.48.0",
		Signal: nil, Now: now, APIStartedAt: now.Add(-24 * time.Hour),
	}
	behindPin := UpgradeInput{
		Reported: "0.47.0", Kind: "hosted", CPVersion: "0.50.0", PinnedWorkerVersion: "0.48.0",
		Signal: nil, Now: now, APIStartedAt: now.Add(-24 * time.Hour),
	}

	if status, _ := ClassifyUpgrade(pinnedCurrent, ceilingParams()); InUpgradeAttentionSet(status) {
		t.Errorf("a worker on its pin classified %q, which is in the attention set — the nav badge would "+
			"count an intentionally-pinned worker as needing attention", status)
	}
	if status, _ := ClassifyUpgrade(behindPin, ceilingParams()); !InUpgradeAttentionSet(status) {
		t.Errorf("a worker genuinely behind its pin classified %q, which is NOT in the attention set — real "+
			"drift would go uncounted", status)
	}
}

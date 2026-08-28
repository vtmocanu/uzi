package workersvc

import (
	"strings"
	"testing"
	"time"
)

// INV-5, the anti-suppression ceiling (PRD #113 M4, per prd-113-inv5-spec.md).
//
// WHY THIS EXISTS. Decision 7 lets a fresh controller `upgrading` signal suppress
// `outdated`, so a clean fleet-wide roll does not turn the nav badge red every release.
// Freshness is a TTL. A TTL bounds a controller that STOPS talking and does nothing
// about one that KEEPS talking: a compromised or wedged controller re-posting
// `upgrading` on every poll satisfies the TTL forever and thereby suppresses every
// `outdated` and `upgrade_failed` alert in the fleet, indefinitely. That is alert
// suppression, not the "wrong badge" Decision 10 budgets for.
//
// WHAT THESE TESTS DO AND DO NOT COVER. They pin the CLASSIFIER given correct
// persistence. The persistence semantics they assume — set-if-NULL on the anchor, and a
// clear only on version movement at register — are pinned separately against a real
// database in upgrade_livedb_test.go, because they live in SQL. Modelling the upsert
// here (see postUpgrading) rather than importing it is deliberate: a helper that shared
// code with the implementation would agree with a broken implementation.

const (
	testTTL    = 60 * time.Second
	testWindow = 45 * time.Minute
)

func ceilingParams() UpgradeParams {
	return UpgradeParams{
		ControllerSignalTTL: testTTL,
		MaxUpgradingWindow:  testWindow,
		// Large enough that neither grace can be what produces an `upgrading` answer in
		// these tests — otherwise a ceiling failure could hide behind R3 or R8.
		RegisterConvergenceGrace: time.Second,
		HostedRollGrace:          time.Second,
	}
}

// fleet is the fixture the spec mandates: ONE hosted worker whose reported version is
// strictly BELOW the target, so version-compare alone says `outdated`.
//
// That is load-bearing rather than incidental. If the fixture's worker would classify
// `up_to_date` anyway, every property below passes vacuously — the suite would prove
// nothing while looking thorough. TestCeilingFixtureBaseline asserts it explicitly
// before any property runs.
func fleetInput(now time.Time, sig *RollSignal) UpgradeInput {
	return UpgradeInput{
		Reported:  "0.11.0",
		Kind:      "hosted",
		CPVersion: "0.11.7",
		Signal:    sig,
		Now:       now,
		// Long ago, so R8's no-signal grace is never the reason for an answer.
		APIStartedAt: now.Add(-24 * time.Hour),
	}
}

// postUpgrading models what ONE controller report does to the persisted row, including
// the set-if-NULL on the anchor. It mirrors the SQL rather than calling it: the SQL is
// pinned by the live-DB test, and sharing code between an implementation and the test
// that judges it is how a broken implementation gets a green.
func postUpgrading(prev *RollSignal, at time.Time, phase, tag, reason string) *RollSignal {
	sig := &RollSignal{
		Phase:          phase,
		ObservedAt:     at, // the api's own receipt time, re-stamped every report
		RolledTag:      tag,
		BlockingReason: reason,
	}
	switch {
	case prev != nil && prev.UpgradingSince != nil:
		// SET-IF-NULL: an existing anchor is KEPT. Re-stamping it here is the bug that
		// deletes the ceiling, and it is the tidier-looking line.
		sig.UpgradingSince = prev.UpgradingSince
	case phase == PhaseRolling || phase == PhaseStuck:
		anchor := at
		sig.UpgradingSince = &anchor
	}
	return sig
}

func TestCeilingFixtureBaseline(t *testing.T) {
	now := time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC)
	// With NO controller signal at all, the fixture worker must be `outdated`. Without
	// this, P1 and P2 could pass against a classifier that never returns `upgrading`.
	status, _ := ClassifyUpgrade(fleetInput(now, nil), ceilingParams())
	if status != UpgradeStatusOutdated {
		t.Fatalf("baseline: no signal ⇒ %q, want %q. The whole suite below is vacuous unless "+
			"version-compare alone would alert on this fixture.", status, UpgradeStatusOutdated)
	}
}

// P1 — anti-suppression. A controller posting `upgrading` continuously for longer than
// MaxUpgradingWindow stops being believed, regardless of freshness.
//
// THE REPORT STREAM CONTINUES ACROSS THE BOUNDARY, and that is the point. The broken
// version of this test stops posting before the boundary, advances the clock, and
// asserts the status changed — it passes, and it measures the freshness TTL, which
// already worked and was never the concern. If this test would still pass with
// MaxUpgradingWindow removed from the code entirely, it is that bug.
func TestP1CeilingStopsSuppressionWhileTheControllerKeepsPosting(t *testing.T) {
	t0 := time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC)
	now := t0
	sig := postUpgrading(nil, now, PhaseRolling, "0.11.7", "Pulling image")

	// Suppression works at the start — without this the rest is unfalsifiable.
	if status, _ := ClassifyUpgrade(fleetInput(now, sig), ceilingParams()); status != UpgradeStatusUpgrading {
		t.Fatalf("at T0 the fresh signal must suppress outdated; got %q", status)
	}

	// Keep posting, every half-TTL, well past the window. The stream never stops.
	posts := 1
	for now.Before(t0.Add(testWindow + 5*time.Minute)) {
		now = now.Add(testTTL / 2)
		sig = postUpgrading(sig, now, PhaseRolling, "0.11.7", "Pulling image")
		posts++
	}
	// One more AFTER the boundary, so the final read follows a post rather than a gap.
	now = now.Add(testTTL / 2)
	sig = postUpgrading(sig, now, PhaseRolling, "0.11.7", "Pulling image")
	posts++

	if fresh := now.Sub(sig.ObservedAt); fresh > testTTL {
		t.Fatalf("precondition broken: the final signal is %v old, i.e. already stale, so this test "+
			"is measuring the TTL and not the ceiling", fresh)
	}
	status, detail := ClassifyUpgrade(fleetInput(now, sig), ceilingParams())
	if status == UpgradeStatusUpgrading {
		t.Fatalf("after %d continuous fresh `upgrading` posts spanning %v (window %v), the worker is STILL "+
			"%q. The freshness TTL cannot bound a controller that keeps talking — only the ceiling can, "+
			"and it did not fire.", posts, now.Sub(t0), testWindow, status)
	}
	if status != UpgradeStatusOutdated {
		t.Errorf("past the ceiling the worker must classify by version-compare alone ⇒ %q; got %q (%q)",
			UpgradeStatusOutdated, status, detail)
	}
}

// P2 — the reset is not controller-forgeable. P1 must hold even when the controller
// varies EVERY field it controls between reports.
//
// The rotating tag is the attack, not a fuzz input: an anchor keyed on
// (worker_id, target_tag) reads as MORE precise and is the trap, because each rotation
// looks like a fresh roll and restarts the clock.
func TestP2CeilingHoldsWhileTheControllerRotatesEveryFieldItControls(t *testing.T) {
	t0 := time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC)
	now := t0
	tags := []string{"0.11.7", "0.11.8", "0.11.9", "0.12.0", "0.12.1", "1.0.0"}
	phases := []string{PhaseRolling, PhaseStuck}

	sig := postUpgrading(nil, now, PhaseRolling, tags[0], "reason 0")
	if status, _ := ClassifyUpgrade(fleetInput(now, sig), ceilingParams()); status == UpgradeStatusOutdated {
		t.Fatalf("precondition: the first post must suppress outdated, else nothing is being tested")
	}

	i := 0
	for now.Before(t0.Add(testWindow + 5*time.Minute)) {
		now = now.Add(testTTL / 2)
		i++
		// A different plausible, INCREASING tag every time; cycling phases within the
		// non-terminal set; different reason text each time.
		sig = postUpgrading(sig, now, phases[i%len(phases)], tags[i%len(tags)], "reason "+time.Duration(i).String())
	}
	now = now.Add(testTTL / 2)
	i++
	// The final post is pinned to `rolling` rather than continuing the rotation. Since
	// #151 the ceiling gates R2 only, so `stuck` is believed past the window BY DESIGN
	// and a final post landing there (which `phases[i%2]` did, by construction) would
	// redden this test for a reason that has nothing to do with anchor forgery. The
	// rotation through both phases still happens above — it is the attack — but the
	// property is only observable through the suppressing direction.
	sig = postUpgrading(sig, now, PhaseRolling, tags[i%len(tags)], "final reason")

	status, _ := ClassifyUpgrade(fleetInput(now, sig), ceilingParams())
	if status == UpgradeStatusUpgrading {
		t.Fatalf("with the tag rotated %d times across %v (window %v) the worker is still %q. "+
			"The anchor is being reset by something the CONTROLLER supplies — most likely keyed on "+
			"(worker, target_tag) or rewritten whenever a field changes.", i, now.Sub(t0), testWindow, status)
	}
	if status != UpgradeStatusOutdated {
		t.Errorf("want %q past the ceiling, got %q", UpgradeStatusOutdated, status)
	}
}

// P2, second half: a FUTURE wire timestamp must not extend anything. The api stamps its
// own observed_at, so the wire's controller_reported_at never reaches the classifier at
// all — RollSignal has no field for it. This test pins the consequence: a stale signal
// is stale no matter what the controller claims the time was.
func TestP2FutureWireTimestampCannotRefreshAStaleSignal(t *testing.T) {
	now := time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC)
	// Observed by the api well outside the TTL. The controller may claim any
	// reported_at it likes; there is nowhere for that claim to land.
	anchor := now.Add(-2 * time.Hour)
	sig := &RollSignal{
		Phase:          PhaseRolling,
		ObservedAt:     now.Add(-10 * time.Minute),
		UpgradingSince: &anchor,
		RolledTag:      "0.11.7",
	}
	status, _ := ClassifyUpgrade(fleetInput(now, sig), ceilingParams())
	if status == UpgradeStatusUpgrading {
		t.Errorf("a signal observed %v ago (TTL %v) is suppressing outdated; freshness is being read "+
			"from something other than the api's own observed_at", now.Sub(sig.ObservedAt), testTTL)
	}
}

// P3 (classifier half) — the ceiling is not a one-shot latch. Once the anchor is
// cleared, a later roll gets the WHOLE window again, not the remainder of the first.
//
// The clear itself happens in SQL at register and is pinned in upgrade_livedb_test.go;
// here the cleared state is modelled as a nil anchor, which is what that SQL produces.
func TestP3AFreshRollGetsAFullWindowAfterTheAnchorClears(t *testing.T) {
	t0 := time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC)

	// A first roll that ran most of the window before the worker came back.
	spent := t0.Add(-testWindow + 2*time.Minute)
	in := fleetInput(t0, &RollSignal{
		Phase: PhaseRolling, ObservedAt: t0, UpgradingSince: &spent, RolledTag: "0.11.7",
	})
	if status, _ := ClassifyUpgrade(in, ceilingParams()); status != UpgradeStatusUpgrading {
		t.Fatalf("precondition: inside the window the signal should suppress; got %q", status)
	}

	// Register cleared the anchor (version moved). A NEW roll starts now.
	newAnchor := t0
	later := t0.Add(testWindow - time.Minute) // beyond the FIRST roll's window
	sig := &RollSignal{
		Phase: PhaseRolling, ObservedAt: later, UpgradingSince: &newAnchor, RolledTag: "0.12.0",
	}
	if status, _ := ClassifyUpgrade(fleetInput(later, sig), ceilingParams()); status != UpgradeStatusUpgrading {
		t.Errorf("the second roll must get a full fresh window, not the remainder of the first; got %q. "+
			"A latch — a persisted boolean, or an anchor that is set-if-NULL with no clear anywhere in the "+
			"register path — fails exactly here.", status)
	}
}

// P4 — structural. The window comes from configuration and nothing on the wire can
// extend it.
//
// The strongest form of this is a type-level fact rather than an assertion: RollSignal
// carries no field for controller_reported_at, poll_interval_seconds or anything else
// the controller could use to argue for more time, and UpgradeParams is passed in by
// the caller. This test pins the observable consequence — the same classification
// verdict for a spread of controller-supplied field values at a fixed elapsed time.
func TestP4NoWireFieldCanExtendTheWindow(t *testing.T) {
	t0 := time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC)
	// Just past the ceiling.
	anchor := t0.Add(-testWindow - time.Second)
	now := t0

	// Every case here is `rolling`, and that is a consequence of #151 rather than an
	// oversight: the window is only observable through R2 now, so a `stuck` case could
	// no longer distinguish "a wire field extended the window" from "R1 is deliberately
	// ungated". The stuck arm's own behaviour past the ceiling is pinned by
	// TestCeilingGatesUpgradingButNotUpgradeFailed.
	for _, tc := range []struct{ name, tag, reason string }{
		{"plain", "0.11.7", "Pulling image"},
		{"huge tag", "999.999.999", "Pulling image"},
		{"empty tag", "", ""},
		{"garbage tag", "not-a-version", "whatever"},
		{"crashloop reason on a rolling phase", "0.11.7", "CrashLoopBackOff"},
	} {
		sig := &RollSignal{
			Phase: PhaseRolling, ObservedAt: now, UpgradingSince: &anchor,
			RolledTag: tc.tag, BlockingReason: tc.reason,
		}
		status, _ := ClassifyUpgrade(fleetInput(now, sig), ceilingParams())
		if status == UpgradeStatusUpgrading {
			t.Errorf("%s: past the ceiling the controller is still believed (%q). Some wire value is "+
				"feeding the window or the anchor.", tc.name, status)
		}
	}

	// The phase itself is a wire field, so pin that it cannot buy MORE time either: a
	// `stuck` report past the ceiling is believed (#151), but it must not re-arm R2 for
	// a subsequent `rolling` report on the same anchor.
	stuck := &RollSignal{
		Phase: PhaseStuck, ObservedAt: now, UpgradingSince: &anchor,
		RolledTag: "0.11.7", BlockingReason: "CrashLoopBackOff",
	}
	if status, _ := ClassifyUpgrade(fleetInput(now, stuck), ceilingParams()); status != UpgradeStatusUpgradeFailed {
		t.Fatalf("precondition: a past-ceiling `stuck` report must be believed, got %q", status)
	}
	after := &RollSignal{
		Phase: PhaseRolling, ObservedAt: now.Add(time.Second), UpgradingSince: &anchor,
		RolledTag: "0.11.7",
	}
	if status, _ := ClassifyUpgrade(fleetInput(now.Add(time.Second), after), ceilingParams()); status == UpgradeStatusUpgrading {
		t.Errorf("a `stuck` report bought the controller a fresh window for `rolling`; the ceiling "+
			"is being re-armed by the phase, got %q", status)
	}
}

// The ceiling gates R2 and NOT R1 (issue #151). This test used to assert the opposite
// and is inverted deliberately — read `ceilingOK`'s comment in upgrade.go for why the
// two directions are not symmetric.
//
// Both halves are asserted in ONE function on purpose: two independent tests are each
// satisfied by a classifier that ignores the ceiling entirely (one wants it to bite,
// the other wants it not to), so only the PAIR distinguishes "gates R2 only" from
// "gates nothing".
func TestCeilingGatesUpgradingButNotUpgradeFailed(t *testing.T) {
	now := time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC)
	anchor := now.Add(-testWindow - time.Minute)

	// R1, past the ceiling: still believed. A stuck pod is a fact about the CURRENT pod
	// state, so this alert self-clears when the pod recovers; expiring it on a timer only
	// ever hid a truthful alarm.
	stuck := &RollSignal{
		Phase: PhaseStuck, ObservedAt: now, UpgradingSince: &anchor,
		RolledTag: "0.11.7", BlockingContainer: "seed-nix", BlockingReason: "CrashLoopBackOff",
	}
	status, detail := ClassifyUpgrade(fleetInput(now, stuck), ceilingParams())
	if status != UpgradeStatusUpgradeFailed {
		t.Errorf("past the ceiling a truthful `stuck` report must STILL be believed (#151): "+
			"want %q, got %q. If this reddens because ceilingOK was put back on R1, the worker "+
			"that motivated #151 goes silent 70 seconds after its pod could first appear.",
			UpgradeStatusUpgradeFailed, status)
	}
	if detail != "seed-nix: CrashLoopBackOff" {
		t.Errorf("the diagnosis must survive the ceiling too, got %q", detail)
	}

	// R2, past the ceiling: NOT believed. This is the direction INV-5 exists for — a
	// controller claiming a roll is still in progress can otherwise suppress `outdated`
	// forever, silently.
	rolling := &RollSignal{
		Phase: PhaseRolling, ObservedAt: now, UpgradingSince: &anchor, RolledTag: "0.11.7",
	}
	status, _ = ClassifyUpgrade(fleetInput(now, rolling), ceilingParams())
	if status == UpgradeStatusUpgrading {
		t.Errorf("past the ceiling a `rolling` report must stop suppressing the version compare; "+
			"INV-5 is gone, got %q", status)
	}
	if status != UpgradeStatusOutdated {
		t.Errorf("want %q past the ceiling, got %q", UpgradeStatusOutdated, status)
	}
}

// Inside the window, both controller rows must still work — otherwise the tests above
// would pass against a classifier that ignores the controller entirely.
func TestInsideTheWindowTheControllerIsBelieved(t *testing.T) {
	now := time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC)
	anchor := now.Add(-time.Minute)

	stuck := &RollSignal{
		Phase: PhaseStuck, ObservedAt: now, UpgradingSince: &anchor, RolledTag: "0.11.7",
		BlockingContainer: "seed-nix", BlockingReason: "CrashLoopBackOff", RestartCount: 6,
	}
	status, detail := ClassifyUpgrade(fleetInput(now, stuck), ceilingParams())
	if status != UpgradeStatusUpgradeFailed {
		t.Errorf("a fresh in-window `stuck` report ⇒ %q, got %q", UpgradeStatusUpgradeFailed, status)
	}
	if detail != "seed-nix: CrashLoopBackOff (6 restarts)" {
		t.Errorf("detail = %q, want the container, the reason and the restart count", detail)
	}

	rolling := &RollSignal{
		Phase: PhaseRolling, ObservedAt: now, UpgradingSince: &anchor, RolledTag: "0.11.7",
		BlockingReason: "Pulling image",
	}
	if status, detail = ClassifyUpgrade(fleetInput(now, rolling), ceilingParams()); status != UpgradeStatusUpgrading {
		t.Errorf("a fresh in-window `rolling` report ⇒ %q, got %q (%q)", UpgradeStatusUpgrading, status, detail)
	}
}

// Decision 9: for a hosted worker the target is the tag the CONTROLLER rolls to, not
// the api's own release, because values.yaml can pin them independently. An unparseable
// tag must fall back rather than blind the fleet.
func TestHostedTargetIsTheRolledTagWithFallback(t *testing.T) {
	now := time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC)
	anchor := now.Add(-time.Minute)

	// The worker runs 0.11.0. The api is 0.11.7 but the controller rolls to 0.11.0 —
	// so against the AUTHORITATIVE hosted target this worker is current.
	settledLongAgo := now.Add(-time.Hour)
	sig := &RollSignal{
		Phase: PhaseSettled, ObservedAt: now, UpgradingSince: &anchor,
		RolledTag: "0.11.0", PhaseSince: &settledLongAgo,
	}
	if status, _ := ClassifyUpgrade(fleetInput(now, sig), ceilingParams()); status != UpgradeStatusUpToDate {
		t.Errorf("with the controller rolling to 0.11.0 the worker is current ⇒ up_to_date, got %q. "+
			"The hosted target must be the rolled tag (Decision 9), not the api's release.", status)
	}

	// A garbage tag falls back to the api's version, so the worker reads outdated —
	// never `unknown`, which would blind the fleet on one bad string.
	sig.RolledTag = "not-a-version"
	if status, _ := ClassifyUpgrade(fleetInput(now, sig), ceilingParams()); status != UpgradeStatusOutdated {
		t.Errorf("an unparseable rolled tag must fall back to the api's release ⇒ outdated, got %q", status)
	}
}

// The two halves of this feature pick their timeouts in two different modules, so the
// relationship between them has to be asserted somewhere or it is not a relationship at
// all — just two numbers that happen to be ordered correctly today.
//
// If the ceiling expired BEFORE the controller's reasonless-stuck arm fires, a worker
// would be reported `rolling` by the controller while the api had already reclassified
// it, and nothing in either package would notice the disagreement.
func TestMaxUpgradingWindowExceedsControllerStuckAge(t *testing.T) {
	if DefaultMaxUpgradingWindow <= ControllerStuckAge {
		t.Fatalf("MaxUpgradingWindow (%v) must exceed the controller's stuckAge (%v). Below it, the "+
			"controller keeps reporting `rolling` for a reasonless-stuck pod while this api has already "+
			"stopped believing it — the two halves disagree about the same worker and neither module can "+
			"see the other's constant.", DefaultMaxUpgradingWindow, ControllerStuckAge)
	}
	// And the value the TESTER eventually measures must satisfy it too, not just the
	// default — so the check runs against whatever a caller configures.
	p := UpgradeParams{MaxUpgradingWindow: ControllerStuckAge}.withDefaults()
	if p.MaxUpgradingWindow <= ControllerStuckAge {
		t.Errorf("a configured window of %v does not exceed the controller's stuckAge (%v); the "+
			"measurement that replaces the placeholder must respect this floor", p.MaxUpgradingWindow, ControllerStuckAge)
	}
}

// RolledTag decides what EVERY hosted worker is compared against (Decision 9), and it
// is entirely controller-supplied. It had no coverage in the main classification table
// and none at all in the adversarial direction, which is how B-1 below went unnoticed.

func TestRolledTagIsHonouredOnlyForHostedWorkersAndOnlyWhenFresh(t *testing.T) {
	now := time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC)
	anchor := now.Add(-time.Minute)
	settledAgesAgo := now.Add(-time.Hour)

	// The signal claims the fleet rolls to 0.11.0, which is what this worker runs.
	sig := func() *RollSignal {
		return &RollSignal{
			Phase: PhaseSettled, ObservedAt: now, UpgradingSince: &anchor,
			RolledTag: "0.11.0", PhaseSince: &settledAgesAgo,
		}
	}

	// EXTERNAL worker: the rolled tag must be ignored entirely. Nothing upgrades an
	// external worker, so a controller has no standing to say what it should be running
	// — if it could, it would be asserting about a worker outside its jurisdiction.
	ext := fleetInput(now, sig())
	ext.Kind = "external"
	if status, _ := ClassifyUpgrade(ext, ceilingParams()); status != UpgradeStatusOutdated {
		t.Errorf("an EXTERNAL worker classified %q; the controller's rolled tag must not be its target, "+
			"so it stays outdated against the api's own release", status)
	}

	// STALE signal: the tag goes with it. A tag is only authoritative while the report
	// carrying it is fresh, otherwise a dead controller pins the fleet's target forever.
	stale := sig()
	stale.ObservedAt = now.Add(-10 * time.Minute)
	if status, _ := ClassifyUpgrade(fleetInput(now, stale), ceilingParams()); status != UpgradeStatusOutdated {
		t.Errorf("a STALE signal's rolled tag was still used as the target (%q); freshness must gate the "+
			"tag as well as the phase", status)
	}

	// HOSTED and fresh: honoured (the positive control for both assertions above).
	if status, _ := ClassifyUpgrade(fleetInput(now, sig()), ceilingParams()); status != UpgradeStatusUpToDate {
		t.Errorf("a hosted worker with a fresh signal must be compared against the rolled tag ⇒ "+
			"up_to_date, got %q", status)
	}
}

// B-1 — CHARACTERIZATION, NOT ENDORSEMENT.
//
// `ceilingOK` gates the two phase-asserted rows but NOT the target assignment. So a
// controller that never lies about phase — reporting `settled`, which is true — and
// instead reports a `worker_image_tag` equal to the fleet's already-stale version makes
// every hosted worker read `up_to_date` INDEFINITELY. The ceiling never engages, because
// no row it gates is being used.
//
// That is the same suppression INV-5 exists to prevent, reached by a route that requires
// no false phase, and it is WORSE than the `upgrading` route in one respect: `upgrading`
// renders a visible badge, while `up_to_date` renders nothing at all. The operator sees a
// healthy fleet.
//
// This is pinned as the CURRENT behaviour, deliberately, not as desired behaviour.
// Decision 9 says the hosted target is the controller's tag, and honouring that is what
// makes an independently-pinned worker image work at all. The agreed resolution is to
// SURFACE the divergence in the Fleet panel when the reported tag is below the control
// plane's version — a display fix in M5, not a classification change here. If someone
// later gates the target by ceilingOK, this test will fail; read it before "fixing" it,
// because that change would break independent pinning.
func TestB1ControllerCanSuppressViaTheTargetWithoutLyingAboutPhase(t *testing.T) {
	now := time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC)
	// The anchor is long past the ceiling, so every controller-ASSERTED row is barred.
	anchor := now.Add(-10 * testWindow)
	settledAgesAgo := now.Add(-24 * time.Hour)

	sig := &RollSignal{
		Phase:      PhaseSettled, // TRUE: the pod really is Ready. No lie about phase.
		ObservedAt: now,
		// Past the ceiling — and irrelevant, which is the finding.
		UpgradingSince: &anchor,
		// The lie is HERE: the fleet's stale version presented as the roll target.
		RolledTag:  "0.11.0",
		PhaseSince: &settledAgesAgo,
	}
	status, detail := ClassifyUpgrade(fleetInput(now, sig), ceilingParams())

	if status != UpgradeStatusUpToDate {
		t.Fatalf("B-1 characterization is out of date: got %q (%q), expected %q. If the target assignment "+
			"is now gated by the ceiling, that is a deliberate behaviour change — update this test and "+
			"check that independently-pinned worker images still work (Decision 9).",
			status, detail, UpgradeStatusUpToDate)
	}
	// State the consequence in the test so it is visible without reading the audit: the
	// ceiling is fully expired and the worker still reports healthy, silently.
	if elapsed := now.Sub(*sig.UpgradingSince); elapsed <= testWindow {
		t.Fatalf("precondition: the anchor must be past the ceiling (elapsed %v, window %v) or this test "+
			"proves nothing about the ceiling being bypassed", elapsed, testWindow)
	}
	if detail != "" {
		t.Errorf("detail = %q; up_to_date renders nothing, which is why this route is worse than the "+
			"`upgrading` one it bypasses — M5 must surface the tag divergence", detail)
	}
}

// The nav badge's count (PRD #113 M6). The attention set is the ONE definition, shared
// with the per-worker badge, so the count and the list cannot disagree.
func TestUpgradeAttentionSet(t *testing.T) {
	for status, want := range map[string]bool{
		UpgradeStatusUpgradeFailed: true,
		UpgradeStatusOutdated:      true,
		// `upgrading` is deliberately NOT an alert: a roll in progress is expected,
		// transient and self-resolving, and counting it would turn the badge red on every
		// release — the cry-wolf this whole PRD exists to prevent.
		UpgradeStatusUpgrading: false,
		UpgradeStatusUpToDate:  false,
		// THE CASE THAT MATTERS MOST FOR A NAV BADGE. A `dev` control plane disables
		// classification fleet-wide (D8), so every worker is `unknown` — and if `unknown`
		// counted, every developer running a local stack would see their whole fleet
		// reported as needing attention. A wrong sign here flips "nothing to see" into
		// "everything is broken", on the surface designed to interrupt people.
		UpgradeStatusUnknown: false,
	} {
		if got := InUpgradeAttentionSet(status); got != want {
			t.Errorf("InUpgradeAttentionSet(%q) = %v, want %v", status, got, want)
		}
	}
}

// A `dev` control plane must produce a ZERO badge, end to end through the classifier —
// not merely a status that is not in the set. Asserted through ClassifyUpgrade because
// that is the path the summary endpoint actually takes.
func TestDevControlPlaneProducesNoAttention(t *testing.T) {
	now := time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC)
	for _, kind := range []string{"hosted", "external"} {
		in := fleetInput(now, nil)
		in.Kind = kind
		in.CPVersion = "dev" // an unstamped control plane, i.e. every local build
		status, _ := ClassifyUpgrade(in, ceilingParams())
		if InUpgradeAttentionSet(status) {
			t.Errorf("kind=%s with a `dev` control plane classified %q, which is in the attention set. "+
				"D8 disables classification fleet-wide there, so every worker of every local stack "+
				"would badge the nav item red.", kind, status)
		}
	}
}

// M7 — THE AUDITOR'S DISCRIMINATING PROBE, and the reason it is worth its own test: it
// FAILS under the anchor design originally proposed and PASSES under the one shipped, so it
// is the case that distinguishes them rather than another instance of P1.
//
// A worker re-registers repeatedly on an UNCHANGED old version — which is exactly what a
// crash-looping agent does, since it registers on every start — while the controller posts
// `upgrading` continuously. Under "clear the anchor on any register" each restart buys a
// fresh window and the ceiling never fires: the worst-off worker in the fleet becomes the
// one most able to suppress its own alert. Under "clear only when the version MOVES" the
// anchor survives every re-register and the ceiling fires on time.
//
// The register half is modelled here the way the SQL behaves — an unchanged version leaves
// the anchor alone — and that behaviour is pinned against a real database in
// store/worker_roll_health_integration_test.go. Modelling it rather than sharing code with
// the implementation is deliberate; a helper built from the same code would agree with a
// broken implementation.
func TestCeilingSurvivesRepeatedReRegistrationOnAnUnchangedVersion(t *testing.T) {
	t0 := time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC)
	now := t0
	sig := postUpgrading(nil, now, PhaseRolling, "0.11.7", "Pulling image")

	if status, _ := ClassifyUpgrade(fleetInput(now, sig), ceilingParams()); status != UpgradeStatusUpgrading {
		t.Fatalf("precondition: the fresh signal must suppress outdated at T0; got %q", status)
	}

	// The crash-loop: a register every half-TTL, on the SAME old version, while the
	// controller keeps posting. Both parties are behaving exactly as they would in the
	// incident this PRD was written for.
	registers := 0
	for now.Before(t0.Add(testWindow + 5*time.Minute)) {
		now = now.Add(testTTL / 2)
		// The agent restarts and re-registers. workers.version does NOT move (the new
		// image never came up), so the anchor is untouched — this is the whole invariant.
		registers++
		sig = postUpgrading(sig, now, PhaseRolling, "0.11.7", "Pulling image")
	}
	now = now.Add(testTTL / 2)
	registers++
	sig = postUpgrading(sig, now, PhaseRolling, "0.11.7", "Pulling image")

	status, detail := ClassifyUpgrade(fleetInput(now, sig), ceilingParams())
	if status == UpgradeStatusUpgrading {
		t.Fatalf("after %d re-registrations on an UNCHANGED version across %v (window %v), the worker "+
			"is still %q. This is the anchor being cleared by a register rather than by a version MOVE — "+
			"a crash-looping agent re-registers on every start, so the worst-off worker in the fleet "+
			"would be the one most able to suppress its own alert, forever.",
			registers, now.Sub(t0), testWindow, status)
	}
	if status != UpgradeStatusOutdated {
		t.Errorf("want %q past the ceiling, got %q (%q)", UpgradeStatusOutdated, status, detail)
	}

	// CONTROL: a register that MOVES the version does clear it, so the assertion above is
	// not passing against an anchor that can never be cleared at all.
	cleared := postUpgrading(nil, now, PhaseRolling, "0.12.0", "Pulling image") // fresh anchor
	in := fleetInput(now, cleared)
	in.Reported = "0.11.7" // the worker came back on the new version
	if status, _ := ClassifyUpgrade(in, ceilingParams()); status == UpgradeStatusOutdated {
		t.Errorf("after a genuine version move the worker still reads outdated; the clear path is dead, "+
			"which would make the assertion above vacuous. got %q", status)
	}
}

// M7 — the COMPOSE DEGRADATION case the PRD names, and the configuration most users run:
// no controller exists at all, so no roll-health row is ever written.
//
// The property is that classification degrades to version comparison and NOTHING cries
// wolf: no `upgrading`, no `upgrade_failed`, and no attention count for anything a version
// comparison would not already have flagged. A controller-shaped state appearing here would
// be an assertion about a component that is not running.
func TestComposeDegradationNoControllerMeansVersionCompareOnly(t *testing.T) {
	now := time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC)
	// A mixed fleet as a compose user would have it. Signal is nil for every one, because
	// nothing posts reports.
	for _, tc := range []struct {
		name, reported, cp, kind, want string
	}{
		{"behind external", "0.11.0", "0.11.7", "external", UpgradeStatusOutdated},
		{"behind hosted", "0.11.0", "0.11.7", "hosted", UpgradeStatusOutdated},
		{"current", "0.11.7", "0.11.7", "external", UpgradeStatusUpToDate},
		{"unstamped worker", "", "0.11.7", "external", UpgradeStatusUnknown},
		// The whole-compose default: an unstamped local api build. Classification off.
		{"dev control plane", "0.11.0", "dev", "external", UpgradeStatusUnknown},
		{"dev control plane, hosted", "0.11.0", "dev", "hosted", UpgradeStatusUnknown},
	} {
		in := UpgradeInput{
			Reported: tc.reported, Kind: tc.kind, CPVersion: tc.cp, Signal: nil, Now: now,
			// Long ago: R8's grace must not be what produces the answer, or this test would
			// silently be measuring the grace instead of the degradation.
			APIStartedAt: now.Add(-24 * time.Hour),
		}
		status, _ := ClassifyUpgrade(in, ceilingParams())
		if status != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, status, tc.want)
		}
		// No controller means no controller-derived state, ever.
		if status == UpgradeStatusUpgrading || status == UpgradeStatusUpgradeFailed {
			t.Errorf("%s: classified %q with NO controller signal — that is an assertion about a "+
				"component that is not running", tc.name, status)
		}
	}
}

// -------------------------------------------------------------------------
// R7.5 (issue #738): trust the controller's `settled` observation over a baked
// UZI_AGENT_VERSION that trails a reuse-retagged image (PRD #422).
//
// The live-observed repro: a release tag advanced to a new version that points at a
// PRIOR release's digest, so a hosted worker's baked version (reported at register)
// trails the tag while the pod is, in fact, running the target image. The controller
// reports `settled` for the current-spec-hash pod, which is direct confirmation the
// worker is on the target. Before R7.5 the version compare read `outdated` and the
// worker was counted for the nav badge; R7.5 clears it.
//
// CALIBRATION: every fixture below reports a version GENUINELY BELOW its target
// (0.66.0 < 0.66.1), so classifyByVersion alone returns `outdated`. If R7.5 were
// removed from upgrade.go, the reuse case would read `outdated` — that is what makes
// these tests non-vacuous.
// -------------------------------------------------------------------------

// hostedSettledInput builds a HOSTED worker with a FRESH `settled` controller signal
// rolling to `target`, reporting `reported`, whose Ready transition happened
// `phaseSinceAge` ago. `phaseSinceAge` older than RegisterConvergenceGrace keeps R3 out
// of the way so any `upgrading` here would have to come from R3 and any `up_to_date`
// from R7.5 — never a grace. The fresh RolledTag == target makes `target` the resolved
// hosted comparison target (Decision 9).
func hostedSettledInput(now time.Time, reported, target string, phaseSinceAge time.Duration) UpgradeInput {
	settledAt := now.Add(-phaseSinceAge)
	anchor := now.Add(-time.Minute)
	return UpgradeInput{
		Reported:  reported,
		Kind:      "hosted",
		CPVersion: target,
		Signal: &RollSignal{
			Phase:          PhaseSettled,
			ObservedAt:     now, // fresh: the api's own receipt time is now
			UpgradingSince: &anchor,
			RolledTag:      target,
			PhaseSince:     &settledAt,
		},
		Now:          now,
		APIStartedAt: now.Add(-24 * time.Hour), // long ago, so R8's grace is never the cause
	}
}

// (a) The reuse case. A hosted worker the controller confirms `settled` on the target
// image, reporting a version that trails the tag (a reuse-retagged image, PRD #422),
// must read `up_to_date` — not `outdated` — and the detail must name the target.
func TestR7_5ReuseRetaggedImageReadsUpToDate_Issue738(t *testing.T) {
	now := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	// Ready long ago (past the register grace), so R3 cannot be the reason.
	in := hostedSettledInput(now, "0.66.0+g4a5c0f4", "0.66.1", time.Hour)

	status, detail := ClassifyUpgrade(in, ceilingParams())
	if status != UpgradeStatusUpToDate {
		t.Fatalf("issue #738: a `settled` worker on a reuse-retagged image must read %q, got %q (%q). "+
			"The controller directly confirms the pod is on the target; the trailing baked version is the lie.",
			UpgradeStatusUpToDate, status, detail)
	}
	if !strings.Contains(detail, "0.66.1") {
		t.Errorf("the detail should name the target image (0.66.1) so the divergence is legible, got %q", detail)
	}
	// (e) attention: `up_to_date` is by definition NOT in the attention set, so the reuse
	// worker no longer inflates the nav-badge count. The summary endpoint counts through
	// exactly this InUpgradeAttentionSet, so pinning the classification pins the count.
	if InUpgradeAttentionSet(status) {
		t.Errorf("issue #738: the reuse worker (%q) is still counted for the nav badge; R7.5 must clear it "+
			"from the attention set", status)
	}
}

// (b) control: a `rolling` signal past the INV-5 ceiling falls through R2 to the compare
// and must stay `outdated` — R7.5 is `settled`-only and must not rescue it.
func TestR7_5DoesNotFireForRollingPastCeiling_Issue738(t *testing.T) {
	now := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	anchor := now.Add(-testWindow - time.Minute) // past the ceiling
	in := UpgradeInput{
		Reported:  "0.66.0",
		Kind:      "hosted",
		CPVersion: "0.66.1",
		Signal: &RollSignal{
			Phase: PhaseRolling, ObservedAt: now, UpgradingSince: &anchor,
			RolledTag: "0.66.1", BlockingReason: "Pulling image",
		},
		Now:          now,
		APIStartedAt: now.Add(-24 * time.Hour),
	}
	status, detail := ClassifyUpgrade(in, ceilingParams())
	if status == UpgradeStatusUpToDate {
		t.Fatalf("issue #738: a `rolling` worker past the ceiling must NOT be cleared by R7.5 (settled-only); got %q", status)
	}
	if status != UpgradeStatusOutdated {
		t.Errorf("want %q for a past-ceiling `rolling` worker, got %q (%q)", UpgradeStatusOutdated, status, detail)
	}
}

// (b) control: a `stuck` signal is R1 (`upgrade_failed`) and must never be softened to
// `up_to_date` by R7.5.
func TestR7_5DoesNotFireForStuckSignal_Issue738(t *testing.T) {
	now := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	anchor := now.Add(-time.Minute)
	in := UpgradeInput{
		Reported:  "0.66.0",
		Kind:      "hosted",
		CPVersion: "0.66.1",
		Signal: &RollSignal{
			Phase: PhaseStuck, ObservedAt: now, UpgradingSince: &anchor,
			RolledTag: "0.66.1", BlockingContainer: "seed-nix", BlockingReason: "CrashLoopBackOff",
		},
		Now:          now,
		APIStartedAt: now.Add(-24 * time.Hour),
	}
	status, _ := ClassifyUpgrade(in, ceilingParams())
	if status == UpgradeStatusUpToDate {
		t.Fatalf("issue #738: a `stuck` worker must stay an alert, never `up_to_date`; got %q", status)
	}
	if status != UpgradeStatusUpgradeFailed {
		t.Errorf("want %q (R1) for a fresh `stuck` signal, got %q", UpgradeStatusUpgradeFailed, status)
	}
}

// (b) control: a hosted worker with NO fresh controller signal past the api-start grace
// classifies by version compare alone — `outdated`. R7.5 needs a fresh `settled`
// observation to fire and there is none. Covers both the nil-signal and stale-signal
// shapes.
func TestR7_5RequiresFreshSettledSignal_Issue738(t *testing.T) {
	now := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)

	// nil signal, past the hosted roll grace.
	nilSig := UpgradeInput{
		Reported: "0.66.0", Kind: "hosted", CPVersion: "0.66.1", Signal: nil,
		Now: now, APIStartedAt: now.Add(-24 * time.Hour),
	}
	if status, _ := ClassifyUpgrade(nilSig, ceilingParams()); status != UpgradeStatusOutdated {
		t.Errorf("no signal + past the grace ⇒ %q, got %q. R7.5 must not fire without a fresh settled signal",
			UpgradeStatusOutdated, status)
	}

	// A `settled` signal that is STALE (observed outside the TTL): freshness gates R7.5,
	// so it must not clear the worker.
	anchor := now.Add(-time.Minute)
	settledAt := now.Add(-time.Hour)
	staleSig := UpgradeInput{
		Reported: "0.66.0", Kind: "hosted", CPVersion: "0.66.1",
		Signal: &RollSignal{
			Phase: PhaseSettled, ObservedAt: now.Add(-10 * time.Minute), // outside the 60s TTL
			UpgradingSince: &anchor, RolledTag: "0.66.1", PhaseSince: &settledAt,
		},
		Now: now, APIStartedAt: now.Add(-24 * time.Hour),
	}
	if status, _ := ClassifyUpgrade(staleSig, ceilingParams()); status != UpgradeStatusOutdated {
		t.Errorf("a STALE settled signal must not clear the worker; want %q, got %q", UpgradeStatusOutdated, status)
	}
}

// (c) fallback unchanged: an EXTERNAL worker genuinely behind stays `outdated` even with a
// fresh `settled` signal — every controller row (R7.5 included) is hosted-only, because
// nothing upgrades an external worker for its owner.
func TestR7_5DoesNotFireForExternalWorker_Issue738(t *testing.T) {
	now := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	in := hostedSettledInput(now, "0.66.0", "0.66.1", time.Hour)
	in.Kind = "external"
	if status, _ := ClassifyUpgrade(in, ceilingParams()); status != UpgradeStatusOutdated {
		t.Errorf("issue #738: R7.5 must be hosted-only; an external worker behind its target stays %q, got %q",
			UpgradeStatusOutdated, status)
	}
}

// (c) fallback unchanged: a `dev` control plane disables version comparison fleet-wide
// (R4 ⇒ `unknown`), and R7.5 only ever softens `outdated`, so it can never override
// `unknown`. Even with a fresh `settled` signal the worker stays `unknown`.
func TestR7_5DoesNotOverrideUnknownOnDevControlPlane_Issue738(t *testing.T) {
	now := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	anchor := now.Add(-time.Minute)
	settledAt := now.Add(-time.Hour)
	in := UpgradeInput{
		Reported:  "0.66.0",
		Kind:      "hosted",
		CPVersion: "dev", // unstamped control plane
		Signal: &RollSignal{
			Phase: PhaseSettled, ObservedAt: now, UpgradingSince: &anchor,
			RolledTag: "", PhaseSince: &settledAt, // no rolled tag, so target stays "dev"
		},
		Now:          now,
		APIStartedAt: now.Add(-24 * time.Hour),
	}
	status, _ := ClassifyUpgrade(in, ceilingParams())
	if status != UpgradeStatusUnknown {
		t.Fatalf("issue #738: a `dev` control plane must stay %q; R7.5 must never override unknown, got %q",
			UpgradeStatusUnknown, status)
	}
	if InUpgradeAttentionSet(status) {
		t.Errorf("a `dev`-plane worker must not badge the nav item, got attention-set status %q", status)
	}
}

// (d) ordering vs R3. SAME versions, differing ONLY in how long ago the pod became Ready:
//   - Ready recently (within RegisterConvergenceGrace): R3 wins ⇒ `upgrading` (awaiting
//     re-registration), NOT R7.5.
//   - Ready long ago (past the grace): R3 falls through and R7.5 clears it ⇒ `up_to_date`,
//     NOT `outdated`.
//
// R7.5 sits BELOW R3 exactly so a still-converging roll reads `upgrading` rather than
// prematurely `up_to_date`.
func TestR7_5OrdersBelowR3_Issue738(t *testing.T) {
	now := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)

	// Ready just now — inside the 1s register grace of ceilingParams(): R3 fires.
	recent := hostedSettledInput(now, "0.66.0+g4a5c0f4", "0.66.1", 0)
	if status, detail := ClassifyUpgrade(recent, ceilingParams()); status != UpgradeStatusUpgrading {
		t.Errorf("within the register grace a settled+behind worker must read %q (R3, awaiting re-register), "+
			"got %q (%q)", UpgradeStatusUpgrading, status, detail)
	}

	// Ready an hour ago — past the grace: R3 falls through, R7.5 clears it.
	old := hostedSettledInput(now, "0.66.0+g4a5c0f4", "0.66.1", time.Hour)
	if status, detail := ClassifyUpgrade(old, ceilingParams()); status != UpgradeStatusUpToDate {
		t.Errorf("past the register grace the same worker must read %q (R7.5), not outdated, got %q (%q)",
			UpgradeStatusUpToDate, status, detail)
	}
}

// (b) control / the narrowing guard: R7.5 fires only when the RolledTag is PARSEABLE, so
// `settled` provably means "on the resolved target" (target == RolledTag). When the pinned
// tag is a non-semver string (e.g. "nightly"), the controller reports `settled` but with an
// UNPARSEABLE RolledTag, so the classifier's target falls back to CPVersion and `settled`
// no longer confirms the worker is on that fallback coordinate. A genuinely-behind worker on
// a stale non-semver pin must therefore STILL surface attention (`outdated`), never be
// falsely cleared. This is the adversarial repro that made the verbatim (guard-less) R7.5
// too broad; the semver.IsValid(RolledTag) conjunct closes it.
func TestR7_5DoesNotFireWhenRolledTagUnparseable_Issue738(t *testing.T) {
	now := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	settledLongAgo := now.Add(-time.Hour) // past R3's grace
	anchor := now.Add(-time.Minute)
	in := UpgradeInput{
		Reported:  "0.11.0", // genuinely below the CPVersion fallback target 0.11.7
		Kind:      "hosted",
		CPVersion: "0.11.7",
		// No valid pin; the controller renders a non-semver tag and reports settled on it.
		PinnedWorkerVersion: "nightly",
		Signal: &RollSignal{
			Phase:          PhaseSettled,
			ObservedAt:     now, // fresh
			UpgradingSince: &anchor,
			RolledTag:      "nightly", // unparseable ⇒ target falls back to CPVersion 0.11.7
			PhaseSince:     &settledLongAgo,
		},
		Now:          now,
		APIStartedAt: now.Add(-24 * time.Hour),
	}
	status, detail, target := ClassifyUpgradeWithTarget(in, ceilingParams())
	if target != "0.11.7" {
		t.Errorf("an unparseable RolledTag must fall the target back to CPVersion 0.11.7, got %q", target)
	}
	if status != UpgradeStatusOutdated {
		t.Fatalf("issue #738: a genuinely-behind worker settled on an UNPARSEABLE pinned tag must stay %q "+
			"(settled proves nothing about the CPVersion fallback), got %q (%q). R7.5's semver.IsValid guard "+
			"exists precisely to keep this real drift visible.", UpgradeStatusOutdated, status, detail)
	}
}

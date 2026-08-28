package workersvc

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"

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

// The controller's roll phases, re-declared on this side (PRD #113 M4).
//
// Re-declared rather than imported: the contract between the api and the controller is
// the JSON on the wire, not a Go package, and the controller is a separate module whose
// dependency graph the api must not pull in. The same split the agent's TypeScript
// protocol lives with. Drift is caught by the shared golden wire contract, not trusted
// — controller/internal/protocol/testdata/controller_status_wire.json is parsed by a
// test on this side and marshalled by one on that side.
//
// CLOSED SET, validated at ingest. A phase outside it means the api does not model that
// state: the ENTRY IS DROPPED, never persisted and never rendered, and the rest of the
// report still lands. One garbage row must not blind the fleet, and a free string must
// never reach the badge.
const (
	PhaseRolling = "rolling"
	PhaseStuck   = "stuck"
	PhaseSettled = "settled"
)

// ValidRollPhase reports whether a controller-supplied phase is one this api models.
func ValidRollPhase(phase string) bool {
	switch phase {
	case PhaseRolling, PhaseStuck, PhaseSettled:
		return true
	default:
		return false
	}
}

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
// This is the VERSION-COMPARE core (rows R4-R7 and R9). ClassifyUpgrade below wraps it
// with the controller roll-health rows, which sit above it in the table and can
// override its answer — but never the other way round: no controller signal can turn
// an unparseable version into a comparison.
func classifyByVersion(reported, target string) (status string, detail string) {
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

// -------------------------------------------------------------------------
// The full decision table (PRD #113 M4): version compare + controller roll health.
// R7.5 (issue #738) adds a settled-only, outdated-only softener below the compare rows:
// a hosted worker the controller confirms is on the target image reads up_to_date even
// when its baked version trails the tag (a reuse-retagged image, PRD #422).
// -------------------------------------------------------------------------

// RollSignal is a persisted controller report, as the classifier reads it. Nil means
// no report exists for this worker — the normal state for an external worker, for any
// hosted worker the controller has not reached, and for the whole fleet under
// docker-compose where no controller runs at all.
type RollSignal struct {
	// Phase is the controller's phase, already validated against the closed enum at
	// ingest. A value outside the enum never reaches persistence, so a non-empty Phase
	// here is one of rolling|stuck|settled.
	Phase string
	// ObservedAt is the API's OWN now() at receipt. Freshness is computed from this and
	// never from the wire's controller_reported_at, which the reporting party controls:
	// a future timestamp there — clock skew is enough, malice makes it permanent —
	// would produce a signal that never goes stale, and staleness is the only thing
	// bounding a controller that has stopped talking.
	ObservedAt time.Time
	// UpgradingSince anchors the INV-5 ceiling. Set-if-NULL at ingest, cleared ONLY by
	// the worker's own authenticated re-registration moving workers.version. Nil means
	// no roll is currently anchored.
	UpgradingSince *time.Time
	// PhaseSince is the controller's timestamp, and ⚠ IT MEANS TWO DIFFERENT THINGS.
	// For `settled` it is the pod's Ready-condition transition — genuine, and what R3
	// uses. For `stuck` it is the pod's CREATION time, NOT when it became stuck: a
	// stateless controller has no memory to record the transition in. So no rule here
	// may treat it as "how long has this been broken" — that would measure something
	// else and err toward looking worse for longer. R3 is the only rule that reads it,
	// and only in the `settled` arm where the meaning is sound.
	PhaseSince *time.Time
	// RolledTag is the tag the controller actually rolls to. For a HOSTED worker this
	// is the authoritative target (Decision 9), because values.yaml may pin the worker
	// image independently of the api's own release.
	RolledTag string
	// Display fields for the failed-worker strip. Already sanitized at ingest.
	PodPhase          string
	BlockingContainer string
	BlockingReason    string
	RestartCount      int32
	LastExitCode      *int32
}

// UpgradeInput is everything the decision table reads.
type UpgradeInput struct {
	// Reported is workers.version — written only at register, so a worker offline
	// mid-roll still carries its OLD version. That single fact is why roll health
	// cannot be derived from this field and has to come from the controller.
	Reported string
	// Kind is "hosted" or "external". Roll health exists only for hosted workers; an
	// external worker's owner runs it by hand and nothing upgrades it for them.
	Kind string
	// CPVersion is the control plane's own release ("dev" on an unstamped build).
	CPVersion string
	// PinnedWorkerVersion is the deploy's concrete pinned hosted-worker image tag
	// (workers.image.tag, PRD #422), or "" if unknown. After M1 the worker image tag is
	// pinned independently of the api's own release (appVersion), so a HOSTED worker's
	// upgrade target is this pin, not CPVersion — otherwise a worker intentionally pinned
	// behind appVersion reads `outdated` fleet-wide whenever there is no fresh controller
	// signal. Empty/unparseable falls back to CPVersion (today's behavior).
	PinnedWorkerVersion string
	// Signal is the persisted controller report, or nil.
	Signal *RollSignal
	// Now and APIStartedAt come from the caller. APIStartedAt anchors the no-signal
	// hosted grace: Model B rolls the api in the same release as the workers, so
	// process start is a free proxy for "a release just happened" and needs no column.
	Now          time.Time
	APIStartedAt time.Time
}

// UpgradeParams are the durations the table compares against. Named fields following
// the Params.WorkerHeartbeatStale precedent — never literals at the comparison site,
// because a literal cannot be overridden in a test without sleeping.
type UpgradeParams struct {
	// ControllerSignalTTL is how long a report stays fresh. Sized to tolerate several
	// consecutive failed reports rather than the minimum: too short and a live
	// `upgrading` decays mid-roll into `outdated`, turning the whole fleet's badge red,
	// which is the exact failure Decision 1 forbids. Too long only leaves an
	// informational badge after the controller dies. Asymmetric, so err long.
	//
	// It is NOT what covers the Recreate pod-gap, and that is worth stating because it
	// briefly was. The controller emits an explicit `rolling` row for a worker whose
	// Deployment exists and whose pods do not (kube/rollhealth.go's no-pod branch), so
	// the gap asserts its own state. Had it emitted NO row instead, this TTL holding the
	// previous report alive would have been the only thing keeping a healthy mid-roll
	// worker from flashing `outdated` — a load-bearing dependency of a constant in this
	// module on a loop in another, where shortening this value would produce cry-wolf
	// nobody would trace back to it.
	ControllerSignalTTL time.Duration
	// HostedRollGrace is how long after api start a behind hosted worker is given the
	// benefit of the doubt with NO controller signal at all.
	HostedRollGrace time.Duration
	// RegisterConvergenceGrace bounds the gap between "the controller says the new pod
	// is Ready" and "the worker has re-registered with its new version". A fresh pod
	// registers on boot, so this is seconds in the healthy case.
	//
	// CORRECTED 2026-07-26 by live validation on 0.11.8. This comment used to claim
	// the window is "generous enough that a pod whose container is Ready while the
	// agent inside crash-loops still surfaces". It is NOT: R3 additionally requires
	// isBehind(), and a crash-looping agent re-registers at the CURRENT release on
	// every start, so it is never behind and R3 never fires — the row falls through
	// to up_to_date. The grace bounds the register gap and nothing more. The
	// crash-looping-agent case is a real gap tracked separately (issue #145), and it
	// is not fixable here: it needs the controller to stop reporting `settled` on
	// Ready alone, since the worker container ships with no readiness probe.
	RegisterConvergenceGrace time.Duration
	// MaxUpgradingWindow is the INV-5 ceiling: the longest a controller may be
	// believed about one roll. A TTL bounds a controller that STOPS talking; nothing
	// but this bounds one that KEEPS talking, and a compromised or wedged controller
	// re-posting `upgrading` every poll would otherwise suppress every `outdated` and
	// `upgrade_failed` alert in the fleet indefinitely.
	//
	// This is a BACKSTOP, not the primary detector — the pod-phase path catches
	// ordinary stuck rolls — so it can afford to be generous and must not be tuned as
	// though it were catching them. It needs a p99 healthy-roll measurement on a real
	// cluster; the default below is a starting point for that measurement, NOT a
	// measured value, and it should not be described as one.
	MaxUpgradingWindow time.Duration
}

// Defaults. Each is REASONED from a measured cadence, not measured itself.
const (
	// 6x the controller's 10s default poll, so five consecutive failed reports are
	// tolerated. The repo's other staleness window is 3x its cadence (heartbeat);
	// this one goes wider deliberately, per the asymmetry above.
	DefaultControllerSignalTTL = 60 * time.Second
	// Must exceed a healthy Recreate of the browser-inflated agent image (cold pull
	// plus /nix reseed) and sit below the ~14 minutes the motivating incident ran
	// before a human noticed.
	DefaultHostedRollGrace = 10 * time.Minute
	// ~7x the 45s offline threshold: generous for a slow first register, short enough
	// that a container-Ready pod with a crash-looping agent inside surfaces on the
	// same order as the incident.
	DefaultRegisterConvergenceGrace = 5 * time.Minute
	// PLACEHOLDER PENDING MEASUREMENT. Do not cite as measured.
	DefaultMaxUpgradingWindow = 45 * time.Minute

	// ControllerStuckAge mirrors the controller's own `stuckAge` (kube/rollhealth.go),
	// and it is declared here ONLY so the relationship below can be asserted. It is not
	// a knob and nothing reads it as configuration.
	//
	// THE RELATIONSHIP: MaxUpgradingWindow MUST exceed this. The controller's reasonless
	// arm keeps reporting `rolling` until a pod has been not-Ready for stuckAge; if the
	// api's ceiling expired first, the two halves would disagree about the same worker —
	// the controller still saying "rolling", the api having already reclassified — and
	// NEITHER package would notice, because the constants live in two modules chosen
	// independently. Asserted by TestMaxUpgradingWindowExceedsControllerStuckAge rather
	// than left to 45 > 10 happening to be true today.
	//
	// Kept in sync by that test failing, not by anyone remembering. If the controller's
	// stuckAge changes, this constant and that test are what surface it.
	ControllerStuckAge = 10 * time.Minute
)

func (p UpgradeParams) withDefaults() UpgradeParams {
	if p.ControllerSignalTTL <= 0 {
		p.ControllerSignalTTL = DefaultControllerSignalTTL
	}
	if p.HostedRollGrace <= 0 {
		p.HostedRollGrace = DefaultHostedRollGrace
	}
	if p.RegisterConvergenceGrace <= 0 {
		p.RegisterConvergenceGrace = DefaultRegisterConvergenceGrace
	}
	if p.MaxUpgradingWindow <= 0 {
		p.MaxUpgradingWindow = DefaultMaxUpgradingWindow
	}
	// FLOOR, not a default: a window at or below the controller's stuckAge creates a
	// silent disagreement between the two modules (the controller still reporting
	// `rolling` for a reasonless-stuck pod that this api has already stopped believing),
	// and neither can see the other's constant. Clamping makes the relationship
	// ENFORCED rather than merely asserted — the test named in ControllerStuckAge's
	// comment then checks the clamp instead of checking that two numbers happen to be
	// ordered.
	//
	// Clamping up rather than rejecting, because this is a display path: a misconfigured
	// window must not be able to fail a request. The value is deliberately not silently
	// accepted either — it is raised to the smallest value that keeps the two halves
	// consistent.
	if p.MaxUpgradingWindow <= ControllerStuckAge {
		p.MaxUpgradingWindow = ControllerStuckAge + time.Minute
	}
	return p
}

// ClassifyUpgrade evaluates the decision table top to bottom, first match wins.
//
// Rule numbers match the design's table so a reviewer can read one against the other.
// The ordering carries two decisions that are not obvious:
//
//   - R1/R2 (roll health) sit ABOVE R4 (unparseable target), so a `dev` control plane
//     disables VERSION COMPARISON but not roll health. A stuck pod is a direct
//     observation, not a comparison; suppressing it because the api was built without
//     a version stamp would throw away the incident's own signal.
//   - R8 (no-signal grace) sits BELOW the compare rows, so it can only ever soften
//     `outdated` — never override `up_to_date` or `unknown`.
//   - R7.5 (issue #738), a `settled`-only, `outdated`-only softener, sits just below
//     the compare rows too: it clears a hosted worker the controller confirms is on the
//     target image despite a trailing baked version (a reuse-retagged image, PRD #422).
//     Like R8 it can only soften `outdated`, never override `up_to_date` or `unknown`.
func ClassifyUpgrade(in UpgradeInput, p UpgradeParams) (status string, detail string) {
	status, detail, _ = classifyWithTarget(in, p)
	return status, detail
}

// ClassifyUpgradeWithTarget also returns the coordinate the worker was actually compared
// against (PRD #113 M5, B-1).
//
// The target is returned rather than recomputed by the caller because the two must never
// disagree: the Fleet panel states "hosted workers target X" beside a badge derived from
// X, and a second derivation is a second chance to be wrong. It is also the only way the
// panel can surface a divergence the classifier is REQUIRED to honour — Decision 9 makes
// a controller-pinned tag authoritative, so a legitimate pin and a suppression attack are
// indistinguishable here and the resolution is to say what the target is, visibly.
func ClassifyUpgradeWithTarget(in UpgradeInput, p UpgradeParams) (status, detail, target string) {
	return classifyWithTarget(in, p)
}

func classifyWithTarget(in UpgradeInput, p UpgradeParams) (status string, detail string, resolvedTarget string) {
	p = p.withDefaults()
	hosted := in.Kind == "hosted"
	s := in.Signal

	// A signal is fresh when the API's OWN receipt time is inside the TTL. Note what
	// is NOT consulted: the wire's controller_reported_at. See RollSignal.ObservedAt.
	signalFresh := s != nil && s.Phase != "" && in.Now.Sub(s.ObservedAt) <= p.ControllerSignalTTL

	// The INV-5 ceiling. Gates R2 ONLY (issue #151): past the window the controller
	// stops being believed when it claims a roll is still IN PROGRESS, and the row
	// classifies by version compare alone, so a suppressed `outdated` re-appears.
	//
	// It deliberately does NOT gate R1. The two directions are not symmetric, and the
	// reason is not the soft one about false alarms being self-limiting:
	//
	//   - The controller already OWNS these workers' lifecycle — it renders and patches
	//     their Deployments. A compromised controller does not need to ASSERT
	//     `upgrade_failed`; it can CAUSE it fleet-wide. Gating R1 bounds nothing an
	//     attacker cannot reach directly.
	//   - The suppression direction this invariant exists for is already conceded
	//     elsewhere: B-1 (a controller reporting a stale tag with phase=settled) makes
	//     the fleet read `up_to_date` indefinitely, the ceiling never engages, and D10's
	//     third amendment accepts that with "keep the behaviour, make it visible".
	//
	// What gating R1 cost, measured on worker d26fb0f9 (dev-cluster, 2026-07-26): the
	// anchor arms on the FIRST non-terminal report, which for a hosted worker is while
	// it is still being provisioned and nothing is wrong yet. That worker spent 33m50s
	// of a 45m budget pod-less before its pod could even appear, leaving a 70-SECOND
	// window in which the truthful `upgrade_failed` could be seen. So the ceiling
	// measures "how long have we believed a roll is in progress" while being read as
	// "how long may a pod be broken before we say so".
	//
	// Past the ceiling a `stuck` report is therefore STILL believed. R1 keeps its two
	// other guards (`signalFresh`, and a `stuck` phase derived from current pod state),
	// so the alert self-clears the moment the pod goes Ready — what it loses is only its
	// expiry, which is the point.
	ceilingOK := s == nil || s.UpgradingSince == nil || in.Now.Sub(*s.UpgradingSince) < p.MaxUpgradingWindow

	// Decision 9: for a hosted worker the target is the tag the controller actually
	// rolls to, since values.yaml can pin it independently of the api's release. An
	// unparseable tag falls back to the api's own version rather than blinding the
	// fleet — the PHASE half of the signal is still honoured.
	target := in.CPVersion
	if hosted {
		// PRD #422: the worker image tag is pinned independently of the api's own
		// release (appVersion), so a hosted worker's target is the pinned worker
		// version, NOT CPVersion. Empty/unparseable pin falls back to CPVersion
		// (today's behavior — no regression for an unconfigured deploy).
		if semver.IsValid(normSemver(in.PinnedWorkerVersion)) {
			target = in.PinnedWorkerVersion
		}
		// A FRESH controller RolledTag stays authoritative above the static pin: it is
		// what the controller actually rolls to, confirmed live (Decision 9).
		if signalFresh && semver.IsValid(normSemver(s.RolledTag)) {
			target = s.RolledTag
		}
	}

	// R1 is NOT ceiling-gated — see the ceilingOK comment above (issue #151).
	if hosted && signalFresh && s.Phase == PhaseStuck {
		return UpgradeStatusUpgradeFailed, stuckDetail(s), target // R1
	}
	if hosted && signalFresh && ceilingOK && s.Phase == PhaseRolling {
		return UpgradeStatusUpgrading, rollingDetail(s), target // R2
	}

	// R3 — the controller says the new pod is Ready but workers.version has not caught
	// up yet. A fresh pod registers on boot, so this is a seconds-long transient in the
	// healthy case; bounding it stops a pod that is container-Ready with a dead agent
	// inside from reading `upgrading` forever. This is the ONLY rule that reads
	// PhaseSince, and only here is its meaning sound (the Ready transition).
	if hosted && signalFresh && s.Phase == PhaseSettled && s.PhaseSince != nil &&
		in.Now.Sub(*s.PhaseSince) <= p.RegisterConvergenceGrace &&
		isBehind(in.Reported, target) {
		return UpgradeStatusUpgrading, fmt.Sprintf("rolled to %s; awaiting re-registration", target), target
	}

	// R4-R7, R9: the version-compare core.
	status, detail = classifyByVersion(in.Reported, target)

	// R7.5 (issue #738) — the controller has CONFIRMED this hosted worker's
	// current-generation pod is running its Deployment's pinned target image
	// (phase == settled means the current-spec-hash pod is Ready). So an `outdated`
	// from the version compare above is the baked UZI_AGENT_VERSION lying under a
	// reuse-retagged image (PRD #422): the tag advanced to a new release that points
	// at a PRIOR release's digest, so the baked version trails the tag while the
	// worker is, in fact, running the target. Trust the controller's direct
	// observation over the self-reported string. Softener only: it fires solely when
	// the compare already said `outdated`, so it can never override `unknown`, a
	// dev-plane, or an ahead/equal worker. Phase-settled only: a `rolling` worker
	// past the INV-5 ceiling (which falls through R2 to here) stays `outdated`.
	//
	// Valid-RolledTag only (the semver.IsValid guard): `settled` only proves the
	// current-gen pod runs the tag the controller RENDERED FROM, which the classifier
	// sees as RolledTag. When RolledTag is unparseable/absent the target falls back to
	// CPVersion/pin (see the target-resolution block above, ~:410 and :415) and
	// `settled` no longer confirms the worker is on that fallback coordinate, so we
	// defer to the version-compare fail-safe (`outdated`) rather than assert a false
	// `up_to_date` and hide real drift. Because R7.5 already requires signalFresh, a
	// valid RolledTag means target == RolledTag (:415-417), so the guard fires exactly
	// where "settled ⟹ on target" holds.
	if status == UpgradeStatusOutdated && hosted && signalFresh &&
		s.Phase == PhaseSettled && semver.IsValid(normSemver(s.RolledTag)) {
		return UpgradeStatusUpToDate,
			fmt.Sprintf("running the target image (%s); reported %s trails it (re-tagged image)", target, in.Reported),
			target
	}

	// R8 — behind, hosted, and NO fresh signal, within the grace after api start.
	// Model B rolls the api in the same release, so a recently started api means a
	// release just happened and an unconfirmed roll is the likeliest explanation for a
	// behind hosted worker. Strictly a softener: it fires only where the compare
	// already said `outdated`.
	if status == UpgradeStatusOutdated && hosted && !signalFresh &&
		in.Now.Sub(in.APIStartedAt) < p.HostedRollGrace {
		return UpgradeStatusUpgrading, "roll in progress (unconfirmed — no controller signal)", target
	}
	return status, detail, target
}

// isBehind reports whether reported is a usable version strictly below target. Used by
// R3, which must not fire for an unparseable or absent version — that is R5's answer
// and `unknown` outranks a guess about a roll.
func isBehind(reported, target string) bool {
	reported = strings.TrimSpace(reported)
	if reported == "" || reported == legacyFrozenAgentVersion {
		return false
	}
	if !semver.IsValid(normSemver(reported)) || !semver.IsValid(normSemver(target)) {
		return false
	}
	return semver.Compare(normSemver(reported), normSemver(target)) < 0
}

// stuckDetail is the failed-worker strip's sentence: which container, why, and the
// numbers an operator needs before opening a terminal. Every field is
// controller-supplied and was sanitized at ingest.
func stuckDetail(s *RollSignal) string {
	who := s.BlockingContainer
	if who == "" {
		who = "pod"
	}
	why := s.BlockingReason
	if why == "" {
		why = "not ready"
	}
	out := fmt.Sprintf("%s: %s", who, why)
	if s.RestartCount > 0 {
		out += fmt.Sprintf(" (%d restarts", s.RestartCount)
		if s.LastExitCode != nil {
			out += fmt.Sprintf(", last exit %d", *s.LastExitCode)
		}
		out += ")"
	} else if s.LastExitCode != nil {
		out += fmt.Sprintf(" (last exit %d)", *s.LastExitCode)
	}
	return out
}

// rollingDetail names what the pod is waiting on when it can, so "upgrading" is not
// an opaque state.
func rollingDetail(s *RollSignal) string {
	if s.BlockingReason != "" {
		return s.BlockingReason
	}
	if s.PodPhase != "" {
		return s.PodPhase
	}
	return "roll in progress"
}

// -------------------------------------------------------------------------
// Persistence (PRD #113 M4).
// -------------------------------------------------------------------------

// RollHealthReport is one validated, sanitized entry from a controller report, ready
// to persist. The handler does the decoding, enum validation and sanitizing; this
// layer only writes.
type RollHealthReport struct {
	WorkerID   uuid.UUID
	Phase      string
	PhaseSince *time.Time
	// TargetImage / PodPhase / Blocking* are controller-supplied display strings,
	// already passed through the 64-byte control-character-stripping sanitizer.
	TargetImage       string
	PodPhase          string
	BlockingContainer *string
	BlockingReason    *string
	RestartCount      int32
	LastExitCode      *int32
	// ControllerReportedAt is the wire's timestamp: persisted for display, never used
	// for freshness. ObservedAt is the api's own receipt time and is what freshness
	// reads. Keeping both named here rather than one "timestamp" is what stops the two
	// being collapsed by a later tidy-up.
	ControllerReportedAt time.Time
	ObservedAt           time.Time
	PollIntervalSeconds  int
	WorkerImageTag       string
}

// RecordRollHealth persists one worker's roll health and returns how many rows the
// upsert touched — 0 when the worker id is unknown or belongs to an EXTERNAL worker.
//
// The confinement to existing hosted rows lives in the SQL, not here (see
// queries/worker_roll_health.sql). The service returns the count rather than an error
// for a non-match because "the controller reported a worker we do not host" is not an
// error condition: it is a skew or a lie, both of which are handled by ignoring the row.
func (s *Service) RecordRollHealth(ctx context.Context, rep RollHealthReport) (int64, error) {
	// The SAME semver.IsValid(normSemver(...)) gate classifyWithTarget uses to decide
	// whether to trust the tag as the roll target, so the arm guard and the classifier
	// agree on which tags are usable. An invalid tag is passed as false so the SQL arms
	// fail-closed (NOT @tag_valid): that is exactly where the classifier falls back to
	// CPVersion and can say `outdated`, so the ceiling must engage.
	tagValid := semver.IsValid(normSemver(rep.WorkerImageTag))
	return s.q.UpsertWorkerRollHealth(ctx, store.UpsertWorkerRollHealthParams{
		WorkerID:             rep.WorkerID,
		Phase:                rep.Phase,
		PhaseSince:           timestamptzPtr(rep.PhaseSince),
		TargetImage:          pgText(rep.TargetImage),
		PodPhase:             pgText(rep.PodPhase),
		BlockingContainer:    textParam(rep.BlockingContainer),
		BlockingReason:       textParam(rep.BlockingReason),
		RestartCount:         rep.RestartCount,
		LastExitCode:         int4Ptr(rep.LastExitCode),
		ControllerReportedAt: pgtype.Timestamptz{Time: rep.ControllerReportedAt, Valid: !rep.ControllerReportedAt.IsZero()},
		ObservedAt:           pgtype.Timestamptz{Time: rep.ObservedAt, Valid: true},
		PollIntervalSeconds:  pgInt4(rep.PollIntervalSeconds),
		WorkerImageTag:       pgText(rep.WorkerImageTag),
		TagValid:             tagValid,
	})
}

// timestamptzPtr / int4Ptr render an optional wire value as SQL NULL rather than a
// zero. A zero time would claim a phase began at the epoch; a zero exit code would
// claim a clean exit for a container that never terminated.
func timestamptzPtr(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: *t, Valid: true}
}

func int4Ptr(v *int32) pgtype.Int4 {
	if v == nil {
		return pgtype.Int4{}
	}
	return pgtype.Int4{Int32: *v, Valid: true}
}

func pgInt4(v int) pgtype.Int4 {
	if v <= 0 {
		return pgtype.Int4{}
	}
	return pgtype.Int4{Int32: int32(v), Valid: true}
}

// -------------------------------------------------------------------------
// Fleet summary (PRD #113 M6): the number behind the Workers nav badge.
// -------------------------------------------------------------------------

// UpgradeFleetSummary is the AppShell's view of one user's fleet.
type UpgradeFleetSummary struct {
	// Attention is the count of the user's workers needing attention: upgrade_failed
	// plus outdated, minus muted (Decision 1). `upgrading` is deliberately excluded —
	// a roll in progress is expected, transient and self-resolving, and counting it
	// would turn the badge red on every release, which is the cry-wolf the whole PRD
	// exists to avoid.
	Attention int `json:"attention"`
	// TargetRelease is the control plane's own version, echoed so the client can label
	// the badge's tooltip without a second request. Empty when unstamped.
	TargetRelease string `json:"target_release"`
}

// UpgradeSummaryForUser counts the caller's workers needing attention.
//
// SCOPED BY CONSTRUCTION, not by a filter anyone has to remember: the query it calls
// joins roll health THROUGH `workers`, which is the only table that knows an owner,
// because worker_upgrade_reports deliberately carries no user_id. A fleet-scoped count
// here would put other users' failing workers into this user's nav badge on every page
// of the app.
//
// The classification is the SAME ClassifyUpgrade the worker DTO uses, over the same
// inputs. That matters more than the small duplication it avoids: if this endpoint
// counted with its own rules, the badge could say "1 needs attention" beside a list in
// which nothing is badged, and no test comparing the two would exist to catch it.
//
// pinnedWorkerVersion is the deploy's concrete pinned hosted-worker image tag (PRD #422,
// the api's HostedWorkerVersion / workers.image.tag), threaded onto each UpgradeInput so a
// hosted worker intentionally pinned behind appVersion is not counted `outdated`. Empty
// falls back to cpVersion — today's behavior for an unconfigured deploy.
func (s *Service) UpgradeSummaryForUser(ctx context.Context, userID uuid.UUID, cpVersion, pinnedWorkerVersion string, apiStartedAt time.Time, p UpgradeParams) (UpgradeFleetSummary, error) {
	rows, err := s.q.GetWorkerUpgradeSummaryForUser(ctx, userID)
	if err != nil {
		return UpgradeFleetSummary{}, err
	}
	now := s.now()
	out := UpgradeFleetSummary{TargetRelease: cpVersion}
	for _, row := range rows {
		var sig *RollSignal
		if row.Phase.Valid && row.ObservedAt.Valid {
			sig = &RollSignal{
				Phase:             row.Phase.String,
				ObservedAt:        row.ObservedAt.Time,
				PodPhase:          row.PodPhase.String,
				BlockingContainer: row.BlockingContainer.String,
				BlockingReason:    row.BlockingReason.String,
				RolledTag:         row.WorkerImageTag.String,
			}
			if row.RestartCount.Valid {
				sig.RestartCount = row.RestartCount.Int32
			}
			if row.LastExitCode.Valid {
				code := row.LastExitCode.Int32
				sig.LastExitCode = &code
			}
			if row.PhaseSince.Valid {
				t := row.PhaseSince.Time
				sig.PhaseSince = &t
			}
			if row.UpgradingSince.Valid {
				t := row.UpgradingSince.Time
				sig.UpgradingSince = &t
			}
		}
		status, _ := ClassifyUpgrade(UpgradeInput{
			Reported:            row.Version.String,
			Kind:                row.Kind,
			CPVersion:           cpVersion,
			PinnedWorkerVersion: pinnedWorkerVersion,
			Signal:              sig,
			Now:                 now,
			APIStartedAt:        apiStartedAt,
		}, p)
		if !InUpgradeAttentionSet(status) {
			continue
		}
		// The mute subtraction is REAL, not decorative: `muted` comes from a join on
		// worker_upgrade_mutes keyed per user, per worker, per release. No UI can set a
		// mute yet, so in practice this never subtracts today — but it is a live branch
		// over live data, so it starts working the moment the action ships rather than
		// needing to be found and wired.
		if row.Muted {
			continue
		}
		out.Attention++
	}
	return out, nil
}

// InUpgradeAttentionSet is the ONE definition of "needs attention" on the server side
// (Decision 1): failed or behind. Exported so the nav badge's count and any other
// consumer cannot drift into two answers.
func InUpgradeAttentionSet(status string) bool {
	return status == UpgradeStatusUpgradeFailed || status == UpgradeStatusOutdated
}

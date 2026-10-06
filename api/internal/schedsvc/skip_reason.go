package schedsvc

import (
	"errors"

	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// SkipReason is the authoritative, closed set of reasons a schedule fire started no
// run for a candidate (PRD #308 M1). It is declared here — once, in Go — as the single
// source of truth: a later-milestone cross-language contract test parses the plain
// "snake_case" string literals out of THIS file to prove the TS reason union has not
// drifted from the Go enum. So keep each reason a bare string literal in the const
// block below, and keep any prose in // comments the parser can strip.
type SkipReason string

const (
	SkipConfigNotSupported SkipReason = "config_not_supported"
	// SkipNotEligible ← workersvc.ErrNotPRDIssue: the issue does not carry the uzi label.
	SkipNotEligible SkipReason = "not_eligible"

	// SkipAlreadyRunning ← the active-run pre-check bool (HasActiveRunForIssue /
	// HasActiveRunForSchedule true) AND the seam's ErrActiveRunExists / ErrActivePromptExists
	// race. A prior run for the same issue/schedule is still live. Also ←
	// workersvc.ErrBranchInUse (issue #1626): an active ci_fix / mr_rework run holds the
	// issue's agent/issue-<iid> branch — except for a one-time issue schedule, which
	// createIssueRun holds un-advanced (transient) so the issue run still starts later.
	SkipAlreadyRunning SkipReason = "already_running"

	// SkipDescriptionTooLarge ← workersvc.ErrDescriptionTooLarge: the composed run
	// description exceeds the run-creation cap.
	SkipDescriptionTooLarge SkipReason = "description_too_large"

	// SkipFetchFailed ← a per-candidate transient error inside a sweep fan-out (an
	// active-run DB error, a forge GetIssue error, or an unexpected mid-sweep create
	// error) that today is logged-and-continued. Recorded so the candidate is not
	// silently dropped and the matched == started + skipped invariant still balances.
	SkipFetchFailed SkipReason = "fetch_failed"

	// SkipVaultLocked ← the self_improve fire path (PRD #590 M1) when the enabling owner's
	// vault DEK is not cached, so the autonomous run cannot spend the owner's token this
	// cycle. Benign: the schedule advances normally (the cadence re-fires on schedule once
	// the vault is unlocked), and the owner gets a selfimprove_skipped notification.
	SkipVaultLocked SkipReason = "vault_locked"

	// SkipSelfImproveMRCapReached ← the self_improve fire path (PRD #686 D10) when the repo
	// already has selfImproveMaxOpenMRs OPEN self-improve MRs (open-state resolved LIVE from
	// the forge per candidate, not from runs.mr_state — D12). Benign: the schedule advances
	// normally and re-fires next cadence once a human merges or closes an outstanding MR, and
	// the owner gets a selfimprove_skipped notification.
	SkipSelfImproveMRCapReached SkipReason = "self_improve_mr_cap_reached"

	// SkipOpenMRExists ← workersvc.ErrOpenMRExists (issue #856): a prior completed run for
	// this issue still owns an OPEN merge request, so createRun refuses a fresh run. Benign:
	// the sweep skips this candidate and advances, and the cadence re-fires once the MR is
	// merged or closed.
	SkipOpenMRExists SkipReason = "open_mr_exists"

	// SkipCodexOverrideConflict ← the fire-time codex+override fail-closed gate (PRD #1429 M2, D5):
	// a null-harness schedule carrying a stored Anthropic credential override now resolves to Codex
	// under D11. The Anthropic override cannot ride a Codex run and there is no cross-harness
	// fallback, so the fire starts no run and leaves the stored override untouched. Benign for a
	// recurring row: the schedule advances and re-fires next cadence (a credential change may make
	// it resolvable again). Also mapped from workersvc.ErrCredentialOverrideHarnessUnsupported by
	// skipReasonForErr, so a stored override that reaches the create seam classifies the same way.
	SkipCodexOverrideConflict SkipReason = "codex_override_conflict"

	// SkipSchedulesPaused ← the user-level "pause all schedules" kill switch (PRD #1093):
	// the owner has paused every schedule they own (optionally until an auto-resume instant),
	// so a fire that comes due while paused starts no run. Benign for a recurring row: the
	// schedule advances normally, the cadence re-fires on resume, and nothing replays (the
	// #396 property). A once row is different — it is held un-advanced in process() so it
	// fires on the first tick after the pause ends — and so never records this skip.
	SkipSchedulesPaused SkipReason = "schedules_paused"

	// SkipNoUsableCredential ← workersvc.ErrNoUsableCredential (PRD #1429 review fix): D11
	// resolved NEITHER harness usable for the owner (no Anthropic token AND no usable Codex
	// credential) and no explicit harness selection forces a hard refusal. Before M2 this fell
	// through skipReasonForErr's default arm as a TRANSIENT error, so the fire never advanced
	// and the schedule re-fired (and re-hit the forge) every tick — a tick-storm. Benign for a
	// recurring row instead: the schedule advances normally and re-fires next cadence, once the
	// owner configures a usable credential. This is deliberately narrower than
	// ErrNoCredentialForHarness (an EXPLICIT/inherited harness that is unusable), which stays a
	// hard, non-advancing refusal — only the implicit "neither harness usable" case is benign.
	SkipNoUsableCredential SkipReason = "no_usable_credential" //nolint:gosec // G101: a schedule-skip-reason VOCABULARY value, not a credential — mirrors the same-shaped exclusion already granted the "credential"-named vocabulary constants elsewhere (e.g. store.KindAnthropicToken, capability.CredentialSwitchV1).

	// SkipCredentialDisabled ← workersvc.ErrCredentialDisabled or
	// workersvc.ErrHarnessCredentialDisabled (PRD #1732 D2/D15): the schedule's stored token pin
	// is a credential the owner has disabled, or its pinned harness has no enabled credential.
	// The fire starts no run and never substitutes another credential or harness; the stored
	// pin is kept. Benign for a recurring row: the schedule advances and re-fires next cadence,
	// once the owner enables the credential or changes the pin. A one-time row is held
	// un-advanced instead, with this skip recorded in last_fire (holdsOnceCredentialDisabled), so
	// its pinned work waits rather than being consumed. An implicit-harness fire with no enabled credential anywhere records
	// no_usable_credential instead (D15).
	SkipCredentialDisabled SkipReason = "credential_disabled" //nolint:gosec // G101: a schedule-skip-reason VOCABULARY value, not a credential (see SkipNoUsableCredential).
)

// AllSkipReasons lists every SkipReason in the closed set. The cross-language contract
// test (a later milestone) reads this to enumerate the Go side.
var AllSkipReasons = []SkipReason{
	SkipConfigNotSupported,
	SkipNotEligible,
	SkipAlreadyRunning,
	SkipDescriptionTooLarge,
	SkipFetchFailed,
	SkipVaultLocked,
	SkipSelfImproveMRCapReached,
	SkipOpenMRExists,
	SkipCodexOverrideConflict,
	SkipSchedulesPaused,
	SkipNoUsableCredential,
	SkipCredentialDisabled,
}

// skipReasonForErr maps the benign run-creation seam sentinels to their SkipReason.
// It returns (reason, true) for a recognized benign sentinel and ("", false) for
// anything else, so the caller decides whether an unrecognized error is transient or
// permanent rather than this helper guessing. ErrActivePromptExists is intentionally NOT
// mapped here — the prompt path records already_running at its own site — but
// ErrActiveRunExists is, since createIssueRun classifies it inline.
func skipReasonForErr(err error) (SkipReason, bool) {
	switch {
	case errors.Is(err, workersvc.ErrNotPRDIssue):
		return SkipNotEligible, true
	case errors.Is(err, workersvc.ErrActiveRunExists):
		return SkipAlreadyRunning, true
	case errors.Is(err, workersvc.ErrBranchInUse):
		// Issue #1626: an active ci_fix / mr_rework run is already working this issue's
		// agent/issue-<iid> branch, so a fresh issue run for it is refused. That is an active
		// run working the issue, so it records the existing already_running reason (the closed
		// set is mirrored by the web union; no new wire value). Benign, advancing: without this
		// arm it fell to the transient default and the schedule re-fired every tick. Exception:
		// createIssueRun returns it as transient for a one-time issue schedule before reaching
		// this helper, since advancing a once row marks it fired and the run never starts.
		return SkipAlreadyRunning, true
	case errors.Is(err, workersvc.ErrDescriptionTooLarge):
		return SkipDescriptionTooLarge, true
	case errors.Is(err, workersvc.ErrOpenMRExists):
		return SkipOpenMRExists, true
	case errors.Is(err, workersvc.ErrCredentialOverrideHarnessUnsupported):
		// PRD #1429 M2 (D5): a stored Anthropic override that reached the create seam on a run
		// resolving to Codex. The scheduler's fire-time gate normally catches this first, but the
		// mapping keeps the classification stable if a fire ever surfaces it via the seam.
		return SkipCodexOverrideConflict, true
	case errors.Is(err, workersvc.ErrCredentialDisabled), errors.Is(err, workersvc.ErrHarnessCredentialDisabled):
		// PRD #1732 D2/D15: a disabled stored pin, or a pinned harness with no enabled
		// credential. Checked before any other credential arm: ErrHarnessCredentialDisabled
		// wraps ErrNoCredentialForHarness, which otherwise stays a hard refusal. Benign,
		// advancing, and never a substitution. Exception: the fire paths return it as transient
		// for a one-time schedule before reaching this helper (holdsOnceCredentialDisabled).
		return SkipCredentialDisabled, true
	case errors.Is(err, workersvc.ErrNoUsableCredential):
		// Review fix (PRD #1429): D11 found neither harness usable for the owner. Checked with
		// errors.Is (not ==) because ErrNoUsableCredential wraps errCredentialUnavailable. Benign,
		// advancing — see the SkipNoUsableCredential doc for why this must NOT fall to the
		// transient default arm (it would tick-storm: re-fire, re-hit the forge, never advance).
		// ErrNoCredentialForHarness is intentionally NOT mapped here: an explicit/inherited
		// harness with no usable credential stays a hard, non-advancing refusal.
		return SkipNoUsableCredential, true
	default:
		return "", false
	}
}

// holdsOnceCredentialDisabled reports whether a fire error is the credential_disabled refusal
// (a disabled stored pin, or a pinned harness with no enabled credential) on a ONE-TIME
// schedule. The fire paths return such an error instead of the benign advancing skip: advancing
// a once row marks it fired and its pinned work would never start, while PRD #1732 D2 says
// pinned work waits. process() then holds the row quietly (holdOnceCredentialDisabled): not
// advanced, the credential_disabled skip recorded in last_fire, and no transient-error warning,
// until the owner enables the credential or changes the pin. process() also runs the same check
// before firing (RunCreator.ScheduleCredentialDisabled), so a held row spends no forge call per
// tick. A recurring row keeps the benign skip and advances.
func holdsOnceCredentialDisabled(sched store.RunSchedule, err error) bool {
	if sched.Timing != "once" {
		return false
	}
	reason, ok := skipReasonForErr(err)
	return ok && reason == SkipCredentialDisabled
}

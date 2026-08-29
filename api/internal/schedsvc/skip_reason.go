package schedsvc

import (
	"errors"

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
	// SkipNoPRDLink ← workersvc.ErrNoPRDLink: the issue has no PRD link and no PRDLESS
	// waiver, so the waiver-free scheduled seam refused to start it.
	SkipNoPRDLink SkipReason = "no_prd_link"

	// SkipNotEligible ← workersvc.ErrNotPRDIssue: the issue does not carry the PRD label.
	SkipNotEligible SkipReason = "not_eligible"

	// SkipAlreadyRunning ← the active-run pre-check bool (HasActiveRunForIssue /
	// HasActiveRunForSchedule true) AND the seam's ErrActiveRunExists / ErrActivePromptExists
	// race. A prior run for the same issue/schedule is still live.
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
)

// AllSkipReasons lists every SkipReason in the closed set. The cross-language contract
// test (a later milestone) reads this to enumerate the Go side.
var AllSkipReasons = []SkipReason{
	SkipNoPRDLink,
	SkipNotEligible,
	SkipAlreadyRunning,
	SkipDescriptionTooLarge,
	SkipFetchFailed,
	SkipVaultLocked,
	SkipSelfImproveMRCapReached,
}

// skipReasonForErr maps the four benign run-creation seam sentinels to their SkipReason.
// It returns (reason, true) for a recognized benign sentinel and ("", false) for
// anything else, so the caller decides whether an unrecognized error is transient or
// permanent rather than this helper guessing. ErrActivePromptExists is intentionally NOT
// mapped here — the prompt path records already_running at its own site — but
// ErrActiveRunExists is, since createIssueRun classifies it inline.
func skipReasonForErr(err error) (SkipReason, bool) {
	switch {
	case errors.Is(err, workersvc.ErrNoPRDLink):
		return SkipNoPRDLink, true
	case errors.Is(err, workersvc.ErrNotPRDIssue):
		return SkipNotEligible, true
	case errors.Is(err, workersvc.ErrActiveRunExists):
		return SkipAlreadyRunning, true
	case errors.Is(err, workersvc.ErrDescriptionTooLarge):
		return SkipDescriptionTooLarge, true
	default:
		return "", false
	}
}

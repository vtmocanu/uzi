// Package weeklylimit builds the owner-only weekly-limit block (PRD #4123): why a run parked on its
// credential's weekly window could not switch, what the owner can do about it, and what will happen to
// the run. It is a pure function of one workersvc.NextClaimSnapshot plus the run row; the run DTO
// builder and the Slack park notifier both call it, so every surface shows one computation.
package weeklylimit

import (
	"context"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/shellquote"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/termsafe"
	"github.com/vtmocanu/uzi/api/internal/usagereserve"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Fix kinds on the wire.
const (
	FixSetToken      = "set_token"
	FixAutoSelect    = "auto_select"
	FixPoolOn        = "pool_on"
	FixResumeReserve = "resume_reserve"
	FixEditReserve   = "edit_reserve"
)

const (
	rateLimitSevenDay = "seven_day"
	holdWeeklyReserve = "weekly_reserve"
	harnessCodex      = "codex"
)

// Loader builds weekly-limit blocks from a workersvc.Service. It holds no per-owner state.
type Loader struct {
	svc *workersvc.Service
	// Now is the clock the snapshot is read at; time.Now when nil.
	Now func() time.Time
}

// NewLoader returns a Loader over svc.
func NewLoader(svc *workersvc.Service) *Loader { return &Loader{svc: svc} }

func (l *Loader) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

// excluded reports a run the block never speaks for, decided from its frozen run-row binding alone
// (so no read is needed): a provider-bound run and a Codex API-key run have no subscription window.
// It is the workersvc predicate itself, so the preview and the block never disagree.
func excluded(run store.Run) bool { return workersvc.ExcludedFromNextClaim(run) }

// parked reports the M1 presence condition: a weekly-window limit_wait park, or a weekly-reserve pause.
func parked(run store.Run) bool {
	switch run.Status {
	case "limit_wait":
		return run.RateLimitType.Valid && run.RateLimitType.String == rateLimitSevenDay
	case "paused":
		return run.HoldReason.Valid && run.HoldReason.String == holdWeeklyReserve
	}
	return false
}

// boundNonTerminal is the read-free half of the near/at presence condition (PRD #4123 M2): the run is
// not terminal and already names the vendor credential it spends. An unbound queued run has none, so
// no credential warning is fabricated for it; whether that credential is actually near its limit needs
// the snapshot's margin and reading.
func boundNonTerminal(run store.Run) bool {
	if apitypes.IsTerminalRunStatus(run.Status) {
		return false
	}
	if run.Harness == harnessCodex {
		return run.CodexSecretID.Valid
	}
	return run.AnthropicSecretID.Valid || run.LimitDeadSecretID.Valid
}

// needsSnapshot is the cheap, read-free pre-filter: structural exclusion first, then every presence
// predicate that can hold for the run (a weekly park, or a bound non-terminal run that may be near its
// limit).
func needsSnapshot(run store.Run) bool {
	if excluded(run) {
		return false
	}
	return parked(run) || boundNonTerminal(run)
}

// nearOrAt reports the M2 presence condition on a loaded snapshot: the early warnings are on (margin >
// 0) and the run's credential has a usable fresh weekly reading that is near or at its effective limit
// (the reserve threshold, else 100). A stale, missing or invalid reading is unknown, never near.
func nearOrAt(run store.Run, snap *workersvc.NextClaimSnapshot) bool {
	if !boundNonTerminal(run) || snap.MarginPts <= 0 {
		return false
	}
	key, _, ok := snap.CurrentCredential(run)
	if !ok {
		return false
	}
	raw, found := snap.Weekly(key)
	if !found {
		return false
	}
	switch usagereserve.EvaluateNear(snap.ReserveThreshold(), snap.MarginPts, raw.Weekly()).State {
	case usagereserve.NearNear, usagereserve.NearAt:
		return true
	}
	return false
}

func anyParked(runs []store.Run) bool {
	for _, r := range runs {
		if parked(r) {
			return true
		}
	}
	return false
}

// ForRuns builds the blocks for one owner's runs. Runs that cannot carry a block are dropped before
// anything is read; when none remain no snapshot is loaded at all. A load failure yields no blocks
// (the surfaces then simply omit the block, as for an unparked run). The map holds only runs with a block.
func (l *Loader) ForRuns(ctx context.Context, ownerID uuid.UUID, runs []store.Run) map[uuid.UUID]*apitypes.WeeklyLimitDTO {
	var eligible []store.Run
	for _, r := range runs {
		if needsSnapshot(r) {
			eligible = append(eligible, r)
		}
	}
	if len(eligible) == 0 {
		return nil
	}
	snap, err := l.svc.LoadNextClaimSnapshot(ctx, ownerID, l.now())
	if err == nil && snap.MarginPts <= 0 && !anyParked(eligible) {
		// Early warnings are off and nothing is parked: no run can carry a block, so skip the
		// per-run settling reads.
		return nil
	}
	if err == nil {
		err = l.svc.LoadNextClaimSettling(ctx, snap, eligible)
	}
	if err != nil {
		slog.WarnContext(ctx, "weeklylimit: next-claim snapshot unavailable; omitting the weekly-limit block", "owner", ownerID, "err", err)
		return nil
	}
	out := make(map[uuid.UUID]*apitypes.WeeklyLimitDTO, len(eligible))
	for _, r := range eligible {
		if dto := l.ForRun(r, snap); dto != nil {
			out[r.ID] = dto
		}
	}
	return out
}

// ForRun builds one run's block from a snapshot, or nil when the run carries none.
func (l *Loader) ForRun(run store.Run, snap *workersvc.NextClaimSnapshot) *apitypes.WeeklyLimitDTO {
	if !needsSnapshot(run) {
		return nil
	}
	if !parked(run) && !nearOrAt(run, snap) {
		return nil
	}
	res := l.svc.NextClaimReason(run, snap)
	if res == nil {
		return nil
	}
	key, label, ok := snap.CurrentCredential(run)
	if !ok {
		return nil
	}
	dto := &apitypes.WeeklyLimitDTO{
		CredentialLabel: termsafe.CellText(label),
		CredentialKey:   key,
		Harness:         run.Harness,
		EffectiveLimit:  snap.EffectiveLimit(),
		Reason:          res.Reason,
		Wait:            previewWait(run, snap),
		EarlyWarningOff: snap.MarginPts <= 0,
		Fix:             []apitypes.WeeklyLimitFixDTO{},
		FutureRuns:      []apitypes.WeeklyLimitFixDTO{},
	}
	if raw, found := snap.Weekly(key); found && raw.OK {
		pct := raw.Pct
		dto.Pct = &pct
		dto.ResetsAt = raw.ResetsAt
	}
	if dto.ResetsAt == nil && run.LimitResetsAt.Valid {
		t := run.LimitResetsAt.Time
		dto.ResetsAt = &t
	}
	// The pinned and not_auto lanes never run the selector, so the result names no alternative;
	// name the best usable token so the owner still sees what a manual switch would move to.
	if res.Alternative == nil && (res.Reason == workersvc.NextClaimPinned || res.Reason == workersvc.NextClaimNotAuto) {
		res.Alternative = snap.UsableAlternative(run)
	}
	if res.Alternative != nil {
		dto.AlternativeLabel = termsafe.CellText(res.Alternative.Label)
	}
	dto.Fix, dto.FutureRuns = fixes(run, res, snap)
	dto.Tier = apitypes.WeeklyLimitTierNotice
	if len(dto.Fix) > 0 {
		dto.Tier = apitypes.WeeklyLimitTierWarning
	}
	return dto
}

// previewWait is what will happen to the run when its weekly window is hit. A run parked on another
// window (five_hour, a per-model window) is not parked on this one, so its stored retry stamps say
// nothing about the weekly window: its wait is unknown rather than a promise taken from the wrong park.
func previewWait(run store.Run, snap *workersvc.NextClaimSnapshot) string {
	if run.Status == "limit_wait" && !parked(run) {
		return workersvc.WaitUnknown
	}
	return snap.PreviewWait(run)
}

// fixes returns the run-level fixes and the future-run advice. A fix appears only when its own
// predicate holds; a label-bearing command is built only for a command-safe label.
func fixes(run store.Run, res *workersvc.NextClaimResult, snap *workersvc.NextClaimSnapshot) (runFix, future []apitypes.WeeklyLimitFixDTO) {
	runFix = []apitypes.WeeklyLimitFixDTO{}
	future = []apitypes.WeeklyLimitFixDTO{}
	runID := run.ID.String()

	if run.Status == "paused" && run.HoldReason.Valid && run.HoldReason.String == holdWeeklyReserve {
		runFix = append(runFix,
			apitypes.WeeklyLimitFixDTO{Kind: FixResumeReserve, TargetID: runID, Command: "uzi run resume " + runID},
			apitypes.WeeklyLimitFixDTO{Kind: FixEditReserve},
		)
	}

	anthropic := run.Harness != harnessCodex
	if res.ManualSwitch && res.Alternative != nil {
		runFix = append(runFix, labelFix(FixSetToken, res.Alternative.ID, res.Alternative.Label,
			func(q string) string { return "uzi run set-token " + runID + " -- " + q }))
	}
	if res.ManualSwitch && anthropic && (res.Reason == workersvc.NextClaimPinned || res.Reason == workersvc.NextClaimNotAuto) {
		runFix = append(runFix, apitypes.WeeklyLimitFixDTO{Kind: FixAutoSelect, TargetID: runID, Command: "uzi run set-token " + runID + " --auto"})
	}
	// A chat spends the owner's default token whatever the worker binding (PRD #104 D1), so
	// re-binding the worker would not help it and would change the worker's other work.
	if res.Reason == workersvc.NextClaimNotAuto && anthropic && run.WorkerID.Valid &&
		run.Kind != runkind.Judge && run.Kind != runkind.SelfImprove && run.Kind != runkind.Chat {
		wid := uuid.UUID(run.WorkerID.Bytes)
		if name, mode, ok := snap.WorkerBinding(wid); ok && mode != workersvc.BindModeAuto {
			fix := apitypes.WeeklyLimitFixDTO{
				Kind: FixAutoSelect, TargetID: wid.String(), TargetLabel: termsafe.CellText(name),
				Command: "uzi worker set-token " + wid.String() + " --auto",
			}
			if hasNoOverride(run) {
				runFix = append(runFix, fix)
			} else {
				future = append(future, fix)
			}
		}
	}

	for _, ff := range res.FutureRuns {
		switch ff.Kind {
		case workersvc.FutureFixPoolOn:
			if ff.Credential == nil {
				continue
			}
			kind := "anthropic"
			if ff.Credential.Kind == store.KindCodexAuth {
				kind = "codex"
			}
			fix := labelFix(FixPoolOn, ff.Credential.ID, ff.Credential.Label,
				func(q string) string { return "uzi token pool --on --kind " + kind + " -- " + q })
			// An Anthropic run in an auto lane re-selects at its next claim, so pooling the
			// alternative helps this run; a Codex run keeps its frozen account, so it only helps
			// runs created afterwards.
			if anthropic && res.Reason == workersvc.NextClaimHeadroomOutside && run.Kind != runkind.Chat {
				runFix = append(runFix, fix)
			} else {
				future = append(future, fix)
			}
		case workersvc.FutureFixCodexModeAuto:
			future = append(future, apitypes.WeeklyLimitFixDTO{Kind: FixAutoSelect, Command: "uzi settings codex-mode auto"})
		}
	}
	return runFix, future
}

// hasNoOverride reports a run whose next claim follows its worker's binding: no per-run override, or
// a pin whose credential was removed (it inherits).
func hasNoOverride(run store.Run) bool {
	if !run.CredentialOverrideMode.Valid || run.CredentialOverrideMode.String == "" {
		return true
	}
	return run.CredentialOverrideMode.String == workersvc.BindModePinned && !run.CredentialOverrideSecretID.Valid
}

// labelFix builds a fix whose command carries an owner-authored label. The command is built only for
// a command-safe label (CellText leaves it unchanged, it validates, and it holds no replacement
// character): sanitizing a grandfathered label would change the argument and could name another
// credential. Otherwise the fix keeps its kind and target but carries no command.
func labelFix(kind string, id uuid.UUID, label string, build func(quoted string) string) apitypes.WeeklyLimitFixDTO {
	fix := apitypes.WeeklyLimitFixDTO{Kind: kind, TargetID: id.String(), TargetLabel: termsafe.CellText(label)}
	if commandSafe(label) {
		fix.Command = build(shellquote.Quote(label))
	} else {
		fix.CommandUnavailable = apitypes.WeeklyFixCommandLabel
	}
	return fix
}

// commandSafe also refuses a backtick, which would end the Slack code span the command is shown in.
func commandSafe(label string) bool {
	return label != "" && termsafe.CellText(label) == label && termsafe.Validate("label", label) == nil &&
		!strings.ContainsRune(label, utf8.RuneError) && !strings.ContainsRune(label, '`')
}

// ForRunID is the Slack park notifier's entry: it reads the run by id and returns its block (owner
// scope, since the notifier delivers to the run's own owner), or nil when the run is gone, carries
// none, or the read fails.
func (l *Loader) ForRunID(ctx context.Context, runID uuid.UUID) *apitypes.WeeklyLimitDTO {
	run, err := l.svc.GetRunForViewer(ctx, uuid.Nil, true, runID)
	if err != nil {
		return nil
	}
	return l.ForRuns(ctx, run.UserID, []store.Run{run})[run.ID]
}

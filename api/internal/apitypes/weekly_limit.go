package apitypes

import "time"

// Weekly-limit block tiers, reasons, fix kinds and wait answers (PRD #4123). The server computes the
// block (weeklylimit.ForRun) and every surface renders it; none re-derives.
const (
	WeeklyLimitTierWarning = "warning"
	WeeklyLimitTierNotice  = "notice"

	// WeeklyFixCommandLabel is the command_unavailable value for a fix whose label cannot be put into
	// a copyable command without changing it.
	WeeklyFixCommandLabel = "label"
)

// WeeklyLimitFixDTO is one allowlisted action the owner can take. Command is rendered once by the
// server (the same string on every surface); it is empty with CommandUnavailable set when the target
// label is not command-safe, and empty for Settings-only fixes.
type WeeklyLimitFixDTO struct {
	// Kind is set_token | auto_select | pool_on | resume_reserve | edit_reserve.
	Kind string `json:"kind"`
	// TargetID is the credential or run the fix is about, when there is one.
	TargetID    string `json:"target_id,omitempty"`
	TargetLabel string `json:"target_label,omitempty"`
	Command     string `json:"command,omitempty"`
	// CommandUnavailable is "label" when Command is withheld because the label is not command-safe.
	CommandUnavailable string `json:"command_unavailable,omitempty"`
}

// WeeklyLimitDTO is the run-detail weekly-limit block. Owner-only: it names credentials and
// alternatives, so it is never built for a viewer who is not the run's owner.
type WeeklyLimitDTO struct {
	CredentialLabel string `json:"credential_label"`
	// CredentialKey is the owner-scoped grouping identity: "anthropic:<secret id>" or
	// "codex:<provider account id>".
	CredentialKey string `json:"credential_key"`
	Harness       string `json:"harness"`
	// Pct is the credential's weekly utilisation, null when no fresh reading exists.
	Pct            *float64   `json:"pct"`
	EffectiveLimit float64    `json:"effective_limit"`
	ResetsAt       *time.Time `json:"resets_at"`
	// Tier is warning when the run itself has a fix, else notice.
	Tier string `json:"tier"`
	// Reason is a workersvc.NextClaimReason value.
	Reason           string `json:"reason"`
	AlternativeLabel string `json:"alternative_label,omitempty"`
	// FutureRuns is advice for runs created afterwards.
	FutureRuns []WeeklyLimitFixDTO `json:"future_runs"`
	// Fix is advice for this run.
	Fix []WeeklyLimitFixDTO `json:"fix"`
	// Wait is waits | fails | reserve_pause | unknown.
	Wait string `json:"wait"`
	// EarlyWarningOff is set when the owner turned early warnings off (weekly_warning_margin_pts
	// = 0). It is server-only: margin 0 keeps the park explanation and its fixes on detail, CLI
	// and Slack, but Summary projects such a block as notice, so it creates no Overview limit
	// decision (PRD #4123 module 6 / D7). The zero value keeps the warning tier.
	EarlyWarningOff bool `json:"-"`
}

// WeeklyLimitSummaryDTO is the compact projection carried on a run summary row.
type WeeklyLimitSummaryDTO struct {
	Tier            string `json:"tier"`
	CredentialKey   string `json:"credential_key"`
	CredentialLabel string `json:"credential_label"`
}

// Summary projects the block onto the summary shape; nil for nil.
func (w *WeeklyLimitDTO) Summary() *WeeklyLimitSummaryDTO {
	if w == nil {
		return nil
	}
	tier := w.Tier
	if tier == WeeklyLimitTierWarning && w.EarlyWarningOff {
		tier = WeeklyLimitTierNotice
	}
	return &WeeklyLimitSummaryDTO{Tier: tier, CredentialKey: w.CredentialKey, CredentialLabel: w.CredentialLabel}
}

// weeklyLimitFromSummary is Summary's inverse for ListItem: only the three summary fields come
// back; the detail fields stay zero, as for every other detail field ListItem omits.
func weeklyLimitFromSummary(s *WeeklyLimitSummaryDTO) *WeeklyLimitDTO {
	if s == nil {
		return nil
	}
	return &WeeklyLimitDTO{Tier: s.Tier, CredentialKey: s.CredentialKey, CredentialLabel: s.CredentialLabel}
}

// ReasonText is the one-sentence, plain-language reason of the block, shared by the surfaces that
// render text (Slack, CLI). It names only server-validated labels; callers escape it for their
// medium. Empty for a nil block.
func (w *WeeklyLimitDTO) ReasonText() string {
	if w == nil {
		return ""
	}
	alt := w.AlternativeLabel
	switch w.Reason {
	case "switch_pending":
		return "a credential switch is already in progress for this run"
	case "can_switch":
		if alt != "" {
			return "this run can move to " + alt + " on its next claim"
		}
		return "this run can move to another token on its next claim"
	case "selection_unchanged":
		return "auto-select still prefers this credential even though another pooled one is usable"
	case "headroom_outside_pool":
		if alt != "" {
			return alt + " has weekly headroom but is not in the auto-select pool"
		}
		return "another token has weekly headroom but is not in the auto-select pool"
	case "pool_exhausted":
		return "every other pooled token is at its weekly limit or held by the reserve"
	case "no_alternative":
		return "there is no other usable token to switch to"
	case "account_bound":
		return "this Codex run stays on the account it was created with"
	case "not_auto":
		return "this run does not auto-select tokens"
	case "pinned":
		return "this run is pinned to " + w.CredentialLabel
	}
	return "it is not known whether this run can switch"
}

// PrimaryCommand is the first copyable command among the run-level fixes, or "" when there is none.
func (w *WeeklyLimitDTO) PrimaryCommand() string {
	if w == nil {
		return ""
	}
	for _, f := range w.Fix {
		if f.Command != "" {
			return f.Command
		}
	}
	return ""
}

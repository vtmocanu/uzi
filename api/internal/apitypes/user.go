package apitypes

import "time"

// UserDTO is the safe, JSON-serializable view of a user. It never exposes the
// password hash or token_version.
type UserDTO struct {
	ID          string  `json:"id"`
	Email       string  `json:"email"`
	DisplayName *string `json:"display_name"`
	IsAdmin     bool    `json:"is_admin"`
	IsActive    bool    `json:"is_active"`
	// AutopilotEnabled is the user's per-user opt-in to unattended autopilot runs
	// (PRD #19 M3, Decision 4). Default false; toggled from the user's Settings page.
	AutopilotEnabled bool `json:"autopilot_enabled"`
	// WaitOnLimit is the user's DEFAULT for the usage-limit park (PRD #35
	// Decision 7): whether a NEW run parks until their Anthropic window reopens
	// rather than failing. Default false.
	//
	// It is a default, not a policy — every run carries its own wait_on_limit,
	// stamped at creation, and flipping this leaves existing runs (parked or
	// otherwise) exactly as they are. A client rendering this as "runs will wait"
	// should say "new runs", or it will misdescribe the switch.
	WaitOnLimit bool `json:"wait_on_limit"`
	// NotifyEarlyLimitReset is the user's per-user opt-in to an alert when the poller
	// observes their Anthropic usage window has reset earlier than its previously
	// reported reset time (PRD #1020). Default true; toggled from their own Settings.
	NotifyEarlyLimitReset bool `json:"notify_early_limit_reset"`
	// JudgeEnabled is the user's per-user opt-in to run retrospectives (PRD #46
	// Decision 7). Default false; the user toggles their own from Settings, and an
	// admin can force-toggle any user's from the admin users surface.
	JudgeEnabled bool `json:"judge_enabled"`
	// CIAutofixEnabled is the user's per-user opt-in to automatic CI fixes (PRD
	// #71). Default false; the user toggles their own from Settings, and an admin
	// can force-toggle any user's from the admin users surface.
	CIAutofixEnabled bool `json:"ci_autofix_enabled"`
	// AttributionEnabled is the user's opt-out for AI attribution in worker commits
	// (issue #916). Default true (current behavior); when false the worker suppresses
	// the SDK's Co-Authored-By: Claude commit trailer.
	AttributionEnabled bool `json:"attribution_enabled"`
	// EphemeralWorkersEnabled is the user's per-user opt-in to run-bound throwaway
	// hosted worker auto-provisioning (PRD #529/#649). Default false; the user
	// toggles their own from the Workers page. Both this and the admin instance
	// kill-switch must be on before the provisioner acts.
	EphemeralWorkersEnabled bool `json:"ephemeral_workers_enabled"`
	// Which Anthropic credential this user's RETROSPECTIVES spend (PRD #104 M4, D1),
	// independent of what their runs spend. Both null ⇒ unbound ⇒ their default
	// token, which is every user's state until they choose otherwise. The label
	// rides so a client can render "judged by: console-key" without a second
	// lookup — a name, never the credential.
	JudgeAnthropicSecretID    *string    `json:"judge_anthropic_secret_id"`
	JudgeAnthropicSecretLabel *string    `json:"judge_anthropic_secret_label"`
	CreatedAt                 time.Time  `json:"created_at"`
	LastLogin                 *time.Time `json:"last_login"`
}

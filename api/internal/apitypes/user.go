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
	// JudgeEnabled is the user's per-user opt-in to run retrospectives (PRD #46
	// Decision 7). Default false; the user toggles their own from Settings, and an
	// admin can force-toggle any user's from the admin users surface.
	JudgeEnabled bool       `json:"judge_enabled"`
	CreatedAt    time.Time  `json:"created_at"`
	LastLogin    *time.Time `json:"last_login"`
}

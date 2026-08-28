package apitypes

// UserSettingsDTO mirrors the handler's own (unexported) userSettingsDTO for the
// CLI to decode GET /api/me/settings (envelope: {"settings": {...}}). It is a
// decoding-tolerant mirror — the handler owning its own type is intentional; the
// TUI only reads SidebarTokenIds today, the rest are carried for fidelity.
type UserSettingsDTO struct {
	DefaultModel *string `json:"default_model"`
	// DefaultEffort is the CLI decode mirror of the per-user default reasoning
	// effort (PRD #617). Fidelity only — carried so a decode never drops it; there
	// is no CLI setter (PRD #617 Decision 6).
	DefaultEffort *string `json:"default_effort"`
	JudgeModel    *string `json:"judge_model"`
	SummaryModel  *string `json:"summary_model"`
	Theme         *string `json:"theme"`
	// MrReworkEnabled is the CLI decode mirror of the per-user MR review-watcher
	// opt-in (PRD #700 M5). Fidelity only — carried so a decode never drops it; null
	// means unset = the default-ON state.
	MrReworkEnabled *bool    `json:"mr_rework_enabled"`
	SidebarTokenIds []string `json:"sidebar_token_ids"`
}

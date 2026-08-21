package apitypes

// UserSettingsDTO mirrors the handler's own (unexported) userSettingsDTO for the
// CLI to decode GET /api/me/settings (envelope: {"settings": {...}}). It is a
// decoding-tolerant mirror — the handler owning its own type is intentional; the
// TUI only reads SidebarTokenIds today, the rest are carried for fidelity.
type UserSettingsDTO struct {
	DefaultModel    *string  `json:"default_model"`
	JudgeModel      *string  `json:"judge_model"`
	SummaryModel    *string  `json:"summary_model"`
	Theme           *string  `json:"theme"`
	SidebarTokenIds []string `json:"sidebar_token_ids"`
}

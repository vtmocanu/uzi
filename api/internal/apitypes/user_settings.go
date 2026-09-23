package apitypes

// UserSettingsDTO mirrors the handler's own (unexported) userSettingsDTO for the
// CLI to decode GET /api/me/settings (envelope: {"settings": {...}}). It is a
// decoding-tolerant mirror — the handler owning its own type is intentional; the
// TUI only reads SidebarTokenIds today, the rest are carried for fidelity.
type UserSettingsDTO struct {
	// DefaultModel is DEPRECATED (PRD #1551 D2/D3): a legacy compatibility projection of the
	// effective harness's lane, kept one release. New clients read the two lanes below.
	DefaultModel *string `json:"default_model"`
	// DefaultEffort is the CLI decode mirror of the per-user default reasoning
	// effort (PRD #617). Fidelity only — carried so a decode never drops it; there
	// is no CLI setter (PRD #617 Decision 6).
	DefaultEffort *string `json:"default_effort"`
	JudgeModel    *string `json:"judge_model"`
	SummaryModel  *string `json:"summary_model"`
	// Theme is the DEPRECATED legacy single-theme override mirror (PRD #21); kept
	// one release. The appearance override lives in the four fields below (PRD #1167).
	Theme *string `json:"theme"`
	// MrReworkEnabled is the CLI decode mirror of the per-user MR review-watcher
	// opt-in (PRD #700 M5). Fidelity only — carried so a decode never drops it; null
	// means unset = the default-ON state.
	MrReworkEnabled *bool    `json:"mr_rework_enabled"`
	SidebarTokenIds []string `json:"sidebar_token_ids"`
	// SidebarCodexAccountIds is the CLI decode mirror of the linked Codex accounts the
	// user surfaced on the sidebar rail (PRD #1209 M1), the codex sibling of
	// SidebarTokenIds. Fidelity only — carried so a decode never drops it.
	SidebarCodexAccountIds []string `json:"sidebar_codex_account_ids"`
	// The four appearance override mirrors (PRD #1167): the user's RAW per-field
	// overrides (each null ⇒ inherit the instance default). Fidelity only — carried
	// so a decode never drops them; there is no CLI setter.
	AppearanceMode *string `json:"appearance_mode"`
	LightTheme     *string `json:"light_theme"`
	DarkTheme      *string `json:"dark_theme"`
	Typeface       *string `json:"typeface"`
	// DefaultHarness is the CLI decode mirror of the per-user default harness (PRD #1429
	// M1 / D3); null = "no preference". Fidelity only — there is deliberately no CLI setter
	// for the user default (D3), so this is carried so a decode never drops it.
	DefaultHarness *string `json:"default_harness"`
	// DefaultClaudeModel and DefaultCodexModel are the CLI decode mirrors of the retained
	// per-harness worker-model lanes (PRD #1551 M1 / D2); each null = inherit. Fidelity only.
	DefaultClaudeModel *string `json:"default_claude_model"`
	DefaultCodexModel  *string `json:"default_codex_model"`
}

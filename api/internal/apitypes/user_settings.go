package apitypes

// CrossCheckPinDTO keeps stored pins separate from resolved family defaults.
// ResolvedModel is null when the Claude cell inherits the SDK model.
type CrossCheckPinDTO struct {
	Stage   string  `json:"stage"`
	Harness string  `json:"harness"`
	Model   *string `json:"model"`
	Effort  *string `json:"effort"`
	// WorkerDefaultModel resolves this cell without checker pins from the same read snapshot.
	// Null delegates model selection to the Claude SDK/account.
	WorkerDefaultModel *string `json:"worker_default_model"`
	ResolvedModel      *string `json:"resolved_model"`
	ResolvedEffort     string  `json:"resolved_effort"`
	ModelSource        string  `json:"model_source"`
	EffortSource       string  `json:"effort_source"`
	Active             bool    `json:"active"`
}

// UserSettingsDTO mirrors the handler's own (unexported) userSettingsDTO for the
// CLI to decode GET /api/me/settings (envelope: {"settings": {...}}). It is a
// decoding-tolerant mirror — the handler owning its own type is intentional; the
// TUI only reads SidebarTokenIds today, the rest are carried for fidelity.
type UserSettingsDTO struct {
	// Browser re-unlock permission is decoded for fidelity; the CLI never holds a slot.
	VaultAutoReunlock bool               `json:"vault_auto_reunlock"`
	CrossCheckPins    []CrossCheckPinDTO `json:"cross_check_pins"`
	// DefaultModel is DEPRECATED (PRD #1551 D2/D3): a legacy compatibility projection of the
	// effective harness's lane, kept one release. New clients read the two lanes below.
	DefaultModel *string `json:"default_model"`
	// DefaultEffort is the CLI decode mirror of the per-user default reasoning
	// effort (PRD #617). Fidelity only — carried so a decode never drops it; there
	// is no CLI setter (PRD #617 Decision 6).
	DefaultEffort *string `json:"default_effort"`
	// DefaultCodexEffort is the independently retained Codex effort; null inherits medium.
	DefaultCodexEffort *string `json:"default_codex_effort"`
	JudgeModel         *string `json:"judge_model"`
	// CodexJudgeModel is the Codex judge lane mirror (PRD #3245): null inherits the
	// instance Codex judge model. JudgeModels is the read-only resolved view of both
	// judge lanes, ordered claude then codex.
	CodexJudgeModel *string `json:"codex_judge_model"`
	SummaryModel    *string `json:"summary_model"`
	// CodexSummaryModel is the Codex summary lane mirror (PRD #3248): null inherits the
	// instance Codex summary model. SummaryModels is the read-only resolved view of both
	// summary lanes, ordered claude then codex.
	CodexSummaryModel *string `json:"codex_summary_model"`
	// TaskReviewModel and CodexTaskReviewModel mirror the owner's per-harness model for the
	// handoff review pass (PRD #1570); null = the built-in review model. TaskReviewModels is the
	// read-only resolved view, ordered claude then codex.
	TaskReviewModel      *string               `json:"task_review_model"`
	CodexTaskReviewModel *string               `json:"codex_task_review_model"`
	TaskReviewModels     []ModelSettingCellDTO `json:"task_review_models"`
	// TaskReviewEffort and CodexTaskReviewEffort mirror the owner's per-harness effort for the review
	// pass (PRD #1570): a level or `runtime`; null = inherit the worker effort. TaskReviewEfforts is
	// the read-only resolved view, ordered claude then codex.
	TaskReviewEffort      *string                `json:"task_review_effort"`
	CodexTaskReviewEffort *string                `json:"codex_task_review_effort"`
	TaskReviewEfforts     []EffortSettingCellDTO `json:"task_review_efforts"`
	// Theme is the DEPRECATED legacy single-theme override mirror (PRD #21); kept
	// one release. The appearance override lives in the four fields below (PRD #1167).
	Theme *string `json:"theme"`
	// MrReworkEnabled is the CLI decode mirror of the per-user MR review-watcher
	// opt-in (PRD #700 M5). Fidelity only — carried so a decode never drops it; null
	// means unset = the default-ON state.
	MrReworkEnabled *bool `json:"mr_rework_enabled"`
	// NowSummaryEnabled is the CLI decode mirror of the per-user model-written "Now" summary
	// switch (PRD #2603); null means unset = the default-ON state, an explicit false is the
	// opt-out. The effective value is this AND the instance switch.
	NowSummaryEnabled *bool    `json:"now_summary_enabled"`
	SidebarTokenIds   []string `json:"sidebar_token_ids"`
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
	// SidebarMotion and EffectiveSidebarMotion mirror the animated-sidebar-icons choice
	// (issue #4510): the raw choice (null = not chosen) and the resolved flag. Fidelity
	// only; there is no CLI setter.
	SidebarMotion          *bool `json:"sidebar_motion"`
	EffectiveSidebarMotion bool  `json:"effective_sidebar_motion"`
	// DefaultHarness is the CLI decode mirror of the per-user default harness (PRD #1429
	// M1 / D3); null = "Default · automatic" (#3845: chosen per run by weekly limits when both
	// harnesses are usable). `uzi settings get` displays it; there is deliberately no CLI
	// setter for the user default (D3).
	DefaultHarness *string `json:"default_harness"`
	// JudgeRunsOn is the CLI decode mirror of the per-user "Judge runs on" harness (PRD
	// #3257); null = same as the run. `uzi settings get` displays it; writes are
	// cookie-only until #3264.
	JudgeRunsOn *string `json:"judge_runs_on"`
	// ClaudeProviderID is the CLI decode mirror of the provider the owner's Claude lane runs on
	// (PRD #4320); null = the vendor Claude ladder. Read-only: set via the provider source route.
	ClaudeProviderID *string `json:"claude_provider_id"`
	// DefaultClaudeModel and DefaultCodexModel are the CLI decode mirrors of the retained
	// per-harness worker-model lanes (PRD #1551 M1 / D2); each null = inherit. Fidelity only.
	DefaultClaudeModel *string `json:"default_claude_model"`
	DefaultCodexModel  *string `json:"default_codex_model"`
	// JudgeModels is read-only (never sent on a PUT): one cell per harness, claude then codex.
	JudgeModels []ModelSettingCellDTO `json:"judge_models"`
	// SummaryModels is read-only (never sent on a PUT): one cell per harness, claude then codex.
	SummaryModels []ModelSettingCellDTO `json:"summary_models"`
	// EffortSettings is read-only (never sent on a PUT): the resolved ordinary effort
	// preference per harness (PRD #3247), claude then codex.
	EffortSettings []EffortSettingCellDTO `json:"effort_settings"`
	// PlanningClaudeModel and PlanningClaudeEffort are the owner's Claude planning pair (PRD
	// #3252): used only while a Claude lead-lane run plans; null = "Same as the run". Writable on
	// PUT /api/me/settings.
	PlanningClaudeModel  *string `json:"planning_claude_model"`
	PlanningClaudeEffort *string `json:"planning_claude_effort"`
	PlanningCodexModel   *string `json:"planning_codex_model"`
	PlanningCodexEffort  *string `json:"planning_codex_effort"`
	// PlanningModels and PlanningEfforts are read-only (never sent on a PUT): the resolved view of
	// the planning pairs, ordered claude then codex.
	PlanningModels  []ModelSettingCellDTO  `json:"planning_models"`
	PlanningEfforts []EffortSettingCellDTO `json:"planning_efforts"`
	// WeeklyReservePct and WeeklyReservePauseRunning mirror the weekly usage reserve (PRD
	// #3522): the 7-day utilization percent (1..100) at which uzi stops spending the user's
	// subscription (null = off), and whether a running run is also paused. They are NOT
	// written through PUT /api/me/settings; PUT /api/me/usage-reserve owns the write.
	WeeklyReservePct          *int `json:"weekly_reserve_pct"`
	WeeklyReservePauseRunning bool `json:"weekly_reserve_pause_running"`
	// WeeklyWarningMarginPts is the weekly-limit early-warning margin in percentage points (PRD
	// #4123), 0..50 with 0 = off, default 15. Not written through PUT /api/me/settings;
	// PUT /api/me/weekly-warning-margin owns the write.
	WeeklyWarningMarginPts int `json:"weekly_warning_margin_pts"`
	// CodexAccountMode is the user's Codex account mode (issue #3719): "default" binds the
	// default alias, "auto" picks among the aliases the user pooled. Read-only here:
	// PUT /api/me/codex-account-mode owns the write.
	CodexAccountMode string `json:"codex_account_mode"`
	// Read-only here; PUT /api/me/harness-overflow owns the write.
	HarnessOverflowEnabled bool `json:"harness_overflow_enabled"`
}

package settings

// keys.go holds the settings schema: the key and default consts, Defaults,
// SecretKeys, IsSecret and Known. Split from settings.go for PRD #1021.

import (
	"github.com/vtmocanu/uzi/api/internal/theme"
)

// Setting keys. These are the only keys the API recognizes; writes to any other
// key are rejected (an admin cannot invent settings the code does not read).
const (
	KeyAutopilotLabel = "autopilot_label"
	// KeyUziLabel is the single run-eligibility gate (PRD #764): an issue is
	// uzi's to run iff it carries this label. Configurable, defaulting to "uzi".
	// It follows the no-seeded-row pattern (like the judge/health keys):
	// an absent row synthesizes from Defaults, so no migration seeds it.
	KeyUziLabel = "uzi_label"
	// KeyDefaultTheme is the instance-default UI theme (PRD #21). It tenants into
	// this same table rather than a parallel settings store; its value is a theme
	// id validated against the canonical theme registry, not a label.
	KeyDefaultTheme = "default_theme"
	// Slack integration keys (PRD #25). slack_enabled/public_base_url are plaintext
	// non-secret settings (in Defaults). slack_bot_token/slack_app_token are SECRET
	// keys (in SecretKeys, NOT Defaults): sealed with secretbox+base64 at rest and
	// structurally excluded from every value-producing read — see SecretKeys.
	KeySlackEnabled  = "slack_enabled"
	KeyPublicBaseURL = "public_base_url"
	KeySlackBotToken = "slack_bot_token" //nolint:gosec // G101: DB setting-key name, not a credential value
	KeySlackAppToken = "slack_app_token" //nolint:gosec // G101: DB setting-key name, not a credential value
	// Run-judge keys (PRD #46 Decision 7). judge_enabled is the global kill-switch
	// (text "true"/"false"); judge_model is the model the judge runs on (opus by
	// default since PRD #69; a model alias, validated with the PRD #17 rules).
	// Both are admin-writable and carry compiled-in defaults.
	KeyJudgeEnabled = "judge_enabled"
	KeyJudgeModel   = "judge_model"
	// KeyJudgeEnforceAll (PRD #69) is an admin bool ("true"/"false"). When true it
	// bypasses the per-user judge_enabled opt-in gate so every run is judged; the
	// kill-switch (KeyJudgeEnabled) and token presence still govern, so a disabled
	// judge or a token-less user is never overridden.
	KeyJudgeEnforceAll = "judge_enforce_all"
	// Per-user judge spend guards (PRD #69 M5, Decision 9). Two admin-tuned,
	// count-based, best-effort anti-runaway backstops checked at enqueue in EVERY
	// mode. judge_cooldown_seconds is an integer in {0} ∪ [60, 86400] (0 disables the
	// cooldown); judge_daily_budget is a non-negative count (0 = unlimited/off).
	// Runtime-tunable from the Admin Settings page; no env var.
	KeyJudgeCooldownSeconds = "judge_cooldown_seconds"
	KeyJudgeDailyBudget     = "judge_daily_budget"
	// Run-summary model key (PRD #362 Decision 8). summary_model is the model the
	// inline plain-English run-summary generator runs on (haiku by default — summaries
	// are lighter and per-run, so the cheap/fast default is right). It mirrors
	// judge_model's settings machinery (admin default + optional per-user override,
	// validated with the PRD #17 model rules) but rides the ISSUE-RUN claim, not the
	// judge claim. Admin-writable with a compiled-in default.
	KeySummaryModel = "summary_model"
	// GitHub Projects v2 Status sync (PRD #364). Instance-wide kill-switch
	// (text "true"/"false"): OFF until an admin enables it, so the whole sync
	// feature is a strict no-op on a fresh instance. Admin-writable with a
	// compiled-in default; no seeded row (an absent row synthesizes to the default).
	KeyGithubProjectSyncEnabled = "github_project_sync_enabled"
	// Ephemeral worker auto-provisioning (PRD #529 M2). Instance-wide kill-switch
	// (text "true"/"false"): OFF until an admin enables it, so the whole
	// auto-provisioner is a strict no-op on a fresh instance. This is the INSTANCE
	// gate; the per-user opt-in lives in users.ephemeral_workers_enabled, and the
	// provisioner fires for a run only when BOTH are true. Admin-writable with a
	// compiled-in default; no seeded row (an absent row synthesizes to the default).
	KeyEphemeralWorkersEnabled = "ephemeral_workers_enabled"
	// Run-health detector keys (PRD #47). health_enabled is a bool ("true"/"false");
	// the rest are integer seconds validated as {0} ∪ [60, 86400] — 0 disables that
	// one signal, and the upper bound stops a fat-fingered value silently disabling
	// it. No new env vars: these are runtime-tunable from the Admin Settings page.
	KeyHealthEnabled = "health_enabled"
	// Capability-aware scheduling kill-switch (PRD #84 Decision 13). A bool
	// ("true"/"false"), default TRUE. It gates ONLY #84's added behavior — the
	// capability-match clause on the claim predicate (M2), and downstream the plan-gate
	// block (M4) and "no eligible worker" reason (M3). It does NOT gate the shipped
	// docker repo allowlist / fn_worker_can_claim authorization clause (#83/#89), which
	// stays enforced regardless: the extended function reads the new clause as
	// `NOT capability_aware OR (required ⊆ caps)`, so "off" neutralizes only the added
	// subset clause. OFF is an explicit, documented degraded mode (best-effort claiming;
	// a docker-needing run may be claimed by a non-docker worker and fail mid-run).
	KeyCapabilityAwareScheduling  = "capability_aware_scheduling"
	KeyHealthStallSeconds         = "health_stall_seconds"
	KeyHealthSlowSeconds          = "health_slow_seconds"
	KeyHealthQueuedSeconds        = "health_queued_seconds"
	KeyHealthApprovalSeconds      = "health_approval_seconds"
	KeyHealthNudgeCooldownSeconds = "health_nudge_cooldown_seconds"
	// Hosted-worker per-user quota (PRD #58 Decision 8): the single knob bounding
	// self-service provisioning. An integer in {0} ∪ [1, maxHostedWorkerQuota],
	// where 0 disables self-service entirely (the API then 403s a provision).
	// Runtime-tunable from the Admin Settings page; no env var.
	KeyHostedWorkerQuota = "hosted_worker_quota"
	// Docker-worker repo allowlist (PRD #89 M-allow): the set of repos a
	// docker-enabled worker is permitted to CLAIM runs for. Stored as a
	// comma-separated list of repo UUIDs. This is the accepted-risk likelihood
	// control for the non-rootless DinD tier — the trigger is repo content, so the
	// gate binds at claim, and an empty value is FAIL-CLOSED (a docker worker then
	// claims no repo-bearing run). Non-docker workers never consult it.
	KeyDockerRepoAllowlist = "docker_repo_allowlist"
	// KeyFindingLabel is the server-mandated marker label attached to every forge
	// issue filed from an incidental finding (PRD #333 D5). Config-overridable, it is
	// EnsureLabels-ed to exist before the file write (Forgejo resolves label ids and
	// errors on an unknown name) and unioned into the filed label set server-side, so a
	// client can never supply a trigger label that bypasses it. A plain single label, it
	// takes the Decision-8 label rules (Validate's default branch).
	KeyFindingLabel = "finding_label"
	// Agent-source repo sync keys (PRD #602 M2). The admin points uzi at a git
	// repo of `.md` role files to layer over the embedded builtins. All four are
	// admin-writable with compiled-in defaults; the whole feature is OFF and
	// unconfigured on a fresh instance (enabled=false, url=""), so a fresh install
	// stays offline/hermetic. No canonical product-agents repo is pre-filled — the
	// repo does not exist yet (ADR-0602 corrects the PRD's Decision 2 draft).
	//   - agent_source_repo_url: the https clone URL; empty = unconfigured. When
	//     non-empty it is ADDITIONALLY checked against the SEPARATE SSRF allowlist
	//     AGENT_SOURCE_ALLOWED_BASE_URLS in the handler (settings.Validate is a pure
	//     (key,value) function and cannot see config; importing config is a cycle).
	//   - agent_source_ref: the pinned tag/SHA (recommended) or floating branch.
	//   - agent_source_enabled: strict "true"/"false" kill-switch (default off).
	//   - agent_source_interval: reconcile cadence, a Go duration with a 1m floor.
	// The engine-managed agent_source_last_sync_* keys are deliberately absent from
	// Defaults (selfimprove precedent) until M3/M4 write them.
	KeyAgentSourceRepoURL  = "agent_source_repo_url"
	KeyAgentSourceRef      = "agent_source_ref"
	KeyAgentSourceEnabled  = "agent_source_enabled"
	KeyAgentSourceInterval = "agent_source_interval"
	// KeyAgentSourceFolder is the repo-relative subfolder the reconcile reads role
	// files from (PRD #702 M1), selecting a subtree of the already-cloned,
	// already-allowlisted source repo — SSRF-neutral, no new egress, no migration.
	// Empty/unset resolves to DefaultAgentSourceFolder (".claude/agents") at read
	// time, so existing installs are unchanged. Admin-writable via Defaults.
	KeyAgentSourceFolder = "agent_source_folder"
	// KeyAgentSourceCredential is the SEALED private-repo clone token (PRD #602 M2):
	// a SecretKeys member, sealed with secretbox+base64 via ValueForStorage, kept
	// OUT of Defaults so it never leaks through a value read, masked in GET. Distinct
	// from the forge push PAT; read-only and clone-scoped — it can never push. Its
	// decrypt accessor lands in M3 (clone auth), so none is added here (an unused
	// accessor would trip deadcode).
	KeyAgentSourceCredential = "agent_source_credential" //nolint:gosec // G101: DB setting-key name, not a credential value
	// Engine-managed agent-source last-sync status keys (PRD #602 M3). Like the
	// selfimprove_* engine state, these are deliberately absent from Defaults so
	// Known() rejects a generic-PUT write; the reconcile job sets them via
	// UpsertAppSetting (then Cache.Invalidate() so the next read is fresh). M4 reads
	// them for the admin sync-status panel. The error string is PAT-scrubbed before
	// it is stored — see agentsource.Reconcile.
	KeyAgentSourceLastSyncAt     = "agent_source_last_sync_at"     // RFC3339 timestamp of the last reconcile attempt
	KeyAgentSourceLastSyncSHA    = "agent_source_last_sync_sha"    // commit SHA staged on the last ok sync
	KeyAgentSourceLastSyncStatus = "agent_source_last_sync_status" // "ok" | "error"
	KeyAgentSourceLastSyncError  = "agent_source_last_sync_error"  // PAT-scrubbed error message (empty on ok)
	KeyAgentSourceLastSyncCounts = "agent_source_last_sync_counts" // small JSON: {"staged":N,"changed":N,"failed":N}
	// Engine-managed applied-tracking keys (PRD #602 M4). These record the LAST
	// APPLIED snapshot rather than the last staged one: Apply writes them after it
	// commits the provenance-aware upsert, and it is how "pending" is decided — a
	// staged snapshot is pending exactly when it exists AND its fetched_sha differs
	// from KeyAgentSourceLastAppliedSHA. Kept out of Defaults (selfimprove pattern)
	// so a generic PUT can never write them; only agentsource.Apply does.
	KeyAgentSourceLastAppliedAt  = "agent_source_last_applied_at"  // RFC3339 timestamp of the last successful apply
	KeyAgentSourceLastAppliedSHA = "agent_source_last_applied_sha" // fetched SHA of the snapshot last applied to agent_templates
	// Engine-managed remote-fact keys (PRD #702 M4). The admin update-check
	// ls-remotes the CONFIGURED source and persists ONLY these remote facts — never a
	// boolean. "Update available" is DERIVED at read time from these plus the live
	// config (current ref / last-applied SHA), so a pin bump or an apply self-clears
	// the badge with no new egress (Decision 6). Kept out of Defaults (selfimprove /
	// engine pattern, like the last-applied keys) so a generic PUT can never write
	// them; only agentsource.CheckForUpdate does.
	KeyAgentSourceLatestRef       = "agent_source_latest_ref"        // newest IsValid semver tag advertised by the source ("" if none)
	KeyAgentSourceRemoteTipSHA    = "agent_source_remote_tip_sha"    // advertised tip SHA of the configured ref (or HEAD)
	KeyAgentSourceUpdateCheckedAt = "agent_source_update_checked_at" // RFC3339 timestamp of the last update check
	// Upstream-release-check settings (PRD #836 M1). A server-side periodic check
	// against the GitHub Releases API for vtmocanu/uzi persists the remote facts, and
	// "update available" / "far behind" / "security" are DERIVED at read time with
	// zero egress — the SAME poll→persist→derive core as the agent-source keys above,
	// a different target. Two independent CONFIG toggles plus an interval (in
	// Defaults); the optional token is the only secret; the six remote-fact keys are
	// ENGINE-written (absent from Defaults, never secret — the release-check Runner
	// sets them via UpsertAppSetting, then Cache.Invalidate()).
	KeyReleaseCheckEnabled       = "release_check_enabled"        // master gate: off → the api never calls github.com
	KeyReleaseCheckBannerEnabled = "release_check_banner_enabled" // governs only the escalation banner (cosmetic)
	KeyReleaseCheckInterval      = "release_check_interval"       // poll cadence, a Go duration with a 1m floor
	// KeyReleaseCheckToken is the OPTIONAL GitHub token (raises the unauth 60 req/hr
	// ceiling to 5,000). A SecretKeys member, sealed like the Slack tokens and kept
	// OUT of Defaults so it never leaks through a value read.
	KeyReleaseCheckToken = "release_check_token"
	// Engine-managed remote-fact keys (PRD #836 M1). Persisted by the release-check
	// Runner from the releases/latest payload; kept out of Defaults (engine pattern,
	// like the agent-source remote-fact keys) so a generic PUT can never write them.
	// "Update available"/"far behind"/"security" are DERIVED from these plus the
	// running version, never stored — see releasecheck.UpdateAvailable / FarBehind /
	// Security.
	KeyReleaseLatestTag   = "release_latest_tag"   // v-prefixed tag_name of the latest upstream release
	KeyReleaseLatestName  = "release_latest_name"  // release name
	KeyReleaseLatestBody  = "release_latest_body"  // markdown release notes (the ### Security scan + notes excerpt)
	KeyReleaseNotesURL    = "release_notes_url"    // html_url of the latest release
	KeyReleasePublishedAt = "release_published_at" // RFC3339 publish timestamp of the latest release
	KeyReleaseCheckedAt   = "release_checked_at"   // RFC3339 timestamp of the last check
	// KeyReleaseBannerSnoozeTag records the release tag an admin snoozed the escalation
	// banner for (PRD #836 M6). Engine/admin-written via the snooze endpoint's
	// UpsertAppSetting — kept OUT of Defaults (like the six remote-fact keys) and never
	// secret. "banner_snoozed" is DERIVED at read time: true iff this equals the current
	// latest_tag, so a newer upstream release changes latest_tag and the snooze
	// auto-expires with no admin action.
	KeyReleaseBannerSnoozeTag = "release_banner_snooze_tag"
	// Instance branding keys (PRD #685 M1). All NON-SECRET public config → they live
	// in Defaults (never SecretKeys), round-trip through PUT /admin/settings, and are
	// served — allowlisted, never via All/AdminView — through the public GET
	// /api/branding. Logo BYTES are NOT keys: they live in the branding_assets table
	// (Decision D7), so nothing here carries a blob and the settings cache stays small.
	KeyAppLogoMode     = "app_logo_mode"      // "default" | "custom" | "preset"
	KeyAppLogoPreset   = "app_logo_preset"    // web-catalog slug; "" = none
	KeyAppLogoKeepName = "app_logo_keep_name" // "true" | "false"
	KeyBrandMode       = "brand_mode"         // "none" | "text" | "logo"
	KeyBrandCompany    = "brand_company"      // ≤64-rune company text (may be ""), rendered to every principal incl. signed-out
	KeyBrandPlacement  = "brand_placement"    // "below" | "topright"
	KeyBrandPlaque     = "brand_plaque"       // "true" | "false"
	// MR review-watcher admin gates (PRD #700 M5, Decision 5). mr_rework_enabled is
	// the global kill-switch (text "true"/"false"); mr_rework_cap is the admin cap on
	// rework cycles per MR (a positive integer, mirroring ci-autofix's maxAttempts).
	// BOTH default ON/5 — this feature ships enabled, the opposite of the judge's
	// default-off (Decision 5) — so the enabled read is a THREE-STATE read
	// (present-true / present-false / absent → the default ON) that PROPAGATES a store
	// read error rather than collapsing it into the value: the caller (the M3
	// detector) maps that error to OFF (fail closed). Admin-writable with compiled-in
	// defaults; no seeded row (an absent row synthesizes to the default). The per-user
	// opt-in lives on users.mr_rework_enabled, not here.
	KeyMrReworkEnabled = "mr_rework_enabled"
	KeyMrReworkCap     = "mr_rework_cap"
	// CI-autofix admin gate (PRD #914). ci_autofix_enabled is the instance-wide admin
	// kill-switch for CI autofix (text "true"/"false"): a THREE-STATE, error-propagating
	// read (present-true / present-false / absent → the default) that ships ON — a
	// settings blip must not silently disable a default-on feature. The per-user opt-in
	// lives on users.ci_autofix_enabled, not here.
	KeyCiAutofixEnabled = "ci_autofix_enabled"
)

// Compiled-in defaults, used when a row is absent so a fresh or partially
// migrated DB still yields a working label set. They mirror the values the
// migration seeds and the hardcoded constants the pre-PRD-19 code used.
const (
	DefaultAutopilotLabel = "autopilot"
	// PRD #764: the single run-eligibility label defaults to "uzi". No-seeded-row
	// pattern — an absent row synthesizes to this default and no migration adds it.
	DefaultUziLabel = "uzi"
	// PRD #25. Slack is off until an admin (or ENV) configures it, so the whole
	// integration is a strict no-op on a fresh instance. The default deep-link base
	// only resolves for the laptop's own user; a Tailscale/LAN URL overrides it.
	DefaultSlackEnabled  = "false"
	DefaultPublicBaseURL = "http://127.0.0.1:8080"
	// PRD #46. The judge is OFF until an admin enables it (it spends user tokens), so
	// the whole feature is a strict no-op on a fresh instance. The default model is
	// opus (PRD #69 Decision 1): the recommendation half feeds self-improvement, so
	// the strongest model is wanted by default. The per-user override (M2) is the
	// cost lever down to haiku/sonnet; opus is ~5–15× haiku per run, which is why
	// M5's spend guards and the per-user override exist.
	DefaultJudgeEnabled = "false"
	DefaultJudgeModel   = "opus"
	// PRD #364. The GitHub Projects v2 Status sync is OFF until an admin enables it
	// (it writes to a user's project board), so the feature is a strict no-op on a
	// fresh instance and on upgrade.
	DefaultGithubProjectSyncEnabled = "false"
	// PRD #529 M2. Ephemeral worker auto-provisioning is OFF until an admin enables it
	// (it spins real cluster capacity on demand), so the feature is a strict no-op on a
	// fresh instance and on upgrade. Both this instance kill-switch and the per-user
	// opt-in (users.ephemeral_workers_enabled) must be true before anything provisions.
	DefaultEphemeralWorkersEnabled = "false"
	// PRD #69. Judge enforcement is OFF by default: the per-user opt-in gate stands
	// until an admin flips this on, so the feature is a strict no-op on upgrade.
	DefaultJudgeEnforceAll = "false"
	// PRD #69 M5 Decision 9. The cooldown is ON by default (60s: skip a judge for a
	// user who had one enqueued within the last minute), a cheap runaway-loop backstop
	// even for opted-in users. The daily budget is OFF by default ("0" = unlimited): a
	// count cap is opt-in because the generous cooldown already catches the loop case.
	DefaultJudgeCooldownSeconds = "60"
	DefaultJudgeDailyBudget     = "0"
	// PRD #362 Decision 8. The run-summary generator defaults to haiku: summaries are
	// lighter and produced per-run (1 intent + 1 plan + one per revise round), so the
	// fast, near-free model is the right default — unlike the judge, which defaults to
	// the strong opus because its recommendations feed self-improvement.
	DefaultSummaryModel = "haiku"
	// PRD #47 run-health defaults (Decision 5). On by default; a fresh instance with
	// no seeded rows detects health out of the box. The thresholds mirror the table
	// in the PRD's Solution Overview.
	DefaultHealthEnabled = "true"
	// PRD #84 Decision 13: capability-aware scheduling is ON by default. Safe and cheap
	// because on a homogeneous fleet the capability match is a no-op (every worker
	// satisfies every run); it only helps heterogeneous fleets and acts as an escape
	// hatch if inference false-positives start blocking runs.
	DefaultCapabilityAwareScheduling  = "true"
	DefaultHealthStallSeconds         = "300"  // 5m of silence (no tool in flight)
	DefaultHealthSlowSeconds          = "2700" // 45m wall clock, clamped < RUN_TIMEOUT at read time
	DefaultHealthQueuedSeconds        = "600"  // 10m stuck queued
	DefaultHealthApprovalSeconds      = "3600" // 1h idle awaiting approval
	DefaultHealthNudgeCooldownSeconds = "1800" // 30m between Slack nudges per run
	// PRD #58 Decision 8: two hosted workers per user by default. Hosting is itself
	// off unless WORKER_HOSTING_ENABLED is set (the compose default), so this value
	// only ever applies on a k8s deployment that deliberately turned hosting on —
	// which is why a permissive-ish default is safe here and the flag, not this
	// number, is the real "is this feature on" switch.
	DefaultHostedWorkerQuota = "2"
	// PRD #89 M-allow: the docker repo allowlist is EMPTY by default, which the claim
	// gate reads as fail-closed — a docker worker claims no repo-bearing run until an
	// admin lists the trusted repos. Empty is the safe default precisely because this
	// is a security control: an unconfigured instance never lets a docker worker pick
	// up an unvetted repo's run.
	DefaultDockerRepoAllowlist = ""
	// PRD #333 D5: the incidental-finding marker defaults to "agent-found", using
	// the no-seeded-row pattern (an absent row synthesizes to this default).
	DefaultFindingLabel = "agent-found"
	// PRD #602 M2 agent-source repo sync. OFF and unconfigured on a fresh instance:
	// the URL is EMPTY (no canonical product-agents repo is pre-filled — ADR-0602
	// corrects the PRD's Decision 2 draft) and enabled is false, so a fresh install
	// stays offline/hermetic. The ref default is empty too (unused while the URL is
	// empty). The reconcile interval defaults to 1h.
	DefaultAgentSourceRepoURL  = ""
	DefaultAgentSourceRef      = ""
	DefaultAgentSourceEnabled  = "false"
	DefaultAgentSourceInterval = "1h"
	// PRD #702 M1: the source folder defaults to the historical hardcoded
	// sourceAgentsDir (".claude/agents"), so an install with no folder set reads
	// role files from exactly the same subtree as before.
	DefaultAgentSourceFolder = ".claude/agents"
	// PRD #836 M1 upstream-release-check. The check ships ON — both the master gate
	// and the banner — unlike agent-source's default-off: it makes a single
	// server-side poll to a compile-time-constant public URL and surfacing that a
	// newer release exists is the whole point of the feature. The cadence defaults to
	// 6h (trivially within GitHub's 60 req/hr unauthenticated budget).
	DefaultReleaseCheckEnabled       = "true"
	DefaultReleaseCheckBannerEnabled = "true"
	DefaultReleaseCheckInterval      = "6h"
	// PRD #685 M1 instance-branding defaults. A fresh install is UNBRANDED (Decision
	// D4): the app mark stays the uzi FactoryIcon + literals (app_logo_mode=default,
	// keep-name on) and there is no POWERED BY brand (brand_mode=none). brand_company
	// is empty and the plaque is off. Same no-seeded-row pattern as the judge keys —
	// an absent row synthesizes to these defaults, so no migration seeds them.
	DefaultAppLogoMode     = "default"
	DefaultAppLogoPreset   = ""
	DefaultAppLogoKeepName = "true"
	DefaultBrandMode       = "none"
	DefaultBrandCompany    = ""
	DefaultBrandPlacement  = "below"
	DefaultBrandPlaque     = "false"
	// PRD #700 M5 Decision 5. The MR review watcher ships ON: an admin global
	// kill-switch (default true — the OPPOSITE of the judge's default-off, an
	// announced behavior change) and a per-MR rework-cycle cap (default 5, mirroring
	// ci-autofix's maxAttempts). Both are the admin-side gates; the per-user opt-in
	// (also default-on) lives on users.mr_rework_enabled.
	DefaultMrReworkEnabled = "true"
	DefaultMrReworkCap     = "5"
	// PRD #914. CI autofix ships ON: an admin global kill-switch (default true), the
	// admin-side gate. The per-user opt-in (users.ci_autofix_enabled) is separate.
	DefaultCiAutofixEnabled = "true"
)

// Defaults maps every known key to its compiled-in default. This is the single
// Go source of the default values: the accessors fall back to it and the
// migration (00036_app_settings) seeds the same literals. Keep the two in sync —
// SQL cannot reference these constants, so a change here that should also change
// the seeded rows needs a follow-up migration. Many keys have NO seeded row
// (Cache.All/Effective synthesize them from these defaults), so an absent row is
// expected and no migration adds them.
// Ranging over Defaults is the canonical way to enumerate the settings the API
// understands.
var Defaults = map[string]string{
	KeyAutopilotLabel: DefaultAutopilotLabel,
	// PRD #764 single run-eligibility label. No seeded row: an absent row
	// synthesizes to DefaultUziLabel ("uzi"), so All/AdminView surface it on every
	// instance and no migration adds it.
	KeyUziLabel: DefaultUziLabel,
	// The instance default theme falls back to the registry's Default ("ember"),
	// so an instance with no seeded row renders exactly as before (PRD #21). No
	// migration seed is needed — this fallback plus the stable GET shape follow
	// automatically from the entry here.
	KeyDefaultTheme: theme.Default,
	// PRD #25 Slack non-secret keys. They have NO seeded row: an absent row
	// synthesizes to these defaults, and no migration adds them.
	KeySlackEnabled:  DefaultSlackEnabled,
	KeyPublicBaseURL: DefaultPublicBaseURL,
	// PRD #46 admin-writable keys. No seeded row (an absent row synthesizes to these
	// defaults).
	KeyJudgeEnabled:         DefaultJudgeEnabled,
	KeyJudgeModel:           DefaultJudgeModel,
	KeyJudgeEnforceAll:      DefaultJudgeEnforceAll,
	KeyJudgeCooldownSeconds: DefaultJudgeCooldownSeconds,
	KeyJudgeDailyBudget:     DefaultJudgeDailyBudget,
	// PRD #364 GitHub Projects v2 Status sync kill-switch. Same no-seeded-row
	// pattern as the judge keys: an absent row synthesizes to the default (off).
	KeyGithubProjectSyncEnabled: DefaultGithubProjectSyncEnabled,
	// PRD #529 M2 ephemeral worker auto-provisioning kill-switch. Same no-seeded-row
	// pattern as the judge keys: an absent row synthesizes to the default (off).
	KeyEphemeralWorkersEnabled: DefaultEphemeralWorkersEnabled,
	// PRD #362 Decision 8 run-summary model. Same no-seeded-row pattern as the judge
	// keys: an absent row synthesizes to DefaultSummaryModel ("haiku"), so All/AdminView
	// surface it to the settings page on every instance and no migration seeds it.
	KeySummaryModel: DefaultSummaryModel,
	// PRD #47 run-health keys. Same no-seeded-row pattern: an absent row synthesizes
	// to these defaults, so All/AdminView surface them to the settings page and no
	// migration seeds them.
	KeyHealthEnabled: DefaultHealthEnabled,
	// PRD #84 capability-aware scheduling kill-switch. Same no-seeded-row pattern: an
	// absent row synthesizes to the default (true), so All/AdminView surface it to the
	// settings page on every instance and no migration seeds it.
	KeyCapabilityAwareScheduling:  DefaultCapabilityAwareScheduling,
	KeyHealthStallSeconds:         DefaultHealthStallSeconds,
	KeyHealthSlowSeconds:          DefaultHealthSlowSeconds,
	KeyHealthQueuedSeconds:        DefaultHealthQueuedSeconds,
	KeyHealthApprovalSeconds:      DefaultHealthApprovalSeconds,
	KeyHealthNudgeCooldownSeconds: DefaultHealthNudgeCooldownSeconds,
	// PRD #58 hosted-worker quota. Same no-seeded-row pattern, so All/AdminView
	// surface it to the settings page on every instance — including compose ones,
	// where hosting is off and the knob is inert. That is deliberate: gating its
	// VISIBILITY on the flag would put a config-dependent branch in a pure map, and
	// an inert knob is cheaper than a settings surface that changes shape with the
	// deployment.
	KeyHostedWorkerQuota: DefaultHostedWorkerQuota,
	// PRD #89 M-allow docker repo allowlist. Same no-seeded-row pattern: an absent
	// row synthesizes to the empty (fail-closed) default, so All/AdminView surface it
	// to the settings page on every instance and no migration seeds it.
	KeyDockerRepoAllowlist: DefaultDockerRepoAllowlist,
	// PRD #333 D5 incidental-finding marker. Same no-seeded-row pattern: an absent row
	// synthesizes to DefaultFindingLabel, so All/AdminView surface it on every instance
	// and no migration seeds it. Validate's default branch applies the label rules.
	KeyFindingLabel: DefaultFindingLabel,
	// PRD #602 M2 agent-source repo sync keys. Same no-seeded-row pattern: an absent
	// row synthesizes to these defaults, so All/AdminView surface them on every
	// instance and no migration seeds them. The credential (agent_source_credential)
	// is a SecretKeys member, deliberately NOT here so it never leaks through a value
	// read.
	KeyAgentSourceRepoURL:  DefaultAgentSourceRepoURL,
	KeyAgentSourceRef:      DefaultAgentSourceRef,
	KeyAgentSourceEnabled:  DefaultAgentSourceEnabled,
	KeyAgentSourceInterval: DefaultAgentSourceInterval,
	// PRD #702 M1: the source folder. Same no-seeded-row pattern — an absent row
	// synthesizes to DefaultAgentSourceFolder, so Known()/admin-writable with no
	// migration and existing installs read the historical ".claude/agents" subtree.
	KeyAgentSourceFolder: DefaultAgentSourceFolder,
	// PRD #836 M1 release-check config toggles/interval. Same no-seeded-row pattern:
	// an absent row synthesizes to these defaults (master + banner ON, 6h cadence), so
	// All/AdminView surface them on every instance and no migration seeds them. The
	// token (release_check_token) is a SecretKeys member, deliberately NOT here; the
	// six engine-written remote-fact keys are likewise absent (only the release-check
	// Runner writes them).
	KeyReleaseCheckEnabled:       DefaultReleaseCheckEnabled,
	KeyReleaseCheckBannerEnabled: DefaultReleaseCheckBannerEnabled,
	KeyReleaseCheckInterval:      DefaultReleaseCheckInterval,
	// PRD #685 M1 instance-branding config keys. Same no-seeded-row pattern: an absent
	// row synthesizes to these defaults, so a fresh install renders unbranded and the
	// public GET /api/branding reports the default shape. NON-SECRET (here, not in
	// SecretKeys) — they are served to everyone incl. signed-out. Logo bytes are NOT
	// here (branding_assets table, D7).
	KeyAppLogoMode:     DefaultAppLogoMode,
	KeyAppLogoPreset:   DefaultAppLogoPreset,
	KeyAppLogoKeepName: DefaultAppLogoKeepName,
	KeyBrandMode:       DefaultBrandMode,
	KeyBrandCompany:    DefaultBrandCompany,
	KeyBrandPlacement:  DefaultBrandPlacement,
	KeyBrandPlaque:     DefaultBrandPlaque,
	// PRD #700 M5 MR review-watcher admin gates. Same no-seeded-row pattern as the
	// judge keys: an absent row synthesizes to these defaults, so All/AdminView
	// surface them to the settings page on every instance and no migration seeds them.
	KeyMrReworkEnabled: DefaultMrReworkEnabled,
	KeyMrReworkCap:     DefaultMrReworkCap,
	// PRD #914 CI-autofix admin kill-switch. Same no-seeded-row pattern as the judge
	// keys: an absent row synthesizes to the default (true), so All/AdminView surface it
	// to the settings page on every instance and no migration seeds it.
	KeyCiAutofixEnabled: DefaultCiAutofixEnabled,
}

// SecretKeys is the set of settings whose values are secrets (PRD #25): sealed
// with secretbox+base64 at rest and NEVER present in any value-producing read.
// They are deliberately kept OUT of Defaults so All/Effective/AdminView.Values —
// which range over Defaults — cannot emit them by construction (the handler
// cannot forget to redact). They are writable (Known reports them) and readable
// only through the decrypt accessors (SlackBotToken/SlackAppToken) used by
// slacksvc. A secret key has no compiled-in default; unset reads as empty.
var SecretKeys = map[string]struct{}{
	KeySlackBotToken: {},
	KeySlackAppToken: {},
	// PRD #602 M2: the private-repo clone credential, sealed like the Slack tokens.
	KeyAgentSourceCredential: {},
	// PRD #836 M1: the optional upstream-release-check GitHub token, sealed like the
	// Slack tokens and kept out of Defaults so it never leaks through a value read.
	KeyReleaseCheckToken: {},
}

// IsSecret reports whether key is a secret setting (sealed at rest, never read
// back through the value-producing paths).
func IsSecret(key string) bool {
	_, ok := SecretKeys[key]
	return ok
}

// Known reports whether key is a setting the API recognizes: a non-secret key
// with a compiled-in default, or a declared secret key. A write to any other key
// is rejected (an admin cannot invent settings the code does not read).
func Known(key string) bool {
	if _, ok := Defaults[key]; ok {
		return true
	}
	return IsSecret(key)
}

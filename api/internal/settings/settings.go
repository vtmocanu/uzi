// Package settings provides instance-level configuration backed by the
// app_settings table (PRD #19). It is a small read-through cache in front of a
// key/value store: the poller and HTTP handlers read the current label values
// on every cycle, so values are cached for a short TTL and an admin write
// invalidates the cache. Compiled-in defaults mean a missing row (or an empty
// table during a fresh boot) never breaks a read.
//
// The cache is per-process: correct for the single-api compose stack. A second
// api replica would serve a stale value for up to the TTL after another
// replica's PUT — a cache-invalidation-across-replicas problem noted for the
// future k8s deployment, out of scope while there is exactly one api process.
package settings

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/agenttmpl"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/termsafe"
	"github.com/vtmocanu/uzi/api/internal/theme"
)

// Setting keys. These are the only keys the API recognizes; writes to any other
// key are rejected (an admin cannot invent settings the code does not read).
const (
	KeyPRDLabel       = "prd_label"
	KeyAutopilotLabel = "autopilot_label"
	// PRDLESS gate-bypass keys (PRD #22). prdless_enabled stores the text
	// "true"/"false"; prdless_label is the escape-hatch label name.
	KeyPrdlessEnabled = "prdless_enabled"
	KeyPrdlessLabel   = "prdless_label"
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
	KeySlackBotToken = "slack_bot_token"
	KeySlackAppToken = "slack_app_token"
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
	// Board membership / run-eligibility label lists (PRD #196). Each is stored as
	// a comma-separated list of label names, following the KeyDockerRepoAllowlist
	// precedent (Decision 8): safe because ValidateLabel rejects commas, so the
	// separator can never collide with a legal label.
	//
	// KeyRunEligibleLabels is the ADMIN-only set of labels a human may point uzi at
	// ("may uzi work this?"). The primary (prd_label) is always in it — the accessor
	// unions it in — so the run gate can never make the primary non-runnable.
	// KeyBoardExtraLabels is the admin DEFAULT for the per-user board membership
	// extras ("which cards do I want to look at?"), applied while a user has no saved
	// set. Board membership is primary ∪ extras (Decision 2), so extras carry no
	// primary union — an extra must be removable.
	// KeyEligibleLabelWaivesPRDLink is an instance-wide bool: an issue eligible by a
	// NON-primary label does not require a prds/*.md link (Decision 7).
	KeyRunEligibleLabels          = "run_eligible_labels"
	KeyBoardExtraLabels           = "board_extra_labels"
	KeyEligibleLabelWaivesPRDLink = "eligible_label_waives_prd_link"
	// KeyFindingLabel is the server-mandated marker label attached to every forge
	// issue filed from an incidental finding (PRD #333 D5). Config-overridable, it is
	// EnsureLabels-ed to exist before the file write (Forgejo resolves label ids and
	// errors on an unknown name) and unioned into the filed label set server-side, so a
	// client can never supply a trigger label that bypasses it. A plain single label, it
	// takes the Decision-8 label rules like prd_label (Validate's default branch).
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
	KeyAgentSourceCredential = "agent_source_credential"
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
	// Instance branding keys (PRD #685 M1). All NON-SECRET public config → they live
	// in Defaults (never SecretKeys), round-trip through PUT /admin/settings, and are
	// served — allowlisted, never via All/AdminView — through the public GET
	// /api/branding. Logo BYTES are NOT keys: they live in the branding_assets table
	// (Decision D7), so nothing here carries a blob and the settings cache stays small.
	KeyAppLogoMode     = "app_logo_mode"      // "default" | "custom"
	KeyAppLogoKeepName = "app_logo_keep_name" // "true" | "false"
	KeyBrandMode       = "brand_mode"         // "none" | "text" | "logo"
	KeyBrandCompany    = "brand_company"      // ≤64-rune company text (may be ""), rendered to every principal incl. signed-out
	KeyBrandPlacement  = "brand_placement"    // "below" | "topright"
	KeyBrandPlaque     = "brand_plaque"       // "true" | "false"
)

// Compiled-in defaults, used when a row is absent so a fresh or partially
// migrated DB still yields a working label set. They mirror the values the
// migration seeds and the hardcoded constants the pre-PRD-19 code used.
const (
	DefaultPRDLabel       = "PRD"
	DefaultAutopilotLabel = "autopilot"
	// PRD #22: on by default (Decision 1). An issue still bypasses the gate only
	// when it carries the label, so default-on weakens nothing for unlabeled
	// issues; admins wanting the strict PRD-only regime flip prdless_enabled off.
	DefaultPrdlessEnabled = "true"
	DefaultPrdlessLabel   = "PRDLESS"
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
	// PRD #196 board membership / run-eligibility defaults (open question 1:
	// opinionated, `bug` ships in both lists). run_eligible_labels ships PRD,bug so a
	// bug is runnable out of the box; board_extra_labels ships bug so a board shows
	// bugs by default. The waiver defaults ON so an admin declaring bug runnable does
	// not then hit the PRD-link gate. On upgrade these apply to any instance that
	// never set the key — a run-gate behaviour change called out in the changelog (M5).
	DefaultRunEligibleLabels          = "PRD,bug"
	DefaultBoardExtraLabels           = "bug"
	DefaultEligibleLabelWaivesPRDLink = "true"
	// PRD #333 D5: the incidental-finding marker defaults to "agent-found", mirroring
	// prd_label's no-seeded-row pattern (an absent row synthesizes to this default).
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
	// PRD #685 M1 instance-branding defaults. A fresh install is UNBRANDED (Decision
	// D4): the app mark stays the uzi FactoryIcon + literals (app_logo_mode=default,
	// keep-name on) and there is no POWERED BY brand (brand_mode=none). brand_company
	// is empty and the plaque is off. Same no-seeded-row pattern as the judge keys —
	// an absent row synthesizes to these defaults, so no migration seeds them.
	DefaultAppLogoMode     = "default"
	DefaultAppLogoKeepName = "true"
	DefaultBrandMode       = "none"
	DefaultBrandCompany    = ""
	DefaultBrandPlacement  = "below"
	DefaultBrandPlaque     = "false"
)

// healthSecondsMin / healthSecondsMax bound the integer health settings (Decision
// 5): a value must be 0 (disable that signal) or within [min, max]. The lower bound
// keeps a signal from firing on a sub-minute jitter; the upper bound stops a
// fat-fingered value (e.g. an extra zero) from silently disabling it.
const (
	healthSecondsMin = 60
	healthSecondsMax = 86400
)

// maxHostedWorkerQuota bounds the per-user hosted-worker quota (PRD #58). Each
// unit is a real pod plus its volumes, so the number an admin types spends cluster
// capacity; the worker namespace's ResourceQuota is the actual backstop (Decision
// 8) and this only catches a typo — an admin meaning 2 and typing 20 gets a
// crowded namespace, one typing 200 gets a rejected write instead of a
// ResourceQuota incident.
const maxHostedWorkerQuota = 20

// maxJudgeDailyBudget bounds the per-user judge daily budget (PRD #69 M5 Decision
// 9). 0 means unlimited (the guard is off); a positive count caps judge runs per
// rolling 24h. The upper bound only catches a fat-fingered value — no real user
// runs thousands of judges a day — so an admin meaning 50 and typing 50000 gets a
// rejected write instead of an effectively-unlimited guard.
const maxJudgeDailyBudget = 10000

// maxLabelLen is Decision 8's length cap (runes, not bytes).
const maxLabelLen = 64

// maxLabelListLen caps the number of entries in a label list setting (PRD #196
// run_eligible_labels / board_extra_labels). A generous bound that only catches a
// runaway paste, enforced in ValidateMerged.
const maxLabelListLen = 32

// Defaults maps every known key to its compiled-in default. This is the single
// Go source of the default values: the accessors fall back to it and the
// migration (00036_app_settings) seeds the same literals. Keep the two in sync —
// SQL cannot reference these constants, so a change here that should also change
// the seeded rows needs a follow-up migration. The PRD #22 prdless keys are the
// exception: they have NO seeded row (Cache.All/Effective synthesize them from
// these defaults), so an absent row is expected and no migration adds them.
// Ranging over Defaults is the canonical way to enumerate the settings the API
// understands.
var Defaults = map[string]string{
	KeyPRDLabel:       DefaultPRDLabel,
	KeyAutopilotLabel: DefaultAutopilotLabel,
	KeyPrdlessEnabled: DefaultPrdlessEnabled,
	KeyPrdlessLabel:   DefaultPrdlessLabel,
	// The instance default theme falls back to the registry's Default ("ember"),
	// so an instance with no seeded row renders exactly as before (PRD #21). No
	// migration seed is needed — this fallback plus the stable GET shape follow
	// automatically from the entry here.
	KeyDefaultTheme: theme.Default,
	// PRD #25 Slack non-secret keys. Like the prdless keys, they have NO seeded
	// row: an absent row synthesizes to these defaults, and no migration adds them.
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
	// PRD #196 board membership / run-eligibility keys. Same no-seeded-row pattern:
	// an absent row synthesizes to these defaults, so All/AdminView surface them to
	// the settings page on every instance and no migration seeds them.
	KeyRunEligibleLabels:          DefaultRunEligibleLabels,
	KeyBoardExtraLabels:           DefaultBoardExtraLabels,
	KeyEligibleLabelWaivesPRDLink: DefaultEligibleLabelWaivesPRDLink,
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
	// PRD #685 M1 instance-branding config keys. Same no-seeded-row pattern: an absent
	// row synthesizes to these defaults, so a fresh install renders unbranded and the
	// public GET /api/branding reports the default shape. NON-SECRET (here, not in
	// SecretKeys) — they are served to everyone incl. signed-out. Logo bytes are NOT
	// here (branding_assets table, D7).
	KeyAppLogoMode:     DefaultAppLogoMode,
	KeyAppLogoKeepName: DefaultAppLogoKeepName,
	KeyBrandMode:       DefaultBrandMode,
	KeyBrandCompany:    DefaultBrandCompany,
	KeyBrandPlacement:  DefaultBrandPlacement,
	KeyBrandPlaque:     DefaultBrandPlaque,
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

// Store is the subset of *store.Queries the cache reads. Declaring it here (not
// depending on the concrete generated type) keeps the package unit-testable
// with an in-memory fake.
type Store interface {
	ListAppSettings(ctx context.Context) ([]store.AppSetting, error)
}

// Cache is a per-process read-through cache over the app_settings table. It is
// safe for concurrent use: the poller and the HTTP handlers read it on every
// cycle, so the fast path (a fresh snapshot) is a read-locked pointer read, and
// the mutex is never held across the store fetch.
type Cache struct {
	q   Store
	ttl time.Duration
	// now is the clock, overridable in tests for deterministic TTL expiry.
	now func() time.Time

	// box seals/opens the secret keys (PRD #25). nil until ConfigureSecrets runs:
	// a nil box means the DB-backed secret decrypt path is unavailable (the
	// env-sourced path never needs it). box and env are set once at boot, before
	// any read, so they need no lock.
	box *secretbox.Box
	// env is the ENV-source overlay (PRD #25): key→plaintext value for the keys an
	// operator set via environment (SLACK_BOT_TOKEN, SLACK_APP_TOKEN,
	// UZI_PUBLIC_BASE_URL). ENV wins over the DB per key. Only keys actually set in
	// the environment appear here.
	env map[string]string

	mu      sync.RWMutex
	values  map[string]string // last fetched rows; replaced wholesale, never mutated in place
	fetched time.Time
	valid   bool
}

// New builds a cache reading through q, refreshing at most once per ttl.
func New(q Store, ttl time.Duration) *Cache {
	return &Cache{q: q, ttl: ttl, now: time.Now}
}

// ConfigureSecrets wires the secret cipher and the ENV-source overlay (PRD #25).
// Called once from main after the Box is built and before serving; box seals and
// opens the secret keys, env carries the operator's environment overrides (only
// keys actually set). Both are read-only after this call.
func (c *Cache) ConfigureSecrets(box *secretbox.Box, env map[string]string) {
	c.box = box
	c.env = env
}

// Invalidate drops the cached snapshot so the next read refetches. Called after
// a settings write commits.
func (c *Cache) Invalidate() {
	c.mu.Lock()
	c.valid = false
	c.mu.Unlock()
}

// snapshot returns the current key→value map, refreshing from the store when the
// cached copy is stale. The returned map is immutable (replaced wholesale on
// refresh), so callers may read it after the lock is released.
//
// On a refresh error it serves the last known-good snapshot when one exists
// (stale-on-error keeps reads working through a transient DB blip); only a cold
// cache with no prior snapshot propagates the error.
func (c *Cache) snapshot(ctx context.Context) (map[string]string, error) {
	c.mu.RLock()
	if c.valid && c.now().Sub(c.fetched) < c.ttl {
		m := c.values
		c.mu.RUnlock()
		return m, nil
	}
	c.mu.RUnlock()

	rows, err := c.q.ListAppSettings(ctx)
	if err != nil {
		c.mu.RLock()
		defer c.mu.RUnlock()
		if c.valid {
			return c.values, nil
		}
		return nil, err
	}

	m := make(map[string]string, len(rows))
	for _, r := range rows {
		m[r.Key] = r.Value
	}
	c.mu.Lock()
	c.values = m
	c.fetched = c.now()
	c.valid = true
	c.mu.Unlock()
	return m, nil
}

// get returns the effective value for a NON-SECRET key: the ENV override when
// set, else the stored row when present and non-empty, else the compiled-in
// default (PRD #25 adds the ENV tier; ENV wins over DB). A cold refresh error
// returns the default alongside the error, so a best-effort caller can ignore
// err and still get a usable value while a strict caller can surface it.
func (c *Cache) get(ctx context.Context, key string) (string, error) {
	if v, ok := c.env[key]; ok && v != "" {
		return v, nil
	}
	m, err := c.snapshot(ctx)
	if err != nil {
		return Defaults[key], err
	}
	return c.effective(key, m), nil
}

// effective is get's pure core against an already-fetched snapshot: ENV over DB
// over the compiled-in default, for a NON-SECRET key. Shared by get and AdminView
// so both apply identical precedence.
func (c *Cache) effective(key string, m map[string]string) string {
	if v, ok := c.env[key]; ok && v != "" {
		return v
	}
	if v, ok := m[key]; ok && v != "" {
		return v
	}
	return Defaults[key]
}

// source reports where a key's effective value comes from (PRD #25): "env" when
// the ENV overlay set it, "db" when a non-empty row exists, else "default". The
// webui greys an env-sourced field and the PUT rejects a write to it.
func (c *Cache) source(key string, m map[string]string) string {
	if v, ok := c.env[key]; ok && v != "" {
		return "env"
	}
	if v, ok := m[key]; ok && v != "" {
		return "db"
	}
	return "default"
}

// configured reports whether a secret key has a value from any source (PRD #25),
// without exposing it — the only thing the admin GET ever reveals about a secret.
func (c *Cache) configured(key string, m map[string]string) bool {
	if v, ok := c.env[key]; ok && v != "" {
		return true
	}
	if v, ok := m[key]; ok && v != "" {
		return true
	}
	return false
}

// IsEnvSourced reports whether key's value is fixed by the ENV overlay. The PUT
// handler rejects writes to such keys (409) so the webui greying reflects an
// enforced policy, not a hint. Pure (no snapshot) — the overlay is static.
func (c *Cache) IsEnvSourced(key string) bool {
	v, ok := c.env[key]
	return ok && v != ""
}

// PRDLabel returns the configured PRD label (Decision 1: the first settings
// tenant). Falls back to DefaultPRDLabel.
func (c *Cache) PRDLabel(ctx context.Context) (string, error) {
	return c.get(ctx, KeyPRDLabel)
}

// AutopilotLabel returns the configured autopilot label. Falls back to
// DefaultAutopilotLabel.
func (c *Cache) AutopilotLabel(ctx context.Context) (string, error) {
	return c.get(ctx, KeyAutopilotLabel)
}

// PrdlessLabel returns the configured PRDLESS escape-hatch label (PRD #22).
// Falls back to DefaultPrdlessLabel.
func (c *Cache) PrdlessLabel(ctx context.Context) (string, error) {
	return c.get(ctx, KeyPrdlessLabel)
}

// PrdlessEnabled reports whether the PRDLESS gate-bypass feature is enabled
// instance-wide (PRD #22, Decision 1). The value is stored as the text
// "true"/"false"; only those two are honored. Any OTHER value falls back to the
// compiled-in default (true) rather than silently reading as false — a deliberate
// junk-tolerance so a malformed value never silently flips a default-on feature
// off. A cold read error also returns the default (true) alongside the error, so
// a best-effort caller can ignore err — an unlabeled issue is still gated, since
// the bypass also requires the label on the fresh snapshot.
func (c *Cache) PrdlessEnabled(ctx context.Context) (bool, error) {
	v, err := c.get(ctx, KeyPrdlessEnabled)
	switch v {
	case "true":
		return true, err
	case "false":
		return false, err
	default:
		return DefaultPrdlessEnabled == "true", err
	}
}

// FindingLabel returns the configured incidental-finding marker label (PRD #333 D5),
// the server-mandated tag every filed finding issue carries. Falls back to
// DefaultFindingLabel ("agent-found"). A single label validated by the Decision-8
// label rules, exactly like PRDLabel.
func (c *Cache) FindingLabel(ctx context.Context) (string, error) {
	return c.get(ctx, KeyFindingLabel)
}

// DefaultTheme returns the configured instance-default theme id (PRD #21).
// Falls back to the theme registry's Default ("ember").
func (c *Cache) DefaultTheme(ctx context.Context) (string, error) {
	return c.get(ctx, KeyDefaultTheme)
}

// SlackEnabled reports whether the Slack integration is enabled instance-wide
// (PRD #25). Stored as the text "true"/"false"; any other value falls back to the
// compiled-in default (false) rather than silently reading true — the same
// junk-tolerance as PrdlessEnabled but defaulting OFF, so a malformed value never
// silently turns the integration on.
func (c *Cache) SlackEnabled(ctx context.Context) (bool, error) {
	v, err := c.get(ctx, KeySlackEnabled)
	switch v {
	case "true":
		return true, err
	case "false":
		return false, err
	default:
		return DefaultSlackEnabled == "true", err
	}
}

// BrandingConfig is the allowlisted instance-branding config (PRD #685 M1): EXACTLY
// the six branding keys, coerced to their typed form. It is the only thing the public
// GET /api/branding reads from settings — built key-by-key here rather than from
// All/AdminView so that anonymous read cannot leak any other settings key (Risk R1).
type BrandingConfig struct {
	AppLogoMode     string
	AppLogoKeepName bool
	BrandMode       string
	BrandCompany    string
	BrandPlacement  string
	BrandPlaque     bool
}

// Branding returns the effective branding config (PRD #685 M1), reading each of the
// six keys individually through the same ENV-over-DB-over-default precedence every
// other accessor uses. The two bools apply the PrdlessEnabled junk-tolerance: only
// "true"/"false" are honored and any other stored value falls back to the compiled-in
// default rather than silently reading false. A cold-refresh error is returned
// alongside a defaults-filled struct so a best-effort caller can ignore err.
//
// It DELIBERATELY does not range over Defaults (as All/AdminView do): the public
// endpoint that consumes this serves anonymous callers, so it must expose only these
// six fields and never the rest of the non-secret settings surface (Risk R1).
func (c *Cache) Branding(ctx context.Context) (BrandingConfig, error) {
	m, err := c.snapshot(ctx)
	boolOf := func(key string) bool {
		switch c.effective(key, m) {
		case "true":
			return true
		case "false":
			return false
		default:
			return Defaults[key] == "true"
		}
	}
	return BrandingConfig{
		AppLogoMode:     c.effective(KeyAppLogoMode, m),
		AppLogoKeepName: boolOf(KeyAppLogoKeepName),
		BrandMode:       c.effective(KeyBrandMode, m),
		BrandCompany:    c.effective(KeyBrandCompany, m),
		BrandPlacement:  c.effective(KeyBrandPlacement, m),
		BrandPlaque:     boolOf(KeyBrandPlaque),
	}, err
}

// PublicBaseURL returns the base URL used to build webui deep links in Slack
// messages (PRD #25). ENV (UZI_PUBLIC_BASE_URL) over the DB row over the
// loopback default.
func (c *Cache) PublicBaseURL(ctx context.Context) (string, error) {
	return c.get(ctx, KeyPublicBaseURL)
}

// JudgeEnabled reports whether the run-judge feature is enabled instance-wide
// (PRD #46 Decision 7): the global kill-switch. Stored as the text "true"/"false";
// any other value falls back to the compiled-in default (false) — the same strict
// junk-tolerance as SlackEnabled, so a malformed value never silently turns token
// spend on.
func (c *Cache) JudgeEnabled(ctx context.Context) (bool, error) {
	v, err := c.get(ctx, KeyJudgeEnabled)
	switch v {
	case "true":
		return true, err
	case "false":
		return false, err
	default:
		return DefaultJudgeEnabled == "true", err
	}
}

// GithubProjectSyncEnabled reports whether the GitHub Projects v2 Status sync is
// enabled instance-wide (PRD #364): the global kill-switch. Stored as the text
// "true"/"false"; any other value falls back to the compiled-in default (false) —
// the same strict junk-tolerance as JudgeEnabled, so a malformed value never
// silently starts writing to a user's project board.
func (c *Cache) GithubProjectSyncEnabled(ctx context.Context) (bool, error) {
	v, err := c.get(ctx, KeyGithubProjectSyncEnabled)
	switch v {
	case "true":
		return true, err
	case "false":
		return false, err
	default:
		return DefaultGithubProjectSyncEnabled == "true", err
	}
}

// EphemeralWorkersEnabled reports whether ephemeral worker auto-provisioning is
// enabled instance-wide (PRD #529 M2): the global kill-switch. Stored as the text
// "true"/"false"; any other value falls back to the compiled-in default (false) —
// the same strict junk-tolerance as JudgeEnabled, so a malformed value never
// silently starts spinning cluster capacity on demand. This is the INSTANCE gate;
// the per-user opt-in (users.ephemeral_workers_enabled) is checked separately, and
// both must be true before the provisioner acts.
func (c *Cache) EphemeralWorkersEnabled(ctx context.Context) (bool, error) {
	v, err := c.get(ctx, KeyEphemeralWorkersEnabled)
	switch v {
	case "true":
		return true, err
	case "false":
		return false, err
	default:
		return DefaultEphemeralWorkersEnabled == "true", err
	}
}

// JudgeEnforceAll reports whether the judge is enforced for every run (PRD #69),
// bypassing the per-user judge_enabled opt-in gate. Stored as the text
// "true"/"false"; any other value falls back to the compiled-in default (false) —
// the same strict junk-tolerance as JudgeEnabled, so a malformed row never
// silently turns forced token spend on.
func (c *Cache) JudgeEnforceAll(ctx context.Context) (bool, error) {
	v, err := c.get(ctx, KeyJudgeEnforceAll)
	switch v {
	case "true":
		return true, err
	case "false":
		return false, err
	default:
		return DefaultJudgeEnforceAll == "true", err
	}
}

// JudgeModel returns the model alias the judge runs on (PRD #46 Decision 7). Falls
// back to the strong DefaultJudgeModel ("opus", PRD #69 Decision 1).
func (c *Cache) JudgeModel(ctx context.Context) (string, error) {
	return c.get(ctx, KeyJudgeModel)
}

// SummaryModel returns the model alias the inline run-summary generator runs on
// (PRD #362 Decision 8). Falls back to DefaultSummaryModel ("haiku"). The per-user
// override (users.summary_model) is resolved user-value-wins at issue-run claim
// assembly, mirroring JudgeModel but on the issue-run claim rather than the judge.
func (c *Cache) SummaryModel(ctx context.Context) (string, error) {
	return c.get(ctx, KeySummaryModel)
}

// AgentSourceEnabled reports whether the agent-source reconcile loop is enabled
// (PRD #602 M3). Strict "true"/"false" with a false fallback, like JudgeEnabled
// — a malformed value never silently starts cloning an external repo.
func (c *Cache) AgentSourceEnabled(ctx context.Context) (bool, error) {
	v, err := c.get(ctx, KeyAgentSourceEnabled)
	switch v {
	case "true":
		return true, err
	case "false":
		return false, err
	default:
		return DefaultAgentSourceEnabled == "true", err
	}
}

// AgentSourceRepoURL returns the configured https clone URL (PRD #602 M3), or ""
// when unconfigured. The value is stored verbatim; the reconcile re-checks it
// against the SSRF allowlist at the clone seam (TOCTOU defense).
func (c *Cache) AgentSourceRepoURL(ctx context.Context) (string, error) {
	return c.get(ctx, KeyAgentSourceRepoURL)
}

// AgentSourceRef returns the pinned tag/SHA or floating branch to clone (PRD #602
// M3), or "" to track the source's default branch.
func (c *Cache) AgentSourceRef(ctx context.Context) (string, error) {
	return c.get(ctx, KeyAgentSourceRef)
}

// AgentSourceFolder returns the repo-relative subfolder the reconcile reads role
// files from (PRD #702 M1). An empty/unset (or whitespace-only) value resolves to
// DefaultAgentSourceFolder (".claude/agents"), so existing installs read the same
// subtree as before. A configured value is returned with any single trailing slash
// trimmed ("product-agents/" → "product-agents"), the normalization that guarantees
// tree.Tree always receives a clean path.
func (c *Cache) AgentSourceFolder(ctx context.Context) (string, error) {
	v, err := c.get(ctx, KeyAgentSourceFolder)
	if strings.TrimSpace(v) == "" {
		return DefaultAgentSourceFolder, err
	}
	return strings.TrimSuffix(v, "/"), err
}

// AgentSourceInterval returns the reconcile cadence (PRD #602 M3). Stored as a Go
// duration string ("1h"); a missing or unparseable value falls back to the
// compiled-in default, and a sub-minute value is floored at 1m so a bad row can
// never make the loop hammer the source (the same floor validateAgentSourceInterval
// enforces at write time).
func (c *Cache) AgentSourceInterval(ctx context.Context) (time.Duration, error) {
	v, err := c.get(ctx, KeyAgentSourceInterval)
	d, perr := time.ParseDuration(v)
	if perr != nil || d <= 0 {
		d, _ = time.ParseDuration(DefaultAgentSourceInterval)
	}
	if d < agentSourceIntervalMin {
		d = agentSourceIntervalMin
	}
	return d, err
}

// AgentSourceCredential returns the private-repo clone token in plaintext (PRD #602
// M3), or "" for a public repo. Same precedence as SlackBotToken: an ENV overlay
// wins, else the sealed DB row is opened with the box. Errors carry no plaintext.
func (c *Cache) AgentSourceCredential(ctx context.Context) (string, error) {
	return c.secret(ctx, KeyAgentSourceCredential)
}

// AgentSourceCredentialConfigured reports whether a private-repo clone token is set
// from any source (PRD #602 M4), without exposing it — the only thing the admin GET
// ever reveals about the sealed credential (mirrors the Slack token's configured bit).
func (c *Cache) AgentSourceCredentialConfigured(ctx context.Context) (bool, error) {
	m, err := c.snapshot(ctx)
	if err != nil {
		return false, err
	}
	return c.configured(KeyAgentSourceCredential, m), nil
}

// AgentSourceLastAppliedSHA returns the fetched SHA of the snapshot last applied to
// agent_templates (PRD #602 M4), or "" when nothing has been applied yet. Apply
// compares the currently-staged snapshot's SHA against this to decide "pending":
// pending == a staged snapshot exists AND its fetched_sha != this value.
func (c *Cache) AgentSourceLastAppliedSHA(ctx context.Context) (string, error) {
	return c.get(ctx, KeyAgentSourceLastAppliedSHA)
}

// AgentSourceStatus is the engine-managed sync/apply status the admin panel reads
// (PRD #602 M4). Every field is stored as an app_setting by the reconcile job
// (last-sync-*) or by Apply (last-applied-*); an absent key reads as "". CountsJSON
// is the raw {"staged":N,"changed":N,"failed":N} blob so the handler can surface it
// without this package importing a counts type.
type AgentSourceStatus struct {
	LastSyncAt     string
	LastSyncSHA    string
	LastSyncStatus string
	LastSyncError  string
	CountsJSON     string
	LastAppliedAt  string
	LastAppliedSHA string
	// Remote facts persisted by the PRD #702 M4 update-check (engine-managed); an
	// absent key reads as "". "Update available" is DERIVED from these + live config,
	// never stored — see agentsource.DeriveUpdate.
	LatestRef       string
	RemoteTipSHA    string
	UpdateCheckedAt string
}

// AgentSourceStatus reads the engine-managed last-sync + last-applied status keys in
// one snapshot pass (PRD #602 M4). Best-effort: a snapshot error returns the zero
// status alongside the error so a best-effort caller can still render an empty panel.
func (c *Cache) AgentSourceStatus(ctx context.Context) (AgentSourceStatus, error) {
	m, err := c.snapshot(ctx)
	if err != nil {
		return AgentSourceStatus{}, err
	}
	return AgentSourceStatus{
		LastSyncAt:      c.effective(KeyAgentSourceLastSyncAt, m),
		LastSyncSHA:     c.effective(KeyAgentSourceLastSyncSHA, m),
		LastSyncStatus:  c.effective(KeyAgentSourceLastSyncStatus, m),
		LastSyncError:   c.effective(KeyAgentSourceLastSyncError, m),
		CountsJSON:      c.effective(KeyAgentSourceLastSyncCounts, m),
		LastAppliedAt:   c.effective(KeyAgentSourceLastAppliedAt, m),
		LastAppliedSHA:  c.effective(KeyAgentSourceLastAppliedSHA, m),
		LatestRef:       c.effective(KeyAgentSourceLatestRef, m),
		RemoteTipSHA:    c.effective(KeyAgentSourceRemoteTipSHA, m),
		UpdateCheckedAt: c.effective(KeyAgentSourceUpdateCheckedAt, m),
	}, nil
}

// HealthEnabled reports whether the run-health detector is enabled instance-wide
// (PRD #47). Stored as "true"/"false"; any other value falls back to the
// compiled-in default (true), the same junk-tolerance as SlackEnabled but
// defaulting ON — a malformed value never silently disables detection.
func (c *Cache) HealthEnabled(ctx context.Context) (bool, error) {
	v, err := c.get(ctx, KeyHealthEnabled)
	switch v {
	case "true":
		return true, err
	case "false":
		return false, err
	default:
		return DefaultHealthEnabled == "true", err
	}
}

// CapabilityAwareScheduling reports whether capability-aware scheduling is enabled
// instance-wide (PRD #84 Decision 13). Stored as "true"/"false"; any other value falls
// back to the compiled-in default (true), the same junk-tolerance as HealthEnabled and
// defaulting ON — a malformed value never silently disables routing. The claim path
// threads the result into ClaimRun as @capability_aware; false makes the capability
// clause trivially true (best-effort claiming) while the docker allowlist clause stays
// enforced.
func (c *Cache) CapabilityAwareScheduling(ctx context.Context) (bool, error) {
	v, err := c.get(ctx, KeyCapabilityAwareScheduling)
	switch v {
	case "true":
		return true, err
	case "false":
		return false, err
	default:
		return DefaultCapabilityAwareScheduling == "true", err
	}
}

// HealthStallSeconds / HealthSlowSeconds / HealthQueuedSeconds /
// HealthApprovalSeconds / HealthNudgeCooldownSeconds return the integer-seconds
// health thresholds (PRD #47 Decision 5). 0 means the caller disables that signal.
// The RUN_TIMEOUT clamp on the slow threshold is applied read-time by the sweeper,
// not here (Validate is pure with no config access).
func (c *Cache) HealthStallSeconds(ctx context.Context) (int, error) {
	return c.intSetting(ctx, KeyHealthStallSeconds)
}
func (c *Cache) HealthSlowSeconds(ctx context.Context) (int, error) {
	return c.intSetting(ctx, KeyHealthSlowSeconds)
}
func (c *Cache) HealthQueuedSeconds(ctx context.Context) (int, error) {
	return c.intSetting(ctx, KeyHealthQueuedSeconds)
}
func (c *Cache) HealthApprovalSeconds(ctx context.Context) (int, error) {
	return c.intSetting(ctx, KeyHealthApprovalSeconds)
}
func (c *Cache) HealthNudgeCooldownSeconds(ctx context.Context) (int, error) {
	return c.intSetting(ctx, KeyHealthNudgeCooldownSeconds)
}

// HostedWorkerQuota returns the per-user hosted-worker quota (PRD #58 Decision 8);
// 0 means self-service provisioning is disabled.
//
// Its caller (the provision handler) reads it STRICTLY — a non-nil error is a 500,
// not a fallback — unlike the best-effort `v, _ :=` prdless reads. Those degrade
// toward the safe side (an unlabeled issue stays gated); this one would degrade
// toward provisioning against a number no admin chose, on a cold-cache blip. The
// junk-tolerance inside intSetting still applies to a hand-edited row, which
// Validate cannot reach retroactively.
func (c *Cache) HostedWorkerQuota(ctx context.Context) (int, error) {
	return c.intSetting(ctx, KeyHostedWorkerQuota)
}

// JudgeCooldownSeconds returns the per-user judge cooldown in seconds (PRD #69 M5
// Decision 9); 0 disables the cooldown guard. Best-effort at the enqueue gate — the
// caller proceeds (fails open) on a read error, since the guard is a soft cost
// backstop, not a correctness control.
func (c *Cache) JudgeCooldownSeconds(ctx context.Context) (int, error) {
	return c.intSetting(ctx, KeyJudgeCooldownSeconds)
}

// JudgeDailyBudget returns the per-user judge daily budget as a count (PRD #69 M5
// Decision 9); 0 means unlimited (the guard is off). Best-effort at the enqueue
// gate, like JudgeCooldownSeconds.
func (c *Cache) JudgeDailyBudget(ctx context.Context) (int, error) {
	return c.intSetting(ctx, KeyJudgeDailyBudget)
}

// DockerRepoAllowlist returns the set of repo ids a docker-enabled worker may claim
// runs for (PRD #89 M-allow). Stored as a comma-separated list of repo UUIDs; an
// absent/empty value yields an EMPTY slice, which the claim gate treats as
// fail-closed (a docker worker then claims no repo-bearing run). Unparseable tokens
// in a hand-edited row are skipped rather than erroring — the same junk-tolerance as
// the bool/int accessors, since write-time validation is the real gate. The slice is
// always non-nil so the claim param encodes as a Postgres array, never NULL.
//
// The claim path (workersvc) reads this STRICTLY — a non-nil error is surfaced and
// the run is left unclaimed — because this is a security control: never claim a repo
// run when the allowlist cannot be read (mirrors HostedWorkerQuota's strict caller).
func (c *Cache) DockerRepoAllowlist(ctx context.Context) ([]uuid.UUID, error) {
	v, err := c.get(ctx, KeyDockerRepoAllowlist)
	return parseRepoAllowlist(v), err
}

// parseRepoAllowlist splits a comma-separated repo-id list into canonical UUIDs,
// skipping empty and unparseable tokens. Always returns a non-nil slice (possibly
// empty). Shared by the accessor and reused as the parse half of validation's intent.
func parseRepoAllowlist(v string) []uuid.UUID {
	out := []uuid.UUID{}
	for _, tok := range strings.Split(v, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		id, err := uuid.Parse(tok)
		if err != nil {
			continue
		}
		out = append(out, id)
	}
	return out
}

// RunEligibleLabels returns the set of labels a human may point uzi at (PRD #196,
// admin-only). The primary label (prd_label) is ALWAYS unioned in and placed
// first, deduped — so even a hand-edited row that dropped the primary can never
// make it non-runnable (fail-safe: the run gate must never refuse the primary).
// Parsed from the comma-separated run_eligible_labels value; junk-tolerant like
// the other list accessor. Always non-nil. A cold read error is returned so a
// strict caller can surface it, alongside the compiled-in default set (the
// default primary unioned with the default eligible list).
func (c *Cache) RunEligibleLabels(ctx context.Context) ([]string, error) {
	primary, err := c.PRDLabel(ctx)
	v, verr := c.get(ctx, KeyRunEligibleLabels)
	if err == nil {
		err = verr
	}
	labels := parseLabelList(v)
	// Union the primary in, first, deduped.
	out := []string{primary}
	seen := map[string]struct{}{primary: {}}
	for _, l := range labels {
		if _, dup := seen[l]; dup {
			continue
		}
		seen[l] = struct{}{}
		out = append(out, l)
	}
	return out, err
}

// BoardExtraLabels returns the admin default set of board membership extras (PRD
// #196), the fallback a client uses while a user has no saved set (per-user
// storage is M3). No primary union — extras are extras, and board membership is
// primary ∪ extras (Decision 2), so an extra must be removable. Parsed from the
// comma-separated board_extra_labels value; always non-nil.
func (c *Cache) BoardExtraLabels(ctx context.Context) ([]string, error) {
	v, err := c.get(ctx, KeyBoardExtraLabels)
	return parseLabelList(v), err
}

// EligibleLabelWaivesPRDLink reports whether an issue eligible by a NON-primary
// label may run without a prds/*.md link (PRD #196 Decision 7), instance-wide.
// Stored as the text "true"/"false"; any other value falls back to the
// compiled-in default (true) rather than silently reading false — the same
// junk-tolerance as PrdlessEnabled, so a malformed value never silently flips a
// default-on gate off.
func (c *Cache) EligibleLabelWaivesPRDLink(ctx context.Context) (bool, error) {
	v, err := c.get(ctx, KeyEligibleLabelWaivesPRDLink)
	switch v {
	case "true":
		return true, err
	case "false":
		return false, err
	default:
		return DefaultEligibleLabelWaivesPRDLink == "true", err
	}
}

// parseLabelList splits a comma-separated label list into trimmed, non-empty
// tokens, preserving order. Always returns a non-nil slice (possibly empty). It
// deliberately does NOT dedup — deduplication is a merged-validation concern
// (ValidateMerged), not a parse concern. Mirrors parseRepoAllowlist.
func parseLabelList(v string) []string {
	out := []string{}
	for _, tok := range strings.Split(v, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		out = append(out, tok)
	}
	return out
}

// intSetting resolves an integer setting to its parsed value, falling back to the
// compiled-in default when the effective value is absent or unparseable. Stored
// values pass validateHealthSeconds at write time, so an unparseable value here is
// a row predating validation or a hand-edited DB — junk-tolerance mirrors the bool
// accessors. A cold read error is returned so a strict caller can surface it.
func (c *Cache) intSetting(ctx context.Context, key string) (int, error) {
	v, err := c.get(ctx, key)
	n, perr := strconv.Atoi(strings.TrimSpace(v))
	if perr != nil {
		n, _ = strconv.Atoi(Defaults[key])
	}
	return n, err
}

// SlackBotToken returns the effective Slack bot token in plaintext (PRD #25):
// the ENV value when set, else the sealed DB row decrypted. Empty string + nil
// error when neither is configured. Only slacksvc calls this — it is the sole
// read path for a secret key, keeping token bytes out of every other accessor.
func (c *Cache) SlackBotToken(ctx context.Context) (string, error) {
	return c.secret(ctx, KeySlackBotToken)
}

// SlackAppToken returns the effective Slack app-level token in plaintext (PRD
// #25), same precedence as SlackBotToken.
func (c *Cache) SlackAppToken(ctx context.Context) (string, error) {
	return c.secret(ctx, KeySlackAppToken)
}

// secret resolves a secret key to plaintext: the ENV overlay value verbatim (it
// is already plaintext), else the base64-of-sealed DB row opened with the box.
// A DB-stored secret with no configured box is an error (misconfiguration), not
// a silent empty. Errors carry no plaintext.
func (c *Cache) secret(ctx context.Context, key string) (string, error) {
	if v, ok := c.env[key]; ok && v != "" {
		return v, nil
	}
	m, err := c.snapshot(ctx)
	if err != nil {
		return "", err
	}
	enc, ok := m[key]
	if !ok || enc == "" {
		return "", nil
	}
	if c.box == nil {
		return "", errors.New("settings: secret decrypt requested but no cipher configured")
	}
	return DecodeSecret(c.box, enc)
}

// SealSecret encrypts a secret setting's plaintext for storage: secretbox-seal
// then base64 (app_settings.value is TEXT). The handler calls this before
// UpsertAppSetting.
func SealSecret(box *secretbox.Box, plaintext string) (string, error) {
	sealed, err := box.Seal([]byte(plaintext))
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// DecodeSecret reverses SealSecret: base64-decode then secretbox-open. A tampered
// or wrong-key row returns an authentication error carrying no plaintext.
func DecodeSecret(box *secretbox.Box, encoded string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("settings: stored secret is not valid base64: %w", err)
	}
	plain, err := box.Open(raw)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// ValueForStorage prepares a setting value for its app_settings row: a secret key
// is sealed (secretbox+base64) so the stored bytes are never the token itself;
// every other key is stored verbatim. This is the single write-side seam — the
// counterpart to the read-side structural exclusion — so the settings PUT cannot
// persist a secret in the clear even by omission.
func ValueForStorage(box *secretbox.Box, key, plaintext string) (string, error) {
	if IsSecret(key) {
		return SealSecret(box, plaintext)
	}
	return plaintext, nil
}

// All returns every known NON-SECRET key with its effective value (ENV over row
// over default). The shape is stable — one entry per key in Defaults, secret keys
// structurally excluded — so the admin UI never has to reason about missing rows
// and a secret value can never leak through here. A cold refresh error is
// returned so the handler can surface it rather than silently show defaults.
func (c *Cache) All(ctx context.Context) (map[string]string, error) {
	m, err := c.snapshot(ctx)
	out := make(map[string]string, len(Defaults))
	for k := range Defaults {
		out[k] = c.effective(k, m)
	}
	return out, err
}

// AdminView is the admin GET /api/admin/settings shape (PRD #25). Values carries
// the non-secret effective values (secret keys structurally absent); Secrets maps
// each secret key to whether it is configured (never the value); Sources maps
// EVERY key to "env"|"db"|"default". Splitting the value map from the secret
// map is what makes a token leak impossible — nothing here can carry a secret's
// bytes.
type AdminView struct {
	Values  map[string]string
	Secrets map[string]bool
	Sources map[string]string
}

// AdminView assembles the admin settings view. A cold refresh error is returned
// (the handler 500s on it) but the maps are still filled from defaults/ENV so a
// caller ignoring err sees a usable shape.
func (c *Cache) AdminView(ctx context.Context) (AdminView, error) {
	m, err := c.snapshot(ctx)
	av := AdminView{
		Values:  make(map[string]string, len(Defaults)),
		Secrets: make(map[string]bool, len(SecretKeys)),
		Sources: make(map[string]string, len(Defaults)+len(SecretKeys)),
	}
	for k := range Defaults {
		av.Values[k] = c.effective(k, m)
		av.Sources[k] = c.source(k, m)
	}
	for k := range SecretKeys {
		av.Secrets[k] = c.configured(k, m)
		av.Sources[k] = c.source(k, m)
	}
	return av, err
}

// Effective computes the effective value map for a slice of stored rows: every
// known key mapped to its row value when present and non-empty, else the
// compiled-in default. Unknown-key rows are ignored. It is the row-slice form of
// All (which applies the same rule to the cache snapshot), for a caller holding
// freshly read rows — the settings PUT reading its own FOR UPDATE-locked rows
// inside the write transaction — that must compute the committed effective state
// without going through the (possibly stale) cache. See ValidateMerged.
func Effective(rows []store.AppSetting) map[string]string {
	out := make(map[string]string, len(Defaults))
	for k, def := range Defaults {
		out[k] = def
	}
	for _, r := range rows {
		if _, known := Defaults[r.Key]; known && r.Value != "" {
			out[r.Key] = r.Value
		}
	}
	return out
}

// Validate applies the per-key write rules, dispatching on key (PRD #21): the
// label keys use the Decision 8 label rules; default_theme must be a known theme
// id. Unknown keys are the caller's responsibility (guard with Known first);
// this only dispatches recognized keys. The cross-key label rule stays in
// ValidateMerged — this is the single-value gate the settings PUT runs per key.
func Validate(key, value string) error {
	switch key {
	case KeyDefaultTheme:
		return theme.Validate(value)
	case KeyPrdlessEnabled, KeySlackEnabled, KeyJudgeEnabled, KeyJudgeEnforceAll, KeyHealthEnabled,
		KeyCapabilityAwareScheduling, KeyEligibleLabelWaivesPRDLink, KeyGithubProjectSyncEnabled,
		KeyEphemeralWorkersEnabled, KeyAgentSourceEnabled,
		KeyAppLogoKeepName, KeyBrandPlaque:
		return validateBool(value)
	case KeyAppLogoMode:
		return validateEnum(value, "default", "custom")
	case KeyBrandMode:
		return validateEnum(value, "none", "text", "logo")
	case KeyBrandPlacement:
		return validateEnum(value, "below", "topright")
	case KeyBrandCompany:
		return validateBrandCompany(value)
	case KeyJudgeModel, KeySummaryModel:
		return validateModelAlias(value)
	case KeyAgentSourceInterval:
		return validateAgentSourceInterval(value)
	case KeyAgentSourceRepoURL:
		return validateAgentSourceRepoURL(value)
	case KeyAgentSourceRef:
		return validateAgentSourceRef(value)
	case KeyAgentSourceFolder:
		return validateAgentSourceFolder(value)
	case KeyHealthStallSeconds, KeyHealthSlowSeconds, KeyHealthQueuedSeconds,
		KeyHealthApprovalSeconds, KeyHealthNudgeCooldownSeconds:
		return validateHealthSeconds(value)
	case KeyJudgeCooldownSeconds:
		// {0} ∪ [60, 86400], identical to the run-health seconds bounds (PRD #69 M5
		// Decision 9), so validateHealthSeconds enforces it verbatim — 0 disables the
		// cooldown, the day cap stops a fat-fingered value silently disabling it.
		return validateHealthSeconds(value)
	case KeyJudgeDailyBudget:
		return validateJudgeDailyBudget(value)
	case KeyHostedWorkerQuota:
		return validateHostedWorkerQuota(value)
	case KeyDockerRepoAllowlist:
		return validateRepoAllowlist(value)
	case KeyRunEligibleLabels, KeyBoardExtraLabels:
		return validateLabelList(value)
	case KeyPublicBaseURL:
		return ValidatePublicBaseURL(value)
	case KeySlackBotToken:
		// Format-only (prefix) here; the live AuthTest runs in the handler. The
		// error must never echo the value (a pasted token), so it names only the
		// expected shape.
		return validateSlackToken(value, "xoxb-", "bot")
	case KeySlackAppToken:
		return validateSlackToken(value, "xapp-", "app-level")
	case KeyAgentSourceCredential:
		return validateAgentSourceCredential(value)
	default:
		// The label keys (prd_label, autopilot_label, prdless_label) all use the
		// Decision 8 label rules; cross-key distinctness is ValidateMerged's job.
		return ValidateLabel(value)
	}
}

// maxAgentSourceCredentialLen caps the sealed clone token (PRD #602 M2). It is
// generous on purpose: a GitHub fine-grained PAT (github_pat_...) is ~93 chars,
// well over the 64-char label cap that KeyAgentSourceCredential would otherwise
// inherit from the ValidateLabel default branch, so a legitimate token must not
// be rejected at write. It is a single-line opaque token, never a multi-line key.
const maxAgentSourceCredentialLen = 1024

// validateAgentSourceCredential is the write-time gate for the sealed private-repo
// clone token (PRD #602 M2). Unlike the label default branch it does NOT cap at 64
// chars or ban commas — a real PAT is longer and may contain them. It rejects an
// empty/whitespace-only value, any control character, and any internal whitespace
// (a clone token is a single opaque token with no spaces), and caps a generous
// length. The error never echoes the value.
func validateAgentSourceCredential(value string) error {
	if strings.TrimSpace(value) == "" {
		return errors.New("credential must not be empty")
	}
	if utf8.RuneCountInString(value) > maxAgentSourceCredentialLen {
		return fmt.Errorf("credential must be at most %d characters", maxAgentSourceCredentialLen)
	}
	for _, r := range value {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return errors.New("credential must not contain whitespace or control characters")
		}
	}
	return nil
}

// validateSlackToken is the format gate for a secret Slack token (PRD #25):
// non-empty and the expected xoxb-/xapp- prefix. It deliberately NEVER includes
// the value in its error — a token must not appear in a validation message.
func validateSlackToken(value, prefix, kind string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s token must not be empty", kind)
	}
	if !strings.HasPrefix(value, prefix) {
		return fmt.Errorf("%s token must start with %s", kind, prefix)
	}
	return nil
}

// validateModelAlias is the format gate for the judge model setting (PRD #46): a
// non-empty model alias / id, checked with the shared PRD #17 rules (single token,
// no control chars, length-capped). Blank is rejected here (unlike the per-user
// inherit case) — the judge always needs a concrete model.
func validateModelAlias(value string) error {
	if strings.TrimSpace(value) == "" {
		return errors.New("must not be empty")
	}
	_, err := agenttmpl.ValidateModel(value)
	return err
}

// agentSourceIntervalMin is the reconcile-cadence floor (PRD #602 M2): a sub-minute
// interval would let the reconcile loop hammer the source, so the write is rejected.
const agentSourceIntervalMin = time.Minute

// maxAgentSourceRefLen caps the git ref length (PRD #602 M2). A tag/branch/SHA is
// short; the bound only catches a runaway paste.
const maxAgentSourceRefLen = 256

// validateAgentSourceInterval is the write-time gate for the agent-source reconcile
// cadence (PRD #602 M2): a Go duration string ("1h", "15m") that parses to at least
// agentSourceIntervalMin. Unlike selfimprove_interval (validateDuration, positive
// only) it enforces a 1-minute floor so a fat-fingered sub-minute value cannot make
// the reconcile loop hammer the source.
func validateAgentSourceInterval(value string) error {
	d, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil {
		return errors.New(`must be a duration like "1h"`)
	}
	if d < agentSourceIntervalMin {
		return errors.New("must be at least 1m")
	}
	return nil
}

// validateAgentSourceRepoURL is the write-time gate for the agent-source clone URL
// (PRD #602 M2). Empty is allowed — it is the "unconfigured" value (the feature is
// off by default and no canonical repo is pre-filled, ADR-0602). A non-empty value
// must be an absolute https URL with a host. The SEPARATE SSRF allowlist check
// against AGENT_SOURCE_ALLOWED_BASE_URLS is the HANDLER's job (Validate is a pure
// (key,value) function with no Config, and importing config here is a cycle), so
// this only enforces the format — https, absolute, non-empty host.
func validateAgentSourceRepoURL(value string) error {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	// Reject surrounding whitespace rather than silently trimming: the value is
	// stored verbatim in a non-secret key, and a padded URL would be fed to go-git
	// at the M3 clone seam. Validate and the allowlist check both trim, so without
	// this the stored form could differ from the checked form.
	if value != strings.TrimSpace(value) {
		return errors.New("must not have leading or trailing whitespace")
	}
	u, err := url.Parse(value)
	if err != nil {
		return errors.New("must be a valid URL")
	}
	if u.Scheme != "https" {
		return errors.New("must use https")
	}
	if u.Host == "" {
		return errors.New("must include a host")
	}
	// Reject URL userinfo (https://token@host/...): agent_source_repo_url is a
	// NON-secret key, stored in cleartext and surfaced in the admin GET and (M3)
	// go-git clone error strings. A credential belongs in the sealed
	// agent_source_credential field, never in the URL.
	if u.User != nil {
		return errors.New("must not embed credentials in the URL; use the agent_source_credential field")
	}
	return nil
}

// validateAgentSourceRef is the write-time gate for the pinned git ref (PRD #602
// M2): a tag, branch, or SHA. Empty is allowed (unused while the URL is empty). A
// non-empty value must be a single token — no whitespace and no control characters,
// length-capped — mirroring the label-style single-token rule. A pinned tag/SHA is
// the recommended form and a bare branch is the floating opt-in; the two are not
// distinguished here.
func validateAgentSourceRef(value string) error {
	if value == "" {
		return nil
	}
	if strings.TrimSpace(value) == "" {
		return errors.New("must not be whitespace only")
	}
	if utf8.RuneCountInString(value) > maxAgentSourceRefLen {
		return fmt.Errorf("must be at most %d characters", maxAgentSourceRefLen)
	}
	for _, r := range value {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return errors.New("must not contain whitespace or control characters")
		}
	}
	return nil
}

// validateAgentSourceFolder is the write-time gate for the agent-source subfolder
// (PRD #702 M1, Decision 2). It selects a subtree of the already-cloned,
// already-allowlisted source repo, so it must be a CLEAN repo-relative subpath — it
// never reaches the network and adds no egress. Empty is allowed (it resolves to
// DefaultAgentSourceFolder at read time, so existing installs are unchanged). A
// non-empty value is rejected when it: is whitespace-only; exceeds the length cap;
// contains a control character; looks like a URL (contains "://") or a UNC path
// (leading "\\"); has a leading "/" (it is repo-relative, not absolute); carries a
// ".." path segment (which would escape the subtree); or has a scheme-like ":" in
// its first segment. A single trailing slash is ACCEPTED here (the Cache reader
// normalizes it away before tree.Tree) — this is validation, not a rewrite, so the
// illegal cases return an error rather than being silently cleaned.
func validateAgentSourceFolder(value string) error {
	if value == "" {
		return nil
	}
	if strings.TrimSpace(value) == "" {
		return errors.New("must not be whitespace only")
	}
	if utf8.RuneCountInString(value) > maxAgentSourceRefLen {
		return fmt.Errorf("must be at most %d characters", maxAgentSourceRefLen)
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return errors.New("must not contain control characters")
		}
	}
	if strings.Contains(value, "://") {
		return errors.New("must be a repo-relative path, not a URL")
	}
	if strings.HasPrefix(value, `\\`) {
		return errors.New("must be a repo-relative path, not a UNC path")
	}
	if strings.HasPrefix(value, "/") {
		return errors.New("must be repo-relative (no leading slash)")
	}
	// Analyze segments against the trailing-slash-normalized form (the reader trims
	// the same single trailing slash before tree.Tree). A ".." segment escapes the
	// subtree; a ":" in the first segment is a scheme/host smell.
	trimmed := strings.TrimSuffix(value, "/")
	for i, seg := range strings.Split(trimmed, "/") {
		if seg == ".." {
			return errors.New(`must not contain a ".." path segment`)
		}
		if i == 0 && strings.Contains(seg, ":") {
			return errors.New("must be a repo-relative path, not a scheme/host")
		}
	}
	return nil
}

// ValidatePublicBaseURL enforces the deep-link base-URL rule (PRD #25): a
// parseable URL with an http or https scheme and a host. It becomes a button URL
// in every DM, so no other scheme is allowed. Reused by config to check the
// UZI_PUBLIC_BASE_URL env value at boot (single source of the rule).
func ValidatePublicBaseURL(value string) error {
	u, err := url.Parse(strings.TrimSpace(value))
	if err != nil {
		return errors.New("must be a valid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("must use http or https")
	}
	if u.Host == "" {
		return errors.New("must include a host")
	}
	return nil
}

// validateBool is the strict on/off parse for a boolean setting (PRD #22): exactly
// "true" or "false", nothing else (no "1"/"yes"/case variants), so a stored
// prdless_enabled is always one of the two values the typed accessor honors.
func validateBool(value string) error {
	if value != "true" && value != "false" {
		return errors.New(`must be "true" or "false"`)
	}
	return nil
}

// validateEnum is the write-time gate for a closed-set string setting (PRD #685 M1:
// the branding enums app_logo_mode / brand_mode / brand_placement). Like the int
// validators, an explicit Validate case backed by this is load-bearing: Validate's
// default branch falls through to ValidateLabel, which accepts ANY non-empty ≤64-char
// string — so an enum key missing from the switch would accept "wat", and the reader
// would then silently fall back to the compiled-in default. An enum must fail the
// WRITE, the only moment a human is present to be told. The error names the allowed
// set so the admin can fix it.
func validateEnum(value string, allowed ...string) error {
	for _, a := range allowed {
		if value == a {
			return nil
		}
	}
	return fmt.Errorf("must be one of: %s", strings.Join(allowed, ", "))
}

// maxBrandCompanyLen caps the POWERED BY company text (PRD #685 M1). 64 runes, the
// same visual cap as a label; unlike ValidateLabel this is measured in RUNES via
// utf8.RuneCountInString so a multibyte name is not undercounted.
const maxBrandCompanyLen = 64

// validateBrandCompany is the DEDICATED write-time gate for brand_company (PRD #685
// M1). It deliberately does NOT reuse ValidateLabel: the branding company text may be
// empty (the default) and may contain commas ("Acme, Inc."), both of which
// ValidateLabel rejects. It DOES enforce a 64-rune cap and — because this text is
// admin-authored yet rendered into every user's chrome, including signed-out
// (the "rendered to a principal other than the author" class .claude/rules/web.md
// governs) — it rejects control and Unicode-format runes via termsafe.Validate, so an
// RTL-override or zero-width rune cannot mangle the chrome for everyone. The empty
// value passes termsafe.Validate (no runes to reject), so no special case is needed.
func validateBrandCompany(value string) error {
	if utf8.RuneCountInString(value) > maxBrandCompanyLen {
		return fmt.Errorf("must be at most %d characters", maxBrandCompanyLen)
	}
	return termsafe.Validate("brand_company", value)
}

// validateHealthSeconds is the write-time gate for an integer run-health threshold
// (PRD #47 Decision 5): a base-10 integer that is either 0 (disable that signal) or
// within [healthSecondsMin, healthSecondsMax]. Negatives, non-integers, 1–59, and
// values above the day cap are rejected. The health_slow_seconds < RUN_TIMEOUT rule
// is NOT enforced here — Validate is pure with no config access, so that is a
// read-time clamp in the sweeper.
func validateHealthSeconds(value string) error {
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return errors.New("must be a whole number of seconds")
	}
	if n == 0 {
		return nil
	}
	if n < healthSecondsMin || n > healthSecondsMax {
		return fmt.Errorf("must be 0 (disabled) or between %d and %d seconds", healthSecondsMin, healthSecondsMax)
	}
	return nil
}

// validateHostedWorkerQuota is the write-time gate for the per-user hosted-worker
// quota (PRD #58 Decision 8): a base-10 integer in {0} ∪ [1, maxHostedWorkerQuota],
// where 0 is the documented "self-service disabled" value rather than a rejection.
// Negatives and non-integers are refused.
//
// The explicit Validate case this backs is load-bearing, not decoration. Validate's
// default branch falls through to ValidateLabel, which accepts any non-empty
// ≤64-char string — so an integer key that is in Defaults but missing from the
// switch would accept "abc", and intSetting would then silently fall back to the
// compiled-in default on every read. An admin typing 0 to disable self-service
// would be told it saved and would still get 2. An int setting must fail the WRITE,
// which is the only moment a human is present to be told.
func validateHostedWorkerQuota(value string) error {
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return errors.New("must be a whole number of workers")
	}
	if n == 0 {
		return nil
	}
	if n < 0 || n > maxHostedWorkerQuota {
		return fmt.Errorf("must be 0 (self-service disabled) or between 1 and %d workers", maxHostedWorkerQuota)
	}
	return nil
}

// validateJudgeDailyBudget is the write-time gate for the per-user judge daily
// budget (PRD #69 M5 Decision 9): a base-10 integer in {0} ∪ [1, maxJudgeDailyBudget],
// where 0 is the documented "unlimited / guard off" value rather than a rejection.
// Negatives, non-integers, and values above the cap are refused.
//
// Like validateHostedWorkerQuota, the explicit Validate case this backs is
// load-bearing: Validate's default branch falls through to ValidateLabel, which
// accepts any non-empty ≤64-char string — so without this case "abc" would save and
// intSetting would silently fall back to the compiled-in default on every read, an
// admin's typed cap silently ignored.
func validateJudgeDailyBudget(value string) error {
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return errors.New("must be a whole number of judge runs")
	}
	if n == 0 {
		return nil
	}
	if n < 0 || n > maxJudgeDailyBudget {
		return fmt.Errorf("must be 0 (unlimited) or between 1 and %d judge runs", maxJudgeDailyBudget)
	}
	return nil
}

// validateRepoAllowlist is the write-time gate for the docker repo allowlist (PRD
// #89 M-allow): a comma-separated list of repo UUIDs. Empty is allowed — it is the
// fail-closed "no repos trusted" value, not a rejection. Each non-empty entry must
// be a valid UUID; a malformed entry fails the WRITE, the only moment a human is
// present to be told, so a typo can never silently widen or void the gate.
//
// Like validateHostedWorkerQuota, this MUST have an explicit Validate case: the
// default branch falls through to ValidateLabel, which REJECTS the comma that an
// allowlist of two or more repos requires — so without this case a valid multi-repo
// allowlist could never be saved at all.
func validateRepoAllowlist(value string) error {
	for _, tok := range strings.Split(value, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		if _, err := uuid.Parse(tok); err != nil {
			return errors.New("must be a comma-separated list of repo ids (UUIDs)")
		}
	}
	return nil
}

// validateLabelList is the write-time gate for a comma-separated label list (PRD
// #196 run_eligible_labels / board_extra_labels). Empty is allowed — it is the "no
// extras" value, and the eligible-set's must-contain-primary rule is enforced in
// ValidateMerged, not here. Each non-empty token must pass ValidateLabel; a
// malformed token fails the WRITE, the only moment a human is present to be told.
//
// Like validateRepoAllowlist, this MUST have an explicit Validate case: the
// default branch falls through to ValidateLabel, which REJECTS the comma that a
// two-or-more label list requires — so without this case a valid multi-label list
// could never be saved at all.
func validateLabelList(value string) error {
	for _, tok := range strings.Split(value, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		if err := ValidateLabel(tok); err != nil {
			return err
		}
	}
	return nil
}

// ValidateLabel checks a single label value against Decision 8's per-value
// rules: non-empty, at most 64 characters, and no comma (GitLab's label-list
// separator). It does not trim: a value with surrounding whitespace would not
// match the same label on the forge, so a whitespace-only value is rejected as
// empty and the caller is expected to send the exact label string.
func ValidateLabel(value string) error {
	if strings.TrimSpace(value) == "" {
		return errors.New("must not be empty")
	}
	if utf8.RuneCountInString(value) > maxLabelLen {
		return errors.New("must be at most 64 characters")
	}
	if strings.ContainsRune(value, ',') {
		return errors.New("must not contain a comma")
	}
	return nil
}

// LabelChanged reports whether any submitted setting that affects which issues a
// board shows actually changed value: a board-filtering label (prd_label or
// autopilot_label) in updates whose value differs from committed. The settings PUT
// uses it to decide whether to force a full repo resync. Only those two keys
// re-filter a board, so the check is a whitelist — every other key
// (default_theme presentation-only, the prdless gate keys, the PRD #25 slack keys)
// is ignored, and a secret key's plaintext never participates. An idempotent write
// (same value) returns false, matching the prior "only resync on a real change".
//
// The PRD #196 list keys (run_eligible_labels, board_extra_labels) are
// DELIBERATELY omitted: the sync fetch (forgesvc) reads only the primary label,
// with ANDed forge semantics, so a resync triggered by these keys would fetch
// nothing new and is pointless — and adding them here is exactly how the
// ANDed-fetch eviction defect gets re-opened from the other end (PRD #196
// Decision 5). Their write path must never force a resync.
func LabelChanged(committed, updates map[string]string) bool {
	for k, v := range updates {
		if k != KeyPRDLabel && k != KeyAutopilotLabel {
			continue
		}
		if committed[k] != v {
			return true
		}
	}
	return false
}

// ValidateMerged enforces the cross-key label rules on the effective post-update
// state (current values overlaid with the pending update), so a PUT touching one
// key is still checked against the others' stored values. The label triple —
// prd_label, autopilot_label, prdless_label — must be pairwise-distinct (Decision 8
// + PRD #22 Decision 7): equal prd/autopilot would autopilot every PRD issue; a
// prdless label equal to prd_label would exempt every issue from the gate, equal to
// autopilot_label would conflate "hands-off" with "spec-less". prdless distinctness
// is enforced REGARDLESS of prdless_enabled — this map carries no toggle state — so
// a disabled-but-colliding label must be renamed before the colliding prd/autopilot
// value can be saved, keeping a later re-enable always safe. Each error names the
// key to change.
func ValidateMerged(merged map[string]string) error {
	if merged[KeyPRDLabel] == merged[KeyAutopilotLabel] {
		return errors.New("prd_label and autopilot_label must differ")
	}
	if merged[KeyPrdlessLabel] == merged[KeyPRDLabel] {
		return errors.New("prdless_label must differ from prd_label")
	}
	if merged[KeyPrdlessLabel] == merged[KeyAutopilotLabel] {
		return errors.New("prdless_label must differ from autopilot_label")
	}

	// PRD #196 list-key cross-checks on the effective post-update state. Parse both
	// lists from the merged map (no dedup at parse time — dedup is checked here).
	eligible := parseLabelList(merged[KeyRunEligibleLabels])
	extras := parseLabelList(merged[KeyBoardExtraLabels])

	// The primary is always eligible (Decision 1: the run gate must never make the
	// primary non-runnable). We UNION it into the effective eligible set here rather
	// than rejecting a set that omits it. A hard "must contain the primary" check
	// wedges every settings PUT on an instance that renamed prd_label: the compiled-in
	// default is the literal "PRD,bug", so on such an instance the effective eligible
	// set never contains the renamed primary, and an unrelated change (e.g.
	// default_theme) would be rejected because it re-validates the whole merged state.
	// The accessor (RunEligibleLabels) unions the primary in for the same fail-safe
	// reason, so a stored set missing it is harmless; the AdminSettings UI additionally
	// pins the primary so a normal save always carries it. Union before the structural
	// checks so the dedup/cap counts reflect what the accessor will actually return.
	primary := merged[KeyPRDLabel]
	if !containsLabel(eligible, primary) {
		eligible = append([]string{primary}, eligible...)
	}

	// Each list: no duplicates, at most maxLabelListLen entries, and no entry equal
	// to a workflow marker (autopilot_label / prdless_label) — those are never
	// membership or eligibility content (mock §5).
	if err := validateLabelListMerged(KeyRunEligibleLabels, eligible, merged); err != nil {
		return err
	}
	if err := validateLabelListMerged(KeyBoardExtraLabels, extras, merged); err != nil {
		return err
	}

	// PRD #602 M2: an ENABLED agent source must carry both a URL and a ref. This is
	// pure string logic on the merged post-update state — the allowlist half of the
	// URL check is the handler's job (ValidateMerged has no Config). It enforces the
	// enable-on atomicity ADR-0602 calls for: a partial write must never leave
	// enabled=true with an empty URL or ref (turning the feature on while pointing it
	// at nothing). Checked against the merged state so a single-key enable PUT is
	// still validated against the stored URL/ref.
	if merged[KeyAgentSourceEnabled] == "true" {
		if strings.TrimSpace(merged[KeyAgentSourceRepoURL]) == "" {
			return errors.New("agent_source_repo_url must be set when agent_source_enabled is true")
		}
		if strings.TrimSpace(merged[KeyAgentSourceRef]) == "" {
			return errors.New("agent_source_ref must be set when agent_source_enabled is true")
		}
	}
	return nil
}

// validateLabelListMerged enforces the cross-key structural rules on a parsed
// label list (PRD #196): no duplicate entries, at most maxLabelListLen entries,
// and no entry equal to autopilot_label or prdless_label (the workflow markers are
// never membership/eligibility content). Errors name the key to change.
func validateLabelListMerged(key string, list []string, merged map[string]string) error {
	if len(list) > maxLabelListLen {
		return fmt.Errorf("%s must have at most %d entries", key, maxLabelListLen)
	}
	seen := make(map[string]struct{}, len(list))
	for _, l := range list {
		if _, dup := seen[l]; dup {
			return fmt.Errorf("%s must not contain duplicate entries (%q)", key, l)
		}
		seen[l] = struct{}{}
		if l == merged[KeyAutopilotLabel] {
			return fmt.Errorf("%s must not contain the autopilot label %q", key, l)
		}
		if l == merged[KeyPrdlessLabel] {
			return fmt.Errorf("%s must not contain the prdless label %q", key, l)
		}
	}
	return nil
}

// containsLabel reports whether list contains target.
func containsLabel(list []string, target string) bool {
	for _, l := range list {
		if l == target {
			return true
		}
	}
	return false
}

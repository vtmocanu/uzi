// DTO types for the api client, split out of ./api.ts (issue #960) so the barrel
// module stays small. Pure data declarations only — no runtime code and NOTHING
// imported from ./api (that would reintroduce a cycle). ./api re-exports every
// name here (`export type *`) so the lib/api barrel surface is unchanged.

import type { RunKind } from "./runKind";

export interface User {
  id: string;
  email: string;
  display_name: string | null;
  is_admin: boolean;
  is_active: boolean;
  // autopilot_enabled is the per-user opt-in to unattended autopilot runs (PRD #19
  // M3). Default false; toggled from the user's own Settings page.
  autopilot_enabled: boolean;
  // judge_enabled is the per-user opt-in to run retrospectives (PRD #46). Default
  // false; the user toggles their own from Settings, an admin can force any user's.
  judge_enabled: boolean;
  // ci_autofix_enabled is the per-user opt-in to automatic CI fixes (PRD #71).
  // Default false; the user toggles their own from Settings, an admin can force any
  // user's.
  ci_autofix_enabled: boolean;
  // attribution_enabled is the per-user opt-out for AI attribution in worker commits
  // (issue #916). Default TRUE (today's behavior): when true the worker's commits keep
  // the Co-Authored-By: Claude trailer; when false it is suppressed on the user's next
  // run. Owner-only; toggled from their own Settings.
  attribution_enabled: boolean;
  // ephemeral_workers_enabled is the per-user opt-in to have the api auto-provision a
  // run-bound throwaway hosted worker when one of the user's runs is unplaceable for a
  // capability (PRD #529/#649). Default false; toggled from the Workers page. No
  // dedicated AuthContext field — it rides `user` like `judge_enabled`.
  ephemeral_workers_enabled: boolean;
  /** PRD #35: this user's DEFAULT for the usage-limit park — every run they create
   *  inherits it, including the three kinds with no start affordance at all
   *  (autopilot, ci_fix, self_improve), which is why the default exists rather than
   *  a per-start prompt. Default false; toggled from their own Settings.
   *
   *  It is a DEFAULT, not a live switch: changing it never touches a run that
   *  already exists. `Run.wait_on_limit` is the per-run value, set from this at
   *  creation and overridable on the run view — so the two disagreeing is the normal
   *  state of a user who overrode one run, not a sync bug to fix. */
  wait_on_limit: boolean;
  // Which Anthropic credential this user's RETROSPECTIVES spend (PRD #104 M4),
  // independent of what their runs spend — the point of the feature. Both null ⇒
  // unbound ⇒ their default token. The label, never the value.
  judge_anthropic_secret_id: string | null;
  judge_anthropic_secret_label: string | null;
  created_at: string;
  last_login: string | null;
}

// SecretMeta is the metadata-only view of ONE stored per-user secret. The secret
// value is never returned by the API, so it never appears here.
//
// Since PRD #104 a user may hold several tokens of one kind, so this carries the
// id (without it a multi-token list is indistinguishable rows), the user-chosen
// label, and which one is the default that unbound workers spend.
export interface SecretMeta {
  id: string;
  kind: string;
  label: string;
  is_default: boolean;
  /** PRD #111 M2: the owner's opt-in to the auto-selection pool — an `auto` worker
   *  spends only tokens flagged here. Default false; opting in is deliberate.
   *
   *  This is the SETTING, not the live answer. Whether the selector could actually
   *  pick the token right now also depends on its rate-limit reading, which arrives
   *  as `auto_status` on TokenRateLimits and is computed server-side. */
  auto_eligible: boolean;
  created_at: string;
  updated_at: string;
}

// UserSettings is the current user's own (non-secret) settings. default_model
// is the per-user default worker model; null means inherit (PRD #17). theme is
// the per-user UI theme override; null means "use the instance default" (PRD
// #21).
export interface UserSettings {
  default_model: string | null;
  /** Per-user default reasoning effort (PRD #617); null means inherit — the worker
   *  omits the SDK effort key, so the SDK default (`high`) applies. One of
   *  low|medium|high|xhigh|max when set. */
  default_effort: string | null;
  /** Per-user judge model override (PRD #69 M2); null means inherit the instance
   *  judge_model (which itself falls back to opus). Written through PUT /me/settings
   *  alongside default_model, validated by the same model rules. */
  judge_model: string | null;
  /** Per-user run-summary model override (PRD #362 M2); null means inherit the
   *  instance summary_model (which itself falls back to haiku). Written through
   *  PUT /me/settings alongside judge_model, validated by the same model rules. */
  summary_model: string | null;
  theme: string | null;
  /** Ids of NON-default tokens whose rate meters the user also wants on the
   *  sidebar rail. The default token always shows and is never listed here.
   *  Absent (older server) reads as []: default-only, the pre-feature look. */
  sidebar_token_ids?: string[];
  /** Per-user opt-in for the MR review watcher (PRD #700 M5/M6); default ON.
   *  null/absent reads as enabled (the default-ON state); an explicit false means
   *  the user opted this account out, so the watcher stops auto-reworking their MRs.
   *  The admin global kill-switch is separate. */
  mr_rework_enabled?: boolean | null;
}

// UserSettingsPatch is the PATCH-like body of PUT /me/settings: a field present
// is applied (null clears it), a field absent is left unchanged — so the model
// card and the Appearance picker save independently over the one endpoint.
export interface UserSettingsPatch {
  default_model?: string | null;
  /** Per-user reasoning effort (PRD #617); present-null clears back to inherit. */
  default_effort?: string | null;
  /** Per-user judge model (PRD #69 M2); present-null clears back to inherit. */
  judge_model?: string | null;
  /** Per-user run-summary model (PRD #362 M2); present-null clears back to inherit. */
  summary_model?: string | null;
  theme?: string | null;
  /** Replaces the whole sidebar-token set (null clears it); absent leaves it. */
  sidebar_token_ids?: string[] | null;
  /** Per-user MR-review-watcher opt-in (PRD #700 M6); present-false opts out,
   *  present-true (or null clearing back to the default-ON) re-enables. */
  mr_rework_enabled?: boolean | null;
}

// AgentTemplateScope mirrors the skill scopes (PRD #18 M6): builtin (shipped),
// global (admin, visible to all), user (self-service, owner-visible).
export type AgentTemplateScope = "builtin" | "global" | "user";

// SlackLink is the current user's own Slack linking state (PRD #25 M3), for the
// Settings → Notifications section. state is derived: unlinked (no resolved id) |
// pending (resolved, awaiting the Confirm DM) | confirmed. member_id is the manual
// override (null = rely on email auto-match); resolved_id is the effective linked
// Slack id (the override, else the cached email match). workspace is the
// server-derived Slack workspace connection state for this uzi instance (PRD #56):
// unconfigured (no Slack set up) | connecting (reconnecting) | connected | error.
export interface SlackLink {
  member_id: string | null;
  notify: boolean;
  resolved_id: string | null;
  confirmed: boolean;
  state: "unlinked" | "pending" | "confirmed";
  workspace: "unconfigured" | "connecting" | "connected" | "error";
}

// AgentTemplate is a stored agent definition. tools is null when the template
// inherits all tools; model is null when it inherits the model. scope/user_id
// carry the M6 ownership model; is_builtin is retained (== scope 'builtin').
export interface AgentTemplate {
  id: string;
  name: string;
  description: string;
  model: string | null;
  tools: string[] | null;
  prompt_body: string;
  is_builtin: boolean;
  scope: AgentTemplateScope;
  user_id: string | null;
  updated_by: string | null;
  // differs_from_builtin is computed server-side per request (issue #201 M4a):
  // whether this row's four mutable columns still match the definition the
  // running release ships under the same name. It is false for anything that has
  // no shipped counterpart — a global row, a user row that merely shares a
  // builtin's name, and a builtin this release no longer ships.
  //
  // NON-OPTIONAL ON PURPOSE. With a `?` every mock literal stays silent and the
  // badge never renders in mock mode, which is exactly the blindness the web-ux
  // pass would then be unable to see. Declared required, typecheck names every
  // literal that has to be updated.
  differs_from_builtin: boolean;
  // origin is the scope-aware provenance of this row (PRD #602 M5): where its body
  // came from, distinct from `scope` (which is about visibility/ownership):
  //   - "embedded" — shipped with this binary, untouched (a pristine builtin);
  //   - "admin"    — a human admin edited a builtin away from the shipped default;
  //   - "synced"   — the body came from the configured agent-source repo (an
  //                  overridden builtin, scope='builtin', OR a synced-only role,
  //                  scope='global' with no embedded counterpart);
  //   - null       — no provenance applies (a plain admin `global`, a `user` row).
  // Optional for api/web rollout skew: a pre-#602 server omits the key. An absent
  // value reads as "embedded" for a builtin (see templateOrigin) so a missing field
  // never turns a shipped default into a synced/admin badge.
  origin?: "embedded" | "synced" | "admin" | null;
  created_at: string;
  updated_at: string;
}

// BuiltinDefinition is the definition THIS BINARY ships for a builtin template
// (GET /agent-templates/{id}/builtin, issue #201 M4a) — the shipped side of the
// drift comparison, served so the editor can show a diff BEFORE Reset overwrites
// the row. Null semantics match AgentTemplate's: model null = inherit, tools null
// = inherit all. It carries no id, scope or timestamps: it lives in the binary,
// not in a table.
export interface BuiltinDefinition {
  name: string;
  description: string;
  model: string | null;
  tools: string[] | null;
  prompt_body: string;
}

// AgentTemplateInput is the create/edit shape. name and scope are only sent on
// create (both immutable afterwards); scope is "global" (admin) or "user"
// (owner) — "builtin" is never creatable via the API. A blank/absent scope
// defaults to global server-side (the pre-M6 admin create).
export interface AgentTemplateInput {
  name?: string;
  description: string;
  model: string | null;
  tools: string[] | null;
  prompt_body: string;
  scope?: "global" | "user";
}

// TemplateAllocation is one template in the caller's allocation view (PRD #18
// M7): whether it is a global default, the caller's own overlay (null = none),
// and the resolved effective decision (overlay wins, else the global default).
export interface TemplateAllocation {
  id: string;
  name: string;
  description: string;
  scope: AgentTemplateScope;
  is_builtin: boolean;
  global_default: boolean;
  my_override: boolean | null;
  effective: boolean;
}

// TemplateAllocationsInput is the replace-set write. Each half is optional: an
// omitted half is left untouched. global_default_ids is admin-only (the shared
// default set); my_overrides is the caller's own overlay.
export interface TemplateAllocationsInput {
  global_default_ids?: string[];
  my_overrides?: { template_id: string; enabled: boolean }[];
}

// ── Agent skills (PRD #16) ────────────────────────────────────────────────

export type SkillScope = "builtin" | "global" | "user";

// Skill is a stored SKILL.md playbook. body is the markdown content (returned,
// unlike a secret — it is user-authored and editable). user_id is set only for
// scope "user"; updated_by tracks the last editor (null on a pristine builtin).
export interface Skill {
  id: string;
  name: string;
  description: string;
  body: string;
  scope: SkillScope;
  user_id: string | null;
  updated_by: string | null;
  created_at: string;
  updated_at: string;
}

// SkillCreateInput is the create body. name and scope are set once (both
// immutable afterwards); scope is "global" (admin) or "user" (owner) — "builtin"
// is never creatable via the API.
export interface SkillCreateInput {
  name: string;
  description: string;
  body: string;
  scope: "global" | "user";
}

// SkillUpdateInput is the edit body: only description and body are mutable.
export interface SkillUpdateInput {
  description: string;
  body: string;
}

// AllocatedSkill is one skill allocated to an agent template, in the caller's
// view (no body — the allocation view lists what is attached, not its content).
export interface AllocatedSkill {
  skill_id: string;
  name: string;
  description: string;
  scope: SkillScope;
}

// TemplateSkills splits a template's allocations the caller may see into the
// shared (admin-managed) half and the caller's own overlay half. The union of
// the two is what the caller's runs on this template actually receive.
export interface TemplateSkills {
  shared: AllocatedSkill[];
  mine: AllocatedSkill[];
}

// AllocationsInput is the replace-set write. Each half is optional: an omitted
// (undefined) half is left untouched; a provided array fully replaces that half.
// shared is admin-only; mine is any user's own overlay.
export interface AllocationsInput {
  shared_skill_ids?: string[];
  my_skill_ids?: string[];
}

// Privilege report (PRD #5): the token-level and per-repo least-privilege
// findings the checker produced. status is the denormalized worst-case tier.
export type PrivilegeStatus = "ok" | "warnings" | "violations" | "error";

export interface PrivilegeTokenReport {
  scopes: string[];
  active: boolean;
  expires_at?: string;
  violations: string[];
  warnings: string[];
}

// A per-repo finding's severity (PRD #66 D5/D6). "block" findings mean the bot can
// reach the default branch (or uzi could not tell — fail closed); "warn" are
// advisory. Mirrors privcheck.Severity's json values exactly.
export type PrivilegeSeverity = "block" | "warn";

// One coded per-repo finding (PRD #66 D5). code is the stable enum
// (default_branch_unprotected, write_role_can_push, …); severity comes from the
// server's single findingSeverity table; message is human copy. Mirrors
// privcheck.Finding's json tags (code, severity, message) exactly.
export interface PrivilegeFinding {
  code: string;
  severity: PrivilegeSeverity;
  message: string;
}

export interface PrivilegeRepoReport {
  repo_id: string;
  path: string;
  // The bot's role on the repo as the neutral forge.Role enum
  // (none|read|write|admin|owner), PRD #65 D7. Was a raw GitLab access level
  // (number); it is now a string because Forgejo has no numeric levels and mapping
  // write→30 would be a driver lying to satisfy a GitLab-shaped contract. Display
  // only — the checker asserts role against RoleWrite server-side and renders any
  // violation copy itself, so the web never compares this numerically.
  role: string;
  member: boolean;
  // Coded per-repo findings (PRD #66 D5), replacing the old free-text
  // violations/warnings string slices. Always present (server serializes [] not
  // null); split by severity at the call site.
  findings: PrivilegeFinding[];
}

export interface PrivilegeReport {
  checked_at: string;
  token: PrivilegeTokenReport;
  repos: PrivilegeRepoReport[];
  status: PrivilegeStatus;
}

export interface ForgeConnection {
  id: string;
  forge_type: string;
  base_url: string;
  bot_username: string;
  bot_forge_user_id: number;
  // human_username is the owning user's own forge account, used for autopilot
  // attribution (PRD #19 M3). Null until the user declares it.
  human_username: string | null;
  created_at: string;
  last_verified_at: string | null;
  // Least-privilege surfacing. A null status means never checked (unchecked
  // badge, never a tick); the report is null until the first check.
  privilege_status: PrivilegeStatus | null;
  privilege_checked_at: string | null;
  privilege_report: PrivilegeReport | null;
}

// PipelineStatus is a watched ref's latest CI pipeline (PRD #6), or null on a DTO
// when the ref has no CI or has not been synced yet. status is the raw GitLab
// pipeline status; the web layer collapses it to a badge tone (pipelineBadge.ts).
// web_url links to the pipeline on the forge; synced_at drives badge staleness.
export interface PipelineStatus {
  /** The watched ref this pipeline is for (default branch or an agent branch) —
   *  what the Fix CI trigger POSTs to fix it (PRD #6). */
  ref: string;
  status: string;
  web_url: string;
  pipeline_id: number;
  synced_at: string;
}

export interface Repo {
  id: string;
  connection_id: string;
  forge_project_id: number;
  path_with_namespace: string;
  web_url: string;
  default_branch: string | null;
  enabled: boolean;
  // Repo-skills opt-in (PRD #16): when true, a run on this repo also loads
  // skills from the repo's own .claude/skills/ (skills only, never hooks/
  // settings/commands). Default false.
  repo_skills_enabled: boolean;
  // Repo-instructions opt-in (PRD #246): when true, the lead reads the repo's
  // root CLAUDE.md as nonce-fenced, advisory, lead-only context. It is the second
  // capability behind the "Trusted repo" affordance, independently revocable from
  // repo_skills_enabled. Default false.
  repo_claudemd_enabled: boolean;
  // Tier-2 opt-in (PRD #18 M5): when true, a run on this repo also unions the
  // packages from the repo's own devbox.json (packages-only). Default false.
  repo_devbox_opt_in: boolean;
  // Per-repo self-improve dogfooding capability (PRD #686): when true, a
  // scheduled self-improvement run on this repo folds the owner's improve_uzi
  // judge backlog into the cycle and selects the uzi-specific worker directive.
  // Default false (a normal project gets the generic "improve this project" run).
  repo_fold_improve_uzi_backlog: boolean;
  // Static per-repo capability hint (PRD #84 M2): a server-owned subset of the
  // capability vocabulary ({docker, jvm}). A run on this repo inherits these as its
  // required_capabilities, so the scheduler routes it only to a worker that has them.
  // The server capability.Filters the list, so only valid names persist. Default [].
  required_capabilities?: string[];
  // Default-branch CI status (PRD #6), null when there is no cached default-branch
  // pipeline (no CI, MR-only pipelines, or not yet synced).
  pipeline: PipelineStatus | null;
  // Admin per-repo guardrail override metadata (PRD #66 D8), null when no override
  // is active. Display-only surfacing shipped by M8 so M9 can render the badge; M8
  // itself adds no UI control. Shares the GuardrailOverrideMeta shape with BlockedRepo.
  guardrail_override?: GuardrailOverrideMeta | null;
  // Server-computed "would a run be refused on this repo right now" (PRD #66 M9,
  // D8): the stored findings run through the single shared Go downgrade + Blocks(),
  // with the override already applied. The badge STATE reads THIS boolean and never
  // re-derives the waivable set. False on a never-checked connection is "unknown,
  // not safe" — the enable/run gates still fail closed server-side.
  guardrail_blocked: boolean;
  // Computed, caller-scoped "is this repo on the global Docker-worker allowlist"
  // (PRD #361): a boolean about the caller's own repo, never the list. Set by the
  // list handlers, like guardrail_blocked. Drives the Repos-page Setup chip.
  docker_allowlisted: boolean;
  // Computed, caller-scoped "is a run on this repo actually blocked by the Docker-
  // allowlist gap right now" (PRD #361): enabled repo, a queued run, ≥1 online worker,
  // and zero eligible online workers. Drives the Setup chip's info escalation; computed
  // from eligibility, not the sweeper's health text. Set by the list handlers.
  docker_blocked: boolean;
  // Caller-scoped GitHub Projects v2 sync-health summary (PRD #576 M2), null/absent
  // when the repo is not linked. Drives the Sync badge tone: linked && healthy → green
  // (ok), linked && !healthy (last_error set) → danger, absent → neutral. Derived from
  // the github_project_links row (last_error/last_synced_at); set by the list handlers.
  github_project_sync?: {
    linked: boolean;
    healthy: boolean;
    last_error?: string;
    last_synced_at?: string;
  } | null;
}

// GuardrailOverrideMeta is the audit metadata for an active admin per-repo guardrail
// override (PRD #66 D8): the reason, the actor (email when resolvable, else the raw
// id), and when it was set.
export interface GuardrailOverrideMeta {
  reason: string;
  by: string;
  at: string;
}

// GitHub Projects v2 sync status for one repo (PRD #534). Returned by the
// per-repo status endpoint when the repo is linked; a 404 means "not linked"
// (indistinguishable from a non-owner probe — existence-hiding is intentional).
export interface ProjectSyncStatus {
  project_number: number;
  owned_by_uzi: boolean;
  last_synced_at: string | null;
  last_error: string | null;
  item_count: number;
  // Board columns with no matching Status option at the last adopt/resync (PRD #576
  // M3): the panel surfaces them with a Resync prompt. Always an array from the server
  // (never null); optional here so a pre-M3 fixture without the field still typechecks.
  unmatched_columns?: string[];
  // True when the synced Status field has no "Done" option, so closed issues cannot show
  // a Done status (PRD #584 M4). Optional so a pre-M4 fixture without the field typechecks.
  no_done_option?: boolean;
}
// Which GitHub owner a provision/adopt targets (PRD #534): the connecting user,
// an org, or the token's own viewer. "user" is the default.
export type ProjectSyncOwnerKind = "user" | "org" | "viewer";

// BlockedRepo is one row of the admin cross-user blocked-repos list (PRD #66 M9,
// D8): a repo that is blocked by the guardrail OR carries an active admin override.
export interface BlockedRepo {
  id: string;
  path: string;
  owner_id: string;
  owner_email: string;
  forge_type: string;
  // Blocked is the stored-report equivalent of Repo.guardrail_blocked. An
  // overridden-clean repo reads false but still appears; an overridden repo whose
  // only finding is protection_unreadable reads true (the override never waives it).
  blocked: boolean;
  // Human messages of the block findings; empty for an overridden-clean repo.
  block_messages: string[];
  guardrail_override: GuardrailOverrideMeta | null;
  // The owning connection's last privilege-check state; null when never checked.
  privilege_status: string | null;
  privilege_checked_at: string | null;
}

// AdminBlockedRepos is the GET /api/admin/blocked-repos envelope (PRD #66 M9). When
// checks_unknown is true at least one connection was never privilege-checked, so an
// empty list is "unknown", NOT "none blocked" (R1) — the page says so.
export interface AdminBlockedRepos {
  repos: BlockedRepo[];
  checks_unknown: boolean;
}

export interface BoardColumn {
  label_name: string;
  position: number;
}

// Tool allowlist entry (PRD #18 M4): an admin-permitted package. pinned_version,
// when set, requires the profile to request exactly that version.
export interface ToolAllowlistEntry {
  id: string;
  name: string;
  pinned_version: string | null;
  note: string | null;
  updated_by: string | null;
  created_at: string;
  updated_at: string;
}

export interface ToolAllowlistWriteInput {
  name?: string; // create only; ignored on update
  pinned_version?: string;
  note?: string;
}

// LatestRun is the newest run for a card's issue (PRD #12 M2), or null when the
// issue has never run. Display-only: no secrets. is_mine gates the in-app run-view
// link (a non-owner would 403 on the run); run_count drives the "×N" retry hint.
export interface LatestRun {
  id: string;
  status: RunStatus;
  mr_iid: number | null;
  // Forge-supplied MR/PR web URL persisted by the worker at creation (PRD #65 D8),
  // null on runs created before it landed. Rendered directly through isHttpsUrl; a
  // null falls back to the legacy GitLab URL reconstruction (forgeUrls.ts). It is
  // the only correct link on Forgejo, whose PR URL grammar differs from GitLab's.
  mr_web_url: string | null;
  // Last merge-request state the PRD #24 watcher observed for mr_iid
  // (opened|closed|merged|locked), null when never observed. Display-only hint
  // (PRD #33): mrChipState maps it to the chip variant. Kept fresh only for the
  // board card (the issue's latest run); a superseded run's value can be stale.
  mr_state: string | null;
  failure_reason: string | null;
  // Server-stamped stop signal (PRD #33, widened by #108 M5); null for every
  // non-stop run. Read by isStoppedRun, which renders the two HUMAN kinds as a calm
  // "stopped" and deliberately leaves "auto_stopped" looking like the breakage it is.
  stop_kind: StopKind | null;
  // issue #525: the operator's OPTIONAL free-text cancel reason. Owner-gated on the
  // shared board (the server sends it only to the run's owner, like failure_reason;
  // a non-owner viewer gets null). Untrusted free text — render via stripUnsafeChars.
  stop_reason: string | null;
  // issue #321: server-computed planning-phase display flag (true only while a run is
  // in its pre-approval PLANNING turn — running, iteration 0, no persisted plan yet).
  // OPTIONAL for the SAME api/web rollout skew as plan_source: a mid-deploy api pod that
  // predates the field omits the key, and an absent value reads as not-planning
  // (isPlanningRun requires `=== true`). Derived, not a real runs.status value.
  is_planning?: boolean;
  // issue #750: server-computed plan-revise display flag (true while the run's latest
  // {plan, plan_revising} message is a plan_revising — a "revise" replan in flight). The
  // server does NOT status-gate it, so the client combines it with status
  // (isRevisingRun requires status === "awaiting_approval" AND `=== true`). OPTIONAL for
  // the SAME rollout skew as is_planning: a pre-feature api pod omits the key, and an
  // absent value reads as not-revising. Derived, not a real runs.status value.
  is_revising?: boolean;
  // Run-health flag (PRD #47). health + health_since are non-sensitive (like
  // stop_kind) and always present. health_reason can name owner state ("your vault
  // is locked"), so the server sends it only to the run's owner (is_mine); a
  // non-owner viewer of a shared board gets null. runBadge shows the warn variant.
  health: RunHealth;
  health_reason: string | null;
  health_since: string | null;
  owner_name: string;
  worker_name: string | null;
  is_mine: boolean;
  run_count: number;
  created_at: string;
  updated_at: string;
}

export interface Card {
  iid: number;
  title: string;
  state: string;
  labels: string[];
  // Forge user ids assigned to the issue (PRD #767 M5). The board widens "is this card
  // uzi's to run" to "carries the `uzi` label OR the board's bot is one of these ids",
  // so this rides the card alongside labels. A current server always sends [] not null,
  // but the field is optional so a payload from an OLD api replica during a rollout skew
  // (a new web bootstrap reading an old card DTO that predates this field) does not throw
  // in the consumer — a missing value is treated as "no assignees".
  assignee_ids?: number[];
  web_url: string;
  // The card's forge ("gitlab"|"forgejo"|"github"), so the UI picks the per-card MR/PR noun
  // (PRD #65 D2). A cross-repo view mixes forges, so it rides each card.
  forge_type: string;
  author: string | null;
  has_prd_link: boolean;
  column: string;
  closed: boolean;
  conflict: boolean;
  // The issue's forge-side updated_at (RFC3339), for the board's "Last updated" sort
  // mode (PRD #102 M5). Always present: the column is NOT NULL server-side. Compare it
  // with Date.parse, never as a string — Go trims trailing zeros from RFC3339
  // fractional seconds, so "…T10:00:00Z" and "…T10:00:00.5Z" do not sort correctly
  // lexicographically.
  forge_updated_at: string;
  latest_run: LatestRun | null;
  // CI status of the card's most-recent run's branch (PRD #6), null when that run
  // has no branch, no CI, or the card has never run. Drives the per-card badge and
  // the Fix CI affordance.
  pipeline: PipelineStatus | null;
}

export interface Board {
  repo_id: string;
  path_with_namespace: string;
  web_url: string;
  // The board's forge ("gitlab"|"forgejo"|"github"), so board-level chrome (the "columns are
  // <forge> labels" hint, the create-issue "opened on <forge>" note) names the right
  // platform (PRD #65 D2). A board is one repo/connection, so it is a single value.
  forge_type: string;
  columns: BoardColumn[];
  cards: Card[];
  // Repo default-branch CI status (PRD #6, the board header badge), null when
  // there is no cached default-branch pipeline.
  pipeline: PipelineStatus | null;
  // The board's single connection's bot forge user id (PRD #767 M5). A card is
  // runnable when it carries the `uzi` label OR this id is one of its assignee_ids.
  // Per-connection (a user may have several connections with different bot ids), so it
  // rides the board, not the user session. 0 when unresolved (never marks a card).
  // Optional: an OLD api replica during a rollout skew may omit it entirely, which the
  // consumer treats as "no bot" (0), the same as unresolved.
  bot_forge_user_id?: number;
}

// BoardPrefs is the current user's per-repo board view preferences (PRD #196 M3),
// persisted server-side (per account, per repo) rather than per browser. It is the
// stored row served by GET/PUT /repos/{id}/board/prefs.
//
// show_all is the per-account "show all other issues" boolean that drives the board's
// uzi-only / all toggle (PRD #764). extra_labels is retained on the wire (a nullable
// sentinel: null = not customised, an array = the user's absolute set) but is no longer
// consumed by the client under the single-`uzi`-label membership model — it is only
// round-tripped so the stored row stays intact. No row yet reads as
// { extra_labels: null, show_all: false }.
export interface BoardPrefs {
  extra_labels: string[] | null;
  show_all: boolean;
}

// IssueDetail is the in-app issue view payload (PRD #12 §3): the board card
// fields plus the issue description (rendered as markdown; it carries the PRD
// link). Fetched live from the forge, so unlike a board card it has no latest_run
// — the issue view shows full run history from a separate listRuns call instead.
export interface IssueDetail {
  iid: number;
  title: string;
  state: string;
  labels: string[];
  // Forge user ids assigned to the issue (PRD #767 M5), fresh from the live forge
  // fetch. The issue view evaluates the same "carries `uzi` OR assigned to the bot"
  // runnable predicate the board does. A current server always sends [] not null,
  // but the field is optional (matching Card.assignee_ids) so a payload from an OLD
  // api replica during a rollout skew that omits it does not throw in the consumer —
  // a missing value is treated as "no assignees".
  assignee_ids?: number[];
  web_url: string;
  author: string | null;
  has_prd_link: boolean;
  column: string;
  closed: boolean;
  conflict: boolean;
  description: string;
  // The issue's forge ("gitlab"|"forgejo"|"github"), so the "Open on <forge>" button names
  // the right platform (PRD #65 D2).
  forge_type: string;
  // The repo's connection's bot forge user id (PRD #767 M5), so the issue view can
  // evaluate assignment-eligibility with the same predicate as the board. Per-connection.
  // Optional (matching Board.bot_forge_user_id): an OLD api replica during a rollout
  // skew may omit it entirely, which the consumer treats as "no bot" (0), the same as
  // unresolved.
  bot_forge_user_id?: number;
}

export interface ForgeConfig {
  allowed_base_urls: string[];
  forge_types: string[];
}

// AppSettings is the instance-level settings surface (PRD #19). Admin-only. The
// API always returns every known key (a missing row reads as its default), so
// every field is always present. default_theme is the instance-default UI theme
// (PRD #21).
export interface AppSettings {
  autopilot_label: string;
  // PRD #764: the single run-eligibility label (default "uzi"). An issue is runnable
  // iff it carries this label; served as a raw string like every other setting.
  uzi_label: string;
  default_theme: string;
  // Slack integration non-secret keys (PRD #25). slack_enabled is the text
  // "true"/"false"; public_base_url is the http(s) base for deep links in Slack
  // messages. The two Slack TOKENS are secret and never returned here — see
  // `secrets` on SettingsResponse.
  slack_enabled: string;
  public_base_url: string;
  // Run-judge keys (PRD #46). judge_enabled is the global kill-switch (text
  // "true"/"false"); judge_model is the model alias the judge runs on (opus by
  // default, PRD #69).
  judge_enabled: string;
  judge_model: string;
  // judge_enforce_all (PRD #69) is the text "true"/"false": when on, EVERY user's
  // finished runs are judged on that user's own token, bypassing the per-user opt-in
  // — but the kill-switch (judge_enabled) still dominates. judge_cooldown_seconds and
  // judge_daily_budget are the per-user spend guards, integer seconds / count as
  // strings (the API serves every setting as a string); 0 disables each guard.
  judge_enforce_all: string;
  judge_cooldown_seconds: string;
  judge_daily_budget: string;
  // Ephemeral worker auto-provisioning instance kill-switch (PRD #529 / #649 M1).
  // The text "true"/"false" (default "false"). When OFF, no run ever auto-provisions
  // a throwaway hosted worker regardless of a user's per-account opt-in; when ON,
  // users can still individually opt in on the Workers page. Web-only surfacing — the
  // key already round-trips through GET/PUT /admin/settings.
  ephemeral_workers_enabled: string;
  // Upstream release-check toggles (PRD #836). Both the text "true"/"false" (default
  // "true"), round-tripping through GET/PUT /admin/settings like every other setting.
  // release_check_enabled is the master air-gap switch: when off, the api never calls
  // github.com. release_check_banner_enabled governs only the intrusive escalation
  // banner (M6); the pip and the admin Updates card do not depend on it. The Updates
  // card reads the LIVE values off the ReleaseCheckStatus DTO and writes them back
  // here through updateSettings (string-space), the same shape as the toggles above.
  release_check_enabled: string;
  release_check_banner_enabled: string;
  // Run-summary model (PRD #362 Decision 8): the model alias the inline run-summary
  // generator runs on (haiku by default), served as a raw string like every other
  // setting. Mirrors judge_model's admin machinery but delivers on the issue-run claim.
  summary_model: string;
  // Run-health detector keys (PRD #47). health_enabled is the text "true"/"false";
  // the rest are integer seconds as strings (the API serves every setting as a
  // string). 0 disables that one signal.
  health_enabled: string;
  health_stall_seconds: string;
  health_slow_seconds: string;
  health_queued_seconds: string;
  health_approval_seconds: string;
  health_nudge_cooldown_seconds: string;
  // Docker-worker repo allowlist (PRD #89 M-allow): a comma-separated list of repo
  // ids (UUIDs). A docker-capable worker may only claim runs for repos on this list;
  // empty is fail-closed (a docker worker then claims no repo-bearing run). Non-docker
  // workers are unaffected. The admin UI edits it as a repo multiselect writing the
  // ids — admins pick paths, never paste UUIDs.
  docker_repo_allowlist: string;
  // Capability-aware scheduling kill-switch (PRD #84 M2). The text "true"/"false"
  // (default "true"). When on, a run is routed only to a worker that can run it
  // (e.g. a docker-needing run only to a docker worker). Turning it OFF reverts to
  // best-effort claiming; it does NOT disable the docker repo allowlist.
  capability_aware_scheduling: string;
  // GitHub Projects v2 sync instance kill-switch (PRD #534 / issue #534 M2). The
  // text "true"/"false" (default "false"). When OFF, no run mirrors board-column
  // labels to a linked GitHub Projects Status field — an instance-wide rate-limit /
  // cost lever. GitLab and Forgejo repos are unaffected either way.
  github_project_sync_enabled: string;
  // Instance branding config (PRD #685). All six round-trip through GET/PUT
  // /admin/settings as raw strings like every other setting — the API serves the
  // whole settings surface as strings, so app_logo_keep_name/brand_plaque are the
  // text "true"/"false" HERE (string-space). The Admin → Branding page edits them
  // through getSettings/updateSettings in this same string-space; the public
  // /api/branding read below re-types them as bools for the chrome (bool-space).
  // Logo BYTES are never settings keys (Decision D7) — they live in branding_assets
  // and move through the dedicated upload/delete endpoints below.
  app_logo_mode: string; // "default" | "custom" | "preset"
  app_logo_preset: string; // "" or a brandPresets slug
  app_logo_keep_name: string; // "true" | "false"
  brand_mode: string; // "none" | "text" | "logo"
  brand_company: string; // free text, ≤ 64 runes, may be ""
  brand_placement: string; // "below" | "topright"
  brand_plaque: string; // "true" | "false"
}

// Branding is the public GET /api/branding shape (PRD #685): the same six config
// keys as AppSettings PLUS the two derived presence flags, but re-TYPED for the
// chrome. This is the bool-space half of the string↔bool split: the admin page
// works in string-space (AppSettings via getSettings/updateSettings), while the
// chrome consumes THIS, where app_logo_keep_name/brand_plaque and the two
// *_present flags are real booleans (the Go handler coerces "true"/"false" and
// derives *_present from branding_assets row existence). Logo bytes are NOT here
// (Decision D7) — presence is a bool; the image itself loads from
// /api/branding/logo/{slot}.
export interface Branding {
  app_logo_mode: string; // "default" | "custom" | "preset"
  app_logo_preset: string; // "" or a brandPresets slug
  app_logo_present: boolean;
  app_logo_keep_name: boolean;
  brand_mode: string; // "none" | "text" | "logo"
  brand_company: string;
  brand_placement: string; // "below" | "topright"
  brand_plaque: boolean;
  brand_logo_present: boolean;
}

// SettingSource reports where a setting's effective value comes from (PRD #25):
// an env var, the DB app_settings row, or the compiled-in default. An env-sourced
// key is greyed in the admin UI and a PUT to it is rejected (409).
export type SettingSource = "env" | "db" | "default";

// SettingsResponse is the admin GET/PUT body (PRD #25). `settings` carries the
// non-secret effective values; `secrets` reports, per secret key, whether a value
// is configured (never the value itself); `sources` reports every key's source.
export interface SettingsResponse {
  settings: AppSettings;
  secrets: Record<string, boolean>;
  sources: Record<string, SettingSource>;
  // Live Slack socket connection state (PRD #25 M2): "disabled" | "connecting" |
  // "connected" | "error:<class>". The admin Slack card renders it as a chip.
  slack_status: string;
  // OIDC SSO health (PRD #45, Nit6): "disabled" | "ok" | "degraded" (configured but
  // discovery is failing). oidc_provider_name is the button label. Optional so an
  // older server omits them.
  oidc_status?: string;
  oidc_provider_name?: string;
}

// UpdateSettingsPayload extends the non-secret settings with the write-only
// secret token fields, sent only when the admin enters a new value (an omitted or
// empty token leaves the stored one unchanged).
export type UpdateSettingsPayload = Partial<AppSettings> & {
  slack_bot_token?: string;
  slack_app_token?: string;
  // Agent-source config (PRD #602 M5) is written through this same generic PUT —
  // there is no dedicated config-write route (that would bypass the SSRF-allowlist
  // gate the generic PUT enforces). Every value is a string like the rest of the
  // settings surface; `agent_source_enabled` is the text "true"/"false". The
  // credential is a WRITE-ONLY secret: send it only when setting or replacing it;
  // an omitted (or empty) value leaves the stored credential unchanged, exactly
  // like the Slack tokens above.
  agent_source_repo_url?: string;
  agent_source_ref?: string;
  // Repo-relative subfolder role files are read from (PRD #702 M1); empty/unset
  // resolves to the default ".claude/agents" server-side.
  agent_source_folder?: string;
  agent_source_enabled?: string;
  agent_source_interval?: string;
  agent_source_credential?: string;
};

// ── Agent source (PRD #602 M5) ────────────────────────────────────────────
// The admin surface for the configurable agent-source repo: config + last-sync
// status + a STAGED snapshot the admin reviews and approves. Nothing a sync
// fetches reaches a run until Approve. These types mirror the Go DTO in
// api/internal/handler/agent_source.go byte-for-byte (snake_case JSON tags).

// AgentSourceConfig is the editable config half. `credential_configured` is the
// only thing the API says about the private-repo credential — its value is never
// returned (write-only, set through updateSettings). `interval` is a Go duration
// string; `enabled` is a real bool here (the DTO decodes the "true"/"false" setting).
export interface AgentSourceConfig {
  url: string;
  ref: string;
  // Repo-relative subfolder role files are read from (PRD #702 M1); the server
  // resolves empty/unset to ".claude/agents", so this is always populated on read.
  folder: string;
  enabled: boolean;
  interval: string;
  credential_configured: boolean;
}

// AgentSourceStatus is the engine-recorded last-sync / last-apply state. Every
// field is optional: a never-synced install has none of them (the server omits
// empties). `last_sync_status` is "ok" | "error"; `last_sync_error` is the
// PAT-scrubbed, TTY-sanitized failure message. `counts` is the last sync's
// staged/changed/failed tally.
export interface AgentSourceStatus {
  last_sync_at?: string;
  last_sync_sha?: string;
  last_sync_status?: string;
  last_sync_error?: string;
  last_applied_at?: string;
  last_applied_sha?: string;
  counts?: AgentSourceCounts;
  // Update-availability signal (PRD #702 M4), DERIVED server-side at read time from
  // the persisted remote facts + the live config, so a pin bump or apply self-clears
  // it with no new egress. `update_available` is the derived boolean; `latest_ref` is
  // the newer semver tag when a tag-pinned update exists (naming it), ABSENT/empty for
  // a branch "moved" signal. `update_checked_at` (RFC3339) is when the last update
  // check ran, so the admin knows how fresh the signal is. The field is always present
  // once a check has run; optional-typed covers the never-checked install.
  update_available?: boolean;
  latest_ref?: string;
  update_checked_at?: string;
}

export interface AgentSourceCounts {
  staged: number;
  changed: number;
  failed: number;
}

// AgentSourceStagedRole is one parsed role in the staged snapshot. `ok` false is a
// skipped/failed role, with `reason` carrying the skip code (invalid, tools_all_denied,
// duplicate, too_large, …). `prompt_body` is ALREADY server-sanitized (termsafe) — it
// is rendered as a plain React text node, NEVER through dangerouslySetInnerHTML.
export interface AgentSourceStagedRole {
  name: string;
  ok: boolean;
  reason?: string;
  description?: string;
  model?: string;
  tools?: string[];
  prompt_body?: string;
  // True when the displayed `prompt_body` differs from the raw applied body because
  // control/bidi/format characters were stripped for display (termsafe). The preview
  // then under-represents the raw body, so the review surface flags it.
  body_sanitized?: boolean;
  notes?: string[];
}

// AgentSourceDiffEntry is one per-name classification of what applying this snapshot
// would do. `action` is the closed set add | override | conflict | unchanged | remove
// (see agentSourceActionMeta in AdminSettings for the display mapping).
export interface AgentSourceDiffEntry {
  name: string;
  action: string;
  detail?: string;
}

// AgentSourceStaged is the staged snapshot itself. `pending` is true while this
// snapshot has NOT been applied (its `fetched_sha` differs from last_applied_sha);
// it is the signal that an Approve is outstanding.
export interface AgentSourceStaged {
  fetched_at?: string;
  fetched_sha: string;
  source_url: string;
  source_ref: string;
  roles: AgentSourceStagedRole[];
  diff: AgentSourceDiffEntry[];
  counts: AgentSourceCounts;
  pending: boolean;
}

// AgentSourceView is the whole GET /admin/agent-source body: config + status +
// the staged snapshot (null when nothing has ever been staged).
export interface AgentSourceView {
  config: AgentSourceConfig;
  status: AgentSourceStatus;
  staged: AgentSourceStaged | null;
}

// AgentSourceApplyResult is the POST /admin/agent-source/apply response summary
// (mirrors agentsource.ApplyResult). The card re-fetches the view after applying,
// so it reads this only for the success notice counts.
export interface AgentSourceApplyResult {
  sha: string;
  applied: number;
  unchanged: number;
  conflicts: number;
  deprovisioned: number;
  skipped_parse: number;
  already_applied: boolean;
  message: string;
}

// Compiled-in label defaults, mirroring the API's settings package. The SPA uses
// them until the session bootstrap resolves the configured values (PRD #19 M2,
// PRD #764 for the `uzi` run-eligibility label).
export const DEFAULT_AUTOPILOT_LABEL = "autopilot";

// SessionResponse is the auth/session bootstrap body (login, register, me). It
// carries the user, the instance forge labels the board and issue-creation UI
// need before their first call (PRD #19 M2, PRD #764: the single `uzi`
// run-eligibility label and the autopilot label), and the three theme fields the
// Appearance picker needs (PRD #21: resolved theme, the user's raw override with
// null = none, and the instance default).
export interface SessionResponse {
  user: User;
  // PRD #764: the single run-eligibility label. An issue is runnable iff it carries
  // this label; the board renders it as a runnable marker + filter facet.
  uzi_label: string;
  autopilot_label: string;
  theme: string;
  theme_override: string | null;
  default_theme: string;
  // Vault status (PRD #32): whether the user's per-user secret vault is unlocked
  // in the server process. Optional so a server that predates the field reads as
  // unlocked (no banner, legacy behavior) rather than falsely locked. `exists`
  // (PRD #45) is whether a vault row exists at all; with has_password it lets a
  // passwordless user's SPA pick the passphrase-create dialog vs the unlock banner.
  vault?: { unlocked: boolean; exists?: boolean };
  // has_password is false for OIDC-only users (NULL password_hash; PRD #45). Absent
  // (older server, or a password user) reads as true — no passphrase-create dialog.
  has_password?: boolean;
  // Judge consent surface (PRD #69 M4), resolved server-side so a non-admin (who
  // cannot read /admin/settings) still sees what their own token is committed to.
  // judge_enforced_by_admin is true only when the judge is ENFORCED (kill-switch on
  // AND enforce_all on) — the RunDefaults enforced banner reads it. effective_judge_model
  // is the model this user's judge actually runs on after the per-user→instance→default
  // resolution. Both optional so an older server (no enforced mode) reads as off / "".
  judge_enforced_by_admin?: boolean;
  effective_judge_model?: string;
}

// AuthConfig is the unauthenticated registration policy the register page reads
// to hide itself or hint the allowed domains before submit. The server stays
// authoritative; this is display + pre-validation only.
export interface AuthConfig {
  registration_enabled: boolean;
  allowed_email_domains: string[];
  // OIDC SSO (PRD #45). oidc_enabled reflects whether SSO is CONFIGURED (not whether
  // discovery has succeeded — the button stays visible so the lazy discovery-retry is
  // reachable when the IdP was down at boot). password_login_enabled hides the
  // password form + register when an operator goes SSO-only. All optional: an older
  // server omits them and reads as OIDC-off / password-on.
  oidc_enabled?: boolean;
  oidc_provider_name?: string;
  password_login_enabled?: boolean;
}

// BuildInfo is the unauthenticated GET /api/version response (PRD #175) — the set
// of coordinates a deployed instance can state about itself. Mirrors
// api/internal/apitypes/buildinfo.go.
//
// `version` and `founded` are always present; everything else is OMITTED when the
// build did not stamp it, never zero-valued. A `dev` build reporting commit "" and
// built_at "0001-01-01T00:00:00Z" would claim to know things it does not, so the
// optional markers here are load-bearing: the degraded shape is the COMMON case
// (every local docker-compose stack), not an edge.
//
// Nothing in this response is private — the image tag is in the chart, the commit
// is in the repo. `uptime_seconds` is the only runtime (rather than build) fact,
// and it is accepted as public; see the Version doc comment in
// api/internal/handler/handler.go, which is where that rule is enforced.
export interface BuildInfo {
  // The Model-B release coordinate (== image tag == chart appVersion), bare: the
  // Dockerfile strips a leading v and the SPA re-adds one for display. "dev" on an
  // un-stamped build. This key and value are unchanged from the single-string
  // response this type widened — WorkersSettings feeds it to PRD #113's worker
  // upgrade classification, so it gates a fleet feature.
  version: string;
  // Date of the project's first commit, YYYY-MM-DD. The AGE is computed HERE from
  // it rather than sent: two sources of truth would disagree the moment a
  // long-lived SPA session crosses midnight, and deriving it means the number stays
  // correct without a release.
  founded: string;
  // When the image was built, RFC3339 UTC. Absent on an un-stamped build.
  built_at?: string;
  // Full 40-char source SHA; consumers truncate for display so the stored value
  // stays greppable. Absent on an un-stamped build.
  commit?: string;
  // Commits in the history the image was built from (PRD #175 M3). Independently
  // droppable — every consumer must render correctly without it.
  commits?: number;
  // Completed / active PRD counts in the source tree the image was built from
  // (#245). Stamped only on a publish build, like `commits`; absent otherwise.
  prds_done?: number;
  prds_open?: number;
  // How long the process has been serving. A pointer server-side because 0 is a
  // legitimate uptime in a process's first second, so absent means UNKNOWN here and
  // must not be rendered as "up 0s".
  uptime_seconds?: number;
  // Upstream-release check (PRD #836). These mirror the api DTO fields M3 added to
  // apitypes.BuildInfoDTO and obey the same OMITTED-when-not-stamped, never
  // zero-valued convention: the server omits all three (and the nested `latest`
  // object) when the release check has not run or the feature is disabled. That
  // keeps three distinct states — absent (unknown / disabled), false (checked, up
  // to date) and true (behind) — which is why `update_available`/`far_behind` are
  // optional booleans here rather than defaulting to false. The pip and popover row
  // render ONLY from these server-derived values; the web never compares versions
  // itself (the semver trap lives server-side, in one place).
  latest?: {
    // The `v`-prefixed release tag (e.g. "v0.66.0"), already prefixed by the
    // server. displayVersion is idempotent for a leading-`v` string, so it is safe
    // to pass through without producing "vv".
    version: string;
    name?: string;
    published_at?: string;
    notes_url?: string;
    security?: boolean;
  };
  update_available?: boolean;
  far_behind?: boolean;
}

// ReleaseCheckStatus is the ADMIN release-check surface (PRD #836 M5): the response
// of both admin release-check endpoints (`GET`/`POST /admin/release-check`) and the
// data source for the admin Updates card. It mirrors `apitypes.ReleaseCheckStatusDTO`
// byte-for-byte (snake_case JSON tags). Unlike the world-readable BuildInfo.latest,
// this is served ONLY to a cookie-authenticated admin, so it carries the full
// persisted release `body` — the RAW release markdown the card excerpts (rendered as
// PLAIN TEXT, never HTML) and scans for the `### Security` heading. That field is
// admin-only and must never migrate onto an unauthenticated response.
//
// The three derived booleans are plain (never omitted) because `status`
// ("disabled" | "never" | "ok" | "error") already carries the "has a check run?"
// distinction — this endpoint always returns the complete picture. The omitempty
// facts are optional here; the required config/version/status fields are always
// present. No token is ever serialized.
export interface ReleaseCheckStatus {
  // The two runtime toggles + poll cadence, read live from settings.
  release_check_enabled: boolean;
  release_check_banner_enabled: boolean;
  interval: string;
  // This instance's own served version (bare, "dev" on an un-stamped build) — the
  // left-hand side of the update delta the card renders.
  running_version: string;
  // The persisted remote facts, omitted until a check has run. `body` is the RAW
  // release markdown (admin-only).
  latest_tag?: string;
  latest_name?: string;
  body?: string;
  notes_url?: string;
  published_at?: string;
  checked_at?: string;
  // Read-time derivations over the facts + running_version (PRD #836 M1).
  update_available: boolean;
  far_behind: boolean;
  security: boolean;
  // banner_snoozed is true iff a snooze tag is set AND equals latest_tag (PRD #836 M6):
  // the escalation banner (surface 4) stays hidden after a Dismiss. Because a newer
  // release changes latest_tag, the snooze auto-expires when a newer release arrives.
  banner_snoozed: boolean;
  // "disabled" (master toggle off) | "never" (enabled, no check yet) | "ok" (facts
  // present) | "error" (the last check failed). message carries a token-scrubbed
  // reason on error, empty otherwise.
  status: string;
  message?: string;
}

// ── CLI tokens (PRD #64) ──────────────────────────────────────────────────
// A Bearer credential the `uzi` CLI presents (sha256 at rest). scope is a
// ceiling: 'user' is the owner's own authority, 'admin_ro' reads the whole
// factory (mintable only by an admin). The token VALUE is never a field — it is
// shown once at mint (CliTokenMint below) and never returned again.
export type CliTokenScope = "user" | "admin_ro";

// CliToken is the metadata-only view of a stored CLI token. token_prefix,
// last_used_at and last_used_ip are the ENTIRE forensic surface (Risk 8): there
// is no per-request audit log and a password change does not revoke these, so
// "which token is this, and was it used by someone who isn't me?" is answerable
// only from those three. They are not optional columns. All three of
// last_used_at/last_used_ip/expires_at are null until set (a never-used or
// never-expiring token). This mirrors the server's cliTokenDTO, which lives in
// handler/cli_tokens.go (not apitypes) — no CLI verb decodes it, the SPA does.
export interface CliToken {
  id: string;
  name: string;
  token_prefix: string;
  scope: CliTokenScope;
  revoked: boolean;
  created_at: string;
  last_used_at: string | null;
  last_used_ip: string | null;
  expires_at: string | null;
}

// AdminCliToken is one row of the factory-wide standing-credential inventory
// (apitypes.AdminCLITokenDTO, admin read-only): the per-user CliToken plus the owner
// attribution — user_id and owner_email — that the `uzi admin cli-tokens` CLI list
// carries. The token value and its hash appear in no field (the value is never stored;
// the hash is projected out server-side). The web SPA does not fetch this list today
// (only the CLI's `GET /admin/cli-tokens` does); this type exists so the api⇄SPA
// contract fixture (PRD #982) can pin the admin DTO's shape against a TS definition.
export interface AdminCliToken extends CliToken {
  user_id: string;
  owner_email: string;
}

// CliTokenMint is the POST /me/cli-tokens response: the plaintext token shown
// exactly once (only its hash is stored) plus the row's metadata, mirroring
// CreateWorker's {worker, token}. `token` is the only place the value ever
// appears — copy it now or lose it.
export interface CliTokenMint {
  token: string;
  cli_token: CliToken;
}

// ── CLI browser-login consent flow (PRD #64 M5/M6) ────────────────────────────
// The `/cli-auth` consent page reads a pending request and approves/denies it.
// status mirrors the server enum; a stale-but-pending row reports "expired".
export type CliAuthStatus =
  "pending" | "approved" | "denied" | "consumed" | "expired";

// CliAuthRequestMeta is GET /api/auth/cli/request/{id}. It carries client_desc +
// status + expiry and DELIBERATELY NOT the user_code: the human must type the
// code shown in their terminal, which is the anti-async-phishing property. The
// consent page therefore renders a code input, never a pre-filled value.
export interface CliAuthRequestMeta {
  client_desc: string;
  status: CliAuthStatus;
  expires_at: string;
}

// ── Agent memory (PRD #90) ────────────────────────────────────────────────
// A durable per-(user, repo) learning an agent saved on a prior run, read back
// into a future run as inert, nonce-fenced, advisory context. The webui surfaces
// it because the owner's ability to SEE and PURGE a (possibly poisoned) entry is
// a security control, not a nicety: a bad entry can outlive the repo injection
// that planted it. The server derives (user_id, repo_id) from the run claim on
// write and owner-scopes every read/delete — the client never sends identity.
// repo_name is the human path (e.g. "vtmocanu/uzi") the list groups by; run_id is
// the provenance run that wrote the entry — OPTIONAL: it is `omitempty` in Go and
// set NULL when its run is pruned (FK ON DELETE SET NULL), so it can be absent.
//
// basis is writer-declared provenance (PRD #266): "observed" — the claim was backed
// by a tool result, command output, or a file:line the writer could name — vs
// "inferred", an untested guess. It is always present on read; legacy rows and any
// missing/unknown value read as "inferred" so an unverified fact is never rendered as
// verified. evidence is an OPTIONAL short pointer to the observation, agent-supplied
// free text (render it through stripUnsafeChars like title/body).
export type MemoryBasis = "observed" | "inferred";
export interface Memory {
  id: string;
  repo_id: string;
  repo_name: string;
  title: string;
  body: string;
  run_id?: string;
  created_at: string;
  basis: MemoryBasis;
  evidence?: string;
}

// ── Scheduled runs (PRD #241) ─────────────────────────────────────────────
// A time-driven run origin alongside manual and autopilot. A schedule is an
// owner-scoped intent to create run(s) at future time(s). Mirrors the Go
// apitypes.ScheduleDTO — snake_case, byte-for-byte.

export type ScheduleTarget = "issue" | "sweep" | "prompt" | "self_improve";
export type ScheduleTiming = "once" | "recurring";
// active — armed; fired — a `once` schedule that has already fired (terminal);
// error — parked because its owner/token/repo is gone (surfaced, not dropped).
export type ScheduleStatus = "active" | "fired" | "error";

// The closed set of reasons a schedule fire started no run for a candidate (PRD #308).
// The authoritative source is Go's schedsvc.SkipReason; scheduleSkipReasons.test.ts is a
// cross-language drift guard that reddens if Go gains a reason this union lacks.
export type ScheduleSkipReason =
  | "not_eligible"
  | "already_running"
  | "description_too_large"
  | "fetch_failed"
  | "vault_locked"
  | "self_improve_mr_cap_reached"
  | "open_mr_exists";

// One run a persisted fire actually created; issue_iid is null for a prompt schedule.
export interface LastFireStarted {
  issue_iid: number | null;
  run_id: string;
  title: string;
  // Forge issue URL snapshotted at fire time (PRD #411). Optional + nullable so pre-#411
  // persisted fires and existing mock entries degrade gracefully to a plain number.
  web_url?: string | null;
}

// One candidate a persisted fire considered but started nothing for, with its typed
// reason (never free text).
export interface LastFireSkip {
  issue_iid: number | null;
  title: string;
  reason: ScheduleSkipReason;
  // Forge issue URL snapshotted at fire time (PRD #411). Optional + nullable so pre-#411
  // persisted fires and existing mock entries degrade gracefully to a plain number.
  web_url?: string | null;
}

// The structured summary of a schedule's most recent persisted fire (PRD #308). matched
// == started.length + skips.length balances.
export interface LastFire {
  fired_at: string;
  matched: number;
  capped: boolean;
  started: LastFireStarted[];
  skips: LastFireSkip[];
}

// The outcome of a manual run-now fire (PRD #308). created/run_ids are retained for
// back-compat and derivable from started; matched/capped/started/skips carry the full
// per-candidate outcome.
export type RunNowResponse = {
  created: number;
  run_ids: string[];
  matched: number;
  capped: boolean;
  started: LastFireStarted[];
  skips: LastFireSkip[];
};

export interface Schedule {
  id: string;
  repo_id: string;
  // Best-effort display path ("vtmocanu/uzi"); "" when the repo can no longer be
  // resolved (disconnected or no longer owned).
  repo_path: string;
  target: ScheduleTarget;
  // Set only for target "issue"; null otherwise.
  issue_iid: number | null;
  // Sweep label selector; null (empty) ⇒ the PRD label at fire time (Decision 9).
  labels: string[] | null;
  // Stored prompt for target "prompt"; "" otherwise (Decision 10).
  prompt: string;
  timing: ScheduleTiming;
  // 5-field cron for a recurring schedule; "" for a once schedule.
  cron_expr: string;
  // The single fire instant (RFC3339) for a once schedule; null for recurring.
  run_at: string | null;
  timezone: string;
  next_fire_at: string | null;
  last_fired_at: string | null;
  // The structured summary of the most recent persisted fire (PRD #308); null = never
  // fired (or a parked/transient fire left the prior summary — or none — in place, since
  // only the success/benign advance path persists).
  last_fire: LastFire | null;
  auto_approve: boolean;
  wait_on_limit: boolean;
  /** PRD #841: per-schedule MR-review-rework override, tri-state. null = inherit (the
   *  default), so runs this schedule fires follow the owner's global setting unless an
   *  explicit true/false override is set. Mirrors the run-level nullable shape. */
  mr_rework_enabled?: boolean | null;
  // Per-sweep upper bound on issues fanned out per fire, oldest-first; null =
  // unlimited. Sweep target only (null for issue/prompt).
  max_issues: number | null;
  // Optional owner guidance steering HOW a run approaches the task (the issue body
  // stays the task); null = none. Issue and sweep targets only (null for prompt,
  // which already carries its own prompt text). Capped at 8192 bytes server-side.
  // For a default SWEEP row (issue #675) this is the owner OVERLAY (null until set);
  // the resolved catalog guidance for that row travels separately in `baked_guidance`.
  guidance: string | null;
  // Resolved catalog guidance for a default SWEEP row (issue #675), shown read-only in
  // the modal. Null for prompt/self_improve/user rows. `guidance` carries the owner
  // overlay appended at fire time. Mirrors apitypes.ScheduleDTO.baked_guidance.
  baked_guidance: string | null;
  // Per-schedule model override; null = inherit the owner's per-user Worker model.
  // Applies to ALL targets (prompt/issue/sweep), unlike guidance which is issue/sweep-only.
  model: string | null;
  // PRD #305: "apply model also to agents" — when true, the run's model overrides
  // every subagent's pin (lead + all subagents on one model). Default false.
  override_subagent_model: boolean;
  enabled: boolean;
  status: ScheduleStatus;
  // Origin (PRD #589): 'user' for an owner-authored schedule, 'default' for one
  // enabled from the builtin schedtmpl catalog. A default row is catalog-owned: its
  // prompt/labels/guidance/target come from the catalog and are read-only in the UI
  // (edit the cadence/model/run-flags, or clone to a user row to edit the rest).
  origin: ScheduleOrigin;
  // The catalog slug a default row was seeded from; null for a user row (including a
  // cloned default, whose prompt/labels are baked in with catalog_slug cleared).
  catalog_slug: string | null;
  // True once a default row's editable fields have drifted from the catalog values,
  // so the UI shows a "customized" indicator and makes Reset prominent. Always false
  // for a user row.
  customized: boolean;
  // Display-only sibling grouping key (PRD #636). Purely a view-grouping tag for custom
  // (origin='user') rows — the analog of catalog_slug for defaults — carrying no
  // behavior: editing one sibling never touches another. null = standalone row (the
  // common single-repo case); a non-null id shared by ≥2 live rows renders them as one
  // expandable group. Owner-scoped, so it can only ever group the caller's own rows.
  sibling_group_id: string | null;
  created_at: string;
  updated_at: string;
  // The live "next N fires" preview (up to 3), computed server-side from the same
  // cron logic the modal preview uses so the list and the modal agree.
  next_fires: string[];
}

// A schedule's provenance (PRD #589): owner-authored vs enabled from the catalog.
export type ScheduleOrigin = "user" | "default";

// CatalogEntry is one builtin default scheduled job (apitypes.CatalogEntryDTO, PRD
// #589): its shipped, read-only shape so the web can render the enable-a-default UI.
// For a prompt entry guidance/labels are empty and max_issues is 0; for a sweep entry
// prompt is empty and the body maps to guidance. auto_approve/wait_on_limit are the
// fixed run flags every default is seeded with, not per-entry.
export interface CatalogEntry {
  slug: string;
  name: string;
  description: string;
  // "prompt", "sweep", or "self_improve" in practice; typed as ScheduleTarget for reuse.
  target: ScheduleTarget;
  cron: string;
  timezone: string;
  // Per-entry model override; "" = inherit the owner's Worker default.
  model: string;
  // Baked prompt for a prompt entry; "" for a sweep entry.
  prompt: string;
  // Sweep selector labels; null for an assigned sweep (selects by assignee), [] for a prompt entry.
  labels: string[] | null;
  // Baked per-issue guidance for a sweep entry; "" for a prompt entry.
  guidance: string;
  // Sweep fan-out cap; 0 for a prompt entry.
  max_issues: number;
  auto_approve: boolean;
  wait_on_limit: boolean;
  /** PRD #767 M4: a sweep entry's selector kind — "label" (the default) selects by
   *  `labels`, "assigned" selects issues assigned to the uzi-bot account (and carries no
   *  labels). Empty/"label" for every non-assigned entry, so the enable-UI/CLI can describe
   *  an assigned sweep honestly rather than rendering its empty labels as a label selector.
   *  OPTIONAL here only to avoid forcing mock-object updates; the server always sends it. */
  selector_kind?: string;
  /** PRD #841 M2: the per-schedule MR-rework override a default is seeded with — null =
   *  inherit the owner default (the catalog default), so the enable-a-default UI reflects
   *  that a default job follows the user's global setting unless explicitly set. Tri-state
   *  (`boolean | null`), unlike wait_on_limit's plain bool. OPTIONAL here only to avoid
   *  forcing mock-object updates. */
  mr_rework_enabled?: boolean | null;
}

// CatalogEnablement records that the caller already has `slug` enabled on `repo_id`
// (apitypes.CatalogEnablementDTO, PRD #589): presence means enabled, absence means
// not. It surfaces the backing schedule id (to deep-link the row) and its pause flag.
export interface CatalogEnablement {
  repo_id: string;
  slug: string;
  schedule_id: string;
  enabled: boolean;
}

// ScheduleCatalog is the GET /api/schedule-catalog view: the builtin catalog plus the
// caller's per-repo enablement state (apitypes.ScheduleCatalogResponse, PRD #589).
export interface ScheduleCatalog {
  entries: CatalogEntry[];
  enablements: CatalogEnablement[];
}

// ScheduleInput is the create/patch body (apitypes.ScheduleRequest). On create,
// omitted flags take their server defaults (auto_approve=true per Decision 4,
// wait_on_limit=true, enabled=true). On PATCH a field present is applied and an
// absent one is left unchanged, so a per-row enable toggle sends just { enabled }.
export interface ScheduleInput {
  target?: ScheduleTarget;
  issue_iid?: number | null;
  labels?: string[];
  prompt?: string;
  timing?: ScheduleTiming;
  cron_expr?: string;
  run_at?: string | null;
  timezone?: string;
  auto_approve?: boolean;
  wait_on_limit?: boolean;
  /** PRD #841: per-schedule MR-review-rework override, tri-state. Present true/false is
   *  applied, present null clears back to inherit, absent leaves it unchanged (PATCH
   *  semantics). The modal always sends it on create (null = inherit the owner default). */
  mr_rework_enabled?: boolean | null;
  // Sweep cap (oldest-first); explicit null clears to unlimited. Sweep target only.
  max_issues?: number | null;
  // Owner guidance for issue/sweep targets; explicit null/"" clears to none.
  // Omitted on the prompt target so the server never rejects it.
  guidance?: string | null;
  // Model override for runs this schedule fires (all targets); explicit null/"" clears
  // to inherit. Unlike guidance it is sent on every target.
  model?: string | null;
  // PRD #305 opt-in; omitted ≡ false (server replace-semantics). The modal always sends it.
  override_subagent_model?: boolean;
  enabled?: boolean;
  // Repoint the schedule to another repo (PATCH only, PRD #344). A non-empty value moves
  // the schedule to that repo; the create path ignores it (repo comes from the URL). An
  // issue-target schedule cannot be repointed (server 422).
  repo_id?: string;
  // Sibling group tag (create-only, PRD #636 Decision 4). A multi-repo create fans out N
  // independent createSchedule calls that all carry ONE client-generated uuid here, so the
  // rows share a display group. A single-repo create omits it (standalone → NULL). The
  // server validates it (malformed → 400) and ignores it on PATCH (UpdateRunSchedule's SET
  // list omits it), mirroring RepoID's create-vs-PATCH asymmetry. `interface Schedule`
  // carries the read-side `sibling_group_id` (M3) separately; this is the write-side input.
  sibling_group_id?: string;
}

// SchedulePreviewInput asks for a live "next fires" preview from a timing spec
// that need not correspond to a stored schedule (the modal computes it as the user
// types). n is clamped to [1,10] server-side (default 3).
export interface SchedulePreviewInput {
  timing: ScheduleTiming;
  cron_expr?: string;
  run_at?: string | null;
  timezone: string;
  n?: number;
}

// ── Agent runtime (PRD #4) ────────────────────────────────────────────────

export interface Worker {
  id: string;
  name: string;
  status: string; // "offline" | "online"
  // Hosted workers (PRD #58). kind is never null — the column is NOT NULL DEFAULT
  // 'external' — so every worker carries one and a pre-#58 row reports "external".
  // hosted_size is the preset name ("s"|"m"|"l", lowercase on the wire) and is null
  // for an external worker. hosted_generation is deliberately not here: it is
  // controller-internal and the DTO does not carry it.
  kind: "external" | "hosted";
  hosted_size: string | null;
  // Whether this hosted worker carries the rootless-DinD sidecar (PRD #83 M3).
  // Absent/undefined or null for an external worker (docker is not applicable),
  // false for a hosted worker without the sidecar, true for a docker-capable one.
  // The wire sends null (not just absent) for an external worker — boolPtrValue on the
  // nil *bool (handler/workers.go:201) — so this is `boolean | null`, not just optional.
  docker?: boolean | null;
  // Auto-provisioned, run-bound throwaway hosted worker (PRD #529/#649): the api
  // provisions it on demand for a run needing a capability no online worker has and
  // reaps it when the run finishes. true marks such a worker; absent/false for a
  // normal hosted or an external worker. Optional so existing literals need no edit.
  ephemeral?: boolean;
  // Server-authoritative capability set (PRD #84 M1): the Filter-ed union of the
  // worker's self-reported caps and its template-derived caps, v1 vocabulary
  // {docker, jvm}. Read-only display. Optional so an older response (or a test
  // fixture) without the field reads as "none".
  capabilities?: string[];
  busy: boolean; // derived: holds ANY run of ANY kind, so a chat-only worker is busy:true with active_runs:0 — NOT active_runs > 0
  // Bounded concurrency (PRD #42 Decision 10). active_runs is the live count of the
  // worker's claimed/running/awaiting_approval runs (busy is derived from it);
  // max_concurrent_runs is the worker's advertised slot cap, null when it advertises
  // none (an older image, or before the M2 agent sends it). Together they drive the
  // "N/M runs" saturation badge (workerRunBadge in lib/workerRuns.ts).
  active_runs: number;
  max_concurrent_runs: number | null;
  // Worker template (PRD #18): the choice recorded at issuance and the value the
  // worker self-reports at register. Either may be null (no choice / older
  // image); a mismatch is surfaced as a drift badge, never a rejection.
  template_declared: string | null;
  template_reported: string | null;
  version: string | null;
  // Derived upgrade health (PRD #113), computed by the api at read time from `version`
  // against the control plane's release — never stored, so it cannot disagree with the
  // row it describes. upgrade_detail is the sentence behind the badge ("running 0.11.0,
  // target 0.11.7"), null in the steady up_to_date case where there is nothing to say.
  //
  // Five states, but the api derives only three from a version comparison:
  // up_to_date / outdated / unknown. "upgrading" and "upgrade_failed" require the
  // controller's roll report, because `version` is written ONLY at register — a worker
  // stuck offline mid-roll keeps reporting its OLD version and cannot self-report that
  // it is stuck. Both arrive with the roll-health fold.
  //
  // "unknown" is the honest answer, not an error: an unstamped image, an unparseable
  // report, or a "dev" control plane all land here, and none of them should raise an
  // alert.
  upgrade_status:
    "up_to_date" | "outdated" | "unknown" | "upgrading" | "upgrade_failed";
  upgrade_detail: string | null;
  // The coordinate this worker was compared AGAINST: the controller's rolled tag for a
  // hosted worker with a fresh report, otherwise the control plane's own version. "" when
  // the control plane has no version stamp — SERVER-SIDE classification is genuinely off
  // in that case, and this field is the api's own, so the phrase is accurate here.
  //
  // Not to be confused with the Fleet panel's arm for a cpVersion the SPA could not
  // fetch, which used to say the same words and no longer does (PRD #175): there,
  // classification had already happened server-side and only the SPA's copy of the
  // release was missing. Same phrase, opposite situation — a grep for the retired
  // wording lands here first.
  //
  // Rendered by the Fleet panel when it differs from the control-plane version. That
  // divergence is a supported operation — values.yaml may pin the worker image — and it is
  // also the shape a compromised controller would use to suppress every alert in the fleet
  // by reporting the fleet's own stale version as the target. The api cannot tell them
  // apart, so the UI states it rather than judging it.
  upgrade_target: string;
  // The blocking container and the k8s waiting REASON behind an upgrade_failed. Null for
  // every other status, and null on responses that carry no roll-health join (register,
  // heartbeat, create). The reason only — `message` is never sent, being free text that
  // carries paths and Secret names.
  upgrade_blocking_container: string | null;
  upgrade_blocking_reason: string | null;
  // The blocking container's last exit code, null when it never terminated (a different
  // fact from "exited 0"). It DISCRIMINATES causes the reason alone conflates — a
  // seed-nix CrashLoopBackOff comes both from a permissions error unpacking the nix store
  // and from that volume filling up.
  upgrade_last_exit_code: number | null;
  last_heartbeat_at: string | null;
  // api-owned anchor of when the worker became online (null offline); uptime = now − this, derived client-side.
  online_since?: string | null;
  // Set (to when it was cordoned) while a hosted worker is draining/cordoned: it
  // finishes its in-flight runs but claims nothing new (PRD #422/#496). null for a
  // worker that will claim normally. Hosted-only by construction. draining is derived
  // client-side as draining_since != null; it is ORTHOGONAL to upgrade_status/busy.
  draining_since: string | null;
  created_at: string;
  // Latest container resource sample (PRD #49), all null until the worker reports
  // one (and re-nulled if it stops). stats_cpu_pct is a percentage of the worker's
  // ALLOWED CPUs (100 = fully using its quota); null on the worker's first tick.
  // stats_mem_bytes is working-set bytes; stats_mem_limit_bytes is the container
  // limit in bytes, null when unlimited or unknown (process fallback) — the UI then
  // shows absolute usage with no percentage bar. stats_source is "cgroup" (container-
  // wide, covers children) or "process" (this worker process only; the UI labels it).
  // Freshness is last_heartbeat_at — an offline worker's stats are last-known, dimmed.
  stats_cpu_pct: number | null;
  stats_mem_bytes: number | null;
  stats_mem_limit_bytes: number | null;
  stats_source: string | null;
  // Which Anthropic credential this worker's RUN-lane claims spend (PRD #104 M3).
  // Both null means unbound: the worker spends its owner's default token, which is
  // every worker's state until someone binds one. The label rides alongside the id
  // so a row can say "spends: console-key" without a second lookup — a name, never
  // the credential.
  //
  // Caveat worth knowing at the call site: a bound worker's CHAT runs still spend
  // the DEFAULT (D1) — the binding covers the run lane only.
  anthropic_secret_id: string | null;
  anthropic_secret_label: string | null;
  /** How this worker's run-lane claims choose a credential (PRD #111 M3):
   *  "default" (the owner's default), "pinned" (the id above), or "auto" (uzi picks
   *  per claim from the owner's opted-in pool, preferring the most headroom).
   *
   *  🔴 It is the EFFECTIVE mode, not the stored column. Deleting a pinned token
   *  nulls the worker's id server-side and leaves the stored mode alone, so the
   *  database legitimately holds "pinned" with no id — a worker that resolves as
   *  default. The API applies that rule before answering, so "pinned" here always
   *  has an id beside it and no client needs to re-derive it. */
  anthropic_bind_mode: BindMode;
}

/** The closed set of worker bind modes (PRD #111 M3), mirroring the server's CHECK. */
export type BindMode = "default" | "pinned" | "auto";

export interface AdminWorker extends Worker {
  owner_email: string;
}

/**
 * Operator-set hosting policy (PRD #58 M2, GET /workers/hosted/config). enabled is
 * the instance kill-switch; quota is the per-user hosted-worker allowance, reported
 * even when enabled is false. A client reading enabled: false renders nothing hosted
 * regardless of the number beside it (Decision 12). quota 0 disables SELF-SERVICE
 * only — hosted workers the user already holds stay listed and deletable.
 */
export interface HostedConfig {
  enabled: boolean;
  quota: number;
  // Whether the instance admin gate permits ephemeral auto-provisioning (PRD #649),
  // independent of `quota`, which governs manual self-service.
  ephemeral_enabled: boolean;
}

export type RunStatus =
  | "queued"
  | "claimed"
  | "running"
  | "awaiting_approval"
  /** PRD #88: parked on an agent-initiated clarification question, awaiting the
   *  owner's answer. A distinct status from awaiting_approval rather than a sub-state
   *  of it, because the two resume through different guards and mean different things
   *  to the person who owes the run an action. M1 lands the type; the badge, tone and
   *  composer are M2. */
  | "awaiting_input"
  /** PRD #517: an interactive task run parked awaiting the owner's next
   *  follow-up. NON-terminal — deliberately absent from TERMINAL_RUN_STATUSES
   *  below — but unlike limit_wait it does NOT resume on its own; it needs the
   *  user. Distinct from awaiting_input (a clarification question) because a
   *  follow-up park is the run's turn-taking pause, not an unanswered question. */
  | "awaiting_followup"
  /** Parked until the owner's Anthropic usage window reopens (PRD #35).
   *  NON-terminal — deliberately absent from TERMINAL_RUN_STATUSES below. */
  | "limit_wait"
  /** Issue #754: an `auto`-lane run parked because the owner's Anthropic token
   *  pool is genuinely EMPTY. NON-terminal — deliberately absent from
   *  TERMINAL_RUN_STATUSES below. Unlike limit_wait this is NOT a usage-limit park:
   *  it carries no reset window and no countdown. It resumes automatically the
   *  moment a token is opted into the pool, and can be resumed on demand via
   *  `resumeRunNow` (POST /runs/{id}/resume-now). */
  | "pool_wait"
  | "completed"
  | "failed"
  | "cancelled";

// StopKind is the server-stamped stop signal (PRD #33, widened by PRD #108 M5):
// "cancelled" or "plan_rejected" for a deliberate HUMAN stop, "auto_stopped" when
// the SERVER stopped a run whose updates could not be saved, and null for a run
// that stopped for any other reason (a genuine failure, a timeout, or is still
// going). isStoppedRun reads this — never the free-text failure_reason — so a
// live-poller plan reject carrying the user's verbatim reason is still recognised.
//
// It does NOT treat every value alike, and that distinction is the point: only the
// two human kinds are styled as a calm "stopped". See isStoppedRun.
//
// This union crosses a JSON decode boundary, so an unlisted member is a silent lie
// rather than a type error — TypeScript cannot catch a missing value here, only a
// test can.
// PRD #517 M4: "stopped" is stamped on a completed interactive run that was
// gracefully stopped (board.go). It is NOT a human-stopped failure/cancel — the
// run lands status="completed" (green success) — so it is deliberately NOT in
// runBadge's HUMAN_STOP_KINDS; see the note there.
export type StopKind =
  | "cancelled"
  | "plan_rejected"
  | "auto_stopped"
  | "stopped";

// RunHealth is the server-side run-health flag (PRD #47): a non-terminal,
// self-clearing signal that a run looks slow, stuck, or looping. "ok" is the
// healthy default. It is orthogonal to RunStatus and never kills a run — the
// existing timeouts remain the only liveness backstops. runBadge renders the warn
// variant only while the run is in a flaggable status.
/** The judge's verdict on a run (PRD #46). Mirrors run_reviews.verdict's CHECK. */
export type JudgeVerdict = "ideal" | "ok" | "issues";

export type RunHealth =
  "ok" | "stalled" | "looping" | "slow" | "waiting_worker" | "approval_idle";

// FixVerdict is a ci_fix run's outcome (PRD #6): verified/fix_failed are stamped
// server-side from the post-fix pipeline; not_code is the agent's "not a code
// problem" verdict; null means the fix is not yet verified.
export type FixVerdict = "verified" | "fix_failed" | "not_code";

// RunPriority is the run's queue-priority CLASS (PRD #320 D8), computed server-side
// from the runs.priority rank + the run's kind + its demotion/grace state — never a
// raw column the web sets. "normal" is an interactive run at normal rank; "background"
// is a judge/self_improve run currently DEMOTED so it yields to interactive work;
// "expedited" is a run the owner bumped to the FRONT of the queue; "restored" is a
// demoted run that aged past the background grace and no longer yields. It is
// orthogonal to RunStatus (only a QUEUED run carries a non-normal class), and like
// StopKind it crosses a JSON decode boundary, so an unlisted member is a silent lie a
// test catches, not a type error. priorityBadge (lib/runBadge.ts) is the single map to
// its pill; "normal"/absent renders no pill.
export type RunPriority = "normal" | "background" | "expedited" | "restored";

// PRD #209: how a run's plan_md was produced. "agent" — the worker planned it at the
// gate (every pre-feature run, via the server's NOT NULL DEFAULT). "seeded" — the user
// supplied the plan at create time; the run skips planning and the approval gate. Not
// exported: the run view keys the seeded-plan surface on a string-literal compare, so
// nothing consumes the name yet (export it when a cross-file consumer appears — the
// convention here, mirroring FixVerdict/AgentSource, which ARE imported elsewhere).
type PlanSource = "agent" | "seeded";

// Milestone is one item of a milestone-structured run (PRD #122): a stable id and a
// human title. The title is REPO/agent-authored UNTRUSTED text — safe as JSX (React
// escapes it) but it must never be rendered through <Markdown> or interpolated into an
// HTML/URL sink, the same rule RepoAgent descriptions follow.
export interface Milestone {
  id: string;
  title: string;
}

/** The create-entrypoint family that started a run (server column `trigger_source`,
 *  a closed 13-value enum). Mirrors the Go CHECK constraint / RunDTO. */
export type RunTriggerSource =
  | "manual"
  | "autopilot"
  | "schedule"
  | "self_improve"
  | "ci_fix"
  | "mr_rework"
  | "chat"
  | "task"
  | "task_review"
  | "then_fix"
  | "judge"
  | "judge_rerun"
  | "resume";

export interface Run {
  id: string;
  /** Nullable since PRD #39: a chat run has no repo (issue/ci_fix runs always do). */
  repo_id: string | null;
  /** The run's forge ("gitlab"|"forgejo"|"github"), for the per-run MR/PR noun + reference
   *  sigil (PRD #65 D2). "" on worker/create DTO paths (no browser MR affordance);
   *  set on the list/detail reads. The web defaults an empty/unknown value to
   *  GitLab's vocabulary. */
  forge_type: string;
  /** Run kind (PRD #6): one of the RunKind union in lib/runKind.ts, pinned to
   *  fixtures/run-kinds/registry.json. */
  kind: RunKind;
  /** The worked issue for an issue run; null for a ci_fix or chat run (no issue). */
  issue_iid: number | null;
  issue_title: string;
  issue_description: string;
  /** Chat conversation title (PRD #39), first-message derived; null for other kinds
   *  and until derived. resume_of_run_id points a continued chat at the ended one. */
  title: string | null;
  resume_of_run_id: string | null;
  status: RunStatus;
  requeue_count: number;
  iteration_count: number;
  /** issue #321: server-computed planning-phase display flag — true only while a run is
   *  in its pre-approval PLANNING turn (running, iteration 0, no persisted plan yet;
   *  chat/judge excluded). Derived, not a real runs.status value. OPTIONAL for the SAME
   *  api/web rollout skew as plan_source: a mid-deploy api pod that predates the field
   *  omits the key, and an absent value reads as not-planning (isPlanningRun requires
   *  `=== true`). RunListItem extends Run, so list rows inherit it. */
  is_planning?: boolean;
  /** PRD #19: an autopilot run (poller-started, plan auto-approved). Drives the
   *  "autopilot" badge; a manually-started run is false. */
  auto_approve: boolean;
  /** issue #857: what/how/who started the run (manual, autopilot, schedule,
   *  self_improve, ci_fix, mr_rework, chat, task, task_review, then_fix, judge,
   *  judge_rerun, resume). A NOT NULL server column (DEFAULT 'manual'), so it is
   *  always present on a live read; OPTIONAL here only to avoid forcing mock-object
   *  updates. RunListItem extends Run, so list rows inherit it. */
  trigger_source?: RunTriggerSource;
  worker_id: string | null;
  branch: string | null;
  /** PRD #300: the per-schedule model a schedule froze onto this run at fire time;
   *  null = the run inherited the owner's per-user Worker default. Applies to all
   *  schedule targets (unlike guidance). */
  model: string | null;
  /** PRD #305: the frozen "apply model also to agents" flag; true means this run's
   *  model was applied to every subagent (overriding pins), false is the default. */
  override_subagent_model: boolean;
  mr_iid: number | null;
  /** Forge-supplied MR/PR web URL persisted by the worker at creation (PRD #65 D8),
   *  null on runs created before it landed. Rendered directly through isHttpsUrl; a
   *  null falls back to the legacy GitLab reconstruction (forgeUrls.ts). */
  mr_web_url: string | null;
  /** Forge-supplied issue web URL (PRD #411), null for issue-less runs or when the
   *  issue is no longer cached. Rendered through isHttpsUrl into the #<iid> link. */
  issue_web_url: string | null;
  /** PRD #764 M2: whether the run's issue links a `prds/*.md` file, so the run view can
   *  show a neutral "PRD" presence badge. OPTIONAL for api/web rollout skew: a pre-#764
   *  api pod omits the key, and an absent value reads as no PRD (the badge stays hidden). */
  has_prd_link?: boolean;
  /** Last MR state the PRD #24 watcher observed for mr_iid
   *  (opened|closed|merged|locked), null when never observed. Display-only hint
   *  (PRD #33); frozen per run, so a superseded run's value can be stale. */
  mr_state: string | null;
  /** PRD #841: this run's per-run MR-review-rework override, tri-state and nullable.
   *  `undefined`/absent (older api pod) and `null` both mean "inherit" — the watcher
   *  resolves the effective value live as `run.mr_rework_enabled ?? owner default`, so
   *  a null here follows the user's own Settings default. An explicit true/false is a
   *  per-run override. Kept `boolean | null` (never `any`) to preserve the
   *  omitted-vs-null-vs-value distinction, exactly like mr_state's nullability. */
  mr_rework_enabled?: boolean | null;
  failure_reason: string | null;
  /** Server-stamped stop signal (PRD #33, widened by #108 M5): "cancelled" or
   *  "plan_rejected" (human), "auto_stopped" (server), null otherwise. isStoppedRun
   *  reads this, not failure_reason — and treats only the two human kinds as calm. */
  stop_kind: StopKind | null;
  /** issue #525: the operator's OPTIONAL free-text cancel reason, stamped beside
   *  stop_kind on the cancel paths. This DTO is owner/admin-scoped, so it rides
   *  unconditionally (like failure_reason). Untrusted free text — via stripUnsafeChars. */
  stop_reason: string | null;
  /** Run-health flag (PRD #47). This owner-scoped DTO carries health_reason
   *  unconditionally (the run view is owner/admin only); health_since drives the
   *  header's "stuck for Xm". */
  health: RunHealth;
  health_reason: string | null;
  health_since: string | null;
  /** PRD #320 D8: the run's queue-priority CLASS, computed server-side (see RunPriority).
   *  Only a QUEUED run is ever non-"normal"; a running/terminal run renders no pill. It
   *  rides the shared run embed (RunListItemDTO embeds RunDTO), so it is present on BOTH
   *  the list and detail reads, like fix_verdict/report_only. OPTIONAL only for the SAME
   *  api/web rollout skew as report_only/plan_source/prd_done_path — a pre-#320 api pod
   *  omits the key, and an ABSENT value MUST be treated as "normal" (priorityBadge maps
   *  both to null, so a normal row stays quiet). */
  priority?: RunPriority;
  /** ci_fix (PRD #6): the failing ref, the failing pipeline's web URL (from the
   *  snapshot), and the fix verdict. All null on an issue run. */
  pipeline_ref: string | null;
  pipeline_web_url: string | null;
  fix_verdict: FixVerdict | null;
  /** issue #279: true when this completed run is a report-only / evidence run that
   *  intentionally opened no merge request; its deliverable is report_md + the transcript.
   *  Rides the shared run embed (RunListItemDTO embeds RunDTO), so it is present on BOTH the
   *  detail and list reads, like fix_verdict/plan_md. OPTIONAL only for rollout-skew — a
   *  pre-#279 api omits it, and a truthy guard treats absent as false. */
  report_only?: boolean;
  /** issue #279: the run's persisted findings summary, server-scrubbed. Non-null only on a
   *  report_only completion; rides the same embed (rendered only on the detail run view).
   *  UNTRUSTED — render as escaped plain text, never <Markdown>. */
  report_md?: string | null;
  /** PRD #377: the worker-preserved branch diff on a workflow-scope failure — a GitHub run
   *  whose branch touched `.github/workflows/**` cannot be pushed by the bot's `repo`-only PAT,
   *  so the run ends `failed` and the agent's (scrubbed, size-capped) unified diff is preserved
   *  here for a human to land as a PR. Non-null only on such a failed run; rides the same embed.
   *  UNTRUSTED worker-authored text — render as escaped plain text through stripUnsafeChars,
   *  never <Markdown>, exactly like report_md/failure_reason. */
  preserved_patch?: string | null;
  /** issue #150: the repo-relative path the run declared it moved a completed PRD to
   *  (e.g. `prds/done/72-x.md`), and the RFC3339 instant its PRD-completion patch settled.
   *  Both null on a run that moved no PRD. OPTIONAL for the SAME api/web rollout skew as
   *  plan_source/milestones — a mid-deploy api pod that predates these fields omits the keys.
   *  `prd_done_path` is WORKER-DECLARED untrusted text: render as escaped plain text through
   *  stripUnsafeChars, never <Markdown> or into a URL sink. `prd_patch_settled_at` is
   *  API/CLI audit metadata and is not rendered in the web UI. */
  prd_done_path?: string | null;
  prd_patch_settled_at?: string | null;
  plan_md: string | null;
  /** PRD #209: where plan_md came from — `"agent"` for a normal run whose worker wrote
   *  the plan at the gate, `"seeded"` for a run created WITH a user-authored plan that
   *  skips planning + the approval gate. The run view reads it to surface a seeded run's
   *  plan (SeededPlanPanel) and to show the roster-pending state, both otherwise
   *  unreachable because the approval UI never renders for a seeded run. Server-side the
   *  column is `NOT NULL DEFAULT 'agent'` so the current api always sends it; OPTIONAL
   *  here for the SAME api/web rollout skew that makes pending_judge optional — a mid-deploy
   *  api pod that predates this field omits the key. It is only ever compared with `===`,
   *  never dereferenced, so an absent value reads as not-seeded and the seeded surfaces
   *  simply do not render (no `?? null` normalization needed, unlike pending_judge). */
  plan_source?: PlanSource;
  /** PRD #212: the git-status porcelain lines the plan turn wrote to the worktree,
   *  surfaced at the approval gate. `[]`/absent renders nothing. UNTRUSTED
   *  repo-controlled paths — render as escaped plain text through stripUnsafeChars,
   *  never <Markdown> or into a URL sink. Optional `?` for api/web rollout skew
   *  (a mid-deploy api pod predating the field omits the key), same as plan_source. */
  plan_changed_files?: string[];
  /** PRD #362: plain-English run summaries. `summary_intent` ("what this run will
   *  implement") lands early in `running`; `summary_plan` ("what the proposed plan will
   *  do") and `summary_deltas` (how the plan diverged from the ask) land at the plan gate.
   *  All null until the worker generates and posts them, and null forever on any
   *  generation failure — summaries are advisory and the UI falls back to the issue title.
   *  UNTRUSTED, model-authored text: render as escaped plain text, never <Markdown>. A
   *  malformed `summary_deltas` is tolerated server-side and arrives as null ("no
   *  deltas"). OPTIONAL for the SAME api/web rollout skew as plan_source — a mid-deploy api
   *  pod that predates these fields omits the keys. */
  summary_intent?: string | null;
  summary_plan?: string | null;
  summary_deltas?: { kind: string; text: string }[] | null;
  /** PRD #37: the roster the worker detected in the clone's `.claude/agents/`.
   *  null = no worker reported (a pre-feature run); `[]` = detection ran and found
   *  none (the plan gate's repo card is inert, NOT the same as null). Names +
   *  descriptions only — REPO-SUPPLIED, untrusted text; render as plain JSX. */
  repo_agents: RepoAgent[] | null;
  /** PRD #37: which roster the run's subagents came from, once a selection is made
   *  (at the gate, or an autopilot run's resolved default). null before then. */
  agent_source: AgentSource | null;
  /** PRD #37: the names excluded from the chosen source. null before a selection. */
  agent_exclusions: string[] | null;
  /** PRD #37 M4-fix: the owner's OWN-source subagent roster (name + description) —
   *  exactly the allocation-resolved templates the worker runs for source="own",
   *  lead already stripped. The plan gate's "My agent templates" card is built from
   *  this, so an excludable chip always matches what approve accepts and the count is
   *  exact. Populated only on the run-detail read (getRun); null on list rows. */
  own_agents: RepoAgent[] | null;
  /** PRD #122: milestone-structured run fields. All six land together across an
   *  api/web deploy boundary — the Go read DTO adds them in parallel — so they are
   *  OPTIONAL here for the SAME rollout-skew reason plan_source is: a mid-deploy api
   *  pod that predates the feature omits the keys. A run that was never milestone-
   *  planned sends them all null. Either way every milestone surface hides and the UI
   *  falls back to the `iteration N` badge, so a null-milestone run renders EXACTLY as
   *  it did before this feature.
   *
   *  `milestones` is the FROZEN approved list — the denominator N. `milestones_completed`
   *  is the monotone union of ids reported complete (it can name an id no longer in the
   *  frozen set, so a badge counts only members — see milestoneBadge). `milestones_in_progress`
   *  is a snapshot of the ids currently in progress. `milestones_candidate` is the
   *  PRE-APPROVAL candidate list, shown ONLY at the plan gate.
   *
   *  Both milestone lists carry human titles that are REPO/agent-authored UNTRUSTED text:
   *  render as PLAIN JSX, never <Markdown> (same rule as repo_agents), so an
   *  attacker-authored title cannot become a link in an approval dialog. */
  milestones?: Milestone[] | null;
  milestones_completed?: string[] | null;
  milestones_in_progress?: string[] | null;
  milestones_candidate?: Milestone[] | null;
  budget_max_iterations?: number | null;
  budget_wall_seconds?: number | null;
  claimed_at: string | null;
  started_at: string | null;
  finished_at: string | null;
  created_at: string;
  updated_at: string;
  /** PRD #111 M1: which Anthropic credential this run's claim actually spent —
   *  what run_usage alone could never say. Both null for a run claimed before the
   *  feature landed, and for a run not yet claimed.
   *
   *  They go null INDEPENDENTLY and that is the design, not a bug to defend
   *  against: the label is a snapshot taken at claim time and survives the token
   *  being renamed or deleted, while the id goes null when the token is deleted
   *  (ON DELETE SET NULL). So `anthropic_secret_id === null` with a non-null label
   *  is the normal shape of a historical run — render the label, and treat the id
   *  as a link target only when it is there.
   *
   *  The label is USER-AUTHORED text. It is safe as JSX (React escapes it) but must
   *  never be interpolated into HTML or a URL unescaped. */
  anthropic_secret_id: string | null;
  anthropic_secret_label: string | null;
  /** PRD #111 M5: the MODE that named that credential, and the measured headroom of
   *  an auto pick (D20). The label alone could never answer the user's question,
   *  because an auto pick and a default fallback can name the SAME token — and PRD
   *  #104's compatibility path creates a row labelled literally `default`, so the
   *  label is not even a reliable hint at the mode.
   *
   *  Three INDEPENDENT nullabilities across the four fields, so branch per field and
   *  never on the group: the label outlives the id, the reason is present on every
   *  run claimed since M1, and the headroom only on an auto pick (and not on D14's
   *  retry, where the reading described the credential that would not open).
   *
   *  🔴 RENDER THE REASON; NEVER RE-DERIVE IT. Same rule as AutoStatus and for the
   *  same reason: it is the server's own record of what its selector did, and a UI
   *  that reconstructed it from the other fields would eventually disagree with the
   *  thing that actually spent the money. */
  anthropic_select_reason: SelectReason | string | null;
  anthropic_headroom_pct: number | null;
  /** PRD #40: the run's rolled-up token/cost totals (greatest-wins per model,
   *  summed across models — the server's run_usage_totals view). Present only when
   *  the run has usage rows; absent/null for a pre-feature run, so the UI shows
   *  nothing rather than a fabricated 0. On both the list rows and the detail read.
   *  Since PRD #111 M1 it can be read together with the credential above: what the
   *  run cost, and which account it cost it against. */
  usage?: RunUsage | null;
  /** PRD #35: this run's usage-limit opt-in — on a sustained Anthropic usage limit
   *  the run parks at status "limit_wait" and resumes when the window reopens,
   *  instead of failing. Present on every run from creation, so it is what a "will
   *  retry on limit" affordance renders BEFORE any park has happened. */
  wait_on_limit: boolean;
  /** PRD #35: when the exhausted window reopens, as REPORTED by the worker off the
   *  SDK frame, and when the server will actually promote the run back to queued.
   *
   *  🔴 THESE ARE NOT THE SAME INSTANT AND THE COUNTDOWN READS `retry_not_before`.
   *  That is when work resumes. It carries jitter, is clamped to RUN_LIMIT_MAX_PARK,
   *  is cross-checked against the owner's own rate-limit gauge, and is POOL-AWARE —
   *  a user whose second credential still has headroom is promoted early, so
   *  retry_not_before is routinely EARLIER than limit_resets_at, not merely offset
   *  from it. Render limit_resets_at as context ("the five-hour window reopens at
   *  …") and never compute the countdown from it.
   *
   *  Both null for a run that has never parked. ISO-8601 strings, like every other
   *  timestamp on this type. */
  limit_resets_at: string | null;
  retry_not_before: string | null;
  /** PRD #35: how many times this run has parked (0 if never), capped server-side
   *  by RUN_LIMIT_MAX_WAITS. The CAP is deliberately not on this type — it is one
   *  server constant and does not belong on every row of a list response — so render
   *  "attempt N", not "attempt N/M", unless the denominator is fetched from /api/me. */
  limit_wait_count: number;
  /** PRD #35: which window rejected the run ("five_hour", "seven_day", …). Already
   *  allowlisted against the SDK union server-side, with anything unrecognised
   *  coerced to "unknown", so it is a safe enum here and never worker free text —
   *  but render an unrecognised value honestly rather than dropping it, since the
   *  vocabulary is the SDK's and a newer server can ship a member this build has not
   *  heard of. Null for a run that has never parked. */
  rate_limit_type: string | null;
  /** PRD #84 M4: the run's inferred/hinted scheduling requirements, surfaced RAW so the
   *  web derives the plan-gate readiness display from them plus the assigned worker's
   *  capabilities (there is no server-computed "capability_block" field — the 409 the
   *  approval gate returns is the authoritative enforcement). `required_capabilities` is
   *  the claim-gating set (M2 repo hint UNION plan-time inference); `required_tools` are
   *  DISPLAY-ONLY provisionable toolchain families that never block; `size_class` is the
   *  clamped s/m/l estimate ("" when plan-time inference never set it).
   *
   *  OPTIONAL + back-compat, mirroring RepoDTO.required_capabilities: a mid-deploy api pod
   *  that predates PRD #84 M4 omits the keys, and an absent value reads as "none inferred"
   *  so the readiness block simply does not render. The server Filter-s the capability set
   *  to the v1 vocabulary ({docker, jvm}), so a name here is always known. */
  required_capabilities?: string[];
  required_tools?: string[];
  size_class?: string;
  /** PRD #634 M2: the operator scope ceiling — the count of milestones the run may
   *  complete over the immutable frozen list; null = unbounded. Rides the running-report
   *  ACK and the claim payload (both built by runToDTO). OPTIONAL here only to avoid forcing
   *  mock-object updates; the server always sends it. */
  scope_ceiling?: number | null;
  /** PRD #400: the task/handoff source ref a kind='task' run branched from — null when it
   *  inherited the caller's local HEAD, and on every non-task run. For a handoff created
   *  without --base (issue #403 F3) it is the resolved SEED COMMIT sha the auto-review uses
   *  as its diff base. OPTIONAL here only to avoid forcing mock-object updates. */
  base_branch?: string | null;
  /** PRD #400: whether a kind='task' run's worker opens an MR at the end (false by default
   *  and for every non-task run — a plain handoff produces commits on the branch, not an
   *  MR). Always on the wire; OPTIONAL here only to avoid forcing mock-object updates. */
  open_mr?: boolean;
  /** PRD #517 M1: marks a long-lived, conversational task run — the worker keeps it alive
   *  (parking in awaiting_followup) after signal_done rather than terminating. Set at create
   *  from --interactive; false by default and for every non-task run. Always on the wire;
   *  OPTIONAL here only to avoid forcing mock-object updates. */
  interactive?: boolean;
  /** PRD #400 Decision 6: when the CLI stamped a task run's dispatch gate — the moment it
   *  became claimable, after its uzi/task/<id> branch was seeded. Null on every non-task run
   *  and on a task run not yet dispatched. OPTIONAL here only to avoid forcing mock updates. */
  dispatched_at?: string | null;
  /** issue #403 F1/F6: server-computed `uzi handoff rm` preconditions, stamped only on the
   *  owner/admin GetRun detail read for a kind='task' run — false on every non-task run and
   *  on the list/create/worker DTO paths. branch_has_active_run is true while ANY run on the
   *  shared uzi/task/<id> branch is non-terminal (rm would race a live push);
   *  branch_has_open_mr is true when the branch's owning task opened an MR (rm exempt — the
   *  MR needs its source branch). Always on the wire (bool); OPTIONAL here only to avoid
   *  forcing mock-object updates. */
  branch_has_active_run?: boolean;
  branch_has_open_mr?: boolean;
}

// RunUsage is a run's server-rolled token/cost totals (PRD #40). The run VIEW
// derives its own richer per-phase/per-agent breakdown from the message stream
// (lib/runUsage.ts); this bundle is the cheap total the list row and detail strip
// read directly.
export interface RunUsage {
  input_tokens: number;
  cache_read_tokens: number;
  cache_creation_tokens: number;
  output_tokens: number;
  cost_usd: number;
}

// RunListItem is a run row for the index + admin overview: the run plus display
// context. owner_email is present only on the admin (all-users) list.
// RunListItem is the LIST row — GET /api/runs. It extends Run, and that inheritance is a
// trap worth naming: on the Go side RunDTO and RunListItemDTO are SEPARATE structs, so a
// field added to one is simply absent from the other. Here, a field added to `Run` is
// silently inherited by RunListItem, so putting a list-only field at the wrong level
// compiles fine and quietly claims that GET /runs/{id} returns something the API never
// sends. Nothing fails at runtime until a caller reads the missing field.
//
// So: a field the API puts on RunListItemDTO belongs HERE, not on Run. (PRD #98 M4's judge
// badge fields were caught doing exactly this, by tsc via the run-view fixtures.)
export interface RunListItem extends Run {
  /** Judge badge (PRD #98 M4). judge_verdict is the run's review verdict, null when
   *  the run was never judged — rendered as NO badge, never a neutral one, since
   *  "unjudged" and "judged fine" are different facts. judge_todo_count is the run's
   *  still-to-triage recommendation count, bucketed server-side by the ONE shared
   *  BucketOf ladder (never a SQL tally), so it agrees with the Judge page and the nav
   *  badge by construction. 0 for both an unjudged and a fully-triaged run; the row
   *  appends it only when > 0. */
  judge_verdict: JudgeVerdict | null;
  judge_todo_count: number;

  /** issue #750: server-computed plan-revise display flag on the LIST row (it rides
   *  RunListItemDTO, deliberately NOT RunDTO — the detail page keeps its own
   *  derivePlanRevision panel). True while the run's latest {plan, plan_revising} message
   *  is a plan_revising — a "revise" replan in flight. The server does NOT status-gate
   *  it, so a client combines it with status (isRevisingRun requires status ===
   *  "awaiting_approval" AND `=== true`). OPTIONAL for rollout skew: a pre-feature api
   *  pod omits the key, and an absent value reads as not-revising. */
  is_revising?: boolean;

  repo_path: string;
  worker_name: string | null;
  owner_email?: string;
}

// SelfUsage is the caller's own consumption (GET /api/usage, PRD #40): lifetime and
// last-7-days totals plus the count of their usage-bearing runs. run_count === 0
// means "nothing yet" — the card renders that state, not fabricated zeros.
export interface SelfUsage {
  lifetime: RunUsage;
  last_7_days: RunUsage;
  run_count: number;
}

// AdminUsageUser is one user's lifetime row in the admin factory breakdown.
export interface AdminUsageUser {
  user_id: string;
  email: string;
  usage: RunUsage;
  run_count: number;
}

// AdminUsage is the factory-wide view (GET /api/admin/usage, admin-only): the
// factory totals plus the per-user breakdown. The per-user rows sum to factory
// lifetime by construction (the server rollup guarantees it).
export interface AdminUsage {
  factory: SelfUsage;
  users: AdminUsageUser[];
  /** ISO timestamp of the factory's earliest usage-bearing run (for the "since
   *  <date>" line); null when the factory has no usage yet (PRD #40). */
  earliest_run: string | null;
}

// ── Claude rate limits (PRD #53) ─────────────────────────────────────────────
// Anthropic enforces two account-wide windows (5-hour and 7-day); a server-side
// poller reads each user's own utilization with their stored token and the SPA
// renders meters in three places. The token never leaves the api container — the
// SPA only ever sees percentages (Decision 1). These shapes mirror the FROZEN DTO
// contract in prds/53-rate-limits.md, discriminated on `status`.

// RateLimitWindow is one window's utilization. pct is 0–100 (server floors +
// clamps the 0–1 fraction Anthropic reports). resets_at is epoch SECONDS, null
// when Anthropic did not report a reset; the SPA renders it as a live countdown
// (Decision 7).
export interface RateLimitWindow {
  pct: number;
  resets_at: number | null;
}

// Which source produced the reading:
//  - "usage_endpoint": the free usage endpoint (Decision 2).
//  - "header_probe": the ~1-token header probe fallback (Decision 2).
//  - "limit_report": a reading recorded at usage-limit park time from the
//    worker's limit report (PRD #217 M1). It is a 100%-consumed INFERENCE for the
//    window that just refused the run, NOT a live measurement — the park writes
//    the pct and nothing else. Because the park deliberately does not bump
//    `synced_at` (D3), this reading is NEWER than the `synced_at` shown beside it,
//    so a surface rendering "updated Xm ago" against a 100% bar must disclose that
//    the 100% was recorded at the park, after that timestamp.
// RATE_LIMIT_SOURCES is the vocabulary AT RUNTIME. RateLimitSource is derived from
// it (`(typeof RATE_LIMIT_SOURCES)[number]`) so the array IS the union — there is no
// hand-maintained second list to fall behind. This is what lets rateLimitSource.test.ts
// pin the union against migration 00109's CHECK, since a bare TS union erases at runtime.
export const RATE_LIMIT_SOURCES = ["usage_endpoint", "header_probe", "limit_report"] as const;
export type RateLimitSource = (typeof RATE_LIMIT_SOURCES)[number];

// MyRateLimits is the per-user reading, discriminated on status:
//  - "ok": a real reading (possibly stale — vault-locked users age silently, D3).
//  - "no_token": the user has no anthropic_token stored.
//  - "unavailable": token saved but no reading yet, probe disabled, or the
//    credential was refused.
export type MyRateLimits =
  | {
      status: "ok";
      five_hour: RateLimitWindow;
      seven_day: RateLimitWindow;
      source: RateLimitSource;
      synced_at: string; // ISO-8601
      stale: boolean;
    }
  | { status: "no_token" }
  | { status: "unavailable" };

// TokenRateLimits is ONE token's meter (PRD #104 M5): the credential's label and
// default flag — a name, never a value — plus the same status union PRD #53 froze.
// secret_id keys the row for a rebind or a delete.
/** AutoStatus is the server's answer to "could auto-selection pick this token right
 *  now, and if not why" (PRD #111 M2). A CLOSED set, mirroring autoselect.Status.
 *
 *  🔴 RENDER IT; NEVER RE-DERIVE IT. It comes from autoselect.Classify, the same
 *  single function the server's ranker gates candidates on, precisely so this page
 *  cannot promise a token is eligible that the selector silently skips (D21).
 *  Reconstructing it here from `limits` — a `100 - pct`, a synced_at comparison —
 *  reintroduces exactly the drift the field exists to remove, and nothing would fail
 *  when the two disagreed. */
/** SelectReason is WHY a run spent the credential it spent (PRD #111 M5, D20) — the
 *  MODE that named it. A CLOSED set of eight, mirroring autoselect.Reason in Go and
 *  migration 00089's CHECK in SQL; the "reason vocabulary is one vocabulary" suite in
 *  runCredential.test.ts (its `reasonsFromMigration()` helper) parses that migration
 *  and pins the three in step.
 *
 *  Typed as `SelectReason | string` on the wire rather than as the union alone: the
 *  API is deployed separately from this bundle, so a newer server can ship a ninth
 *  reason, and a union that lied about being total would make a renderer's exhaustive
 *  switch look safe while dropping it. The renderer handles the unknown case
 *  explicitly instead. */
export type SelectReason =
  | "default"
  | "pinned"
  | "judge"
  | "auto"
  | "best_of_pool"
  | "pool_empty"
  | "pool_stale"
  | "open_failed";

export type AutoStatus =
  | "eligible"
  | "not_pooled"
  | "no_reading"
  | "unmeasured"
  | "stale"
  | "below_threshold";

export interface TokenRateLimits {
  secret_id: string;
  label: string;
  is_default: boolean;
  /** The owner's pool opt-in, and the live eligibility it produces (PRD #111 M2).
   *  Not redundant: a token can be opted IN and still unpickable — its gauge never
   *  polled, or its reading aged out — which is the silent no-op the status exists
   *  to surface. */
  auto_eligible: boolean;
  auto_status: AutoStatus;
  limits: MyRateLimits;
}

// MyRateLimitsResponse is what GET /me/rate-limits returns since PRD #104 M5: an
// ARRAY of per-token meters, replacing PRD #53's single reading. An EMPTY array is
// the token-less signal — there is no per-token status to report when there is no
// token, so the "no_token" branch of MyRateLimits is now only ever produced
// client-side (see rateLimits.ts).
export interface MyRateLimitsResponse {
  tokens: TokenRateLimits[];
}

// AdminRateLimitUser is one row of the admin all-users view: every user appears,
// including token-less ones (whose `tokens` is empty). vault_locked flags a user
// whose dek-sealed tokens can't be opened right now (their readings age, marked
// stale). Since #104 M5 the row carries one meter PER TOKEN.
export interface AdminRateLimitUser {
  id: string;
  email: string;
  name: string;
  vault_locked: boolean;
  tokens: TokenRateLimits[];
}

export interface AdminRateLimits {
  users: AdminRateLimitUser[];
}

// ── Notifications inbox (PRD #46 M2) ─────────────────────────────────────────
// A generic in-app notification. kind + payload let any feature enqueue one; the
// judge is tenant #1. payload is the render blob — by convention a `title` and
// optional `body` the inbox shows, but readers must tolerate any shape. run_id /
// review_id are optional deep-link anchors. owner is present ONLY on the admin
// all-view so the admin sees whose inbox a row belongs to.
export interface NotificationOwner {
  id: string;
  email: string;
  display_name: string | null;
}

export interface Notification {
  id: string;
  kind: string;
  payload: Record<string, unknown>;
  run_id: string | null;
  review_id: string | null;
  read_at: string | null;
  created_at: string;
  owner?: NotificationOwner;
}

// NotificationList is the inbox envelope: one page of rows, the caller's own
// unread count (the bell badge), and the scope total for paging.
export interface NotificationList {
  notifications: Notification[];
  unread: number;
  total: number;
}

// ── Run judge review (PRD #46 M4) ────────────────────────────────────────────
// The judge's retrospective of a finished run: a verdict + structured
// recommendations. Every free-text field (summary_md, each rationale_md, target)
// was validated + capped + secret-scrubbed at the review POST and is UNTRUSTED
// judge/worker output — the run page renders it as escaped text (never markdown/
// HTML), through lib/safeText's stripUnsafeChars. Escaping alone is NOT the whole
// guarantee (issue #124): the browser honours Cf bidi overrides, which were persisted
// by two IsControl-only ingest scrubbers until those learned Cf too — so rows written
// before that still need the renderer-side strip.
// verdict/category/confidence are closed enums.
export type ReviewVerdict = "ideal" | "ok" | "issues";
export type ReviewStatus = "complete" | "failed";
export type RecommendationCategory =
  | "enable_tool"
  | "install_worker_tool"
  | "adjust_template"
  | "improve_agent"
  | "add_agent"
  | "improve_uzi"
  | "cost_efficiency";

export interface ReviewRecommendation {
  id: string;
  category: RecommendationCategory;
  target: string;
  rationale_md: string;
  confidence: "" | "low" | "medium" | "high";
  created_at: string;
}

// FiledIssue is a SETTLED recommendation→forge-issue link (PRD #68 M4). The panel
// matches it to a recommendation by (category, target) and renders the filed row (issue
// link) instead of the File-issue button; filed_at < review.updated_at flags a stale link
// ("filed for an earlier version"). Only settled links appear.
export interface FiledIssue {
  category: RecommendationCategory;
  target: string;
  issue_iid: number;
  issue_url: string;
  filed_at: string;
}

// Disposition is the user's triage verdict on a recommendation (PRD #94): done, or
// dismissed with a reason (wont_do / not_an_issue = false positive). Coordinate-keyed
// (category, target) like FiledIssue, so it matches a recommendation the same way and
// survives a re-judge. Only coordinates with a current matching recommendation appear.
// `stale` is server-computed (a rationale-hash compare, D#3) — RENDER it, never
// recompute it in TS; the browser never sees a hash.
export interface Disposition {
  category: string;
  target: string;
  status: "done" | "dismissed";
  reason: "" | "wont_do" | "not_an_issue";
  set_at: string;
  stale: boolean;
}

// TriageCounts is the bucketed tally the server computes with ONE Go helper (the
// D#2 ladder dismissed > done > filed > todo), so the per-review bar and the global
// strip cannot drift. false_positives is the not_an_issue sub-count of dismissed. The
// web renders these DIRECTLY — never re-derived from the rows on screen (D#7/D#8).
export interface TriageCounts {
  total: number;
  todo: number;
  filed: number;
  done: number;
  dismissed: number;
  false_positives: number;
}

// JudgeCategoryStats is GET /me/judge/category-stats (PRD #270): the per-category GROUP
// count for the Judge filter chips, now a bucket-keyed MATRIX. `counts_by_bucket` maps each
// triage bucket (always exactly `todo`, `filed`, `done`, `dismissed`, `all` — all five
// present, each an object, possibly `{}`) to a per-category count, where the inner map keys
// each raw recommendation category to the number of distinct groups in that bucket. `all`
// is the whole-backlog per-category count and equals `todo+filed+done+dismissed` per
// category. It is a real server aggregate over an UNCAPPED load, NOT a tally of the
// on-screen groups: those are capped-before-grouping and bucket-filtered, so a chip can
// honestly read 6 while the truncated list shows 4 cards. The matrix is TAB-SCOPED
// (the page indexes it by the active bucket) and TRIAGE-VARIANT: a mark-done moves a group
// between buckets, so it is refetched on every disposition/undo/file mutation and on a
// run-anchor change — NOT fetched once, NOT triage-invariant. It is anchor-aware (respects
// `?run=`) but never category-filtered. A SEPARATE endpoint from the nav-badge stats. A
// nested MAP, not a fixed-field struct, so a category the client has no chip for does not
// break the wire — the client reads `counts_by_bucket[bucket][cat] ?? 0` per chip.
export interface JudgeCategoryStats {
  counts_by_bucket: Record<string, Record<string, number>>;
}

export interface RunReview {
  id: string;
  target_run_id: string;
  verdict: ReviewVerdict;
  summary_md: string;
  judge_model: string;
  status: ReviewStatus;
  created_at: string;
  updated_at: string;
  recommendations: ReviewRecommendation[];
  filed_issues: FiledIssue[];
  // PRD #94: the caller's triage dispositions (coordinate-keyed) + the bucketed
  // per-review counts, both server-computed. The panel mirrors dispositions into a
  // dispByCoord map (like filedByCoord) and renders `triage` verbatim.
  dispositions: Disposition[];
  triage: TriageCounts;
  // PRD #69 M6: the judge run's OWN timing + token/cost usage, for the panel's
  // time/tokens/cost strip. Omitted (absent) when there is no judge-run detail; `usage`
  // is null for a pre-feature judge that posted no result frame (render NO strip, never a
  // fabricated 0). Duration is finished_at - started_at.
  judge_run?: {
    judge_run_id: string;
    claimed_at: string | null;
    started_at: string | null;
    finished_at: string | null;
    usage: RunUsage | null;
  };
}

// PendingJudge is the ACTIVE judge run for a target (PRD #119) — "a verdict is already
// coming". It is the fact `review: null` alone cannot express: an unjudged run whose
// auto-judge is already queued and one that was never judged at all look identical
// through the review field, and the panel used to offer a Run-judge button in both —
// a button whose only possible outcome in the first case is a 409 from the
// one-active-judge-per-target index. This says which of the two it is.
//
// `state` is a CLOSED union here because the server normalizes it with a total mapper
// (handler.pendingJudgeState): `queued` → "scheduled", every other status in the index's
// active set (claimed, running, awaiting_approval, and anything a future migration adds)
// → "running". The raw runs.status set is wider than these two and is allowed to grow;
// learning that is the server's job, not a client's, so nothing here should widen the
// union or default an unknown value. enqueued_at is the judge run's created_at.
export interface PendingJudge {
  state: "scheduled" | "running";
  enqueued_at: string;
}

// IssueDraft is the templated, human-editable draft for filing a forge issue from a
// recommendation (PRD #68 M2/M4). The body is server-rendered + sanitized (fenced/
// stripped/scanned), but the panel treats title/description as INERT text (never Markdown)
// like ProposalCard — the load-bearing controls re-run server-side at the POST. A blank
// default_repo_id means no default resolved (empty picker, mock state D); default_note is
// the picker hint / no-default reason; provenance names whose worker produced the text.
export interface IssueDraft {
  default_repo_id: string;
  title: string;
  description: string;
  labels: string[];
  provenance: string;
  default_note: string;
}

// ── Judge menu — cross-run recommendation backlog (PRD #98) ──────────────────
// The Judge page reads the caller's recommendations deduped by (category, target)
// across all their runs, and disposes a whole group in one call. Every free-text
// field below (rationale_preview, run_title, target) is UNTRUSTED judge/worker
// output, shipped as PLAIN TEXT and rendered as escaped React text (never Markdown /
// dangerouslySetInnerHTML) and stripped of Cc/Cf by lib/safeText (issue #124) — both
// guarantees are CLIENT-side, matching RunView's handling of the same fields.

// The ?bucket= filter matches the GROUP rollup, not a member. "all" is unfiltered;
// the other four are the #94 ladder's rungs. Default is "todo".
export type JudgeBacklogBucket =
  "todo" | "filed" | "done" | "dismissed" | "all";

// Disposition scope for the bulk fan-out (PRD #98 Decision 3). "open" (default) only
// touches members the ladder buckets as todo — a filed/settled member is left alone;
// "all" re-asserts across every member.
export type JudgeDispositionScope = "open" | "all";

// JudgeFiledIssueRef is the settled forge issue a single occurrence was filed as
// (PRD #98 M1). Present only on a settled link; absent for a never-filed or in-flight
// coordinate. Coordinate-free — the enclosing group carries (category, target).
export interface JudgeFiledIssueRef {
  issue_iid: number;
  issue_url: string;
  filed_at: string;
}

// JudgeOccurrence is one run's instance of a deduped recommendation (PRD #98
// Decision 2). Triage state stays PER-COORDINATE, so each occurrence carries its own
// bucket and its own filed link. rec_id + run_id are what the per-recommendation
// File-issue draft (#68) needs from the occurrence expander.
export interface JudgeOccurrence {
  run_id: string;
  run_title: string;
  review_id: string;
  rec_id: string;
  verdict: ReviewVerdict;
  confidence: "" | "low" | "medium" | "high";
  bucket: JudgeBacklogBucket;
  // The disposition's PROVENANCE (PRD #98 Decision 6). Absent means a PERSON set it;
  // "issue_close" means the M6 poller sync did when the filed issue was closed. Both are
  // bucket "done", so the bucket alone cannot tell them apart and a client that ignores
  // this renders "I decided this was done" and "the system inferred it" identically — two
  // different claims, only one of them the user's.
  //
  // Typed as a literal union, not `string`: that is what makes `set_via === "issue_close"`
  // compiler-guarded here. (A raw string comparison would be the one client-side shape that
  // fails silently — see isBucket, the one site tsc does not guard.)
  //
  // NOTE this NARROWS HARDER THAN THE WIRE GUARANTEES. Go ships `SetVia string`, so a future
  // server-side provenance value is a state this type calls impossible. It fails safe — an
  // unrecognised value falls through to the plain chip (a bare "✓ Done" or "Dismissed")
  // rather than mis-labelling — but the guarantee lives in Go's SQL writers, not in this
  // declaration. The third value is "denied_cli" (issue #167): a system auto-dismissal of a
  // recommendation whose target names a credential-bearing CLI that policy permanently bars
  // (glab, gh, aws, az, …). Widen the union here when a further value is added server-side.
  set_via?: "issue_close" | "denied_cli";
  filed_issue?: JudgeFiledIssueRef;
}

// JudgeRecommendationGroup is one (category, target) coordinate deduped across every
// run it recurs in (PRD #98 Decisions 1/2) — the Judge menu's row. open_count is the
// number of members whose bucket is todo; run_count is the DISTINCT run count ("seen
// in N runs", the frequency signal the backlog ranks by); bucket is the group rollup
// (todo whenever open_count >= 1, else the highest member rung).
export interface JudgeRecommendationGroup {
  category: RecommendationCategory;
  target: string;
  bucket: JudgeBacklogBucket;
  open_count: number;
  run_count: number;
  rationale_preview: string;
  occurrences: JudgeOccurrence[];
}

// JudgeBacklog is GET /api/me/judge/recommendations (PRD #98 M1). `bucket` echoes the
// applied filter and `run` the ?run= anchor (""" when absent). `triage` is the
// canonical GET /me/judge/stats aggregate — NEVER tallied from `groups` — so the page
// tabs, the nav badge and the notification cannot drift; it survives both filters and
// truncation. `truncated` says the hard row cap bit: when true a SURVIVING group's
// counts/rollup may be understated, so a truncated page is never authoritative.
export interface JudgeBacklog {
  bucket: JudgeBacklogBucket;
  run: string;
  groups: JudgeRecommendationGroup[];
  truncated: boolean;
  triage: TriageCounts;
}

// JudgeDispositionCoord is one requested (category, target) coordinate for the bulk
// fan-out. It is the caller's REQUEST — nothing here is written; the server writes off
// the resolved rows.
export interface JudgeDispositionCoord {
  category: RecommendationCategory;
  target: string;
}

// JudgeDispositionResult is the response to the bulk group disposition (PRD #98 M2).
// `updated` counts member (review_id, category, target) TRIPLES, so it can be LOWER
// than the recommendations a group visibly spans — "dismissed a group of 5, said 4"
// is correct, not a bug. `groups` are the acted-on coordinates RE-READ at bucket=all,
// so a group that just left To triage still returns with its new rollup and the row
// re-renders rather than vanishing. `truncated`: past the cap a settled coordinate can
// fall outside the read window and have NO group here — treat a missing group as
// UNKNOWN, not settled.
// `settled` names the members this call ACTUALLY wrote, as (run, rec) addresses for the
// per-recommendation disposition route. Undo MUST revert these and never a set the client
// computed itself: with scope=open, membership is decided server-side at write time, so a
// member settled since the page last loaded is "open" in the client's view and outside the
// action. Undoing from that stale view deletes dispositions the action never created — and
// for an M6 issue-close auto-done that is IRREVERSIBLE (close_synced_at is stamped and the
// poller is edge-triggered, so it never re-fires and the provenance is gone). `updated` is
// a bare count and cannot substitute: a count cannot say WHICH.
export interface JudgeSettledMember {
  run_id: string;
  rec_id: string;
}

export interface JudgeDispositionResult {
  updated: number;
  settled: JudgeSettledMember[];
  groups: JudgeRecommendationGroup[];
  truncated: boolean;
  triage: TriageCounts;
}

// ── Incidental Findings (PRD #333 M7) ────────────────────────────────────────
// The web mirror of the server DTOs in api/internal/apitypes/finding.go. These are
// hand-maintained interfaces whose JSON keys must match the Go `json:"…"` tags exactly.

// IncidentalFindingBucket is the GET /api/findings ?bucket= filter (D7). The default
// (`to_file`) is the backlog's reason to exist — what still needs filing.
export type IncidentalFindingBucket = "to_file" | "filed" | "dismissed" | "all";

// IncidentalFinding is one (repo, location) coordinate in the per-repo Findings backlog,
// deduped across every run it recurs in (mirrors apitypes.IncidentalFindingDTO, D7).
//
// `finding_id` is the latest evidence row's id — the id the file/dismiss actions drive on
// (M5). It is UNDEFINED (omitempty) on a filed/dismissed coordinate whose evidence rows were
// cascaded away with a deleted run (D12): the coordinate still appears (the read is
// disposition-driven) and `last_title` keeps it legible, but there is no evidence row to act
// on, so a nil finding_id means "not actionable from here".
//
// `location`, `repo_path` and `last_title` are agent-authored, already-sanitised (inert at
// rest) — but like the judge's rationale_preview EVERY consumer renders them as escaped text
// through stripUnsafeChars, never markdown/HTML (issue #124 hardening).
export interface IncidentalFinding {
  finding_id?: string;
  location: string;
  repo_id: string;
  repo_path: string;
  status: string;
  last_title: string;
  seen_in_runs: number;
  filed_issue_iid?: number;
  // filed_issue_url is the stored forge URL a filed coordinate produced (stamped at settle
  // time). It is the DTO-carried source the backlog links "Filed #<iid>" through — present for a
  // backlog-loaded filed row too, not just one filed this session — and is undefined until
  // filed. Rendered as a link only when it is a real https URL.
  filed_issue_url?: string;
  resolved_at?: string;
}

// IncidentalFindingBacklog is GET /api/findings (D7/D8). `bucket`/`repo`/`run` echo the
// applied filters ("" when a filter is absent). `open_count` is the D8 nav-badge count that
// rides on this response meta (the judge pattern), scoped by the same ?repo= filter. `findings`
// is never null on the wire (an empty backlog encodes []).
export interface IncidentalFindingBacklog {
  bucket: string;
  repo: string;
  run: string;
  open_count: number;
  findings: IncidentalFinding[];
}

// IncidentalFindingIssueDraft is GET /api/findings/{id}/issue-draft (D4): the deterministic,
// human-editable draft. Every field is already inert; `labels` seed the editable selection
// (the server-mandated marker is added at file time, D5, never here).
export interface IncidentalFindingIssueDraft {
  title: string;
  description: string;
  location: string;
  labels: string[];
  provenance: string;
}

// IncidentalFindingFiledIssue is the real forge issue POST /api/findings/{id}/issue created
// (mirrors the handler's createdIssueDTO): only what the click produced.
export interface IncidentalFindingFiledIssue {
  iid: number;
  web_url: string;
  title: string;
}

// IncidentalFindingFileResult is the POST /api/findings/{id}/issue response (M5): the created
// forge issue plus a non-empty `warning` when the issue WAS created but its local disposition
// could not settle (created-with-warning — a success, never a retry signal).
export interface IncidentalFindingFileResult {
  issue: IncidentalFindingFiledIssue;
  warning?: string;
}

// RunMessage is one persisted, seq-numbered event in a run's stream.
export interface RunMessage {
  seq: number;
  kind: string;
  agent: string | null;
  // agent_instance is the subagent INVOCATION id (the SDK's per-frame
  // parent_tool_use_id, PRD #99), agent_label the task description that
  // invocation was given. Both null when the frame carried no
  // `parent_tool_use_id` — the orchestrator's own turns, infra frames, and every
  // pre-migration message. NOT the same as `agent === "lead"`: a repo may ship an
  // agent NAMED lead, which is a real subagent and does carry an id.
  // Consumers fall back to `agent`.
  agent_instance: string | null;
  agent_label: string | null;
  payload: unknown;
  created_at: string;
}

/** PRD #88 adds "answer": the reply to an ask_user question. Unlike every other kind
 *  its body is JSON — `{ question_id, answers }` — because an answer has to name the
 *  question it answers; the api rejects one that names a question which is no longer
 *  open, and one submitted while the run is not parked at all. */
export type RunInputKind =
  | "follow_up"
  | "approve_plan"
  | "reject_plan"
  | "revise_plan"
  | "cancel"
  | "answer";

// SteerInput is one steer-queue entry (PRD #95, extended by PRD #634), from
// GET /api/runs/{id}/inputs. `kind` is "follow_up" or "scope" (an operator
// scope-ceiling directive). Delivery status is derived client-side per kind:
//   - a follow_up derives its state from consumed_at (null ⇒ Queued; set ⇒
//     Delivered) and its disposition is always null;
//   - a scope row is NEVER consumed (consumed_at always null); its state lives
//     ENTIRELY in disposition ("applied" | "declined" | "superseded" | null pending).
// body is nullable to match the wire, though every entry carries one in practice.
export interface SteerInput {
  id: number;
  body: string | null;
  created_at: string;
  consumed_at: string | null;
  // "follow_up" | "scope" (PRD #634). Scope rows carry disposition; follow_up
  // rows derive their state from consumed_at (disposition always null).
  kind: string;
  // For a scope row: "applied" | "declined" | "superseded" | null (pending).
  // Always null for a follow_up row.
  disposition: string | null;
}

// AgentSource is which roster a run's subagents come from (PRD #37): the repo's
// own .claude/agents/, or the user's uzi templates. The lead orchestrator is
// always uzi's builtin and is never selectable.
export type AgentSource = "repo" | "own";

// RepoAgent is one agent the worker detected in the cloned repo's .claude/agents/
// (PRD #37): names + descriptions ONLY (the prompt bodies never leave the worker).
// These are REPO-SUPPLIED, untrusted text — the plan gate renders them as plain
// JSX, never through <Markdown>, so an attacker-authored link can't be clickable
// inside the approval panel.
export interface RepoAgent {
  name: string;
  description: string;
}

// AgentSelectionInput is the plan-gate agent choice submitted with approve_plan
// (PRD #37). The server validates it against the run's real roster and writes its
// own canonical body; the client never composes the worker-bound body itself.
export interface AgentSelectionInput {
  source: AgentSource;
  exclusions: string[];
}

// WsEvent is a live frame from /api/ws. A "message" carries a persisted message
// (rendered directly, deduped by seq); "state" signals a status change, "health" a
// run-health flag change (PRD #47), and "input" a steer-queue change (PRD #95). For
// "state", "health", and "input" the client re-reads over REST — WS is never the
// source of truth, so the (owner-gated) flag reason / steer text never rides the
// socket. The "input" frame is data-less and a FAST-PATH only: the queue also
// reconciles by REST refetch (mount/reconnect/state/health), so a dropped frame
// self-heals — its handling lands in M3.
export interface WsEvent {
  type: "message" | "state" | "health" | "input";
  seq?: number;
  kind?: string;
  agent?: string | null;
  // PRD #99: the live frame carries the lane identity itself — useRunStream
  // builds its RunMessage straight from the frame with no REST re-read, so a
  // subagent message can only land in the right lane if the frame says which
  // invocation produced it. Omitted (not "") when absent, exactly like `agent`.
  agent_instance?: string | null;
  agent_label?: string | null;
  payload?: unknown;
  created_at?: string;
  status?: string;
}

// ── Chat (PRD #39) — PROVISIONAL wire shapes ────────────────────────────────
// Chat rides the run machinery: a conversation IS a run row (runs.kind='chat'),
// so its LIVE VIEW reuses the existing stream verbatim — pass a Chat.id to
// getRun / getRunMessages / createRunSocket. Only the conversation-level verbs
// below (create/list/message/end/continue and the two proposal actions) are new.
// These types + the seven realApi methods mirror the PRD #39 endpoint contracts
// reconciled to M1's landed wire (Phase 3, per the wire catalog).

// A chat conversation's lifecycle reuses the run state machine
// (queued → claimed → running → … → completed/failed/cancelled); a terminal chat
// is an ended conversation (Continue starts a fresh one, Decision 11).
export type ChatStatus = RunStatus;

// Chat is the unified CLIENT VIEW of a conversation the page components consume.
// The API returns two shapes: GET /api/chats returns this shape per item (plus a
// max_turns envelope constant); POST /api/chats and .../continue return a full
// runDTO under `run`. `chatFromRun` (lib/chat.ts) maps a runDTO into this view, so
// the components never branch on which endpoint produced it. Note: max_turns is
// NOT here — it is an instance constant carried on the list envelope.
export interface Chat {
  // The chat run id. This is what the streaming machinery keys on.
  id: string;
  // First-message-derived conversation title; null until the worker derives one.
  title: string | null;
  status: ChatStatus;
  // Server-counted user turns (persisted follow_ups incl. the seeded first
  // message) — preferred over the stream-derived count for the turn-cap gate.
  turn_count: number;
  // Set when this chat continues an ended one (Decision 11); null otherwise.
  resume_of_run_id: string | null;
  // Newest message time — drives list ordering + the "last activity" label; null
  // before the worker emits one.
  last_message_at: string | null;
  created_at: string;
  updated_at: string;
}

// ChatListResponse is the GET /api/chats envelope: the conversations plus the
// instance-wide turn cap (a constant, not per-chat).
export interface ChatListResponse {
  chats: Chat[];
  max_turns: number;
}

// CreatedIssue is the confirm response (200 {issue}): the real forge issue the
// human's Create click produced. The card renders its link (https-guarded).
export interface CreatedIssue {
  iid: number;
  web_url: string;
  title: string;
}

export type ProposalStatus = "pending" | "confirmed" | "dismissed";

// IssueProposal is the payload of a `proposal`-kind run message (Decision 8): the
// chat agent's issue draft. Its title/description/labels are MODEL-authored and
// untrusted, so the card renders them as plain inert text (never Markdown, no
// clickable model links). The forge write happens only on the human's Create
// click; the created-issue link comes from the confirm response (CreatedIssue),
// NOT from this payload. The internal repo_id UUID is intentionally absent: the
// worker only handles the human-readable repo_path (Decision 7), which is what the
// card shows, and repo_path is worker-computed at emit time, so it is optional here.
export interface IssueProposal {
  id: string;
  run_id: string;
  // Worker-computed display path; absent when the worker could not resolve it.
  repo_path?: string;
  title: string;
  description: string;
  labels: string[];
  status: ProposalStatus;
  created_at: string;
}

// RunRequest is the payload of a `run_request`-kind run message (PRD #191 M5): the
// chat agent's REQUEST to start a run on an existing issue. repo_path/issue_iid are
// what the human confirms; title is a model-authored (untrusted) hint shown inert.
// Nothing is started until the human's Start click, which re-resolves and re-reads
// everything server-side (gated exactly as the board start button).
export interface RunRequest {
  repo_path: string;
  issue_iid: number;
  title?: string;
}

// CancelRequest is the payload of a `cancel_request`-kind run message (PRD #322): the
// chat agent's REQUEST to cancel a live run. run_id is UNTRUSTED and re-resolved
// server-side; nothing is cancelled until the human's Cancel click.
export interface CancelRequest {
  run_id: string;
}

// SteerRequest is the payload of a `steer_request`-kind run message (PRD #322): the
// chat agent's PROPOSED follow-up to steer a live issue run. run_id + message are
// untrusted; nothing is sent until the human reviews/edits the message and clicks Send,
// which routes to SubmitInput(follow_up) server-side.
export interface SteerRequest {
  run_id: string;
  message: string;
}

// RunSocketLike is the exact socket surface useRunStream drives — satisfied by
// a real WebSocket and by the timer-driven MockRunSocket.
export interface RunSocketLike {
  onopen: (() => void) | null;
  onmessage: ((ev: { data: string }) => void) | null;
  onclose: (() => void) | null;
  onerror: (() => void) | null;
  close(): void;
}


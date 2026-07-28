// Thin API client. All requests are same-origin (nginx proxies /api) and rely
// on the HttpOnly auth cookie, so we always send credentials. State-changing
// requests echo the readable CSRF cookie back in the X-CSRF-Token header.
//
// MOCK MODE: when built with VITE_UZI_MOCK=1 the exported `api` object and the
// run socket factory are swapped for fully in-browser implementations
// (src/mocks/*) — no request ever leaves the page. The flag is baked at build
// time, so a mock bundle physically contains no code path to a live backend.

import { mockApi } from "../mocks/mockApi";
import { MockRunSocket } from "../mocks/socket";

export const MOCK_MODE = import.meta.env.VITE_UZI_MOCK === "1";

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
  theme: string | null;
}

// UserSettingsPatch is the PATCH-like body of PUT /me/settings: a field present
// is applied (null clears it), a field absent is left unchanged — so the model
// card and the Appearance picker save independently over the one endpoint.
export interface UserSettingsPatch {
  default_model?: string | null;
  theme?: string | null;
}

// AgentTemplateScope mirrors the skill scopes (PRD #18 M6): builtin (shipped),
// global (admin, visible to all), user (self-service, owner-visible).
export type AgentTemplateScope = "builtin" | "global" | "user";

// SlackLink is the current user's own Slack linking state (PRD #25 M3), for the
// Settings → Notifications section. state is derived: unlinked (no resolved id) |
// pending (resolved, awaiting the Confirm DM) | confirmed. member_id is the manual
// override (null = rely on email auto-match); resolved_id is the effective linked
// Slack id (the override, else the cached email match).
export interface SlackLink {
  member_id: string | null;
  notify: boolean;
  resolved_id: string | null;
  confirmed: boolean;
  state: "unlinked" | "pending" | "confirmed";
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
  created_at: string;
  updated_at: string;
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
  violations: string[];
  warnings: string[];
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
  // Tier-2 opt-in (PRD #18 M5): when true, a run on this repo also unions the
  // packages from the repo's own devbox.json (packages-only). Default false.
  repo_devbox_opt_in: boolean;
  // Default-branch CI status (PRD #6), null when there is no cached default-branch
  // pipeline (no CI, MR-only pipelines, or not yet synced).
  pipeline: PipelineStatus | null;
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
  web_url: string;
  // The card's forge ("gitlab"|"forgejo"), so the UI picks the per-card MR/PR noun
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
  // The board's forge ("gitlab"|"forgejo"), so board-level chrome (the "columns are
  // <forge> labels" hint, the create-issue "opened on <forge>" note) names the right
  // platform (PRD #65 D2). A board is one repo/connection, so it is a single value.
  forge_type: string;
  columns: BoardColumn[];
  cards: Card[];
  // Repo default-branch CI status (PRD #6, the board header badge), null when
  // there is no cached default-branch pipeline.
  pipeline: PipelineStatus | null;
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
  web_url: string;
  author: string | null;
  has_prd_link: boolean;
  column: string;
  closed: boolean;
  conflict: boolean;
  description: string;
  // The issue's forge ("gitlab"|"forgejo"), so the "Open on <forge>" button names
  // the right platform (PRD #65 D2).
  forge_type: string;
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
  prd_label: string;
  autopilot_label: string;
  default_theme: string;
  // PRDLESS escape hatch (PRD #22). prdless_enabled is the text "true"/"false"
  // (the API serves every setting as a string); prdless_label is the label name.
  prdless_enabled: string;
  prdless_label: string;
  // Slack integration non-secret keys (PRD #25). slack_enabled is the text
  // "true"/"false"; public_base_url is the http(s) base for deep links in Slack
  // messages. The two Slack TOKENS are secret and never returned here — see
  // `secrets` on SettingsResponse.
  slack_enabled: string;
  public_base_url: string;
  // Run-judge keys (PRD #46). judge_enabled is the global kill-switch (text
  // "true"/"false"); judge_model is the cheap model alias the judge runs on. The
  // self-improvement keys are engine-managed and NOT surfaced here.
  judge_enabled: string;
  judge_model: string;
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
};

// Compiled-in label defaults, mirroring the API's settings package. The SPA uses
// them until the session bootstrap resolves the configured values (PRD #19 M2,
// PRD #22 for prdless).
export const DEFAULT_PRD_LABEL = "PRD";
export const DEFAULT_AUTOPILOT_LABEL = "autopilot";
export const DEFAULT_PRDLESS_LABEL = "PRDLESS";

// SessionResponse is the auth/session bootstrap body (login, register, me). It
// carries the user, the instance forge labels the board and issue-creation UI
// need before their first call (PRD #19 M2), the three theme fields the
// Appearance picker needs (PRD #21: resolved theme, the user's raw override with
// null = none, and the instance default), and the prdless fields (PRD #22,
// optional: a server that predates them omits both and the SPA treats the feature
// as off).
export interface SessionResponse {
  user: User;
  prd_label: string;
  autopilot_label: string;
  theme: string;
  theme_override: string | null;
  default_theme: string;
  prdless_label?: string;
  prdless_enabled?: boolean;
  // Vault status (PRD #32): whether the user's per-user secret vault is unlocked
  // in the server process. Optional so a server that predates the field reads as
  // unlocked (no banner, legacy behavior) rather than falsely locked. `exists`
  // (PRD #45) is whether a vault row exists at all; with has_password it lets a
  // passwordless user's SPA pick the passphrase-create dialog vs the unlock banner.
  vault?: { unlocked: boolean; exists?: boolean };
  // has_password is false for OIDC-only users (NULL password_hash; PRD #45). Absent
  // (older server, or a password user) reads as true — no passphrase-create dialog.
  has_password?: boolean;
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
export type CliAuthStatus = "pending" | "approved" | "denied" | "consumed" | "expired";

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
export interface Memory {
  id: string;
  repo_id: string;
  repo_name: string;
  title: string;
  body: string;
  run_id?: string;
  created_at: string;
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
  docker?: boolean;
  busy: boolean; // derived: holds a claimed/running/awaiting_approval run (== active_runs > 0)
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
  upgrade_status: "up_to_date" | "outdated" | "unknown" | "upgrading" | "upgrade_failed";
  upgrade_detail: string | null;
  // The coordinate this worker was compared AGAINST: the controller's rolled tag for a
  // hosted worker with a fresh report, otherwise the control plane's own version. "" when
  // the control plane has no version stamp (classification off).
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
  | "completed"
  | "failed"
  | "cancelled";

// TERMINAL_RUN_STATUSES mirrors the DB CHECK: a run in any of these is finished.
export const TERMINAL_RUN_STATUSES: RunStatus[] = ["completed", "failed", "cancelled"];

export function isTerminalRun(status: string): boolean {
  return (TERMINAL_RUN_STATUSES as string[]).includes(status);
}

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
export type StopKind = "cancelled" | "plan_rejected" | "auto_stopped";

// RunHealth is the server-side run-health flag (PRD #47): a non-terminal,
// self-clearing signal that a run looks slow, stuck, or looping. "ok" is the
// healthy default. It is orthogonal to RunStatus and never kills a run — the
// existing timeouts remain the only liveness backstops. runBadge renders the warn
// variant only while the run is in a flaggable status.
/** The judge's verdict on a run (PRD #46). Mirrors run_reviews.verdict's CHECK. */
export type JudgeVerdict = "ideal" | "ok" | "issues";

export type RunHealth =
  | "ok"
  | "stalled"
  | "looping"
  | "slow"
  | "waiting_worker"
  | "approval_idle";

// FixVerdict is a ci_fix run's outcome (PRD #6): verified/fix_failed are stamped
// server-side from the post-fix pipeline; not_code is the agent's "not a code
// problem" verdict; null means the fix is not yet verified.
export type FixVerdict = "verified" | "fix_failed" | "not_code";

export interface Run {
  id: string;
  /** Nullable since PRD #39: a chat run has no repo (issue/ci_fix runs always do). */
  repo_id: string | null;
  /** The run's forge ("gitlab"|"forgejo"), for the per-run MR/PR noun + reference
   *  sigil (PRD #65 D2). "" on worker/create DTO paths (no browser MR affordance);
   *  set on the list/detail reads. The web defaults an empty/unknown value to
   *  GitLab's vocabulary. */
  forge_type: string;
  /** Run kind (PRD #6): "issue" works issue_iid's card; "ci_fix" fixes a failed
   *  pipeline (pipeline_ref/pipeline_web_url/fix_verdict below); "chat" (PRD #39). */
  kind: string;
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
  /** PRD #19: an autopilot run (poller-started, plan auto-approved). Drives the
   *  "autopilot" badge; a manually-started run is false. */
  auto_approve: boolean;
  worker_id: string | null;
  branch: string | null;
  mr_iid: number | null;
  /** Forge-supplied MR/PR web URL persisted by the worker at creation (PRD #65 D8),
   *  null on runs created before it landed. Rendered directly through isHttpsUrl; a
   *  null falls back to the legacy GitLab reconstruction (forgeUrls.ts). */
  mr_web_url: string | null;
  /** Last MR state the PRD #24 watcher observed for mr_iid
   *  (opened|closed|merged|locked), null when never observed. Display-only hint
   *  (PRD #33); frozen per run, so a superseded run's value can be stale. */
  mr_state: string | null;
  failure_reason: string | null;
  /** Server-stamped stop signal (PRD #33, widened by #108 M5): "cancelled" or
   *  "plan_rejected" (human), "auto_stopped" (server), null otherwise. isStoppedRun
   *  reads this, not failure_reason — and treats only the two human kinds as calm. */
  stop_kind: StopKind | null;
  /** Run-health flag (PRD #47). This owner-scoped DTO carries health_reason
   *  unconditionally (the run view is owner/admin only); health_since drives the
   *  header's "stuck for Xm". */
  health: RunHealth;
  health_reason: string | null;
  health_since: string | null;
  /** ci_fix (PRD #6): the failing ref, the failing pipeline's web URL (from the
   *  snapshot), and the fix verdict. All null on an issue run. */
  pipeline_ref: string | null;
  pipeline_web_url: string | null;
  fix_verdict: FixVerdict | null;
  plan_md: string | null;
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

// Which source produced the reading: the free usage endpoint, or the ~1-token
// header probe fallback (Decision 2).
export type RateLimitSource = "usage_endpoint" | "header_probe";

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
 *  migration 00089's CHECK in SQL; selectReasonMatchesMigration in
 *  runCredential.test.ts parses that migration and pins the three in step.
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
  | "improve_uzi";

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
export type JudgeBacklogBucket = "todo" | "filed" | "done" | "dismissed" | "all";

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
  // unrecognised value falls through to the plain "✓ Done" chip rather than mis-labelling —
  // but the guarantee lives in Go's two SQL writers (the M6 literal and the human-write
  // NULL), not in this declaration. Widen the union here when a third value is added there.
  set_via?: "issue_close";
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

// ── Self-improvement config (PRD #46 M5) ─────────────────────────────────────
// The admin-facing view of the autonomous self-improvement job. repo_path /
// user_email are display-only resolutions of the ids; last_run_at is the durable
// cadence gate; active reports whether a cycle is currently in flight.
export interface SelfimproveConfig {
  enabled: boolean;
  interval: string;
  repo_id: string | null;
  repo_path: string | null;
  user_id: string | null;
  user_email: string | null;
  last_run_at: string | null;
  active: boolean;
}

// SelfimproveUpdate enables/disables and configures the job. The enabling admin
// becomes the run owner server-side (from the session, never sent here — audit H3).
export interface SelfimproveUpdate {
  enabled: boolean;
  interval?: string;
  repo_id?: string | null;
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
export type RunInputKind = "follow_up" | "approve_plan" | "reject_plan" | "revise_plan" | "cancel" | "answer";

// SteerInput is one follow_up steer-queue entry (PRD #95), from GET /api/runs/{id}/
// inputs. Delivery status is derived client-side: consumed_at null ⇒ Queued (the worker
// has not drained it), set ⇒ Delivered (handed to the worker for its next turn). body is
// nullable to match the wire, though a follow_up always carries one.
export interface SteerInput {
  id: number;
  body: string | null;
  created_at: string;
  consumed_at: string | null;
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

// runSocketUrl builds the same-origin WebSocket URL for a run. The HttpOnly auth
// cookie rides along automatically (same origin through nginx); Origin==Host is
// enforced server-side against cross-site hijacking.
export function runSocketUrl(runId: string): string {
  const proto = window.location.protocol === "https:" ? "wss:" : "ws:";
  return `${proto}//${window.location.host}/api/ws?run=${encodeURIComponent(runId)}`;
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

// createRunSocket is the single place a run socket is constructed, so mock mode
// swaps the transport without touching the streaming hook.
export function createRunSocket(runId: string): RunSocketLike {
  if (MOCK_MODE) return new MockRunSocket(runId);
  // A real WebSocket is runtime-compatible with RunSocketLike (the hook's
  // handlers simply ignore the extra Event arguments); the cast bridges the
  // nominal handler types.
  return new WebSocket(runSocketUrl(runId)) as unknown as RunSocketLike;
}

// isHttpsUrl guards rendering forge-supplied URLs as links: only https URLs are
// turned into anchors, so a hostile or malformed web_url (e.g. javascript:) is
// never made clickable.
export function isHttpsUrl(url: string | null | undefined): boolean {
  return typeof url === "string" && url.startsWith("https://");
}

// preferForgeUrl chooses the forge-supplied persisted MR/PR URL when it is a usable
// https URL (PRD #65 D8 — the only correct link on Forgejo, whose PR URL grammar the
// legacy GitLab reconstruction never knew), else the caller's legacy reconstruction.
// The persisted value is WORKER-supplied and stored without scheme validation, so
// routing it through isHttpsUrl here is the load-bearing guard: a hostile http: or
// javascript: mr_web_url is rejected and never becomes an anchor href. Shared by
// every MR-link surface so the guard can never be forgotten at one of them.
export function preferForgeUrl(persisted: string | null | undefined, legacy: string | null): string | null {
  return isHttpsUrl(persisted) ? persisted! : legacy;
}

export class ApiError extends Error {
  status: number;
  // body is the full parsed error payload, so a caller can read structured
  // fields beyond the message (e.g. a 422's `violations` array).
  body: unknown;
  constructor(status: number, message: string, body: unknown = null) {
    super(message);
    this.status = status;
    this.body = body;
    this.name = "ApiError";
  }
}

function readCookie(name: string): string | null {
  const match = document.cookie.match(new RegExp("(?:^|; )" + name + "=([^;]*)"));
  return match ? decodeURIComponent(match[1]) : null;
}

// ── Global 401 handling ─────────────────────────────────────────────────────
// Every authenticated request funnels its 401s through one app-registered
// handler, so an expired or absent session is handled centrally — AuthContext
// clears the user, and ProtectedRoute then redirects to /login — instead of each
// page inventing its own 401 string. It fires inside request() before the error
// propagates, so even a 401 the caller swallows (the board's background poll)
// still trips it. Clearing the session (not an imperative redirect) is what
// composes safely: the initial me() probe's expected 401 just clears an already
// -empty session and never bounces a signed-out visitor off a public page.
type UnauthorizedHandler = () => void;
let unauthorizedHandler: UnauthorizedHandler | null = null;
export function setUnauthorizedHandler(handler: UnauthorizedHandler | null): void {
  unauthorizedHandler = handler;
}

// ── Global vault-locked handling (PRD #32) ────────────────────────────────────
// A save that races a pod restart comes back 409 with body {code:"vault_locked"}.
// Like the 401 path, one app-registered handler (AuthContext refreshes the
// session, so the SPA learns the vault is locked and shows the unlock banner)
// fires inside request() before the error propagates, so even a caller that
// swallows the 409 still trips the refresh.
type VaultLockedHandler = () => void;
let vaultLockedHandler: VaultLockedHandler | null = null;
export function setVaultLockedHandler(handler: VaultLockedHandler | null): void {
  vaultLockedHandler = handler;
}

// isVaultLocked reports whether an error is the 409 vault_locked signal, so a
// caller (the secrets form) can show a tailored "unlock first" message.
export function isVaultLocked(err: unknown): boolean {
  return (
    err instanceof ApiError &&
    err.status === 409 &&
    (err.body as { code?: string } | null)?.code === "vault_locked"
  );
}

async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  const headers: Record<string, string> = {};
  if (body !== undefined) {
    headers["Content-Type"] = "application/json";
  }
  if (method !== "GET" && method !== "HEAD") {
    const csrf = readCookie("uzi_csrf");
    if (csrf) headers["X-CSRF-Token"] = csrf;
  }

  const res = await fetch(`/api${path}`, {
    method,
    headers,
    credentials: "same-origin",
    body: body !== undefined ? JSON.stringify(body) : undefined,
  });

  let payload: unknown = null;
  const text = await res.text();
  if (text) {
    try {
      payload = JSON.parse(text);
    } catch {
      payload = null;
    }
  }

  if (!res.ok) {
    if (res.status === 401) unauthorizedHandler?.();
    if (res.status === 409 && (payload as { code?: string } | null)?.code === "vault_locked") {
      vaultLockedHandler?.();
    }
    const message =
      (payload as { error?: string } | null)?.error ?? `request failed (${res.status})`;
    throw new ApiError(res.status, message, payload);
  }
  return payload as T;
}

const realApi = {
  register: (email: string, password: string, displayName: string) =>
    request<SessionResponse>("POST", "/auth/register", {
      email,
      password,
      display_name: displayName,
    }),
  login: (email: string, password: string) =>
    request<SessionResponse>("POST", "/auth/login", { email, password }),
  authConfig: () => request<AuthConfig>("GET", "/auth/config"),
  // Server build version (Model B: the release git tag; "dev" on a local build).
  // Unauthenticated, like /health — the shell footer reads it.
  version: () => request<{ version: string }>("GET", "/version"),
  // The Workers nav badge's count (PRD #113 M6). Its own endpoint rather than a fold over
  // listWorkers: the Workers page's poll is page-local and visibility-gated, so a badge
  // fed from it would be stale or absent exactly when the operator is not on that page,
  // which is the only situation a nav badge exists for.
  workerUpgradeSummary: () =>
    request<{ attention: number; target_release: string }>("GET", "/me/workers/upgrade-summary"),
  logout: () => request<{ status: string }>("POST", "/auth/logout"),
  me: () => request<SessionResponse>("GET", "/auth/me"),
  listUsers: () => request<{ users: User[] }>("GET", "/admin/users"),
  setUserActive: (id: string, isActive: boolean) =>
    request<{ user: User }>("PATCH", `/admin/users/${id}`, { is_active: isActive }),
  // Admin per-user run-judge toggle (PRD #46): force any user's opt-in. Actor is
  // admin (route-gated); target is the path id, never the body. Returns the user.
  setUserJudgeEnabled: (id: string, enabled: boolean) =>
    request<{ user: User }>("PUT", `/admin/users/${id}/judge`, { enabled }),
  getSettings: () => request<SettingsResponse>("GET", "/admin/settings"),
  updateSettings: (settings: UpdateSettingsPayload) =>
    request<SettingsResponse>("PUT", "/admin/settings", { settings }),
  // Vault migration progress (PRD #32): count of stored secrets still master-sealed
  // (owners who have not unlocked since the vault rolled out). Admin-only.
  vaultMigration: () => request<{ master_sealed: number }>("GET", "/admin/vault-migration"),
  // Self-improvement config (PRD #46 M5). Admin-only. update sets the enabling admin
  // as the run owner from the session (never the body).
  getSelfimprove: () => request<{ selfimprove: SelfimproveConfig }>("GET", "/admin/selfimprove"),
  updateSelfimprove: (input: SelfimproveUpdate) =>
    request<{ selfimprove: SelfimproveConfig }>("PUT", "/admin/selfimprove", input),
  // Flip the current user's autopilot opt-in (PRD #19 M3). Returns the updated user.
  setAutopilotEnabled: (enabled: boolean) =>
    request<{ user: User }>("PUT", "/me/autopilot", { enabled }),
  // Flip the current user's run-judge opt-in (PRD #46). Session identity only —
  // the body carries no user id. Returns the updated user.
  // enabled is required; anthropicToken is the three-way token field (PRD #104 M4):
  // omitted leaves the binding alone, null clears it back to the default, a label
  // binds it. Omitting it is what every pre-#104 caller did, and must stay a no-op
  // on the binding.
  setJudgeEnabled: (enabled: boolean, anthropicToken?: string | null) =>
    request<{ user: User }>(
      "PUT",
      "/me/judge",
      anthropicToken === undefined
        ? { enabled }
        : { enabled, anthropic_token: anthropicToken },
    ),
  listSecrets: () => request<{ secrets: SecretMeta[] }>("GET", "/me/secrets"),
  // PRD #104 M2 token CRUD. create/rename/set-default/rotate/delete are all
  // cookie-only (D8) — the SPA is the only client that can reach them.
  createAnthropicToken: (token: string, label: string, isDefault: boolean) =>
    request<{ secret: SecretMeta }>("POST", "/me/secrets/anthropic_token", {
      token,
      label,
      default: isDefault,
    }),
  // PATCH carries only the fields being changed: label renames, default promotes
  // (false is refused server-side — promote another instead), token rotates.
  patchAnthropicToken: (
    id: string,
    body: { label?: string; default?: boolean; token?: string },
  ) => request<{ secret: SecretMeta }>("PATCH", `/me/secrets/anthropic_token/${id}`, body),
  // The auto-selection pool toggle (PRD #111 M2, D13). Its OWN narrow route, not a
  // field on the PATCH above: every other secrets write is cookie-only because a
  // Bearer-reachable mint would let a stolen CLI token replace a user's credentials,
  // and moving that PATCH to reach this toggle would have taken rename, rotate and
  // set-default along with it.
  setTokenAutoEligible: (id: string, autoEligible: boolean) =>
    request<{ secret: SecretMeta }>("PATCH", `/me/secrets/anthropic_token/${id}/auto-eligible`, {
      auto_eligible: autoEligible,
    }),
  deleteAnthropicTokenById: (id: string) =>
    request<null>("DELETE", `/me/secrets/anthropic_token/${id}`),
  putAnthropicToken: (token: string) =>
    request<{ secret: SecretMeta }>("PUT", "/me/secrets/anthropic_token", { token }),
  deleteAnthropicToken: () => request<null>("DELETE", "/me/secrets/anthropic_token"),

  // Vault (PRD #32): unlock re-derives the DEK from the login password (204, or
  // 403 on a wrong password); lock evicts it; status is a lightweight poll. Unlock
  // and lock return no body.
  vaultUnlock: (password: string) => request<null>("POST", "/vault/unlock", { password }),
  // Create a passwordless (OIDC) user's vault from a chosen passphrase (PRD #45).
  // Create-only: 409 if a vault already exists; 204 on success (vault then unlocked).
  vaultCreatePassphrase: (passphrase: string) =>
    request<null>("POST", "/vault/passphrase", { passphrase }),
  vaultLock: () => request<null>("POST", "/vault/lock"),
  vaultStatus: () => request<{ unlocked: boolean }>("GET", "/vault/status"),
  getMySettings: () => request<{ settings: UserSettings }>("GET", "/me/settings"),
  putMySettings: (patch: UserSettingsPatch) =>
    request<{ settings: UserSettings }>("PUT", "/me/settings", patch),
  // Slack linking (PRD #25 M3), own-user only. member_id null clears the override
  // (falls back to email auto-match). A 409 from setMySlackOverride means the id is
  // already linked to another account.
  getMySlack: () => request<{ slack: SlackLink }>("GET", "/me/slack"),
  setMySlackNotify: (notify: boolean) =>
    request<{ slack: SlackLink }>("PUT", "/me/slack/notify", { notify }),
  setMySlackOverride: (memberId: string | null) =>
    request<{ slack: SlackLink }>("PUT", "/me/slack/override", { member_id: memberId }),
  testMySlackDM: () => request<{ status: string }>("POST", "/me/slack/test-dm"),
  // Just the live Slack socket state, for the admin chip's poll (PRD #25 M3).
  getSlackStatus: () => request<{ slack_status: string }>("GET", "/admin/slack/status"),
  listAgentTemplates: () =>
    request<{ templates: AgentTemplate[] }>("GET", "/agent-templates"),
  getTemplateAllocations: () =>
    request<{ templates: TemplateAllocation[] }>("GET", "/agent-templates/allocations"),
  setTemplateAllocations: (input: TemplateAllocationsInput) =>
    request<{ templates: TemplateAllocation[] }>("PUT", "/agent-templates/allocations", input),
  getAgentTemplate: (id: string) =>
    request<{ template: AgentTemplate }>("GET", `/agent-templates/${id}`),
  createAgentTemplate: (input: AgentTemplateInput) =>
    request<{ template: AgentTemplate }>("POST", "/agent-templates", input),
  updateAgentTemplate: (id: string, input: AgentTemplateInput) =>
    request<{ template: AgentTemplate }>("PUT", `/agent-templates/${id}`, input),
  deleteAgentTemplate: (id: string) =>
    request<null>("DELETE", `/agent-templates/${id}`),
  resetAgentTemplate: (id: string) =>
    request<{ template: AgentTemplate }>("POST", `/agent-templates/${id}/reset`),

  // Agent skills (PRD #16).
  listSkills: () => request<{ skills: Skill[] }>("GET", "/skills"),
  getSkill: (id: string) => request<{ skill: Skill }>("GET", `/skills/${id}`),
  createSkill: (input: SkillCreateInput) =>
    request<{ skill: Skill }>("POST", "/skills", input),
  updateSkill: (id: string, input: SkillUpdateInput) =>
    request<{ skill: Skill }>("PUT", `/skills/${id}`, input),
  deleteSkill: (id: string) => request<null>("DELETE", `/skills/${id}`),
  resetSkill: (id: string) => request<{ skill: Skill }>("POST", `/skills/${id}/reset`),
  getTemplateSkills: (id: string) =>
    request<{ allocations: TemplateSkills }>("GET", `/agent-templates/${id}/skills`),
  setTemplateSkills: (id: string, input: AllocationsInput) =>
    request<{ allocations: TemplateSkills }>("PUT", `/agent-templates/${id}/skills`, input),

  // Tool allowlist + per-repo tool profiles (PRD #18 M4). The allowlist is readable
  // by any user (the repo picker needs it); writes are admin-only. A repo's profile
  // is owner-only.
  listToolAllowlist: () =>
    request<{ allowlist: ToolAllowlistEntry[] }>("GET", "/tool-allowlist"),
  createToolAllowlistEntry: (input: ToolAllowlistWriteInput) =>
    request<{ entry: ToolAllowlistEntry }>("POST", "/tool-allowlist", input),
  updateToolAllowlistEntry: (id: string, input: ToolAllowlistWriteInput) =>
    request<{ entry: ToolAllowlistEntry }>("PUT", `/tool-allowlist/${id}`, input),
  deleteToolAllowlistEntry: (id: string) =>
    request<null>("DELETE", `/tool-allowlist/${id}`),
  getRepoToolProfile: (repoId: string) =>
    request<{ packages: string[] }>("GET", `/repos/${repoId}/tool-profile`),
  setRepoToolProfile: (repoId: string, packages: string[]) =>
    request<{ packages: string[] }>("PUT", `/repos/${repoId}/tool-profile`, { packages }),

  // Forge integration.
  forgeConfig: () => request<ForgeConfig>("GET", "/forge/config"),
  listConnections: () => request<{ connections: ForgeConnection[] }>("GET", "/forge/connections"),
  createConnection: (baseUrl: string, token: string, forgeType = "gitlab") =>
    request<{ connection: ForgeConnection }>("POST", "/forge/connections", {
      base_url: baseUrl,
      token,
      forge_type: forgeType,
    }),
  verifyConnection: (id: string) =>
    request<{ connection: ForgeConnection }>("POST", `/forge/connections/${id}/verify`),
  // Set (or clear, with "") the connecting user's own forge username for autopilot
  // attribution. The API best-effort-verifies it and may return a `warning` while
  // still saving (verified-or-warned, PRD #19 M3).
  updateConnection: (id: string, humanUsername: string) =>
    request<{ connection: ForgeConnection; warning?: string }>("PUT", `/forge/connections/${id}`, {
      human_username: humanUsername,
    }),
  privilegeCheck: (id: string) =>
    request<{ report: PrivilegeReport }>("POST", `/forge/connections/${id}/privilege-check`),
  deleteConnection: (id: string) => request<null>("DELETE", `/forge/connections/${id}`),
  listProjects: (connectionId: string) =>
    request<{ repos: Repo[] }>("GET", `/forge/connections/${connectionId}/projects`),

  listRepos: () => request<{ repos: Repo[] }>("GET", "/repos"),
  setRepoEnabled: (id: string, enabled: boolean) =>
    request<{ repo: Repo }>("PUT", `/repos/${id}`, { enabled }),
  setRepoSkillsEnabled: (id: string, enabled: boolean) =>
    request<{ repo: Repo }>("PATCH", `/repos/${id}`, { repo_skills_enabled: enabled }),
  // Tier-2 repo devbox.json opt-in (PRD #18 M5). Owner or admin.
  setRepoDevboxOptIn: (id: string, enabled: boolean) =>
    request<{ repo: Repo }>("PATCH", `/repos/${id}`, { repo_devbox_opt_in: enabled }),

  getBoard: (repoId: string) => request<{ board: Board }>("GET", `/repos/${repoId}/board`),
  configureColumns: (repoId: string, columns: { label_name: string }[]) =>
    request<{ board: Board }>("PUT", `/repos/${repoId}/board/columns`, { columns }),
  // Replace the board's manual card order wholesale (PRD #102 M5). `iids` is the
  // board-GLOBAL order of every non-closed card, not just the column that changed:
  // the drop freezes the whole displayed order before it moves anything, so a card
  // sorted by something other than issue number does not re-sort under the user's
  // hand. Returns the authoritative board, which the caller adopts wholesale.
  reorderBoard: (repoId: string, iids: number[]) =>
    request<{ board: Board }>("PUT", `/repos/${repoId}/board/order`, { iids }),
  getIssue: (repoId: string, iid: number) =>
    request<{ issue: IssueDetail }>("GET", `/repos/${repoId}/issues/${iid}`),
  moveIssue: (repoId: string, iid: number, toColumn: string) =>
    request<{ card: Card }>("POST", `/repos/${repoId}/issues/${iid}/move`, { to_column: toColumn }),
  // Apply/remove the PRDLESS label from the UI (PRD #22 M4). Forge-first, so the
  // returned card is authoritative — the caller replaces its card with it (no
  // optimistic update).
  setIssuePrdless: (repoId: string, iid: number, apply: boolean) =>
    request<{ card: Card }>("POST", `/repos/${repoId}/issues/${iid}/prdless`, { apply }),
  // Promote a non-PRD issue by adding the PRD label (PRD #102 M6, Decision 15).
  // Forge-first and apply-only — there is no demote, so no boolean. The returned
  // card is authoritative, like the prdless toggle above.
  promoteIssue: (repoId: string, iid: number) =>
    request<{ card: Card }>("POST", `/repos/${repoId}/issues/${iid}/promote`),
  syncRepo: (repoId: string) => request<{ board: Board }>("POST", `/repos/${repoId}/sync`),
  createIssue: (repoId: string, title: string, description: string) =>
    request<{ card: Card }>("POST", `/repos/${repoId}/issues`, { title, description }),

  // Agent runtime (PRD #4).
  listWorkers: () => request<{ workers: Worker[] }>("GET", "/workers"),
  createWorker: (name: string, template?: string) =>
    request<{ worker: Worker; token: string }>("POST", "/workers", { name, template }),
  deleteWorker: (id: string) => request<null>("DELETE", `/workers/${id}`),
  // Point a worker at one of the caller's named tokens, or clear the binding with
  // null so it falls back to the default (PRD #104 M3). Takes a LABEL, not an id —
  // the name is what a human picks. Lands on the worker's NEXT claim: no restart,
  // no re-minted join token.
  /** Set HOW a worker chooses its Anthropic credential (PRD #111 M3). The mode and
   *  the label travel together because the server refuses a contradictory pair
   *  (a label with "default"/"auto", or "pinned" with none) rather than silently
   *  reconciling it — either winner would spend a credential the caller did not
   *  ask for. */
  setWorkerBindMode: (id: string, mode: BindMode, label: string | null) =>
    request<{ worker: Worker }>("PATCH", `/workers/${id}`, {
      anthropic_bind_mode: mode,
      anthropic_token: mode === "pinned" ? label : null,
    }),

  // Hosted workers (PRD #58). Deletion rides deleteWorker above — the route is
  // kind-blind on purpose, so there is no hosted delete to add here.
  hostedConfig: () => request<HostedConfig>("GET", "/workers/hosted/config"),
  /**
   * Provision a hosted worker: one the CONTROLLER runs in the cluster.
   *
   * Returns `{ worker }` and NO TOKEN, unlike createWorker above — and the return
   * type says so on purpose. A hosted worker's join token has exactly one consumer,
   * the controller, which collects it from its desired-state poll; the user is never
   * in that path, so there is nothing to show and nothing to copy (Decision 3). The
   * server cannot send one either — provisionHostedWorker's transaction returns no
   * token at all. Reading `.token` off this call is a typecheck failure, which is
   * the point: the sibling createWorker flow twenty lines away renders a prominent
   * one-time-token card, and it must not be copied onto this path.
   *
   * template and size are mandatory (400 otherwise), unlike createWorker's optional
   * template: we run the image, so a silent default would pick it for the user.
   * name is optional — empty means the server derives one from template + size.
   *
   * docker (PRD #83 M3) opts the worker into a rootless Docker-in-Docker sidecar so its
   * agent can run docker/docker compose. It rides ahead of the rarely-used name (the
   * form sets docker, never name) and is always sent as an explicit bool: absent reads
   * as false server-side, but sending it keeps the request self-describing.
   */
  provisionHostedWorker: (template: string, size: string, docker = false, name?: string) =>
    request<{ worker: Worker }>("POST", "/workers/hosted", { template, size, name, docker }),

  createRun: (repoId: string, issueIid: number) =>
    request<{ run: Run }>("POST", `/repos/${repoId}/runs`, { issue_iid: issueIid }),
  /** Queue a CI-fix run for a failed pipeline on a watched ref (PRD #6). */
  createCIFixRun: (repoId: string, ref: string) =>
    request<{ run: Run }>("POST", `/repos/${repoId}/ci-fix-runs`, { ref }),
  listRuns: (params?: { repoId?: string; issueIid?: number }) => {
    const q = new URLSearchParams();
    if (params?.repoId) q.set("repo_id", params.repoId);
    if (params?.issueIid != null) q.set("issue_iid", String(params.issueIid));
    const qs = q.toString();
    return request<{ runs: RunListItem[] }>("GET", qs ? `/runs?${qs}` : "/runs");
  },
  getRun: (id: string) => request<{ run: Run }>("GET", `/runs/${id}`),
  /** The caller's own token/cost usage (PRD #40): lifetime + last-7-days + run count. */
  getUsage: () => request<SelfUsage>("GET", "/usage"),
  /** Factory-wide usage + per-user breakdown (PRD #40). Admin-only — a non-admin 403s. */
  getAdminUsage: () => request<AdminUsage>("GET", "/admin/usage"),
  /** The caller's own Claude rate-limit reading (PRD #53): the two windows, or a
   *  no_token / unavailable status. Percentages only — the token never leaves the api. */
  getMyRateLimits: () => request<MyRateLimitsResponse>("GET", "/me/rate-limits"),
  /** Every user's rate-limit reading (PRD #53). Admin-only — a non-admin 403s. */
  getAdminRateLimits: () => request<AdminRateLimits>("GET", "/admin/rate-limits"),
  getRunMessages: (id: string, afterSeq = 0) =>
    request<{ messages: RunMessage[] }>(
      "GET",
      afterSeq > 0 ? `/runs/${id}/messages?after=${afterSeq}` : `/runs/${id}/messages`,
    ),
  // The run's follow_up steer queue with delivery status (PRD #95). Owner-only: a
  // non-owner (incl. admin_ro) gets 404, which the caller treats as "no queue".
  getRunInputs: (id: string) => request<{ inputs: SteerInput[] }>("GET", `/runs/${id}/inputs`),
  // A follow_up write returns the created row's id + created_at (PRD #95 S2) so the
  // web's optimistic queue entry adopts the real id and reconciles; other kinds omit
  // them (they are server-side or own their own UI). Both fields optional on the wire.
  submitRunInput: (id: string, kind: RunInputKind, body = "", selection?: AgentSelectionInput) =>
    request<{ server_side: boolean; id?: number; created_at?: string }>("POST", `/runs/${id}/inputs`, {
      kind,
      body,
      // PRD #37: the structured agent selection is legal only on approve_plan; the
      // server ignores/validates it per kind. Omitted entirely when absent so a
      // plain follow-up/cancel body is unchanged.
      ...(selection ? { selection } : {}),
    }),

  // ── Run judge review (PRD #46 M4) ──────────────────────────────────────────
  // getRunReview reads the verdict + recommendations for the run page (owner-or-
  // admin scoped server-side); review is null for a visible-but-unjudged run.
  // rerunJudge enqueues a fresh judge for a terminal run (owner-only spend), behind
  // the per-user spend limiter; the new verdict arrives asynchronously once the
  // judge run finishes, so callers re-fetch getRunReview.
  getRunReview: (id: string) => request<{ review: RunReview | null }>("GET", `/runs/${id}/review`),
  rerunJudge: (id: string) => request<{ run: Run }>("POST", `/runs/${id}/rejudge`),

  // ── File a forge issue from a recommendation (PRD #68) ──────────────────────
  // getIssueDraft reads the server-templated draft (owner-or-admin, no write, no token
  // spend). fileIssue is the forge write (cookie+CSRF, per-user forge limiter): 201
  // {issue, warning?} — warning is set when the issue was created but its local link/
  // cache could not be settled (created-with-warning), a success, never a retry signal.
  getIssueDraft: (runId: string, recId: string) =>
    request<{ draft: IssueDraft }>(
      "GET",
      `/runs/${runId}/review/recommendations/${recId}/issue-draft`,
    ),
  fileIssue: (runId: string, recId: string, body: { repo_id: string; title: string; description: string }) =>
    request<{ issue: CreatedIssue; warning?: string }>(
      "POST",
      `/runs/${runId}/review/recommendations/${recId}/issue`,
      body,
    ),

  // ── Triage a recommendation (PRD #94) ───────────────────────────────────────
  // setDisposition upserts the coordinate row (RequireUser, owner-only, no token
  // spend, no forge write): reason is REQUIRED iff dismissed and MUST be omitted for
  // done (the server 400s otherwise), so it is dropped from the body on done.
  // Idempotent — re-clicking is last-writer-wins. Returns 204 (no body).
  setDisposition: (
    runId: string,
    recId: string,
    status: "done" | "dismissed",
    reason?: "wont_do" | "not_an_issue",
  ) =>
    request<null>(
      "PUT",
      `/runs/${runId}/review/recommendations/${recId}/disposition`,
      status === "dismissed" ? { status, reason } : { status },
    ),
  // deleteDisposition is Undo: it clears the coordinate row (204). A 404 means there
  // was nothing to undo (already cleared, or a concurrent undo) — that is a SUCCESS,
  // not a loud error, so it is swallowed to null; any other status propagates.
  deleteDisposition: async (runId: string, recId: string): Promise<null> => {
    try {
      return await request<null>(
        "DELETE",
        `/runs/${runId}/review/recommendations/${recId}/disposition`,
      );
    } catch (e) {
      if (e instanceof ApiError && e.status === 404) return null;
      throw e;
    }
  },
  // getJudgeStats is the global "across all your runs" backlog tally (RequireUser,
  // owner-scoped, all-time). It DELIBERATELY ignores any list filter — it is a global
  // backlog, not the filtered view — and is bucketed by the same Go ladder as `triage`.
  // Feeds the Judge nav badge (via .todo) and the /runs list strip's successor.
  getJudgeStats: () => request<TriageCounts>("GET", "/me/judge/stats"),

  // ── Judge menu — cross-run backlog + bulk disposition (PRD #98) ─────────────
  // getJudgeBacklog reads the deduped, grouped backlog (RequireUser, owner-scoped, no
  // token spend). bucket filters the group ROLLUP (default todo server-side); run is
  // the notification deep-link anchor (/judge?run={id}) — it keeps groups recurring in
  // that run while preserving their other-run occurrences. `triage` in the response is
  // the canonical count; render it, never re-derive from `groups`.
  getJudgeBacklog: (bucket?: JudgeBacklogBucket, run?: string) => {
    const qs = new URLSearchParams();
    if (bucket) qs.set("bucket", bucket);
    if (run) qs.set("run", run);
    const suffix = qs.toString();
    return request<JudgeBacklog>("GET", `/me/judge/recommendations${suffix ? `?${suffix}` : ""}`);
  },
  // bulkSetJudgeDisposition fans one verdict out to every member coordinate of the
  // given groups (RequireUser, owner-only, idempotent, no token spend, no forge write).
  // reason is REQUIRED iff dismissed and MUST be omitted for done (mirrors the per-rec
  // route). scope defaults to "open" (settle only todo members; leave settled ones).
  // Returns updated count + the acted-on groups re-read at bucket=all + the recomputed
  // triage — enough to update rows AND the badge from one round-trip.
  bulkSetJudgeDisposition: (
    items: JudgeDispositionCoord[],
    status: "done" | "dismissed",
    reason?: "wont_do" | "not_an_issue",
    scope: JudgeDispositionScope = "open",
  ) =>
    request<JudgeDispositionResult>("PUT", "/me/judge/recommendations/disposition", {
      items,
      status,
      scope,
      ...(status === "dismissed" ? { reason } : {}),
    }),

  // ── Chat (PRD #39) — reconciled to M1's landed wire (Phase 3) ───────────────
  // The live view (messages, WS, replay) reuses getRun/getRunMessages/
  // createRunSocket with the chat's id — only these conversation verbs are new.
  // create/continue return a full runDTO under `run`; the list returns the Chat
  // view shape per item plus the max_turns envelope constant.
  listChats: () => request<ChatListResponse>("GET", "/chats"),
  createChat: (message: string) => request<{ run: Run }>("POST", "/chats", { message }),
  // 202 {server_side}; the reply arrives over the run stream (mirrors submitRunInput).
  sendChatMessage: (id: string, message: string) =>
    request<{ server_side: boolean }>("POST", `/chats/${id}/messages`, { message }),
  endChat: (id: string) => request<{ server_side: boolean }>("POST", `/chats/${id}/end`),
  // Continue creates a NEW chat run carrying resume_of_run_id (Decision 11).
  continueChat: (id: string) => request<{ run: Run }>("POST", `/chats/${id}/continue`),
  // The ONLY forge-write path from chat: session + CSRF, forge-first (Decision 8).
  // 200 {issue}: the real created issue (the card renders its link).
  confirmProposal: (chatId: string, proposalId: string) =>
    request<{ issue: CreatedIssue }>("POST", `/chats/${chatId}/proposals/${proposalId}/confirm`),
  // 204 No Content: the card updates its state locally.
  dismissProposal: (chatId: string, proposalId: string) =>
    request<null>("POST", `/chats/${chatId}/proposals/${proposalId}/dismiss`),

  adminListWorkers: () => request<{ workers: AdminWorker[] }>("GET", "/admin/workers"),
  adminListRuns: () => request<{ runs: RunListItem[] }>("GET", "/admin/runs"),

  // Notifications inbox (PRD #46 M2). listNotifications is the caller's own inbox;
  // { all: true } asks for every user's (admin only — a non-admin gets 403). The
  // envelope's `unread` is always the caller's own count (the bell badge).
  // unreadNotificationCount is the bell's lightweight poll (no rows).
  listNotifications: (params?: { all?: boolean; limit?: number; offset?: number }) => {
    const q = new URLSearchParams();
    if (params?.all) q.set("all", "1");
    if (params?.limit != null) q.set("limit", String(params.limit));
    if (params?.offset != null) q.set("offset", String(params.offset));
    const qs = q.toString();
    return request<NotificationList>("GET", qs ? `/notifications?${qs}` : "/notifications");
  },
  unreadNotificationCount: () => request<{ unread: number }>("GET", "/notifications/unread_count"),
  markNotificationRead: (id: string) =>
    request<{ notification: Notification }>("POST", `/notifications/${id}/read`),

  // ── CLI tokens (PRD #64) — cookie-only CRUD ────────────────────────────────
  // A CLI token can never reach these endpoints (deliberate: a stolen token would
  // mint replacements, making revocation whack-a-mole) — they are the webui's own.
  // The mint returns the plaintext once; the list never carries a value.
  listCliTokens: () => request<{ tokens: CliToken[] }>("GET", "/me/cli-tokens"),
  createCliToken: (name: string, scope: CliTokenScope) =>
    request<CliTokenMint>("POST", "/me/cli-tokens", { name, scope }),
  revokeCliToken: (id: string) => request<null>("DELETE", `/me/cli-tokens/${id}`),
  // The panic button for a lost laptop: one query revokes every un-revoked token
  // of the caller. Idempotent (a second call is a no-op 204).
  revokeAllCliTokens: () => request<null>("POST", "/me/cli-tokens/revoke-all"),

  // ── CLI browser-login consent flow (PRD #64) ───────────────────────────────
  // The `/cli-auth` page's three calls. getCliAuthRequest is a cookie-only read
  // (the human's login happens on the way to it); approve/deny are CSRF writes.
  getCliAuthRequest: (id: string) =>
    request<CliAuthRequestMeta>("GET", `/auth/cli/request/${id}`),
  approveCliAuth: (requestId: string, userCode: string, scope: CliTokenScope) =>
    request<{ status: string }>("POST", "/auth/cli/approve", {
      request_id: requestId,
      user_code: userCode,
      scope,
    }),
  denyCliAuth: (requestId: string) =>
    request<{ status: string }>("POST", "/auth/cli/deny", { request_id: requestId }),

  // ── Agent memory (PRD #90 M6) — cookie-only, owner-scoped ──────────────────
  // list is newest-first across all the caller's repos (the component groups by
  // repo_name); delete is a single owner-scoped purge. The server derives the
  // owner from the session, so neither call carries a user_id.
  listMemory: () => request<{ memories: Memory[] }>("GET", "/me/memory"),
  deleteMemory: (id: string) => request<null>("DELETE", `/me/memory/${id}`),
};

// The one client the app talks to. `mockApi` implements the identical surface
// (typechecked against realApi's shape here), so pages never know which mode
// they run in.
export const api: typeof realApi = MOCK_MODE ? mockApi : realApi;

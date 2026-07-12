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
  created_at: string;
  last_login: string | null;
}

// SecretMeta is the metadata-only view of a stored per-user secret. The secret
// value is never returned by the API, so it never appears here.
export interface SecretMeta {
  kind: string;
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
  role: number;
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
  // Last merge-request state the PRD #24 watcher observed for mr_iid
  // (opened|closed|merged|locked), null when never observed. Display-only hint
  // (PRD #33): mrChipState maps it to the chip variant. Kept fresh only for the
  // board card (the issue's latest run); a superseded run's value can be stale.
  mr_state: string | null;
  failure_reason: string | null;
  // Server-stamped deliberate-stop signal (PRD #33); null for every non-stop run.
  // Read by isStoppedRun to render a stop as calm "stopped" instead of rose "failed".
  stop_kind: StopKind | null;
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
  author: string | null;
  has_prd_link: boolean;
  column: string;
  closed: boolean;
  conflict: boolean;
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

// ── Agent runtime (PRD #4) ────────────────────────────────────────────────

export interface Worker {
  id: string;
  name: string;
  status: string; // "offline" | "online"
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
  last_heartbeat_at: string | null;
  created_at: string;
}

export interface AdminWorker extends Worker {
  owner_email: string;
}

export type RunStatus =
  | "queued"
  | "claimed"
  | "running"
  | "awaiting_approval"
  | "completed"
  | "failed"
  | "cancelled";

// TERMINAL_RUN_STATUSES mirrors the DB CHECK: a run in any of these is finished.
export const TERMINAL_RUN_STATUSES: RunStatus[] = ["completed", "failed", "cancelled"];

export function isTerminalRun(status: string): boolean {
  return (TERMINAL_RUN_STATUSES as string[]).includes(status);
}

// StopKind is the server-stamped deliberate-stop signal (PRD #33): "cancelled" or
// "plan_rejected", null for a run that stopped for any other reason (a genuine
// failure, a timeout, or is still going). isStoppedRun reads this — never the
// free-text failure_reason — so a live-poller plan reject carrying the user's
// verbatim reason is still recognised as a deliberate stop.
export type StopKind = "cancelled" | "plan_rejected";

// FixVerdict is a ci_fix run's outcome (PRD #6): verified/fix_failed are stamped
// server-side from the post-fix pipeline; not_code is the agent's "not a code
// problem" verdict; null means the fix is not yet verified.
export type FixVerdict = "verified" | "fix_failed" | "not_code";

export interface Run {
  id: string;
  /** Nullable since PRD #39: a chat run has no repo (issue/ci_fix runs always do). */
  repo_id: string | null;
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
  /** Last MR state the PRD #24 watcher observed for mr_iid
   *  (opened|closed|merged|locked), null when never observed. Display-only hint
   *  (PRD #33); frozen per run, so a superseded run's value can be stale. */
  mr_state: string | null;
  failure_reason: string | null;
  /** Server-stamped deliberate-stop signal (PRD #33): "cancelled" or
   *  "plan_rejected", null otherwise. isStoppedRun reads this, not failure_reason. */
  stop_kind: StopKind | null;
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
  /** PRD #40: the run's rolled-up token/cost totals (greatest-wins per model,
   *  summed across models — the server's run_usage_totals view). Present only when
   *  the run has usage rows; absent/null for a pre-feature run, so the UI shows
   *  nothing rather than a fabricated 0. On both the list rows and the detail read. */
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
export interface RunListItem extends Run {
  repo_path: string;
  worker_name: string | null;
  owner_email?: string;
}

// RunMessage is one persisted, seq-numbered event in a run's stream.
export interface RunMessage {
  seq: number;
  kind: string;
  agent: string | null;
  payload: unknown;
  created_at: string;
}

export type RunInputKind = "follow_up" | "approve_plan" | "reject_plan" | "cancel";

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
// (rendered directly, deduped by seq); a "state" signals a status change (the
// client re-reads the run over REST — WS is never the source of truth).
export interface WsEvent {
  type: "message" | "state";
  seq?: number;
  kind?: string;
  agent?: string | null;
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
  logout: () => request<{ status: string }>("POST", "/auth/logout"),
  me: () => request<SessionResponse>("GET", "/auth/me"),
  listUsers: () => request<{ users: User[] }>("GET", "/admin/users"),
  setUserActive: (id: string, isActive: boolean) =>
    request<{ user: User }>("PATCH", `/admin/users/${id}`, { is_active: isActive }),
  getSettings: () => request<SettingsResponse>("GET", "/admin/settings"),
  updateSettings: (settings: UpdateSettingsPayload) =>
    request<SettingsResponse>("PUT", "/admin/settings", { settings }),
  // Vault migration progress (PRD #32): count of stored secrets still master-sealed
  // (owners who have not unlocked since the vault rolled out). Admin-only.
  vaultMigration: () => request<{ master_sealed: number }>("GET", "/admin/vault-migration"),
  // Flip the current user's autopilot opt-in (PRD #19 M3). Returns the updated user.
  setAutopilotEnabled: (enabled: boolean) =>
    request<{ user: User }>("PUT", "/me/autopilot", { enabled }),
  listSecrets: () => request<{ secrets: SecretMeta[] }>("GET", "/me/secrets"),
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
  getIssue: (repoId: string, iid: number) =>
    request<{ issue: IssueDetail }>("GET", `/repos/${repoId}/issues/${iid}`),
  moveIssue: (repoId: string, iid: number, toColumn: string) =>
    request<{ card: Card }>("POST", `/repos/${repoId}/issues/${iid}/move`, { to_column: toColumn }),
  // Apply/remove the PRDLESS label from the UI (PRD #22 M4). Forge-first, so the
  // returned card is authoritative — the caller replaces its card with it (no
  // optimistic update).
  setIssuePrdless: (repoId: string, iid: number, apply: boolean) =>
    request<{ card: Card }>("POST", `/repos/${repoId}/issues/${iid}/prdless`, { apply }),
  syncRepo: (repoId: string) => request<{ board: Board }>("POST", `/repos/${repoId}/sync`),
  createIssue: (repoId: string, title: string, description: string) =>
    request<{ card: Card }>("POST", `/repos/${repoId}/issues`, { title, description }),

  // Agent runtime (PRD #4).
  listWorkers: () => request<{ workers: Worker[] }>("GET", "/workers"),
  createWorker: (name: string, template?: string) =>
    request<{ worker: Worker; token: string }>("POST", "/workers", { name, template }),
  deleteWorker: (id: string) => request<null>("DELETE", `/workers/${id}`),

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
  getRunMessages: (id: string, afterSeq = 0) =>
    request<{ messages: RunMessage[] }>(
      "GET",
      afterSeq > 0 ? `/runs/${id}/messages?after=${afterSeq}` : `/runs/${id}/messages`,
    ),
  submitRunInput: (id: string, kind: RunInputKind, body = "", selection?: AgentSelectionInput) =>
    request<{ server_side: boolean }>("POST", `/runs/${id}/inputs`, {
      kind,
      body,
      // PRD #37: the structured agent selection is legal only on approve_plan; the
      // server ignores/validates it per kind. Omitted entirely when absent so a
      // plain follow-up/cancel body is unchanged.
      ...(selection ? { selection } : {}),
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
};

// The one client the app talks to. `mockApi` implements the identical surface
// (typechecked against realApi's shape here), so pages never know which mode
// they run in.
export const api: typeof realApi = MOCK_MODE ? mockApi : realApi;

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
  failure_reason: string | null;
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
  // unlocked (no banner, legacy behavior) rather than falsely locked.
  vault?: { unlocked: boolean };
}

// AuthConfig is the unauthenticated registration policy the register page reads
// to hide itself or hint the allowed domains before submit. The server stays
// authoritative; this is display + pre-validation only.
export interface AuthConfig {
  registration_enabled: boolean;
  allowed_email_domains: string[];
}

// ── Agent runtime (PRD #4) ────────────────────────────────────────────────

export interface Worker {
  id: string;
  name: string;
  status: string; // "offline" | "online"
  busy: boolean; // derived: holds a claimed/running/awaiting_approval run
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

// FixVerdict is a ci_fix run's outcome (PRD #6): verified/fix_failed are stamped
// server-side from the post-fix pipeline; not_code is the agent's "not a code
// problem" verdict; null means the fix is not yet verified.
export type FixVerdict = "verified" | "fix_failed" | "not_code";

export interface Run {
  id: string;
  repo_id: string;
  /** Run kind (PRD #6): "issue" works issue_iid's card; "ci_fix" fixes a failed
   *  pipeline (pipeline_ref/pipeline_web_url/fix_verdict below). */
  kind: string;
  /** The worked issue for an issue run; null for a ci_fix run (no issue). */
  issue_iid: number | null;
  issue_title: string;
  issue_description: string;
  status: RunStatus;
  requeue_count: number;
  iteration_count: number;
  /** PRD #19: an autopilot run (poller-started, plan auto-approved). Drives the
   *  "autopilot" badge; a manually-started run is false. */
  auto_approve: boolean;
  worker_id: string | null;
  branch: string | null;
  mr_iid: number | null;
  failure_reason: string | null;
  /** ci_fix (PRD #6): the failing ref, the failing pipeline's web URL (from the
   *  snapshot), and the fix verdict. All null on an issue run. */
  pipeline_ref: string | null;
  pipeline_web_url: string | null;
  fix_verdict: FixVerdict | null;
  plan_md: string | null;
  claimed_at: string | null;
  started_at: string | null;
  finished_at: string | null;
  created_at: string;
  updated_at: string;
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
  submitRunInput: (id: string, kind: RunInputKind, body = "") =>
    request<{ server_side: boolean }>("POST", `/runs/${id}/inputs`, { kind, body }),

  adminListWorkers: () => request<{ workers: AdminWorker[] }>("GET", "/admin/workers"),
  adminListRuns: () => request<{ runs: RunListItem[] }>("GET", "/admin/runs"),
};

// The one client the app talks to. `mockApi` implements the identical surface
// (typechecked against realApi's shape here), so pages never know which mode
// they run in.
export const api: typeof realApi = MOCK_MODE ? mockApi : realApi;

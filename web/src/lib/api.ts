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

// AgentTemplate is a stored agent definition. tools is null when the template
// inherits all tools; model is null when it inherits the model.
export interface AgentTemplate {
  id: string;
  name: string;
  description: string;
  model: string | null;
  tools: string[] | null;
  prompt_body: string;
  is_builtin: boolean;
  updated_by: string | null;
  created_at: string;
  updated_at: string;
}

// AgentTemplateInput is the admin-editable shape. name is only sent on create
// (it is immutable afterwards).
export interface AgentTemplateInput {
  name?: string;
  description: string;
  model: string | null;
  tools: string[] | null;
  prompt_body: string;
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
}

export interface BoardColumn {
  label_name: string;
  position: number;
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
}

export interface Board {
  repo_id: string;
  path_with_namespace: string;
  web_url: string;
  columns: BoardColumn[];
  cards: Card[];
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
}

// Compiled-in label defaults, mirroring the API's settings package. The SPA uses
// them until the session bootstrap resolves the configured values (PRD #19 M2).
export const DEFAULT_PRD_LABEL = "PRD";
export const DEFAULT_AUTOPILOT_LABEL = "autopilot";

// SessionResponse is the auth/session bootstrap body (login, register, me). It
// carries the user, the instance forge labels the board and issue-creation UI
// need before their first call (PRD #19 M2), and the three theme fields the
// Appearance picker needs (PRD #21): the resolved theme the SPA renders, the
// user's raw override (null = none), and the instance default.
export interface SessionResponse {
  user: User;
  prd_label: string;
  autopilot_label: string;
  theme: string;
  theme_override: string | null;
  default_theme: string;
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

export interface Run {
  id: string;
  repo_id: string;
  issue_iid: number;
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
  getSettings: () => request<{ settings: AppSettings }>("GET", "/admin/settings"),
  updateSettings: (settings: Partial<AppSettings>) =>
    request<{ settings: AppSettings }>("PUT", "/admin/settings", { settings }),
  // Flip the current user's autopilot opt-in (PRD #19 M3). Returns the updated user.
  setAutopilotEnabled: (enabled: boolean) =>
    request<{ user: User }>("PUT", "/me/autopilot", { enabled }),
  listSecrets: () => request<{ secrets: SecretMeta[] }>("GET", "/me/secrets"),
  putAnthropicToken: (token: string) =>
    request<{ secret: SecretMeta }>("PUT", "/me/secrets/anthropic_token", { token }),
  deleteAnthropicToken: () => request<null>("DELETE", "/me/secrets/anthropic_token"),
  getMySettings: () => request<{ settings: UserSettings }>("GET", "/me/settings"),
  putMySettings: (patch: UserSettingsPatch) =>
    request<{ settings: UserSettings }>("PUT", "/me/settings", patch),
  listAgentTemplates: () =>
    request<{ templates: AgentTemplate[] }>("GET", "/agent-templates"),
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

  getBoard: (repoId: string) => request<{ board: Board }>("GET", `/repos/${repoId}/board`),
  configureColumns: (repoId: string, columns: { label_name: string }[]) =>
    request<{ board: Board }>("PUT", `/repos/${repoId}/board/columns`, { columns }),
  getIssue: (repoId: string, iid: number) =>
    request<{ issue: IssueDetail }>("GET", `/repos/${repoId}/issues/${iid}`),
  moveIssue: (repoId: string, iid: number, toColumn: string) =>
    request<{ card: Card }>("POST", `/repos/${repoId}/issues/${iid}/move`, { to_column: toColumn }),
  syncRepo: (repoId: string) => request<{ board: Board }>("POST", `/repos/${repoId}/sync`),
  createIssue: (repoId: string, title: string, description: string) =>
    request<{ card: Card }>("POST", `/repos/${repoId}/issues`, { title, description }),

  // Agent runtime (PRD #4).
  listWorkers: () => request<{ workers: Worker[] }>("GET", "/workers"),
  createWorker: (name: string) =>
    request<{ worker: Worker; token: string }>("POST", "/workers", { name }),
  deleteWorker: (id: string) => request<null>("DELETE", `/workers/${id}`),

  createRun: (repoId: string, issueIid: number) =>
    request<{ run: Run }>("POST", `/repos/${repoId}/runs`, { issue_iid: issueIid }),
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

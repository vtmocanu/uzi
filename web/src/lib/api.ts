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
// ApiError moved to its own leaf module to break the api → mockApi → api
// runtime import cycle (issue #165); imported here for api.ts's own internal
// uses. TypeScript permits importing a name this module also re-exports below.
import { ApiError } from "./apiError";

export const MOCK_MODE = import.meta.env.VITE_UZI_MOCK === "1";

// ── DTO types (moved to ./apiTypes, issue #960) ──────────────────────────────
// Every request/response DTO now lives in ./apiTypes; re-export the whole set so
// the lib/api barrel surface is byte-identical for its importers. The explicit
// type import below names only the DTOs this module's own code (realApi, the
// request helpers, ProjectSyncVisibility) references in its signatures.
import type {
  AdminBlockedRepos,
  AdminRateLimits,
  AdminUsage,
  AdminWorker,
  AgentSelectionInput,
  AgentSourceApplyResult,
  AgentSourceView,
  AgentTemplate,
  AgentTemplateInput,
  AllocationsInput,
  AuthConfig,
  BindMode,
  Board,
  BoardPrefs,
  Branding,
  BuildInfo,
  BuiltinDefinition,
  Card,
  ChatListResponse,
  CliAuthRequestMeta,
  CliToken,
  CliTokenMint,
  CliTokenScope,
  CreatedIssue,
  ForgeConfig,
  ForgeConnection,
  HostedConfig,
  IncidentalFindingBacklog,
  IncidentalFindingBucket,
  IncidentalFindingFileResult,
  IncidentalFindingIssueDraft,
  IssueDetail,
  IssueDraft,
  JudgeBacklog,
  JudgeBacklogBucket,
  JudgeCategoryStats,
  JudgeDispositionCoord,
  JudgeDispositionResult,
  JudgeDispositionScope,
  Memory,
  MyRateLimitsResponse,
  Notification,
  NotificationList,
  PendingJudge,
  PrivilegeReport,
  ProjectSyncOwnerKind,
  ProjectSyncStatus,
  ReleaseCheckStatus,
  Repo,
  Run,
  RunInputKind,
  RunListItem,
  RunMessage,
  RunNowResponse,
  RunReview,
  RunSocketLike,
  Schedule,
  ScheduleCatalog,
  ScheduleInput,
  SchedulePauseDTO,
  SchedulePreviewInput,
  SecretMeta,
  SelfUsage,
  SessionResponse,
  SettingsResponse,
  Skill,
  SkillCreateInput,
  SkillUpdateInput,
  SlackLink,
  SteerInput,
  TemplateAllocation,
  TemplateAllocationsInput,
  TemplateSkills,
  ToolAllowlistEntry,
  ToolAllowlistWriteInput,
  TriageCounts,
  UpdateSettingsPayload,
  User,
  UserSettings,
  UserSettingsPatch,
  Worker,
} from "./apiTypes";
export type * from "./apiTypes";
export { DEFAULT_AUTOPILOT_LABEL, RATE_LIMIT_SOURCES } from "./apiTypes";

// Board visibility of a linked GitHub Project v2 (PRD #557). `public` round-trips
// through the GET/PUT visibility routes — GitHub's `ProjectV2.public` is both
// readable and writable, so the toggle reflects and writes true state.
// Not exported: used only as the internal response type of the visibility client
// methods below (no external consumer imports it by name).
interface ProjectSyncVisibility {
  public: boolean;
}

// TERMINAL_RUN_STATUSES / isTerminalRun moved to the ./runStatus leaf module to
// break the api → mockApi → api runtime import cycle (issue #165). Only
// isTerminalRun is re-exported here — the barrel is its consumers' entry point.
// TERMINAL_RUN_STATUSES was also re-exported for surface parity but nothing
// imported it via the barrel (its only consumer takes it from ./runStatus
// directly), so the dead re-export was dropped (issue #596).
export { isTerminalRun } from "./runStatus";

// runSocketUrl builds the same-origin WebSocket URL for a run. The HttpOnly auth
// cookie rides along automatically (same origin through nginx); Origin==Host is
// enforced server-side against cross-site hijacking.
function runSocketUrl(runId: string): string {
  const proto = window.location.protocol === "https:" ? "wss:" : "ws:";
  return `${proto}//${window.location.host}/api/ws?run=${encodeURIComponent(runId)}`;
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
export function preferForgeUrl(
  persisted: string | null | undefined,
  legacy: string | null,
): string | null {
  return isHttpsUrl(persisted) ? persisted! : legacy;
}

// ApiError moved to the ./apiError leaf module to break the api → mockApi → api
// runtime import cycle (issue #165). Re-exported here so the barrel's public
// surface is unchanged; api.ts's own internal uses come from the top import.
export { ApiError } from "./apiError";

function readCookie(name: string): string | null {
  const match = document.cookie.match(
    new RegExp("(?:^|; )" + name + "=([^;]*)"),
  );
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
export function setUnauthorizedHandler(
  handler: UnauthorizedHandler | null,
): void {
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
export function setVaultLockedHandler(
  handler: VaultLockedHandler | null,
): void {
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

// isOpenMRConflict reports whether an error is the 409 issue_has_open_mr signal
// (issue #856): a completed prior run still owns an open MR, so a fresh run was
// refused. The board/issue Start flow offers a force-retry on this specific code.
export function isOpenMRConflict(err: unknown): boolean {
  return (
    err instanceof ApiError &&
    err.status === 409 &&
    (err.body as { code?: string } | null)?.code === "issue_has_open_mr"
  );
}

// openMRConflictMRIID returns the MR iid carried by a 409 issue_has_open_mr body
// (issue #856), or null when absent — the web composes its own confirm copy from it.
export function openMRConflictMRIID(err: unknown): number | null {
  if (!(err instanceof ApiError)) return null;
  const n = (err.body as { mr_iid?: unknown } | null)?.mr_iid;
  return typeof n === "number" ? n : null;
}

async function request<T>(
  method: string,
  path: string,
  body?: unknown,
  extraHeaders?: Record<string, string>,
): Promise<T> {
  const headers: Record<string, string> = {};
  if (body !== undefined) {
    headers["Content-Type"] = "application/json";
  }
  if (method !== "GET" && method !== "HEAD") {
    const csrf = readCookie("uzi_csrf");
    if (csrf) headers["X-CSRF-Token"] = csrf;
  }
  // Append-only extra headers (e.g. the passive-poll marker, #331). Merged last so a
  // caller can override, but callers pass only additive keys, not Content-Type/CSRF.
  if (extraHeaders) Object.assign(headers, extraHeaders);

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
    if (
      res.status === 409 &&
      (payload as { code?: string } | null)?.code === "vault_locked"
    ) {
      vaultLockedHandler?.();
    }
    const message =
      (payload as { error?: string } | null)?.error ??
      `request failed (${res.status})`;
    throw new ApiError(res.status, message, payload);
  }
  return payload as T;
}

// uploadBrandingLogo PUTs a RAW image body to the admin logo endpoint (PRD #685).
// It cannot use request(), which force-sets Content-Type: application/json and
// JSON.stringifies the body — the backend PutBrandingLogo reads the raw bytes with
// io.ReadAll and parses the Content-Type header for the type allowlist. So this
// sends the File directly with its own MIME type, mirroring request()'s CSRF-cookie
// echo and its non-ok → ApiError handling.
async function uploadBrandingLogo(slot: string, file: File): Promise<void> {
  const headers: Record<string, string> = { "Content-Type": file.type };
  const csrf = readCookie("uzi_csrf");
  if (csrf) headers["X-CSRF-Token"] = csrf;
  const res = await fetch(`/api/admin/branding/logo/${slot}`, {
    method: "PUT",
    headers,
    credentials: "same-origin",
    body: file,
  });
  if (!res.ok) {
    let payload: unknown = null;
    const text = await res.text();
    if (text) {
      try {
        payload = JSON.parse(text);
      } catch {
        payload = null;
      }
    }
    if (res.status === 401) unauthorizedHandler?.();
    const message =
      (payload as { error?: string } | null)?.error ??
      `request failed (${res.status})`;
    throw new ApiError(res.status, message, payload);
  }
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
  // Server build info (PRD #175). Unauthenticated, like /health — the shell footer
  // reads it for the version badge and its popover. Widened from {version} to
  // BuildInfo; the `version` key did not move, rename or nest, because it also
  // feeds PRD #113's worker upgrade classification.
  version: () => request<BuildInfo>("GET", "/version"),
  // Public instance branding (PRD #685). Unauthenticated like /version — the chrome
  // reads it (signed-in and out) to decide the app mark and POWERED BY block. Returns
  // the typed (bool) shape; logo bytes load separately from /api/branding/logo/{slot}.
  branding: () => request<Branding>("GET", "/branding"),
  // Admin logo upload (raw image body) / delete for a slot ∈ {app, brand} (PRD #685).
  // Upload bypasses request() for the raw-body reason documented on uploadBrandingLogo;
  // delete is an ordinary admin DELETE.
  uploadBrandingLogo: (slot: string, file: File) => uploadBrandingLogo(slot, file),
  deleteBrandingLogo: (slot: string) =>
    request<{ status: string }>("DELETE", `/admin/branding/logo/${slot}`),
  // The Workers nav badge's count (PRD #113 M6). Its own endpoint rather than a fold over
  // listWorkers: the Workers page's poll is page-local and visibility-gated, so a badge
  // fed from it would be stale or absent exactly when the operator is not on that page,
  // which is the only situation a nav badge exists for.
  workerUpgradeSummary: () =>
    request<{ attention: number; target_release: string }>(
      "GET",
      "/me/workers/upgrade-summary",
    ),
  logout: () => request<{ status: string }>("POST", "/auth/logout"),
  me: () => request<SessionResponse>("GET", "/auth/me"),
  listUsers: () => request<{ users: User[] }>("GET", "/admin/users"),
  setUserActive: (id: string, isActive: boolean) =>
    request<{ user: User }>("PATCH", `/admin/users/${id}`, {
      is_active: isActive,
    }),
  // Admin per-user run-judge toggle (PRD #46): force any user's opt-in. Actor is
  // admin (route-gated); target is the path id, never the body. Returns the user.
  setUserJudgeEnabled: (id: string, enabled: boolean) =>
    request<{ user: User }>("PUT", `/admin/users/${id}/judge`, { enabled }),
  // Admin per-user CI-autofix toggle (PRD #71): force any user's opt-in. Actor is
  // admin (route-gated); target is the path id, never the body. Returns the user.
  setUserCIAutofixEnabled: (id: string, enabled: boolean | null) =>
    request<{ user: User }>("PUT", `/admin/users/${id}/ci-autofix`, { enabled }),
  getSettings: () => request<SettingsResponse>("GET", "/admin/settings"),
  updateSettings: (settings: UpdateSettingsPayload) =>
    request<SettingsResponse>("PUT", "/admin/settings", { settings }),
  // Vault migration progress (PRD #32): count of stored secrets still master-sealed
  // (owners who have not unlocked since the vault rolled out). Admin-only.
  vaultMigration: () =>
    request<{ master_sealed: number }>("GET", "/admin/vault-migration"),
  // Agent-source admin surface (PRD #602 M5). getAgentSource is the read (config +
  // status + staged review); syncAgentSource runs "Sync now" (the same reconcile the
  // interval loop calls) and returns the refreshed view; applyAgentSource approves
  // and applies the staged snapshot. expectedSha is REQUIRED and binds the exact
  // snapshot the admin reviewed — the server 409s (ApiError.status 409) if it changed
  // since. Config edits reuse updateSettings (the agent_source_* keys above).
  getAgentSource: () =>
    request<{ agent_source: AgentSourceView }>("GET", "/admin/agent-source"),
  syncAgentSource: () =>
    request<{ agent_source: AgentSourceView }>("POST", "/admin/agent-source/sync"),
  applyAgentSource: (expectedSha: string) =>
    request<{ result: AgentSourceApplyResult }>("POST", "/admin/agent-source/apply", {
      expected_sha: expectedSha,
    }),
  // Trigger an update check against the CONFIGURED, saved source (PRD #702 M4): the
  // server ls-remotes it with the sealed credential, persists the fresh remote facts,
  // and returns the refreshed view whose derived `status.update_available`/`latest_ref`
  // reflect them. Cookie-only admin. The check reuses the last_sync_error slot for its
  // own error message, so read `status.last_sync_status`/`last_sync_error` after it.
  updateCheckAgentSource: () =>
    request<{ agent_source: AgentSourceView }>("POST", "/admin/agent-source/update-check"),
  // Resolve the latest semver tag for a supplied (possibly unsaved) source URL, via
  // M2's ls-remote endpoint (PRD #702 M3). Anonymous — it works only against a public
  // source, and the URL host is SSRF-rechecked against the deployment allowlist. The
  // Preset button uses it to fill the ref at click time (never a hardcoded version);
  // `latest_ref` is empty when the source publishes no semver tag yet.
  resolveAgentSourceLatest: (url: string) =>
    request<{ latest_ref: string }>("POST", "/admin/agent-source/resolve-latest", { url }),
  // Upstream release-check admin surface (PRD #836 M3/M5). getReleaseCheck is the read
  // (RequireAdminRO) that backs the admin Updates card — the full status incl. the raw
  // admin-only `body`. checkReleaseNow is the "Check now" write (RequireAdmin): it
  // triggers one poll against the GitHub Releases API and returns the refreshed status.
  // The two runtime toggles are edited through updateSettings (the release_check_*
  // keys above), never here. Both return the same envelope shape.
  getReleaseCheck: () =>
    request<{ release_check: ReleaseCheckStatus }>("GET", "/admin/release-check"),
  checkReleaseNow: () =>
    request<{ release_check: ReleaseCheckStatus }>("POST", "/admin/release-check"),
  // Snooze the admin escalation banner (PRD #836 M6) for the current release: upserts
  // the snooze tag = latest_tag server-side and returns the refreshed status (now with
  // banner_snoozed:true). Keyed to the release tag, so a newer release auto-clears it.
  // RequireAdmin (cookie-only), no egress. Called by UpdateEscalationBanner's Dismiss.
  snoozeReleaseBanner: () =>
    request<{ release_check: ReleaseCheckStatus }>("POST", "/admin/release-check/snooze"),
  // Flip the current user's autopilot opt-in (PRD #19 M3). Returns the updated user.
  setAutopilotEnabled: (enabled: boolean) =>
    request<{ user: User }>("PUT", "/me/autopilot", { enabled }),
  // Flip the current user's CI-autofix opt-in (PRD #71). Session identity only —
  // the body carries no user id. Returns the updated user.
  setCIAutofixEnabled: (enabled: boolean | null) =>
    request<{ user: User }>("PUT", "/me/ci-autofix", { enabled }),
  // Flip the current user's AI-attribution opt-out (issue #916). Session identity only —
  // the body carries no user id. Returns the updated user.
  setAttributionEnabled: (enabled: boolean) =>
    request<{ user: User }>("PUT", "/me/attribution", { enabled }),
  // Flip the current user's ephemeral-workers opt-in (PRD #649). Session identity
  // only — the body carries no user id. Returns the updated user.
  setEphemeralWorkersEnabled: (enabled: boolean) =>
    request<{ user: User }>("PUT", "/me/ephemeral-workers", { enabled }),
  /**
   * PRD #35: flip the current user's DEFAULT for the usage-limit park. Returns the
   * updated user.
   *
   * 🔴 IT DOES NOT REACH RUNS THAT ALREADY EXIST — not even queued ones, and not the
   * one the user is looking at. The flag is copied onto each run at creation, so this
   * changes what the NEXT run inherits and nothing else. The per-run control is
   * setRunWaitOnLimit below; the two are separate endpoints because they answer
   * different questions, and a single "sync everything" write would silently undo
   * every per-run override the user had made.
   *
   * The reason this default is load-bearing rather than a convenience: autopilot,
   * ci_fix and self_improve runs have NO start affordance at all, so for two of the
   * three kinds that park, this setting is the only way the opt-in can ever be
   * expressed.
   */
  setWaitOnLimit: (enabled: boolean) =>
    request<{ user: User }>("PUT", "/me/wait-on-limit", { enabled }),
  // Flip the current user's early-limit-reset Slack alert opt-in (PRD #1020 M4/M5).
  // Session identity only — the body carries no user id. Returns the updated user.
  setNotifyEarlyReset: (enabled: boolean) =>
    request<{ user: User }>("PUT", "/me/notify-early-reset", { enabled }),
  // Flip the current user's run-judge opt-in (PRD #46). Session identity only —
  // the body carries no user id. Returns the updated user.
  // enabled is required; anthropicToken is the three-way token field (PRD #104 M4):
  // omitted leaves the binding alone, null clears it back to the default, a label
  // binds it. Omitting it is what every pre-#104 caller did, and must stay a no-op
  // on the binding. judgeBindMode (PRD #1140 M3) is the three-valued bind mode
  // (default / pinned / auto), sent as judge_bind_mode only when provided, so every
  // existing caller's body shape is unchanged.
  setJudgeEnabled: (enabled: boolean, anthropicToken?: string | null, judgeBindMode?: BindMode) => {
    const body: Record<string, unknown> = { enabled };
    if (anthropicToken !== undefined) body.anthropic_token = anthropicToken;
    if (judgeBindMode !== undefined) body.judge_bind_mode = judgeBindMode;
    return request<{ user: User }>("PUT", "/me/judge", body);
  },
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
  ) =>
    request<{ secret: SecretMeta }>(
      "PATCH",
      `/me/secrets/anthropic_token/${id}`,
      body,
    ),
  // The auto-selection pool toggle (PRD #111 M2, D13). Its OWN narrow route, not a
  // field on the PATCH above: every other secrets write is cookie-only because a
  // Bearer-reachable mint would let a stolen CLI token replace a user's credentials,
  // and moving that PATCH to reach this toggle would have taken rename, rotate and
  // set-default along with it.
  setTokenAutoEligible: (id: string, autoEligible: boolean) =>
    request<{ secret: SecretMeta }>(
      "PATCH",
      `/me/secrets/anthropic_token/${id}/auto-eligible`,
      {
        auto_eligible: autoEligible,
      },
    ),
  deleteAnthropicTokenById: (id: string) =>
    request<null>("DELETE", `/me/secrets/anthropic_token/${id}`),
  putAnthropicToken: (token: string) =>
    request<{ secret: SecretMeta }>("PUT", "/me/secrets/anthropic_token", {
      token,
    }),
  deleteAnthropicToken: () =>
    request<null>("DELETE", "/me/secrets/anthropic_token"),

  // Vault (PRD #32): unlock re-derives the DEK from the login password (204, or
  // 403 on a wrong password); lock evicts it; status is a lightweight poll. Unlock
  // and lock return no body.
  vaultUnlock: (password: string) =>
    request<null>("POST", "/vault/unlock", { password }),
  // Create a passwordless (OIDC) user's vault from a chosen passphrase (PRD #45).
  // Create-only: 409 if a vault already exists; 204 on success (vault then unlocked).
  vaultCreatePassphrase: (passphrase: string) =>
    request<null>("POST", "/vault/passphrase", { passphrase }),
  vaultLock: () => request<null>("POST", "/vault/lock"),
  vaultStatus: () => request<{ unlocked: boolean }>("GET", "/vault/status"),
  getMySettings: () =>
    request<{ settings: UserSettings }>("GET", "/me/settings"),
  putMySettings: (patch: UserSettingsPatch) =>
    request<{ settings: UserSettings }>("PUT", "/me/settings", patch),
  // Slack linking (PRD #25 M3), own-user only. member_id null clears the override
  // (falls back to email auto-match). A 409 from setMySlackOverride means the id is
  // already linked to another account.
  getMySlack: () => request<{ slack: SlackLink }>("GET", "/me/slack"),
  setMySlackNotify: (notify: boolean) =>
    request<{ slack: SlackLink }>("PUT", "/me/slack/notify", { notify }),
  setMySlackOverride: (memberId: string | null) =>
    request<{ slack: SlackLink }>("PUT", "/me/slack/override", {
      member_id: memberId,
    }),
  testMySlackDM: () => request<{ status: string }>("POST", "/me/slack/test-dm"),
  // Just the live Slack socket state, for the admin chip's poll (PRD #25 M3).
  getSlackStatus: () =>
    request<{ slack_status: string }>("GET", "/admin/slack/status"),
  listAgentTemplates: () =>
    request<{ templates: AgentTemplate[] }>("GET", "/agent-templates"),
  getTemplateAllocations: () =>
    request<{ templates: TemplateAllocation[] }>(
      "GET",
      "/agent-templates/allocations",
    ),
  setTemplateAllocations: (input: TemplateAllocationsInput) =>
    request<{ templates: TemplateAllocation[] }>(
      "PUT",
      "/agent-templates/allocations",
      input,
    ),
  getAgentTemplate: (id: string) =>
    request<{ template: AgentTemplate }>("GET", `/agent-templates/${id}`),
  // The shipped definition behind a builtin row. 400 when the row is not a
  // builtin, 409 when this release no longer ships one (the state
  // differs_from_builtin reports as false, and the signal that Reset would 409).
  getBuiltinAgentTemplate: (id: string) =>
    request<{ builtin: BuiltinDefinition }>(
      "GET",
      `/agent-templates/${id}/builtin`,
    ),
  createAgentTemplate: (input: AgentTemplateInput) =>
    request<{ template: AgentTemplate }>("POST", "/agent-templates", input),
  updateAgentTemplate: (id: string, input: AgentTemplateInput) =>
    request<{ template: AgentTemplate }>(
      "PUT",
      `/agent-templates/${id}`,
      input,
    ),
  deleteAgentTemplate: (id: string) =>
    request<null>("DELETE", `/agent-templates/${id}`),
  resetAgentTemplate: (id: string) =>
    request<{ template: AgentTemplate }>(
      "POST",
      `/agent-templates/${id}/reset`,
    ),

  // Agent skills (PRD #16).
  listSkills: () => request<{ skills: Skill[] }>("GET", "/skills"),
  getSkill: (id: string) => request<{ skill: Skill }>("GET", `/skills/${id}`),
  createSkill: (input: SkillCreateInput) =>
    request<{ skill: Skill }>("POST", "/skills", input),
  updateSkill: (id: string, input: SkillUpdateInput) =>
    request<{ skill: Skill }>("PUT", `/skills/${id}`, input),
  deleteSkill: (id: string) => request<null>("DELETE", `/skills/${id}`),
  resetSkill: (id: string) =>
    request<{ skill: Skill }>("POST", `/skills/${id}/reset`),
  getTemplateSkills: (id: string) =>
    request<{ allocations: TemplateSkills }>(
      "GET",
      `/agent-templates/${id}/skills`,
    ),
  setTemplateSkills: (id: string, input: AllocationsInput) =>
    request<{ allocations: TemplateSkills }>(
      "PUT",
      `/agent-templates/${id}/skills`,
      input,
    ),

  // Tool allowlist + per-repo tool profiles (PRD #18 M4). The allowlist is readable
  // by any user (the repo picker needs it); writes are admin-only. A repo's profile
  // is owner-only.
  listToolAllowlist: () =>
    request<{ allowlist: ToolAllowlistEntry[] }>("GET", "/tool-allowlist"),
  createToolAllowlistEntry: (input: ToolAllowlistWriteInput) =>
    request<{ entry: ToolAllowlistEntry }>("POST", "/tool-allowlist", input),
  updateToolAllowlistEntry: (id: string, input: ToolAllowlistWriteInput) =>
    request<{ entry: ToolAllowlistEntry }>(
      "PUT",
      `/tool-allowlist/${id}`,
      input,
    ),
  deleteToolAllowlistEntry: (id: string) =>
    request<null>("DELETE", `/tool-allowlist/${id}`),
  getRepoToolProfile: (repoId: string) =>
    request<{ packages: string[] }>("GET", `/repos/${repoId}/tool-profile`),
  setRepoToolProfile: (repoId: string, packages: string[]) =>
    request<{ packages: string[] }>("PUT", `/repos/${repoId}/tool-profile`, {
      packages,
    }),

  // Forge integration.
  forgeConfig: () => request<ForgeConfig>("GET", "/forge/config"),
  listConnections: () =>
    request<{ connections: ForgeConnection[] }>("GET", "/forge/connections"),
  createConnection: (baseUrl: string, token: string, forgeType = "gitlab") =>
    request<{ connection: ForgeConnection }>("POST", "/forge/connections", {
      base_url: baseUrl,
      token,
      forge_type: forgeType,
    }),
  verifyConnection: (id: string) =>
    request<{ connection: ForgeConnection }>(
      "POST",
      `/forge/connections/${id}/verify`,
    ),
  // Set (or clear, with "") the connecting user's own forge username for autopilot
  // attribution. The API best-effort-verifies it and may return a `warning` while
  // still saving (verified-or-warned, PRD #19 M3).
  updateConnection: (id: string, humanUsername: string) =>
    request<{ connection: ForgeConnection; warning?: string }>(
      "PUT",
      `/forge/connections/${id}`,
      {
        human_username: humanUsername,
      },
    ),
  privilegeCheck: (id: string) =>
    request<{ report: PrivilegeReport }>(
      "POST",
      `/forge/connections/${id}/privilege-check`,
    ),
  deleteConnection: (id: string) =>
    request<null>("DELETE", `/forge/connections/${id}`),
  listProjects: (connectionId: string) =>
    request<{ repos: Repo[] }>(
      "GET",
      `/forge/connections/${connectionId}/projects`,
    ),

  listRepos: () => request<{ repos: Repo[] }>("GET", "/repos"),
  setRepoEnabled: (id: string, enabled: boolean) =>
    request<{ repo: Repo }>("PUT", `/repos/${id}`, { enabled }),
  // Explicit per-repo remove (PRD #357). Owner-scoped; the server permits it only
  // on a DISABLED repo with no in-flight run (409 otherwise), then deletes the
  // repos row and cascades its board/run history. Empty 204 body.
  deleteRepo: (id: string) => request<null>("DELETE", `/repos/${id}`),
  setRepoSkillsEnabled: (id: string, enabled: boolean) =>
    request<{ repo: Repo }>("PATCH", `/repos/${id}`, {
      repo_skills_enabled: enabled,
    }),
  // Repo-instructions opt-in (PRD #246). Owner or admin. The second capability
  // behind the "Trusted repo" affordance; toggled independently of repo skills.
  setRepoClaudemdEnabled: (id: string, enabled: boolean) =>
    request<{ repo: Repo }>("PATCH", `/repos/${id}`, {
      repo_claudemd_enabled: enabled,
    }),
  // Set both trust capabilities in ONE request (PRD #246). Used by the "Trusted
  // repo" master control: enabling turns both on, disabling turns both off. The
  // server accepts the two trust flags together (still atomic, devbox untouched).
  setRepoTrustFlags: (
    id: string,
    flags: { repo_skills_enabled?: boolean; repo_claudemd_enabled?: boolean },
  ) => request<{ repo: Repo }>("PATCH", `/repos/${id}`, flags),
  // Tier-2 repo devbox.json opt-in (PRD #18 M5). Owner or admin.
  setRepoDevboxOptIn: (id: string, enabled: boolean) =>
    request<{ repo: Repo }>("PATCH", `/repos/${id}`, {
      repo_devbox_opt_in: enabled,
    }),
  // Per-repo self-improve dogfooding capability (PRD #686). Owner or admin.
  // Mutually exclusive in one request with the devbox/trust/caps groups, so it
  // is sent alone.
  setRepoFoldImproveUziBacklog: (id: string, enabled: boolean) =>
    request<{ repo: Repo }>("PATCH", `/repos/${id}`, {
      repo_fold_improve_uzi_backlog: enabled,
    }),
  // Static per-repo capability hint (PRD #84 M2). Owner or admin. Mutually
  // exclusive in one request with repo_devbox_opt_in and the trust flags, so it is
  // sent alone. The server capability.Filters the list to the {docker, jvm}
  // vocabulary, so only valid names persist.
  setRepoRequiredCapabilities: (id: string, caps: string[]) =>
    request<{ repo: Repo }>("PATCH", `/repos/${id}`, {
      required_capabilities: caps,
    }),
  // Admin per-repo guardrail override (PRD #66 D8). ADMIN-ONLY, no member path — a
  // dedicated route, not PatchRepo. Requires a non-empty reason; returns the repo.
  setRepoGuardrailOverride: (id: string, reason: string) =>
    request<{ repo: Repo }>("POST", `/admin/repos/${id}/guardrail-override`, {
      reason,
    }),
  // Revoke the override (PRD #66 D8): NULLs it, re-arming the guardrail immediately.
  clearRepoGuardrailOverride: (id: string) =>
    request<{ repo: Repo }>("DELETE", `/admin/repos/${id}/guardrail-override`),

  // GitHub Projects v2 sync (PRD #534). Owner-or-admin; the server 404s a
  // non-linked or non-owner repo (existence-hiding). Read the current link
  // status; a 404 is the caller's "not linked yet" signal.
  getProjectSyncStatus: (id: string) =>
    request<ProjectSyncStatus>("GET", `/repos/${id}/github-project-sync`),
  // Provision a brand-new Project v2 for this repo (201 { status: "provisioned" }).
  // An empty title lets the server pick a default.
  provisionProjectSync: (
    id: string,
    body: { owner_kind: ProjectSyncOwnerKind; title?: string },
  ) =>
    request<{ status: string }>(
      "POST",
      `/repos/${id}/github-project-sync/provision`,
      { owner_kind: body.owner_kind, title: body.title ?? "" },
    ),
  // Read the repo owner's GitHub type for the Adopt-first Provision nudge (PRD
  // #576 M1): a live forge round-trip (repositoryOwner __typename), fetched for a
  // not-yet-linked repo. "User" means Provision cannot own a project under a
  // personal account (Adopt instead); "Organization" means Provision is available.
  getProjectSyncOwnerType: (id: string) =>
    request<{ owner_type: "User" | "Organization" }>(
      "GET",
      `/repos/${id}/github-project-sync/owner-type`,
    ),
  // Adopt an EXISTING Project v2 by number (200 { status: "linked" }).
  adoptProjectSync: (
    id: string,
    body: { project_number: number; owner_kind: ProjectSyncOwnerKind },
  ) => request<{ status: string }>("POST", `/repos/${id}/github-project-sync`, body),
  // Re-seed an already-linked board (PRD #576 M3): re-reads the Status field so
  // newly-added options resolve and re-persists the unmatched set. Idempotent (Adopt
  // re-diffs every item); needs no body. 200 { status: "resynced" }; 404 when the repo
  // has no link.
  resyncProjectSync: (id: string) =>
    request<{ status: string }>(
      "POST",
      `/repos/${id}/github-project-sync/resync`,
    ),
  // Safe column auto-create (PRD #576 M6): create a FRESH uzi-owned "uzi Status"
  // field on the adopted board carrying ALL the repo's columns and switch the link to
  // it, turning skipped columns into synced ones with no manual GitHub edit and no
  // destructive field replace. Needs no body. 200 { status: "columns_created" }; 404
  // when the repo has no link. Tradeoff: the board then carries two status-like fields.
  autocreateProjectSyncColumns: (id: string) =>
    request<{ status: string }>(
      "POST",
      `/repos/${id}/github-project-sync/autocreate-columns`,
    ),
  // Unlink the repo from its project (204, empty body). Idempotent server-side.
  disableProjectSync: (id: string) =>
    request<null>("DELETE", `/repos/${id}/github-project-sync`),

  // Board access — visibility + write-only sharing (PRD #557). Owner-or-admin,
  // GitHub-only; the server 404s a non-linked/non-owner repo (existence-hiding),
  // 409s when the instance flag is off, and 422s a bad username / non-GitHub repo.

  // Read the linked board's current public/private flag. A SEPARATE live-forge
  // call (D4), issued lazily when the Board-access section opens — kept off the
  // DB-only status GET so the common status open pays nothing.
  getProjectSyncVisibility: (id: string) =>
    request<ProjectSyncVisibility>(
      "GET",
      `/repos/${id}/github-project-sync/visibility`,
    ),
  // Flip the board's public flag (updateProjectV2). Returns the new state; the
  // JSON key is `public`, matching the Go handler's setVisibilityRequest.
  setProjectSyncVisibility: (id: string, isPublic: boolean) =>
    request<ProjectSyncVisibility>(
      "PUT",
      `/repos/${id}/github-project-sync/visibility`,
      { public: isPublic },
    ),
  // Grant a GitHub user Reader access to the board (204, empty body). WRITE-ONLY
  // by necessity (D2): GitHub exposes no readable collaborator list, so uzi can
  // grant but cannot enumerate current collaborators. A 422 means "no such user".
  shareProjectSync: (id: string, username: string) =>
    request<null>("POST", `/repos/${id}/github-project-sync/collaborators`, {
      username,
    }),
  // Revoke a GitHub user's access (204, empty body). The DELETE carries a body —
  // `request` JSON-stringifies it for any non-GET/HEAD method (D2, write-only).
  unshareProjectSync: (id: string, username: string) =>
    request<null>("DELETE", `/repos/${id}/github-project-sync/collaborators`, {
      username,
    }),

  getBoard: (repoId: string) =>
    request<{ board: Board }>("GET", `/repos/${repoId}/board`),
  configureColumns: (repoId: string, columns: { label_name: string }[]) =>
    request<{ board: Board }>("PUT", `/repos/${repoId}/board/columns`, {
      columns,
    }),
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
    request<{ card: Card }>("POST", `/repos/${repoId}/issues/${iid}/move`, {
      to_column: toColumn,
    }),
  // Promote a non-runnable issue by adding the `uzi` label (PRD #102 M6, Decision 15;
  // PRD #764). Forge-first and apply-only — there is no demote, so no boolean. The
  // returned card is authoritative — the caller replaces its card with it.
  promoteIssue: (repoId: string, iid: number) =>
    request<{ card: Card }>("POST", `/repos/${repoId}/issues/${iid}/promote`),
  syncRepo: (repoId: string) =>
    request<{ board: Board }>("POST", `/repos/${repoId}/sync`),
  createIssue: (repoId: string, title: string, description: string) =>
    request<{ card: Card }>("POST", `/repos/${repoId}/issues`, {
      title,
      description,
    }),

  // Per-user, per-repo board view preferences (PRD #196 M3). The stored row is the
  // single source of truth for the board's extras override and the "show all other
  // issues" toggle, replacing the M1 localStorage keys. GET returns the row (or the
  // pristine { extra_labels: null, show_all: false } when none exists); PUT writes it
  // and echoes back the stored row.
  getBoardPrefs: (repoId: string) =>
    request<BoardPrefs>("GET", `/repos/${repoId}/board/prefs`),
  setBoardPrefs: (repoId: string, prefs: BoardPrefs) =>
    request<BoardPrefs>("PUT", `/repos/${repoId}/board/prefs`, prefs),

  // Agent runtime (PRD #4).
  listWorkers: () => request<{ workers: Worker[] }>("GET", "/workers"),
  createWorker: (name: string, template?: string) =>
    request<{ worker: Worker; token: string }>("POST", "/workers", {
      name,
      template,
    }),
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
  provisionHostedWorker: (
    template: string,
    size: string,
    docker = false,
    name?: string,
  ) =>
    request<{ worker: Worker }>("POST", "/workers/hosted", {
      template,
      size,
      name,
      docker,
    }),

  createRun: (repoId: string, issueIid: number, force?: boolean) =>
    request<{ run: Run }>("POST", `/repos/${repoId}/runs`, {
      issue_iid: issueIid,
      ...(force ? { force: true } : {}),
    }),
  /** Queue a CI-fix run for a failed pipeline on a watched ref (PRD #6). */
  createCIFixRun: (repoId: string, ref: string) =>
    request<{ run: Run }>("POST", `/repos/${repoId}/ci-fix-runs`, { ref }),
  listRuns: (params?: {
    repoId?: string;
    issueIid?: number;
    passive?: boolean;
  }) => {
    const q = new URLSearchParams();
    if (params?.repoId) q.set("repo_id", params.repoId);
    if (params?.issueIid != null) q.set("issue_iid", String(params.issueIid));
    const qs = q.toString();
    // A passive poll (the hidden-tab favicon poll, #331) carries X-Uzi-Passive so
    // the server authenticates it but does NOT slide the session forward on it.
    return request<{ runs: RunListItem[] }>(
      "GET",
      qs ? `/runs?${qs}` : "/runs",
      undefined,
      params?.passive ? { "X-Uzi-Passive": "1" } : undefined,
    );
  },
  getRun: (id: string) => request<{ run: Run }>("GET", `/runs/${id}`),
  /** The caller's own token/cost usage (PRD #40): lifetime + last-7-days + run count. */
  getUsage: () => request<SelfUsage>("GET", "/usage"),
  /** Factory-wide usage + per-user breakdown (PRD #40). Admin-only — a non-admin 403s. */
  getAdminUsage: () => request<AdminUsage>("GET", "/admin/usage"),
  /** The caller's own Claude rate-limit reading (PRD #53): the two windows, or a
   *  no_token / unavailable status. Percentages only — the token never leaves the api. */
  getMyRateLimits: () =>
    request<MyRateLimitsResponse>("GET", "/me/rate-limits"),
  /** Every user's rate-limit reading (PRD #53). Admin-only — a non-admin 403s. */
  getAdminRateLimits: () =>
    request<AdminRateLimits>("GET", "/admin/rate-limits"),
  getRunMessages: (id: string, afterSeq = 0) =>
    request<{ messages: RunMessage[] }>(
      "GET",
      afterSeq > 0
        ? `/runs/${id}/messages?after=${afterSeq}`
        : `/runs/${id}/messages`,
    ),
  // The run's follow_up steer queue with delivery status (PRD #95). Owner-only: a
  // non-owner (incl. admin_ro) gets 404, which the caller treats as "no queue".
  getRunInputs: (id: string) =>
    request<{ inputs: SteerInput[] }>("GET", `/runs/${id}/inputs`),
  // A follow_up write returns the created row's id + created_at (PRD #95 S2) so the
  // web's optimistic queue entry adopts the real id and reconciles; other kinds omit
  // them (they are server-side or own their own UI). Both fields optional on the wire.
  submitRunInput: (
    id: string,
    kind: RunInputKind,
    body = "",
    selection?: AgentSelectionInput,
    // PRD #84 M4 4c: the "run without the capability" override, meaningful ONLY with
    // approve_plan (the server ignores it on any other kind). When true the server clears
    // the run's inferred/hinted required_capabilities before approving, so a plan the
    // capability gate would otherwise 409-BLOCK is approved anyway — the deliberate
    // false-positive-inference correction. Sent only when truthy so an ordinary approve
    // body is unchanged (default false server-side).
    overrideCapabilities?: boolean,
  ) =>
    request<{ server_side: boolean; id?: number; created_at?: string }>(
      "POST",
      `/runs/${id}/inputs`,
      {
        kind,
        body,
        // PRD #37: the structured agent selection is legal only on approve_plan; the
        // server ignores/validates it per kind. Omitted entirely when absent so a
        // plain follow-up/cancel body is unchanged.
        ...(selection ? { selection } : {}),
        ...(overrideCapabilities ? { override_capabilities: true } : {}),
      },
    ),

  /**
   * PRD #35: flip THIS run's usage-limit opt-in. Owner-scoped; returns the updated run.
   *
   * 🔴 IT CHANGES THE NEXT LIMIT, NOT THE CURRENT STATUS. Sending `false` to a run
   * that is already parked does NOT un-park, cancel or fail it — the park keeps its
   * clock and the run still resumes; only a future limit is affected. Cancelling is
   * `submitRunInput(id, "cancel")`, and conflating the two would be the expensive
   * mistake here, so it is written at the call site rather than left to the name.
   *
   * The server guards it with the same NEGATIVE predicate CancelRunServerSide uses
   * (`status NOT IN ('completed','failed','cancelled')`), so it is a no-op on a
   * terminal run and covers `limit_wait` for free. Callers still gate the control on
   * canToggleWaitOnLimit (lib/limitWait.ts) — that is the UI agreeing with the
   * server, not the enforcement.
   */
  setRunWaitOnLimit: (id: string, enabled: boolean) =>
    request<{ run: Run }>("PUT", `/runs/${id}/wait-on-limit`, { enabled }),

  /**
   * PRD #841: set (or clear) THIS run's per-run MR-review-rework override. `true`/`false`
   * is an explicit override; `null` clears back to inherit, so the watcher resolves the
   * effective value from the owner's Settings default. Unlike setRunWaitOnLimit this route
   * is RequireUser (cookie OR `uzc_` Bearer) and carries NO status guard — the watcher acts
   * AFTER the run completes, so the toggle stays live on a completed run whose MR is still
   * open. The write is inert once the MR merges/closes (the candidate query excludes a
   * non-`opened` MR), so the web hides the control then rather than the server rejecting it.
   * Returns the updated run.
   */
  setRunMrRework: (id: string, enabled: boolean | null) =>
    request<{ run: Run }>("PUT", `/runs/${id}/mr-rework`, { enabled }),

  /**
   * Issue #754: resume an `auto`-lane run parked at `pool_wait` (the owner's
   * Anthropic token pool was empty) right now, without waiting for a token to be
   * opted into the pool. No request body; returns the updated run, which moves to
   * `queued` on success.
   *
   * Owner-scoped (404 when not owned/unknown) and pool_wait-ONLY: the server 409s
   * (`run is not waiting for a pooled token`) when the run is not currently
   * parked at pool_wait — including a run that has already been resumed to
   * `queued`. Callers gate the control on `status === "pool_wait"`; the 409 is the
   * backstop that lets the panel surface "this run is no longer waiting" when the
   * WS/refetch has not yet caught up.
   */
  resumeRunNow: (id: string) =>
    request<{ run: Run }>("POST", `/runs/${id}/resume-now`),

  /**
   * PRD #320 M6: bump THIS run to the front of the queue (`expedite: true`) or clear
   * that override (`expedite: false`), returning the updated run with its recomputed
   * `priority` class. Owner-scoped (the server 404s a non-owner) and QUEUED-ONLY (409 on
   * a non-queued run), so callers gate the control on `status === "queued"` + ownership;
   * the 404/409 are the backstop, not the affordance. Clearing the override does NOT
   * cancel or restart the run — it only returns it to its natural rank.
   */
  expediteRun: (id: string, expedite: boolean) =>
    request<{ run: Run }>("PATCH", `/runs/${id}/priority`, { expedite }),

  // ── Run judge review (PRD #46 M4, PRD #119) ────────────────────────────────
  // getRunReview reads the verdict + recommendations for the run page (owner-or-
  // admin scoped server-side) PLUS the active judge run for the target. BOTH keys are
  // always present and either may be null, and they are independent: `review` is null
  // for a visible-but-unjudged run, `pending_judge` is set whenever a judge run for
  // this target is queued/claimed/running — including while a re-judge is in flight
  // over an existing verdict. Callers must read the pair: review:null alone does not
  // mean "nobody is judging this", which is exactly the confusion PRD #119 removes.
  // rerunJudge enqueues a fresh judge for a terminal run (owner-only spend), behind
  // the per-user spend limiter; the new verdict arrives asynchronously once the
  // judge run finishes, so callers re-fetch getRunReview.
  getRunReview: (id: string) =>
    request<{ review: RunReview | null; pending_judge: PendingJudge | null }>(
      "GET",
      `/runs/${id}/review`,
    ),
  rerunJudge: (id: string) =>
    request<{ run: Run }>("POST", `/runs/${id}/rejudge`),

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
  fileIssue: (
    runId: string,
    recId: string,
    body: { repo_id: string; title: string; description: string },
  ) =>
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

  // getJudgeCategoryStats is the canonical per-category GROUP count matrix (RequireUser,
  // owner-scoped, all-time, uncapped) the Judge filter chips render (PRD #270). It is a
  // SEPARATE endpoint from getJudgeStats deliberately: the nav badge reads only
  // TriageCounts.todo from /me/judge/stats, so a per-category payload has no path to it.
  // The matrix is bucket-keyed and triage-variant, so the page refetches it on every
  // disposition/undo/file mutation and on a run-anchor change (NOT once on mount) — but not
  // on a bucket-tab or category toggle, since all buckets arrive in one payload. `run` is
  // the notification deep-link anchor (mirrors getJudgeBacklog): it scopes the matrix to
  // groups recurring in that run while keeping their other-run occurrences.
  getJudgeCategoryStats: (run?: string) =>
    request<JudgeCategoryStats>(
      "GET",
      `/me/judge/category-stats${run ? `?run=${encodeURIComponent(run)}` : ""}`,
    ),

  // ── Judge menu — cross-run backlog + bulk disposition (PRD #98) ─────────────
  // getJudgeBacklog reads the deduped, grouped backlog (RequireUser, owner-scoped, no
  // token spend). bucket filters the group ROLLUP (default todo server-side); run is
  // the notification deep-link anchor (/judge?run={id}) — it keeps groups recurring in
  // that run while preserving their other-run occurrences. `triage` in the response is
  // the canonical count; render it, never re-derive from `groups`.
  getJudgeBacklog: (bucket?: JudgeBacklogBucket, run?: string, categories?: string[]) => {
    const qs = new URLSearchParams();
    if (bucket) qs.set("bucket", bucket);
    if (run) qs.set("run", run);
    // ?category= is a comma-joined list, enforced server-side before the row cap (PRD #235,
    // same shape as ?bucket=/?run=). Empty/absent → no param → all labels. The server does
    // NOT echo it back on the DTO (Decision 9); the page owns its own ?category= URL state.
    if (categories && categories.length) qs.set("category", categories.join(","));
    const suffix = qs.toString();
    return request<JudgeBacklog>(
      "GET",
      `/me/judge/recommendations${suffix ? `?${suffix}` : ""}`,
    );
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
    request<JudgeDispositionResult>(
      "PUT",
      "/me/judge/recommendations/disposition",
      {
        items,
        status,
        scope,
        ...(status === "dismissed" ? { reason } : {}),
      },
    ),

  // ── Incidental Findings backlog (PRD #333 M7) ───────────────────────────────
  // listFindings reads the coordinate-deduped, per-repo Findings backlog (RequireUser,
  // owner-scoped, no forge write, no token spend), mirroring getJudgeBacklog's shape.
  // bucket filters by disposition status (default to_file server-side); repo/run narrow by
  // coordinate repo and by a run semi-join (the notification deep-link anchor). `open_count`
  // in the response meta feeds the nav badge — render it, never re-derive from `findings`.
  listFindings: (bucket?: IncidentalFindingBucket, repo?: string, run?: string) => {
    const qs = new URLSearchParams();
    if (bucket) qs.set("bucket", bucket);
    if (repo) qs.set("repo", repo);
    if (run) qs.set("run", run);
    const suffix = qs.toString();
    return request<IncidentalFindingBacklog>("GET", `/findings${suffix ? `?${suffix}` : ""}`);
  },
  // findingIssueDraft reads the deterministic, human-editable filing draft for one finding
  // (D4). Every field is already sanitised server-side; the Edit-and-file panel seeds from it.
  findingIssueDraft: (id: string) =>
    request<IncidentalFindingIssueDraft>("GET", `/findings/${id}/issue-draft`),
  // fileFinding is the human-gated forge write (M5, D4/D5): claim-first, marker-labelled,
  // on the caller's own connection. The body is OPTIONAL — omitted, the server files the
  // stored (already-sanitised) title/description; supplied, its title/description/labels are
  // user EDITS re-run through the write-boundary sanitisers. A stale card acting on an
  // already-filed coordinate gets a 409 (the guarded claim), which the caller renders as a
  // friendly "already filed" state — the backlog is the source of truth.
  fileFinding: (id: string, body?: { title?: string; description?: string; labels?: string[] }) =>
    request<IncidentalFindingFileResult>("POST", `/findings/${id}/issue`, body ?? {}),
  // dismissFinding triages one coordinate to `dismissed` with a required reason from the
  // closed enum (M5). A LOCAL write — no forge call, no token spend.
  dismissFinding: (id: string, reason: "wont_do" | "not_an_issue") =>
    request<{ status: string; reason: string }>("POST", `/findings/${id}/dismiss`, { reason }),

  // ── Chat (PRD #39) — reconciled to M1's landed wire (Phase 3) ───────────────
  // The live view (messages, WS, replay) reuses getRun/getRunMessages/
  // createRunSocket with the chat's id — only these conversation verbs are new.
  // create/continue return a full runDTO under `run`; the list returns the Chat
  // view shape per item plus the max_turns envelope constant.
  listChats: () => request<ChatListResponse>("GET", "/chats"),
  createChat: (message: string) =>
    request<{ run: Run }>("POST", "/chats", { message }),
  // 202 {server_side}; the reply arrives over the run stream (mirrors submitRunInput).
  sendChatMessage: (id: string, message: string) =>
    request<{ server_side: boolean }>("POST", `/chats/${id}/messages`, {
      message,
    }),
  endChat: (id: string) =>
    request<{ server_side: boolean }>("POST", `/chats/${id}/end`),
  // Continue creates a NEW chat run carrying resume_of_run_id (Decision 11).
  continueChat: (id: string) =>
    request<{ run: Run }>("POST", `/chats/${id}/continue`),
  // The ONLY forge-write path from chat: session + CSRF, forge-first (Decision 8).
  // 200 {issue}: the real created issue (the card renders its link).
  confirmProposal: (chatId: string, proposalId: string) =>
    request<{ issue: CreatedIssue }>(
      "POST",
      `/chats/${chatId}/proposals/${proposalId}/confirm`,
    ),
  // 204 No Content: the card updates its state locally.
  dismissProposal: (chatId: string, proposalId: string) =>
    request<null>("POST", `/chats/${chatId}/proposals/${proposalId}/dismiss`),
  // Start a run from a chat's start-run card (PRD #191 M5). 201 {run}: gated exactly
  // as the board start button (StartRunForUser), so an issue with no PRD is refused
  // with the same message. Keyed by the human repo_path the card shows.
  startRunFromChat: (repoPath: string, issueIid: number) =>
    request<{ run: Run }>("POST", "/chats/run-requests", {
      repo_path: repoPath,
      issue_iid: issueIid,
    }),
  // Cancel a run from a chat's cancel card (PRD #322). 202: SubmitInput(cancel),
  // owner-scoped and terminality-guarded server-side. run_id is untrusted.
  cancelRunFromChat: (runId: string) =>
    request<{ server_side: boolean }>("POST", "/chats/cancel-requests", { run_id: runId }),
  // Steer a run from a chat's steer card (PRD #322). 202: SubmitInput(follow_up),
  // owner-scoped + terminality-guarded server-side; a chat-run target is refused 409.
  steerRunFromChat: (runId: string, message: string) =>
    request<{ server_side: boolean }>("POST", "/chats/steer-requests", { run_id: runId, message }),

  adminListWorkers: () =>
    request<{ workers: AdminWorker[] }>("GET", "/admin/workers"),
  adminListRuns: () => request<{ runs: RunListItem[] }>("GET", "/admin/runs"),
  // Admin cross-user blocked-repos list (PRD #66 M9, D8). Returns the envelope with
  // checks_unknown so the page can say "unknown" rather than "none blocked" (R1).
  adminListBlockedRepos: () =>
    request<AdminBlockedRepos>("GET", "/admin/blocked-repos"),

  // Notifications inbox (PRD #46 M2). listNotifications is the caller's own inbox;
  // { all: true } asks for every user's (admin only — a non-admin gets 403). The
  // envelope's `unread` is always the caller's own count (the bell badge).
  // unreadNotificationCount is the bell's lightweight poll (no rows).
  listNotifications: (params?: {
    all?: boolean;
    limit?: number;
    offset?: number;
  }) => {
    const q = new URLSearchParams();
    if (params?.all) q.set("all", "1");
    if (params?.limit != null) q.set("limit", String(params.limit));
    if (params?.offset != null) q.set("offset", String(params.offset));
    const qs = q.toString();
    return request<NotificationList>(
      "GET",
      qs ? `/notifications?${qs}` : "/notifications",
    );
  },
  unreadNotificationCount: () =>
    request<{ unread: number }>("GET", "/notifications/unread_count"),
  // Runs-in-progress count for the Runs nav badge (PRD #239). Owner-scoped, one
  // indexed count(*): the caller's non-terminal runs, kind NOT IN ('chat','judge')
  // — the same scope predicate the /runs page's ListRunsForUser uses (Decision 4).
  runsInProgressCount: () =>
    request<{ count: number }>("GET", "/me/runs/in-progress-count"),
  markNotificationRead: (id: string) =>
    request<{ notification: Notification }>(
      "POST",
      `/notifications/${id}/read`,
    ),

  // ── CLI tokens (PRD #64) — cookie-only CRUD ────────────────────────────────
  // A CLI token can never reach these endpoints (deliberate: a stolen token would
  // mint replacements, making revocation whack-a-mole) — they are the webui's own.
  // The mint returns the plaintext once; the list never carries a value.
  listCliTokens: () => request<{ tokens: CliToken[] }>("GET", "/me/cli-tokens"),
  createCliToken: (name: string, scope: CliTokenScope) =>
    request<CliTokenMint>("POST", "/me/cli-tokens", { name, scope }),
  revokeCliToken: (id: string) =>
    request<null>("DELETE", `/me/cli-tokens/${id}`),
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
    request<{ status: string }>("POST", "/auth/cli/deny", {
      request_id: requestId,
    }),

  // ── Agent memory (PRD #90 M6) — cookie-only, owner-scoped ──────────────────
  // list is newest-first across all the caller's repos (the component groups by
  // repo_name); delete is a single owner-scoped purge. The server derives the
  // owner from the session, so neither call carries a user_id.
  listMemory: () => request<{ memories: Memory[] }>("GET", "/me/memory"),
  deleteMemory: (id: string) => request<null>("DELETE", `/me/memory/${id}`),

  // ── Scheduled runs (PRD #241) — owner-scoped ───────────────────────────────
  // The list is a BARE JSON array (not an envelope); create/get/patch return a
  // bare ScheduleDTO. The scheduler fires each schedule through the same shared
  // seam autopilot uses, so a schedule can do nothing a manual start cannot.
  listSchedules: () => request<Schedule[]>("GET", "/me/schedules"),
  createSchedule: (repoId: string, input: ScheduleInput) =>
    request<Schedule>("POST", `/repos/${repoId}/schedules`, input),
  getSchedule: (id: string) => request<Schedule>("GET", `/schedules/${id}`),
  // PATCH merges (field present = apply, absent = keep). The per-row enable toggle
  // sends just { enabled }, which the server flips without re-validating the config.
  updateSchedule: (id: string, input: ScheduleInput) =>
    request<Schedule>("PATCH", `/schedules/${id}`, input),
  deleteSchedule: (id: string) => request<null>("DELETE", `/schedules/${id}`),
  // Fire immediately through the seam WITHOUT advancing the recurring cadence
  // (202). created counts the runs actually started (0 on a benign dedup skip).
  runScheduleNow: (id: string) =>
    request<RunNowResponse>("POST", `/schedules/${id}/run-now`),
  // Live "next fires" preview (RFC3339 UTC), computed from the same cron logic the
  // scheduler fires on, so the modal always matches server truth (Decision 6).
  previewSchedule: (input: SchedulePreviewInput) =>
    request<{ fires: string[] }>("POST", "/schedules/preview", input),

  // ── Pause all schedules (PRD #1093 D7) — the user-level kill switch ─────────
  // A singleton resource under the RequireUser schedules group (CLI-reachable). GET
  // reads the NORMALIZED live state (an expired `until` reads paused:false, until:null);
  // PUT pauses (body { until } — a null until is indefinite, a past until 422s); DELETE
  // resumes idempotently. All three return the state. Per-row `enabled` is never touched.
  getSchedulePause: () =>
    request<SchedulePauseDTO>("GET", "/schedules/pause"),
  putSchedulePause: (until: string | null) =>
    request<SchedulePauseDTO>("PUT", "/schedules/pause", { until }),
  deleteSchedulePause: () =>
    request<SchedulePauseDTO>("DELETE", "/schedules/pause"),

  // ── Default scheduled jobs (PRD #589) — owner-scoped ───────────────────────
  // The catalog view: the builtin default jobs plus the caller's per-repo
  // enablement state (which defaults are already enabled on which repos).
  listScheduleCatalog: () => request<ScheduleCatalog>("GET", "/schedule-catalog"),
  // Enable a catalog default on one repo (idempotent: 201 new / 200 already-enabled,
  // including a paused row returned untouched). Client fans out one call per repo.
  // An optional detected browser timezone (issue #660) seeds the new schedule's zone; it
  // is sent as the body ONLY when non-empty, so the no-tz path sends no body and stays
  // byte-identical to before (the server keeps the catalog/UTC zone on an absent tz, and
  // ignores the override on the idempotent re-enable path).
  enableCatalogSchedule: (repoId: string, slug: string, timezone?: string) =>
    request<Schedule>(
      "POST",
      `/repos/${repoId}/schedule-catalog/${slug}`,
      timezone ? { timezone } : undefined,
    ),
  // Restore a default row's editable fields to the catalog values and clear the
  // customized flag; a no-op-shaped 200 otherwise.
  resetSchedule: (id: string) => request<Schedule>("POST", `/schedules/${id}/reset`),
  // Clone a schedule into a new origin='user' editable row. A cloned default bakes its
  // prompt/labels in with catalog_slug=null; repoId (optional) clones into another repo.
  cloneSchedule: (id: string, repoId?: string) =>
    request<Schedule>("POST", `/schedules/${id}/clone`, repoId ? { repo_id: repoId } : {}),
  // Add another repo to a custom schedule (PRD #636): replicates the source's config onto
  // repoId as a new independent sibling, stamping a shared sibling_group_id on both in one
  // server transaction. Returns the new sibling (carrying the group id). A duplicate repo
  // already in the group is a 409 (idempotent-safe); a foreign source/repo is a 404.
  addScheduleRepo: (id: string, repoId: string) =>
    request<Schedule>("POST", `/schedules/${id}/add-repo`, { repo_id: repoId }),
  // Sweep-label WARN: which of the given selector labels do NOT exist on the repo's forge.
  checkRepoLabels: (repoId: string, labels: string[]) =>
    request<{ missing: string[] }>("POST", `/repos/${repoId}/labels/check`, { labels }),
  // Sweep-label CONFIRM: create the given labels on the repo's forge (idempotent by name).
  ensureRepoLabels: (repoId: string, labels: string[]) =>
    request<{ ensured: string[] }>("POST", `/repos/${repoId}/labels/ensure`, { labels }),
};

// The one client the app talks to. `mockApi` implements the identical surface
// (typechecked against realApi's shape here), so pages never know which mode
// they run in.
export const api: typeof realApi = MOCK_MODE ? mockApi : realApi;

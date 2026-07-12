// The in-browser mock implementation of the API client. Same surface, same
// response shapes, zero network: every method resolves from the in-memory store
// after a small jittered delay (so loading states render believably). Board
// moves, template CRUD, worker tokens, run inputs — all work locally.

import {
  ApiError,
  type AgentSelectionInput,
  type AgentTemplate,
  type AgentTemplateInput,
  type AllocatedSkill,
  type AllocationsInput,
  type AppSettings,
  type Chat,
  type CreatedIssue,
  type Notification,
  type NotificationList,
  type PrivilegeReport,
  type Run,
  type SelfimproveConfig,
  type SelfimproveUpdate,
  type RunMessage,
  type SettingSource,
  type SettingsResponse,
  type SlackLink,
  type UpdateSettingsPayload,
  type RunInputKind,
  type SecretMeta,
  type Skill,
  type SkillCreateInput,
  type SkillUpdateInput,
  type TemplateAllocation,
  type TemplateAllocationsInput,
  type ToolAllowlistEntry,
  type ToolAllowlistWriteInput,
  type User,
  type UserSettings,
  type UserSettingsPatch,
} from "../lib/api";
import { isTheme, resolveTheme } from "../lib/theme";
import { bodyError, descriptionError, SKILL_NAME_RE } from "../lib/skills";
import {
  LIVE_RUN_ID,
  mockAdmin,
  mockAdminWorkers,
  mockAllocations,
  mockConnection,
  mockForgeConfig,
  mockNotifications,
  type MockNotification,
  mockRepos,
  mockReviews,
  type MockReview,
  mockRepoToolProfiles,
  mockSecrets,
  mockSkills,
  mockTemplates,
  mockToolAllowlist,
  mockUsers,
  mockWorkers,
  runListItem,
} from "./data";
import { ensureLive, handleInput, scheduleChatReply, startNewRun } from "./engine";
import { appendMessage, getProposal, getRun, nextRunId, patchRun, putProposal, state } from "./store";

const jitter = () => 90 + Math.random() * 180;
const delay = <T>(value: T, ms = jitter()): Promise<T> =>
  new Promise((resolve) => setTimeout(() => resolve(value), ms));

function requireSession(): User {
  if (!state.session) throw new ApiError(401, "authentication required");
  return state.session;
}

// ── Settings persistence (demo build) ────────────────────────────────────────
// The mock persists ONLY the settings maps to localStorage so a hard reload of
// the demo keeps the picked theme (and labels / worker model) instead of snapping
// back to seed — making no-flash + persistence witnessable end to end in the
// sanctioned preview vehicle. Runs, issues, workers, secrets etc. are
// deliberately NOT persisted. Versioned + shape-checked: a blob from an older
// seed schema (or a corrupt one) is discarded and re-seeded, never served, so
// stale demo state can't outlive a seed-schema change.
const MOCK_SETTINGS_KEY = "uzi.mock.v1";
const SEED_USER_SETTINGS: UserSettings = { default_model: null, theme: null };
const SEED_APP_SETTINGS: AppSettings = {
  prd_label: "PRD",
  autopilot_label: "autopilot",
  default_theme: "ember",
  prdless_enabled: "true",
  prdless_label: "PRDLESS",
  slack_enabled: "false",
  public_base_url: "http://127.0.0.1:8080",
  judge_enabled: "false",
  judge_model: "haiku",
};

interface PersistedSettings {
  v: 1;
  userSettings: UserSettings;
  appSettings: AppSettings;
}

// isPersistedSettings validates the version AND the shape (key presence + value
// types) so only a blob matching the current schema is trusted; anything else
// falls through to a fresh seed.
function isPersistedSettings(p: unknown): p is PersistedSettings {
  if (typeof p !== "object" || p === null) return false;
  const o = p as Record<string, unknown>;
  if (o.v !== 1) return false;
  const us = o.userSettings;
  const as = o.appSettings;
  if (typeof us !== "object" || us === null || typeof as !== "object" || as === null) return false;
  const u = us as Record<string, unknown>;
  const a = as as Record<string, unknown>;
  const okUser =
    (u.default_model === null || typeof u.default_model === "string") &&
    (u.theme === null || typeof u.theme === "string");
  const okApp =
    typeof a.prd_label === "string" &&
    typeof a.autopilot_label === "string" &&
    typeof a.default_theme === "string" &&
    typeof a.prdless_enabled === "string" &&
    typeof a.prdless_label === "string" &&
    typeof a.slack_enabled === "string" &&
    typeof a.public_base_url === "string" &&
    typeof a.judge_enabled === "string" &&
    typeof a.judge_model === "string";
  return okUser && okApp;
}

function loadSettings(): { userSettings: UserSettings; appSettings: AppSettings } {
  try {
    const raw = localStorage.getItem(MOCK_SETTINGS_KEY);
    if (raw) {
      const parsed: unknown = JSON.parse(raw);
      if (isPersistedSettings(parsed)) {
        return {
          userSettings: { ...parsed.userSettings },
          appSettings: { ...parsed.appSettings },
        };
      }
    }
  } catch {
    // Storage unavailable (private mode) or a corrupt/legacy blob: re-seed.
  }
  return { userSettings: { ...SEED_USER_SETTINGS }, appSettings: { ...SEED_APP_SETTINGS } };
}

// persistSettings write-throughs the current settings maps. Called from the
// putMySettings / updateSettings mock handlers after they mutate.
function persistSettings(): void {
  try {
    const blob: PersistedSettings = { v: 1, userSettings, appSettings };
    localStorage.setItem(MOCK_SETTINGS_KEY, JSON.stringify(blob));
  } catch {
    // Storage unavailable: the demo still works in-memory for this session.
  }
}

const loadedSettings = loadSettings();

// Mutable copies of seed collections (CRUD operates on these).
let templates: AgentTemplate[] = mockTemplates.map((t) => ({ ...t }));
let users: User[] = mockUsers.map((u) => ({ ...u }));
let notifications: MockNotification[] = mockNotifications.map((n) => ({ ...n }));
const reviews: MockReview[] = mockReviews.map((r) => ({ ...r, recommendations: r.recommendations.map((x) => ({ ...x })) }));
let selfimprove: SelfimproveConfig = {
  enabled: false,
  interval: "48h",
  repo_id: null,
  repo_path: null,
  user_id: null,
  user_email: null,
  last_run_at: null,
  active: false,
};
let secrets: SecretMeta[] = mockSecrets.map((s) => ({ ...s }));
let userSettings: UserSettings = loadedSettings.userSettings;
let workers = mockWorkers.map((w) => ({ ...w }));
let connections = [{ ...mockConnection }];
let repos = mockRepos.map((r) => ({ ...r }));
let skills: Skill[] = mockSkills.map((s) => ({ ...s }));
let allocations: Record<string, { shared: string[]; mine: string[] }> = Object.fromEntries(
  Object.entries(mockAllocations).map(([k, v]) => [k, { shared: [...v.shared], mine: [...v.mine] }]),
);
let appSettings: AppSettings = loadedSettings.appSettings;
// Slack secret tokens (PRD #25) are write-only: the demo tracks only whether one
// is configured, never a value, mirroring the real API's `secrets` map. There is
// no ENV overlay in the demo, so every key's source is db/default.
const slackSecrets: Record<string, boolean> = { slack_bot_token: false, slack_app_token: false };

// The current user's Slack linking state (PRD #25 M3). The demo starts unlinked;
// setting an override moves it to "pending" (a real deployment would then DM the
// target a Confirm card), and there is no inbound socket here to confirm it.
let slackLink: Omit<SlackLink, "state"> = { member_id: null, notify: true, resolved_id: null, confirmed: false };

// slackLinkResponse derives the state field the real API returns, so the mock and
// the server never disagree on how member_id/resolved_id/confirmed map to a state.
function slackLinkResponse(): { slack: SlackLink } {
  const state: SlackLink["state"] = !slackLink.resolved_id
    ? "unlinked"
    : slackLink.confirmed
      ? "confirmed"
      : "pending";
  return { slack: { ...slackLink, state } };
}

// settingsResponse builds the admin SettingsResponse from the mock's current
// state: readable non-secret values, per-secret configured flags, and per-key
// sources (all db/default — the demo has no ENV overlay).
function settingsResponse(): SettingsResponse {
  const sources: Record<string, SettingSource> = {};
  for (const key of Object.keys(appSettings)) sources[key] = "db";
  for (const key of Object.keys(slackSecrets)) sources[key] = slackSecrets[key] ? "db" : "default";
  // The demo has no real socket, so Slack is always "disabled" here.
  return { settings: { ...appSettings }, secrets: { ...slackSecrets }, sources, slack_status: "disabled" };
}
let templateCounter = 0;
let workerCounter = 0;
let skillCounter = 0;
// Tool allowlist + per-repo profiles (PRD #18 M4).
let toolAllowlist: ToolAllowlistEntry[] = mockToolAllowlist.map((e) => ({ ...e }));
const repoToolProfiles = new Map<string, string[]>(
  Object.entries(mockRepoToolProfiles).map(([k, v]) => [k, [...v]]),
);
let toolEntryCounter = 0;

// Template allocations (PRD #18 M7). Global defaults are seeded for every
// builtin/global template (no empty-means-all cliff); the per-user overlay maps
// a template id to a forced on/off decision.
const templateGlobalDefaults = new Set<string>(
  templates.filter((t) => t.scope !== "user").map((t) => t.id),
);
const templateOverrides = new Map<string, Map<string, boolean>>();

// The reserved lead names mirror the server's leadNameRe / worker LEAD_NAME_RE.
const LEAD_NAME_RE = /^(lead|orchestrator)$/i;

// visibleSkills mirrors the real read: admins see every scope, everyone else
// sees builtin ∪ global ∪ their own user skills.
function visibleSkills(me: User): Skill[] {
  return skills.filter((s) => me.is_admin || s.scope !== "user" || s.user_id === me.id);
}

// visibleTemplates mirrors the real read: builtin ∪ global ∪ own user templates
// (admins see all).
function visibleTemplates(me: User): AgentTemplate[] {
  return templates.filter((t) => me.is_admin || t.scope !== "user" || t.user_id === me.id);
}

// templateAllocationView resolves each visible template's allocation state for
// me: overlay wins, else the global default.
function templateAllocationView(me: User): TemplateAllocation[] {
  const overlay = templateOverrides.get(me.id) ?? new Map<string, boolean>();
  return visibleTemplates(me).map((t) => {
    const globalDefault = templateGlobalDefaults.has(t.id);
    const myOverride = overlay.has(t.id) ? (overlay.get(t.id) as boolean) : null;
    return {
      id: t.id,
      name: t.name,
      description: t.description,
      scope: t.scope,
      is_builtin: t.is_builtin,
      global_default: globalDefault,
      my_override: myOverride,
      effective: myOverride ?? globalDefault,
    };
  });
}

function toAllocated(id: string): AllocatedSkill | null {
  const s = skills.find((x) => x.id === id);
  return s ? { skill_id: s.id, name: s.name, description: s.description, scope: s.scope } : null;
}

function allocationView(templateId: string): { shared: AllocatedSkill[]; mine: AllocatedSkill[] } {
  const a = allocations[templateId] ?? { shared: [], mine: [] };
  const map = (ids: string[]) => ids.map(toAllocated).filter((x): x is AllocatedSkill => x !== null);
  return { shared: map(a.shared), mine: map(a.mine) };
}

function listRunsFor(): Run[] {
  return [...state.runs.values()].sort((a, b) => b.created_at.localeCompare(a.created_at));
}

// notifDTO maps an internal mock notification row to the API shape, attaching the
// owner block only for the admin all-view (own-scope rows carry no owner), exactly
// like the server's two query paths.
function notifDTO(n: MockNotification, includeOwner: boolean): Notification {
  return {
    id: n.id,
    kind: n.kind,
    payload: n.payload,
    run_id: n.run_id,
    review_id: n.review_id,
    read_at: n.read_at,
    created_at: n.created_at,
    ...(includeOwner
      ? { owner: { id: n.user_id, email: n.owner_email, display_name: n.owner_display_name } }
      : {}),
  };
}

// sessionBody is the auth/session bootstrap payload: the signed-in user, the
// current instance labels (PRD #19 M2), and the three resolved theme fields (PRD
// #21), mirroring the real API so the mocked SPA resolves them the same way.
function sessionBody() {
  return {
    user: requireSession(),
    prd_label: appSettings.prd_label,
    autopilot_label: appSettings.autopilot_label,
    theme: resolveTheme(userSettings.theme, appSettings.default_theme),
    theme_override: userSettings.theme,
    default_theme: appSettings.default_theme,
    prdless_label: appSettings.prdless_label,
    prdless_enabled: appSettings.prdless_enabled === "true",
    vault: { unlocked: state.vaultUnlocked },
  };
}

export const mockApi = {
  // ── Auth: instant and fake. Any credentials sign in as the admin. ──────────
  // The session bootstrap carries the instance labels alongside the user, mirroring
  // the real API (PRD #19 M2), so the mocked SPA resolves them the same way.
  register: async (email: string, _password: string, displayName: string) => {
    state.session = { ...mockAdmin, email, display_name: displayName || mockAdmin.display_name };
    return delay(sessionBody());
  },
  login: async (email: string, _password: string) => {
    // Persona switch for the demo: logging in as a seeded non-admin (e.g.
    // mira@uzi.local) signs in AS that user, so the non-admin rendering paths
    // (no Global create, view-only builtin/global, own-skills-only) are
    // browser-checkable. Any other email is the admin, as before.
    const persona = users.find((u) => u.email === email.trim().toLowerCase());
    state.session = persona ? { ...persona } : { ...mockAdmin, email: email || mockAdmin.email };
    return delay(sessionBody());
  },
  // Demo mode has registration open and unrestricted.
  authConfig: async () => delay({ registration_enabled: true, allowed_email_domains: [] }),
  logout: async () => {
    state.session = null;
    return delay({ status: "ok" });
  },
  me: async () => {
    if (!state.session) throw new ApiError(401, "authentication required");
    return delay(sessionBody(), 40);
  },

  // ── Admin: users ────────────────────────────────────────────────────────────
  listUsers: async () => delay({ users: users.map((u) => ({ ...u })) }),
  setUserActive: async (id: string, isActive: boolean) => {
    const u = users.find((x) => x.id === id);
    if (!u) throw new ApiError(404, "user not found");
    u.is_active = isActive;
    return delay({ user: { ...u } });
  },

  // ── Admin: instance settings (PRD #19) ───────────────────────────────────────
  // Mirrors the server's Decision 8 validation so the demo surfaces the same
  // rejection messages the real API would.
  getSettings: async () => delay(settingsResponse()),
  // Demo is fully DEK-sealed (no legacy rows), so the admin migration notice is
  // hidden; the wiring is still exercised by the AdminSettings unit test.
  vaultMigration: async () => delay({ master_sealed: 0 }),

  // ── Self-improvement config (PRD #46 M5) ─────────────────────────────────────
  getSelfimprove: async () => delay({ selfimprove: { ...selfimprove } }),
  updateSelfimprove: async (input: SelfimproveUpdate) => {
    const me = requireSession();
    selfimprove = { ...selfimprove, enabled: input.enabled };
    if (input.interval != null) selfimprove.interval = input.interval;
    if (input.enabled) {
      // The enabling admin becomes the owner (session identity, mirroring the server).
      selfimprove.user_id = me.id;
      selfimprove.user_email = me.email;
      if (input.repo_id != null) {
        selfimprove.repo_id = input.repo_id;
        selfimprove.repo_path = repos.find((r) => r.id === input.repo_id)?.path_with_namespace ?? null;
      }
    }
    return delay({ selfimprove: { ...selfimprove } });
  },
  updateSettings: async (updates: UpdateSettingsPayload) => {
    // Secret tokens are write-only: validated + recorded as configured, never
    // merged into the readable settings (mirrors the real structural exclusion).
    const nonSecret: Partial<AppSettings> = {};
    for (const [key, raw] of Object.entries(updates)) {
      const value = raw ?? "";
      if (key === "slack_bot_token" || key === "slack_app_token") {
        const prefix = key === "slack_bot_token" ? "xoxb-" : "xapp-";
        if (!value.startsWith(prefix)) {
          throw new ApiError(400, `${key}: token must start with ${prefix}`);
        }
        slackSecrets[key] = true;
        continue;
      }
      // default_theme routes to the theme registry, not the label rules (PRD #21).
      if (key === "default_theme") {
        if (!isTheme(value)) throw new ApiError(400, `default_theme: unknown theme: "${value}"`);
        nonSecret.default_theme = value;
        continue;
      }
      // prdless_enabled / slack_enabled / judge_enabled are strict bools, not labels.
      if (key === "prdless_enabled" || key === "slack_enabled" || key === "judge_enabled") {
        if (value !== "true" && value !== "false") {
          throw new ApiError(400, `${key}: must be "true" or "false"`);
        }
        (nonSecret as Record<string, string>)[key] = value;
        continue;
      }
      // judge_model is a model alias (PRD #46): non-empty single token, mirroring the
      // server's PRD #17 ValidateModel rules.
      if (key === "judge_model") {
        if (value.trim() === "") throw new ApiError(400, "judge_model: must not be empty");
        if (/\s/.test(value)) throw new ApiError(400, "judge_model: must be a single token with no spaces");
        nonSecret.judge_model = value;
        continue;
      }
      // public_base_url must be http(s) (PRD #25).
      if (key === "public_base_url") {
        if (!/^https?:\/\/.+/.test(value)) {
          throw new ApiError(400, "public_base_url: must use http or https");
        }
        nonSecret.public_base_url = value;
        continue;
      }
      if (key !== "prd_label" && key !== "autopilot_label" && key !== "prdless_label") {
        throw new ApiError(400, `unknown setting: ${key}`);
      }
      if (!value || value.trim() === "") throw new ApiError(400, `${key}: must not be empty`);
      if (value.length > 64) throw new ApiError(400, `${key}: must be at most 64 characters`);
      if (value.includes(",")) throw new ApiError(400, `${key}: must not contain a comma`);
      (nonSecret as Record<string, string>)[key] = value;
    }
    const merged = { ...appSettings, ...nonSecret };
    // The label triple must be pairwise-distinct (Decision 8 + PRD #22 Decision 7).
    if (merged.prd_label === merged.autopilot_label) {
      throw new ApiError(400, "prd_label and autopilot_label must differ");
    }
    if (merged.prdless_label === merged.prd_label) {
      throw new ApiError(400, "prdless_label must differ from prd_label");
    }
    if (merged.prdless_label === merged.autopilot_label) {
      throw new ApiError(400, "prdless_label must differ from autopilot_label");
    }
    appSettings = merged;
    persistSettings();
    return delay(settingsResponse());
  },

  // ── Autopilot opt-in (PRD #19 M3) ────────────────────────────────────────────
  setAutopilotEnabled: async (enabled: boolean) => {
    const u = requireSession();
    u.autopilot_enabled = enabled;
    return delay({ user: { ...u } }, 200);
  },

  // ── Run-judge opt-in (PRD #46) ───────────────────────────────────────────────
  // Own-user (session identity, never a body id, mirroring the server's audit H3).
  setJudgeEnabled: async (enabled: boolean) => {
    const u = requireSession();
    u.judge_enabled = enabled;
    return delay({ user: { ...u } }, 200);
  },
  // Admin per-user toggle: target from the id argument (the path on the server).
  setUserJudgeEnabled: async (id: string, enabled: boolean) => {
    const u = users.find((x) => x.id === id);
    if (!u) throw new ApiError(404, "user not found");
    u.judge_enabled = enabled;
    return delay({ user: { ...u } });
  },

  // ── Notifications inbox (PRD #46 M2) ─────────────────────────────────────────
  // Own view filters to the session user; { all: true } shows everyone but only
  // for an admin (else 403, like the server). `unread` is always the caller's own
  // count. Rows come back newest-first, paginated.
  listNotifications: async (params?: { all?: boolean; limit?: number; offset?: number }): Promise<NotificationList> => {
    const me = requireSession();
    const all = params?.all ?? false;
    if (all && !me.is_admin) throw new ApiError(403, "admin only");
    const limit = Math.min(Math.max(params?.limit ?? 30, 1), 100);
    const offset = Math.max(params?.offset ?? 0, 0);
    const scope = all ? notifications : notifications.filter((n) => n.user_id === me.id);
    const sorted = [...scope].sort((a, b) => b.created_at.localeCompare(a.created_at));
    const page = sorted.slice(offset, offset + limit).map((n) => notifDTO(n, all));
    const unread = notifications.filter((n) => n.user_id === me.id && !n.read_at).length;
    return delay({ notifications: page, unread, total: scope.length });
  },
  unreadNotificationCount: async () => {
    const me = requireSession();
    return delay({ unread: notifications.filter((n) => n.user_id === me.id && !n.read_at).length }, 40);
  },
  markNotificationRead: async (id: string) => {
    const me = requireSession();
    // Ownership is the (id, user_id) match — a foreign or unknown id is a 404,
    // exactly like the server's query.
    const n = notifications.find((x) => x.id === id && x.user_id === me.id);
    if (!n) throw new ApiError(404, "notification not found");
    if (!n.read_at) n.read_at = new Date().toISOString();
    return delay({ notification: notifDTO(n, false) });
  },

  // ── Secrets ─────────────────────────────────────────────────────────────────
  listSecrets: async () => delay({ secrets: secrets.map((s) => ({ ...s })) }),
  putAnthropicToken: async (_token: string) => {
    // Mirror the real API: a locked vault cannot seal a new token (PRD #32).
    if (!state.vaultUnlocked) {
      throw new ApiError(409, "vault is locked; unlock it with your password, then save again", {
        code: "vault_locked",
      });
    }
    const now = new Date().toISOString();
    const existing = secrets.find((s) => s.kind === "anthropic_token");
    if (existing) existing.updated_at = now;
    else secrets.push({ kind: "anthropic_token", created_at: now, updated_at: now });
    return delay({ secret: { ...secrets.find((s) => s.kind === "anthropic_token")! } });
  },

  // ── Vault (PRD #32) ───────────────────────────────────────────────────────────
  // Any non-empty password unlocks in the demo (there is no real crypto); an empty
  // password is treated as the "wrong password" 403 so the banner's error path is
  // browsable.
  vaultUnlock: async (password: string) => {
    if (password.trim() === "") throw new ApiError(403, "incorrect password");
    state.vaultUnlocked = true;
    return delay(null, 150);
  },
  vaultLock: async () => {
    state.vaultUnlocked = false;
    return delay(null, 100);
  },
  vaultStatus: async () => delay({ unlocked: state.vaultUnlocked }, 40),
  deleteAnthropicToken: async () => {
    secrets = secrets.filter((s) => s.kind !== "anthropic_token");
    return delay(null);
  },
  getMySettings: async () => delay({ settings: { ...userSettings } }),
  putMySettings: async (patch: UserSettingsPatch) => {
    // PATCH-like: apply only the fields present in the body, mirroring the real
    // handler so a theme-only save never clears the model and vice versa.
    if (patch.default_model !== undefined) {
      const trimmed = patch.default_model?.trim() ?? "";
      userSettings = { ...userSettings, default_model: trimmed === "" ? null : trimmed };
    }
    if (patch.theme !== undefined) {
      const t = patch.theme?.trim() ?? "";
      if (t !== "" && !isTheme(t)) throw new ApiError(400, `unknown theme: "${t}"`);
      userSettings = { ...userSettings, theme: t === "" ? null : t };
    }
    persistSettings();
    return delay({ settings: { ...userSettings } });
  },

  // ── Slack linking (PRD #25 M3) ───────────────────────────────────────────────
  getMySlack: async () => delay(slackLinkResponse()),
  setMySlackNotify: async (notify: boolean) => {
    slackLink = { ...slackLink, notify };
    return delay(slackLinkResponse());
  },
  setMySlackOverride: async (memberId: string | null) => {
    const member = memberId?.trim() ?? "";
    if (member === "") {
      // Clear the override: fall back to email auto-match (nothing resolved here).
      slackLink = { ...slackLink, member_id: null, resolved_id: null, confirmed: false };
    } else {
      if (!/^[A-Za-z0-9]{1,64}$/.test(member)) throw new ApiError(400, "invalid Slack member ID");
      // A set resets confirmation: the target must Confirm before content flows.
      slackLink = { ...slackLink, member_id: member, resolved_id: member, confirmed: false };
    }
    return delay(slackLinkResponse());
  },
  testMySlackDM: async () => {
    if (!slackLink.resolved_id) throw new ApiError(400, "no linked Slack account to send a test DM to");
    return delay({ status: "sent" });
  },
  getSlackStatus: async () => delay({ slack_status: "disabled" }),

  // ── Agent templates ─────────────────────────────────────────────────────────
  listAgentTemplates: async () => delay({ templates: templates.map((t) => ({ ...t })) }),
  getAgentTemplate: async (id: string) => {
    const t = templates.find((x) => x.id === id);
    if (!t) throw new ApiError(404, "template not found");
    return delay({ template: { ...t } });
  },
  createAgentTemplate: async (input: AgentTemplateInput) => {
    const me = requireSession();
    if (!input.name || !/^[a-z0-9]+(-[a-z0-9]+)*$/.test(input.name)) {
      throw new ApiError(400, "name must be kebab-case");
    }
    if (LEAD_NAME_RE.test(input.name)) {
      throw new ApiError(400, "name is reserved for the built-in lead orchestrator");
    }
    // Blank scope defaults to global (the pre-M6 admin create).
    const scope = input.scope ?? "global";
    if (scope === "global" && !me.is_admin) {
      throw new ApiError(403, "only admins can create global templates");
    }
    if (scope !== "global" && scope !== "user") {
      throw new ApiError(400, "scope must be 'global' or 'user'");
    }
    // Name uniqueness: shared names are unique across builtin+global; a user's
    // names are unique to that user (they may reuse a builtin/global name).
    const clash =
      scope === "user"
        ? templates.some((t) => t.scope === "user" && t.user_id === me.id && t.name === input.name)
        : templates.some((t) => t.scope !== "user" && t.name === input.name);
    if (clash) {
      throw new ApiError(409, "a template with this name already exists");
    }
    const now = new Date().toISOString();
    const t: AgentTemplate = {
      id: `t-custom-${++templateCounter}`,
      name: input.name,
      description: input.description,
      model: input.model,
      tools: input.tools,
      prompt_body: input.prompt_body,
      is_builtin: false,
      scope,
      user_id: scope === "user" ? me.id : null,
      updated_by: me.email,
      created_at: now,
      updated_at: now,
    };
    templates.push(t);
    // A new global template is a global default from creation (removable).
    if (scope === "global") templateGlobalDefaults.add(t.id);
    return delay({ template: { ...t } });
  },
  getTemplateAllocations: async () => delay({ templates: templateAllocationView(requireSession()) }),
  setTemplateAllocations: async (input: TemplateAllocationsInput) => {
    const me = requireSession();
    if (input.global_default_ids === undefined && input.my_overrides === undefined) {
      throw new ApiError(400, "provide global_default_ids and/or my_overrides");
    }
    const canSee = (id: string) => visibleTemplates(me).some((t) => t.id === id);
    if (input.global_default_ids !== undefined) {
      if (!me.is_admin) throw new ApiError(403, "only admins can set global default allocations");
      for (const id of input.global_default_ids) {
        const t = templates.find((x) => x.id === id);
        if (!t || t.scope === "user") {
          throw new ApiError(400, "only builtin or global templates can be global defaults");
        }
      }
      templateGlobalDefaults.clear();
      for (const id of input.global_default_ids) templateGlobalDefaults.add(id);
    }
    if (input.my_overrides !== undefined) {
      for (const o of input.my_overrides) {
        if (!canSee(o.template_id)) throw new ApiError(400, "one or more templates are not allocatable");
      }
      const overlay = new Map<string, boolean>();
      for (const o of input.my_overrides) overlay.set(o.template_id, o.enabled);
      templateOverrides.set(me.id, overlay);
    }
    return delay({ templates: templateAllocationView(me) });
  },
  updateAgentTemplate: async (id: string, input: AgentTemplateInput) => {
    const t = templates.find((x) => x.id === id);
    if (!t) throw new ApiError(404, "template not found");
    t.description = input.description;
    t.model = input.model;
    t.tools = input.tools;
    t.prompt_body = input.prompt_body;
    t.updated_by = requireSession().email;
    t.updated_at = new Date().toISOString();
    return delay({ template: { ...t } });
  },
  deleteAgentTemplate: async (id: string) => {
    const t = templates.find((x) => x.id === id);
    if (!t) throw new ApiError(404, "template not found");
    if (t.is_builtin) throw new ApiError(409, "builtin templates cannot be deleted");
    templates = templates.filter((x) => x.id !== id);
    return delay(null);
  },
  resetAgentTemplate: async (id: string) => {
    const t = templates.find((x) => x.id === id);
    if (!t) throw new ApiError(404, "template not found");
    if (!t.is_builtin) throw new ApiError(400, "only builtins can be reset");
    const shipped = mockTemplates.find((x) => x.id === id)!;
    Object.assign(t, { ...shipped, updated_at: new Date().toISOString() });
    return delay({ template: { ...t } });
  },

  // ── Agent skills (PRD #16) ────────────────────────────────────────────────
  listSkills: async () => delay({ skills: visibleSkills(requireSession()).map((s) => ({ ...s })) }),
  getSkill: async (id: string) => {
    const me = requireSession();
    const s = skills.find((x) => x.id === id);
    if (!s || (!me.is_admin && s.scope === "user" && s.user_id !== me.id)) {
      throw new ApiError(404, "skill not found");
    }
    return delay({ skill: { ...s } });
  },
  createSkill: async (input: SkillCreateInput) => {
    const me = requireSession();
    const name = input.name.trim();
    if (!SKILL_NAME_RE.test(name)) {
      throw new ApiError(400, "name must be kebab-case (lowercase letters, digits, hyphens; max 64 chars)");
    }
    if (input.scope === "global") {
      if (!me.is_admin) throw new ApiError(403, "only admins can create global skills");
    } else if (input.scope !== "user") {
      throw new ApiError(400, "scope must be 'global' or 'user'");
    }
    const descErr = descriptionError(input.description);
    if (descErr) throw new ApiError(400, descErr);
    const bErr = bodyError(input.body);
    if (bErr) throw new ApiError(400, bErr);
    const clash = skills.some((s) =>
      s.name === name &&
      (input.scope === "user" ? s.scope === "user" && s.user_id === me.id : s.scope !== "user"),
    );
    if (clash) throw new ApiError(409, "a skill with that name already exists");
    const now = new Date().toISOString();
    const s: Skill = {
      id: `skill-custom-${++skillCounter}`,
      name,
      description: input.description.trim(),
      body: input.body,
      scope: input.scope,
      user_id: input.scope === "user" ? me.id : null,
      updated_by: me.email,
      created_at: now,
      updated_at: now,
    };
    skills.push(s);
    return delay({ skill: { ...s } }, 300);
  },
  updateSkill: async (id: string, input: SkillUpdateInput) => {
    const me = requireSession();
    const s = skills.find((x) => x.id === id);
    if (!s) throw new ApiError(404, "skill not found");
    if (s.scope === "builtin" || s.scope === "global") {
      if (!me.is_admin) throw new ApiError(403, "you do not have permission to modify this skill");
    } else if (s.user_id !== me.id) {
      throw new ApiError(me.is_admin ? 403 : 404, me.is_admin ? "you do not have permission to modify this skill" : "skill not found");
    }
    const descErr = descriptionError(input.description);
    if (descErr) throw new ApiError(400, descErr);
    const bErr = bodyError(input.body);
    if (bErr) throw new ApiError(400, bErr);
    s.description = input.description.trim();
    s.body = input.body;
    s.updated_by = me.email;
    s.updated_at = new Date().toISOString();
    return delay({ skill: { ...s } });
  },
  deleteSkill: async (id: string) => {
    const me = requireSession();
    const s = skills.find((x) => x.id === id);
    if (!s) throw new ApiError(404, "skill not found");
    if (s.scope === "builtin") throw new ApiError(409, "builtin skills cannot be deleted; reset them instead");
    if (s.scope === "global") {
      if (!me.is_admin) throw new ApiError(403, "you do not have permission to modify this skill");
    } else if (s.user_id !== me.id) {
      throw new ApiError(me.is_admin ? 403 : 404, me.is_admin ? "you do not have permission to modify this skill" : "skill not found");
    }
    skills = skills.filter((x) => x.id !== id);
    return delay(null);
  },
  resetSkill: async (id: string) => {
    const me = requireSession();
    const s = skills.find((x) => x.id === id);
    if (!s) throw new ApiError(404, "skill not found");
    if (s.scope !== "builtin") throw new ApiError(400, "only builtin skills can be reset");
    if (!me.is_admin) throw new ApiError(403, "you do not have permission to modify this skill");
    const shipped = mockSkills.find((x) => x.id === id)!;
    s.description = shipped.description;
    s.body = shipped.body;
    s.updated_by = me.email;
    s.updated_at = new Date().toISOString();
    return delay({ skill: { ...s } });
  },
  getTemplateSkills: async (id: string) => {
    requireSession();
    if (!templates.some((t) => t.id === id)) throw new ApiError(404, "template not found");
    return delay({ allocations: allocationView(id) });
  },
  setTemplateSkills: async (id: string, input: AllocationsInput) => {
    const me = requireSession();
    if (!templates.some((t) => t.id === id)) throw new ApiError(404, "template not found");
    if (input.shared_skill_ids === undefined && input.my_skill_ids === undefined) {
      throw new ApiError(400, "provide shared_skill_ids and/or my_skill_ids");
    }
    const a = allocations[id] ?? { shared: [], mine: [] };
    if (input.shared_skill_ids !== undefined) {
      if (!me.is_admin) throw new ApiError(403, "only admins can set shared skill allocations");
      for (const sid of input.shared_skill_ids) {
        const sk = skills.find((x) => x.id === sid);
        if (!sk || (sk.scope !== "builtin" && sk.scope !== "global")) {
          throw new ApiError(400, "one or more skills are not allocatable");
        }
      }
      a.shared = [...new Set(input.shared_skill_ids)];
    }
    if (input.my_skill_ids !== undefined) {
      for (const sid of input.my_skill_ids) {
        const sk = skills.find((x) => x.id === sid);
        const ok = sk && (sk.scope === "builtin" || sk.scope === "global" || (sk.scope === "user" && sk.user_id === me.id));
        if (!ok) throw new ApiError(400, "one or more skills are not allocatable");
      }
      a.mine = [...new Set(input.my_skill_ids)];
    }
    allocations[id] = a;
    return delay({ allocations: allocationView(id) });
  },

  // ── Forge ───────────────────────────────────────────────────────────────────
  forgeConfig: async () => delay({ ...mockForgeConfig, allowed_base_urls: [...mockForgeConfig.allowed_base_urls] }),
  listConnections: async () => delay({ connections: connections.map((c) => ({ ...c })) }),
  createConnection: async (baseUrl: string, _token: string, forgeType = "gitlab") => {
    const conn = {
      ...mockConnection,
      id: `conn-${Date.now()}`,
      base_url: baseUrl,
      forge_type: forgeType,
      created_at: new Date().toISOString(),
      last_verified_at: new Date().toISOString(),
      // A freshly connected bot is unchecked until the first privilege check.
      privilege_status: null,
      privilege_checked_at: null,
      privilege_report: null,
    };
    connections = [conn];
    return delay({ connection: { ...conn } }, 600);
  },
  verifyConnection: async (id: string) => {
    const c = connections.find((x) => x.id === id);
    if (!c) throw new ApiError(404, "connection not found");
    c.last_verified_at = new Date().toISOString();
    return delay({ connection: { ...c } }, 500);
  },
  // Mirrors the real save path (PRD #19 M3): a collision on the same host is a hard
  // 409, an unknown username still saves but returns a warning (verified-or-warned),
  // and "" clears the mapping.
  updateConnection: async (id: string, humanUsername: string) => {
    const c = connections.find((x) => x.id === id);
    if (!c) throw new ApiError(404, "connection not found");
    const username = humanUsername.trim();
    if (username) {
      const clash = connections.some(
        (x) => x.id !== id && x.base_url === c.base_url && x.human_username === username,
      );
      if (clash) {
        throw new ApiError(409, "that forge username is already mapped by another user on this host");
      }
    }
    c.human_username = username || null;
    // Demo the warning branch for an obviously-fake username without a live forge.
    const warning =
      username && username.toLowerCase() === "ghost"
        ? "Saved, but no forge account with this username was found — double-check it matches your own forge username."
        : undefined;
    return delay({ connection: { ...c }, ...(warning ? { warning } : {}) }, 400);
  },
  privilegeCheck: async (id: string) => {
    const c = connections.find((x) => x.id === id);
    if (!c) throw new ApiError(404, "connection not found");
    const now = new Date().toISOString();
    const report: PrivilegeReport = {
      checked_at: now,
      status: "ok",
      token: { scopes: ["api"], active: true, violations: [], warnings: [] },
      repos: repos
        .filter((r) => r.enabled)
        .map((r) => ({
          repo_id: r.id,
          path: r.path_with_namespace,
          role: 30,
          member: true,
          violations: [],
          warnings: [],
        })),
    };
    c.privilege_status = "ok";
    c.privilege_checked_at = now;
    c.privilege_report = report;
    return delay({ report }, 500);
  },
  deleteConnection: async (id: string) => {
    connections = connections.filter((x) => x.id !== id);
    return delay(null);
  },
  listProjects: async (_connectionId: string) => delay({ repos: repos.map((r) => ({ ...r })) }, 350),

  listRepos: async () => delay({ repos: repos.filter((r) => r.enabled).map((r) => ({ ...r })) }),
  setRepoEnabled: async (id: string, enabled: boolean) => {
    const r = repos.find((x) => x.id === id);
    if (!r) throw new ApiError(404, "repo not found");
    r.enabled = enabled;
    return delay({ repo: { ...r } });
  },
  setRepoSkillsEnabled: async (id: string, enabled: boolean) => {
    const r = repos.find((x) => x.id === id);
    if (!r) throw new ApiError(404, "repo not found");
    r.repo_skills_enabled = enabled;
    return delay({ repo: { ...r } });
  },
  setRepoDevboxOptIn: async (id: string, enabled: boolean) => {
    const r = repos.find((x) => x.id === id);
    if (!r) throw new ApiError(404, "repo not found");
    r.repo_devbox_opt_in = enabled;
    return delay({ repo: { ...r } });
  },

  // ── Tool allowlist + repo tool profiles (PRD #18 M4) ─────────────────────────
  listToolAllowlist: async () => delay({ allowlist: toolAllowlist.map((e) => ({ ...e })) }),
  createToolAllowlistEntry: async (input: ToolAllowlistWriteInput) => {
    const name = (input.name ?? "").trim();
    if (name === "") throw new ApiError(400, "name is required");
    if (toolAllowlist.some((e) => e.name === name)) throw new ApiError(409, "that package is already on the allowlist");
    const now = new Date().toISOString();
    const entry: ToolAllowlistEntry = {
      id: `tal-${++toolEntryCounter}`,
      name,
      pinned_version: input.pinned_version?.trim() || null,
      note: input.note?.trim() || null,
      updated_by: requireSession().id,
      created_at: now,
      updated_at: now,
    };
    toolAllowlist = [...toolAllowlist, entry].sort((a, b) => a.name.localeCompare(b.name));
    return delay({ entry: { ...entry } });
  },
  updateToolAllowlistEntry: async (id: string, input: ToolAllowlistWriteInput) => {
    const entry = toolAllowlist.find((e) => e.id === id);
    if (!entry) throw new ApiError(404, "allowlist entry not found");
    entry.pinned_version = input.pinned_version?.trim() || null;
    entry.note = input.note?.trim() || null;
    entry.updated_at = new Date().toISOString();
    return delay({ entry: { ...entry } });
  },
  deleteToolAllowlistEntry: async (id: string) => {
    toolAllowlist = toolAllowlist.filter((e) => e.id !== id);
    return delay(null);
  },
  getRepoToolProfile: async (repoId: string) => {
    if (!repos.some((r) => r.id === repoId)) throw new ApiError(404, "repo not found");
    return delay({ packages: [...(repoToolProfiles.get(repoId) ?? [])] });
  },
  setRepoToolProfile: async (repoId: string, packages: string[]) => {
    if (!repos.some((r) => r.id === repoId)) throw new ApiError(404, "repo not found");
    // Mirror the server's allowlist validation so the demo rejects the same way.
    const allowed = new Set<string>();
    for (const e of toolAllowlist) allowed.add(e.pinned_version ? `${e.name}@${e.pinned_version}` : e.name);
    const rejected = packages.filter((p) => !allowed.has(p));
    if (rejected.length > 0) throw new ApiError(400, "these packages are not on the allowlist: " + rejected.join(", "));
    const cleaned = [...new Set(packages)].sort();
    repoToolProfiles.set(repoId, cleaned);
    return delay({ packages: cleaned });
  },

  // ── Board ───────────────────────────────────────────────────────────────────
  getBoard: async (repoId: string) => {
    const b = state.boards.get(repoId);
    if (!b) throw new ApiError(404, "board not found");
    return delay({ board: { ...b, cards: b.cards.map((c) => ({ ...c })) } });
  },
  configureColumns: async (repoId: string, columns: { label_name: string }[]) => {
    const b = state.boards.get(repoId);
    if (!b) throw new ApiError(404, "board not found");
    b.columns = columns.map((c, i) => ({ label_name: c.label_name, position: i }));
    const names = new Set(b.columns.map((c) => c.label_name));
    for (const card of b.cards) if (card.column && !names.has(card.column)) card.column = "";
    return delay({ board: { ...b, cards: b.cards.map((c) => ({ ...c })) } });
  },
  moveIssue: async (repoId: string, iid: number, toColumn: string) => {
    const b = state.boards.get(repoId);
    const card = b?.cards.find((c) => c.iid === iid);
    if (!b || !card) throw new ApiError(404, "issue not found");
    const to = toColumn === "open" ? "" : toColumn;
    const columnNames = b.columns.map((c) => c.label_name);
    const prd = appSettings.prd_label;
    card.labels = [prd, ...card.labels.filter((l) => l !== prd && !columnNames.includes(l)), ...(to ? [to] : [])];
    card.column = to;
    card.conflict = false;
    return delay({ card: { ...card } }, 320);
  },
  // PRDLESS label toggle (PRD #22 M4): 422 when disabled, else an idempotent
  // add/remove of the one label (mirrors the server's forge-first helper —
  // has_prd_link is untouched, every other label preserved).
  setIssuePrdless: async (repoId: string, iid: number, apply: boolean) => {
    const b = state.boards.get(repoId);
    const card = b?.cards.find((c) => c.iid === iid);
    if (!b || !card) throw new ApiError(404, "issue not found");
    if (appSettings.prdless_enabled !== "true") {
      throw new ApiError(422, "the PRDLESS label feature is disabled");
    }
    const label = appSettings.prdless_label;
    if (card.labels.includes(label) !== apply) {
      card.labels = apply ? [...card.labels, label] : card.labels.filter((l) => l !== label);
    }
    return delay({ card: { ...card } }, 320);
  },
  getIssue: async (repoId: string, iid: number) => {
    const b = state.boards.get(repoId);
    const card = b?.cards.find((c) => c.iid === iid);
    if (!b || !card) throw new ApiError(404, "issue not found");
    // IssueDetail is the card fields (minus latest_run) plus a live description.
    // Synthesize one consistent with has_prd_link so the "no PRD link" gate lines
    // up with what the description shows.
    const { latest_run: _latestRun, ...rest } = card;
    const description = card.has_prd_link
      ? `## Summary\n\nImplement the change described in the linked PRD.\n\nSee \`prds/${iid}-feature.md\` for the full specification.`
      : "This issue has no linked `prds/*.md` file yet, so an agent run cannot be started from it. Add a PRD link to the issue description on the forge to enable it.";
    return delay({ issue: { ...rest, description } });
  },
  syncRepo: async (repoId: string) => {
    const b = state.boards.get(repoId);
    if (!b) throw new ApiError(404, "board not found");
    return delay({ board: { ...b, cards: b.cards.map((c) => ({ ...c })) } }, 650);
  },
  createIssue: async (repoId: string, title: string, description: string) => {
    const b = state.boards.get(repoId);
    if (!b) throw new ApiError(404, "board not found");
    const iid = Math.max(0, ...b.cards.map((c) => c.iid)) + 1;
    const card = {
      iid,
      title,
      state: "opened",
      labels: [appSettings.prd_label],
      web_url: `${b.web_url}/-/issues/${iid}`,
      author: requireSession().display_name?.toLowerCase() ?? "you",
      has_prd_link: /prds\/[\w.-]+\.md/.test(description),
      column: "",
      closed: false,
      conflict: false,
      latest_run: null,
      pipeline: null,
    };
    b.cards.unshift(card);
    return delay({ card: { ...card } }, 450);
  },

  // ── Workers ─────────────────────────────────────────────────────────────────
  listWorkers: async () => delay({ workers: workers.map((w) => ({ ...w })) }),
  createWorker: async (name: string, template?: string) => {
    const w = {
      id: `w-new-${++workerCounter}`,
      name,
      status: "offline",
      busy: false,
      // No runs and no advertised cap until the worker registers (PRD #42).
      active_runs: 0,
      max_concurrent_runs: null,
      // Declared at issuance; reported stays null until the worker registers.
      template_declared: template ?? null,
      template_reported: null,
      version: null,
      last_heartbeat_at: null,
      created_at: new Date().toISOString(),
    };
    workers.push(w);
    const token = `uzi_wk_${Array.from(crypto.getRandomValues(new Uint8Array(18)), (b) => b.toString(16).padStart(2, "0")).join("")}`;
    return delay({ worker: { ...w }, token });
  },
  deleteWorker: async (id: string) => {
    workers = workers.filter((w) => w.id !== id);
    return delay(null);
  },

  // ── Runs ────────────────────────────────────────────────────────────────────
  createRun: async (repoId: string, issueIid: number) => {
    const b = state.boards.get(repoId);
    const card = b?.cards.find((c) => c.iid === issueIid);
    if (!b || !card) throw new ApiError(404, "issue not found");
    const active = [...state.runs.values()].some(
      (r) => r.repo_id === repoId && r.issue_iid === issueIid && !["completed", "failed", "cancelled"].includes(r.status),
    );
    if (active) throw new ApiError(409, "a run is already in progress for this issue");
    const now = new Date().toISOString();
    const run: Run = {
      id: nextRunId(),
      repo_id: repoId,
      kind: "issue",
      issue_iid: issueIid,
      issue_title: card.title,
      issue_description: "See the linked PRD.",
      title: null,
      resume_of_run_id: null,
      status: "queued",
      requeue_count: 0,
      iteration_count: 0,
      auto_approve: false,
      worker_id: null,
      branch: null,
      mr_iid: null,
      mr_state: null,
      failure_reason: null,
      stop_kind: null,
      pipeline_ref: null,
      pipeline_web_url: null,
      fix_verdict: null,
      plan_md: null,
      repo_agents: null,
      agent_source: null,
      agent_exclusions: null,
      own_agents: null,
      claimed_at: null,
      started_at: null,
      finished_at: null,
      created_at: now,
      updated_at: now,
    };
    state.runs.set(run.id, run);
    startNewRun(run.id);
    return delay({ run: { ...run } }, 350);
  },
  createCIFixRun: async (repoId: string, ref: string) => {
    if (!state.boards.get(repoId)) throw new ApiError(404, "repo not found");
    const active = [...state.runs.values()].some(
      (r) => r.repo_id === repoId && r.kind === "ci_fix" && r.pipeline_ref === ref && !["completed", "failed", "cancelled"].includes(r.status),
    );
    if (active) throw new ApiError(409, "an active CI-fix run already exists for this ref");
    const now = new Date().toISOString();
    const run: Run = {
      id: nextRunId(),
      repo_id: repoId,
      kind: "ci_fix",
      issue_iid: null,
      issue_title: `Fix CI: ${ref} pipeline`,
      issue_description: `Diagnose and fix the failed pipeline for \`${ref}\`.`,
      title: null,
      resume_of_run_id: null,
      status: "queued",
      requeue_count: 0,
      iteration_count: 0,
      auto_approve: false,
      worker_id: null,
      branch: null,
      mr_iid: null,
      mr_state: null,
      failure_reason: null,
      stop_kind: null,
      pipeline_ref: ref,
      pipeline_web_url: `https://gitlab.example.com/vtmocanu/uzi/-/pipelines/4242`,
      fix_verdict: null,
      plan_md: null,
      repo_agents: null,
      agent_source: null,
      agent_exclusions: null,
      own_agents: null,
      claimed_at: null,
      started_at: null,
      finished_at: null,
      created_at: now,
      updated_at: now,
    };
    state.runs.set(run.id, run);
    startNewRun(run.id);
    return delay({ run: { ...run } }, 350);
  },
  listRuns: async (params?: { repoId?: string; issueIid?: number }) =>
    delay({
      runs: listRunsFor()
        // Chat conversations ride runs but have their own page (PRD #39).
        .filter((r) => r.kind !== "chat")
        .filter((r) => (params?.repoId ? r.repo_id === params.repoId : true))
        .filter((r) => (params?.issueIid != null ? r.issue_iid === params.issueIid : true))
        .map((r) => runListItem(r)),
    }),
  getRun: async (id: string) => {
    const run = getRun(id);
    if (!run) throw new ApiError(404, "run not found");
    if (id === LIVE_RUN_ID) ensureLive(id);
    // Mirror the server's run-detail read (PRD #37 M4-fix): own_agents is resolved
    // here from the owner's templates (lead stripped), so the plan gate's "My agent
    // templates" card has chips in mock mode without a separate fetch.
    const own_agents = templates
      .filter((t) => !LEAD_NAME_RE.test(t.name))
      .map((t) => ({ name: t.name, description: t.description }));
    return delay({ run: { ...run, own_agents } }, 60);
  },
  // ── Run judge review (PRD #46 M4) ──────────────────────────────────────────
  getRunReview: async (id: string) => {
    if (!getRun(id)) throw new ApiError(404, "run not found");
    const review = reviews.find((r) => r.target_run_id === id);
    return delay(
      { review: review ? { ...review, recommendations: review.recommendations.map((x) => ({ ...x })) } : null },
      60,
    );
  },
  rerunJudge: async (id: string) => {
    const run = getRun(id);
    if (!run) throw new ApiError(404, "run not found");
    if (run.status !== "completed" && run.status !== "failed") {
      throw new ApiError(422, "this run cannot be judged");
    }
    if (run.kind !== "issue" && run.kind !== "ci_fix") {
      throw new ApiError(422, "this run cannot be judged");
    }
    // A mock judge run: no worker executes it, so the seeded review is unchanged —
    // the panel just shows the "re-queued" note. Cloning the target run yields a
    // valid Run shape for the envelope.
    const judge: Run = { ...run, id: nextRunId(), kind: "judge", status: "queued" };
    return delay({ run: judge }, 120);
  },
  getRunMessages: async (id: string, afterSeq = 0) => {
    const log = state.messages.get(id);
    if (!log) throw new ApiError(404, "run not found");
    return delay({ messages: log.filter((m) => m.seq > afterSeq).map((m) => ({ ...m })) }, 60);
  },
  submitRunInput: async (id: string, kind: RunInputKind, body = "", selection?: AgentSelectionInput) => {
    if (!getRun(id)) throw new ApiError(404, "run not found");
    handleInput(id, kind, body);
    // PRD #37: mirror the selection onto the run row so the mock's read-only
    // post-approval view has something to show.
    if (kind === "approve_plan" && selection) {
      patchRun(id, { agent_source: selection.source, agent_exclusions: selection.exclusions });
    }
    return delay({ server_side: false }, 150);
  },

  adminListWorkers: async () => delay({ workers: mockAdminWorkers.map((w) => ({ ...w })) }),
  adminListRuns: async () =>
    delay({
      runs: listRunsFor()
        .filter((r) => r.kind !== "chat")
        .filter((r) => !["completed", "failed", "cancelled"].includes(r.status))
        .map((r) => runListItem(r, requireSession().email)),
    }),

  // ── Chat (PRD #39) — real M1 wire ─────────────────────────────────────────
  listChats: async () =>
    delay({
      chats: [...state.runs.values()].filter((r) => r.kind === "chat").map((r) => chatDTO(r)),
      max_turns: CHAT_MAX_TURNS, // the envelope constant, not per-chat
    }),
  createChat: async (message: string) => {
    const now = new Date().toISOString();
    const run: Run = {
      id: nextRunId(),
      repo_id: null,
      kind: "chat",
      issue_iid: null,
      issue_title: truncateChatTitle(message),
      issue_description: "",
      title: truncateChatTitle(message),
      resume_of_run_id: null,
      status: "running",
      requeue_count: 0,
      iteration_count: 0,
      auto_approve: false,
      worker_id: "w-laptop",
      branch: null,
      mr_iid: null,
      mr_state: null,
      failure_reason: null,
      stop_kind: null,
      pipeline_ref: null,
      pipeline_web_url: null,
      fix_verdict: null,
      plan_md: null,
      repo_agents: null,
      agent_source: null,
      agent_exclusions: null,
      own_agents: null,
      claimed_at: now,
      started_at: now,
      finished_at: null,
      created_at: now,
      updated_at: now,
    };
    state.runs.set(run.id, run);
    state.messages.set(run.id, []);
    appendMessage(run.id, "user_message", null, { text: message });
    scheduleChatReply(run.id, message);
    return delay({ run: { ...run } }, 300);
  },
  sendChatMessage: async (id: string, message: string) => {
    const run = getRun(id);
    if (!run || run.kind !== "chat") throw new ApiError(404, "chat not found");
    if (["completed", "failed", "cancelled"].includes(run.status)) {
      throw new ApiError(409, "this conversation has ended");
    }
    appendMessage(id, "user_message", null, { text: message });
    scheduleChatReply(id, message);
    return delay({ server_side: false }, 150);
  },
  endChat: async (id: string) => {
    const run = getRun(id);
    if (!run || run.kind !== "chat") throw new ApiError(404, "chat not found");
    patchRun(id, { status: "completed", finished_at: new Date().toISOString() });
    return delay({ server_side: false }, 200);
  },
  continueChat: async (id: string) => {
    const src = getRun(id);
    if (!src || src.kind !== "chat") throw new ApiError(404, "chat not found");
    const now = new Date().toISOString();
    const run: Run = {
      ...src,
      id: nextRunId(),
      status: "running",
      resume_of_run_id: id,
      finished_at: null,
      created_at: now,
      updated_at: now,
    };
    state.runs.set(run.id, run);
    state.messages.set(run.id, []);
    appendMessage(run.id, "status", null, { text: "continuing the conversation on your worker" });
    return delay({ run: { ...run } }, 250);
  },
  confirmProposal: async (chatId: string, proposalId: string) => {
    const p = getProposal(proposalId);
    if (!p || p.run_id !== chatId) throw new ApiError(404, "proposal not found");
    if (p.status !== "pending") throw new ApiError(409, "proposal already resolved");
    // Mark resolved (a re-confirm 409s); the confirm response is the created issue.
    putProposal({ ...p, status: "confirmed" });
    const iid = 200 + Math.floor(Math.random() * 800);
    const issue: CreatedIssue = {
      iid,
      web_url: `https://gitlab.example.com/${p.repo_path ?? "grp/proj"}/-/issues/${iid}`,
      title: p.title,
    };
    // The created-issue link is appended to the conversation (Decision 8).
    appendMessage(chatId, "text", "chat", { text: `Created issue #${iid}: ${issue.web_url}` });
    return delay({ issue }, 350);
  },
  dismissProposal: async (chatId: string, proposalId: string) => {
    const p = getProposal(proposalId);
    if (!p || p.run_id !== chatId) throw new ApiError(404, "proposal not found");
    putProposal({ ...p, status: "dismissed" });
    appendMessage(chatId, "status", null, { text: "proposal dismissed — nothing written to the forge" });
    return delay(null, 200); // 204 No Content
  },
};

const CHAT_MAX_TURNS = 50;

// chatDTO derives the chatListDTO shape from a chat run + its message log: the
// title (the run's chat title, else derived from the first user turn), the
// user-turn count, and last activity from the newest message (PRD #39 wire). No
// max_turns here — that rides the list envelope as an instance constant.
function chatDTO(run: Run): Chat {
  const msgs: RunMessage[] = state.messages.get(run.id) ?? [];
  const firstUser = msgs.find((m) => m.kind === "user_message");
  const derived = (firstUser?.payload as { text?: string } | null)?.text;
  const title = run.title ?? (derived ? truncateChatTitle(derived) : run.issue_title || null);
  const turnCount = msgs.reduce((n, m) => (m.kind === "user_message" ? n + 1 : n), 0);
  return {
    id: run.id,
    title,
    status: run.status,
    turn_count: turnCount,
    resume_of_run_id: run.resume_of_run_id,
    last_message_at: msgs[msgs.length - 1]?.created_at ?? null,
    created_at: run.created_at,
    updated_at: run.updated_at,
  };
}

function truncateChatTitle(s: string): string {
  const t = s.trim().replace(/\s+/g, " ");
  return t.length > 60 ? `${t.slice(0, 59)}…` : t;
}

// A run patch helper other mock surfaces can use (kept for symmetry/tests).
export { patchRun };

// The in-browser mock implementation of the API client. Same surface, same
// response shapes, zero network: every method resolves from the in-memory store
// after a small jittered delay (so loading states render believably). Board
// moves, template CRUD, worker tokens, run inputs — all work locally.

import {
  ApiError,
  type AgentTemplate,
  type AgentTemplateInput,
  type AllocatedSkill,
  type AllocationsInput,
  type AppSettings,
  type PrivilegeReport,
  type Run,
  type RunInputKind,
  type SecretMeta,
  type Skill,
  type SkillCreateInput,
  type SkillUpdateInput,
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
  mockRepos,
  mockSecrets,
  mockSkills,
  mockTemplates,
  mockUsers,
  mockWorkers,
  runListItem,
} from "./data";
import { ensureLive, handleInput, startNewRun } from "./engine";
import { getRun, nextRunId, patchRun, state } from "./store";

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
    typeof a.prdless_label === "string";
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
let templateCounter = 0;
let workerCounter = 0;
let skillCounter = 0;

// visibleSkills mirrors the real read: admins see every scope, everyone else
// sees builtin ∪ global ∪ their own user skills.
function visibleSkills(me: User): Skill[] {
  return skills.filter((s) => me.is_admin || s.scope !== "user" || s.user_id === me.id);
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
  getSettings: async () => delay({ settings: { ...appSettings } }),
  updateSettings: async (updates: Partial<AppSettings>) => {
    const merged = { ...appSettings, ...updates };
    for (const [key, value] of Object.entries(updates)) {
      // default_theme routes to the theme registry, not the label rules (PRD #21).
      if (key === "default_theme") {
        if (!isTheme(value)) throw new ApiError(400, `default_theme: unknown theme: "${value}"`);
        continue;
      }
      // prdless_enabled is a strict bool, not a label (PRD #22 M1).
      if (key === "prdless_enabled") {
        if (value !== "true" && value !== "false") {
          throw new ApiError(400, `prdless_enabled: must be "true" or "false"`);
        }
        continue;
      }
      if (key !== "prd_label" && key !== "autopilot_label" && key !== "prdless_label") {
        throw new ApiError(400, `unknown setting: ${key}`);
      }
      if (!value || value.trim() === "") throw new ApiError(400, `${key}: must not be empty`);
      if (value.length > 64) throw new ApiError(400, `${key}: must be at most 64 characters`);
      if (value.includes(",")) throw new ApiError(400, `${key}: must not contain a comma`);
    }
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
    return delay({ settings: { ...appSettings } });
  },

  // ── Autopilot opt-in (PRD #19 M3) ────────────────────────────────────────────
  setAutopilotEnabled: async (enabled: boolean) => {
    const u = requireSession();
    u.autopilot_enabled = enabled;
    return delay({ user: { ...u } }, 200);
  },

  // ── Secrets ─────────────────────────────────────────────────────────────────
  listSecrets: async () => delay({ secrets: secrets.map((s) => ({ ...s })) }),
  putAnthropicToken: async (_token: string) => {
    const now = new Date().toISOString();
    const existing = secrets.find((s) => s.kind === "anthropic_token");
    if (existing) existing.updated_at = now;
    else secrets.push({ kind: "anthropic_token", created_at: now, updated_at: now });
    return delay({ secret: { ...secrets.find((s) => s.kind === "anthropic_token")! } });
  },
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

  // ── Agent templates ─────────────────────────────────────────────────────────
  listAgentTemplates: async () => delay({ templates: templates.map((t) => ({ ...t })) }),
  getAgentTemplate: async (id: string) => {
    const t = templates.find((x) => x.id === id);
    if (!t) throw new ApiError(404, "template not found");
    return delay({ template: { ...t } });
  },
  createAgentTemplate: async (input: AgentTemplateInput) => {
    if (!input.name || !/^[a-z0-9]+(-[a-z0-9]+)*$/.test(input.name)) {
      throw new ApiError(400, "name must be kebab-case");
    }
    if (templates.some((t) => t.name === input.name)) {
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
      updated_by: requireSession().email,
      created_at: now,
      updated_at: now,
    };
    templates.push(t);
    return delay({ template: { ...t } });
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
  createWorker: async (name: string) => {
    const w = {
      id: `w-new-${++workerCounter}`,
      name,
      status: "offline",
      busy: false,
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
      status: "queued",
      requeue_count: 0,
      iteration_count: 0,
      auto_approve: false,
      worker_id: null,
      branch: null,
      mr_iid: null,
      failure_reason: null,
      pipeline_ref: null,
      pipeline_web_url: null,
      fix_verdict: null,
      plan_md: null,
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
      status: "queued",
      requeue_count: 0,
      iteration_count: 0,
      auto_approve: false,
      worker_id: null,
      branch: null,
      mr_iid: null,
      failure_reason: null,
      pipeline_ref: ref,
      pipeline_web_url: `https://gitlab.example.com/vtmocanu/uzi/-/pipelines/4242`,
      fix_verdict: null,
      plan_md: null,
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
        .filter((r) => (params?.repoId ? r.repo_id === params.repoId : true))
        .filter((r) => (params?.issueIid != null ? r.issue_iid === params.issueIid : true))
        .map((r) => runListItem(r)),
    }),
  getRun: async (id: string) => {
    const run = getRun(id);
    if (!run) throw new ApiError(404, "run not found");
    if (id === LIVE_RUN_ID) ensureLive(id);
    return delay({ run: { ...run } }, 60);
  },
  getRunMessages: async (id: string, afterSeq = 0) => {
    const log = state.messages.get(id);
    if (!log) throw new ApiError(404, "run not found");
    return delay({ messages: log.filter((m) => m.seq > afterSeq).map((m) => ({ ...m })) }, 60);
  },
  submitRunInput: async (id: string, kind: RunInputKind, body = "") => {
    if (!getRun(id)) throw new ApiError(404, "run not found");
    handleInput(id, kind, body);
    return delay({ server_side: false }, 150);
  },

  adminListWorkers: async () => delay({ workers: mockAdminWorkers.map((w) => ({ ...w })) }),
  adminListRuns: async () =>
    delay({
      runs: listRunsFor()
        .filter((r) => !["completed", "failed", "cancelled"].includes(r.status))
        .map((r) => runListItem(r, requireSession().email)),
    }),
};

// A run patch helper other mock surfaces can use (kept for symmetry/tests).
export { patchRun };

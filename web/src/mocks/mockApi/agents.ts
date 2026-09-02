import type {
  AgentTemplate,
  AgentTemplateInput,
  AllocatedSkill,
  AllocationsInput,
  BuiltinDefinition,
  Skill,
  SkillCreateInput,
  SkillUpdateInput,
  TemplateAllocation,
  TemplateAllocationsInput,
  User,
} from "../../lib/api";
import { ApiError } from "../../lib/apiError";
import { bodyError, descriptionError, SKILL_NAME_RE } from "../../lib/skills";
import { mockAllocations, mockShippedBuiltins, mockSkills, mockTemplates } from "../data";
import { delay, requireSession } from "./shared";

// Mutable copies of seed collections (CRUD operates on these).
export let templates: AgentTemplate[] = mockTemplates.map((t) => ({ ...t }));
let skills: Skill[] = mockSkills.map((s) => ({ ...s }));
let allocations: Record<string, { shared: string[]; mine: string[] }> = Object.fromEntries(
  Object.entries(mockAllocations).map(([k, v]) => [k, { shared: [...v.shared], mine: [...v.mine] }]),
);
let templateCounter = 0;
let skillCounter = 0;

// Template allocations (PRD #18 M7). Global defaults are seeded for every
// builtin/global template (no empty-means-all cliff); the per-user overlay maps
// a template id to a forced on/off decision.
const templateGlobalDefaults = new Set<string>(
  templates.filter((t) => t.scope !== "user").map((t) => t.id),
);
const templateOverrides = new Map<string, Map<string, boolean>>();

// The reserved lead names mirror the server's leadNameRe / worker LEAD_NAME_RE.
export const LEAD_NAME_RE = /^(lead|orchestrator)$/i;

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

// shippedBuiltin is the mock's BuiltinByName: the definition this "release"
// carries under a name, or undefined for a builtin a later release dropped.
function shippedBuiltin(name: string): BuiltinDefinition | undefined {
  return mockShippedBuiltins.find((d) => d.name === name);
}

// sameContent mirrors the server's agenttmpl.SameContent over the four mutable
// columns. It is a SECOND IMPLEMENTATION of that comparison and that is
// deliberate: the mock has no server to ask, and a hard-coded flag would make
// every drift the fixture claims unfalsifiable. The rules it must keep in step
// with, each of which the server states its reason for:
//
//   - name is never compared (it is the lookup key);
//   - tools is order-SENSITIVE (the order is rendered), and null and [] both mean
//     inherit-all so they compare equal;
//   - description and prompt_body are compared exactly, never trimmed.
export function sameContent(row: AgentTemplate, def: BuiltinDefinition): boolean {
  const a = row.tools ?? [];
  const b = def.tools ?? [];
  return (
    row.description === def.description &&
    (row.model ?? "") === (def.model ?? "") &&
    row.prompt_body === def.prompt_body &&
    a.length === b.length &&
    a.every((t, i) => t === b[i])
  );
}

// withDrift stamps the computed differs_from_builtin onto a row on its way out,
// so the mock answers the same question the server does rather than serving a
// stored flag. False for anything with no shipped counterpart: a non-builtin
// scope (including a user row that merely shares a builtin's NAME) and a builtin
// this release no longer ships.
function withDrift(t: AgentTemplate): AgentTemplate {
  if (t.scope !== "builtin") return { ...t, differs_from_builtin: false };
  const def = shippedBuiltin(t.name);
  if (!def) return { ...t, differs_from_builtin: false };
  return { ...t, differs_from_builtin: !sameContent(t, def) };
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

export const agentsApi = {
  // ── Agent templates ─────────────────────────────────────────────────────────
  listAgentTemplates: async () => delay({ templates: templates.map(withDrift) }),
  getAgentTemplate: async (id: string) => {
    const t = templates.find((x) => x.id === id);
    if (!t) throw new ApiError(404, "template not found");
    return delay({ template: withDrift(t) });
  },
  // The shipped definition behind a builtin row, mirroring the server's status
  // matrix: 400 for a row with no shipped counterpart (including a user template
  // that merely shares a builtin's name) and 409 for a builtin this release no
  // longer ships — the state the UI reads as "do not offer Reset".
  getBuiltinAgentTemplate: async (id: string) => {
    const t = templates.find((x) => x.id === id);
    if (!t) throw new ApiError(404, "template not found");
    if (t.scope !== "builtin") {
      throw new ApiError(400, "only builtin templates have a shipped definition");
    }
    const def = shippedBuiltin(t.name);
    if (!def) throw new ApiError(409, "no builtin definition to reset to");
    return delay({ builtin: { ...def } });
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
      // Never a builtin, so never drifted. Recomputed on read anyway.
      differs_from_builtin: false,
    };
    templates.push(t);
    // A new global template is a global default from creation (removable).
    if (scope === "global") templateGlobalDefaults.add(t.id);
    return delay({ template: withDrift(t) });
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
    return delay({ template: withDrift(t) });
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
    // The reset target is the SHIPPED definition, not the seeded row. Those were
    // the same object before #201 M4a, which is why a "reset" could never clear a
    // badge: it restored the drifted seed. A builtin this release no longer ships
    // has nothing to reset to and answers 409, exactly as the server does.
    const shipped = shippedBuiltin(t.name);
    if (!shipped) throw new ApiError(409, "no builtin definition to reset to");
    Object.assign(t, {
      description: shipped.description,
      model: shipped.model,
      tools: shipped.tools,
      prompt_body: shipped.prompt_body,
      // Reset restores the embedded default in every sense, including provenance:
      // a previously synced/admin builtin becomes 'embedded' again (PRD #602 M5).
      origin: "embedded",
      updated_at: new Date().toISOString(),
    });
    return delay({ template: withDrift(t) });
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
};

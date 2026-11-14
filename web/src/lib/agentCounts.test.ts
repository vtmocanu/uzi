import { describe, it, expect } from "vitest";
import type { AgentTemplate, TemplateAllocation } from "./api";
import { agentCountModel, isShadowedTemplate, sharedTemplateNames } from "./agentCounts";

const VIEWER = "u-me";

function tmpl(id: string, name: string, scope: AgentTemplate["scope"], userId?: string): AgentTemplate {
  return {
    id,
    name,
    scope,
    description: "",
    model: null,
    codex_model: null,
    tools: null,
    prompt_body: "body",
    is_builtin: scope === "builtin",
    user_id: scope === "user" ? (userId ?? VIEWER) : null,
    updated_by: null,
    differs_from_builtin: false,
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
  };
}

function alloc(
  id: string,
  name: string,
  scope: TemplateAllocation["scope"],
  over: Partial<TemplateAllocation> = {},
): TemplateAllocation {
  return {
    id,
    name,
    scope,
    description: "",
    is_builtin: scope === "builtin",
    global_default: scope !== "user",
    my_override: null,
    effective: scope !== "user",
    ...over,
  };
}

const member = { isAdmin: false, viewerId: VIEWER };
const admin = { isAdmin: true, viewerId: VIEWER };

describe("sharedTemplateNames / isShadowedTemplate", () => {
  it("collects names of non-user rows only", () => {
    const names = sharedTemplateNames([
      { scope: "builtin", name: "coder" },
      { scope: "global", name: "ops" },
      { scope: "user", name: "mine" },
    ]);
    expect([...names].sort()).toEqual(["coder", "ops"]);
  });

  it("shadows only a user row whose name is shared", () => {
    const names = new Set(["coder"]);
    expect(isShadowedTemplate({ scope: "user", name: "coder" }, names)).toBe(true);
    expect(isShadowedTemplate({ scope: "user", name: "other" }, names)).toBe(false);
    expect(isShadowedTemplate({ scope: "builtin", name: "coder" }, names)).toBe(false);
  });
});

describe("agentCountModel", () => {
  it("returns an empty model for empty input", () => {
    expect(agentCountModel([], [], member)).toEqual({ total: 0, inRuns: 0, byScope: [] });
  });

  it("counts total and inRuns from effective, lead included", () => {
    const templates = [tmpl("b1", "lead", "builtin"), tmpl("b2", "coder", "builtin"), tmpl("b3", "tester", "builtin")];
    const allocs = [
      alloc("b1", "lead", "builtin"),
      alloc("b2", "coder", "builtin"),
      alloc("b3", "tester", "builtin", { effective: false }),
    ];
    const m = agentCountModel(allocs, templates, member);
    expect(m.total).toBe(3);
    expect(m.inRuns).toBe(2);
    expect(m.byScope).toEqual([{ scope: "builtin", label: "Builtin", total: 3, inRuns: 2 }]);
  });

  it("counts a shadowed user row in total but not inRuns", () => {
    const templates = [tmpl("b1", "coder", "builtin"), tmpl("u1", "coder", "user")];
    const allocs = [
      alloc("b1", "coder", "builtin"),
      alloc("u1", "coder", "user", { my_override: true, effective: true }),
    ];
    const m = agentCountModel(allocs, templates, member);
    expect(m.total).toBe(2);
    expect(m.inRuns).toBe(1);
    expect(m.byScope.find((s) => s.scope === "user")).toEqual({
      scope: "user",
      label: "Mine",
      total: 1,
      inRuns: 0,
    });
  });

  it("excludes another user's private row even for an admin", () => {
    const templates = [tmpl("b1", "coder", "builtin"), tmpl("u9", "theirs", "user", "u-other")];
    const allocs = [
      alloc("b1", "coder", "builtin"),
      alloc("u9", "theirs", "user", { my_override: true, effective: true }),
    ];
    const m = agentCountModel(allocs, templates, admin);
    expect(m.total).toBe(1);
    expect(m.inRuns).toBe(1);
    expect(m.byScope.map((s) => s.scope)).toEqual(["builtin"]);
  });

  it("shadows a user row by a shared template that has no allocation row (matches the page badge)", () => {
    const templates = [tmpl("g1", "coder", "global"), tmpl("u1", "coder", "user")];
    const allocs = [alloc("u1", "coder", "user", { effective: true })];
    const m = agentCountModel(allocs, templates, { isAdmin: false, viewerId: VIEWER });
    expect(m.total).toBe(1);
    expect(m.inRuns).toBe(0);
    expect(isShadowedTemplate(templates[1], sharedTemplateNames(templates))).toBe(true);
  });

  it("drops allocations with no matching template", () => {
    const templates = [tmpl("b1", "coder", "builtin")];
    const allocs = [alloc("b1", "coder", "builtin"), alloc("ghost", "ghost", "builtin")];
    expect(agentCountModel(allocs, templates, member).total).toBe(1);
  });

  it("omits a scope with zero rows and orders builtin, global, user", () => {
    const templates = [tmpl("b1", "coder", "builtin"), tmpl("u1", "mine", "user")];
    const allocs = [alloc("b1", "coder", "builtin"), alloc("u1", "mine", "user", { effective: true })];
    expect(agentCountModel(allocs, templates, member).byScope.map((s) => s.scope)).toEqual(["builtin", "user"]);

    const withGlobal = [...templates, tmpl("g1", "ops", "global")];
    const withGlobalAllocs = [...allocs, alloc("g1", "ops", "global")];
    expect(agentCountModel(withGlobalAllocs, withGlobal, member).byScope.map((s) => s.scope)).toEqual([
      "builtin",
      "global",
      "user",
    ]);
  });

  it("reports globalDefaults for admins and omits the key otherwise", () => {
    const templates = [tmpl("b1", "coder", "builtin"), tmpl("g1", "ops", "global"), tmpl("u1", "mine", "user")];
    const allocs = [
      alloc("b1", "coder", "builtin"),
      alloc("g1", "ops", "global", { global_default: false }),
      alloc("u1", "mine", "user"),
    ];
    expect(agentCountModel(allocs, templates, admin).globalDefaults).toBe(1);
    expect(agentCountModel(allocs, templates, member)).not.toHaveProperty("globalDefaults");
  });
});

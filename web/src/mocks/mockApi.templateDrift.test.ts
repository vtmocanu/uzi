import { afterEach, describe, it, expect, vi } from "vitest";
import { mockShippedBuiltins, mockTemplateDriftCases, mockTemplates } from "./data";

// Each test gets a fresh mockApi module so the in-memory template state starts
// from seed (the mock mutates rows in place).
async function fresh() {
  vi.resetModules();
  return (await import("./mockApi")).mockApi;
}

const ADMIN = "vlad@uzi.local";

afterEach(() => vi.resetModules());

// The fixture set is asserted, not assumed. A later edit that collapses two
// cases into one — or drops the removed-builtin row, or makes the "pristine"
// control drift — reds here instead of leaving every other test green over a
// case that no longer exists. §7 of the brief's Amendment 1 asks for exactly
// this, and names the golden-snapshot alternative as the trap: a golden agrees
// with whatever the fixture happens to be.
describe("the drift fixture set still discriminates (issue #201 M4a)", () => {
  it("carries every named case, each present exactly once", () => {
    for (const id of Object.keys(mockTemplateDriftCases)) {
      const rows = mockTemplates.filter((t) => t.id === id);
      expect(rows, `fixture case "${mockTemplateDriftCases[id]}" (${id}) is missing`).toHaveLength(1);
    }
  });

  it("keeps the pristine control pristine and the drifted rows drifted, per column", async () => {
    const api = await fresh();
    await api.login(ADMIN, "x");
    const { templates } = await api.listAgentTemplates();
    const byId = new Map(templates.map((t) => [t.id, t]));

    // The control. If this ever badges, every "drifted" assertion below is
    // measuring the fixture rather than the comparison.
    expect(byId.get("t-coder")!.differs_from_builtin).toBe(false);

    for (const id of ["t-reviewer", "t-auditor", "t-tester", "t-documenter", "t-fact-checker"]) {
      expect(byId.get(id)!.differs_from_builtin, `${mockTemplateDriftCases[id]} must report drift`).toBe(true);
    }

    // No shipped counterpart ⇒ false, whatever the content says.
    expect(byId.get("t-spec-keeper")!.differs_from_builtin).toBe(false); // removed builtin
    expect(byId.get("t-release-notes")!.differs_from_builtin).toBe(false); // global
    expect(byId.get("t-my-coder")!.differs_from_builtin).toBe(false); // user row named "coder"
  });

  it("differs from the shipped twin in exactly ONE column per drifted row", () => {
    const shipped = new Map(mockShippedBuiltins.map((d) => [d.name, d]));
    const columnsThatDiffer = (id: string) => {
      const row = mockTemplates.find((t) => t.id === id)!;
      const def = shipped.get(row.name)!;
      const a = def.tools ?? [];
      const b = row.tools ?? [];
      return [
        row.description !== def.description && "description",
        (row.model ?? "") !== (def.model ?? "") && "model",
        row.prompt_body !== def.prompt_body && "prompt_body",
        (a.length !== b.length || a.some((t, i) => t !== b[i])) && "tools",
      ].filter(Boolean);
    };

    expect(columnsThatDiffer("t-reviewer")).toEqual(["tools"]);
    expect(columnsThatDiffer("t-auditor")).toEqual(["tools"]);
    expect(columnsThatDiffer("t-tester")).toEqual(["prompt_body"]);
    expect(columnsThatDiffer("t-documenter")).toEqual(["model"]);
    expect(columnsThatDiffer("t-fact-checker")).toEqual(["description"]);
    expect(columnsThatDiffer("t-coder")).toEqual([]);
  });

  it("keeps the tools-ORDER case an order-only difference — nothing else pins do-not-sort", () => {
    const row = mockTemplates.find((t) => t.id === "t-reviewer")!;
    const def = mockShippedBuiltins.find((d) => d.name === "reviewer")!;
    const sorted = (xs: string[] | null) => [...(xs ?? [])].sort();
    expect(sorted(row.tools)).toEqual(sorted(def.tools)); // same members
    expect(row.tools).not.toEqual(def.tools); // different order
  });

  it("keeps the user-scope collision genuinely different in content, or it proves nothing", () => {
    // A name-only implementation is separated from a scope-checking one only when
    // the colliding row's CONTENT differs from the builtin's. Identical content
    // would report false under both.
    const row = mockTemplates.find((t) => t.id === "t-my-coder")!;
    const def = mockShippedBuiltins.find((d) => d.name === "coder")!;
    expect(row.scope).toBe("user");
    expect(row.name).toBe(def.name);
    expect(row.prompt_body).not.toBe(def.prompt_body);
  });
});

describe("mock GET /agent-templates/{id}/builtin", () => {
  it("serves the shipped definition for a builtin row", async () => {
    const api = await fresh();
    await api.login(ADMIN, "x");
    const { builtin } = await api.getBuiltinAgentTemplate("t-fact-checker");
    const def = mockShippedBuiltins.find((d) => d.name === "fact-checker")!;
    expect(builtin.description).toBe(def.description);
    expect(builtin.prompt_body).toBe(def.prompt_body);
    // ...and NOT the stored row, which differs in the description.
    const stored = mockTemplates.find((t) => t.id === "t-fact-checker")!;
    expect(builtin.description).not.toBe(stored.description);
  });

  it("409s for a builtin this release no longer ships, and 400s for a non-builtin", async () => {
    const api = await fresh();
    await api.login(ADMIN, "x");
    await expect(api.getBuiltinAgentTemplate("t-spec-keeper")).rejects.toMatchObject({ status: 409 });
    await expect(api.getBuiltinAgentTemplate("t-release-notes")).rejects.toMatchObject({ status: 400 });
    await expect(api.getBuiltinAgentTemplate("t-my-coder")).rejects.toMatchObject({ status: 400 });
  });
});

describe("mock reset restores the SHIPPED definition, not the seeded row", () => {
  it("clears the drift flag on the response itself, with no refetch", async () => {
    const api = await fresh();
    await api.login(ADMIN, "x");

    const before = (await api.getAgentTemplate("t-tester")).template;
    expect(before.differs_from_builtin).toBe(true);

    const after = (await api.resetAgentTemplate("t-tester")).template;
    expect(after.differs_from_builtin).toBe(false);
    expect(after.prompt_body).toBe(mockShippedBuiltins.find((d) => d.name === "tester")!.prompt_body);
  });

  it("409s rather than resetting a builtin with no shipped definition", async () => {
    const api = await fresh();
    await api.login(ADMIN, "x");
    await expect(api.resetAgentTemplate("t-spec-keeper")).rejects.toMatchObject({ status: 409 });
  });

  it("computes drift on a save rather than trusting the stored flag", async () => {
    const api = await fresh();
    await api.login(ADMIN, "x");

    const pristine = (await api.getAgentTemplate("t-coder")).template;
    expect(pristine.differs_from_builtin).toBe(false);

    const edited = (
      await api.updateAgentTemplate("t-coder", {
        description: pristine.description,
        model: pristine.model,
        tools: pristine.tools,
        prompt_body: `${pristine.prompt_body}\n- One more rule.`,
      })
    ).template;
    expect(edited.differs_from_builtin).toBe(true);

    // ...and back again: the flag follows the content, in both directions.
    const restored = (
      await api.updateAgentTemplate("t-coder", {
        description: pristine.description,
        model: pristine.model,
        tools: pristine.tools,
        prompt_body: pristine.prompt_body,
      })
    ).template;
    expect(restored.differs_from_builtin).toBe(false);
  });
});

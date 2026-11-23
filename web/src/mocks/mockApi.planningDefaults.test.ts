// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import fixture from "../../../fixtures/settings/planning-defaults-cases.json";

type Api = (typeof import("./mockApi"))["mockApi"];

async function reload(): Promise<Api> {
  localStorage.clear();
  vi.resetModules();
  return (await import("./mockApi")).mockApi;
}
beforeEach(() => { localStorage.clear(); vi.resetModules(); });
afterEach(() => vi.resetModules());

type Case = {
  id: string;
  harness?: "claude" | "codex";
  seed: Record<string, string | null>;
  body: Record<string, string | null>;
  expect: { [key: string]: string | number | null | undefined; status: number; error_contains?: string; planning_claude_model?: string | null; planning_claude_effort?: string | null; default_effort?: string };
};

// PRD #3252 M2: the mock PUT is a second implementation of the server's planning
// validation, fed the same cases TestPlanningDefaultsFixture runs against Go.
describe("mock planning defaults vs fixtures/settings/planning-defaults-cases.json", () => {
  it("applies every case's status and stored pair", async () => {
    const got = new Set<string>();
    for (const c of fixture.cases as unknown as Case[]) {
      const api = await reload();
      if (Object.keys(c.seed).length > 0) await api.putMySettings(c.seed);
      let status = 200;
      let message = "";
      try {
        await api.putMySettings(c.body);
      } catch (err) {
        status = (err as { status: number }).status;
        message = (err as Error).message;
      }
      expect(status, c.id).toBe(c.expect.status);
      if (c.expect.error_contains) expect(message, c.id).toContain(c.expect.error_contains);
      const { settings } = await api.getMySettings();
      const harness = c.harness ?? "claude";
      expect(settings[`planning_${harness}_model`] ?? null, `${c.id} model`).toBe(c.expect[`planning_${harness}_model`]);
      expect(settings[`planning_${harness}_effort`] ?? null, `${c.id} effort`).toBe(c.expect[`planning_${harness}_effort`]);
      if (c.expect.default_codex_effort !== undefined) expect(settings.default_codex_effort, c.id).toBe(c.expect.default_codex_effort);
      if (c.expect.default_effort !== undefined) expect(settings.default_effort, `${c.id} default_effort`).toBe(c.expect.default_effort);
      got.add(c.id);
    }
    const ids = (fixture.cases as unknown as Case[]).map((c) => c.id);
    expect(new Set(ids).size, "duplicate fixture ids").toBe(ids.length);
    expect([...got].sort(), "exercised case ids").toEqual([...ids].sort());
  });

  // Mirrors agenttmpl.ValidateEffort / ValidateModel wording and rules (the server prefixes
  // each message with the field name).
  it("refuses values with the server's wording", async () => {
    const api = await reload();
    const refused = async (body: Record<string, string>) => {
      try {
        await api.putMySettings(body);
      } catch (err) {
        expect((err as { status: number }).status).toBe(400);
        return (err as Error).message;
      }
      return "accepted";
    };
    expect(await refused({ planning_claude_effort: "runtime" }))
      .toBe("planning_claude_effort: effort must be one of low, medium, high, xhigh, max");
    // 100 bytes pass, 101 fail: 34 three-byte characters are 102 bytes yet only 34 runes.
    expect(await refused({ planning_claude_model: "x".repeat(100) })).toBe("accepted");
    expect(await refused({ planning_claude_model: "x".repeat(101) }))
      .toBe("planning_claude_model: model must be at most 100 characters");
    expect(await refused({ planning_claude_model: "€".repeat(34) }))
      .toBe("planning_claude_model: model must be at most 100 characters");
    for (const bad of ["a\u0007b", "a‮b", "a​b", "a�b"]) {
      expect(await refused({ planning_claude_model: bad }), JSON.stringify(bad))
        .toBe("planning_claude_model: model must not contain newlines or control characters");
    }
    expect(await refused({ planning_claude_model: "two words" }))
      .toBe("planning_claude_model: model must be a single token with no spaces");
  });

  it("serves both harness planning cells, resolving like the server", async () => {
    const api = await reload();
    let { settings } = await api.getMySettings();
    // The default recording was taken with a lead template pinned (opus / gpt-6-sol), and the
    // server resolves the planning Default through that pin (planningModelCells' leadClaudePin).
    expect(settings.planning_models).toEqual([
      { harness: "claude", model: null, default_model: "opus", default_source: "template", resolved_model: "opus", source: "template" },
      { harness: "codex", model: null, default_model: "gpt-6-sol", default_source: "template", resolved_model: "gpt-6-sol", source: "template" },
    ]);
    const effective = await api.getEffectiveSettings();
    for (const h of ["claude", "codex"] as const) {
      const worker = effective.models.find(c => c.setting === "worker_model" && c.harness === h)!;
      expect(settings.planning_models!.find(c => c.harness === h), `${h} planning default matches the effective worker default`)
        .toMatchObject({ default_model: worker.default_model, default_source: worker.default_source });
    }
    expect(settings.planning_efforts?.filter(c => c.harness === "claude")).toEqual([
      { harness: "claude", effort: null, default_effort: "medium", default_source: "builtin", resolved_effort: "medium", source: "builtin", mode: "explicit" },
    ]);
    ({ settings } = await api.putMySettings({
      default_claude_model: "haiku", default_effort: "runtime", planning_claude_model: "opus", planning_claude_effort: "high",
    }));
    expect(settings.planning_models![0]).toMatchObject({ model: "opus", default_model: "haiku", default_source: "user_default", resolved_model: "opus", source: "user_planning_default" });
    expect(settings.planning_efforts![0]).toMatchObject({ effort: "high", default_effort: "runtime", default_source: "user_default", resolved_effort: "high", source: "user_planning_default", mode: "explicit" });
    ({ settings } = await api.putMySettings({ planning_claude_effort: null }));
    expect(settings.planning_efforts![0]).toMatchObject({ effort: null, default_effort: "runtime", resolved_effort: null, source: "user_default", mode: "runtime_default" });
  });

  // The server composes planningEffortCellFromOrdinary(..., leadEffortSettingCell(...)), so a
  // delivered lead template's per-harness effort is the planning default while the matching
  // preference is blank, and a stored planning level still wins.
  it.each(["claude", "codex"] as const)("%s planning default follows the delivered lead template effort", async (harness) => {
    const api = await reload();
    const { templates } = await import("./mockApi/agents");
    const lead = {
      ...templates[0]!, id: "b-lead-planning-cell", name: "lead", scope: "builtin" as const, is_builtin: true,
      model: null, codex_model: null, effort: harness === "claude" ? "high" : "low", codex_effort: harness === "codex" ? "high" : "low",
    };
    templates.push(lead);
    await api.setTemplateAllocations({ my_overrides: [{ template_id: lead.id, enabled: true }] });
    let { settings } = await api.getMySettings();
    expect(settings.planning_efforts!.find(c => c.harness === harness)).toMatchObject({
      effort: null, default_effort: "high", default_source: "template", resolved_effort: "high", source: "template", mode: "explicit",
    });
    ({ settings } = await api.putMySettings({ [`planning_${harness}_effort`]: "low" }));
    expect(settings.planning_efforts!.find(c => c.harness === harness)).toMatchObject({
      effort: "low", default_effort: "high", default_source: "template", resolved_effort: "low", source: "user_planning_default",
    });
    ({ settings } = await api.putMySettings({ [`planning_${harness}_effort`]: null, [harness === "claude" ? "default_effort" : "default_codex_effort"]: "max" }));
    expect(settings.planning_efforts!.find(c => c.harness === harness)).toMatchObject({
      default_effort: "max", default_source: "user_default", resolved_effort: "max", source: "user_default",
    });
  });
});

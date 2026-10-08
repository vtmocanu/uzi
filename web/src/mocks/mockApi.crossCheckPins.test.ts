// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { UserSettingsPatch } from "../lib/apiTypes";

async function fresh() {
  vi.resetModules();
  const api = (await import("./mockApi")).mockApi;
  await api.login("vlad@uzi.local", "x");
  return api;
}
beforeEach(() => {
  localStorage.clear();
  window.history.replaceState({}, "", "/");
});
afterEach(() => { vi.resetModules(); vi.doUnmock("./data"); });

describe("mock checker pins", () => {
  it("resolves two independent cells afresh, resets one field, and persists only stored overrides", async () => {
    let api = await fresh();
    let cells = (await api.getMySettings()).settings.cross_check_pins!;
    expect(cells).toHaveLength(2);
    expect(cells.find(p => p.harness === "codex")).toMatchObject({
      model: null, effort: null, resolved_model: "gpt-6.1-sol", resolved_effort: "medium", model_source: "worker default", effort_source: "worker default", active: true,
    });
    await api.putMySettings({ cross_check_pins: [
      { stage: "plan", harness: "claude", model: " custom-claude ", effort: "max" },
      { stage: "plan", harness: "codex", model: "custom-codex" },
    ] });
    await api.putMySettings({ default_claude_model: "haiku", default_codex_model: "worker-codex", default_codex_effort: "xhigh" });
    cells = (await api.getMySettings()).settings.cross_check_pins!;
    expect(cells.find(p => p.harness === "claude")).toMatchObject({ model: "custom-claude", effort: "max", resolved_model: "custom-claude", active: false });
    expect(cells.find(p => p.harness === "codex")).toMatchObject({ resolved_model: "custom-codex", resolved_effort: "xhigh", effort_source: "worker default" });
    cells[0].model = "mutated-return";
    expect((await api.getMySettings()).settings.cross_check_pins![0].model).toBe("custom-claude");
    await api.putMySettings({ cross_check_pins: [{ stage: "plan", harness: "codex", model: null, effort: "high" }] });
    api = await fresh();
    expect((await api.getMySettings()).settings.cross_check_pins!.find(p => p.harness === "codex")).toMatchObject({ model: null, effort: "high", resolved_model: "worker-codex", model_source: "worker default" });
    await api.putMySettings({ cross_check_pins: [{ stage: "plan", harness: "codex", effort: "  " }] });
    expect((await api.getMySettings()).settings.cross_check_pins!.find(p => p.harness === "codex")).toMatchObject({ model: null, effort: null, resolved_effort: "xhigh" });
  });

  it("preserves simultaneous model-only and effort-only patches and Go whitespace normalization", async () => {
    const api = await fresh();
    await Promise.all([
      api.putMySettings({ cross_check_pins: [{ stage: "plan", harness: "codex", model: "\u0085\tcustom-codex\r\u3000" }] }),
      api.putMySettings({ cross_check_pins: [{ stage: "plan", harness: "codex", effort: "max" }] }),
    ]);
    expect((await api.getMySettings()).settings.cross_check_pins!.find(p => p.harness === "codex")).toMatchObject({
      model: "custom-codex", effort: "max", resolved_model: "custom-codex", resolved_effort: "max",
    });
    await api.putMySettings({ cross_check_pins: [{ stage: "plan", harness: "codex", effort: null }] });
    expect((await api.getMySettings()).settings.cross_check_pins!.find(p => p.harness === "codex")).toMatchObject({
      model: "custom-codex", effort: null, resolved_effort: "medium",
    });
    expect((await api.getMySettings()).settings.default_codex_model).toBeNull();
    expect((await api.getMySettings()).settings.default_codex_effort).toBeNull();
  });

  it.each([
    null, {}, [null], [{ stage: "code", harness: "codex" }], [{ stage: "plan", harness: "other" }],
    [{ stage: "plan", harness: "codex", resolved_model: "bad" }],
    [{ stage: "plan", harness: "codex" }, { stage: "plan", harness: "codex" }],
    Array.from({ length: 3 }, () => ({ stage: "plan", harness: "claude" })),
  ])("rejects malformed list %j without changing worker fields", async list => {
    const api = await fresh();
    await expect(api.putMySettings({ default_effort: "max", cross_check_pins: list } as unknown as UserSettingsPatch)).rejects.toMatchObject({ status: 400 });
    expect((await api.getMySettings()).settings.default_effort).toBeNull();
  });

  it.each([
    ["model", "opus"], ["model", "two words"], ["model", "x".repeat(101)], ["model", "bad\u202e"],
    ["model", "\uFFFD"], ["model", "é".repeat(51)], ["model", "bad\uFEFF"], ["model", "a\u0085b"], ["model", "a\u0001b"], ["model", 4], ["effort", "extreme"], ["effort", []],
  ])("rejects bad codex %s atomically and identifies cell/field", async (field, value) => {
    const api = await fresh();
    await expect(api.putMySettings({ cross_check_pins: [
      { stage: "plan", harness: "claude", model: "sonnet" }, { stage: "plan", harness: "codex", [field]: value },
    ] } as UserSettingsPatch)).rejects.toMatchObject({ status: 400, message: expect.stringContaining(`[plan/codex].${field}`) });
    expect((await api.getMySettings()).settings.cross_check_pins!.every(p => p.model === null)).toBe(true);
  });

  it("rejects the closed Codex family in Claude, but accepts syntactically valid custom IDs and all efforts", async () => {
    const api = await fresh();
    await expect(api.putMySettings({ cross_check_pins: [{ stage: "plan", harness: "claude", model: "gpt-6.1-sol" }] })).rejects.toMatchObject({ status: 400 });
    for (const effort of ["low", "medium", "high", "xhigh", "max"]) {
      await api.putMySettings({ cross_check_pins: [{ stage: "plan", harness: "claude", model: "arbitrary.valid-id", effort }] });
      expect((await api.getMySettings()).settings.cross_check_pins!.find(p => p.harness === "claude")?.effort).toBe(effort);
    }
    const before = (await api.getMySettings()).settings;
    await api.putMySettings({ cross_check_pins: [] });
    await api.putMySettings({ cross_check_pins: [{ stage: "plan", harness: "claude" }] });
    expect((await api.getMySettings()).settings).toEqual(before);
  });

  it("resolves inherited Claude from delivered templates, stops at model-less lead, and ignores dormant pin for worker defaults", async () => {
    vi.doMock("./data", async importActual => {
      const actual = await importActual<typeof import("./data")>();
      const base = actual.mockTemplates[0];
      return { ...actual, mockTemplates: [...actual.mockTemplates,
        { ...base, id: "test-lead", name: "LEAD", model: "first", scope: "global", user_id: null },
        { ...base, id: "test-orchestrator", name: "orchestrator", model: "must-not-fall-through", scope: "global", user_id: null },
      ] };
    });
    const api = await fresh();
    const rows = (await api.listAgentTemplates()).templates;
    const lead = rows.filter(t => /^(lead|orchestrator)$/i.test(t.name)).sort((a, b) => a.name < b.name ? -1 : 1)[0];
    expect(lead).toBeTruthy();
    await api.updateAgentTemplate(lead.id, { description: lead.description, model: "custom-template", tools: lead.tools, prompt_body: lead.prompt_body });
    await api.putMySettings({ cross_check_pins: [{ stage: "plan", harness: "claude", effort: "max" }] });
    expect((await api.getMySettings()).settings.cross_check_pins!.find(p => p.harness === "claude")).toMatchObject({ resolved_model: "custom-template", resolved_effort: "max", active: false });
    await api.updateAgentTemplate(lead.id, { description: lead.description, model: null, tools: lead.tools, prompt_body: lead.prompt_body });
    expect((await api.getMySettings()).settings.cross_check_pins!.find(p => p.harness === "claude")?.resolved_model).toBeNull();
    await api.setTemplateAllocations({ global_default_ids: [] });
    expect((await api.getMySettings()).settings.cross_check_pins!.find(p => p.harness === "claude")?.resolved_model).toBeNull();
    await api.putMySettings({ default_claude_model: "haiku", cross_check_pins: [{ stage: "plan", harness: "claude", model: "sonnet" }] });
    expect((await api.getMySettings()).settings.default_claude_model).toBe("haiku");
  });

  it.each(["cross-check-mixed", "cross-check-dormant-claude"])("seeds %s and respects persisted changes on reload", async scenario => {
    window.history.replaceState({}, "", `/?mock=${scenario}`);
    let api = await fresh();
    const harness = scenario === "cross-check-mixed" ? "codex" : "claude";
    expect((await api.getMySettings()).settings.cross_check_pins!.find(p => p.harness === harness)?.model).toBe(scenario === "cross-check-mixed" ? "gpt-6-sol" : "sonnet");
    await api.putMySettings({ cross_check_pins: [{ stage: "plan", harness, model: null, effort: "low" }] });
    api = await fresh();
    expect((await api.getMySettings()).settings.cross_check_pins!.find(p => p.harness === harness)).toMatchObject({ model: null, effort: "low" });
  });
});

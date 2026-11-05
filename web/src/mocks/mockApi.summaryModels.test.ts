// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import summary from "../../../fixtures/model-settings/summary.json";
import { MOCK_SUMMARY_BUILTINS, resolveMockSummaryModel } from "./mockApi/summaryModels";
import { CODEX_ROLE_MODELS } from "../lib/codexModels";

async function reload() {
  vi.resetModules();
  return (await import("./mockApi")).mockApi;
}
beforeEach(() => { localStorage.clear(); vi.resetModules(); });
afterEach(() => vi.resetModules());

// PRD #3248 "Mock mirror (differential)": the mock resolver is a second implementation
// of agenttmpl.ResolveSummaryModel, so both are fed the same authored cases.
describe("mock summary resolver vs fixtures/model-settings/summary.json", () => {
  it("agrees with every shared case", () => {
    let ran = 0;
    for (const c of summary.cases) {
      const got = resolveMockSummaryModel(c.harness, c.user, c.instance);
      expect(got, c.name).toEqual(c.want);
      if (c.harness === "codex") expect(CODEX_ROLE_MODELS, c.name).toContain(got.model);
      ran++;
    }
    expect(ran).toBe(summary.cases.length);
    expect(ran).toBeGreaterThan(0);
  });

  it("pins the mock built-ins to the fixture's builtins", () => {
    expect(MOCK_SUMMARY_BUILTINS).toEqual(summary.builtins);
  });
});

describe("mock user settings: summary lanes (PRD #3248)", () => {
  it("seeds codex_summary_model null and returns both cells", async () => {
    const api = await reload();
    const { settings } = await api.getMySettings();
    expect(settings.codex_summary_model).toBeNull();
    expect(settings.summary_models).toEqual([
      { harness: "claude", model: null, default_model: "haiku", default_source: "builtin", resolved_model: "haiku", source: "builtin" },
      { harness: "codex", model: null, default_model: "gpt-6-luna", default_source: "builtin", resolved_model: "gpt-6-luna", source: "builtin" },
    ]);
  });

  it("trims and stores each lane, and blank or null clears it", async () => {
    const api = await reload();
    let { settings } = await api.putMySettings({ summary_model: " sonnet ", codex_summary_model: " gpt-6-astra " });
    expect(settings.summary_model).toBe("sonnet");
    expect(settings.codex_summary_model).toBe("gpt-6-astra");
    expect(settings.summary_models?.map((c) => [c.harness, c.resolved_model, c.source])).toEqual([
      ["claude", "sonnet", "user_default"],
      ["codex", "gpt-6-astra", "user_default"],
    ]);
    ({ settings } = await api.putMySettings({ codex_summary_model: "   " }));
    expect(settings.codex_summary_model).toBeNull();
    ({ settings } = await api.putMySettings({ summary_model: null }));
    expect(settings.summary_model).toBeNull();
    expect(settings.summary_models?.[1]).toMatchObject({ resolved_model: "gpt-6-luna", source: "builtin" });
  });

  it("accepts the private Codex role model in the Codex lane", async () => {
    const api = await reload();
    const { settings } = await api.putMySettings({ codex_summary_model: "gpt-6-luna" });
    expect(settings.codex_summary_model).toBe("gpt-6-luna");
  });

  it("refuses a Codex role id in the Claude lane and a non-role id in the Codex lane", async () => {
    const api = await reload();
    for (const id of ["gpt-6-astra", "gpt-6-luna"]) {
      await expect(api.putMySettings({ summary_model: id })).rejects.toMatchObject({
        status: 400, message: "summary_model: model is a Codex model id",
      });
    }
    await expect(api.putMySettings({ codex_summary_model: "opus" })).rejects.toMatchObject({
      status: 400, message: 'codex_summary_model: "opus" is not a supported Codex model',
    });
    const { settings } = await api.getMySettings();
    expect(settings.summary_model).toBeNull();
    expect(settings.codex_summary_model).toBeNull();
  });

  it("loads a persisted blob that predates the Codex summary lane", async () => {
    const api = await reload();
    await api.putMySettings({ summary_model: "sonnet" });
    const key = Array.from({ length: localStorage.length }, (_, i) => localStorage.key(i))
      .find((k) => k?.startsWith("uzi.mock."))!;
    const blob = JSON.parse(localStorage.getItem(key)!);
    delete blob.userSettings.codex_summary_model;
    localStorage.setItem(key, JSON.stringify(blob));
    const next = await reload();
    const { settings } = await next.getMySettings();
    expect(settings.summary_model).toBe("sonnet");
    expect(settings.codex_summary_model ?? null).toBeNull();
    expect(settings.summary_models?.[1]).toMatchObject({ resolved_model: "gpt-6-luna", source: "builtin" });
  });
});

// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import judge from "../../../fixtures/model-settings/judge.json";
import contract from "../../../fixtures/model-settings/contract.json";
import { MOCK_JUDGE_BUILTINS, resolveMockJudgeModel } from "./mockApi/judgeModels";
import { modelSettingSourceLabel } from "../lib/modelSettings";
import { CODEX_CURATED_MODELS } from "../lib/codexModels";

async function reload() {
  vi.resetModules();
  return (await import("./mockApi")).mockApi;
}
beforeEach(() => { localStorage.clear(); vi.resetModules(); });
afterEach(() => vi.resetModules());

// PRD #3245 "Mock mirror (differential)": the mock resolver is a second implementation
// of agenttmpl.ResolveJudgeModel, so both are fed the same authored cases.
describe("mock judge resolver vs fixtures/model-settings/judge.json", () => {
  it("agrees with every shared case", () => {
    let ran = 0;
    for (const c of judge.cases) {
      const got = resolveMockJudgeModel(c.harness, c.user, c.instance);
      expect(got, c.name).toEqual(c.want);
      if (c.harness === "codex") expect(CODEX_CURATED_MODELS, c.name).toContain(got.model);
      ran++;
    }
    expect(ran).toBe(judge.cases.length);
    expect(ran).toBeGreaterThan(0);
  });

  it("pins the mock built-ins to the fixture's builtins", () => {
    expect(MOCK_JUDGE_BUILTINS).toEqual(judge.builtins);
  });

  it("labels every contract.json source row the same way, and an unknown pair as unknown", () => {
    let ran = 0;
    for (const row of contract.source_labels) {
      expect(modelSettingSourceLabel(row.field, row.token)).toBe(row.label);
      ran++;
    }
    expect(ran).toBe(contract.source_labels.length);
    expect(modelSettingSourceLabel("model", "run_override")).toBe("unknown");
  });
});

describe("mock admin settings: judge keys (PRD #3245)", () => {
  it("reports both judge built-ins in defaults, equal to judge.json builtins", async () => {
    const api = await reload();
    const { defaults } = await api.getSettings();
    expect(defaults?.judge_model).toBe(judge.builtins.claude);
    expect(defaults?.codex_judge_model).toBe(judge.builtins.codex);
  });

  it("trims the Codex key, resets on blank, and reports source default when empty", async () => {
    const api = await reload();
    let resp = await api.updateSettings({ codex_judge_model: " gpt-6-sol " });
    expect(resp.settings.codex_judge_model).toBe("gpt-6-sol");
    expect(resp.sources.codex_judge_model).toBe("db");
    resp = await api.updateSettings({ codex_judge_model: "   " });
    expect(resp.settings.codex_judge_model).toBe(judge.builtins.codex);
    expect(resp.sources.codex_judge_model).toBe("default");
  });

  it("refuses a Codex role id in the Claude key and a non-curated id in the Codex key", async () => {
    const api = await reload();
    await expect(api.updateSettings({ judge_model: "gpt-6-luna" })).rejects.toMatchObject({
      status: 400, message: "judge_model: model is a Codex model id",
    });
    await expect(api.updateSettings({ codex_judge_model: "opus" })).rejects.toMatchObject({
      status: 400, message: "codex_judge_model: must be a Codex model; this is a Claude model alias",
    });
    await expect(api.updateSettings({ codex_judge_model: "gpt-6-luna" })).rejects.toMatchObject({
      status: 400, message: "codex_judge_model: is not a Codex judge model",
    });
    // Nothing was written by the refusals.
    const { settings, sources } = await api.getSettings();
    expect(settings.judge_model).toBe("opus");
    expect(sources.codex_judge_model).toBe("default");
  });
});

describe("mock user settings: judge lanes (PRD #3245)", () => {
  it("resolves judge_models from the user lanes and the instance values", async () => {
    const api = await reload();
    await api.updateSettings({ judge_model: "sonnet", codex_judge_model: "gpt-6-sol" });
    let { settings } = await api.getMySettings();
    expect(settings.judge_models).toEqual([
      { harness: "claude", model: null, default_model: "sonnet", default_source: "instance_default", resolved_model: "sonnet", source: "instance_default" },
      { harness: "codex", model: null, default_model: "gpt-6-sol", default_source: "instance_default", resolved_model: "gpt-6-sol", source: "instance_default" },
    ]);
    ({ settings } = await api.putMySettings({ judge_model: " haiku ", codex_judge_model: "gpt-6-astra" }));
    expect(settings.judge_model).toBe("haiku");
    expect(settings.codex_judge_model).toBe("gpt-6-astra");
    expect(settings.judge_models?.map((c) => [c.harness, c.resolved_model, c.source])).toEqual([
      ["claude", "haiku", "user_default"],
      ["codex", "gpt-6-astra", "user_default"],
    ]);
    // A null clears the lane back to the default.
    ({ settings } = await api.putMySettings({ codex_judge_model: null }));
    expect(settings.codex_judge_model).toBeNull();
    expect(settings.judge_models?.[1]).toMatchObject({ resolved_model: "gpt-6-sol", source: "instance_default" });
  });

  it("refuses a Codex role id in the Claude lane and a non-curated id in the Codex lane", async () => {
    const api = await reload();
    await expect(api.putMySettings({ judge_model: "gpt-6-astra" })).rejects.toMatchObject({
      status: 400, message: "judge_model: model is a Codex model id",
    });
    await expect(api.putMySettings({ judge_model: "gpt-6-luna" })).rejects.toMatchObject({
      status: 400, message: "judge_model: model is a Codex model id",
    });
    await expect(api.putMySettings({ codex_judge_model: "gpt-6-luna" })).rejects.toMatchObject({
      status: 400, message: 'codex_judge_model: "gpt-6-luna" is not a supported Codex model',
    });
    const { settings } = await api.getMySettings();
    expect(settings.judge_model).toBeNull();
    expect(settings.codex_judge_model).toBeNull();
  });

  it("derives the session's effective_judge_model from the same resolver (Claude lane)", async () => {
    const api = await reload();
    await api.login("admin@uzi.local", "whatever");
    await api.updateSettings({ judge_model: "sonnet" });
    expect((await api.me()).effective_judge_model).toBe("sonnet");
    await api.updateSettings({ judge_model: "" });
    expect((await api.me()).effective_judge_model).toBe(judge.builtins.claude);
  });

  it("loads a persisted blob that predates the Codex judge lane", async () => {
    const api = await reload();
    await api.updateSettings({ uzi_label: "eligible" });
    const key = Array.from({ length: localStorage.length }, (_, i) => localStorage.key(i))
      .find((k) => k?.startsWith("uzi.mock."))!;
    const blob = JSON.parse(localStorage.getItem(key)!);
    delete blob.appSettings.codex_judge_model;
    delete blob.userSettings.codex_judge_model;
    localStorage.setItem(key, JSON.stringify(blob));
    const next = await reload();
    const admin = await next.getSettings();
    expect(admin.settings).toMatchObject({ uzi_label: "eligible", codex_judge_model: judge.builtins.codex });
    expect(admin.sources.codex_judge_model).toBe("default");
    const { settings } = await next.getMySettings();
    expect(settings.judge_models?.[1]).toMatchObject({ resolved_model: judge.builtins.codex, source: "builtin" });
  });
});

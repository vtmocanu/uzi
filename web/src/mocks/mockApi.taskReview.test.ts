// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

async function reload() {
  vi.resetModules();
  return (await import("./mockApi")).mockApi;
}
beforeEach(() => { localStorage.clear(); vi.resetModules(); });
afterEach(() => vi.resetModules());

describe("mock user settings: task review (PRD #1570)", () => {
  it("seeds null models and efforts and returns the cells, inheriting the built-in medium", async () => {
    const api = await reload();
    const { settings } = await api.getMySettings();
    expect(settings.task_review_effort).toBeNull();
    expect(settings.codex_task_review_effort).toBeNull();
    expect(settings.task_review_efforts?.map((c) => [c.harness, c.effort, c.default_effort, c.resolved_effort, c.mode])).toEqual([
      ["claude", null, "medium", "medium", "explicit"],
      ["codex", null, "medium", "medium", "explicit"],
    ]);
  });

  it("stores and clears each effort key and inherits a runtime worker effort", async () => {
    const api = await reload();
    let { settings } = await api.putMySettings({ default_codex_effort: "runtime", default_effort: "high" });
    expect(settings.task_review_efforts?.[0]).toMatchObject({ default_effort: "high", resolved_effort: "high" });
    expect(settings.task_review_efforts?.[1]).toMatchObject({ default_effort: "runtime", resolved_effort: null, mode: "runtime_default" });
    ({ settings } = await api.putMySettings({ task_review_effort: " xhigh ", codex_task_review_effort: "runtime" }));
    expect(settings.task_review_effort).toBe("xhigh");
    expect(settings.codex_task_review_effort).toBe("runtime");
    expect(settings.task_review_efforts?.[0]).toMatchObject({ effort: "xhigh", resolved_effort: "xhigh", source: "user_default", mode: "explicit" });
    expect(settings.task_review_efforts?.[1]).toMatchObject({ effort: "runtime", resolved_effort: null, mode: "runtime_default" });
    ({ settings } = await api.putMySettings({ task_review_effort: null, codex_task_review_effort: "  " }));
    expect(settings.task_review_effort).toBeNull();
    expect(settings.codex_task_review_effort).toBeNull();
    expect(settings.task_review_efforts?.[0]).toMatchObject({ effort: null, resolved_effort: "high" });
  });

  it("refuses an invalid effort and writes nothing", async () => {
    const api = await reload();
    await expect(api.putMySettings({ task_review_effort: "turbo" })).rejects.toMatchObject({ status: 400 });
    await expect(api.putMySettings({ codex_task_review_effort: "turbo" })).rejects.toMatchObject({ status: 400 });
    const { settings } = await api.getMySettings();
    expect(settings.task_review_effort).toBeNull();
    expect(settings.codex_task_review_effort).toBeNull();
  });

  it("trims and stores each model lane, and blank or null clears it", async () => {
    const api = await reload();
    let { settings } = await api.putMySettings({ task_review_model: " opus ", codex_task_review_model: " gpt-6-astra " });
    expect(settings.task_review_model).toBe("opus");
    expect(settings.codex_task_review_model).toBe("gpt-6-astra");
    expect(settings.task_review_models?.map((c) => [c.harness, c.resolved_model, c.source])).toEqual([
      ["claude", "opus", "user_default"],
      ["codex", "gpt-6-astra", "user_default"],
    ]);
    ({ settings } = await api.putMySettings({ task_review_model: null, codex_task_review_model: "   " }));
    expect(settings.task_review_model).toBeNull();
    expect(settings.task_review_models?.[1]).toMatchObject({ resolved_model: "gpt-6-sol", source: "builtin" });
  });

  it("refuses a Codex id in the Claude lane, a non-Codex id in the Codex lane, and a spaced id", async () => {
    const api = await reload();
    await expect(api.putMySettings({ task_review_model: "gpt-6-astra" })).rejects.toMatchObject({
      status: 400, message: "task_review_model: model is a Codex model id",
    });
    await expect(api.putMySettings({ task_review_model: "two words" })).rejects.toMatchObject({ status: 400 });
    await expect(api.putMySettings({ codex_task_review_model: "opus" })).rejects.toMatchObject({
      status: 400, message: 'codex_task_review_model: "opus" is not a supported Codex model',
    });
    const { settings } = await api.getMySettings();
    expect(settings.task_review_model).toBeNull();
    expect(settings.codex_task_review_model).toBeNull();
  });
});

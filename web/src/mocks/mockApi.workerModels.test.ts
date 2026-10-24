// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { CODEX_ROLE_MODELS } from "../lib/codexModels";

async function fresh() {
  vi.resetModules();
  return (await import("./mockApi")).mockApi;
}
beforeEach(() => { localStorage.clear(); window.history.replaceState({}, "", "/"); });
afterEach(() => vi.resetModules());

describe("mock worker model Claude rejection", () => {
  it.each(CODEX_ROLE_MODELS)("rejects %s in explicit and legacy Claude writes atomically", async id => {
    let api = await fresh();
    await api.putMySettings({ default_harness: "claude", default_claude_model: "sonnet", default_codex_model: "codex-custom" });
    const before = await api.getMySettings();
    for (const modelPatch of [{ default_claude_model: ` ${id} ` }, { default_model: ` ${id} ` }]) {
      await expect(api.putMySettings({ ...modelPatch, default_harness: "claude", default_codex_model: "changed-custom", summary_model: "haiku" }))
        .rejects.toMatchObject({ status: 400, message: "default_claude_model: a Codex model is not valid for the Claude lane" });
      expect(await api.getMySettings()).toEqual(before);
      api = await fresh();
      expect(await api.getMySettings()).toEqual(before);
    }
  });

  it.each(["claude-private-model", "gpt-looking-custom"])("accepts custom %s and explicit/legacy resets", async id => {
    let api = await fresh();
    await api.putMySettings({ default_harness: "claude", default_claude_model: ` ${id} ` });
    api = await fresh();
    expect((await api.getMySettings()).settings.default_claude_model).toBe(id);
    await api.putMySettings({ default_claude_model: null });
    expect((await api.getMySettings()).settings.default_claude_model).toBeNull();
    await api.putMySettings({ default_model: ` ${id} ` });
    api = await fresh();
    expect((await api.getMySettings()).settings.default_claude_model).toBe(id);
    await api.putMySettings({ default_model: "   " });
    api = await fresh();
    expect((await api.getMySettings()).settings.default_claude_model).toBeNull();
    await api.putMySettings({ default_harness: "codex", default_model: id });
    api = await fresh();
    expect((await api.getMySettings()).settings.default_codex_model).toBe(id);
  });
});

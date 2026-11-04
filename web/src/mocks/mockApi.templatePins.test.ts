import { afterEach, describe, expect, it, vi } from "vitest";

afterEach(() => vi.resetModules());

async function fresh() {
  vi.resetModules();
  const { mockApi } = await import("./mockApi");
  await mockApi.login("vlad@uzi.local", "x");
  return { api: mockApi };
}

describe("mock template Codex pin contract", () => {
  it("defaults an omitted pin on create to null", async () => {
    const { api } = await fresh();
    const { template } = await api.createAgentTemplate({
      name: "pin-default", description: "d", model: null, tools: null, prompt_body: "b",
    });
    expect(template.codex_model).toBeNull();
  });

  it("preserves an omitted pin and its drift status", async () => {
    const { api } = await fresh();
    const { builtin } = await api.getBuiltinAgentTemplate("t-coder");
    const { template: pinned } = await api.updateAgentTemplate("t-coder", {
      ...builtin, codex_model: "gpt-6-sol",
    });
    expect(pinned).toMatchObject({ codex_model: "gpt-6-sol", differs_from_builtin: true });
    const { codex_model: _pin, ...oldClient } = builtin;
    const { template: kept } = await api.updateAgentTemplate("t-coder", oldClient);
    expect(kept).toMatchObject({ codex_model: "gpt-6-sol", differs_from_builtin: true });
    expect((await api.getAgentTemplate("t-coder")).template.codex_model).toBe("gpt-6-sol");
  });

  for (const clear of [null, "", "   "]) {
    it(`explicit ${JSON.stringify(clear)} clears the pin and returns shipped content without drift`, async () => {
      const { api } = await fresh();
      const { builtin } = await api.getBuiltinAgentTemplate("t-coder");
      await api.updateAgentTemplate("t-coder", { ...builtin, codex_model: "gpt-6-sol" });
      const { template } = await api.updateAgentTemplate("t-coder", { ...builtin, codex_model: clear });
      expect(template).toMatchObject({ codex_model: null, differs_from_builtin: false });
    });
  }

  it("trims a pin on create and Reset restores the builtin null", async () => {
    const { api } = await fresh();
    expect((await api.createAgentTemplate({
      name: "pin-trim", description: "d", model: null, codex_model: " gpt-6-sol ",
      tools: null, prompt_body: "b",
    })).template.codex_model).toBe("gpt-6-sol");
    const { builtin } = await api.getBuiltinAgentTemplate("t-coder");
    await api.updateAgentTemplate("t-coder", { ...builtin, codex_model: "gpt-6-sol" });
    expect((await api.resetAgentTemplate("t-coder")).template).toMatchObject({
      codex_model: null, differs_from_builtin: false, origin: "embedded",
    });
  });

  it("seeds Codex-only and dual-pin non-builtin examples", async () => {
    const { api } = await fresh();
    const { templates } = await api.listAgentTemplates();
    expect(templates.find((t) => t.id === "t-migration-writer")).toMatchObject({
      model: null, codex_model: "gpt-6-luna", scope: "global", differs_from_builtin: false,
    });
    expect(templates.find((t) => t.id === "t-my-coder")).toMatchObject({
      model: "sonnet", codex_model: "gpt-6-sol", scope: "user", differs_from_builtin: false,
    });
  });

  it.each([
    { model: "gpt-6-luna", codex_model: null },
    { model: null, codex_model: "sonnet" },
    { model: null, codex_model: "gpt-custom" },
  ])("rejects wrong-family create and update without mutating stored content: %j", async (pins) => {
    const { api } = await fresh();
    const content = { description: "d", tools: null, prompt_body: "b", ...pins };
    await expect(api.createAgentTemplate({ name: "invalid-pin", ...content })).rejects.toMatchObject({ status: 400 });
    const before = (await api.getAgentTemplate("t-coder")).template;
    await expect(api.updateAgentTemplate("t-coder", content)).rejects.toMatchObject({ status: 400 });
    expect((await api.getAgentTemplate("t-coder")).template).toEqual(before);
  });

});

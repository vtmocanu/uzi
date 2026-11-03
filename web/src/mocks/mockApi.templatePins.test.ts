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
});

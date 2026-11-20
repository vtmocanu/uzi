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
  seed: Record<string, string | null>;
  body: Record<string, string | null>;
  expect: { status: number; error_contains?: string; planning_claude_model?: string | null; planning_claude_effort?: string | null; default_effort?: string };
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
      expect(settings.planning_claude_model ?? null, `${c.id} model`).toBe(c.expect.planning_claude_model);
      expect(settings.planning_claude_effort ?? null, `${c.id} effort`).toBe(c.expect.planning_claude_effort);
      if (c.expect.default_effort !== undefined) expect(settings.default_effort, `${c.id} default_effort`).toBe(c.expect.default_effort);
      got.add(c.id);
    }
    const ids = (fixture.cases as unknown as Case[]).map((c) => c.id);
    expect(new Set(ids).size, "duplicate fixture ids").toBe(ids.length);
    expect([...got].sort(), "exercised case ids").toEqual([...ids].sort());
  });

  it("serves one Claude planning cell each, resolving like the server", async () => {
    const api = await reload();
    let { settings } = await api.getMySettings();
    expect(settings.planning_models).toEqual([
      { harness: "claude", model: null, default_model: "", default_source: "builtin", resolved_model: "", source: "builtin" },
    ]);
    expect(settings.planning_efforts).toEqual([
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
});

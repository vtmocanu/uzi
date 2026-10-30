// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import cases from "../../../fixtures/effort-defaults/cases.json";

type Api = (typeof import("./mockApi"))["mockApi"];

async function reload(): Promise<Api> {
  vi.resetModules();
  return (await import("./mockApi")).mockApi;
}
beforeEach(() => { localStorage.clear(); vi.resetModules(); });
afterEach(() => vi.resetModules());

const KEY: Record<string, "default_effort" | "default_codex_effort"> = {
  claude: "default_effort",
  codex: "default_codex_effort",
};

// Sets the stored ordinary preference for a harness. The PUT validator refuses corrupt
// tokens (and normalises blank to NULL), so a value the API cannot store is injected
// into the persisted blob and the mock reloaded, exactly as a stale row would arrive.
async function withStored(harness: string, stored: string | null): Promise<Api> {
  const api = await reload();
  const field = KEY[harness];
  await api.putMySettings({ [field]: "low" });
  const key = Array.from({ length: localStorage.length }, (_, i) => localStorage.key(i))
    .find((k) => k?.startsWith("uzi.mock."))!;
  const blob = JSON.parse(localStorage.getItem(key)!);
  blob.userSettings[field] = stored;
  localStorage.setItem(key, JSON.stringify(blob));
  return reload();
}

function assertExercised(section: string, got: Set<string>, want: { id: string }[]) {
  const ids = want.map((c) => c.id);
  expect(new Set(ids).size, `${section}: duplicate fixture ids`).toBe(ids.length);
  expect([...got].sort(), `${section}: exercised case ids`).toEqual([...ids].sort());
}

// PRD #3247 "Mock contract": the mock projection is a second implementation of
// agenttmpl.ResolveHarnessEffort / ValidateDefaultEffort, fed the Go test's cases.
describe("mock effort defaults vs fixtures/effort-defaults/cases.json", () => {
  it("projects every ordinary case's cell", async () => {
    const got = new Set<string>();
    for (const c of cases.ordinary) {
      const api = await withStored(c.harness, c.stored);
      const { settings } = await api.getMySettings();
      expect(settings.effort_settings?.map((x) => x.harness), c.id).toEqual(["claude", "codex"]);
      const cell = settings.effort_settings!.find((x) => x.harness === c.harness)!;
      expect(cell.mode, c.id).toBe(c.expect.mode);
      expect(cell.resolved_effort, c.id).toBe(c.expect.resolved_effort);
      expect(cell.source, c.id).toBe(c.expect.source);
      expect(cell.effort, c.id).toBe(c.stored);
      expect(cell.default_effort, c.id).toBe("medium");
      expect(cell.default_source, c.id).toBe("builtin");
      got.add(c.id);
    }
    assertExercised("ordinary", got, cases.ordinary);
  });

  it("validates every input through the user settings PUT", async () => {
    const got = new Set<string>();
    for (const c of cases.validators) {
      for (const field of ["default_effort", "default_codex_effort"] as const) {
        const api = await reload();
        if (c.accepted) {
          const { settings } = await api.putMySettings({ [field]: c.input });
          expect(settings[field], `${c.id} ${field}`).toBe(c.stored);
        } else {
          // Seed a known non-null value so "unchanged" cannot pass on leftover state.
          await api.putMySettings({ [field]: "low" });
          await expect(api.putMySettings({ [field]: c.input }), `${c.id} ${field}`).rejects.toMatchObject({
            status: 400,
            message: "effort must be one of low, medium, high, xhigh, max or runtime",
          });
          const { settings } = await api.getMySettings();
          expect(settings[field], `${c.id} ${field} unchanged`).toBe("low");
        }
      }
      got.add(c.id);
    }
    assertExercised("validators", got, cases.validators);
  });

  it("keeps checker effort independent of the ordinary preference", async () => {
    const got = new Set<string>();
    for (const c of cases.checker) {
      const api = await withStored(c.family, c.ordinary_stored);
      if (c.pin !== null) {
        await api.putMySettings({ cross_check_pins: [{ stage: c.stage as "plan" | "code", harness: c.family as "claude" | "codex", effort: c.pin }] });
      }
      const { settings } = await api.getMySettings();
      const cell = settings.cross_check_pins!.find((p) => p.stage === c.stage && p.harness === c.family)!;
      expect(cell.resolved_effort, c.id).toBe(c.expect.effort);
      expect(cell.effort_source, c.id).toBe(c.expect.source);
      got.add(c.id);
    }
    assertExercised("checker", got, cases.checker);
  });
});

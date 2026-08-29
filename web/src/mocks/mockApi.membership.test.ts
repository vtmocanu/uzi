// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";

// PRD #764: the mock is the browsable spec, so it re-implements the server's new
// settings validation (the single `uzi` label distinct from autopilot) and delivers
// `uzi_label` on the session. Each test reloads the module against a fresh (empty)
// localStorage so appSettings re-seeds from its defaults.

const KEY = "uzi.mock.v2";

function installStorage(initial: Record<string, string> = {}): void {
  const m = new Map<string, string>(Object.entries(initial));
  const storage = {
    getItem: (k: string) => (m.has(k) ? m.get(k)! : null),
    setItem: (k: string, v: string) => void m.set(k, String(v)),
    removeItem: (k: string) => void m.delete(k),
    clear: () => m.clear(),
    key: (i: number) => [...m.keys()][i] ?? null,
    get length() {
      return m.size;
    },
  } as Storage;
  Object.defineProperty(window, "localStorage", { configurable: true, value: storage });
}

async function reload() {
  vi.resetModules();
  return (await import("./mockApi")).mockApi;
}

beforeEach(() => installStorage({ [KEY]: "" }));
afterEach(() => vi.resetModules());

describe("mockApi — settings validation (PRD #764)", () => {
  it("rejects a uzi label equal to the autopilot label", async () => {
    const api = await reload();
    const err = await api.updateSettings({ uzi_label: "autopilot" }).catch((e) => e);
    // A 400 whose message names the differ requirement (ApiError, but the module
    // reload gives it a fresh class identity, so assert on the shape not instanceof).
    expect(err).toMatchObject({ status: 400 });
    expect(String(err.message)).toMatch(/differ/i);
  });

  it("rejects an empty uzi label", async () => {
    const api = await reload();
    await expect(api.updateSettings({ uzi_label: "" })).rejects.toMatchObject({ status: 400 });
  });

  it("rejects a uzi label containing a comma", async () => {
    const api = await reload();
    await expect(api.updateSettings({ uzi_label: "a,b" })).rejects.toMatchObject({ status: 400 });
  });

  it("accepts a valid update and round-trips uzi_label on GET", async () => {
    const api = await reload();
    await api.updateSettings({ uzi_label: "runnable" });
    const app = (await api.getSettings()).settings;
    expect(app.uzi_label).toBe("runnable");
  });
});

describe("mockApi — session payload (PRD #764)", () => {
  it("delivers the uzi label on the session (seed default)", async () => {
    const api = await reload();
    const session = await api.me();
    expect(session.uzi_label).toBe("uzi");
  });

  it("delivers the renamed uzi label on the session after an update", async () => {
    const api = await reload();
    await api.updateSettings({ uzi_label: "runnable" });
    const session = await api.me();
    expect(session.uzi_label).toBe("runnable");
  });
});

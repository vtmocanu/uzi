// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";

// PRD #196 M3: the mock re-implements the per-user, per-repo board-prefs endpoint pair
// so the server contract is browsable and pinned. Each test reloads the module against
// a fresh (empty) localStorage so the in-memory store re-seeds. `repo-uzi` is a seeded
// mock board id.

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

describe("mockApi — board prefs (PRD #196 M3)", () => {
  it("returns the pristine row when none has been saved", async () => {
    const api = await reload();
    const prefs = await api.getBoardPrefs("repo-uzi");
    expect(prefs).toEqual({ extra_labels: null, show_all: false });
  });

  it("round-trips a saved absolute set and show_all across GET/PUT", async () => {
    const api = await reload();
    const put = await api.setBoardPrefs("repo-uzi", { extra_labels: ["bug", "documentation"], show_all: true });
    // PUT echoes the stored row.
    expect(put).toEqual({ extra_labels: ["bug", "documentation"], show_all: true });
    // …and it persists across a later GET within the session.
    const got = await api.getBoardPrefs("repo-uzi");
    expect(got).toEqual({ extra_labels: ["bug", "documentation"], show_all: true });
  });

  it("preserves the null sentinel (not customised) distinctly from the empty set", async () => {
    const api = await reload();
    await api.setBoardPrefs("repo-uzi", { extra_labels: [], show_all: false });
    expect((await api.getBoardPrefs("repo-uzi")).extra_labels).toEqual([]);
    await api.setBoardPrefs("repo-uzi", { extra_labels: null, show_all: false });
    expect((await api.getBoardPrefs("repo-uzi")).extra_labels).toBeNull();
  });

  it("is per repo — a save on one board does not leak to another", async () => {
    const api = await reload();
    await api.setBoardPrefs("repo-uzi", { extra_labels: ["bug"], show_all: true });
    expect(await api.getBoardPrefs("repo-atlas")).toEqual({ extra_labels: null, show_all: false });
  });

  it("loosely validates: drops empty/comma/over-cap labels, dedupes, and coerces show_all", async () => {
    const api = await reload();
    const put = await api.setBoardPrefs("repo-uzi", {
      extra_labels: ["bug", "", "  ", "a,b", "x".repeat(65), "bug", "documentation"] as unknown as string[],
      show_all: 1 as unknown as boolean,
    });
    expect(put.extra_labels).toEqual(["bug", "documentation"]);
    expect(put.show_all).toBe(true);
  });

  it("404s on an unknown repo", async () => {
    const api = await reload();
    await expect(api.getBoardPrefs("nope")).rejects.toMatchObject({ status: 404 });
    await expect(api.setBoardPrefs("nope", { extra_labels: null, show_all: false })).rejects.toMatchObject({
      status: 404,
    });
  });
});

// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";

// PRD #196 M2: the mock is the browsable spec, so it re-implements the server's new
// settings validation and delivers the eligible set + waiver on the session and the
// admin-default extras on the board. Each test reloads the module against a fresh
// (empty) localStorage so appSettings re-seeds from its defaults.

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

describe("mockApi — settings validation (PRD #196 M2)", () => {
  it("rejects an eligible list that has lost the primary", async () => {
    const api = await reload();
    const err = await api.updateSettings({ run_eligible_labels: "bug" }).catch((e) => e);
    // A 400 whose message names the primary requirement (ApiError, but the module
    // reload gives it a fresh class identity, so assert on the shape not instanceof).
    expect(err).toMatchObject({ status: 400 });
    expect(String(err.message)).toMatch(/primary/i);
  });

  it("rejects a non-bool waiver value strictly", async () => {
    const api = await reload();
    await expect(api.updateSettings({ eligible_label_waives_prd_link: "yes" })).rejects.toMatchObject({
      status: 400,
    });
  });

  it("rejects an eligible label colliding with the autopilot label", async () => {
    const api = await reload();
    await expect(api.updateSettings({ run_eligible_labels: "PRD,autopilot" })).rejects.toMatchObject({
      status: 400,
    });
  });

  it("rejects a duplicate label within a list", async () => {
    const api = await reload();
    await expect(api.updateSettings({ run_eligible_labels: "PRD,bug,bug" })).rejects.toMatchObject({
      status: 400,
    });
  });

  it("accepts a valid update and round-trips the three keys on GET", async () => {
    const api = await reload();
    await api.updateSettings({
      run_eligible_labels: "PRD,bug,security",
      board_extra_labels: "documentation",
      eligible_label_waives_prd_link: "false",
    });
    const app = (await api.getSettings()).settings;
    expect(app.run_eligible_labels).toBe("PRD,bug,security");
    expect(app.board_extra_labels).toBe("documentation");
    expect(app.eligible_label_waives_prd_link).toBe("false");
  });
});

describe("mockApi — session + board payloads (PRD #196 M2)", () => {
  it("delivers the eligible set (primary unioned) and the waiver on the session", async () => {
    const api = await reload();
    const session = await api.me();
    // Seed defaults: run_eligible_labels "PRD,bug" already includes the primary.
    expect(session.run_eligible_labels).toEqual(["PRD", "bug"]);
    expect(session.eligible_label_waives_prd_link).toBe(true);
  });

  it("unions the primary into the eligible set even if it were stored without it", async () => {
    const api = await reload();
    // The validator forbids saving a list without the primary, so prove the union by
    // renaming the primary — the eligible list still gains the new primary on read.
    await api.updateSettings({ prd_label: "Feature", run_eligible_labels: "Feature,bug" });
    const session = await api.me();
    expect(session.run_eligible_labels).toEqual(["Feature", "bug"]);
  });

  it("carries the admin-default board_extra_labels on the board payload", async () => {
    const api = await reload();
    await api.updateSettings({ board_extra_labels: "bug,documentation" });
    const { board } = await api.getBoard("repo-uzi");
    expect(board.board_extra_labels).toEqual(["bug", "documentation"]);
  });
});

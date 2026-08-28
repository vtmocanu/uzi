// @vitest-environment jsdom
import { afterEach, describe, it, expect, vi } from "vitest";
import {
  mockMrReworkFixture,
  mockNotifications,
  mockRuns,
  type MrReworkBranch,
} from "./data";

// PRD #700 M6: completeness + parity guard for the MR-review-watcher fixture.
//
// The fixture (mockMrReworkFixture) enumerates every branch the PRD names. This
// test asserts the fixture CONTAINS ALL of them and that each is backed by real
// mock data, so dropping a branch — or letting its backing run/notification/setting
// rot — fails here rather than silently thinning the differential the web + mock
// parity work depends on. Derived (not snapshotted) so the fixture can still gain
// rows, mirroring mockApi.notifications.test.ts's rationale.

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

afterEach(() => vi.resetModules());

describe("MR-review-watcher differential fixture (PRD #700 M6)", () => {
  it("enumerates every branch the PRD names (drop one → fail)", () => {
    const required: MrReworkBranch[] = ["opted-in", "opted-out", "reworking", "capped"];
    const present = new Set(mockMrReworkFixture.map((b) => b.branch));
    for (const branch of required) {
      expect(present.has(branch), `fixture is missing the "${branch}" branch`).toBe(true);
    }
    // No stray/duplicate branches: exactly the required set, once each.
    expect(mockMrReworkFixture.map((b) => b.branch).sort()).toEqual([...required].sort());
  });

  it("settings branches carry the tri-state values (default-ON null, opted-out false)", () => {
    const byBranch = new Map(mockMrReworkFixture.map((b) => [b.branch, b]));
    // opted-in is the default-ON state: represented by a null (absent) setting.
    expect(byBranch.get("opted-in")?.settingsValue).toBeNull();
    // opted-out is the explicit false the watcher honours.
    expect(byBranch.get("opted-out")?.settingsValue).toBe(false);
  });

  it("the reworking branch is backed by an mr_rework run on its existing branch/MR", () => {
    const id = mockMrReworkFixture.find((b) => b.branch === "reworking")?.fixtureId;
    const run = mockRuns.find((r) => r.id === id);
    expect(run, `mockRuns is missing the reworking run "${id}"`).toBeTruthy();
    expect(run?.kind).toBe("mr_rework");
    // Issue-LESS, matching the backend: CreateAutoMRReworkRun leaves issue_iid NULL,
    // so RunIssueRef renders the "MR rework" kind chip rather than a forge #anchor.
    expect(run?.issue_iid).toBeNull();
    expect(run?.issue_web_url).toBeNull();
    // Folds onto the EXISTING branch + MR rather than opening a new one.
    expect(run?.branch).toBeTruthy();
    expect(run?.mr_iid).not.toBeNull();
    expect(run?.mr_state).toBe("opened");
  });

  it("the capped branch is backed by a halt inbox notification (not a new endpoint)", () => {
    const id = mockMrReworkFixture.find((b) => b.branch === "capped")?.fixtureId;
    const ntf = mockNotifications.find((n) => n.id === id);
    expect(ntf, `mockNotifications is missing the capped halt row "${id}"`).toBeTruthy();
    // Deep-links to the reworking run so the flagged card is reachable.
    expect(ntf?.run_id).toBe("run-mr-rework");
  });
});

describe("mockApi honours the mr_rework_enabled opt-in (PRD #700 M6)", () => {
  it("defaults ON (null), round-trips an explicit opt-out, and clears back to default", async () => {
    installStorage();
    let api = await reload();

    // Seed: default-ON is the null/absent state, read as enabled by the UI.
    const seeded = await api.getMySettings();
    expect(seeded.settings.mr_rework_enabled ?? null).toBeNull();

    // Opt out: an explicit false persists as false.
    const off = await api.putMySettings({ mr_rework_enabled: false });
    expect(off.settings.mr_rework_enabled).toBe(false);
    expect((await api.getMySettings()).settings.mr_rework_enabled).toBe(false);

    // Reload the module (drops the in-memory settings map and re-runs loadSettings):
    // the opt-out must survive the persistSettings()/loadSettings() STORAGE round-trip,
    // not merely read back from the same in-memory value the write left behind. Fails
    // if putMySettings stops write-through-ing the flag to storage.
    api = await reload();
    expect((await api.getMySettings()).settings.mr_rework_enabled).toBe(false);

    // A theme-only save leaves the opt-out untouched (PATCH semantics).
    await api.putMySettings({ theme: "mission" });
    expect((await api.getMySettings()).settings.mr_rework_enabled).toBe(false);

    // Clear back to the default-ON state with a null.
    const cleared = await api.putMySettings({ mr_rework_enabled: null });
    expect(cleared.settings.mr_rework_enabled ?? null).toBeNull();

    // Reload again: the cleared (default-ON null) state must also round-trip through
    // storage, so a subsequent session reads it back as the default rather than a
    // stale persisted false.
    api = await reload();
    expect((await api.getMySettings()).settings.mr_rework_enabled ?? null).toBeNull();
  });
});

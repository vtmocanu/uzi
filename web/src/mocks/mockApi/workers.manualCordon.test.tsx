// TSX keeps this test outside the production-only *.ts glob in mockApi.parity.test.ts.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

vi.mock("./shared", async (importOriginal) => ({
  ...await importOriginal<typeof import("./shared")>(),
  delay: <T,>(value: T) => Promise.resolve(value),
  mockScenario: vi.fn(() => ""),
  requireSession: vi.fn(),
}));

async function fresh() {
  vi.resetModules();
  const { workersApi, workers } = await import("./workers");
  const { state } = await import("../store");
  const { mockScenario, requireSession } = await import("./shared");
  vi.mocked(mockScenario).mockReturnValue("");
  vi.mocked(requireSession).mockImplementation(() => {
    if (!state.session) throw new Error("expected test session");
    return state.session;
  });
  if (!state.session) throw new Error("expected seeded session");
  state.session = { ...state.session, id: "owner-id", display_name: " Ada\n\u202e🚀 " };
  return { api: workersApi, workers, state, scenario: vi.mocked(mockScenario) };
}

beforeEach(() => vi.clearAllMocks());
afterEach(() => vi.restoreAllMocks());

describe("mock manual cordons", () => {
  it("leaves an initially uncordoned worker unchanged on uncordon, and bounds actor snapshots by code points", async () => {
    const { api, workers, state } = await fresh();
    const source = workers[0];
    const before = { ...source };
    expect((await api.uncordonWorker(source.id)).worker).toEqual(before);
    state.session!.display_name = "🚀".repeat(201);
    const first = (await api.cordonWorker(source.id)).worker;
    expect(first.manual_cordoned_by_label).toBe("🚀".repeat(200));
    expect(first.manual_cordon_reason).toBeNull();
  });

  it.each(["external", "hosted"] as const)("persists %s first-transition snapshots and clears only the manual tuple", async (kind) => {
    const { api, workers, state } = await fresh();
    const source = workers.find((w) => w.kind === kind && !w.ephemeral && !w.draining_since)!;
    source.effective_cordoned = false;
    const first = (await api.cordonWorker(source.id, " \u001b[31mFix\u001b[0m\nthis\t\u202e now ")).worker;
    expect(first).toMatchObject({
      manual_cordoned_by: "owner-id", manual_cordoned_by_label: "Ada 🚀",
      manual_cordon_reason: "Fix this  now", effective_cordoned: true,
    });
    expect(Number.isFinite(Date.parse(first.manual_cordoned_at!))).toBe(true);
    state.session = { ...state.session!, id: "changed-actor", display_name: "Changed" };
    expect((await api.cordonWorker(source.id, "replacement")).worker).toEqual(first);
    expect((await api.listWorkers()).workers.find((w) => w.id === source.id)).toEqual(first);
    const rebound = (await api.setWorkerBindMode(source.id, "auto", null)).worker;
    expect(rebound.manual_cordoned_at).toBe(first.manual_cordoned_at);
    expect(rebound.manual_cordon_reason).toBe(first.manual_cordon_reason);
    const cleared = (await api.uncordonWorker(source.id)).worker;
    expect(cleared).toMatchObject({
      manual_cordoned_at: null, manual_cordoned_by: null, manual_cordoned_by_label: null,
      manual_cordon_reason: null, effective_cordoned: false,
    });
    expect((await api.uncordonWorker(source.id)).worker).toEqual(cleared);
    expect((await api.listWorkers()).workers.find((w) => w.id === source.id)).toEqual(cleared);
    const { manual_cordoned_at: _at, manual_cordoned_by: _by,
      manual_cordoned_by_label: _label, manual_cordon_reason: _reason,
      effective_cordoned: _effective, ...rest } = cleared;
    const original = { ...source };
    delete original.effective_cordoned;
    expect(rest).toEqual(original);
  });

  it.each([
    { draining: true, prior: false },
    { draining: false, prior: true },
    { draining: true, prior: true },
    { draining: false, prior: false },
  ])("restores independent automatic/effective evidence %j even after repeat cordons", async ({ draining, prior }) => {
    const { api, workers } = await fresh();
    const source = workers[0];
    source.draining_since = draining ? "2026-10-20T12:00:00Z" : null;
    source.effective_cordoned = prior;
    const first = (await api.cordonWorker(source.id)).worker;
    expect((await api.cordonWorker(source.id, "later")).worker).toEqual(first);
    const after = (await api.uncordonWorker(source.id)).worker;
    expect(after.draining_since).toBe(source.draining_since);
    expect(after.effective_cordoned).toBe(draining || prior);
    expect(after).not.toHaveProperty("priorEffective");
    expect(after).not.toHaveProperty("maintenance_fenced");
    expect(after).not.toHaveProperty("maintenance_phase");
  });

  it("validates raw code points before sanitation, including on an idempotent call", async () => {
    const { api, workers, state } = await fresh();
    const id = workers[0].id;
    state.session!.display_name = "\u202e\n\t";
    const first = (await api.cordonWorker(id, "🚀".repeat(500))).worker;
    expect(first.manual_cordon_reason).toBe("🚀".repeat(500));
    expect(first.manual_cordoned_by_label).toBe("owner-id");
    for (const reason of ["🚀".repeat(501), " ".repeat(501), "\u001b[31m".repeat(101), "a" + "\n".repeat(500)]) {
      await expect(api.cordonWorker(id, reason)).rejects.toMatchObject({
        status: 400, message: "reason must be at most 500 codepoints",
      });
    }
    expect((await api.listWorkers()).workers.find((w) => w.id === id)).toEqual(first);
    await api.uncordonWorker(id);
    expect((await api.cordonWorker(id, " \u001b]0;hidden\u0007\u202e\n\t ")).worker.manual_cordon_reason).toBeNull();
  });

  it("rejects ephemeral and unknown workers for both methods without mutating the fleet", async () => {
    const { api, workers } = await fresh();
    const ephemeral = workers.find((w) => w.ephemeral)!;
    const before = await api.listWorkers();
    for (const method of [api.cordonWorker, api.uncordonWorker]) {
      await expect(method(ephemeral.id)).rejects.toMatchObject({
        status: 409, message: "manual cordon is only supported for persistent workers",
      });
      await expect(method("unknown")).rejects.toMatchObject({ status: 404 });
    }
    expect(await api.listWorkers()).toEqual(before);
  });

  it("persists transitions across regenerated incident owner/admin lists and keeps other scenarios intact", async () => {
    const { api, scenario } = await fresh();
    scenario.mockReturnValue("health-incident");
    const before = (await api.listWorkers()).workers[0];
    const first = (await api.cordonWorker(before.id, "incident reason")).worker;
    const owner = (await api.listWorkers()).workers.find((w) => w.id === before.id)!;
    const admin = (await api.adminListWorkers()).workers.find((w) => w.id === before.id)!;
    expect(owner.manual_cordoned_at).toBe(first.manual_cordoned_at);
    expect(admin.manual_cordon_reason).toBe("incident reason");
    expect(admin).toHaveProperty("owner_email");
    await api.uncordonWorker(before.id);
    expect((await api.adminListWorkers()).workers.find((w) => w.id === before.id)?.manual_cordoned_at).toBeNull();
    scenario.mockReturnValue("health-confirmed-drain");
    expect((await api.adminListWorkers()).workers.some((w) => !!w.draining_since)).toBe(true);
  });
});

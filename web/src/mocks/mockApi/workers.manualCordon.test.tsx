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
  // Expected snapshots recorded from store.ManualCordonText at c3dd0c3d with Go 1.27.3.
  it.each([
    // Post-ST snapshots recorded from actual store.ManualCordonText at 48e42433, Go 1.27.3.
    ["osc-execute-tail", "a\u001b]\u1700b", "a\ufffdb"],
    ["dcs-execute-tail", "a\u001bPq\u1700b", "a\ufffdb"],
    ["osc-title-execute-tail", "a\u001b]title\u2700\u001b\\b", "a\ufffdb"],
    ["osc-a0-tail", "a\u001b]\u1720b", "ab"],
    ["dcs-a0-tail", "a\u001bPq\u1720b", "ab"],
    ["osc-two-execute-tails", "a\u001b]\u{1c000}b", "a\ufffd\ufffdb"],
    ["dcs-two-execute-tails", "a\u001bPq\u{1c000}b", "a\ufffd\ufffdb"],
    ["osc-repeated-st", "a\u001b]\u171cb", "a\ufffdb"],
    ["osc-csi-tail", "a\u001b]\u171bb", "a"],
    ["osc-dcs-tail", "a\u001b]\u1710b", "a"],
    ["osc-osc-tail", "a\u001b]\u171db", "a"],
    ["osc-sos-tail", "a\u001b]\u1718b", "a"],
    ["osc-inside-unicode", "a\u001b]\u1600\u001b\\b", "ab"],
    ["dcs-inside-unicode", "a\u001bPq\u1600\u001b\\b", "ab"],
    ["osc-execute-then-a0", "a\u001b]\u{1c020}b", "a\ufffdb"],
    ["ordinary", " a\nb\tc ", "a b c"],
    ["lf-csi", "a\u001b[3\n1mb", "a b"],
    ["tab-csi", "a\u001b[3\t1mb", "a b"],
    ["del-csi", "a\u001b[3\u007f1mb", "ab"],
    ["lf-esc", "a\u001b\n7b", "a b"],
    ["tab-legacy", "a\u001b(\t0b", "a b"],
    ["sgr", "a\u001b[31mb\u001b[0m", "ab"],
    ["string-]", "a\u001b]0;hidden\n\ttext\u001b\\b", "ab"],
    ["bel-]", "a\u001b]hidden\u0007b", "ab"],
    ["cancel-]", "a\u001b]hidden\u0018b", "ab"],
    ["unicode-]", "a\u001b]hé🚀\u001b\\b", "ab"],
    ["string-P", "a\u001bP0;hidden\n\ttext\u001b\\b", "ab"],
    ["bel-P", "a\u001bPhidden\u0007b", "a"],
    ["cancel-P", "a\u001bPhidden\u0018b", "ab"],
    ["unicode-P", "a\u001bPhé🚀\u001b\\b", "ab"],
    ["string-X", "a\u001bX0;hidden\n\ttext\u001b\\b", "ab"],
    ["bel-X", "a\u001bXhidden\u0007b", "a"],
    ["cancel-X", "a\u001bXhidden\u0018b", "ab"],
    ["unicode-X", "a\u001bXhé🚀\u001b\\b", "aé🚀b"],
    ["string-^", "a\u001b^0;hidden\n\ttext\u001b\\b", "ab"],
    ["bel-^", "a\u001b^hidden\u0007b", "a"],
    ["cancel-^", "a\u001b^hidden\u0018b", "ab"],
    ["unicode-^", "a\u001b^hé🚀\u001b\\b", "aé🚀b"],
    ["string-_", "a\u001b_0;hidden\n\ttext\u001b\\b", "ab"],
    ["bel-_", "a\u001b_hidden\u0007b", "a"],
    ["cancel-_", "a\u001b_hidden\u0018b", "ab"],
    ["unicode-_", "a\u001b_hé🚀\u001b\\b", "aé🚀b"],
    ["c1-csi", "a\u009b31mb", "a31mb"],
    ["c1-osc", "a\u009dtitle\u0007b", "atitleb"],
    ["c1-in-csi", "a\u001b[3\u009b32mb", "a32mb"],
    ["c1-in-osc", "a\u001b]title\u009b31mtextb", "a"],
    ["c1-st", "a\u001b]title\u009cb", "ab"],
    ["html", " <b>漢🚀\u2028x\u2029y\u202ez\ue000</b> ", "<b>漢🚀\u2028x\u2029yz\ue000</b>"],
    ["blank", "\u001b[31m \n\t\u202e", ""],
    ["cancel-csi", "a\u001b[3\n\u00181mb", "a 1mb"],
    ["restart", "a\u001b[3\u001b[31mb", "ab"],
    ["incomplete", "a\u001b[3\n", "a"],
    ["dcs-entry-lf", "a\u001bP\ntext\u001b\\b", "ab"],
    ["cap", " 🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀 ", "🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀", "🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀  🚀"],
  ])("matches server actor/reason snapshots for %s through list and no-op", async (_name, input, expected, actorExpected = expected) => {
    const { api, workers, state } = await fresh();
    const id = workers[0].id;
    state.session!.display_name = input;
    const first = (await api.cordonWorker(id, input)).worker;
    const listed = (await api.listWorkers()).workers.find((w) => w.id === id)!;
    state.session = { ...state.session!, id: "replacement-owner", display_name: "Replacement" };
    const repeated = (await api.cordonWorker(id, "replacement reason")).worker;
    const listedAgain = (await api.listWorkers()).workers.find((w) => w.id === id)!;
    // Individual bounded assertions retain useful failures without dumping the fleet.
    for (const snapshot of [first, listed, repeated, listedAgain]) {
      expect(snapshot.manual_cordon_reason).toBe(expected || null);
      expect(snapshot.manual_cordoned_by_label).toBe(actorExpected || "owner-id");
      expect(snapshot.manual_cordoned_by).toBe("owner-id");
      expect(snapshot.manual_cordoned_at).toBe(first.manual_cordoned_at);
    }
  });

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

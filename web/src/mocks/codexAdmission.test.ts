import { afterEach, expect, it, vi } from "vitest";

vi.mock("./mockApi/shared", async (importOriginal) => ({
  ...await importOriginal<typeof import("./mockApi/shared")>(),
  delay: <T>(value: T) => Promise.resolve(value),
}));
afterEach(() => vi.resetModules());

it.each(["rate_limit", "overload"] as const)("Codex admission %s survives owner detail/list/summary and board mirroring", async (cls) => {
  const { mockApi } = await import("./mockApi");
  const { state, patchRun } = await import("./store");
  const at = "2027-01-01T12:00:37Z";
  const fields = { status: "queued" as const, queue_reason: cls === "rate_limit" ? "codex_rate_limit" as const : "codex_overload" as const,
    codex_admission_class: cls, codex_admission_eligible_at: at };
  patchRun("run-queued", fields);
  expect((await mockApi.getRun("run-queued")).run).toMatchObject(fields);
  expect((await mockApi.listRuns()).runs.find(r => r.id === "run-queued")).toMatchObject(fields);
  expect((await mockApi.listRuns({ view: "summary" })).runs.find(r => r.id === "run-queued")).toMatchObject(fields);
  const card = [...state.boards.values()].flatMap(b => b.cards).find(c => c.latest_run?.id === "run-queued");
  expect(card).toBeDefined();
  expect(card!.latest_run).toMatchObject(fields);
  card!.latest_run!.is_mine = false;
  patchRun("run-queued", { ...fields, health_reason: "private account detail" });
  expect(card!.latest_run!.codex_admission_class).toBeUndefined();
  expect(card!.latest_run!.codex_admission_eligible_at).toBeUndefined();
  expect(card!.latest_run!.queue_reason).toBeUndefined();
  expect(card!.latest_run!.health_reason).toBeNull();
});

it("Codex admission admin summaries redact foreign account details while retaining maintenance", async () => {
  const { mockApi } = await import("./mockApi");
  const { patchRun } = await import("./store");
  patchRun("run-andrei-queued", { status: "queued", queue_reason: "maintenance", codex_admission_class: "overload",
    codex_admission_eligible_at: "2027-01-01T12:00:37Z", health_reason: "private account detail" });
  for (const rows of [(await mockApi.adminListRuns()).runs, (await mockApi.adminListRuns({ view: "summary" })).runs]) {
    const row = rows.find(r => r.id === "run-andrei-queued")!;
    expect(row).toBeDefined();
    expect(row.queue_reason).toBe("maintenance");
    expect(row.codex_admission_class).toBeUndefined();
    expect(row.codex_admission_eligible_at).toBeUndefined();
    expect(row.health_reason).toBeNull();
  }
});

import { afterEach, expect, it, vi } from "vitest";
vi.mock("./mockApi/shared", async original => ({ ...await original<typeof import("./mockApi/shared")>(), delay: <T>(value: T) => Promise.resolve(value) }));
afterEach(() => { vi.resetModules(); vi.unstubAllGlobals(); });

it.each(["rate_limit", "overload"] as const)("active %s fields survive owner projections and redact foreign projections", async cls => {
  const { mockApi } = await import("./mockApi");
  const { state, patchRun } = await import("./store");
  const fields = { status: "claimed" as const, health: "waiting_account_cooldown" as const, account_cooldown_class: cls, account_cooldown_eligible_at: "2027-01-01T12:00:37Z" };
  patchRun("run-queued", fields);
  expect((await mockApi.getRun("run-queued")).run).toMatchObject(fields);
  for (const rows of [(await mockApi.listRuns()).runs, (await mockApi.listRuns({ view: "summary" })).runs]) {
    expect(rows.find(r => r.id === "run-queued")).toMatchObject(fields);
    expect(rows.find(r => r.id === "run-queued")?.progress?.state).toBe("waiting");
  }
  const card = [...state.boards.values()].flatMap(b => b.cards).find(c => c.latest_run?.id === "run-queued")!;
  expect(card.latest_run).toMatchObject(fields);
  card.latest_run!.is_mine = false;
  patchRun("run-queued", fields);
  expect(card.latest_run!.account_cooldown_class).toBeUndefined();
  expect(card.latest_run!.account_cooldown_eligible_at).toBeUndefined();
  patchRun("run-andrei-queued", fields);
  const foreign = (await mockApi.getRun("run-andrei-queued")).run;
  expect(foreign.account_cooldown_class).toBeUndefined();
  expect(foreign.account_cooldown_eligible_at).toBeUndefined();
  for (const rows of [(await mockApi.adminListRuns()).runs, (await mockApi.adminListRuns({ view: "summary" })).runs]) {
    const row = rows.find(r => r.id === "run-andrei-queued")!;
    expect(row.health).toBe("waiting_account_cooldown");
    expect(row.account_cooldown_class).toBeUndefined();
    expect(row.account_cooldown_eligible_at).toBeUndefined();
  }
});

it("demo scenario includes an owned active wait, foreign expected health, and near-wall precedence", async () => {
  vi.stubGlobal("window", { location: { search: "?mock=account-cooldown" }, localStorage: { getItem: () => null } });
  const { state } = await import("./store");
  expect(state.runs.get("run-live")).toMatchObject({ health: "waiting_account_cooldown", account_cooldown_class: "rate_limit", progress: { state: "waiting", pct: null } });
  expect(state.runs.get("run-mira-embed")).toMatchObject({ health: "waiting_account_cooldown" });
  expect(state.runs.get("run-stalled")).toMatchObject({ health: "slow", account_cooldown_class: "overload" });
});

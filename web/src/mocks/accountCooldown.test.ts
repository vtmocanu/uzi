import { afterEach, expect, it, vi } from "vitest";
vi.mock("./mockApi/shared", async original => ({ ...await original<typeof import("./mockApi/shared")>(), delay: <T>(value: T) => Promise.resolve(value) }));
vi.mock("./engine", async original => ({ ...await original<typeof import("./engine")>(), ensureLive: vi.fn() }));
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
  expect(card.latest_run!.health).toBe("ok");
  expect(card.latest_run!.account_cooldown_class).toBeUndefined();
  expect(card.latest_run!.account_cooldown_eligible_at).toBeUndefined();
  patchRun("run-andrei-queued", fields);
  const foreign = (await mockApi.getRun("run-andrei-queued")).run;
  expect(foreign.health).toBe("ok");
  expect(foreign.health_reason).toBeNull();
  expect(foreign.progress?.state).toBe("queued");
  expect(state.runs.get("run-andrei-queued")?.health).toBe("waiting_account_cooldown");
  expect(foreign.account_cooldown_class).toBeUndefined();
  expect(foreign.account_cooldown_eligible_at).toBeUndefined();
  for (const rows of [(await mockApi.adminListRuns()).runs, (await mockApi.adminListRuns({ view: "summary" })).runs]) {
    const row = rows.find(r => r.id === "run-andrei-queued")!;
    expect(row.health).toBe("ok");
    expect(row.progress?.state).toBe("queued");
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

it("foreign running projections derive progress after stripping cooldown and preserve other faults", async () => {
  const { mockApi } = await import("./mockApi");
  const { patchRun } = await import("./store");
  patchRun("run-andrei-queued", { status: "running", health: "waiting_account_cooldown",
    is_planning: true, health_reason: "waiting for account cooldown",
    account_cooldown_class: "overload", account_cooldown_eligible_at: "2099-01-01T00:00:37Z" });
  const detail = (await mockApi.getRun("run-andrei-queued")).run;
  expect(detail).toMatchObject({ health: "ok", health_reason: null, progress: { state: "planning", pct: null } });
  for (const rows of [(await mockApi.adminListRuns()).runs, (await mockApi.adminListRuns({ view: "summary" })).runs]) {
    expect(rows.find(r => r.id === detail.id)).toMatchObject({ health: "ok", health_reason: null, progress: { state: "planning", pct: null } });
  }
  patchRun(detail.id, { health: "slow", health_reason: "near timeout" });
  expect((await mockApi.getRun(detail.id)).run).toMatchObject({ health: "slow", health_reason: null });
});

it("cooldown scenario seeds and refreshes truthful Codex cards without changing ordinary Claude fixtures", async () => {
  vi.stubGlobal("window", { location: { search: "?mock=account-cooldown" }, localStorage: { getItem: () => null } });
  const { state, patchRun } = await import("./store");
  const { mockApi } = await import("./mockApi");
  const liveCard = [...state.boards.values()].flatMap(b => b.cards).find(c => c.latest_run?.id === "run-live")!;
  expect(liveCard.latest_run).toMatchObject({ health: "waiting_account_cooldown", account_cooldown_class: "rate_limit" });
  for (const id of ["run-live", "run-mira-embed", "run-stalled"]) {
    expect(state.runs.get(id)).toMatchObject({ harness: "codex", model: null });
    expect((await mockApi.getRun(id)).run.harness).toBe("codex");
    const cards = [...state.boards.values()].flatMap(b => b.cards).filter(c => c.latest_run?.id === id);
    for (const card of cards) {
      if (!card.latest_run!.is_mine) {
        expect(card.latest_run).toMatchObject({ health: "ok", health_reason: null });
        expect(card.latest_run!.account_cooldown_class).toBeUndefined();
      }
    }
    patchRun(id, { health: "waiting_account_cooldown" });
    for (const card of cards) {
      expect(card.latest_run!.health).toBe(card.latest_run!.is_mine ? "waiting_account_cooldown" : "ok");
    }
  }
  liveCard.latest_run!.is_mine = false;
  patchRun("run-live", { health: "waiting_account_cooldown" });
  expect(liveCard.latest_run).toMatchObject({ health: "ok", health_reason: null });
  expect(liveCard.latest_run!.account_cooldown_class).toBeUndefined();
  expect(state.runs.get("run-live")!.health).toBe("waiting_account_cooldown");
  vi.resetModules();
  vi.stubGlobal("window", { location: { search: "" }, localStorage: { getItem: () => null } });
  const normal = await import("./store");
  expect(normal.state.runs.get("run-live")!.harness).toBe("claude");
});

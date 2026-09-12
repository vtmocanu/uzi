// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { handleInput } from "./engine";
import { patchRun, state } from "./store";

// PRD #1189: the mock engine's `extend` handler is a CONTRACT (see engineQuestion.test.ts) —
// it must mirror the real api's rejections and apply the extension in place, so a web surface
// built against the mock is not laxer than the wire.

const RUN_ID = "run-live"; // a running `issue` kind with an 8h budget and a 16h allowance.
const DEADLINE = "2026-01-01T20:00:00Z";

beforeEach(() => {
  vi.useFakeTimers();
  state.messages.set(RUN_ID, []);
  patchRun(RUN_ID, {
    status: "running",
    health: "ok",
    health_reason: null,
    health_since: null,
    budget_wall_seconds: 28800,
    budget_total_seconds: 28800,
    budget_extension_seconds: 0,
    budget_extension_cap_seconds: 57600,
    deadline_at: DEADLINE,
  });
});

afterEach(() => {
  vi.clearAllTimers();
  vi.useRealTimers();
});

describe("mock engine: extend (PRD #1189)", () => {
  it("applies the extension in place — grows the column, total and deadline by the granted time", () => {
    expect(handleInput(RUN_ID, "extend", "7200")).toBeNull();
    const run = state.runs.get(RUN_ID)!;
    expect(run.budget_extension_seconds).toBe(7200);
    expect(run.budget_total_seconds).toBe(28800 + 7200);
    expect(Date.parse(run.deadline_at!)).toBe(Date.parse(DEADLINE) + 7200 * 1000);
  });

  it("clears a near-timeout ('slow') flag, because extending moves the near-timeout line", () => {
    patchRun(RUN_ID, { health: "slow", health_reason: "near timeout", health_since: DEADLINE });
    expect(handleInput(RUN_ID, "extend", "3600")).toBeNull();
    const run = state.runs.get(RUN_ID)!;
    expect(run.health).toBe("ok");
    expect(run.health_reason).toBeNull();
    expect(run.health_since).toBeNull();
  });

  it("refuses a body under 60 seconds with 400", () => {
    expect(handleInput(RUN_ID, "extend", "30")).toMatchObject({ status: 400 });
    expect(state.runs.get(RUN_ID)!.budget_extension_seconds).toBe(0);
  });

  it("refuses an over-cap total with 409 naming the remaining allowance", () => {
    patchRun(RUN_ID, { budget_extension_seconds: 54000 }); // 15h of the 16h cap already granted
    const rej = handleInput(RUN_ID, "extend", "7200"); // +2h would exceed the remaining 1h
    expect(rej).toMatchObject({ status: 409 });
    expect(rej!.message).toMatch(/left of 57600s/);
    expect(state.runs.get(RUN_ID)!.budget_extension_seconds).toBe(54000);
  });

  it("refuses a kind that never times out (a judge run) with 409", () => {
    // run-judge-meta is a judge kind; extending it must 409 rather than silently no-op.
    const rej = handleInput("run-judge-meta", "extend", "3600");
    expect(rej).toMatchObject({ status: 409 });
  });

  it("extends a queued (parked, not-yet-claimed) run — the run-queued fixture seeds its cap (issue #1259)", () => {
    // A queued issue run is a non-terminal timed kind, so extend must succeed (the column is inert
    // until it runs). This relies on the run-queued FIXTURE carrying budget_extension_cap_seconds:
    // before #1259 the fixture omitted the extension fields, so the mock read the cap as 0 and
    // returned a spurious "extensions are turned off" 409. Deliberately does NOT patch the fields
    // here — the fixture's seeding is what is under test.
    const before = state.runs.get("run-queued")!.budget_extension_seconds ?? 0;
    expect(handleInput("run-queued", "extend", "3600")).toBeNull();
    expect(state.runs.get("run-queued")!.budget_extension_seconds).toBe(before + 3600);
  });
});

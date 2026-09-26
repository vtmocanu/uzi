// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { handleInput } from "./engine";
import { mockApi } from "./mockApi";
import { patchRun, state } from "./store";

// Issue #1727: the mock stamps runs.status_since the way the server does. It moves ONLY on a
// real status transition (patchRun's central stamp), never on an unrelated write such as an
// extend, which still advances updated_at. A demo that let status_since follow updated_at
// would teach exactly the drift the field exists to remove.

const RUN_ID = "run-live"; // a pausable `issue` kind with an 8h budget and a 16h allowance.
const T0 = Date.parse("2026-03-01T10:00:00Z");

/** Runs every timer the engine scheduled, including ones scheduled BY a timer. */
function drain() {
  for (let i = 0; i < 40; i++) vi.advanceTimersByTime(5_000);
}

beforeEach(() => {
  vi.useFakeTimers();
  vi.setSystemTime(T0);
  state.messages.set(RUN_ID, []);
  patchRun(RUN_ID, {
    status: "awaiting_approval",
    pause_requested_at: null,
    pause_mode: null,
    pause_after_count: null,
    budget_wall_seconds: 28800,
    budget_extension_seconds: 0,
    budget_extension_cap_seconds: 57600,
  });
});

afterEach(() => {
  vi.clearAllTimers();
  vi.useRealTimers();
});

describe("mock status_since (issue #1727)", () => {
  it("a pause stamps status_since, an extend keeps it, a resume advances it", async () => {
    handleInput(RUN_ID, "approve_plan", "");
    expect(handleInput(RUN_ID, "pause", "now")).toBeNull();
    drain();
    const paused = state.runs.get(RUN_ID)!;
    expect(paused.status).toBe("paused");
    const pausedSince = paused.status_since;
    expect(typeof pausedSince).toBe("string");
    // The park is the transition, so status_since is the instant of that write.
    expect(pausedSince).toBe(paused.updated_at);

    // An extend while paused is an unrelated write: updated_at moves, status_since does not.
    vi.advanceTimersByTime(10 * 60_000);
    expect(handleInput(RUN_ID, "extend", "3600")).toBeNull();
    const extended = state.runs.get(RUN_ID)!;
    expect(extended.status).toBe("paused");
    expect(extended.budget_extension_seconds).toBe(3600);
    expect(Date.parse(extended.updated_at)).toBeGreaterThan(Date.parse(pausedSince!));
    expect(extended.status_since).toBe(pausedSince);

    // A later status transition (resume: paused → queued) advances status_since.
    vi.advanceTimersByTime(5 * 60_000);
    const pending = mockApi.resumeRun(RUN_ID);
    await vi.advanceTimersByTimeAsync(200);
    const { run: resumed } = await pending;
    expect(resumed.status).toBe("queued");
    expect(Date.parse(resumed.status_since!)).toBeGreaterThan(Date.parse(pausedSince!));
    expect(resumed.status_since).toBe(state.runs.get(RUN_ID)!.updated_at);
  });

  it("a patch that re-sets the SAME status does not move status_since", () => {
    const before = state.runs.get(RUN_ID)!.status_since;
    vi.advanceTimersByTime(60_000);
    patchRun(RUN_ID, { status: "awaiting_approval" });
    const after = state.runs.get(RUN_ID)!;
    expect(after.status_since).toBe(before);
    expect(Date.parse(after.updated_at)).toBeGreaterThan(Date.parse(before!));
  });

  it("an explicit status_since in the patch wins over the central stamp", () => {
    const explicit = "2026-02-01T00:00:00Z";
    patchRun(RUN_ID, { status: "running", status_since: explicit });
    expect(state.runs.get(RUN_ID)!.status_since).toBe(explicit);
  });

  it("seeds every fixture with status_since, defaulting to its updated_at", () => {
    for (const run of state.runs.values()) {
      expect(typeof run.status_since).toBe("string");
    }
    // The paused demo run keeps its explicit, earlier status_since (an unrelated write after
    // the pause moved updated_at), so mock mode shows the pause instant, not updated_at.
    const demo = state.runs.get("run-paused")!;
    expect(Date.parse(demo.status_since!)).toBeLessThan(Date.parse(demo.updated_at));
  });

  it("stamps status_since at creation, the same instant as created_at", async () => {
    // continueChat copies its source run, so an unstamped copy would inherit the source's
    // status_since; the new run must carry its own.
    const src = [...state.runs.values()].find((r) => r.kind === "chat")!;
    const pending = mockApi.continueChat(src.id);
    await vi.advanceTimersByTimeAsync(400);
    const { run } = await pending;
    expect(run.status_since).toBe(run.created_at);
    expect(run.status_since).not.toBe(src.status_since);
  });
});

// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { handleInput } from "./engine";
import { listMessages, patchRun, state } from "./store";

// PRD #1190 (finding [4]): the mock engine is a CONTRACT (see engineQuestion.test.ts).
// A cancelled pause must leave the run EXECUTING — the interrupted script continues.
// The bug this guards: `pause` cleared the run's single execution-timer array and
// `pause_cancel` cleared it again, so a cancelled pause wiped the script and left the
// mock run permanently `running` with no further activity. The park (pause not cancelled)
// must still stop the run.

const RUN_ID = "run-live";

/** Runs every timer the engine scheduled, including ones scheduled BY a timer. */
function drain() {
  for (let i = 0; i < 40; i++) vi.advanceTimersByTime(5_000);
}

beforeEach(() => {
  vi.useFakeTimers();
  // A clean log + a gated run, so each test drives the same starting point regardless of
  // which script a previous test left mid-flight. run-live is a pausable `issue` kind.
  state.messages.set(RUN_ID, []);
  patchRun(RUN_ID, {
    status: "awaiting_approval",
    pause_requested_at: null,
    pause_mode: null,
    pause_after_count: null,
  });
});

afterEach(() => {
  vi.clearAllTimers();
  vi.useRealTimers();
});

describe("mock engine: pause / pause_cancel timer separation (PRD #1190, finding [4])", () => {
  it("a pause CANCELLED before the park fires leaves the run running its script", () => {
    // approve_plan resumes the run and schedules the post-approval clarification script
    // (askScript) on the execution timers — this is the "interrupted script".
    expect(handleInput(RUN_ID, "approve_plan", "")).toBeNull();
    expect(state.runs.get(RUN_ID)!.status).toBe("running");

    // Request a pause (milestone mode parks at 2500ms) — a FLAG, not a stop: the run stays
    // running and only the pause columns are set.
    expect(handleInput(RUN_ID, "pause", "milestone")).toBeNull();
    expect(state.runs.get(RUN_ID)!.status).toBe("running");
    expect(state.runs.get(RUN_ID)!.pause_requested_at).not.toBeNull();

    // Withdraw the pause BEFORE the park timer fires (no time has been advanced yet).
    expect(handleInput(RUN_ID, "pause_cancel", "")).toBeNull();
    expect(state.runs.get(RUN_ID)!.pause_requested_at).toBeNull();

    const before = listMessages(RUN_ID).length;

    // Advance well past both the (cancelled) park window and the script's own delays.
    drain();

    const after = listMessages(RUN_ID).length;
    const run = state.runs.get(RUN_ID)!;
    // The interrupted script CONTINUED — it appended more frames (with the bug it wiped
    // itself on cancel and appended nothing here) …
    expect(after).toBeGreaterThan(before);
    // … and the cancelled pause never parked the run.
    expect(run.status).not.toBe("paused");
    expect(run.pause_requested_at).toBeNull();
  });

  it("a pause left in place still parks the run (the park path is untouched)", () => {
    handleInput(RUN_ID, "approve_plan", "");
    expect(state.runs.get(RUN_ID)!.status).toBe("running");
    handleInput(RUN_ID, "pause", "now");
    // Let the park timer fire.
    drain();
    const run = state.runs.get(RUN_ID)!;
    expect(run.status).toBe("paused");
    // parkPaused clears the request columns and stamps the checkpoint on the wire.
    expect(run.pause_requested_at).toBeNull();
    expect(run.pause_mode).toBeNull();
    expect(typeof run.checkpoint_tip_at).toBe("string");
  });

  // PRD #1190 (D6 / ADR I4): a pending milestone pause SURVIVES an involuntary park and
  // re-arms. The demo's post-approval clarification question flips the run to awaiting_input
  // at ~2100ms — BEFORE the milestone boundary fires at 2500ms — so the park is overtaken.
  // The pause must NOT be silently dropped: it re-arms and lands once the answer returns the
  // run to `running`. (Regression: before the re-arm the one-shot park no-op'd on the
  // non-running status and pause_requested_at stayed set forever.)
  it("a milestone pause overtaken by a clarification question still lands after the answer", () => {
    // approve_plan resumes the run and schedules askScript (the clarification question fires at
    // ~2100ms, flipping the run to awaiting_input).
    handleInput(RUN_ID, "approve_plan", "");
    expect(state.runs.get(RUN_ID)!.status).toBe("running");

    // Request a milestone pause (boundary park at 2500ms) — a FLAG, not a stop.
    expect(handleInput(RUN_ID, "pause", "milestone")).toBeNull();
    expect(state.runs.get(RUN_ID)!.pause_requested_at).not.toBeNull();

    // Advance PAST the question (~2100ms) but not past the park (2500ms): the involuntary
    // park overtakes the pending pause — status becomes awaiting_input.
    vi.advanceTimersByTime(2200);
    expect(state.runs.get(RUN_ID)!.status).toBe("awaiting_input");
    expect(state.runs.get(RUN_ID)!.pause_requested_at).not.toBeNull();

    // Advance past the park window. The boundary timer fires while the run is awaiting_input,
    // so the pending pause must SURVIVE and re-arm rather than no-op away — the run is still
    // parked on the question and the pause request is still pending.
    vi.advanceTimersByTime(2000);
    const overtaken = state.runs.get(RUN_ID)!;
    expect(overtaken.status).toBe("awaiting_input");
    expect(overtaken.status).not.toBe("paused");
    expect(overtaken.pause_requested_at).not.toBeNull();

    // Answer the question → the run returns to `running` (schedules askAgainScript). The
    // survived pause now has a `running` boundary to land on.
    const answer = JSON.stringify({ question_id: "q-mock-0001", answers: ["Postgres table"] });
    expect(handleInput(RUN_ID, "answer", answer)).toBeNull();
    expect(state.runs.get(RUN_ID)!.status).toBe("running");

    // Let the re-armed park land.
    drain();

    const run = state.runs.get(RUN_ID)!;
    // The milestone pause EVENTUALLY landed after the involuntary park — not silently dropped.
    expect(run.status).toBe("paused");
    // …with the pause columns cleared and the checkpoint stamped on the park.
    expect(run.pause_requested_at).toBeNull();
    expect(run.pause_mode).toBeNull();
    expect(typeof run.checkpoint_tip_at).toBe("string");
  });
});

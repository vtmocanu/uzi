// @vitest-environment jsdom
import { afterEach, expect, it, vi } from "vitest";
import { mockApi } from "./mockApi";
import { handleInput } from "./engine";
import { getRun, patchRun } from "./store";
import type { RunInputKind } from "../lib/api";

const ids = ["run-iteration-hold", "run-iteration-spent"];
const originals = ids.map((id) => ({ ...getRun(id)! }));
afterEach(() => {
  originals.forEach((run) => patchRun(run.id, run));
  vi.clearAllTimers();
  vi.useRealTimers();
});

it("iteration mock Resume refuses spent wall before mutation; Extend leaves it paused until explicit Resume", async () => {
  vi.useFakeTimers();
  const id = "run-iteration-spent";
  const before = { ...getRun(id)! };
  await expect(mockApi.resumeRun(id)).rejects.toMatchObject({ status: 409, message: expect.stringContaining("out of time; extend it to resume") });
  expect(getRun(id)).toEqual(before);
  expect(handleInput(id, "extend", "3600")).toBeNull();
  expect(getRun(id)).toMatchObject({ status: "paused", hold_reason: "iteration_budget_exhausted",
    budget_total_seconds: 7200, budget_used_seconds: 3600, budget_extension_seconds: 3600 });
  const resume = mockApi.resumeRun(id);
  await vi.advanceTimersByTimeAsync(100);
  await expect(resume).resolves.toMatchObject({ run: { status: "queued", hold_reason: null, hold_context: null,
    worker_id: before.worker_id, iteration_count: before.iteration_count } });
});

it("iteration mock Resume accepts positive wall without Extend", async () => {
  vi.useFakeTimers();
  const pending = mockApi.resumeRun("run-iteration-hold");
  await vi.advanceTimersByTimeAsync(100);
  await expect(pending).resolves.toMatchObject({ run: { status: "queued", hold_reason: null, budget_extension_seconds: 0 } });
});

it("explicit set-token-and-resume continues an iteration hold with remaining wall", async () => {
  vi.useFakeTimers();
  const before = { ...getRun("run-iteration-hold")! };
  const pending = mockApi.setRunCredential("run-iteration-hold", { mode: "auto" });
  await vi.advanceTimersByTimeAsync(200);
  await expect(pending).resolves.toMatchObject({ run: { status: "queued", hold_reason: null, hold_context: null,
    worker_id: before.worker_id, iteration_count: before.iteration_count, budget_extension_seconds: before.budget_extension_seconds } });
});

it("explicit set-token-and-resume refuses spent wall and retains the iteration hold", async () => {
  await expect(mockApi.setRunCredential("run-iteration-spent", { mode: "auto" })).rejects.toMatchObject({
    status: 409, message: "credential switch: run state changed, retry",
  });
  expect(getRun("run-iteration-spent")).toMatchObject({ status: "paused", hold_reason: "iteration_budget_exhausted",
    credential_override: { mode: "auto" } });
});

it.each([
  ["issue", []],
  ["issue", ["one"]],
  ["task", []],
] as const)("iteration mock refuses direct Stop for %s %j without mutation", async (kind, completed) => {
  const id = "run-iteration-hold";
  patchRun(id, { kind, milestones: [{ id: "one", title: "One" }], milestones_completed: [...completed] });
  const before = { ...getRun(id)! };
  // Stop is accepted by the server's broader input endpoint, though web only sends Cancel.
  await expect(mockApi.submitRunInput(id, "stop" as RunInputKind)).rejects.toMatchObject({
    status: 409, message: "Stop is unavailable for an iteration hold; Resume to keep working, or Cancel the run",
  });
  expect(getRun(id)).toEqual(before);
});

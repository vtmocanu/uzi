// @vitest-environment jsdom
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { mockApi } from "./mockApi";
import { getRun, patchRun } from "./store";
import { mockRuns } from "./data/runs";
import { MockRunSocket } from "./socket";

beforeEach(() => {
  vi.useFakeTimers();
  for (const run of mockRuns.filter((r) => r.id.startsWith("run-memory-hold"))) patchRun(run.id, { ...run });
});
afterEach(() => {
  vi.clearAllTimers();
  vi.useRealTimers();
  window.localStorage.removeItem("uzi_mock_scenario");
});
async function settle<T>(request: Promise<T>): Promise<T> {
  await vi.advanceTimersByTimeAsync(1000);
  return request;
}
it("reads known and unknown memory allowances independently of requeues", async () => {
  expect((await settle(mockApi.getRun("run-memory-hold"))).run.memory_hold)
    .toEqual({ episode: 4, used: 2, max: 2, remaining: 0 });
  expect((await settle(mockApi.getRun("run-memory-hold-unknown"))).run.memory_hold)
    .toEqual({ episode: 4, used: 2, max: null, remaining: null });
});
it("source-only fixture has no currently available archive and does not resume on socket or messages", async () => {
  const id = "run-memory-hold-source-only";
  const socket = new MockRunSocket(id);
  try {
    await vi.advanceTimersByTimeAsync(60_000);
    await settle(mockApi.submitRunInput(id, "follow_up", "continue"));
    const { run } = await settle(mockApi.getRun(id));
    expect(run.status).toBe("recovery_wait");
    expect(run.worker_recovery?.evidence).toMatchObject({
      checkpoint_tip: null, available_capture: false, custody_uncertain: true,
    });
    const archives = await settle(mockApi.getRunArchives(id));
    expect(archives.counts.available).toBe(0);
    expect(archives.archives).toEqual([]);
  } finally { socket.close(); }
});
it("only explicit Resume releases the projection and preserves requeue accounting", async () => {
  const { run } = await settle(mockApi.resumeRun("run-memory-hold"));
  expect(run.status).toBe("queued");
  expect(run.recovery_wait_cause).toBeNull();
  expect(run.memory_hold).toBeUndefined();
  expect(run.requeue_count).toBe(9);
  expect(run.worker_recovery?.episode_used).toBe(1);
});
it("Cancel ends the hold and clears held-only allowance", async () => {
  await settle(mockApi.submitRunInput("run-memory-hold", "cancel"));
  const run = getRun("run-memory-hold")!;
  expect(run.status).toBe("failed");
  expect(run.stop_kind).toBe("cancelled");
  expect(run.memory_hold).toBeUndefined();
});
it("nonowner scenario refuses owner proof, Resume and Cancel while allowing detail", async () => {
  window.localStorage.setItem("uzi_mock_scenario", "memory-hold-nonowner");
  expect((await settle(mockApi.getRun("run-memory-hold"))).run.status).toBe("recovery_wait");
  await expect(mockApi.getRunInputs("run-memory-hold")).rejects.toMatchObject({ status: 404 });
  await expect(mockApi.resumeRun("run-memory-hold")).rejects.toMatchObject({ status: 404 });
  await expect(mockApi.submitRunInput("run-memory-hold", "cancel")).rejects.toMatchObject({ status: 404 });
  expect(getRun("run-memory-hold")?.status).toBe("recovery_wait");
});

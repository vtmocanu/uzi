// @vitest-environment jsdom
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { mockApi } from "./mockApi";
import { getRun, patchRun } from "./store";
import { mockRuns } from "./data/runs";
import { MockRunSocket } from "./socket";

beforeEach(() => {
  vi.useFakeTimers();
  for (const run of mockRuns.filter((r) => r.id.startsWith("run-forge-"))) patchRun(run.id, { ...run });
});
afterEach(() => {
  vi.clearAllTimers(); vi.useRealTimers();
  window.localStorage.removeItem("uzi_mock_scenario");
});
async function settle<T>(request: Promise<T>): Promise<T> {
  await vi.advanceTimersByTimeAsync(1000);
  return request;
}
it("keeps NULL, timed, overdue and omitted advice in full and summary reads", async () => {
  const { runs } = await settle(mockApi.listRuns({ view: "summary" }));
  for (const id of ["run-forge-hold", "run-forge-timed", "run-forge-overdue", "run-forge-unknown"]) {
    const { run } = await settle(mockApi.getRun(id));
    const summary = runs.find((r) => r.id === id)!;
    expect(summary).toBeTruthy();
    expect(summary.recovery_retry_not_before).toBe(run.recovery_retry_not_before);
    expect(summary.forge_origin_cooldown).toEqual(run.forge_origin_cooldown);
  }
  expect(getRun("run-forge-unknown")?.recovery_retry_not_before).toBeUndefined();
});
it("keeps a hold through socket activity and resumes one ordinary attempt without resetting counts or advice", async () => {
  const id = "run-forge-hold";
  const before = { ...getRun(id)! };
  const socket = new MockRunSocket(id);
  try {
    await vi.advanceTimersByTimeAsync(60_000);
    expect(getRun(id)?.status).toBe("recovery_wait");
    const { run } = await settle(mockApi.resumeRun(id));
    expect(run.status).toBe("queued");
    expect(run.forge_park_count).toBe(before.forge_park_count);
    expect(run.forge_origin_cooldown).toEqual(before.forge_origin_cooldown);
    expect(run.requeue_count).toBe(before.requeue_count);
  } finally { socket.close(); }
});
it("does not fabricate a recovery archive and supports explicit Cancel", async () => {
  const archives = await settle(mockApi.getRunArchives("run-forge-hold"));
  expect(archives.counts.available).toBe(0);
  expect(archives.archives).toEqual([]);
  await settle(mockApi.submitRunInput("run-forge-hold", "cancel"));
  expect(getRun("run-forge-hold")?.stop_kind).toBe("cancelled");
});
it("refuses owner actions on nonowner fixtures while retaining read access", async () => {
  window.localStorage.setItem("uzi_mock_scenario", "forge-hold-nonowner");
  expect((await settle(mockApi.getRun("run-forge-hold"))).run.status).toBe("recovery_wait");
  await expect(mockApi.getRunInputs("run-forge-hold")).rejects.toMatchObject({ status: 404 });
  await expect(mockApi.resumeRun("run-forge-hold")).rejects.toMatchObject({ status: 404 });
  await expect(mockApi.submitRunInput("run-forge-hold", "cancel")).rejects.toMatchObject({ status: 404 });
});

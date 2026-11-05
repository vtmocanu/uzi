// @vitest-environment jsdom
// The mock mirrors the server's custody refusal on DELETE /api/workers/{id}: open mock holds
// whose worker_id is the worker block the delete with the same 409 body the real route sends.
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { mockApi } from "./mockApi";
import { ApiError } from "../lib/apiError";
import { state } from "./store";
import { mockRuns } from "./data";

beforeEach(() => window.localStorage.setItem("uzi_mock_scenario", "custody"));
afterEach(() => {
  window.localStorage.removeItem("uzi_mock_scenario");
  vi.clearAllTimers();
  vi.useRealTimers();
});

it("lists the hosted holder under the custody scenario and refuses its delete with the holds", async () => {
  vi.useFakeTimers();
  const listP = mockApi.listWorkers();
  await vi.advanceTimersByTimeAsync(1000);
  const holder = (await listP).workers.find((w) => w.id === "wkr-jvm");
  expect(holder).toMatchObject({ name: "jvm-worker", kind: "hosted" });

  const p = mockApi.deleteWorker("wkr-jvm").catch((e: unknown) => e);
  await vi.advanceTimersByTimeAsync(1000);
  const e = (await p) as ApiError;
  expect(e).toBeInstanceOf(ApiError);
  expect(e.status).toBe(409);
  const { blocking_holds } = e.body as { blocking_holds: { hold_id: string }[] };
  expect(blocking_holds.map((h) => h.hold_id)).toEqual(["hold-b1", "hold-b2"]);
});

it("does not list the holder outside the custody scenario", async () => {
  window.localStorage.removeItem("uzi_mock_scenario");
  vi.useFakeTimers();
  const listP = mockApi.listWorkers();
  await vi.advanceTimersByTimeAsync(1000);
  expect((await listP).workers.some((w) => w.id === "wkr-jvm")).toBe(false);
});

it("refuses to delete a worker that holds open custody holds, oldest first, with the server body", async () => {
  state.runs.set("run-b1", { ...mockRuns[0], id: "run-b1", kind: "issue", title: "Fix login" });
  state.runs.set("run-b2", { ...mockRuns[0], id: "run-b2", kind: "chat", title: "Chat about CI" });
  vi.useFakeTimers();
  const p = mockApi.deleteWorker("wkr-jvm").catch((e: unknown) => e);
  await vi.advanceTimersByTimeAsync(1000);
  const err = await p;
  expect(err).toBeInstanceOf(ApiError);
  const e = err as ApiError;
  expect(e.status).toBe(409);
  const msg =
    "this worker is retaining unpublished committed work for recovery in 2 custody hold(s); recover it (uzi run export) or explicitly discard those recovery archives before removing it";
  expect(e.message).toBe(msg);
  expect(e.body).toMatchObject({
    error: msg,
    custody_holds: 2,
    blocking_holds_total: 2,
  });
  // Oldest first (hold-b1 is seeded older), carrying the run's own kind, title and generation.
  expect((e.body as { blocking_holds: unknown[] }).blocking_holds).toEqual([
    { hold_id: "hold-b1", run_id: "run-b1", generation: 1, kind: "issue", title: "Fix login" },
    { hold_id: "hold-b2", run_id: "run-b2", generation: 2, kind: "chat", title: "Chat about CI" },
  ]);
  state.runs.delete("run-b1");
  state.runs.delete("run-b2");
});

it("deletes a worker without open holds as before", async () => {
  vi.useFakeTimers();
  const p = mockApi.deleteWorker("wkr-no-such-holds");
  await vi.advanceTimersByTimeAsync(1000);
  await expect(p).resolves.toBeNull();
});

// Last in the file: it discards the scenario's holds and deletes the synthetic holder, which is
// module state the earlier cases rely on.
it("keeps the custody holder deleted after its holds are discarded and it is deleted", async () => {
  vi.useFakeTimers();
  for (const [runId, holdId] of [["run-b1", "hold-b1"], ["run-b2", "hold-b2"]]) {
    const d = mockApi.discardHold(runId, holdId);
    await vi.advanceTimersByTimeAsync(1000);
    await d;
  }
  const del = mockApi.deleteWorker("wkr-jvm");
  await vi.advanceTimersByTimeAsync(1000);
  await expect(del).resolves.toBeNull();

  const listP = mockApi.listWorkers();
  await vi.advanceTimersByTimeAsync(1000);
  expect((await listP).workers.some((w) => w.id === "wkr-jvm")).toBe(false);
});

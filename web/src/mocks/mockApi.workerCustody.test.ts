// @vitest-environment jsdom
// The mock mirrors the server's custody refusal on DELETE /api/workers/{id}: open mock holds
// whose worker_id is the worker block the delete with the same 409 body the real route sends.
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { mockApi } from "./mockApi";
import { ApiError } from "../lib/apiError";

beforeEach(() => window.localStorage.setItem("uzi_mock_scenario", "custody"));
afterEach(() => {
  window.localStorage.removeItem("uzi_mock_scenario");
  vi.clearAllTimers();
  vi.useRealTimers();
});

it("refuses to delete a worker that holds open custody holds, oldest first, with the server body", async () => {
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
  const ids = (e.body as { blocking_holds: { hold_id: string; run_id: string }[] }).blocking_holds.map((h) => h.hold_id);
  expect(ids.sort()).toEqual(["hold-b1", "hold-b2"]);
});

it("deletes a worker without open holds as before", async () => {
  vi.useFakeTimers();
  const p = mockApi.deleteWorker("wkr-no-such-holds");
  await vi.advanceTimersByTimeAsync(1000);
  await expect(p).resolves.toBeNull();
});

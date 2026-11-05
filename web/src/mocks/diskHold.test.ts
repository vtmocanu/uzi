// @vitest-environment jsdom
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { mockApi } from "./mockApi";
import { ensureLive } from "./engine";
import { getRun, patchRun, state } from "./store";
import { MockRunSocket } from "./socket";

const id = "run-forge-hold";
let original: NonNullable<ReturnType<typeof getRun>>;
let originalMessages: ReturnType<typeof state.messages.get>;
let originalScenario: string | null;

beforeEach(() => {
  vi.useFakeTimers();
  vi.setSystemTime(new Date("2026-11-05T12:00:00Z"));
  original = { ...getRun(id)! };
  originalMessages = state.messages.get(id)?.map((message) => ({ ...message }));
  originalScenario = window.localStorage.getItem("uzi_mock_scenario");
  window.localStorage.removeItem("uzi_mock_scenario");
  // Reuse an owned issue fixture; no production disk scenario or new wire fields.
  patchRun(id, {
    status: "recovery_wait",
    recovery_wait_cause: "data_volume_full",
    recovery_retry_not_before: null,
    recovery_wait_phase: null,
    hold_reason: null,
    hold_context: "unavailable(same_worker_only)",
    disk_park_count: 5,
    requeue_count: 3,
    worker_id: "w-laptop",
    home_bytes: 8192,
    cache_bytes: 4096,
    disk_truncated: true,
    checkpoint_contains_latest: false,
    checkpoint_tip_at: "2026-11-05T10:00:00Z",
    preserved_patch: "diff --git a/example b/example",
    budget_wall_seconds: 3600,
    budget_total_seconds: 5400,
    budget_extension_seconds: 1200,
    budget_finalize_seconds: 600,
    budget_used_seconds: 1800,
  });
});

afterEach(() => {
  state.runs.set(id, original);
  if (originalMessages === undefined) state.messages.delete(id);
  else state.messages.set(id, originalMessages);
  if (originalScenario === null) window.localStorage.removeItem("uzi_mock_scenario");
  else window.localStorage.setItem("uzi_mock_scenario", originalScenario);
  vi.clearAllTimers();
  vi.useRealTimers();
});

// Attach both outcomes before advancing fake time, including unexpected refusals.
async function settleMockApi<T>(request: Promise<T>): Promise<T> {
  const settled = request.then((value) => ({ value }), (error: unknown) => ({ error }));
  await vi.advanceTimersByTimeAsync(1000);
  const result = await settled;
  if ("error" in result) throw result.error;
  return result.value;
}

it("keeps the NULL disk hold through socket, reads and engine timer activity", async () => {
  const before = { ...getRun(id)! };
  const messages = [...(state.messages.get(id) ?? [])];
  const socket = new MockRunSocket(id);
  const opened = vi.fn();
  socket.onopen = opened;
  try {
    ensureLive(id);
    await vi.advanceTimersByTimeAsync(60_000);
    expect(opened).toHaveBeenCalledOnce();
    expect((await settleMockApi(mockApi.getRun(id))).run).toMatchObject({
      status: "recovery_wait", recovery_wait_cause: "data_volume_full",
      recovery_retry_not_before: null, disk_park_count: 5,
    });
    expect(getRun(id)).toEqual(before);
    expect(state.messages.get(id) ?? []).toEqual(messages);
  } finally {
    socket.close();
  }
});

it("queues one attempt, preserves metadata and refuses concurrent and repeated Resume", async () => {
  const before = { ...getRun(id)! };
  const socket = new MockRunSocket(id);
  const frames: { type: string; status?: string }[] = [];
  socket.onmessage = (event) => frames.push(JSON.parse(event.data));
  try {
    await vi.advanceTimersByTimeAsync(100);
    const results = await settleMockApi(Promise.allSettled([
      mockApi.resumeRun(id), mockApi.resumeRun(id),
    ]));
    expect(results[0]).toMatchObject({ status: "fulfilled", value: { run: { status: "queued" } } });
    expect(results[1]).toMatchObject({ status: "rejected", reason: { status: 409 } });
    const queued = { ...getRun(id)! };
    expect(queued).toEqual({
      ...before,
      status: "queued",
      recovery_wait_cause: null,
      recovery_retry_not_before: null,
      recovery_wait_phase: null,
      health: "ok",
      health_reason: null,
      health_since: null,
      updated_at: expect.any(String),
      status_since: expect.any(String),
      progress: expect.objectContaining({ state: "queued" }),
    });
    expect(queued.status_since).not.toBe(before.status_since);
    expect(queued.status_since).toBe(queued.updated_at);
    await expect(mockApi.resumeRun(id)).rejects.toMatchObject({ status: 409 });
    expect(getRun(id)).toEqual(queued);
    expect(frames.filter((frame) => frame.type === "state" && frame.status === "queued")).toHaveLength(1);
  } finally {
    socket.close();
  }
});

it("does not renew a provider budget carried by retained hold metadata", async () => {
  patchRun(id, {
    hold_reason: "provider_unavailable", provider_park_count: 7,
    provider_park_streak_started_at: "2026-11-05T10:00:00Z",
    can_stop_at_provider_hold: true,
  });
  const { run } = await settleMockApi(mockApi.resumeRun(id));
  expect(run).toMatchObject({
    status: "queued", hold_reason: "provider_unavailable", disk_park_count: 5,
    provider_park_count: 7, provider_park_streak_started_at: "2026-11-05T10:00:00Z",
    can_stop_at_provider_hold: true,
  });
});

it("requires a new Resume for a subsequent NULL disk hold", async () => {
  await settleMockApi(mockApi.resumeRun(id));
  patchRun(id, {
    status: "recovery_wait", recovery_wait_cause: "data_volume_full",
    recovery_retry_not_before: null, disk_park_count: 5,
  });
  const held = { ...getRun(id)! };
  const socket = new MockRunSocket(id);
  try {
    ensureLive(id);
    await vi.advanceTimersByTimeAsync(120_000);
    expect(getRun(id)).toEqual(held);
    const { run } = await settleMockApi(mockApi.resumeRun(id));
    expect(run).toMatchObject({ status: "queued", disk_park_count: 5 });
    await expect(mockApi.resumeRun(id)).rejects.toMatchObject({ status: 409 });
  } finally {
    socket.close();
  }
});

const nonHeldTiming = [
  ["timed", "2026-11-05T12:05:00Z"],
  ["overdue", "2026-11-05T11:55:00Z"],
  ["omitted", undefined],
  ["invalid", "not-a-date"],
] as const;

it.each(nonHeldTiming)("refuses Resume for %s disk recovery without mutation", async (_label, retry) => {
  patchRun(id, { recovery_retry_not_before: retry });
  if (retry === undefined) delete getRun(id)!.recovery_retry_not_before;
  const before = { ...getRun(id)! };
  await expect(mockApi.resumeRun(id)).rejects.toMatchObject({ status: 409 });
  expect(getRun(id)).toEqual(before);
});

it("refuses token switching on a NULL disk hold before mutating credentials", async () => {
  const before = { ...getRun(id)! };
  await expect(mockApi.setRunCredential(id, { mode: "auto" })).rejects.toMatchObject({
    status: 409,
    message: "data_volume_full held: use Resume before switching its token",
  });
  expect(getRun(id)).toEqual(before);
});

it.each(nonHeldTiming)("retains normal token switching for %s disk recovery", async (_label, retry) => {
  patchRun(id, { recovery_retry_not_before: retry });
  if (retry === undefined) delete getRun(id)!.recovery_retry_not_before;
  const { run } = await settleMockApi(mockApi.setRunCredential(id, { mode: "auto" }));
  expect(run).toMatchObject({
    status: "recovery_wait", recovery_wait_cause: "data_volume_full",
    credential_override: { mode: "auto" }, credential_switch: null, disk_park_count: 5,
  });
  expect(run.recovery_retry_not_before).toBe(retry);
});

it.each([
  ["total exhausted", 5400, 5400],
  ["total exceeded", 5400, 5401],
  ["fallback exhausted", null, 5400],
] as const)("refuses Resume with %s wall budget without mutation", async (_label, total, used) => {
  patchRun(id, { budget_total_seconds: total, budget_used_seconds: used });
  const before = { ...getRun(id)! };
  await expect(mockApi.resumeRun(id)).rejects.toMatchObject({
    status: 409, message: expect.stringContaining("out of time; extend it to resume"),
  });
  expect(getRun(id)).toEqual(before);
});

it("uses the existing extension and finalize budget when the total is unavailable", async () => {
  patchRun(id, { budget_total_seconds: null, budget_used_seconds: 5399 });
  const { run } = await settleMockApi(mockApi.resumeRun(id));
  expect(run).toMatchObject({
    status: "queued", budget_total_seconds: null, budget_wall_seconds: 3600,
    budget_used_seconds: 5399, budget_extension_seconds: 1200, budget_finalize_seconds: 600,
  });
});

it("allows Resume when the used wall budget is unavailable", async () => {
  patchRun(id, { budget_total_seconds: null, budget_used_seconds: null });
  const { run } = await settleMockApi(mockApi.resumeRun(id));
  expect(run).toMatchObject({
    status: "queued", budget_total_seconds: null, budget_used_seconds: null,
    budget_extension_seconds: 1200, budget_finalize_seconds: 600,
  });
});

it("keeps the existing nonowner backstop and read access for a disk hold", async () => {
  window.localStorage.setItem("uzi_mock_scenario", "forge-hold-nonowner");
  expect((await settleMockApi(mockApi.getRun(id))).run.status).toBe("recovery_wait");
  const before = { ...getRun(id)! };
  await expect(mockApi.getRunInputs(id)).rejects.toMatchObject({ status: 404 });
  await expect(mockApi.resumeRun(id)).rejects.toMatchObject({ status: 404 });
  await expect(mockApi.submitRunInput(id, "cancel")).rejects.toMatchObject({ status: 404 });
  expect(getRun(id)).toEqual(before);
});

it("uses the existing Cancel terminal stamp without changing disk or retained metadata", async () => {
  const before = { ...getRun(id)! };
  const cancelledAt = new Date().toISOString();
  await settleMockApi(mockApi.submitRunInput(id, "cancel"));
  const cancelled = { ...getRun(id)! };
  expect(cancelled).toMatchObject({
    status: "failed", stop_kind: "cancelled", failure_reason: "run cancelled",
    finished_at: cancelledAt, disk_park_count: 5,
    worker_id: before.worker_id, preserved_patch: before.preserved_patch,
    checkpoint_contains_latest: false, checkpoint_tip_at: before.checkpoint_tip_at,
    budget_used_seconds: before.budget_used_seconds,
  });
  ensureLive(id);
  await vi.advanceTimersByTimeAsync(60_000);
  expect(getRun(id)).toEqual(cancelled);
});

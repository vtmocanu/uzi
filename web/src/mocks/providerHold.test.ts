// @vitest-environment jsdom
import { afterEach, expect, it, vi } from "vitest";
import { mockApi } from "./mockApi";

afterEach(() => { vi.clearAllTimers(); vi.useRealTimers(); });

it("provider mock Stop scopes completed work before Resume and clears provider streak", async () => {
  vi.useFakeTimers();
  const pending = mockApi.submitRunInput("run-provider-hold", "scope", "3");
  await vi.advanceTimersByTimeAsync(1000);
  await expect(pending).resolves.toMatchObject({ server_side: true });
  const heldRead = mockApi.getRun("run-provider-hold");
  await vi.advanceTimersByTimeAsync(1000);
  const { run: held } = await heldRead;
  expect(held.status).toBe("paused");
  expect(held.scope_ceiling).toBe(3);
  expect(held.provider_park_count).toBe(12);
  const resume = mockApi.resumeRun("run-provider-hold");
  await vi.advanceTimersByTimeAsync(1000);
  await expect(resume).resolves.toMatchObject({ run: {
    status: "queued", scope_ceiling: 3, provider_park_count: 0,
    provider_park_streak_started_at: null, can_stop_at_provider_hold: false,
  } });
});

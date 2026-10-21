// @vitest-environment jsdom
// Issue #3181: the mock API mirrors the server for the token-switch holds. A settling hold
// refuses Resume and another token switch with the server's 409 text; an unsettled hold
// resumes (and a switch requeues it on the chosen token).
import { afterEach, expect, it, vi } from "vitest";
import { mockApi } from "./mockApi";
import { ApiError } from "../lib/apiError";

afterEach(() => { vi.clearAllTimers(); vi.useRealTimers(); });

const REFUSAL = "the worker is still stopping this run's processes; Resume becomes available when it finishes, or Cancel the run";

async function settle<T>(p: Promise<T>): Promise<T> {
  await vi.advanceTimersByTimeAsync(1000);
  return p;
}

it("settling refuses Resume and Switch token with the server's 409 and stays held", async () => {
  vi.useFakeTimers();
  for (const call of [
    () => mockApi.resumeRun("run-cred-switch-settling"),
    () => mockApi.setRunCredential("run-cred-switch-settling", { mode: "auto" }),
  ]) {
    const err = await call().catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).status).toBe(409);
    expect((err as ApiError).message).toBe(REFUSAL);
  }
  const { run } = await settle(mockApi.getRun("run-cred-switch-settling"));
  expect(run).toMatchObject({ status: "paused", hold_reason: "credential_switch_settling" });
});

it("unsettled Resume requeues and clears the hold", async () => {
  vi.useFakeTimers();
  const { run } = await settle(mockApi.resumeRun("run-cred-switch-unsettled"));
  expect(run).toMatchObject({ status: "queued", hold_reason: null });
});

it("unsettled set-token requeues on the chosen token, clears the hold and keeps the override", async () => {
  // A fresh store: the Resume case above already requeued this fixture in the shared one.
  vi.resetModules();
  const { mockApi } = await import("./mockApi");
  vi.useFakeTimers();
  const { run: before } = await settle(mockApi.getRun("run-cred-switch-unsettled"));
  expect(before).toMatchObject({ status: "paused", hold_reason: "credential_switch_unsettled" });
  const { run } = await settle(mockApi.setRunCredential("run-cred-switch-unsettled", { mode: "pinned", secret_id: "sec-default" }));
  expect(run).toMatchObject({ status: "queued", hold_reason: null, credential_override: { mode: "pinned", label: "default" } });
  const { run: read } = await settle(mockApi.getRun("run-cred-switch-unsettled"));
  expect(read).toMatchObject({ status: "queued", hold_reason: null, credential_override: { mode: "pinned", label: "default" } });
});

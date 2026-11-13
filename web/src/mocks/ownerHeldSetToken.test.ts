// @vitest-environment jsdom
import { afterEach, beforeEach, expect, it } from "vitest";
import { mockApi } from "./mockApi";
import { getRun, patchRun } from "./store";

// Issue #4231: set-token on an owner-held recovery_wait run is refused with the server's
// RunOwnerHeldError text (api/internal/workersvc/run_credential.go) and writes nothing.
const id = "run-memory-hold";
const generic =
  "this owner hold prevents switching the token; inspect the run's recovery options: Resume may re-hold it, Cancel ends the run";
let original: NonNullable<ReturnType<typeof getRun>>;

beforeEach(() => {
  original = { ...getRun(id)! };
});

afterEach(() => {
  patchRun(id, original);
});

it.each([
  ["worker_memory_pressure", "worker memory pressure"],
  ["worker_requeue_exhausted", "worker requeue exhausted"],
  ["codex_account_unavailable", "codex account unavailable"],
  ["retained_recovery_adoption_failed", "retained recovery adoption failed"],
  ["retained_continuation_unwitnessed", "retained continuation unwitnessed"],
  ["retained_recovery_exhausted", "retained recovery exhausted"],
] as const)("refuses set-token on a %s hold without mutation", async (cause, label) => {
  patchRun(id, { status: "recovery_wait", recovery_wait_cause: cause, harness: "claude" });
  const before = { ...getRun(id)! };
  await expect(mockApi.setRunCredential(id, { mode: "auto" })).rejects.toMatchObject({
    status: 409,
    message: `${generic} (hold: ${label})`,
  });
  expect(getRun(id)).toEqual(before);
});

it("still accepts set-token on a promotable recovery_wait cause", async () => {
  patchRun(id, {
    status: "recovery_wait", recovery_wait_cause: "provider_outage", harness: "claude",
    recovery_retry_not_before: "2026-11-13T00:10:00Z",
  });
  await expect(mockApi.setRunCredential(id, { mode: "auto" })).resolves.toMatchObject({
    run: { credential_override: { mode: "auto" } },
  });
});

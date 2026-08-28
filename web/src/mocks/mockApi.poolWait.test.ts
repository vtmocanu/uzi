// @vitest-environment jsdom
import { beforeEach, describe, it, expect } from "vitest";
import { ApiError } from "../lib/api";
import { mockApi } from "./mockApi";
import { patchRun } from "./store";

// The mock store is a module singleton seeded once, so `resumeRunNow` (which moves the
// run to `queued`, irreversibly through the API) would leak state across these cases.
// Re-park the fixture before each so every test is order-independent.
beforeEach(() => {
  patchRun("run-pool-wait", { status: "pool_wait" });
});

// Issue #754: the "Resume now" control, as the demo implements it. The mock is a
// second implementation of the server's rule (owner-scoped, pool_wait-only, 409
// otherwise), so these keep it honest — most importantly the idempotent-ish 409 on a
// run already resumed to `queued`, which the panel's "no longer waiting" note relies on.
describe("mockApi.resumeRunNow (issue #754)", () => {
  it("moves a pool_wait run to queued", async () => {
    const before = await mockApi.getRun("run-pool-wait");
    expect(before.run.status).toBe("pool_wait");
    const { run } = await mockApi.resumeRunNow("run-pool-wait");
    expect(run.status).toBe("queued");
  });

  it("409s on a second call — the run is no longer waiting", async () => {
    // First call resumes it (idempotent-ish: the second sees `queued`, not pool_wait).
    await mockApi.resumeRunNow("run-pool-wait");
    await expect(mockApi.resumeRunNow("run-pool-wait")).rejects.toMatchObject({
      status: 409,
    });
  });

  it("409s on a run that is not pool_wait, with the server's message", async () => {
    await expect(mockApi.resumeRunNow("run-limit-wait")).rejects.toMatchObject({
      status: 409,
      message: "run is not waiting for a pooled token",
    });
    await expect(mockApi.resumeRunNow("run-done")).rejects.toBeInstanceOf(ApiError);
  });

  it("404s for a run that does not exist", async () => {
    await expect(mockApi.resumeRunNow("run-nope")).rejects.toMatchObject({ status: 404 });
  });
});

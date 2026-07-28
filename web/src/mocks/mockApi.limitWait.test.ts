// @vitest-environment jsdom
import { afterEach, describe, it, expect, vi } from "vitest";
import { ApiError } from "../lib/api";
import { mockApi } from "./mockApi";

afterEach(() => {
  vi.resetModules();
});

// PRD #35: the per-run usage-limit toggle, as the demo implements it. The mock is a
// second implementation of the server's rule, so these exist to keep it from
// teaching the wrong thing — a demo that un-parked a run on toggle-off would be a
// convincing, hands-on lesson in exactly the misreading this control invites.
describe("mockApi.setRunWaitOnLimit (PRD #35)", () => {
  it("flips the flag on a parked run", async () => {
    const { run } = await mockApi.setRunWaitOnLimit("run-limit-wait", false);
    expect(run.wait_on_limit).toBe(false);
    const back = await mockApi.setRunWaitOnLimit("run-limit-wait", true);
    expect(back.run.wait_on_limit).toBe(true);
  });

  it("🔴 does NOT change status — it is not a cancel, and the park keeps its clock", async () => {
    const before = await mockApi.getRun("run-limit-wait");
    const { run } = await mockApi.setRunWaitOnLimit("run-limit-wait", false);
    expect(run.status).toBe("limit_wait");
    expect(run.retry_not_before).toBe(before.run.retry_not_before);
    expect(run.limit_resets_at).toBe(before.run.limit_resets_at);
    expect(run.limit_wait_count).toBe(before.run.limit_wait_count);
    expect(run.finished_at).toBeNull();
    // Restore, so ordering between tests in this file cannot matter.
    await mockApi.setRunWaitOnLimit("run-limit-wait", true);
  });

  it("is a no-op error on a terminal run, mirroring the server's negative guard", async () => {
    await expect(mockApi.setRunWaitOnLimit("run-done", true)).rejects.toBeInstanceOf(ApiError);
    await expect(mockApi.setRunWaitOnLimit("run-failed", true)).rejects.toBeInstanceOf(ApiError);
    await expect(mockApi.setRunWaitOnLimit("run-cancelled", true)).rejects.toBeInstanceOf(ApiError);
  });

  it("admits every non-terminal status, limit_wait included", async () => {
    // The server's guard is the same NEGATIVE predicate the cancel path uses, so
    // limit_wait is admitted for free. An allowlist here would drift from that.
    for (const id of ["run-live", "run-awaiting", "run-limit-wait"]) {
      const { run } = await mockApi.setRunWaitOnLimit(id, true);
      expect(run.wait_on_limit, `${id} should be togglable`).toBe(true);
    }
  });

  it("404s for a run that does not exist", async () => {
    await expect(mockApi.setRunWaitOnLimit("run-nope", true)).rejects.toMatchObject({ status: 404 });
  });
});

// PRD #35 M3: the per-user DEFAULT. Separate from the per-run toggle above, and the
// separation is the thing being asserted.
describe("mockApi.setWaitOnLimit — the user default (PRD #35)", () => {
  it("flips the session user's default", async () => {
    const { user } = await mockApi.setWaitOnLimit(true);
    expect(user.wait_on_limit).toBe(true);
    const off = await mockApi.setWaitOnLimit(false);
    expect(off.user.wait_on_limit).toBe(false);
  });

  it("🔴 does NOT retro-apply to runs that already exist", async () => {
    // The flag is copied onto a run at CREATION. A mock that swept state.runs would
    // silently undo every per-run override — including on the run the user is
    // looking at — and would teach the demo's users the opposite of how this works.
    const before = await mockApi.getRun("run-limit-wait");
    expect(before.run.wait_on_limit).toBe(true);

    await mockApi.setWaitOnLimit(false);

    const after = await mockApi.getRun("run-limit-wait");
    expect(after.run.wait_on_limit).toBe(true);
    expect(after.run.status).toBe("limit_wait");

    await mockApi.setWaitOnLimit(true);
  });
});

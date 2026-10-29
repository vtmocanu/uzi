import assert from "node:assert/strict";
import { it } from "node:test";
import type { Executor } from "../src/executor.js";
import { client, deferred, fakeGitlab, fx, gitlabClaim, installHarness, runnerWith } from "./runner-harness.js";

// #3404 M1b rework: the runner's ctx.reportProgress must hand back the ACTUAL enqueued report, so an
// executor that owns the fire-and-forget tail (the Codex lifetime) has something real to join.

installHarness();

for (const mode of ["settles-after-post", "rejection-is-handled"] as const) {
  it(`runner reportProgress ${mode}`, async t => {
    const post = deferred();
    let posted = false;
    t.mock.method(client, "reportState", async (...args: Parameters<typeof client.reportState>) => {
      if (!args[1].milestones_in_progress?.includes("join-m1")) return { staleClaim: false } as never;
      await post.promise;
      posted = true;
      if (mode === "rejection-is-handled") throw new Error("report transport down");
      return { staleClaim: false } as never;
    });
    let observed: { settledBeforePost: boolean; settledAfterPost: boolean; rejected: boolean } | undefined;
    const executor: Executor = {
      run: async ctx => {
        let settled = false, rejected = false;
        const pending = ctx.reportProgress!({ completed: [], in_progress: ["join-m1"] })
          .then(() => { settled = true; }, () => { rejected = true; });
        await new Promise<void>(resolve => setImmediate(resolve));
        const settledBeforePost = settled || rejected;
        post.resolve();
        await pending;
        observed = { settledBeforePost, settledAfterPost: settled && posted, rejected };
        throw new Error("observation complete");
      },
    };
    const forge = fakeGitlab();
    const worker = runnerWith(() => ({ executor }), forge.gitlab);
    await worker.execute(gitlabClaim(340401, { repo: { id: "r1", url: "https://gitlab.example.test/org/repo", clone_url: fx.originPath } }))
      .catch(() => undefined);
    assert.ok(observed, "the executor observed the returned promise");
    assert.equal(observed.settledBeforePost, false, "the promise is the real report, not an immediate resolve");
    assert.equal(observed.settledAfterPost, true, "it settles only after the report POST settled, and never rejects");
    assert.equal(observed.rejected, false);
  });
}

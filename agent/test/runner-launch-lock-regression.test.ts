// issue #4246 regression, driven only through runner.execute: a launch round whose wait for the bare
// repository lock expires while an independent holder owns the lock must not fail the run at startup.
// Real bare lock, real time (a short real boundary deadline), no timer or clock mocks. The test touches
// no retry helper, so it runs unchanged against code that predates the launch-round retry, where it
// fails on the run-failed assertion rather than on a missing symbol.
import assert from "node:assert/strict";
import { AsyncLocalStorage } from "node:async_hooks";
import { test } from "node:test";
import { StubExecutor } from "../src/executor.js";
import { nullLogger } from "./helpers.js";
import { api, fakeGitlab, git, gitlabClaim, installHarness, runner as runnerFor } from "./runner-harness.js";

installHarness();

const SHORT_DEADLINE_MS = 200;

function latch() {
  let release!: () => void;
  const promise = new Promise<void>(resolve => { release = resolve; });
  return { promise, release };
}

test("#4246 a launch round whose bare-lock wait expires behind another holder does not fail the run", async t => {
  const claim = gitlabClaim(4246);
  const stub = new StubExecutor(nullLogger());
  const origRun = stub.run.bind(stub);
  let executorRuns = 0;
  const runSpy = t.mock.method(stub, "run", async (...args: Parameters<typeof origRun>) => {
    executorRuns++;
    return origRun(...args);
  });
  const runner = runnerFor(stub, fakeGitlab().gitlab, undefined, { recoveryRetryMs: 1 });
  const subject = runner as any;
  const defaultDeadlineMs = subject.codexBoundaryDeadlineMs;

  // Bound outside any operation scope, so the holder is independent of the round it blocks.
  const hold = AsyncLocalStorage.bind((bare: string, entered: () => void, until: Promise<void>) =>
    git.withBareLock(bare, async () => { entered(); await until; }));

  let rounds = 0, inFirstRound = false, injected = false, holder: Promise<void> | undefined;
  let releaseHolder: (() => void) | undefined;

  const authority = subject.requireLaunchAuthority.bind(subject);
  const roundSpy = t.mock.method(subject, "requireLaunchAuthority", async (...args: unknown[]) => {
    rounds++;
    const first = rounds === 1;
    if (first) { inFirstRound = true; subject.codexBoundaryDeadlineMs = SHORT_DEADLINE_MS; }
    try { return await authority(...args); }
    catch (error) { releaseHolder?.(); throw error; }
    finally { if (first) { inFirstRound = false; subject.codexBoundaryDeadlineMs = defaultDeadlineMs; } }
  });

  const readFacts = (git.readRemediationRestrictionFacts as (...args: any[]) => Promise<any>).bind(git);
  const readSpy = t.mock.method(git, "readRemediationRestrictionFacts", async (...args: any[]) => {
    if (inFirstRound && !injected) {
      injected = true;
      const entered = latch(), until = latch();
      releaseHolder = until.release;
      holder = hold(args[0], entered.release, until.promise);
      await entered.promise; // the holder owns the lock before this read proceeds
    }
    return readFacts(...args);
  });

  try {
    await runner.execute(claim);
  } finally {
    releaseHolder?.();
    await holder;
    // LIFO restore, then stop the runner.
    readSpy.mock.restore();
    roundSpy.mock.restore();
    runSpy.mock.restore();
    runner.shutdown();
  }

  const failed = api.states.filter(s => s.runId === claim.run_id && s.body.status === "failed").map(s => s.body as any);
  assert.ok(injected, "the holder was injected into the first launch round");
  assert.equal(failed.length, 0, "no failed report for the run; reasons: " +
    failed.map(f => String(f.failure_reason)).join("; "));
  assert.equal(executorRuns, 1, "the stub executor ran exactly once");
});

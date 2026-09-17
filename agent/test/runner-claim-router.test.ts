import { describe, it } from "node:test";
import assert from "node:assert/strict";
import path from "node:path";

import { type ExecutorFactory } from "../src/runner.js";
import { nullLogger } from "./helpers.js";
import { api, client, deferred, fakeGitlab, gitlabClaim, homeDir, installHarness, runnerWith } from "./runner-harness.js";

// PRD #1391 Run B M4 — the generation-aware queued-duplicate claim router (runner.ts execute()) and
// the phaseClone first-running-ack guard. A SECOND claim of a run serialised behind the first must
// probe ownership and proceed ONLY on a claimed/running row AT the claim's generation; a terminal
// status, a different generation, a held/queued row, or an exhausted transient-probe budget end the
// attempt with NO report. phaseClone stops when its first `running` report is refused with a terminal
// status.

installHarness();

/**
 * Drive a queued-duplicate scenario: a first execution blocks in its executor until released (holding
 * its run's execution tail), a second execution of the same run is started (so its `previous` is the
 * first's pending tail and the M4 router runs), the ownership probe is configured, then the first is
 * released so the second's router runs. Returns whether the SECOND execution actually reached the
 * executor (proceeded) and how many ownership probes fired.
 */
async function driveQueuedDuplicate(opts: {
  claimGen?: number;
  budgetMs?: number;
  configureProbe: (runId: string) => void;
}): Promise<{ secondExecuted: boolean; probes: number }> {
  const { gitlab } = fakeGitlab();
  const claim = gitlabClaim(2000, opts.claimGen !== undefined ? { claim_generation: opts.claimGen } : {});
  const runId = claim.run_id;
  const firstStarted = deferred();
  const firstGate = deferred();
  let attempt = 0;
  let secondExecuted = false;
  const factory: ExecutorFactory = () => {
    const a = ++attempt;
    return {
      homeDir: path.join(homeDir, runId),
      executor: {
        run: async () => {
          if (a === 1) {
            firstStarted.resolve();
            await firstGate.promise;
            throw new Error("first attempt ends (holds no terminal outcome)");
          }
          secondExecuted = true;
          throw new Error("second attempt executed");
        },
      },
    };
  };
  const origOwn = client.getRunOwnership.bind(client);
  let probes = 0;
  client.getRunOwnership = async (rid) => {
    probes++;
    return origOwn(rid);
  };
  const runner = runnerWith(factory, gitlab, undefined, nullLogger(), {
    recoveryRetryMs: 5,
    queuedDuplicateProbeBudgetMs: opts.budgetMs ?? 20_000,
  });
  const p1 = runner.execute(claim);
  let p2: Promise<void> = Promise.resolve();
  try {
    await Promise.race([
      firstStarted.promise,
      p1.then(() => {
        throw new Error("first execution ended before it started");
      }),
    ]);
    // The duplicate: its `previous` is the first's still-pending tail, so execute() runs the router.
    p2 = runner.execute({ ...claim });
    opts.configureProbe(runId);
    firstGate.resolve();
    await Promise.allSettled([p1, p2]);
    return { secondExecuted, probes };
  } finally {
    firstGate.resolve();
    runner.shutdown();
    await Promise.allSettled([p1, p2]);
  }
}

describe("RunRunner queued-duplicate claim router (PRD #1391 Run B M4)", () => {
  it("a same-generation running probe PROCEEDS (the duplicate re-adopts and executes)", async () => {
    const { secondExecuted } = await driveQueuedDuplicate({
      claimGen: 5,
      configureProbe: (runId) => api.setOwnershipStatus(runId, "running", 5),
    });
    assert.equal(secondExecuted, true, "claimed/running at the claim's generation ⇒ proceed to execute");
  });

  it("a duplicate racing a pending COMPLETION never executes (terminal status ends the attempt)", async () => {
    const { secondExecuted } = await driveQueuedDuplicate({
      claimGen: 5,
      configureProbe: (runId) => api.setOwnershipStatus(runId, "completed", 5),
    });
    assert.equal(secondExecuted, false, "a terminal (completed) status ends the attempt with no execution");
  });

  it("a DIFFERENT generation ends the attempt (a newer claim owns the run)", async () => {
    const { secondExecuted } = await driveQueuedDuplicate({
      claimGen: 5,
      // running, but at generation 999 — a reclaim bumped it. The duplicate holds the STALE claim 5.
      configureProbe: (runId) => api.setOwnershipStatus(runId, "running", 999),
    });
    assert.equal(secondExecuted, false, "running at a DIFFERENT generation ends the attempt");
  });

  it("a held/queued row at the SAME generation ends the attempt without executing", async () => {
    const { secondExecuted } = await driveQueuedDuplicate({
      claimGen: 5,
      budgetMs: 30, // a tight budget so the bounded wait ends fast
      // `queued` (e.g. SweepClaimedNeverStarted, which bumps nothing) — never proceed.
      configureProbe: (runId) => api.setOwnershipStatus(runId, "queued", 5),
    });
    assert.equal(secondExecuted, false, "a queued/held row ends the attempt without executing");
  });

  it("a transient probe failure retries then gives up cleanly (no execution)", async () => {
    const { secondExecuted, probes } = await driveQueuedDuplicate({
      claimGen: 5,
      budgetMs: 40,
      configureProbe: (runId) => api.failOwnership(runId, 503),
    });
    assert.equal(secondExecuted, false, "a persistently-failing probe ends the attempt without executing");
    assert.ok(probes >= 2, "the transient failure was retried before giving up");
  });
});

describe("RunRunner phaseClone first-running-ack guard (PRD #1391 Run B M4)", () => {
  it("stops the phase clone when the first running report is refused with a terminal status", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(2001, { claim_generation: 3 });
    let executorRuns = 0;
    const factory: ExecutorFactory = () => ({
      homeDir: path.join(homeDir, claim.run_id),
      executor: {
        run: async () => {
          executorRuns++;
          throw new Error("must never run: the running ack was refused terminal");
        },
      },
    });
    // The FIRST `running` report comes back 409 with the run already `completed` (a racing owner
    // cancel / an outcome that landed). applied:false + a terminal status ⇒ stop the phase clone.
    api.failStateWhen(claim.run_id, (b) => b.status === "running", { httpStatus: 409, runStatus: "completed" });

    await runnerWith(factory, gitlab, undefined, nullLogger(), { recoveryRetryMs: 5 }).execute(claim);

    assert.equal(executorRuns, 0, "a phase clone whose first running ack was refused never starts the model");
    assert.equal(
      api.states.some((s) => s.body.status === "failed"),
      false,
      "a terminal-refused running ack stops the flight with NO failed report (the outcome already landed)",
    );
  });
});

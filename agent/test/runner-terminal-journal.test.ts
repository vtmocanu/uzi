import { afterEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import fsp from "node:fs/promises";
import os from "node:os";
import path from "node:path";

import { nullLogger } from "./helpers.js";
import { StubExecutor, type Executor, type ExecutorResult } from "../src/executor.js";
import { Outbox } from "../src/outbox.js";
import { api, fakeGitlab, gitlabClaim, installHarness, runner, simulateCommittedWork } from "./runner-harness.js";

// PRD #1391 Run B M3b — the run-lane WRITE-AHEAD terminal send path, end to end through RunRunner.
// A real Outbox is threaded via RunnerOptions; the FakeApi refuses the terminal report with a
// benign 409 `running` (nothing changed server-side) so the write-ahead journal STAYS on disk and
// the test can prove it was journalled before the send, and that a non-terminal 409 keeps it.

installHarness();

const tmpRoots: string[] = [];
afterEach(async () => {
  for (const r of tmpRoots.splice(0)) await fsp.rm(r, { recursive: true, force: true }).catch(() => undefined);
});

async function mkOutbox(): Promise<Outbox> {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "runner-journal-"));
  tmpRoots.push(dir);
  const outbox = new Outbox({
    root: path.join(dir, "outbox"),
    log: nullLogger(),
    runMaxBytes: 64 * 1024 * 1024,
    maxBytes: 512 * 1024 * 1024,
    retentionMs: 7 * 86_400_000,
  });
  await outbox.init();
  return outbox;
}

/** An executor that throws immediately, so execute() unwinds into reportGenericFailure. */
class ThrowingExecutor implements Executor {
  async run(): Promise<ExecutorResult> {
    throw new Error("agent boom");
  }
}

describe("RunRunner terminal journaling (PRD #1391 Run B M3b)", () => {
  it("journals the COMPLETED terminal write-ahead at finishCommittedPublish; a 409 running keeps the durable journal", async () => {
    const { gitlab } = fakeGitlab();
    const outbox = await mkOutbox();
    const claim = gitlabClaim(7, { claim_generation: 3 });
    simulateCommittedWork(); // a real completing run committed work, so the empty-diff guard stays quiet
    // The completed report is refused with a benign 409 `running` (nothing applied), so the
    // write-ahead journal is KEPT rather than retired — exactly the outage/lost-ack shape.
    api.failStateWhen(claim.run_id, (b) => b.status === "completed", { httpStatus: 409, runStatus: "running" });

    await runner(new StubExecutor(nullLogger()), gitlab, undefined, {
      outbox,
      outboxTerminalMaxBytes: 1 << 20,
      gapFillMax: 100,
    }).execute(claim);

    assert.equal(outbox.hasPendingTerminal(claim.run_id, 3), true, "the completed outcome is journalled write-ahead and kept on a 409");
    const j = await outbox.readTerminalJournal(claim.run_id, 3);
    assert.equal(j?.body.status, "completed", "the journal holds the terminal completed state");
    assert.equal(j?.body.branch, "agent/issue-7", "with the pushed branch");
    assert.equal(typeof j?.messagesThroughSeq, "number", "the fence is the run's durable emitted tail");
  });

  it("retires the journal on a normal 200 completed — no residue on the happy path", async () => {
    const { gitlab } = fakeGitlab();
    const outbox = await mkOutbox();
    const claim = gitlabClaim(8, { claim_generation: 4 });
    simulateCommittedWork();

    await runner(new StubExecutor(nullLogger()), gitlab, undefined, {
      outbox,
      outboxTerminalMaxBytes: 1 << 20,
      gapFillMax: 100,
    }).execute(claim);

    const statuses = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body.status);
    assert.ok(statuses.includes("completed"), "the run completed");
    assert.equal(outbox.hasPendingTerminal(claim.run_id, 4), false, "a 200 completed retires the write-ahead journal");
  });

  it("reportGenericFailure journals the FAILED terminal write-ahead; a 409 running keeps it", async () => {
    const { gitlab } = fakeGitlab();
    const outbox = await mkOutbox();
    const claim = gitlabClaim(9, { claim_generation: 5 });
    api.failStateWhen(claim.run_id, (b) => b.status === "failed", { httpStatus: 409, runStatus: "running" });

    await runner(new ThrowingExecutor(), gitlab, undefined, {
      outbox,
      outboxTerminalMaxBytes: 1 << 20,
      gapFillMax: 100,
    }).execute(claim);

    assert.equal(outbox.hasPendingTerminal(claim.run_id, 5), true, "the generic-failure outcome is journalled write-ahead");
    const j = await outbox.readTerminalJournal(claim.run_id, 5);
    assert.equal(j?.body.status, "failed", "the journal holds the terminal failed state");
  });
});

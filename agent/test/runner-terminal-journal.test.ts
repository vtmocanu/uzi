import { afterEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import fsp from "node:fs/promises";
import os from "node:os";
import path from "node:path";

import { nullLogger } from "./helpers.js";
import { StubExecutor, type Executor, type ExecutorResult, type RunContext } from "../src/executor.js";
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

  // B1 (BLOCKING): the run-lane reportState choke point (flight.reportState) THROWS StaleClaimError
  // on a stale ack instead of returning it. Before the fix, resolvePendingTerminal's catch read that
  // throw as a transport failure and KEPT the journal forever — the run's whole outbox tree leaked,
  // holding its terminal_pending lease open (D11). This drives the REAL run-lane path (the api
  // answers the completed report a 409 with the stale_claim disposition, the shape flight.reportState
  // throws on) and proves the journal is STALE-RETIRED, not leaked.
  it("PRD #1391 Run B M3 (B1): a run-lane stale_claim STALE-RETIRES the journal (no leak, no re-send)", async () => {
    const { gitlab } = fakeGitlab();
    const outbox = await mkOutbox();
    const claim = gitlabClaim(11, { claim_generation: 6 });
    simulateCommittedWork();
    // The completed report is refused 409 with a TOP-LEVEL stale_claim disposition — flight.reportState
    // reads it into ack.staleClaim and THROWS StaleClaimError, exactly as a real superseded claim.
    api.failStateWhen(claim.run_id, (b) => b.status === "completed", {
      httpStatus: 409,
      runStatus: "running",
      disposition: "stale_claim",
    });

    await runner(new StubExecutor(nullLogger()), gitlab, undefined, {
      outbox,
      outboxTerminalMaxBytes: 1 << 20,
      gapFillMax: 100,
    }).execute(claim);

    assert.equal(outbox.hasPendingTerminal(claim.run_id, 6), false, "the journal was STALE-RETIRED, not leaked (the B1 bug leaves it pending)");
    assert.equal(outbox.depthFor(claim.run_id)?.pendingTerminal ?? 0, 0, "no pending terminal remains");
    assert.equal(outbox.depthFor(claim.run_id)?.staleRetired, 1, "the D11 stale-retired counter advanced by one (0 on the unfixed code)");
    // No APPLIED completed report: the one send was refused stale, and there is no re-send after the
    // local stale-retire (a re-send would be applied 200 and recorded here).
    const appliedCompleted = api.states.filter((s) => s.runId === claim.run_id && s.body.status === "completed");
    assert.equal(appliedCompleted.length, 0, "no applied completed — no server write, no re-send after the stale-retire");
  });

  // PRD #1391 Run B M3 (D5): the permanent-failure hook must install the durable `failed` journal
  // BEFORE aborting the attempt, so a completion racing the trip can never reverse the first durable
  // winner (D4). This tests the REAL handlePermanentFailure method (extracted from the batcher hook)
  // and snapshots the on-disk journal at the EXACT moment the abort fires. A mutation reordering the
  // method to abort-BEFORE-journal reddens this (journalOnDiskAtAbort would be false).
  it("PRD #1391 Run B M3 (D5): handlePermanentFailure installs the failed journal on disk BEFORE the abort fires", async () => {
    const { gitlab } = fakeGitlab();
    const outbox = await mkOutbox();
    const run = runner(new StubExecutor(nullLogger()), gitlab, undefined, {
      outbox,
      outboxTerminalMaxBytes: 1 << 20,
      gapFillMax: 100,
    });
    const runId = "run-d5-order";
    const gen = 6;
    const cancel = new AbortController();
    let journalOnDiskAtAbort: boolean | undefined;
    cancel.signal.addEventListener("abort", () => {
      // Snapshot at the exact moment the abort fires: the durable failed journal MUST already exist.
      journalOnDiskAtAbort = outbox.hasPendingTerminal(runId, gen);
    });
    // A minimal flight carrying only the fields handlePermanentFailure + journalAndSendTerminal read.
    // reportState answers a benign 409 running so the journal is KEPT (observably on disk at abort).
    const flight = {
      runId,
      claimGeneration: gen,
      batcher: { currentSeq: () => 5 },
      reportState: async () => ({ applied: false, status: "running" }),
      cancel,
      runLog: nullLogger(),
      terminalResolved: false,
    };
    await (run as unknown as { handlePermanentFailure(f: unknown, r: string): Promise<void> }).handlePermanentFailure(
      flight,
      "message persistence failed permanently: 401",
    );

    assert.equal(cancel.signal.aborted, true, "the attempt was aborted");
    assert.equal(journalOnDiskAtAbort, true, "the failed journal was durably installed BEFORE the abort (abort-first reads false)");
    assert.equal(flight.terminalResolved, true, "the terminal-resolved latch is set");
    assert.equal(outbox.hasPendingTerminal(runId, gen), true, "the 409 running kept the durable journal");
    const j = await outbox.readTerminalJournal(runId, gen);
    assert.equal(j?.body.status, "failed", "the journal holds the terminal failed state");
  });

  // PRD #1391 Run B M3 (N1): a report_only COMPLETED must be journalled WRITE-AHEAD, so an outage at
  // the finalize report keeps the exact report_only outcome (report_md) rather than degrading it into
  // a generic agent_failure. The completed report is refused 409 running so the journal stays on disk.
  it("PRD #1391 Run B M3 (N1): a report_only COMPLETED is journalled write-ahead (report_md survives an outage)", async () => {
    const { gitlab } = fakeGitlab();
    const outbox = await mkOutbox();
    const claim = gitlabClaim(13, { claim_generation: 8 });
    const summary = "verified: config already correct; no code change needed";
    const exec: Executor = { run: async (ctx) => ({ branch: ctx.branch, reportOnly: true, summary }) };
    api.failStateWhen(claim.run_id, (b) => b.status === "completed", { httpStatus: 409, runStatus: "running" });

    await runner(exec, gitlab, undefined, {
      outbox,
      outboxTerminalMaxBytes: 1 << 20,
      gapFillMax: 100,
    }).execute(claim);

    assert.equal(outbox.hasPendingTerminal(claim.run_id, 8), true, "the report_only completion is journalled write-ahead");
    const j = await outbox.readTerminalJournal(claim.run_id, 8);
    assert.equal(j?.body.status, "completed", "the journal holds the completed outcome");
    assert.equal(j?.body.report_only, true, "with report_only preserved (not degraded to a generic failure)");
    assert.equal(j?.body.report_md, summary, "and the exact report_md the owner needs");
  });

  // PRD #1391 Run B M3 (N2): when the permanent-failure hook's /state send succeeds (200) it RETIRES
  // the journal, so hasPendingTerminal then reads false. Without the terminalResolved latch,
  // reportGenericFailure would fall through and report a SECOND `failed`. This trips a REAL permanent
  // message-transport failure through execute() and asserts exactly ONE failed report is applied.
  it("PRD #1391 Run B M3 (N2): a permanent failure that resolves 200 does NOT produce a second failed", async () => {
    const { gitlab } = fakeGitlab();
    const outbox = await mkOutbox();
    const claim = gitlabClaim(12, { claim_generation: 7 });
    // Every /messages post fails 401 → the batcher trips permanently on its first flush, firing the
    // permanent-failure hook. The /state (failed) report is NOT refused, so it applies 200 and the
    // hook RETIRES the journal — the exact condition the N2 latch guards.
    api.failMessagesNext(100, 401);
    const exec: Executor = {
      run: async (ctx: RunContext): Promise<ExecutorResult> => {
        // Emit one message so the batcher posts, hits the 401, and trips; then wait for the trip's
        // abort and THROW (as a real SDK subprocess does on abort) so execute() unwinds into
        // reportGenericFailure — which must find the terminalResolved latch and report no 2nd failed.
        ctx.emit({ kind: "text", agent: "lead", payload: { text: "hello" } });
        await new Promise<void>((_resolve, reject) => {
          if (ctx.signal?.aborted) return reject(new Error("aborted"));
          ctx.signal?.addEventListener("abort", () => reject(new Error("aborted")), { once: true });
        });
        return { branch: ctx.branch };
      },
    };

    await runner(exec, gitlab, undefined, {
      outbox,
      outboxTerminalMaxBytes: 1 << 20,
      gapFillMax: 100,
    }).execute(claim);

    const failedReports = api.states.filter((s) => s.runId === claim.run_id && s.body.status === "failed");
    assert.equal(failedReports.length, 1, "exactly ONE failed report — the latch stops the second after the 200 retired the journal");
    assert.equal(outbox.hasPendingTerminal(claim.run_id, 7), false, "the failed journal was retired on the 200");
  });
});

import { afterEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import fsp from "node:fs/promises";
import os from "node:os";
import path from "node:path";

import { nullLogger } from "./helpers.js";
import { StubExecutor, type Executor, type ExecutorResult, type RunContext } from "../src/executor.js";
import { Outbox, type RawWriteSeam } from "../src/outbox.js";
import { api, fakeGitlab, gitlabClaim, installHarness, runner, simulateCommittedWork } from "./runner-harness.js";
import { commitInTree, fixture, makeRecoveryCoordinator, FakeRecoveryClient, FakeRecoveryGit } from "./codex-reap-fixture.js";

// PRD #1391 Run B M3b — the run-lane WRITE-AHEAD terminal send path, end to end through RunRunner.
// A real Outbox is threaded via RunnerOptions; the FakeApi refuses the terminal report with a
// benign 409 `running` (nothing changed server-side) so the write-ahead journal STAYS on disk and
// the test can prove it was journalled before the send, and that a non-terminal 409 keeps it.

installHarness();

const tmpRoots: string[] = [];
afterEach(async () => {
  for (const r of tmpRoots.splice(0)) await fsp.rm(r, { recursive: true, force: true }).catch(() => undefined);
});

async function mkOutbox(opts?: { rawWrite?: RawWriteSeam }): Promise<Outbox> {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "runner-journal-"));
  tmpRoots.push(dir);
  const outbox = new Outbox({
    root: path.join(dir, "outbox"),
    log: nullLogger(),
    runMaxBytes: 64 * 1024 * 1024,
    maxBytes: 512 * 1024 * 1024,
    retentionMs: 7 * 86_400_000,
    rawWrite: opts?.rawWrite,
  });
  await outbox.init();
  return outbox;
}

/** A rawWrite seam that fails EVERY terminal temp write with ENOSPC (even after the reserve is
 *  released), so `journalTerminal` returns `reserve_exhausted` while the store stays ENABLED — the
 *  path where journalAndSendTerminal falls back to an UNJOURNALED send. */
const enospcTerminalWrite: RawWriteSeam = async (write, ctx) => {
  if (ctx.kind === "terminal") {
    const err = new Error("no space left on device") as NodeJS.ErrnoException;
    err.code = "ENOSPC";
    throw err;
  }
  await write();
};

/** A minimal RunFlight-shaped object carrying only the fields handlePermanentFailure +
 *  journalAndSendTerminal + reapRecoveryProviderForSettle read. `events` records the abort, the reap
 *  (via a recording killAgentTree on the stub executor) and the send (via reportState), so a test can
 *  assert the abort < reap < send order the #1539 hook guarantees. */
function minimalFailFlight(opts: {
  runId: string;
  gen: number;
  events: string[];
  reportState: () => Promise<{ applied: boolean; status?: string; staleClaim?: boolean }>;
  safety?: unknown;
}) {
  const cancel = new AbortController();
  cancel.signal.addEventListener("abort", () => opts.events.push("abort"));
  return {
    runId: opts.runId,
    claimGeneration: opts.gen,
    // barePath present so reapRecoveryProviderForSettle passes its guard and reaps.
    barePath: "/tmp/does-not-matter-bare",
    batcher: { currentSeq: () => 5 },
    reportState: async (_b: unknown, _sig?: unknown) => {
      opts.events.push("send");
      return opts.reportState();
    },
    cancel,
    runLog: nullLogger(),
    terminalResolved: false,
    permanentFailureReap: undefined as boolean | undefined,
    permanentFailureReapSafety: undefined as unknown,
    executor: {
      safety: opts.safety,
      killAgentTree: () => opts.events.push("reap"),
    },
  };
}

/** A claim shaped only for handlePermanentFailure's reap (kind defaults to issue = code-publishing). */
const failClaim = (runId: string, gen: number) => gitlabClaim(1, { run_id: runId, claim_generation: gen });

/** Invoke the (private) handlePermanentFailure(claim, flight, reason) against the REAL shipping code. */
function callHandle(run: unknown, claim: unknown, flight: unknown, reason: string): Promise<void> {
  return (
    run as unknown as { handlePermanentFailure(c: unknown, f: unknown, r: string): Promise<void> }
  ).handlePermanentFailure(claim, flight, reason);
}

/** Read the (private) stale-epoch guard the settle consumers gate on. */
function reapValid(run: unknown, flight: unknown): boolean {
  return (run as unknown as { permanentFailureReapValid(f: unknown): boolean }).permanentFailureReapValid(flight);
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

  // PRD #1391 Run B M3 (D5) / #1539: the permanent-failure hook must (1) install the durable `failed`
  // journal, then (2) abort the attempt, then (3) reap this generation's provider WHILE still
  // actively-claimed, then (4) send — the order install → abort → reap → send. This drives the REAL
  // handlePermanentFailure(claim, flight, reason) and snapshots the on-disk journal at the abort AND
  // records the abort/reap/send order. Mutations: install-after-abort reddens journalOnDiskAtAbort;
  // reap-before-abort / reap-after-send (M-a) reddens the order assertion.
  it("PRD #1391 Run B M3 (D5/#1539): install BEFORE abort, then reap BEFORE send (abort < reap < send)", async () => {
    const { gitlab } = fakeGitlab();
    const outbox = await mkOutbox();
    const { coord } = makeRecoveryCoordinator(new FakeRecoveryClient(), new FakeRecoveryGit());
    const run = runner(new StubExecutor(nullLogger()), gitlab, undefined, {
      outbox,
      outboxTerminalMaxBytes: 1 << 20,
      gapFillMax: 100,
      recovery: coord,
    });
    const runId = "run-d5-order";
    const gen = 6;
    const events: string[] = [];
    let journalOnDiskAtAbort: boolean | undefined;
    // reportState answers a benign 409 running so the journal is KEPT (observably on disk at abort).
    const flight = minimalFailFlight({
      runId,
      gen,
      events,
      reportState: async () => ({ applied: false, status: "running" }),
    });
    flight.cancel.signal.addEventListener("abort", () => {
      // Snapshot at the exact moment the abort fires: the durable failed journal MUST already exist.
      journalOnDiskAtAbort = outbox.hasPendingTerminal(runId, gen);
    });
    await callHandle(run, failClaim(runId, gen), flight, "message persistence failed permanently: 401");

    assert.equal(flight.cancel.signal.aborted, true, "the attempt was aborted");
    assert.equal(journalOnDiskAtAbort, true, "the failed journal was durably installed BEFORE the abort (abort-first reads false)");
    assert.deepEqual(events, ["abort", "reap", "send"], `order must be abort < reap < send; got ${JSON.stringify(events)}`);
    assert.equal(flight.permanentFailureReap, true, "the reap confirmed and is recorded on the flight");
    assert.equal(flight.terminalResolved, true, "the terminal-resolved latch is set");
    assert.equal(outbox.hasPendingTerminal(runId, gen), true, "the 409 running kept the durable journal");
    const j = await outbox.readTerminalJournal(runId, gen);
    assert.equal(j?.body.status, "failed", "the journal holds the terminal failed state");
  });

  // #1539: no-outbox degradation — beforeResolve (abort + reap) still runs before the DIRECT send,
  // and exactly one failed is sent, in the order abort < reap < send.
  it("#1539: no-outbox permanent failure still runs abort < reap < send, exactly one failed", async () => {
    const { gitlab } = fakeGitlab();
    const { coord } = makeRecoveryCoordinator(new FakeRecoveryClient(), new FakeRecoveryGit());
    const run = runner(new StubExecutor(nullLogger()), gitlab, undefined, { recovery: coord }); // NO outbox
    const events: string[] = [];
    let sends = 0;
    const flight = minimalFailFlight({
      runId: "run-no-outbox",
      gen: 1,
      events,
      reportState: async () => {
        sends += 1;
        return { applied: true, status: "failed" };
      },
    });
    await callHandle(run, failClaim("run-no-outbox", 1), flight, "boom");
    assert.deepEqual(events, ["abort", "reap", "send"], `order must be abort < reap < send; got ${JSON.stringify(events)}`);
    assert.equal(sends, 1, "exactly one failed send on the no-outbox path");
    assert.equal(flight.permanentFailureReap, true, "the reap ran on the no-outbox path too");
    assert.equal(flight.terminalResolved, true, "the terminal-resolved latch is set");
  });

  // #1539: reserve-exhausted (the terminal-sized reserve cannot admit the journal) — the outcome
  // is sent UNJOURNALED, and beforeResolve (abort + reap) still runs before that send.
  it("#1539: reserve-exhausted permanent failure sends UNJOURNALED, abort < reap < send", async () => {
    const { gitlab } = fakeGitlab();
    const outbox = await mkOutbox({ rawWrite: enospcTerminalWrite });
    const { coord } = makeRecoveryCoordinator(new FakeRecoveryClient(), new FakeRecoveryGit());
    const run = runner(new StubExecutor(nullLogger()), gitlab, undefined, {
      outbox,
      outboxTerminalMaxBytes: 1 << 20,
      gapFillMax: 100,
      recovery: coord,
    });
    const events: string[] = [];
    let sends = 0;
    const flight = minimalFailFlight({
      runId: "run-reserve-exhausted",
      gen: 1,
      events,
      reportState: async () => {
        sends += 1;
        return { applied: true, status: "failed" };
      },
    });
    await callHandle(run, failClaim("run-reserve-exhausted", 1), flight, "boom");
    assert.deepEqual(events, ["abort", "reap", "send"], `order must be abort < reap < send; got ${JSON.stringify(events)}`);
    assert.equal(sends, 1, "exactly one (unjournaled) failed send");
    assert.equal(outbox.hasPendingTerminal("run-reserve-exhausted", 1), false, "reserve-exhausted: nothing was journaled");
    assert.equal(flight.terminalResolved, true, "the terminal-resolved latch is set after the unjournaled send");
  });

  // #1539 (test 6): permanent failure END TO END with the REAL Codex boundary + a real Outbox.
  // `api.failMessagesNext(100, 401)` trips the breaker on the first flush; the Codex-shaped executor
  // emits a message, awaits the abort, then throws. Asserts: exactly ONE `failed` applied; the reap
  // (reconcile) runs BEFORE the failed report and the committed-tree CAPTURE (reserve) AFTER it; and
  // exactly ONE reconcile — the already-journaled arm does NO second reap. Mutation M-a (reap after
  // the resolve, or restoring the old arm reap) reddens the ordering / the one-reconcile assertion.
  it("#1539 (6): real Codex boundary — reap BEFORE the failed report, one failed, one reconcile", async () => {
    const { gitlab } = fakeGitlab();
    const outbox = await mkOutbox();
    const events: string[] = [];
    const claim = gitlabClaim(1600, { claim_generation: 9 });
    // The 409 model: the reconcile throws ONCE a terminal `failed` has been recorded for the run.
    const decideThrow = (): boolean =>
      api.states.some((s) => s.runId === claim.run_id && s.body.status === "failed");
    const { coord, safety, root } = fixture(events, decideThrow);
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1539-e2e-home-"));
    try {
      api.failMessagesNext(100, 401); // every /messages fails → the batcher trips permanently on the first flush
      api.onState(claim.run_id, (body) => {
        if (body.status === "failed") events.push("failed");
      });
      const exec: Executor = {
        safety,
        run: async (ctx: RunContext): Promise<ExecutorResult> => {
          fs.mkdirSync(homeRoot, { recursive: true });
          // Commit work so the committed-tree custody settle CAPTURES (reserve) rather than releases.
          commitInTree(ctx.worktreePath, "WORK.txt", "committed before the codex failure\n");
          // Emit one message so the batcher posts, hits the 401 and trips; then wait for the trip's
          // abort and THROW (as a real SDK subprocess does on abort) so execute() unwinds into
          // reportGenericFailure.
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
        recovery: coord,
      }).execute(claim);

      const failedApplied = api.states.filter((s) => s.runId === claim.run_id && s.body.status === "failed");
      assert.equal(failedApplied.length, 1, "exactly ONE failed applied");
      assert.equal(events.filter((e) => e === "reconcile").length, 1, `exactly ONE reconcile (no second reap); got ${JSON.stringify(events)}`);
      assert.ok(events.indexOf("reconcile") >= 0 && events.indexOf("failed") >= 0, `both reconcile and failed happened; got ${JSON.stringify(events)}`);
      assert.ok(
        events.indexOf("reconcile") < events.indexOf("failed"),
        `the reap reconcile must precede the failed report; got ${JSON.stringify(events)}`,
      );
      assert.ok(
        events.indexOf("failed") < events.indexOf("reserve"),
        `the committed-tree capture (reserve) must run after the failed report; got ${JSON.stringify(events)}`,
      );
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
      fs.rmSync(root, { recursive: true, force: true });
    }
  });

  // #1539 (test 7b): stale epoch. The hook records the safety epoch it reaped; if CodexExecutor
  // then swaps `this.safety` (a checkpoint epoch swap), the settle guard must fail closed — no
  // release/reserve — while the one `failed` journal still stands. Chosen as a focused unit test over
  // the settle guard: orchestrating a deterministic mid-boundary checkpoint swap in an e2e run is
  // racy, whereas driving the REAL hook reap (recording the real safety epoch) and then swapping the
  // executor's safety exercises exactly the identity check the settle consumers gate on, and mutation
  // M-c (dropping the identity check) reddens the swapped-epoch assertion.
  it("#1539 (7b): a safety-epoch swap after the hook reaps makes the settle guard fail closed", async () => {
    const { gitlab } = fakeGitlab();
    const outbox = await mkOutbox();
    const events: string[] = [];
    const { coord, client, safety, root } = fixture(events, () => false); // the reap's reconcile SUCCEEDS
    try {
      const run = runner(new StubExecutor(nullLogger()), gitlab, undefined, {
        outbox,
        outboxTerminalMaxBytes: 1 << 20,
        gapFillMax: 100,
        recovery: coord,
      });
      const runId = "run-7b-stale";
      const gen = 3;
      const flight = minimalFailFlight({
        runId,
        gen,
        events,
        safety,
        reportState: async () => ({ applied: false, status: "running" }),
      });
      await callHandle(run, failClaim(runId, gen), flight, "boom");

      assert.equal(flight.permanentFailureReap, true, "the hook reaped through the real boundary");
      assert.equal(flight.permanentFailureReapSafety, safety, "and recorded the exact safety epoch it reaped");
      assert.equal(reapValid(run, flight), true, "same epoch → the settle is allowed");
      // CodexExecutor swaps this.safety after a checkpoint — the recorded epoch is now stale.
      flight.executor.safety = { epoch: "post-checkpoint" };
      assert.equal(reapValid(run, flight), false, "swapped epoch → the guard fails closed: no settle");
      assert.deepEqual(client.releaseCalls, [], "no release ran under the stale epoch");
      assert.deepEqual(client.reserveCalls, [], "no reserve ran under the stale epoch");
      assert.equal(outbox.hasPendingTerminal(runId, gen), true, "the one failed journal is still installed (kept on the 409)");
    } finally {
      fs.rmSync(root, { recursive: true, force: true });
    }
  });

  // #1539 (8, N4): when the drainer skipped a resolve while the hook held it, the hook's `finally`
  // release re-drives ONE resolve — so a terminal whose hook send did not retire it (a benign 409
  // running) is not stranded until boot. Here the first (hook) send answers 409 running AND records a
  // drainer skip (a concurrent drainer that skipped the held entry); the release then re-resolves, and
  // the second send applies 200 and retires the journal.
  it("#1539 (8/N4): a recorded drainer skip makes the hook re-resolve once on release", async () => {
    const { gitlab } = fakeGitlab();
    const outbox = await mkOutbox();
    const { coord } = makeRecoveryCoordinator(new FakeRecoveryClient(), new FakeRecoveryGit());
    const run = runner(new StubExecutor(nullLogger()), gitlab, undefined, {
      outbox,
      outboxTerminalMaxBytes: 1 << 20,
      gapFillMax: 100,
      recovery: coord,
    });
    const runId = "run-n4-reresolve";
    const gen = 4;
    const events: string[] = [];
    let sends = 0;
    const flight = minimalFailFlight({
      runId,
      gen,
      events,
      reportState: async () => {
        sends += 1;
        if (sends === 1) {
          // Model a concurrent drainer that skipped the held entry during the hook's own send.
          outbox.noteHeldSkip(runId, gen);
          return { applied: false, status: "running" }; // 409 running keeps the journal
        }
        return { applied: true, status: "failed" }; // the release-driven re-resolve lands
      },
    });
    await callHandle(run, failClaim(runId, gen), flight, "boom");

    assert.equal(sends, 2, "the hook sent once, then re-resolved once on release after the recorded skip");
    assert.equal(outbox.hasPendingTerminal(runId, gen), false, "the release-driven re-resolve retired the journal (no strand)");
  });

  // #1539 (hold coverage): the REAL handlePermanentFailure must TAKE the process-local resolve hold
  // (Outbox.holdTerminalResolve) BEFORE it installs the journal and keep it across the whole
  // abort → reap → send window, so the per-worker drainer (Worker.resolveRunTerminal) cannot send the
  // journaled `failed` and race the hook's own resolve; the `finally` must RELEASE it after. The
  // hold-during-window is observed from inside the recorded reap (a recording killAgentTree) and from
  // inside the send closure, so the observations survive even if an in-reap assertion were swallowed
  // by reapRecoveryProviderForSettle's catch. Mutations: delete `holdTerminalResolve` → the
  // held-during-reap/send assertions go RED; delete `releaseTerminalResolve` → the released-after
  // assertion goes RED (the hold is never dropped). A benign 409 running keeps the journal so the
  // hook's own resolve does not retire it — the hold's whole reason to exist.
  it("#1539: the real hook HOLDS the drainer's terminal resolve across abort→reap→send and RELEASES it after", async () => {
    const { gitlab } = fakeGitlab();
    const outbox = await mkOutbox();
    const { coord } = makeRecoveryCoordinator(new FakeRecoveryClient(), new FakeRecoveryGit());
    const run = runner(new StubExecutor(nullLogger()), gitlab, undefined, {
      outbox,
      outboxTerminalMaxBytes: 1 << 20,
      gapFillMax: 100,
      recovery: coord,
    });
    const runId = "run-1539-hold";
    const gen = 7;
    const events: string[] = [];
    let heldDuringSend: boolean | undefined;
    const flight = minimalFailFlight({
      runId,
      gen,
      events,
      // 409 running keeps the journal, so the hook's own resolve does not retire it.
      reportState: async () => {
        heldDuringSend = outbox.isTerminalResolveHeld(runId, gen);
        return { applied: false, status: "running" };
      },
    });
    // Record the hold state from inside the reap without asserting there (an assertion throw would be
    // swallowed by reapRecoveryProviderForSettle's catch, which only flips permanentFailureReap).
    let heldDuringReap: boolean | undefined;
    const realReap = flight.executor.killAgentTree;
    flight.executor.killAgentTree = () => {
      heldDuringReap = outbox.isTerminalResolveHeld(runId, gen);
      return realReap();
    };
    assert.equal(outbox.isTerminalResolveHeld(runId, gen), false, "unheld before the hook runs");
    await callHandle(run, failClaim(runId, gen), flight, "message persistence failed permanently: 401");

    assert.deepEqual(events, ["abort", "reap", "send"], `order must be abort < reap < send; got ${JSON.stringify(events)}`);
    assert.equal(heldDuringReap, true, "the resolve is HELD during the reap (delete holdTerminalResolve → RED)");
    assert.equal(heldDuringSend, true, "the resolve is HELD during the send");
    assert.equal(
      outbox.isTerminalResolveHeld(runId, gen),
      false,
      "the hold is RELEASED after the hook returns (delete releaseTerminalResolve → RED)",
    );
    assert.equal(outbox.hasPendingTerminal(runId, gen), true, "the 409 running kept the durable journal");
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

import { describe, it, mock } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { StubExecutor } from "../src/executor.js";
import { Outbox } from "../src/outbox.js";
import { nullLogger } from "./helpers.js";
import { api, fakeGitlab, fx, git, gitlabClaim, installHarness, runner } from "./runner-harness.js";
import {
  HELD_LINUX_ONLY, JOIN_TOKEN, PUBLICATION_ID, redrive, watchFreezes, assertCustodyRetained, bare, command, commit, failedStates, failingWork, frozenRecords,
  heldServer, packObjects, recoveryOf, skipCheckpoints, type Work,
} from "./runner-held-publication-fixture.js";

installHarness();

// Issue #2545 M5: a failed run's held work is published to the forge and released with a
// held_publication FINAL INSTEAD of an archive. The order is the point: the archive capture is
// deferred BEFORE the failed terminal is sent, the held step runs on that exact frozen record after
// the failed ACK, and the guarded hold stays open until the FINAL is ACKed.

describe("held publication of a failed run", () => {
  it("dispatch failure: publishes WIP and commits at the frozen record's tip, FINAL ACKed, no archive", { skip: HELD_LINUX_ONLY }, async () => {
    const claim = gitlabClaim(2545, { claim_generation: 41, inventory_guarded: true });
    const { gitlab } = fakeGitlab();
    const server = heldServer(claim);
    const freezes = watchFreezes(server);
    skipCheckpoints();
    const work: Partial<Work> = {};
    const r = runner(failingWork(work), gitlab, JOIN_TOKEN, { checkpointIntervalMs: 1 });
    const capture = mock.method(recoveryOf(r), "captureAndUpload");
    const order: string[] = [];
    server.onHeld = async call => {
      order.push("held");
      assert.equal(server.failedAcked(), true, "step A runs only after the failed terminal was ACKed");
      assert.equal(capture.mock.callCount(), 0, "the archive capture was deferred, not run");
      // Custody retention at the moment between the failed send and the held FINAL.
      const record = await assertCustodyRetained(server, claim, work, "at the held upload");
      assert.equal(call.tip, record.sourceSha, "the tip is the frozen record's source");
      assert.equal(call.coverage, record.coverageDigest);
      assert.equal(call.generation, 41);
      const roots = record.originalRoots!.map(x => x.sha);
      for (const root of [work.rootA!, work.rootB!]) assert.ok(roots.includes(root), "several roots are covered");
    };
    server.onFinal = async call => {
      order.push("final:" + call.disposition?.kind);
    };

    await r.execute(claim);

    // The freeze happened BEFORE the failed terminal was sent, and nothing froze again after it.
    assert.ok(freezes.failedAckedAtFreeze.length > 0, "an immutable coverage was built");
    assert.ok(freezes.failedAckedAtFreeze.every(sent => !sent), "every freeze precedes the failed send");

    // Step A: one upload, whose pack carries the commits AND the WIP.
    assert.equal(server.heldCalls.length, 1);
    const [call] = server.heldCalls;
    const objects = packObjects(call!.pack);
    for (const sha of [work.rootA!, work.rootB!, work.afterCheckpoint!, call!.tip]) {
      assert.ok(objects.has(sha), `the pack carries commit ${sha.slice(0, 12)}`);
    }
    for (const [file, blob] of Object.entries(work.wipBlobs!)) assert.ok(objects.has(blob), `the pack carries the WIP blob of ${file}`);

    // Exactly one frozen record per generation, and the archive never ran.
    const records = await frozenRecords(claim);
    assert.equal(records.length, 1, "exactly one recovery record per generation");
    assert.equal(call!.tip, records[0]!.sourceSha);
    assert.equal(capture.mock.callCount(), 0, "captureAndUpload was never called");
    assert.equal(server.archiveReserves(), 0);
    assert.equal(server.archiveUploads(), 0);
    assert.equal(failedStates(claim.run_id).length, 1, "the honest failed report is sent exactly once");

    // The FINAL is journaled in the flight but cannot be sent while the run still executes: the
    // custody stays exactly as retained until the live re-drive sends it.
    assert.deepEqual(order, ["held"]);
    const journaled = await assertCustodyRetained(server, claim, work, "after execute()");
    assert.deepEqual(journaled.finalRequest?.disposition, {
      kind: "held_publication", publication_id: PUBLICATION_ID, source_sha: journaled.sourceSha,
      coverage_digest: journaled.coverageDigest,
    });
    assert.equal(server.finals.length, 0, "no FINAL was sent from inside the flight");

    // The re-drive sends the journaled FINAL; the ACK closes the hold and cleans up the generation.
    await redrive(r);
    assert.equal(server.finals.length, 1);
    assert.deepEqual(server.finals[0]!.disposition, journaled.finalRequest!.disposition);
    assert.equal(server.finals[0]!.generation, 41);
    assert.equal(server.acked(), 1);
    assert.equal(server.holdOpen(), false);
    assert.equal(capture.mock.callCount(), 0, "the archive was still never captured");
    assert.deepEqual(order, ["held", "final:held_publication"]);
    const after = await frozenRecords(claim);
    // The ACK authorizes local cleanup: either the generation's journal is already cleaned up, or the one
    // record that remains is the very record frozen before the send, now acknowledged.
    assert.ok(after.length <= 1, "never a second record for the generation");
    for (const record of after) {
      assert.equal(record.finalAcknowledged, true);
      assert.equal(record.captureId, journaled.captureId);
    }
    assert.equal(server.archiveUploads(), 0);
    assert.ok(fs.existsSync(bare()));
    assert.equal(command(bare(), ["rev-parse", "--verify", `${call!.tip}^{commit}`]), call!.tip);
  });

  it("publish phase: a failed finalization is held, not archived, on the same record", { skip: HELD_LINUX_ONLY }, async () => {
    const claim = gitlabClaim(2546, { claim_generation: 42, inventory_guarded: true });
    const { gitlab } = fakeGitlab();
    const server = heldServer(claim);
    // The push is refused with a typed failure, so phasePublish reports `failed` and drives the
    // recovery terminal itself (driveRecoveryTerminal), not reportGenericFailure.
    git.pushBranch = (async () => { throw new Error("refusing to update workflow without workflow scope"); }) as typeof git.pushBranch;
    const work: Partial<Work> = {};
    const r = runner({ run: async ctx => {
      work.clone = ctx.worktreePath;
      work.rootA = commit(ctx.worktreePath, "published-work.txt");
      fs.writeFileSync(path.join(ctx.worktreePath, "wip.txt"), "wip\n");
      return { branch: ctx.branch };
    } }, gitlab, JOIN_TOKEN);
    const capture = mock.method(recoveryOf(r), "captureAndUpload");
    server.onHeld = async () => {
      assert.equal(server.failedAcked(), true);
      await assertCustodyRetained(server, claim, work, "publish phase, at the held upload");
      assert.equal(capture.mock.callCount(), 0);
    };
    await r.execute(claim);
    assert.equal(failedStates(claim.run_id).length, 1);
    assert.equal(failedStates(claim.run_id)[0]!.fail_origin, "workflow_scope_missing");
    assert.equal(server.heldCalls.length, 1, "the publish-phase drive ran the held step");
    assert.equal(capture.mock.callCount(), 0, "no archive capture on the held path");
    assert.equal((await frozenRecords(claim)).length, 1, "one record per generation");
    await assertCustodyRetained(server, claim, work, "publish phase, after execute()");
    await redrive(r);
    assert.equal(server.acked(), 1);
    assert.equal(server.finals[0]!.disposition?.kind, "held_publication");
  });

  it("permanent message failure: the hook's failed send is held, not archived", { skip: HELD_LINUX_ONLY }, async () => {
    const claim = gitlabClaim(2547, { claim_generation: 43, inventory_guarded: true });
    const { gitlab } = fakeGitlab();
    const server = heldServer(claim);
    skipCheckpoints();
    const work: Partial<Work> = {};
    // Once armed, every message post fails permanently, so the batcher trips handlePermanentFailure: it
    // journals `failed`, reaps, and sends it through journalAndSendTerminal. The executor then unwinds.
    // Armed from inside the executor, after failingWork has written its WIP: armed earlier, a start-up
    // post can trip the hook while the executor is still writing, so the freeze misses that WIP and the
    // source stays dirty, which the inventory boundary rightly refuses to release.
    const outbox = new Outbox({
      root: path.join(fx.dataDir, "outbox"), log: nullLogger(), runMaxBytes: 64 * 1024 * 1024,
      maxBytes: 512 * 1024 * 1024, retentionMs: 7 * 86_400_000,
    });
    await outbox.init();
    const r = runner(failingWork(work, async ctx => {
      api.failMessagesNext(100, 401);
      ctx.emit({ kind: "text", agent: "lead", payload: { text: "hello" } });
      await new Promise<void>((_resolve, reject) => {
        if (ctx.signal?.aborted) return reject(new Error("aborted"));
        ctx.signal?.addEventListener("abort", () => reject(new Error("aborted")), { once: true });
      });
    }), gitlab, JOIN_TOKEN, { checkpointIntervalMs: 1, outbox, outboxTerminalMaxBytes: 1 << 20, gapFillMax: 100 });
    const capture = mock.method(recoveryOf(r), "captureAndUpload");
    const permanent = mock.method(r as unknown as Record<string, (...a: unknown[]) => unknown>, "handlePermanentFailure");
    server.onHeld = async () => {
      assert.equal(server.failedAcked(), true);
      await assertCustodyRetained(server, claim, work, "permanent failure, at the held upload");
      assert.equal(capture.mock.callCount(), 0);
    };
    await r.execute(claim);
    assert.ok(permanent.mock.callCount() >= 1, "the permanent-failure hook ran");
    assert.equal(failedStates(claim.run_id).length, 1, "exactly one failed report");
    assert.equal(server.heldCalls.length, 1);
    assert.equal(capture.mock.callCount(), 0);
    assert.equal((await frozenRecords(claim)).length, 1);
    await assertCustodyRetained(server, claim, work, "permanent failure, after execute()");
    await redrive(r);
    assert.equal(server.acked(), 1);
    assert.equal(server.finals[0]!.disposition?.kind, "held_publication");
  });

  it("a completed run never defers and never publishes held work", async () => {
    const claim = gitlabClaim(2548, { claim_generation: 44, inventory_guarded: true });
    const { gitlab } = fakeGitlab();
    const server = heldServer(claim);
    const r = runner(new StubExecutor(nullLogger()), gitlab, JOIN_TOKEN);
    await r.execute(claim);
    assert.ok(api.states.some(s => s.runId === claim.run_id && s.body.status === "completed"));
    assert.equal(failedStates(claim.run_id).length, 0);
    assert.equal(server.heldCalls.length, 0, "a completed run never reaches step A");
    assert.equal(server.finals.filter(f => f.disposition?.kind === "held_publication").length, 0);
  });
});

import { describe, it, mock } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { WorkerClient } from "../src/client.js";
import type { RecoveryFinalDisposition } from "../src/protocol.js";
import { StubExecutor } from "../src/executor.js";
import { Outbox } from "../src/outbox.js";
import { nullLogger } from "./helpers.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, installHarness, runner } from "./runner-harness.js";
import {
  HELD_LINUX_ONLY, JOIN_TOKEN, PUBLICATION_ID, redrive, watchFreezes, assertCustodyRetained, bare, command, commit, failedStates, failingWork, frozenRecords,
  heldServer, packObjects, recoveryOf, skipCheckpoints, type Work,
} from "./runner-held-publication-fixture.js";

installHarness();

for (const answer of ["actual", "wrong", "missing"] as const) {
 it(`V2 lost FINAL ACK with advertisement off restarts through real client: ${answer}`, { skip: HELD_LINUX_ONLY }, async t => {
  const claim = gitlabClaim(30370, { claim_generation: 71, inventory_guarded: true });
  const features = ["recovery_inventory_v1", "recovery_held_publication_v1", "recovery_held_publication_v2"];
  api.setRegisterProtocolFeatures(features);
  const registration = await client.register("replay-test", undefined, undefined, undefined, ["recovery_held_publication_v2"]);
  const server = heldServer(claim, features);
  const holdId = "00000000-0000-4000-8000-000000000071";
  claim.repo.id = "00000000-0000-4000-8000-000000000072";
  client.listRecoveryHolds = async () => ({ run_id: claim.run_id, holds: server.holdOpen() ? [{
   hold_id: holdId, generation: 71, inventory_guarded: true, has_available_capture: false,
  }] : [] });
  const fakeRelease = client.releaseRecoveryCustody.bind(client);
  client.releaseRecoveryCustody = WorkerClient.prototype.releaseRecoveryCustody.bind(client);
  client.getExactRecoveryHold = WorkerClient.prototype.getExactRecoveryHold.bind(client);
  const hasFeature = client.hasFeature.bind(client);
  t.mock.method(client, "hasFeature", (name: string) => server.holdOpen() ? hasFeature(name) : name === "recovery_inventory_v1");
  let restarted = false;
  const bodies: unknown[] = [];
  let saved: RecoveryFinalDisposition | undefined;
  const fetch = globalThis.fetch;
  t.mock.method(globalThis, "fetch", async (input: Parameters<typeof fetch>[0], init?: Parameters<typeof fetch>[1]) => {
   const url = String(input);
   if (url.endsWith(`/runs/${claim.run_id}/recovery-holds?generation=71`)) {
    return Response.json({ run_id: claim.run_id, exact_hold: {
     run_id: claim.run_id, generation: 71, hold_id: holdId, repo_id: claim.repo.id, inventory_guarded: true,
     original_worker_id: registration.worker_id, original_worker_identity: "original",
     ...(server.holdOpen() ? { state: "open", live_worker_id: registration.worker_id, live_run_id: claim.run_id }
      : { state: "released", release_evidence: "held_publication", final_receipt: saved }),
     run_status: server.holdOpen() ? "failed" : "running", current_claim_generation: 72, claim_released: true,
    } });
   }
   if (!url.endsWith(`/runs/${claim.run_id}/archives/release`)) return fetch(input, init);
   const body = JSON.parse(String(init?.body));
   bodies.push(body);
   saved ??= body.final_disposition;
   const ack = await fakeRelease(claim.run_id, body.generation, body.release_evidence, body.final_disposition);
   if (!restarted) throw new Error("ACK lost after server commit");
   return Response.json(answer === "actual" ? ack : answer === "wrong" ? { ...ack, generation: 72 } : {});
  });
  skipCheckpoints();
  const r = runner(failingWork({}), fakeGitlab().gitlab, JOIN_TOKEN, { checkpointIntervalMs: 1 });
  await r.execute(claim);
  await redrive(r, 1);
  assert.equal(server.heldCalls.length, 1);
  assert.ok(bodies.length >= 1);
  const pending = await frozenRecords(claim);
  assert.equal(pending.length, 1);
  assert.notEqual(pending[0]!.finalAcknowledged, true, "GET and lost ACK do not authorize cleanup");
  restarted = true;
  const before = bodies.length;
  const r2 = runner({ run: async () => ({ branch: "x" }) }, fakeGitlab().gitlab, JOIN_TOKEN);
  await r2.resumePendingRecoveries(undefined, await r2.snapshotBootRecoveries());
  assert.equal(bodies.length, before + 1, "restart POST passes the real feature gate");
  for (const body of bodies) assert.deepEqual(body, bodies[0], "immutable FINAL replay");
  assert.equal(server.heldCalls.length, 1);
  assert.equal(server.archiveUploads(), 0);
  assert.equal(server.archiveReserves(), 0);
  const remaining = await frozenRecords(claim);
  if (answer === "actual") assert.ok(remaining.length === 0 || remaining[0]!.finalAcknowledged);
  else {
   assert.equal(remaining.length, 1);
   assert.notEqual(remaining[0]!.finalAcknowledged, true);
  }
 });
}

// Issue #2545 M5: a failed run's held work is published to the forge and released with a
// held_publication FINAL INSTEAD of an archive. The order is the point: the archive capture is
// deferred BEFORE the failed terminal is sent, the held step runs on that exact frozen record after
// the failed ACK, and the guarded hold stays open until the FINAL is ACKed.

it("M3 V2 failed body with cancelled terminal ACK publishes held work", { skip: HELD_LINUX_ONLY }, async () => {
  const claim = gitlabClaim(3030, { claim_generation: 71, inventory_guarded: true });
  const features = ["recovery_inventory_v1", "recovery_held_publication_v2"];
  api.setRegisterProtocolFeatures(features);
  await client.register("v2-test", undefined, undefined, undefined, ["recovery_held_publication_v2"]);
  assert.equal(client.hasHeldPublicationV2(), true);
  const server = heldServer(claim, features);
  const holdId = "00000000-0000-4000-8000-000000000071";
  const repoId = "00000000-0000-4000-8000-000000000072";
  claim.repo.id = repoId;
  client.listRecoveryHolds = async () => ({ run_id: claim.run_id, holds: [{
    hold_id: holdId, generation: 71, inventory_guarded: true, has_available_capture: false,
  }] });
  client.getExactRecoveryHold = async () => ({
    run_id: claim.run_id, generation: 71, hold_id: holdId, repo_id: repoId, inventory_guarded: true,
    original_worker_id: "original", original_worker_identity: "original",
    live_worker_id: "original", live_run_id: claim.run_id, state: "open",
    run_status: "cancelled", current_claim_generation: 71, claim_released: true,
  });
  const send = client.reportState.bind(client);
  mock.method(client, "reportState", async (...args: Parameters<typeof send>) => {
    const ack = await send(...args);
    return args[1].status === "failed" ? { ...ack, status: "cancelled" } : ack;
  });
  skipCheckpoints();
  const r = runner(failingWork({}), fakeGitlab().gitlab, JOIN_TOKEN, { checkpointIntervalMs: 1 });
  await r.execute(claim);
  assert.equal(failedStates(claim.run_id).length, 1);
  assert.equal(server.heldCalls.length, 1);
  assert.equal(server.heldCalls[0]!.generation, 71);
  const records = await frozenRecords(claim);
  assert.equal(records.length, 1);
  assert.equal(records[0]!.finalRequest?.disposition.kind, "held_publication");
  assert.equal(server.archiveReserves(), 0);
});

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

    // Every new frozen coverage identity precedes the failed send. Later FINAL
    // verification may rebuild the same aggregate without freezing another inventory.
    assert.ok(freezes.failedAckedAtFreeze.length > 0, "an immutable coverage was built");
    assert.ok(freezes.failedAckedAtFreeze.every(sent => !sent), "every new frozen coverage identity precedes the failed send");

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

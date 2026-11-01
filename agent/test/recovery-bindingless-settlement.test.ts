import assert from "node:assert/strict";
import fs from "node:fs/promises";
import { createHash } from "node:crypto";
import path from "node:path";
import { afterEach, test } from "node:test";
import { GitCache } from "../src/git.js";
import { canonicalJson, type RecoveryRecord } from "../src/recovery.js";
import { MaintenanceBudget } from "../src/maintenance-budget.js";
import { RunRunner } from "../src/runner.js";
import type { RecoveryExactHold } from "../src/protocol.js";
import { nullLogger } from "./helpers.js";
import { makeOutbox, stored } from "./publication-intent-fixture.js";
import { binding, closed, environment, fixture, git, inventory, patch, sourceProof,
  type Variant } from "./fixtures/recovery-bindingless-compat.js";

const roots: string[] = [];
afterEach(async () => { for (const root of roots.splice(0)) await fs.rm(root, { recursive: true, force: true }); });
async function setup(variant: Variant = "uploaded") {
  const root = await fs.mkdtemp(path.resolve("../.uzi/scratch/bindingless-settlement-"));
  roots.push(root);
  const f = await fixture(root, variant);
  const e = environment(f);
  const client = e.coordinator["client"], producer = e.coordinator["git"];
  let observations = 0, walks = 0, holds = 0;
  let answer: RecoveryExactHold | undefined = closed(f);
  client.getBindinglessRecoveryHold = async () => { observations++; return answer; };
  const list = client.listRecoveryHolds.bind(client);
  client.listRecoveryHolds = async (...args) => { holds++; return list(...args); };
  const enumerate = producer.enumerateOwedCandidates!.bind(producer);
  producer.enumerateOwedCandidates = async (...args) => { walks++; return enumerate(...args); };
  return { f, e, client, producer,
    counts: () => ({ observations, walks, holds }),
    answer: (value: RecoveryExactHold | undefined) => { answer = value; },
    input: () => ({ record: f.record, barePath: f.context.barePath, defaultBranch: "main" }),
  };
}

for (const variant of ["pinned", "needs_action", "uploaded", "saved-final"] as const) {
  test(`closed bindingless ${variant} persists refusal and filters later live discovery across restart`, async () => {
    const s = await setup(variant), { f, e, producer } = s;
    let discovery = 0;
    producer.discoverOwedCandidates = async skip => {
      discovery++;
      if (await skip?.(f.context)) return [];
      await producer.enumerateOwedCandidates!(f.context.barePath, f.context.runId);
      return [{ context: f.context, candidates: [] }];
    };
    assert.equal(await e.coordinator.heldOperationState(f.record), "settled");
    const before = await inventory(f.root);
    await e.coordinator.resumeLive({ authenticatedAtMs: 1000, isExecuting: () => false });
    const saved = (await e.coordinator.inspect(f.context.runId))[0]!;
    assert.deepEqual(saved.exactHoldBinding, binding);
    assert.deepEqual(saved.holdSettlement, closed(f));
    assert.equal(saved.heldPolicy, undefined);
    assert.deepEqual(saved.finalRequest, f.record.finalRequest);
    assert.equal(saved.finalAcknowledged, f.record.finalAcknowledged);
    assert.deepEqual(s.counts(), { observations: 1, walks: 0, holds: 0 });
    assert.deepEqual(e.calls, { produce: 0, reserve: 0, upload: 0, final: 0, held: 0, cleanup: 0 });
    assert.equal(discovery, 1);
    const after = await inventory(f.root);
    assert.equal(after.length, before.length);
    const journal = path.relative(f.root, e.coordinator["recordPath"](f.record));
    for (let i = 0; i < before.length; i++) {
      const actual = { ...after[i] };
      if (actual.path === journal) actual.bytes = before[i]!.bytes;
      assert.ok(JSON.stringify(actual) === JSON.stringify(before[i]), `first retained-tree mismatch: ${before[i]!.path}`);
    }
    await sourceProof(f);
    const restarted = environment(f);
    restarted.coordinator["git"].discoverOwedCandidates = producer.discoverOwedCandidates;
    restarted.coordinator["client"].getExactRecoveryHold = async () => { throw new Error("durable closure needs no observation"); };
    restarted.coordinator["client"].getBindinglessRecoveryHold = async () => { throw new Error("durable closure needs no bindingless observation"); };
    await restarted.coordinator.snapshotOwedInventory();
    await restarted.coordinator.resumePending();
    await restarted.coordinator.captureAndUpload(s.input());
    await restarted.coordinator.operateFrozenInventory(s.input());
    assert.equal(await restarted.coordinator.freezeInventory({ context: f.context, currentSha: f.head,
      defaultBranch: "main", repoId: binding.repoId }), undefined, "direct freeze cannot rebuild settled custody");
    assert.equal(s.counts().walks, 0);
    assert.equal(s.counts().holds, 0);
    assert.deepEqual(restarted.calls, { produce: 0, reserve: 0, upload: 0, final: 0, held: 0, cleanup: 0 });
    await sourceProof(f);
  });
}

for (const answer of ["unknown", "open", "bound"] as const) {
  test(`${answer} does not learn binding or held policy`, async () => {
    const s = await setup("pinned");
    if (answer === "bound") await patch(s.e.coordinator, s.f.record, { exactHoldBinding: binding });
    s.answer(answer === "unknown" ? undefined : { ...closed(s.f), state: "open", claim_released: false, release_evidence: undefined });
    const before = (await s.e.coordinator.inspect(s.f.context.runId))[0]!;
    await s.e.coordinator["withCancellationAttempt"](before, async () => {
      await s.e.coordinator.heldOperationState(before);
      await s.e.coordinator.heldOperationState(before);
    });
    const after = (await s.e.coordinator.inspect(s.f.context.runId))[0]!;
    assert.deepEqual(after, before);
    assert.equal(s.counts().observations, answer === "bound" ? 0 : 1);
    assert.equal(after.heldPolicy, undefined);
  });
}

for (const failure of ["write", "refused", "identity"] as const) {
  test(`closed observation ${failure} cannot report nondurable settlement`, async () => {
    const s = await setup();
    if (failure === "identity") s.client.getBindinglessRecoveryHold = async () => {
      await patch(s.e.coordinator, s.f.record, { checksum: "a".repeat(64) });
      return closed(s.f);
    };
    else {
      const write = s.e.coordinator["writeExistingRecord"].bind(s.e.coordinator);
      Object.assign(s.e.coordinator, { writeExistingRecord: async (...args: Parameters<typeof write>) => {
        if (failure === "write") throw new Error("fixture journal unavailable");
        return write(args[0], () => undefined);
      } });
    }
    assert.equal(await s.e.coordinator.heldOperationState(s.f.record), "unknown");
    const saved = (await s.e.coordinator.inspect(s.f.context.runId))[0]!;
    assert.equal(saved.holdSettlement, undefined);
    assert.equal(saved.exactHoldBinding, undefined);
  });
}

test("first live pass discovers before learning closure and does no later source work", async () => {
  const s = await setup("pinned"), { f, e, producer } = s;
  let discovery = 0;
  producer.discoverOwedCandidates = async skip => {
    discovery++;
    if (!await skip?.(f.context)) await producer.enumerateOwedCandidates!(f.context.barePath, f.context.runId);
    return [];
  };
  let observations = 0;
  let atObservation: ReturnType<typeof s.counts> | undefined;
  let effectsAtObservation: typeof e.calls | undefined;
  s.client.getBindinglessRecoveryHold = async () => {
    observations++;
    atObservation = s.counts();
    effectsAtObservation = { ...e.calls };
    return closed(f);
  };
  await e.coordinator.resumeLive({ authenticatedAtMs: 1000, isExecuting: () => false });
  assert.deepEqual(atObservation, { observations: 0, walks: 1, holds: 0 });
  assert.deepEqual(s.counts(), atObservation);
  assert.deepEqual(e.calls, effectsAtObservation);
  assert.deepEqual(e.calls, { produce: 0, reserve: 0, upload: 0, final: 0, held: 0, cleanup: 0 });
  assert.deepEqual((await e.coordinator.inspect(f.context.runId))[0]!.holdSettlement, closed(f));
  assert.equal(e.coordinator["lastAttemptAt"].get(f.record.captureId), 1000);
  Object.assign(e.coordinator, { nextPassAt: 0 });
  await e.coordinator.resumeLive({ authenticatedAtMs: 1000, isExecuting: () => false });
  await e.coordinator.captureAndUpload(s.input());
  await e.coordinator.operateFrozenInventory(s.input());
  assert.equal(discovery, 2);
  assert.equal(observations, 1);
  assert.deepEqual(s.counts(), atObservation);
  assert.deepEqual(e.calls, effectsAtObservation);
  await sourceProof(f);
});

test("cap one never observes a deferred record or refills after learning closure", async () => {
  const s = await setup("pinned");
  Object.assign(s.e.coordinator, { liveMaxPerPass: 1 });
  const eligible = { ...s.f.record, captureId: "31050000-0000-4000-8000-000000000087" };
  const peer = { ...s.f.record, captureId: "31050000-0000-4000-8000-000000000088" };
  await patch(s.e.coordinator, s.f.record, { reason: "inventory_snapshot_changed" });
  await s.e.coordinator["withJournalLock"](eligible.runId, () => s.e.coordinator["writeRecordUnlocked"](eligible));
  await s.e.coordinator["withJournalLock"](peer.runId, () => s.e.coordinator["writeRecordUnlocked"](peer));
  const observed: string[] = [];
  const held = s.e.coordinator.heldOperationState.bind(s.e.coordinator);
  s.e.coordinator.heldOperationState = async (...args) => {
    observed.push(args[0].captureId);
    return held(...args);
  };
  await s.e.coordinator.resumeLive({ authenticatedAtMs: 1000, isExecuting: () => false });
  assert.deepEqual(observed, [eligible.captureId]);
  assert.equal(s.counts().observations, 1);
  assert.equal(s.e.coordinator["lastAttemptAt"].get(eligible.captureId), 1000);
  assert.equal(s.e.coordinator["lastAttemptAt"].has(peer.captureId), false, "closure does not refill its consumed slot");
  assert.equal(s.e.coordinator["lastAttemptAt"].has(s.f.record.captureId), false);
  Object.assign(s.e.coordinator, { nextPassAt: 0 });
  await s.e.coordinator.resumeLive({ authenticatedAtMs: 1000, isExecuting: () => false });
  assert.deepEqual(observed, [eligible.captureId, peer.captureId], "the next pass admits the waiting peer");
  assert.equal(s.e.coordinator["lastAttemptAt"].get(peer.captureId), 1000);
  const saved = await s.e.coordinator.inspect(s.f.context.runId);
  assert.equal(saved.find(r => r.captureId === s.f.record.captureId)!.holdSettlement, undefined);
  assert.deepEqual(s.e.calls, { produce: 0, reserve: 0, upload: 0, final: 0, held: 0, cleanup: 0 });
});

for (const [variant, fenced] of [["pinned", false], ["completion-receipt", false], ["completion-receipt", true]] as const) {
  test(`runner cancellation skips durable closure probes without starving ${variant} closeout (fenced=${fenced})`, async () => {
    const s = await setup(variant);
    await patch(s.e.coordinator, s.f.record, { exactHoldBinding: binding, holdSettlement: closed(s.f),
      ...(variant === "completion-receipt" ? { state: "needs_action" as const, reason: "upload_transient" } : {}) });
    let probes = 0, captures = 0;
    const instance = Object.create(RunRunner.prototype) as RunRunner;
    Object.assign(instance, {
      recovery: s.e.coordinator,
      client: { hasHeldPublicationV2: () => true, getExactRecoveryHold: async () => { probes++; return closed(s.f); } },
      isCredentialFreeOwnerCancel: () => true,
      captureFrozenInventory: async () => { captures++; },
    });
    s.e.coordinator.inventoryCleanupState = async () => "pending";
    const flight = { heldDeferredCapture: s.f.record.captureId, preserveRecoveryClone: false,
      steering: { claimFence: () => fenced ? { kind: "stale" } : undefined }, runLog: nullLogger() };
    await instance["settleHeldDeferred"](
      { run_id: s.f.context.runId, claim_generation: 7 } as Parameters<RunRunner["settleHeldDeferred"]>[0],
      flight as unknown as Parameters<RunRunner["settleHeldDeferred"]>[1]);
    assert.equal(probes, 0, "persisted custody needs no redundant runner probe");
    assert.equal(captures, variant === "completion-receipt" && !fenced ? 1 : 0, "existing claim fence still refuses");
    assert.equal(flight.preserveRecoveryClone, variant === "pinned" || !fenced);
    assert.deepEqual(s.e.calls, { produce: 0, reserve: 0, upload: 0, final: 0, held: 0, cleanup: 0 });
    await sourceProof(s.f);
  });
}

test("caller executing filter prevents bindingless observation before live admission", async () => {
  const s = await setup("pinned");
  Object.assign(s.e.coordinator, { liveMaxPerPass: 1 });
  await s.e.coordinator.resumeLive({ authenticatedAtMs: 1000, isExecuting: () => true });
  assert.deepEqual(s.counts(), { observations: 0, walks: 0, holds: 0 });
  assert.equal(s.e.coordinator["lastAttemptAt"].has(s.f.record.captureId), false);
  assert.equal((await s.e.coordinator.inspect(s.f.context.runId))[0]!.holdSettlement, undefined);
});

test("unknown observation exceptions preserve legacy open behavior with one observer per attempt", async () => {
  const s = await setup("pinned");
  let reads = 0;
  s.client.getBindinglessRecoveryHold = async () => { reads++; throw new Error("unavailable"); };
  await s.e.coordinator["withCancellationAttempt"](s.f.record, async () => {
    assert.equal(await s.e.coordinator.heldOperationState(s.f.record), "open");
    assert.equal(await s.e.coordinator.heldOperationState(s.f.record), "open");
  });
  assert.equal(reads, 1);
  assert.equal((await s.e.coordinator.inspect(s.f.context.runId))[0]!.exactHoldBinding, undefined);
});

test("one observation budget cannot settle an unobserved sibling in the same generation", async () => {
  const s = await setup("pinned");
  const sibling = { ...s.f.record, captureId: "31050000-0000-4000-8000-000000000088" };
  await s.e.coordinator["withJournalLock"](sibling.runId, () => s.e.coordinator["writeRecordUnlocked"](sibling));
  await s.e.coordinator["withCancellationAttempt"](s.f.record, async () => {
    assert.equal(await s.e.coordinator.heldOperationState(s.f.record), "settled");
    assert.equal(await s.e.coordinator.heldOperationState(sibling), "unknown");
  });
  assert.equal(s.counts().observations, 1);
  assert.equal((await s.e.coordinator.inspect(s.f.context.runId)).find(r => r.captureId === sibling.captureId)!.holdSettlement, undefined);
});

test("bound closed write refusal cannot falsely return settled", async () => {
  const s = await setup("pinned");
  const bound = await patch(s.e.coordinator, s.f.record, { exactHoldBinding: binding });
  s.client.getExactRecoveryHold = async () => closed(s.f);
  const write = s.e.coordinator["writeExistingRecord"].bind(s.e.coordinator);
  Object.assign(s.e.coordinator, { writeExistingRecord: (...args: Parameters<typeof write>) => write(args[0], () => undefined) });
  assert.equal(await s.e.coordinator.heldOperationState(bound), "unknown");
  assert.equal((await s.e.coordinator.inspect(s.f.context.runId))[0]!.holdSettlement, undefined);
});

test("closed producer bootstrap never consults the new observer or backfills policy", async () => {
  const s = await setup("pinned");
  s.e.state.open = false;
  assert.equal(await s.e.coordinator.freezeInventory({ context: s.f.context, currentSha: s.f.head, defaultBranch: "main" }), undefined);
  assert.equal(s.counts().observations, 0);
  const saved = (await s.e.coordinator.inspect(s.f.context.runId))[0]!;
  assert.equal(saved.exactHoldBinding, undefined);
  assert.equal(saved.heldPolicy, undefined);
});

test("concurrent FINAL ACK and completion fields survive atomic settlement learning", async () => {
  const s = await setup("completion-receipt");
  const receipt = s.f.record.completionReceipt!;
  s.client.getBindinglessRecoveryHold = async () => {
    await patch(s.e.coordinator, s.f.record, {
      finalRequest: { disposition: { kind: "archive", capture_id: s.f.record.serverCaptureId!,
        source_sha: s.f.record.sourceSha, coverage_digest: s.f.record.coverageDigest! }, evidence: "publication" },
      finalAcknowledged: true, completionReceipt: receipt,
    });
    return closed(s.f);
  };
  assert.equal(await s.e.coordinator.heldOperationState(s.f.record), "settled");
  const saved = (await s.e.coordinator.inspect(s.f.context.runId))[0]!;
  assert.equal(saved.finalAcknowledged, true);
  assert.equal(saved.finalRequest?.evidence, "publication");
  assert.deepEqual(saved.completionReceipt, receipt);
  assert.deepEqual(saved.exactHoldBinding, binding);
  assert.deepEqual(saved.holdSettlement, closed(s.f));
});

for (const disposition of ["discarded", "mismatch", "matching"] as const) {
  test(`${disposition} closed receipt controls immutable replay`, async () => {
    const s = await setup("saved-final");
    const request = s.f.record.finalRequest!;
    const proof: RecoveryExactHold = { ...closed(s.f), state: disposition === "discarded" ? "discarded" : "released",
      release_evidence: request.evidence, final_receipt: disposition === "mismatch"
        ? { kind: "settled", coverage_digest: s.f.record.coverageDigest! } : request.disposition };
    s.answer(proof);
    s.e.state.allowFinal = true;
    const sent: string[] = [];
    const release = s.client.releaseRecoveryCustody.bind(s.client);
    s.client.releaseRecoveryCustody = async (...args) => {
      sent.push(canonicalJson({ disposition: args[3], ...(args[2] ? { evidence: args[2] } : {}) }));
      return release(...args);
    };
    await s.e.coordinator.resumePending();
    assert.equal(sent.length, disposition === "matching" ? 1 : 0);
    if (disposition === "matching") assert.equal(sent[0], canonicalJson(request));
    assert.equal(s.e.calls.produce + s.e.calls.reserve + s.e.calls.upload + s.e.calls.held, 0);
    await sourceProof(s.f);
  });
}

test("matching released thin FINAL replays before unavailable capture status", async () => {
  const s = await setup("saved-final"), { f } = s;
  const base = f.context.defaultIdentity!.sha;
  git(f.context.barePath, "update-ref", "refs/heads/recovered-source", f.record.sourceSha);
  const bundle = path.join(f.root, "thin.bundle");
  git(f.context.barePath, "bundle", "create", bundle, "refs/heads/recovered-source", "^" + base);
  const bytes = await fs.readFile(bundle);
  f.record = await patch(s.e.coordinator, f.record, { bundlePath: bundle, byteSize: bytes.length,
    checksum: createHash("sha256").update(bytes).digest("hex"), chunkCount: 1,
    selfContained: false, prerequisiteShas: [base] });
  s.answer({ ...closed(f), state: "released", final_receipt: f.record.finalRequest!.disposition,
    release_evidence: f.record.finalRequest!.evidence });
  let statuses = 0;
  s.client.getRecoveryCaptureStatus = async () => { statuses++; throw new Error("capture unavailable"); };
  const sent: string[] = [];
  const release = s.client.releaseRecoveryCustody.bind(s.client);
  s.client.releaseRecoveryCustody = async (...args) => {
    sent.push(canonicalJson({ disposition: args[3], ...(args[2] ? { evidence: args[2] } : {}) }));
    return release(...args);
  };
  s.e.state.allowFinal = true;
  await s.e.coordinator.resumePending();
  assert.equal(statuses, 0);
  assert.deepEqual(sent, [canonicalJson(f.record.finalRequest)]);
  assert.equal(s.counts().observations, 1);
  assert.equal(s.e.calls.reserve + s.e.calls.upload + s.e.calls.produce, 0);
});

for (const variant of ["acknowledged", "completion-receipt"] as const) {
  test(`independent ${variant} closeout remains eligible under saved discard`, async () => {
    const s = await setup(variant);
    await patch(s.e.coordinator, s.f.record, { exactHoldBinding: binding, holdSettlement: closed(s.f) });
    s.e.cleanup();
    await s.e.coordinator.resumeLive({ authenticatedAtMs: 1000, isExecuting: () => false });
    assert.ok(s.e.calls.cleanup > 0, "existing independent authority still reaches guarded cleanup");
    assert.equal(s.e.calls.final + s.e.calls.produce + s.e.calls.reserve + s.e.calls.upload + s.e.calls.held, 0);
    assert.equal(s.counts().observations, 0);
  });
}

test("bound discarded receipt always refuses even when it matches the saved request", async () => {
  const s = await setup("saved-final");
  await patch(s.e.coordinator, s.f.record, { exactHoldBinding: binding,
    holdSettlement: { ...closed(s.f), final_receipt: s.f.record.finalRequest!.disposition } });
  await s.e.coordinator.resumePending();
  assert.equal(s.e.calls.final, 0);
});

test("cancellation observation uses the existing single budget and cannot repeat the observer", async () => {
  const s = await setup("pinned");
  let budgets = 0;
  Object.assign(s.e.coordinator, { testHooks: { maintenanceBudget: () => { budgets++; return new MaintenanceBudget(); } } });
  s.answer({ ...closed(s.f), run_status: "cancelled" });
  assert.equal(await s.e.coordinator.heldOperationState(s.f.record), "settled");
  assert.equal(budgets, 1);
  assert.equal(s.counts().observations, 1);
  await s.e.coordinator.heldOperationState(s.f.record);
  assert.equal(budgets, 1);
  assert.equal(s.counts().observations, 1);
});

test("learning a matching binding preserves an existing immutable FINAL backoff", async () => {
  const s = await setup("saved-final");
  let now = 1000;
  Object.assign(s.e.coordinator, { now: () => now });
  s.answer(undefined);
  await s.e.coordinator.resumePending();
  assert.equal(s.e.calls.final, 1);
  s.answer({ ...closed(s.f), state: "released", final_receipt: s.f.record.finalRequest!.disposition,
    release_evidence: s.f.record.finalRequest!.evidence });
  assert.equal(await s.e.coordinator.heldOperationState(s.f.record), "settled");
  await s.e.coordinator.resumePending();
  assert.equal(s.e.calls.final, 1, "learning binding must not reset the saved request's attempt clock");
  now += 900_000;
  await s.e.coordinator.resumePending();
  assert.equal(s.e.calls.final, 2);
});

test("expired cancellation budget prevents settlement writes and capture effects", async () => {
  const s = await setup("pinned");
  let mono = 0;
  const budget = new MaintenanceBudget(10, () => mono);
  Object.assign(s.e.coordinator, { testHooks: { maintenanceBudget: () => budget } });
  s.client.getBindinglessRecoveryHold = async () => {
    mono = 10;
    return { ...closed(s.f), run_status: "cancelled" };
  };
  try {
    await assert.rejects(s.e.coordinator.captureAndUpload(s.input()), /maintenance deadline/);
    const saved = (await s.e.coordinator.inspect(s.f.context.runId))[0]!;
    assert.equal(saved.exactHoldBinding, undefined);
    assert.equal(saved.holdSettlement, undefined);
    assert.deepEqual(s.e.calls, { produce: 0, reserve: 0, upload: 0, final: 0, held: 0, cleanup: 0 });
  } finally { budget.dispose(); }
});

test("real Git discovery skips only settled contexts and retains metadata siblings and temps", async () => {
  const s = await setup("pinned"), { f } = s;
  const cache = new GitCache(path.join(f.root, "data"), nullLogger());
  // Use the existing writer to create authentic Git metadata; discovery remains public.
  await cache["pinOwedUnderLock"](f.context, f.head);
  const sibling = { ...f.context, generation: 8, branch: "agent/sibling" };
  await cache["pinOwedUnderLock"](sibling, f.attempts[0]!);
  const meta = path.join(f.context.barePath, "uzi-owed");
  await fs.writeFile(path.join(meta, ".tmp-31050000-0000-4000-8000-000000000098"), "pending");
  let walks = 0;
  const enumerate = cache.enumerateOwedUnderLock.bind(cache);
  cache.enumerateOwedUnderLock = async (...args) => { walks++; return enumerate(...args); };
  s.producer.discoverOwedCandidates = cache.discoverOwedCandidates.bind(cache);
  assert.equal(await s.e.coordinator.heldOperationState(f.record), "settled");
  const before = await inventory(f.context.barePath);
  await s.e.coordinator.snapshotOwedInventory();
  assert.equal(walks, 1, "unsettled sibling still enumerates its run");
  assert.deepEqual(s.e.coordinator["bootOwed"].map(x => x.context.generation), [8]);
  assert.deepEqual(await inventory(f.context.barePath), before);
  assert.equal(git(f.context.barePath, "rev-parse", "refs/owed/fixture"), f.head);
});

test("real Git solely settled discovery avoids source enumeration but still rejects corrupt metadata", async () => {
  const s = await setup("pinned"), { f } = s;
  const cache = new GitCache(path.join(f.root, "data"), nullLogger());
  await cache["pinOwedUnderLock"](f.context, f.head);
  let walks = 0;
  cache.enumerateOwedUnderLock = async () => { walks++; throw new Error("settled source walked"); };
  s.producer.discoverOwedCandidates = cache.discoverOwedCandidates.bind(cache);
  assert.equal(await s.e.coordinator.heldOperationState(f.record), "settled");
  const before = await inventory(f.context.barePath);
  await s.e.coordinator.snapshotOwedInventory();
  await s.e.coordinator.materializeBootInventory();
  assert.deepEqual(s.e.coordinator["bootOwed"], []);
  assert.equal(walks, 0);
  assert.equal(s.counts().holds, 0);
  assert.deepEqual(await inventory(f.context.barePath), before);
  await fs.writeFile(path.join(f.context.barePath, "uzi-owed", "untrusted-entry"), "invalid metadata");
  await assert.rejects(s.e.coordinator.snapshotOwedInventory(), /unknown owed metadata entry/);
  assert.equal(walks, 0);
  await sourceProof(f);
});

for (const corruption of ["target", "blob", "tree", "missing-metadata"] as const) {
  test(`fully skipped real Git run rejects ${corruption} pin without enumeration`, async () => {
    const s = await setup("pinned"), { f } = s;
    const cache = new GitCache(path.join(f.root, "data"), nullLogger());
    await cache["pinOwedUnderLock"](f.context, f.head);
    assert.equal(await s.e.coordinator.heldOperationState(f.record), "settled");
    let walks = 0;
    cache.enumerateOwedUnderLock = async () => { walks++; throw new Error("unexpected enumeration"); };
    s.producer.discoverOwedCandidates = cache.discoverOwedCandidates.bind(cache);
    await s.e.coordinator.snapshotOwedInventory();
    assert.equal(walks, 0, "valid skipped control");
    const ref = `refs/uzi-owed/${f.context.runId}/${f.head}`;
    if (corruption === "target") {
      git(f.context.barePath, "update-ref", ref, f.attempts[0]!);
    } else if (corruption === "missing-metadata") {
      const dir = path.join(f.context.barePath, "uzi-owed");
      const names = (await fs.readdir(dir)).filter(n => n.startsWith("candidate-"));
      assert.equal(names.length, 1);
      await fs.unlink(path.join(dir, names[0]!));
    } else {
      const sha = git(f.context.barePath, "rev-parse", corruption === "tree" ? f.head + "^{tree}" : f.head + ":retained.txt");
      git(f.context.barePath, "update-ref", "-d", ref);
      git(f.context.barePath, "update-ref", `refs/uzi-owed/${f.context.runId}/${sha}`, sha);
    }
    await assert.rejects(s.e.coordinator.snapshotOwedInventory(),
      corruption === "target" ? /malformed owed pin/ : corruption === "missing-metadata"
        ? /owed pin has no discovery metadata/ : /owed candidate is not an actual lowercase commit OID/);
    assert.equal(walks, 0);
    await sourceProof(f);
  });
}

test("settlement retains actual unacknowledged outbox publication and crash-left temp protection", async () => {
  const s = await setup("pinned"), { f } = s;
  const root = path.join(f.root, "data", "outbox");
  const outbox = makeOutbox(root, { retentionMs: 10, now: () => 1000 });
  await outbox.init();
  const record = stored({ run_id: f.context.runId, claim_generation: 7, branch: f.context.branch,
    original_head: f.head, candidate: f.attempts[0]! });
  assert.equal((await outbox.savePublicationRecord(record)).ok, true);
  const runDir = path.join(root, f.context.runId);
  const temp = path.join(runDir, "publication-7.json.31050000-0000-4000-8000-000000000086.tmp");
  await fs.copyFile(path.join(runDir, "publication-7.json"), temp);
  const before = await inventory(root);
  assert.equal(await s.e.coordinator.heldOperationState(f.record), "settled");
  await s.e.coordinator.resumeLive({ authenticatedAtMs: 1000, isExecuting: () => false });
  await s.e.coordinator.captureAndUpload(s.input());
  await s.e.coordinator.operateFrozenInventory(s.input());
  await outbox.sweepRetention(1_000_000);
  await outbox.retireRun(f.context.runId);
  assert.equal(await outbox.hasPhysicalTerminalProtection(f.context.runId), true);
  const loaded = await outbox.loadPublicationRecord(f.context.runId, 7);
  assert.deepEqual(loaded, { kind: "ok", record });
  const after = await inventory(root);
  assert.equal(after.length, before.length);
  for (let i = 0; i < before.length; i++) {
    assert.ok(JSON.stringify(after[i]) === JSON.stringify(before[i]), `first outbox mismatch: ${before[i]!.path}`);
  }
  await fs.access(temp);
  const tempRoot = path.join(f.root, "temp-only-outbox");
  const tempOutbox = makeOutbox(tempRoot);
  await tempOutbox.init();
  const tempRun = path.join(tempRoot, f.context.runId);
  await fs.mkdir(tempRun, { mode: 0o700 });
  const tempOnly = path.join(tempRun, path.basename(temp));
  await fs.copyFile(temp, tempOnly);
  assert.equal(await tempOutbox.hasPhysicalTerminalProtection(f.context.runId), true, "temp independently protects");
  await tempOutbox.retireRun(f.context.runId);
  await fs.access(tempOnly);
  await sourceProof(f);
});

test("corrupt sibling journal prevents context suppression and retains physical sources", async () => {
  const s = await setup("pinned");
  await s.e.coordinator.heldOperationState(s.f.record);
  const dir = path.dirname(s.e.coordinator["recordPath"](s.f.record));
  await fs.writeFile(path.join(dir, "broken.json"), "{corrupt");
  let skip: boolean | undefined;
  s.producer.discoverOwedCandidates = async predicate => {
    skip = await predicate?.(s.f.context);
    return [{ context: s.f.context, candidates: [] }];
  };
  await s.e.coordinator.snapshotOwedInventory();
  assert.equal(skip, false);
  assert.equal(s.counts().holds, 0, "materialization authenticates before hold RPC");
  await s.e.coordinator.materializeBootInventory();
  assert.equal(s.counts().holds, 0);
  await sourceProof(s.f);
});

test("settled record with an unsettled same-context journal sibling cannot suppress discovery", async () => {
  const s = await setup("pinned");
  await s.e.coordinator.heldOperationState(s.f.record);
  const sibling: RecoveryRecord = { ...s.f.record, captureId: "31050000-0000-4000-8000-000000000099" };
  await s.e.coordinator["withJournalLock"](sibling.runId, () => s.e.coordinator["writeRecordUnlocked"](sibling));
  s.client.getBindinglessRecoveryHold = undefined;
  let skipped: boolean | undefined;
  s.producer.discoverOwedCandidates = async skip => {
    skipped = await skip?.(s.f.context); return [];
  };
  await s.e.coordinator.snapshotOwedInventory();
  assert.equal(skipped, false);
});

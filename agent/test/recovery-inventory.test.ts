import { it } from "node:test";
import { RequestError } from "../src/client.js";
import { execFileSync } from "node:child_process";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { RecoveryCoordinator, canonicalJson, type RecoveryRecord } from "../src/recovery.js";
import { createHash, createHmac } from "node:crypto";
import { GitCache, RecoveryBundleTooLargeError, type OwedCandidate, type PositiveOwedCandidateContext } from "../src/git.js";
import { nullLogger, testGitCacheOptions } from "./helpers.js";

const H = "a".repeat(40);
const H2 = "b".repeat(40);

function ownershipLost() {
  return new RequestError("GET", "/api/worker/runs/" + "run-1/ownership", 404,
    JSON.stringify({ error: "run not found " + "for this worker" }));
}

it("issue1924 crossworker ownership404 archives all original roots under open hold", async () => {
  const f = await fixture();
  try {
    f.state.ownershipError = ownershipLost();
    const record = await f.freeze(H2);
    assert.ok(record);
    assert.deepEqual(record.originalRoots?.map(r => r.sha), [H]);
    assert.ok(f.aggregates.some(roots => roots.includes(H) && roots.includes(H2)));
    await f.capture(record);
    assert.equal(f.finals.length, 1);
    assert.equal((await f.coordinator.inspect("run-1"))[0]?.finalAcknowledged, true);
  } finally { await f.close(); }
});

for (const settled of [false, true]) {
  it(`issue1924 lost FINAL ACK foreign ownership404 closed hold exact replay settled=${settled}`, async () => {
    const f = await fixture();
    try {
      if (settled) { f.state.candidates = []; f.state.status = "completed"; }
      const record = settled ? await f.coordinator.freezeInventory({
        context: f.context, currentSha: H, defaultBranch: "main", settledEvidence: "publication",
      }) : await f.freeze();
      assert.ok(record);
      f.state.loseAck = true;
      await f.capture(record);
      assert.equal(f.finals.length, 1);
      const identity = (await f.coordinator.inspect("run-1"))[0]!.finalRequest;
      const reserves = f.reserves(), produced = f.state.produced;
      f.state.ownershipError = ownershipLost();
      f.state.loseAck = false;
      assert.equal(f.state.open, false);
      await f.make().resumePending(undefined, [record]);
      assert.equal(f.finals.length, 2);
      assert.deepEqual(f.finals[1], f.finals[0]);
      assert.deepEqual((await f.coordinator.inspect("run-1"))[0]!.finalRequest, identity);
      assert.equal((await f.coordinator.inspect("run-1"))[0]!.finalAcknowledged, true);
      assert.equal(f.reserves(), reserves);
      assert.equal(f.state.produced, produced);
    } finally { await f.close(); }
  });
}

it("issue1924 ownership loss never mints publication or reopens closed hold", async () => {
  const f = await fixture();
  try {
    f.state.candidates = [];
    f.state.ownershipError = ownershipLost();
    assert.equal(await f.coordinator.freezeInventory({
      context: f.context, currentSha: H, defaultBranch: "main",
      settledEvidence: "publication", locallyQuiescent: true,
    }), undefined);
    f.state.open = false;
    assert.equal(await f.freeze(), undefined);
    assert.equal(f.reserves(), 0);
    assert.equal(f.finals.length, 0);
  } finally { await f.close(); }
});

it("issue1924 invalid ownership route body network and HTTP failures never FINAL", async () => {
  for (const error of [
    new RequestError("GET", "/api/worker/runs/run-1/unknown", 404, ownershipLost().body),
    new RequestError("GET", ownershipLost().path, 404, '{"error":"unknown route"}'),
    new RequestError("GET", ownershipLost().path, 401, ownershipLost().body),
    new RequestError("GET", ownershipLost().path, 500, ownershipLost().body),
    new Error("network unavailable"),
  ]) {
    const f = await fixture();
    try {
      f.state.ownershipError = error;
      await f.freeze().catch(() => undefined);
      assert.equal(f.reserves(), 0);
      assert.equal(f.finals.length, 0);
    } finally { await f.close(); }
  }
});

async function fixture() {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), "inventory-"));
  const context: PositiveOwedCandidateContext = {
    runId: "run-1", generation: 7, kind: "issue", branch: "task",
    barePath: path.join(root, "repo.git"),
    defaultIdentity: { ref: "refs/remotes/origin/main", sha: "d".repeat(40) },
  };
  const state = {
    candidates: [{ sha: H, pinRef: "refs/owed/a", contexts: [structuredClone(context)] }] as OwedCandidate[],
    status: "failed", ownGeneration: 7, ownGuarded: true, feature: true, open: true, expires: "2099-01-01T00:00:00Z",
    ownershipError: undefined as Error | undefined,
    loseAck: false, wrongAck: false, failProduce: false, now: 1000,
    oversized: false, cloneHeads: [] as string[], cloneReadable: true,
    produceWait: undefined as Promise<void> | undefined, produced: 0,
    onProduce: undefined as (() => void) | undefined,
  };
  const aggregates: string[][] = [];
  const finals: unknown[][] = [];
  let reserves = 0;
  const captures = new Map<string, { id: string; manifest?: { checksum: string; byte_size: number } }>();
  const client = {
    hasFeature: () => state.feature,
    getRunOwnership: async () => {
      if (state.ownershipError) throw state.ownershipError;
      return { status: state.status, claim_generation: state.ownGeneration, inventory_guarded: state.ownGuarded };
    },
    listRecoveryHolds: async () => ({ run_id: context.runId, holds: state.open ? [{
      hold_id: "hold-7", generation: 7, inventory_guarded: true, has_available_capture: false,
    }] : [] }),
    reserveRecoveryCapture: async (_run: string, request: { idempotency_key: string }) => {
      reserves++;
      let capture = captures.get(request.idempotency_key);
      if (!capture) {
        capture = { id: "server-" + (captures.size + 1) };
        captures.set(request.idempotency_key, capture);
      }
      return { capture_id: capture.id, state: "preparing" };
    },
    getRecoveryCaptureStatus: async (_run: string, id: string) => {
      const capture = [...captures.values()].find(c => c.id === id);
      assert.ok(capture);
      return {
        capture_id: id, state: capture.manifest ? "available" : "preparing", manifest_bound: !!capture.manifest,
        checksum: capture.manifest?.checksum, byte_size: capture.manifest?.byte_size, expires_at: state.expires,
      };
    },
    uploadRecoveryBundle: async (_run: string, id: string, m: { checksum: string; byte_size: number }, stream: AsyncIterable<unknown>) => {
      for await (const chunk of stream) assert.ok(chunk);
      const capture = [...captures.values()].find(c => c.id === id);
      assert.ok(capture);
      capture.manifest = m;
      return { capture_id: id, state: "available", manifest_bound: true };
    },
    releaseRecoveryCustody: async (...args: unknown[]) => {
      finals.push(args);
      if (state.loseAck) { state.open = false; throw new Error("ACK lost"); }
      return { run_id: state.wrongAck ? "other-run" : context.runId, generation: 7, released: true, holds_released: 1 };
    },
  };
  const git = {
    readInventoryCloneHeads: async () => state.cloneReadable
      ? ({ kind: "verified", heads: state.cloneHeads, foreignOwners: [] }) : ({ kind: "unknown" }),
    ancestry: async (_bare: string, head: string) => head === H ? "ancestor" : "unknown",
    enumerateOwedCandidates: async () => structuredClone(state.candidates),
    discoverOwedCandidates: async () => [{ context, candidates: structuredClone(state.candidates) }],
    resolveRecoveryBareDir: async () => context.barePath,
    buildRecoveryCoverage: async (_bare: string, _ctx: unknown, roots: string[], currentSha: string) => {
      aggregates.push(roots);
      const fingerprint = createHash("sha256").update(JSON.stringify({ roots: [...roots].sort(), currentSha })).digest("hex");
      return { sha: fingerprint.slice(0, 40), fingerprint };
    },
    fetchDefaultTip: async () => { throw new Error("guarded capture must be credential free"); },
    produceRecoveryBundle: async (_bare: string, opts: { outPath: string; forgeTip?: string }) => {
      state.produced++;
      state.onProduce?.();
      await state.produceWait;
      assert.equal(opts.forgeTip, undefined);
      if (state.oversized) throw new RecoveryBundleTooLargeError(100, 10);
      if (state.failProduce) throw new Error("bundle failure");
      const bytes = Buffer.from("bundle-fixture");
      await fs.writeFile(opts.outPath, bytes);
      return { bundlePath: opts.outPath, byteSize: bytes.length,
        checksum: createHash("sha256").update(bytes).digest("hex"),
        chunkCount: 1, prerequisiteShas: [], selfContained: true, alreadyPublished: false };
    },
  };
  const make = () => new RecoveryCoordinator({
    recoveryRoot: path.join(root, "journal"), workerToken: "local-worker-fixture",
    log: nullLogger(), client: client as never, git: git as never, now: () => state.now,
  });
  const coordinator = make();
  const freeze = (originalSourceSha = H) => coordinator.freezeInventory({
    context, currentSha: H, originalSourceSha, defaultBranch: "main",
  });
  const capture = (record: RecoveryRecord) => coordinator.captureAndUpload({
    record, barePath: context.barePath, defaultBranch: "main",
  });
  return { root, context, state, aggregates, finals, coordinator, make, freeze, capture,
    reserves: () => reserves, close: () => fs.rm(root, { recursive: true, force: true }) };
}

it("final inventory ACK covers an earlier available aggregate without treating it as an adopted head", async () => {
  const f = await fixture();
  try {
    f.state.status = "running";
    const earlier = await f.coordinator.freezeInventory({
      context: f.context, currentSha: H, defaultBranch: "main", locallyQuiescent: true,
    });
    assert.ok(earlier);
    await f.capture(earlier);
    assert.equal(f.finals.length, 0, "an active generation cannot finalize its earlier archive");
    f.state.candidates.push({ sha: H2, pinRef: "refs/owed/b", contexts: [structuredClone(f.context)] });
    f.state.status = "failed";
    const covering = await f.freeze(H2);
    assert.ok(covering);
    assert.notEqual(covering.captureId, earlier.captureId);
    await f.capture(covering);
    const records = await f.coordinator.inspect(f.context.runId);
    const final = records.find(r => r.captureId === covering.captureId)!;
    assert.equal(final.finalAcknowledged, true);
    assert.notEqual(final.serverCaptureId, records.find(r => r.captureId === earlier.captureId)?.serverCaptureId);
    assert.equal(await f.coordinator.inventoryCleanupState(f.context.runId, f.context.generation), "acknowledged");
  } finally { await f.close(); }
});

it("exact disposition source absent from owed refs is included without invented production", async () => {
  const f = await fixture();
  try {
    const record = await f.freeze(H2);
    assert.ok(record);
    assert.ok(f.aggregates.at(-1)?.includes(H2));
    assert.equal(record.originalSourceSha, H2);
    assert.deepEqual(record.originalRoots?.map(r => r.sha), [H]);
    assert.equal((await f.coordinator.inspect("run-1")).length, 1);
  } finally { await f.close(); }
});

it("snapshot identity preserves producer context and creates a distinct immutable archive", async () => {
  const f = await fixture();
  try {
    const first = await f.freeze();
    assert.ok(first);
    assert.equal((await f.freeze())?.captureId, first.captureId);
    f.state.candidates[0]!.contexts[0]!.branch = "original-producer-branch";
    const next = await f.freeze();
    assert.ok(next);
    assert.notEqual(next.captureId, first.captureId);
    const records = await f.coordinator.inspect("run-1");
    assert.equal(records.length, 2);
    assert.deepEqual(records.find(r => r.captureId === first.captureId)?.originalRoots, first.originalRoots);
  } finally { await f.close(); }
});

it("boot uses a new exact source fingerprint when owed roots stayed unchanged", async () => {
  const f = await fixture();
  try {
    f.state.expires = "invalid";
    const old = await f.freeze();
    assert.ok(old);
    await f.capture(old);
    const before = (await f.coordinator.inspect("run-1")).find(r => r.captureId === old.captureId)!;
    await f.coordinator.pin({
      runId: "run-1", generation: 7, inventoryGuarded: true,
      sourceSha: H2, kind: "issue", branch: "task",
    });
    const restarted = f.make();
    await restarted.snapshotOwedInventory();
    const reconstructed = await restarted.materializeBootInventory();
    assert.equal(reconstructed.length, 1);
    assert.notEqual(reconstructed[0]!.captureId, old.captureId);
    assert.equal(reconstructed[0]!.originalSourceSha, H2);
    assert.equal(reconstructed[0]!.inventoryCurrentSha, H2);
    assert.notEqual(reconstructed[0]!.coverageDigest, old.coverageDigest);
    assert.deepEqual((await restarted.inspect("run-1")).find(r => r.captureId === old.captureId), before);
    assert.ok(f.aggregates.some(roots => roots.includes(H) && roots.includes(H2)));
  } finally { await f.close(); }
});

it("an original-source pin never rewrites a frozen aggregate in the same real generation", async () => {
  const f = await fixture();
  try {
    const frozen = await f.freeze();
    assert.ok(frozen);
    const before = structuredClone(frozen);
    const original = await f.coordinator.pin({
      runId: "run-1", generation: 7, inventoryGuarded: true,
      sourceSha: H2, kind: "issue", branch: "task",
    });
    assert.ok(original);
    assert.notEqual(original.captureId, frozen.captureId);
    assert.equal(original.sourceSha, H2);
    const records = await f.coordinator.inspect("run-1");
    assert.equal(records.length, 2);
    assert.deepEqual(records.find(record => record.captureId === frozen.captureId), before);
    assert.equal((await f.freeze())?.captureId, frozen.captureId);
  } finally { await f.close(); }
});

it("exact-generation clone-only source journal blocks older uploaded FINAL after restart", async () => {
  const f = await fixture();
  try {
    f.state.expires = "invalid";
    const record = await f.freeze();
    assert.ok(record);
    await f.capture(record);
    await f.coordinator.pin({
      runId: "run-1", generation: 7, inventoryGuarded: true,
      sourceSha: H2, kind: "issue", branch: "task",
    });
    f.state.expires = "2099-01-01T00:00:00Z";
    await f.make().resumePending(undefined, [record]);
    assert.deepEqual(f.finals, []);
    assert.equal(f.state.open, true);
  } finally { await f.close(); }
});

it("positively later-generation source does not invalidate an ACK-lost receipt", async () => {
  const f = await fixture();
  try {
    const record = await f.freeze();
    assert.ok(record);
    f.state.loseAck = true;
    await f.capture(record);
    await f.coordinator.pin({
      runId: "run-1", generation: 8, inventoryGuarded: true,
      sourceSha: H2, kind: "issue", branch: "task",
    });
    f.state.candidates[0]!.contexts.push({ ...f.context, generation: 8 });
    f.state.candidates.push({ sha: H2, pinRef: "refs/owed/later",
      contexts: [{ ...f.context, generation: 8 }] });
    f.state.cloneHeads = [H2];
    f.state.ownGeneration = 8;
    f.state.loseAck = false;
    await f.make().resumePending(undefined, [record]);
    assert.equal(f.finals.length, 2);
    assert.equal((await f.coordinator.inspect("run-1")).find(r => r.captureId === record.captureId)?.finalAcknowledged, true);
  } finally { await f.close(); }
});

it("retained clone blocks settled FINAL and unreadable clone evidence never means empty", async () => {
  const f = await fixture();
  try {
    f.state.candidates = [];
    const record = await f.coordinator.freezeInventory({
      context: f.context, currentSha: H, defaultBranch: "main", settledEvidence: "forge_no_output",
    });
    assert.ok(record);
    f.state.cloneReadable = false;
    await f.capture(record);
    assert.deepEqual(f.finals, []);
    f.state.cloneReadable = true;
    f.state.cloneHeads = [H2];
    await f.make().resumePending(undefined, [record]);
    assert.deepEqual(f.finals, []);
    assert.equal(f.state.open, true);
  } finally { await f.close(); }
});

it("ACK-lost archive receipt stays immutable when a new unattributed clone head appears", async () => {
  const f = await fixture();
  try {
    const record = await f.freeze();
    assert.ok(record);
    f.state.loseAck = true;
    await f.capture(record);
    const before = (await f.coordinator.inspect("run-1"))[0]!.finalRequest;
    assert.ok(before);
    f.state.cloneHeads = [H2];
    f.state.loseAck = false;
    await f.make().resumePending(undefined, [record]);
    assert.equal(f.finals.length, 1);
    const pending = (await f.coordinator.inspect("run-1"))[0]!;
    assert.deepEqual(pending.finalRequest, before);
    assert.equal(pending.finalAcknowledged, undefined);
  } finally { await f.close(); }
});

it("closed hold cannot reserve and active generation cannot send final RPC", async () => {
  const f = await fixture();
  try {
    const record = await f.freeze();
    assert.ok(record);
    f.state.open = false;
    await f.capture(record);
    assert.equal(f.reserves(), 0);
    assert.deepEqual(f.finals, []);
  } finally { await f.close(); }
});

it("malformed expiry never closes a guarded hold", async () => {
  const f = await fixture();
  try {
    const record = await f.freeze();
    assert.ok(record);
    f.state.expires = "not-a-date";
    await f.capture(record);
    assert.deepEqual(f.finals, []);
    assert.equal(f.reserves(), 1);
    assert.equal((await f.coordinator.inspect("run-1"))[0]?.state, "uploaded");
    assert.equal((await f.coordinator.inspect("run-1"))[0]?.finalRequest, undefined);
  } finally { await f.close(); }
});

it("discovery with no journal survives active API until a later spaced live pass", async () => {
  const f = await fixture();
  try {
    f.state.status = "running";
    await f.coordinator.snapshotOwedInventory();
    assert.deepEqual(await f.coordinator.materializeBootInventory(), []);
    f.state.status = "failed";
    await f.coordinator.resumeLive({ authenticatedAtMs: 1000, isExecuting: () => false });
    assert.equal((await f.coordinator.inspect("run-1")).length, 1);
    assert.equal(f.reserves(), 1);
  } finally { await f.close(); }
});


it("active API permits local reaped freeze but refuses final RPC until the generation ends", async () => {
  const f = await fixture();
  try {
    f.state.status = "running";
    assert.equal(await f.freeze(), undefined);
    const record = await f.coordinator.freezeInventory({
      context: f.context, currentSha: H, defaultBranch: "main", locallyQuiescent: true,
    });
    assert.ok(record);
    assert.equal((await f.capture(record)).state, "uploaded");
    assert.equal(f.reserves(), 1);
    assert.deepEqual(f.finals, []);
    f.state.status = "failed";
    await f.coordinator.resumePending(undefined, [record]);
    assert.equal(f.finals.length, 1);
    assert.equal((await f.coordinator.inspect("run-1"))[0]?.finalAcknowledged, true);
  } finally { await f.close(); }
});

it("earlier uploaded archive cannot release added roots; a new snapshot preserves it", async () => {
  const f = await fixture();
  try {
    f.state.expires = "invalid";
    const first = await f.freeze();
    assert.ok(first);
    assert.equal((await f.capture(first)).state, "uploaded");
    f.state.candidates.push({ sha: H2, pinRef: "refs/owed/b", contexts: [f.context] });
    f.state.expires = "2099-01-01T00:00:00Z";
    await f.coordinator.resumePending(undefined, [first]);
    assert.deepEqual(f.finals, []);
    const next = await f.freeze();
    assert.ok(next);
    assert.notEqual(first.captureId, next.captureId);
    const old = (await f.coordinator.inspect("run-1")).find(r => r.captureId === first.captureId)!;
    assert.equal(old.sourceSha, first.sourceSha);
    assert.equal(old.coverageDigest, first.coverageDigest);
    assert.equal(old.finalRequest, undefined);
  } finally { await f.close(); }
});

it("lost final ACK replays exact uploaded request after restart with closed hold; wrong ACK retains", async () => {
  const f = await fixture();
  try {
    const record = await f.freeze();
    assert.ok(record);
    f.state.loseAck = true;
    assert.equal((await f.capture(record)).state, "uploaded");
    assert.equal(f.finals.length, 1);
    const pending = (await f.coordinator.inspect("run-1"))[0]!;
    assert.ok(pending.finalRequest);
    assert.equal(pending.finalAcknowledged, undefined);
    f.state.loseAck = false;
    f.state.wrongAck = true;
    const restarted = f.make();
    const snapshot = await restarted.snapshotBootRecords();
    assert.equal(snapshot.length, 1);
    await restarted.resumePending(undefined, snapshot);
    assert.deepEqual(f.finals[1], f.finals[0]);
    assert.equal((await restarted.inspect("run-1"))[0]?.finalAcknowledged, undefined);
    f.state.wrongAck = false;
    await restarted.resumePending(undefined, snapshot);
    assert.deepEqual(f.finals[2], f.finals[0]);
    assert.equal((await restarted.inspect("run-1"))[0]?.finalAcknowledged, true);
    assert.equal(f.reserves(), 1);
  } finally { await f.close(); }
});

it("settled ACK loss retries the same request after its exact hold closes", async () => {
  const f = await fixture();
  try {
    f.state.candidates = [];
    f.state.status = "completed";
    const record = await f.coordinator.freezeInventory({
      context: f.context, currentSha: H, defaultBranch: "main", settledEvidence: "publication",
    });
    assert.ok(record);
    f.state.loseAck = true;
    await f.capture(record);
    assert.equal(f.finals.length, 1);
    assert.equal(f.state.open, false);
    f.state.loseAck = false;
    const restarted = f.make();
    await restarted.resumePending();
    assert.equal(f.finals.length, 2);
    assert.deepEqual(f.finals[1], f.finals[0]);
    assert.equal((await restarted.inspect("run-1"))[0]?.finalAcknowledged, true);
    assert.equal(f.reserves(), 0);
  } finally { await f.close(); }
});

it("confirmed containment removes owed pins without changing an uploaded final archive retry", async () => {
  const f = await fixture();
  try {
    const record = await f.freeze();
    assert.ok(record);
    f.state.loseAck = true;
    await f.capture(record);
    const identity = (await f.coordinator.inspect("run-1"))[0]!.finalRequest;
    f.state.candidates = []; // Git reconcile removed only remotely confirmed roots.
    f.state.loseAck = false;
    const restarted = f.make();
    await restarted.resumePending();
    assert.equal(f.finals.length, 2);
    assert.deepEqual(f.finals[1], f.finals[0]);
    const acknowledged = (await restarted.inspect("run-1"))[0]!;
    assert.deepEqual(acknowledged.finalRequest, identity);
    assert.equal(acknowledged.finalAcknowledged, true);
    assert.deepEqual(acknowledged.originalRoots, record.originalRoots);
    assert.equal(f.reserves(), 1);
  } finally { await f.close(); }
});

it("a later active unguarded claim still proves the original guarded generation ended", async () => {
  const f = await fixture();
  try {
    f.state.ownGeneration = 8;
    f.state.ownGuarded = false;
    f.state.status = "running";
    const record = await f.freeze();
    assert.ok(record);
    await f.capture(record);
    assert.equal(f.finals.length, 1);
    assert.equal((await f.coordinator.inspect("run-1"))[0]?.finalAcknowledged, true);
  } finally { await f.close(); }
});

it("new roots after durable final request fail closed without replacing its identity", async () => {
  const f = await fixture();
  try {
    const record = await f.freeze();
    assert.ok(record);
    f.state.loseAck = true;
    await f.capture(record);
    const before = (await f.coordinator.inspect("run-1"))[0]!.finalRequest;
    f.state.candidates.push({ sha: H2, pinRef: "refs/owed/b", contexts: [f.context] });
    f.state.open = true;
    assert.equal(await f.freeze(), undefined);
    await f.coordinator.resumePending(undefined, [record]);
    const pending = (await f.coordinator.inspect("run-1"))[0]!;
    assert.deepEqual(pending.finalRequest, before);
    assert.equal(pending.reason, "inventory_quiescence_breach");
    assert.equal(f.finals.length, 1);
  } finally { await f.close(); }
});

it("feature loss blocks reserve and pending final retry without stripping coverage", async () => {
  const f = await fixture();
  try {
    const record = await f.freeze();
    assert.ok(record);
    f.state.feature = false;
    await f.capture(record);
    assert.equal(f.reserves(), 0);
    assert.deepEqual(f.finals, []);
    f.state.feature = true;
    f.state.loseAck = true;
    await f.capture(record);
    f.state.feature = false;
    await f.coordinator.resumePending(undefined, [record]);
    assert.equal(f.finals.length, 1);
    assert.equal((await f.coordinator.inspect("run-1"))[0]?.coverageDigest, record.coverageDigest);
  } finally { await f.close(); }
});

it("failed capture retains exact inventory and hold without final RPC", async () => {
  const f = await fixture();
  try {
    const record = await f.freeze();
    assert.ok(record);
    f.state.failProduce = true;
    assert.equal((await f.capture(record)).state, "needs_action");
    const retained = (await f.coordinator.inspect("run-1"))[0]!;
    assert.deepEqual(retained.originalRoots, record.originalRoots);
    assert.equal(retained.reason, "bundle_failed");
    await f.coordinator.forgetGeneration("run-1", 7);
    assert.equal((await f.coordinator.inspect("run-1")).length, 1);
    assert.equal(f.reserves(), 0);
    assert.deepEqual(f.finals, []);
  } finally { await f.close(); }
});

it("MAC tampering and authenticated malformed producer context never become empty settlement", async () => {
  const f = await fixture();
  try {
    const record = await f.freeze();
    assert.ok(record);
    const file = path.join(f.root, "journal", "run-1", record.captureId + ".json");
    const saved = await fs.readFile(file, "utf8");
    const tampered = JSON.parse(saved);
    tampered.originalRoots[0].contexts[0].branch = "tampered";
    await fs.writeFile(file, JSON.stringify(tampered));
    assert.deepEqual(await f.coordinator.inspect("run-1"), []);
    const malformed = JSON.parse(saved);
    malformed.originalRoots[0].contexts[0].kind = "invented-kind";
    const { mac: _oldMac, ...payload } = malformed;
    const key = createHmac("sha256", "local-worker-fixture").update("uzi-recovery-journal-v1").digest();
    malformed.mac = createHmac("sha256", key).update(canonicalJson(payload)).digest("hex");
    await fs.writeFile(file, JSON.stringify(malformed));
    await f.coordinator.resumePending(undefined, [record]);
    assert.deepEqual(await f.coordinator.inspect("run-1"), []);
    assert.deepEqual(f.finals, []);
  } finally { await f.close(); }
});

it("foreground, boot, live and forget serialize the whole generation without nested lock", async () => {
  const f = await fixture();
  let unblock!: () => void;
  try {
    const first = await f.freeze();
    f.state.candidates.push({ sha: H2, pinRef: "refs/owed/b", contexts: [f.context] });
    const second = await f.freeze();
    assert.ok(first);
    assert.ok(second);
    f.state.produceWait = new Promise<void>(resolve => { unblock = resolve; });
    const started = new Promise<void>(resolve => { f.state.onProduce = resolve; });
    const foreground = f.capture(first);
    await started;
    const boot = f.coordinator.resumePending(undefined, [second]);
    await f.coordinator.resumeLive({ authenticatedAtMs: 1000, isExecuting: () => false });
    const forget = f.coordinator.forgetGeneration("run-1", 7);
    assert.equal(f.state.produced, 1);
    unblock();
    await Promise.all([foreground, boot, forget]);
    assert.equal(f.state.produced, 2);
    assert.equal(f.finals.length, 1);
    assert.equal((await f.coordinator.inspect("run-1")).length, 2);
  } finally { unblock?.(); await f.close(); }
});


it("oversized capture keeps the guarded source and never reserves or finalizes", async () => {
  const f = await fixture();
  try {
    const record = await f.freeze();
    assert.ok(record);
    f.state.oversized = true;
    assert.equal((await f.capture(record)).reason, "oversized");
    assert.equal((await f.coordinator.inspect("run-1"))[0]?.coverageDigest, record.coverageDigest);
    assert.equal(f.reserves(), 0);
    assert.deepEqual(f.finals, []);
  } finally { await f.close(); }
});

it("permanent oversized inventory is retained without repeated boot or live production", async () => {
  const f = await fixture();
  try {
    f.state.oversized = true;
    const record = await f.freeze();
    assert.ok(record);
    assert.equal((await f.capture(record)).reason, "oversized");
    const restarted = f.make();
    await restarted.resumePending();
    f.state.now += 100_000;
    await restarted.resumeLive({ authenticatedAtMs: f.state.now, isExecuting: () => false });
    assert.equal(f.state.produced, 1);
    assert.equal(f.reserves(), 0);
    assert.deepEqual(f.finals, []);
    assert.equal((await restarted.inspect("run-1"))[0]?.reason, "oversized");
  } finally { await f.close(); }
});

it("real aggregate bundle imports H and exact disposition H2 with current tree and original production intact", async () => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), "inventory-git-"));
  const gitEnv = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null",
    GIT_TERMINAL_PROMPT: "0" };
  const git = (cwd: string, args: string[]) =>
    execFileSync("git", ["-C", cwd, ...args], { env: gitEnv, encoding: "utf8", timeout: 10000 }).trim();
  try {
    const data = path.join(root, "data");
    const repos = path.join(data, "repos");
    const bare = path.join(repos, "repo.git");
    await fs.mkdir(repos, { recursive: true, mode: 0o700 });
    execFileSync("git", ["init", "--bare", bare], { env: gitEnv, stdio: "pipe", timeout: 10000 });
    const work = path.join(root, "work");
    await fs.mkdir(work);
    execFileSync("git", ["init", "-b", "main", work], { env: gitEnv, stdio: "pipe", timeout: 10000 });
    git(work, ["config", "user.name", "fixture"]);
    git(work, ["config", "user.email", "fixture@uzi.local"]);
    git(work, ["config", "commit.gpgsign", "false"]);
    await fs.writeFile(path.join(work, "work.txt"), "base");
    git(work, ["add", "work.txt"]);
    git(work, ["commit", "-m", "base"]);
    const base = git(work, ["rev-parse", "HEAD"]);
    await fs.writeFile(path.join(work, "work.txt"), "original H");
    git(work, ["commit", "-am", "H"]);
    const h = git(work, ["rev-parse", "HEAD"]);
    git(work, ["checkout", "--detach", base]);
    await fs.writeFile(path.join(work, "work.txt"), "current H2");
    git(work, ["commit", "-am", "H2"]);
    const h2 = git(work, ["rev-parse", "HEAD"]);
    git(work, ["push", bare, h + ":refs/heads/producer", h2 + ":refs/heads/current"]);
    const cache = new GitCache(data, nullLogger(), undefined, testGitCacheOptions());
    const producer: PositiveOwedCandidateContext = {
      runId: "run-real", generation: 1, kind: "issue", branch: "producer", barePath: bare,
      defaultIdentity: { ref: "refs/remotes/origin/main", sha: base },
    };
    assert.equal((await cache.updateTrackingRef(bare, producer.branch, h, { context: producer })).kind, "updated");
    assert.equal((await cache.committedTrackingOwnership(bare, producer.branch, producer.runId, h, 1)).kind, "owned");
    const context = { ...producer, generation: 7, branch: "current" };
    const coordinator = new RecoveryCoordinator({
      recoveryRoot: path.join(data, "recovery"), workerToken: "local-worker-fixture", log: nullLogger(),
      git: cache,
      client: {
        hasFeature: () => true,
        getRunOwnership: async () => ({ status: "failed", claim_generation: 7, inventory_guarded: true }),
        listRecoveryHolds: async () => ({ run_id: "run-real", holds: [{
          hold_id: "hold-real", generation: 7, inventory_guarded: true, has_available_capture: false,
        }] }),
      } as never,
    });
    const record = await coordinator.freezeInventory({
      context, currentSha: h2, originalSourceSha: h2, defaultBranch: "main",
    });
    assert.ok(record);
    assert.deepEqual(record.originalRoots?.map(r => r.sha), [h]);
    assert.equal(record.originalRoots?.[0]?.contexts[0]?.generation, 1);
    assert.equal(record.originalRoots?.[0]?.contexts[0]?.branch, "producer");
    const bundle = await cache.produceRecoveryBundle(bare, {
      sourceSha: record.sourceSha, outPath: path.join(root, "aggregate.bundle"), forgeTip: undefined,
    });
    assert.equal(bundle.selfContained, true);
    const imported = path.join(root, "import.git");
    execFileSync("git", ["init", "--bare", imported], { env: gitEnv, stdio: "pipe", timeout: 10000 });
    git(imported, ["bundle", "unbundle", bundle.bundlePath]);
    for (const sha of [h, h2]) {
      assert.equal(git(imported, ["cat-file", "-t", sha]), "commit");
      git(imported, ["merge-base", "--is-ancestor", sha, record.sourceSha]);
    }
    assert.equal(git(imported, ["rev-parse", record.sourceSha + "^{tree}"]),
      git(imported, ["rev-parse", h2 + "^{tree}"]));
    assert.deepEqual((await cache.enumerateOwedCandidates(bare, "run-real")).map(c => c.sha), [h]);
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});

it("restart from persisted divergent owed refs without a journal uploads an importable inventory", async () => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), "inventory-git-"));
  const gitEnv = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null",
    GIT_TERMINAL_PROMPT: "0" };
  const git = (cwd: string, args: string[]) =>
    execFileSync("git", ["-C", cwd, ...args], { env: gitEnv, encoding: "utf8", timeout: 10000 }).trim();
  try {
    const data = path.join(root, "data");
    const repos = path.join(data, "repos");
    const bare = path.join(repos, "repo.git");
    await fs.mkdir(repos, { recursive: true, mode: 0o700 });
    execFileSync("git", ["init", "--bare", bare], { env: gitEnv, stdio: "pipe", timeout: 10000 });
    const work = path.join(root, "work");
    await fs.mkdir(work);
    execFileSync("git", ["init", "-b", "main", work], { env: gitEnv, stdio: "pipe", timeout: 10000 });
    git(work, ["config", "user.name", "fixture"]);
    git(work, ["config", "user.email", "fixture@uzi.local"]);
    git(work, ["config", "commit.gpgsign", "false"]);
    await fs.writeFile(path.join(work, "work.txt"), "base");
    git(work, ["add", "work.txt"]);
    git(work, ["commit", "-m", "base"]);
    const base = git(work, ["rev-parse", "HEAD"]);
    await fs.writeFile(path.join(work, "work.txt"), "original H");
    git(work, ["commit", "-am", "H"]);
    const h = git(work, ["rev-parse", "HEAD"]);
    git(work, ["checkout", "--detach", base]);
    await fs.writeFile(path.join(work, "work.txt"), "current H2");
    git(work, ["commit", "-am", "H2"]);
    const h2 = git(work, ["rev-parse", "HEAD"]);
    git(work, ["push", bare, h + ":refs/heads/producer", h2 + ":refs/heads/current"]);
    const cache = new GitCache(data, nullLogger(), undefined, testGitCacheOptions());
    const producer: PositiveOwedCandidateContext = {
      runId: "run-real", generation: 1, kind: "issue", branch: "producer", barePath: bare,
      defaultIdentity: { ref: "refs/remotes/origin/main", sha: base },
    };
    assert.equal((await cache.updateTrackingRef(bare, producer.branch, h, { context: producer })).kind, "updated");
    assert.equal((await cache.committedTrackingOwnership(bare, producer.branch, producer.runId, h, 1)).kind, "owned");
    const current: PositiveOwedCandidateContext = { ...producer, branch: "current" };
    assert.equal((await cache.updateTrackingRef(bare, current.branch, h2, { context: current })).kind, "updated");
    assert.equal((await cache.committedTrackingOwnership(bare, current.branch, current.runId, h2, 1)).kind, "owned");
    const recoveryRoot = path.join(data, "recovery");
    assert.equal(await fs.stat(recoveryRoot).then(() => true, () => false), false, "crash precedes any journal");
    // No cache or coordinator RAM crosses the restart boundary.
    const restartedCache = new GitCache(data, nullLogger(), undefined, testGitCacheOptions());
    restartedCache.fetchDefaultTip = async () => { throw new Error("restart capture must not use a PAT"); };
    let open = true, reserves = 0, uploads = 0, finals = 0;
    const serverId = "00000000-0000-4000-8000-000000001945";
    let manifest: { checksum: string; byte_size: number } | undefined;
    let uploaded = Buffer.alloc(0);
    const coordinator = new RecoveryCoordinator({
      recoveryRoot, workerToken: "local-worker-fixture", log: nullLogger(), git: restartedCache,
      client: {
        hasFeature: (name: string) => name === "recovery_inventory_v1",
        getRunOwnership: async () => ({ status: "failed", claim_generation: 1, inventory_guarded: true }),
        listRecoveryHolds: async (runId: string) => {
          assert.equal(runId, producer.runId);
          return { run_id: runId, holds: open ? [{
            hold_id: "persisted-hold", generation: 1, inventory_guarded: true, has_available_capture: false,
          }] : [] };
        },
        reserveRecoveryCapture: async (runId: string) => {
          assert.equal(runId, producer.runId);
          assert.equal(open, true);
          assert.equal(++reserves, 1, "one bounded reservation");
          return { capture_id: serverId, state: "preparing" };
        },
        uploadRecoveryBundle: async (runId: string, id: string,
          m: { checksum: string; byte_size: number }, stream: AsyncIterable<Uint8Array>) => {
          assert.equal(runId, producer.runId);
          assert.equal(id, serverId);
          assert.equal(open, true);
          const chunks: Buffer[] = [];
          for await (const chunk of stream) chunks.push(Buffer.from(chunk));
          uploaded = Buffer.concat(chunks);
          assert.equal(uploaded.length, m.byte_size);
          assert.equal(createHash("sha256").update(uploaded).digest("hex"), m.checksum);
          manifest = m;
          uploads++;
          return { capture_id: serverId, state: "available", manifest_bound: true };
        },
        getRecoveryCaptureStatus: async () => ({
          capture_id: serverId, state: manifest ? "available" : "preparing", manifest_bound: !!manifest,
          checksum: manifest?.checksum, byte_size: manifest?.byte_size, expires_at: "2099-01-01T00:00:00Z",
        }),
        releaseRecoveryCustody: async (runId: string, generation: number) => {
          assert.equal(runId, producer.runId);
          assert.equal(generation, 1);
          assert.equal(uploads, 1);
          assert.equal(open, true);
          finals++;
          open = false;
          return { run_id: runId, generation, released: true, holds_released: 1 };
        },
      } as never,
    });
    assert.deepEqual(await coordinator.snapshotBootRecords(), []);
    await coordinator.snapshotOwedInventory();
    const records = await coordinator.materializeBootInventory();
    assert.equal(records.length, 1);
    const record = records[0]!;
    assert.deepEqual(record.originalRoots?.map(r => r.sha).sort(), [h, h2].sort());
    assert.deepEqual(record.originalRoots?.find(r => r.sha === h)?.contexts, [producer]);
    assert.deepEqual(record.originalRoots?.find(r => r.sha === h2)?.contexts, [current]);
    await coordinator.resumeLive({ authenticatedAtMs: Date.now(), isExecuting: () => false });
    assert.equal(reserves, 1);
    assert.equal(uploads, 1);
    assert.equal(finals, 1);
    assert.equal((await coordinator.inspect(producer.runId))[0]?.finalAcknowledged, true);
    const bundlePath = path.join(root, "download.bundle");
    await fs.writeFile(bundlePath, uploaded);
    const imported = path.join(root, "import");
    execFileSync("git", ["init", imported], { env: gitEnv, stdio: "pipe", timeout: 10000 });
    git(imported, ["bundle", "unbundle", bundlePath]);
    for (const [sha, contents] of [[h, "original H"], [h2, "current H2"]]) {
      assert.equal(git(imported, ["cat-file", "-t", sha!]), "commit");
      git(imported, ["merge-base", "--is-ancestor", sha!, record.sourceSha]);
      git(imported, ["checkout", "--detach", sha!]);
      assert.equal(await fs.readFile(path.join(imported, "work.txt"), "utf8"), contents);
    }
    assert.equal(git(imported, ["rev-parse", record.sourceSha + "^{tree}"]),
      git(imported, ["rev-parse", record.originalSourceSha + "^{tree}"]));
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});

it("a guarded journal never uses the legacy release RPC after feature loss", async () => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), "inventory-"));
  const calls: unknown[] = [];
  const coordinator = new RecoveryCoordinator({
    recoveryRoot: root, workerToken: "local-worker-fixture", log: nullLogger(),
    client: {
      hasFeature: () => false,
      releaseRecoveryCustody: async (...args: unknown[]) => {
        calls.push(args);
        return { run_id: "run-1", released: true, holds_released: 1, generation: 7 };
      },
      listRecoveryHolds: async () => ({ run_id: "run-1", holds: [] }),
    } as never,
    git: {} as never,
  });
  try {
    await coordinator.pin({
      runId: "run-1", generation: 7, sourceSha: "a".repeat(40),
      kind: "issue", branch: "task", inventoryGuarded: true,
    });
    await coordinator.release("run-1", 7, "publication");
    assert.deepEqual(calls, []);
    assert.equal((await coordinator.inspect("run-1")).length, 1);
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});

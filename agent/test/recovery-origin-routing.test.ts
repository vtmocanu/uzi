import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import path from "node:path";
import { createHash, createHmac } from "node:crypto";
import { RecoveryCoordinator, canonicalJson, type RecoveryRecord, type RecoveryArchiveClient } from "../src/recovery.js";
import { MaintenanceResources } from "../src/recovery-maintenance.js";
import type { PositiveOwedCandidateContext } from "../src/git.js";
import type { RecoveryExactHold, RecoveryFinalDisposition } from "../src/protocol.js";
import { nullLogger } from "./helpers.js";

const producer = "maintenance-v1-00000000-0000-4000-8000-000000000001";
const ordinary = "00000000-0000-4000-8000-000000000002";
const holdId = "00000000-0000-4000-8000-000000000007";
const repoId = "00000000-0000-4000-8000-000000000008";
const publicationId = "00000000-0000-4000-8000-000000000009";
const source = "a".repeat(40), aggregate = "b".repeat(40), digest = "c".repeat(64);
const token = "origin-routing-fixture";

async function fixture(t: test.TestContext) {
  const root = await fs.mkdtemp(path.resolve("../.uzi/scratch/origin-routing-"));
  t.after(() => fs.rm(root, { recursive: true, force: true }));
  const recoveryRoot = path.join(root, "journal");
  const context: PositiveOwedCandidateContext = {
    runId: "run-origin", generation: 7, kind: "issue", branch: "task",
    barePath: path.join(root, "repo.git"),
    defaultIdentity: { ref: "refs/remotes/origin/main", sha: "d".repeat(40) },
  };
  const roots = [{ sha: source, contexts: [context] }];
  const calls = { produce: 0, coverage: 0, prepare: 0, publish: 0, upload: 0, reserve: 0, final: 0 };
  const warnings: Array<{ message: string; fields?: Record<string, unknown> }> = [];
  const uploaded: Buffer[] = [];
  const finals: RecoveryFinalDisposition[] = [];
  let status = "failed", fallback = false;
  let settled: RecoveryExactHold | undefined;
  const bytes = (tip: string) => {
    const pack = Buffer.concat([Buffer.from("PACK"), Buffer.from([0, 0, 0, 2, 0, 0, 0, 0])]);
    return Buffer.concat([Buffer.from(`# v2 git bundle\n${tip} refs/heads/recovered-source\n\n`),
      pack, createHash("sha1").update(pack).digest()]);
  };
  const client: RecoveryArchiveClient = {
    hasFeature: () => true,
    hasHeldPublicationV2: () => true,
    getRunOwnership: async () => ({ status, claim_generation: 8, inventory_guarded: true }) as never,
    listRecoveryHolds: async () => ({ run_id: context.runId, holds: [{
      hold_id: holdId, generation: 7, inventory_guarded: true, has_available_capture: false,
    }] }),
    getExactRecoveryHold: async () => settled ?? ({
      run_id: context.runId, generation: 7, hold_id: holdId, repo_id: repoId, state: "open",
      original_worker_id: "worker", original_worker_identity: "worker", inventory_guarded: true,
      run_status: status, current_claim_generation: 8, claim_released: true,
      live_worker_id: "worker", live_run_id: context.runId,
    } as RecoveryExactHold),
    reserveRecoveryCapture: async () => { calls.reserve++; return { capture_id: "server-capture", state: "preparing" }; },
    uploadRecoveryBundle: async (_run, id, _manifest, stream) => {
      calls.upload++;
      const chunks: Buffer[] = [];
      for await (const chunk of stream) chunks.push(Buffer.from(chunk));
      uploaded.push(Buffer.concat(chunks));
      return { capture_id: id, state: "available", manifest_bound: true };
    },
    getRecoveryCaptureStatus: async (_run, id) => ({
      capture_id: id, state: "available", manifest_bound: true,
      checksum: createHash("sha256").update(bytes(aggregate)).digest("hex"),
      byte_size: bytes(aggregate).length, expires_at: "2099-01-01T00:00:00Z",
    }),
    releaseRecoveryCustody: async (run, generation, _evidence, disposition) => {
      calls.final++; finals.push(structuredClone(disposition!));
      if (fallback) return { run_id: run, generation, released: false, holds_released: 0,
        retained: true, reason: "held_publication_ref_missing" } as never;
      return { run_id: run, generation, released: true, holds_released: 1 };
    },
    publishHeld: async (_run, _gen, _tip, _digest, pack) => {
      calls.publish++;
      assert.ok(Buffer.isBuffer(pack));
      assert.equal(pack.length, 0, "only uncertain invocation readback is admitted");
      return { ok: false, reason: "pack_invalid" } as never;
    },
  };
  const git = {
    enumerateOwedCandidates: async () => roots.map(r => ({ ...r, pinRef: "refs/owed/source" })),
    discoverOwedCandidates: async () => [{ context, candidates: roots.map(r => ({ ...r, pinRef: "refs/owed/source" })) }],
    buildRecoveryCoverage: async () => { calls.coverage++; return { sha: aggregate, fingerprint: digest }; },
    readInventoryCloneHeads: async () => ({ kind: "verified", heads: [], clones: [], foreignOwners: [] }),
    ancestry: async () => "ancestor",
    resolveRecoveryBareDir: async () => context.barePath,
    resolveRestartSource: async () => ({ status: "unpublished", barePath: context.barePath }),
    fetchDefaultTip: async () => undefined,
    produceRecoveryBundle: async (_bare: string, opts: { outPath: string; sourceSha: string }) => {
      calls.produce++;
      const body = bytes(opts.sourceSha);
      await fs.writeFile(opts.outPath, body);
      return { bundlePath: opts.outPath, byteSize: body.length, checksum: createHash("sha256").update(body).digest("hex"),
        chunkCount: 1, prerequisiteShas: [], selfContained: true, alreadyPublished: false };
    },
  };
  const log = nullLogger();
  log.warn = (message, fields) => warnings.push({ message, fields });
  const make = () => new RecoveryCoordinator({
    recoveryRoot, workerToken: token, client, git: git as never, log, now: () => 1000,
    withInventorySourceBoundary: async (_ctx, action) => { await action(async () => true); return "passed"; },
  });
  const coordinator = make();
  const partial = (captureId = producer): RecoveryRecord => ({
    version: 1, runId: context.runId, generation: 7, captureId, sourceSha: source,
    kind: "issue", branch: "task", createdAt: 1, state: "pinned",
    finalizationPin: true, bareDir: "repo.git", defaultBranch: "main",
  });
  const frozen = (captureId = producer): RecoveryRecord => ({
    ...partial(captureId), inventoryGuarded: true, sourceSha: aggregate, originalSourceSha: source,
    inventoryCurrentSha: source, originalRoots: roots, coverageContext: context, coverageDigest: digest,
    exactHoldBinding: { holdId, repoId },
    heldPolicy: { version: 1, blocked: false, everKnown: false, flaggedCommits: [], ciGlobs: [], extraFloors: [] },
  });
  const file = (id: string) => path.join(recoveryRoot, context.runId, id + ".json");
  const seal = async (record: RecoveryRecord, filename = record.captureId, validMac = true) => {
    await fs.mkdir(path.dirname(file(filename)), { recursive: true });
    const key = createHmac("sha256", token).update("uzi-recovery-journal-v1").digest();
    const mac = createHmac("sha256", key).update(canonicalJson(record)).digest("hex");
    await fs.writeFile(file(filename), JSON.stringify({ ...record, mac: validMac ? mac : "0".repeat(64) }));
    return record;
  };
  const bundled = async (record: RecoveryRecord) => {
    const body = bytes(record.sourceSha);
    const bundlePath = path.join(recoveryRoot, context.runId, record.captureId + ".bundle");
    await fs.mkdir(path.dirname(bundlePath), { recursive: true });
    await fs.writeFile(bundlePath, body);
    return { ...record, state: "bundled" as const, bundlePath, byteSize: body.length,
      checksum: createHash("sha256").update(body).digest("hex"), chunkCount: 1,
      prerequisiteShas: [], selfContained: true };
  };
  const capture = (record: RecoveryRecord) => coordinator.captureAndUpload({
    record, barePath: context.barePath, defaultBranch: "main", attemptedHeadSha: "e".repeat(40),
  });
  const operate = (record: RecoveryRecord) => coordinator.operateFrozenInventory({
    record, barePath: context.barePath, defaultBranch: "main",
  }, { foreground: true, terminal: { status: "failed" }, prepare: async () => {
    calls.prepare++; return { pack: Buffer.from("fresh-pack") };
  } });
  const freeze = () => coordinator.freezeInventory({ context, currentSha: source, defaultBranch: "main" });
  const pin = () => coordinator.pin({ runId: context.runId, generation: 7, sourceSha: source, kind: "issue", branch: "task" });
  return { root, recoveryRoot, context, calls, warnings, uploaded, finals, client, git, coordinator, make,
    partial, frozen, seal, bundled, capture, operate, freeze, pin, file,
    cancel: () => { status = "cancelled"; }, refuseFinal: () => { fallback = true; },
    settle: (exact: RecoveryExactHold) => { settled = exact; } };
}

const malformed = ["maintenance", "maintenan", "maintenance-v1-", "Maintenance-v1-00000000-0000-4000-8000-000000000001",
  "maintenance-v1-00000000-0000-4000-8000-000000000001-extra"];

for (const id of [producer, ...malformed]) {
  for (const mode of ["foreground", "boot", "live"] as const) {
    test(`authenticated ${id} partial stays protected through ${mode}`, async t => {
      const f = await fixture(t);
      const record = await f.seal(mode === "foreground" ? f.partial(id) : f.frozen(id));
      const before = await fs.readFile(f.file(id));
      f.cancel();
      if (mode === "foreground") {
        const out = await f.capture(record);
        assert.equal(out.state, "pinned");
        assert.equal(out.reason, undefined);
      } else if (mode === "boot") await f.coordinator.resumePending();
      else await f.coordinator.resumeLive({ isExecuting: () => false, authenticatedAtMs: 1000 });
      assert.deepEqual(f.calls, { produce: 0, coverage: 0, prepare: 0, publish: 0, upload: 0, reserve: 0, final: 0 });
      assert.deepEqual(await fs.readFile(f.file(id)), before);
      assert.ok(f.warnings.some(w => w.message === "recovery: new production denied; source retained" &&
        w.fields?.reason === (id === producer ? "maintenance_admission_required" : "maintenance_origin_invalid")));
    });
  }
}

test("legacy boot partial is denied before restart source production or failure rewrite", async t => {
  const f = await fixture(t), record = await f.seal(f.partial());
  await f.coordinator.resumePending();
  assert.equal(f.calls.produce, 0);
  assert.deepEqual((await f.coordinator.inspect(record.runId))[0], record);
});

for (const bad of ["invalid-mac", "filename-mismatch"] as const) {
  test(`record authentication precedes origin routing: ${bad}`, async t => {
    const f = await fixture(t), record = f.partial();
    await f.seal(record, bad === "filename-mismatch" ? ordinary : record.captureId, bad !== "invalid-mac");
    const request = { ...record, captureId: bad === "filename-mismatch" ? ordinary : record.captureId };
    const out = await f.capture(request);
    assert.equal(out.reason, "record_unauthenticated");
    assert.equal(await f.freeze(), undefined);
    assert.equal(f.calls.coverage, 0);
    assert.ok(!f.warnings.some(w => w.message === "recovery: new production denied; source retained"));
  });
}

for (const leftover of ["maintenance-v1-", "unknown.journal", "oversize.json", "many-entries"] as const) {
  test(`incomplete physical provenance blocks reconstruction: ${leftover}`, async t => {
    const f = await fixture(t);
    const dir = path.dirname(f.file(ordinary));
    await fs.mkdir(dir, { recursive: true });
    if (leftover === "many-entries") {
      for (let i = 0; i < 257; i++) await fs.writeFile(path.join(dir, `entry-${i}`), "");
    } else await fs.writeFile(path.join(dir, leftover), leftover === "oversize.json" ? "x".repeat(1024 * 1024 + 1) : "{}");
    assert.equal(await f.freeze(), undefined);
    assert.equal(await f.pin(), undefined);
    await f.coordinator.snapshotOwedInventory();
    assert.deepEqual(await f.coordinator.materializeBootInventory(), []);
    assert.equal(f.calls.coverage, 0);
    assert.equal(f.calls.produce, 0);
  });
}

test("ordinary matching pin keeps its identity with mixed provenance; new freeze and boot cannot convert producer", async t => {
  const f = await fixture(t);
  const original = await f.seal(f.partial(ordinary));
  const producerRecord = await f.seal(f.partial());
  const producerBytes = await fs.readFile(f.file(producer));
  const pinned = await f.pin();
  assert.equal(pinned?.captureId, original.captureId);
  assert.equal(await f.freeze(), undefined);
  await f.coordinator.snapshotOwedInventory();
  assert.deepEqual(await f.coordinator.materializeBootInventory(), []);
  assert.deepEqual(await fs.readFile(f.file(producer)), producerBytes);
  assert.equal((await f.coordinator.inspect(original.runId)).length, 2);
  assert.equal(f.calls.coverage, 0);
  // Existing ordinary capture remains independently eligible in this mixed inventory.
  await f.capture(original);
  assert.equal(f.calls.produce, 1);
  assert.equal(f.calls.upload, 1);
  assert.equal((await f.coordinator.inspect(producerRecord.runId)).find(r => r.captureId === producer)?.state, "pinned");
});

for (const id of [producer, malformed[0]!]) {
  for (const mode of ["foreground", "boot", "live"] as const) {
    test(`immutable bundle ${id} replays original bytes through ${mode}`, async t => {
      const f = await fixture(t);
      const record = await f.seal(await f.bundled(f.partial(id)));
      const bytes = await fs.readFile(record.bundlePath!);
      if (mode === "foreground") await f.capture(record);
      else if (mode === "boot") await f.coordinator.resumePending();
      else await f.coordinator.resumeLive({ isExecuting: () => false, authenticatedAtMs: 1000 });
      assert.equal(f.calls.upload, 1);
      assert.equal(f.calls.produce, 0);
      assert.equal(f.calls.coverage, 0);
      assert.deepEqual(f.uploaded, [bytes]);
      assert.equal((await f.coordinator.inspect(record.runId))[0]?.attemptedHeadSha, undefined);
    });
  }
}

for (const kind of ["archive", "held_publication"] as const) {
  test(`saved producer ${kind} FINAL retains full coverage proof and replays before guard`, async t => {
    const f = await fixture(t);
    let record = f.frozen();
    if (kind === "archive") record = { ...await f.bundled(record), state: "uploaded", serverCaptureId: "server-capture" };
    record.finalRequest = { disposition: kind === "archive"
      ? { kind, capture_id: "server-capture", source_sha: aggregate, coverage_digest: digest }
      : { kind, publication_id: publicationId, source_sha: aggregate, coverage_digest: digest } };
    await f.seal(record);
    await f.operate(record);
    assert.equal(f.calls.final, 1);
    assert.ok(f.calls.coverage > 0, "the existing full buildRecoveryCoverage proof must run");
    assert.equal(f.calls.produce, 0);
    assert.equal(f.calls.prepare, 0);
    assert.equal(f.calls.publish, 0);
    assert.deepEqual(f.finals, [record.finalRequest.disposition]);
  });
}

test("producer self-contained uploaded archive can mint its archive FINAL from immutable evidence", async t => {
  const f = await fixture(t);
  const record = await f.seal({ ...await f.bundled(f.frozen()), state: "uploaded", serverCaptureId: "server-capture" });
  await f.operate(record);
  assert.equal(f.calls.final, 1);
  assert.ok(f.calls.coverage > 0);
  assert.equal(f.calls.prepare, 0);
  assert.equal(f.calls.produce, 0);
});

test("held FINAL definitive fallback cannot clear producer identity or start new capture", async t => {
  const f = await fixture(t), record = f.frozen();
  record.finalRequest = { disposition: { kind: "held_publication", publication_id: publicationId,
    source_sha: aggregate, coverage_digest: digest } };
  await f.seal(record);
  f.refuseFinal();
  await f.operate(record);
  const after = (await f.coordinator.inspect(record.runId))[0]!;
  assert.equal(f.calls.final, 1);
  assert.ok(f.calls.coverage > 0);
  assert.deepEqual(after.finalRequest, record.finalRequest);
  assert.equal(after.reason, undefined);
  assert.equal(f.calls.produce, 0);
  assert.equal(f.calls.prepare, 0);
});

test("uncertain held invocation readback precedes denial of fresh pack and publication", async t => {
  const f = await fixture(t);
  const record = await f.seal({ ...f.frozen(), heldCreateUncertain: true });
  await f.operate(record);
  assert.equal(f.calls.publish, 1);
  assert.equal(f.calls.prepare, 0);
  assert.equal(f.calls.coverage, 0);
  assert.equal(f.calls.produce, 0);
  assert.equal((await f.coordinator.inspect(record.runId))[0]!.heldCreateUncertain, true);
});

test("matching closed-hold receipt replays without admitting new producer work", async t => {
  const f = await fixture(t);
  const record = f.frozen();
  const disposition: RecoveryFinalDisposition = { kind: "held_publication", publication_id: publicationId,
    source_sha: aggregate, coverage_digest: digest };
  record.finalRequest = { disposition };
  const exact = (await f.client.getExactRecoveryHold!(record.runId, 7, record.exactHoldBinding!))!;
  const closed: RecoveryExactHold = { ...exact, state: "released", final_receipt: disposition, release_evidence: "held_publication" };
  delete closed.live_run_id; delete closed.live_worker_id;
  record.holdSettlement = closed;
  f.settle(closed);
  await f.seal(record);
  await f.operate(record);
  assert.equal(f.calls.final, 1);
  assert.deepEqual(f.finals, [disposition]);
  assert.equal(f.calls.prepare, 0);
  assert.equal(f.calls.produce, 0);
});

test("unsafe authenticated malformed suffix never appears in the fixed guard diagnostic", async t => {
  const f = await fixture(t), id = "maintenance-private-body" + "\u001b[31m\n";
  const record = await f.seal(f.partial(id));
  await f.capture(record);
  const diagnostic = f.warnings.find(w => w.message === "recovery: new production denied; source retained");
  assert.ok(diagnostic);
  assert.ok(JSON.stringify(diagnostic).length < 512);
  assert.ok(!JSON.stringify(diagnostic).includes("private-body"));
});

for (const id of [producer, "maintenance"]) {
  test(`frozen ${id} foreground denies new held work before coverage or preparation`, async t => {
    const f = await fixture(t), record = await f.seal(f.frozen(id));
    const before = await fs.readFile(f.file(id));
    const out = await f.operate(record);
    assert.equal(out.held, "kept");
    assert.deepEqual(f.calls, { produce: 0, coverage: 0, prepare: 0, publish: 0, upload: 0, reserve: 0, final: 0 });
    assert.deepEqual(await fs.readFile(f.file(id)), before);
  });

  test(`guarded journaled ${id} bytes upload and retain the existing full FINAL proof`, async t => {
    const f = await fixture(t);
    const record = await f.seal(await f.bundled(f.frozen(id)));
    const bytes = await fs.readFile(record.bundlePath!);
    await f.operate(record);
    assert.deepEqual(f.uploaded, [bytes]);
    assert.equal(f.calls.produce, 0);
    assert.equal(f.calls.prepare, 0);
    assert.equal(f.calls.publish, 0);
    assert.equal(f.calls.final, 1);
    assert.ok(f.calls.coverage > 0);
  });

  test(`replaceable reconciliation cannot clear ${id} FINAL or rotate reservation identity`, async t => {
    const f = await fixture(t);
    const record = { ...await f.bundled(f.frozen(id)), state: "uploaded" as const,
      serverCaptureId: "server-capture", reserveIdempotencyKey: ordinary };
    const saved: RecoveryRecord = { ...record, finalRequest: { disposition: {
      kind: "archive", capture_id: "server-capture", source_sha: aggregate, coverage_digest: digest,
    } } };
    await f.seal(saved);
    const originalStatus = f.client.getRecoveryCaptureStatus;
    f.client.getRecoveryCaptureStatus = async (...args) => ({
      ...await originalStatus(...args), state: "expired", expires_at: "1970-01-01T00:00:00Z",
    });
    let reconciliations = 0;
    f.client.reconcileRecoveryCapture = async (run, id, request) => {
      reconciliations++;
      return { run_id: run, generation: request.generation, capture_id: id, outcome: "replaceable" };
    };
    await f.operate(saved);
    assert.equal(reconciliations, 1);
    assert.ok(f.calls.coverage > 0);
    assert.equal(f.calls.final, 0);
    assert.equal(f.calls.reserve, 0);
    assert.equal(f.calls.produce, 0);
    assert.deepEqual((await f.coordinator.inspect(saved.runId))[0], saved);
  });
}

test("producer-only matching input cannot mint an ordinary pin or snapshot", async t => {
  const f = await fixture(t), record = await f.seal(f.partial());
  const before = await fs.readFile(f.file(producer));
  assert.equal(await f.pin(), undefined);
  assert.equal(await f.freeze(), undefined);
  await f.coordinator.snapshotOwedInventory();
  assert.deepEqual(await f.coordinator.materializeBootInventory(), []);
  assert.deepEqual((await f.coordinator.inspect(record.runId)), [record]);
  assert.deepEqual(await fs.readFile(f.file(producer)), before);
  assert.equal(f.calls.coverage, 0);
});

test("unreadable sibling does not gate an independently authenticated ordinary existing capture", async t => {
  const f = await fixture(t), record = await f.seal(f.partial(ordinary));
  await fs.writeFile(f.file("maintenance-unreadable"), "{");
  assert.equal((await f.pin())?.captureId, ordinary);
  assert.equal(await f.freeze(), undefined);
  await f.capture(record);
  assert.equal(f.calls.produce, 1);
  assert.equal(f.calls.upload, 1);
});

test("direct capture authenticates ordinary body against producer filename before routing", async t => {
  const f = await fixture(t);
  await f.seal(f.partial(ordinary), producer);
  const out = await f.capture(f.partial(producer));
  assert.equal(out.reason, "record_unauthenticated");
  assert.deepEqual(f.calls, { produce: 0, coverage: 0, prepare: 0, publish: 0, upload: 0, reserve: 0, final: 0 });
  assert.ok(!f.warnings.some(w => w.fields?.reason === "maintenance_admission_required"));
});

for (const generation of [6, undefined]) {
  test(`producer generation ${generation} is excluded only when positively different`, async t => {
    const f = await fixture(t);
    const old = await f.seal({ ...f.partial(), generation });
    const before = await fs.readFile(f.file(producer));
    const record = await f.freeze();
    if (generation === undefined) {
      assert.equal(record, undefined);
      assert.equal(f.calls.coverage, 0);
    } else {
      assert.ok(record);
      assert.equal(record.generation, 7);
      await f.capture(record);
      assert.equal(f.calls.upload, 1);
      assert.ok(f.calls.coverage > 0);
    }
    assert.deepEqual(await fs.readFile(f.file(old.captureId)), before);
  });
}

for (const resource of ["metadataBytes", "adminEntries"] as const) {
  test(`reconstruction shares the existing discovery ${resource} meter`, async t => {
    const f = await fixture(t);
    await f.seal(f.partial(ordinary));
    const result = await MaintenanceResources.runDiscovery(async () => {
      MaintenanceResources.consumeIfActive(resource, resource === "metadataBytes" ? 32 * 1024 ** 2 - 100 : 4096);
      return f.freeze();
    }).catch(() => undefined);
    assert.equal(result, undefined);
    assert.equal(f.calls.coverage, 0);
    assert.ok(f.warnings.some(w => w.message === "recovery: reconstruction provenance incomplete; source retained"));
  });
}

test("earlier producer does not block later ordinary reconstruction during a live pass", async t => {
  const f = await fixture(t);
  await f.seal({ ...f.partial(), generation: 6 });
  const before = await fs.readFile(f.file(producer));
  await f.coordinator.snapshotOwedInventory();
  await f.coordinator.resumeLive({ isExecuting: () => false, authenticatedAtMs: 1000 });
  const records = await f.coordinator.inspect(f.context.runId);
  assert.ok(records.some(r => r.generation === 7 && r.captureId !== producer));
  assert.equal(f.calls.produce, 1);
  assert.equal(f.calls.upload, 1);
  assert.deepEqual(await fs.readFile(f.file(producer)), before);
});

test("ordinary cancellation preserves fresh coordinator boot reconstruction", async t => {
  const f = await fixture(t);
  f.cancel();
  const restarted = f.make();
  await restarted.snapshotOwedInventory();
  const records = await restarted.materializeBootInventory();
  assert.equal(records.length, 1);
  assert.equal(records[0]?.generation, 7);
  assert.ok(f.calls.coverage > 0);
});

test("ordinary cancelled foreground freeze and capture retain existing behavior", async t => {
  const f = await fixture(t);
  f.cancel();
  const record = await f.freeze();
  assert.ok(record);
  assert.match(record.captureId, /^[0-9a-f-]{36}$/);
  await f.capture(record);
  assert.equal(f.calls.produce, 1);
  assert.equal(f.calls.upload, 1);
  assert.ok(f.calls.coverage > 0);
});

test("#3031 U4a F1: only a NEW finalization of a live slot's own record defers; saved FINALs replay", async t => {
  const f = await fixture(t);
  type Guarded = { finalizeInventory(r: RecoveryRecord): Promise<unknown>;
    finalizeInventoryInAttempt(r: RecoveryRecord): Promise<unknown>;
    producerPermit: { run<T>(permit: { captureIds: Set<string>; live: () => boolean }, action: () => T): T } };
  const coordinator = f.make() as unknown as Guarded;
  const reached: string[] = [];
  coordinator.finalizeInventoryInAttempt = async r => { reached.push(r.captureId); return undefined; };
  const fresh = await f.seal({ ...await f.bundled(f.frozen(producer)), state: "uploaded" as const,
    serverCaptureId: "server-capture", reserveIdempotencyKey: ordinary });
  const saved = (kind: "archive" | "held_publication"): RecoveryRecord => kind === "archive"
    ? { ...fresh, finalRequest: { disposition: { kind, capture_id: "server-capture", source_sha: aggregate, coverage_digest: digest } } }
    : { ...f.frozen(), finalRequest: { disposition: { kind, publication_id: publicationId, source_sha: aggregate, coverage_digest: digest } } };
  const inPermit = (live: boolean, r: RecoveryRecord) => coordinator.producerPermit.run(
    { captureIds: new Set([producer]), live: () => live }, () => coordinator.finalizeInventory(r));
  assert.deepEqual(await inPermit(true, fresh), { state: "uploaded", captureId: producer });
  assert.deepEqual(reached, [], "a new finalization of the live slot's own record is deferred");
  for (const kind of ["archive", "held_publication"] as const) {
    const record = await f.seal(saved(kind));
    await inPermit(true, record);
    assert.equal(reached.at(-1), producer, `a saved ${kind} FINAL for the same ID replays through the existing path`);
  }
  await f.seal(fresh);
  reached.length = 0;
  await inPermit(false, fresh);
  assert.deepEqual(reached, [producer], "a dead permit defers nothing");
});

/** Every journal directory open advances the wall clock past the ten-second discovery cap. */
function slowListings(t: test.TestContext, runDir: string) {
  let offset = 0;
  const now = Date.now.bind(Date);
  t.mock.method(Date, "now", () => now() + offset);
  const open = fs.opendir.bind(fs);
  t.mock.method(fs, "opendir", async (...args: Parameters<typeof fs.opendir>) => {
    if (path.resolve(String(args[0])) === runDir) offset += 11_000;
    return open(...args);
  });
}

test("ordinary pin still writes its journal when the listing outlasts the discovery cap", async t => {
  const f = await fixture(t);
  await f.seal({ ...f.partial(ordinary), generation: 6 });
  slowListings(t, path.join(f.recoveryRoot, f.context.runId));
  const pinned = await f.pin();
  assert.ok(pinned, JSON.stringify(f.warnings));
  assert.equal(pinned.generation, 7);
  assert.notEqual(pinned.captureId, ordinary);
  assert.ok((await f.coordinator.inspect(f.context.runId)).some(r => r.captureId === pinned.captureId));
  assert.ok(!f.warnings.some(w => w.message === "recovery: reconstruction provenance incomplete; source retained"));
});

test("a slow listing inside a discovery scope still refuses new reconstruction", async t => {
  const f = await fixture(t);
  await f.seal({ ...f.partial(ordinary), generation: 6 });
  slowListings(t, path.join(f.recoveryRoot, f.context.runId));
  const result = await MaintenanceResources.runDiscovery(() => f.freeze()).catch(() => undefined);
  assert.equal(result, undefined);
  assert.equal(f.calls.coverage, 0);
  assert.ok(f.warnings.some(w => w.message === "recovery: reconstruction provenance incomplete; source retained"));
});

test("a slow ordinary listing still refuses over an unfinished producer origin", async t => {
  const f = await fixture(t);
  await f.seal(f.partial());
  const before = await fs.readFile(f.file(producer));
  slowListings(t, path.join(f.recoveryRoot, f.context.runId));
  assert.equal(await f.pin(), undefined);
  assert.deepEqual((await f.coordinator.inspect(f.context.runId)).map(r => r.captureId), [producer]);
  assert.deepEqual(await fs.readFile(f.file(producer)), before);
});

const tempUuid = "0123abcd-0000-4000-8000-00000000abcd";
for (const temp of [`${ordinary}.json.${tempUuid}.tmp`, `${ordinary}.${tempUuid}.bundle.tmp`]) {
  test(`a stale ordinary temp entry does not block an ordinary pin: ${temp.slice(ordinary.length)}`, async t => {
    const f = await fixture(t);
    await f.seal({ ...f.partial(ordinary), generation: 6 });
    await fs.writeFile(path.join(f.recoveryRoot, f.context.runId, temp), "partial");
    const pinned = await f.pin();
    assert.ok(pinned, JSON.stringify(f.warnings));
    assert.equal(pinned.generation, 7);
    assert.ok(!f.warnings.some(w => w.message === "recovery: reconstruction provenance incomplete; source retained"));
  });
}

test("a stale producer temp entry still refuses an ordinary pin", async t => {
  const f = await fixture(t);
  await f.seal({ ...f.partial(ordinary), generation: 6 });
  await fs.writeFile(path.join(f.recoveryRoot, f.context.runId, `${producer}.json.${tempUuid}.tmp`), "partial");
  assert.equal(await f.pin(), undefined);
  assert.ok(f.warnings.some(w => w.message === "recovery: reconstruction provenance incomplete; source retained"));
});

test("a stale ordinary temp entry still refuses inside a maintenance scope", async t => {
  const f = await fixture(t);
  await f.seal({ ...f.partial(ordinary), generation: 6 });
  await fs.writeFile(path.join(f.recoveryRoot, f.context.runId, `${ordinary}.json.${tempUuid}.tmp`), "partial");
  const result = await MaintenanceResources.runDiscovery(() => f.freeze()).catch(() => undefined);
  assert.equal(result, undefined);
  assert.equal(f.calls.coverage, 0);
});

for (const near of [
  `${ordinary.slice(0, -1)}A.json.${tempUuid}.tmp`,
  `${ordinary}.json.${tempUuid.toUpperCase()}.tmp`,
  `${ordinary}.json.0123abcd-0000-1000-8000-00000000abcd.tmp`,
  `${ordinary}.json.0123abcd-0000-4000-c000-00000000abcd.tmp`,
  `legacy-capture.json.${tempUuid}.tmp`,
  `${ordinary}.json.${tempUuid}.tmp.x`,
  `${ordinary}.json.${tempUuid}.tmp\n`,
  `${ordinary}.${tempUuid}.bundle`,
  `${ordinary}.json.tmp`,
]) {
  test(`a near-miss temp name still refuses an ordinary pin: ${JSON.stringify(near.slice(ordinary.length - 4))}`, async t => {
    const f = await fixture(t);
    await f.seal({ ...f.partial(ordinary), generation: 6 });
    await fs.writeFile(path.join(f.recoveryRoot, f.context.runId, near), "partial");
    assert.equal(await f.pin(), undefined);
  });
}

for (const kind of ["symlink", "directory"] as const) {
  test(`an ordinary temp name that is a ${kind} still refuses an ordinary pin`, async t => {
    const f = await fixture(t);
    await f.seal({ ...f.partial(ordinary), generation: 6 });
    const entry = path.join(f.recoveryRoot, f.context.runId, `${ordinary}.json.${tempUuid}.tmp`);
    if (kind === "symlink") await fs.symlink(f.file(ordinary), entry);
    else await fs.mkdir(entry);
    assert.equal(await f.pin(), undefined);
  });
}

test("an orphaned ordinary bundle whose record was removed does not block an ordinary pin", async t => {
  const f = await fixture(t);
  await f.seal({ ...f.partial(ordinary), generation: 6 });
  await fs.writeFile(path.join(f.recoveryRoot, f.context.runId, `${tempUuid}.bundle`), "orphan");
  const pinned = await f.pin();
  assert.ok(pinned, JSON.stringify(f.warnings));
  assert.equal(pinned.generation, 7);
});

test("an orphaned producer bundle still refuses an ordinary pin", async t => {
  const f = await fixture(t);
  await f.seal({ ...f.partial(ordinary), generation: 6 });
  await fs.writeFile(path.join(f.recoveryRoot, f.context.runId, `${producer}.bundle`), "orphan");
  assert.equal(await f.pin(), undefined);
});

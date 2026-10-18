import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import { writeFileSync } from "node:fs";
import path from "node:path";
import { createHash, createHmac } from "node:crypto";
import { RecoveryCoordinator, canonicalJson, type ClosedSourceObservation, type RecoveryRecord, type RecoveryArchiveClient } from "../src/recovery.js";
import { setImmediate as turn } from "node:timers/promises";
import { MaintenanceBudget } from "../src/maintenance-budget.js";
import { MaintenanceResources, RecoveryMaintenance } from "../src/recovery-maintenance.js";
import type { PositiveOwedCandidateContext } from "../src/git.js";
import type { RecoveryExactHold, RecoveryFinalDisposition } from "../src/protocol.js";
import { nullLogger } from "./helpers.js";

const producer = "maintenance-v1-00000000-0000-4000-8000-000000000001";
const ordinary = "00000000-0000-4000-8000-000000000002";
const holdId = "00000000-0000-4000-8000-000000000007";
const repoId = "00000000-0000-4000-8000-000000000008";
const publicationId = "00000000-0000-4000-8000-000000000009";
const source = "a".repeat(40), aggregate = "b".repeat(40), digest = "c".repeat(64);
const token = "source-pass-fixture";

async function fixture(t: test.TestContext) {
  const root = await fs.mkdtemp(path.resolve("../.uzi/scratch/source-pass-"));
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
  let executing = false;
  let executionCheck: (() => boolean) | undefined;
  const make = () => new RecoveryCoordinator({
    isExecuting: () => executionCheck?.() ?? executing,
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
    executing: (value: boolean) => { executing = value; },
    onExecutionCheck: (check: () => boolean) => { executionCheck = check; },
    settle: (exact: RecoveryExactHold) => { settled = exact; } };
}


// Real admission exercises the production accounting context without enabling a producer.
async function journalSlot(t: test.TestContext, f: Awaited<ReturnType<typeof fixture>>,
  signal?: AbortSignal, now?: () => number) {
  const ledger = new RecoveryMaintenance({ root: path.join(f.root, "recovery-maintenance"),
    workerToken: token, now });
  const admitted = await ledger.admit({
    identity: { run: f.context.runId, generation: 7, hold: holdId, repo: repoId, custodian: "worker",
      bareBasename: "repo.git", origin: digest, originalSource: digest, sourceConfig: digest },
    config: RecoveryMaintenance.configuration(), exactCustody: true, terminalNonStopped: true, source: digest,
  }, { signal });
  assert.ok(admitted.ok);
  t.after(() => ledger.finish(admitted.value, "nonmutating"));
  return admitted.value;
}

test("active journal reader charges actual bytes on every nested inspect and keeps body quotas separate", async t => {
  const f = await fixture(t);
  await f.seal(f.partial(ordinary));
  const slot = await journalSlot(t, f);
  const size = (await fs.stat(f.file(ordinary))).size;
  const nameBytes = Buffer.byteLength(ordinary + ".json");
  await MaintenanceResources.run(slot, async () => {
    MaintenanceResources.consume("metadataBytes", 256 * 1024 ** 2 - 2 * (size + nameBytes));
    assert.equal((await f.coordinator.inspect(f.context.runId)).length, 1);
    assert.equal((await f.coordinator.inspect(f.context.runId)).length, 1);
    MaintenanceResources.consume("decoded", 8 * 1024 ** 3);
    MaintenanceResources.consume("artifactBytes", 256 * 1024 ** 2);
    await assert.rejects(f.coordinator.inspect(f.context.runId), /maintenance_resource_exhausted/);
  });
});

test("active journal byte exhaustion refuses actual read bytes before decode and delivery", async t => {
  const f = await fixture(t);
  await f.seal(f.partial(ordinary));
  const size = (await fs.stat(f.file(ordinary))).size;
  const reads = await observeJournalReads(t, f.file(ordinary), async () => {});
  const slot = await journalSlot(t, f);
  await MaintenanceResources.run(slot, async () => {
    MaintenanceResources.consume("metadataBytes",
      256 * 1024 ** 2 - Buffer.byteLength(ordinary + ".json") - size + 1);
    await assert.rejects(f.coordinator.inspect(f.context.runId), /maintenance_resource_exhausted|maintenance_token_invalid/);
    assert.equal(reads.reduce((sum, r) => sum + r.bytes, 0), size);
    assert.equal(MaintenanceResources.signalIfActive()?.aborted, true);
  });
});

test("active journal growing after stat stays within the one-MiB read sentinel", async t => {
  const f = await fixture(t);
  await f.seal(f.partial(ordinary));
  const reads = await observeJournalReads(t, f.file(ordinary), async () => {}, async () => {
    await fs.appendFile(f.file(ordinary), " ".repeat(2 * 1024 ** 2));
  });
  const slot = await journalSlot(t, f);
  await MaintenanceResources.run(slot, async () => {
    assert.deepEqual(await f.coordinator.inspect(f.context.runId), []);
    assert.equal(reads.reduce((sum, r) => sum + r.bytes, 0), 1024 * 1024 + 1);
    MaintenanceResources.consume("metadataBytes",
      256 * 1024 ** 2 - Buffer.byteLength(ordinary + ".json") - (1024 * 1024 + 1));
    assert.throws(() => MaintenanceResources.consume("metadataBytes", 1), /maintenance_resource_exhausted/);
  });
});

for (const strict of [false, true]) {
  test(`active ${strict ? "strict" : "advisory"} journal enumeration charges raw non-JSON visits cumulatively`, async t => {
    const f = await fixture(t);
    await f.seal(f.partial(ordinary));
    seedRawNames(path.dirname(f.file(ordinary)), 4097);
    const io = observeJournalDirectory(t, path.dirname(f.file(ordinary)));
    const slot = await journalSlot(t, f);
    await MaintenanceResources.run(slot, async () => {
      const read = () => strict ? f.coordinator.inventoryCleanupState(f.context.runId, 7)
        : f.coordinator.inspect(f.context.runId);
      await assert.rejects(read(), /maintenance_resource_exhausted/);
      assert.equal(io.wholeListings(), 0);
      assert.equal(io.listings.reduce((sum, n) => sum + n, 0), 4096);
      await assert.rejects(read(), /maintenance_token_invalid/);
    });
  });
}

test("active journal reader bounds oversized files and rejects symlinks while unscoped inspect stays advisory", async t => {
  const f = await fixture(t);
  await f.seal(f.partial(ordinary));
  const huge = f.file("huge");
  await fs.writeFile(huge, " ".repeat(1024 * 1024 + 1));
  await fs.symlink(f.file(ordinary), f.file("alias"));
  const reads = await observeJournalReads(t, huge, async () => {});
  const slot = await journalSlot(t, f);
  await MaintenanceResources.run(slot, async () => {
    assert.deepEqual((await f.coordinator.inspect(f.context.runId)).map(r => r.captureId), [ordinary]);
    assert.ok(reads.every(r => r.bytes === 0), "oversize is rejected before reading");
    await assert.rejects(f.coordinator.inventoryCleanupState(f.context.runId, 7), /unverified recovery journal/);
    const target = path.join(f.root, ordinary + ".json");
    await fs.rename(f.file(ordinary), target);
    await fs.symlink(target, f.file(ordinary));
    assert.deepEqual(await f.coordinator.inspect(f.context.runId), [], "exact journal symlink cannot deliver valid target bytes");
  });
  assert.equal((await f.coordinator.inspect(f.context.runId)).length, 1);
});

for (const stop of ["signal", "clock", "parent"] as const) {
  test(`active journal reader checks inherited ${stop} after actual read settlement`, async t => {
    const f = await fixture(t);
    await f.seal(f.partial(ordinary));
    const controller = new AbortController();
    let wall = 1_000_000, parentNow = 0;
    const slot = await journalSlot(t, f, controller.signal, () => wall);
    const parent = new MaintenanceBudget(1000, () => parentNow);
    t.after(() => parent.dispose());
    await observeJournalReads(t, f.file(ordinary), async () => {}, undefined, async () => {
      if (stop === "signal") controller.abort();
      if (stop === "clock") wall += 120_000;
      if (stop === "parent") parentNow = 1000;
    });
    await assert.rejects(MaintenanceBudget.run(parent, () =>
      MaintenanceResources.run(slot, () => f.coordinator.inspect(f.context.runId))),
    /maintenance|abort/i);
  });
}

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>(r => { resolve = r; });
  return { promise, resolve };
}
function observation(terminal: ClosedSourceObservation["terminal"] = "normal_failed"): ClosedSourceObservation {
  const genesis = { runId: "run-origin", generation: 7, clonePath: "/fixture/clone", attemptId: "fixture" };
  return { genesis, terminal, family: { originals: [{ ...genesis, identity: "fixture-identity" }],
    observedExecution: { runId: genesis.runId, generation: 7 }, metadata: "fixture-metadata" } };
}
function request(f: Awaited<ReturnType<typeof fixture>>, source = observation()) {
  const tail = deferred<ClosedSourceObservation | void>();
  const pass = f.coordinator.resumeTerminalSource(f.context.runId, tail.promise, 7);
  tail.resolve(source);
  return pass;
}

// Count bytes returned by the real filesystem before journal parsing/authentication.
// Each entry represents one open/readFile, including repeated reads on its handle.
async function observeJournalReads(t: test.TestContext, filename: string,
  beforeOpen: (ordinal: number) => Promise<void>,
  afterStat?: (ordinal: number) => Promise<void>,
  afterRead?: (ordinal: number) => Promise<void>) {
  const reads: Array<{ bytes: number }> = [];
  const begin = async () => {
    const entry = { bytes: 0 };
    reads.push(entry);
    const ordinal = reads.length;
    await beforeOpen(ordinal);
    return { entry, ordinal };
  };
  const readFile = fs.readFile.bind(fs);
  t.mock.method(fs, "readFile", async (...args: Parameters<typeof fs.readFile>) => {
    const observed = args[0] === filename ? await begin() : undefined;
    const result = await readFile(...args);
    if (observed) observed.entry.bytes += Buffer.byteLength(result);
    return result;
  });
  const open = fs.open.bind(fs);
  t.mock.method(fs, "open", async (...args: Parameters<typeof fs.open>) => {
    const observed = args[0] === filename ? await begin() : undefined;
    const file = await open(...args);
    if (!observed) return file;
    const read = file.read.bind(file), readFile = file.readFile.bind(file), stat = file.stat.bind(file);
    t.mock.method(file, "read", async (...args: unknown[]) => {
      const result = await Reflect.apply(read, file, args);
      observed.entry.bytes += result.bytesRead;
      await afterRead?.(observed.ordinal);
      return result;
    });
    t.mock.method(file, "readFile", async (...args: Parameters<typeof file.readFile>) => {
      const result = await readFile(...args);
      observed.entry.bytes += Buffer.byteLength(result);
      return result;
    });
    let firstStat = true;
    t.mock.method(file, "stat", async (...args: Parameters<typeof file.stat>) => {
      const result = await stat(...args);
      if (firstStat) { firstStat = false; await afterStat?.(observed.ordinal); }
      return result;
    });
    return file;
  });
  return reads;
}

// Observe real directory IO, without manufacturing an entry or a returned name array.
function observeJournalDirectory(t: test.TestContext, dir: string) {
  const listings: number[] = [];
  const wholeNames: number[] = [];
  const readdir = fs.readdir.bind(fs), opendir = fs.opendir.bind(fs);
  t.mock.method(fs, "readdir", async (...args: Parameters<typeof fs.readdir>) => {
    const actual = await readdir(...args);
    if (args[0] === dir) wholeNames.push(actual.length);
    return actual;
  });
  t.mock.method(fs, "opendir", async (...args: Parameters<typeof fs.opendir>) => {
    const directory = await opendir(...args);
    if (args[0] !== dir) return directory;
    const index = listings.push(0) - 1;
    const read = directory.read.bind(directory);
    t.mock.method(directory, "read", async () => {
      const actual = await read();
      if (actual) listings[index] = listings[index]! + 1;
      return actual;
    });
    return directory;
  });
  return { listings, wholeListings: () => wholeNames.length, wholeNames };
}

function seedRawNames(dir: string, count = 10_000) {
  for (let i = 0; i < count; i++) writeFileSync(path.join(dir, "non-json-" + i), "");
}

test("real late raw names cannot turn an incomplete cycleKey listing into an unguarded replay", async t => {
  const f = await fixture(t);
  const record = await f.seal(await f.bundled(f.partial(ordinary)));
  const filename = f.file(ordinary), dir = path.dirname(filename);
  const original = await fs.readFile(filename), body = await fs.readFile(record.bundlePath!);
  const io = observeJournalDirectory(t, dir);
  let seeded = false;
  f.onExecutionCheck(() => {
    if (!seeded) { seeded = true; seedRawNames(dir); }
    return false;
  });
  await request(f);
  assert.equal(seeded, true, "the callback runs after initial discovery");
  assert.deepEqual(io.wholeNames, [], "lock selection must never buffer the whole directory");
  assert.equal(io.listings[0], 2, "discovery sees only the original journal and bundle");
  assert.ok(io.listings.length >= 2, "fresh lock selection reopens the real directory");
  assert.ok(io.listings[1]! > 4000);
  assert.ok(io.listings.reduce((a, b) => a + b, 0) <= 4096, "raw names share the discovery counter");
  assert.equal(f.calls.upload, 0);
  assert.equal(f.calls.final, 0);
  assert.equal(f.calls.reserve, 0, "an incomplete lock key cannot dispatch");
  assert.equal(f.calls.produce, 0);
  t.mock.restoreAll();
  assert.deepEqual(await fs.readFile(filename), original);
  assert.deepEqual(await fs.readFile(record.bundlePath!), body);
  for (let i = 0; i < 10_000; i++) await fs.unlink(path.join(dir, "non-json-" + i));
  await f.coordinator.resumePending();
  assert.equal(f.calls.upload, 1);
  assert.deepEqual(f.uploaded, [body]);
});

for (const kind of ["archive", "held_publication"] as const) {
  test(`real late raw names stop fresh guarded ${kind} FINAL completeness proof`, async t => {
    const f = await fixture(t);
    const record = { ...await f.bundled(f.frozen(ordinary)), state: "uploaded" as const,
      serverCaptureId: "server-capture" };
    const disposition: RecoveryFinalDisposition = kind === "archive"
      ? { kind, capture_id: "server-capture", source_sha: aggregate, coverage_digest: digest }
      : { kind, publication_id: publicationId, source_sha: aggregate, coverage_digest: digest };
    const finalRecord: RecoveryRecord = { ...record, finalRequest: { disposition } };
    if (kind === "held_publication") {
      const exact = (await f.client.getExactRecoveryHold!(record.runId, 7, record.exactHoldBinding!))!;
      const closed: RecoveryExactHold = { ...exact, state: "released", final_receipt: disposition,
        release_evidence: "held_publication" };
      delete closed.live_run_id; delete closed.live_worker_id;
      finalRecord.holdSettlement = closed;
      f.settle(closed);
    }
    await f.seal(finalRecord);
    const filename = f.file(ordinary), dir = path.dirname(filename);
    const original = await fs.readFile(filename), body = await fs.readFile(record.bundlePath!);
    const io = observeJournalDirectory(t, dir);
    const enumerate = f.git.enumerateOwedCandidates;
    let seeded = false;
    t.mock.method(f.git, "enumerateOwedCandidates", async () => {
      const result = await enumerate();
      assert.equal(MaintenanceResources.active(), false, "fresh proof is outside the discovery timer");
      if (!seeded) { seeded = true; seedRawNames(dir); }
      return result;
    });
    await request(f);
    assert.equal(seeded, true, "the mutation occurs during fresh FINAL proof");
    assert.deepEqual(io.wholeNames, [], "omitted strict budget must still stream");
    assert.ok(io.listings.some(count => count > 4000), "strict proof visits actual late raw names");
    assert.ok(io.listings.reduce((a, b) => a + b, 0) <= 4096);
    assert.equal(f.calls.final, 0);
    assert.equal(f.calls.upload, 0);
    assert.equal(f.calls.publish, 0);
    assert.equal(f.calls.produce, 0);
    t.mock.restoreAll();
    assert.deepEqual(await fs.readFile(filename), original, "FINAL journal and original roots are retained");
    assert.deepEqual(await fs.readFile(record.bundlePath!), body);
  });
}

test("nested repeated advisory listings share discovery raw-name accounting without resetting", async t => {
  const f = await fixture(t);
  const record = await f.seal(await f.bundled(f.partial(ordinary)));
  const dir = path.dirname(f.file(ordinary)), body = await fs.readFile(record.bundlePath!);
  seedRawNames(dir, 600);
  const io = observeJournalDirectory(t, dir);
  let completed = 0;
  f.client.reserveRecoveryCapture = async () => {
    for (let i = 0; i < 12; i++) {
      assert.equal((await f.coordinator.inspect(record.runId)).length, 1);
      completed++;
    }
    return { capture_id: "server-capture", state: "preparing" };
  };
  await request(f);
  assert.equal(io.wholeListings(), 0);
  assert.ok(completed > 0 && completed < 12, "per-call reset would allow every complete listing");
  assert.ok(io.listings.length > 3);
  const total = io.listings.reduce((a, b) => a + b, 0);
  assert.ok(total > 4000 && total <= 4096, "raw non-JSON visits share the reserved read ceiling");
  assert.equal(f.calls.upload, 0);
  assert.equal(f.calls.final, 0);
  t.mock.restoreAll();
  assert.ok(await fs.stat(f.file(ordinary)));
  assert.deepEqual(await fs.readFile(record.bundlePath!), body);
});

test("retained bundle payload above metadata quota still uploads its original bytes", async t => {
  const f = await fixture(t);
  const record = await f.bundled(f.partial(ordinary));
  // The journal binds these opaque archive bytes; the worker never decodes them here.
  const body = Buffer.concat([await fs.readFile(record.bundlePath!), Buffer.alloc(33 * 1024 ** 2)]);
  await fs.writeFile(record.bundlePath!, body);
  await f.seal({ ...record, byteSize: body.length,
    checksum: createHash("sha256").update(body).digest("hex"),
    chunkCount: Math.ceil(body.length / (1024 * 1024)) });
  const reads = await observeJournalReads(t, f.file(ordinary), async () => {});
  await request(f);
  assert.equal(f.calls.upload, 1);
  assert.deepEqual(f.uploaded, [body]);
  assert.ok(reads.reduce((sum, read) => sum + read.bytes, 0) < 1024 * 1024);
  assert.equal(f.calls.produce, 0);
});

test("nested valid sub-MiB journals exhaust actual aggregate metadata before another body read", async t => {
  const f = await fixture(t);
  const record = await f.seal(await f.bundled(f.partial(ordinary)));
  const filename = f.file(ordinary), body = await fs.readFile(record.bundlePath!);
  const original = await fs.readFile(filename);
  // JSON whitespace keeps the original ARC and its MAC valid.
  await fs.appendFile(filename, " ".repeat(900_000 - original.length));
  const reads = await observeJournalReads(t, filename, async () => {});
  let completed = 0;
  f.client.reserveRecoveryCapture = async () => {
    for (let i = 0; i < 45; i++) {
      assert.equal((await f.coordinator.inspect(record.runId)).length, 1);
      completed++;
    }
    return { capture_id: "server-capture", state: "preparing" };
  };
  await request(f);
  assert.ok(completed > 0 && completed < 45);
  assert.ok(reads.reduce((sum, read) => sum + read.bytes, 0) <= 32 * 1024 ** 2 + 1);
  assert.equal(reads.at(-1)!.bytes, 0, "known body exceeds remaining quota: no body read");
  assert.equal(f.calls.upload, 0);
  assert.equal(f.calls.final, 0);
  t.mock.restoreAll();
  assert.deepEqual(await fs.readFile(record.bundlePath!), body);
});

for (const id of [ordinary, producer]) {
  for (const ordinal of [2, 4]) {
    for (const mode of ["whitespace", "invalid", "growth", "symlink"] as const) {
      test(`saved journal reopen ${ordinal} ${mode} refuses bounded bytes for ${id}`, async t => {
        const f = await fixture(t);
        const record = await f.seal(await f.bundled(f.partial(id)));
        const filename = f.file(id);
        const original = await fs.readFile(filename);
        const body = await fs.readFile(record.bundlePath!);
        const target = path.join(f.root, "valid-record");
        await fs.writeFile(target, original);
        const oversized = mode === "invalid" ? "{" + "x".repeat(33 * 1024 ** 2)
          : "{}" + " ".repeat(33 * 1024 ** 2);
        const replace = async () => {
          if (mode === "symlink") { await fs.unlink(filename); await fs.symlink(target, filename); }
          else await fs.writeFile(filename, oversized);
        };
        const reads = await observeJournalReads(t, filename,
          async n => { if (n === ordinal && mode !== "growth") await replace(); },
          async n => { if (n === ordinal && mode === "growth") await replace(); });
        await request(f);
        assert.ok(reads.length >= ordinal, "the selected nested reopen must execute");
        const replay = reads.slice(ordinal - 1);
        assert.ok(replay[0]);
        if (mode === "growth") assert.equal(replay[0].bytes, 1024 * 1024 + 1, "growth stops at the overflow byte");
        else assert.equal(replay[0].bytes, 0, "oversize stat or no-follow rejects before reading bytes");
        assert.ok(replay.every(r => r.bytes <= 1024 * 1024 + 1));
        assert.equal(f.calls.upload, 0);
        assert.equal(f.calls.final, 0);
        assert.equal(f.calls.produce, 0);
        assert.equal(f.calls.publish, 0);
        // Restore instrumentation before inspecting retained evidence.
        t.mock.restoreAll();
        assert.ok(await fs.lstat(filename));
        assert.deepEqual(await fs.readFile(record.bundlePath!), body);
        assert.ok(f.warnings.every(w => w.fields === undefined), "refusal diagnostics contain no file content or identity");
      });
    }
  }
}

for (const kind of ["archive", "held_publication"] as const) {
  for (const mode of ["whitespace", "growth", "symlink"] as const) {
    test(`saved ${kind} FINAL journal proof refuses late ${mode} replacement`, async t => {
      const f = await fixture(t);
      let record: RecoveryRecord = await f.bundled(f.frozen(ordinary));
      record = { ...record, state: "uploaded", serverCaptureId: "server-capture" };
      const disposition: RecoveryFinalDisposition = kind === "archive"
        ? { kind, capture_id: "server-capture", source_sha: aggregate, coverage_digest: digest }
        : { kind, publication_id: publicationId, source_sha: aggregate, coverage_digest: digest };
      record.finalRequest = { disposition };
      if (kind === "held_publication") {
        const exact = (await f.client.getExactRecoveryHold!(record.runId, 7, record.exactHoldBinding!))!;
        const closed: RecoveryExactHold = { ...exact, state: "released", final_receipt: disposition,
          release_evidence: "held_publication" };
        delete closed.live_run_id; delete closed.live_worker_id;
        record.holdSettlement = closed;
        f.settle(closed);
      }
      await f.seal(record);
      const filename = f.file(ordinary), target = path.join(f.root, "valid-final");
      await fs.copyFile(filename, target);
      const body = await fs.readFile(record.bundlePath!);
      let armed = false, replacedAt = 0;
      const coverage = f.git.buildRecoveryCoverage;
      t.mock.method(f.git, "buildRecoveryCoverage", async () => {
        const result = await coverage();
        armed = true;
        return result;
      });
      const replace = async () => {
        if (mode === "symlink") { await fs.unlink(filename); await fs.symlink(target, filename); }
        else await fs.writeFile(filename, "{}" + " ".repeat(33 * 1024 ** 2));
      };
      const reads = await observeJournalReads(t, filename, async n => {
        if (!armed || replacedAt) return;
        replacedAt = n;
        if (mode !== "growth") await replace();
      }, async n => { if (mode === "growth" && n === replacedAt) await replace(); });
      await request(f);
      assert.ok(replacedAt > 4, "replacement occurs during fresh FINAL proofs after initial nested reads");
      const replay = reads.slice(replacedAt - 1);
      assert.ok(replay[0]);
      assert.equal(replay[0].bytes, mode === "growth" ? 1024 * 1024 + 1 : 0);
      assert.ok(replay.every(r => r.bytes <= 1024 * 1024 + 1));
      assert.equal(f.calls.final, 0);
      assert.equal(f.calls.upload, 0);
      assert.equal(f.calls.produce, 0);
      assert.equal(f.calls.publish, 0);
      t.mock.restoreAll();
      assert.ok(await fs.lstat(filename));
      assert.deepEqual(await fs.readFile(record.bundlePath!), body);
    });
  }
}

test("saved journal delayed read settlement remains joined by shutdown", async t => {
  const f = await fixture(t);
  const record = await f.seal(await f.bundled(f.partial(ordinary)));
  const entered = deferred<void>(), release = deferred<void>();
  let delayed = false;
  await observeJournalReads(t, f.file(ordinary), async () => {}, undefined, async n => {
    if (n !== 2 || delayed) return;
    delayed = true;
    entered.resolve();
    await release.promise;
  });
  const pass = request(f);
  await entered.promise;
  let stopped = false;
  const shutdown = f.coordinator.shutdown().then(() => { stopped = true; });
  await turn();
  assert.equal(stopped, false, "shutdown joins actual filesystem settlement after abort");
  release.resolve();
  await Promise.all([pass, shutdown]);
  assert.equal(f.calls.upload, 0);
  assert.equal(f.calls.final, 0);
  assert.equal(f.calls.produce, 0);
  t.mock.restoreAll();
  assert.ok(await fs.stat(f.file(ordinary)));
  assert.ok(await fs.stat(record.bundlePath!));
});

for (const id of [ordinary, producer, "maintenance"]) {
  test(`terminal saved original ${id} uploads exact bytes despite malformed sibling`, async t => {
    const f = await fixture(t);
    const record = await f.seal(await f.bundled(f.partial(id)));
    const body = await fs.readFile(record.bundlePath!);
    await fs.writeFile(f.file("maintenance-v1-invalid"), "{invalid");
    await request(f);
    assert.deepEqual(f.uploaded, [body]);
    assert.equal(f.calls.produce, 0);
    assert.equal(f.calls.coverage, 0);
    assert.equal(MaintenanceResources.active(), false);
  });
}

test("source pass synchronously owns the exact pending tail and shutdown joins it", async t => {
  const f = await fixture(t);
  await f.seal(await f.bundled(f.partial(ordinary)));
  const tail = deferred<ClosedSourceObservation | void>();
  const pass = f.coordinator.resumeTerminalSource(f.context.runId, tail.promise, 7);
  await turn(); // dispatch has begun and is waiting for the exact pending promise
  let joined = false;
  const drain = f.coordinator.shutdown().then(() => { joined = true; });
  await turn();
  assert.equal(joined, false);
  tail.resolve(observation());
  await Promise.all([pass, drain]);
  assert.equal(f.calls.upload, 0);
});

test("source pass ownership exists before its dispatch microtask", async t => {
  const f = await fixture(t);
  const tail = deferred<ClosedSourceObservation | void>();
  const pass = f.coordinator.resumeTerminalSource(f.context.runId, tail.promise, 7);
  // trackPass owns the task immediately; its established abort-before-dispatch rule skips work.
  await f.coordinator.shutdown();
  await pass;
  assert.equal(f.calls.reserve, 0);
  tail.resolve(observation());
});

test("busy boot pass cannot drop a source pass; shutdown aborts and joins delayed child", async t => {
  const f = await fixture(t);
  const first = await f.seal(await f.bundled(f.partial(ordinary)));
  const secondId = "00000000-0000-4000-8000-000000000003";
  await f.seal(await f.bundled(f.partial(secondId)));
  const bootEntered = deferred<void>(), bootRelease = deferred<void>();
  const sourceEntered = deferred<void>(), sourceRelease = deferred<void>();
  let sourceSignal: AbortSignal | undefined;
  let reservations = 0;
  const upload = f.client.uploadRecoveryBundle;
  f.client.uploadRecoveryBundle = async (run, id, manifest, stream, signal) => {
    reservations++;
    const result = await upload(run, id, manifest, stream, signal);
    if (reservations === 1) { bootEntered.resolve(); await bootRelease.promise; }
    else { sourceSignal = signal; sourceEntered.resolve(); await sourceRelease.promise; }
    return result;
  };
  const boot = f.coordinator.resumePending(undefined, [first]);
  await bootEntered.promise;
  const pass = request(f);
  // Source pass may wait on the boot record's existing cycle lock.
  bootRelease.resolve();
  const enteredBeforeFinished = await Promise.race([
    sourceEntered.promise.then(() => true), pass.then(() => false),
  ]);
  assert.equal(enteredBeforeFinished, true, "busy admission must not drop the registered source pass");
  let joined = false;
  const drain = f.coordinator.shutdown().then(() => { joined = true; });
  await turn();
  assert.equal(sourceSignal?.aborted, true);
  assert.equal(joined, false);
  sourceRelease.resolve();
  await Promise.all([boot, pass, drain]);
  assert.equal(reservations, 2);
});

for (const mode of ["park", "undefined", "wrong-run", "wrong-generation", "no-family", "no-tag"] as const) {
  test(`source observation refuses ${mode}`, async t => {
    const f = await fixture(t);
    await f.seal(await f.bundled(f.partial(ordinary)));
    let value: ClosedSourceObservation | void = observation();
    if (mode === "park") value = observation("normal_park");
    if (mode === "undefined") value = undefined;
    if (mode === "wrong-run") value = { ...observation(), genesis: { ...observation().genesis, runId: "other" } };
    if (mode === "wrong-generation") value = { ...observation(), family: { ...observation().family!,
      observedExecution: { runId: f.context.runId, generation: 8 } } };
    if (mode === "no-family") value = { ...observation(), family: undefined };
    if (mode === "no-tag") value = { ...observation(), terminal: undefined } as unknown as ClosedSourceObservation;
    await f.coordinator.resumeTerminalSource(f.context.runId, Promise.resolve(value), 7);
    assert.equal(f.calls.upload, 0);
    assert.equal(f.calls.produce, 0);
  });
}

test("queued successor's existing reservation conservatively defers saved work", async t => {
  const f = await fixture(t);
  await f.seal(await f.bundled(f.partial(ordinary)));
  const tail = deferred<ClosedSourceObservation | void>();
  const pass = f.coordinator.resumeTerminalSource(f.context.runId, tail.promise, 7);
  f.executing(true);
  tail.resolve(observation());
  await pass;
  assert.equal(f.calls.reserve, 0);
  assert.equal(f.calls.upload, 0);
  f.executing(false);
  await request(f);
  assert.equal(f.calls.upload, 1);
});

for (const empty of [true, false]) {
  test(`saved pass does not bootstrap ${empty ? "empty set" : "metadata-only pin"}; foreground remains eligible`, async t => {
    const f = await fixture(t);
    if (!empty) await f.seal(f.partial(ordinary));
    await request(f);
    assert.equal((await f.coordinator.inspect(f.context.runId)).length, empty ? 0 : 1);
    assert.equal(f.calls.produce, 0);
    assert.equal(f.calls.coverage, 0);
    const pinned = await f.pin();
    assert.ok(pinned);
    await f.capture(pinned);
    assert.equal(f.calls.produce, 1);
    assert.equal(f.calls.upload, 1);
  });
}

for (const kind of ["archive", "held_publication"] as const) {
  test(`saved ${kind} FINAL uses existing full proofs and matching receipt`, async t => {
    const f = await fixture(t);
    let record = f.frozen(ordinary);
    if (kind === "archive") record = { ...await f.bundled(record), state: "uploaded", serverCaptureId: "server-capture" };
    const disposition: RecoveryFinalDisposition = kind === "archive"
      ? { kind, capture_id: "server-capture", source_sha: aggregate, coverage_digest: digest }
      : { kind, publication_id: publicationId, source_sha: aggregate, coverage_digest: digest };
    record.finalRequest = { disposition };
    if (kind === "held_publication") {
      const exact = (await f.client.getExactRecoveryHold!(record.runId, 7, record.exactHoldBinding!))!;
      const closed: RecoveryExactHold = { ...exact, state: "released", final_receipt: disposition, release_evidence: "held_publication" };
      delete closed.live_run_id; delete closed.live_worker_id;
      record.holdSettlement = closed;
      f.settle(closed);
    }
    await f.seal(record);
    await fs.appendFile(f.file(ordinary), " ".repeat(128 * 1024));
    assert.ok((await fs.stat(f.file(ordinary))).size < 1024 * 1024);
    await request(f);
    assert.equal(f.calls.final, 1);
    assert.ok(f.calls.coverage > 0);
    assert.deepEqual(f.finals, [disposition]);
    assert.equal(f.calls.produce, 0);
    assert.equal(f.calls.publish, 0);
    assert.equal((await f.coordinator.inspect(record.runId))[0]?.finalAcknowledged, true);
  });
}

test("ordinary uncertain held readback runs before the saved-only production denial", async t => {
  const f = await fixture(t);
  await f.seal({ ...f.frozen(ordinary), heldCreateUncertain: true });
  await request(f);
  assert.equal(f.calls.publish, 1);
  assert.equal(f.calls.produce, 0);
  assert.equal(f.calls.prepare, 0);
  assert.equal(f.calls.coverage, 0);
});

test("immutable guarded bundle can finish archive FINAL without new production", async t => {
  const f = await fixture(t);
  await f.seal(await f.bundled(f.frozen(ordinary)));
  await request(f);
  assert.equal(f.calls.upload, 1);
  assert.equal(f.calls.final, 1);
  assert.ok(f.calls.coverage > 0);
  assert.equal(f.calls.produce, 0);
});


test("saved-only public reentrant pin and freeze refuse even an empty inventory", async t => {
  const f = await fixture(t);
  await f.seal(await f.bundled(f.partial(ordinary)));
  let attempted = false;
  f.client.reserveRecoveryCapture = async () => {
    attempted = true;
    assert.equal(await f.coordinator.pin({ runId: "empty-run", generation: 7,
      sourceSha: source, kind: "issue", branch: "work" }), undefined);
    assert.equal(await f.coordinator.freezeInventory({
      context: { ...f.context, runId: "empty-run" }, currentSha: source, defaultBranch: "main",
    }), undefined);
    return { capture_id: "server-capture", state: "preparing" };
  };
  await request(f);
  assert.equal(attempted, true);
  await assert.rejects(fs.stat(path.join(f.recoveryRoot, "empty-run")), { code: "ENOENT" });
  assert.equal(f.calls.coverage, 0);
  assert.equal(f.calls.produce, 0);
  assert.ok(await f.coordinator.pin({ runId: "empty-run", generation: 7,
    sourceSha: source, kind: "issue", branch: "work" }));
});

for (const route of ["boot", "live", "foreground"] as const) {
test(`raw-name discovery exhaustion denies additional replay; ${route} retries independently`, async t => {
  const f = await fixture(t);
  const record = await f.seal(await f.bundled(f.partial(ordinary)));
  const original = await fs.readFile(f.file(ordinary));
  const body = await fs.readFile(record.bundlePath!);
  const open = fs.opendir.bind(fs);
  let rawNames = 0;
  t.mock.method(fs, "opendir", async (...args: Parameters<typeof fs.opendir>) => {
    const directory = await open(...args);
    const read = directory.read.bind(directory);
    t.mock.method(directory, "read", async () => {
      const actual = await read();
      if (actual) return actual;
      rawNames++;
      return { name: "maintenance-v1-invalid-" + rawNames } as never;
    });
    return directory;
  });
  const upload = f.client.uploadRecoveryBundle;
  f.client.uploadRecoveryBundle = async (...args) => {
    assert.equal(MaintenanceResources.active(), false, "discovery quota cannot govern saved replay proofs");
    return upload(...args);
  };
  await request(f);
  assert.ok(rawNames > 4000 && rawNames <= 4096, "raw non-JSON entries exhaust the admin budget");
  assert.equal(f.calls.upload, 0);
  assert.equal(f.calls.final, 0);
  assert.equal(f.calls.produce, 0);
  assert.ok(f.warnings.some(w => w.message === "recovery: terminal saved evidence discovery incomplete"));
  t.mock.restoreAll();
  assert.deepEqual(await fs.readFile(f.file(ordinary)), original);
  assert.deepEqual(await fs.readFile(record.bundlePath!), body);
  if (route === "boot") await f.coordinator.resumePending();
  else if (route === "live") await f.coordinator.resumeLive({ isExecuting: () => false, authenticatedAtMs: 1000 });
  else await f.capture(record);
  // Ordinary live recovery also discovers the fixture's independent owed inventory.
  assert.ok(f.uploaded.some(bytes => bytes.equals(body)), "independent retry uploads the exact original bytes");
  if (route !== "live") assert.equal(f.calls.upload, 1);
});
}

test("ordinary held FINAL definitive fallback cannot clear evidence or produce new bytes in saved pass", async t => {
  const f = await fixture(t);
  const record = f.frozen(ordinary);
  record.finalRequest = { disposition: { kind: "held_publication", publication_id: publicationId,
    source_sha: aggregate, coverage_digest: digest } };
  await f.seal(record);
  f.refuseFinal();
  await request(f);
  const after = (await f.coordinator.inspect(record.runId))[0]!;
  assert.equal(f.calls.final, 1);
  assert.ok(f.calls.coverage > 0);
  assert.deepEqual(after.finalRequest, record.finalRequest);
  assert.equal(f.calls.produce, 0);
  assert.equal(f.calls.publish, 0);
});


test("FINAL cleared concurrently after saved replay still cannot fall through to new evidence", async t => {
  const f = await fixture(t);
  const record = f.frozen(ordinary);
  record.finalRequest = { disposition: { kind: "held_publication", publication_id: publicationId,
    source_sha: aggregate, coverage_digest: digest } };
  await f.seal(record);
  const release = f.client.releaseRecoveryCustody;
  f.refuseFinal();
  f.client.releaseRecoveryCustody = async (...args) => {
    const cleared = { ...record };
    delete cleared.finalRequest;
    await f.seal(cleared); // An independent authenticated writer cleared the old request.
    return release(...args);
  };
  await request(f);
  assert.equal(f.calls.final, 1);
  assert.equal((await f.coordinator.inspect(record.runId))[0]?.finalRequest, undefined);
  assert.equal(f.calls.produce, 0);
  assert.equal(f.calls.upload, 0);
  assert.equal(f.calls.publish, 0);
  assert.equal(f.calls.reserve, 0);
});

test("blocked execution selects saved originals using its own current generation", async t => {
  const f = await fixture(t);
  await f.seal(await f.bundled(f.partial(ordinary)));
  const value = observation("normal_blocked");
  await f.coordinator.resumeTerminalSource(f.context.runId, Promise.resolve({ ...value,
    family: { ...value.family!, observedExecution: { runId: f.context.runId, generation: 8 } },
  }), 8);
  assert.equal(f.calls.upload, 1);
  assert.equal(f.calls.produce, 0);
});

for (const timing of ["before", "pending"] as const) {
  test(`caller abort ${timing} tail settlement prevents saved replay`, async t => {
    const f = await fixture(t);
    await f.seal(await f.bundled(f.partial(ordinary)));
    const controller = new AbortController();
    const tail = deferred<ClosedSourceObservation | void>();
    if (timing === "before") controller.abort();
    const pass = f.coordinator.resumeTerminalSource(f.context.runId, tail.promise, 7, controller.signal);
    await turn();
    controller.abort();
    tail.resolve(observation());
    await pass;
    assert.equal(f.calls.upload, 0);
    assert.equal(f.calls.produce, 0);
  });
}


test("ordinary saved originals precede eligible immutable producer evidence", async t => {
  const f = await fixture(t);
  const producerRecord = await f.seal(await f.bundled(f.partial(producer)));
  const ordinaryRecord = await f.seal(await f.bundled({ ...f.partial(ordinary), sourceSha: "e".repeat(40) }));
  const producerBytes = await fs.readFile(producerRecord.bundlePath!);
  const ordinaryBytes = await fs.readFile(ordinaryRecord.bundlePath!);
  await request(f);
  assert.deepEqual(f.uploaded, [ordinaryBytes, producerBytes]);
  assert.equal(f.calls.produce, 0);
});

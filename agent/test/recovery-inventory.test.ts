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

it("review probe: a history of acknowledged generations does not cause exponential settlement work", async () => {
  const f = await fixture();
  try {
    f.state.closeOnRelease = true;
    const snapshot = await f.freeze();
    assert.ok(snapshot);
    await f.capture(snapshot);
    const template = (await f.coordinator.inspect("run-1"))[0]!;
    assert.equal(template.finalAcknowledged, true);
    const count = 12;
    const records = Array.from({ length: count }, (_, index) => ({
      ...structuredClone(template), captureId: `generation-${index + 1}`, generation: index + 1,
      coverageContext: { ...template.coverageContext!, generation: index + 1 },
    }));
    const inner = f.coordinator as unknown as {
      cleanupStateOf(records: RecoveryRecord[], generation: number): Promise<string>;
    };
    const cleanup = inner.cleanupStateOf.bind(inner);
    let calls = 0;
    inner.cleanupStateOf = (rs, generation) => { calls++; return cleanup(rs, generation); };
    assert.equal(await inner.cleanupStateOf(records, count), "acknowledged");
    assert.ok(calls <= count * count,
      `${calls} settlement evaluations for ${count} generations must not grow exponentially`);
  } finally { await f.close(); }
});

it("an initial guarded pin record does not stop a covering FINAL ACK from authorizing cleanup", async () => {
  const f = await fixture();
  try {
    f.state.closeOnRelease = true;
    await f.coordinator.pin({ runId: "run-1", generation: 7, kind: "issue", branch: "task", sourceSha: H, inventoryGuarded: true });
    const record = await f.freeze();
    assert.ok(record);
    await f.capture(record);
    assert.equal((await f.coordinator.inspect("run-1")).find(r => r.captureId === record.captureId)?.finalAcknowledged, true);
    assert.equal(await f.coordinator.inventoryCleanupState("run-1", 7), "acknowledged");
  } finally { await f.close(); }
});

it("issue1924 an acknowledged older generation's source journal does not block a later FINAL", async () => {
  const f = await fixture();
  try {
    f.state.closeOnRelease = true;
    // Generation 5 archived its head H3 and was acknowledged; its unfrozen source journal stays on disk.
    const H3 = "c".repeat(40);
    f.state.ancestors.add(H3);
    f.state.holdGeneration = 5; f.state.ownGeneration = 8;
    const ctx5 = { ...f.context, generation: 5 };
    f.state.candidates = [{ sha: H3, pinRef: "refs/owed/a", contexts: [structuredClone(ctx5)] }];
    await f.coordinator.pin({ runId: "run-1", generation: 5, kind: "issue", branch: "task", sourceSha: H3, inventoryGuarded: true });
    const rec5 = await f.coordinator.freezeInventory({ context: ctx5, currentSha: H3, defaultBranch: "main" });
    assert.ok(rec5);
    await f.capture(rec5);
    assert.equal(await f.coordinator.inventoryCleanupState("run-1", 5), "acknowledged");
    // The run resumed as generation 7 and rewrote H3 away; H3 stays durable via the generation-5 archive.
    f.state.holdGeneration = 7; f.state.open = true; f.state.candidates = [];
    await f.coordinator.pin({ runId: "run-1", generation: 7, kind: "issue", branch: "task", sourceSha: H, inventoryGuarded: true });
    const rec7 = await f.coordinator.freezeInventory({ context: f.context, currentSha: H, defaultBranch: "main" });
    assert.ok(rec7);
    f.state.refused.set(H3, rec7.sourceSha); // the generation-7 archive does not contain H3
    await f.capture(rec7);
    assert.equal((await f.coordinator.inspect("run-1")).find(r => r.captureId === rec7.captureId)?.finalAcknowledged, true,
      "an older generation whose own FINAL is acknowledged must not block this generation's FINAL");
    assert.equal(await f.coordinator.inventoryCleanupState("run-1", 7), "acknowledged",
      "a later FINAL accepted without the separately archived old head must authorize cleanup too");
  } finally { await f.close(); }
});

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
      // Positive control: freeze while ownership is readable, so the record exists and the
      // FINAL boundary is reachable; only then does the injected error decide the outcome.
      const record = await f.freeze();
      assert.ok(record, "a frozen record exists before the error is injected");
      const reserves = f.reserves();
      f.state.ownershipError = error;
      // Drives the resume path, whose FINAL is gated by inactiveInventory (the ownership read).
      // captureAndUpload reaches the same gate through finalizeInventory, but it first uploads the
      // bundle (raising reserves), so asserting an unchanged reserve count would need resumePending
      // alone; both paths were checked to send no FINAL on each injected error.
      await f.make().resumePending(undefined, [record]).catch(() => undefined);
      assert.equal(f.finals.length, 0, "no FINAL is sent on an invalid ownership answer");
      assert.equal(f.reserves(), reserves);
      assert.notEqual((await f.coordinator.inspect("run-1"))[0]?.finalAcknowledged, true);
      assert.equal(f.state.open, true, "the hold stays open");
    } finally { await f.close(); }
  }
});

it("issue1924 a discovery that no longer returns a context drops it from the boot queue", async () => {
  const f = await fixture();
  try {
    await f.coordinator.snapshotOwedInventory();
    let holdReads = 0;
    Object.defineProperty(f.state, "open", { get: () => { holdReads++; return true; } });
    await f.coordinator.materializeBootInventory();
    const visited = holdReads;
    assert.ok(visited > 0, "the discovered context is visited");
    f.git.discoverOwedCandidates = async () => [];
    await f.coordinator.snapshotOwedInventory();
    await f.coordinator.materializeBootInventory();
    assert.equal(holdReads, visited, "a context whose pins are gone costs no further hold RPC");
  } finally { await f.close(); }
});

it("issue1924 a failed discovery keeps the queued contexts", async () => {
  const f = await fixture();
  try {
    await f.coordinator.snapshotOwedInventory();
    f.git.discoverOwedCandidates = async () => { throw new Error("unreadable refs"); };
    await assert.rejects(f.coordinator.snapshotOwedInventory());
    let holdReads = 0;
    Object.defineProperty(f.state, "open", { get: () => { holdReads++; return true; } });
    await f.coordinator.materializeBootInventory();
    assert.ok(holdReads > 0, "the retained context is still visited");
  } finally { await f.close(); }
});

it("issue1924 queue rotation survives repeated discovery so later contexts are not starved", async () => {
  const f = await fixture();
  try {
    const contexts = ["run-1", "run-2", "run-3"].map(runId => ({ ...f.context, runId }));
    f.git.discoverOwedCandidates = async () => contexts.map(context => ({
      context, candidates: structuredClone(f.state.candidates) }));
    const inner = f.coordinator as unknown as {
      liveMaxPerPass: number; client: { listRecoveryHolds(id: string): Promise<unknown> };
    };
    inner.liveMaxPerPass = 1;
    const visited: string[] = [];
    const list = inner.client.listRecoveryHolds.bind(inner.client);
    inner.client.listRecoveryHolds = async (id: string) => { visited.push(id); return list(id); };
    for (let pass = 0; pass < 3; pass++) {
      await f.coordinator.snapshotOwedInventory();
      await f.coordinator.materializeBootInventory();
    }
    assert.deepEqual([...new Set(visited)], ["run-1", "run-2", "run-3"]);
  } finally { await f.close(); }
});

it("issue1924 a symlinked ancestor of the recovery root still finalizes; a symlinked run directory does not", async () => {
  const outer = await fs.mkdtemp(path.join(os.tmpdir(), "inventory-link-"));
  try {
    await fs.mkdir(path.join(outer, "real"));
    await fs.symlink(path.join(outer, "real"), path.join(outer, "link"), "dir");
    const f = await fixture(false, path.join(outer, "link"));
    try {
      const record = await f.freeze();
      assert.ok(record);
      await f.capture(record);
      assert.equal(f.finals.length, 1, "FINAL is sent although the data dir is reached through a symlink");
      assert.equal(await f.coordinator.inventoryCleanupState("run-1", 7), "acknowledged");
      const runDir = path.join(f.root, "journal", "run-1");
      await fs.rename(runDir, runDir + "-moved");
      await fs.symlink(runDir + "-moved", runDir, "dir");
      await assert.rejects(f.coordinator.inventoryCleanupState("run-1", 7), /unsafe recovery directory/);
    } finally { await f.close(); }
  } finally { await fs.rm(outer, { recursive: true, force: true }); }
});

async function fixture(sourceBoundary = false, rootParent = os.tmpdir()) {
  const root = await fs.mkdtemp(path.join(rootParent, "inventory-"));
  const context: PositiveOwedCandidateContext = {
    runId: "run-1", generation: 7, kind: "issue", branch: "task",
    barePath: path.join(root, "repo.git"),
    defaultIdentity: { ref: "refs/remotes/origin/main", sha: "d".repeat(40) },
  };
  const state = {
    candidates: [{ sha: H, pinRef: "refs/owed/a", contexts: [structuredClone(context)] }] as OwedCandidate[],
    status: "failed", ownGeneration: 7, ownGuarded: true, feature: true, open: true, expires: "2099-01-01T00:00:00Z",
    ownershipError: undefined as Error | undefined,
    loseAck: false, wrongAck: false, closeOnRelease: false, holdGeneration: 7, ancestors: new Set<string>([H]), refused: new Map<string, string>(), failProduce: false, now: 1000,
    reserveError: undefined as Error | undefined,
    finalError: undefined as Error | undefined,
    oversized: false, cloneHeads: [] as string[], cloneReadable: true,
    sourceRefused: false, uploads: 0,
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
    listRecoveryHolds: async (runId = context.runId) => ({ run_id: runId, holds: state.open ? [{
      hold_id: "hold-7", generation: state.holdGeneration, inventory_guarded: true, has_available_capture: false,
    }] : [] }),
    reserveRecoveryCapture: async (_run: string, request: { idempotency_key: string }) => {
      reserves++;
      if (state.reserveError) throw state.reserveError;
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
      state.uploads++;
      capture.manifest = m;
      return { capture_id: id, state: "available", manifest_bound: true };
    },
    releaseRecoveryCustody: async (...args: unknown[]) => {
      finals.push(args);
      if (state.finalError) throw state.finalError;
      if (state.loseAck) { state.open = false; throw new Error("ACK lost"); }
      if (state.closeOnRelease && !state.wrongAck) state.open = false; // opt-in: a real server closes the hold
      return { run_id: state.wrongAck ? "other-run" : args[0], generation: args[1] as number, released: true, holds_released: 1 };
    },
  };
  const git = {
    readInventoryCloneHeads: async () => state.cloneReadable
      ? ({ kind: "verified", heads: state.cloneHeads, clones: [], foreignOwners: [] }) : ({ kind: "unknown" }),
    ancestry: async (_bare: string, head: string, target?: string) =>
      state.ancestors.has(head) && state.refused.get(head) !== target ? "ancestor" : "unknown",
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
  const make = (sourceBoundary = false) => new RecoveryCoordinator({
    recoveryRoot: path.join(root, "journal"), workerToken: "local-worker-fixture",
    log: nullLogger(), client: client as never, git: git as never, now: () => state.now,
    withInventorySourceBoundary: sourceBoundary ? async (_context, action) => {
      if (state.sourceRefused) return "retained";
      await action(async () => !state.sourceRefused);
      return "passed";
    } : undefined,
  });
  const coordinator = make(sourceBoundary);
  const freeze = (originalSourceSha = H) => coordinator.freezeInventory({
    context, currentSha: H, originalSourceSha, defaultBranch: "main",
  });
  const capture = (record: RecoveryRecord) => coordinator.captureAndUpload({
    record, barePath: context.barePath, defaultBranch: "main",
  });
  return { root, context, state, aggregates, finals, coordinator, make, freeze, capture, git,
    reserves: () => reserves, close: () => fs.rm(root, { recursive: true, force: true }) };
}

it("guarded boot failure does not block a sibling run", async () => {
  const f = await fixture();
  try {
    f.state.candidates = [];
    f.state.status = "completed";
    const first = await f.coordinator.freezeInventory({
      context: f.context, currentSha: H, defaultBranch: "main", settledEvidence: "publication",
    });
    const second = await f.coordinator.freezeInventory({
      context: { ...f.context, runId: "run-2" }, currentSha: H, defaultBranch: "main", settledEvidence: "publication",
    });
    assert.ok(first && second);
    const visited: string[] = [];
    f.git.enumerateOwedCandidates = async (_bare?: string, run?: string) => {
      visited.push(run!);
      if (run === "run-1") throw new Error("unreadable owed inventory");
      return [];
    };
    await assert.doesNotReject(f.coordinator.resumePending(undefined, [first, second]));
    assert.deepEqual(visited, ["run-1", "run-2"]);
    assert.equal(f.finals.length, 1);
    assert.equal((await f.coordinator.inspect("run-2"))[0]!.finalAcknowledged, true);
    assert.equal((await f.coordinator.inspect("run-1"))[0]!.finalAcknowledged, undefined);
  } finally { await f.close(); }
});

for (const phase of ["capture", "FINAL"] as const) {
  for (const credential of [false, true]) {
    it(`guarded ${phase} ${credential ? "credential rejection" : "transient failure"} spaces live retries`, async () => {
      const f = await fixture();
      try {
        const record = await f.freeze();
        assert.ok(record);
        const error = credential
          ? new RequestError("POST", "/api/worker/recovery", 401, "unauthorized")
          : new Error("network unavailable");
        if (phase === "capture") f.state.reserveError = error;
        else f.state.finalError = error;
        const outcome = await f.capture(record);
        assert.equal(outcome.reason, credential ? "credential_rejected" : phase === "capture" ? "capture_error" : "upload_transient");
        const before = (await f.coordinator.inspect("run-1"))[0]!;
        await f.coordinator.resumeLive({ isExecuting: () => false, authenticatedAtMs: f.state.now + 1 });
        const pending = (await f.coordinator.inspect("run-1"))[0]!;
        assert.equal(pending.finalAcknowledged, undefined);
        if (phase === "FINAL") {
          assert.equal(pending.state, "uploaded");
          assert.deepEqual(pending.finalRequest, before.finalRequest);
        }
        const attempts = phase === "capture" ? f.reserves() : f.finals.length;
        f.state.now += 30_000;
        await f.coordinator.resumeLive({ isExecuting: () => false, authenticatedAtMs: credential ? 1000 : f.state.now });
        assert.equal(phase === "capture" ? f.reserves() : f.finals.length, attempts);
        f.state.now += 30_000;
        f.state.reserveError = undefined;
        f.state.finalError = undefined;
        await f.coordinator.resumeLive({ isExecuting: () => false, authenticatedAtMs: f.state.now });
        const after = (await f.coordinator.inspect("run-1"))[0]!;
        assert.equal(after.finalAcknowledged, true);
        if (phase === "FINAL") {
          assert.equal(before.state, "uploaded");
          assert.equal(after.state, "uploaded");
          assert.deepEqual(after.finalRequest, before.finalRequest);
          assert.ok(f.finals.every(request => canonicalJson(request) === canonicalJson(f.finals[0])));
          assert.equal(f.reserves(), 1);
        }
      } finally { await f.close(); }
    });
  }
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


for (const pendingFinal of [false, true]) {
  it(`source refusal repeats backoff without replacing uploaded identity pendingFINAL=${pendingFinal}`, async () => {
    const f = await fixture(true);
    try {
      const record = await f.freeze();
      assert.ok(record);
      if (pendingFinal) {
        f.state.finalError = new Error("ACK unavailable");
        await f.capture(record);
      } else {
        f.state.onProduce = () => { f.state.sourceRefused = true; };
        await f.capture(record);
      }
      f.state.sourceRefused = true;
      const before = (await f.coordinator.inspect("run-1"))[0]!;
      const identity = (r: RecoveryRecord) => [r.captureId, r.serverCaptureId, r.sourceSha,
        r.coverageDigest, r.checksum, r.finalRequest];
      await f.coordinator.resumeLive({ isExecuting: () => false, authenticatedAtMs: f.state.now });
      const first = (await f.coordinator.inspect("run-1"))[0]!;
      assert.equal(first.reason, "inventory_source_not_quiescent");
      assert.equal(first.state, "uploaded");
      assert.deepEqual(identity(first), identity(before));
      f.state.now += 30_000;
      f.state.sourceRefused = false;
      f.state.finalError = undefined;
      await f.coordinator.resumeLive({ isExecuting: () => false, authenticatedAtMs: f.state.now });
      assert.equal((await f.coordinator.inspect("run-1"))[0]!.finalAcknowledged, undefined);
      f.state.sourceRefused = true;
      f.state.now += 30_000;
      await f.coordinator.resumeLive({ isExecuting: () => false, authenticatedAtMs: f.state.now });
      f.state.now += 60_000;
      f.state.sourceRefused = false;
      await f.coordinator.resumeLive({ isExecuting: () => false, authenticatedAtMs: f.state.now });
      assert.equal((await f.coordinator.inspect("run-1"))[0]!.finalAcknowledged, undefined);
      f.state.now += 60_000;
      await f.coordinator.resumeLive({ isExecuting: () => false, authenticatedAtMs: f.state.now });
      const after = (await f.coordinator.inspect("run-1"))[0]!;
      assert.equal(after.finalAcknowledged, true);
      assert.equal(after.captureId, before.captureId);
      assert.equal(after.serverCaptureId, before.serverCaptureId);
      if (pendingFinal) assert.deepEqual(after.finalRequest, before.finalRequest);
      assert.equal(f.reserves(), 1);
      assert.equal(f.state.uploads, 1);
      assert.equal(f.state.produced, 1);
      assert.equal(f.state.open, true);
    } finally { await f.close(); }
  });
}

it("physical clones fail closed without runner boundary even when heads are covered", async () => {
  const f = await fixture();
  try {
    f.git.readInventoryCloneHeads = async () => ({ kind: "verified", heads: [H],
      clones: [{ clonePath: "/retained/task", branch: "task", runId: "run-1" }], foreignOwners: [] }) as never;
    const record = await f.freeze();
    assert.ok(record);
    await f.capture(record);
    assert.equal(f.finals.length, 0);
    assert.equal(f.state.open, true);
    const retained = (await f.coordinator.inspect("run-1"))[0]!;
    assert.equal(retained.state, "uploaded", "available bytes remain uploaded while FINAL is refused");
    assert.equal(retained.reason, "inventory_source_not_quiescent");
    assert.equal(retained.finalAcknowledged, undefined);
  } finally { await f.close(); }
});

// issue #2213 / #1924: the guarded FINAL release is a custody release, so a latched worker must not send it.
type ClientSeam = Record<string, (...args: unknown[]) => Promise<unknown>>;
const clientOf = (f: Awaited<ReturnType<typeof fixture>>): ClientSeam =>
  (f.coordinator as unknown as { client: ClientSeam }).client;

it("issue2213 a quarantine latched before finalization holds the guarded FINAL and keeps the evidence", async () => {
  const { latchResidueQuarantine, resetResidueQuarantineForTests } = await import("../src/residue-quarantine.js");
  const f = await fixture();
  try {
    const client = clientOf(f);
    const upload = client.uploadRecoveryBundle!.bind(client);
    client.uploadRecoveryBundle = async (...args) => {
      const result = await upload(...args);
      latchResidueQuarantine({ cause: "review fixture", site: "after_upload" }, nullLogger());
      return result;
    };
    const record = await f.freeze();
    assert.ok(record);
    await f.capture(record);
    assert.equal(f.finals.length, 0, "no guarded release while quarantine is latched");
    const [kept] = await f.coordinator.inspect("run-1");
    assert.notEqual(kept?.finalAcknowledged, true);
    assert.equal(kept?.state, "uploaded");
    resetResidueQuarantineForTests();
    await f.capture(kept!);
    assert.equal(f.finals.length, 1, "the retained evidence finalizes once unlatched");
  } finally { resetResidueQuarantineForTests(); await f.close(); }
});

it("issue2213 a quarantine during the final status read prevents the guarded FINAL", async () => {
  const { latchResidueQuarantine, resetResidueQuarantineForTests } = await import("../src/residue-quarantine.js");
  const f = await fixture();
  try {
    const client = clientOf(f);
    const status = client.getRecoveryCaptureStatus!.bind(client);
    client.getRecoveryCaptureStatus = async (...args) => {
      const result = await status(...args);
      latchResidueQuarantine({ cause: "review fixture", site: "final_status_read" }, nullLogger());
      return result;
    };
    const record = await f.freeze();
    assert.ok(record);
    await f.capture(record);
    assert.equal(f.finals.length, 0, "no new guarded release while quarantine is latched");
    const [kept] = await f.coordinator.inspect("run-1");
    assert.equal(kept?.finalRequest, undefined, "no FINAL identity is journaled after the latch");
  } finally { resetResidueQuarantineForTests(); await f.close(); }
});

it("issue2213 a quarantine during the FINAL ACK keeps the journal unacknowledged and the bundle local", async () => {
  const { latchResidueQuarantine, resetResidueQuarantineForTests } = await import("../src/residue-quarantine.js");
  const f = await fixture();
  try {
    const client = clientOf(f);
    const release = client.releaseRecoveryCustody!.bind(client);
    client.releaseRecoveryCustody = async (...args) => {
      const result = await release(...args);
      latchResidueQuarantine({ cause: "review fixture", site: "final_ack" }, nullLogger());
      return result;
    };
    const released: string[] = [];
    (f.coordinator as unknown as { onAuthoritativeGenerationReleased?: (run: string, g: number) => void })
      .onAuthoritativeGenerationReleased = run => { released.push(run); };
    const record = await f.freeze();
    assert.ok(record);
    await f.capture(record);
    assert.equal(f.finals.length, 1, "the release was already in flight");
    const [kept] = await f.coordinator.inspect("run-1");
    assert.notEqual(kept?.finalAcknowledged, true, "a latched worker does not journal the ACK");
    assert.deepEqual(released, []);
    await fs.access(kept!.bundlePath!);
  } finally { resetResidueQuarantineForTests(); await f.close(); }
});

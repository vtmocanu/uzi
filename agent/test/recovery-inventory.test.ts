import { it } from "node:test";
import { RunRunner } from "../src/runner.js";
import { RequestError } from "../src/client.js";
import type { RecoveryFinalDisposition, RecoveryReserveRequest } from "../src/protocol.js";
import { execFileSync } from "node:child_process";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { RecoveryCoordinator, canonicalJson, type InventorySourceBoundaryContext, type InventorySourceDiagnostic, type RecoveryArchiveClient, type RecoveryRecord } from "../src/recovery.js";
import { createHash, createHmac } from "node:crypto";
import { GitCache, RecoveryBundleTooLargeError, type OwedCandidate, type PositiveOwedCandidateContext } from "../src/git.js";
import { nullLogger, testGitCacheOptions } from "./helpers.js";

const H = "a".repeat(40);
const H2 = "b".repeat(40);

for (const failure of ["none", "bundle", "ack", "pins", "unreadable", "foreign", "uncovered", "attacker-path"] as const) {
  it(`M2 checked cleanup retains authority on ${failure} and retries without upload`, async t => {
    const f = await fixture();
    try {
      f.state.closeOnRelease = true;
      await f.coordinator.pin({ runId: "run-1", generation: 7, kind: "issue", branch: "task",
        sourceSha: H, inventoryGuarded: true });
      const record = await f.freeze();
      assert.ok(record);
      // Capture first with no helper: persisted ACK models a crash before local cleanup.
      await f.capture(record);
      const ack = (await f.coordinator.inspect("run-1")).find(r => r.captureId === record.captureId)!;
      assert.equal(ack.finalAcknowledged, true);
      assert.ok(ack.bundlePath);
      const journal = path.join(f.root, "journal", "run-1", ack.captureId + ".json");
      const saved = await fs.readFile(journal, "utf8");
      const roots = await f.coordinator.inspect("run-1");
      const source = roots.find(r => !r.coverageDigest)!;
      const sourceFile = path.join(f.root, "journal", "run-1", source.captureId + ".json");
      const sourceSaved = await fs.readFile(sourceFile, "utf8");
      const sign = async (file: string, payload: RecoveryRecord) => {
        const key = createHmac("sha256", "local-worker-fixture").update("uzi-recovery-journal-v1").digest();
        const mac = createHmac("sha256", key).update(canonicalJson(payload)).digest("hex");
        await fs.writeFile(file, JSON.stringify({ ...payload, mac }));
      };
      if (failure === "unreadable") { await fs.unlink(sourceFile); await fs.mkdir(sourceFile); }
      if (failure === "foreign") await sign(sourceFile, { ...source, runId: "foreign" });
      if (failure === "uncovered") await sign(sourceFile, { ...source, sourceSha: H2 });
      const attacker = path.join(f.root, "attacker.bundle");
      if (failure === "attacker-path") {
        await fs.writeFile(attacker, "keep");
        await sign(journal, { ...ack, bundlePath: attacker });
      }
      let pins = 0, pinFailure = failure === "pins";
      Object.assign(f.git, { cleanupRecoveryGeneration: async () => {
        pins++;
        if (pinFailure) throw new Error("pin readback failed");
        return "removed";
      } });
      const originalUnlink = fs.unlink;
      let unlinkFailure = failure === "bundle" || failure === "ack";
      t.mock.method(fs, "unlink", async (file: Parameters<typeof fs.unlink>[0]) => {
        if (unlinkFailure && String(file) === (failure === "bundle" ? ack.bundlePath : journal))
          throw Object.assign(new Error("unlink failure"), { code: "EACCES" });
        return originalUnlink(file);
      });
      const reserves = f.reserves(), uploads = f.state.uploads, finals = f.finals.length;
      const restarted = f.make();
      await restarted.resumePending();
      if (failure !== "none") {
        assert.equal((await restarted.inspect("run-1")).find(r => r.captureId === ack.captureId)?.finalAcknowledged, true);
        assert.ok(await fs.stat(journal));
        if (["unreadable", "foreign", "uncovered", "attacker-path"].includes(failure)) assert.equal(pins, 0);
        if (failure === "unreadable") await fs.rmdir(sourceFile);
        if (["unreadable", "foreign", "uncovered"].includes(failure)) await fs.writeFile(sourceFile, sourceSaved);
        if (failure === "attacker-path") {
          assert.equal(await fs.readFile(attacker, "utf8"), "keep");
          await fs.writeFile(journal, saved);
        }
        pinFailure = false; unlinkFailure = false;
        f.state.now += 100_000;
        await restarted.resumeLive({ authenticatedAtMs: f.state.now, isExecuting: () => false });
      }
      assert.deepEqual(await restarted.inspect("run-1"), []);
      await assert.rejects(fs.stat(path.join(f.root, "journal", "run-1")), { code: "ENOENT" });
      assert.equal(f.reserves(), reserves);
      assert.equal(f.state.uploads, uploads);
      assert.equal(f.finals.length, finals);
    } finally { await f.close(); }
  });
}

for (const protection of ["quarantine", "execution", "during-pins"] as const) {
  it(`M2 ACK cleanup observes ${protection} source protection`, async () => {
    const { latchResidueQuarantine, resetResidueQuarantineForTests } = await import("../src/residue-quarantine.js");
    const f = await fixture();
    try {
      f.state.closeOnRelease = true;
      const record = await f.freeze();
      assert.ok(record);
      await f.capture(record);
      let executing = protection === "execution", calls = 0;
      Object.assign(f.git, { cleanupRecoveryGeneration: async () => {
        calls++;
        if (protection === "during-pins") executing = true;
        return "removed";
      } });
      const live = f.make();
      if (protection === "quarantine") latchResidueQuarantine({ cause: "cleanup fixture", site: "cleanup" }, nullLogger());
      await live.resumeLive({ authenticatedAtMs: f.state.now, isExecuting: () => executing });
      assert.equal((await live.inspect("run-1"))[0]?.finalAcknowledged, true);
      const ack = (await live.inspect("run-1"))[0]!;
      assert.ok(await fs.stat(ack.bundlePath!));
      assert.equal(calls, protection === "during-pins" ? 1 : 0);
      resetResidueQuarantineForTests();
      executing = false;
      Object.assign(f.git, { cleanupRecoveryGeneration: async () => "removed" });
      f.state.now += 100_000;
      await live.resumeLive({ authenticatedAtMs: f.state.now, isExecuting: () => executing });
      assert.deepEqual(await live.inspect("run-1"), []);
      assert.equal(f.state.uploads, 1);
      assert.equal(f.finals.length, 1);
    } finally { resetResidueQuarantineForTests(); await f.close(); }
  });
}

it("M2 multiple ACK cleanup keeps one covering authority through every source unlink", async t => {
  const f = await fixture();
  try {
    await f.coordinator.pin({ runId: "run-1", generation: 7, kind: "issue", branch: "task",
      sourceSha: H, inventoryGuarded: true });
    const record = await f.freeze();
    assert.ok(record);
    await f.capture(record);
    const ack = (await f.coordinator.inspect("run-1")).find(r => r.finalAcknowledged)!;
    const copy = { ...ack, captureId: "second-ack",
      bundlePath: path.join(f.root, "journal", "run-1", "second-ack.bundle") };
    const dir = path.join(f.root, "journal", "run-1");
    const key = createHmac("sha256", "local-worker-fixture").update("uzi-recovery-journal-v1").digest();
    await fs.writeFile(path.join(dir, copy.captureId + ".json"), JSON.stringify({ ...copy,
      mac: createHmac("sha256", key).update(canonicalJson(copy)).digest("hex") }));
    Object.assign(f.git, { cleanupRecoveryGeneration: async () => "removed" });
    const unlink = fs.unlink;
    let failLast = true;
    const removed: string[] = [];
    t.mock.method(fs, "unlink", async (file: Parameters<typeof fs.unlink>[0]) => {
      if (String(file).endsWith(".json")) {
        const journals = await f.coordinator.inspect("run-1");
        assert.ok(journals.some(r => r.finalAcknowledged), "a covering ACK exists before every journal unlink");
        if (journals.length === 1 && failLast) throw new Error("last ACK unlink interruption");
        removed.push(String(file));
      }
      return unlink(file);
    });
    await assert.rejects(f.coordinator.forgetGeneration("run-1", 7), /last ACK unlink/);
    assert.equal((await f.coordinator.inspect("run-1")).length, 1);
    assert.equal((await f.coordinator.inspect("run-1"))[0]?.finalAcknowledged, true);
    assert.ok(!removed[0]!.endsWith(copy.captureId + ".json"), "non-ACK source journal goes first");
    failLast = false;
    await f.make().resumePending();
    assert.deepEqual(await f.coordinator.inspect("run-1"), []);
  } finally { await f.close(); }
});

it("M2 live ACK retry obeys pass cap and spacing without reserve or upload", async () => {
  const f = await fixture();
  try {
    const record = await f.freeze();
    assert.ok(record);
    await f.capture(record);
    let attempts = 0;
    Object.assign(f.git, { cleanupRecoveryGeneration: async () => { attempts++; throw new Error("retry"); } });
    const live = f.make();
    await live.resumeLive({ authenticatedAtMs: f.state.now, isExecuting: () => false });
    assert.equal(attempts, 1);
    await live.resumeLive({ authenticatedAtMs: f.state.now, isExecuting: () => false });
    assert.equal(attempts, 1);
    f.state.now += 100_000;
    await live.resumeLive({ authenticatedAtMs: f.state.now, isExecuting: () => false });
    assert.equal(attempts, 2);
    assert.equal(f.reserves(), 1);
    assert.equal(f.state.uploads, 1);
    assert.equal(f.finals.length, 1);
  } finally { await f.close(); }
});

it("M2 credential rejection still permits spaced local ACK cleanup", async () => {
  const f = await fixture();
  try {
    f.state.closeOnRelease = true;
    const record = await f.freeze();
    assert.ok(record);
    await f.capture(record);
    f.state.open = true;
    f.state.holdGeneration = 8;
    f.state.ownGeneration = 8;
    const next = await f.coordinator.freezeInventory({
      context: { ...f.context, generation: 8 }, currentSha: H, defaultBranch: "main",
    });
    assert.ok(next);
    f.state.reserveError = new RequestError("POST", "/api/worker/recovery", 401, "unauthorized");
    await f.capture(next);
    const reserves = f.reserves(), uploads = f.state.uploads, finals = f.finals.length;
    let attempts = 0;
    Object.assign(f.git, { cleanupRecoveryGeneration: async () => {
      attempts++;
      if (attempts === 1) throw new Error("local interruption");
      return "removed";
    } });
    const opts = { authenticatedAtMs: f.state.now, isExecuting: () => false };
    await f.coordinator.resumeLive(opts);
    assert.equal(attempts, 1, "same-age rejected credentials cannot block local ACKs");
    await f.coordinator.resumeLive(opts);
    assert.equal(attempts, 1, "local failure retains backoff");
    f.state.now += 100_000;
    await f.coordinator.resumeLive(opts);
    assert.equal(attempts, 2);
    assert.deepEqual((await f.coordinator.inspect("run-1")).map(r => r.generation), [8]);
    assert.equal(f.reserves(), reserves);
    assert.equal(f.state.uploads, uploads);
    assert.equal(f.finals.length, finals);
  } finally { await f.close(); }
});

it("M2 feature rollback cannot reopen an ACK-cleaned guarded journal", async () => {
  const f = await fixture();
  try {
    f.state.closeOnRelease = true;
    const record = await f.freeze();
    assert.ok(record);
    Object.assign(f.git, { cleanupRecoveryGeneration: async () => "removed" });
    await f.capture(record);
    assert.deepEqual(await f.coordinator.inspect("run-1"), []);
    f.state.feature = false;
    const fresh = f.make();
    assert.equal(await fresh.pin({ runId: "run-1", generation: 7, kind: "issue",
      branch: "task", sourceSha: H, inventoryGuarded: true }), undefined);
    assert.deepEqual(await fresh.inspect("run-1"), []);
    assert.equal(f.reserves(), 1);
    assert.equal(f.state.uploads, 1);
  } finally { await f.close(); }
});

for (const closure of ["ACK", "discard"] as const) {
  it(`M2 delayed guarded pin cannot recreate a journal after ${closure}`, async () => {
    const f = await fixture();
    try {
      f.state.closeOnRelease = true;
      const record = await f.freeze();
      assert.ok(record);
      await f.capture(record);
      let entered!: () => void, release!: () => void;
      const enteredCleanup = new Promise<void>(resolve => { entered = resolve; });
      const finishCleanup = new Promise<void>(resolve => { release = resolve; });
      Object.assign(f.git, { cleanupRecoveryGeneration: async () => {
        entered();
        await finishCleanup;
        return "removed";
      } });
      const cleaning = f.coordinator.forgetGeneration("run-1", 7);
      await enteredCleanup;
      // This real writer queues behind the generation cycle and observes absence only
      // after cleanup. No persisted tombstone can rescue its creation re-check.
      const writer = f.make();
      const pinning = writer.pin({ runId: "run-1", generation: 7, kind: "issue",
        branch: "task", sourceSha: H, inventoryGuarded: true });
      release();
      await cleaning;
      assert.equal(await pinning, undefined);
      if (closure === "discard") {
        f.state.open = true;
        assert.ok(await writer.pin({ runId: "run-1", generation: 7, kind: "issue",
          branch: "task", sourceSha: H, inventoryGuarded: true }));
        // Owner discard grants no deletion authority, but closes fresh creation authority.
        f.state.open = false;
        await writer.forgetGeneration("run-1", 7);
        assert.equal((await writer.inspect("run-1")).length, 1);
      }
      await writer.snapshotOwedInventory();
      assert.deepEqual(await writer.materializeBootInventory(), []);
      assert.equal(f.reserves(), 1);
      assert.equal(f.state.uploads, 1);
    } finally { await f.close(); }
  });
}

it("M2 successful persisted FINAL cleanup preserves sibling generations", async () => {
  const f = await fixture();
  try {
    const record = await f.freeze();
    assert.ok(record);
    Object.assign(f.git, { cleanupRecoveryGeneration: async () => "removed" });
    f.state.holdGeneration = 8;
    const sibling = await f.coordinator.pin({ runId: "run-1", generation: 8, kind: "issue", branch: "task",
      sourceSha: H2, inventoryGuarded: true });
    assert.ok(sibling);
    f.state.holdGeneration = 7;
    await f.capture(record);
    assert.deepEqual((await f.coordinator.inspect("run-1")).map(r => r.captureId), [sibling.captureId]);
    await assert.rejects(fs.stat(path.join(f.root, "journal", "run-1", record.captureId + ".bundle")), { code: "ENOENT" });
  } finally { await f.close(); }
});


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

const sourceDiagnosticCauses = [
  "attribution_unreadable", "clone_ancestor_invalid", "clone_path_invalid",
  "clone_head_unreadable", "git_or_filesystem_error", "other",
] as const;

for (const cause of [...sourceDiagnosticCauses, undefined, "unknown/raw/error"]) {
  for (const actionTime of [false, true]) it(`#2507 fallback cause ${cause} actionTime=${actionTime} authenticates across restart`, async () => {
    const f = await fixture();
    try {
      const record = await f.freeze();
      assert.ok(record);
      let reads = 0;
      f.git.readInventoryCloneHeads = async () => ++reads === (actionTime ? 3 : 1)
        ? { kind: "unknown", cause } as never : { kind: "verified", heads: [], clones: [], foreignOwners: [] };
      const outcome = await f.capture(record);
      const normalized = sourceDiagnosticCauses.includes(cause as never) ? cause : "other";
      const reason = "inventory_source_not_quiescent:inventory_read_not_verified:" + normalized;
      assert.equal(outcome.reason, reason);
      const before = (await f.coordinator.inspect("run-1"))[0]!;
      assert.equal(before.reason, reason);
      assert.equal(before.state, "uploaded");
      assert.equal(before.finalAcknowledged, undefined);
      assert.equal(f.state.open, true);
      assert.deepEqual(f.finals, []);
      assert.deepEqual(f.logs, [{ run_id: "run-1", generation: 7, capture_id: record.captureId,
        source_boundary_reason: "inventory_read_not_verified", inventory_read_cause: normalized }]);
      const restarted = f.make();
      assert.equal((await restarted.inspect("run-1"))[0]!.reason, reason, "fresh MAC-authenticated read");
      f.git.readInventoryCloneHeads = async () => ({ kind: "verified", heads: [], clones: [], foreignOwners: [] });
      await restarted.resumePending(undefined, [before]);
      const after = (await restarted.inspect("run-1"))[0]!;
      assert.equal(after.finalAcknowledged, true);
      assert.equal(after.reason, reason, "last failure survives success");
      for (const key of ["captureId", "serverCaptureId", "sourceSha", "checksum", "coverageDigest"] as const)
        assert.equal(after[key], before[key]);
      if (before.finalRequest) assert.deepEqual(after.finalRequest, before.finalRequest);
      assert.equal(f.state.uploads, 1);
      assert.equal(f.reserves(), 1);
    } finally { await f.close(); }
  });
}

for (const original of [false, true]) for (const actionTime of [false, true]) {
  it(`#2507 boot refusal logs independently of writes original=${original} actionTime=${actionTime}`, async () => {
    const f = await fixture(true);
    try {
      if (original) await f.coordinator.pin({ runId: "run-1", generation: 7, kind: "issue",
        branch: "task", sourceSha: H, inventoryGuarded: true });
      await f.coordinator.snapshotOwedInventory();
      const before = await f.coordinator.inspect("run-1");
      f.state.diagnostic = { check: "inventory_read_not_verified", cause: "attribution_unreadable" };
      f.state.sourceRefused = !actionTime;
      f.state.sourceProofRefused = actionTime;
      assert.deepEqual(await f.coordinator.materializeBootInventory(), []);
      assert.deepEqual(f.logs, [{ run_id: "run-1", generation: 7,
        ...(original ? { capture_id: before[0]!.captureId } : {}),
        source_boundary_reason: "inventory_read_not_verified", inventory_read_cause: "attribution_unreadable" }]);
      const after = await f.make(true).inspect("run-1");
      if (original && !actionTime) {
        assert.equal(after[0]!.reason, "inventory_source_not_quiescent:inventory_read_not_verified:attribution_unreadable");
        assert.equal(after[0]!.captureId, before[0]!.captureId);
      } else assert.deepEqual(after, before, "outer passed or no original must not write");
      assert.deepEqual(f.finals, []);
    } finally { await f.close(); }
  });
}

for (const source of ["missing", "unknown", "malformed", "empty", "physical", "legacy"] as const) {
  it(`#2507 coordinator boundary ${source} preserves custody decisions`, async () => {
    const f = await fixture(source === "legacy");
    try {
      const record = await f.freeze();
      assert.ok(record);
      if (source === "missing") delete (f.git as { readInventoryCloneHeads?: unknown }).readInventoryCloneHeads;
      if (source === "unknown") f.state.cloneReadable = false;
      if (source === "malformed") f.git.readInventoryCloneHeads = async () => ({ kind: "verified", clones: [null] }) as never;
      if (source === "physical") f.git.readInventoryCloneHeads = async () => ({ kind: "verified", heads: [H],
        clones: [{ clonePath: "/fixture/clone", runId: "run-1", branch: "task" }], foreignOwners: [] }) as never;
      if (source === "legacy") f.state.sourceRefused = true;
      await f.capture(record);
      const retained = (await f.make().inspect("run-1"))[0]!;
      if (source === "empty") {
        assert.equal(retained.finalAcknowledged, true);
        assert.deepEqual(f.logs, []);
      } else {
        const check = source === "legacy" ? "inventory_source_not_quiescent" :
          source === "physical" || source === "malformed" ? "physical_sources_without_boundary" : "inventory_read_not_verified";
        assert.equal(retained.reason, source === "legacy" ? check : "inventory_source_not_quiescent:" + check +
          (check === "inventory_read_not_verified" ? ":other" : ""));
        assert.equal(f.logs[0]!.source_boundary_reason, check);
        assert.equal(f.finals.length, 0);
        assert.equal(f.state.open, true);
        assert.equal(retained.finalAcknowledged, undefined);
      }
    } finally { await f.close(); }
  });
}

for (const kind of ["issue", "mr_rework"] as const) for (const cause of sourceDiagnosticCauses)
  for (const finalRead of [false, true]) {
    it(`#2507 real runner to coordinator completed ${kind} cause=${cause} finalRead=${finalRead}`, async () => {
      const f = await fixture("runner");
      try {
        f.context.kind = kind;
        f.state.candidates[0]!.contexts = [structuredClone(f.context)];
        f.state.status = "completed";
        const record = await f.freeze();
        assert.ok(record);
        let reads = 0;
        f.git.readInventoryCloneHeads = async () => ++reads === (finalRead ? 2 : 1)
          ? { kind: "unknown", cause } : { kind: "verified", heads: [], clones: [], foreignOwners: [] };
        const outcome = await f.capture(record);
        const reason = "inventory_source_not_quiescent:inventory_read_not_verified:" + cause;
        assert.equal(outcome.reason, reason);
        const retained = (await f.make("runner").inspect("run-1"))[0]!;
        assert.equal(retained.reason, reason, "diagnostic survives authenticated restart");
        assert.equal(retained.kind, kind);
        assert.equal(retained.state, "uploaded");
        assert.equal(retained.finalAcknowledged, undefined);
        assert.equal(f.state.open, true);
        assert.deepEqual(f.finals, []);
        assert.deepEqual(f.logs, [{ run_id: "run-1", generation: 7, capture_id: record.captureId,
          source_boundary_reason: "inventory_read_not_verified", inventory_read_cause: cause }]);
        assert.equal(f.state.uploads, 1);
        assert.equal(f.reserves(), 1);
      } finally { await f.close(); }
    });
  }

async function fixture(sourceBoundary: boolean | "runner" = false, rootParent = os.tmpdir(), receiptModel = false) {
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
    sourceRefused: false, sourceProofRefused: false, diagnostic: undefined as InventorySourceDiagnostic | undefined, cloneCause: undefined as string | undefined, uploads: 0, pinDeletes: 0, statusReads: 0, thin: false,
    produceWait: undefined as Promise<void> | undefined, produced: 0,
    onProduce: undefined as (() => void) | undefined,
    beforeFinalAck: undefined as (() => Promise<void>) | undefined,
  };
  const logs: Array<Record<string, unknown>> = [];
  const aggregates: string[][] = [];
  const finals: unknown[][] = [];
  let reserves = 0;
  const reserveKeys: string[] = [];
  const committedReceipts = new Map<number, Readonly<RecoveryFinalDisposition>>();
  const captures = new Map<string, {
    id: string; request: RecoveryReserveRequest; state?: string; expires?: string; reason?: string;
    manifest?: { checksum: string; byte_size: number };
  }>();
  const client = {
    hasFeature: () => state.feature,
    getRunOwnership: async () => {
      if (state.ownershipError) throw state.ownershipError;
      return { status: state.status, claim_generation: state.ownGeneration, inventory_guarded: state.ownGuarded };
    },
    listRecoveryHolds: async (runId = context.runId) => ({ run_id: runId, holds: state.open ? [{
      hold_id: "hold-7", generation: state.holdGeneration, inventory_guarded: true, has_available_capture: false,
    }] : [] }),
    reserveRecoveryCapture: async (_run: string, request: RecoveryReserveRequest) => {
      reserves++;
      reserveKeys.push(request.idempotency_key);
      if (state.reserveError) throw state.reserveError;
      let capture = captures.get(request.idempotency_key);
      if (!capture) {
        capture = { id: "server-" + (captures.size + 1), request: structuredClone(request),
          expires: receiptModel ? state.expires : undefined };
        captures.set(request.idempotency_key, capture);
      }
      return { capture_id: capture.id, state: "preparing" };
    },
    getRecoveryCaptureStatus: async (_run: string, id: string) => {
      state.statusReads++;
      const capture = [...captures.values()].find(c => c.id === id);
      assert.ok(capture);
      return {
        capture_id: id, state: capture.state ?? (capture.manifest ? "available" : "preparing"), manifest_bound: !!capture.manifest,
        checksum: capture.manifest?.checksum, byte_size: capture.manifest?.byte_size, expires_at: capture.expires ?? state.expires, reason: capture.reason,
      };
    },
    uploadRecoveryBundle: async (_run: string, id: string, m: { checksum: string; byte_size: number }, stream: AsyncIterable<unknown>) => {
      for await (const chunk of stream) assert.ok(chunk);
      const capture = [...captures.values()].find(c => c.id === id);
      assert.ok(capture);
      state.uploads++;
      capture.manifest = structuredClone(m);
      return { capture_id: id, state: "available", manifest_bound: true };
    },
    ...(receiptModel ? {
      reconcileRecoveryCapture: async (run: string, id: string, request: {
        generation: number; source_sha: string; coverage_digest: string; checksum: string; byte_size: number;
      }) => {
        const response = { run_id: run, generation: request.generation, capture_id: id };
        const receipt = committedReceipts.get(request.generation);
        if (receipt) return { ...response, outcome: "accepted", final_receipt: structuredClone(receipt) };
        const capture = [...captures.values()].find(c => c.id === id);
        assert.ok(capture);
        assert.equal(request.generation, capture.request.generation);
        assert.equal(request.source_sha, capture.request.source_sha);
        assert.equal(request.coverage_digest, capture.request.coverage_digest);
        if ((capture.manifest && (request.checksum !== capture.manifest.checksum || request.byte_size !== capture.manifest.byte_size))) {
          return { ...response, outcome: "retained", reason: "capture_manifest_integrity_mismatch" };
        }
        if (capture.state === "available" && !capture.manifest) return { ...response, outcome: "retained", reason: "available_capture_manifest_unbound" };
        if (capture.reason === "archive integrity check failed" ||
            (capture.state === "needs_action" && !["upload_retry_window_exhausted", "storage quota exceeded", "upload failed; retry available"].includes(capture.reason ?? "")) ||
            (["preparing", "uploading", "discarded", "available"].includes(capture.state ?? "") && !!capture.reason)) {
          return { ...response, outcome: "retained", reason: "capture_failure_requires_attention" };
        }
        if (state.open && (capture.state === "expired" || Date.parse(capture.expires ?? "") <= state.now ||
            ["preparing", "uploading", "discarded", "needs_action"].includes(capture.state ?? ""))) {
          capture.state = "expired";
          return { ...response, outcome: "replaceable" };
        }
        return { ...response, outcome: "retained", reason: "capture_available" };
      },
    } : {}),
    releaseRecoveryCustody: async (...args: unknown[]) => {
      finals.push(structuredClone(args));
      if (state.finalError) throw state.finalError;
      if (receiptModel) {
        const generation = args[1] as number;
        const disposition = args[3] as RecoveryFinalDisposition;
        const committed = committedReceipts.get(generation);
        if (committed) {
          assert.deepEqual(disposition, committed, "committed FINAL receipts cannot be replaced");
        } else {
          assert.equal(disposition.kind, "archive");
          const capture = [...captures.values()].find(c => c.id === disposition.capture_id);
          assert.ok(capture);
          if (capture.state === "expired" || Date.parse(capture.expires ?? "") <= state.now) {
            throw new RequestError("POST", "/api/worker/runs/run-1/recovery/release", 409, "capture expired");
          }
          assert.equal(disposition.source_sha, capture.request.source_sha);
          assert.equal(disposition.coverage_digest, capture.request.coverage_digest);
          assert.ok(capture.manifest);
          await state.beforeFinalAck?.();
          committedReceipts.set(generation, Object.freeze(structuredClone(disposition)));
          state.open = false;
        }
      }
      if (state.loseAck) { state.open = false; throw new Error("ACK lost"); }
      if (state.closeOnRelease && !state.wrongAck) state.open = false; // opt-in: a real server closes the hold
      return { run_id: state.wrongAck ? "other-run" : args[0], generation: args[1] as number, released: true, holds_released: 1 };
    },
  };
  const git = {
    recoverySettlementRoot: path.join(root, "settlement"),
    deleteRecoveryPin: async () => { state.pinDeletes++; state.candidates = []; },
    readInventoryCloneHeads: async () => state.cloneReadable
      ? ({ kind: "verified", heads: state.cloneHeads, clones: [], foreignOwners: [] }) : ({ kind: "unknown", cause: state.cloneCause }),
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
    produceRecoveryBundle: async (_bare: string, opts: { outPath: string; sourceSha: string; forgeTip?: string; guardedDefaultBranch?: string }) => {
      state.produced++;
      state.onProduce?.();
      await state.produceWait;
      assert.equal(opts.forgeTip, undefined);
      if (state.oversized) throw new RecoveryBundleTooLargeError(100, 10);
      if (state.failProduce) throw new Error("bundle failure");
      assert.equal(opts.guardedDefaultBranch, "main");
      // Valid empty pack for the fake Git seam; the advertised source is exact.
      const pack = Buffer.concat([Buffer.from("PACK"), Buffer.from([0, 0, 0, 2, 0, 0, 0, 0])]);
      const bytes = Buffer.concat([Buffer.from(`# v2 git bundle\n${state.thin ? `-${H} retained dependency\n` : ""}${opts.sourceSha} refs/heads/recovered-source\n\n`),
        pack, createHash("sha1").update(pack).digest()]);
      await fs.writeFile(opts.outPath, bytes);
      return { bundlePath: opts.outPath, byteSize: bytes.length,
        checksum: createHash("sha256").update(bytes).digest("hex"),
        chunkCount: 1, prerequisiteShas: [], selfContained: true, alreadyPublished: false };
    },
  };
  const log = nullLogger();
  log.warn = (message, fields) => {
    if (message === "recovery: inventory source boundary retained") logs.push(fields!);
  };
  const make = (sourceBoundary: boolean | "runner" = false) => {
    let physical: { withInventorySourceBoundary(context: InventorySourceBoundaryContext,
      action: (prove: () => Promise<boolean>) => Promise<void>): Promise<"passed" | "retained"> };
    const coordinator = new RecoveryCoordinator({
    recoveryRoot: path.join(root, "journal"), workerToken: "local-worker-fixture",
    log, client: client as never, git: git as never, now: () => state.now,
    withInventorySourceBoundary: sourceBoundary === "runner" ? (context, action) =>
      physical.withInventorySourceBoundary(context, action) : sourceBoundary ? async (context, action) => {
      if (state.sourceRefused) {
        if (state.diagnostic) context.reportDiagnostic?.(state.diagnostic);
        return "retained";
      }
      await action(async () => {
        if (state.sourceRefused || state.sourceProofRefused) {
          if (state.diagnostic) context.reportDiagnostic?.(state.diagnostic);
          return false;
        }
        return true;
      });
      return "passed";
    } : undefined,
    });
    if (sourceBoundary === "runner") physical = new RunRunner(client as never, git as never,
      () => { throw new Error("unreached executor"); }, nullLogger(), 20, undefined,
      { recovery: coordinator }) as unknown as typeof physical;
    return coordinator;
  };
  const coordinator = make(sourceBoundary);
  const freeze = (originalSourceSha = H) => coordinator.freezeInventory({
    context, currentSha: H, originalSourceSha, defaultBranch: "main",
  });
  const capture = (record: RecoveryRecord) => coordinator.captureAndUpload({
    record, barePath: context.barePath, defaultBranch: "main",
  });
  return { root, context, state, logs, aggregates, finals, coordinator, make, freeze, capture, git,
    captures, reserveKeys, committedReceipts, client, log,
    reserves: () => reserves, close: () => fs.rm(root, { recursive: true, force: true }) };
}

async function pendingReceiptFixture(savedFinal = true) {
  const f = await fixture(false, os.tmpdir(), true);
  if (savedFinal) f.state.finalError = new Error("unaccepted FINAL");
  else f.state.expires = "1970-01-01T00:00:00Z";
  const record = await f.freeze();
  assert.ok(record);
  await f.capture(record);
  f.state.finalError = undefined;
  f.state.expires = "2099-01-01T00:00:00Z";
  const before = (await f.coordinator.inspect("run-1"))[0]!;
  return { f, before, capture: f.captures.get(f.reserveKeys[0]!)! };
}

for (const state of ["expired", "available", "preparing", "uploading", "discarded", "needs_action"]) {
  const reasons = state === "needs_action"
    ? ["upload_retry_window_exhausted", "storage quota exceeded", "upload failed; retry available"] : [undefined];
  for (const reason of reasons) for (const saved of [false, true]) {
    it(`issue2416 replacement allowlist ${state}/${reason} saved=${saved}`, async () => {
      const { f, before, capture } = await pendingReceiptFixture(saved);
      try {
        capture.state = state; capture.reason = reason; capture.expires = "1970-01-01T00:00:00Z";
        await f.make().resumePending(undefined, [before]);
        const after = (await f.coordinator.inspect("run-1"))[0]!;
        assert.equal(after.finalAcknowledged, true);
        assert.equal(after.captureId, before.captureId);
        assert.equal(after.bundlePath, before.bundlePath);
        assert.equal(f.state.produced, 1);
        assert.equal(f.reserveKeys.length, 2);
        assert.equal(f.reserveKeys[0], before.captureId, "legacy reserve key fallback");
        assert.match(after.reserveIdempotencyKey!, /^[0-9a-f-]{36}$/);
      } finally { await f.close(); }
    });
  }
}

for (const variant of ["checksum", "size", "expired-checksum", "expired-size", "discarded-checksum", "discarded-size", "integrity", "unknown", "absent", "unbound"]) {
  for (const saved of [false, true]) for (const expired of [false, true]) {
    it(`issue2416 retained ${variant} saved=${saved} expired=${expired} keeps bytes and cleanup pending`, async () => {
      const { f, before, capture } = await pendingReceiptFixture(saved);
      try {
        capture.expires = expired ? "1970-01-01T00:00:00Z" : "2099-01-01T00:00:00Z";
        capture.state = variant.startsWith("expired") ? "expired" : variant.startsWith("discarded") ? "discarded" :
          ["integrity", "unknown", "absent"].includes(variant) ? "needs_action" : "available";
        if (variant.includes("checksum")) capture.manifest!.checksum = "0".repeat(64);
        if (variant.includes("size")) capture.manifest!.byte_size++;
        if (variant === "integrity") capture.reason = "archive integrity check failed";
        if (variant === "unknown") capture.reason = "unexpected failure";
        if (variant === "unbound") delete capture.manifest;
        const bytes = await fs.readFile(before.bundlePath!);
        const pins = structuredClone(f.state.candidates);
        const finals = f.finals.length;
        const remote = structuredClone([...f.captures]);
        const outcome = await f.capture(before);
        const reason = variant.includes("checksum") || variant.includes("size") ? "capture_manifest_integrity_mismatch" :
          variant === "unbound" ? "available_capture_manifest_unbound" : "capture_failure_requires_attention";
        assert.equal(outcome.reason, reason, "foreground outcome exposes bounded owner attention");
        const expected = { ...before, reason };
        assert.deepEqual((await f.coordinator.inspect("run-1"))[0], expected, "only bounded reason may change");
        assert.deepEqual([...f.captures], remote, "remote manifests and captures are untouched");
        assert.equal(f.state.uploads, 1);
        assert.equal(f.reserveKeys.length, 1);
        assert.equal(f.finals.length, finals);
        assert.deepEqual(await fs.readFile(before.bundlePath!), bytes);
        assert.equal(await f.make().inventoryCleanupState("run-1", 7), "pending");
        await f.make().forgetGeneration("run-1", 7);
        assert.deepEqual((await f.coordinator.inspect("run-1"))[0], expected);
        assert.deepEqual(await fs.readFile(before.bundlePath!), bytes);
        assert.deepEqual(f.state.candidates, pins);
        assert.equal(f.state.pinDeletes, 0, "forget must not invoke the pin deletion seam before ACK");
      } finally { await f.close(); }
    });
  }
}

for (const state of ["preparing", "uploading", "needs_action"]) {
  it(`issue2416 incomplete unbound ${state} explicitly replaces before expiry`, async () => {
    const { f, before, capture } = await pendingReceiptFixture();
    try {
      capture.state = state;
      capture.expires = "2099-01-01T00:00:00Z";
      capture.reason = state === "needs_action" ? "upload_retry_window_exhausted" : undefined;
      delete capture.manifest;
      await f.make().resumePending(undefined, [before]);
      assert.equal(f.reserveKeys.length, 2);
      assert.equal((await f.coordinator.inspect("run-1"))[0]!.finalAcknowledged, true);
    } finally { await f.close(); }
  });
}

it("issue2416 replacement FINAL lost ACK preserves custody then exact accepted receipt avoids third key", async () => {
  const { f, before, capture } = await pendingReceiptFixture();
  try {
    capture.state = "expired";
    await patchAuthenticated(f, before, { recoveryPinBareDir: "repo.git" });
    const bytes = await fs.readFile(before.bundlePath!);
    const pins = structuredClone(f.state.candidates);
    const client = f.client as RecoveryArchiveClient;
    const reconcile = client.reconcileRecoveryCapture!;
    client.reconcileRecoveryCapture = async (...args) => {
      if (f.committedReceipts.has(7)) throw new Error("reconciliation temporarily unavailable after lost ACK");
      return reconcile(...args);
    };
    f.state.loseAck = true;
    await f.make().resumePending(undefined, [before]);
    const pending = (await f.coordinator.inspect("run-1"))[0]!;
    assert.ok(pending.finalRequest);
    assert.notDeepEqual(pending.finalRequest, before.finalRequest);
    assert.notEqual(pending.finalAcknowledged, true);
    assert.equal(await f.make().inventoryCleanupState("run-1", 7), "pending");
    await f.make().forgetGeneration("run-1", 7);
    assert.deepEqual((await f.make().inspect("run-1"))[0], pending);
    assert.deepEqual(await fs.readFile(pending.bundlePath!), bytes);
    assert.deepEqual(f.state.candidates, pins);
    assert.equal(f.state.pinDeletes, 0);
    assert.equal(f.reserveKeys.length, 2);
    const finals = f.finals.length;
    client.reconcileRecoveryCapture = reconcile;
    // Force reconciliation rather than a healthy status replay of FINAL.
    f.captures.get(pending.reserveIdempotencyKey!)!.state = "expired";
    await f.make().resumePending(undefined, [pending]);
    const accepted = (await f.make().inspect("run-1"))[0]!;
    assert.equal(accepted.finalAcknowledged, true);
    assert.deepEqual(accepted.finalRequest, pending.finalRequest);
    assert.equal(accepted.reserveIdempotencyKey, pending.reserveIdempotencyKey);
    assert.equal(f.reserveKeys.length, 2);
    assert.equal(f.finals.length, finals);
    assert.equal(await f.make().inventoryCleanupState("run-1", 7), "acknowledged");
  } finally { await f.close(); }
});

for (const newer of ["identity", "acknowledged"]) {
  it(`issue2416 stale replaceable response preserves newer ${newer} journal`, async () => {
    const { f, before, capture } = await pendingReceiptFixture();
    try {
      capture.state = "expired";
      const client = f.client as RecoveryArchiveClient;
      const reconcile = client.reconcileRecoveryCapture!;
      let latest: RecoveryRecord | undefined;
      client.reconcileRecoveryCapture = async (...args) => {
        const response = await reconcile(...args);
        const patch = newer === "acknowledged" ? { finalAcknowledged: true } : {
          serverCaptureId: "new-server-identity", reserveIdempotencyKey: "00000000-0000-4000-8000-000000000241",
          finalRequest: { disposition: { ...before.finalRequest!.disposition, capture_id: "new-server-identity" } },
        };
        await patchAuthenticated(f, before, patch);
        latest = (await f.make().inspect("run-1"))[0]!;
        return response;
      };
      await f.make().resumePending(undefined, [before]);
      assert.ok(latest);
      assert.deepEqual((await f.make().inspect("run-1"))[0], latest);
      assert.equal(f.reserveKeys.length, 1);
      assert.equal(f.state.uploads, 1);
    } finally { await f.close(); }
  });
}

it("issue2416 retained manifest invalid reason is bounded and stale reason writes are refused", async () => {
  const { f, before, capture } = await pendingReceiptFixture();
  try {
    capture.state = "expired";
    const client = f.client as RecoveryArchiveClient;
    client.reconcileRecoveryCapture = async (run, id, req) => ({
      run_id: run, capture_id: id, generation: req.generation,
      outcome: "retained", reason: "capture_manifest_invalid",
    });
    assert.equal((await f.capture(before)).reason, "capture_manifest_invalid");
    const current = (await f.make().inspect("run-1"))[0]!;
    client.reconcileRecoveryCapture = async (run, id, req) => {
      await patchAuthenticated(f, current, { reserveIdempotencyKey: "00000000-0000-4000-8000-000000000241" });
      return { run_id: run, capture_id: id, generation: req.generation,
        outcome: "retained", reason: "capture_failure_requires_attention" };
    };
    await f.capture(current);
    const latest = (await f.make().inspect("run-1"))[0]!;
    assert.equal(latest.reason, "capture_manifest_invalid");
    assert.equal(latest.reserveIdempotencyKey, "00000000-0000-4000-8000-000000000241");
    assert.deepEqual(latest.finalRequest, before.finalRequest);
    assert.equal(f.reserveKeys.length, 1);
  } finally { await f.close(); }
});

for (const same of [true, false]) {
  it(`issue2416 accepted expired receipt exact=${same} never reserves`, async () => {
    const f = await fixture(false, os.tmpdir(), true);
    try {
      f.state.loseAck = true;
      const reconcile = f.client.reconcileRecoveryCapture;
      delete f.client.reconcileRecoveryCapture;
      const record = await f.freeze(); assert.ok(record);
      await f.capture(record);
      f.client.reconcileRecoveryCapture = reconcile;
      const before = (await f.coordinator.inspect("run-1"))[0]!;
      f.captures.get(f.reserveKeys[0]!)!.state = "expired";
      f.captures.get(f.reserveKeys[0]!)!.manifest!.checksum = "0".repeat(64);
      if (!same) f.committedReceipts.set(7, { ...before.finalRequest!.disposition, source_sha: H2 });
      await f.make().resumePending(undefined, [before]);
      const after = (await f.coordinator.inspect("run-1"))[0]!;
      assert.equal(after.finalAcknowledged === true, same);
      assert.deepEqual(after.finalRequest, before.finalRequest);
      assert.equal(f.reserveKeys.length, 1);
      assert.equal(f.finals.length, 1);
    } finally { await f.close(); }
  });
}

for (const mode of ["foreground", "boot", "live", "concurrent"]) {
  it(`issue2416 ${mode} replacement uses existing generation lock`, async () => {
    const { f, before, capture } = await pendingReceiptFixture();
    try {
      capture.state = "expired";
      const restarted = f.make();
      if (mode === "foreground") await f.capture(before);
      if (mode === "boot") await restarted.resumePending(undefined, [before]);
      if (mode === "live") await restarted.resumeLive({ authenticatedAtMs: f.state.now, isExecuting: () => false });
      if (mode === "concurrent") {
        let active = 0, maximum = 0;
        let entered!: () => void, release!: () => void;
        const entry = new Promise<void>(resolve => { entered = resolve; });
        const barrier = new Promise<void>(resolve => { release = resolve; });
        const timer = setTimeout(release, 2000);
        const upload = f.client.uploadRecoveryBundle;
        f.client.uploadRecoveryBundle = async (...args) => {
          active++; maximum = Math.max(maximum, active); entered();
          try { await barrier; return await upload(...args); } finally { active--; }
        };
        const first = f.capture(before);
        const tasks: Promise<unknown>[] = [first];
        try {
          await Promise.race([entry, new Promise<never>((_, reject) => {
            const timeout = setTimeout(() => reject(new Error("upload barrier not reached")), 3000);
            entry.finally(() => clearTimeout(timeout));
          })]);
          const siblings = [restarted.resumePending(undefined, [before]), f.capture(before)];
          tasks.push(...siblings);
          await new Promise<void>(resolve => setTimeout(resolve, 25));
          assert.equal(active, 1);
          release();
          await Promise.all([first, ...siblings]);
          assert.equal(maximum, 1, "concurrent boot/foreground uploads never overlap");
          assert.equal(active, 0);
        } finally { clearTimeout(timer); release(); await Promise.allSettled(tasks); }
      }
      assert.equal(f.reserveKeys.length, 2);
      assert.equal(f.state.uploads, 2);
      assert.equal((await f.coordinator.inspect("run-1"))[0]!.finalAcknowledged, true);
    } finally { await f.close(); }
  });
}

it("issue2416 immediately unusable replacement is bounded to one reserve per pass", async () => {
  const { f, before, capture } = await pendingReceiptFixture();
  try {
    capture.state = "expired";
    f.state.expires = "1970-01-01T00:00:00Z";
    await f.make().resumePending(undefined, [before]);
    assert.equal(f.reserveKeys.length, 2);
    assert.equal(f.state.uploads, 2);
    assert.equal((await f.coordinator.inspect("run-1"))[0]!.finalAcknowledged, undefined);
    assert.equal(await f.make().inventoryCleanupState("run-1", 7), "pending");
  } finally { await f.close(); }
});

for (const lostAck of [true, false]) {
  it(`issue2416 persisted replacement key survives restart reserve failure lostAck=${lostAck}`, async () => {
    const { f, before, capture } = await pendingReceiptFixture();
    try {
      capture.state = "expired";
      await patchAuthenticated(f, before, { finalAcknowledged: false, recoveryPinBareDir: "repo.git" });
      const bytes = await fs.readFile(before.bundlePath!);
      const pins = structuredClone(f.state.candidates);
      const assertPending = async () => {
        assert.equal(await f.make().inventoryCleanupState("run-1", 7), "pending");
        const journal = await f.make().inspect("run-1");
        await f.make().forgetGeneration("run-1", 7);
        assert.deepEqual(await f.make().inspect("run-1"), journal);
        assert.deepEqual(await fs.readFile(before.bundlePath!), bytes);
        assert.deepEqual(f.state.candidates, pins);
        assert.equal(f.state.pinDeletes, 0);
      };
      const reserve = f.client.reserveRecoveryCapture;
      let fail = true;
      f.client.reserveRecoveryCapture = async (run, req) => {
        const persisted = (await f.coordinator.inspect("run-1"))[0]!;
        assert.equal(persisted.reserveIdempotencyKey, req.idempotency_key);
        assert.equal(Object.hasOwn(persisted, "finalAcknowledged"), false);
        assert.equal(persisted.finalRequest, undefined);
        if (!fail) return reserve(run, req);
        if (lostAck) await reserve(run, req);
        else { f.reserveKeys.push(req.idempotency_key); }
        throw new RequestError("POST", "/archives/reserve", lostAck ? 500 : 507, "storage quota exceeded");
      };
      await f.make().resumePending(undefined, [before]);
      const pending = (await f.coordinator.inspect("run-1"))[0]!;
      assert.ok(pending.reserveIdempotencyKey);
      assert.equal(pending.serverCaptureId, undefined);
      await assertPending();
      await f.make().resumePending(undefined, [pending]);
      assert.equal((await f.coordinator.inspect("run-1"))[0]!.reserveIdempotencyKey, pending.reserveIdempotencyKey);
      await assertPending();
      fail = false;
      await f.make().resumePending(undefined, [pending]);
      assert.equal((await f.coordinator.inspect("run-1"))[0]!.finalAcknowledged, true);
      assert.deepEqual(new Set(f.reserveKeys.slice(1)), new Set([pending.reserveIdempotencyKey]));
      assert.equal(await f.make().inventoryCleanupState("run-1", 7), "acknowledged");
      // #2464: an acknowledged guarded generation is removed only through guarded cleanup.
      Object.assign(f.git, { cleanupRecoveryGeneration: async () => "removed" });
      await f.make().forgetGeneration("run-1", 7);
      assert.deepEqual(await f.make().inspect("run-1"), []);
      await assert.rejects(fs.readFile(before.bundlePath!), { code: "ENOENT" });
    } finally { await f.close(); }
  });
}

it("issue2416 authenticated malformed reservation UUID and tampering are refused", async () => {
  const { f, before } = await pendingReceiptFixture();
  try {
    await patchAuthenticated(f, before, { reserveIdempotencyKey: "invalid-uuid" });
    assert.deepEqual(await f.make().inspect("run-1"), []);
    await patchAuthenticated(f, before, { reserveIdempotencyKey: "00000000-0000-4000-8000-000000000241" });
    assert.equal((await f.make().inspect("run-1"))[0]!.reserveIdempotencyKey, "00000000-0000-4000-8000-000000000241");
    const file = path.join(f.root, "journal", before.runId, before.captureId + ".json");
    await fs.writeFile(file, (await fs.readFile(file, "utf8")).replace("000000000241", "000000000242"));
    assert.deepEqual(await f.make().inspect("run-1"), []);
  } finally { await f.close(); }
});

for (const variant of ["missing", "route", "auth", "transport", "run", "generation", "capture", "outcome", "receipt", "evidence", "reason", "integrity-contradiction", "status", "hold", "null", "array", "retained-unknown", "retained-receipt", "retained-evidence"]) {
  it(`issue2416 reconciliation fails closed for ${variant}`, async () => {
    const { f, before, capture } = await pendingReceiptFixture();
    try {
      capture.state = "expired";
      const client = f.client as RecoveryArchiveClient;
      const reconcile = client.reconcileRecoveryCapture!;
      if (variant === "missing") delete client.reconcileRecoveryCapture;
      else client.reconcileRecoveryCapture = async (run, id, req) => {
        if (["route", "auth", "transport"].includes(variant)) throw new RequestError("POST", "/archives/reconcile", variant === "route" ? 404 : variant === "auth" ? 401 : 500, "unavailable");
        if (variant === "null") return null as never;
        if (variant === "array") return [] as never;
        const result = await reconcile(run, id, req);
        if (variant.startsWith("retained-")) {
          result.outcome = "retained";
          result.reason = variant === "retained-unknown" ? "unbounded raw failure " + "x".repeat(2048) : "capture_manifest_invalid";
          if (variant === "retained-receipt") result.final_receipt = before.finalRequest!.disposition;
          if (variant === "retained-evidence") result.release_evidence = "archive";
        }
        if (variant === "run") result.run_id = "other";
        if (variant === "generation") result.generation++;
        if (variant === "capture") result.capture_id = "other";
        if (variant === "outcome") result.outcome = "unknown" as never;
        if (variant === "receipt") result.final_receipt = before.finalRequest!.disposition;
        if (variant === "evidence") result.release_evidence = "archive";
        if (variant === "reason") result.reason = "unknown";
        if (variant === "integrity-contradiction" || variant === "status") return { run_id: run, generation: req.generation, capture_id: id, outcome: "replaceable" };
        return result;
      };
      if (variant === "integrity-contradiction") capture.manifest!.checksum = "0".repeat(64);
      if (variant === "status") client.getRecoveryCaptureStatus = async () => ({ capture_id: "other", state: "expired", manifest_bound: true });
      if (variant === "hold") client.listRecoveryHolds = async () => { throw new Error("hold transport failed"); };
      const finals = f.finals.length;
      await f.make().resumePending(undefined, [before]);
      assert.deepEqual((await f.coordinator.inspect("run-1"))[0], before);
      assert.equal(f.reserveKeys.length, 1);
      assert.equal(f.finals.length, finals);
    } finally { await f.close(); }
  });
}

for (const seam of ["reconcile", "retained-reconcile", "hold"]) {
  it(`issue2416 quarantine arriving during ${seam} prevents reset and replacement RPCs`, async () => {
    const { latchResidueQuarantine, resetResidueQuarantineForTests } = await import("../src/residue-quarantine.js");
    const { f, before, capture } = await pendingReceiptFixture();
    try {
      capture.state = "expired";
      const client = f.client as RecoveryArchiveClient;
      if (seam === "reconcile" || seam === "retained-reconcile") {
        const reconcile = client.reconcileRecoveryCapture!;
        client.reconcileRecoveryCapture = async (...args) => {
          const result = await reconcile(...args);
          latchResidueQuarantine({ cause: "fixture", site: "reconcile_return" }, nullLogger());
          return seam === "retained-reconcile" ? { ...result, outcome: "retained", reason: "capture_manifest_invalid" } : result;
        };
      } else {
        const hold = client.listRecoveryHolds;
        client.listRecoveryHolds = async (...args) => {
          const result = await hold(...args);
          latchResidueQuarantine({ cause: "fixture", site: "hold_return" }, nullLogger());
          return result;
        };
      }
      const finals = f.finals.length;
      await f.make().resumePending(undefined, [before]);
      assert.deepEqual((await f.coordinator.inspect("run-1"))[0], before);
      assert.equal(f.reserveKeys.length, 1);
      assert.equal(f.state.uploads, 1);
      assert.equal(f.finals.length, finals);
    } finally { resetResidueQuarantineForTests(); await f.close(); }
  });
}

async function patchAuthenticated(f: Awaited<ReturnType<typeof fixture>>, record: RecoveryRecord, patch: Partial<RecoveryRecord>) {
  const file = path.join(f.root, "journal", record.runId, record.captureId + ".json");
  const envelope = JSON.parse(await fs.readFile(file, "utf8"));
  const { mac: _mac, ...payload } = envelope;
  Object.assign(payload, patch);
  const key = createHmac("sha256", "local-worker-fixture").update("uzi-recovery-journal-v1").digest();
  await fs.writeFile(file, JSON.stringify({ ...payload, mac: createHmac("sha256", key).update(canonicalJson(payload)).digest("hex") }));
}

for (const savedFinal of [false, true]) {
  it(`issue2416 core expired ${savedFinal ? "saved-unaccepted FINAL" : "uploaded no FINAL"} reserves replacement and acknowledges stable journal`, async () => {
    const f = await fixture(false, os.tmpdir(), true);
    try {
      if (savedFinal) f.state.finalError = new Error("FINAL rejected before acceptance");
      else f.state.expires = "1970-01-01T00:00:00Z";
      const record = await f.freeze();
      assert.ok(record);
      await f.capture(record);
      const before = (await f.coordinator.inspect("run-1")).find(r => r.captureId === record.captureId)!;
      assert.equal(before.state, "uploaded");
      assert.equal(!!before.finalRequest, savedFinal);
      assert.notEqual(before.finalAcknowledged, true);
      assert.equal(f.committedReceipts.size, 0, "the original FINAL was never accepted");
      assert.equal(f.state.open, true);
      assert.equal(f.reserveKeys.length, 1);
      const oldKey = f.reserveKeys[0]!;
      const oldCapture = f.captures.get(oldKey)!;
      oldCapture.state = "expired";
      oldCapture.expires = "1970-01-01T00:00:00Z";
      f.state.expires = "2099-01-01T00:00:00Z";
      f.state.finalError = undefined;
      const pins = structuredClone(f.state.candidates);
      const bundle = await fs.readFile(before.bundlePath!);
      const savedIdentity = structuredClone(before.finalRequest);
      let ackBoundaryChecked = false;
      f.state.beforeFinalAck = async () => {
        const pending = (await f.coordinator.inspect("run-1")).find(r => r.captureId === record.captureId)!;
        assert.ok(pending, "the original source journal survives replacement");
        assert.notEqual(pending.finalAcknowledged, true);
        assert.equal(pending.bundlePath, before.bundlePath);
        assert.deepEqual(await fs.readFile(pending.bundlePath!), bundle, "bundle bytes survive until ACK");
        assert.deepEqual(f.state.candidates, pins, "owed pins survive until ACK");
        assert.deepEqual(pending.originalRoots, before.originalRoots);
        assert.equal(pending.sourceSha, before.sourceSha);
        assert.equal(pending.coverageDigest, before.coverageDigest);
        assert.equal(await f.make().inventoryCleanupState("run-1", 7), "pending");
        ackBoundaryChecked = true;
      };
      await f.make().resumePending(undefined, [before]);
      assert.ok(f.reserveKeys.length > 1, "expired unaccepted capture must reserve a replacement");
      assert.notEqual(f.reserveKeys[1], oldKey, "replacement uses a distinct server idempotency key");
      assert.equal(f.state.produced, 1, "replacement reuses the verified local bundle");
      assert.equal(f.state.uploads, 2);
      assert.ok(ackBoundaryChecked, "replacement reaches FINAL while local custody is retained");
      const after = (await f.coordinator.inspect("run-1")).find(r => r.captureId === record.captureId)!;
      assert.ok(after, "replacement preserves the stable journal captureId");
      assert.equal(after.finalAcknowledged, true);
      assert.notEqual(after.serverCaptureId, before.serverCaptureId);
      assert.equal(after.finalRequest?.disposition.capture_id, after.serverCaptureId);
      assert.deepEqual(before.finalRequest, savedIdentity, "the saved request snapshot remains immutable");
      assert.deepEqual(f.committedReceipts.get(7), after.finalRequest?.disposition);
      assert.equal(f.committedReceipts.size, 1);
      assert.equal(f.state.open, false);
      assert.equal(await f.make().inventoryCleanupState("run-1", 7), "acknowledged");
    } finally { await f.close(); }
  });
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

it("issue2416 live FINAL success that forgets the generation reports uploaded, not record_removed", async () => {
  const f = await fixture(false, os.tmpdir(), true);
  try {
    const record = await f.freeze();
    assert.ok(record);
    f.state.finalError = new Error("network unavailable");
    assert.equal((await f.capture(record)).reason, "upload_transient");
    const pending = (await f.coordinator.inspect("run-1"))[0]!;
    assert.equal(pending.state, "uploaded");
    assert.ok(pending.finalRequest);
    const outcomes: unknown[] = [];
    f.log.info = (message, fields) => {
      if (message === "recovery live re-drive") outcomes.push((fields as { outcome?: unknown }).outcome);
    };
    Object.assign(f.git, { cleanupRecoveryGeneration: async () => "removed" });
    f.state.finalError = undefined;
    f.state.now += 60_000;
    await f.coordinator.resumeLive({ isExecuting: () => false, authenticatedAtMs: f.state.now });
    assert.equal(f.finals.length, 2, "the live pass replays the saved FINAL");
    assert.deepEqual(await f.coordinator.inspect("run-1"), [], "guarded cleanup removed the generation");
    await assert.rejects(fs.readFile(pending.bundlePath!), { code: "ENOENT" });
    assert.deepEqual(outcomes, ["uploaded"]);
  } finally { await f.close(); }
});

for (const variant of ["network", "5xx", "timeout", "generation_not_ended", "attention"] as const) {
  it(`issue2416 retained FINAL reconciliation preserves retry classification ${variant}`, async () => {
    const f = await fixture(false, os.tmpdir(), true);
    try {
      const warnings: string[] = [];
      f.log.warn = message => { warnings.push(message); };
      const record = await f.freeze();
      assert.ok(record);
      f.state.finalError = variant === "5xx"
        ? new RequestError("POST", "/api/worker/runs/run-1/recovery/release", 503, "service unavailable")
        : variant === "timeout" ? new DOMException("FINAL timed out", "TimeoutError")
        : new Error("network unavailable");
      const reason = variant === "generation_not_ended" ? "generation_not_ended"
        : variant === "attention" ? "capture_failure_requires_attention" : "capture_available";
      const reconcile = f.client.reconcileRecoveryCapture!;
      let reconciliations = 0;
      f.client.reconcileRecoveryCapture = async (run, id, request) => {
        reconciliations++;
        const response = await reconcile(run, id, request);
        assert.equal(response.outcome, "retained");
        assert.ok("reason" in response);
        assert.equal(response.reason, "capture_available");
        return { ...response, reason };
      };
      const outcome = await f.capture(record);
      assert.equal(reconciliations, 1, "the receipt endpoint is reached after FINAL fails");
      const expected = variant === "attention" ? reason : "upload_transient";
      assert.equal(outcome.reason, expected);
      const before = (await f.coordinator.inspect("run-1"))[0]!;
      assert.equal(before.state, "uploaded");
      assert.ok(before.finalRequest);
      assert.equal(before.finalAcknowledged, undefined);
      assert.equal(before.reason, variant === "attention" ? reason : undefined);
      const capture = [...f.captures.values()][0]!;
      assert.ok(capture.manifest, "the retained capture has healthy available bytes");
      assert.ok(Date.parse(capture.expires!) > f.state.now);
      const bytes = await fs.readFile(before.bundlePath!);
      const pins = structuredClone(f.state.candidates);
      const assertPending = async () => {
        const current = (await f.coordinator.inspect("run-1"))[0]!;
        assert.deepEqual(current, before, "retries preserve the journal and exact FINAL identity");
        assert.deepEqual(await fs.readFile(before.bundlePath!), bytes);
        assert.deepEqual(f.state.candidates, pins);
        assert.equal(f.state.pinDeletes, 0);
        assert.equal(await f.coordinator.inventoryCleanupState("run-1", 7), "pending");
        assert.equal(f.reserves(), 1);
        assert.equal(f.state.uploads, 1);
        assert.equal(f.state.produced, 1);
        assert.ok(f.finals.every(request => canonicalJson(request) === canonicalJson(f.finals[0])));
        assert.equal(warnings.includes("recovery: inventory requires owner attention"), variant === "attention");
      };
      const live = () => f.coordinator.resumeLive({ isExecuting: () => false, authenticatedAtMs: f.state.now });
      await live();
      assert.equal(f.finals.length, 2, "first transient live pass reaches FINAL");
      await assertPending();
      if (variant === "attention") {
        f.state.now += 30_000;
        await live();
        assert.equal(f.finals.length, 3, "attention reasons retain the ordinary live interval");
        await assertPending();
      } else {
        f.state.now += 60_000 - 1;
        await live();
        assert.equal(f.finals.length, 2, "first transient delay is 60 seconds");
        f.state.now++;
        await live();
        assert.equal(f.finals.length, 3, "second transient live pass reaches FINAL");
        await assertPending();
        f.state.now += 120_000 - 1;
        await live();
        assert.equal(f.finals.length, 3, "second transient delay doubles to 120 seconds");
        f.state.now++;
        f.state.finalError = undefined;
        await live();
        assert.equal(f.finals.length, 4);
        const after = (await f.coordinator.inspect("run-1"))[0]!;
        assert.equal(after.finalAcknowledged, true);
        assert.deepEqual(after.finalRequest, before.finalRequest);
        assert.deepEqual(await fs.readFile(before.bundlePath!), bytes);
        assert.deepEqual(f.state.candidates, pins);
        assert.equal(f.state.pinDeletes, 0);
      }
    } finally { await f.close(); }
  });
}

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
    assert.deepEqual(await coordinator.inspect(producer.runId), []);
    await assert.rejects(fs.stat(path.join(recoveryRoot, producer.runId)), { code: "ENOENT" });
    assert.equal(git(bare, ["for-each-ref", "--format=%(refname)", "refs/uzi-coverage/", "refs/uzi-recovery-pin/", "refs/uzi-owed/"]), "");
    assert.deepEqual(await restartedCache.enumerateOwedCandidates(bare, producer.runId), []);
    const freshCache = new GitCache(data, nullLogger(), undefined, testGitCacheOptions());
    assert.equal((await freshCache.committedTrackingOwnership(bare, producer.branch, producer.runId, h, 1)).kind, "owned",
      "archive cleanup preserves receipt identity and does not retire publication proof");
    await coordinator.snapshotOwedInventory();
    assert.deepEqual(await coordinator.materializeBootInventory(), [], "retained contexts never reopen closed holds");
    assert.equal(reserves, 1);
    assert.equal(uploads, 1);
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
  let feature = true;
  const coordinator = new RecoveryCoordinator({
    recoveryRoot: root, workerToken: "local-worker-fixture", log: nullLogger(),
    client: {
      hasFeature: () => feature,
      releaseRecoveryCustody: async (...args: unknown[]) => {
        calls.push(args);
        return { run_id: "run-1", released: true, holds_released: 1, generation: 7 };
      },
      listRecoveryHolds: async () => ({ run_id: "run-1", holds: [
        { hold_id: "hold-7", generation: 7, inventory_guarded: true, has_available_capture: false },
      ] }),
    } as never,
    git: {} as never,
  });
  try {
    await coordinator.pin({
      runId: "run-1", generation: 7, sourceSha: "a".repeat(40),
      kind: "issue", branch: "task", inventoryGuarded: true,
    });
    feature = false;
    await coordinator.release("run-1", 7, "publication");
    assert.deepEqual(calls, []);
    assert.equal((await coordinator.inspect("run-1")).length, 1);
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});


const diagnosticRetryReasons = [
  "inventory_source_not_quiescent", "inventory_source_not_quiescent:inventory_read_not_verified",
  ...["execution_tail_present", "boundary_exception", "non_linux_host", "live_attempt_path",
    "process_not_quiescent_before_git", "process_not_quiescent_after_git", "worktree_status_dirty",
    "worktree_status_unreadable", "source_identity_changed", "physical_sources_without_boundary"]
    .map(check => "inventory_source_not_quiescent:" + check),
  ...sourceDiagnosticCauses.map(cause => "inventory_source_not_quiescent:inventory_read_not_verified:" + cause),
];
const unknownDiagnosticReasons = [
  "inventory_source_not_quiescent:invented",
  "inventory_source_not_quiescent:worktree_status_dirty:extra",
  "inventory_source_not_quiescent:inventory_read_not_verified:invented",
  "inventory_source_not_quiescent:inventory_read_not_verified:other:extra",
];

async function authenticateDiagnosticRecord(f: Awaited<ReturnType<typeof fixture>>, record: RecoveryRecord, state: string, reason: string) {
  const file = path.join(f.root, "journal", "run-1", record.captureId + ".json");
  const saved = JSON.parse(await fs.readFile(file, "utf8"));
  const { mac: _oldMac, ...payload } = { ...saved, state, reason };
  const key = createHmac("sha256", "local-worker-fixture").update("uzi-recovery-journal-v1").digest();
  const mac = createHmac("sha256", key).update(canonicalJson(payload)).digest("hex");
  await fs.writeFile(file, JSON.stringify({ ...payload, mac }));
}

for (const reason of [...diagnosticRetryReasons, ...unknownDiagnosticReasons]) {
  it(`#2507 needs_action exact retry allowlist ${reason}`, async () => {
    const f = await fixture(true);
    try {
      const record = await f.freeze();
      assert.ok(record);
      f.state.finalError = new Error("ACK unavailable");
      await f.capture(record);
      const before = (await f.coordinator.inspect("run-1"))[0]!;
      await authenticateDiagnosticRecord(f, before, "needs_action", reason);
      delete (f.git as { discoverOwedCandidates?: unknown }).discoverOwedCandidates;
      f.state.finalError = undefined;
      f.state.sourceRefused = true;
      f.state.diagnostic = { check: "worktree_status_dirty" };
      const restarted = f.make(true);
      await restarted.resumeLive({ isExecuting: () => false, authenticatedAtMs: f.state.now });
      const after = (await restarted.inspect("run-1"))[0]!;
      const eligible = diagnosticRetryReasons.includes(reason);
      assert.equal(after.reason, eligible ? "inventory_source_not_quiescent:worktree_status_dirty" : reason);
      assert.equal(after.finalAcknowledged, undefined);
      assert.deepEqual(after.finalRequest, before.finalRequest);
      assert.equal(f.logs.length, eligible ? 1 : 0);
      assert.equal(f.state.open, true);
      assert.equal(f.state.uploads, 1);
    } finally { await f.close(); }
  });
}

for (const reason of unknownDiagnosticReasons) {
  it(`#2507 uploaded unknown suffix still eligible without transient backoff ${reason}`, async () => {
    const f = await fixture();
    try {
      const record = await f.freeze();
      assert.ok(record);
      f.state.expires = "invalid";
      await f.capture(record);
      const before = (await f.coordinator.inspect("run-1"))[0]!;
      await authenticateDiagnosticRecord(f, before, "uploaded", reason);
      delete (f.git as { discoverOwedCandidates?: unknown }).discoverOwedCandidates;
      const restarted = f.make();
      for (let i = 0; i < 2; i++) {
        await restarted.resumeLive({ isExecuting: () => false, authenticatedAtMs: f.state.now });
        f.state.now += 30_000;
      }
      f.state.expires = "2099-01-01T00:00:00Z";
      await restarted.resumeLive({ isExecuting: () => false, authenticatedAtMs: f.state.now });
      const after = (await restarted.inspect("run-1"))[0]!;
      assert.equal(after.finalAcknowledged, true, "unknown reason does not grow transient backoff");
      assert.equal(after.reason, reason);
      assert.equal(after.captureId, before.captureId);
      assert.equal(f.state.uploads, 1);
    } finally { await f.close(); }
  });
}

for (const pendingFinal of [false, true]) for (const cause of [undefined, ...sourceDiagnosticCauses]) {
  it(`source refusal repeats backoff without replacing uploaded identity pendingFINAL=${pendingFinal} cause=${cause}`, async () => {
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
      if (cause) f.state.diagnostic = { check: "inventory_read_not_verified", cause };
      const before = (await f.coordinator.inspect("run-1"))[0]!;
      const identity = (r: RecoveryRecord) => [r.captureId, r.serverCaptureId, r.sourceSha,
        r.coverageDigest, r.checksum, r.finalRequest];
      await f.coordinator.resumeLive({ isExecuting: () => false, authenticatedAtMs: f.state.now });
      const first = (await f.coordinator.inspect("run-1"))[0]!;
      assert.equal(first.reason, cause ? "inventory_source_not_quiescent:inventory_read_not_verified:" + cause : "inventory_source_not_quiescent");
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
    assert.equal(retained.reason, "inventory_source_not_quiescent:physical_sources_without_boundary");
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

for (const uploadFails of [false, true]) {
  it(`guarded thin retained bytes retry without FINAL or reproduction uploadFails=${uploadFails}`, async () => {
    const f = await fixture();
    try {
      f.state.thin = true;
      const record = await f.freeze(); assert.ok(record);
      if (uploadFails) f.state.reserveError = new Error("network unavailable");
      await f.capture(record);
      const before = (await f.coordinator.inspect("run-1"))[0]!;
      assert.equal(before.selfContained, false);
      assert.deepEqual(before.prerequisiteShas, [H]);
      const bytes = await fs.readFile(before.bundlePath!);
      const produced = f.state.produced;
      f.state.reserveError = undefined;
      await f.make().resumePending(undefined, [before]);
      const uploaded = (await f.coordinator.inspect("run-1"))[0]!;
      assert.equal(uploaded.state, "uploaded");
      assert.equal(uploaded.captureId, before.captureId);
      assert.equal(uploaded.checksum, before.checksum);
      assert.deepEqual(uploaded.prerequisiteShas, before.prerequisiteShas);
      const statusReads = f.state.statusReads;
      await f.capture(uploaded);
      await f.make().resumePending(undefined, [uploaded]);
      assert.equal(f.state.statusReads, statusReads, "thin FINAL never polls status");
      assert.equal(f.finals.length, 0);
      assert.equal(f.state.produced, produced);
      assert.deepEqual(await fs.readFile(uploaded.bundlePath!), bytes);
      assert.equal(await f.coordinator.inventoryCleanupState("run-1", 7), "pending");
      await f.coordinator.forgetGeneration("run-1", 7);
      assert.equal((await f.coordinator.inspect("run-1")).length, 1);
      assert.equal(f.state.open, true);
    } finally { await f.close(); }
  });
}
for (const corruption of ["missing", "source", "ref", "dependencies", "oversized-header", "metadata"] as const) {
  it(`guarded queued archive FINAL refuses retained evidence ${corruption}`, async () => {
    const f = await fixture();
    try {
      f.state.loseAck = true;
      const record = await f.freeze(); assert.ok(record);
      await f.capture(record);
      const queued = (await f.coordinator.inspect("run-1"))[0]!;
      assert.ok(queued.finalRequest); assert.equal(f.finals.length, 1);
      if (corruption === "missing") await fs.rm(queued.bundlePath!);
      else if (corruption === "metadata") {
        const inner = f.coordinator as unknown as { writeExistingRecord(r: RecoveryRecord, fn: (r: RecoveryRecord) => RecoveryRecord): Promise<RecoveryRecord> };
        await inner.writeExistingRecord(queued, cur => ({ ...cur, selfContained: undefined, prerequisiteShas: undefined }));
      } else {
        const header = corruption === "source" ? `${H} refs/heads/recovered-source`
          : corruption === "ref" ? `${queued.sourceSha} refs/heads/other`
          : corruption === "dependencies" ? `-${H} prerequisite\n${queued.sourceSha} refs/heads/recovered-source`
          : "x".repeat(20 * 1024);
        await fs.writeFile(queued.bundlePath!, `# v2 git bundle\n${header}\n\nPACK`);
      }
      f.state.loseAck = false;
      await f.make().resumePending(undefined, [queued]);
      assert.equal(f.finals.length, 1);
      assert.notEqual((await f.coordinator.inspect("run-1"))[0]!.finalAcknowledged, true);
      assert.equal(await f.coordinator.inventoryCleanupState("run-1", 7), "pending");
    } finally { await f.close(); }
  });
}

import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { createHash, createHmac, randomUUID } from "node:crypto";
import fs from "node:fs";
import fsp from "node:fs/promises";
import path from "node:path";
import { it, type TestContext } from "node:test";
import { acquireWorkerLifetime } from "../src/authority-lock.js";
import { AuthorityRuntime } from "../src/authority-runtime.js";
import { GitCache } from "../src/git.js";
import { canonicalJson, RecoveryCoordinator, type RecoveryArchiveClient, type RecoveryRecord } from "../src/recovery.js";
import { makeTmp, testLockOptions } from "./authority-test-support.js";
import { makeFixture } from "./fixture-repo.js";
import { nullLogger, testGitCacheOptions } from "./helpers.js";

const TOKEN = "selection-fixture-token";
const RUN = "run-selection";
const SHA = "a".repeat(40);
const key = createHmac("sha256", TOKEN).update("uzi-recovery-journal-v1").digest();

async function rig(t: TestContext, guarded = false) {
  const forbidden = async (): Promise<never> => { throw new Error("unexpected selection RPC"); };
  const root = makeTmp(t, "pin-selection-");
  const lifetime = await acquireWorkerLifetime(root, testLockOptions());
  const authority = new AuthorityRuntime({ lifetime, workerToken: TOKEN, workerIncarnation: randomUUID() });
  const git = new GitCache(root, nullLogger(), undefined, testGitCacheOptions());
  const coordinator = new RecoveryCoordinator({
    recoveryRoot: git.recoveryRoot, workerToken: TOKEN, git, authority,
    client: {
      reserveRecoveryCapture: forbidden, getRecoveryCaptureStatus: forbidden,
      uploadRecoveryBundle: forbidden, releaseRecoveryCustody: forbidden,
      hasFeature: (feature: string) => feature === "recovery_inventory_v1",
      listRecoveryHolds: async () => ({ run_id: RUN, holds: [{
        hold_id: "selection-hold", generation: 1, inventory_guarded: true, has_available_capture: false,
      }] }),
    }, log: nullLogger(),
  });
  const initial = await coordinator.pin({ runId: RUN, generation: 1, sourceSha: SHA, branch: "agent/test", kind: "issue", inventoryGuarded: guarded || undefined });
  assert.ok(initial);
  const file = path.join(git.recoveryRoot, RUN, initial.captureId + ".json");
  const write = (record: RecoveryRecord) => {
    const mac = createHmac("sha256", key).update(canonicalJson(record)).digest("hex");
    fs.writeFileSync(path.join(path.dirname(file), record.captureId + ".json"), JSON.stringify({ ...record, mac }));
  };
  const input = () => ({
    runId: RUN, predecessorGeneration: 1, branch: "agent/test", bareDir: "repo.git", kind: "issue" as const,
    signal: new AbortController().signal, deadline: Date.now() + 10_000,
  });
  const refuse = async () => {
    let calls = 0;
    const result = await coordinator.withPinRestoreRecord(input(), async () => { calls++; });
    assert.equal(result.kind, "refused");
    assert.equal(calls, 0);
  };
  return { root, git, authority, coordinator, initial, file, write, input, refuse };
}

it("selects current ordinary nonfinal pinned contents, freezes them, and expires scoped revalidation", async t => {
  const r = await rig(t);
  let escaped!: () => Promise<boolean>;
  const before = fs.readFileSync(r.file);
  const selected = await r.coordinator.withPinRestoreRecord(r.input(), async s => {
    escaped = s.revalidate;
    assert.equal(s.record.sourceSha, SHA);
    assert.equal(s.record.bareDir, undefined);
    assert.equal(s.record.bundlePath, undefined);
    assert.ok(Object.isFrozen(s.record));
    assert.equal(s.digest, createHash("sha256").update(canonicalJson(r.initial)).digest("hex"));
    assert.equal(await s.revalidate(), true);
    return "saved";
  });
  assert.deepEqual(selected, { kind: "selected", value: "saved" });
  assert.equal(await escaped(), false);
  assert.deepEqual(fs.readFileSync(r.file), before);
  const newer = { ...r.initial, sourceSha: "b".repeat(40), createdAt: 1 };
  r.write(newer);
  assert.deepEqual(await r.coordinator.withPinRestoreRecord(r.input(), async s => s.record.sourceSha),
    { kind: "selected", value: newer.sourceSha });
});

it("selects production guarded ordinary pins before and after finalization without inventing a capture", async t => {
  const r = await rig(t, true);
  for (const finalizationPin of [false, true]) {
    const record = await r.coordinator.pin({ runId: RUN, generation: 1, sourceSha: SHA, branch: "agent/test",
      kind: "issue", inventoryGuarded: true, finalizationPin, bareDir: "repo.git", defaultBranch: "main" });
    assert.ok(record);
    const before = fs.readFileSync(r.file);
    assert.deepEqual(await r.coordinator.withPinRestoreRecord(r.input(), async s => {
      assert.equal(s.record.captureId, r.initial.captureId);
      assert.equal(s.record.inventoryGuarded, true);
      assert.equal(s.record.finalizationPin === true, finalizationPin);
      assert.equal(s.record.coverageDigest, undefined);
      assert.equal(s.record.bundlePath, undefined);
      assert.equal(await s.revalidate(), true);
      return s.record.sourceSha;
    }), { kind: "selected", value: SHA });
    assert.deepEqual(fs.readFileSync(r.file), before);
  }
});

it("selects the same authenticated ordinary pin after production resumePending marks attention", async t => {
  const r = await rig(t);
  await r.coordinator.resumePending(undefined, [r.initial]);
  const current = (await r.coordinator.inspect(RUN))[0]!;
  assert.equal(current.state, "needs_action");
  assert.equal(current.reason, "early_pin_only_after_restart");
  assert.equal(current.captureId, r.initial.captureId);
  assert.equal(current.sourceSha, SHA);
  const before = fs.readFileSync(r.file);
  assert.deepEqual(await r.coordinator.withPinRestoreRecord(r.input(), async s => {
    assert.equal(s.record.state, "needs_action");
    assert.equal(s.record.bundlePath, undefined);
    assert.equal(s.digest, createHash("sha256").update(canonicalJson(current)).digest("hex"));
    assert.equal(await s.revalidate(), true);
    return s.record.sourceSha;
  }), { kind: "selected", value: SHA });
  assert.deepEqual(fs.readFileSync(r.file), before);
});

it("allows authenticated historical producer custody beside the unique exact ordinary generation", async t => {
  const r = await rig(t);
  const older: RecoveryRecord = { ...r.initial, captureId: "maintenance-v1-" + randomUUID(),
    generation: 1, inventoryGuarded: true, state: "needs_action", reason: "source_not_verifiable_after_restart",
    exactHoldBinding: { holdId: "00000000-0000-4000-8000-000000000001", repoId: "00000000-0000-4000-8000-000000000002" },
    holdSettlement: { run_id: RUN, generation: 1, hold_id: "00000000-0000-4000-8000-000000000001", repo_id: "00000000-0000-4000-8000-000000000002",
      inventory_guarded: true, state: "released", original_worker_id: "fixture-worker",
      original_worker_identity: "fixture-worker", run_status: "failed", current_claim_generation: 2, claim_released: true } };
  r.write(older);
  r.write({ ...r.initial, generation: 2 });
  const before = fs.readFileSync(path.join(path.dirname(r.file), older.captureId + ".json"));
  const select = () => r.coordinator.withPinRestoreRecord({ ...r.input(), predecessorGeneration: 2 }, async s => s.record.captureId);
  assert.deepEqual(await select(), { kind: "selected", value: r.initial.captureId });
  assert.deepEqual(fs.readFileSync(path.join(path.dirname(r.file), older.captureId + ".json")), before);
  for (const generation of [2, 3]) {
    r.write({ ...older, generation, holdSettlement: undefined, exactHoldBinding: undefined });
    assert.equal((await select()).kind, "refused");
  }
});

it("accepts the genuine current finalization pin and refuses its bare mismatch without fallback", async t => {
  const r = await rig(t);
  r.write({ ...r.initial, finalizationPin: true, bareDir: "repo.git", defaultBranch: "main" });
  assert.equal((await r.coordinator.withPinRestoreRecord(r.input(), async s => s.record.finalizationPin)).kind, "selected");
  r.write({ ...r.initial, finalizationPin: true, bareDir: "other.git", defaultBranch: "main" });
  await r.refuse();
});

it("refuses every incomplete or competing physical record set before action", async t => {
  const r = await rig(t);
  for (const change of [
    { runId: "other-run" }, { generation: 2 }, { branch: "agent/other" }, { kind: "prompt" as const },
    { recoveryPinBareDir: "other.git" }, { inventoryGuarded: true, finalizationPin: true, coverageDigest: "c".repeat(64),
      originalSourceSha: SHA, inventoryCurrentSha: SHA, originalRoots: [], bareDir: "repo.git",
      coverageContext: { runId: RUN, generation: 1, branch: "agent/test", kind: "issue" as const,
        barePath: path.join(r.root, "repos", "repo.git"), defaultIdentity: { ref: "refs/remotes/origin/main", sha: SHA } } },
    { coverageDigest: "c".repeat(64), finalizationPin: true }, { heldCreateUncertain: true },
    { finalAcknowledged: true },
    { finalRequest: { disposition: { kind: "settled" as const, coverage_digest: "c".repeat(64) } } },
  ]) {
    r.write({ ...r.initial, ...change });
    if ("coverageContext" in change) assert.equal((await r.coordinator.inspect(RUN)).length, 1,
      "the synthetic negative must be an authentic readable aggregate, not malformed metadata");
    await r.refuse();
  }
  r.write(r.initial);
  const authenticated = fs.readFileSync(r.file, "utf8");
  for (const raw of [
    authenticated.replace('"version":1', '"version":1,"futureSchema":true'),
    authenticated.replace('"version":1', '"version":1,"version":1'),
    authenticated.replace('"version":1', '"version":2'),
  ]) {
    fs.writeFileSync(r.file, raw);
    await r.refuse();
  }
  r.write(r.initial);
  fs.writeFileSync(r.file, fs.readFileSync(r.file, "utf8").replace(SHA, "b".repeat(40)));
  await r.refuse();
  r.write(r.initial);
  const other = { ...r.initial, captureId: randomUUID(), createdAt: Date.now() + 9999 };
  r.write(other);
  await r.refuse();
  fs.unlinkSync(path.join(path.dirname(r.file), other.captureId + ".json"));
  for (const name of ["unknown", r.initial.captureId + ".json." + randomUUID() + ".tmp"]) {
    const p = path.join(path.dirname(r.file), name);
    fs.writeFileSync(p, "");
    await r.refuse();
    fs.unlinkSync(p);
  }
});

it("refuses selected capture/digest drift and authenticated changes during action", async t => {
  const r = await rig(t);
  for (const input of [{ expectedCaptureId: randomUUID() }, { expectedDigest: "0".repeat(64) }]) {
    const result = await r.coordinator.withPinRestoreRecord({ ...r.input(), ...input }, async () => assert.fail("action"));
    assert.equal(result.kind, "refused");
  }
  const result = await r.coordinator.withPinRestoreRecord(r.input(), async s => {
    r.write({ ...r.initial, sourceSha: "b".repeat(40) });
    assert.equal(await s.revalidate(), false);
  });
  assert.equal(result.kind, "refused");
});

it("verifies advertised bundle bytes, header, and prerequisite facts without fabricating a pinned bundle", async t => {
  const r = await rig(t);
  const bundlePath = path.join(path.dirname(r.file), r.initial.captureId + ".bundle");
  const fx = makeFixture();
  t.after(() => fx.cleanup());
  const git = (args: string[]) => execFileSync("git", ["-C", fx.originPath, ...args], { encoding: "utf8",
    env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" } }).trim();
  const sourceSha = git(["rev-parse", "HEAD"]);
  git(["update-ref", "refs/heads/recovered-source", sourceSha]);
  git(["bundle", "create", bundlePath, "refs/heads/recovered-source"]);
  const bundle = fs.readFileSync(bundlePath);
  const bundled: RecoveryRecord = { ...r.initial, sourceSha, state: "bundled", bundlePath, byteSize: bundle.length,
    checksum: createHash("sha256").update(bundle).digest("hex"), chunkCount: 1, prerequisiteShas: [], selfContained: true };
  fs.writeFileSync(bundlePath, bundle);
  r.write(bundled);
  assert.equal((await r.coordinator.withPinRestoreRecord(r.input(), async s => s.record.state)).kind, "selected");
  fs.appendFileSync(bundlePath, "corruption");
  await r.refuse();
  fs.writeFileSync(bundlePath, bundle);
  r.write({ ...bundled, prerequisiteShas: ["b".repeat(40)], selfContained: false });
  await r.refuse();
  const badHeader = Buffer.from(bundle);
  badHeader.write("incorrect-source", badHeader.indexOf("recovered-source"), "ascii");
  fs.writeFileSync(bundlePath, badHeader);
  r.write({ ...bundled, byteSize: badHeader.length, checksum: createHash("sha256").update(badHeader).digest("hex") });
  await r.refuse();
});

it("caps every raw inventory visit before filtering", async t => {
  const r = await rig(t);
  const dir = path.dirname(r.file);
  for (let i = 0; i < 8192; i++) fs.writeFileSync(path.join(dir, "untrusted-" + i), "");
  const opendir = fsp.opendir;
  let visits = 0;
  t.mock.method(fsp, "opendir", async (...args: Parameters<typeof fsp.opendir>) => {
    const h = await Reflect.apply(opendir, fsp, args);
    if (args[0] === dir) {
      const read = h.read.bind(h);
      t.mock.method(h, "read", async () => { visits++; return read(); });
    }
    return h;
  });
  await r.refuse();
  assert.equal(visits, 4096);
});

it("shares the item cap across all scoped revalidations without resetting it", async t => {
  const r = await rig(t);
  let validations = 0, exhausted = false;
  const result = await r.coordinator.withPinRestoreRecord({ ...r.input(), deadline: Date.now() + 30_000 }, async s => {
    for (let i = 0; i < 4096; i++) {
      if (!await s.revalidate()) break;
      validations++;
    }
    exhausted = !await s.revalidate();
  });
  assert.ok(validations > 0);
  assert.ok(validations < 2048, "each listing charges the record and EOF");
  assert.equal(exhausted, true, "exhaustion stays latched");
  assert.equal(result.kind, "refused");
});

async function bundledRig(t: TestContext) {
  const r = await rig(t);
  const fx = makeFixture();
  t.after(() => fx.cleanup());
  const bundlePath = path.join(path.dirname(r.file), r.initial.captureId + ".bundle");
  const git = (args: string[]) => execFileSync("git", ["-C", fx.originPath, ...args], { encoding: "utf8",
    env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" } }).trim();
  const sourceSha = git(["rev-parse", "HEAD"]);
  git(["update-ref", "refs/heads/recovered-source", sourceSha]);
  git(["bundle", "create", bundlePath, "refs/heads/recovered-source"]);
  const body = fs.readFileSync(bundlePath);
  r.write({ ...r.initial, sourceSha, state: "bundled", bundlePath, byteSize: body.length,
    checksum: createHash("sha256").update(body).digest("hex"), chunkCount: 1, prerequisiteShas: [], selfContained: true });
  return r;
}

// Observe both legacy whole-file IO and bounded handle IO through the public selector.
function observeRecordReads(t: TestContext, file: string, beforeOpen?: (visit: number) => void) {
  let bytes = 0, opens = 0, wholeReads = 0;
  const open = fsp.open, readFile = fsp.readFile;
  t.mock.method(fsp, "readFile", async (...args: Parameters<typeof fsp.readFile>) => {
    if (args[0] === file) { wholeReads++; beforeOpen?.(++opens); }
    const value = await Reflect.apply(readFile, fsp, args);
    if (args[0] === file) bytes += Buffer.byteLength(value);
    return value;
  });
  t.mock.method(fsp, "open", async (...args: Parameters<typeof fsp.open>) => {
    if (args[0] === file) beforeOpen?.(++opens);
    const h = await Reflect.apply(open, fsp, args);
    if (args[0] === file) {
      const read = h.read.bind(h);
      t.mock.method(h, "read", async (...readArgs: Parameters<typeof h.read>) => {
        const result = await Reflect.apply(read, h, readArgs);
        bytes += result.bytesRead;
        return result;
      });
    }
    return h;
  });
  return () => ({ bytes, opens, wholeReads });
}

it("caps cumulative metadata bytes including nested bundle reopens and revalidation IO", async t => {
  const r = await bundledRig(t);
  const raw = fs.readFileSync(r.file, "utf8");
  fs.writeFileSync(r.file, " ".repeat(900 * 1024) + raw);
  const reads = observeRecordReads(t, r.file);
  let validations = 0, exhausted = false;
  const result = await r.coordinator.withPinRestoreRecord({ ...r.input(), deadline: Date.now() + 30_000 }, async s => {
    for (let i = 0; i < 40; i++) {
      if (!await s.revalidate()) break;
      validations++;
    }
    exhausted = !await s.revalidate();
  });
  assert.ok(validations > 0 && validations < 20, "nested reads share the same byte allowance");
  assert.equal(exhausted, true);
  assert.equal(result.kind, "refused");
  assert.equal(reads().wholeReads, 0);
  assert.ok(reads().bytes > 30 * 1024 ** 2);
  assert.ok(reads().bytes <= 32 * 1024 ** 2);
});

for (const swap of ["oversize", "symlink", "unknown-field"] as const) {
  it("strictly bounds the nested selected-record reopen after " + swap + " swap", async t => {
    const r = await bundledRig(t);
    const raw = fs.readFileSync(r.file, "utf8");
    const alternate = path.join(r.root, "alternate.json");
    fs.writeFileSync(alternate, raw);
    const reads = observeRecordReads(t, r.file, visit => {
      if (visit !== 2) return;
      if (swap === "oversize") fs.writeFileSync(r.file, " ".repeat(2 * 1024 ** 2) + raw);
      if (swap === "unknown-field") fs.writeFileSync(r.file, raw.replace('"version":1', '"version":1,"futureSchema":true'));
      if (swap === "symlink") { fs.unlinkSync(r.file); fs.symlinkSync(alternate, r.file); }
    });
    await r.refuse();
    assert.equal(reads().wholeReads, 0);
    assert.ok(reads().bytes <= 1024 * 1024, "no oversize record bytes are consumed");
  });
}

it("bounds queued authority and journal waits and rejects unsupported authority", async t => {
  const r = await rig(t);
  const aborted = new AbortController();
  aborted.abort();
  assert.equal((await r.coordinator.withPinRestoreRecord({ ...r.input(), signal: aborted.signal }, async () => assert.fail())).kind, "refused");
  const legacy = new RecoveryCoordinator({ recoveryRoot: r.git.recoveryRoot, workerToken: TOKEN, git: r.git,
    client: {} as RecoveryArchiveClient, log: nullLogger() });
  assert.deepEqual(await legacy.withPinRestoreRecord(r.input(), async () => assert.fail()), { kind: "refused", reason: "unsupported" });
  let release!: () => void;
  let entered!: () => void;
  const ready = new Promise<void>(resolve => { entered = resolve; });
  const pending = r.coordinator.withPinRestoreRecord(r.input(), async () => {
    entered();
    await new Promise<void>(resolve => { release = resolve; });
  });
  await ready;
  let pinFinished = false;
  const pin = r.coordinator.pin({ runId: RUN, generation: 1, sourceSha: SHA, branch: "agent/test", kind: "issue" })
    .then(() => { pinFinished = true; });
  try {
    const result = await r.coordinator.withPinRestoreRecord({ ...r.input(), deadline: Date.now() + 40 }, async () => assert.fail());
    assert.equal(result.kind, "refused");
    assert.equal(pinFinished, false, "ordinary pin waits on J through the action");
  } finally { release(); }
  assert.equal((await pending).kind, "selected");
  await pin;
});

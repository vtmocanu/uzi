import assert from "node:assert/strict";
import { execFileSync, spawn } from "node:child_process";
import { createHash, createHmac, randomUUID } from "node:crypto";
import fs from "node:fs";
import fsp from "node:fs/promises";
import path from "node:path";
import { it, type TestContext } from "node:test";
import { acquireWorkerLifetime } from "../src/authority-lock.js";
import { AuthorityRuntime } from "../src/authority-runtime.js";
import { testLockOptions } from "./authority-test-support.js";
import { GitCache } from "../src/git.js";
import type { PinRestoreLink, RetainedPinEnvelope } from "../src/pin-restore-journal.js";
import { canonicalJson, RecoveryCoordinator, type RecoveryArchiveClient } from "../src/recovery.js";
import { attemptClonePath } from "../src/attempt-path.js";
import { makeFixture } from "./fixture-repo.js";
import { nullLogger, testGitCacheOptions } from "./helpers.js";

const RUN = "run-pin-protection", BRANCH = "agent/issue-2512", KEY = "issue-2512";
const OLD = "0123abcd-0123-4abc-8def-0123456789ab", NEXT = "1123abcd-0123-4abc-8def-0123456789ab";
const ATTEMPT = "20261008T171401Z-g2-0123456789abcdef";
const hash = (raw: string) => createHash("sha256").update(raw).digest("hex");
function git(dir: string, args: string[]) {
  return execFileSync("git", ["-C", dir, ...args], { encoding: "utf8",
    env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" }, stdio: "pipe" });
}
async function rig(t: TestContext) {
  const fx = makeFixture();
  t.after(() => fx.cleanup());
  const cache = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
  const bare = await cache.ensureClone(fx.originPath);
  fs.chmodSync(path.dirname(bare), 0o700);
  fs.chmodSync(bare, 0o700);
  const original = cache.runnerClonePath(bare, KEY);
  const tip = git(bare, ["rev-parse", "HEAD"]).trim();
  const tree = git(bare, ["rev-parse", "HEAD^{tree}"]).trim();
  const raw = " \n" + JSON.stringify({ runId: RUN, clonePath: original, sourceGeneration: 1 }) + "\t";
  const link: PinRestoreLink = {
    version: 1, intentId: "intent-protection", predecessorJournalDigest: hash(raw), predecessorGeneration: 1, predecessorEpoch: OLD,
    successor: { runId: RUN, claimGeneration: 2, workerId: "fixture-worker", registerNonce: "fixture-register",
      authorityIncarnation: NEXT, executorIncarnation: null, sourceEpoch: NEXT, attemptId: ATTEMPT,
      clonePath: attemptClonePath(original, ATTEMPT) },
    restore: { captureId: "fixture-capture", authenticatedRecordDigest: "a".repeat(64), predecessorGeneration: 1,
      bareDir: path.basename(bare), pinRef: "refs/uzi-recovery-pin/" + RUN + "/1", tip, contentSource: tip,
      expectedHead: tip, expectedTree: tree },
  };
  const envelope: RetainedPinEnvelope = { version: 2, runId: RUN, branch: BRANCH, key: KEY,
    predecessors: [{ rawJournal: raw, journalDigest: hash(raw), generation: 1, sourceEpoch: OLD }],
    intent: { version: 1, phase: "prepared", link } };
  const config = (key: string, value: string) => git(bare, ["config", "--local", key, value]);
  const retainedKey = "uzi-retained." + RUN + ".journal", activeKey = "uzi-recovery." + BRANCH + ".clone";
  const retain = () => config(retainedKey, JSON.stringify(envelope));
  config(activeKey, raw);
  git(bare, ["update-ref", link.restore.pinRef, tip]);
  const read = () => cache.readPinRestoreProtection(RUN, { signal: new AbortController().signal, deadline: Date.now() + 10_000 });
  const activate = () => {
    git(bare, ["config", "--local", "--add", "uzi-attempts." + BRANCH + ".entry", JSON.stringify({
      runId: RUN, clonePath: link.successor.clonePath, attemptId: link.successor.attemptId, state: "live",
    })]);
    config(activeKey, JSON.stringify({ runId: RUN, clonePath: link.successor.clonePath, attemptId: link.successor.attemptId, sourceGeneration: link.successor.claimGeneration, pinRestore: link }));
  };
  return { fx, cache, bare, original, tip, envelope, link, config, retain, read, activate, activeKey };
}

it("settlement writes retain protected and unknown refs while legacy unprotected writes work", async t => {
  const r = await rig(t);
  const ref = "refs/uzi-settle/" + RUN + "/hold/source";
  git(r.bare, ["update-ref", ref, r.tip]);
  const next = git(r.bare, ["-c", "user.name=Fixture", "-c", "user.email=fixture@example.com",
    "commit-tree", r.tip + "^{tree}", "-p", r.tip, "-m", "settlement candidate"]).trim();
  const refs = () => git(r.bare, ["for-each-ref", "--format=%(refname) %(objectname)"]);
  const internal = r.cache as unknown as { runGit: (dir: string, args: string[]) => Promise<string> };
  const runGit = internal.runGit.bind(r.cache);
  let commitProbes = 0;
  t.mock.method(internal, "runGit", async (dir: string, args: string[]) => {
    if (args[0] === "rev-parse" && args.includes("--verify")) commitProbes++;
    return runGit(dir, args);
  });
  for (const metadata of ["protected", "unknown", "version-downgrade"]) {
    if (metadata === "protected") r.retain();
    else r.config("uzi-retained." + RUN + ".journal", metadata === "unknown" ? "{" :
      JSON.stringify(r.envelope).replace('"version":2,', '"version":2,"version":1,'));
    const before = refs();
    if (metadata === "version-downgrade") assert.equal((await r.read()).kind, "unknown");
    await r.cache.deleteSettlementPin(r.bare, RUN, "hold", "source");
    assert.equal(refs(), before);
    assert.equal(await r.cache.pinSettlementRefs(r.bare, RUN, "hold", { source: next, adopted: next }), false);
    assert.equal(refs(), before);
    assert.equal(commitProbes, 0, "protection must precede ref verification");
  }
  git(r.bare, ["config", "--local", "--unset-all", "uzi-retained." + RUN + ".journal"]);
  const foreign = "uzi-retained.run-foreign.journal";
  for (const raw of [
    '{"version":2,"version":2,"intent":',
    '{"version":2,"version":2,',
    '{"version":2',
    '{"\\u0076ersion" \t:\r\n2e0,"version":2,',
    '{"version":3,"version":3,',
  ]) {
    r.config(foreign, raw);
    const before = refs();
    await r.cache.deleteSettlementPin(r.bare, RUN, "hold", "source");
    assert.equal(refs(), before);
    assert.equal(await r.cache.pinSettlementRefs(r.bare, RUN, "hold", { source: next }), false);
    assert.equal(refs(), before);
    assert.equal(commitProbes, 0, "foreign version evidence must precede ref verification");
  }
  git(r.bare, ["config", "--local", "--unset-all", foreign]);
  assert.equal(await r.cache.pinSettlementRefs(r.bare, RUN, "hold", { source: next }), true);
  assert.equal(git(r.bare, ["rev-parse", ref]).trim(), next);
});

// Model ordinary recovery retaining a canonical source of unknown generation under G2.
// No generation is assigned to that canonical source.
function historicalFamily(r: Awaited<ReturnType<typeof rig>>, options: {
  embedded?: number | string; claim?: number; adopting?: boolean;
} = {}) {
  const attempt = (generation: number | string) => ATTEMPT.replace("-g2-", `-g${generation}-`);
  const source = { runId: RUN, clonePath: r.original };
  const primaryId = attempt(2);
  const primary = { runId: RUN, clonePath: attemptClonePath(r.original, primaryId), attemptId: primaryId };
  const embeddedId = options.embedded === undefined ? undefined : attempt(options.embedded);
  const embedded = embeddedId === undefined ? undefined :
    { runId: RUN, clonePath: attemptClonePath(r.original, embeddedId), attemptId: embeddedId };
  const successorId = attempt(3);
  const successor = { runId: RUN, clonePath: attemptClonePath(r.original, successorId), attemptId: successorId, restoreTip: r.tip };
  for (const member of [primary, ...(embedded ? [embedded] : []), ...(options.adopting ? [successor] : [])])
    git(r.bare, ["config", "--local", "--add", "uzi-attempts." + BRANCH + ".entry", JSON.stringify({ ...member, restoreTip: undefined, state: "live" })]);
  const raw = " \n" + JSON.stringify({ ...primary, sourceGeneration: 2,
    ...(options.adopting ? { restoreTip: r.tip } : {}),
    retainedSources: [source, primary, ...(embedded ? [embedded] : [])],
    recovery: { version: 1, source, attempts: 1, startedAt: 0, deadline: 300000, backoffMs: 0,
      stage: options.adopting ? "adopting" : "capturing", claimGeneration: options.claim ?? 2,
      ...(options.adopting ? { successor, restoreTip: r.tip } : {}) },
  }) + "\t";
  r.envelope.predecessors = [{ rawJournal: raw, journalDigest: hash(raw), generation: 2, sourceEpoch: OLD }];
  Object.assign(r.link, { predecessorGeneration: 2, predecessorJournalDigest: hash(raw) });
  Object.assign(r.link.restore, { predecessorGeneration: 2, pinRef: "refs/uzi-recovery-pin/" + RUN + "/2" });
  Object.assign(r.link.successor, { claimGeneration: 4, attemptId: attempt(4), clonePath: attemptClonePath(r.original, attempt(4)) });
  r.config(r.activeKey, raw);
  r.retain();
}

it("protects originals through prepared, ready, and absent relations without any predecessor path probes", async t => {
  const r = await rig(t);
  const probes: string[] = [];
  const protectedPath = (p: unknown) => typeof p === "string" &&
    (p === r.original || p.startsWith(r.original + path.sep) || p.startsWith(r.original + ".attempt-"));
  const lstat = fsp.lstat;
  t.mock.method(fsp, "lstat", async (...args: Parameters<typeof fsp.lstat>) => {
    if (protectedPath(args[0])) { probes.push(String(args[0])); throw new Error("old source probe"); }
    return Reflect.apply(lstat, fsp, args);
  });
  const open = fsp.open;
  t.mock.method(fsp, "open", async (...args: Parameters<typeof fsp.open>) => {
    if (protectedPath(args[0])) { probes.push(String(args[0])); throw new Error("old source probe"); }
    return Reflect.apply(open, fsp, args);
  });
  r.retain();
  for (const relation of ["prepared-old", "prepared-new", "ready-new", "absent"]) {
    if (relation === "prepared-new") r.activate();
    if (relation === "ready-new") { r.envelope.intent.phase = "ready"; r.retain(); }
    if (relation === "absent") git(r.bare, ["config", "--local", "--unset-all", r.activeKey]);
    const before = fs.readFileSync(path.join(r.bare, "config"));
    const result = await r.read();
    assert.equal(result.kind, "complete", relation);
    if (result.kind === "complete") assert.deepEqual([...result.originalGenerations], [1]);
    assert.deepEqual(fs.readFileSync(path.join(r.bare, "config")), before);
  }
  assert.deepEqual(probes, []);
});

for (const state of ["pinned", "uploaded"] as const) {
it(`retains exact ${state} MAC journal, bundle, pins, and raw history through repeated boot/live/release/forget/pin callbacks`, async t => {
  const r = await rig(t);
  let rpcCalls = 0;
  const forbidden = async (): Promise<never> => { rpcCalls++; throw new Error("forbidden RPC"); };
  const client = { reserveRecoveryCapture: forbidden, uploadRecoveryBundle: forbidden,
    releaseRecoveryCustody: forbidden, reconcileRecoveryCapture: forbidden, listRecoveryHolds: forbidden,
    getRecoveryCaptureStatus: forbidden, publishHeld: forbidden } as RecoveryArchiveClient;
  let creating = true;
  client.hasFeature = feature => feature === "recovery_inventory_v1";
  client.listRecoveryHolds = async () => {
    if (!creating) return forbidden();
    return { run_id: RUN, holds: [{ hold_id: "fixture-hold", generation: 1,
      inventory_guarded: true, has_available_capture: false }] };
  };
  fs.chmodSync(r.fx.dataDir, 0o700);
  const lifetime = await acquireWorkerLifetime(r.fx.dataDir, testLockOptions());
  const authority = new AuthorityRuntime({ lifetime, workerToken: "protection-fixture-token", workerIncarnation: randomUUID() });
  const coordinator = new RecoveryCoordinator({ recoveryRoot: r.cache.recoveryRoot, workerToken: "protection-fixture-token", authority,
    git: r.cache, client, log: nullLogger() });
  const record = await coordinator.pin({ runId: RUN, generation: 1, sourceSha: r.tip, kind: "issue", branch: BRANCH,
    inventoryGuarded: true, finalizationPin: true, bareDir: path.basename(r.bare), defaultBranch: "main" });
  creating = false;
  assert.ok(record);
  const probes: string[] = [];
  const protectedPath = (p: unknown) => typeof p === "string" &&
    (p === r.original || p.startsWith(r.original + path.sep) || p.startsWith(r.original + ".attempt-"));
  const lstat = fsp.lstat, open = fsp.open;
  t.mock.method(fsp, "lstat", async (...args: Parameters<typeof fsp.lstat>) => {
    if (protectedPath(args[0])) { probes.push(String(args[0])); throw new Error("old source probe"); }
    return Reflect.apply(lstat, fsp, args);
  });
  t.mock.method(fsp, "open", async (...args: Parameters<typeof fsp.open>) => {
    if (protectedPath(args[0])) { probes.push(String(args[0])); throw new Error("old source probe"); }
    return Reflect.apply(open, fsp, args);
  });
  for (const method of ["stat", "readFile", "readdir", "opendir", "realpath", "rm", "unlink", "rename", "access"] as const) {
    const original = fsp[method];
    t.mock.method(fsp, method, (...args: unknown[]) => {
      if (args.some(protectedPath)) { probes.push(method); throw new Error("old source callback"); }
      return Reflect.apply(original, fsp, args);
    });
  }
  const dir = path.join(r.cache.recoveryRoot, RUN);
  const file = path.join(dir, record.captureId + ".json");
  const bundle = path.join(dir, record.captureId + ".bundle");
  git(r.bare, ["bundle", "create", bundle, "--all"]);
  if (state === "uploaded") {
    const body = fs.readFileSync(bundle);
    Object.assign(record, { state, bundlePath: bundle, byteSize: body.length, checksum: createHash("sha256").update(body).digest("hex"),
      chunkCount: 1, prerequisiteShas: [], selfContained: true, serverCaptureId: "fixture-server-capture",
      recoveryPinBareDir: path.basename(r.bare) });
    const key = createHmac("sha256", "protection-fixture-token").update("uzi-recovery-journal-v1").digest();
    const mac = createHmac("sha256", key).update(canonicalJson(record)).digest("hex");
    fs.writeFileSync(file, JSON.stringify({ ...record, mac }));
  }
  const before = fs.readFileSync(file), bundleBefore = fs.readFileSync(bundle);
  historicalFamily(r, { adopting: true, claim: 3 });
  const protection = await r.read();
  assert.equal(protection.kind, "complete");
  if (protection.kind === "complete") {
    assert.deepEqual([...protection.originalGenerations], [2]);
    assert.equal(protection.protectedThroughGeneration, 3);
  }
  const configBefore = fs.readFileSync(path.join(r.bare, "config"));
  const refsBefore = git(r.bare, ["for-each-ref"]);
  await r.cache.withBoundaryProcessSpawner(async request => {
    if (protectedPath(request.cwd) || request.argv.some(protectedPath)) {
      probes.push("git");
      throw new Error("old source Git callback");
    }
    const child = spawn(request.argv[0]!, request.argv.slice(1), {
      cwd: request.cwd, env: request.env, timeout: request.timeoutMs,
    });
    const completed = new Promise<{ code: number }>((resolve, reject) => {
      child.once("error", reject);
      child.once("close", code => resolve({ code: code ?? -1 }));
    });
    return { stdin: child.stdin, stdout: child.stdout, stderr: child.stderr, completed,
      cancel: async () => { child.kill("SIGKILL"); await completed; } };
  }, new AbortController().signal, async () => {
  for (let attempt = 0; attempt < 2; attempt++) {
    await coordinator.resumePending(undefined, [record]);
    await coordinator.resumeLive({ isExecuting: () => false, authenticatedAtMs: Date.now() + 999999 });
    await coordinator.release(RUN, 1);
    await coordinator.release(RUN);
    await coordinator.forgetGeneration(RUN, 1);
    assert.equal(await coordinator.pin({ runId: RUN, generation: 1, sourceSha: "f".repeat(40), kind: "issue", branch: BRANCH }), undefined);
    await r.cache.deleteRecoveryPin(r.bare, RUN, 0);
    await r.cache.deleteRecoveryPin(r.bare, RUN, 1);
    await r.cache.deleteRecoveryPin(r.bare, RUN, 1, r.tip);
    assert.deepEqual(await coordinator.captureAndUpload({ record, barePath: r.bare, defaultBranch: "main" }),
      { state: record.state, captureId: record.captureId });
    assert.equal(await coordinator.settleUnadoptedGuardedGeneration({ runId: RUN, generation: 1, barePath: r.bare }), "retained");
    assert.equal(await r.cache.anchorRecoveryHead(r.bare, RUN, 1, r.tip), false);
    await assert.rejects(r.cache.markRecoveryCapture(r.bare, r.original, BRANCH, RUN, undefined, undefined, 1), /original retained/);
    await assert.rejects(r.cache.releaseAttemptInPlace(r.bare, r.original, BRANCH, RUN, "abandoned"), /original retained/);
    await assert.rejects(r.cache.retireRunnerClone(r.bare, r.original, BRANCH, RUN, { discard: true }), /original retained/);
    assert.equal(await r.cache.cleanupRecoveryGeneration(r.bare, { runId: RUN, generation: 1, branch: BRANCH, barePath: r.bare,
      kind: "issue", defaultIdentity: { ref: "refs/remotes/origin/main", sha: r.tip } }, [], [], [r.tip], r.tip), "retained");
    assert.equal(await coordinator.releaseHeldPublication(record, randomUUID()), "kept");
    assert.deepEqual(await coordinator.operateFrozenInventory({ record, barePath: r.bare, defaultBranch: "main" }), { state: record.state, captureId: record.captureId });
    const cleanup = await r.cache.cleanupArchivedRecoveryPin({ runId: RUN, generation: 1, sourceSha: r.tip,
      recoveryPinBareDir: path.basename(r.bare), isEligible: () => true });
    assert.equal(cleanup.state, "retained");
  }
  });
  assert.equal(rpcCalls, 0);
  assert.deepEqual(probes, []);
  assert.deepEqual(fs.readFileSync(file), before);
  assert.deepEqual(fs.readFileSync(bundle), bundleBefore);
  assert.deepEqual(fs.readFileSync(path.join(r.bare, "config")), configBefore);
  assert.equal(git(r.bare, ["for-each-ref"]), refsBefore);
  const successor = await coordinator.pin({ runId: RUN, generation: 4, sourceSha: r.tip, kind: "issue", branch: BRANCH });
  assert.ok(successor, "an exact unprotected successor remains mutable");
  await coordinator.forgetGeneration(RUN, 4);
  assert.deepEqual(fs.readFileSync(file), before);
});

}

it("derives the family bound independently from embedded attempts and raw recovery claims", async t => {
  for (const [options, bound] of [
    [{}, 2], [{ embedded: 3 }, 3], [{ claim: 3 }, 3], [{ adopting: true, claim: 3 }, 3],
  ] as const) {
    const r = await rig(t);
    historicalFamily(r, options);
    const before = fs.readFileSync(path.join(r.bare, "config"));
    const metadata = await r.cache.readRetainedPinMetadata(r.bare);
    assert.equal(metadata[0]?.protectedThroughGeneration, bound);
    assert.equal(metadata[0]?.originals[0]?.journal.recovery?.source.attemptId, undefined);
    const result = await r.read();
    assert.equal(result.kind, "complete");
    if (result.kind === "complete") {
      assert.deepEqual([...result.originalGenerations], [2]);
      assert.equal(result.protectedThroughGeneration, bound);
    }
    for (const generation of [0, 1, 2, bound]) {
      assert.equal(await r.cache.anchorRecoveryHead(r.bare, RUN, generation, r.tip), false);
      await r.cache.deleteRecoveryPin(r.bare, RUN, generation);
    }
    assert.deepEqual(fs.readFileSync(path.join(r.bare, "config")), before);
    assert.equal(await r.cache.anchorRecoveryHead(r.bare, RUN, bound + 1, r.tip), true);
  }
});

it("lists shape-valid active metadata at the family bound without proving fresh execution admission", async t => {
  const r = await rig(t);
  historicalFamily(r, { claim: 4 });
  r.activate();
  const [metadata] = await r.cache.readRetainedPinMetadata(r.bare);
  assert.ok(metadata);
  assert.equal(metadata.relation, "prepared-new");
  assert.equal(metadata.protectedThroughGeneration, 4);
  // Necessary future-writer precondition only: this metadata read grants no execution.
  assert.equal(metadata.envelope.intent.link.successor.claimGeneration > metadata.protectedThroughGeneration, false);
  assert.equal(await r.cache.anchorRecoveryHead(r.bare, RUN, 4, r.tip), false);
  assert.equal(await r.cache.anchorRecoveryHead(r.bare, RUN, 5, r.tip), true);
});

it("unknown or unsafe embedded attempt generations never infer current minus one", async t => {
  for (const embedded of ["x", "9007199254740992"]) {
    const r = await rig(t);
    historicalFamily(r, { embedded });
    const before = fs.readFileSync(path.join(r.bare, "config"));
    assert.equal((await r.read()).kind, "unknown");
    await assert.rejects(r.cache.readRetainedPinMetadata(r.bare));
    assert.equal(await r.cache.anchorRecoveryHead(r.bare, RUN, 5, r.tip), false);
    assert.deepEqual(fs.readFileSync(path.join(r.bare, "config")), before);
  }
});

it("distinguishes an initially absent deny inventory from a positive root lost before opendir", async t => {
  const r = await rig(t);
  const opendir = fsp.opendir;
  let injected = false;
  t.mock.method(fsp, "opendir", async (...args: Parameters<typeof fsp.opendir>) => {
    if (args[0] === path.dirname(r.bare)) {
      injected = true;
      throw Object.assign(new Error("root disappeared after lstat"), { code: "ENOENT" });
    }
    return Reflect.apply(opendir, fsp, args);
  });
  assert.equal((await r.read()).kind, "unknown");
  assert.equal(injected, true);
  const fresh = new GitCache(path.join(r.fx.dataDir, "fresh"), nullLogger(), undefined, testGitCacheOptions());
  const result = await fresh.readPinRestoreProtection(RUN, {
    signal: new AbortController().signal, deadline: Date.now() + 10_000,
  });
  assert.equal(result.kind, "complete");
  if (result.kind === "complete") assert.equal(result.originalGenerations.size, 0);
});

it("unknown, cancelled, excessive, or symlinked bare inventories never prove an empty protection set", async t => {
  const r = await rig(t);
  r.retain();
  r.config("uzi-retained." + RUN + ".journal", "{broken");
  assert.equal((await r.read()).kind, "unknown");
  r.retain();
  const controller = new AbortController();
  controller.abort();
  assert.equal((await r.cache.readPinRestoreProtection(RUN, { signal: controller.signal, deadline: Date.now() + 1000 })).kind, "unknown");
  assert.equal((await r.cache.readPinRestoreProtection(RUN, { signal: new AbortController().signal, deadline: Infinity })).kind, "unknown");
  const link = path.join(path.dirname(r.bare), "unreadable.git");
  fs.symlinkSync(r.bare, link);
  assert.equal((await r.read()).kind, "unknown");
  fs.unlinkSync(link);
  for (let i = 0; i < 4097; i++) fs.writeFileSync(path.join(path.dirname(r.bare), "entry-" + i), "");
  assert.equal((await r.read()).kind, "unknown");
});

it("applies one metadata budget across the complete bare inventory", async t => {
  const r = await rig(t);
  // Ordinary unrelated config is still physically read and must consume the shared cap.
  fs.appendFileSync(path.join(r.bare, "config"), "\n[fixture]\n padding = " + "x".repeat(4 * 1024 * 1024) + "\n");
  assert.equal((await r.read()).kind, "complete");
  const sibling = path.join(path.dirname(r.bare), "budget-sibling.git");
  fs.cpSync(r.bare, sibling, { recursive: true });
  assert.equal((await r.read()).kind, "unknown");
});

it("includes every immutable original of chained handoffs even after active metadata is absent", async t => {
  const r = await rig(t);
  r.activate();
  const raw = git(r.bare, ["config", "--local", "--get", r.activeKey]).slice(0, -1);
  r.envelope.predecessors.push({ rawJournal: raw, journalDigest: hash(raw), generation: 2, sourceEpoch: NEXT });
  const next = structuredClone(r.link);
  next.predecessorGeneration = 2;
  next.predecessorEpoch = NEXT;
  next.predecessorJournalDigest = hash(raw);
  next.successor.claimGeneration = 3;
  next.successor.attemptId = ATTEMPT.replace("-g2-", "-g3-");
  next.successor.clonePath = attemptClonePath(r.original, next.successor.attemptId);
  next.successor.sourceEpoch = "2123abcd-0123-4abc-8def-0123456789ab";
  next.restore.predecessorGeneration = 2;
  next.restore.pinRef = "refs/uzi-recovery-pin/" + RUN + "/2";
  r.envelope.intent = { version: 1, phase: "prepared", link: next };
  r.retain();
  for (const absent of [false, true]) {
    if (absent) git(r.bare, ["config", "--local", "--unset-all", r.activeKey]);
    const result = await r.read();
    assert.equal(result.kind, "complete");
    if (result.kind === "complete") assert.deepEqual([...result.originalGenerations], [1, 2]);
  }
});

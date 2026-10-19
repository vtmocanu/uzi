import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import path from "node:path";
import { createHash, createHmac } from "node:crypto";
import { RecoveryCoordinator, canonicalJson, type ClosedSourceObservation, type RecoveryArchiveClient, type RecoveryRecord } from "../src/recovery.js";
import { MaintenanceResources, RecoveryMaintenance } from "../src/recovery-maintenance.js";
import { recordingLogger } from "./helpers.js";

const runId = "00000000-0000-4000-8000-000000000031";
const holdId = "00000000-0000-4000-8000-000000000032";
const repoId = "00000000-0000-4000-8000-000000000033";
const workerId = "00000000-0000-4000-8000-000000000034";
const workerToken = "origin-admission-fixture";
const sha = "a".repeat(40), origin = "b".repeat(64);
const digest = (value: unknown) => createHash("sha256").update(canonicalJson(value)).digest("hex");

async function fixture(t: test.TestContext) {
  const root = await fs.mkdtemp(path.resolve("../.uzi/scratch/terminal-origin-"));
  t.after(() => fs.rm(root, { recursive: true, force: true }));
  const recoveryRoot = path.join(root, "recovery");
  const ledgerRoot = path.join(root, "recovery-maintenance");
  const dir = path.join(recoveryRoot, runId);
  const clonePath = path.join(root, "source");
  await fs.mkdir(clonePath);
  const sourceFiles = ["HEAD", "index", "config", "tracking", "owed", "WIP"];
  for (const name of sourceFiles) await fs.writeFile(path.join(clonePath, name), "original-" + name);
  const genesis = { runId, generation: 1, clonePath, attemptId: "20261018T120000Z-g1-1234567890abcdef" };
  const observation: ClosedSourceObservation = {
    genesis, terminal: "normal_failed",
    originalContext: { barePath: path.join(root, "repo.git"), branch: "task", key: "key", kind: "issue",
      defaultBranch: "main", cloneUrl: "https://example.com/org/repo.git", repoId, baseCommit: "c".repeat(40),
      context: { runId, generation: 1, barePath: path.join(root, "repo.git"), branch: "task", kind: "issue",
        defaultIdentity: { ref: "refs/remotes/origin/main", sha: "d".repeat(40) } } },
    family: { originals: [{ ...genesis, identity: "1:2" }],
      observedExecution: { runId, generation: 1 }, metadata: "original-metadata" },
  };
  let stopped = false, registration = true, closed = false, executing = false, open = true;
  let metadata = observation.family!.metadata;
  let repo = repoId, quiescent = true, quiesceCalls = 0, holdCalls = 0, headCalls = 0, sourceHead = sha;
  let afterCharge: (() => Promise<void> | void) | undefined;
  const signal = new AbortController();
  const token = Object.freeze({});
  const events: string[] = [];
  const client: RecoveryArchiveClient = {
    observeTerminalMaintenanceHold: async () => {
      holdCalls++;
      MaintenanceResources.consumeIfActive("rpcAttempts", 2);
      if (closed) return { ok: false, reason: "invalid_exact_hold" };
      return { ok: true, token, hold: { run_id: runId, generation: 1, hold_id: holdId, repo_id: repo,
        original_worker_id: workerId, original_worker_identity: "human-label",
        live_worker_id: workerId, live_run_id: runId, inventory_guarded: true, state: "open",
        run_status: "failed", current_claim_generation: 1, claim_released: true } };
    },
    isCurrentTerminalMaintenanceHold: candidate => candidate === token && registration,
    listRecoveryHolds: async () => { throw new Error("advisory listing forbidden"); },
    reserveRecoveryCapture: async () => { throw new Error("reserve forbidden"); },
    uploadRecoveryBundle: async () => { throw new Error("upload forbidden"); },
    getRecoveryCaptureStatus: async () => { throw new Error("status forbidden"); },
    releaseRecoveryCustody: async () => { throw new Error("FINAL forbidden"); },
  };
  const git = {
    matchMaintenanceSourceFamily: async () => {
      if (!MaintenanceResources.discoveryActive()) {
        events.push("paid-scan");
        assert.ok((await table()).cycles[0].slots.length >= 1, "proof follows durable charge");
        if (afterCharge) { const action = afterCharge; afterCharge = undefined; await action(); }
      }
      return stopped ? undefined : { sources: observation.family!.originals, metadata };
    },
    maintenanceOriginDigest: async () => origin,
    worktreeHead: async () => { headCalls++; assert.equal(MaintenanceResources.discoveryActive(), false); return sourceHead; },
  };
  const log = recordingLogger();
  const make = () => new RecoveryCoordinator({
    recoveryRoot, workerToken, client, git: git as never, log: log.logger, isExecuting: () => executing,
    withTerminalSourceBoundary: async (_run, _paths, action) => {
      if (!open || executing) return { entered: false };
      return { entered: true, value: await action({
        open: () => open && !executing,
        isLivePath: () => executing,
        quiesce: async () => { throw new Error("bridge owns quiescence"); },
        withMaintenanceTransfer: async action => {
          // Coordinator fixture only; the runner ownership tests exercise actual child settlement.
          const check = () => { signal.signal.throwIfAborted(); MaintenanceResources.check(); };
          check();
          if (!quiescent) throw new Error("not quiescent");
          const result = await action(check);
          check();
          if (!quiescent) throw new Error("not quiescent");
          return result;
        },
        captureWip: async (_path, oldHead, callbacks) => {
          quiesceCalls++; events.push("quiesce");
          if (!quiescent) throw new Error("not quiescent");
          await callbacks.beforeStep("status", "prepare");
          await callbacks.beforeStep("status", "dispatch");
          await callbacks.afterRunnerGit("status");
          return { kind: "clean", head: oldHead, parent: null };
        },
      }) };
    },
  });
  const coordinator = make();
  const table = async () => JSON.parse(await fs.readFile(path.join(ledgerRoot, "complete.json"), "utf8")).value;
  const run = (target = coordinator, obs = observation) => target.resumeTerminalSource(runId, Promise.resolve(obs),
    obs.family!.observedExecution.generation, signal.signal);
  const prefixes = async () => (await fs.readdir(dir).catch(() => [])).filter(n => /^maintenance-v1-.+\.json$/.test(n));
  const chargeCount = async () => ((await table().catch(() => undefined))?.cycles ?? []).reduce(
    (n: number, cycle: { slots: unknown[] }) => n + cycle.slots.length, 0);
  const unchanged = async () => {
    for (const name of sourceFiles) assert.equal(await fs.readFile(path.join(clonePath, name), "utf8"), "original-" + name);
  };
  const seal = async (record: RecoveryRecord) => {
    await fs.mkdir(dir, { recursive: true });
    const key = createHmac("sha256", workerToken).update("uzi-recovery-journal-v1").digest();
    await fs.writeFile(path.join(dir, record.captureId + ".json"), JSON.stringify({ ...record,
      mac: createHmac("sha256", key).update(canonicalJson(record)).digest("hex") }));
  };
  return { root, dir, ledgerRoot, recoveryRoot, client, git, coordinator, make, run, observation, prefixes, table,
    chargeCount, unchanged, seal, events, signal, log,
    stopped: () => { stopped = true; }, close: () => { closed = true; },
    registration: () => { registration = false; }, crossRepo: () => { repo = "other"; },
    active: () => { executing = true; }, boundaryClosed: () => { open = false; },
    unquiescent: () => { quiescent = false; }, headChanged: () => { sourceHead = "e".repeat(40); },
    afterCharge: (action: () => Promise<void> | void) => { afterCharge = action; },
    metadata: (value: string) => { metadata = value; },
    counts: () => ({ quiesceCalls, holdCalls, headCalls }) };
}

test("terminal origin: real ledger indexes a MAC standard prefix before publication; source and UUID unchanged", async t => {
  const f = await fixture(t);
  const ordinaryId = "00000000-0000-4000-8000-000000000035";
  await f.seal({ version: 1, runId, generation: 1, captureId: ordinaryId, sourceSha: sha,
    kind: "issue", branch: "task", createdAt: 1, state: "pinned", inventoryGuarded: true,
    bareDir: "repo.git", defaultBranch: "main" });
  const ordinaryPath = path.join(f.dir, ordinaryId + ".json");
  const ordinaryBytes = await fs.readFile(ordinaryPath, "utf8");
  const rename = fs.rename.bind(fs);
  let publications = 0;
  t.mock.method(fs, "rename", async (...args: Parameters<typeof fs.rename>) => {
    if (String(args[1]).startsWith(f.dir + path.sep)) {
      publications++;
      const captureId = path.basename(String(args[1]), ".json");
      assert.equal(captureId.length, 51);
      const table = await f.table();
      assert.equal(table.cycles.length, 1);
      assert.equal(table.cycles[0].identity.custodian, workerId);
      assert.equal(table.cycles[0].fenced, true);
      assert.deepEqual(table.cycles[0].outputs, [digest(captureId)]);
      assert.ok(f.events.includes("quiesce"));
    }
    return rename(...args);
  });
  await f.run();
  assert.equal(publications, 1);
  const records = await f.coordinator.inspect(runId);
  assert.equal(records.length, 2);
  assert.equal(await fs.readFile(ordinaryPath, "utf8"), ordinaryBytes);
  const record = records.find(r => r.captureId !== ordinaryId);
  assert.ok(record);
  assert.equal(record.version, 1);
  assert.equal(record.sourceSha, sha);
  assert.equal(record.state, "pinned");
  assert.equal(record.heldPolicy, undefined);
  assert.deepEqual(record.exactHoldBinding, { holdId, repoId });
  assert.equal((await fs.stat(path.join(f.dir, record.captureId + ".json"))).mode & 0o777, 0o600);
  await f.unchanged();
  const bytes = await fs.readFile(path.join(f.dir, record.captureId + ".json"), "utf8");
  await f.run();
  await f.run(f.make());
  assert.equal(await f.chargeCount(), 1);
  assert.equal(await fs.readFile(ordinaryPath, "utf8"), ordinaryBytes);
  assert.equal(await fs.readFile(path.join(f.dir, record.captureId + ".json"), "utf8"), bytes);
  // Independent authenticated lookup uses the exact prefixed ID digest.
  const identity = (await f.table()).cycles[0].identity;
  const { originalSource: _source, sourceConfig: _config, ...binding } = identity;
  const resolved = await new RecoveryMaintenance({ root: f.ledgerRoot, workerToken }).resolveOutput(digest(record.captureId), binding);
  assert.deepEqual(resolved, { ok: true, value: identity });
});

for (const mode of ["stopped", "registration", "closed", "cross-repo", "missing-context", "active", "shutdown",
  "incomplete", "malformed", "symlink", "unknown-prefix", "invalid-history"] as const) {
  test("terminal origin refuses before charge: " + mode, async t => {
    const f = await fixture(t);
    let obs = f.observation;
    if (mode === "stopped") f.stopped();
    if (mode === "registration") f.registration();
    if (mode === "closed") f.close();
    if (mode === "cross-repo") f.crossRepo();
    if (mode === "active") f.active();
    if (mode === "shutdown") f.signal.abort();
    if (mode === "missing-context") obs = { ...obs, originalContext: undefined };
    if (["incomplete", "malformed", "symlink", "unknown-prefix"].includes(mode)) {
      await fs.mkdir(f.dir, { recursive: true });
      if (mode === "symlink") await fs.symlink(f.root, path.join(f.dir, "unknown.json"));
      else await fs.writeFile(path.join(f.dir, mode === "incomplete" ? "unknown.tmp" : "maintenance-v1-bad.json"), "{");
    }
    if (mode === "invalid-history") {
      await fs.mkdir(f.ledgerRoot);
      await fs.writeFile(path.join(f.ledgerRoot, "complete.json"), "{}");
    }
    await f.run(f.coordinator, obs);
    assert.equal(await f.chargeCount(), 0);
    assert.equal((await f.prefixes()).filter(n => n !== "maintenance-v1-bad.json").length, 0);
    assert.equal(f.counts().quiesceCalls, 0);
    await f.unchanged();
  });
}

for (const mode of ["stopped", "registration", "closed", "active", "shutdown", "unquiescent"] as const) {
  test("terminal origin retains paid charge without prefix after: " + mode, async t => {
    const f = await fixture(t);
    f.afterCharge(() => {
      if (mode === "stopped") f.stopped();
      if (mode === "registration") f.registration();
      if (mode === "closed") f.close();
      if (mode === "active") f.active();
      if (mode === "shutdown") f.signal.abort();
      if (mode === "unquiescent") f.unquiescent();
    });
    await f.run();
    assert.equal(await f.chargeCount(), 1);
    assert.deepEqual(await f.prefixes(), []);
    assert.equal((await f.table()).cycles[0].fenced, true);
    await f.unchanged();
  });
}

test("terminal origin missing indexed artifact cannot charge or mint after changed HEAD", async t => {
  const f = await fixture(t);
  t.mock.timers.enable({ apis: ["Date"], now: 1_000_000 });
  await f.run();
  const published = await f.prefixes();
  assert.equal(published.length, 1, "ordinary first admission publishes an origin");
  assert.ok(published[0]);
  assert.equal((await f.coordinator.inspect(runId)).length, 1);
  const before = (await f.table()).cycles[0];
  assert.deepEqual(before.outputs, [digest(path.basename(published[0], ".json")),
    digest(["maintenance-wip-v1", before.identity, sha])]);
  assert.equal(before.slots.length, 1);
  await fs.unlink(path.join(f.dir, published[0]));
  f.headChanged();
  t.mock.timers.setTime(before.slots[0].start + 180_001);
  await f.run(f.make(), structuredClone(f.observation));
  assert.equal(await f.chargeCount(), 1, "missing indexed origin must not charge a second slot");
  assert.deepEqual(await f.prefixes(), [], "missing indexed origin must not mint another ID");
  const after = (await f.table()).cycles[0];
  assert.deepEqual(after.outputs, before.outputs);
  assert.equal(after.originalCycleExpiry, before.originalCycleExpiry);
  assert.equal(after.fenced, before.fenced);
});

test("terminal origin syncs published run directory before authenticated readback", async t => {
  const f = await fixture(t);
  const open = fs.open.bind(fs), rename = fs.rename.bind(fs);
  let published = false, synced = false, readback = false, parentSyncs = 0, fileSyncs = 0;
  t.mock.method(fs, "rename", async (...args: Parameters<typeof fs.rename>) => {
    await rename(...args);
    if (String(args[1]).startsWith(f.dir + path.sep)) published = true;
  });
  t.mock.method(fs, "open", async (...args: Parameters<typeof fs.open>) => {
    const file = await open(...args);
    const name = String(args[0]), sync = file.sync.bind(file);
    file.sync = async () => {
      await sync();
      if (name === f.dir && published) synced = true;
      else if (name === f.dir) parentSyncs++;
      else if (name.startsWith(f.dir + path.sep) && name.endsWith(".tmp")) fileSyncs++;
    };
    if (published && name.startsWith(f.dir + path.sep) && name.endsWith(".json")) {
      readback = true;
      if (!synced) await file.close();
      assert.equal(synced, true, "published run directory must sync before authenticated readback");
    }
    return file;
  });
  await f.run();
  assert.equal(published, true);
  assert.ok(parentSyncs > 0, "new run directory was synced before publication");
  assert.equal(fileSyncs, 1, "temporary file fsync is separate");
  assert.equal(synced, true, "post-rename run directory sync must occur");
  assert.equal(readback, true);
  assert.equal((await f.coordinator.inspect(runId)).length, 1);
  await f.unchanged();
});

for (const mode of ["fsync", "rename", "readback"] as const) {
  test("terminal origin durable publication fault preserves charge/evidence, cannot renew: " + mode, async t => {
    const f = await fixture(t);
    const open = fs.open.bind(fs), rename = fs.rename.bind(fs);
    let injected = false;
    t.mock.method(fs, "open", async (...args: Parameters<typeof fs.open>) => {
      const file = await open(...args);
      if (String(args[0]).startsWith(f.dir + path.sep)) {
        if (mode === "fsync" && String(args[0]).endsWith(".tmp")) {
          const sync = file.sync.bind(file);
          file.sync = async () => { if (!injected) { injected = true; throw new Error("fsync fault"); } await sync(); };
        }
        if (mode === "readback" && String(args[0]).endsWith(".json")) {
          const read = file.read.bind(file);
          file.read = (async (...readArgs: Parameters<typeof read>) => {
            if (!injected) { injected = true; throw new Error("readback fault"); }
            return read(...readArgs);
          }) as typeof file.read;
        }
      }
      return file;
    });
    t.mock.method(fs, "rename", async (...args: Parameters<typeof fs.rename>) => {
      if (mode === "rename" && String(args[1]).startsWith(f.dir + path.sep)) {
        injected = true; throw new Error("rename fault");
      }
      return rename(...args);
    });
    await f.run();
    assert.equal(injected, true);
    assert.equal(await f.chargeCount(), 1);
    const evidence = (await fs.readdir(f.dir)).sort();
    assert.ok(evidence.length > 0);
    await f.run(f.make());
    assert.equal(await f.chargeCount(), 1);
    assert.deepEqual((await fs.readdir(f.dir)).sort(), evidence);
    assert.equal((await f.table()).cycles[0].fenced, true);
    await f.unchanged();
  });
}

for (const resource of ["metadataBytes", "adminEntries"] as const) {
  test("terminal origin reserves " + resource + " before writer dispatch", async t => {
    const f = await fixture(t);
    let used = 0, armed = false, tempOpens = 0, pathVisits = 0;
    const consume = MaintenanceResources.consumeIfActive.bind(MaintenanceResources);
    t.mock.method(MaintenanceResources, "consumeIfActive", (...args: Parameters<typeof consume>) => {
      if (args[0] === resource && MaintenanceResources.active() && !MaintenanceResources.discoveryActive()) used += args[1];
      return consume(...args);
    });
    const before = RecoveryMaintenance.prototype.beforeMutation;
    t.mock.method(RecoveryMaintenance.prototype, "beforeMutation", async function (this: RecoveryMaintenance,
      ...args: Parameters<typeof before>) {
      const result = await before.apply(this, args);
      assert.ok(result.ok);
      MaintenanceResources.consume(resource, (resource === "metadataBytes" ? 256 * 1024 ** 2 - 1 : 4096) - used);
      armed = true;
      return result;
    });
    const open = fs.open.bind(fs), stat = fs.lstat.bind(fs);
    t.mock.method(fs, "open", async (...args: Parameters<typeof fs.open>) => {
      if (armed && String(args[0]).startsWith(f.dir + path.sep) && String(args[0]).endsWith(".tmp")) tempOpens++;
      return open(...args);
    });
    t.mock.method(fs, "lstat", ((...args: Parameters<typeof fs.lstat>) => {
      if (armed && MaintenanceResources.active() && !MaintenanceResources.discoveryActive()) pathVisits++;
      return stat(...args);
    }) as typeof fs.lstat);
    await f.run();
    assert.equal(armed, true);
    assert.equal(tempOpens, 0, "no producer temp file may open beyond its reservation");
    assert.equal(pathVisits, 0, "quota refusal precedes the writer ancestor walk");
    assert.equal(f.log.lines.filter(line => (line as { reason?: string }).reason ===
      "maintenance_resource_exhausted").length, 1, "one specific quota diagnostic survives scope settlement");
    assert.deepEqual(await f.prefixes(), []);
    assert.equal(await f.chargeCount(), 1);
  });
}

for (const mismatch of ["repo", "hold", "source-context"] as const) {
  test("terminal origin refuses authenticated local mismatch before charge: " + mismatch, async t => {
    const f = await fixture(t);
    await f.seal({ version: 1, runId, generation: 1, captureId: "00000000-0000-4000-8000-000000000035",
      kind: "issue", branch: "task", sourceSha: sha, createdAt: Date.now(), state: "pinned",
      inventoryGuarded: true, bareDir: "repo.git",
      exactHoldBinding: { holdId: mismatch === "hold" ? "00000000-0000-4000-8000-000000000036" : holdId,
        repoId: mismatch === "repo" ? "00000000-0000-4000-8000-000000000037" : repoId },
      ...(mismatch === "source-context" ? {
        coverageDigest: "f".repeat(64), originalSourceSha: sha, inventoryCurrentSha: sha, originalRoots: [],
        coverageContext: { ...f.observation.originalContext!.context,
          defaultIdentity: { ref: "refs/remotes/origin/main", sha: "e".repeat(40) } },
      } : {}) });
    assert.equal((await f.coordinator.inspect(runId)).length, 1, "negative binding is authenticated");
    await f.run();
    assert.equal(await f.chargeCount(), 0);
    assert.deepEqual(await f.prefixes(), []);
    assert.ok(JSON.stringify(f.log.lines).includes("local_binding_mismatch"));
    assert.ok(!JSON.stringify(f.log.lines).includes(f.root));
  });
}

test("terminal origin refuses once with bounded fixed diagnostic on invalid history", async t => {
  const f = await fixture(t);
  await fs.mkdir(f.ledgerRoot);
  await fs.writeFile(path.join(f.ledgerRoot, "complete.json"), "{}");
  await f.run();
  const refusals = f.log.lines.filter((line: unknown) =>
    (line as { msg?: string }).msg === "recovery: terminal origin refused");
  assert.equal(refusals.length, 1);
  assert.equal((refusals[0] as { reason: string }).reason, "maintenance_state_invalid");
  assert.ok(!JSON.stringify(refusals).includes(f.root));
});

test("terminal origin blocked inheritance uses original identity across successor metadata", async t => {
  const f = await fixture(t);
  const blocked: ClosedSourceObservation = { ...f.observation, terminal: "normal_blocked", family: {
    ...f.observation.family!, observedExecution: { runId, generation: 9 }, metadata: "successor-bookkeeping",
  } };
  f.metadata("successor-bookkeeping");
  await f.run(f.coordinator, blocked);
  const identity = (await f.table()).cycles[0].identity;
  assert.equal(identity.generation, 1);
  assert.equal(identity.originalSource, digest([runId, 1, blocked.genesis.clonePath, blocked.genesis.attemptId,
    blocked.originalContext!.baseCommit]));
  assert.equal(identity.custodian, workerId);
  await f.unchanged();
});

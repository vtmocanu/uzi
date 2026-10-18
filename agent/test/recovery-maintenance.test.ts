import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import path from "node:path";
import { createHmac, createHash } from "node:crypto";
import { execFile } from "node:child_process";
import { promisify } from "node:util";
import { RecoveryMaintenance, MaintenanceResources, MAINTENANCE_VERIFIER_VERSION } from "../src/recovery-maintenance.js";
import { MaintenanceBudget } from "../src/maintenance-budget.js";
import type { MaintenanceAdmission, MaintenanceEligibility, MaintenanceIdentity, MaintenanceResult, MaintenanceSlot } from "../src/recovery-maintenance.js";

const workerToken = "test-worker-key";
const sha = (s: string) => createHash("sha256").update(s).digest("hex");
function eligible(): MaintenanceEligibility {
  return { identity: { run: "run", generation: 1, hold: "hold", repo: "repo", custodian: "worker",
    bareBasename: "repo.git", origin: sha("origin"), originalSource: sha("input"), sourceConfig: sha("source-config") },
  config: { decodedCap: 8 * 1024 ** 3, verifierVersion: MAINTENANCE_VERIFIER_VERSION },
  exactCustody: true, terminalNonStopped: true, quiescent: true, sourceOutputContinuity: true, source: sha("input") };
}
function take<T>(r: MaintenanceResult<T>): T {
  assert.equal(r.ok, true, JSON.stringify(r));
  if (!r.ok) throw new Error("admission refused");
  return r.value;
}
function refused<T>(r: MaintenanceResult<T>, reason: string): void { assert.deepEqual(r, { ok: false, reason }); }
async function fixture(t: test.TestContext) {
  const dir = await fs.mkdtemp(path.resolve("../.uzi/scratch/maintenance-test-"));
  t.after(() => fs.rm(dir, { recursive: true, force: true }));
  const root = path.join(dir, "recovery-maintenance");
  let now = 1_000_000;
  const make = (extra = {}) => new RecoveryMaintenance({ root, workerToken, now: () => now, ...extra });
  return { root, make, time: (n: number) => { now = n; }, get now() { return now; } };
}
function canonical(value: unknown): string {
  if (Array.isArray(value)) return "[" + value.map(canonical).join(",") + "]";
  if (value !== null && typeof value === "object") return "{" + Object.entries(value)
    .sort(([a], [b]) => a.localeCompare(b, "en")).map(([k, v]) => JSON.stringify(k) + ":" + canonical(v)).join(",") + "}";
  return JSON.stringify(value);
}
async function signed(file: string, value: unknown): Promise<void> {
  const key = createHmac("sha256", workerToken).update("uzi-recovery-maintenance-v1").digest();
  const mac = createHmac("sha256", key).update(canonical(value)).digest("hex");
  await fs.writeFile(file, canonical({ value, mac }));
}
async function reseal(root: string, value: any): Promise<void> {
  await signed(path.join(root, "complete.json"), value);
  const marker = JSON.parse(await fs.readFile(path.join(root, "initialized.json"), "utf8")).value;
  marker.tableHash = sha(canonical(value));
  await signed(path.join(root, "initialized.json"), marker);
}
async function table(root: string): Promise<any> {
  return JSON.parse(await fs.readFile(path.join(root, "complete.json"), "utf8")).value;
}

function expectedBinding(identity: MaintenanceIdentity) {
  const { originalSource: _source, sourceConfig: _config, ...binding } = identity;
  return binding;
}
async function storageSnapshot(dir: string): Promise<unknown> {
  // Bounded test fixture tree: capture names, bytes and write metadata, not atime.
  const names = (await fs.readdir(dir)).sort();
  return Promise.all(names.map(async name => {
    const file = path.join(dir, name), stat = await fs.lstat(file);
    return [name, stat.ino, stat.mtimeMs, stat.ctimeMs, stat.isDirectory()
      ? await storageSnapshot(file) : await fs.readFile(file, "utf8")];
  }));
}

function barrier() {
  let resolve!: () => void;
  const promise = new Promise<void>(done => { resolve = done; });
  return { promise, resolve };
}

// Pause one real durable write; every test releases and joins it even on assertion failure.
function pauseIndexWrite(t: test.TestContext, root: string) {
  const entered = barrier(), release = barrier(), rename = fs.rename.bind(fs);
  let armed = true;
  t.mock.method(fs, "rename", async (...args: Parameters<typeof fs.rename>) => {
    await rename(...args);
    if (armed && args[1] === path.join(root, "complete.json")) {
      armed = false;
      entered.resolve();
      await release.promise;
    }
  });
  return { entered: entered.promise, release: release.resolve };
}

test("concurrent refresh latest invalid proof denies effects across components", async t => {
  const f = await fixture(t), ledger = f.make(), peer = f.make(), proof = eligible();
  const token = take(await ledger.admit(proof));
  const invalid = { ...proof, quiescent: false } as unknown as MaintenanceEligibility;
  const results = await Promise.all([ledger.revalidate(token, proof), peer.revalidate(token, invalid)]);
  refused(results[1], "maintenance_ineligible");
  let effects = 0;
  if ((await ledger.beforeMutation(token, [sha("concurrent-invalid")])).ok) effects++;
  assert.equal(effects, 0);
  refused(await ledger.finish(token, "permanent_nonmutating"), "maintenance_ineligible");
  const cycle = (await table(f.root)).cycles[0];
  assert.equal(cycle.fenced, true);
  assert.equal(cycle.slots.length, 1);
});

test("concurrent refresh invalid then valid retains latest positive proof", async t => {
  const f = await fixture(t), ledger = f.make(), peer = f.make(), proof = eligible();
  const token = take(await ledger.admit(proof));
  const results = await Promise.all([
    ledger.revalidate(token, { ...proof, quiescent: false } as unknown as MaintenanceEligibility),
    peer.revalidate(token, proof),
  ]);
  refused(results[0], "maintenance_ineligible");
  take(results[1]);
  take(await ledger.beforeMutation(token, [sha("latest-valid")]));
  take(await ledger.finish(token, "nonmutating"));
});

for (const failure of ["proof", "history", "key", "root"] as const) {
  test(`concurrent refresh older completion cannot verify pending latest ${failure} failure`, async t => {
    const f = await fixture(t), ledger = f.make(), proof = eligible();
    const token = take(await ledger.admit(proof));
    take(await ledger.revalidate(token, proof));
    const gate = pauseIndexWrite(t, f.root);
    const older = ledger.revalidate(token, proof);
    await gate.entered;
    let latest: Promise<MaintenanceResult<void>>;
    if (failure === "history") {
      const open = fs.open.bind(fs);
      let reads = 0;
      t.mock.method(fs, "open", async (...args: Parameters<typeof fs.open>) => {
        // Allow the older write to authenticate its own replacement, then fail the latest load.
        if (args[0] === path.join(f.root, "complete.json") && ++reads > 1) throw new Error("controlled unreadable history");
        return open(...args);
      });
      latest = f.make().revalidate(token, proof);
    } else if (failure === "key") {
      latest = f.make({ workerToken: "different-test-key" }).revalidate(token, proof);
    } else if (failure === "root") {
      const other = await fixture(t);
      latest = other.make().revalidate(token, proof);
    } else {
      latest = f.make().revalidate(token, { ...proof, quiescent: false } as unknown as MaintenanceEligibility);
    }
    gate.release();
    const [oldResult, latestResult] = await Promise.all([older, latest]);
    assert.equal(latestResult.ok, false);
    // Restore only test filesystem interception before inspecting actual guard behavior.
    t.mock.restoreAll();
    let effects = 0;
    if ((await ledger.beforeMutation(token, [sha("stale-" + failure)])).ok) effects++;
    assert.equal(effects, 0);
    assert.equal(oldResult.ok, false);
    take(await ledger.revalidate(token, proof));
    take(await ledger.beforeMutation(token, [sha("recovered-" + failure)]));
    take(await ledger.finish(token, "nonmutating"));
  });
}

for (const failure of ["proof", "history", "key", "root"] as const) {
  test(`concurrent refresh delayed older ${failure} failure preserves later positive completion`, async t => {
    const f = await fixture(t), ledger = f.make(), proof = eligible();
    const token = take(await ledger.admit(proof));
    const other = failure === "root" ? await fixture(t) : undefined;
    const failedLedger = failure === "root" ? other!.make() :
      failure === "key" ? f.make({ workerToken: "different-test-key" }) : f.make();
    const entered = barrier(), release = barrier(), lstat = fs.lstat.bind(fs);
    let armed = true;
    t.mock.method(fs, "lstat", async (...args: Parameters<typeof fs.lstat>) => {
      if (armed) {
        armed = false;
        entered.resolve();
        await release.promise;
      }
      return lstat(...args);
    });
    const older = failedLedger.revalidate(token, failure === "proof" ?
      { ...proof, quiescent: false } as unknown as MaintenanceEligibility : proof);
    await entered.promise;
    try {
      take(await ledger.revalidate(token, proof));
      take(await ledger.beforeMutation(token, [sha("newer-before-" + failure)]));
    } finally { release.resolve(); }
    if (failure === "history") {
      const open = fs.open.bind(fs);
      t.mock.method(fs, "open", async (...args: Parameters<typeof fs.open>) => {
        if (args[0] === path.join(f.root, "complete.json")) throw new Error("controlled older load failure");
        return open(...args);
      });
    }
    const failed = await older;
    assert.equal(failed.ok, false);
    t.mock.restoreAll();
    take(await ledger.beforeMutation(token, [sha("newer-after-" + failure)]));
    take(await ledger.finish(token, "nonmutating"));
  });
}

for (const latestValid of [false, true]) {
  test(`mutation save loses permission when refresh starts during persistence (valid=${latestValid})`, async t => {
    const f = await fixture(t), ledger = f.make(), proof = eligible(), output = sha("racing-output");
    const token = take(await ledger.admit(proof));
    take(await ledger.revalidate(token, proof));
    const gate = pauseIndexWrite(t, f.root);
    const mutation = ledger.beforeMutation(token, [output]);
    await gate.entered;
    const refresh = f.make().revalidate(token, latestValid ? proof :
      { ...proof, quiescent: false } as unknown as MaintenanceEligibility);
    gate.release();
    const [permission, refreshed] = await Promise.all([mutation, refresh]);
    let effects = 0;
    if (permission.ok) effects++;
    assert.equal(effects, 0);
    refused(permission, "maintenance_ineligible");
    assert.equal(refreshed.ok, latestValid);
    const cycle = (await table(f.root)).cycles[0];
    assert.deepEqual(cycle.outputs, [output]);
    assert.equal(cycle.slots.length, 1);
    assert.equal(cycle.reservedWork, 120_000);
    assert.equal(cycle.fenced, true);
    if (latestValid) take(await ledger.beforeMutation(token, [output]));
    else refused(await ledger.beforeMutation(token, [output]), "maintenance_ineligible");
    take(await ledger.finish(token, "transient"));
  });
}

test("output lookup routes immutable capture digests to original identity without new slots or writes", async t => {
  const f = await fixture(t), input = eligible(), ledger = f.make();
  const output = sha("maintenance-v1-12345678-1234-4abc-8abc-123456789abc");
  const token = take(await ledger.admit(input));
  take(await ledger.revalidate(token, input));
  take(await ledger.beforeMutation(token, [output]));
  take(await ledger.finish(token, "transient"));
  const before = await storageSnapshot(path.dirname(f.root));
  // Evidence routing must not consult or advance either admission clock.
  const reader = f.make({ now: () => { throw new Error("lookup read the clock"); },
    monotonicNow: () => { throw new Error("lookup read the monotonic clock"); } });
  const binding = expectedBinding(input.identity);
  const resolved = take(await reader.resolveOutput(output, binding));
  assert.deepEqual(resolved, input.identity);
  resolved.originalSource = sha("caller mutation");
  assert.deepEqual(take(await reader.resolveOutput(output, binding)), input.identity);
  for (const [key, value] of Object.entries(binding)) {
    const wrong = { ...binding, [key]: typeof value === "number" ? 2 : key === "origin" ? sha("wrong") : "wrong" };
    refused(await reader.resolveOutput(output, wrong), "maintenance_identity_invalid");
  }
  refused(await reader.resolveOutput(output, { ...binding, generation: 0 }), "maintenance_identity_invalid");
  refused(await reader.resolveOutput(output + "\n", binding), "maintenance_identity_invalid");
  for (const unknown of [sha("new capture id"), sha("updated tip"), input.identity.originalSource])
    refused(await reader.resolveOutput(unknown, binding), "maintenance_lineage_invalid");
  assert.deepEqual(await storageSnapshot(path.dirname(f.root)), before);
  f.time(f.now + 180_000);
  // The record's output cannot become a fresh input with a fresh config identity.
  const replacement = structuredClone(input);
  replacement.identity.originalSource = output; replacement.source = output;
  replacement.identity.sourceConfig = sha("record updated config");
  refused(await ledger.admit(replacement), "maintenance_lineage_invalid");
  const continuation = { ...input, identity: take(await reader.resolveOutput(output, binding)), source: output };
  const second = take(await ledger.admit(continuation));
  assert.equal(second.ordinal, 2);
  assert.equal(second.admittedAt, token.admittedAt);
  assert.equal(second.originalCycleExpiry, token.originalCycleExpiry);
  take(await ledger.revalidate(second, continuation));
  take(await ledger.finish(second, "nonmutating"));
  const fresh = eligible();
  fresh.identity.originalSource = sha("positively unseen source");
  fresh.source = fresh.identity.originalSource;
  const newSlot = take(await ledger.admit(fresh));
  assert.equal(newSlot.ordinal, 1);
  take(await ledger.revalidate(newSlot, fresh));
  take(await ledger.finish(newSlot, "nonmutating"));
});

test("output lookup retains one original identity across configuration siblings and completed cycles", async t => {
  const f = await fixture(t), ledger = f.make(), input = eligible(), output = sha("shared-config-output");
  input.config.decodedCap = 4096;
  const first = take(await ledger.admit(input));
  take(await ledger.revalidate(first, input));
  take(await ledger.beforeMutation(first, [output]));
  take(await ledger.finish(first, "nonmutating"));
  const before = await storageSnapshot(path.dirname(f.root));
  assert.deepEqual(take(await f.make().resolveOutput(output, expectedBinding(input.identity))), input.identity);
  assert.deepEqual(await storageSnapshot(path.dirname(f.root)), before);
  refused(await ledger.admit(input), "maintenance_slot_spacing");
  f.time(f.now + 180_000);
  const raised = { ...input, config: { ...input.config, decodedCap: 8192 }, source: output };
  const next = take(await ledger.admit(raised));
  take(await ledger.revalidate(next, raised));
  take(await ledger.beforeMutation(next, [output]));
  take(await ledger.finish(next, "complete"));
  const siblings = await storageSnapshot(path.dirname(f.root));
  assert.deepEqual(take(await f.make().resolveOutput(output, expectedBinding(input.identity))), input.identity);
  assert.deepEqual(await storageSnapshot(path.dirname(f.root)), siblings);
  refused(await ledger.admit(raised), "maintenance_cycle_exhausted");
});

test("output lookup never initializes absent or interrupted storage; ordinary first admission still initializes", async t => {
  for (const staged of [false, true]) {
    const f = await fixture(t);
    if (staged) {
      await fs.mkdir(f.root + ".initializing");
      await fs.writeFile(path.join(f.root + ".initializing", "evidence"), "interrupted");
    }
    const before = await storageSnapshot(path.dirname(f.root));
    refused(await f.make().resolveOutput(sha("output"), expectedBinding(eligible().identity)), "maintenance_state_invalid");
    assert.deepEqual(await storageSnapshot(path.dirname(f.root)), before);
    await assert.rejects(fs.lstat(f.root), { code: "ENOENT" });
    if (!staged) {
      const slot = take(await f.make().admit(eligible()));
      take(await f.make().revalidate(slot, eligible()));
      take(await f.make().finish(slot, "nonmutating"));
    }
  }
});

test("output lookup preserves missing, corrupt, tampered, index-lost and rotated-key evidence", async t => {
  for (const mode of ["missing-table", "missing-marker", "corrupt", "tampered", "index-lost",
    "output-index-lost", "invalid-identity", "incomplete", "conflict", "rotated", "stage"] as const) {
    const f = await fixture(t), ledger = f.make(), input = eligible(), output = sha("indexed output");
    const slot = take(await ledger.admit(input));
    take(await ledger.revalidate(slot, input));
    take(await ledger.beforeMutation(slot, [output]));
    take(await ledger.revalidate(slot, input));
    take(await ledger.finish(slot, "nonmutating"));
    const tableFile = path.join(f.root, "complete.json"), markerFile = path.join(f.root, "initialized.json");
    if (mode === "missing-table") await fs.unlink(tableFile);
    else if (mode === "missing-marker") await fs.unlink(markerFile);
    else if (mode === "corrupt") await fs.writeFile(tableFile, "{");
    else if (mode === "tampered") {
      const raw = await fs.readFile(tableFile, "utf8");
      await fs.writeFile(tableFile, raw.replace('"run":"run"', '"run":"other"'));
    } else if (mode === "stage") await fs.mkdir(f.root + ".initializing");
    else if (mode === "index-lost") {
      const marker = JSON.parse(await fs.readFile(markerFile, "utf8")).value;
      marker.history.pop();
      await signed(markerFile, marker);
    } else if (mode !== "rotated") {
      const stored = await table(f.root);
      if (mode === "output-index-lost") stored.cycles[0].outputs = [];
      if (mode === "invalid-identity") stored.cycles[0].identity.generation = 0;
      if (mode === "incomplete") stored.complete = false;
      if (mode === "conflict") {
        const conflict = structuredClone(stored.cycles[0]);
        conflict.identity.run = "other-run";
        conflict.identity.originalSource = sha("other-source");
        conflict.slots[0].nonce = "other-nonce";
        stored.cycles.push(conflict);
      }
      await reseal(f.root, stored);
    }
    const before = await storageSnapshot(path.dirname(f.root));
    refused(await f.make(mode === "rotated" ? { workerToken: "rotated-worker" } : {})
      .resolveOutput(output, expectedBinding(input.identity)), "maintenance_state_invalid");
    assert.deepEqual(await storageSnapshot(path.dirname(f.root)), before, mode);
  }
});

test("output lookup works at full capacity and exhausted/expired cycles without rebasing history", async t => {
  const f = await fixture(t), ledger = f.make(), input = eligible();
  const outputs = Array.from({ length: 4094 }, (_, i) => sha("lookup-output-" + i));
  for (let ordinal = 1; ordinal <= 3; ordinal++) {
    const slot = take(await ledger.admit(input));
    take(await ledger.revalidate(slot, input));
    if (ordinal === 1) take(await ledger.beforeMutation(slot, outputs));
    take(await ledger.revalidate(slot, input));
    take(await ledger.finish(slot, "nonmutating"));
    f.time(f.now + 180_000);
  }
  for (const expired of [false, true]) {
    if (expired) f.time(1_000_000 + 86_400_000);
    const before = await storageSnapshot(path.dirname(f.root));
    for (const output of [outputs[0]!, outputs[4093]!])
      assert.deepEqual(take(await f.make().resolveOutput(output, expectedBinding(input.identity))), input.identity);
    refused(await ledger.resolveOutput(sha("unknown"), expectedBinding(input.identity)), "maintenance_lineage_invalid");
    assert.deepEqual(await storageSnapshot(path.dirname(f.root)), before);
    refused(await ledger.admit(input), expired ? "maintenance_cycle_expired" : "maintenance_cycle_exhausted");
  }
  const fresh = eligible(); fresh.identity.run = "new-run";
  refused(await ledger.admit(fresh), "maintenance_table_capacity");
});

test("new origin admission protects indexed history without changing continuation or expiry priority", async t => {
  const f = await fixture(t), input = eligible(), ledger = f.make(), output = sha("initial-origin");
  const first = take(await ledger.admit(input, { newOrigin: true }));
  take(await ledger.revalidate(first, input));
  take(await ledger.beforeMutation(first, [output]));
  take(await ledger.finish(first, "nonmutating"));
  f.time(first.start + 180_001);
  refused(await f.make().admit(input, { newOrigin: true }), "maintenance_lineage_invalid");
  const raised = { ...input, config: { ...input.config, decodedCap: input.config.decodedCap + 1 } };
  refused(await f.make().admit(raised, { newOrigin: true }), "maintenance_lineage_invalid");
  let stored = await table(f.root);
  assert.equal(stored.cycles.length, 1, "cap changes cannot persist a new cycle");
  assert.equal(stored.cycles[0].slots.length, 1);
  assert.deepEqual(stored.cycles[0].outputs, [output]);
  const continuation = { ...input, source: output };
  const second = take(await f.make().admit(continuation));
  take(await ledger.revalidate(second, continuation));
  take(await ledger.finish(second, "nonmutating"));
  f.time(second.start + 180_001);
  const third = take(await ledger.admit(continuation));
  take(await ledger.revalidate(third, continuation));
  take(await ledger.finish(third, "nonmutating"));
  refused(await ledger.admit(input, { newOrigin: true }), "maintenance_cycle_exhausted");
  f.time(first.originalCycleExpiry);
  refused(await f.make().admit(input, { newOrigin: true }), "maintenance_cycle_expired");
  f.time(first.originalCycleExpiry - 1);
  refused(await f.make().admit(input, { newOrigin: true }), "maintenance_clock_invalid");
  stored = await table(f.root);
  assert.equal(stored.cycles.length, 1);
  assert.equal(stored.cycles[0].slots.length, 3);
  assert.equal(stored.cycles[0].originalCycleExpiry, first.originalCycleExpiry);
});

test("new origin pre-index crash retains spaced eligibility and its mutation fence", async t => {
  const f = await fixture(t), input = eligible(), ledger = f.make();
  const first = take(await ledger.admit(input, { newOrigin: true }));
  take(await ledger.finish(first, "transient"));
  f.time(first.start + 180_001);
  const second = take(await f.make().admit(input, { newOrigin: true }));
  assert.equal(second.ordinal, 2);
  assert.equal(second.originalCycleExpiry, first.originalCycleExpiry);
  const stored = (await table(f.root)).cycles[0];
  assert.deepEqual(stored.outputs, []);
  assert.equal(stored.fenced, true);
  take(await ledger.finish(second, "transient"));
});

test("durable slots survive interruption, reserve full totals, retain expiry and stop after three", async t => {
  const f = await fixture(t), input = eligible(), ledger = f.make();
  const first = take(await ledger.admit(input));
  assert.equal(first.deadline, f.now + 120_000);
  assert.equal(first.originalCycleExpiry, first.admittedAt + 86_400_000);
  take(await ledger.revalidate(first, input));
  await ledger.finish(first, "nonmutating");
  assert.throws(() => MaintenanceResources.run(first, () => {}), /maintenance_token_invalid/);
  f.time(f.now + 15 * 60_000);
  const second = take(await f.make().admit(input));
  assert.equal(second.ordinal, 2);
  assert.equal(second.originalCycleExpiry, first.originalCycleExpiry);
  take(await f.make().revalidate(second, input));
  await ledger.finish(second, "nonmutating");
  f.time(f.now + 180_000);
  const third = take(await ledger.admit(input));
  take(await ledger.revalidate(third, input));
  await ledger.finish(third, "nonmutating");
  const stored = (await table(f.root)).cycles[0];
  assert.equal(stored.reservedWork, 360_000);
  assert.equal(stored.reserved.rpcAttempts, 96);
  assert.equal(stored.reserved.decoded, 3 * input.config.decodedCap);
  refused(await ledger.admit(input), "maintenance_cycle_exhausted");
  f.time(first.originalCycleExpiry);
  refused(await f.make().admit(input), "maintenance_cycle_expired");
});

test("crash hooks distinguish uncharged admission from durable precharge without reviving a token", async t => {
  for (const phase of ["before", "after"] as const) {
    const f = await fixture(t);
    const failed = f.make({ prechargeHook: (p: string) => { if (p === phase) throw new Error("crash"); } });
    refused(await failed.admit(eligible()), "maintenance_state_invalid");
    f.time(f.now + 900_000);

    const token = take(await f.make().admit(eligible()));
    assert.equal(token.ordinal, phase === "before" ? 1 : 2);
    take(await f.make().revalidate(token, eligible()));
    await f.make().finish(token, "nonmutating");
  }
});

test("SIGTERM loses its permit; a fresh process gets slot two after fifteen virtual minutes", async t => {
  const f = await fixture(t);
  const script = `
    import { RecoveryMaintenance } from "./src/recovery-maintenance.ts";
    const ledger = new RecoveryMaintenance({ root: process.argv[1], workerToken: "test-worker-key", now: () => Number(process.argv[2]) });
    const result = await ledger.admit(JSON.parse(process.argv[3]));
    if (!result.ok) process.exit(2);
    if (!(await ledger.revalidate(result.value, JSON.parse(process.argv[3]))).ok) process.exit(3);
    process.kill(process.pid, "SIGTERM");
  `;
  await assert.rejects(promisify(execFile)(process.execPath,
    ["--import", "tsx", "--input-type=module", "--eval", script, f.root, String(f.now), JSON.stringify(eligible())],
    { timeout: 10_000 }), (error: unknown) => {
      assert.equal((error as { signal: string }).signal, "SIGTERM");
      return true;
    });
  const original = (await table(f.root)).cycles[0];
  f.time(f.now + 900_000);
  const token = take(await f.make().admit(eligible()));
  assert.equal(token.ordinal, 2);
  assert.equal(token.originalCycleExpiry, original.originalCycleExpiry);
  assert.equal(token.admittedAt, original.admittedAt);
  assert.deepEqual((await table(f.root)).cycles[0].slots[1].reserved, original.slots[0].reserved);
  refused(await f.make().beforeMutation(token, [sha("replacement-output")]), "maintenance_ineligible");
  refused(await f.make().revalidate({ ...token }, eligible()), "maintenance_token_invalid");
  await MaintenanceResources.run(token, () => MaintenanceBudget.runProducer(async () => {
    assert.equal((await table(f.root)).cycles[0].slots.length, 2);
    MaintenanceResources.consume("sourceBytes", 1);
    take(await f.make().revalidate(token, eligible()));
    take(await f.make().beforeMutation(token, [sha("replacement-output")]));
  }, { durationMs: MaintenanceResources.remainingMsIfActive(), signal: MaintenanceResources.signalIfActive() }));
  take(await f.make().finish(token, "nonmutating"));
  f.time(original.originalCycleExpiry);
  refused(await f.make().admit(eligible()), "maintenance_cycle_expired");
});

test("canonical-root instances serialize precharge and deny concurrent live slots", async t => {
  const f = await fixture(t);
  const results = await Promise.all([f.make().admit(eligible()), f.make().admit(eligible())]);
  assert.equal(results.filter(r => r.ok).length, 1);
  assert.equal(results.filter(r => !r.ok && r.reason === "maintenance_slot_active").length, 1);
  await f.make().finish(take(results.find(r => r.ok)!));
});

test("late admission is bounded by the original expiry rather than receiving a new full slot", async t => {
  // Both clocks are virtual: real fsync latency must not consume the 10 ms remainder.
  const f = await fixture(t), ledger = f.make({ monotonicNow: () => 0 }), first = take(await ledger.admit(eligible()));
  take(await ledger.revalidate(first, eligible()));
  take(await ledger.finish(first, "nonmutating"));
  f.time(first.originalCycleExpiry - 10);
  const late = take(await ledger.admit(eligible()));
  assert.equal(late.ordinal, 2);
  assert.equal(late.deadline, first.originalCycleExpiry);
  f.time(first.originalCycleExpiry);
  assert.throws(() => MaintenanceResources.run(late, () => {}), /maintenance_token_invalid/);
  refused(await ledger.admit(eligible()), "maintenance_cycle_expired");
});

test("spacing, clock rollback recovery, tighter bounds and invalid clocks", async t => {
  const f = await fixture(t), ledger = f.make(), input = eligible();
  const first = take(await ledger.admit(input, { deadline: f.now + 1000 }));
  take(await ledger.revalidate(first, input));
  await ledger.finish(first, "nonmutating");
  f.time(first.start - 1);
  refused(await ledger.admit(input), "maintenance_clock_invalid");
  f.time(first.start);
  refused(await ledger.admit(input), "maintenance_slot_spacing");
  f.time(first.start + 180_000);
  const second = take(await ledger.admit(input));
  assert.equal(second.ordinal, 2);
  take(await ledger.revalidate(second, input));
  await ledger.finish(second, "nonmutating");
  for (const n of [NaN, Infinity, -1, 1.5, Number.MAX_SAFE_INTEGER + 1]) {
    f.time(n); refused(await ledger.admit(input), "maintenance_clock_invalid");
  }
});

test("cap oscillation never renews an admission; indexed output cannot become a new original", async t => {
  const f = await fixture(t), ledger = f.make(), input = eligible();
  const first = take(await ledger.admit(input));
  const output = sha("output");
  take(await ledger.revalidate(first, input));
  take(await ledger.beforeMutation(first, [output]));
  await ledger.finish(first, "transient");
  f.time(f.now + 180_000);
  assert.equal((await table(f.root)).cycles[0].fenced, true);
  const wrongIdentity = structuredClone(input); wrongIdentity.identity.origin = sha("different-origin");
  refused(await ledger.admit(wrongIdentity), "maintenance_identity_invalid");
  const changedSourceConfig = structuredClone(input); changedSourceConfig.identity.sourceConfig = output;
  refused(await ledger.admit(changedSourceConfig), "maintenance_config_changed");
  const changed = structuredClone(input); changed.config.decodedCap *= 2;
  refused(await ledger.admit(changed), "maintenance_mutation_fenced");
  const replacement = structuredClone(input);
  replacement.identity.originalSource = output; replacement.source = output;
  refused(await ledger.admit(replacement), "maintenance_lineage_invalid");
  const continuation = { ...input, source: output };
  const next = take(await ledger.admit(continuation));
  assert.equal(next.ordinal, 2);
  assert.equal(next.admittedAt, first.admittedAt);
  await ledger.finish(next, "permanent");
  f.time(f.now + 180_000);
  refused(await ledger.admit(input), "maintenance_cycle_exhausted");
});

test("nested resource scopes share charges, reject forged/revoked tokens and precharge every counter", async t => {
  const f = await fixture(t), ledger = f.make(), token = take(await ledger.admit(eligible()));
  const limits = { decoded: 8 * 1024 ** 3, closureEntries: 65536, sourceBytes: 256 * 1024 ** 2,
    sourceEntries: 65536, artifactBytes: 256 * 1024 ** 2, metadataBytes: 256 * 1024 ** 2,
    adminEntries: 4096, sources: 128, launches: 512, rpcAttempts: 32 };
  assert.throws(() => MaintenanceResources.run({ ...token }, () => {}), /maintenance_token_invalid/);
  MaintenanceResources.run(token, () => {
    for (const [resource, limit] of Object.entries(limits)) {
      const key = resource as keyof typeof limits;
      MaintenanceResources.consume(key, limit - 1);
      MaintenanceResources.run({} as MaintenanceSlot, () => MaintenanceResources.consume(key, 1));
    }
    assert.throws(() => MaintenanceResources.consume("decoded", 1), /maintenance_resource_exhausted/);
    assert.throws(() => MaintenanceResources.consume("rpcAttempts", 0), /maintenance_token_invalid/);
  });
  await ledger.finish(token, "transient");
  assert.throws(() => MaintenanceResources.run(token, () => {}), /maintenance_token_invalid/);
});

test("forward observations on refusals survive restart and rollback recovers only at the high water mark", async t => {
  const f = await fixture(t), ledger = f.make(), input = eligible();
  const first = take(await ledger.admit(input));
  take(await ledger.revalidate(first, input));
  take(await ledger.finish(first, "nonmutating"));
  f.time(first.start + 100_000);
  refused(await ledger.admit(input), "maintenance_slot_spacing");
  assert.equal((await table(f.root)).cycles[0].lastObserved, f.now);
  f.time(first.start + 99_999);
  refused(await f.make().admit(input), "maintenance_clock_invalid");
  f.time(first.start + 100_000);
  refused(await f.make().admit(input), "maintenance_slot_spacing");
});

test("a fence survives timeout and continuity proof waits for joined work", async t => {
  const f = await fixture(t), ledger = f.make(), input = eligible();
  const token = take(await ledger.admit(input));
  take(await ledger.revalidate(token, input));
  take(await ledger.beforeMutation(token, [sha("output")]));
  f.time(token.start + 180_000);
  refused(await ledger.admit(input), "maintenance_slot_active");
  refused(await f.make().revalidate(token, input), "maintenance_token_invalid");
  take(await ledger.finish(token, "transient"));
  const next = take(await ledger.admit(input));
  assert.equal(next.ordinal, 2);
  assert.throws(() => MaintenanceResources.run(token, () => {}), /maintenance_token_invalid/);
  take(await ledger.finish(next));
});

test("an expired live process permit blocks its next slot until finish", async t => {
  const f = await fixture(t), ledger = f.make(), first = take(await ledger.admit(eligible()));
  f.time(f.now + 900_000);
  refused(await f.make().admit(eligible()), "maintenance_slot_active");
  take(await ledger.finish(first, "transient"));
  const second = take(await f.make().admit(eligible()));
  refused(await ledger.finish(first), "maintenance_token_invalid");
  assert.equal(second.ordinal, 2);
  assert.equal(second.originalCycleExpiry, first.originalCycleExpiry);
  assert.throws(() => MaintenanceResources.run(first, () => {}), /maintenance_token_invalid/);
  take(await ledger.finish(second));
});

test("slot deadlines and abort revoke work without refund", async t => {
  const f = await fixture(t), ledger = f.make(), cancel = new AbortController();
  const token = take(await ledger.admit(eligible(), { signal: cancel.signal }));
  cancel.abort();
  assert.throws(() => MaintenanceResources.run(token, () => {}), /maintenance_token_invalid/);
  f.time(f.now + 180_000);
  refused(await ledger.admit(eligible()), "maintenance_slot_active");
  refused(await ledger.revalidate(token, eligible()), "maintenance_token_invalid");
  take(await ledger.finish(token, "transient"));
  const second = take(await ledger.admit(eligible()));
  f.time(second.deadline);
  assert.throws(() => MaintenanceResources.run(second, () => {}), /maintenance_token_invalid/);
  assert.equal((await table(f.root)).cycles[0].slots.length, 2);
});

test("missing/partial/corrupt state, token rotation and unsafe paths retain evidence", async t => {
  for (const mutation of ["missing-marker", "missing-table", "garbage", "empty-root", "symlink", "rotation"]) {
    const f = await fixture(t), ledger = f.make();
    const token = take(await ledger.admit(eligible()));
    take(await ledger.revalidate(token, eligible()));
    await ledger.finish(token, "nonmutating");
    if (mutation === "missing-marker") await fs.unlink(path.join(f.root, "initialized.json"));
    if (mutation === "missing-table") await fs.unlink(path.join(f.root, "complete.json"));
    if (mutation === "garbage") await fs.writeFile(path.join(f.root, "complete.json"), "{}");
    if (mutation === "empty-root") {
      await fs.unlink(path.join(f.root, "complete.json")); await fs.unlink(path.join(f.root, "initialized.json"));
    }
    if (mutation === "symlink") {
      await fs.rename(path.join(f.root, "complete.json"), path.join(path.dirname(f.root), "original"));
      await fs.symlink("../original", path.join(f.root, "complete.json"));
    }
    const before = await fs.readdir(f.root);
    const reader = mutation === "rotation" ? f.make({ workerToken: "rotated" }) : ledger;
    refused(await reader.admit(eligible()), "maintenance_state_invalid");
    refused(await reader.admit(eligible()), "maintenance_state_invalid");
    assert.deepEqual(await fs.readdir(f.root), before);
  }
});

test("whole loss and coherent older restoration are explicitly indistinguishable", async t => {
  const f = await fixture(t), ledger = f.make(), first = take(await ledger.admit(eligible()));
  take(await ledger.revalidate(first, eligible()));
  await ledger.finish(first, "nonmutating");
  const old = await Promise.all(["complete.json", "initialized.json"].map(n => fs.readFile(path.join(f.root, n))));
  f.time(f.now + 180_000);
  const second = take(await ledger.admit(eligible()));
  take(await ledger.revalidate(second, eligible()));
  await ledger.finish(second, "nonmutating");
  await Promise.all(["complete.json", "initialized.json"].map((n, i) => fs.writeFile(path.join(f.root, n), old[i]!)));
  const restored = take(await ledger.admit(eligible()));
  assert.equal(restored.ordinal, 2);
  take(await ledger.revalidate(restored, eligible()));
  await ledger.finish(restored, "nonmutating");
  await fs.rm(f.root, { recursive: true });
  const reset = take(await ledger.admit(eligible()));
  assert.equal(reset.ordinal, 1);
  await ledger.finish(reset);
});

test("authenticated malformed timestamps, unknown fields and missing indexed records refuse", async t => {
  for (const mutation of ["time", "unknown", "missing", "identity", "duplicate"]) {
    const f = await fixture(t), ledger = f.make(), token = take(await ledger.admit(eligible()));
    take(await ledger.revalidate(token, eligible()));
    await ledger.finish(token, "nonmutating");
    const value = await table(f.root);
    if (mutation === "time") value.cycles[0].originalCycleExpiry++;
    if (mutation === "unknown") value.cycles[0].surprise = true;
    if (mutation === "missing") value.cycles = [];
    if (mutation === "identity") value.cycles[0].identity.generation++;
    if (mutation === "duplicate") value.cycles.push(value.cycles[0]);
    await reseal(f.root, value);
    refused(await ledger.admit(eligible()), "maintenance_state_invalid");
  }
});

test("bounded files, duplicate JSON keys, and nonempty first initialization fail closed", async t => {
  for (const mode of ["large", "duplicate", "nonempty"]) {
    const f = await fixture(t), ledger = f.make();
    if (mode === "nonempty") {
      await fs.mkdir(f.root); await fs.writeFile(path.join(f.root, "evidence"), "preserve");
    } else {
      const token = take(await ledger.admit(eligible()));
      take(await ledger.revalidate(token, eligible()));
      await ledger.finish(token, "nonmutating");
      if (mode === "large") await fs.truncate(path.join(f.root, "complete.json"), 16 * 1024 ** 2 + 1);
      else {
        const raw = await fs.readFile(path.join(f.root, "complete.json"), "utf8");
        await fs.writeFile(path.join(f.root, "complete.json"), raw.replace('"version":1', '"version":1,"version":1'));
      }
    }
    refused(await ledger.admit(eligible()), "maintenance_state_invalid");
  }
});

test("history capacity keeps exhausted cycles and outputs instead of evicting", async t => {
  const f = await fixture(t), ledger = f.make(), token = take(await ledger.admit(eligible()));
  const outputs = Array.from({ length: 4094 }, (_, i) => sha("output-" + i));
  take(await ledger.revalidate(token, eligible()));
  take(await ledger.beforeMutation(token, outputs));
  take(await ledger.revalidate(token, eligible()));
  await ledger.finish(token, "nonmutating");
  f.time(f.now + 180_000);
  const next = take(await ledger.admit(eligible()));
  take(await ledger.revalidate(next, eligible()));
  refused(await ledger.beforeMutation(next, [sha("one-too-many")]), "maintenance_table_capacity");
  take(await ledger.revalidate(next, eligible()));
  await ledger.finish(next, "nonmutating");
  const newInput = eligible(); newInput.identity.run = "another-run"; newInput.identity.hold = "another";
  const before = await Promise.all(["complete.json", "initialized.json"].map(n => fs.readFile(path.join(f.root, n), "utf8")));
  refused(await ledger.admit(newInput), "maintenance_table_capacity");
  assert.deepEqual(await Promise.all(["complete.json", "initialized.json"].map(n => fs.readFile(path.join(f.root, n), "utf8"))), before);
  assert.equal((await table(f.root)).cycles[0].outputs.length, 4094);
});

test("eligibility must be positive and missing current decoded cap is refused", async t => {
  const f = await fixture(t), input = eligible();
  refused(await f.make().admit({ ...input, terminalNonStopped: false } as unknown as MaintenanceEligibility), "maintenance_ineligible");
  refused(await f.make().admit({ ...input, config: { verifierVersion: 1 } } as MaintenanceEligibility), "maintenance_identity_invalid");
  assert.deepEqual(RecoveryMaintenance.configuration(), input.config);
  for (const cap of [0, NaN, 64 * 1024 ** 3 + 1]) {
    refused(await f.make().admit({ ...input, config: { ...input.config, decodedCap: cap } }), "maintenance_identity_invalid");
  }
});

test("changed hold binding cannot make the same canonical source an unseen lineage", async t => {
  const f = await fixture(t), ledger = f.make(), input = eligible();
  const first = take(await ledger.admit(input));
  take(await ledger.revalidate(first, input));
  take(await ledger.finish(first, "nonmutating"));
  const before = await fs.readFile(path.join(f.root, "complete.json"), "utf8");
  for (const field of ["hold", "repo", "custodian", "origin", "sourceConfig"] as const) {
    const changed = structuredClone(input);
    changed.identity[field] = field === "origin" || field === "sourceConfig" ? sha("changed") : "changed";
    refused(await ledger.admit(changed), field === "sourceConfig" ? "maintenance_config_changed" : "maintenance_identity_invalid");
  }
  assert.equal(await fs.readFile(path.join(f.root, "complete.json"), "utf8"), before);
});

test("legacy unknown prior cap receives one current-config cycle without inferring old cap", async t => {
  const f = await fixture(t), ledger = f.make(), input = eligible();
  input.identity.sourceConfig = sha("legacy-prior-cap-unknown");
  input.config = RecoveryMaintenance.configuration();
  const slot = take(await ledger.admit(input));
  assert.equal(slot.ordinal, 1);
  take(await ledger.finish(slot, "permanent"));
  refused(await ledger.admit(input), "maintenance_cycle_exhausted");
  assert.equal((await table(f.root)).cycles[0].config.decodedCap, 8 * 1024 ** 3);
});

test("unresolved expired configuration cannot use a new cap to escape its fence", async t => {
  const f = await fixture(t), ledger = f.make(), input = eligible();
  input.config.decodedCap = 4096;
  const slot = take(await ledger.admit(input));
  take(await ledger.finish(slot, "permanent"));
  f.time(slot.originalCycleExpiry + 1);
  const raised = structuredClone(input); raised.config.decodedCap = 8192;
  refused(await ledger.admit(raised), "maintenance_mutation_fenced");
  refused(await ledger.revalidate(slot, raised), "maintenance_token_invalid");
  refused(await ledger.admit(input), "maintenance_cycle_expired");
  assert.equal((await table(f.root)).cycles.length, 1);
});

test("raised caps retain every configuration and restored exhausted caps cannot renew", async t => {
  const f = await fixture(t), ledger = f.make(), small = eligible();
  small.config.decodedCap = 4096;
  const first = take(await ledger.admit(small));
  take(await ledger.revalidate(first, small));
  take(await ledger.beforeMutation(first, [sha("shared-output")]));
  take(await ledger.finish(first, "nonmutating"));
  // Three independently completed slots exhaust this configuration without uncertainty.
  for (let ordinal = 2; ordinal <= 3; ordinal++) {
    f.time(f.now + 180_000);
    const slot = take(await ledger.admit(small));
    take(await ledger.revalidate(slot, small));
    take(await ledger.finish(slot, "nonmutating"));
  }
  const raised = structuredClone(small);
  raised.config.decodedCap = 1048576;
  raised.source = sha("shared-output");
  const larger = take(await ledger.admit(raised));
  assert.equal(larger.ordinal, 1);
  take(await ledger.revalidate(larger, raised));
  take(await ledger.beforeMutation(larger, [sha("shared-output")]));
  take(await ledger.revalidate(larger, raised));
  take(await ledger.finish(larger, "nonmutating"));
  refused(await ledger.admit(small), "maintenance_cycle_exhausted");
  const lower = structuredClone(small); lower.config.decodedCap = 2048;
  refused(await ledger.admit(lower), "maintenance_config_changed");
  refused(await ledger.admit(small), "maintenance_cycle_exhausted");
  f.time(f.now + 180_000);
  const resumed = take(await ledger.admit(raised));
  assert.equal(resumed.ordinal, 2);
  assert.equal(resumed.admittedAt, larger.admittedAt);
  take(await ledger.revalidate(resumed, raised));
  take(await ledger.finish(resumed, "nonmutating"));
  const persisted = await table(f.root);
  assert.equal(persisted.cycles.length, 2);
  assert.deepEqual(persisted.cycles.map((c: any) => c.config.decodedCap), [4096, 1048576]);
  const marker = JSON.parse(await fs.readFile(path.join(f.root, "initialized.json"), "utf8")).value;
  assert.equal(marker.history.length, 4); // one lineage, two configurations, one output
  const minted = structuredClone(raised);
  minted.identity.hold = "other";
  minted.identity.originalSource = raised.source;
  refused(await ledger.admit(minted), "maintenance_lineage_invalid");
});

test("uncertain producer mutation cannot become an unseen source in the same custody", async t => {
  const f = await fixture(t), ledger = f.make(), input = eligible();
  const token = take(await ledger.admit(input));
  take(await ledger.finish(token, "transient"));
  const rewritten = structuredClone(input);
  rewritten.identity.originalSource = sha("unindexed-postmutation-tip");
  rewritten.source = rewritten.identity.originalSource;
  refused(await ledger.admit(rewritten), "maintenance_mutation_fenced");
  assert.equal((await table(f.root)).cycles.length, 1);
});

test("configuration changes cannot bypass the durable lineage clock watermark", async t => {
  const f = await fixture(t), ledger = f.make(), input = eligible();
  input.config.decodedCap = 4096;
  const slot = take(await ledger.admit(input));
  take(await ledger.revalidate(slot, input));
  take(await ledger.finish(slot, "nonmutating"));
  f.time(slot.start - 1);
  const raised = structuredClone(input); raised.config.decodedCap *= 2;
  refused(await ledger.admit(raised), "maintenance_clock_invalid");
  f.time(slot.start);
  const next = take(await ledger.admit(raised));
  assert.equal(next.ordinal, 1);
  take(await ledger.revalidate(next, raised));
  take(await ledger.finish(next, "nonmutating"));
});

test("expiry observation cannot be forgotten by a later clock rollback", async t => {
  const f = await fixture(t), ledger = f.make(), input = eligible();
  const slot = take(await ledger.admit(input));
  take(await ledger.revalidate(slot, input));
  take(await ledger.finish(slot, "nonmutating"));
  f.time(slot.originalCycleExpiry);
  refused(await ledger.admit(input), "maintenance_cycle_expired");
  f.time(slot.start + 900_000);
  refused(await ledger.admit(input), "maintenance_clock_invalid");
});

test("postcharge expiry observation stays durable across rollback and revalidation", async t => {
  const f = await fixture(t), input = eligible();
  let jump = true;
  const ledger = f.make({ prechargeHook: (phase: string) => {
    if (phase === "after" && jump) f.time(1_000_000 + 86_400_000);
  } });
  refused(await ledger.admit(input), "maintenance_token_invalid");
  const value = (await table(f.root)).cycles[0];
  assert.equal(value.lastObserved, value.originalCycleExpiry);
  assert.equal(value.slots.length, 1);
  jump = false;
  f.time(1_900_000);
  refused(await ledger.revalidate({} as MaintenanceSlot, input), "maintenance_token_invalid");
  refused(await ledger.admit(input), "maintenance_clock_invalid");
});

test("precharge rollback and elapsed monotonic work never mint a live permit", async t => {
  for (const cut of ["wall", "monotonic"] as const) {
    const f = await fixture(t);
    let monotonic = 100;
    const ledger = f.make({ monotonicNow: () => monotonic, prechargeHook: (phase: string) => {
      if (phase !== "after") return;
      if (cut === "wall") f.time(0);
      else monotonic += 120_000;
    } });
    refused(await ledger.admit(eligible()), cut === "wall" ? "maintenance_clock_invalid" : "maintenance_token_invalid");
    const charged = (await table(f.root)).cycles[0];
    assert.equal(charged.slots.length, 1);
    assert.equal(charged.reservedWork, 120_000);
    assert.equal(charged.fenced, true);
  }
});

test("deterministic initialization cuts preserve evidence and never restart initialization", async t => {
  for (const phase of ["mkdir", "table", "pair", "installed"]) {
    const f = await fixture(t);
    const ledger = f.make({ initializationHook: (p: string) => { if (p === phase) throw new Error("cut"); } });
    refused(await ledger.admit(eligible()), "maintenance_state_invalid");
    const stage = f.root + ".initializing";
    if (phase === "installed") {
      const token = take(await f.make().admit(eligible()));
      assert.equal(token.ordinal, 1);
      take(await f.make().revalidate(token, eligible()));
      take(await f.make().finish(token, "nonmutating"));
    } else {
      const names = await fs.readdir(stage);
      refused(await f.make().admit(eligible()), "maintenance_state_invalid");
      assert.deepEqual(await fs.readdir(stage), names);
      await assert.rejects(fs.lstat(f.root), { code: "ENOENT" });
    }
  }
  const f = await fixture(t), token = take(await f.make().admit(eligible()));
  take(await f.make().revalidate(token, eligible()));
  take(await f.make().finish(token, "nonmutating"));
  await fs.mkdir(f.root + ".initializing");
  refused(await f.make().admit(eligible()), "maintenance_state_invalid");
  await fs.rm(f.root, { recursive: true });
  refused(await f.make().admit(eligible()), "maintenance_state_invalid");
  assert.deepEqual(await fs.readdir(f.root + ".initializing"), []);
});

test("identity fields and digest sources reject final newlines while foo.git remains valid", async t => {
  const f = await fixture(t), ledger = f.make();
  for (const field of ["run", "hold", "repo", "custodian", "bareBasename", "origin", "originalSource", "sourceConfig"] as const) {
    const input = eligible(); input.identity[field] += "\n";
    refused(await ledger.admit(input), "maintenance_identity_invalid");
  }
  refused(await ledger.admit({ ...eligible(), source: sha("input") + "\n" }), "maintenance_identity_invalid");
  const input = eligible(); input.identity.bareBasename = "foo.git";
  const token = take(await ledger.admit(input));
  take(await ledger.revalidate(token, input));
  refused(await ledger.beforeMutation(token, [sha("out") + "\n"]), "maintenance_lineage_invalid");
  take(await ledger.revalidate(token, input));
  take(await ledger.finish(token, "nonmutating"));
});

test("bare basename roundtrips canonical literal spelling and UTF-8 byte boundaries", async t => {
  for (const name of ["repo.git", "github.com+org+repo.git", "forge.example%3A8443+org+repo.git",
    "é".repeat(125) + "a.git", "a".repeat(255), "e\u0301+Repo.git"]) {
    const f = await fixture(t), input = eligible();
    input.identity.bareBasename = name;
    const token = take(await f.make().admit(input));
    take(await f.make().revalidate(token, input));
    take(await f.make().finish(token, "nonmutating"));
    const before = await Promise.all(["complete.json", "initialized.json"].map(n => fs.readFile(path.join(f.root, n), "utf8")));
    assert.equal((await table(f.root)).cycles[0].identity.bareBasename, name);
    refused(await f.make().admit(input), "maintenance_slot_spacing");
    assert.deepEqual(await Promise.all(["complete.json", "initialized.json"].map(n => fs.readFile(path.join(f.root, n), "utf8"))), before);
  }
});

test("bare basename rejects unsafe components and malformed Unicode without admitting history", async t => {
  const f = await fixture(t);
  for (const name of ["", ".", "..", "a/b", "a\\b", "a".repeat(256), "é".repeat(126) + ".git",
    "\uD800", "\uDC00", "a\uD800b", ...Array.from({ length: 32 }, (_, n) => "a" + String.fromCharCode(n)),
    "a\u007f", "a\u0085", ...[0x061c, 0x200e, 0x200f, 0x202a, 0x202b, 0x202c, 0x202d, 0x202e,
      0x2066, 0x2067, 0x2068, 0x2069].map(n => "a" + String.fromCharCode(n))]) {
    const input = eligible(); input.identity.bareBasename = name;
    refused(await f.make().admit(input), "maintenance_identity_invalid");
  }
  assert.deepEqual((await table(f.root)).cycles, []);
});

test("bare basename malformed 256-byte authenticated history remains protected", async t => {
  const f = await fixture(t), input = eligible();
  const token = take(await f.make().admit(input));
  take(await f.make().revalidate(token, input));
  take(await f.make().finish(token, "nonmutating"));
  const value = await table(f.root);
  value.cycles[0].identity.bareBasename = "a".repeat(256);
  await reseal(f.root, value);
  const before = await storageSnapshot(f.root);
  refused(await f.make().admit(input), "maintenance_state_invalid");
  assert.deepEqual(await storageSnapshot(f.root), before);
});

test("optional instrumentation is inert outside scope and exhaustion latches until joined finish", async t => {
  MaintenanceResources.checkIfActive();
  MaintenanceResources.consumeIfActive("decoded", Number.MAX_SAFE_INTEGER);
  assert.throws(() => MaintenanceResources.check(), /maintenance_token_invalid/);
  assert.throws(() => MaintenanceResources.consume("decoded", 0), /maintenance_token_invalid/);
  const f = await fixture(t), ledger = f.make(), token = take(await ledger.admit(eligible()));
  assert.equal((await table(f.root)).cycles[0].fenced, true);
  MaintenanceResources.run(token, () => {
    assert.throws(() => MaintenanceResources.consumeIfActive("sources", 129), /maintenance_resource_exhausted/);
    assert.throws(() => MaintenanceResources.consumeIfActive("decoded", 1), /maintenance_token_invalid/);
    assert.throws(() => MaintenanceResources.checkIfActive(), /maintenance_token_invalid/);
  });
  f.time(f.now + 900_000);
  refused(await ledger.admit(eligible()), "maintenance_slot_active");
  take(await ledger.finish(token, "transient"));
  assert.equal((await table(f.root)).cycles[0].fenced, true);
  const next = take(await ledger.admit(eligible()));
  assert.equal(next.ordinal, 2);
  assert.equal(next.admittedAt, token.admittedAt);
  refused(await ledger.finish(token), "maintenance_token_invalid");
  refused(await ledger.admit(eligible()), "maintenance_slot_active");
  take(await ledger.revalidate(next, eligible()));
  take(await ledger.finish(next, "nonmutating"));
  assert.throws(() => MaintenanceResources.run(token, () => {}), /maintenance_token_invalid/);
});

test("returning from a producer leaves the charged fence until positive nonmutation finish", async t => {
  const f = await fixture(t), ledger = f.make(), token = take(await ledger.admit(eligible()));
  await MaintenanceResources.run(token, async () => {
    MaintenanceResources.consume("sourceBytes", 1);
    await Promise.resolve();
    MaintenanceResources.check();
  });
  assert.equal((await table(f.root)).cycles[0].fenced, true);
  take(await ledger.revalidate(token, eligible()));
  take(await ledger.beforeMutation(token, [sha("proposed")]));
  assert.deepEqual((await table(f.root)).cycles[0].outputs, [sha("proposed")]);
  take(await ledger.revalidate(token, eligible()));
  take(await ledger.finish(token, "nonmutating"));
  assert.equal((await table(f.root)).cycles[0].fenced, false);
  assert.equal((await table(f.root)).cycles[0].slots.length, 1);
});

test("permanent nonmutating decoded refusal exhausts one charged slot and permits a meaningful cap increase", async t => {
  const f = await fixture(t), ledger = f.make(), small = eligible();
  small.config.decodedCap = 8;
  const { quiescent: _q, sourceOutputContinuity: _c, ...admission } = small;
  const token = take(await ledger.admit(admission));
  const output = sha("proposed-without-source-effect");
  let sourceEffects = 0;
  await MaintenanceResources.run(token, async () => {
    // The caller supplies positive physical/source proof under the paid scope.
    MaintenanceResources.consume("sourceBytes", 1);
    take(await ledger.revalidate(token, small));
    // Proposed output indexing is durable bookkeeping, before any source effect.
    take(await ledger.beforeMutation(token, [output]));
    MaintenanceResources.consume("decoded", 8);
    assert.throws(() => MaintenanceResources.consume("decoded", 1), /maintenance_resource_exhausted/);
    assert.throws(() => MaintenanceResources.check(), /maintenance_token_invalid/);
  });
  const before = await table(f.root);
  take(await ledger.finish(token, "permanent_nonmutating")); // All work joined; caller positively knows no source effect.
  const settled = await table(f.root);
  assert.deepEqual(settled, { ...before, cycles: [{ ...before.cycles[0], exhausted: true, fenced: false }] });
  assert.equal(sourceEffects, 0);
  refused(await ledger.admit(small), "maintenance_cycle_exhausted");
  refused(await ledger.revalidate(token, small), "maintenance_token_invalid");
  refused(await ledger.finish(token, "permanent_nonmutating"), "maintenance_token_invalid");
  assert.throws(() => MaintenanceResources.run(token, () => sourceEffects++), /maintenance_token_invalid/);

  const raised = { ...small, config: { ...small.config, decodedCap: 16 }, source: output };
  const fresh = take(await f.make().admit(raised));
  assert.equal(fresh.ordinal, 1);
  const combined = await table(f.root);
  assert.deepEqual(combined.cycles[0], settled.cycles[0]);
  assert.equal(combined.cycles[1].reserved.decoded, 16);
  assert.equal(combined.cycles[1].reservedWork, 120_000);
  assert.equal(combined.cycles[1].originalCycleExpiry, fresh.originalCycleExpiry);
  assert.deepEqual(take(await ledger.resolveOutput(output, expectedBinding(small.identity))), small.identity);
  await MaintenanceResources.run(fresh, async () => {
    take(await ledger.revalidate(fresh, raised));
    MaintenanceResources.consume("decoded", 16);
  });
  take(await ledger.finish(fresh, "permanent_nonmutating"));
  refused(await ledger.admit(raised), "maintenance_cycle_exhausted");
  refused(await ledger.admit({ ...small, config: { ...small.config, decodedCap: 4 } }), "maintenance_config_changed");
  refused(await ledger.admit(small), "maintenance_cycle_exhausted");
  assert.deepEqual((await table(f.root)).cycles[0], settled.cycles[0]);
  const marker = JSON.parse(await fs.readFile(path.join(f.root, "initialized.json"), "utf8")).value;
  assert.equal(marker.history.length, 4); // original identity, two configs, retained proposed output
});

test("permanent nonmutating settlement after deadline or abort needs previously established positive proof", async t => {
  for (const mode of ["deadline", "abort"] as const) {
    const f = await fixture(t), ledger = f.make(), proof = eligible(), cancel = new AbortController();
    const token = take(await ledger.admit(proof, { signal: cancel.signal }));
    await MaintenanceResources.run(token, async () => {
      MaintenanceResources.consume("sourceBytes", 1);
      take(await ledger.revalidate(token, proof));
    });
    if (mode === "deadline") f.time(token.deadline);
    else cancel.abort();
    assert.throws(() => MaintenanceResources.run(token, () => {}), /maintenance_token_invalid/);
    const before = await table(f.root);
    take(await ledger.finish(token, "permanent_nonmutating"));
    assert.deepEqual(await table(f.root), { ...before, cycles: [{ ...before.cycles[0], exhausted: true, fenced: false }] });
    assert.throws(() => MaintenanceResources.run(token, () => {}), /maintenance_token_invalid/);
  }
});

test("unverified, failed and uncertain permanent outcomes retain the fence and all charged history", async t => {
  for (const mode of ["unverified", "failed", "uncertain", "expired-unverified"] as const) {
    const f = await fixture(t), ledger = f.make(), proof = eligible();
    proof.config.decodedCap = 8;
    const token = take(await ledger.admit(proof));
    if (mode === "failed" || mode === "uncertain") {
      take(await ledger.revalidate(token, proof));
      if (mode === "failed")
        refused(await ledger.revalidate(token, { ...proof, quiescent: false } as unknown as MaintenanceEligibility), "maintenance_ineligible");
      else take(await ledger.beforeMutation(token, [sha("uncertain-output")]));
    }
    if (mode === "expired-unverified") f.time(token.deadline);
    const before = await table(f.root);
    if (mode === "uncertain") take(await ledger.finish(token, "permanent"));
    else refused(await ledger.finish(token, "permanent_nonmutating"), "maintenance_ineligible");
    const after = await table(f.root);
    assert.deepEqual(after, mode === "uncertain"
      ? { ...before, cycles: [{ ...before.cycles[0], exhausted: true }] } : before);
    assert.equal(after.cycles[0].fenced, true);
    refused(await ledger.admit({ ...proof, config: { ...proof.config, decodedCap: 16 } }), "maintenance_mutation_fenced");
    refused(await ledger.revalidate(token, proof), "maintenance_token_invalid");
    assert.equal((await table(f.root)).cycles.length, 1);
  }
});

test("cheap admission never enables effects; missing and false complete proofs retain uncertainty", async t => {
  const f = await fixture(t), ledger = f.make(), proof = eligible();
  const { quiescent: _quiescent, sourceOutputContinuity: _continuity, ...cheap } = proof;
  const admission: MaintenanceAdmission = cheap;
  const token = take(await ledger.admit(admission));
  const before = await table(f.root);
  refused(await ledger.beforeMutation(token, [sha("unverified-output")]), "maintenance_ineligible");
  assert.deepEqual(await table(f.root), before);
  for (const field of ["quiescent", "sourceOutputContinuity", "exactCustody", "terminalNonStopped"] as const) {
    for (const value of [false, undefined]) {
      const bad = { ...proof, [field]: value } as unknown as MaintenanceEligibility;
      refused(await ledger.revalidate(token, bad), "maintenance_ineligible");
      refused(await ledger.beforeMutation(token, [sha("rejected-output")]), "maintenance_ineligible");
    }
  }
  refused(await ledger.finish(token, "nonmutating"), "maintenance_ineligible");
  refused(await ledger.finish(token, "nonmutating"), "maintenance_token_invalid");
  assert.equal((await table(f.root)).cycles[0].fenced, true);
  assert.equal((await table(f.root)).cycles[0].reservedWork, 120_000);
  f.time(f.now + 180_000);
  const next = take(await ledger.admit(admission)); // Failed settlement released the joined owner once.
  refused(await ledger.revalidate(token, proof), "maintenance_token_invalid");
  refused(await ledger.beforeMutation(next, [sha("old-flag")]), "maintenance_ineligible");
  take(await ledger.finish(next, "transient"));
});

test("verification requires exact active token, root, original identity, config and admitted source", async t => {
  const f = await fixture(t), ledger = f.make(), proof = eligible();
  const token = take(await ledger.admit(proof));
  refused(await ledger.revalidate({ ...token }, proof), "maintenance_token_invalid");
  const other = await fixture(t), foreign = other.make(), foreignToken = take(await foreign.admit(proof));
  refused(await foreign.revalidate(token, proof), "maintenance_token_invalid");
  refused(await ledger.revalidate(foreignToken, proof), "maintenance_token_invalid");
  for (const field of Object.keys(proof.identity) as Array<keyof MaintenanceIdentity>) {
    const changed = structuredClone(proof);
    if (field === "generation") changed.identity.generation++;
    else changed.identity[field] = ["origin", "originalSource", "sourceConfig"].includes(field) ? sha("foreign") : "foreign";
    refused(await ledger.revalidate(token, changed), "maintenance_identity_invalid");
  }
  refused(await ledger.revalidate(token, { ...proof, config: { ...proof.config, decodedCap: 4096 } }), "maintenance_config_changed");
  refused(await ledger.revalidate(token, { ...proof, source: sha("unindexed") }), "maintenance_lineage_invalid");
  take(await ledger.revalidate(token, proof));
  const output = sha("indexed-but-not-admitted");
  take(await ledger.beforeMutation(token, [output]));
  refused(await ledger.revalidate(token, { ...proof, source: output }), "maintenance_lineage_invalid");
  refused(await ledger.beforeMutation(token, [sha("failed-refresh")]), "maintenance_ineligible");
  assert.equal((await table(f.root)).cycles[0].fenced, true);
  take(await ledger.finish(token, "transient"));
  take(await foreign.finish(foreignToken, "transient"));
});

test("durable whole-slot charge precedes verification callback and shares its resource and time budget with effects", async t => {
  const f = await fixture(t), proof = eligible(), events: string[] = [];
  proof.config.decodedCap = 8;
  let monotonic = 10;
  const ledger = f.make({ monotonicNow: () => monotonic, prechargeHook: async (phase: string) => {
    if (phase === "after") {
      const cycle = (await table(f.root)).cycles[0];
      assert.equal(cycle.reservedWork, 120_000);
      assert.equal(cycle.slots.length, 1);
      assert.equal(cycle.fenced, true);
      events.push("durable-charge");
    }
  } });
  const { quiescent: _q, sourceOutputContinuity: _c, ...admission } = proof;
  const token = take(await ledger.admit(admission, { deadline: f.now + 90_000 }));
  const output = sha("paid-output");
  await MaintenanceResources.run(token, () => MaintenanceBudget.runProducer(async budget => {
    assert.equal(budget.remainingMs(), 90_000);
    const verify = async (): Promise<MaintenanceEligibility> => {
      events.push("verification-callback");
      assert.equal((await table(f.root)).cycles[0].reserved.decoded, 8);
      refused(await ledger.beforeMutation(token, [output]), "maintenance_ineligible");
      MaintenanceResources.consume("decoded", 3);
      monotonic += 40_000;
      assert.equal(budget.remainingMs(), 50_000);
      return proof;
    };
    take(await ledger.revalidate(token, await verify()));
    assert.equal((await table(f.root)).cycles[0].fenced, true); // Verification itself never clears it.
    take(await ledger.beforeMutation(token, [output]));
    events.push("mutation");
    MaintenanceResources.consume("decoded", 4);
    await MaintenanceResources.run(token, async () => {
      events.push("fallback");
      MaintenanceResources.consume("decoded", 1);
      assert.equal(budget.remainingMs(), 50_000);
    });
  }, { durationMs: MaintenanceResources.remainingMsIfActive(), signal: MaintenanceResources.signalIfActive(), now: () => monotonic }));
  assert.deepEqual(events, ["durable-charge", "verification-callback", "mutation", "fallback"]);
  MaintenanceResources.run(token, () => {
    assert.throws(() => MaintenanceResources.consume("decoded", 1), /maintenance_resource_exhausted/);
  });
  take(await ledger.finish(token, "transient"));
  assert.equal((await table(f.root)).cycles[0].reservedWork, 120_000);
});

test("invalid history and expired admissions never invoke a verification callback", async t => {
  for (const mode of ["missing", "corrupt", "ambiguous", "expired"] as const) {
    const f = await fixture(t), ledger = f.make(), proof = eligible();
    const first = take(await ledger.admit(proof));
    take(await ledger.finish(first, "transient"));
    f.time(first.originalCycleExpiry);
    const file = path.join(f.root, "complete.json");
    if (mode === "missing") await fs.unlink(file);
    if (mode === "corrupt") await fs.writeFile(file, "{");
    if (mode === "ambiguous") {
      const value = await table(f.root);
      value.cycles.push(structuredClone(value.cycles[0]));
      await reseal(f.root, value);
    }
    let callbacks = 0;
    const admitted = await ledger.admit(proof);
    if (admitted.ok) {
      await MaintenanceResources.run(admitted.value, async () => { callbacks++; });
      await ledger.finish(admitted.value, "transient");
    }
    refused(admitted, mode === "expired" ? "maintenance_cycle_expired" : "maintenance_state_invalid");
    assert.equal(callbacks, 0);
  }
});

test("revalidation never revives aborted, elapsed, backward or malformed-clock reservations", async t => {
  for (const mode of ["abort", "deadline", "monotonic", "backward", "nan", "infinity", "fraction"] as const) {
    const f = await fixture(t), cancel = new AbortController(), proof = eligible();
    let monotonic = 100;
    const ledger = f.make({ monotonicNow: () => monotonic });
    const token = take(await ledger.admit(proof, { signal: cancel.signal }));
    if (mode === "abort") cancel.abort();
    if (mode === "deadline") f.time(token.deadline);
    if (mode === "monotonic") monotonic += 120_000;
    if (mode === "backward") f.time(token.start - 1);
    if (mode === "nan") f.time(NaN);
    if (mode === "infinity") f.time(Infinity);
    if (mode === "fraction") f.time(token.start + 0.5);
    const reason = ["backward", "nan", "infinity", "fraction"].includes(mode) ? "maintenance_clock_invalid" : "maintenance_token_invalid";
    refused(await ledger.revalidate(token, proof), reason);
    // Restore clocks: once checked has revoked a slot, restoration must not revive it.
    f.time(token.start); monotonic = 100;
    refused(await ledger.revalidate(token, proof), "maintenance_token_invalid");
    take(await ledger.finish(token, "transient"));
    assert.equal((await table(f.root)).cycles[0].reservedWork, 120_000);
  }
});

test("revalidation refuses lost roots and never recreates history", async t => {
  const f = await fixture(t), ledger = f.make(), proof = eligible();
  const token = take(await ledger.admit(proof));
  await fs.rm(f.root, { recursive: true });
  refused(await ledger.revalidate(token, proof), "maintenance_state_invalid");
  refused(await ledger.beforeMutation(token, [sha("lost-root")]), "maintenance_state_invalid");
  refused(await ledger.finish(token, "nonmutating"), "maintenance_state_invalid");
  await assert.rejects(fs.lstat(f.root), { code: "ENOENT" });
});

test("an existing lower-cap cycle cannot bypass a fenced configuration sibling", async t => {
  const f = await fixture(t), ledger = f.make(), small = eligible();
  small.config.decodedCap = 4096;
  const first = take(await ledger.admit(small));
  take(await ledger.revalidate(first, small));
  take(await ledger.finish(first, "nonmutating"));
  const raised = { ...small, config: { ...small.config, decodedCap: 8192 } };
  const second = take(await ledger.admit(raised));
  take(await ledger.finish(second, "transient"));
  f.time(f.now + 180_000);
  refused(await ledger.admit(small), "maintenance_mutation_fenced");
  assert.deepEqual((await table(f.root)).cycles.map((c: any) => c.slots.length), [1, 1]);
  const retry = take(await ledger.admit(raised));
  assert.equal(retry.ordinal, 2);
  refused(await ledger.beforeMutation(retry, [sha("sibling-flag")]), "maintenance_ineligible");
  take(await ledger.finish(retry, "transient"));
});

test("unknown sources and changed original bindings are refused before any slot charge", async t => {
  const f = await fixture(t), proof = eligible();
  let charges = 0;
  const ledger = f.make({ prechargeHook: (phase: string) => { if (phase === "after") charges++; } });
  refused(await ledger.admit({ ...proof, source: sha("unknown-current-source") }), "maintenance_lineage_invalid");
  assert.equal(charges, 0);
  const token = take(await ledger.admit(proof));
  take(await ledger.finish(token, "transient"));
  f.time(f.now + 180_000);
  const changed = structuredClone(proof);
  changed.identity.custodian = "another-worker";
  refused(await ledger.admit(changed), "maintenance_identity_invalid");
  refused(await ledger.admit({ ...proof, source: sha("unindexed-postmutation-source") }), "maintenance_lineage_invalid");
  assert.equal(charges, 1);
  assert.equal((await table(f.root)).cycles[0].slots.length, 1);
});

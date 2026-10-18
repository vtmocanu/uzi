import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import path from "node:path";
import { createHmac, createHash } from "node:crypto";
import { execFile } from "node:child_process";
import { promisify } from "node:util";
import { RecoveryMaintenance, MaintenanceResources, MAINTENANCE_VERIFIER_VERSION } from "../src/recovery-maintenance.js";
import type { MaintenanceEligibility, MaintenanceResult, MaintenanceSlot } from "../src/recovery-maintenance.js";

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

test("durable slots survive interruption, reserve full totals, retain expiry and stop after three", async t => {
  const f = await fixture(t), input = eligible(), ledger = f.make();
  const first = take(await ledger.admit(input));
  assert.equal(first.deadline, f.now + 120_000);
  assert.equal(first.originalCycleExpiry, first.admittedAt + 86_400_000);
  await ledger.finish(first, "nonmutating");
  assert.throws(() => MaintenanceResources.run(first, () => {}), /maintenance_token_invalid/);
  f.time(f.now + 15 * 60_000);
  const second = take(await f.make().admit(input));
  assert.equal(second.ordinal, 2);
  assert.equal(second.originalCycleExpiry, first.originalCycleExpiry);
  await ledger.finish(second, "nonmutating");
  f.time(f.now + 180_000);
  const third = take(await ledger.admit(input));
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
    if (phase === "after") take(await f.make().revalidate(eligible()));
    const token = take(await f.make().admit(eligible()));
    assert.equal(token.ordinal, phase === "before" ? 1 : 2);
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
  take(await f.make().revalidate(eligible()));
  const token = take(await f.make().admit(eligible()));
  assert.equal(token.ordinal, 2);
  assert.equal(token.originalCycleExpiry, original.originalCycleExpiry);
  await f.make().finish(token, "nonmutating");
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
  const f = await fixture(t), ledger = f.make(), first = take(await ledger.admit(eligible()));
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
  await ledger.finish(first, "nonmutating");
  f.time(first.start - 1);
  refused(await ledger.admit(input), "maintenance_clock_invalid");
  f.time(first.start);
  refused(await ledger.admit(input), "maintenance_slot_spacing");
  f.time(first.start + 180_000);
  const second = take(await ledger.admit(input));
  assert.equal(second.ordinal, 2);
  await ledger.finish(second, "nonmutating");
  for (const n of [NaN, Infinity, -1, 1.5, Number.MAX_SAFE_INTEGER + 1]) {
    f.time(n); refused(await ledger.admit(input), "maintenance_clock_invalid");
  }
});

test("cap oscillation never renews an admission; indexed output cannot become a new original", async t => {
  const f = await fixture(t), ledger = f.make(), input = eligible();
  const first = take(await ledger.admit(input));
  const output = sha("output");
  take(await ledger.beforeMutation(first, [output]));
  await ledger.finish(first, "transient");
  f.time(f.now + 180_000);
  refused(await ledger.admit(input), "maintenance_mutation_fenced");
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
  take(await ledger.revalidate(continuation));
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
  await ledger.finish(token, "nonmutating");
  assert.throws(() => MaintenanceResources.run(token, () => {}), /maintenance_token_invalid/);
});

test("forward observations on refusals survive restart and rollback recovers only at the high water mark", async t => {
  const f = await fixture(t), ledger = f.make(), input = eligible();
  const first = take(await ledger.admit(input));
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
  take(await ledger.beforeMutation(token, [sha("output")]));
  f.time(token.start + 180_000);
  refused(await ledger.admit(input), "maintenance_slot_active");
  refused(await f.make().revalidate(input), "maintenance_slot_active");
  take(await ledger.finish(token, "transient"));
  take(await f.make().revalidate(input));
  const next = take(await ledger.admit(input));
  assert.equal(next.ordinal, 2);
  assert.throws(() => MaintenanceResources.run(token, () => {}), /maintenance_token_invalid/);
  take(await ledger.finish(next));
});

test("an expired live process permit blocks its next slot until finish", async t => {
  const f = await fixture(t), ledger = f.make(), first = take(await ledger.admit(eligible()));
  f.time(f.now + 900_000);
  refused(await f.make().admit(eligible()), "maintenance_slot_active");
  take(await ledger.finish(first, "nonmutating"));
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
  refused(await ledger.revalidate(eligible()), "maintenance_slot_active");
  take(await ledger.finish(token, "nonmutating"));
  const second = take(await ledger.admit(eligible()));
  f.time(second.deadline);
  assert.throws(() => MaintenanceResources.run(second, () => {}), /maintenance_token_invalid/);
  assert.equal((await table(f.root)).cycles[0].slots.length, 2);
});

test("missing/partial/corrupt state, token rotation and unsafe paths retain evidence", async t => {
  for (const mutation of ["missing-marker", "missing-table", "garbage", "empty-root", "symlink", "rotation"]) {
    const f = await fixture(t), ledger = f.make();
    const token = take(await ledger.admit(eligible()));
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
  await ledger.finish(first, "nonmutating");
  const old = await Promise.all(["complete.json", "initialized.json"].map(n => fs.readFile(path.join(f.root, n))));
  f.time(f.now + 180_000);
  const second = take(await ledger.admit(eligible())); await ledger.finish(second, "nonmutating");
  await Promise.all(["complete.json", "initialized.json"].map((n, i) => fs.writeFile(path.join(f.root, n), old[i]!)));
  const restored = take(await ledger.admit(eligible()));
  assert.equal(restored.ordinal, 2);
  await ledger.finish(restored, "nonmutating");
  await fs.rm(f.root, { recursive: true });
  const reset = take(await ledger.admit(eligible()));
  assert.equal(reset.ordinal, 1);
  await ledger.finish(reset);
});

test("authenticated malformed timestamps, unknown fields and missing indexed records refuse", async t => {
  for (const mutation of ["time", "unknown", "missing", "identity", "duplicate"]) {
    const f = await fixture(t), ledger = f.make(), token = take(await ledger.admit(eligible()));
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
      const token = take(await ledger.admit(eligible())); await ledger.finish(token, "nonmutating");
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
  take(await ledger.beforeMutation(token, outputs));
  await ledger.finish(token, "nonmutating");
  f.time(f.now + 180_000);
  take(await ledger.revalidate(eligible()));
  const next = take(await ledger.admit(eligible()));
  refused(await ledger.beforeMutation(next, [sha("one-too-many")]), "maintenance_table_capacity");
  await ledger.finish(next, "nonmutating");
  const newInput = eligible(); newInput.identity.hold = "another";
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

test("fresh continuity permits raised configuration after expiry without renewing old cycle", async t => {
  const f = await fixture(t), ledger = f.make(), input = eligible();
  input.config.decodedCap = 4096;
  const slot = take(await ledger.admit(input));
  take(await ledger.finish(slot, "permanent"));
  f.time(slot.originalCycleExpiry + 1);
  const raised = structuredClone(input); raised.config.decodedCap = 8192;
  take(await ledger.revalidate(raised));
  const newSlot = take(await ledger.admit(raised));
  assert.equal(newSlot.ordinal, 1);
  assert.equal(newSlot.admittedAt, f.now);
  refused(await ledger.admit(input), "maintenance_cycle_expired");
  const old = (await table(f.root)).cycles[0];
  assert.equal(old.originalCycleExpiry, slot.originalCycleExpiry);
  assert.equal(old.slots.length, 1);
  take(await ledger.finish(newSlot, "nonmutating"));
});

test("raised caps retain every configuration and restored exhausted caps cannot renew", async t => {
  const f = await fixture(t), ledger = f.make(), small = eligible();
  small.config.decodedCap = 4096;
  const first = take(await ledger.admit(small));
  take(await ledger.beforeMutation(first, [sha("shared-output")]));
  take(await ledger.finish(first, "permanent"));
  take(await ledger.revalidate(small));
  const raised = structuredClone(small);
  raised.config.decodedCap = 1048576;
  raised.source = sha("shared-output");
  const larger = take(await ledger.admit(raised));
  assert.equal(larger.ordinal, 1);
  take(await ledger.beforeMutation(larger, [sha("shared-output")]));
  take(await ledger.finish(larger, "nonmutating"));
  refused(await ledger.admit(small), "maintenance_cycle_exhausted");
  const lower = structuredClone(small); lower.config.decodedCap = 2048;
  const lowerToken = take(await ledger.admit(lower));
  take(await ledger.finish(lowerToken, "nonmutating"));
  refused(await ledger.admit(small), "maintenance_cycle_exhausted");
  f.time(f.now + 180_000);
  const resumed = take(await ledger.admit(raised));
  assert.equal(resumed.ordinal, 2);
  assert.equal(resumed.admittedAt, larger.admittedAt);
  take(await ledger.finish(resumed, "nonmutating"));
  const persisted = await table(f.root);
  assert.equal(persisted.cycles.length, 3);
  assert.deepEqual(persisted.cycles.map((c: any) => c.config.decodedCap), [4096, 1048576, 2048]);
  const marker = JSON.parse(await fs.readFile(path.join(f.root, "initialized.json"), "utf8")).value;
  assert.equal(marker.history.length, 5); // one lineage, three configurations, one output
  const minted = structuredClone(raised);
  minted.identity.hold = "other";
  minted.identity.originalSource = raised.source;
  refused(await ledger.admit(minted), "maintenance_lineage_invalid");
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
      take(await f.make().finish(token, "nonmutating"));
    } else {
      const names = await fs.readdir(stage);
      refused(await f.make().admit(eligible()), "maintenance_state_invalid");
      assert.deepEqual(await fs.readdir(stage), names);
      await assert.rejects(fs.lstat(f.root), { code: "ENOENT" });
    }
  }
  const f = await fixture(t), token = take(await f.make().admit(eligible()));
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
  refused(await ledger.beforeMutation(token, [sha("out") + "\n"]), "maintenance_lineage_invalid");
  take(await ledger.finish(token, "nonmutating"));
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
  refused(await ledger.admit(eligible()), "maintenance_mutation_fenced");
  take(await ledger.revalidate(eligible()));
  const next = take(await ledger.admit(eligible()));
  assert.equal(next.ordinal, 2);
  assert.equal(next.admittedAt, token.admittedAt);
  refused(await ledger.finish(token), "maintenance_token_invalid");
  refused(await ledger.admit(eligible()), "maintenance_slot_active");
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
  take(await ledger.beforeMutation(token, [sha("proposed")]));
  assert.deepEqual((await table(f.root)).cycles[0].outputs, [sha("proposed")]);
  take(await ledger.finish(token, "nonmutating"));
  assert.equal((await table(f.root)).cycles[0].fenced, false);
  assert.equal((await table(f.root)).cycles[0].slots.length, 1);
});

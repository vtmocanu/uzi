import { it, type TestContext } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import { type Dirent } from "node:fs";
import os from "node:os";
import path from "node:path";
import { Outbox, type OutboxOptions } from "../src/outbox.js";
import { nullLogger } from "./helpers.js";

const run = "11111111-1111-4111-8111-111111111111";
const canonical = "terminal-3.json", alias = "terminal-0003.json", filler = "terminal-1.json";

async function fixture(t: TestContext) {
  const root = await fs.mkdtemp(path.join(await fs.realpath(os.tmpdir()), "outbox-deferral-"));
  const boxes: Outbox[] = [];
  const make = (extra: Partial<OutboxOptions> = {}) => {
    const box = new Outbox({ root, log: nullLogger(), runMaxBytes: 1e6, maxBytes: 1e7, retentionMs: 1, ...extra });
    boxes.push(box);
    return box;
  };
  t.after(async () => {
    try { for (const box of boxes) await box.closeTerminalObservationScan(); }
    finally { await fs.rm(root, { recursive: true, force: true }); }
  });
  const writer = make();
  await writer.init();
  const install = async (generation: number, body: Record<string, unknown>) => {
    assert.equal((await writer.journalTerminal(run, generation, "running", 0, body)).journaled, true);
  };
  return { root, dir: path.join(root, run), make, writer, install };
}

/** Reorder only the finitely many real entries of this fixture, retaining real handles. */
async function ordered<T>(directory: string, names: string[], body: () => Promise<T>): Promise<T> {
  const original = fs.opendir;
  fs.opendir = (async (...args: Parameters<typeof fs.opendir>) => {
    const handle = await original(...args);
    if (String(args[0]) !== directory) return handle;
    const read = handle.read.bind(handle), entries: Dirent[] = [];
    try {
      for (let entry = await read(); entry; entry = await read()) entries.push(entry);
      assert.ok(entries.length <= 8, "the ordering hook is bounded to this small fixture");
      for (const name of names) assert.ok(entries.some(entry => entry.name === name), `missing ordering control ${name}`);
      const rank = (name: string) => names.includes(name) ? names.indexOf(name) : names.length;
      entries.sort((a, b) => rank(a.name) - rank(b.name));
    } catch (err) { await handle.close(); throw err; }
    let index = 0;
    handle.read = (async () => entries[index++] ?? null) as typeof handle.read;
    return handle;
  }) as typeof fs.opendir;
  try { return await body(); }
  finally { fs.opendir = original; }
}

it("deferral identity: an alias cannot lend eligibility or version to an oversized UNKNOWN canonical", async t => {
  const f = await fixture(t), file = path.join(f.dir, canonical), other = path.join(f.dir, alias);
  await f.install(3, { status: "failed", error: "older small outcome" });
  const small = await fs.readFile(file);
  assert.equal(await f.writer.retireTerminal(run, 3), true);
  await f.install(3, { status: "completed", report_md: "L".repeat(200000) });
  assert.ok((await fs.stat(file)).size > 100000, "a genuine authenticated oversized outcome is installed");
  await f.install(1, { status: "failed", error: "f".repeat(2500) });
  const fillerBytes = (await fs.stat(path.join(f.dir, filler))).size;
  await fs.link(file, other);
  const inode = (await fs.lstat(file)).ino;
  const reader = f.make({ terminalMaxBytes: 1024, runMaxBytes: fillerBytes + Math.floor(small.length / 2) });
  const original = fs.lstat;
  let oversizedSeen = 0, rewrites = 0;
  fs.lstat = (async (...args: Parameters<typeof fs.lstat>) => {
    if (String(args[0]) === other && rewrites === 0) {
      assert.ok(oversizedSeen > 0, "the canonical was observed before the alias");
      rewrites++;
      await fs.writeFile(file, small); // Same inode, different authenticated outcome/version.
    }
    const stat = await original(...args);
    if (String(args[0]) === file && stat.size > 100000) oversizedSeen++;
    return stat;
  }) as typeof fs.lstat;
  try { await ordered(f.dir, [filler, canonical, alias], () => reader.init()); }
  finally { fs.lstat = original; }
  assert.equal(rewrites, 1);
  assert.equal((await fs.lstat(file)).ino, inode);
  assert.equal(reader.listPendingTerminals().find(p => p.claim_generation === 3)?.outcome_kind, "unknown");
  assert.equal(await reader.readTerminalJournalForRetirement(run, 3), undefined,
    "an alias must not turn the uninspected canonical into an ordinary terminal send source");
  assert.equal(reader.listPendingTerminals().find(p => p.claim_generation === 3)?.outcome_kind, "unknown");
  assert.equal(await reader.hasPendingPolicyOutcome(run), true);
});

async function deferredCanonical(t: TestContext) {
  const f = await fixture(t);
  await f.install(1, { status: "failed", error: "f".repeat(2500) });
  await f.install(3, { status: "failed", error: "original" });
  const file = path.join(f.dir, canonical), small = await fs.readFile(file);
  const fillerBytes = (await fs.stat(path.join(f.dir, filler))).size;
  const reader = f.make({ runMaxBytes: fillerBytes + Math.floor(small.length / 2) });
  return { ...f, file, small, reader };
}

it("deferral identity: an oversized alias cannot invalidate the canonical's legitimate boot deferral", async t => {
  const f = await deferredCanonical(t);
  await fs.writeFile(path.join(f.dir, alias), "X".repeat(200000));
  await ordered(f.dir, [filler, canonical, alias], () => f.reader.init());
  assert.equal(f.reader.listPendingTerminals().find(p => p.claim_generation === 3)?.outcome_kind, "unknown");
  const captured = await f.reader.readTerminalJournalForRetirement(run, 3);
  assert.equal(captured?.journal.body.error, "original");
  assert.ok(captured?.context.expectedIdentity, "the canonical's own unchanged bytes supply readback authority");
});

for (const scanBadAlias of [false, true])
it(`deferral identity: an unchanged deferred canonical remains readable (scan bad alias=${scanBadAlias})`, async t => {
  const f = await deferredCanonical(t);
  await ordered(f.dir, [filler, canonical], () => f.reader.init());
  assert.equal(f.reader.listPendingTerminals().find(p => p.claim_generation === 3)?.outcome_kind, "unknown");
  if (scanBadAlias) {
    const rejected = JSON.parse(f.small.toString());
    rejected.body = { status: "failed", error: "tampered" };
    await fs.writeFile(path.join(f.dir, alias), JSON.stringify(rejected));
    await ordered(f.dir, [alias, canonical, filler], async () => {
      for (let page = 0; page < 20; page++) {
        if ((await f.reader.scanTerminalObservationsPage()).passComplete) return;
      }
      assert.fail("the finite fixture must complete within twenty bounded pages");
    });
  }
  const captured = await f.reader.readTerminalJournalForRetirement(run, 3);
  assert.equal(captured?.journal.body.error, "original");
  assert.ok(captured?.context.expectedIdentity);
});

type RuntimeScan = "global" | "target" | "page";
async function runtimeScan(box: Outbox, mode: RuntimeScan): Promise<void> {
  if (mode === "global") { await box.hasPendingPolicyOutcome(); return; }
  if (mode === "target") { await box.hasPendingPolicyOutcome(run); return; }
  for (let page = 0; page < 20; page++) if ((await box.scanTerminalObservationsPage()).passComplete) return;
  assert.fail("runtime fixture must finish within twenty bounded pages");
}

for (const mode of ["global", "target", "page"] as const) {
  for (const observation of ["oversized", "unreadable"] as const)
  it(`deferral identity: ${mode} runtime ${observation} evidence cannot authenticate later rewritten bytes`, async t => {
    const f = await fixture(t);
    await f.install(9, { status: "failed", error: "anchor" });
    const reader = f.make({ terminalMaxBytes: 1024, runMaxBytes: 100000 });
    await reader.init();
    const file = path.join(f.dir, canonical);
    await f.install(3, { status: "failed", error: "older small outcome" });
    const small = await fs.readFile(file);
    assert.equal(await f.writer.retireTerminal(run, 3), true);
    await f.install(3, { status: "completed", report_md: "L".repeat(200000) });
    assert.ok((await fs.stat(file)).size > 100000);
    const inode = (await fs.lstat(file)).ino;
    const lstat = fs.lstat;
    let rejectedStats = 0;
    if (observation === "unreadable") fs.lstat = (async (...args: Parameters<typeof fs.lstat>) => {
      if (String(args[0]) === file) {
        rejectedStats++;
        throw Object.assign(new Error("runtime fixture cannot observe the file version"), { code: "EIO" });
      }
      return lstat(...args);
    }) as typeof fs.lstat;
    try { await runtimeScan(reader, mode); }
    finally { fs.lstat = lstat; }
    if (observation === "unreadable") assert.ok(rejectedStats > 0, "the actual stat path must exercise EIO");
    assert.equal(reader.listPendingTerminals().find(p => p.claim_generation === 3)?.outcome_kind, "unknown");
    await fs.writeFile(file, small);
    assert.equal((await fs.lstat(file)).ino, inode);
    assert.equal(await reader.readTerminalJournalForRetirement(run, 3), undefined,
      "a retained UNKNOWN must not treat an unbound version as future authentication authority");
    assert.equal(reader.listPendingTerminals().find(p => p.claim_generation === 3)?.outcome_kind, "unknown");
    assert.equal(await reader.hasPendingPolicyOutcome(run), true);
  });

  it(`deferral identity: ${mode} runtime first-sight authentication of valid unchanged bytes still works`, async t => {
    const f = await fixture(t);
    await f.install(9, { status: "failed", error: "anchor" });
    const reader = f.make({ terminalMaxBytes: 1024, runMaxBytes: 100000 });
    await reader.init();
    await f.install(3, { status: "failed", error: "original" });
    await runtimeScan(reader, mode);
    const captured = await reader.readTerminalJournalForRetirement(run, 3);
    assert.equal(captured?.journal.body.error, "original");
    assert.ok(captured?.context.expectedIdentity);
  });
}

it("deferral identity: a later page cannot refresh the version of an already deferred first writer", async t => {
  const f = await fixture(t);
  await f.install(9, { status: "failed", error: "anchor" });
  const file = path.join(f.dir, canonical);
  await f.install(3, { status: "failed", error: "older small outcome" });
  const older = await fs.readFile(file);
  assert.equal(await f.writer.retireTerminal(run, 3), true);
  // Create the reader before the current first writer appears, but serve a
  // per-run budget that will defer that writer after reading the filler.
  await f.install(3, { status: "completed", report_md: "N".repeat(400) });
  const current = await fs.readFile(file);
  assert.equal(await f.writer.retireTerminal(run, 3), true);
  await f.install(1, { status: "failed", error: "f".repeat(2500) });
  const fillerBytes = (await fs.stat(path.join(f.dir, filler))).size;
  // Temporarily move the filler so it, too, first appears after reader.init().
  const savedFiller = path.join(f.root, "saved-filler");
  await fs.rename(path.join(f.dir, filler), savedFiller);
  const reader = f.make({ runMaxBytes: fillerBytes + Math.floor(current.length / 2) });
  await reader.init();
  await fs.rename(savedFiller, path.join(f.dir, filler));
  await fs.writeFile(file, current);
  await ordered(f.dir, [filler, canonical, "terminal-9.json"], async () => {
    const page = await reader.scanTerminalObservationsPage();
    assert.equal(page.passComplete, false, "the first bounded page defers the canonical");
    assert.equal(reader.listPendingTerminals().find(p => p.claim_generation === 3)?.outcome_kind, "unknown");
    const inode = (await fs.lstat(file)).ino;
    await fs.writeFile(file, older);
    assert.equal((await fs.lstat(file)).ino, inode);
    await runtimeScan(reader, "page");
  });
  assert.equal(await reader.readTerminalJournalForRetirement(run, 3), undefined);
  assert.equal(reader.listPendingTerminals().find(p => p.claim_generation === 3)?.outcome_kind, "unknown");
});

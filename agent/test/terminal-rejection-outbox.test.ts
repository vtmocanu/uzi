import { test, afterEach } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import path from "node:path";
import { Outbox, OUTBOX_RANGE_RESERVE_BYTES } from "../src/outbox.js";
import type { TerminalRejectionCustodyResponse } from "../src/protocol.js";
import { recordingLogger } from "./helpers.js";
import os from "node:os";
import { realpathSync, type Dirent } from "node:fs";

const scratch = realpathSync(os.tmpdir());
const run = "11111111-1111-4111-8111-111111111111";
const worker = "22222222-2222-4222-8222-222222222222";
const hold = "33333333-3333-4333-8333-333333333333";
const roots: string[] = [];
const outboxes: Outbox[] = [];
afterEach(async () => {
  for (const box of outboxes.splice(0)) await box.closeTerminalObservationScan();
  for (const root of roots.splice(0)) await fs.rm(root, { recursive: true, force: true });
});
async function fixture(terminalMaxBytes = 1024) {
  const root = await fs.mkdtemp(path.join(scratch, "terminal-outbox-"));
  roots.push(root);
  const log = recordingLogger();
  const box = new Outbox({ root, log: log.logger, runMaxBytes: 1e6, maxBytes: 1e7, retentionMs: 1, terminalMaxBytes });
  outboxes.push(box);
  await box.init();
  await box.journalTerminal(run, 3, "implement", 0, { status: "failed", error: "private body" });
  const file = path.join(root, run, "terminal-3.json");
  return { root, box, file, log };
}
test("unit 1: an absent selected winner is reinstalled, not adopted", async () => {
  const { box, file } = await fixture();
  await fs.unlink(file);
  assert.deepEqual(await box.journalTerminal(run, 3, "implement", 9, { status: "completed" }), { journaled: true, adopted: false });
  assert.equal((await box.readTerminalJournal(run, 3))?.messagesThroughSeq, 9);
});

test("unit 1: unreadable and key-unavailable selected winners defer without adoption", async () => {
  const { box, file } = await fixture();
  const original = await fs.readFile(file);
  await fs.unlink(file);
  await fs.mkdir(file);
  assert.deepEqual(await box.journalTerminal(run, 3, "implement", 9, { status: "completed" }), { journaled: false, reason: "winner_unavailable" });
  assert.equal(box.hasPendingTerminal(run, 3), true);
  await fs.rmdir(file);
  await fs.writeFile(file, original);
  const keyed = box as unknown as { key: Buffer | undefined };
  const key = keyed.key;
  try {
    keyed.key = undefined;
    assert.deepEqual(await box.journalTerminal(run, 3, "implement", 9, { status: "completed" }), { journaled: false, reason: "winner_unavailable" });
  } finally {
    keyed.key = key;
  }
  assert.deepEqual(await fs.readFile(file), original);
});

for (const kind of ["malformed", "oversized", "symlink"] as const) {
  test(`unit 1: selected ${kind} invalidates metadata and retains the invalid file`, async () => {
    const { box, file, root } = await fixture();
    await fs.unlink(file);
    if (kind === "symlink") await fs.symlink(path.join(root, ".key"), file);
    else await fs.writeFile(file, kind === "malformed" ? "{" : " ".repeat(1024 + OUTBOX_RANGE_RESERVE_BYTES + 1));
    const before = await fs.lstat(file);
    assert.deepEqual(await box.journalTerminal(run, 3, "implement", 9, { status: "completed" }), { journaled: false, reason: "reserve_exhausted" });
    assert.equal(box.hasPendingTerminal(run, 3), false);
    assert.equal((await fs.lstat(file)).ino, before.ino);
    if (kind !== "symlink") assert.equal(await fs.readFile(file, "utf8"), kind === "malformed" ? "{" : " ".repeat(1024 + OUTBOX_RANGE_RESERVE_BYTES + 1));
  });
}

async function reject(file: string) {
  const obj = JSON.parse(await fs.readFile(file, "utf8"));
  obj.body = { status: "failed", error: "changed private body" };
  await fs.writeFile(file, JSON.stringify(obj));
}
function settled(): TerminalRejectionCustodyResponse {
  return { run_id: run, worker_id: worker, generation: 3, exact_holds: [{ id: hold, state: "released" }],
    sibling_holds: [], exact_count: 1, sibling_count: 0, exact_complete: true, sibling_complete: true, complete: true, outcome: "settled" };
}

test("real computed MAC failure is content free, not trusted or adopted; ACK/stale/age/run retirement retain it", async () => {
  const { box, file, root, log } = await fixture();
  assert.equal((await box.observeTerminalAuthentication(run, 3)).kind, "authenticated");
  await reject(file);
  const restarted = new Outbox({ root, log: log.logger, runMaxBytes: 1e6, maxBytes: 1e7, retentionMs: 1 });
  outboxes.push(restarted);
  await restarted.init();
  assert.deepEqual(restarted.listPendingTerminals(), []);
  assert.equal(await restarted.readTerminalJournal(run, 3), undefined);
  assert.equal((await restarted.observeTerminalAuthentication(run, 3)).kind, "mac_failure");
  assert.equal((await restarted.journalTerminal(run, 3, "implement", 0, { status: "completed" })).journaled, false);
  await restarted.retireTerminal(run, 3);
  await restarted.staleRetireTerminal(run, 3);
  await restarted.retireRun(run);
  await box.readTerminalJournal(run, 3);
  await box.sweepRetention(Date.now() + 100000);
  await restarted.sweepRetention(Date.now() + 100000);
  assert.ok(await fs.stat(file));
  const warnings = log.lines.filter((v: any) => v.fact === "mac_failure");
  assert.ok(warnings.length);
  for (const v of warnings as Record<string, unknown>[]) {
    assert.deepEqual(Object.keys(v).sort(), ["claim_generation", "fact", "level", "msg", "run_id"]);
    assert.equal(v.run_id, run);
    assert.equal(v.claim_generation, 3);
  }
  assert.equal(JSON.stringify(warnings).includes("private body"), false);
});

test("unknown kinds are retained and never confused with computed rejection", async () => {
  const { box, file, root } = await fixture(16);
  await fs.unlink(file);
  assert.equal((await box.observeTerminalAuthentication(run, 3)).kind, "absent");
  await fs.symlink(path.join(root, ".key"), file);
  assert.equal((await box.observeTerminalAuthentication(run, 3)).kind, "symlink");
  await fs.unlink(file);
  await fs.mkdir(file);
  assert.equal((await box.observeTerminalAuthentication(run, 3)).kind, "unreadable");
  await fs.rmdir(file);
  await fs.writeFile(file, "{");
  assert.equal((await box.observeTerminalAuthentication(run, 3)).kind, "malformed");
  await fs.writeFile(file, JSON.stringify({ mac: "not a MAC" }));
  assert.equal((await box.observeTerminalAuthentication(run, 3)).kind, "malformed");
  await fs.writeFile(file, " ".repeat(16 + OUTBOX_RANGE_RESERVE_BYTES + 1));
  assert.equal((await box.observeTerminalAuthentication(run, 3)).kind, "oversized");
  await fs.writeFile(file, "{}");
  const uninitialized = new Outbox({ root, log: recordingLogger().logger, runMaxBytes: 1e6, maxBytes: 1e7, retentionMs: 1 });
  assert.equal((await uninitialized.observeTerminalAuthentication(run, 3)).kind, "key_unavailable");
  assert.equal(await box.hasPhysicalTerminalProtection(run), true);
  await box.retireRun(run);
  assert.ok(await fs.stat(file));
  const escaped = await box.observeTerminalAuthentication("../private", 3);
  assert.equal(escaped.kind, "malformed");
});

test("unsafe generations stay unknown, leading-zero aliases preserve exact names; cursor advances bounded pages and repeats", async () => {
  const { box, file, root } = await fixture();
  await reject(file);
  await fs.copyFile(file, path.join(root, run, "terminal-0003.json"));
  await fs.writeFile(path.join(root, run, "terminal-9007199254740993.json"), "{}");
  for (let n = 10; n < 310; n++) await fs.writeFile(path.join(root, run, `terminal-${n}.json`), "{}");
  const pages = [];
  do {
    pages.push(await box.scanTerminalObservationsPage());
    assert.ok(pages.at(-1)!.observations.length <= 256);
    assert.ok(pages.length < 10);
  } while (!pages.at(-1)!.passComplete);
  assert.ok(pages.length >= 2);
  const all = pages.flatMap(p => p.observations);
  assert.equal(all.length, 303);
  assert.equal(all.find(o => o.fileName === "terminal-0003.json")!.generation, 3);
  const unsafe = all.find(o => o.fileName === "terminal-9007199254740993.json")!;
  assert.equal(unsafe.generation, undefined);
  assert.equal(unsafe.kind, "malformed");
  assert.ok((await box.scanTerminalObservationsPage()).observations.length > 0);
  await box.retireRun(run);
  assert.ok(await fs.stat(path.join(root, run, unsafe.fileName)));
});

test("exact cleanup requires fresh complete settled custody and unchanged dev/inode/content; removes aliases only", async () => {
  const { box, file, root } = await fixture();
  await reject(file);
  const alias = path.join(root, run, "terminal-003.json");
  await fs.copyFile(file, alias);
  const observations = [await box.observeTerminalAuthentication(run, 3), await box.observeTerminalAuthentication(run, 3, "terminal-003.json")];
  for (const custody of [undefined, { ...settled(), complete: false }, { ...settled(), exact_count: 0, exact_holds: [] }, { ...settled(), outcome: "retained" as const },
    { ...settled(), sibling_count: 1, sibling_holds: [{ id: worker, generation: 4 }] }]) {
    assert.equal(await box.cleanupRejectedTerminalFiles(run, 3, worker, observations, async () => custody), 0);
    assert.ok(await fs.stat(file));
  }
  await fs.writeFile(file, (await fs.readFile(file, "utf8")) + " ");
  assert.equal(await box.cleanupRejectedTerminalFiles(run, 3, worker, observations, async () => settled()), 0);
  const refreshed = [await box.observeTerminalAuthentication(run, 3), observations[1]!];
  await fs.writeFile(path.join(root, run, "terminal-9007199254740993.json"), "{}");
  let called = 0;
  assert.equal(await box.cleanupRejectedTerminalFiles(run, 3, worker, refreshed, async () => { called++; return settled(); }), 2);
  assert.equal(called, 1);
  await assert.rejects(fs.stat(file), { code: "ENOENT" });
  await assert.rejects(fs.stat(alias), { code: "ENOENT" });
  assert.ok(await fs.stat(path.join(root, run, "terminal-9007199254740993.json")));
});

test("cleanup reconfirms after authority, retaining a changed inode or lost key", async () => {
  const { box, file, root } = await fixture();
  await reject(file);
  const observed = await box.observeTerminalAuthentication(run, 3);
  assert.equal(await box.cleanupRejectedTerminalFiles(run, 3, worker, [observed], async () => {
    const content = await fs.readFile(file);
    await fs.rename(file, path.join(root, run, "old"));
    await fs.writeFile(file, content);
    return settled();
  }), 0);
  assert.ok(await fs.stat(file));
  const noKey = new Outbox({ root, log: recordingLogger().logger, runMaxBytes: 1e6, maxBytes: 1e7, retentionMs: 1 });
  assert.equal(await noKey.cleanupRejectedTerminalFiles(run, 3, worker, [await box.observeTerminalAuthentication(run, 3)], async () => settled()), 0);
});

test("uppercase physical UUID directories report normalized identities and clean up exact aliases", async () => {
  const { box, root, log } = await fixture();
  const lower = "abcdefab-1111-4111-8111-111111111111";
  const upper = lower.toUpperCase();
  await box.journalTerminal(upper, 3, "implement", 0, { status: "failed" });
  const file = path.join(root, upper, "terminal-3.json");
  await reject(file);
  const pages = [];
  do {
    pages.push(await box.scanTerminalObservationsPage());
    assert.ok(pages.length < 10);
  } while (!pages.at(-1)!.passComplete);
  const observed = pages.flatMap(p => p.observations).find(o => o.physicalRunId === upper)!;
  assert.equal(observed.runId, lower);
  assert.equal(observed.kind, "mac_failure");
  assert.ok(log.lines.some((line: any) => line.fact === "mac_failure" && line.run_id === lower));
  assert.equal(await box.cleanupRejectedTerminalFiles(lower, 3, worker, [observed], async () => ({ ...settled(), run_id: lower })), 1);
  await assert.rejects(fs.stat(file), { code: "ENOENT" });
});

test("generic bare run names retain message and journal contracts without becoming report identities", async () => {
  const { box, root } = await fixture();
  assert.equal((await box.journalTerminal("fixture-run", 3, "implement", 0, { status: "failed" })).journaled, true);
  assert.ok(await box.readTerminalJournal("fixture-run", 3));
  assert.equal((await box.observeTerminalAuthentication("fixture-run", 3)).kind, "malformed");
  await reject(path.join(root, "fixture-run", "terminal-3.json"));
  await box.retireTerminal("fixture-run", 3);
  await box.retireRun("fixture-run");
  assert.ok(await fs.stat(path.join(root, "fixture-run", "terminal-3.json")));
});

test("absent physical directories allow ordinary cleanup while UUID case aliases remain protected", async () => {
  const { box, root } = await fixture();
  const absent = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa";
  assert.equal(await box.hasPhysicalTerminalProtection(absent), false);
  await fs.mkdir(path.join(root, absent.toUpperCase()));
  await fs.copyFile(path.join(root, run, "terminal-3.json"), path.join(root, absent.toUpperCase(), "terminal-3.json"));
  assert.equal(await box.hasPhysicalTerminalProtection(absent), true);
  await fs.rm(root, { recursive: true, force: true });
  assert.equal(await box.hasPhysicalTerminalProtection(run), false);
});

test("an authenticated alias wins before a competing canonical outcome is installed", async () => {
  const { root, file, box, log } = await fixture();
  const alias = path.join(root, run, "terminal-0003.json");
  await fs.rename(file, alias);
  const restarted = new Outbox({ root, log: log.logger, runMaxBytes: 1e6, maxBytes: 1e7, retentionMs: 1 });
  outboxes.push(restarted);
  await restarted.init();
  assert.deepEqual(await restarted.journalTerminal(run, 3, "implement", 0, { status: "completed" }), { journaled: true, adopted: true });
  assert.equal((await restarted.readTerminalJournal(run, 3))!.body.status, "failed");
  await assert.rejects(fs.stat(file), { code: "ENOENT" });
  await restarted.retireTerminal(run, 3);
  await assert.rejects(fs.stat(alias), { code: "ENOENT" });
  const again = new Outbox({ root, log: log.logger, runMaxBytes: 1e6, maxBytes: 1e7, retentionMs: 1 });
  outboxes.push(again);
  await again.init();
  assert.deepEqual(again.listPendingTerminals(), []);
  // Original-process metadata preserves the selected winner without claiming adoption during a transient read.
  await fs.mkdir(file);
  assert.deepEqual(await box.journalTerminal(run, 3, "implement", 0, { status: "completed" }), { journaled: false, reason: "winner_unavailable" });
  assert.equal(box.hasPendingTerminal(run, 3), true);
  await fs.rmdir(file);
});

test("accepted boundary journals stay readable across every blocked-state update and restart", async () => {
  const { root, box, log } = await fixture(1024);
  const cap = 1024 + OUTBOX_RANGE_RESERVE_BYTES;
  const reasons = ["gap_unrecoverable", "reserve_exhausted", "completion_permit_mismatch"] as const;
  // Search the admission boundary with distinct generations: an installed winner is immutable.
  let low = cap - 1024;
  let high = cap;
  let generation = 10;
  let accepted = 0;
  while (low <= high) {
    const length = Math.floor((low + high) / 2);
    const gen = generation++;
    const result = await box.journalTerminal(run, gen, "implement", 0, { status: "failed", failure_reason: "x".repeat(length) });
    if (!result.journaled) { high = length - 1; continue; }
    accepted++;
    for (const reason of reasons) {
      await box.markTerminalBlocked(run, gen, reason);
      assert.ok((await fs.stat(path.join(root, run, `terminal-${gen}.json`))).size <= cap);
      assert.equal((await box.observeTerminalAuthentication(run, gen)).kind, "authenticated");
      assert.ok(await box.readTerminalJournal(run, gen));
    }
    low = length + 1;
  }
  assert.ok(accepted > 0);
  const restarted = new Outbox({ root, log: log.logger, runMaxBytes: 1e6, maxBytes: 1e7, retentionMs: 1, terminalMaxBytes: 1024 });
  outboxes.push(restarted);
  await restarted.init();
  assert.equal(restarted.listPendingTerminals().length, accepted + 1);
});

test("blocking a historical cap-sized journal preserves its authenticated outcome without an oversized rewrite", async () => {
  const { root, box, file, log } = await fixture(1024);
  const record = JSON.parse(await fs.readFile(file, "utf8"));
  delete record.mac;
  record.body = { status: "failed", failure_reason: "" };
  const legacy = box as unknown as { seal(domain: string, body: unknown): string };
  const cap = 1024 + OUTBOX_RANGE_RESERVE_BYTES;
  record.body.failure_reason = "x".repeat(cap - Buffer.byteLength(legacy.seal("uzi.outbox.terminal.v1", record)));
  const serialized = legacy.seal("uzi.outbox.terminal.v1", record);
  assert.equal(Buffer.byteLength(serialized), cap);
  await fs.writeFile(file, serialized);
  assert.equal((await box.observeTerminalAuthentication(run, 3)).kind, "authenticated");
  await box.markTerminalBlocked(run, 3, "completion_permit_mismatch");
  assert.equal(await fs.readFile(file, "utf8"), serialized);
  assert.equal((await box.readTerminalJournal(run, 3))!.body.status, "failed");
  const restarted = new Outbox({ root, log: log.logger, runMaxBytes: 1e6, maxBytes: 1e7, retentionMs: 1, terminalMaxBytes: 1024 });
  outboxes.push(restarted);
  await restarted.init();
  assert.equal(restarted.listPendingTerminals().length, 1);
  assert.ok(await restarted.readTerminalJournal(run, 3));
});

test("symlinked run containment prevents observation and recursive deletion", async () => {
  const { box, root } = await fixture();
  const original = path.join(root, run);
  const moved = path.join(root, "saved");
  await fs.rename(original, moved);
  await fs.symlink(moved, original);
  assert.equal((await box.observeTerminalAuthentication(run, 3)).kind, "symlink");
  assert.equal(await box.hasPhysicalTerminalProtection(run), true);
  await box.retireRun(run);
  assert.ok(await fs.lstat(original));
  assert.ok(await fs.stat(path.join(moved, "terminal-3.json")));
});

test("accepted serialized journals are readable and oversized writes never claim durability", async () => {
  const { box, root } = await fixture(1024);
  const accepted = { status: "failed", error: "x".repeat(60 * 1024) };
  assert.equal((await box.journalTerminal(run, 4, "implement", 0, accepted)).journaled, true);
  assert.deepEqual((await box.readTerminalJournal(run, 4))!.body, accepted);
  assert.deepEqual(await box.journalTerminal(run, 5, "implement", 0, { status: "failed", error: "x".repeat(1024 + OUTBOX_RANGE_RESERVE_BYTES) }),
    { journaled: false, reason: "reserve_exhausted" });
  assert.equal(box.hasPendingTerminal(run, 5), false);
  await assert.rejects(fs.stat(path.join(root, run, "terminal-5.json")), { code: "ENOENT" });
});

test("temporary directory replacement preserves pending retry eligibility until file restoration", async () => {
  const { box, file } = await fixture();
  const saved = `${file}.saved`;
  await fs.rename(file, saved);
  await fs.mkdir(file);
  assert.equal(await box.readTerminalJournal(run, 3), undefined);
  assert.equal(box.hasPendingTerminal(run, 3), true);
  await fs.rmdir(file);
  await fs.rename(saved, file);
  assert.equal((await box.readTerminalJournal(run, 3))!.body.error, "private body");
  await box.retireTerminal(run, 3);
  assert.equal(box.hasPendingTerminal(run, 3), false);
});

test("startup selects valid leading-zero alias; bad canonical observations preserve it and ACK removes only winner", async () => {
  const { box, file, root, log } = await fixture();
  const alias = path.join(root, run, "terminal-0003.json");
  await fs.copyFile(file, alias);
  await reject(file);
  const restarted = new Outbox({ root, log: log.logger, runMaxBytes: 1e6, maxBytes: 1e7, retentionMs: 1 });
  outboxes.push(restarted);
  await restarted.init();
  assert.equal(restarted.hasPendingTerminal(run, 3), true);
  assert.equal((await restarted.observeTerminalAuthentication(run, 3)).kind, "mac_failure");
  await restarted.scanTerminalObservationsPage();
  assert.equal(restarted.hasPendingTerminal(run, 3), true);
  assert.equal((await restarted.readTerminalJournal(run, 3))!.body.error, "private body");
  await restarted.markTerminalBlocked(run, 3, "gap_unrecoverable");
  assert.equal((await restarted.readTerminalJournal(run, 3))!.blocked, true);
  await restarted.retireTerminal(run, 3);
  await assert.rejects(fs.stat(alias), { code: "ENOENT" });
  assert.ok(await fs.stat(file));
  // The original selected canonical winner must lose trust when scan confirms its rejection.
  await box.scanTerminalObservationsPage();
  assert.equal(box.hasPendingTerminal(run, 3), false);
});

// Reorder real entries from finite local fixtures, preserving real open/close handles.
// Only reads made by the protection scan are recorded, not the ordering pre-read.
async function withProtectionReads(
  options: { last?: { directory: string; name: string }; failure?: { directory: string; code: string } },
  check: (visited: Map<string, string[]>) => Promise<void>,
) {
  const original = fs.opendir;
  const visited = new Map<string, string[]>();
  let opened = 0;
  let closed = 0;
  fs.opendir = async (...args: Parameters<typeof fs.opendir>) => {
    const dir = await original(...args);
    opened++;
    const directory = String(args[0]);
    const read = dir.read.bind(dir);
    const close = dir.close.bind(dir);
    const entries: Dirent[] = [];
    try {
      // Each fixture has finitely many entries; one read per entry plus EOF, no retries.
      for (let entry = await read(); entry; entry = await read()) entries.push(entry);
      if (options.last?.directory === directory) {
        const target = entries.find(entry => entry.name === options.last?.name);
        assert.ok(target, "ordered target must exist in the real directory");
        entries.splice(entries.indexOf(target), 1);
        entries.push(target);
      }
    } catch (err) {
      await close();
      closed++;
      throw err;
    }
    const names: string[] = [];
    visited.set(directory, names);
    let index = 0;
    dir.read = (async () => {
      if (options.failure?.directory === directory) {
        throw Object.assign(new Error("injected protection directory read failure"), { code: options.failure.code });
      }
      const entry = entries[index++];
      if (!entry) return null;
      names.push(entry.name);
      return entry;
    }) as typeof dir.read;
    dir.close = (async () => {
      await close();
      closed++;
    }) as typeof dir.close;
    return dir;
  };
  try {
    await check(visited);
  } finally {
    fs.opendir = original;
    assert.equal(closed, opened, "every protection directory handle must close");
  }
}

function ordinaryUUID(n: number): string {
  return `${n.toString(16).padStart(8, "0")}-aaaa-4aaa-8aaa-aaaaaaaaaaaa`;
}

test("physical protection exhausts 257 junk entries and retirement removes the run", async () => {
  const { box, file, root } = await fixture();
  await fs.unlink(file);
  for (let n = 0; n < 257; n++) await fs.writeFile(path.join(root, run, `junk-${n}`), "");
  assert.equal(await box.hasPhysicalTerminalProtection(run), false);
  await box.retireRun(run);
  await assert.rejects(fs.stat(path.join(root, run)), { code: "ENOENT" });
});

test("physical protection preserves UUID case aliases", async () => {
  const { box, root } = await fixture();
  const lower = "abcdefab-1111-4111-8111-111111111111";
  const mixed = "ABCDefab-1111-4111-8111-111111111111";
  await box.journalTerminal(lower, 3, "implement", 0, { status: "failed" });
  await fs.unlink(path.join(root, lower, "terminal-3.json"));
  await box.journalTerminal(mixed, 3, "implement", 0, { status: "failed" });
  assert.equal(await box.hasPhysicalTerminalProtection(lower), true);
  await box.retireRun(lower);
  assert.ok(await fs.stat(path.join(root, mixed, "terminal-3.json")));
});

test("physical protection retirement removes all 257 ordinary UUID subdirectories without terminals", async () => {
  const { box, file, root } = await fixture();
  await fs.unlink(file);
  const directories = Array.from({ length: 257 }, (_, n) => path.join(root, run, ordinaryUUID(n)));
  for (const directory of directories) await fs.mkdir(directory);
  assert.equal((await fs.readdir(path.join(root, run))).length, 257);
  // Exercise retirement itself first: old code must fail regardless of enumeration order.
  await box.retireRun(run);
  for (const directory of directories) await assert.rejects(fs.stat(directory), { code: "ENOENT" });
  await assert.rejects(fs.stat(path.join(root, run)), { code: "ENOENT" });
  assert.equal(await box.hasPhysicalTerminalProtection(run), false);
});

test("physical protection retirement removes all 257 ordinary UUID run directories at the root", async () => {
  const { box, file, root } = await fixture();
  await fs.unlink(file);
  const runIds = [run, ...Array.from({ length: 256 }, (_, n) => ordinaryUUID(n))];
  for (const id of runIds.slice(1)) await fs.mkdir(path.join(root, id));
  assert.equal((await fs.readdir(root, { withFileTypes: true })).filter(entry => entry.isDirectory()).length, 257);
  for (const id of runIds) await box.retireRun(id);
  for (const id of runIds) await assert.rejects(fs.stat(path.join(root, id)), { code: "ENOENT" });
  assert.equal(await box.hasPhysicalTerminalProtection(run), false);
});

test("physical protection visits a terminal beyond 256 junk entries and closes handles", async () => {
  const { box, file, root } = await fixture();
  const directory = path.join(root, run);
  for (let n = 0; n < 257; n++) await fs.writeFile(path.join(directory, `junk-${n}`), "");
  await withProtectionReads({ last: { directory, name: path.basename(file) } }, async visited => {
    assert.equal(await box.hasPhysicalTerminalProtection(run), true);
    const names = visited.get(directory);
    assert.ok(names, "the run directory must be read");
    assert.equal(names.indexOf(path.basename(file)), 257, "the protecting terminal must actually be visited beyond the old budget");
    await box.retireRun(run);
    assert.ok(await fs.stat(file));
  });
});

test("physical protection visits a matching UUID case alias beyond 256 unrelated root entries", async () => {
  const { box, file, root } = await fixture();
  await fs.unlink(file);
  const lower = "abcdefab-1111-4111-8111-111111111111";
  const mixed = "ABCDefab-1111-4111-8111-111111111111";
  await fs.mkdir(path.join(root, lower));
  await box.journalTerminal(mixed, 3, "implement", 0, { status: "failed" });
  for (let n = 0; n < 257; n++) await fs.mkdir(path.join(root, ordinaryUUID(n)));
  await withProtectionReads({ last: { directory: root, name: mixed } }, async visited => {
    assert.equal(await box.hasPhysicalTerminalProtection(lower), true);
    const names = visited.get(root);
    assert.ok(names, "the root directory must be read");
    assert.ok(names.indexOf(mixed) > 256, "the matching case alias must actually be visited beyond the old budget");
    const aliasNames = visited.get(path.join(root, mixed));
    assert.ok(aliasNames, "the matching case alias directory must be read");
    assert.ok(aliasNames.includes("terminal-3.json"), "the case alias terminal must actually be visited");
    await box.retireRun(lower);
    assert.ok(await fs.stat(path.join(root, lower)));
    assert.ok(await fs.stat(path.join(root, mixed, "terminal-3.json")));
  });
});

for (const kind of ["symlink", "file"] as const) {
  test(`physical protection retains an unsafe ${kind} root even when the run is absent`, async () => {
    const { box, root } = await fixture();
    const moved = `${root}-saved`;
    await fs.rename(root, moved);
    roots.push(moved);
    if (kind === "symlink") await fs.symlink(moved, root);
    else await fs.writeFile(root, "");
    assert.equal(await box.hasPhysicalTerminalProtection(worker), true);
    await box.retireRun(worker);
    assert.ok(await fs.lstat(root));
  });
}

for (const kind of ["symlink", "file"] as const) {
  test(`physical protection retains an unsafe matching UUID ${kind} directory`, async () => {
    const { box, file, root } = await fixture();
    await fs.unlink(file);
    const lower = "abcdefab-1111-4111-8111-111111111111";
    const mixed = "ABCDefab-1111-4111-8111-111111111111";
    await fs.mkdir(path.join(root, lower));
    const alias = path.join(root, mixed);
    if (kind === "symlink") await fs.symlink(path.join(root, run), alias);
    else await fs.writeFile(alias, "");
    await withProtectionReads({ last: { directory: root, name: mixed } }, async visited => {
      assert.equal(await box.hasPhysicalTerminalProtection(lower), true);
      const names = visited.get(root);
      assert.ok(names, "the root directory must be read");
      assert.ok(names.includes(mixed), "the unsafe matching alias must actually be visited");
      await box.retireRun(lower);
      assert.ok(await fs.stat(path.join(root, lower)));
      assert.ok(await fs.lstat(alias));
    });
  });
}

for (const location of ["run", "root"] as const) {
  for (const code of ["EIO", "ENOENT"] as const) {
    test(`physical protection retains the run on ${location} read failure ${code} and closes handles`, async () => {
      const { box, file, root } = await fixture();
      await fs.unlink(file);
      const directory = location === "run" ? path.join(root, run) : root;
      await withProtectionReads({ failure: { directory, code } }, async visited => {
        assert.equal(await box.hasPhysicalTerminalProtection(run), true);
        assert.ok(visited.has(directory), "the failing directory must have been opened");
        await box.retireRun(run);
        assert.ok(await fs.stat(path.join(root, run)));
      });
    });
  }
}

test("busy outbox writes do not queue custody disposal or permit late cleanup", async () => {
  const { box, file } = await fixture();
  await reject(file);
  const observed = await box.observeTerminalAuthentication(run, 3);
  let unlock!: () => void;
  let entered!: () => void;
  const held = new Promise<void>((resolve) => { unlock = resolve; });
  const ready = new Promise<void>((resolve) => { entered = resolve; });
  const lock = box as unknown as { withRunLock<T>(id: string, fn: () => Promise<T>): Promise<T> };
  const writer = lock.withRunLock(run, async () => { entered(); await held; });
  await ready;
  let reads = 0;
  let allowed = true;
  const cleanup = box.cleanupRejectedTerminalFiles(run, 3, worker, [observed], async () => {
    reads++;
    return settled();
  }, () => allowed);
  let timer: ReturnType<typeof setTimeout> | undefined;
  try {
    const deadline = new Promise<never>((_resolve, reject) => {
      timer = setTimeout(() => reject(new Error("disposal queued behind busy outbox")), 200);
    });
    assert.equal(await Promise.race([cleanup, deadline]), 0);
    assert.equal(reads, 0);
    assert.ok(await fs.stat(file));
  } finally {
    clearTimeout(timer);
    allowed = false;
    unlock();
    await writer;
    await cleanup;
  }
  assert.equal(reads, 0);
  assert.ok(await fs.stat(file));
  assert.equal(await box.cleanupRejectedTerminalFiles(run, 3, worker, [observed], async () => settled()), 1);
});

test("rejected cleanup directory fsync failure propagates without claiming durable completion", async () => {
  const { box, file, root } = await fixture();
  await reject(file);
  const alias = path.join(root, run, "terminal-003.json");
  await fs.copyFile(file, alias);
  const observations = [await box.observeTerminalAuthentication(run, 3), await box.observeTerminalAuthentication(run, 3, "terminal-003.json")];
  const seam = box as unknown as { fsyncDir(dir: string, strict?: boolean): Promise<void> };
  const original = seam.fsyncDir;
  let calls = 0;
  seam.fsyncDir = async (_dir, strict) => { calls++; assert.equal(strict, true); throw new Error("injected directory fsync failure"); };
  try {
    await assert.rejects(box.cleanupRejectedTerminalFiles(run, 3, worker, observations, async () => settled()), /injected directory fsync failure/);
    assert.equal(calls, 1);
    assert.ok(await fs.stat(alias));
    assert.equal(await box.cleanupRejectedTerminalFiles(run, 3, worker, observations, async () => settled()), 0);
    assert.equal(await box.cleanupRejectedTerminalFiles(run, 3, worker, [observations[1]!], async () => ({ ...settled(), complete: false })), 0);
    assert.equal(calls, 1);
    assert.ok(await fs.stat(alias));
  } finally {
    seam.fsyncDir = original;
  }
});

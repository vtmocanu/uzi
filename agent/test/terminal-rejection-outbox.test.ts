import { test, afterEach } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { Outbox, OUTBOX_RANGE_RESERVE_BYTES } from "../src/outbox.js";
import type { TerminalRejectionCustodyResponse } from "../src/protocol.js";
import { recordingLogger } from "./helpers.js";

const scratch = fileURLToPath(new URL("../../.uzi/scratch/", import.meta.url));
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

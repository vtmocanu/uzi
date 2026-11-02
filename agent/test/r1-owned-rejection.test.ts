import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { Outbox } from "../src/outbox.js";
import { makeScratchRoot } from "./fixture-repo.js";
import { nullLogger } from "./helpers.js";

const run = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa";
const other = "cccccccc-cccc-4ccc-8ccc-cccccccccccc";
const worker = "dddddddd-dddd-4ddd-8ddd-dddddddddddd";
const request = {
  outcome_id: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", source_generation: 7, claim_generation: 7,
  detail: "refused" as const, continuation_mode: "same_conversation" as const,
  tag: "cyberPolicy" as const, phase: "implementation" as const,
  correlation_id: "pr-" + "a".repeat(32) + "-1", session_id: "s".repeat(128), head: "a".repeat(40),
  published: true, checkpoint_contains_latest: true,
};
const custody = () => Promise.resolve({
  run_id: run, worker_id: worker, generation: 7,
  exact_holds: [{ id: "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee", state: "released" as const }],
  sibling_holds: [], exact_count: 1, sibling_count: 0,
  exact_complete: true, sibling_complete: true, complete: true, outcome: "settled" as const,
});
async function fixture(t: { after(fn: () => Promise<void>): void }, quota = 1e6) {
  const dir = makeScratchRoot("r1-rejection-owner-", await fs.realpath(os.tmpdir()));
  t.after(() => fs.rm(dir, { recursive: true, force: true }));
  const root = path.join(dir, "outbox");
  const box = new Outbox({ root, log: nullLogger(), maxBytes: quota, runMaxBytes: quota, retentionMs: 1 });
  await box.init();
  return { box, root, file: path.join(root, run, "terminal-7.json") };
}
async function reject(file: string) {
  const record = JSON.parse(await fs.readFile(file, "utf8"));
  record.body = { status: "completed", error: "MAC changed" };
  await fs.writeFile(file, JSON.stringify(record));
}

it("R1 explicit body-budget policy identity survives authorized physical rejection disposal", async t => {
  const quota = Buffer.byteLength(JSON.stringify(request)) - 1;
  const { box, root, file } = await fixture(t, quota);
  assert.equal((await box.journalPolicyHold(run, request)).journaled, false);
  assert.equal(await box.hasPendingPolicyOutcome(run), true);
  const writer = new Outbox({ root, log: nullLogger(), maxBytes: 1e6, runMaxBytes: 1e6, retentionMs: 1 });
  await writer.init();
  assert.equal((await writer.journalTerminal(run, 7, "running", 0, { status: "failed" })).journaled, true);
  await reject(file);
  assert.ok((await fs.stat(file)).size <= quota, "setup reaches MAC authentication rather than oversize rejection");
  const observation = await box.observeTerminalAuthentication(run, 7);
  assert.equal(observation.kind, "mac_failure");
  assert.equal(await box.cleanupRejectedTerminalFiles(run, 7, worker, [observation], custody), 1);
  await assert.rejects(fs.lstat(file), { code: "ENOENT" });
  assert.equal(await box.hasPendingPolicyOutcome(run), true,
    "physical custody cannot erase the independently chosen memory-only policy identity");
  assert.deepEqual(await box.journalTerminal(run, 8, "running", 0, { status: "completed" }),
    { journaled: false, reason: "winner_unavailable" });
});

it("R1 guarded rejection disposal reuses only its counted physical slot at full capacity", async t => {
  const { box, file } = await fixture(t);
  assert.equal((await box.journalTerminal(run, 7, "running", 0, { status: "failed" })).journaled, true);
  await reject(file);
  const observation = await box.observeTerminalAuthentication(run, 7);
  assert.equal(observation.kind, "mac_failure");
  assert.equal(await box.hasPendingPolicyOutcome(run), true);
  const tokens = Array.from({ length: 1023 }, (_, i) => box.reservePolicyFlight(other, i + 1));
  assert.ok(tokens.every(token => token !== undefined));
  assert.equal(box.reservePolicyFlight(other, 2000), undefined, "the rejection owns the remaining shared slot");
  assert.equal(await box.cleanupRejectedTerminalFiles(run, 7, worker, [observation], custody), 1);
  assert.equal(await box.hasPendingPolicyOutcome(run), false);
  const reused = box.reservePolicyFlight(other, 2000);
  assert.ok(reused, "verified disposal makes exactly that identity capacity reusable");
  box.releasePolicyFlight(reused);
  for (const token of tokens) box.releasePolicyFlight(token!);
});

for (const opaqueGeneration of [7, 8]) it("R1 a later MAC disposal cannot erase an older opaque path uncertainty: " + opaqueGeneration, async t => {
  const { box, root, file } = await fixture(t);
  const actual = path.join(root, "actual");
  await fs.mkdir(actual);
  await fs.symlink(actual, path.join(root, run));
  assert.equal((await box.observeTerminalAuthentication(run, opaqueGeneration)).kind, "symlink");
  await fs.unlink(path.join(root, run));
  const writer = new Outbox({ root, log: nullLogger(), maxBytes: 1e6, runMaxBytes: 1e6, retentionMs: 1 });
  await writer.init();
  assert.equal((await writer.journalTerminal(run, 7, "running", 0, { status: "failed" })).journaled, true);
  await reject(file);
  const observation = await box.observeTerminalAuthentication(run, 7);
  assert.equal(observation.kind, "mac_failure");
  assert.equal(await box.cleanupRejectedTerminalFiles(run, 7, worker, [observation], custody), 1);
  await assert.rejects(fs.lstat(file), { code: "ENOENT" });
  assert.equal(await box.hasPendingPolicyOutcome(run), true,
    "a fresh fingerprint at the same path is not authority for the older opaque obligation");
});

it("R1 failed directory durability never discharges its captured physical rejection", async t => {
  const { box, file } = await fixture(t);
  assert.equal((await box.journalTerminal(run, 7, "running", 0, { status: "failed" })).journaled, true);
  await reject(file);
  const observation = await box.observeTerminalAuthentication(run, 7);
  assert.equal(observation.kind, "mac_failure");
  const seam = box as unknown as { fsyncDir(dir: string, strict?: boolean): Promise<void> };
  const original = seam.fsyncDir;
  seam.fsyncDir = async () => { throw new Error("owned rejection fsync failure"); };
  try {
    await assert.rejects(box.cleanupRejectedTerminalFiles(run, 7, worker, [observation], custody),
      /owned rejection fsync failure/);
  } finally { seam.fsyncDir = original; }
  assert.equal(await box.hasPendingPolicyOutcome(run), true);
});

for (const discovery of ["boot", "admission", "recovery"] as const) {
  it("R1 pre-enrolled physical MAC rejection discharges after verified cleanup: " + discovery, async t => {
    const { box: writer, root, file } = await fixture(t);
    const reader = new Outbox({ root, log: nullLogger(), maxBytes: 1e6, runMaxBytes: 1e6, retentionMs: 1 });
    if (discovery !== "boot") await reader.init();
    assert.equal((await writer.journalTerminal(run, 7, "running", 0, { status: "failed" })).journaled, true);
    await reject(file);
    if (discovery === "boot") await reader.init();
    else if (discovery === "admission") assert.equal(await reader.hasPendingPolicyOutcome(run), true);
    else {
      let page = await reader.scanTerminalObservationsPage();
      for (let count = 0; !page.passComplete && count < 10; count++) page = await reader.scanTerminalObservationsPage();
      assert.equal(page.passComplete, true);
      await reader.closeTerminalObservationScan();
    }
    assert.equal(reader.listPendingTerminals().find(source => source.claim_generation === 7)?.outcome_kind, "unknown");
    const observed = await reader.observeTerminalAuthentication(run, 7);
    assert.equal(observed.kind, "mac_failure");
    assert.equal(await reader.cleanupRejectedTerminalFiles(run, 7, worker, [observed], custody), 1);
    await assert.rejects(fs.lstat(file), { code: "ENOENT" });
    assert.equal(await reader.hasPendingPolicyOutcome(run), false,
      "verified durable disposal discharges unselected physical metadata, without requiring a second scope owner");
  });
}

it("R1 boot rejection cleanup transfers protection to a surviving alias", async t => {
  const { box: writer, root, file } = await fixture(t);
  assert.equal((await writer.journalTerminal(run, 7, "running", 0, { status: "failed" })).journaled, true);
  await reject(file);
  const aliasName = "terminal-0007.json";
  const alias = path.join(root, run, aliasName);
  await fs.copyFile(file, alias);
  const reader = new Outbox({ root, log: nullLogger(), maxBytes: 1e6, runMaxBytes: 1e6, retentionMs: 1 });
  await reader.init();
  const canonical = await reader.observeTerminalAuthentication(run, 7);
  assert.equal(canonical.kind, "mac_failure");
  assert.equal(await reader.cleanupRejectedTerminalFiles(run, 7, worker, [canonical], custody), 1);
  assert.equal(await reader.hasPendingPolicyOutcome(run), true);
  assert.ok(await fs.stat(alias));
  const remaining = await reader.observeTerminalAuthentication(run, 7, aliasName);
  assert.equal(remaining.kind, "mac_failure");
  assert.equal(await reader.cleanupRejectedTerminalFiles(run, 7, worker, [remaining], custody), 1);
  assert.equal(await reader.hasPendingPolicyOutcome(run), false);
});

it("R1 replaced ordinary symlink defers before enrollment at full capacity and protects after enrollment", async t => {
  const { box, root, file } = await fixture(t);
  assert.equal((await box.journalTerminal(run, 7, "running", 0, { status: "failed" })).journaled, true);
  const tokens = Array.from({ length: 1024 }, (_, i) => box.reservePolicyFlight(other, i + 1));
  assert.ok(tokens.every(token => token !== undefined));
  const target = path.join(root, "untouched");
  await fs.writeFile(target, "untouched");
  await fs.unlink(file);
  await fs.symlink(target, file);
  assert.equal((await box.observeTerminalAuthentication(run, 7)).kind, "unreadable",
    "new unsafe lifetime must not be interpreted through the represented-ordinary exception at full capacity");
  assert.deepEqual(await box.journalTerminal(run, 7, "running", 0, { status: "completed" }),
    { journaled: false, reason: "winner_unavailable" });
  assert.equal(box.hasPendingTerminal(run, 7), true, "capacity deferral must keep the original ordinary metadata");
  box.releasePolicyFlight(tokens[0]!);
  assert.deepEqual(await box.journalTerminal(run, 7, "running", 0, { status: "completed" }),
    { journaled: false, reason: "reserve_exhausted" });
  assert.equal(await box.hasPendingPolicyOutcome(run), true);
  await fs.unlink(file);
  assert.equal(await box.hasPendingPolicyOutcome(run), true, "enrolled unsafe evidence cannot be released by disappearance");
  assert.equal(await fs.readFile(target, "utf8"), "untouched");
  for (const token of tokens.slice(1)) box.releasePolicyFlight(token!);
});

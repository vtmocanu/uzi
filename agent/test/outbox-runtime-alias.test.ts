import { test, afterEach } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import os from "node:os";
import { createHash } from "node:crypto";
import path from "node:path";
import { realpathSync, type Dirent } from "node:fs";
import { Outbox } from "../src/outbox.js";
import type { PolicyHoldRequest } from "../src/protocol.js";
import { nullLogger } from "./helpers.js";

// Runtime counterpart of outbox-boot-arbitration.test.ts: a rejected canonical and a valid
// alias that appear after init must give the same enrolled entry in either directory order.

const scratch = realpathSync(os.tmpdir());
const run = "11111111-1111-4111-8111-111111111111";
const roots: string[] = [];
afterEach(async () => {
  for (const root of roots.splice(0)) await fs.rm(root, { recursive: true, force: true });
});

type Order = "forward" | "reversed";
const orders: Order[] = ["forward", "reversed"];

async function withOpendirOrder<T>(order: Order, body: () => Promise<T>): Promise<T> {
  const original = fs.opendir;
  fs.opendir = (async (...args: Parameters<typeof fs.opendir>) => {
    const dir = await original(...args);
    const read = dir.read.bind(dir);
    const close = dir.close.bind(dir);
    const entries: Dirent[] = [];
    try {
      for (let entry = await read(); entry; entry = await read()) entries.push(entry);
    } catch (err) {
      await close();
      throw err;
    }
    entries.sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
    if (order === "reversed") entries.reverse();
    let index = 0;
    dir.read = (async () => entries[index++] ?? null) as typeof dir.read;
    return dir;
  }) as typeof fs.opendir;
  try {
    return await body();
  } finally {
    fs.opendir = original;
  }
}

const opts = (root: string, extra: Record<string, unknown> = {}) =>
  ({ root, log: nullLogger(), runMaxBytes: 1e6, maxBytes: 1e7, retentionMs: 1, ...extra });

// A running outbox that sees a rejected canonical plus a valid alias appear after init.
async function runtimeFixture(extra: Record<string, unknown> = {}, rejectCanonical = true) {
  const root = await fs.mkdtemp(path.join(scratch, "runtime-alias-"));
  roots.push(root);
  const box = new Outbox(opts(root, extra));
  await box.init();
  const writer = new Outbox(opts(root));
  await writer.init();
  await writer.journalTerminal(run, 3, "implement", 0, { status: "failed", error: "private body" });
  const dir = path.join(root, run);
  const file = path.join(dir, "terminal-3.json");
  const alias = path.join(dir, "terminal-0003.json");
  await fs.copyFile(file, alias);
  if (rejectCanonical) {
    const obj = JSON.parse(await fs.readFile(file, "utf8"));
    obj.body = { status: "failed", error: "changed private body" };
    await fs.writeFile(file, JSON.stringify(obj));
  }
  return { box, file, alias, writer };
}

async function pageScan(box: Outbox) {
  for (let i = 0; i < 20; i++) if ((await box.scanTerminalObservationsPage()).passComplete) return;
  assert.fail("scan did not complete");
}

const kinds = (box: Outbox) => box.listPendingTerminals().filter(p => p.claim_generation === 3).map(p => p.outcome_kind);

for (const order of orders) {
  test(`runtime page scan (${order}): a valid alias is not shadowed by a rejected canonical, which stays protected`, async () => {
    const { box, file, alias } = await runtimeFixture();
    await withOpendirOrder(order, () => pageScan(box));
    assert.equal((await box.readTerminalJournal(run, 3))?.body.error, "private body");
    assert.deepEqual(kinds(box), [undefined], "the authenticated alias is the enrolled ordinary entry");
    assert.equal(await box.hasPhysicalTerminalProtection(run), true);
    assert.equal(await box.hasPendingPolicyOutcome(run), true, "the rejected canonical keeps the run's policy scope");
    await fs.unlink(file);
    assert.equal(await box.hasPhysicalTerminalProtection(run), true, "retained scope protects the rejected file");
    assert.equal(await box.retireTerminal(run, 3), true);
    assert.equal(await fs.lstat(alias).then(() => true, () => false), false, "retirement removes the enrolled alias");
  });

  test(`runtime target scan then page scan (${order}): same enrolled alias`, async () => {
    const { box } = await runtimeFixture();
    assert.equal(await withOpendirOrder(order, () => box.hasPendingPolicyOutcome(run)), true);
    await withOpendirOrder(order, () => pageScan(box));
    assert.equal((await box.readTerminalJournal(run, 3))?.body.error, "private body");
    assert.deepEqual(kinds(box), [undefined]);
  });

  test(`runtime page scan (${order}): an oversized (uninspected) canonical is never displaced`, async () => {
    const { box, writer, file, alias } = await runtimeFixture({ terminalMaxBytes: 1024 }, false);
    await fs.unlink(file);
    await writer.journalTerminal(run, 3, "implement", 9, { status: "failed", error: "x".repeat(200 * 1024) });
    const before = await fs.readFile(file);
    await withOpendirOrder(order, () => pageScan(box));
    assert.deepEqual(await fs.readFile(file), before);
    assert.equal(await box.hasPendingPolicyOutcome(run), true);
    if (order === "reversed") assert.deepEqual(kinds(box), ["unknown"], "the uninspected canonical keeps its entry");
    assert.ok(await fs.lstat(alias), "the alias file is untouched");
  });
}

// White-box views of the live catalog; each assertion names the observable it pins.
type Internals = {
  runs: Map<string, { terminals: Map<number, { fileName: string; outcomeKind: string; selectionToken?: symbol; capturePending?: boolean }> }>;
  policyInspections: Map<symbol, { runId: string; held: boolean; sourceKey?: string; macBinding?: { fileName: string; sha256: string };
    sourceBinding?: { fileName: string } }>;
  hasCountedPhysicalScope(runId: string, fileName: string): boolean;
};
const internals = (box: Outbox) => box as unknown as Internals;
const sourceKey = (name: string) => createHash("sha256").update(name).digest("hex");
const scopesFor = (box: Outbox, name: string) =>
  [...internals(box).policyInspections.values()].filter(s => s.held && s.runId === run && s.sourceKey === sourceKey(name));

for (const order of orders) {
  test(`runtime page scan (${order}): the displaced canonical keeps a held scope with its own MAC binding, alias gone`, async () => {
    const { box, file, alias } = await runtimeFixture();
    await withOpendirOrder(order, () => pageScan(box));
    const canonical = await box.observeTerminalAuthentication(run, 3, "terminal-3.json");
    assert.equal(canonical.kind, "mac_failure");
    assert.deepEqual(kinds(box), [undefined]);
    assert.equal(internals(box).hasCountedPhysicalScope(run, "terminal-3.json"), true);
    const scopes = scopesFor(box, "terminal-3.json");
    assert.equal(scopes.length, 1);
    assert.equal(scopes[0]!.macBinding?.fileName, "terminal-3.json");
    assert.equal(scopes[0]!.macBinding?.sha256, canonical.fingerprint?.sha256);
    assert.equal(scopes[0]!.sourceBinding, undefined, "the alias's own source binding is not kept");
    assert.equal(scopesFor(box, "terminal-0003.json").length, 0, "no scope is left keyed to the alias");
    await fs.unlink(alias);
    assert.ok(await fs.lstat(file));
    assert.equal(internals(box).hasCountedPhysicalScope(run, "terminal-3.json"), true);
    assert.equal(await box.hasPhysicalTerminalProtection(run), true);
  });

  test(`runtime page scan (${order}): a malformed canonical is rejection evidence and keeps a source-bound scope`, async () => {
    const { box, file } = await runtimeFixture({}, false);
    await fs.writeFile(file, "this is not json");
    await withOpendirOrder(order, () => pageScan(box));
    assert.deepEqual(kinds(box), [undefined], "the authenticated alias is the enrolled ordinary entry");
    const scopes = scopesFor(box, "terminal-3.json");
    assert.equal(scopes.length, 1);
    assert.equal(scopes[0]!.macBinding, undefined);
    assert.equal(scopes[0]!.sourceBinding?.fileName, "terminal-3.json");
  });

  test(`runtime page scan (${order}): a selected policy flight is never displaced`, async () => {
    const { box } = await runtimeFixture();
    const token = box.reservePolicyFlight(run, 3);
    assert.ok(token);
    assert.equal(box.selectPolicyFlight(token, "22222222-2222-4222-8222-222222222222"), true);
    const selected = internals(box).runs.get(run)!.terminals.get(3)!;
    for (let i = 0; i < 3; i++) await withOpendirOrder(order, () => pageScan(box));
    const after = internals(box).runs.get(run)!.terminals.get(3);
    assert.equal(after, selected, "same entry object");
    assert.equal(after?.selectionToken, token);
    assert.equal(after?.fileName, "terminal-3.json");
  });

  test(`runtime page scan (${order}): evidence about a replaced canonical does not authorise displacement`, async () => {
    const { box, file, alias } = await runtimeFixture({ terminalMaxBytes: 1024 }, false);
    const side = path.join(await fs.mkdtemp(path.join(scratch, "runtime-alias-side-")), "alias.json");
    roots.push(path.dirname(side));
    await fs.rename(alias, side);
    const obj = JSON.parse(await fs.readFile(file, "utf8"));
    obj.body = { status: "failed", error: "changed private body" };
    await fs.writeFile(file, JSON.stringify(obj));
    await pageScan(box);
    assert.deepEqual(kinds(box), ["unknown"], "pass 1 enrolls the MAC-rejected canonical");
    // Pass 2: the canonical is replaced by an oversized (uninspected) file and a valid alias appears.
    await fs.unlink(file);
    const second = new Outbox(opts(path.dirname(path.dirname(file))));
    await second.init();
    await second.journalTerminal(run, 3, "implement", 9, { status: "failed", error: "x".repeat(200 * 1024) });
    await fs.copyFile(side, alias);
    const before = await fs.readFile(file);
    await withOpendirOrder(order, () => pageScan(box));
    assert.deepEqual(kinds(box), ["unknown"], "the uninspected canonical keeps its entry");
    assert.deepEqual(await fs.readFile(file), before);
  });

  test(`runtime page scan (${order}): evidence does not survive an in-place rewrite of the same inode`, async () => {
    const { box, file, alias } = await runtimeFixture({ terminalMaxBytes: 1024 }, false);
    const side = path.join(await fs.mkdtemp(path.join(scratch, "runtime-alias-side-")), "alias.json");
    roots.push(path.dirname(side));
    await fs.rename(alias, side);
    const obj = JSON.parse(await fs.readFile(file, "utf8"));
    obj.body = { status: "failed", error: "changed private body" };
    await fs.writeFile(file, JSON.stringify(obj));
    await pageScan(box);
    assert.deepEqual(kinds(box), ["unknown"], "pass 1 rejects the canonical and records evidence");
    const ino = (await fs.lstat(file)).ino;
    await fs.writeFile(file, "x".repeat(200 * 1024));
    assert.equal((await fs.lstat(file)).ino, ino, "the canonical was rewritten in place");
    await fs.copyFile(side, alias);
    await withOpendirOrder(order, () => pageScan(box));
    assert.deepEqual(kinds(box), ["unknown"], "the rewritten (uninspected) canonical keeps its entry");
    assert.equal(internals(box).runs.get(run)!.terminals.get(3)?.fileName, "terminal-3.json");
  });

  test(`runtime page scan (${order}): a selectionToken-only unknown entry is never displaced`, async () => {
    const root = await fs.mkdtemp(path.join(scratch, "runtime-alias-sel-"));
    roots.push(root);
    const holdFor = (g: number): PolicyHoldRequest => ({ outcome_id: `bbbbbbbb-bbbb-4bbb-8bbb-${String(g).padStart(12, "b")}`,
      source_generation: g, claim_generation: g, detail: "resume_rejected", continuation_mode: "same_conversation", published: false });
    const writer = new Outbox(opts(root));
    await writer.init();
    await writer.journalTerminal(run, 3, "implement", 0, { status: "failed" });
    const sealed = await fs.readFile(path.join(root, run, "terminal-3.json"));
    await fs.rm(path.join(root, run), { recursive: true });
    const box = new Outbox(opts(root, { runMaxBytes: 1200 }));
    await box.init();
    for (const g of [5, 6, 7]) assert.deepEqual(await box.journalPolicyHold(run, holdFor(g)), { journaled: true, adopted: false });
    const token = box.reservePolicyFlight(run, 3);
    assert.ok(token);
    assert.equal(box.selectPolicyFlight(token, holdFor(3).outcome_id), true);
    assert.deepEqual(await box.journalPolicyHold(run, holdFor(3)), { journaled: false, reason: "winner_unavailable" });
    const selected = internals(box).runs.get(run)!.terminals.get(3)!;
    assert.equal(selected.outcomeKind, "unknown");
    assert.ok(selected.selectionToken);
    assert.ok(!selected.capturePending, "only the selectionToken guard applies");
    // A valid sealed record for generation 3, made by a writer that shares this root's key before
    // the box holds any policy state (this outbox refuses a competing outcome once it does).
    const dir = path.join(root, run);
    await fs.mkdir(dir, { recursive: true });
    await fs.writeFile(path.join(dir, "terminal-0003.json"), sealed);
    await fs.writeFile(path.join(dir, "terminal-3.json"), sealed);
    const obj = JSON.parse(await fs.readFile(path.join(dir, "terminal-3.json"), "utf8"));
    obj.body = { status: "failed", error: "tampered" };
    await fs.writeFile(path.join(dir, "terminal-3.json"), JSON.stringify(obj));
    for (let i = 0; i < 3; i++) await withOpendirOrder(order, () => pageScan(box));
    const after = internals(box).runs.get(run)!.terminals.get(3);
    assert.equal(after, selected, "same entry object");
    assert.equal(after?.selectionToken, selected.selectionToken);
    assert.equal(after?.fileName, "terminal-3.json");
  });

  test(`runtime page scan (${order}): rejection of a different file never counts as the canonical's evidence`, async () => {
    const { box, writer, file, alias } = await runtimeFixture({ terminalMaxBytes: 1024 }, false);
    await fs.unlink(file);
    await writer.journalTerminal(run, 3, "implement", 9, { status: "failed", error: "x".repeat(200 * 1024) });
    const obj = JSON.parse(await fs.readFile(alias, "utf8"));
    obj.body = { status: "failed", error: "garbage" };
    await fs.writeFile(path.join(path.dirname(alias), "terminal-03.json"), JSON.stringify(obj));
    await withOpendirOrder(order, () => pageScan(box));
    if (order === "reversed") {
      assert.deepEqual(kinds(box), ["unknown"], "the uninspected canonical keeps its enrolled unknown entry");
      assert.equal(internals(box).runs.get(run)!.terminals.get(3)?.fileName, "terminal-3.json");
    }
  });
}

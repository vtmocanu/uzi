import { test, afterEach } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { realpathSync, type Dirent } from "node:fs";
import { Outbox } from "../src/outbox.js";
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

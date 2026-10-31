import { test, afterEach } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { realpathSync, type Dirent } from "node:fs";
import { Outbox } from "../src/outbox.js";
import { nullLogger, recordingLogger } from "./helpers.js";

// Boot terminal arbitration (ADR-3607): the enrolled file per generation and the
// pending order must not depend on the order the directory stream yields entries.
// The libuv readdir order of the base was strcmp-sorted; opendir is raw filesystem
// order, so each case runs in sorted ("forward") and reversed order.

const scratch = realpathSync(os.tmpdir());
const run = "11111111-1111-4111-8111-111111111111";
const roots: string[] = [];
afterEach(async () => {
  for (const root of roots.splice(0)) await fs.rm(root, { recursive: true, force: true });
});

type Order = "forward" | "reversed";
const orders: Order[] = ["forward", "reversed"];

// Test-local fs.opendir hook: buffer the real entries, reorder, restore in finally.
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

function options(root: string, extra: Record<string, unknown> = {}) {
  return { root, log: nullLogger(), runMaxBytes: 1e6, maxBytes: 1e7, retentionMs: 1, ...extra };
}

async function writerFixture(generations: number[] = [3], writeNow?: () => number) {
  const root = await fs.mkdtemp(path.join(scratch, "boot-arbitration-"));
  roots.push(root);
  const writer = new Outbox(options(root, writeNow ? { now: writeNow } : {}));
  await writer.init();
  for (const generation of generations) {
    await writer.journalTerminal(run, generation, "implement", 0, { status: "failed", error: "private body" });
  }
  const dir = path.join(root, run);
  return { root, writer, dir, file: path.join(dir, "terminal-3.json") };
}

async function restart(order: Order, root: string, extra: Record<string, unknown> = {}, preInit?: (box: Outbox) => void) {
  const box = new Outbox(options(root, extra));
  preInit?.(box);
  await withOpendirOrder(order, () => box.init());
  return box;
}

async function reject(file: string) {
  const obj = JSON.parse(await fs.readFile(file, "utf8"));
  obj.body = { status: "failed", error: "changed private body" };
  await fs.writeFile(file, JSON.stringify(obj));
}

async function exists(file: string): Promise<boolean> {
  try {
    await fs.lstat(file);
    return true;
  } catch (err) {
    if ((err as NodeJS.ErrnoException).code === "ENOENT") return false;
    throw err;
  }
}

for (const order of orders) {
  test(`R1 (${order}): pending terminals list in ascending claim_generation`, async () => {
    // 2 and 10 differ between numeric and lexicographic ("terminal-10" sorts before "terminal-2").
    const { root } = await writerFixture([2, 10]);
    const box = await restart(order, root);
    assert.deepEqual(box.listPendingTerminals().map(p => p.claim_generation), [2, 10]);
  });

  test(`R6 (${order}): generation 0 canonical outranks a valid alias`, async () => {
    const { root, dir } = await writerFixture([0]);
    const canonical = path.join(dir, "terminal-0.json");
    const alias = path.join(dir, "terminal-00.json");
    await fs.copyFile(canonical, alias);
    const box = await restart(order, root);
    assert.equal(await box.retireTerminal(run, 0), true);
    assert.equal(await exists(canonical), false, "the canonical is the enrolled winner ('.' sorts before '0')");
    assert.ok(await exists(alias));
  });

  test(`R7 (${order}): a loser read after an enrolled file leaves its deferral fields untouched`, async () => {
    const { root, dir, file } = await writerFixture();
    const alias = path.join(dir, "terminal-0003.json");
    await fs.copyFile(file, alias);
    const size = (await fs.stat(file)).size;
    // Room for exactly one full read per run: the second file is budget-deferred.
    const box = await restart(order, root, { runMaxBytes: Math.floor(size * 1.5) });
    const runs = (box as unknown as { runs: Map<string, { terminals: Map<number, { budgetDeferred?: boolean; deferredVersion?: string }> }> }).runs;
    const enrolled = runs.get(run)?.terminals.get(3);
    assert.ok(enrolled);
    assert.notEqual(enrolled.budgetDeferred, true, "the deferred loser must not mark the enrolled entry");
    assert.equal((await box.readTerminalJournal(run, 3))?.body.error, "private body");
  });

  test(`R8 (${order}): without a usable key the catalog is still ascending`, async () => {
    const { root } = await writerFixture([2, 10]);
    await fs.unlink(path.join(root, ".key"));
    const box = await restart(order, root);
    assert.deepEqual(box.listPendingOutcomeSources().map(p => p.claim_generation), [2, 10]);
  });

  test(`R2 (${order}): valid alias beats a rejected canonical, which stays protected`, async () => {
    const { root, dir, file } = await writerFixture();
    const alias = path.join(dir, "terminal-0003.json");
    await fs.copyFile(file, alias);
    await reject(file);
    const box = await restart(order, root);
    assert.equal((await box.readTerminalJournal(run, 3))?.body.error, "private body");
    assert.equal(await box.hasPhysicalTerminalProtection(run), true);
    assert.ok(await exists(file));
    assert.ok(box.listPendingOutcomeSources().some(p => p.claim_generation === 3 && p.outcome_kind === "unknown"),
      "the rejected canonical keeps a counted physical scope");
    assert.equal(await box.retireTerminal(run, 3), true);
    assert.equal(await exists(alias), false);
    assert.ok(await exists(file));
    // With both files gone only the retained in-memory scope can still protect the run.
    await fs.unlink(file);
    assert.equal(await box.hasPhysicalTerminalProtection(run), true);
  });

  test(`R3 (${order}): canonical outranks a valid alias`, async () => {
    const { root, dir, file } = await writerFixture();
    const alias = path.join(dir, "terminal-0003.json");
    await fs.copyFile(file, alias);
    const box = await restart(order, root);
    assert.equal(await box.retireTerminal(run, 3), true);
    assert.equal(await exists(file), false, "retirement deletes the enrolled canonical winner");
    assert.ok(await exists(alias), "the alias is not the enrolled winner");
  });

  test(`R4 (${order}): the greater code-unit filename wins between valid aliases`, async () => {
    const { root, dir, file } = await writerFixture();
    const low = path.join(dir, "terminal-003.json");
    const high = path.join(dir, "terminal-03.json");
    await fs.copyFile(file, low);
    await fs.copyFile(file, high);
    await fs.unlink(file);
    const box = await restart(order, root);
    assert.equal(await box.retireTerminal(run, 3), true);
    assert.equal(await exists(high), false, "terminal-03.json is the enrolled winner");
    assert.ok(await exists(low));
  });

  // Control (green before and after): an uninspected file is never displaced.
  test(`C1 control (${order}): an uninspected oversized canonical is never displaced`, async () => {
    const { root, writer, dir, file } = await writerFixture();
    const alias = path.join(dir, "terminal-0003.json");
    await fs.copyFile(file, alias);
    await fs.unlink(file);
    await writer.journalTerminal(run, 3, "implement", 9, { status: "failed", error: "x".repeat(200 * 1024) });
    const before = await fs.readFile(file);
    // The read cap is terminalMaxBytes + 64 KiB, so the large canonical is never inspected.
    const box = await restart(order, root, { terminalMaxBytes: 1024 });
    assert.ok(box.listPendingTerminals().some(p => p.claim_generation === 3));
    await box.retireTerminal(run, 3);
    assert.deepEqual(await fs.readFile(file), before, "the uninspected canonical is untouched");
    // Whichever file was enrolled, the other is covered by a held scope: protection survives deletion of both.
    await fs.unlink(file);
    if (await exists(alias)) await fs.unlink(alias);
    assert.equal(await box.hasPhysicalTerminalProtection(run), true);
  });
}

async function rejectedCanonicalWithAlias() {
  const fixture = await writerFixture([3], () => 1e6);
  await fs.copyFile(fixture.file, path.join(fixture.dir, "terminal-0003.json"));
  await reject(fixture.file);
  return fixture;
}
// terminalInventoryComplete only turns true after a full scan; seed it so a boot-time
// failure is observable through the public policyInventoryCoverageAvailable().
const seedInventory = (limit?: number) => (box: Outbox) => {
  const internals = box as unknown as { terminalInventoryComplete: boolean; policyCatalogLimit: number };
  internals.terminalInventoryComplete = true;
  if (limit !== undefined) internals.policyCatalogLimit = limit;
};
const catalogOutcome = (box: Outbox) => box.listPendingTerminals().find(p => p.claim_generation === 3)?.outcome_kind;

// The displaced file inherits the winner's own admitted scope, so a tight catalog
// limit gives the same enrolled entry and protection in both orders.
test("R5: at a tight policy catalog limit both orders enroll the same entry and protection", async () => {
  const snapshots = [];
  for (const order of orders) {
    const { root, dir, file } = await rejectedCanonicalWithAlias();
    const box = await restart(order, root, { now: () => 9e6 }, seedInventory(2));
    assert.equal((await box.readTerminalJournal(run, 3))?.body.error, "private body");
    assert.ok(await exists(file));
    assert.ok(await exists(path.join(dir, "terminal-0003.json")));
    snapshots.push({
      outcome: catalogOutcome(box),
      coverage: box.policyInventoryCoverageAvailable(),
      sources: box.listPendingOutcomeSources(),
      protection: await box.hasPhysicalTerminalProtection(run),
    });
  }
  assert.equal(snapshots[0]!.outcome, undefined, "the valid alias replaces the rejected canonical");
  assert.deepEqual(snapshots[1], snapshots[0]);
});

// B1: the displaced file's protection must be identical to the forward-order one, including `since`.
async function sourcesAfterBoot(order: Order, build: (f: Awaited<ReturnType<typeof writerFixture>>) => Promise<void>) {
  const root = await fs.mkdtemp(path.join(scratch, "boot-arbitration-"));
  roots.push(root);
  const writer = new Outbox(options(root, { now: () => 1e6 }));
  await writer.init();
  await writer.journalTerminal(run, 3, "implement", 0, { status: "failed", error: "private body" });
  const dir = path.join(root, run);
  await build({ root, writer, dir, file: path.join(dir, "terminal-3.json") });
  const box = await restart(order, root, { now: () => 9e6 });
  return box.listPendingOutcomeSources();
}

test("B1a: canonical plus valid alias report identical sources in both orders", async () => {
  const build = async (f: { dir: string; file: string }) => { await fs.copyFile(f.file, path.join(f.dir, "terminal-0003.json")); };
  const forward = await sourcesAfterBoot("forward", build);
  const reversed = await sourcesAfterBoot("reversed", build);
  assert.ok(forward.length > 0);
  assert.deepEqual(reversed, forward);
});

test("B1b: rejected canonical plus valid alias report identical sources in both orders", async () => {
  const build = async (f: { dir: string; file: string }) => {
    await fs.copyFile(f.file, path.join(f.dir, "terminal-0003.json"));
    await reject(f.file);
  };
  const forward = await sourcesAfterBoot("forward", build);
  const reversed = await sourcesAfterBoot("reversed", build);
  assert.ok(forward.some(p => p.outcome_kind === "unknown"));
  assert.deepEqual(reversed, forward);
});

// N1: the logged file counts do not depend on which file wins.
test("N1: authenticated_files and rejected_files are order independent", async () => {
  for (const [rejectCanonical, authenticated, rejected] of [[false, 2, 0], [true, 1, 1]] as const) {
    for (const order of orders) {
      const { root, dir, file } = await writerFixture();
      await fs.copyFile(file, path.join(dir, "terminal-0003.json"));
      if (rejectCanonical) await reject(file);
      const rec = recordingLogger();
      await restart(order, root, { log: rec.logger });
      const line = rec.lines.find((l: any) => l.msg === "outbox terminal journal load" && l.outcome === "complete") as any;
      assert.equal(line.authenticated_files, authenticated, `${order} rejectCanonical=${rejectCanonical}`);
      assert.equal(line.rejected_files, rejected, `${order} rejectCanonical=${rejectCanonical}`);
    }
  }
});

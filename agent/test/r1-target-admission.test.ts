import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { Outbox } from "../src/outbox.js";
import { Worker } from "../src/worker.js";
import { makeScratchRoot } from "./fixture-repo.js";
import { nullLogger } from "./helpers.js";

const run = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa";
async function fixture(t: { after(fn: () => Promise<void>): void }) {
  const dir = makeScratchRoot("r1-target-", await fs.realpath(os.tmpdir()));
  t.after(() => fs.rm(dir, { recursive: true, force: true }));
  const root = path.join(dir, "outbox");
  const outbox = new Outbox({ root, log: nullLogger(), maxBytes: 1e6, runMaxBytes: 1e6, retentionMs: 1 });
  await outbox.init();
  return { outbox, root };
}

it("R1 direct ordinary admission completes through 4100 retired directories and unrelated unknown", async t => {
  const { outbox, root } = await fixture(t);
  for (let i = 0; i < 4100; i++) await fs.mkdir(path.join(root, i.toString(16).padStart(8, "0") + "-0000-4000-8000-000000000000"));
  const unrelated = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb";
  await fs.mkdir(path.join(root, unrelated));
  await fs.writeFile(path.join(root, unrelated, "terminal-1.json"), "unknown");
  assert.equal(await outbox.hasPendingPolicyOutcome(run), false);
  assert.equal((await outbox.journalTerminal(run, 1, "running", 0, { status: "completed" })).journaled, true);
});

it("R1 completed uncertain targets release proof interests and preserve unrelated ordinary admission", async t => {
  const { outbox, root } = await fixture(t);
  const ids = Array.from({ length: 40 }, (_, i) =>
    i.toString(16).padStart(8, "0") + "-aaaa-4aaa-8aaa-aaaaaaaaaaaa");
  for (const id of ids) {
    await fs.mkdir(path.join(root, id));
    const source = path.join(root, id, "terminal-unsafe.json");
    await fs.writeFile(source, "unknown");
    assert.equal(await outbox.hasPendingPolicyOutcome(id), true);
    await fs.unlink(source);
  }
  assert.equal(await outbox.hasPendingPolicyOutcome(run), false,
    "completed known-other-run uncertainties must not exhaust live proof interests");
  assert.equal((await outbox.journalTerminal(run, 1, "running", 0, { status: "completed" })).journaled, true);
  for (const id of ids) {
    assert.equal(await outbox.hasPendingPolicyOutcome(id), true,
      "source disappearance must not release the affected run's evidence");
  }
});

it("R1 unclassified filenames and alias overflow share capacity with producer reservations", async t => {
  const { outbox, root } = await fixture(t);
  await fs.mkdir(path.join(root, run));
  await fs.writeFile(path.join(root, run, "terminal-unsafe.json"), "unknown");
  assert.equal(await outbox.hasPendingPolicyOutcome(run), true);
  const aliased = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb";
  for (let i = 0; i < 65; i++) {
    const alias = aliased.split("").map((c, n) =>
      n < 7 && (i & (1 << n)) ? c.toUpperCase() : c).join("");
    await fs.mkdir(path.join(root, alias));
  }
  assert.equal(await outbox.hasPendingPolicyOutcome(aliased), true);
  const producer = "cccccccc-cccc-4ccc-8ccc-cccccccccccc";
  const tokens: symbol[] = [];
  for (let generation = 1; generation <= 1024; generation++) {
    const token = outbox.reservePolicyFlight(producer, generation);
    if (token !== undefined) tokens.push(token);
  }
  assert.equal(tokens.length, 1022,
    "both retained unclassified physical scopes consume exactly one shared identity slot");
  assert.ok(tokens.length > 0, "bounded attributed evidence leaves producer capacity available");
  for (const token of tokens) outbox.releasePolicyFlight(token);
  await fs.unlink(path.join(root, run, "terminal-unsafe.json"));
  assert.equal(await outbox.hasPendingPolicyOutcome(run), true,
    "releasing producer reservations cannot release an unrelated unclassified source");
});

it("R1 validates an already scanned empty alias even when root is unchanged", async t => {
  const { outbox, root } = await fixture(t);
  const alias = run.toUpperCase();
  await fs.mkdir(path.join(root, alias));
  const before = await fs.stat(root, { bigint: true });
  const open = fs.opendir.bind(fs);
  let mutated = false;
  t.mock.method(fs, "opendir", async (file: Parameters<typeof fs.opendir>[0]) => {
    const opened = await open(file);
    return new Proxy(opened, { get(target, key) {
      if (key === "close") return async () => {
        await target.close();
        if (String(file) === path.join(root, alias) && !mutated) {
          mutated = true;
          await fs.writeFile(path.join(root, alias, "terminal-1.json"), "unknown");
        }
      };
      const value = Reflect.get(target, key);
      return typeof value === "function" ? value.bind(target) : value;
    } });
  });
  assert.equal(await outbox.hasPendingPolicyOutcome(run), true);
  assert.equal(mutated, true);
  assert.equal((await fs.stat(root, { bigint: true })).mtimeNs, before.mtimeNs);
  await fs.unlink(path.join(root, alias, "terminal-1.json"));
  assert.equal(await outbox.hasPendingPolicyOutcome(run), true, "observed unknown survives disappearance");
});

it("R1 full unrelated catalog permits ordinary reads and defers new enrollment until capacity releases", async t => {
  const { outbox, root } = await fixture(t);
  assert.equal((await outbox.journalTerminal(run, 1, "running", 0, { status: "completed" })).journaled, true);
  const unrelated = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb";
  const tokens = Array.from({ length: 1024 }, (_, i) => outbox.reservePolicyFlight(unrelated, i + 1));
  assert.ok(tokens.every(token => token !== undefined));
  assert.equal(await outbox.hasPendingPolicyOutcome(run), false);
  assert.equal((await outbox.journalTerminal(run, 3, "running", 0, { status: "completed" })).journaled, true);
  await fs.writeFile(path.join(root, run, "terminal-2.json"), "unknown");
  assert.equal(await outbox.hasPendingPolicyOutcome(run), true);
  const empty = "dddddddd-dddd-4ddd-8ddd-dddddddddddd";
  await fs.mkdir(path.join(root, empty));
  assert.equal(await outbox.hasPendingPolicyOutcome(empty), false,
    "a genuinely full catalog cannot block an empty sibling");
  await fs.writeFile(path.join(root, empty, "manifest.json"), "{}");
  assert.equal(await outbox.hasPendingPolicyOutcome(empty), false,
    "ignored ordinary files cannot require catalog enrollment");
  assert.equal(await outbox.readTerminalJournal(run, 1).then(j => j?.body.status), "completed");
  outbox.releasePolicyFlight(tokens[0]!);
  assert.equal(await outbox.hasPendingPolicyOutcome(run), true);
  assert.ok(outbox.listPendingTerminals().some(p => p.run_id === run && p.claim_generation === 2 && p.outcome_kind === "unknown"));
  await fs.unlink(path.join(root, run, "terminal-2.json"));
  assert.equal(await outbox.hasPendingPolicyOutcome(run), true,
    "an enrolled obligation survives physical disappearance");
});

it("R1 synchronous selection after an absence yield blocks ordinary installation", async t => {
  const { outbox } = await fixture(t);
  const token = outbox.reservePolicyFlight(run, 1)!;
  const original = outbox.hasPendingPolicyOutcome.bind(outbox);
  outbox.hasPendingPolicyOutcome = async id => {
    const result = await original(id);
    assert.equal(result, false);
    assert.equal(outbox.selectPolicyFlight(token, "cccccccc-cccc-4ccc-8ccc-cccccccccccc"), true);
    return result;
  };
  assert.deepEqual(await outbox.journalTerminal(run, 1, "running", 0, { status: "completed" }),
    { journaled: false, reason: "winner_unavailable" });
});

it("R1 unclassifiable source retains a bounded target barrier without inventing a generation", async t => {
  const { outbox, root } = await fixture(t);
  await fs.mkdir(path.join(root, run));
  const file = path.join(root, run, "terminal-unsafe.json");
  await fs.writeFile(file, "unknown");
  assert.equal(await outbox.hasPendingPolicyOutcome(run), true);
  await fs.unlink(file);
  assert.equal(await outbox.hasPendingPolicyOutcome(run), true);
  assert.deepEqual(outbox.listPendingTerminals(), []);
  const ordinary = "dddddddd-dddd-4ddd-8ddd-dddddddddddd";
  assert.equal(await outbox.hasPendingPolicyOutcome(ordinary), false);
});

it("R1 matching symlink evidence survives removal without following its source", async t => {
  const { outbox, root } = await fixture(t);
  await fs.mkdir(path.join(root, run));
  const source = path.join(root, "unrelated-contents");
  await fs.writeFile(source, "unreadable policy");
  const alias = path.join(root, run, "terminal-1.json");
  await fs.symlink(source, alias);
  assert.equal(await outbox.hasPendingPolicyOutcome(run), true);
  assert.equal(await fs.readFile(source, "utf8"), "unreadable policy");
  await fs.unlink(alias);
  assert.equal(await outbox.hasPendingPolicyOutcome(run), true);
});

it("R1 alias binding overflow protects only the matching target", async t => {
  const { outbox, root } = await fixture(t);
  for (let i = 0; i < 65; i++) {
    const alias = run.split("").map((c, n) => n < 7 && (i & (1 << n)) ? c.toUpperCase() : c).join("");
    await fs.mkdir(path.join(root, alias));
  }
  assert.equal(await outbox.hasPendingPolicyOutcome(run), true);
  assert.equal(await outbox.hasPendingPolicyOutcome("dddddddd-dddd-4ddd-8ddd-dddddddddddd"), false);
});

it("R1 exact-target interests have fixed capacity and release completed cursors", async t => {
  const { outbox, root } = await fixture(t);
  const open = fs.opendir.bind(fs);
  let opens = 0, closes = 0;
  let resume!: () => void;
  const gate = new Promise<void>(r => { resume = r; });
  t.mock.method(fs, "opendir", async (file: Parameters<typeof fs.opendir>[0]) => {
    const opened = await open(file);
    assert.equal(String(file), root);
    opens++;
    return new Proxy(opened, { get(target, key) {
      if (key === "read") return async () => { await gate; return target.read(); };
      if (key === "close") return async () => { closes++; await target.close(); };
      const value = Reflect.get(target, key);
      return typeof value === "function" ? value.bind(target) : value;
    } });
  });
  const id = (i: number) => i.toString(16).padStart(8, "0") + "-0000-4000-8000-000000000000";
  const calls = Array.from({ length: 32 }, (_, i) => outbox.hasPendingPolicyOutcome(id(i)));
  try { assert.equal(await outbox.hasPendingPolicyOutcome(id(33)), true); }
  finally { resume(); }
  assert.ok((await Promise.all(calls)).every(value => value === false));
  assert.equal(opens, 32);
  assert.equal(closes, 32);
  assert.equal(await outbox.hasPendingPolicyOutcome(id(33)), false);
  assert.equal(opens, 33);
  assert.equal(closes, 33);
});

it("R1 coalesces target demands and closes cursors on read errors", async t => {
  const { outbox, root } = await fixture(t);
  const open = fs.opendir.bind(fs);
  let opens = 0, closes = 0;
  let resume!: () => void;
  const gate = new Promise<void>(r => { resume = r; });
  t.mock.method(fs, "opendir", async (file: Parameters<typeof fs.opendir>[0]) => {
    const opened = await open(file);
    if (String(file) !== root) return opened;
    opens++;
    return new Proxy(opened, { get(target, key) {
      if (key === "read") return async () => { await gate; throw new Error("read failure"); };
      if (key === "close") return async () => { closes++; await target.close(); };
      const value = Reflect.get(target, key);
      return typeof value === "function" ? value.bind(target) : value;
    } });
  });
  const calls = Array.from({ length: 100 }, () => outbox.hasPendingPolicyOutcome(run));
  resume();
  assert.ok((await Promise.all(calls)).every(Boolean));
  assert.equal(opens, 1);
  assert.equal(closes, 1);
  assert.equal(await outbox.hasPendingPolicyOutcome(run), true);
  assert.equal(opens, 2, "failed interest is released");
});

for (const fileName of ["terminal-9007199254740993.json", "terminal-" + String.fromCharCode(1).repeat(230)]) {
  it("R1 opaque unclassified source shares the final slot and protects finalize/subtree: " + JSON.stringify(fileName.slice(0, 40)), async t => {
    const { outbox, root } = await fixture(t);
    assert.equal((await outbox.journalFinalize(run, 1)).written, true);
    const other = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb";
    const tokens = Array.from({ length: 1023 }, (_, i) => outbox.reservePolicyFlight(other, i + 1));
    assert.ok(tokens.every(token => token !== undefined));
    await fs.writeFile(path.join(root, run, fileName), "unknown");
    assert.equal(await outbox.hasPendingPolicyOutcome(run), true);
    assert.equal(outbox.reservePolicyFlight(other, 1024), undefined,
      "unclassified identity consumes the final shared slot");
    assert.deepEqual(outbox.listPendingTerminals(), [], "no source generation was fabricated");
    await fs.unlink(path.join(root, run, fileName));
    assert.equal(await outbox.hasPhysicalTerminalProtection(run), true);
    assert.equal(await outbox.confirmReportsAbsent(run, 1), false);
    assert.deepEqual(outbox.listPendingFinalizes(), []);
    assert.deepEqual(outbox.listPendingFinalizeGenerations(), []);
    assert.deepEqual(outbox.listPendingFinalizeGenerations(true), []);
    assert.equal(await outbox.retireFinalize(run, 1), false);
    await outbox.retireRun(run);
    assert.ok((await fs.stat(path.join(root, run))).isDirectory());
    assert.equal(await outbox.hasPendingPolicyOutcome("dddddddd-dddd-4ddd-8ddd-dddddddddddd"), false);
    for (const token of tokens) outbox.releasePolicyFlight(token!);
    assert.equal(await outbox.hasPendingPolicyOutcome(run), true);
  });
}

it("R1 a changed represented ordinary lifetime transfers uncertainty into the shared catalog", async t => {
  const { outbox, root } = await fixture(t);
  assert.equal((await outbox.journalTerminal(run, 1, "running", 0, { status: "completed" })).journaled, true);
  await fs.writeFile(path.join(root, run, "terminal-1.json"), "unknown");
  assert.equal(await outbox.hasPendingPolicyOutcome(run), true);
  await fs.unlink(path.join(root, run, "terminal-1.json"));
  assert.equal(await outbox.hasPendingPolicyOutcome(run), true);
  const other = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb";
  const tokens = Array.from({ length: 1024 }, (_, i) => outbox.reservePolicyFlight(other, i + 1));
  assert.equal(tokens.filter(token => token !== undefined).length, 1023);
  for (const token of tokens) if (token) outbox.releasePolicyFlight(token);
});

it("R1 opaque scopes remain pending in generation-free depth and fallback readers", async t => {
  const { outbox, root } = await fixture(t);
  await fs.mkdir(path.join(root, run));
  await fs.writeFile(path.join(root, run, "terminal-unsafe.json"), "unknown");
  assert.equal(outbox.depthFor(run), undefined);
  assert.equal(await outbox.hasPendingPolicyOutcome(run), true);
  assert.equal(outbox.hasPendingTerminal(run, 123), true);
  const depth = outbox.depthFor(run);
  assert.equal(depth?.pendingTerminal, 1);
  assert.equal(depth?.policyPendingSourceGeneration, undefined);
  assert.equal(depth?.policyPendingOutcomeId, undefined);
  assert.equal(depth?.blockedReason, undefined);
  assert.equal(outbox.listPendingTerminals().length, 0, "an unknown source generation is never fabricated as a wire tuple");
  assert.equal(outbox.policyInventoryCoverageAvailable(), false);
});

it("R1 demand byte allowance spans mutation retries", async t => {
  const { outbox, root } = await fixture(t);
  for (const generation of [1, 2])
    assert.equal((await outbox.journalTerminal(run, generation, "running", 0,
      { status: "completed", error_message: "x".repeat(1024) })).journaled, true);
  const directory = path.join(root, run);
  let totalBytes = 0;
  for (const generation of [1, 2]) {
    const file = path.join(directory, `terminal-${generation}.json`);
    totalBytes += (await fs.stat(file)).size;
    await fs.rename(file, path.join(directory, `parked-${generation}`));
  }
  const quota = totalBytes - 1;
  const reader = new Outbox({ root, log: nullLogger(), maxBytes: quota, runMaxBytes: quota, retentionMs: 1 });
  await reader.init();
  assert.equal(reader.listPendingTerminals().length, 0);
  await fs.rename(path.join(directory, "parked-1"), path.join(directory, "terminal-1.json"));
  const open = fs.opendir.bind(fs);
  let mutated = false;
  t.mock.method(fs, "opendir", async (file: Parameters<typeof fs.opendir>[0]) => {
    const opened = await open(file);
    return new Proxy(opened, { get(target, key) {
      if (key === "close") return async () => {
        await target.close();
        if (String(file) === directory && !mutated) {
          mutated = true;
          await fs.rename(path.join(directory, "parked-2"), path.join(directory, "terminal-2.json"));
        }
      };
      const value = Reflect.get(target, key);
      return typeof value === "function" ? value.bind(target) : value;
    } });
  });
  assert.equal(await reader.hasPendingPolicyOutcome(run), true,
    "a mutation retry must not authenticate two bodies exceeding one demand's byte quota");
  assert.equal(mutated, true);
  assert.equal(reader.listPendingTerminals().find(source => source.claim_generation === 2)?.outcome_kind, "unknown");
  assert.equal((await reader.readTerminalJournal(run, 2))?.body.status, "completed",
    "the deferred ordinary candidate remains resolvable with a fresh bounded budget");
  assert.equal(await reader.hasPendingPolicyOutcome(run), false);
});

it("R1 heartbeat includes generation-free scope depth without inventing source authority", async t => {
  const { outbox, root } = await fixture(t);
  await fs.mkdir(path.join(root, run));
  await fs.writeFile(path.join(root, run, "terminal-unsafe.json"), "unknown");
  assert.equal(await outbox.hasPendingPolicyOutcome(run), true);
  const worker = Object.assign(Object.create(Worker.prototype), { outbox }) as Worker;
  const entries = (worker as unknown as { outboxEntries(): Record<string, unknown>[] | undefined }).outboxEntries();
  assert.equal(entries?.find(entry => entry.run_id === run)?.pending_terminal, 1);
  assert.equal(entries?.[0]?.policy_pending_source_generation, undefined);
  assert.equal(entries?.[0]?.policy_pending_outcome_id, undefined);
  assert.equal(outbox.listPendingTerminals().length, 0);
  assert.equal(outbox.policyInventoryCoverageAvailable(), false);
});

it("R1 local reconciliation returns an incomplete continuation within 256 visited entries", async t => {
  const { outbox, root } = await fixture(t);
  await fs.mkdir(path.join(root, run));
  for (let i = 0; i < 600; i++) await fs.writeFile(path.join(root, run, `ignored-${i}`), "");
  for (let i = 0; i < 300; i++) await fs.writeFile(path.join(root, `ignored-${i}`), "");
  const open = fs.opendir.bind(fs);
  let visited = 0;
  t.mock.method(fs, "opendir", async (file: Parameters<typeof fs.opendir>[0]) => {
    const directory = await open(file);
    return new Proxy(directory, { get(target, key) {
      if (key === "read") return async () => {
        const entry = await target.read();
        if (entry) visited++;
        return entry;
      };
      const value = Reflect.get(target, key);
      return typeof value === "function" ? value.bind(target) : value;
    } });
  });
  const resolver = outbox as unknown as {
    scanTargetPolicyPages?: (id: string) => AsyncGenerator<void, boolean, void>;
    scanTargetPolicy(id: string): Promise<boolean>;
  };
  // Old code has a single unbounded invocation; keep its behavioral assertion
  // reachable rather than treating a missing post-fix helper as a red.
  const pages = resolver.scanTargetPolicyPages?.(run) ?? {
    next: async () => ({ done: true as const, value: await resolver.scanTargetPolicy(run) }),
    return: async (value: boolean) => ({ done: true as const, value }),
  };
  let previous = 0, steps = 0;
  try {
    for (;;) {
      const result = await pages.next();
      steps++;
      assert.ok(visited - previous <= 256, `one invocation visited ${visited - previous} entries`);
      previous = visited;
      if (result.done) { assert.equal(result.value, false); break; }
      await new Promise<void>(resolve => setImmediate(resolve));
    }
  } finally { await pages.return(true); }
  assert.ok(steps >= 4);
  assert.ok(visited >= 901, "all ignored root/run entries are eventually covered without losing the demand");
});

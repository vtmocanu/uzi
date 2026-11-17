import { afterEach, beforeEach, describe, it, type TestContext } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { randomUUID } from "node:crypto";
import fs from "node:fs";
import fsp from "node:fs/promises";
import path from "node:path";
import {
  GitCache, RETIRE_BINDING_MAX_PER_BARE, parseRetireBinding, retireBindingKey,
  type RetireBinding, type RetireBindingStep, type RetireDispositionClass,
} from "../src/git.js";
import { formatAttemptId } from "../src/attempt-path.js";
import { latchResidueQuarantine, resetResidueQuarantineForTests } from "../src/residue-quarantine.js";
import type { Logger } from "../src/log.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { noProofReseed, nullLogger, testGitCacheOptions } from "./helpers.js";

// issue #4263 M2: the durable retirement-scratch binding written by retireRunnerClone on the EXDEV branch.
// Real git bares and clones in temp dirs; the cross-device move is simulated by failing a rename into the holding
// root, and the uid split by the GitCache retentionDelete seam. Every observation of "the binding at this step" is
// taken inside the retireBindingStep hook, so the recovery table is checked at each point rather than inferred.

const ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" };
const ATTEMPT = formatAttemptId(new Date("2026-11-05T10:10:10Z"), 1, "0123456789abcdef");

let fx: Fixture;
const runnerRoot = (): string => path.join(fx.dataDir, "runner");
const holdingRoot = (): string => path.join(fx.dataDir, "runner-quarantine");

interface Logs { warn: string[]; info: string[] }
function mk(opts: { step?: (s: RetireBindingStep) => void | Promise<void>; split?: boolean; logs?: Logs } = {}): GitCache {
  const logger: Logger = {
    ...nullLogger(),
    warn: (event: string) => { opts.logs?.warn.push(event); },
    info: (event: string) => { opts.logs?.info.push(event); },
    child: () => logger,
  };
  return new GitCache(fx.dataDir, logger, undefined, testGitCacheOptions({
    ...(opts.step ? { retireBindingStep: opts.step } : {}),
    ...(opts.split === undefined ? {} : { retentionDelete: { split: opts.split } }),
  }));
}

/** Model the docker lane: a rename into the holding root is cross-device. */
function exdev(t: TestContext): void {
  const rename = fsp.rename.bind(fsp);
  t.mock.method(fsp, "rename", async (...args: Parameters<typeof fsp.rename>) => {
    if (String(args[1]).startsWith(holdingRoot())) throw Object.assign(new Error("cross device"), { code: "EXDEV" });
    return rename(...args);
  });
}

interface Seed { bare: string; branch: string; clonePath: string; owner: string }
async function seed(c: GitCache, iid: number, owner = randomUUID()): Promise<Seed> {
  const bare = await c.ensureClone(fx.originPath);
  fs.chmodSync(path.dirname(bare), 0o700);
  fs.chmodSync(bare, 0o700);
  const branch = `agent/issue-${iid}`;
  const clone = await c.createOrAttachRunnerClone(bare, iid, noProofReseed, owner, false, undefined,
    { attemptId: ATTEMPT, isLive: () => false, beforeSeed: async () => {} });
  await c.markRecoveryCapture(bare, clone.path, branch, owner, ATTEMPT);
  fs.writeFileSync(path.join(clone.path, "owner.txt"), "owner bytes\n");
  return { bare, branch, clonePath: clone.path, owner };
}

const gitIn = (bare: string, ...args: string[]): string => execFileSync("git", ["-C", bare, ...args], { encoding: "utf8", env: ENV }).trim();

function bindings(bare: string): Array<{ key: string; raw: string; binding?: RetireBinding }> {
  let out = "";
  try { out = gitIn(bare, "config", "--local", "--null", "--get-regexp", "^uzi-retire-scratch\\."); } catch { return []; }
  return out.split("\0").filter(Boolean).map(item => {
    const nl = item.indexOf("\n");
    const key = item.slice(0, nl), raw = item.slice(nl + 1);
    const uuid = /^uzi-retire-scratch\.([0-9a-f-]{36})\.binding$/.exec(key)?.[1] ?? "";
    const parsed = parseRetireBinding(uuid, raw);
    return { key, raw, ...(parsed.ok ? { binding: parsed.binding } : {}) };
  });
}
const phase = (bare: string): string => bindings(bare)[0]?.binding?.phase ?? "absent";
const parents = (): string[] => fs.readdirSync(runnerRoot()).filter(n => n.startsWith(".retire-"));
const journal = (bare: string, branch: string): string => { try { return gitIn(bare, "config", "--local", "--get", `uzi-recovery.${branch}.clone`); } catch { return ""; } };
const ledger = (bare: string, branch: string): string[] => { try { return gitIn(bare, "config", "--local", "--get-all", `uzi-attempts.${branch}.entry`).split("\n"); } catch { return []; } };
const hasRetiredEntry = (bare: string, branch: string): boolean => ledger(bare, branch).some(raw => (JSON.parse(raw) as { state: string }).state === "retired");

beforeEach(() => { fx = makeFixture(); });
afterEach(() => { resetResidueQuarantineForTests(); fx.cleanup(); });

describe("happy paths", () => {
  it("owner_terminal on a simulated split settles the binding, keeps the parent and runs no rm", async (t) => {
    exdev(t);
    const rm = fsp.rm.bind(fsp);
    const removed: string[] = [];
    t.mock.method(fsp, "rm", async (...args: Parameters<typeof fsp.rm>) => { removed.push(String(args[0])); return rm(...args); });
    const seen: Array<[RetireBindingStep, string]> = [];
    const logs: Logs = { warn: [], info: [] };
    let c!: GitCache;
    let s!: Seed;
    const lost: string[] = [];
    c = mk({ split: true, logs, step: step => {
      seen.push([step, phase(s.bare)]);
      // The clone is never lost: it is canonical, or inside exactly one scratch parent.
      const inParent = parents().some(p => fs.existsSync(path.join(runnerRoot(), p, "clone")));
      if (fs.existsSync(s.clonePath) === inParent) lost.push(step);
    } });
    s = await seed(c, 4263);
    await c.retireRunnerClone(s.bare, s.clonePath, s.branch, s.owner, { discard: true, attemptId: ATTEMPT, dispositionClass: "owner_terminal" });
    assert.deepEqual(lost, []);
    const [b] = bindings(s.bare);
    assert.equal(bindings(s.bare).length, 1);
    assert.equal(b!.binding!.phase, "settled");
    assert.equal(b!.binding!.class, "owner_terminal");
    assert.equal(b!.binding!.attemptId, ATTEMPT);
    assert.equal(b!.binding!.runId, s.owner);
    assert.equal(b!.binding!.clonePath, s.clonePath);
    assert.equal(b!.binding!.pod, "");
    const [parent] = parents();
    assert.equal(b!.key, retireBindingKey(parent!.slice(".retire-".length)));
    assert.equal(b!.binding!.parentIno, String(fs.statSync(path.join(runnerRoot(), parent!), { bigint: true }).ino));
    assert.equal(b!.binding!.cloneIno, String(fs.statSync(path.join(runnerRoot(), parent!, "clone"), { bigint: true }).ino));
    assert.equal(fs.existsSync(s.clonePath), false);
    assert.equal(journal(s.bare, s.branch), "", "journal cleared");
    assert.equal(hasRetiredEntry(s.bare, s.branch), true);
    assert.deepEqual(removed.filter(p => p.includes(".retire-")), [], "no rm of the parent");
    assert.ok(logs.info.includes("retire scratch parent deferred to verified epoch"));
    const phaseAt = (step: RetireBindingStep): string | undefined => seen.find(([name]) => name === step)?.[1];
    for (const step of ["parent-created", "parent-pinned", "root-synced", "cap-checked", "intent-write"] as const) assert.equal(phaseAt(step), "absent", step);
    for (const step of ["intent-written", "renamed", "move-verified", "journal-cleared", "ledger-appended", "config-sync", "settle-write"] as const)
      assert.equal(phaseAt(step), "intent", step);
    assert.equal(phaseAt("settled"), "settled");
  });

  it("owner_terminal on a single uid writes removing before the rm, then unsets and removes the parent", async (t) => {
    exdev(t);
    const seen: Array<[RetireBindingStep, string, boolean]> = [];
    let s!: Seed;
    const c = mk({ split: false, step: step => { seen.push([step, phase(s.bare), parents().length > 0]); } });
    s = await seed(c, 4264);
    await c.retireRunnerClone(s.bare, s.clonePath, s.branch, s.owner, { discard: true, attemptId: ATTEMPT, dispositionClass: "owner_terminal" });
    assert.deepEqual(bindings(s.bare), []);
    assert.deepEqual(parents(), []);
    assert.equal(fs.existsSync(s.clonePath), false);
    assert.equal(hasRetiredEntry(s.bare, s.branch), true);
    const at = (step: RetireBindingStep) => seen.find(([name]) => name === step)!;
    assert.deepEqual(at("removing-written").slice(1), ["removing", true], "removing is durable while the parent still exists");
    assert.deepEqual(at("removed").slice(1), ["removing", false]);
    assert.equal(at("unset")[1], "removing");
    assert.equal(seen.some(([name]) => name === "settle-write"), false, "a single-uid retire never settles");
  });

  for (const [cls, discard] of [["owner_terminal_covered", true], ["other", false]] as const) {
    it(`${cls} never settles on a split`, async (t) => {
      exdev(t);
      const c = mk({ split: true });
      const s = await seed(c, 4265);
      await c.retireRunnerClone(s.bare, s.clonePath, s.branch, s.owner, { discard, attemptId: ATTEMPT, dispositionClass: cls });
      assert.equal(phase(s.bare), "intent");
      assert.equal(parents().length, 1);
      assert.equal(fs.existsSync(s.clonePath), false);
    });
  }

  it("foreign orphan (other, discard false) on a single uid: removing then unset, quarantine copy retained", async (t) => {
    exdev(t);
    const c = mk({ split: false });
    const s = await seed(c, 4266);
    await c.retireRunnerClone(s.bare, s.clonePath, s.branch, s.owner, { discard: false, attemptId: ATTEMPT, dispositionClass: "other" });
    assert.deepEqual(bindings(s.bare), []);
    assert.deepEqual(parents(), []);
    assert.equal(fs.readdirSync(holdingRoot()).length, 1, "the copy-before-free quarantine is retained");
  });

  it("completion success: removing before the rm, unset after the metadata, parent gone", async (t) => {
    exdev(t);
    const seen: Array<[RetireBindingStep, string]> = [];
    let s!: Seed;
    const c = mk({ step: step => { seen.push([step, phase(s.bare)]); } });
    s = await seed(c, 4267);
    const head = await c.worktreeHead(s.clonePath);
    assert.ok(head);
    await c.retireRunnerClone(s.bare, s.clonePath, s.branch, s.owner,
      { discard: true, attemptId: ATTEMPT, dispositionClass: "completion", completionSource: { expectedHead: head, canDelete: () => true } });
    assert.deepEqual(bindings(s.bare), []);
    assert.deepEqual(parents(), []);
    assert.equal(seen.some(([name]) => name === "settle-write"), false, "completion never settles");
    assert.equal(seen.find(([name]) => name === "removing-written")![1], "removing");
    assert.equal(seen.find(([name]) => name === "unset")![1], "removing");
    assert.equal(hasRetiredEntry(s.bare, s.branch), true);
  });

  it("completion: a partial rm then a successful rename-back keeps removing", async (t) => {
    exdev(t);
    const rm = fsp.rm.bind(fsp);
    let s!: Seed;
    t.mock.method(fsp, "rm", async (...args: Parameters<typeof fsp.rm>) => {
      const target = String(args[0]);
      if (path.basename(target).startsWith(".retire-")) {
        await rm(path.join(target, "clone", "owner.txt"));
        throw Object.assign(new Error("partial"), { code: "EIO" });
      }
      return rm(...args);
    });
    const c = mk();
    s = await seed(c, 4268);
    const head = await c.worktreeHead(s.clonePath);
    assert.ok(head);
    const journalBefore = journal(s.bare, s.branch);
    await assert.rejects(c.retireRunnerClone(s.bare, s.clonePath, s.branch, s.owner,
      { discard: true, attemptId: ATTEMPT, dispositionClass: "completion", completionSource: { expectedHead: head, canDelete: () => true } }), /partial/);
    assert.equal(fs.existsSync(s.clonePath), true, "renamed back");
    assert.equal(phase(s.bare), "removing", "removing is kept even though the rename-back succeeded");
    assert.equal(journal(s.bare, s.branch), journalBefore);
  });
});

describe("failures before the move fail closed", () => {
  const classes: Array<[RetireDispositionClass, boolean]> = [
    ["completion", true], ["verified_successor", true], ["owner_terminal", true], ["other", false],
  ];
  for (const [cls, discard] of classes) for (const fault of ["intent-write", "intent-sync"] as const) {
    it(`${cls}: ${fault} failure keeps the canonical clone, journal and ledger and leaves no parent`, async (t) => {
      exdev(t);
      let s!: Seed;
      const c = mk({ step: step => { if (step === fault) throw new Error(`injected ${fault}`); } });
      s = await seed(c, 4270);
      const before = { journal: journal(s.bare, s.branch), ledger: ledger(s.bare, s.branch) };
      const head = await c.worktreeHead(s.clonePath);
      assert.ok(head);
      const completionSource = cls === "completion" ? { completionSource: { expectedHead: head, canDelete: () => true } } : {};
      // verified_successor needs a predecessor set and is exercised for real in runner-clone-retire.test.ts; here it
      // shares the owner path up to the intent write, so the plain discard stands in for it.
      await assert.rejects(c.retireRunnerClone(s.bare, s.clonePath, s.branch, s.owner,
        { discard, attemptId: ATTEMPT, dispositionClass: cls, ...completionSource }), new RegExp(`injected ${fault}`));
      assert.equal(fs.existsSync(path.join(s.clonePath, "owner.txt")), true);
      assert.deepEqual(parents(), []);
      assert.equal(journal(s.bare, s.branch), before.journal);
      assert.deepEqual(ledger(s.bare, s.branch), before.ledger);
      if (fault === "intent-write") assert.deepEqual(bindings(s.bare), []);
    });
  }
});

describe("crash and failure injection recovery table", () => {
  type Where = "canonical" | "scratch" | "gone";
  interface Row { step: RetireBindingStep; split: boolean; rejects: boolean; bind: string[]; where: Where }
  const pre = (step: RetireBindingStep, split: boolean, bind = ["absent"]): Row => ({ step, split, rejects: true, bind, where: "canonical" });
  const rows: Row[] = [];
  for (const split of [true, false]) {
    for (const step of ["parent-created", "parent-pinned", "root-synced", "cap-checked", "intent-write"] as const) rows.push(pre(step, split));
    rows.push(pre("intent-sync", split, ["absent", "intent"]), pre("intent-written", split, ["intent"]));
    for (const step of ["renamed", "move-verified", "journal-cleared", "ledger-appended"] as const)
      rows.push({ step, split, rejects: true, bind: ["intent"], where: "scratch" });
  }
  // Soft metadata steps after the move: the retire still completes and the binding is left where it was.
  for (const step of ["rename-dir-sync", "config-sync", "settle-write"] as const) rows.push({ step, split: true, rejects: false, bind: ["intent"], where: "scratch" });
  rows.push({ step: "settle-sync", split: true, rejects: false, bind: ["settled", "intent"], where: "scratch" });
  rows.push({ step: "settled", split: true, rejects: false, bind: ["settled"], where: "scratch" });
  for (const step of ["rename-dir-sync", "config-sync"] as const) rows.push({ step, split: false, rejects: false, bind: ["absent"], where: "gone" });
  rows.push({ step: "removing-write", split: false, rejects: false, bind: ["intent"], where: "scratch" });
  for (const step of ["removing-sync", "removing-written"] as const) rows.push({ step, split: false, rejects: false, bind: ["removing"], where: "scratch" });
  for (const step of ["removed", "root-synced-after-rm", "unset"] as const) rows.push({ step, split: false, rejects: false, bind: ["removing"], where: "gone" });
  for (const step of ["unset-sync", "unset-done"] as const) rows.push({ step, split: false, rejects: false, bind: ["absent"], where: "gone" });

  for (const row of rows) {
    it(`${row.split ? "split" : "single uid"}: a fault at ${row.step} leaves ${row.bind.join("|")} and the clone ${row.where}`, async (t) => {
      exdev(t);
      let s!: Seed;
      const c = mk({ split: row.split, step: step => { if (step === row.step) throw new Error(`injected ${row.step}`); } });
      s = await seed(c, 4271);
      const before = { journal: journal(s.bare, s.branch), ledger: ledger(s.bare, s.branch) };
      const run = c.retireRunnerClone(s.bare, s.clonePath, s.branch, s.owner, { discard: true, attemptId: ATTEMPT, dispositionClass: "owner_terminal" });
      if (row.rejects) await assert.rejects(run, new RegExp(`injected ${row.step}`)); else await run;
      assert.ok(row.bind.includes(phase(s.bare)), `binding is ${phase(s.bare)}`);
      const inParent = parents().filter(p => fs.existsSync(path.join(runnerRoot(), p, "clone")));
      if (row.where === "canonical") {
        assert.equal(fs.existsSync(path.join(s.clonePath, "owner.txt")), true);
        assert.deepEqual(parents(), []);
        assert.equal(journal(s.bare, s.branch), before.journal);
        assert.deepEqual(ledger(s.bare, s.branch), before.ledger);
      } else if (row.where === "scratch") {
        assert.equal(fs.existsSync(s.clonePath), false);
        assert.equal(inParent.length, 1, "the clone is inside its scratch parent");
      } else {
        assert.deepEqual(parents(), []);
        assert.equal(fs.existsSync(s.clonePath), false);
      }
    });
  }
});

describe("arrivals between intent and settle", () => {
  it("terminal protection arriving after the move leaves intent and the parent", async (t) => {
    exdev(t);
    const c = mk({ split: true });
    const s = await seed(c, 4272);
    const real = c.hasPhysicalTerminalProtection.bind(c);
    let calls = 0;
    t.mock.method(c, "hasPhysicalTerminalProtection", async (id: string) => (++calls > 1 ? true : real(id)));
    await c.retireRunnerClone(s.bare, s.clonePath, s.branch, s.owner, { discard: true, attemptId: ATTEMPT, dispositionClass: "owner_terminal" });
    assert.equal(phase(s.bare), "intent");
    assert.equal(parents().length, 1);
  });

  it("terminal protection on a single uid keeps the parent and leaves intent (no removing)", async (t) => {
    exdev(t);
    const c = mk({ split: false });
    const s = await seed(c, 4273);
    const real = c.hasPhysicalTerminalProtection.bind(c);
    let calls = 0;
    t.mock.method(c, "hasPhysicalTerminalProtection", async (id: string) => (++calls > 1 ? true : real(id)));
    await c.retireRunnerClone(s.bare, s.clonePath, s.branch, s.owner, { discard: true, attemptId: ATTEMPT, dispositionClass: "owner_terminal" });
    assert.equal(phase(s.bare), "intent");
    assert.equal(parents().length, 1);
  });

  it("a quarantine latch after the journal clear leaves intent", async () => {
    let s!: Seed;
    const c = mk({ split: true, step: step => {
      if (step === "config-sync") latchResidueQuarantine({ cause: "arrival", site: "test", runId: s.owner }, nullLogger());
    } });
    s = await seed(c, 4274);
    const rename = fsp.rename.bind(fsp);
    const orig = fsp.rename;
    (fsp as { rename: typeof fsp.rename }).rename = (async (from: fs.PathLike, to: fs.PathLike) => {
      if (String(to).startsWith(holdingRoot())) throw Object.assign(new Error("cross device"), { code: "EXDEV" });
      return rename(from, to);
    }) as typeof fsp.rename;
    try {
      await c.retireRunnerClone(s.bare, s.clonePath, s.branch, s.owner, { discard: true, attemptId: ATTEMPT, dispositionClass: "owner_terminal" });
    } finally { (fsp as { rename: typeof fsp.rename }).rename = orig; }
    assert.equal(phase(s.bare), "intent");
    assert.equal(parents().length, 1);
  });

  it("a quarantine latch right after the move rolls the clone back and leaves intent", async (t) => {
    exdev(t);
    let s!: Seed;
    const c = mk({ split: true, step: step => {
      if (step === "move-verified") latchResidueQuarantine({ cause: "arrival", site: "test", runId: s.owner }, nullLogger());
    } });
    s = await seed(c, 4275);
    await assert.rejects(c.retireRunnerClone(s.bare, s.clonePath, s.branch, s.owner, { discard: true, attemptId: ATTEMPT, dispositionClass: "owner_terminal" }));
    assert.equal(fs.existsSync(path.join(s.clonePath, "owner.txt")), true, "canonical restored");
    assert.equal(phase(s.bare), "intent");
    assert.deepEqual(parents(), []);
  });
});

describe("cap and absent inputs", () => {
  for (const split of [true, false]) {
    it(`${RETIRE_BINDING_MAX_PER_BARE} existing entries: no binding, logged once, disposal exactly as before (split=${split})`, async (t) => {
      exdev(t);
      const logs: Logs = { warn: [], info: [] };
      const c = mk({ split, logs });
      const s = await seed(c, 4276);
      for (let i = 0; i < RETIRE_BINDING_MAX_PER_BARE; i++) gitIn(s.bare, "config", "--local", retireBindingKey(randomUUID()), "x");
      const before = bindings(s.bare).map(b => b.key).sort();
      await c.retireRunnerClone(s.bare, s.clonePath, s.branch, s.owner, { discard: true, attemptId: ATTEMPT, dispositionClass: "owner_terminal" });
      assert.deepEqual(bindings(s.bare).map(b => b.key).sort(), before);
      assert.equal(logs.warn.filter(w => w === "retire scratch binding cap reached").length, 1);
      assert.deepEqual(parents(), [], "today's in-process disposal on both postures");
      assert.equal(fs.existsSync(s.clonePath), false);
      assert.equal(journal(s.bare, s.branch), "");
      assert.equal(hasRetiredEntry(s.bare, s.branch), true);
    });
  }

  it("63 existing entries still bind", async (t) => {
    exdev(t);
    const c = mk({ split: true });
    const s = await seed(c, 4277);
    for (let i = 0; i < RETIRE_BINDING_MAX_PER_BARE - 1; i++) gitIn(s.bare, "config", "--local", retireBindingKey(randomUUID()), "x");
    await c.retireRunnerClone(s.bare, s.clonePath, s.branch, s.owner, { discard: true, attemptId: ATTEMPT, dispositionClass: "owner_terminal" });
    assert.equal(bindings(s.bare).length, RETIRE_BINDING_MAX_PER_BARE);
  });

  for (const missing of ["attemptId", "dispositionClass"] as const) {
    it(`no binding without ${missing}`, async (t) => {
      exdev(t);
      const c = mk({ split: true });
      const s = await seed(c, 4278);
      await c.retireRunnerClone(s.bare, s.clonePath, s.branch, s.owner, {
        discard: true, ...(missing === "attemptId" ? { dispositionClass: "owner_terminal" as const } : { attemptId: ATTEMPT }),
      });
      assert.deepEqual(bindings(s.bare), []);
      assert.deepEqual(parents(), [], "unbound disposal is unchanged");
    });
  }

  it("the non-EXDEV holding path never binds", async () => {
    const c = mk({ split: true });
    const s = await seed(c, 4279);
    await c.retireRunnerClone(s.bare, s.clonePath, s.branch, s.owner, { discard: true, attemptId: ATTEMPT, dispositionClass: "owner_terminal" });
    assert.deepEqual(bindings(s.bare), []);
    assert.deepEqual(parents(), []);
  });
});

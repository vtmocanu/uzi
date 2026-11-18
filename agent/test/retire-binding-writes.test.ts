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

interface Logs { warn: string[]; info: string[]; warnFields?: Array<{ event: string; fields: Record<string, unknown> }> }
function mk(opts: { step?: (s: RetireBindingStep) => void | Promise<void>; split?: boolean; logs?: Logs } = {}): GitCache {
  const logger: Logger = {
    ...nullLogger(),
    warn: (event: string, fields?: Record<string, unknown>) => { opts.logs?.warn.push(event); opts.logs?.warnFields?.push({ event, fields: fields ?? {} }); },
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
  it("owner_terminal on a simulated split leaves the binding at intent, keeps the parent and runs no rm", async (t) => {
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
    assert.equal(b!.binding!.phase, "intent");
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
    assert.ok(logs.info.includes("retire scratch parent kept in place (split pod, binding stays intent)"));
    const phaseAt = (step: RetireBindingStep): string | undefined => seen.find(([name]) => name === step)?.[1];
    for (const step of ["parent-created", "parent-pinned", "root-synced", "cap-checked", "intent-write"] as const) assert.equal(phaseAt(step), "absent", step);
    for (const step of ["intent-written", "renamed", "move-verified", "journal-cleared", "ledger-appended", "config-sync"] as const)
      assert.equal(phaseAt(step), "intent", step);
    assert.deepEqual(seen.map(([name]) => String(name)).filter(n => n.startsWith("settle")), [], "no settle step is ever reached");
  });

  // C-prime (#4263 [7]): a uid-split retire never writes `settled`. The verified_successor twin is in runner-clone-retire.test.ts.
  it("C-prime: a split owner_terminal retire with a valid proof never reaches a settle step and stays intent", async (t) => {
    exdev(t);
    const seen: string[] = [];
    const c = mk({ split: true, step: step => { seen.push(String(step)); } });
    const s = await seed(c, 4903);
    await c.retireRunnerClone(s.bare, s.clonePath, s.branch, s.owner,
      { discard: true, attemptId: ATTEMPT, dispositionClass: "owner_terminal", processProof: () => true });
    assert.deepEqual(seen.filter(n => n.startsWith("settle")), [], "no step whose name starts with settle");
    assert.equal(phase(s.bare), "intent");
    assert.equal(parents().length, 1, "the parent is present");
    assert.equal(fs.existsSync(s.clonePath), false);
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
    assert.equal(seen.some(([name]) => String(name) === "settle-write"), false, "a single-uid retire never settles");
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
    assert.equal(seen.some(([name]) => String(name) === "settle-write"), false, "completion never settles");
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
  // C-prime: the split retire has no settle step, so config-sync is its last binding step and the binding stays intent.
  for (const step of ["rename-dir-sync", "config-sync"] as const) rows.push({ step, split: true, rejects: false, bind: ["intent"], where: "scratch" });
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

describe("arrivals after the move on a split", () => {
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

describe("setgid runner root", () => {
  it("the retire scratch parent is 0700 even when the runner root is setgid (production 3775)", async (t) => {
    exdev(t);
    const c = mk({ split: true });
    const s = await seed(c, 4292);
    // Precondition: a host that cannot set g+s must fail loudly, not give a vacuous green.
    fs.chmodSync(runnerRoot(), 0o3775);
    assert.equal(fs.statSync(runnerRoot()).mode & 0o2000, 0o2000, "runner root carries S_ISGID");
    const probe = path.join(runnerRoot(), ".probe-setgid");
    fs.mkdirSync(probe, 0o700);
    try {
      assert.equal(fs.statSync(probe).mode & 0o7777, 0o2700, "a plain mkdir under the root inherits S_ISGID");
    } finally { fs.rmdirSync(probe); }
    await c.retireRunnerClone(s.bare, s.clonePath, s.branch, s.owner, { discard: true, attemptId: ATTEMPT, dispositionClass: "owner_terminal" });
    const [parent] = parents();
    assert.ok(parent, "the retire kept a bound scratch parent");
    assert.equal(fs.statSync(path.join(runnerRoot(), parent)).mode & 0o7777, 0o700);
    // The discard preflight accepts the production-made parent: every posture/identity check (wrong_mode included) passes
    // and the call stops only at the effect gate, which refuses here so nothing is launched or deleted.
    const parentPath = path.join(runnerRoot(), parent);
    const rootFd = fs.openSync(runnerRoot(), fs.constants.O_RDONLY | fs.constants.O_DIRECTORY);
    const prevSplit = process.env.UZI_UID_SPLIT;
    process.env.UZI_UID_SPLIT = "1";
    try {
      const mntId = /^mnt_id:\s+(\d+)$/m.exec(fs.readFileSync(`/proc/self/fdinfo/${rootFd}`, "utf8"))![1]!;
      let stopped = false;
      const scope = {
        assertExclusive() { if (stopped) throw new Error("stopped"); },
        beginMutation() { return { settle() {} }; },
        inheritStdio(stdio: unknown[] = ["pipe", "pipe", "pipe"]) { return stdio; },
        stop() { stopped = true; }, stopped() { return stopped; },
      };
      let gateReached = false;
      const result = await c.discardRetireScratchParent({
        scope: scope as never, root: runnerRoot(), rootPin: rootFd, rootDev: String(fs.statSync(runnerRoot(), { bigint: true }).dev),
        rootMntId: mntId, parentName: parent, parentIno: String(fs.statSync(parentPath, { bigint: true }).ino),
        cloneIno: String(fs.statSync(path.join(parentPath, "clone"), { bigint: true }).ino), live: () => {},
        beforeEffect: async () => { gateReached = true; throw new Error("test: stop before any effect"); },
        signal: new AbortController().signal, deadline: Date.now() + 600_000, maxEntries: 1000, maxDepth: 64,
      });
      assert.deepEqual(result, { kind: "refused", reason: "not_live" }, "preflight passed (never wrong_mode); stopped at the effect gate");
      assert.equal(gateReached, true, "the effect gate was reached");
      assert.equal(fs.existsSync(path.join(parentPath, "clone")), true, "nothing was deleted");
    } finally {
      if (prevSplit === undefined) delete process.env.UZI_UID_SPLIT; else process.env.UZI_UID_SPLIT = prevSplit;
      fs.closeSync(rootFd);
    }
  });
});

describe("audit rework", () => {
  const retire = (c: GitCache, s: Seed, extra: Record<string, unknown> = {}) =>
    c.retireRunnerClone(s.bare, s.clonePath, s.branch, s.owner, { discard: true, attemptId: ATTEMPT, dispositionClass: "owner_terminal", ...extra });

  it("terminal protection arriving at the former settle point (after config-sync) leaves intent and no settle step", async (t) => {
    exdev(t);
    let flipped = false;
    const seen: string[] = [];
    const c = mk({ split: true, step: step => { seen.push(String(step)); if (step === "config-sync") flipped = true; } });
    const s = await seed(c, 4280);
    const real = c.hasPhysicalTerminalProtection.bind(c);
    t.mock.method(c, "hasPhysicalTerminalProtection", async (id: string) => (flipped ? true : real(id)));
    await retire(c, s);
    assert.equal(flipped, true, "the config-sync step was reached");
    assert.deepEqual(seen.filter(n => n.startsWith("settle")), []);
    assert.equal(phase(s.bare), "intent");
    assert.equal(parents().length, 1);
  });

  it("a decoy swapped into clonePath at the bind step is never bound or settled", async (t) => {
    exdev(t);
    let s!: Seed;
    const c = mk({ split: true, step: step => {
      if (step !== "root-synced") return;
      fs.renameSync(s.clonePath, `${s.clonePath}.aside`);
      fs.mkdirSync(s.clonePath);
    } });
    s = await seed(c, 4281);
    await assert.rejects(retire(c, s), /pinned at the start of the retire/);
    assert.deepEqual(bindings(s.bare), []);
    assert.deepEqual(parents(), []);
    assert.equal(fs.existsSync(path.join(`${s.clonePath}.aside`, "owner.txt")), true);
  });

  it("a decoy swapped into the moved clone leaves the binding at intent", async (t) => {
    exdev(t);
    let s!: Seed;
    const c = mk({ split: true, step: step => {
      if (step !== "renamed") return;
      const [parent] = parents();
      fs.renameSync(path.join(runnerRoot(), parent!, "clone"), path.join(runnerRoot(), "aside-clone"));
      fs.mkdirSync(path.join(runnerRoot(), parent!, "clone"));
    } });
    s = await seed(c, 4282);
    await retire(c, s);
    assert.equal(phase(s.bare), "intent");
    assert.equal(parents().length, 1);
  });

  // C-prime (#4263 [7]): the former settle point is the end of the split retire. A proof revocation or quarantine latch
  // injected there, or at the step before it, leaves the binding at intent and no settle step is ever observed.
  for (const at of ["ledger-appended", "config-sync"] as const) {
    for (const arrival of ["process proof revoked", "residue quarantine latched"] as const) {
      it(`${arrival} at ${at} leaves intent, never settled, and reaches no settle step`, async (t) => {
        exdev(t);
        let revoked = false;
        const seen: string[] = [];
        let s!: Seed;
        const c = mk({ split: true, step: step => {
          seen.push(String(step));
          if (step !== at) return;
          if (arrival === "process proof revoked") revoked = true;
          else latchResidueQuarantine({ cause: "arrival", site: "test", runId: s.owner }, nullLogger());
        } });
        s = await seed(c, at === "config-sync" ? 4900 : 4901);
        await retire(c, s, { processProof: () => !revoked }).catch(() => undefined);
        assert.ok(seen.includes(at), `${at} was reached`);
        assert.deepEqual(seen.filter(n => n.startsWith("settle")), []);
        assert.notEqual(phase(s.bare), "settled", "the parent stays ineligible for epoch deletion");
        assert.equal(phase(s.bare), "intent");
        // A revoked proof rolls the clone back to its canonical path (the binding is left for the epoch's gone-parent
        // recovery); a quarantine latch keeps the clone in its parent. Either way the clone is never lost.
        if (arrival === "process proof revoked") assert.equal(fs.existsSync(path.join(s.clonePath, "owner.txt")), true);
        else assert.equal(parents().length, 1);
      });
    }
  }

  it("control: a valid process proof through config-sync still ends at intent with the parent kept", async (t) => {
    exdev(t);
    const seen: string[] = [];
    const c = mk({ split: true, step: step => { seen.push(String(step)); } });
    const s = await seed(c, 4902);
    await retire(c, s, { processProof: () => true });
    assert.ok(seen.includes("config-sync"));
    assert.equal(phase(s.bare), "intent");
    assert.equal(parents().length, 1);
  });

  it("a removing sync failure after git config wrote the value logs the actual phase", async (t) => {
    exdev(t);
    const logs: Logs = { warn: [], info: [], warnFields: [] };
    const c = mk({ split: false, logs, step: step => { if (step === "removing-sync") throw new Error("injected removing-sync"); } });
    const s = await seed(c, 4287);
    await retire(c, s);
    const entry = logs.warnFields!.find(w => w.event === "retire scratch parent kept: its removing binding was not confirmed durable");
    assert.ok(entry);
    assert.equal(entry.fields.binding_phase, "removing");
    assert.equal(phase(s.bare), "removing");
  });

  it("completion exclusion lost after the bind drops the intent and the parent; nothing moved", async (t) => {
    exdev(t);
    let open = true;
    const seen: RetireBindingStep[] = [];
    const c = mk({ step: step => { seen.push(step); if (step === "intent-written") open = false; } });
    const s = await seed(c, 4288);
    const head = await c.worktreeHead(s.clonePath);
    assert.ok(head);
    const journalBefore = journal(s.bare, s.branch);
    await assert.rejects(retire(c, s, { dispositionClass: "completion", completionSource: { expectedHead: head, canDelete: () => open } }),
      /completion cleanup exclusion changed/);
    assert.deepEqual(bindings(s.bare), []);
    assert.deepEqual(parents(), []);
    assert.equal(fs.existsSync(path.join(s.clonePath, "owner.txt")), true);
    assert.equal(journal(s.bare, s.branch), journalBefore);
    assert.deepEqual(seen.filter(n => n.startsWith("unmoved-")), ["unmoved-unset", "unmoved-unset-sync", "unmoved-unset-done"]);
    assert.equal(seen.includes("unset"), false, "the unmoved drop has its own step names");
  });

  it("a split deferral reports the retirement unproven; a single-uid disposal does not", async (t) => {
    exdev(t);
    let unproven = 0;
    const c = mk({ split: true });
    const s = await seed(c, 4289);
    await retire(c, s, { onRetirementUnproven: () => { unproven++; } });
    assert.equal(phase(s.bare), "intent");
    assert.equal(unproven, 1);
    let single = 0;
    const c2 = mk({ split: false });
    const s2 = await seed(c2, 4290);
    await retire(c2, s2, { onRetirementUnproven: () => { single++; } });
    assert.equal(single, 0);
  });

  it("a failed retired ledger append on a split leaves the binding at intent", async (t) => {
    exdev(t);
    const c = mk({ split: true });
    const s = await seed(c, 4291);
    const priv = c as unknown as { appendAttemptLedger(...a: unknown[]): Promise<void> };
    t.mock.method(priv, "appendAttemptLedger", async () => { throw new Error("injected append failure"); });
    await retire(c, s);
    assert.equal(hasRetiredEntry(s.bare, s.branch), false);
    assert.equal(phase(s.bare), "intent");
    assert.equal(parents().length, 1);
  });
});

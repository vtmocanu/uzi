import { describe, it, type TestContext } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import fsp from "node:fs/promises";
import path from "node:path";
import { latchResidueQuarantine } from "../src/residue-quarantine.js";
import { nullLogger } from "./helpers.js";
import { installHarness, git } from "./runner-harness.js";
import { openAuthorityStore } from "../src/completion-authority.js";
import {
  BRANCH, ENV, GENERATION, TOKEN, fenceOf, isV2, journalRaw, readRecords, recordDir, recordNames, rewriteSources, rig, slotAt, slotOf, slotVia, snapshotFiles,
  type Rig,
} from "./completion-retirement-fixture.js";

installHarness();

// issue #3404 M2 U5 - restart retirement consumes the durable publication. The REAL runner publishes a
// flight's completing source (close -> seal -> publish), a SECOND worker incarnation (no in-memory binding)
// sweeps the receipt-bearing record, and the exact clone, skills dir, journal entry, bundles and records go,
// or nothing does. Real authority store, real recovery coordinator, real Git.

const cloneAlive = (r: Rig): boolean => fs.existsSync(path.join(r.clone, ".git"));

/** A sibling the retirement must not touch: another generation's record+bundle in the same run directory. */
async function siblingGeneration(r: Rig): Promise<{ record: string; bundle: string }> {
  const internals = r.p.recovery as unknown as { writeRecordUnlocked(rec: unknown): Promise<void> };
  const [first] = await r.p.recovery.inspect(r.runId);
  const captureId = "00000000-0000-4000-8000-0000000000aa";
  await internals.writeRecordUnlocked({ ...first, captureId, generation: 30, completionSource: undefined, completionReceipt: undefined });
  const bundle = path.join(recordDir(r), `${captureId}.bundle`);
  fs.writeFileSync(bundle, "sibling bundle");
  return { record: path.join(recordDir(r), `${captureId}.json`), bundle };
}

/** Another run's clone on the SAME bare, journaled under its own branch. */
async function otherRunClone(r: Rig): Promise<string> {
  const other = await git.runnerCloneForBranch(r.bare, "agent/issue-9999", "other-source", {
    isLive: () => false, beforeFree: async () => { throw new Error("unexpected incumbent"); },
  }, "run-other", false, undefined, { attemptId: "20261010T000000Z-g3-aaaaaaaaaaaaaaaa", isLive: () => false, beforeSeed: async () => {} });
  await git.markRecoveryCapture(r.bare, other.path, "agent/issue-9999", "run-other", "20261010T000000Z-g3-aaaaaaaaaaaaaaaa");
  return other.path;
}

for (const codex of [false, true]) {
  describe(`production close -> seal -> publish -> restart (${codex ? "Codex" : "Claude"})`, () => {
    it("retires the exact clone, skills, journal entry, bundles and records, tombstones the slot, and preserves siblings", async t => {
      const r = await rig(t, { codex, cohort: 3 });
      const sibling = await siblingGeneration(r);
      const otherClone = await otherRunClone(r);
      const otherJournal = execFileSync("git", ["-C", r.bare, "config", "--get", "uzi-recovery.agent/issue-9999.clone"], { encoding: "utf8", env: ENV }).trim();
      await r.end();
      assert.equal((await slotOf(r))?.state, codex ? "sealed" : "claude_ready");
      await r.receipt();
      assert.ok(cloneAlive(r) && fs.existsSync(r.skills));
      assert.ok(fenceOf(r));
      const before = await slotOf(r);

      const second = r.restart();
      await second.sweep();

      assert.equal(fs.existsSync(r.clone), false, "the exact clone is gone");
      assert.equal(fs.existsSync(r.skills), false, "its paired skills dir is gone");
      assert.equal(journalRaw(r), "", "the completing journal entry is cleared");
      for (const id of r.captureIds) {
        assert.equal(fs.existsSync(path.join(recordDir(r), `${id}.json`)), false, "cohort record removed");
        assert.equal(fs.existsSync(path.join(recordDir(r), `${id}.bundle`)), false, "cohort bundle removed");
      }
      assert.deepEqual(recordNames(r), [path.basename(sibling.bundle), path.basename(sibling.record)].sort(), "only the sibling generation remains");
      assert.equal(fs.readFileSync(sibling.bundle, "utf8"), "sibling bundle");
      assert.ok(fs.existsSync(path.join(otherClone, ".git")), "another run's clone on the same bare is untouched");
      assert.equal(execFileSync("git", ["-C", r.bare, "config", "--get", "uzi-recovery.agent/issue-9999.clone"], { encoding: "utf8", env: ENV }).trim(), otherJournal);
      const slot = await slotVia(second.rt, r.clone);
      assert.equal(slot?.state, "retired", "the durable tombstone");
      assert.equal(slot?.publication, null);
      assert.notEqual(slot?.epoch, undefined);
      assert.equal(slot?.epoch, before?.epoch, "the tombstone keeps the epoch history; it is never unlinked");
      assert.equal(slot?.owner.worker, second.incarnation, "retired by the restarted incarnation");
      // Durability is awaited and bounded: each slot write is 4 barriers (slot file+dir, head shard file+dir). The
      // retirement writes the slot a fixed number of times (begin, progress, tombstone), never per cohort member
      // or per file; the run directory is synced once.
      assert.equal(second.barriers.authority % 4, 0, "whole slot writes only");
      assert.equal(second.barriers.authority / 4, 8, "begin, skills record open+settle, four progress records, tombstone");
      assert.equal(second.barriers.runDirectory, 1, "one awaited run-directory sync for the record deletions");
      process.stderr.write(`# U5 barriers (${codex ? "codex" : "claude"}, cohort 3): authority=${second.barriers.authority} runDirectory=${second.barriers.runDirectory}\n`);
    });
  });
}

describe("retirement durability cost does not scale with the cohort", () => {
  const totals = new Map<number, number>();
  for (const cohort of [2, 6]) {
    it(`retires a ${cohort}-member cohort`, async t => {
      const r = await rig(t, { codex: false, cohort });
      await r.end();
      await r.receipt();
      const second = r.restart();
      await second.sweep();
      assert.equal(fs.existsSync(r.clone), false);
      totals.set(cohort, second.barriers.authority + second.barriers.runDirectory * 1000);
    });
  }
  it("the same number of awaited barriers retires both", () => {
    assert.equal(totals.size, 2);
    assert.equal(totals.get(2), totals.get(6));
  });
});

describe("restart retirement is the only way a restarted worker releases a completed source", () => {
  it("a restarted Codex flight whose seal was unproven publishes nothing and retains", async t => {
    const r = await rig(t, { codex: true, cohort: 2, unproven: true });
    await r.end();
    assert.equal((await slotOf(r))?.state, "ineligible");
    await r.receipt();
    const before = snapshotFiles(r);
    const preflight = t.mock.method(git, "cleanupRecoveryGeneration");
    await r.restart().sweep();
    assert.equal(preflight.mock.callCount(), 0, "no source-dependent preflight for an unproven source");
    assert.ok(cloneAlive(r));
    assert.deepEqual(snapshotFiles(r), before);
  });

  for (const codex of [false, true]) {
    it(`a legacy V1 ${codex ? "Codex" : "Claude"}-era cohort retains after a restart`, async t => {
      const r = await rig(t, { codex, cohort: 2 });
      await r.receipt();
      assert.equal(readRecords(r).some(isV2), false, "never published: V1 records only");
      const before = snapshotFiles(r);
      const preflight = t.mock.method(git, "cleanupRecoveryGeneration");
      const inventory = t.mock.method(git, "readInventoryCloneHeads");
      await r.restart().sweep();
      assert.equal(preflight.mock.callCount(), 0);
      assert.equal(inventory.mock.callCount(), 0);
      assert.ok(cloneAlive(r));
      assert.ok(fs.existsSync(r.skills));
      assert.deepEqual(snapshotFiles(r), before);
    });
  }

  it("an old-format cohort written after the publication of another run stays retained while the published one retires", async t => {
    const published = await rig(t, { codex: false, cohort: 2 });
    await published.end();
    await published.receipt();
    // A recurring old-format arrival of the SAME run and a later generation: V1 records, a receipt, no publication.
    const internals = published.p.recovery as unknown as { writeRecordUnlocked(rec: unknown): Promise<void> };
    const [seed] = await published.p.recovery.inspect(published.runId);
    const legacyId = "00000000-0000-4000-8000-0000000000bb";
    await internals.writeRecordUnlocked({ ...seed, captureId: legacyId, generation: 31, completionSource: { ...published.bound,
      context: { ...published.bound.context, generation: 31 }, attemptId: undefined }, completionReceipt: undefined });
    const second = published.restart();
    await second.sweep();
    assert.equal(fs.existsSync(published.clone), false, "the published generation retired");
    assert.ok(fs.existsSync(path.join(recordDir(published), `${legacyId}.json`)), "the V1 arrival's record is untouched");
  });
});

describe("a published proof is exact: anything else retains before any inspection", () => {
  type Change = (r: Rig) => Promise<void>;
  const rewrite = (change: (s: Record<string, unknown>, i: number) => Record<string, unknown> | undefined): Change => r => rewriteSources(r, change);
  const flip = (hex: string): string => (hex.startsWith("0") ? "1" : "0") + hex.slice(1);

  const cases: Array<[name: string, codex: boolean, change: Change]> = [
    ["swapped executor incarnation", true, rewrite(s => ({ ...s, executorIncarnation: "swapped-executor" }))],
    ["swapped worker incarnation", false, rewrite(s => ({ ...s, workerIncarnation: "99999999-9999-4999-8999-999999999999" }))],
    ["swapped registry/facade pair", true, rewrite(s => ({ ...s, registryFacadePairs: [["swapped-registry", "swapped-facade"]] }))],
    ["codex custody laundered as claude", true, rewrite(s => {
      const { executorIncarnation: _e, registryFacadePairs: _p, ...rest } = s;
      return { ...rest, harness: "claude" };
    })],
    ["changed epoch", false, rewrite(s => ({ ...s, sourceUseEpoch: "99999999-9999-4999-8999-999999999999" }))],
    ["changed context", false, rewrite(s => ({ ...s, context: { ...(s.context as object), branch: "agent/other" } }))],
    ["changed clone path", false, rewrite(s => ({ ...s, clonePath: `${s.clonePath as string}-x`, relativeSource: `${s.relativeSource as string}-x` }))],
    ["changed attempt", false, rewrite(s => ({ ...s, attemptId: "20261011T000000Z-g29-bbbbbbbbbbbbbbbb" }))],
    ["changed HEAD", false, rewrite(s => ({ ...s, expectedHead: "d".repeat(40) }))],
    ["changed roots", false, rewrite(s => ({ ...s, roots: [{ sha: "e".repeat(40), contexts: [s.context] }] }))],
    ["malformed version", false, rewrite(s => ({ ...s, version: 3 }))],
    ["certificate MAC tampered", false, rewrite(s => ({ ...s, certificate: flip(s.certificate as string) }))],
    ["certificate removed", false, rewrite(s => { const { certificate: _c, ...rest } = s; return rest; })],
    ["one member back to the bound V1 core", false, rewrite((s, i) => (i === 1 ? {
      context: s.context, clonePath: s.clonePath, expectedHead: s.expectedHead, roots: s.roots,
      ...(s.attemptId === undefined ? {} : { attemptId: s.attemptId }) } : undefined))],
    ["a member removed after success", false, async r => {
      const id = r.captureIds[1]!;
      fs.rmSync(path.join(recordDir(r), `${id}.json`));
      fs.rmSync(path.join(recordDir(r), `${id}.bundle`));
    }],
    ["an unattributable physical entry", false, async r => { fs.writeFileSync(path.join(recordDir(r), "stray.txt"), "x"); }],
    ["an extra member of the generation", false, async r => {
      const internals = r.p.recovery as unknown as { writeRecordUnlocked(rec: unknown): Promise<void> };
      const [first] = await r.p.recovery.inspect(r.runId);
      await internals.writeRecordUnlocked({ ...first, captureId: "00000000-0000-4000-8000-0000000000cc" });
    }],
    ["the journal fence removed", false, async r => {
      const raw = JSON.parse(journalRaw(r)) as Record<string, unknown>;
      delete raw.completionSourceFence;
      execFileSync("git", ["-C", r.bare, "config", "--local", `uzi-recovery.${BRANCH}.clone`, JSON.stringify(raw)], { env: ENV });
    }],
    ["the journal fence names another epoch", false, async r => {
      const raw = JSON.parse(journalRaw(r)) as { completionSourceFence: { sourceUseEpoch: string } };
      raw.completionSourceFence.sourceUseEpoch = "99999999-9999-4999-8999-999999999999";
      execFileSync("git", ["-C", r.bare, "config", "--local", `uzi-recovery.${BRANCH}.clone`, JSON.stringify(raw)], { env: ENV });
    }],
  ];

  for (const [name, codex, change] of cases) {
    it(`${name}: retained, no preflight, no inventory, nothing destroyed`, async t => {
      const r = await rig(t, { codex, cohort: 3 });
      await r.end();
      assert.equal((await slotOf(r))?.state, codex ? "sealed" : "claude_ready");
      await r.receipt();
      await change(r);
      const files = snapshotFiles(r);
      const slotBefore = await slotOf(r);
      const preflight = t.mock.method(git, "cleanupRecoveryGeneration");
      const inventory = t.mock.method(git, "readInventoryCloneHeads");
      const head = t.mock.method(git, "worktreeHead");
      const retire = t.mock.method(git, "retireRunnerClone");
      await r.restart().sweep();
      assert.equal(preflight.mock.callCount(), 0, "the authority check precedes the preflight");
      assert.equal(inventory.mock.callCount(), 0, "and every inventory");
      assert.equal(head.mock.callCount(), 0);
      assert.equal(retire.mock.callCount(), 0);
      assert.ok(cloneAlive(r), "the clone stays");
      assert.ok(fs.existsSync(r.skills), "the skills dir stays");
      assert.deepEqual(snapshotFiles(r), files, "no record or bundle changed");
      assert.deepEqual(await slotOf(r), slotBefore, "the slot is untouched");
    });
  }
});

describe("the authority check happens before any inventory or preflight", () => {
  it("orders verify -> bare resolution -> claims -> preflight -> inventory -> destruction (instrumented, no sleeps)", async t => {
    const r = await rig(t, { codex: false, cohort: 2 });
    await r.end();
    await r.receipt();
    const second = r.restart();
    const events: string[] = [];
    const scope = second.rt.retirement;
    const inspect = scope.inspect.bind(scope);
    (scope as { inspect: typeof scope.inspect }).inspect = async value => { events.push("verify:start"); const view = await inspect(value); events.push("verify:done"); return view; };
    const enter = scope.enter.bind(scope);
    (scope as { enter: typeof scope.enter }).enter = async (view, manifest) => {
      events.push("retiring");
      // The first entry must precede the first destructive effect: the clone and its skills dir are still there.
      if (!events.includes("retiring:entered")) events.push("retiring:entered", fs.existsSync(r.clone) && fs.existsSync(r.skills) ? "retiring:sources-present" : "retiring:sources-gone");
      return enter(view, manifest);
    };
    const wrap = <K extends keyof typeof git>(name: K): void => {
      const real = (git[name] as unknown as (...a: unknown[]) => unknown).bind(git);
      t.mock.method(git, name, ((...args: unknown[]) => { events.push(String(name) + (name === "cleanupRecoveryGeneration" ? `:${String(args[7])}` : "")); return real(...args); }) as never);
    };
    for (const name of ["resolveRecoveryBareDir", "readCompletionSourceFence", "cleanupRecoveryGeneration", "readInventoryCloneHeads", "worktreeHead", "retireRunnerClone"] as const) wrap(name);
    await second.sweep();
    const at = (e: string): number => events.indexOf(e);
    assert.ok(at("verify:done") >= 0, events.join(","));
    for (const later of ["resolveRecoveryBareDir", "cleanupRecoveryGeneration:completion-check", "readInventoryCloneHeads", "worktreeHead", "retireRunnerClone"]) {
      assert.ok(at(later) > at("verify:done"), `${later} after the authority check: ${events.join(",")}`);
    }
    assert.ok(at("retiring:sources-present") >= 0, `retiring is entered before the first destruction: ${events.join(",")}`);
    assert.ok(at("retiring") > at("cleanupRecoveryGeneration:completion-check"), "retiring is entered after the preflight");
    assert.ok(at("retiring") > at("readInventoryCloneHeads"), "and after the inventory");
    assert.ok(at("retiring") < at("cleanupRecoveryGeneration:completion"), "and before the first ref is destroyed");
    assert.equal(fs.existsSync(r.clone), false);
  });
});

describe("a valid proof still loses to each independent guard", () => {
  async function published(t: Parameters<typeof rig>[0], codex = false): Promise<Rig> {
    const r = await rig(t, { codex, cohort: 2 });
    await r.end();
    await r.receipt();
    return r;
  }
  const untouched = async (r: Rig, files: Record<string, string>): Promise<void> => {
    assert.ok(cloneAlive(r), "the clone stays");
    assert.ok(fs.existsSync(r.skills));
    assert.deepEqual(snapshotFiles(r), files);
    assert.ok(fenceOf(r), "the journal fence stays");
    assert.equal((await slotOf(r))?.state === "claude_ready" || (await slotOf(r))?.state === "retiring", true);
  };

  it("receipt missing: nothing to release", async t => {
    const r = await rig(t, { codex: false, cohort: 2 });
    await r.end();
    const files = snapshotFiles(r);
    const preflight = t.mock.method(git, "cleanupRecoveryGeneration");
    const second = r.restart();
    await second.p.recovery.resumePending(undefined, await second.p.recovery.inspect(r.runId));
    assert.equal(preflight.mock.callCount(), 0);
    await untouched(r, files);
  });

  it("a live execution attempt of the clone", async t => {
    const r = await published(t);
    const files = snapshotFiles(r);
    const second = r.restart();
    t.mock.method(second.p.liveAttempts, "isLivePath", () => true);
    await second.sweep();
    await untouched(r, files);
  });

  it("a HEAD that moved since the proof", async t => {
    const r = await published(t);
    execFileSync("git", ["-C", r.clone, "-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "advance"], { env: ENV, stdio: "pipe" });
    const files = snapshotFiles(r);
    await r.restart().sweep();
    await untouched(r, files);
  });

  it("the worker residue quarantine", async t => {
    const r = await published(t);
    const files = snapshotFiles(r);
    latchResidueQuarantine({ cause: "u5 fixture", site: "terminal_drive", runId: r.runId }, nullLogger());
    await r.restart().sweep();
    await untouched(r, files);
  });

  it("a run that is executing", async t => {
    const r = await published(t);
    const files = snapshotFiles(r);
    const second = r.restart();
    (second.p as unknown as { executionTails: Map<string, Promise<void>> }).executionTails.set(r.runId, new Promise(() => {}));
    await second.sweep();
    await untouched(r, files);
  });

  it("a terminal record's custody protection", async t => {
    const r = await published(t);
    const files = snapshotFiles(r);
    t.mock.method(git, "hasPhysicalTerminalProtection", async () => true);
    await r.restart().sweep();
    await untouched(r, files);
  });

  it("a current-process Codex binding without its live disposal witness stays a veto over a valid durable proof", async t => {
    const r = await published(t, true);
    const files = snapshotFiles(r);
    // The SAME process still holds a knownCodex binding for this source (as a flight that never finished its
    // terminal disposal does). The in-memory classification vetoes; the durable record cannot override it.
    const internals = r.p as unknown as {
      rememberCompletionSource(source: unknown, flight: unknown, executor: unknown, terminal: unknown): unknown;
    };
    const ctx = r.bound.context;
    internals.rememberCompletionSource(r.bound, r.flight, r.executor, {
      runId: ctx.runId, generation: ctx.generation, barePath: ctx.barePath, branch: ctx.branch, clonePath: r.bound.clonePath,
      attemptId: r.bound.attemptId, safety: {},
    });
    const snapshot = (await r.p.recovery.inspect(r.runId)).filter(x => x.completionReceipt);
    await r.p.recovery.resumePending(undefined, snapshot);
    assert.ok(cloneAlive(r), "the vetoed current-process source stays");
    assert.deepEqual(snapshotFiles(r), files);
    assert.equal((await slotOf(r))?.state, "sealed", "no retiring entry was made");
  });

  it("the dirty-own-clone positive: dirty and untracked files do not stop a proven retirement", async t => {
    const r = await published(t);
    fs.writeFileSync(path.join(r.clone, "untracked.txt"), "untracked remainder\n");
    fs.writeFileSync(path.join(r.clone, "README.md"), "dirty remainder\n");
    await r.restart().sweep();
    assert.equal(fs.existsSync(r.clone), false);
    assert.equal(readRecords(r).length, 0);
    assert.equal((await slotOf(r))?.state, "retired");
  });
});

describe("partial retirement keeps the authenticated retiring record as the only retry authority", () => {
  /** Run the retirement with the first bundle unlink failing: a JOINED failure after Git cleared the journal. */
  async function partial(t: Parameters<typeof rig>[0], codex = false): Promise<{ r: Rig; first: ReturnType<Rig["restart"]> }> {
    const r = await rig(t, { codex, cohort: 3 });
    await r.end();
    await r.receipt();
    const first = r.restart();
    const real = fsp.unlink.bind(fsp);
    let failed = false;
    t.mock.method(fsp, "unlink", async (file: string) => {
      if (!failed && String(file).endsWith(".bundle")) { failed = true; throw Object.assign(new Error("EIO"), { code: "EIO" }); }
      return real(file);
    });
    await first.sweep();
    assert.equal(failed, true, "the injected failure fired");
    return { r, first };
  }

  it("keeps retiring with the residue: Git cleared the journal and moved the clone, the bundles and records remain", async t => {
    const { r } = await partial(t);
    assert.equal(fs.existsSync(r.clone), false);
    assert.equal(fs.existsSync(r.skills), false);
    assert.equal(journalRaw(r), "", "Git cleared the journal");
    assert.equal(recordNames(r).length, 6, "three records and three bundles remain");
    const slot = await slotOf(r);
    assert.equal(slot?.state, "retiring");
    assert.equal(slot?.retiring?.deleted, 3, "clone, skills and journal fence are recorded as deleted");
    assert.equal(slot?.retiring?.manifest.length, 3 + 3 + 3);
    assert.ok(slot?.publication, "the prior proof is kept");
  });

  it("the same worker's retry removes only the remaining residue and tombstones", async t => {
    const { r, first } = await partial(t);
    t.mock.restoreAll();
    await first.sweep();
    assert.deepEqual(recordNames(r), []);
    assert.equal((await slotOf(r))?.state, "retired");
  });

  it("a successor incarnation resumes a dead owner's retirement; a running helper would block it", async t => {
    const { r } = await partial(t);
    t.mock.restoreAll();
    const successor = r.restart();
    await successor.sweep();
    assert.deepEqual(recordNames(r), []);
    const slot = await slotOf(r);
    assert.equal(slot?.state, "retired");
    assert.equal(slot?.owner.worker, successor.incarnation);
  });

  it("an unresolved (running) destructive record of a dead owner blocks the successor's retry", async t => {
    const { r, first } = await partial(t);
    t.mock.restoreAll();
    // The dying owner had a detached helper running over the skills footprint: its record was never settled.
    await first.rt.withTransaction(() => first.rt.hooks.beginDestructive([r.skills], "runner-uid-delete"));
    assert.equal((await slotVia(first.rt, r.clone))?.pending?.status, "running");
    const files = snapshotFiles(r);
    const successor = r.restart();
    await successor.sweep();
    assert.deepEqual(snapshotFiles(r), files, "nothing was destroyed");
    const slot = await slotVia(successor.rt, r.clone);
    assert.equal(slot?.state, "retiring");
    assert.equal(slot?.owner.worker, first.incarnation, "the dead owner's retirement was not taken over");
  });

  it("an addition to the generation refuses the retry", async t => {
    const { r, first } = await partial(t);
    t.mock.restoreAll();
    const internals = r.p.recovery as unknown as { writeRecordUnlocked(rec: unknown): Promise<void> };
    const [seed] = await first.p.recovery.inspect(r.runId);
    await internals.writeRecordUnlocked({ ...seed, captureId: "00000000-0000-4000-8000-0000000000dd" });
    const files = snapshotFiles(r);
    await first.sweep();
    assert.deepEqual(snapshotFiles(r), files, "nothing more was deleted");
    assert.equal((await slotOf(r))?.state, "retiring");
  });

  it("a bundle appearing for a member the manifest never named refuses the retry", async t => {
    const r = await rig(t, { codex: false, cohort: 3 });
    // The third member had no bundle when the retirement began, so the manifest does not name one.
    fs.rmSync(path.join(recordDir(r), `${r.captureIds[2]}.bundle`));
    await r.end();
    await r.receipt();
    const first = r.restart();
    const real = fsp.unlink.bind(fsp);
    let failed = false;
    t.mock.method(fsp, "unlink", async (file: string) => {
      if (!failed && String(file).endsWith(".bundle")) { failed = true; throw Object.assign(new Error("EIO"), { code: "EIO" }); }
      return real(file);
    });
    await first.sweep();
    assert.equal(failed, true);
    t.mock.restoreAll();
    fs.writeFileSync(path.join(recordDir(r), `${r.captureIds[2]}.bundle`), "late bundle");
    const files = snapshotFiles(r);
    await first.sweep();
    assert.deepEqual(snapshotFiles(r), files);
    assert.equal((await slotOf(r))?.state, "retiring");
  });

  it("an unexplained absence (a later artifact gone while an earlier one remains) refuses the retry", async t => {
    const { r, first } = await partial(t);
    t.mock.restoreAll();
    const bundles = recordNames(r).filter(n => n.endsWith(".bundle"));
    fs.rmSync(path.join(recordDir(r), bundles[bundles.length - 1]!));
    const files = snapshotFiles(r);
    await first.sweep();
    assert.deepEqual(snapshotFiles(r), files, "nothing more was deleted");
    assert.equal((await slotOf(r))?.state, "retiring");
  });

  it("a changed journal fence refuses", async t => {
    const { r, first } = await partial(t);
    t.mock.restoreAll();
    // A journal naming this very source with a different fence is a change, not an absence.
    execFileSync("git", ["-C", r.bare, "config", "--local", `uzi-recovery.${BRANCH}.clone`, JSON.stringify({
      runId: r.runId, clonePath: r.clone, attemptId: r.bound.attemptId,
      completionSourceFence: { version: 1, sourceUseEpoch: "99999999-9999-4999-8999-999999999999", sourceCoreDigest: "a".repeat(64) } })], { env: ENV });
    const files = snapshotFiles(r);
    await first.sweep();
    assert.deepEqual(snapshotFiles(r), files);
    assert.equal((await slotOf(r))?.state, "retiring");
  });

  // Capture ids the authority store admits that are NOT v4 UUIDs: their bundles do not match the ordinary-temp
  // pattern, so the residue check must accept a manifest-named bundle on its own terms.
  const NON_V4 = ["00000000-0000-1000-8000-000000000001", "00000000-0000-1000-8000-000000000002"];
  const failFirstBundle = (t: TestContext): { fired(): boolean } => {
    const real = fsp.unlink.bind(fsp);
    let failed = false;
    t.mock.method(fsp, "unlink", async (file: string) => {
      if (!failed && String(file).endsWith(".bundle")) { failed = true; throw Object.assign(new Error("EIO"), { code: "EIO" }); }
      return real(file);
    });
    return { fired: () => failed };
  };

  for (const successor of [false, true]) {
    it(`bundles of non-v4 members left by a partial retirement are residue a retry removes (${successor ? "successor incarnation" : "same worker"})`, async t => {
      const r = await rig(t, { codex: false, cohort: 3, memberIds: NON_V4 });
      await r.end();
      await r.receipt();
      const first = r.restart();
      const injected = failFirstBundle(t);
      await first.sweep();
      assert.equal(injected.fired(), true);
      t.mock.restoreAll();
      assert.ok(recordNames(r).some(n => n === `${NON_V4[0]}.bundle`), "a non-v4 bundle is part of the residue");
      assert.equal((await slotOf(r))?.state, "retiring");
      const retry = successor ? r.restart() : first;
      await retry.sweep();
      assert.deepEqual(recordNames(r), []);
      assert.equal((await slotOf(r))?.state, "retired");
    });
  }

  it("a non-v4 bundle of another generation's record is a sibling a retry leaves alone", async t => {
    const r = await rig(t, { codex: false, cohort: 3, memberIds: NON_V4 });
    await r.end();
    await r.receipt();
    const first = r.restart();
    const injected = failFirstBundle(t);
    await first.sweep();
    assert.equal(injected.fired(), true);
    t.mock.restoreAll();
    const internals = r.p.recovery as unknown as { writeRecordUnlocked(rec: unknown): Promise<void> };
    const [seed] = await first.p.recovery.inspect(r.runId);
    const sibling = "00000000-0000-1000-8000-0000000000ab";
    await internals.writeRecordUnlocked({ ...seed, captureId: sibling, generation: 30, completionSource: undefined, completionReceipt: undefined });
    fs.writeFileSync(path.join(recordDir(r), `${sibling}.bundle`), "sibling bundle");
    await first.sweep();
    assert.deepEqual(recordNames(r), [`${sibling}.bundle`, `${sibling}.json`]);
    assert.equal((await slotOf(r))?.state, "retired");
  });

  it("a bundle appearing for a non-v4 member the manifest never named refuses the retry", async t => {
    const r = await rig(t, { codex: false, cohort: 3, memberIds: NON_V4 });
    fs.rmSync(path.join(recordDir(r), `${NON_V4[1]}.bundle`));
    await r.end();
    await r.receipt();
    const first = r.restart();
    const injected = failFirstBundle(t);
    await first.sweep();
    assert.equal(injected.fired(), true);
    t.mock.restoreAll();
    fs.writeFileSync(path.join(recordDir(r), `${NON_V4[1]}.bundle`), "late bundle");
    const files = snapshotFiles(r);
    await first.sweep();
    assert.deepEqual(snapshotFiles(r), files);
    assert.equal((await slotOf(r))?.state, "retiring");
  });

  // The run directory enumerates in reverse-sorted order (readdir/opendir order is not sorted), and the deletion
  // fails after the first SUCCESSFUL unlink of the given kind: the residue must still be a manifest suffix.
  for (const kind of ["bundle", "record"] as const) {
    for (const successor of [false, true]) {
      it(`an unsorted enumeration still deletes in manifest order: a failure after the first ${kind} unlink resumes (${successor ? "successor incarnation" : "same worker"})`, async t => {
        const r = await rig(t, { codex: false, cohort: 3 });
        await r.end();
        await r.receipt();
        const first = r.restart();
        const realReaddir = fsp.readdir.bind(fsp) as (...args: unknown[]) => Promise<unknown>;
        t.mock.method(fsp, "readdir", async (...args: unknown[]) => {
          const names = await realReaddir(...args);
          return path.resolve(String(args[0])) === path.resolve(recordDir(r)) && Array.isArray(names) && names.every(n => typeof n === "string")
            ? [...(names as string[])].sort().reverse() : names;
        });
        const realUnlink = fsp.unlink.bind(fsp);
        const suffix = kind === "bundle" ? ".bundle" : ".json";
        let succeeded = 0;
        let failed = false;
        t.mock.method(fsp, "unlink", async (file: string) => {
          if (path.dirname(String(file)) === recordDir(r) && String(file).endsWith(suffix)) {
            if (succeeded >= 1 && !failed) { failed = true; throw Object.assign(new Error("EIO"), { code: "EIO" }); }
            await realUnlink(file);
            succeeded++;
            return;
          }
          return realUnlink(file);
        });
        await first.sweep();
        assert.equal(failed, true, "the injected failure fired after one successful unlink");
        assert.equal((await slotOf(r))?.state, "retiring");
        // Keep the unsorted enumeration on the retry; only the failure injection goes.
        (fsp.unlink as unknown as { mock: { restore(): void } }).mock.restore();
        const retry = successor ? r.restart() : first;
        await retry.sweep();
        assert.deepEqual(recordNames(r), []);
        assert.equal((await slotOf(r))?.state, "retired");
      });
    }
  }

  it("an unknown non-UUID bundle with no record refuses the retry", async t => {
    const r = await rig(t, { codex: false, cohort: 3, memberIds: NON_V4 });
    await r.end();
    await r.receipt();
    const first = r.restart();
    const injected = failFirstBundle(t);
    await first.sweep();
    assert.equal(injected.fired(), true);
    t.mock.restoreAll();
    fs.writeFileSync(path.join(recordDir(r), "00000000-0000-1000-8000-0000000000ff.bundle"), "stray bundle");
    const files = snapshotFiles(r);
    await first.sweep();
    assert.deepEqual(snapshotFiles(r), files);
    assert.equal((await slotOf(r))?.state, "retiring");
  });
});

/** The identity the runner's pending-binder set uses for this rig's completing source (completionSourceIdentity). */
const binderKey = (r: Rig): string => JSON.stringify([r.runId, GENERATION, r.bare, BRANCH, r.clone, r.bound.attemptId]);
const binderSet = (p: unknown): Set<{ key: string }> => (p as { completionSourceBinds: Set<{ key: string }> }).completionSourceBinds;

describe("a fully deleted retirement never wedges the next use of the path", () => {
  it("a binder pending in the process defers every tombstone (the retirement's and the admission finisher's); once it clears, the next admission finishes it and issues a new epoch", async t => {
    const r = await rig(t, { codex: false, cohort: 2 });
    await r.end();
    await r.receipt();
    const published = await slotOf(r);
    const second = r.restart();
    const binder = { key: binderKey(r) };
    binderSet(second.p).add(binder);
    await second.sweep();
    assert.deepEqual(recordNames(r), [], "everything was deleted");
    const waiting = await slotVia(second.rt, r.clone);
    assert.equal(waiting?.state, "retiring", "the retirement's tombstone waited for the binder");
    assert.equal(waiting?.retiring?.deleted, waiting?.retiring?.manifest.length, "the whole manifest is recorded deleted");
    // The admission finisher consults the same pending-binder state: it does not tombstone beneath a pending binder.
    await assert.rejects(second.rt.withTransaction(() => second.rt.hooks.admitSourceUse(r.clone, "seed-canonical")), /being retired/);
    assert.equal((await slotVia(second.rt, r.clone))?.state, "retiring", "still retiring: no tombstone with a binder pending");
    binderSet(second.p).delete(binder);
    await second.rt.withTransaction(() => second.rt.hooks.admitSourceUse(r.clone, "seed-canonical"));
    const next = await slotVia(second.rt, r.clone);
    assert.equal(next?.state, "active");
    assert.notEqual(next?.epoch, published?.epoch, "fresh future use issues a new random epoch");
    assert.equal(next?.publication, null);
  });

  it("a retiring slot with undeleted artifacts is not finished by an admission: the path stays refused", async t => {
    const r = await rig(t, { codex: false, cohort: 2 });
    await r.end();
    await r.receipt();
    const first = r.restart();
    const real = fsp.unlink.bind(fsp);
    t.mock.method(fsp, "unlink", async (file: string) => {
      if (String(file).endsWith(".bundle")) throw Object.assign(new Error("EIO"), { code: "EIO" });
      return real(file);
    });
    await first.sweep();
    t.mock.restoreAll();
    const successor = r.restart();
    await assert.rejects(successor.rt.withTransaction(() => successor.rt.hooks.admitSourceUse(r.clone, "seed-canonical")), /being retired/);
    assert.equal((await slotVia(successor.rt, r.clone))?.state, "retiring");
  });

  it("a failed run-directory sync after the last unlink leaves the slot retiring, not retired, and no record to resume from", async t => {
    const r = await rig(t, { codex: false, cohort: 2 });
    await r.end();
    await r.receipt();
    const second = r.restart();
    (second.p.recovery as unknown as { publicationOps: { syncDir(h: unknown): Promise<void> } }).publicationOps.syncDir = async () => { throw new Error("injected run-directory sync failure"); };
    await second.sweep().catch(() => undefined);
    const slot = await slotAt(r, r.clone);
    assert.notEqual(slot?.state, "retired", "no tombstone before the record deletions are durable");
    assert.equal(slot?.state, "retiring");
    assert.deepEqual(recordNames(r), [], "every record is already gone: nothing receipt-bearing remains to resume from");
    assert.ok((slot?.retiring?.deleted ?? 0) < (slot?.retiring?.manifest.length ?? 0), "the final progress write never happened");
  });

  for (const successor of [false, true]) {
    it(`a slot left retiring after the last unlink is finished by the next admission when every manifest artifact is confirmed absent (${successor ? "successor incarnation" : "same worker"})`, async t => {
      const r = await rig(t, { codex: false, cohort: 2 });
      await r.end();
      await r.receipt();
      const published = await slotOf(r);
      const second = r.restart();
      (second.p.recovery as unknown as { publicationOps: { syncDir(h: unknown): Promise<void> } }).publicationOps.syncDir = async () => { throw new Error("injected run-directory sync failure"); };
      await second.sweep().catch(() => undefined);
      assert.equal((await slotVia(second.rt, r.clone))?.state, "retiring");
      const user = successor ? r.restart() : second;
      await user.rt.withTransaction(() => user.rt.hooks.admitSourceUse(r.clone, "seed-canonical"));
      const next = await slotVia(user.rt, r.clone);
      assert.equal(next?.state, "active");
      assert.notEqual(next?.epoch, published?.epoch, "a new epoch");
      assert.equal(next?.owner.worker, user.incarnation);
    });
  }

  it("a pending binder holding a NON-normalized spelling of the path still defers the admission finisher", async t => {
    const r = await rig(t, { codex: false, cohort: 2 });
    await r.end();
    await r.receipt();
    const second = r.restart();
    (second.p.recovery as unknown as { publicationOps: { syncDir(h: unknown): Promise<void> } }).publicationOps.syncDir = async () => { throw new Error("injected run-directory sync failure"); };
    await second.sweep().catch(() => undefined);
    assert.equal((await slotVia(second.rt, r.clone))?.state, "retiring");
    const odd = `${path.dirname(r.clone)}/./${path.basename(r.clone)}`;
    assert.notEqual(odd, r.clone, "the spelling really differs from the resolved slot key");
    assert.equal(path.resolve(odd), r.clone);
    const binder = { key: JSON.stringify([r.runId, GENERATION, r.bare, BRANCH, odd, r.bound.attemptId]) };
    binderSet(second.p).add(binder);
    await assert.rejects(second.rt.withTransaction(() => second.rt.hooks.admitSourceUse(r.clone, "seed-canonical")), /being retired/);
    assert.equal((await slotVia(second.rt, r.clone))?.state, "retiring", "no tombstone with a binder pending for this path");
    binderSet(second.p).delete(binder);
    await second.rt.withTransaction(() => second.rt.hooks.admitSourceUse(r.clone, "seed-canonical"));
    assert.equal((await slotVia(second.rt, r.clone))?.state, "active");
  });

  it("an artifact reappearing before the finisher runs (a stray record) keeps the slot retiring and the path refused", async t => {
    const r = await rig(t, { codex: false, cohort: 2 });
    await r.end();
    await r.receipt();
    const second = r.restart();
    (second.p.recovery as unknown as { publicationOps: { syncDir(h: unknown): Promise<void> } }).publicationOps.syncDir = async () => { throw new Error("injected run-directory sync failure"); };
    await second.sweep().catch(() => undefined);
    // The authority record is back: a manifest member that is present is not confirmed deleted.
    const name = `${r.captureIds[0]}.json`;
    fs.mkdirSync(recordDir(r), { recursive: true });
    fs.writeFileSync(path.join(recordDir(r), name), "{}");
    await assert.rejects(second.rt.withTransaction(() => second.rt.hooks.admitSourceUse(r.clone, "seed-canonical")), /being retired/);
    assert.equal((await slotVia(second.rt, r.clone))?.state, "retiring");
  });
});

describe("a retirement interrupted after the clone moved and before the skills dir was deleted completes on retry", () => {
  async function interrupted(t: Parameters<typeof rig>[0], codex = false): Promise<{ r: Rig; first: ReturnType<Rig["restart"]> }> {
    const r = await rig(t, { codex, cohort: 2 });
    await r.end();
    await r.receipt();
    const first = r.restart();
    t.mock.method(git, "removeCompletionSkillsDir", async () => { throw new Error("injected skills failure"); });
    await first.sweep();
    return { r, first };
  }

  for (const codex of [false, true]) {
    it(`${codex ? "Codex" : "Claude"}: the retiring slot keeps the skills dir and the records; the same worker's retry completes and tombstones`, async t => {
      const { r, first } = await interrupted(t, codex);
      assert.equal(fs.existsSync(r.clone), false, "Git moved the clone");
      assert.equal(journalRaw(r), "", "and cleared the journal fence");
      assert.ok(fs.existsSync(r.skills), "the skills dir is still there");
      assert.equal((await slotOf(r))?.state, "retiring");
      t.mock.restoreAll();
      await first.sweep();
      assert.equal(fs.existsSync(r.skills), false, "the retry removed the skills dir");
      assert.deepEqual(recordNames(r), []);
      assert.equal((await slotOf(r))?.state, "retired");
    });

    it(`${codex ? "Codex" : "Claude"}: a successor incarnation retries after the owner died between the clone move and the skills delete`, async t => {
      const { r } = await interrupted(t, codex);
      t.mock.restoreAll();
      const successor = r.restart();
      await successor.sweep();
      assert.equal(fs.existsSync(r.skills), false);
      assert.deepEqual(recordNames(r), []);
      const slot = await slotVia(successor.rt, r.clone);
      assert.equal(slot?.state, "retired");
      assert.equal(slot?.owner.worker, successor.incarnation);
    });
  }

  it("the manifest lists the artifacts in the order they are deleted: clone, fence, skills, bundles, records", async t => {
    const { r } = await interrupted(t);
    const manifest = (await slotOf(r))?.retiring?.manifest ?? [];
    const kinds = manifest.map(item => item.startsWith("fence:") ? "fence" : item.startsWith("bundle:") ? "bundle" : item.startsWith("record:") ? "record"
      : path.basename(item).startsWith(".uzi-skills-") ? "skills" : "clone");
    assert.deepEqual(kinds, ["clone", "fence", "skills", "bundle", "bundle", "record", "record"]);
  });
});

describe("the skills dir is deleted only after its own quiescence proof", () => {
  type Quiesce = (req: { site?: string; targetPaths: string[] }) => Promise<unknown>;
  const outcome = (process: string, docker: string): unknown => ({ process: { state: process }, docker: { state: docker } });

  async function withQuiesce(t: Parameters<typeof rig>[0], decide: (req: { site?: string; targetPaths: string[] }) => unknown,
    options: { dockerHost?: string } = {}): Promise<{ r: Rig; second: ReturnType<Rig["restart"]>; skillsRequests: Array<{ site?: string; targetPaths: string[] }> }> {
    const r = await rig(t, { codex: false, cohort: 2 });
    await r.end();
    await r.receipt();
    const second = r.restart();
    const skillsRequests: Array<{ site?: string; targetPaths: string[] }> = [];
    (second.p as unknown as { quiesceImpl: Quiesce }).quiesceImpl = async req => {
      if (req.site === "completion_source:skills") { skillsRequests.push({ site: req.site, targetPaths: [...req.targetPaths] }); return decide(req); }
      return outcome("quiescent", "docker_unconfirmed");
    };
    if (options.dockerHost !== undefined) (second.p as unknown as { dockerHost: string }).dockerHost = options.dockerHost;
    return { r, second, skillsRequests };
  }

  it("a skills dir whose writers are not proven gone is retained: the retiring record stays and nothing further is deleted", async t => {
    let quiet = false;
    const { r, second, skillsRequests } = await withQuiesce(t, () => outcome(quiet ? "quiescent" : "survivors", "docker_unconfirmed"));
    const removeSkills = t.mock.method(git, "removeCompletionSkillsDir");
    await second.sweep();
    assert.equal(removeSkills.mock.callCount(), 0, "no delete was attempted");
    assert.deepEqual(skillsRequests, [{ site: "completion_source:skills", targetPaths: [r.skills] }], "exactly the paired skills path was proven");
    assert.ok(fs.existsSync(r.skills));
    assert.equal(recordNames(r).length, 4, "records and bundles remain");
    assert.equal((await slotOf(r))?.state, "retiring");
    quiet = true;
    await second.sweep();
    assert.equal(fs.existsSync(r.skills), false, "the later retry, with the proof, completes");
    assert.deepEqual(recordNames(r), []);
    assert.equal((await slotOf(r))?.state, "retired");
  });

  it("Docker-wired: only docker_unconfirmed proves it; docker_error retains, docker_unconfirmed proceeds", async t => {
    let docker = "docker_error";
    const { r, second } = await withQuiesce(t, () => outcome("quiescent", docker), { dockerHost: "unix:///docker-test.sock" });
    const removeSkills = t.mock.method(git, "removeCompletionSkillsDir");
    await second.sweep();
    assert.equal(removeSkills.mock.callCount(), 0);
    assert.ok(fs.existsSync(r.skills));
    assert.equal((await slotOf(r))?.state, "retiring");
    docker = "docker_unconfirmed";
    await second.sweep();
    assert.equal(fs.existsSync(r.skills), false);
    assert.equal((await slotOf(r))?.state, "retired");
  });
});

describe("a completed source retires in the SAME process too (no restart)", () => {
  for (const codex of [false, true]) {
    it(`${codex ? "Codex" : "Claude"}: the publishing worker's own sweep retires the exact clone, skills, journal entry, bundles and records and tombstones`, async t => {
      const r = await rig(t, { codex, cohort: 2 });
      await r.end();
      await r.receipt();
      assert.ok(cloneAlive(r));
      const snapshot = (await r.p.recovery.inspect(r.runId)).filter(x => x.completionReceipt);
      await r.p.recovery.resumePending(undefined, snapshot);
      assert.equal(fs.existsSync(r.clone), false, "the clone is retired by the publishing process");
      assert.equal(fs.existsSync(r.skills), false);
      assert.equal(journalRaw(r), "");
      assert.deepEqual(recordNames(r), []);
      assert.equal((await slotOf(r))?.state, "retired");
    });
  }
});

describe("the withCompletionSourceBoundary memory veto", () => {
  it("a runner with an authority runtime and no durable retirement never releases a source on memory alone", async t => {
    const r = await rig(t, { codex: false, cohort: 2 });
    await r.end();
    await r.receipt();
    const second = r.restart();
    const p = second.p as unknown as {
      classifyCompletionSource(s: unknown): { kind: string };
      withCompletionSourceBoundary(s: unknown, action: (open: () => boolean) => Promise<void>): Promise<string>;
    };
    assert.equal(p.classifyCompletionSource(r.bound).kind, "noReliableBinding");
    const inventory = t.mock.method(git, "readInventoryCloneHeads");
    let ran = 0;
    assert.equal(await p.withCompletionSourceBoundary(r.bound, async () => { ran++; }), "retained");
    assert.equal(ran, 0);
    assert.equal(inventory.mock.callCount(), 0, "refused before any inventory read");
    assert.ok(fs.existsSync(path.join(r.clone, ".git")), "the clone stays");
    assert.equal((await slotOf(r))?.state, "claude_ready");
  });
});

describe("operator discard never takes over a published source or a restart retirement", () => {
  const discardFootprint = (r: Rig): string[] => [r.clone, r.skills];
  const attempt = (rt: Rig["rt"], r: Rig): Promise<unknown> => rt.withTransaction(async tx => {
    const store = await openAuthorityStore(tx, { workerToken: TOKEN });
    return store.admitOperatorDiscard(tx, r.clone, discardFootprint(r));
  });

  for (const codex of [false, true]) {
    it(`${codex ? "sealed" : "claude_ready"}: refused, the slot and publication are unchanged`, async t => {
      const r = await rig(t, { codex, cohort: 2 });
      await r.end();
      const before = await slotOf(r);
      assert.equal(before?.state, codex ? "sealed" : "claude_ready");
      await assert.rejects(attempt(r.restart().rt, r), /published/);
      assert.deepEqual(await slotOf(r), before);
    });
  }

  it("a restart retirement in progress (retiring, authenticated manifest) is refused and left intact", async t => {
    const r = await rig(t, { codex: false, cohort: 2 });
    await r.end();
    await r.receipt();
    const first = r.restart();
    t.mock.method(git, "removeCompletionSkillsDir", async () => { throw new Error("injected skills failure"); });
    await first.sweep();
    t.mock.restoreAll();
    const before = await slotOf(r);
    assert.equal(before?.state, "retiring");
    await assert.rejects(attempt(r.restart().rt, r), /being retired/);
    assert.deepEqual(await slotOf(r), before, "the authenticated retirement manifest was not replaced");
  });
});

describe("the bare ref deletions of the retirement go through the existing bare-mutation hooks", () => {
  it("recovery cleanup deletes refs under a bare-mutation record that settles ok", async t => {
    const r = await rig(t, { codex: false, cohort: 2 });
    await r.end();
    await r.receipt();
    const second = r.restart();
    const kinds: string[] = [];
    const real = second.rt.hooks.beginBareMutation;
    (second.rt.hooks as { beginBareMutation: typeof real }).beginBareMutation = async (bare, op, footprint) => { kinds.push(op); return real(bare, op, footprint); };
    Object.assign(git, { authority: second.rt.hooks });
    await second.sweep();
    assert.equal(fs.existsSync(r.clone), false);
    for (const op of kinds) assert.equal(op, "recovery-cleanup-delete-ref");
    assert.equal((await slotAt(r, r.bare))?.pending ?? null, null, "no bare mutation is left unresolved");
  });
});


describe("a retirement that crashed after the attempt ledger append and before the journal clear", () => {
  /** Every write that clears the completing journal fails (Git retires once on EIO, so both tries fail). */
  const failJournalClear = (t: TestContext): void => {
    const target = git as unknown as { runGit(bare: string, args: string[], ...rest: unknown[]): Promise<string> };
    const real = target.runGit.bind(git);
    t.mock.method(target, "runGit", async (bare: string, args: string[], ...rest: unknown[]) => {
      if (args[0] === "config" && args[2] === `uzi-recovery.${BRANCH}.clone` && args[3] === "") throw Object.assign(new Error("EIO"), { code: "EIO" });
      return real(bare, args, ...rest);
    });
  };
  const ledger = (r: Rig): string => execFileSync("git", ["-C", r.bare, "config", "--get-all", `uzi-attempts.${BRANCH}.entry`], { encoding: "utf8", env: ENV });

  async function crashed(t: TestContext) {
    const r = await rig(t, { codex: false, cohort: 2 });
    await r.end();
    await r.receipt();
    const first = r.restart();
    failJournalClear(t);
    await first.sweep();
    assert.equal(fs.existsSync(r.clone), false, "the clone is gone");
    assert.match(ledger(r), /retired/, "the ledger already says retired");
    assert.ok(fenceOf(r), "the journal still names the source with its fence");
    assert.equal((await slotOf(r))?.state, "retiring");
    return { r, first };
  }

  for (const successor of [false, true]) {
    it(`the retry clears the fenced journal and completes: records go and the slot tombstones (${successor ? "successor incarnation" : "same worker"})`, async t => {
      const { r, first } = await crashed(t);
      t.mock.restoreAll();
      await (successor ? r.restart() : first).sweep();
      assert.equal(journalRaw(r), "", "the fenced journal was cleared by the certified step");
      assert.deepEqual(recordNames(r), []);
      assert.equal(fs.existsSync(r.skills), false);
      assert.equal((await slotOf(r))?.state, "retired");
    });
  }

  it("a journal carrying a different fence is not cleared", async t => {
    const { r, first } = await crashed(t);
    t.mock.restoreAll();
    execFileSync("git", ["-C", r.bare, "config", "--local", `uzi-recovery.${BRANCH}.clone`, JSON.stringify({
      runId: r.runId, clonePath: r.clone, attemptId: r.bound.attemptId,
      completionSourceFence: { version: 1, sourceUseEpoch: "99999999-9999-4999-8999-999999999999", sourceCoreDigest: "a".repeat(64) } })], { env: ENV });
    const raw = journalRaw(r);
    const files = snapshotFiles(r);
    await first.sweep();
    assert.equal(journalRaw(r), raw);
    assert.deepEqual(snapshotFiles(r), files);
    assert.equal((await slotOf(r))?.state, "retiring");
  });
});

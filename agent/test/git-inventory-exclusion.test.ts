// issue #4632 U2: the same-key inventory exclusion in GitCache (registry, scope-classed clear, locked section,
// seed-site refusals, reclaim admission) and the namespace restart boundary detector. The observe-only probe is a
// fake; the git layer, the bare repo and the clone paths are real.
import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { execFileSync } from "node:child_process";
import {
  GitCache,
  INVENTORY_CONTENT_PROOF_BOUNDS,
  type InventoryExclusionProbe,
  type InventoryExclusionProbeRequest,
  type InventorySection,
  type InventorySourceLockOptions,
} from "../src/git.js";
import {
  INVENTORY_LOCK_ACQUIRE_MS,
  InventoryExclusionRegistry,
  InventorySourceExclusionRefusal,
  detectNamespaceRestartBoundary,
} from "../src/inventory-exclusion.js";
import { cloneKeyOf } from "../src/attempt-path.js";
import { inventoryClearVerdict, mintAttemptId, type ProcessQuiescence } from "../src/run-quiescence.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { noProofReseed, recordingLogger, testGitCacheOptions } from "./helpers.js";
import { defaultGitleaksShim } from "./gitleaks-shim.js";

const cleanEnv = Object.fromEntries(Object.entries(process.env).filter(([key]) => !key.startsWith("GIT_")));
const GIT_ENV = { ...cleanEnv, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };

interface Priv {
  clearExclusion(bare: string, branch: string, canonical: string, isLive: (p: string) => boolean, cls: "full" | "restore",
    deadlineAt: number, opaque?: ReadonlySet<string>, protectedMarkers?: string[]): Promise<"admit" | "deferred_restore" | "not_clear">;
  inventoryExclusion: InventoryExclusionRegistry;
  boundaryProcesses: { run<R>(scope: unknown, fn: () => R): R };
  restoreHolder(entries: readonly string[], branch: string): { runId?: string } | undefined;
  nonLiveKeyPaths(...a: unknown[]): Promise<string[]>;
  pathPresent(p: string): Promise<boolean>;
  assertBareClear(bare: string, op: string): Promise<void>;
  holdsLock(key: string): boolean;
}

const tick = (ms: number): Promise<void> => new Promise((r) => setTimeout(r, ms));
function deferred<T = void>(): { promise: Promise<T>; resolve: (v: T) => void } {
  let resolve!: (v: T) => void;
  const promise = new Promise<T>((r) => { resolve = r; });
  return { promise, resolve };
}

let fx: Fixture;
let git: GitCache;
let priv: Priv;
let bare: string;
let rec: ReturnType<typeof recordingLogger>;
let probeCalls: InventoryExclusionProbeRequest[];
let verdict: "clear" | "not_clear";
const branch = "agent/issue-4632";

beforeEach(async () => {
  fx = makeFixture({});
  rec = recordingLogger();
  git = new GitCache(fx.dataDir, rec.logger, undefined, testGitCacheOptions({ gitleaksBin: defaultGitleaksShim() }));
  priv = git as unknown as Priv;
  bare = await git.ensureClone(fx.originPath);
  probeCalls = [];
  verdict = "clear";
  const probe: InventoryExclusionProbe = async (req) => { probeCalls.push(req); return verdict; };
  git.setInventoryExclusionProbe(probe);
});
afterEach(() => { fx.cleanup(); });

const canonicalOf = (key: string): string => git.runnerClonePath(bare, key);
const keyOf = (key: string): string => cloneKeyOf(canonicalOf(key)).cloneKey;
const section = <T,>(key: string, body: (s: InventorySection) => Promise<T>, o: Partial<InventorySourceLockOptions> = {}) =>
  git.withInventorySourceLock(bare, { clonePath: canonicalOf(key), branch, isLive: () => false, ...o }, body);
const withoutHeld = (r: { heldMs?: number }): object => { const { heldMs: _heldMs, ...rest } = r; return rest; };
const logged = (msg: string): boolean => rec.lines.some((l) => (l as { msg?: string }).msg === msg);

/** A section that started git, settled it and found the clone quiescent, with the given probe verdict. */
async function leaveEntry(key: string, v: "clear" | "not_clear" = "not_clear"): Promise<void> {
  verdict = v;
  const r = await section(key, async (s) => {
    s.markGitStarted();
    await s.stage(async () => 1);
    s.afterScanQuiescent(true);
  });
  assert.equal(r.kind, "ran");
}

describe("registry", () => {
  it("tracks entries, settlement and admissions", async () => {
    const reg = new InventoryExclusionRegistry();
    const entry = { id: "e1", clonePath: "/c", gitStarted: false, pending: new Set<Promise<unknown>>() };
    assert.equal(reg.excluded("b", "r/k"), false);
    assert.equal(reg.allPendingSettled("b", "r/k"), true);
    reg.addEntry("b", "r/k", entry);
    assert.equal(reg.excluded("b", "r/k"), true);
    assert.equal(reg.excluded("b", "r/other"), false);
    const d = deferred();
    reg.trackPending(entry, d.promise);
    assert.equal(reg.allPendingSettled("b", "r/k"), false);
    let settled = false;
    const waiting = reg.settled("b", "r/k").then(() => { settled = true; });
    await tick(5);
    assert.equal(settled, false);
    d.resolve();
    await waiting;
    assert.equal(reg.allPendingSettled("b", "r/k"), true);
    reg.removeEntry("b", "r/k", "e1");
    assert.equal(reg.excluded("b", "r/k"), false);
    assert.equal(reg.admissionAge("b", "r/k"), undefined);
    reg.addAdmission("b", "r/k", "a1", 1_000);
    reg.addAdmission("b", "r/k", "a2", 4_000);
    assert.equal(reg.admissionAge("b", "r/k", 5_000), 4_000);
    reg.removeAdmission("b", "r/k", "a1");
    assert.equal(reg.admissionAge("b", "r/k", 5_000), 1_000);
    reg.removeAdmission("b", "r/k", "a2");
    assert.equal(reg.admissionAge("b", "r/k"), undefined);
  });

  it("a rejected tracked promise counts as settled", async () => {
    const reg = new InventoryExclusionRegistry();
    const entry = { id: "e", clonePath: "/c", gitStarted: true, pending: new Set<Promise<unknown>>() };
    reg.addEntry("b", "r/k", entry);
    const d = deferred();
    reg.trackPending(entry, d.promise.then(() => { throw new Error("boom"); }));
    d.resolve();
    await reg.settled("b", "r/k");
    assert.equal(reg.allPendingSettled("b", "r/k"), true);
  });
});

describe("section lifecycle", () => {
  it("removes the entry when git never started", async () => {
    const r = await section("issue-1", async () => "v");
    assert.deepEqual(withoutHeld(r), { kind: "ran", value: "v", timedOut: false, aborted: false, entryKept: false });
    assert.equal(priv.inventoryExclusion.excluded(bare, keyOf("issue-1")), false);
    assert.equal(probeCalls.length, 0);
  });

  it("a started git is cleared only by the step-7 full verdict, over the key's non-live paths", async () => {
    const canonical = canonicalOf("issue-2");
    const sibling = `${canonical}.attempt-${mintAttemptId(1)}`;
    fs.mkdirSync(sibling, { recursive: true });
    fs.mkdirSync(canonical, { recursive: true });
    const r = await section("issue-2", async (s) => {
      s.markGitStarted();
      await s.stage(async () => 1);
      s.afterScanQuiescent(true);
      return 7;
    });
    assert.deepEqual(withoutHeld(r), { kind: "ran", value: 7, timedOut: false, aborted: false, entryKept: false });
    assert.equal(probeCalls.length, 1);
    assert.deepEqual(probeCalls[0]!.targetPaths, [canonical, sibling].sort());
    assert.equal(probeCalls[0]!.canonicalPath, canonical);
    assert.equal(priv.inventoryExclusion.excluded(bare, keyOf("issue-2")), false);
  });

  it("keeps the entry on a not_clear step-7 verdict and clears it lazily at the next section", async () => {
    await leaveEntry("issue-3");
    assert.equal(priv.inventoryExclusion.excluded(bare, keyOf("issue-3")), true);
    assert.ok(logged("recovery: inventory_source_exclusion_retained"));
    verdict = "clear";
    const before = probeCalls.length;
    const r = await section("issue-3", async () => "second");
    assert.equal(r.kind, "ran");
    assert.equal(probeCalls.length, before + 1, "the lazy clear probed once");
    assert.equal(priv.inventoryExclusion.excluded(bare, keyOf("issue-3")), false);
  });

  it("reports the reason a started git stayed excluded", async () => {
    verdict = "not_clear";
    const notClear = await section("issue-4", async (s) => { s.markGitStarted(); s.afterScanQuiescent(true); });
    assert.deepEqual(withoutHeld(notClear), { kind: "ran", value: undefined, timedOut: false, aborted: false, entryKept: true, keptReason: "full_probe_not_clear" });
    const calls = probeCalls.length;
    const skipped = await section("issue-5", async (s) => { s.markGitStarted(); s.afterScanQuiescent(false); });
    assert.equal(skipped.kind === "ran" && skipped.keptReason, "full_probe_skipped");
    const never = await section("issue-6", async (s) => { s.markGitStarted(); });
    assert.equal(never.kind === "ran" && never.keptReason, "full_probe_skipped");
    assert.equal(probeCalls.length, calls, "no step-7 probe without a quiescent after-scan");
    const tooLate = await section("issue-7", async (s) => { s.markGitStarted(); s.afterScanQuiescent(true); }, { deadlineMs: 500 });
    assert.equal(tooLate.kind === "ran" && tooLate.keptReason, "full_probe_skipped", "under the minimum time no probe is attempted");
    assert.equal(probeCalls.length, calls);
  });

  it("a timed-out body keeps the entry until its staged work settles, and a late continuation cannot clear", async () => {
    const hold = deferred();
    let late: Promise<unknown> = Promise.resolve();
    const r = await section("issue-8", async (s) => {
      s.markGitStarted();
      const staged = await s.stage(() => hold.promise);
      late = Promise.resolve().then(() => { s.afterScanQuiescent(true); });
      return staged;
    }, { deadlineMs: 1_300 });
    assert.equal(r.kind === "ran" && r.timedOut, true);
    assert.equal(r.kind === "ran" && r.entryKept, true);
    assert.equal(r.kind === "ran" && r.keptReason, "unsettled");
    assert.ok(logged("recovery: inventory_source_settlement_unproven"));
    const k = keyOf("issue-8");
    assert.equal(priv.inventoryExclusion.allPendingSettled(bare, k), false);
    hold.resolve();
    await git.inventoryExclusionSettled(bare, k);
    await late;
    assert.equal(priv.inventoryExclusion.excluded(bare, k), true, "a late continuation never clears");
    assert.equal(probeCalls.length, 0);
  });

  it("an abort keeps the entry and starts no stage afterwards", async () => {
    let started = false;
    const ac = new AbortController();
    const r = await section("issue-9", async (s) => {
      s.markGitStarted();
      s.noteAbort();
      assert.equal(s.stopped(), true);
      const staged = await s.stage(async () => { started = true; return 1; });
      assert.deepEqual(staged, { ok: false, why: "stopped" });
    }, { signal: ac.signal });
    assert.equal(started, false);
    assert.equal(r.kind === "ran" && r.entryKept, true);
    assert.equal(probeCalls.length, 0);
    assert.ok(logged("recovery: inventory_source_settlement_unproven"));
  });

  it("an aborted caller signal stops the section", async () => {
    const ac = new AbortController();
    const r = await section("issue-10", async (s) => {
      s.markGitStarted();
      ac.abort();
      assert.equal(s.stopped(), true);
      s.afterScanQuiescent(true);
    }, { signal: ac.signal });
    assert.equal(r.kind === "ran" && r.entryKept, true);
    assert.equal(probeCalls.length, 0);
  });

  it("a body that never returns is raced against the deadline", async () => {
    const r = await section("issue-11", () => new Promise<never>(() => {}), { deadlineMs: 120 });
    assert.equal(r.kind === "ran" && r.timedOut, true);
    assert.equal(priv.inventoryExclusion.excluded(bare, keyOf("issue-11")), false, "git never started and nothing pending");
  });

  it("a stage started after the deadline but before the timer fired is refused on the clock", async () => {
    let second = false;
    let firstValue: unknown;
    let secondValue: unknown;
    let latchedBeforeSecond: boolean | undefined;
    const r = await section("issue-15", async (s) => {
      s.markGitStarted();
      // Block the event loop past D inside the first stage, so the deadline timer cannot fire before the next stage
      // starts: only the clock check can refuse it.
      firstValue = await s.stage(async () => {
        const until = s.deadlineAt + 20;
        while (Date.now() < until) { /* busy-wait past D */ }
        return 1;
      });
      latchedBeforeSecond = s.stopped();
      secondValue = await s.stage(async () => { second = true; return 2; });
      s.afterScanQuiescent(true);
    }, { deadlineMs: 150 });
    assert.deepEqual(firstValue, { ok: true, value: 1 });
    assert.equal(latchedBeforeSecond, false, "the timer had not latched yet");
    assert.deepEqual(secondValue, { ok: false, why: "deadline" });
    assert.equal(second, false, "the stage body was never invoked");
    assert.equal(r.kind === "ran" && r.timedOut, true);
    assert.equal(r.kind === "ran" && r.entryKept, true);
    assert.equal(priv.inventoryExclusion.excluded(bare, keyOf("issue-15")), true);
    assert.equal(probeCalls.length, 0);
  });

  it("an abort while the step-7 probe is pending keeps the entry and reports aborted", async () => {
    const ac = new AbortController();
    git.setInventoryExclusionProbe(async () => { ac.abort(); await tick(5); return "clear"; });
    const r = await section("issue-13", async (s) => {
      s.markGitStarted();
      await s.stage(async () => 1);
      s.afterScanQuiescent(true);
      return "body";
    }, { signal: ac.signal });
    assert.equal(r.kind === "ran" && r.aborted, true);
    assert.equal(r.kind === "ran" && r.timedOut, false);
    assert.equal(r.kind === "ran" && r.entryKept, true);
    assert.equal(priv.inventoryExclusion.excluded(bare, keyOf("issue-13")), true, "the abort kept the entry");
  });

  it("heldMs covers a slow lazy clear", async () => {
    await leaveEntry("issue-14");
    git.setInventoryExclusionProbe(async () => { await tick(150); return "clear"; });
    const r = await section("issue-14", async () => "x");
    assert.equal(r.kind, "ran");
    assert.ok(r.kind === "ran" && r.heldMs >= 140, `heldMs ${r.kind === "ran" ? r.heldMs : "n/a"} covers the 150 ms lazy clear`);
  });

  it("two entries of one key are all removed by one clear full verdict", async () => {
    const k = keyOf("issue-12");
    const extra = { id: "extra", clonePath: canonicalOf("issue-12"), gitStarted: true, pending: new Set<Promise<unknown>>() };
    priv.inventoryExclusion.addEntry(bare, k, extra);
    const d = deferred();
    priv.inventoryExclusion.trackPending(extra, d.promise);
    const refused = await section("issue-12", async () => "never");
    assert.equal(refused.kind, "refused");
    assert.equal(refused.kind === "refused" && refused.check, "source_settlement_unproven");
    assert.equal(probeCalls.length, 0, "the probe runs only when everything settled");
    d.resolve();
    await git.inventoryExclusionSettled(bare, k);
    priv.inventoryExclusion.addEntry(bare, k, { id: "second", clonePath: canonicalOf("issue-12"), gitStarted: true, pending: new Set() });
    const ran = await section("issue-12", async () => "ran");
    assert.equal(ran.kind, "ran");
    assert.equal(probeCalls.length, 1);
    assert.equal(priv.inventoryExclusion.excluded(bare, k), false);
  });
});

describe("clearExclusion", () => {
  it("admits at once without an entry", async () => {
    assert.equal(await priv.clearExclusion(bare, branch, canonicalOf("issue-20"), () => false, "full", Date.now() + 1_000), "admit");
    assert.equal(probeCalls.length, 0);
  });

  it("fails closed without a registered probe", async () => {
    const fresh = new GitCache(fx.dataDir, rec.logger, undefined, testGitCacheOptions({ gitleaksBin: defaultGitleaksShim() }));
    const p = fresh as unknown as Priv;
    p.inventoryExclusion.addEntry(bare, keyOf("issue-21"), { id: "x", clonePath: "/c", gitStarted: true, pending: new Set() });
    assert.equal(await p.clearExclusion(bare, branch, canonicalOf("issue-21"), () => false, "full", Date.now() + 1_000), "not_clear");
  });

  it("a probe verdict that arrives after the deadline is not_clear", async () => {
    priv.inventoryExclusion.addEntry(bare, keyOf("issue-22"), { id: "x", clonePath: "/c", gitStarted: true, pending: new Set() });
    git.setInventoryExclusionProbe(async () => { await tick(150); return "clear"; });
    assert.equal(await priv.clearExclusion(bare, branch, canonicalOf("issue-22"), () => false, "full", Date.now() + 60), "not_clear");
    assert.equal(priv.inventoryExclusion.excluded(bare, keyOf("issue-22")), true);
  });

  it("a timed-out probe keeps the key unsettled until it settles, and a second clear removes nothing meanwhile", async () => {
    const k = keyOf("issue-26");
    priv.inventoryExclusion.addEntry(bare, k, { id: "x", clonePath: "/c", gitStarted: true, pending: new Set() });
    const hold = deferred<"clear">();
    let calls = 0;
    git.setInventoryExclusionProbe(() => { calls++; return hold.promise; });
    assert.equal(await priv.clearExclusion(bare, branch, canonicalOf("issue-26"), () => false, "full", Date.now() + 60), "not_clear");
    assert.equal(priv.inventoryExclusion.allPendingSettled(bare, k), false, "the abandoned probe is still pending");
    assert.equal(await priv.clearExclusion(bare, branch, canonicalOf("issue-26"), () => false, "full", Date.now() + 1_000), "not_clear");
    assert.equal(calls, 1, "no second probe while the first is unsettled");
    assert.equal(priv.inventoryExclusion.excluded(bare, k), true);
    hold.resolve("clear");
    await git.inventoryExclusionSettled(bare, k);
    assert.equal(priv.inventoryExclusion.allPendingSettled(bare, k), true);
    assert.equal(await priv.clearExclusion(bare, branch, canonicalOf("issue-26"), () => false, "full", Date.now() + 1_000), "admit");
    assert.equal(priv.inventoryExclusion.excluded(bare, k), false);
  });

  it("full under a restore entry is deferred with no probe and no lstat of the key", async (t) => {
    const k = keyOf("issue-23");
    priv.inventoryExclusion.addEntry(bare, k, { id: "x", clonePath: "/c", gitStarted: true, pending: new Set() });
    t.mock.method(priv, "restoreHolder", () => ({ runId: "r" }));
    const lstats = t.mock.method(priv, "pathPresent", async () => true);
    const paths = t.mock.method(priv, "nonLiveKeyPaths", async () => []);
    assert.equal(await priv.clearExclusion(bare, branch, canonicalOf("issue-23"), () => false, "full", Date.now() + 1_000), "deferred_restore");
    assert.equal(probeCalls.length, 0);
    assert.equal(lstats.mock.callCount(), 0);
    assert.equal(paths.mock.callCount(), 0);
    assert.equal(priv.inventoryExclusion.excluded(bare, k), true);
  });

  it("restore admits one call, keeps the entry and never names an opaque path", async (t) => {
    const k = keyOf("issue-24");
    priv.inventoryExclusion.addEntry(bare, k, { id: "x", clonePath: "/c", gitStarted: true, pending: new Set() });
    const canonical = canonicalOf("issue-24");
    const opaquePath = `${canonical}.attempt-${mintAttemptId(1)}`;
    fs.mkdirSync(opaquePath, { recursive: true });
    const live = `${canonical}.attempt-${mintAttemptId(2)}`;
    fs.mkdirSync(live, { recursive: true });
    const seen: string[] = [];
    const real = priv.pathPresent.bind(priv);
    t.mock.method(priv, "pathPresent", async (p: string) => { seen.push(p); return real(p); });
    const out = await priv.clearExclusion(bare, branch, canonical, (p) => p === live, "restore", Date.now() + 2_000, new Set([opaquePath]), ["run:att"]);
    assert.equal(out, "admit");
    assert.equal(priv.inventoryExclusion.excluded(bare, k), true, "restore removes nothing");
    assert.deepEqual(probeCalls[0]!.targetPaths, []);
    assert.deepEqual(probeCalls[0]!.protectedMarkers, ["run:att"]);
    assert.ok(!seen.includes(opaquePath), "the opaque path is never lstat'd");
  });

  it("a scope read failure is not_clear", async (t) => {
    priv.inventoryExclusion.addEntry(bare, keyOf("issue-25"), { id: "x", clonePath: "/c", gitStarted: true, pending: new Set() });
    t.mock.method(priv, "nonLiveKeyPaths", async () => { throw new Error("EACCES"); });
    assert.equal(await priv.clearExclusion(bare, branch, canonicalOf("issue-25"), () => false, "full", Date.now() + 1_000), "not_clear");
    assert.equal(probeCalls.length, 0);
  });
});

describe("section refusals", () => {
  it("refuses a nested call under the bare lock", async () => {
    const r = await git.withBareLock(bare, () => section("issue-30", async () => "never"));
    assert.equal(r.kind === "refused" && r.check, "source_lock_unavailable");
  });

  it("refuses under a scope that releases the lock itself", async () => {
    const r = await priv.boundaryProcesses.run(
      { spawn: () => { throw new Error("unused"); }, signal: new AbortController().signal, beforeLockRelease: async () => {} },
      () => section("issue-31", async () => "never"));
    assert.equal(r.kind === "refused" && r.check, "source_lock_unavailable");
  });

  it("refuses when the lock cannot be acquired before the caller aborts", async () => {
    const release = deferred();
    const held = git.withBareLock(bare, () => release.promise);
    const ac = new AbortController();
    setTimeout(() => ac.abort(), 40);
    let called = false;
    const r = await section("issue-32", async () => { called = true; }, { signal: ac.signal });
    assert.equal(r.kind === "refused" && r.check, "source_lock_unavailable");
    assert.equal(called, false);
    release.resolve();
    await held;
  });

  it("refuses while a reclaim admission covers the key, and runs once it ends", async () => {
    const target = { path: canonicalOf("issue-33"), branch };
    const gate = deferred();
    let inside: Awaited<ReturnType<typeof section>> | undefined;
    const reclaim = git.withReclaimAdmission(bare, [target], () => false, async () => {
      inside = await section("issue-33", async () => "never");
      await gate.promise;
    });
    await tick(30);
    assert.equal(inside?.kind === "refused" && inside.check, "source_lock_unavailable");
    assert.match(inside?.kind === "refused" ? inside.detail : "", /age \d+ ms/);
    gate.resolve();
    await reclaim;
    const after = await section("issue-33", async () => "ok");
    assert.equal(after.kind === "ran" && after.value, "ok");
  });
});

describe("withReclaimAdmission", () => {
  it("refuses a key that stays excluded and installs no admission", async () => {
    await leaveEntry("issue-40", "not_clear");
    verdict = "not_clear";
    let ran = false;
    await assert.rejects(
      git.withReclaimAdmission(bare, [{ path: canonicalOf("issue-40"), branch }], () => false, async () => { ran = true; }),
      (e: unknown) => e instanceof InventorySourceExclusionRefusal && e.reason === "excluded" && e.key === keyOf("issue-40"));
    assert.equal(ran, false);
    assert.equal(priv.inventoryExclusion.admissionAge(bare, keyOf("issue-40")), undefined);
  });

  it("clears a clearable key, overlaps, and removes its admission when fn throws", async () => {
    await leaveEntry("issue-41", "not_clear");
    verdict = "clear";
    const k = keyOf("issue-41");
    const target = { path: canonicalOf("issue-41"), branch };
    const g1 = deferred();
    const g2 = deferred();
    const a = git.withReclaimAdmission(bare, [target], () => false, () => g1.promise);
    const b = git.withReclaimAdmission(bare, [target], () => false, () => g2.promise);
    await tick(30);
    assert.equal(priv.inventoryExclusion.excluded(bare, k), false, "the clear removed the entry");
    assert.notEqual(priv.inventoryExclusion.admissionAge(bare, k), undefined);
    g1.resolve();
    await a;
    assert.notEqual(priv.inventoryExclusion.admissionAge(bare, k), undefined, "the other admission still stands");
    g2.resolve();
    await b;
    assert.equal(priv.inventoryExclusion.admissionAge(bare, k), undefined);
    await assert.rejects(git.withReclaimAdmission(bare, [target], () => false, async () => { throw new Error("fn failed"); }), /fn failed/);
    assert.equal(priv.inventoryExclusion.admissionAge(bare, k), undefined);
  });
});

describe("seed sites", () => {
  const attemptOpts = (id: string) => ({ attemptId: id, isLive: () => false, beforeSeed: async () => {} });

  it("attemptCloneForBranch refuses an excluded key before assertBareClear and seeds nothing", async (t) => {
    const iid = 50;
    const key = `issue-${iid}`;
    await leaveEntry(key, "not_clear");
    verdict = "not_clear";
    const assertClear = t.mock.method(priv, "assertBareClear", async () => {});
    const id = mintAttemptId(1);
    await assert.rejects(
      git.createOrAttachRunnerClone(bare, iid, noProofReseed, "run-a", false, undefined, attemptOpts(id)),
      (e: unknown) => e instanceof InventorySourceExclusionRefusal && e.reason === "excluded");
    assert.equal(assertClear.mock.callCount(), 0);
    assert.equal(fs.existsSync(`${canonicalOf(key)}.attempt-${id}`), false);
    assert.throws(() => execFileSync("git", ["-C", bare, "config", "--get-all", `uzi-attempts.agent/issue-${iid}.entry`], { env: GIT_ENV, stdio: "pipe" }),
      (e: unknown) => (e as { status?: number }).status === 1, "no ledger entry was written");
  });

  it("an unverified probe result carrying an unreadable_unattributed process keeps the entry and refuses a same-key seed (real clear verdict)", async () => {
    const iid = 54;
    const key = `issue-${iid}`;
    const unverified: ProcessQuiescence = {
      state: "unverified", killed: [], detail: "one process could not be read",
      processes: [{ pid: 73, uid: 1000, comm: "sleep", cwd: canonicalOf(key), reason: "unreadable_unattributed" }],
    };
    assert.equal(inventoryClearVerdict(unverified), "not_clear");
    git.setInventoryExclusionProbe(async () => inventoryClearVerdict(unverified));
    const r = await section(key, async (s) => { s.markGitStarted(); await s.stage(async () => 1); s.afterScanQuiescent(true); });
    assert.equal(r.kind === "ran" && r.entryKept, true, "the section kept its entry");
    assert.equal(priv.inventoryExclusion.excluded(bare, keyOf(key)), true, "the unverified probe never clears the key");
    const id = mintAttemptId(1);
    await assert.rejects(
      git.createOrAttachRunnerClone(bare, iid, noProofReseed, "run-e", false, undefined, attemptOpts(id)),
      (e: unknown) => e instanceof InventorySourceExclusionRefusal && e.reason === "excluded" && e.key === keyOf(key));
    assert.equal(fs.existsSync(`${canonicalOf(key)}.attempt-${id}`), false, "the refused seed seeded nothing");
    assert.equal(priv.inventoryExclusion.excluded(bare, keyOf(key)), true, "the refusal left the entry");
  });

  it("a clearable exclusion is cleared by the seed, which then proceeds", async () => {
    const iid = 51;
    await leaveEntry(`issue-${iid}`, "not_clear");
    verdict = "clear";
    const clone = await git.createOrAttachRunnerClone(bare, iid, noProofReseed, "run-b", false, undefined, attemptOpts(mintAttemptId(1)));
    assert.ok(fs.existsSync(path.join(clone.path, ".git")));
    assert.equal(priv.inventoryExclusion.excluded(bare, keyOf(`issue-${iid}`)), false);
  });

  it("the canonical reseed refuses an excluded key before it touches the path", async (t) => {
    const iid = 52;
    const key = `issue-${iid}`;
    await leaveEntry(key, "not_clear");
    verdict = "not_clear";
    const assertClear = t.mock.method(priv, "assertBareClear", async () => {});
    await assert.rejects(
      git.createOrAttachRunnerClone(bare, iid, noProofReseed, "run-c", false, undefined, undefined),
      (e: unknown) => e instanceof InventorySourceExclusionRefusal && e.reason === "excluded");
    assert.equal(assertClear.mock.callCount(), 0);
    assert.equal(fs.existsSync(canonicalOf(key)), false);
  });

  it("a restore entry defers the seed without a probe", async (t) => {
    const iid = 53;
    await leaveEntry(`issue-${iid}`, "not_clear");
    const calls = probeCalls.length;
    t.mock.method(priv, "restoreHolder", () => ({ runId: "run-d" }));
    t.mock.method(git as unknown as { refuseOtherRunRestore(): Promise<void> }, "refuseOtherRunRestore", async () => {});
    await assert.rejects(
      git.createOrAttachRunnerClone(bare, iid, noProofReseed, "run-d", false, undefined, attemptOpts(mintAttemptId(1))),
      (e: unknown) => e instanceof InventorySourceExclusionRefusal && e.reason === "deferred_restore");
    assert.equal(probeCalls.length, calls);
  });
});

describe("worktreeStatus and the content proof", () => {
  it("worktreeStatus passes the timeout through, and omits it by default", async (t) => {
    const seen: Array<number | undefined> = [];
    t.mock.method(git as unknown as { runGitAsRunner(c: string, a: string[], o: { timeoutMs?: number }): Promise<string> }, "runGitAsRunner",
      async (_c: string, _a: string[], o: { timeoutMs?: number }) => { seen.push(o.timeoutMs); return ""; });
    await git.worktreeStatus("/x", { timeoutMs: 1234 });
    await git.worktreeStatus("/x");
    assert.deepEqual(seen, [1234, undefined]);
  });

  it("the content proof starts no git once stopped() is true", async (t) => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "excl-proof-"));
    try {
      const g = (...args: string[]) => execFileSync("git", ["-C", dir, ...args], { env: GIT_ENV, stdio: "pipe" });
      g("init", "-q", "-b", "main");
      for (const [k, v] of [["user.name", "f"], ["user.email", "f@example.com"], ["commit.gpgsign", "false"]]) g("config", k!, v!);
      fs.writeFileSync(path.join(dir, "a.txt"), "a\n");
      g("add", "a.txt");
      g("commit", "-q", "-m", "init");
      const plain = new GitCache(dir, rec.logger);
      // Record every dispatch at the Git runner boundary the proof's pinned calls go through.
      type RunBytes = (cwd: string, args: string[], opts: unknown) => Promise<unknown>;
      const target = plain as unknown as { runGitAsRunnerBytes: RunBytes };
      const original = target.runGitAsRunnerBytes.bind(plain);
      const dispatched = t.mock.method(target, "runGitAsRunnerBytes", (cwd: string, args: string[], opts: unknown) => original(cwd, args, opts));
      assert.equal(await plain.inventorySourceContentProof(dir, { ...INVENTORY_CONTENT_PROOF_BOUNDS }), "clean");
      assert.ok(dispatched.mock.callCount() >= 1, "a clean proof dispatches git through the recorded boundary");
      dispatched.mock.resetCalls();
      let polls = 0;
      const stopped = await plain.inventorySourceContentProof(dir, { ...INVENTORY_CONTENT_PROOF_BOUNDS, stopped: () => { polls++; return true; } });
      assert.equal(stopped, "unreadable");
      assert.ok(polls >= 1);
      assert.equal(dispatched.mock.callCount(), 0, "a stopped proof dispatched no git");
    } finally { fs.rmSync(dir, { recursive: true, force: true }); }
  });
});

describe("a probe that ignores its deadline and settles only after a gate", () => {
  const attemptOpts = (id: string) => ({ attemptId: id, isLive: () => false, beforeSeed: async () => {} });
  /** Time to take and release B from outside: B must be free once the section or clear returned. */
  const bFreeMs = async (): Promise<number> => { const t0 = Date.now(); await git.withBareLock(bare, async () => {}); return Date.now() - t0; };
  const seedRefused = (iid: number): Promise<void> =>
    assert.rejects(git.createOrAttachRunnerClone(bare, iid, noProofReseed, `run-${iid}`, false, undefined, attemptOpts(mintAttemptId(1))),
      (e: unknown) => e instanceof InventorySourceExclusionRefusal && e.reason === "excluded");
  const immediateClear = (): void => { git.setInventoryExclusionProbe(async () => "clear"); };

  it("step 7: past D, B is free, the entry stays, a seed refuses, a late clear removes nothing, and a fresh clear removes it", async () => {
    const iid = 60;
    const key = `issue-${iid}`;
    const k = keyOf(key);
    const gate = deferred<"clear">();
    let calls = 0;
    git.setInventoryExclusionProbe(() => { calls++; return gate.promise; });
    const r = await section(key, async (s) => { s.markGitStarted(); await s.stage(async () => 1); s.afterScanQuiescent(true); }, { deadlineMs: 1_300 });
    assert.equal(r.kind, "ran");
    assert.equal(r.kind === "ran" && r.entryKept, true, "a verdict that never arrived in time keeps the entry");
    assert.equal(calls, 1, "step 7 did probe");
    assert.ok(await bFreeMs() < 500, "B was released at the deadline");
    assert.equal(priv.inventoryExclusion.excluded(bare, k), true);
    await seedRefused(iid);
    assert.equal(calls, 1, "the seed did not probe while the abandoned probe is unsettled");
    gate.resolve("clear");
    await git.inventoryExclusionSettled(bare, k);
    await tick(20);
    assert.equal(priv.inventoryExclusion.excluded(bare, k), true, "a late clear verdict never removes the entry");
    immediateClear();
    assert.equal(await git.withBareLock(bare, () => priv.clearExclusion(bare, branch, canonicalOf(key), () => false, "full", Date.now() + 1_000)), "admit");
    assert.equal(priv.inventoryExclusion.excluded(bare, k), false, "a fresh clear after the gate removes it");
  });

  it("lazy clear inside a section: past its budget the section refuses, B is free, the entry stays, and a fresh clear removes it", async () => {
    const key = "issue-61";
    const k = keyOf(key);
    await leaveEntry(key, "not_clear");
    const gate = deferred<"clear">();
    git.setInventoryExclusionProbe(() => gate.promise);
    const r = await section(key, async () => "never", { deadlineMs: 300 });
    assert.equal(r.kind === "refused" && r.check, "source_settlement_unproven");
    assert.ok(r.kind === "refused" && r.heldMs !== undefined && r.heldMs < 1_500, `the refusal reports heldMs (${r.kind === "refused" ? r.heldMs : "n/a"})`);
    assert.ok(await bFreeMs() < 500, "B free after the refusal");
    assert.equal(priv.inventoryExclusion.excluded(bare, k), true);
    gate.resolve("clear");
    await git.inventoryExclusionSettled(bare, k);
    await tick(20);
    assert.equal(priv.inventoryExclusion.excluded(bare, k), true, "the late verdict removed nothing");
    immediateClear();
    const again = await section(key, async () => "ok");
    assert.equal(again.kind === "ran" && again.value, "ok", "the next section clears lazily and runs");
    assert.equal(priv.inventoryExclusion.excluded(bare, k), false);
  });

  it("full clear at a seed: the budget expires, B is free, the entry stays, the seed refuses, and the seed after the gate clears and seeds", { timeout: 20_000 }, async () => {
    const iid = 62;
    const key = `issue-${iid}`;
    const k = keyOf(key);
    await leaveEntry(key, "not_clear");
    const gate = deferred<"clear">();
    let calls = 0;
    git.setInventoryExclusionProbe(() => { calls++; return gate.promise; });
    await seedRefused(iid);
    assert.equal(calls, 1);
    assert.ok(await bFreeMs() < 500, "B free after the refused seed");
    assert.equal(priv.inventoryExclusion.excluded(bare, k), true);
    await seedRefused(iid);
    assert.equal(calls, 1, "no second probe while the first is unsettled");
    gate.resolve("clear");
    await git.inventoryExclusionSettled(bare, k);
    await tick(20);
    assert.equal(priv.inventoryExclusion.excluded(bare, k), true, "the late verdict removed nothing");
    immediateClear();
    const clone = await git.createOrAttachRunnerClone(bare, iid, noProofReseed, `run-${iid}`, false, undefined, attemptOpts(mintAttemptId(2)));
    assert.ok(fs.existsSync(path.join(clone.path, ".git")));
    assert.equal(priv.inventoryExclusion.excluded(bare, k), false);
  });

  it("restore-class clear: past the deadline it is not_clear, a repeat while pending does not probe, and after the gate it admits but removes nothing", async () => {
    const key = "issue-63";
    const k = keyOf(key);
    priv.inventoryExclusion.addEntry(bare, k, { id: "x", clonePath: "/c", gitStarted: true, pending: new Set() });
    const gate = deferred<"clear">();
    let calls = 0;
    git.setInventoryExclusionProbe(() => { calls++; return gate.promise; });
    const restore = (ms: number) => git.withBareLock(bare, () => priv.clearExclusion(bare, branch, canonicalOf(key), () => false, "restore", Date.now() + ms, new Set()));
    assert.equal(await restore(80), "not_clear");
    assert.ok(await bFreeMs() < 500, "B free after the deadline");
    assert.equal(await restore(500), "not_clear");
    assert.equal(calls, 1, "no second probe while the first is unsettled");
    gate.resolve("clear");
    await git.inventoryExclusionSettled(bare, k);
    await tick(20);
    assert.equal(priv.inventoryExclusion.excluded(bare, k), true, "the late verdict removed nothing");
    immediateClear();
    assert.equal(await restore(500), "admit");
    assert.equal(priv.inventoryExclusion.excluded(bare, k), true, "a restore-class admission never removes the entry");
    assert.equal(await git.withBareLock(bare, () => priv.clearExclusion(bare, branch, canonicalOf(key), () => false, "full", Date.now() + 500)), "admit");
    assert.equal(priv.inventoryExclusion.excluded(bare, k), false, "a fresh full clear removes it");
  });
});

describe("cross-kind reclaim admission (5g): the lookup is keyed by each target's own path", () => {
  const ownerPath = (n: number): string => canonicalOf(`issue-${n}`);
  const claimantPath = (n: number): string => canonicalOf(`agent-issue-${n}`);
  const entry = (id: string) => ({ id, clonePath: "/c", gitStarted: true, pending: new Set<Promise<unknown>>() });

  it("an entry under the owner's key refuses a claimant whose own key differs, for the canonical and an attempt path", async () => {
    verdict = "not_clear";
    priv.inventoryExclusion.addEntry(bare, keyOf("issue-70"), entry("owner"));
    for (const targetPath of [ownerPath(70), `${ownerPath(70)}.attempt-${mintAttemptId(1)}`]) {
      let ran = false;
      await assert.rejects(
        git.withReclaimAdmission(bare, [{ path: targetPath, branch: "agent/issue-70" }], () => false, async () => { ran = true; }),
        (e: unknown) => e instanceof InventorySourceExclusionRefusal && e.reason === "excluded" && e.key === keyOf("issue-70"));
      assert.equal(ran, false);
    }
    assert.notEqual(keyOf("issue-70"), keyOf("agent-issue-70"), "the claimant's own key is a different key");
    assert.equal(priv.inventoryExclusion.admissionAge(bare, keyOf("issue-70")), undefined);
  });

  it("the refusal comes from ANY target, not only the first", async () => {
    verdict = "not_clear";
    priv.inventoryExclusion.addEntry(bare, keyOf("issue-71"), entry("owner"));
    await assert.rejects(
      git.withReclaimAdmission(bare, [{ path: claimantPath(71), branch: "agent/issue-71" }, { path: ownerPath(71), branch: "agent/issue-71" }], () => false, async () => {}),
      (e: unknown) => e instanceof InventorySourceExclusionRefusal && e.key === keyOf("issue-71"));
  });

  it("an entry only under the claimant's key does not refuse a reclaim of the owner's paths", async () => {
    verdict = "not_clear";
    priv.inventoryExclusion.addEntry(bare, keyOf("agent-issue-72"), entry("claimant"));
    let ran = false;
    await git.withReclaimAdmission(bare, [{ path: ownerPath(72), branch: "agent/issue-72" }], () => false, async () => { ran = true; });
    assert.equal(ran, true);
    assert.equal(priv.inventoryExclusion.excluded(bare, keyOf("agent-issue-72")), true, "the claimant's own entry is untouched");
  });
});

describe("withReclaimAdmission lifecycle", () => {
  const target = (n: number) => ({ path: canonicalOf(`issue-${n}`), branch });

  it("removes the admission when the lock call rejects after the locked registration step ran", async (t) => {
    const k = keyOf("issue-80");
    const real = git.withBareLock.bind(git) as <T>(b: string, fn: () => Promise<T>) => Promise<T>;
    t.mock.method(git, "withBareLock", async <T,>(b: string, fn: () => Promise<T>): Promise<T> => {
      await real(b, fn);
      assert.notEqual(priv.inventoryExclusion.admissionAge(bare, k), undefined, "the registration step had installed the admission");
      throw new Error("lock call rejected after the step");
    });
    let ran = false;
    await assert.rejects(git.withReclaimAdmission(bare, [target(80)], () => false, async () => { ran = true; }), /lock call rejected after the step/);
    assert.equal(ran, false);
    assert.equal(priv.inventoryExclusion.admissionAge(bare, k), undefined, "no admission leaked");
  });

  it("removes the admission when fn throws synchronously", async () => {
    const k = keyOf("issue-81");
    await assert.rejects(git.withReclaimAdmission(bare, [target(81)], () => false, (() => { throw new Error("sync boom"); }) as () => Promise<void>), /sync boom/);
    assert.equal(priv.inventoryExclusion.admissionAge(bare, k), undefined);
  });

  it("fn runs without holding B: it can take the bare lock itself, and holdsLock is false", async () => {
    const k = keyOf("issue-82");
    let held: boolean | undefined;
    let took = false;
    const inner = git.withReclaimAdmission(bare, [target(82)], () => false, async () => {
      held = priv.holdsLock(bare);
      assert.notEqual(priv.inventoryExclusion.admissionAge(bare, k), undefined, "the admission stands inside fn");
      await git.withBareLock(bare, async () => { took = true; });
    });
    await Promise.race([inner, tick(3_000).then(() => { throw new Error("deadlock: fn could not take B"); })]);
    assert.equal(held, false);
    assert.equal(took, true);
  });

  it("the acquisition timeout is INVENTORY_LOCK_ACQUIRE_MS, and a section refuses when B cannot be taken in time", async (t) => {
    const seen: number[] = [];
    const realTimeout = AbortSignal.timeout.bind(AbortSignal);
    // Observe the configured value, then substitute a short one so the wait ends at once.
    t.mock.method(AbortSignal, "timeout", (ms: number) => { seen.push(ms); return realTimeout(20); });
    const release = deferred();
    const held = git.withBareLock(bare, () => release.promise);
    let called = false;
    const r = await section("issue-83", async () => { called = true; });
    assert.equal(r.kind === "refused" && r.check, "source_lock_unavailable");
    assert.match(r.kind === "refused" ? r.detail : "", /not acquired in time/);
    assert.equal(called, false);
    assert.deepEqual(seen, [INVENTORY_LOCK_ACQUIRE_MS], "the section waited with the configured acquire timeout");
    release.resolve();
    await held;
  });

  it("the restart-boundary warning is logged once per GitCache across two sections, and a fresh GitCache starts with an empty registry", async (t) => {
    const real = fs.readlinkSync;
    t.mock.method(fs, "readlinkSync", ((p: fs.PathLike, ...rest: unknown[]) =>
      String(p) === "/proc/1/exe" ? "/usr/lib/systemd/systemd" : (real as (...a: unknown[]) => unknown)(p, ...rest)) as typeof fs.readlinkSync);
    const warnings = (lines: typeof rec.lines): number => lines.filter((l) => String((l as { msg?: string }).msg).startsWith("inventory exclusion: restart boundary not namespace-scoped")).length;
    await leaveEntry("issue-84", "not_clear");
    await section("issue-85", async () => "second");
    assert.equal(warnings(rec.lines), 1, "two sections, one warning");
    assert.equal(priv.inventoryExclusion.excluded(bare, keyOf("issue-84")), true);

    const fresh = recordingLogger();
    const other = new GitCache(fx.dataDir, fresh.logger, undefined, testGitCacheOptions({ gitleaksBin: defaultGitleaksShim() }));
    const otherPriv = other as unknown as Priv;
    assert.equal(otherPriv.inventoryExclusion.excluded(bare, keyOf("issue-84")), false, "a fresh cache remembers no exclusion");
    other.setInventoryExclusionProbe(async () => "clear");
    const r = await other.withInventorySourceLock(bare, { clonePath: canonicalOf("issue-84"), branch, isLive: () => false }, async () => "fresh");
    assert.equal(r.kind === "ran" && r.value, "fresh", "the key is unexcluded for the fresh cache, so its section runs");
    assert.equal(warnings(fresh.lines), 1, "the fresh cache warns for itself");
    assert.equal(warnings(rec.lines), 1, "and the first cache's count did not move");
  });
});

describe("detectNamespaceRestartBoundary", () => {
  function procRoot(t: { after(fn: () => void): void }, spec: { exe1?: string; children?: string; kids?: Record<string, { exe: string; ppid: number }> }): string {
    const root = fs.mkdtempSync(path.join(os.tmpdir(), "fake-proc-"));
    t.after(() => fs.rmSync(root, { recursive: true, force: true }));
    fs.mkdirSync(path.join(root, "1", "task", "1"), { recursive: true });
    if (spec.exe1 !== undefined) fs.symlinkSync(spec.exe1, path.join(root, "1", "exe"));
    if (spec.children !== undefined) fs.writeFileSync(path.join(root, "1", "task", "1", "children"), spec.children);
    for (const [pid, k] of Object.entries(spec.kids ?? {})) {
      fs.mkdirSync(path.join(root, pid));
      fs.symlinkSync(k.exe, path.join(root, pid, "exe"));
      fs.writeFileSync(path.join(root, pid, "stat"), `${pid} (a b) S ${k.ppid} 0 0\n`);
    }
    return root;
  }

  it("is scoped when PID 1 is tini", (t) => {
    assert.equal(detectNamespaceRestartBoundary(procRoot(t, { exe1: "/sbin/tini" })).scoped, true);
  });
  it("is scoped for docker-init over a single tini child, via the children file", (t) => {
    const root = procRoot(t, { exe1: "/usr/bin/docker-init", children: "7 \n", kids: { "7": { exe: "/sbin/tini", ppid: 1 } } });
    assert.equal(detectNamespaceRestartBoundary(root).scoped, true);
  });
  it("is scoped for docker-init over a single tini child, found by scanning stat", (t) => {
    const root = procRoot(t, { exe1: "/sbin/docker-init", kids: { "7": { exe: "/sbin/tini", ppid: 1 }, "9": { exe: "/bin/sh", ppid: 7 } } });
    assert.equal(detectNamespaceRestartBoundary(root).scoped, true);
  });
  it("is not scoped for docker-init with two children or a non-tini child", (t) => {
    const two = procRoot(t, { exe1: "/sbin/docker-init", children: "7 8", kids: { "7": { exe: "/sbin/tini", ppid: 1 } } });
    assert.equal(detectNamespaceRestartBoundary(two).scoped, false);
    const other = procRoot(t, { exe1: "/sbin/docker-init", children: "7", kids: { "7": { exe: "/bin/sh", ppid: 1 } } });
    assert.equal(detectNamespaceRestartBoundary(other).scoped, false);
  });
  it("is not scoped under systemd or when unreadable", (t) => {
    assert.equal(detectNamespaceRestartBoundary(procRoot(t, { exe1: "/usr/lib/systemd/systemd" })).scoped, false);
    assert.equal(detectNamespaceRestartBoundary(procRoot(t, {})).scoped, false);
    assert.equal(detectNamespaceRestartBoundary(path.join(os.tmpdir(), "no-such-proc-root")).scoped, false);
  });

  /** A real fake root with docker-init as PID 1 and one tini child (no children file, so the stat census runs),
   *  plus a second listed pid whose stat read the injected fs fails with `code`. */
  function censusRoot(t: { after(fn: () => void): void }): string {
    return procRoot(t, { exe1: "/sbin/docker-init", kids: { "7": { exe: "/sbin/tini", ppid: 1 }, "9": { exe: "/bin/sh", ppid: 7 } } });
  }
  const failingStat = (root: string, pid: string, code: string): typeof fs => ({
    ...fs,
    readFileSync: ((p: fs.PathOrFileDescriptor, ...rest: unknown[]) => {
      if (String(p) === path.join(root, pid, "stat")) throw Object.assign(new Error(`${code}: injected`), { code });
      return (fs.readFileSync as (...a: unknown[]) => unknown)(p, ...rest);
    }) as typeof fs.readFileSync,
  } as typeof fs);

  it("a complete census with exactly one tini child is scoped", (t) => {
    assert.equal(detectNamespaceRestartBoundary(censusRoot(t)).scoped, true);
  });
  it("a pid that vanished (ENOENT) during the census is skipped", (t) => {
    const root = censusRoot(t);
    assert.equal(detectNamespaceRestartBoundary(root, failingStat(root, "9", "ENOENT")).scoped, true);
  });
  it("a pid that vanished (ESRCH) during the census is skipped", (t) => {
    const root = censusRoot(t);
    assert.equal(detectNamespaceRestartBoundary(root, failingStat(root, "9", "ESRCH")).scoped, true);
  });
  it("an unreadable stat (EACCES) leaves the census incomplete, so it is not scoped", (t) => {
    const root = censusRoot(t);
    const r = detectNamespaceRestartBoundary(root, failingStat(root, "9", "EACCES"));
    assert.equal(r.scoped, false);
    assert.match(r.detail, /unreadable: EACCES/);
  });
});

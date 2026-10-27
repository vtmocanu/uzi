import { describe, it, type TestContext } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { randomBytes, randomUUID } from "node:crypto";
import fs from "node:fs";
import fsp from "node:fs/promises";
import path from "node:path";
import { acquireWorkerLifetime } from "../src/authority-lock.js";
import { AuthorityRuntime } from "../src/authority-runtime.js";
import { openAuthorityStore, type AuthorityFsOps, type SlotSnapshot } from "../src/completion-authority.js";
import { sourceCoreDigest, sourceFenceOf, verifyCertifiedSource, verifyPublishedSource, type CompletionSourceV2 } from "../src/completion-source.js";
import { ExecutorLifetime, type ExecutorTerminalSeal } from "../src/codex/executor-lifetime.js";
import type { Executor } from "../src/executor.js";
import { legacyCompletionCore, type CompletionSource, type PublicationFsOps, type RecoveryCoordinator } from "../src/recovery.js";
import { mintAttemptId } from "../src/run-quiescence.js";
import { testLockOptions } from "./authority-test-support.js";
import { nullLogger } from "./helpers.js";
import { client, fakeGitlab, fx, git, gitlabClaim, installHarness, runnerWith } from "./runner-harness.js";

installHarness();

// issue #3404 M2 U4 - durable publication of a flight's completing source at the end of its source use.
// The REAL RunRunner endFlightSourceUse seam over the real authority store (real kernel flock), the real
// recovery coordinator and real git. Only the executor is a stand-in, and for Codex it wraps the REAL
// ExecutorLifetime seal, so the certificate is minted from an actual terminal seal.

const TOKEN = "unit-join-" + randomBytes(12).toString("hex");
const GENERATION = 29;
const BRANCH = "agent/issue-3404";
const ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };

interface Counts { authority: number; records: number; fence: number }

interface PrivateRunner {
  recovery: RecoveryCoordinator;
  buildFlight(...args: unknown[]): Record<string, unknown>;
  bindCompletionSource(flight: Record<string, unknown>): Promise<void>;
  endFlightSourceUse(flight: Record<string, unknown>, log: unknown): Promise<void>;
}

interface Rig {
  rt: AuthorityRuntime;
  p: PrivateRunner;
  flight: Record<string, unknown>;
  bound: CompletionSource;
  bare: string;
  clone: string;
  runId: string;
  captureIds: string[];
  executor: Executor;
  warnings: string[];
  events: string[];
  counts: Counts;
  /** Injected failure points. Each fires once unless `always` is set. */
  fail: { authority?: (kind: string, n: number) => boolean; records?: (kind: string, n: number) => boolean; always?: boolean };
  end(): Promise<void>;
}

function sealingExecutor(events: string[], unproven = false): Executor {
  const lifetime = new ExecutorLifetime();
  return {
    run: async ctx => ({ branch: ctx.branch }),
    executorIncarnation: lifetime.executorIncarnation,
    sealLifetime: (sourceUse: object) => {
      events.push("seal");
      return lifetime.seal(sourceUse, async () => {}, () => !unproven);
    },
    acceptsLifetimeSeal: (seal: unknown, sourceUse: object): seal is ExecutorTerminalSeal => lifetime.accepts(seal, sourceUse),
  } as Executor;
}

async function rig(t: TestContext, options: { codex: boolean; cohort?: number; unproven?: boolean } ): Promise<Rig> {
  const dataDir = fs.realpathSync(fx.dataDir);
  const lifetime = await acquireWorkerLifetime(dataDir, testLockOptions());
  const counts: Counts = { authority: 0, records: 0, fence: 0 };
  const events: string[] = [];
  const fail: Rig["fail"] = {};
  let authorityCalls = 0, recordCalls = 0;
  const trip = (which: "authority" | "records", kind: string, n: number): void => {
    const rule = fail[which];
    if (rule?.(kind, n) === true) {
      if (fail.always !== true) fail[which] = undefined;
      throw new Error(`injected ${which} ${kind} failure`);
    }
  };
  const authorityOps: AuthorityFsOps = {
    syncFile: async handle => { counts.authority++; trip("authority", "syncFile", authorityCalls++); await handle.sync(); },
    syncDir: async handle => { counts.authority++; trip("authority", "syncDir", authorityCalls++); await handle.sync(); },
    rename: async (from, to) => { trip("authority", "rename", authorityCalls++); await fsp.rename(from, to); },
  };
  const rt = new AuthorityRuntime({ lifetime, workerToken: TOKEN, ops: authorityOps, timeoutMs: 5000 });

  const executor: Executor = options.codex ? sealingExecutor(events, options.unproven === true) : { run: async ctx => ({ branch: ctx.branch }) };
  const claim = gitlabClaim(3404, { claim_generation: GENERATION, inventory_guarded: true });
  const runner = runnerWith(() => ({ executor }), fakeGitlab().gitlab, "u4-publication-join", undefined, { authority: rt });
  const p = runner as unknown as PrivateRunner;
  client.hasFeature = () => true;
  client.listRecoveryHolds = async () => ({ run_id: claim.run_id, holds: [{
    hold_id: "u4-open-hold", generation: GENERATION, inventory_guarded: true, has_available_capture: false,
  }] });
  const bare = await git.ensureClone(fx.originPath);
  fs.chmodSync(path.dirname(bare), 0o700);
  fs.chmodSync(bare, 0o700);
  fs.chmodSync(path.join(bare, "config"), 0o600);
  const attemptId = mintAttemptId(GENERATION);
  const cloneInfo = await git.runnerCloneForBranch(bare, BRANCH, "u4-source", {
    isLive: () => false, beforeFree: async () => { throw new Error("unexpected incumbent"); },
  }, claim.run_id, false, undefined, {
    attemptId, isLive: () => false, beforeSeed: async paths => assert.deepEqual(paths, []),
  });
  await git.markRecoveryCapture(bare, cloneInfo.path, BRANCH, claim.run_id, attemptId);
  const head = await git.worktreeHead(cloneInfo.path);
  assert.ok(head);
  const first = await p.recovery.pin({ runId: claim.run_id, generation: GENERATION, kind: "issue", branch: BRANCH, sourceSha: head, inventoryGuarded: true });
  assert.ok(first);
  const captureIds = [first.captureId];
  for (let i = 1; i < (options.cohort ?? 1); i++) {
    const extra = { ...first, captureId: randomUUID() };
    await (p.recovery as unknown as { writeRecordUnlocked(r: unknown): Promise<void> }).writeRecordUnlocked(extra);
    captureIds.push(extra.captureId);
  }
  captureIds.sort();

  const flight = p.buildFlight(claim, claim.run_id, executor, undefined, "", []);
  Object.assign(flight, { barePath: bare, worktreePath: cloneInfo.path, branch: BRANCH, runnerClone: cloneInfo });
  await p.bindCompletionSource(flight);
  const bound = flight.completionSourceBound as CompletionSource | undefined;
  assert.ok(bound, "the real binder bound the completing source");
  const sink = new Set<string>();
  flight.sourceUsePaths = sink;
  flight.sourceLease = await rt.recordAdmissions(sink, () => rt.bindSource(cloneInfo.path, executor.executorIncarnation));
  flight.sourceTailsJoined = true;

  // Durability barriers of the record writes: counted, and a failure can be injected at any of them.
  const real: PublicationFsOps = { syncFile: h => h.sync(), syncDir: h => h.sync(), rename: (a, b) => fsp.rename(a, b) };
  (p.recovery as unknown as { publicationOps: PublicationFsOps }).publicationOps = {
    syncFile: async h => { counts.records++; trip("records", "syncFile", recordCalls++); await real.syncFile(h); },
    syncDir: async h => { counts.records++; trip("records", "syncDir", recordCalls++); await real.syncDir(h); },
    rename: async (a, b) => { trip("records", "rename", recordCalls++); await real.rename(a, b); },
  };
  const original = git.installCompletionSourceFence.bind(git);
  t.mock.method(git, "installCompletionSourceFence", (...args: Parameters<typeof git.installCompletionSourceFence>) =>
    original(args[0], args[1], args[2], args[3], { ...args[4], sync: async (h: { sync(): Promise<void> }) => { counts.fence++; await h.sync(); } }));

  const warnings: string[] = [];
  const log = Object.assign(nullLogger(), { warn: (message: string, fields?: unknown) => { warnings.push(`${message} ${JSON.stringify(fields ?? {})}`); } });
  return {
    rt, p, flight, bound, bare, clone: cloneInfo.path, runId: claim.run_id, captureIds, executor, warnings, events, counts, fail,
    end: () => p.endFlightSourceUse(flight, log),
  };
}

const recordDir = (r: Rig): string => path.join(git.recoveryRoot, r.runId);
const readRecords = (r: Rig): Array<Record<string, unknown>> =>
  r.captureIds.map(id => JSON.parse(fs.readFileSync(path.join(recordDir(r), `${id}.json`), "utf8")) as Record<string, unknown>);
const snapshotFiles = (r: Rig): Record<string, string> => Object.fromEntries(
  fs.readdirSync(recordDir(r)).sort().map(name => [name, fs.readFileSync(path.join(recordDir(r), name), "utf8")]));
const fenceOf = (r: Rig): unknown => {
  try {
    const raw = execFileSync("git", ["-C", r.bare, "config", "--get", `uzi-recovery.${BRANCH}.clone`], { encoding: "utf8", env: ENV }).trim();
    return (JSON.parse(raw) as { completionSourceFence?: unknown }).completionSourceFence;
  } catch { return undefined; }
};
async function slotOf(r: Rig): Promise<SlotSnapshot | null> {
  return r.rt.withTransaction(async tx => {
    const store = await openAuthorityStore(tx, { workerToken: TOKEN });
    return store.readSlot(tx, await store.slotKey(tx, r.clone));
  });
}
async function eligibleRecords(r: Rig): Promise<number> {
  return r.rt.withTransaction(async tx => {
    const store = await openAuthorityStore(tx, { workerToken: TOKEN });
    let eligible = 0;
    for (const record of readRecords(r)) {
      if (record.completionSource && (record.completionSource as { version?: number }).version === 2) {
        try { await verifyPublishedSource(tx, store, record.completionSource); eligible++; } catch { /* not published */ }
      }
    }
    return eligible;
  });
}
const isV2 = (record: Record<string, unknown>): boolean => (record.completionSource as { version?: number } | undefined)?.version === 2;

describe("durable publication at the end of source use", () => {
  it("Claude: publishes claude_ready with the fence and N identical certified records", async t => {
    const r = await rig(t, { codex: false, cohort: 3 });
    await r.end();
    const slot = await slotOf(r);
    assert.equal(slot?.state, "claude_ready");
    assert.deepEqual(slot?.publication?.cohort.captureIds, r.captureIds, "the cohort manifest is the exact sorted membership");
    assert.equal(slot?.publication?.harness, "claude");
    const records = readRecords(r);
    assert.equal(records.length, 3);
    const sources = records.map(x => x.completionSource as CompletionSourceV2);
    for (const s of sources) assert.deepEqual(s, sources[0], "every member carries the identical certified source");
    const digest = sourceCoreDigest(sources[0]!);
    assert.equal(slot?.publication?.sourceCoreDigest, digest);
    assert.deepEqual(fenceOf(r), sourceFenceOf(sources[0]!));
    assert.equal(sources[0]!.harness, "claude");
    assert.equal(sources[0]!.executorIncarnation, undefined);
    assert.equal(await eligibleRecords(r), 3, "the certificate verifies against the published slot");
    // The coordinator decodes the V2 records strictly and keeps the certificate verbatim.
    const decoded = await r.p.recovery.inspect(r.runId);
    assert.equal(decoded.length, 3);
    for (const d of decoded) assert.equal((d.completionSource as CompletionSourceV2).certificate, sources[0]!.certificate);
    assert.deepEqual(legacyCompletionCore(sources[0]!), r.bound, "the legacy projection is exactly the bound core");
    assert.equal(r.events.length, 0, "no executor seal for Claude");
    assert.equal(r.rt.leaseFor(r.clone), undefined, "the published lease is released");
  });

  it("Codex: seals first, then publishes sealed carrying the seal-derived executor identity", async t => {
    const r = await rig(t, { codex: true, cohort: 2 });
    await r.end();
    assert.deepEqual(r.events, ["seal"]);
    const slot = await slotOf(r);
    assert.equal(slot?.state, "sealed");
    assert.equal(slot?.publication?.harness, "codex");
    const source = readRecords(r)[0]!.completionSource as CompletionSourceV2;
    assert.equal(source.harness, "codex");
    assert.equal(source.executorIncarnation, r.executor.executorIncarnation);
    assert.deepEqual(readRecords(r)[1]!.completionSource, source);
    assert.deepEqual(fenceOf(r), sourceFenceOf(source));
    assert.equal(await eligibleRecords(r), 2);
  });

  it("an unproven Codex seal publishes nothing: no fence, records byte-identical, slot ineligible", async t => {
    const r = await rig(t, { codex: true, cohort: 2, unproven: true });
    const before = snapshotFiles(r);
    await r.end();
    assert.equal((await slotOf(r))?.state, "ineligible");
    assert.equal(fenceOf(r), undefined);
    assert.deepEqual(snapshotFiles(r), before);
    assert.match(r.warnings.join("\n"), /seal is unproven/);
  });

  it("tails that were not joined publish nothing and leave the slot active", async t => {
    const r = await rig(t, { codex: false });
    r.flight.sourceTailsJoined = false;
    const before = snapshotFiles(r);
    await r.end();
    assert.equal((await slotOf(r))?.state, "active");
    assert.equal(fenceOf(r), undefined);
    assert.deepEqual(snapshotFiles(r), before);
  });

  it("a flight that never durably bound a source is only ended, as before", async t => {
    const r = await rig(t, { codex: false });
    r.flight.completionSourceBound = undefined;
    await r.end();
    assert.equal((await slotOf(r))?.state, "ineligible");
    assert.equal(fenceOf(r), undefined);
    assert.equal(readRecords(r).some(isV2), false);
  });

  it("a Claude classification cannot be published for a Codex-initial flight", async t => {
    const r = await rig(t, { codex: false });
    (r.flight as { initiallyCodex: boolean }).initiallyCodex = true;
    await r.end();
    assert.equal((await slotOf(r))?.state, "ineligible");
    assert.equal(fenceOf(r), undefined);
  });

  for (const change of ["head", "roots"] as const) {
    it(`a changed ${change} since binding is ineligible: no fence, no record change`, async t => {
      const r = await rig(t, { codex: false, cohort: 2 });
      if (change === "head") {
        execFileSync("git", ["-C", r.clone, "-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false",
          "commit", "--allow-empty", "-m", "advance"], { env: ENV, stdio: "pipe" });
      } else {
        t.mock.method(git, "enumerateOwedCandidates", async () => [{ sha: "c".repeat(40), contexts: [r.bound.context] }]);
      }
      const before = snapshotFiles(r);
      await r.end();
      assert.equal((await slotOf(r))?.state, "ineligible");
      assert.equal(fenceOf(r), undefined);
      assert.deepEqual(snapshotFiles(r), before);
      assert.match(r.warnings.join("\n"), new RegExp(`${change} changed`, "i"));
      assert.doesNotMatch(r.warnings.join("\n"), /left active/, "the failed publication already ended the slot");
    });
  }

  it("a cohort record that no longer carries the bound source refuses the whole publication", async t => {
    const r = await rig(t, { codex: false, cohort: 2 });
    const internals = r.p.recovery as unknown as { writeRecordUnlocked(rec: unknown): Promise<void> };
    const [record] = await r.p.recovery.inspect(r.runId);
    await internals.writeRecordUnlocked({ ...record, completionSource: { ...r.bound, expectedHead: "d".repeat(40) } });
    const before = snapshotFiles(r);
    await r.end();
    assert.equal((await slotOf(r))?.state, "ineligible");
    assert.deepEqual(snapshotFiles(r), before);
    assert.equal(fenceOf(r), undefined);
  });

  for (const stray of ["junk.txt", "stray.json", `${randomUUID()}.json.${randomUUID()}.tmp`, `${randomUUID()}.bundle`, "subdir"]) {
    it(`an unattributable cohort-directory entry (${stray.slice(0, 9)}) refuses publication`, async t => {
      const r = await rig(t, { codex: false, cohort: 2 });
      const target = path.join(recordDir(r), stray);
      if (stray === "subdir") fs.mkdirSync(target); else fs.writeFileSync(target, "{ not a record");
      const before = fs.readdirSync(recordDir(r)).filter(n => n !== stray).sort().map(n => fs.readFileSync(path.join(recordDir(r), n), "utf8"));
      await r.end();
      assert.equal((await slotOf(r))?.state, "ineligible");
      assert.equal(fenceOf(r), undefined);
      assert.deepEqual(fs.readdirSync(recordDir(r)).filter(n => n !== stray).sort().map(n => fs.readFileSync(path.join(recordDir(r), n), "utf8")), before);
    });
  }

  it("a record of the run that fails authentication refuses publication", async t => {
    const r = await rig(t, { codex: false, cohort: 2 });
    const victim = path.join(recordDir(r), `${r.captureIds[1]}.json`);
    const tampered = JSON.parse(fs.readFileSync(victim, "utf8")) as Record<string, unknown>;
    tampered.sourceSha = "e".repeat(40);
    fs.writeFileSync(victim, JSON.stringify(tampered));
    await r.end();
    assert.equal((await slotOf(r))?.state, "ineligible");
    assert.equal(fenceOf(r), undefined);
  });

  it("a stale publisher (its epoch was replaced) refuses and writes nothing", async t => {
    const r = await rig(t, { codex: false, cohort: 2 });
    const before = snapshotFiles(r);
    const stale = r.flight.sourceLease;
    // Another admission of the same clone mints a fresh epoch; the flight still holds the old lease.
    await r.rt.withTransaction(() => r.rt.hooks.admitSourceUse(r.clone, "seed-canonical"));
    const fresh = (await slotOf(r))!.epoch;
    r.flight.sourceLease = stale;
    await r.end();
    assert.equal(fenceOf(r), undefined);
    assert.deepEqual(snapshotFiles(r), before);
    const slot = await slotOf(r);
    assert.notEqual(slot?.state, "claude_ready");
    assert.notEqual(slot?.state, "sealed");
    assert.equal(slot?.publication ?? null, null);
    assert.ok(fresh);
  });

  it("an installed fence for a different source refuses and rewrites no record", async t => {
    const r = await rig(t, { codex: false, cohort: 2 });
    const original = git.installCompletionSourceFence.bind(git);
    // A different, already-installed fence on the completing journal: the install must refuse it.
    await original(r.bare, BRANCH, { runId: r.runId, clonePath: r.clone, attemptId: r.bound.attemptId! },
      { version: 1, sourceUseEpoch: randomUUID(), sourceCoreDigest: "a".repeat(64) });
    const before = snapshotFiles(r);
    await r.end();
    assert.equal((await slotOf(r))?.state, "ineligible");
    assert.deepEqual(snapshotFiles(r), before);
  });
});

describe("a failure at each publication step leaves nothing eligible", () => {
  // Crash-point table (live failure; the slot is demoted to ineligible, and a process death instead would
  // leave it active). "V2" counts records already rewritten with the certified source.
  //   fence write refused              fence none      V2 0   slot ineligible
  //   fence installed, then failure    fence present   V2 0   slot ineligible
  //   record k file sync / rename      fence present   V2 k   slot ineligible (temp removed)
  //   run directory sync               fence present   V2 N   slot ineligible
  //   slot file sync                   fence present   V2 N   slot ineligible
  //   slot directory sync (renamed)    fence present   V2 N   slot demoted from sealed/claude_ready to ineligible
  //   slot write AND demotion fail     fence present   V2 N   slot active (never published)
  // In every row the published-source check fails for every record: certified records confer nothing alone.
  const N = 2;

  async function expectNothingEligible(r: Rig, state: string, v2: number, fence: boolean): Promise<void> {
    assert.equal(await eligibleRecords(r), 0, "no record verifies as published");
    assert.equal((await slotOf(r))?.state, state);
    assert.equal(readRecords(r).filter(isV2).length, v2);
    assert.equal(fenceOf(r) !== undefined, fence);
    assert.equal(fs.readdirSync(recordDir(r)).some(n => n.endsWith(".tmp")), false, "no temp file is left behind");
  }

  it("fence write refused", async t => {
    const r = await rig(t, { codex: false, cohort: N });
    t.mock.method(git, "installCompletionSourceFence", async () => { throw new Error("injected fence failure"); });
    await r.end();
    await expectNothingEligible(r, "ineligible", 0, false);
  });

  it("fence installed, then the install reports failure", async t => {
    const r = await rig(t, { codex: false, cohort: N });
    const original = git.installCompletionSourceFence.bind(git);
    t.mock.method(git, "installCompletionSourceFence", async (...args: Parameters<typeof git.installCompletionSourceFence>) => {
      await original(...args);
      throw new Error("injected failure after the fence");
    });
    await r.end();
    await expectNothingEligible(r, "ineligible", 0, true);
  });

  for (const kind of ["syncFile", "rename"] as const) {
    for (const k of [0, 1]) {
      it(`record ${k} ${kind} failure`, async t => {
        const r = await rig(t, { codex: false, cohort: N });
        // Calls per record: syncFile then rename.
        r.fail.records = (what, n) => what === kind && n === (kind === "syncFile" ? 2 * k : 2 * k + 1);
        await r.end();
        await expectNothingEligible(r, "ineligible", k, true);
      });
    }
  }

  it("run directory sync failure", async t => {
    const r = await rig(t, { codex: false, cohort: N });
    r.fail.records = kind => kind === "syncDir";
    await r.end();
    await expectNothingEligible(r, "ineligible", N, true);
  });

  it("slot file sync failure", async t => {
    const r = await rig(t, { codex: false, cohort: N });
    arm(r, kind => kind === "syncFile");
    await r.end();
    await expectNothingEligible(r, "ineligible", N, true);
  });

  it("slot directory sync failure after the rename", async t => {
    const r = await rig(t, { codex: false, cohort: N });
    arm(r, kind => kind === "syncDir");
    await r.end();
    await expectNothingEligible(r, "ineligible", N, true);
  });

  it("slot write and demotion both fail: the slot stays active, never published", async t => {
    const r = await rig(t, { codex: false, cohort: N });
    r.fail.always = true;
    arm(r, kind => kind === "syncFile");
    await r.end();
    r.fail.authority = undefined;
    await expectNothingEligible(r, "active", N, true);
  });

  /** Fail the authority store only once the record directory is durable, i.e. at the slot transition. */
  function arm(r: Rig, rule: (kind: string, n: number) => boolean): void {
    const ops = (r.p.recovery as unknown as { publicationOps: PublicationFsOps }).publicationOps;
    const syncDir = ops.syncDir.bind(ops);
    ops.syncDir = async handle => { await syncDir(handle); r.fail.authority = rule; };
  }
});

describe("publication lock order and barrier totals", () => {
  it("A is taken first, then J, then B beneath J; the seal runs before A and outside every lock", async t => {
    const r = await rig(t, { codex: true, cohort: 2 });
    const events = r.events;
    const withTransaction = r.rt.withTransaction.bind(r.rt);
    t.mock.method(r.rt, "withTransaction", async (fn: Parameters<typeof withTransaction>[0], wait?: Parameters<typeof withTransaction>[1]) => {
      const outer = r.rt.active() === undefined;
      if (outer) events.push("A:enter");
      try { return await withTransaction(fn, wait); } finally { if (outer) events.push("A:exit"); }
    });
    const recovery = r.p.recovery as unknown as { withJournalLock<T>(runId: string, fn: () => Promise<T>, signal?: AbortSignal): Promise<T> };
    const journal = recovery.withJournalLock.bind(recovery);
    let heldJ = 0;
    const journalHeld = (): boolean => heldJ > 0;
    t.mock.method(recovery, "withJournalLock", async (runId: string, fn: () => Promise<unknown>, signal?: AbortSignal) => {
      events.push("J:enter");
      return journal(runId, async () => { heldJ++; try { return await fn(); } finally { heldJ--; events.push("J:exit"); } }, signal);
    });
    const bareLock = (git as unknown as { withLock<T>(bare: string, fn: () => Promise<T>, signal?: AbortSignal): Promise<T> }).withLock.bind(git);
    t.mock.method(git, "withLock" as never, (async (...args: [string, () => Promise<unknown>, AbortSignal?]) => {
      events.push(journalHeld() ? "B:enter:inJ" : "B:enter");
      try { return await bareLock(...args); } finally { events.push("B:exit"); }
    }) as never);
    events.length = 0;
    await r.end();
    assert.equal((await slotOf(r))?.state, "sealed");
    const first = (e: string): number => events.indexOf(e);
    assert.ok(first("seal") >= 0 && first("seal") < first("A:enter"), "the seal precedes A");
    assert.ok(first("A:enter") < first("J:enter"), `A before J: ${events.join(",")}`);
    assert.ok(first("J:enter") < first("B:enter:inJ"), `the fence takes B beneath J: ${events.join(",")}`);
    // Replay: J is only entered inside A, B only inside A, and A is never acquired beneath J or B.
    let a = 0, j = 0, b = 0;
    for (const event of events) {
      if (event === "A:enter") { assert.equal(j + b, 0, "A is never acquired beneath J or B"); a++; }
      if (event === "A:exit") a--;
      if (event === "J:enter") { assert.equal(a, 1); j++; }
      if (event === "J:exit") j--;
      if (event.startsWith("B:enter")) { assert.equal(a, 1, "B is only entered inside A"); b++; }
      if (event === "B:exit") b--;
      if (event === "seal") assert.equal(a + j + b, 0, "the seal runs outside every lock");
    }
    assert.deepEqual([a, j, b], [0, 0, 0]);
  });

  for (const n of [1, 3]) {
    it(`barrier totals for a cohort of ${n}`, async t => {
      const r = await rig(t, { codex: false, cohort: n });
      const before = { ...r.counts };
      await r.end();
      assert.equal((await slotOf(r))?.state, "claude_ready");
      const delta = { authority: r.counts.authority - before.authority, records: r.counts.records - before.records, fence: r.counts.fence - before.fence };
      // Fence: config file + bare directory. Records: one file sync each plus the run directory.
      // Slot: file + directory for the slot, then file + directory for its head shard.
      assert.deepEqual(delta, { authority: 4, records: n + 1, fence: 2 });
      const total = delta.authority + delta.records + delta.fence;
      console.log(`publication durability barriers for N=${n}: ${total} (fence ${delta.fence}, records ${delta.records}, slot+head ${delta.authority})`);
      assert.equal(total, n + 7);
    });
  }
});

describe("record decoding of published sources", () => {
  it("a V2 record with an extra key, or a forged certificate shape, is refused even with a valid MAC", async t => {
    const r = await rig(t, { codex: false });
    await r.end();
    const file = path.join(recordDir(r), `${r.captureIds[0]}.json`);
    const record = JSON.parse(fs.readFileSync(file, "utf8")) as Record<string, unknown>;
    const { mac: _mac, ...body } = record;
    const mac = (value: unknown): string => (r.p.recovery as unknown as { computeMac(v: unknown): string }).computeMac(value);
    for (const mutate of [
      (s: Record<string, unknown>) => ({ ...s, extra: 1 }),
      (s: Record<string, unknown>) => ({ ...s, certificate: "xyz" }),
      (s: Record<string, unknown>) => ({ ...s, harness: "other" }),
    ]) {
      const forged = { ...body, completionSource: mutate(body.completionSource as Record<string, unknown>) };
      fs.writeFileSync(file, JSON.stringify({ ...forged, mac: mac(forged) }));
      assert.deepEqual(await r.p.recovery.inspect(r.runId), [], "an undecodable record is omitted, never trusted");
    }
    // A tampered body under the old MAC is also refused.
    fs.writeFileSync(file, JSON.stringify({ ...body, sourceSha: "f".repeat(40), mac: record.mac }));
    assert.deepEqual(await r.p.recovery.inspect(r.runId), []);
    // The untouched record decodes, and its certificate still verifies.
    fs.writeFileSync(file, JSON.stringify(record));
    const [ok] = await r.p.recovery.inspect(r.runId);
    await r.rt.withTransaction(async tx => {
      const store = await openAuthorityStore(tx, { workerToken: TOKEN });
      verifyCertifiedSource(tx, store, ok!.completionSource);
    });
  });

  it("legacy records keep decoding as legacy (no version, exact V1 key set)", async t => {
    const r = await rig(t, { codex: false });
    const [legacy] = await r.p.recovery.inspect(r.runId);
    assert.deepEqual(legacy!.completionSource, r.bound);
    assert.equal((legacy!.completionSource as { version?: number }).version, undefined);
  });
});

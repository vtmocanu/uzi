import { describe, it, type TestContext } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { randomBytes, randomUUID } from "node:crypto";
import fs from "node:fs";
import fsp from "node:fs/promises";
import path from "node:path";
import { acquireWorkerLifetime, type WorkerLifetime } from "../src/authority-lock.js";
import { AuthorityRuntime } from "../src/authority-runtime.js";
import { leaseEpoch, openAuthorityStore, type AuthorityFsOps, type SlotSnapshot, type SourceUseLease } from "../src/completion-authority.js";
import { sourceCoreDigest, sourceFenceOf, verifyCertifiedSource, verifyPublishedSource, type CompletionSourceV2 } from "../src/completion-source.js";
import { ExecutorLifetime, type ExecutorTerminalSeal } from "../src/codex/executor-lifetime.js";
import type { Executor } from "../src/executor.js";
import { legacyCompletionCore, type CompletionSource, type PublicationFsOps, type RecoveryCoordinator } from "../src/recovery.js";
import { mintAttemptId } from "../src/run-quiescence.js";
import { testLockOptions } from "./authority-test-support.js";
import { validCompletionSource as baseValidCompletionSource } from "./fixtures/base-valid-completion-source.js";
import { parseRecoveryJournal as baseParseRecoveryJournal } from "./fixtures/base-parse-recovery-journal.js";
import type { CompletedPublicationReceipt } from "../src/protocol.js";
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
  lifetime: WorkerLifetime;
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
    lifetime, rt, p, flight, bound, bare, clone: cloneInfo.path, runId: claim.run_id, captureIds, executor, warnings, events, counts, fail,
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

  it("a publication that cannot get A in time never publishes late: the end is queued and lands ineligible", async t => {
    const r = await rig(t, { codex: false });
    (r.p as unknown as { sourceUseEndWaitMs: number }).sourceUseEndWaitMs = 150;
    const before = snapshotFiles(r);
    let release!: () => void;
    const held = new Promise<void>(resolve => { release = resolve; });
    let acquired!: () => void;
    const holding = new Promise<void>(resolve => { acquired = resolve; });
    // A long recovery pass owns A through the whole bounded wait of both the publication and the end.
    const holder = r.rt.withTransaction(async () => { acquired(); await held; });
    await holding;
    await r.end();
    release();
    await holder;
    let slot = await slotOf(r);
    for (let i = 0; i < 200 && slot?.state === "active"; i++) { await new Promise(resolve => setTimeout(resolve, 20)); slot = await slotOf(r); }
    assert.equal(slot?.state, "ineligible", "the queued end landed, with no further caller");
    assert.equal(slot?.publication ?? null, null, "nothing was published late");
    assert.equal(fenceOf(r), undefined);
    assert.deepEqual(snapshotFiles(r), before);
    assert.equal(await eligibleRecords(r), 0);
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

  for (const stray of ["junk.txt", "stray.json", "subdir"]) {
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

  it("an installed fence naming the slot's live epoch is not stale: it refuses and rewrites no record", async t => {
    const r = await rig(t, { codex: false, cohort: 2 });
    const live = { version: 1 as const, sourceUseEpoch: (await slotOf(r))!.epoch, sourceCoreDigest: "a".repeat(64) };
    await git.installCompletionSourceFence(r.bare, BRANCH, { runId: r.runId, clonePath: r.clone, attemptId: r.bound.attemptId! }, live);
    const before = snapshotFiles(r);
    await r.end();
    assert.equal((await slotOf(r))?.state, "ineligible");
    assert.deepEqual(fenceOf(r), live, "the foreign fence is untouched");
    assert.deepEqual(snapshotFiles(r), before);
    assert.match(r.warnings.join("\n"), /different fence/);
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
    // While the failure persists the slot stays active (every retry of the queued end fails too)...
    await expectNothingEligible(r, "active", N, true);
    // ...and once it stops, the queued end lands: it is retried, not forgotten, and still never published.
    r.fail.authority = undefined;
    let slot = await slotOf(r);
    for (let i = 0; i < 200 && slot?.state === "active"; i++) { await new Promise(resolve => setTimeout(resolve, 20)); slot = await slotOf(r); }
    assert.equal(slot?.state, "ineligible");
    assert.equal(slot?.publication ?? null, null);
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

// ---- U4 validator wave: stale fences, temps, bare clearance, path forms, lease identity, restart ----------

const receiptOf = (r: Rig): CompletedPublicationReceipt => ({
  hold_id: "11111111-1111-4111-8111-111111111111", run_id: r.runId,
  owner_id: "33333333-3333-4333-8333-333333333333", worker_id: "44444444-4444-4444-8444-444444444444", generation: GENERATION,
  final_head: r.bound.expectedHead, repo_id: "55555555-5555-4555-8555-555555555555",
  connection_id: "66666666-6666-4666-8666-666666666666", project_id: 7, forge_type: "github",
  base_url: "https://example.invalid", branch: BRANCH, mr_iid: 3, observed_branch_head: r.bound.expectedHead,
});

/** The run resumes on the same clone: a fresh epoch, the journal re-binds, and the flight publishes again. */
async function resume(r: Rig, mint: () => Promise<void>): Promise<void> {
  await mint();
  await r.p.bindCompletionSource(r.flight);
  const sink = new Set<string>();
  r.flight.sourceUsePaths = sink;
  r.flight.sourceLease = await r.rt.recordAdmissions(sink, () => r.rt.bindSource(r.clone, r.executor.executorIncarnation));
  r.flight.sourceTailsJoined = true;
}

describe("a stale fence never wedges a clone", () => {
  it("failed publication, then the resumed flight publishes over the fence the failure left", async t => {
    const r = await rig(t, { codex: false, cohort: 3 });
    r.fail.records = (what, n) => what === "syncFile" && n === 2;
    await r.end();
    const stale = fenceOf(r) as { sourceUseEpoch: string };
    assert.ok(stale, "the failed publication left its fence");
    assert.equal((await slotOf(r))?.state, "ineligible");
    assert.equal(readRecords(r).filter(isV2).length, 1);

    await resume(r, async () => undefined);
    await r.end();
    const slot = await slotOf(r);
    assert.equal(slot?.state, "claude_ready");
    const sources = readRecords(r).map(x => x.completionSource as CompletionSourceV2);
    assert.equal(sources.length, 3);
    for (const s of sources) assert.deepEqual(s, sources[0]);
    const fence = fenceOf(r) as { sourceUseEpoch: string };
    assert.deepEqual(fence, sourceFenceOf(sources[0]!), "the journal carries the new publication's fence");
    assert.notEqual(fence.sourceUseEpoch, stale.sourceUseEpoch);
    assert.equal(fence.sourceUseEpoch, slot?.epoch);
    assert.equal(await eligibleRecords(r), 3);
  });

  // A published source is not resumed in production: git.ts throws PendingRecoveryCaptureError for a journal
  // that carries a fence before any seed can consume it, so only a never-completed publication's fence is replaced.
  for (const [label, disturb, discard] of [
    ["the slot was replaced by a fresh active epoch", (r: Rig) => r.rt.withTransaction(() => r.rt.hooks.admitSourceUse(r.clone, "seed-canonical")), false],
    ["the slot is retiring under an operator discard", (r: Rig) => r.rt.withTransaction(() => r.rt.hooks.admitSourceUse(r.clone, "discard")), true],
  ] as const) {
    it(`a stale fence is not replaced when ${label}`, async t => {
      const r = await rig(t, { codex: false, cohort: 2 });
      const stale = r.flight.sourceLease;
      const foreign = { version: 1 as const, sourceUseEpoch: randomUUID(), sourceCoreDigest: "c".repeat(64) };
      await git.installCompletionSourceFence(r.bare, BRANCH, { runId: r.runId, clonePath: r.clone, attemptId: r.bound.attemptId! }, foreign);
      await disturb(r);
      const slotBefore = await slotOf(r);
      const before = snapshotFiles(r);
      r.flight.sourceLease = stale;
      await r.end();
      assert.deepEqual(fenceOf(r), foreign, "the stale fence is untouched");
      assert.deepEqual(snapshotFiles(r), before);
      assert.equal(readRecords(r).some(isV2), false);
      const after = await slotOf(r);
      if (discard) {
        // The discard's quiescence proof ended this incarnation's own active epoch: it is not put back (nobody in
        // this process would hold it), it ends ineligible under the discard's epoch.
        assert.equal(after?.state, "ineligible");
        assert.equal(after?.epoch, slotBefore?.epoch);
        assert.notEqual(after?.epoch, leaseEpoch(stale!), "the displaced own-incarnation active epoch is not resurrected");
      } else {
        assert.ok([slotBefore?.epoch, leaseEpoch(stale!)].includes(after?.epoch), "the slot's epoch is not adopted");
      }
      assert.notEqual(after?.state, "claude_ready");
    });
  }

  it("a discard that deleted nothing leaves no dead own-incarnation active slot: a successor incarnation admits afterwards", async t => {
    const r = await rig(t, { codex: false, cohort: 2 });
    const active = await slotOf(r);
    assert.equal(active?.state, "active", "bindSource L1 left the slot active under this incarnation");
    await r.rt.withTransaction(() => r.rt.hooks.admitSourceUse(r.clone, "discard"));
    await r.end();
    const ended = await slotOf(r);
    assert.equal(ended?.state, "ineligible");
    assert.notEqual(ended?.epoch, active?.epoch);
    const successor = new AuthorityRuntime({ lifetime: r.lifetime, workerToken: TOKEN, workerIncarnation: randomUUID(), timeoutMs: 5000 });
    // A restored dead active epoch would be refused here as "active under another worker incarnation".
    await successor.withTransaction(() => successor.hooks.admitSourceUse(r.clone, "seed-canonical"));
    const next = await successor.withTransaction(async tx => {
      const store = await openAuthorityStore(tx, { workerToken: TOKEN });
      return store.readSlot(tx, await store.slotKey(tx, r.clone));
    });
    assert.equal(next?.state, "active");
    assert.notEqual(next?.epoch, ended?.epoch);
  });
});

describe("crash-era temps do not block publication", () => {
  const uuid = (): string => randomUUID();
  for (const [label, name] of [
    ["record temp", (): string => `${uuid()}.json.${uuid()}.tmp`],
    ["bundle temp", (): string => `${uuid()}.${uuid()}.bundle.tmp`],
    ["orphan bundle", (): string => `${uuid()}.bundle`],
  ] as const) {
    it(`a ${label} left by the coordinator is tolerated and left in place`, async t => {
      const r = await rig(t, { codex: false, cohort: 2 });
      const temp = name();
      fs.writeFileSync(path.join(recordDir(r), temp), "half written");
      await r.end();
      assert.equal((await slotOf(r))?.state, "claude_ready");
      assert.equal(await eligibleRecords(r), 2);
      assert.equal(fs.readFileSync(path.join(recordDir(r), temp), "utf8"), "half written");
    });
  }

  for (const impostor of [
    (): string => `${uuid()}.json.${uuid()}.tmp.extra`, (): string => `${uuid().toUpperCase()}.json.${uuid()}.tmp`,
    (): string => `${uuid()}.json.not-a-uuid.tmp`, (): string => `x.json.${uuid()}.tmp`, (): string => `${uuid()}.bundle.tmp`,
    (): string => `${uuid()}.json.bak`,
  ]) {
    it(`a near-miss name (${impostor().slice(0, 6)}...) still refuses`, async t => {
      const r = await rig(t, { codex: false, cohort: 2 });
      fs.writeFileSync(path.join(recordDir(r), impostor()), "{ not a record");
      const before = snapshotFiles(r);
      await r.end();
      assert.equal((await slotOf(r))?.state, "ineligible");
      assert.equal(fenceOf(r), undefined);
      assert.deepEqual(snapshotFiles(r), before);
    });
  }

  it("a directory carrying a temp name refuses", async t => {
    const r = await rig(t, { codex: false, cohort: 2 });
    fs.mkdirSync(path.join(recordDir(r), `${uuid()}.json.${uuid()}.tmp`));
    await r.end();
    assert.equal((await slotOf(r))?.state, "ineligible");
    assert.equal(fenceOf(r), undefined);
  });
});

describe("publication preconditions", () => {
  it("an unresolved bare mutation on the flight's bare refuses publication", async t => {
    const r = await rig(t, { codex: false, cohort: 2 });
    // A bare mutator another operation began and never settled (its owner may be a dead incarnation).
    await r.rt.withTransaction(() => r.rt.hooks.beginBareMutation(r.bare, "recovery-pin-anchor", ["refs/uzi/pin"]));
    const before = snapshotFiles(r);
    await r.end();
    assert.equal((await slotOf(r))?.state, "ineligible");
    assert.equal(fenceOf(r), undefined);
    assert.deepEqual(snapshotFiles(r), before);
    assert.match(r.warnings.join("\n"), /unresolved/);
  });

  it("source-use paths in a non-normalized form still publish", async t => {
    const r = await rig(t, { codex: false, cohort: 2 });
    const odd = `${path.dirname(r.clone)}/./${path.basename(r.clone)}`;
    assert.notEqual(odd, r.clone);
    r.flight.sourceUsePaths = new Set();
    r.flight.worktreePath = odd;
    r.flight.runnerClone = { ...(r.flight.runnerClone as object), path: `${odd}/` };
    await r.end();
    assert.equal((await slotOf(r))?.state, "claude_ready");
    assert.equal(await eligibleRecords(r), 2);
  });

  for (const how of ["false", "throw"] as const) {
    it(`a later bind that ${how === "false" ? "returns false" : "throws"} clears the bound source`, async t => {
      const r = await rig(t, { codex: false });
      assert.ok(r.flight.completionSourceBound);
      t.mock.method(r.p.recovery, "bindCompletionSource", async () => { if (how === "throw") throw new Error("journal down"); return false; });
      await r.p.bindCompletionSource(r.flight);
      assert.equal(r.flight.completionSourceBound, undefined);
      // With nothing bound the flight is only ended, never published.
      await r.end();
      assert.equal((await slotOf(r))?.state, "ineligible");
      assert.equal(fenceOf(r), undefined);
    });
  }
});

describe("lease identity at publication", () => {
  async function refused(r: Rig, lease: SourceUseLease): Promise<void> {
    const before = snapshotFiles(r);
    let ran = 0;
    const outcome = await r.rt.publishSource(r.clone, lease, { timeoutMs: 2000 }, async () => { ran++; });
    assert.equal(outcome.published, false);
    assert.equal(ran, 0, "the publication work never ran");
    assert.equal(fenceOf(r), undefined);
    assert.deepEqual(snapshotFiles(r), before);
    assert.equal(r.rt.leaseFor(r.clone) !== undefined, true, "the current lease is untouched");
    const slot = await slotOf(r);
    assert.equal(slot?.state, "active");
    assert.equal(slot?.publication ?? null, null);
  }

  it("a lease object that is not the runtime's current entry for the clone refuses and writes nothing", async t => {
    const r = await rig(t, { codex: false, cohort: 2 });
    // Same epoch, different object.
    await refused(r, Object.create(r.flight.sourceLease as SourceUseLease) as SourceUseLease);
    // A stale lease: the clone was re-admitted since.
    const stale = r.flight.sourceLease as SourceUseLease;
    await r.rt.withTransaction(() => r.rt.hooks.admitSourceUse(r.clone, "seed-canonical"));
    const before = snapshotFiles(r);
    const outcome = await r.rt.publishSource(r.clone, stale, { timeoutMs: 2000 }, async () => { throw new Error("must not run"); });
    assert.equal(outcome.published, false);
    assert.deepEqual(snapshotFiles(r), before);
    assert.equal(fenceOf(r), undefined);
  });
});

describe("rollback: a pre-#3404 worker refuses what publication writes", () => {
  it("the captured base gates reject the V2 source and the fenced journal, and accept the bound core", async t => {
    const r = await rig(t, { codex: false, cohort: 2 });
    assert.equal(baseValidCompletionSource(r.bound), true, "the base gate accepts the V1 core (control)");
    assert.doesNotThrow(() => baseParseRecoveryJournal(execFileSync("git", ["-C", r.bare, "config", "--get", `uzi-recovery.${BRANCH}.clone`], { encoding: "utf8", env: ENV }).trim()));
    await r.end();
    for (const record of readRecords(r)) {
      assert.equal(baseValidCompletionSource(record.completionSource), false,
        "base coerceRecord returns null for a V2 source, so the old listRecords skips the record");
    }
    const raw = execFileSync("git", ["-C", r.bare, "config", "--get", `uzi-recovery.${BRANCH}.clone`], { encoding: "utf8", env: ENV }).trim();
    assert.throws(() => baseParseRecoveryJournal(raw), /invalid retained recovery clone journal/,
      "an old worker cannot attach the branch's clone (the path that reaches release()), so it never runs there");
  });
});

describe("restart retirement consumes the durable publication (U5)", () => {
  /** What the receipt-driven cleanup decides for a cohort in the given shape (run-id independent). */
  async function decide(t: TestContext, shape: "v1" | "v2" | "mixed"): Promise<unknown> {
    const r = await rig(t, { codex: false, cohort: 3 });
    if (shape === "v2") await r.end();
    if (shape === "mixed") {
      r.fail.records = (what, n) => what === "syncFile" && n === 2;
      await r.end();
      const v2 = readRecords(r).filter(isV2).length;
      assert.ok(v2 > 0 && v2 < 3, `a partial publication left ${v2} of 3 rewritten`);
    }
    assert.equal(await r.p.recovery.persistCompletionReceipt(receiptOf(r)), true);
    const calls: unknown[][] = [];
    t.mock.method(git, "cleanupRecoveryGeneration", async (...args: unknown[]) => { calls.push(args); return "retained"; });
    const snapshot = (await r.p.recovery.inspect(r.runId)).filter(x => x.completionReceipt);
    assert.equal(snapshot.length, 1);
    await r.p.recovery.resumePending(undefined, snapshot);
    return {
      preflights: calls.length, modes: calls.map(c => c[7]),
      barePath: calls.map(c => c[0] === r.bare), rootsAreTheBoundRoots: calls.map(c => JSON.stringify(c[2]) === JSON.stringify(r.bound.roots)),
      finalHead: calls.map(c => c[5] === r.bound.expectedHead),
    };
  }
  const RETAINED = { preflights: 0, modes: [], barePath: [], rootsAreTheBoundRoots: [], finalHead: [] };
  const RELEASE_DECISION = { preflights: 1, modes: ["completion-check"], barePath: [true], rootsAreTheBoundRoots: [true], finalHead: [true] };

  // A legacy V1 cohort and a partially published one used to reach the release decision after a restart. Under
  // the plan's unknown-retain they never reach the preflight: nothing proves them durably.
  for (const shape of ["v1", "mixed"] as const) {
    it(`${shape === "v1" ? "a V1 cohort" : "a mixed V1/V2 cohort (partial publication)"} is retained before any preflight`, async t => {
      assert.deepEqual(await decide(t, shape), RETAINED);
    });
  }
  it("an all-V2 published cohort reaches the release decision", async t => {
    assert.deepEqual(await decide(t, "v2"), RELEASE_DECISION);
  });
});

describe("an interrupted publication (process death) confers nothing on a restart", () => {
  it("fence plus k of N rewrites and no slot commit: a new incarnation finds nothing eligible and cannot publish over it", async t => {
    const r = await rig(t, { codex: false, cohort: 3 });
    // The publication dies after record 0 is rewritten; the demotion cannot be written either, which is
    // exactly the durable state a killed process leaves: fence installed, 1 of 3 records V2, slot active.
    r.fail.records = (what, n) => what === "syncFile" && n === 2;
    r.fail.authority = () => true;
    r.fail.always = true;
    await r.end();
    const dead = (await slotOf(r))!;
    assert.equal(dead.state, "active");
    assert.equal(dead.owner.worker !== undefined, true);
    assert.equal(readRecords(r).filter(isV2).length, 1);
    assert.ok(fenceOf(r), "the fence is installed");
    const before = snapshotFiles(r);
    const fence = fenceOf(r);

    // Restart: a fresh worker incarnation and a fresh runner/coordinator over the same data dir and journal.
    const incarnation = randomUUID();
    const rt2 = new AuthorityRuntime({ lifetime: r.lifetime, workerToken: TOKEN, workerIncarnation: incarnation, timeoutMs: 5000 });
    const p2 = runnerWith(() => ({ executor: r.executor }), fakeGitlab().gitlab, "u4-publication-join", undefined, { authority: rt2 }) as unknown as PrivateRunner;
    const records = await p2.recovery.inspect(r.runId);
    assert.equal(records.length, 3, "the successor decodes the mixed cohort");
    await rt2.withTransaction(async tx => {
      const store = await openAuthorityStore(tx, { workerToken: TOKEN, workerIncarnation: incarnation });
      for (const record of records) {
        const source = record.completionSource as { version?: number } | undefined;
        if (source?.version === 2) await assert.rejects(verifyPublishedSource(tx, store, source), "a certified record without a published slot confers nothing");
      }
      const slot = await store.readSlot(tx, await store.slotKey(tx, r.clone));
      assert.equal(slot?.state, "active", "the slot is still active under the dead incarnation");
      assert.equal(slot?.epoch, dead.epoch);
      assert.equal(slot?.publication ?? null, null);
    });
    // The successor can neither adopt the dead epoch nor publish with the dead owner's lease.
    await assert.rejects(rt2.bindSource(r.clone, r.executor.executorIncarnation), /another worker incarnation|active/);
    let ran = 0;
    const outcome = await rt2.publishSource(r.clone, r.flight.sourceLease as SourceUseLease, { timeoutMs: 2000 }, async () => { ran++; });
    assert.equal(outcome.published, false);
    assert.equal(ran, 0);
    assert.deepEqual(snapshotFiles(r), before, "no record changed");
    assert.deepEqual(fenceOf(r), fence);
    const after = (await slotOf(r))!;
    assert.equal(after.state, "active");
    assert.equal(after.epoch, dead.epoch);
    assert.equal(await eligibleRecords(r), 0);
  });
});

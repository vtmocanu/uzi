import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
import fs from "node:fs";
import fsp from "node:fs/promises";
import path from "node:path";
import { GitCache, type MemoryParkRecord, type RecoveryJournalEntry } from "../src/git.js";
import type { AuthoritySettlement, GitAuthorityHooks } from "../src/git-authority-hooks.js";
import { formatAttemptId } from "../src/attempt-path.js";
import { type CompletionSourceFence } from "../src/completion-source.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { nullLogger, noProofReseed, testGitCacheOptions } from "./helpers.js";

// issue #3638 D3: the versioned cooperative memory-park record in the recovery journal. Real git in temp dirs.
// Pins the strict v1 parse, the two-phase lock-held writer and reader (injected fsync / read-back faults per phase),
// the conflict rules, and the per-writer carry / retire inventory (with proof that each branch under test ran).

function scrubGitEnv(base: NodeJS.ProcessEnv): NodeJS.ProcessEnv {
  const env: NodeJS.ProcessEnv = {};
  for (const [key, value] of Object.entries(base)) if (!key.startsWith("GIT_")) env[key] = value;
  return { ...env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" };
}
const ENV = scrubGitEnv(process.env);
const BRANCH = "agent/issue-3638";
const KEY = "issue-3638";
const RUN = "run-3638";
const FENCE: CompletionSourceFence = { version: 1, sourceUseEpoch: "0123abcd-0123-4abc-8def-0123456789ab", sourceCoreDigest: "a".repeat(64) };
const UUID_A = "11111111-1111-4111-8111-111111111111";
const UUID_B = "22222222-2222-4222-8222-222222222222";
const UUID_C = "33333333-3333-4333-8333-333333333333";
const aid = (n: number) => formatAttemptId(new Date("2026-01-01T00:00:00Z"), 1, n.toString(16).padStart(16, "0"));

let fx: Fixture;
let cache: GitCache;
let bare: string;
let clonePath: string;

function gitIn(dir: string, args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], { encoding: "utf8", env: ENV, stdio: ["pipe", "pipe", "pipe"] }).trim();
}
const journalKey = (branch = BRANCH): string => `uzi-recovery.${branch}.clone`;
const rawJournal = (branch = BRANCH): string => gitIn(bare, ["config", "--get", journalKey(branch)]);
const journal = (): Record<string, unknown> => JSON.parse(rawJournal()) as Record<string, unknown>;
const parkJson = (): string => JSON.stringify(journal().memoryPark);
const configBytes = (): Buffer => fs.readFileSync(path.join(bare, "config"));
const expected = () => ({ runId: RUN, clonePath });

const BINDING = { runId: RUN, claimGeneration: 7, workerId: "worker-1", registerNonce: "nonce-1", memoryEpisode: 3 };
const ORIGIN = { flightId: UUID_A, attemptId: null, executorIncarnation: null, authorityIncarnation: UUID_B, sourceEpoch: UUID_C };
const evidence = (binding = BINDING) => ({
  parkAck: { ...binding }, interventionIds: [] as string[], processJoin: "executor" as const, commands: "not_required" as const,
  steering: "joined" as const, auxiliaries: "none" as const, feedback: "drained" as const, sinkReceipts: "joined" as const,
  parkReap: "not_applicable" as const,
});
const source = (p: string, n = 1) => ({ path: p, authorityKey: n.toString(16).padStart(64, "a"), attemptId: null, epoch: UUID_C, seq: n });
const unsettled = (over: Partial<MemoryParkRecord> = {}): MemoryParkRecord =>
  ({ version: 1, binding: BINDING, ack: "sent_unconfirmed", origin: ORIGIN, ...over });
const settled = (over: Partial<MemoryParkRecord> = {}, sources = [source(clonePath)]): MemoryParkRecord => ({
  version: 1, binding: BINDING, ack: "confirmed", origin: ORIGIN,
  settlement: { version: 1, confirmed: true, sources, evidence: evidence() }, ...over,
});
const clone = <T,>(value: T): T => structuredClone(value);

function recorder(events: string[]): GitAuthorityHooks {
  const settlement = (label: string): AuthoritySettlement => ({ settle: async (outcome) => { events.push(`settle:${label}:${JSON.stringify(outcome)}`); } });
  return {
    admitSourceUse: async (p, op) => { events.push(`admit:${op}:${path.basename(p)}`); },
    beginBareMutation: async (_b, op) => { events.push(`bare:${op}`); return settlement(op); },
    assertBareClear: async (_b, op) => { events.push(`clear:${op}`); },
    beginDestructive: async (_p, op) => { events.push(`destructive:${op}`); return settlement(op); },
  };
}

beforeEach(async () => {
  fx = makeFixture();
  cache = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
  bare = await cache.ensureClone(fx.originPath);
  fs.chmodSync(path.dirname(bare), 0o700);
  fs.chmodSync(bare, 0o700);
  const runnerClone = await cache.createOrAttachRunnerClone(bare, 3638, noProofReseed, RUN);
  clonePath = runnerClone.path;
  await cache.markRecoveryCapture(bare, clonePath, BRANCH, RUN);
});
afterEach(() => fx.cleanup());

describe("memoryPark strict v1 parse", () => {
  it("accepts the exact v1 record and persists it in its fixed field order, immediately before the fence", async () => {
    await cache.installCompletionSourceFence(bare, BRANCH, expected(), FENCE);
    assert.equal(await cache.recordMemoryPark(bare, BRANCH, expected(), settled()), "settled");
    const keys = Object.keys(journal());
    assert.deepEqual(keys.slice(-2), ["memoryPark", "completionSourceFence"]);
    const park = journal().memoryPark as Record<string, unknown>;
    assert.deepEqual(Object.keys(park), ["version", "binding", "ack", "origin", "settlement"]);
    assert.deepEqual(Object.keys(park.binding as object), ["runId", "claimGeneration", "workerId", "registerNonce", "memoryEpisode"]);
    assert.deepEqual(Object.keys(park.origin as object), ["flightId", "attemptId", "executorIncarnation", "authorityIncarnation", "sourceEpoch"]);
    const settlement = park.settlement as Record<string, unknown>;
    assert.deepEqual(Object.keys(settlement), ["version", "confirmed", "sources", "evidence"]);
    assert.equal(settlement.confirmed, true);
    assert.deepEqual(Object.keys(settlement.evidence as object),
      ["parkAck", "interventionIds", "processJoin", "commands", "steering", "auxiliaries", "feedback", "sinkReceipts", "parkReap"]);
  });

  const bad: Array<[string, (r: Record<string, any>) => void]> = [
    ["an unknown version", r => { r.version = 2; }],
    ["an unknown top-level key", r => { r.extra = 1; }],
    ["an unknown binding key", r => { r.binding.extra = 1; }],
    ["an unknown origin key", r => { r.origin.extra = 1; }],
    ["an unknown settlement key", r => { r.settlement.extra = 1; }],
    ["an unknown evidence key", r => { r.settlement.evidence.extra = 1; }],
    ["an unknown source key", r => { r.settlement.sources[0].extra = 1; }],
    ["a missing origin", r => { delete r.origin; }],
    ["an ack outside the three states", r => { r.ack = "maybe"; }],
    ["a settlement on an unconfirmed ack", r => { r.ack = "sent_unconfirmed"; }],
    ["a settlement parkAck that differs from the binding", r => { r.settlement.evidence.parkAck.memoryEpisode = 9; }],
    ["a binding naming another run", r => { r.binding.runId = "run-other"; r.settlement.evidence.parkAck.runId = "run-other"; }],
    ["a zero claim generation", r => { r.binding.claimGeneration = 0; r.settlement.evidence.parkAck.claimGeneration = 0; }],
    ["a negative memory episode", r => { r.binding.memoryEpisode = -1; r.settlement.evidence.parkAck.memoryEpisode = -1; }],
    ["a non-uuid flight id", r => { r.origin.flightId = "not-a-uuid"; }],
    ["a non-uuid authority incarnation", r => { r.origin.authorityIncarnation = "nope"; }],
    ["a non-uuid source epoch", r => { r.origin.sourceEpoch = "nope"; }],
    ["a missing source epoch", r => { delete r.origin.sourceEpoch; }],
    ["a malformed executor incarnation", r => { r.origin.executorIncarnation = "has space"; }],
    ["a malformed attempt id", r => { r.origin.attemptId = "attempt"; }],
    ["no sources", r => { r.settlement.sources = []; }],
    ["a relative source path", r => { r.settlement.sources[0].path = "relative/path"; }],
    ["a non-normalized source path", r => { r.settlement.sources[0].path = `${clonePath}/../x`; }],
    ["a malformed authority key", r => { r.settlement.sources[0].authorityKey = "abc"; }],
    ["a malformed epoch", r => { r.settlement.sources[0].epoch = "nope"; }],
    ["a zero seq", r => { r.settlement.sources[0].seq = 0; }],
    ["a non-boolean confirmed", r => { r.settlement.confirmed = "yes"; }],
    ["a duplicated source", r => { r.settlement.sources.push({ ...r.settlement.sources[0] }); }],
    ["unsorted sources", r => { r.settlement.sources = [source("/z/b", 2), source("/a/b", 3)]; }],
    ["more than 16 sources", r => { r.settlement.sources = Array.from({ length: 17 }, (_, i) => source(`/p/${String(i).padStart(2, "0")}`, i + 1)); }],
    ["more than 64 intervention ids", r => { r.settlement.evidence.interventionIds = Array.from({ length: 65 }, (_, i) => `id-${i}`); }],
    ["duplicate intervention ids", r => { r.settlement.evidence.interventionIds = ["a", "a"]; }],
    ["an unknown processJoin", r => { r.settlement.evidence.processJoin = "neither"; }],
    ["a steering that is not joined", r => { r.settlement.evidence.steering = "pending"; }],
  ];

  it("a raw journal carrying the valid record is accepted (the control for every refusal below)", async () => {
    gitIn(bare, ["config", journalKey(), JSON.stringify({ ...journal(), memoryPark: settled() })]);
    await cache.markRecoveryCapture(bare, clonePath, BRANCH, RUN);
    assert.equal(parkJson(), JSON.stringify(settled()));
  });

  for (const [name, mutate] of bad) {
    it(`refuses ${name}, invalidating the whole journal (the existing fail-closed path)`, async () => {
      const record = clone(settled()) as unknown as Record<string, any>;
      mutate(record);
      gitIn(bare, ["config", journalKey(), JSON.stringify({ ...journal(), memoryPark: record })]);
      await assert.rejects(cache.markRecoveryCapture(bare, clonePath, BRANCH, RUN));
    });
  }

  it("refuses a record whose binding names a different run than its journal", async () => {
    const other = unsettled({ binding: { ...BINDING, runId: "run-other" } });
    gitIn(bare, ["config", journalKey(), JSON.stringify({ ...journal(), memoryPark: other })]);
    await assert.rejects(cache.markRecoveryCapture(bare, clonePath, BRANCH, RUN));
  });

  it("recordMemoryPark refuses an invalid record or a record for another run before any write", async () => {
    const before = configBytes();
    await assert.rejects(cache.recordMemoryPark(bare, BRANCH, expected(), { ...settled(), ack: "sent_unconfirmed" }), /invalid memory park record/);
    await assert.rejects(cache.recordMemoryPark(bare, BRANCH, { runId: "run-other", clonePath }, settled()), /another run/);
    assert.deepEqual(configBytes(), before);
  });
});

describe("recordMemoryPark durability: two phases, each under the bare lock", () => {
  const syncFailing = (nth: number, what: "config" | "directory") => {
    let n = 0;
    return async (handle: { sync(): Promise<void> }, kind: "config" | "directory") => {
      n++;
      if (n === nth && kind === what) throw new Error("injected sync failure");
      await handle.sync();
    };
  };
  const readbackTampered = (nth: number) => {
    let n = 0;
    return async (read: () => Promise<string | undefined>) => (++n === nth ? "tampered" : read());
  };
  const stored = () => (journal().memoryPark as MemoryParkRecord);
  const confirmedRead = (record: MemoryParkRecord = settled()) =>
    cache.readConfirmedMemoryPark(bare, BRANCH, { ...expected(), record });

  it("success: phase 1 then phase 2, each syncing the config file and then the bare directory, then a confirmed read", async () => {
    const synced: string[] = [];
    let readbacks = 0;
    const out = await cache.recordMemoryPark(bare, BRANCH, expected(), settled(), {
      sync: async (h, what) => { synced.push(what); await h.sync(); },
      readback: async read => { readbacks++; return read(); },
    });
    assert.equal(out, "settled");
    assert.deepEqual(synced, ["config", "directory", "config", "directory"]);
    assert.equal(readbacks, 2);
    assert.equal(stored().settlement?.confirmed, true);
    assert.deepEqual(await confirmedRead(), settled());
  });

  it("a record without a settlement is one write with one sync pair and one read-back", async () => {
    const synced: string[] = [];
    let readbacks = 0;
    assert.equal(await cache.recordMemoryPark(bare, BRANCH, expected(), unsettled(), {
      sync: async (h, what) => { synced.push(what); await h.sync(); },
      readback: async read => { readbacks++; return read(); },
    }), "recorded");
    assert.deepEqual(synced, ["config", "directory"]);
    assert.equal(readbacks, 1);
    assert.equal(stored().settlement, undefined);
  });

  const phase1: Array<[string, { sync?: ReturnType<typeof syncFailing>; readback?: ReturnType<typeof readbackTampered> }]> = [
    ["config fsync", { sync: syncFailing(1, "config") }],
    ["directory fsync", { sync: syncFailing(2, "directory") }],
    ["read-back", { readback: readbackTampered(1) }],
  ];
  for (const [name, opts] of phase1) {
    it(`phase 1 ${name} failure: the record never qualifies (confirmed:false stays, the reader refuses)`, async () => {
      await assert.rejects(cache.recordMemoryPark(bare, BRANCH, expected(), settled(), opts));
      assert.equal(stored().settlement?.confirmed, false, "only the unconfirmed phase-1 value can be readable");
      assert.equal(await confirmedRead(), undefined);
      assert.equal(await confirmedRead(settled({ settlement: { ...settled().settlement!, confirmed: false } })), undefined,
        "an expected record that is itself unconfirmed never qualifies");
    });
  }

  const phase2: Array<[string, { sync?: ReturnType<typeof syncFailing>; readback?: ReturnType<typeof readbackTampered> }]> = [
    ["config fsync", { sync: syncFailing(3, "config") }],
    ["directory fsync", { sync: syncFailing(4, "directory") }],
    ["read-back", { readback: readbackTampered(2) }],
  ];
  for (const [name, opts] of phase2) {
    it(`phase 2 ${name} failure: reports unconfirmed and reverts to confirmed:false`, async () => {
      assert.equal(await cache.recordMemoryPark(bare, BRANCH, expected(), settled(), opts), "unconfirmed");
      assert.equal(stored().settlement?.confirmed, false);
      assert.equal(await confirmedRead(), undefined);
    });
  }

  it("an aborted signal stops phase 2 from starting: the phase-1 draft stays unconfirmed and nothing qualifies", async () => {
    const abort = new AbortController();
    let pairs = 0;
    const out = await cache.recordMemoryPark(bare, BRANCH, expected(), settled(), {
      signal: abort.signal,
      sync: async (h, what) => { await h.sync(); if (what === "directory" && ++pairs === 1) abort.abort(); },
    });
    assert.equal(out, "unconfirmed");
    assert.equal(pairs, 1, "no phase-2 sync pair ran");
    assert.equal(stored().settlement?.confirmed, false, "phase 2 never wrote confirmed:true");
    assert.equal(await confirmedRead(), undefined);
  });

  it("a signal aborted during phase 2 whose phase 2 then fails still reverts: unconfirmed and confirmed:false is stored", async () => {
    const abort = new AbortController();
    let syncs = 0;
    const out = await cache.recordMemoryPark(bare, BRANCH, expected(), settled(), {
      signal: abort.signal,
      sync: async (h, what) => {
        // Sync calls 1-2 are phase 1 (config, directory); call 3 is phase 2's config fsync.
        if (++syncs === 3 && what === "config") { abort.abort(); throw new Error("injected phase 2 sync failure"); }
        await h.sync();
      },
    });
    assert.equal(out, "unconfirmed");
    assert.equal(syncs, 3, "phase 2 got its lock and failed at its config fsync");
    assert.equal(stored().settlement?.confirmed, false, "the revert ran although the signal was already aborted");
    assert.equal(await confirmedRead(), undefined);
  });

  it("an already aborted signal forfeits the lock hold: nothing is written", async () => {
    const before = configBytes();
    await assert.rejects(cache.recordMemoryPark(bare, BRANCH, expected(), settled(), { signal: AbortSignal.abort() }));
    assert.deepEqual(configBytes(), before);
  });

  it("a replay after a failed phase 2 completes both phases", async () => {
    assert.equal(await cache.recordMemoryPark(bare, BRANCH, expected(), settled(), { sync: syncFailing(3, "config") }), "unconfirmed");
    assert.equal(await cache.recordMemoryPark(bare, BRANCH, expected(), settled()), "settled");
    assert.deepEqual(await confirmedRead(), settled());
  });

  it("phase 2 refuses to confirm a record another writer replaced between the phases", async (t) => {
    // The bare lock is released between the phases: a competing rewrite lands before phase 2's lock hold.
    const seam = cache as unknown as { withLock: <T>(key: string, fn: () => Promise<T>) => Promise<T> };
    const withLock = seam.withLock.bind(cache);
    let holds = 0;
    t.mock.method(seam, "withLock", async <T,>(key: string, fn: () => Promise<T>): Promise<T> => {
      if (++holds === 2) {
        gitIn(bare, ["config", journalKey(), JSON.stringify({ ...journal(), memoryPark: unsettled({ binding: { ...BINDING, claimGeneration: 8 } }) })]);
      }
      return withLock(key, fn);
    });
    const out = await cache.recordMemoryPark(bare, BRANCH, expected(), settled());
    assert.equal(out, "unconfirmed");
    assert.equal(stored().binding.claimGeneration, 8, "the foreign record is untouched by the revert");
    assert.equal(await confirmedRead(), undefined);
  });

  describe("readConfirmedMemoryPark", () => {
    beforeEach(async () => { assert.equal(await cache.recordMemoryPark(bare, BRANCH, expected(), settled()), "settled"); });

    it("returns the record only when every step succeeds and it equals the exact expected record", async () => {
      assert.deepEqual(await confirmedRead(), settled());
      const syncs: string[] = [];
      assert.deepEqual(await cache.readConfirmedMemoryPark(bare, BRANCH, { ...expected(), record: settled() }, {
        sync: async (h, what) => { syncs.push(what); await h.sync(); } }), settled());
      assert.deepEqual(syncs, ["config", "directory"], "the reader itself syncs the config file and then the directory");
    });

    it("refuses on a config fsync failure, a directory fsync failure and a read-back that differs", async () => {
      assert.equal(await cache.readConfirmedMemoryPark(bare, BRANCH, { ...expected(), record: settled() }, { sync: syncFailing(1, "config") }), undefined);
      assert.equal(await cache.readConfirmedMemoryPark(bare, BRANCH, { ...expected(), record: settled() }, { sync: syncFailing(2, "directory") }), undefined);
      assert.equal(await cache.readConfirmedMemoryPark(bare, BRANCH, { ...expected(), record: settled() }, { readback: readbackTampered(1) }), undefined);
    });

    it("refuses any expected record, path, run or attempt that is not byte-identical to the stored one", async () => {
      const other = clone(settled()) as MemoryParkRecord;
      other.settlement!.sources[0]!.seq = 99;
      assert.equal(await confirmedRead(other), undefined);
      assert.equal(await confirmedRead(settled({ binding: { ...BINDING, memoryEpisode: 4 } })), undefined);
      assert.equal(await cache.readConfirmedMemoryPark(bare, BRANCH, { runId: RUN, clonePath: `${clonePath}-x`, record: settled() }), undefined);
      assert.equal(await cache.readConfirmedMemoryPark(bare, BRANCH, { runId: "run-other", clonePath, record: settled() }), undefined);
      assert.equal(await cache.readConfirmedMemoryPark(bare, BRANCH, { runId: RUN, clonePath, attemptId: aid(1), record: settled() }), undefined);
      assert.deepEqual(await confirmedRead(), settled(), "the unchanged expected record still qualifies");
    });

    it("a competing git-config rewrite on the same bare waits on the bare lock while the reader runs", async () => {
      let release!: () => void;
      const gate = new Promise<void>(resolve => { release = resolve; });
      let entered!: () => void;
      const inside = new Promise<void>(resolve => { entered = resolve; });
      const reading = cache.readConfirmedMemoryPark(bare, BRANCH, { ...expected(), record: settled() }, {
        sync: async (h, what) => { if (what === "config") { entered(); await gate; } await h.sync(); },
      });
      await inside;
      const before = rawJournal();
      let rewritten = false;
      const rewrite = cache.markRecoveryCapture(bare, clonePath, BRANCH, RUN, undefined, "c".repeat(40)).then(() => { rewritten = true; });
      await new Promise(resolve => setTimeout(resolve, 250));
      assert.equal(rewritten, false, "the competing rewrite is still waiting on the bare lock");
      assert.equal(rawJournal(), before, "and nothing it would write has landed");
      release();
      assert.deepEqual(await reading, settled());
      await rewrite;
      assert.equal(rewritten, true);
      assert.deepEqual(journal().peeledMarkers, ["c".repeat(40)]);
      assert.equal(parkJson(), JSON.stringify(settled()), "the rewrite carried the record");
    });
  });
});

describe("recordMemoryPark conflicts and replay", () => {
  it("an identical replay repeats the sync and read-back work and changes nothing", async () => {
    await cache.recordMemoryPark(bare, BRANCH, expected(), settled());
    const before = rawJournal();
    const syncs: string[] = [];
    let readbacks = 0;
    assert.equal(await cache.recordMemoryPark(bare, BRANCH, expected(), settled(), {
      sync: async (h, what) => { syncs.push(what); await h.sync(); },
      readback: async read => { readbacks++; return read(); },
    }), "settled");
    assert.deepEqual(syncs, ["config", "directory"]);
    assert.equal(readbacks, 1);
    assert.equal(rawJournal(), before);
    // The same for an unsettled record.
    await cache.recordMemoryPark(bare, BRANCH, { runId: RUN, clonePath }, unsettled({ binding: { ...BINDING, claimGeneration: 8 } }));
    const syncs2: string[] = [];
    await cache.recordMemoryPark(bare, BRANCH, expected(), unsettled({ binding: { ...BINDING, claimGeneration: 8 } }), {
      sync: async (h, what) => { syncs2.push(what); await h.sync(); } });
    assert.deepEqual(syncs2, ["config", "directory"]);
  });

  it("an identical replay fails when its sync fails (no short-circuit)", async () => {
    await cache.recordMemoryPark(bare, BRANCH, expected(), settled());
    await assert.rejects(cache.recordMemoryPark(bare, BRANCH, expected(), settled(), {
      sync: async () => { throw new Error("injected sync failure"); } }), /injected sync failure/);
  });

  const conflicts: Array<[string, () => MemoryParkRecord]> = [
    ["a different memory episode in the same claim generation", () => settled({ binding: { ...BINDING, memoryEpisode: 4 }, settlement: { ...settled().settlement!, evidence: evidence({ ...BINDING, memoryEpisode: 4 }) } })],
    ["a different source epoch in the same claim generation", () => settled({ origin: { ...ORIGIN, sourceEpoch: UUID_A } })],
    ["a different flight in the same claim generation", () => settled({ origin: { ...ORIGIN, flightId: UUID_C } })],
    ["a lower claim generation", () => settled({ binding: { ...BINDING, claimGeneration: 6 }, settlement: { ...settled().settlement!, evidence: evidence({ ...BINDING, claimGeneration: 6 }) } })],
    ["different sources in the same claim generation", () => settled({}, [source(clonePath, 2)])],
    ["an unsettled record over the settled one of the same claim generation", () => ({ version: 1, binding: BINDING, ack: "confirmed", origin: ORIGIN })],
    ["a downgraded ack over the settled one", () => unsettled()],
  ];
  for (const [name, makeIncoming] of conflicts) {
    it(`refuses ${name}, writing nothing`, async () => {
      await cache.recordMemoryPark(bare, BRANCH, expected(), settled());
      const before = configBytes();
      await assert.rejects(cache.recordMemoryPark(bare, BRANCH, expected(), makeIncoming()), /conflicts with the journal's/);
      assert.deepEqual(configBytes(), before);
    });
  }

  it("within one claim generation the ack only upgrades: not_sent, sent_unconfirmed, confirmed, then the settlement", async () => {
    const send = (r: MemoryParkRecord) => cache.recordMemoryPark(bare, BRANCH, expected(), r);
    assert.equal(await send(unsettled({ ack: "not_sent" })), "recorded");
    assert.equal(await send(unsettled({ ack: "sent_unconfirmed" })), "recorded");
    await assert.rejects(send(unsettled({ ack: "not_sent" })), /conflicts/);
    assert.equal(await send({ version: 1, binding: BINDING, ack: "confirmed", origin: ORIGIN }), "recorded");
    await assert.rejects(send(unsettled({ ack: "sent_unconfirmed" })), /conflicts/);
    assert.equal(await send(settled()), "settled");
    assert.equal((journal().memoryPark as MemoryParkRecord).settlement?.confirmed, true);
  });

  it("a strictly greater claim generation replaces, even a settled record, and keeps a provenance-only origin", async () => {
    await cache.recordMemoryPark(bare, BRANCH, expected(), settled());
    const later = unsettled({ binding: { ...BINDING, claimGeneration: 9, registerNonce: "nonce-2" }, ack: "not_sent",
      origin: { ...ORIGIN, flightId: UUID_C } });
    assert.equal(await cache.recordMemoryPark(bare, BRANCH, expected(), later), "recorded");
    assert.equal(parkJson(), JSON.stringify(later));
    assert.equal((journal().memoryPark as MemoryParkRecord).origin.authorityIncarnation, ORIGIN.authorityIncarnation);
  });

  it("refuses a journal that does not name the expected source, writing nothing", async () => {
    const before = configBytes();
    await assert.rejects(cache.recordMemoryPark(bare, BRANCH, { runId: RUN, clonePath: `${clonePath}-x` }, unsettled()), /does not name this source/);
    await assert.rejects(cache.recordMemoryPark(bare, BRANCH, { runId: RUN, clonePath, attemptId: aid(2) }, unsettled()), /does not name this source/);
    gitIn(bare, ["config", journalKey(), ""]);
    await assert.rejects(cache.recordMemoryPark(bare, BRANCH, expected(), unsettled()), /does not name this source/);
    assert.notDeepEqual(configBytes(), before);
    assert.equal(gitIn(bare, ["config", "--get", journalKey()]), "");
  });
});

describe("memoryPark carry and retire inventory", () => {
  const sourceOf = () => ({ runId: RUN, clonePath });
  const head = () => gitIn(clonePath, ["rev-parse", "HEAD"]);
  beforeEach(async () => { await cache.recordMemoryPark(bare, BRANCH, expected(), settled()); });
  const original = () => JSON.stringify(settled());

  it("the spread rewriters carry the record byte-identically: markRecoveryCapture, reserve, capture", async () => {
    await cache.markRecoveryCapture(bare, clonePath, BRANCH, RUN, undefined, "d".repeat(40));
    assert.equal(parkJson(), original());
    const r = await cache.reserveRecoveryIteration(bare, BRANCH, KEY, sourceOf());
    assert.equal(parkJson(), original());
    await cache.recordRecoveryCapture(bare, BRANCH, KEY, sourceOf(), r.attempts, head());
    assert.equal(parkJson(), original());
    assert.equal((journal().recovery as { stage: string }).stage, "captured", "the capture branch ran");
  });

  it("blockRecoveryEpisode carries the record", async () => {
    const r = await cache.reserveRecoveryIteration(bare, BRANCH, KEY, sourceOf());
    await cache.blockRecoveryEpisode(bare, BRANCH, KEY, sourceOf(), "capture_failed", r);
    assert.equal((journal().recovery as { stage: string }).stage, "blocked", "the block branch ran");
    assert.equal(parkJson(), original());
  });

  it("releaseAttemptInPlace's spread branch (a journal with an episode) carries the record; its clear branch clears the key", async () => {
    await cache.reserveRecoveryIteration(bare, BRANCH, KEY, sourceOf());
    await cache.releaseAttemptInPlace(bare, clonePath, BRANCH, RUN, "reclaimed");
    assert.ok(Array.isArray(journal().retainedSources), "the spread branch rewrote the journal with its retained sources");
    assert.equal(parkJson(), original());
    // Plain journal: the clear branch clears the whole active key, the record with it (source disposal is unchanged).
    gitIn(bare, ["config", journalKey(), JSON.stringify({ runId: RUN, clonePath, memoryPark: settled() })]);
    await cache.releaseAttemptInPlace(bare, clonePath, BRANCH, RUN, "reclaimed");
    assert.equal(gitIn(bare, ["config", "--get", journalKey()]), "");
  });

  it("adopt successor is a same-run predecessor replacement: its fresh journal object retires the record", async () => {
    const r = await cache.reserveRecoveryIteration(bare, BRANCH, KEY, sourceOf());
    await cache.recordRecoveryCapture(bare, BRANCH, KEY, sourceOf(), r.attempts, head());
    assert.equal(parkJson(), original(), "the predecessor journal carried it up to the adoption");
    const successor = await cache.prepareRecoverySuccessor(bare, BRANCH, KEY, sourceOf(), r.attempts, aid(2));
    assert.equal((journal() as unknown as RecoveryJournalEntry).clonePath, successor.path, "the journal was replaced by the successor's");
    assert.equal(journal().memoryPark, undefined, "the predecessor's record is not the successor's");
  });

  it("detachTerminalRetained transfers the record byte-identically into the protected descriptor, which no discovery consumes", async () => {
    const r = await cache.reserveRecoveryIteration(bare, BRANCH, KEY, sourceOf());
    await cache.recordRecoveryCapture(bare, BRANCH, KEY, sourceOf(), r.attempts, head());
    const active = journal() as unknown as RecoveryJournalEntry;
    await cache.detachTerminalRetained(bare, BRANCH, KEY, active);
    // The transfer ran: the descriptor exists and the active key is cleared.
    assert.equal(gitIn(bare, ["config", "--get", journalKey()]), "");
    const descriptor = JSON.parse(gitIn(bare, ["config", "--get", `uzi-retained.${RUN}.journal`])) as { journal: { memoryPark?: unknown } };
    assert.equal(JSON.stringify(descriptor.journal.memoryPark), original());
    // D4 discovery reads only the active key: the descriptor is preserved, not consumed.
    assert.equal(await cache.discoverRetainedRecovery(fx.originPath, BRANCH, KEY, RUN), undefined);
  });

  it("an active journal equal to its protected descriptor is never rewritten by the producer", async () => {
    await cache.reserveRecoveryIteration(bare, BRANCH, KEY, sourceOf());
    const raw = rawJournal();
    gitIn(bare, ["config", `uzi-retained.${RUN}.journal`, JSON.stringify({ version: 1, branch: BRANCH, key: KEY, journal: journal() })]);
    const before = configBytes();
    for (const record of [unsettled(), settled()]) {
      assert.equal(await cache.recordMemoryPark(bare, BRANCH, expected(), record), "unconfirmed");
      assert.deepEqual(configBytes(), before, "nothing was written");
      assert.equal(rawJournal(), raw);
    }
  });

  it("a stopped canonical marker never acquires the record, and its absence authorizes nothing", async () => {
    assert.equal(await cache.markStoppedCanonical(bare, BRANCH, KEY, { runId: RUN, clonePath, generation: null, reason: "test" }), "retained");
    const marker = gitIn(bare, ["config", "--get-all", `uzi-stopped.${RUN}.entry`]);
    assert.doesNotMatch(marker, /memoryPark/);
    assert.equal(gitIn(bare, ["config", "--get", journalKey()]), "", "the bound journal was cleared");
    assert.equal(JSON.parse(marker).journalDigest.length, 64, "the marker is bound to the raw journal by digest only");
    // No journal remains, so no record can be written for the stopped source and none is read from the marker.
    await assert.rejects(cache.recordMemoryPark(bare, BRANCH, expected(), unsettled({ binding: { ...BINDING, claimGeneration: 9 } })), /does not name this source/);
    assert.equal(await cache.readConfirmedMemoryPark(bare, BRANCH, { ...expected(), record: settled() }), undefined);
    assert.doesNotMatch(gitIn(bare, ["config", "--get-all", `uzi-stopped.${RUN}.entry`]), /memoryPark/);
  });

  it("a journal bound by a stopped marker whose clear failed is never rewritten by the producer, and still heals", async () => {
    const markerKey = `uzi-stopped.${RUN}.entry`;
    const raw = rawJournal();
    gitIn(bare, ["config", "--add", markerKey, JSON.stringify({ version: 1, runId: RUN, generation: null, branch: BRANCH, key: KEY, clonePath,
      journalDigest: createHash("sha256").update(raw).digest("hex"), reason: "test", at: "2026-10-27T00:00:00.000Z" })]);
    const before = configBytes();
    for (const record of [unsettled(), settled()]) {
      assert.equal(await cache.recordMemoryPark(bare, BRANCH, expected(), record), "unconfirmed", "a non-qualifying outcome");
      assert.deepEqual(configBytes(), before, "nothing was written");
      assert.equal(rawJournal(), raw, "the raw journal bytes are unchanged, so the marker digest still matches");
    }
    assert.equal(parkJson(), original(), "the record the journal already carried is untouched");
    // The marker's own heal still clears the bound journal.
    assert.equal(await cache.discoverRetainedRecovery(fx.originPath, BRANCH, KEY, RUN), undefined);
    assert.equal(gitIn(bare, ["config", "--get", journalKey()]), "");
  });

  it("a stopped attempt retention carries no record into the ledger either", async () => {
    const AB = "agent/issue-3639", AKEY = "issue-3639", ARUN = "run-3639";
    const attemptId = aid(5);
    const info = await cache.runnerCloneForBranch(bare, AB, AKEY, {
      isLive: () => false, beforeFree: async () => { throw new Error("unexpected incumbent"); },
    }, ARUN, false, undefined, { attemptId, isLive: () => false, beforeSeed: async () => {} });
    await cache.markRecoveryCapture(bare, info.path, AB, ARUN, attemptId);
    const record = unsettled({ binding: { ...BINDING, runId: ARUN }, origin: { ...ORIGIN, attemptId } });
    await cache.recordMemoryPark(bare, AB, { runId: ARUN, clonePath: info.path, attemptId }, record);
    assert.equal(await cache.retainStoppedAttempt(bare, AB, { runId: ARUN, clonePath: info.path, attemptId }), "retained");
    assert.equal(gitIn(bare, ["config", "--get", journalKey(AB)]), "", "the active key (and the record) was cleared by the unchanged retention");
    assert.doesNotMatch(gitIn(bare, ["config", "--get-all", `uzi-attempts.${AB}.entry`]), /memoryPark/);
    assert.match(gitIn(bare, ["config", "--get-all", `uzi-attempts.${AB}.entry`]), /"state":"reclaimed"/);
  });
});

describe("negative control: a fenced journal carrying the record keeps the fence refusal", () => {
  it("releaseAttemptInPlace refuses at refuseFenceDrop, before the ledger append or the clear, bytes retained", async () => {
    const events: string[] = [];
    const strict = new GitCache(fx.dataDir, nullLogger(), undefined,
      testGitCacheOptions({ authority: recorder(events), authorityRequired: true }));
    await cache.installCompletionSourceFence(bare, BRANCH, expected(), FENCE);
    await cache.recordMemoryPark(bare, BRANCH, expected(), settled());
    const before = configBytes();
    const rawBefore = rawJournal();
    assert.deepEqual(Object.keys(journal()).slice(-2), ["memoryPark", "completionSourceFence"], "the fixture carries both");
    // The refusal path itself: the fence-drop message names this very operation, so it is not a setup error.
    await assert.rejects(strict.releaseAttemptInPlace(bare, clonePath, BRANCH, RUN, "reclaimed"),
      /releaseAttemptInPlace: the journal carries a completion source fence/);
    assert.deepEqual(configBytes(), before, "no ledger append, no clear");
    assert.equal(rawJournal(), rawBefore);
    // The record alone neither refuses nor authorizes: without the fence the same call proceeds and clears the plain journal.
    const { completionSourceFence: _drop, ...plain } = journal();
    gitIn(bare, ["config", journalKey(), JSON.stringify(plain)]);
    await strict.releaseAttemptInPlace(bare, clonePath, BRANCH, RUN, "reclaimed");
    assert.equal(gitIn(bare, ["config", "--get", journalKey()]), "");
  });
});

describe("record survival through the retireRunnerClone rollback sites (the branch under test ran, then bytes are identical)", () => {
  type Seam = {
    runGit: (cwd: string | undefined, args: string[]) => Promise<string>;
    rewriteLedgerAtomically: (bare: string, key: string, values: string[]) => Promise<void>;
  };
  const holdingRoot = (): string => path.join(fx.dataDir, "runner-quarantine");
  const B2 = "agent/issue-3641", RUN2 = "run-3641";
  const key2 = (): string => journalKey(B2);
  const raw2 = (): string => rawJournal(B2);
  const park2 = (): string => JSON.stringify((JSON.parse(raw2()) as { memoryPark?: unknown }).memoryPark);

  /** A real attempt clone whose journal carries the record AND a fence (cache is not authorityRequired, so the
   *  fence does not refuse before the rollback branch is reached). */
  async function fenced(): Promise<{ attemptId: string; clone: string; raw: string; park: string }> {
    const attemptId = aid(7);
    const info = await cache.createOrAttachRunnerClone(bare, 3641, noProofReseed, RUN2, false, undefined,
      { attemptId, isLive: () => false, beforeSeed: async () => {} });
    const runId = RUN2;
    const binding = { ...BINDING, runId };
    await cache.markRecoveryCapture(bare, info.path, B2, runId, attemptId);
    const exp = { runId, clonePath: info.path, attemptId };
    await cache.installCompletionSourceFence(bare, B2, exp, FENCE);
    const record = settled({ binding, origin: { ...ORIGIN, attemptId },
      settlement: { version: 1, confirmed: true, sources: [source(info.path)], evidence: evidence(binding) } });
    assert.equal(await cache.recordMemoryPark(bare, B2, exp, record), "settled");
    assert.deepEqual(Object.keys(JSON.parse(raw2()) as object).slice(-2), ["memoryPark", "completionSourceFence"]);
    return { attemptId, clone: info.path, raw: raw2(), park: JSON.stringify(record) };
  }

  /** Revoke the process proof at the moment the real journal clear (or the retired ledger append) settled, after the clone moved. */
  function revokeAt(t: import("node:test").TestContext, fx2: { clone: string; failedRestore?: boolean; at?: "clear" | "retired" }) {
    const state = { live: true, movedTo: undefined as string | undefined, moves: [] as string[][], clears: 0, writes: [] as string[] };
    const rename = fsp.rename.bind(fsp);
    t.mock.method(fsp, "rename", async (...args: Parameters<typeof fsp.rename>) => {
      const from = String(args[0]), to = String(args[1]);
      if (fx2.failedRestore && from === state.movedTo && to === fx2.clone) throw Object.assign(new Error("restore denied"), { code: "EACCES" });
      await rename(...args);
      if (from === fx2.clone) state.movedTo = to;
      if (from === fx2.clone || to === fx2.clone) state.moves.push([from, to]);
    });
    const seam = cache as unknown as Seam;
    const runGit = seam.runGit.bind(cache);
    t.mock.method(seam, "runGit", async (cwd: string | undefined, args: string[]) => {
      const result = await runGit(cwd, args);
      if (args.at(-2) === key2()) {
        const value = args.at(-1)!;
        state.writes.push(value);
        if (value === "") state.clears++;
        if (state.live && state.movedTo && value === "" && (fx2.at ?? "clear") === "clear") state.live = false;
      }
      if (state.live && state.movedTo && fx2.at === "retired" && args.includes("--add") && args.at(-1)?.includes('"state":"retired"')) state.live = false;
      return result;
    });
    return state;
  }

  it("raw restore: the journal is restored from its pre-move raw bytes, record and fence included", async (t) => {
    const f = await fenced();
    const state = revokeAt(t, { clone: f.clone });
    await assert.rejects(cache.retireRunnerClone(bare, f.clone, B2, RUN2, {
      discard: true, attemptId: f.attemptId, processProof: () => state.live,
    }), /process proof revoked/);
    // The rollback branch ran: the real clear settled, then the clone moved back and the raw journal was rewritten.
    assert.equal(state.clears, 1);
    assert.deepEqual(state.moves, [[f.clone, state.movedTo], [state.movedTo, f.clone]]);
    assert.equal(state.writes.at(-1), f.raw, "the rollback wrote the exact pre-move raw value");
    assert.equal(raw2(), f.raw);
    assert.equal(park2(), f.park);
  });

  it("rollback spread (ledger repair failed): the rewritten journal carries the record byte-identically", async (t) => {
    const f = await fenced();
    const state = revokeAt(t, { clone: f.clone, at: "retired" });
    const seam = cache as unknown as Seam;
    t.mock.method(seam, "rewriteLedgerAtomically", async () => { throw new Error("injected ledger write failure"); });
    const warn = t.mock.method((cache as unknown as { log: { warn: (...a: unknown[]) => void } }).log, "warn", () => {});
    await assert.rejects(cache.retireRunnerClone(bare, f.clone, B2, RUN2, {
      discard: true, attemptId: f.attemptId, processProof: () => state.live,
    }), /process proof revoked/);
    assert.ok(warn.mock.calls.some(c => c.arguments[0] === "retireRunnerClone: process revocation rollback ledger repair failed"),
      "the ledger-repair-failed spread branch ran");
    const restored = JSON.parse(raw2()) as Record<string, unknown>;
    assert.ok(Array.isArray(restored.retainedSources), "the spread rewrote the journal with retained sources");
    assert.notEqual(raw2(), f.raw, "this is the spread, not the raw restore");
    assert.equal(park2(), f.park);
    assert.deepEqual(restored.completionSourceFence, FENCE);
    assert.equal(fs.existsSync(f.clone), true, "the clone was returned");
    assert.equal(fs.existsSync(holdingRoot()) ? fs.readdirSync(holdingRoot()).length : 0, 0);
  });

  it("interrupted rollback (the clone cannot be returned): the residue-retaining journal carries the record", async (t) => {
    const f = await fenced();
    const state = revokeAt(t, { clone: f.clone, failedRestore: true });
    await assert.rejects(cache.retireRunnerClone(bare, f.clone, B2, RUN2, {
      discard: true, attemptId: f.attemptId, processProof: () => state.live,
    }), /process proof revoked/);
    // The interrupted-rollback branch ran: the restore rename was denied, so the journal now names the holding path.
    assert.deepEqual(state.moves, [[f.clone, state.movedTo]]);
    const held = JSON.parse(raw2()) as RecoveryJournalEntry;
    assert.equal(held.clonePath, state.movedTo);
    assert.equal(held.attemptId, undefined);
    assert.ok(held.retainedSources?.some(s => s.clonePath === state.movedTo));
    assert.equal(park2(), f.park);
    assert.deepEqual(held.completionSourceFence, FENCE);
  });
});

describe("a journal carrying a settled record is not accepted by the base parser (older-worker rollback fence)", () => {
  it("the unknown key invalidates the whole journal for a parser that predates it", async () => {
    const { parseRecoveryJournal: baseParse } = await import("./fixtures/base-parse-recovery-journal.js");
    assert.doesNotThrow(() => baseParse(rawJournal()));
    await cache.recordMemoryPark(bare, BRANCH, expected(), unsettled());
    assert.throws(() => baseParse(rawJournal()), /invalid retained recovery clone journal/);
  });
});

// Issue #3644 Gate B: boundary record, compose cross-check, restore predicate, attribution census.
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import * as fsp from "node:fs/promises";
import * as os from "node:os";
import * as path from "node:path";
import { afterEach, beforeEach, describe, it } from "node:test";
import { restoreJournalJson } from "../src/git.js";
import { formatAttemptId } from "../src/attempt-path.js";
import { restoreDigest, restoreTombstoneJson, type RestoreBasis } from "../src/restore-journal.js";
import type { PendingFootprint, SlotPublication, SlotSnapshot } from "../src/completion-authority.js";
import {
  type BoundaryFsOps, type BoundaryRecord, type CensusResult, type LiveBoundaryIdentity, type RestoreEvalInput,
  boundaryFileName, enumerateAttributedPaths, establishProcessBoundary, evaluateComposeRestore, processBoundary,
  processBoundaryDisabledReason, readBoundaryRecord, readLiveIdentity, realBoundaryOps, resetProcessBoundaryForTests,
  runAttributionCensus, sealBoundaryRecord, unsealBoundaryRecord, writeBoundaryRecord,
} from "../src/restore-boundary.js";

const TOKEN = "worker-token-for-boundary-tests";
const CID = "ab".repeat(32);
const BOOT = "11111111-2222-3333-4444-555555555555";
const UUID = "11111111-1111-4111-8111-111111111111";
let root: string;
let proc: string;
let dataDir: string;

interface ProcOpts { cid?: string | null; boot?: string; ticks?: string; timens?: string | null; mountRoot?: string }
async function makeProc(dir: string, o: ProcOpts = {}): Promise<void> {
  await fsp.mkdir(path.join(dir, "self/ns"), { recursive: true });
  await fsp.mkdir(path.join(dir, "1"), { recursive: true });
  await fsp.mkdir(path.join(dir, "sys/kernel/random"), { recursive: true });
  await fsp.writeFile(path.join(dir, "self/ns/pid"), "");
  const ticks = o.ticks ?? "1000";
  await fsp.writeFile(path.join(dir, "1/stat"), `1 (init (x)) S 0 1 1 0 -1 4194560 1 0 0 0 0 0 0 0 20 0 1 0 ${ticks} 1 1 18446744073709551615\n`);
  await fsp.writeFile(path.join(dir, "sys/kernel/random/boot_id"), `${o.boot ?? BOOT}\n`);
  if (o.timens !== null) await fsp.writeFile(path.join(dir, "self/timens_offsets"), o.timens ?? "monotonic           0         0\nboottime            0         0\n");
  if (o.cid !== null) {
    const mroot = o.mountRoot ?? `/var/lib/docker/containers/${o.cid ?? CID}/hostname`;
    await fsp.writeFile(path.join(dir, "self/mountinfo"),
      `20 10 0:30 / / rw - overlay overlay rw\n31 20 8:1 ${mroot} /etc/hostname rw,relatime - ext4 /dev/sda1 rw\n`);
  }
}

beforeEach(async () => {
  root = await fsp.mkdtemp(path.join(os.tmpdir(), "restore-boundary-"));
  proc = path.join(root, "proc");
  root = await fsp.realpath(root);
  proc = path.join(root, "proc");
  dataDir = path.join(root, "data");
  await fsp.mkdir(dataDir);
  await fsp.mkdir(path.join(dataDir, ".uzi-authority"), { mode: 0o700 });
  await makeProc(proc);
  resetProcessBoundaryForTests();
});
afterEach(async () => { await fsp.rm(root, { recursive: true, force: true }); });

const host = (): string => CID.slice(0, 12);
const base = (extra: object = {}) => ({ dataDir, workerToken: TOKEN, procRoot: proc, hostname: host, incarnation: "inc-S", ...extra });

describe("live identity and cross-check", () => {
  it("passes for a matching hostname bind", async () => {
    const r = await readLiveIdentity({ procRoot: proc, hostname: host });
    assert.ok(r.ok);
    assert.deepEqual(r.identity.slot, { kind: "compose", hostname: host(), containerId: CID });
    assert.equal(r.identity.execution.initStartTicks, "1000");
    assert.equal(r.identity.execution.bootId, BOOT);
  });
  for (const [name, o, hostname, reason] of [
    ["non-hex id", { cid: "zz".repeat(32) }, "zzzzzzzzzzzz", "cross_check_failed"],
    ["short id", { cid: "ab".repeat(31) }, "abababababab", "cross_check_failed"],
    ["uppercase id", { cid: "AB".repeat(32) }, "ABABABABABAB", "cross_check_failed"],
    ["hostname mismatch", {}, "otherhost123", "cross_check_failed"],
    ["missing row", { cid: null }, host(), "cross_check_failed"],
    ["wrong file name", { mountRoot: `/x/containers/${CID}/resolv.conf` }, host(), "cross_check_failed"],
    ["timens non-zero", { timens: "monotonic 5 0\nboottime 0 0\n" }, host(), "timens_refused"],
    ["timens unreadable", { timens: null }, host(), "timens_refused"],
  ] as const) {
    it(`refuses: ${name}`, async () => {
      await fsp.rm(proc, { recursive: true });
      await makeProc(proc, o as ProcOpts);
      const r = await readLiveIdentity({ procRoot: proc, hostname: () => hostname });
      assert.deepEqual(r, { ok: false, reason });
    });
  }
  it("hosted lane needs no cross-check", async () => {
    await fsp.rm(proc, { recursive: true });
    await makeProc(proc, { cid: null });
    const r = await readLiveIdentity({ procRoot: proc, podUid: "123e4567-e89b-12d3-a456-426614174000", container: "worker" });
    assert.ok(r.ok);
    assert.equal(r.identity.slot.kind, "hosted");
    assert.equal(r.identity.crossCheck, "na");
  });
  it("a non-UUID UZI_POD_UID is a compose candidate", async () => {
    const r = await readLiveIdentity({ procRoot: proc, hostname: host, podUid: "not-a-uuid" });
    assert.ok(r.ok && r.identity.slot.kind === "compose");
  });
});

describe("record write and read", () => {
  it("round-trips and is write-once", async () => {
    const w = await writeBoundaryRecord(base());
    assert.ok(w.ok);
    const r = await readBoundaryRecord({ dataDir, workerToken: TOKEN }, "inc-S");
    assert.deepEqual(r, { state: "ok", record: w.record });
    assert.deepEqual(await writeBoundaryRecord(base()), { ok: false, reason: "exists" });
    const dir = path.join(dataDir, ".uzi-authority/boundaries");
    assert.deepEqual((await fsp.readdir(dir)).sort(), [boundaryFileName("inc-S")]);
    assert.equal((await fsp.stat(dir)).mode & 0o777, 0o700);
  });
  it("a missing file reads as missing; another incarnation is not a fallback", async () => {
    await writeBoundaryRecord(base());
    assert.deepEqual(await readBoundaryRecord({ dataDir, workerToken: TOKEN }, "inc-P"), { state: "missing" });
  });
  describe("the reader refuses an unsafe record file", () => {
    const withRecord = async (): Promise<{ dir: string; file: string; raw: string }> => {
      await writeBoundaryRecord(base());
      const dir = path.join(dataDir, ".uzi-authority/boundaries");
      const file = path.join(dir, boundaryFileName("inc-S"));
      return { dir, file, raw: await fsp.readFile(file, "utf8") };
    };
    const read = () => readBoundaryRecord({ dataDir, workerToken: TOKEN }, "inc-S");
    it("a FIFO at the path refuses promptly instead of blocking", async () => {
      const { file } = await withRecord();
      await fsp.unlink(file);
      execFileSync("mkfifo", [file]);
      const timer = new Promise<string>(resolve => setTimeout(() => resolve("hung"), 5000).unref());
      assert.deepEqual(await Promise.race([read(), timer]), { state: "invalid" });
    });
    it("a group/world-writable record refuses", async () => {
      const { file } = await withRecord();
      await fsp.chmod(file, 0o666);
      assert.deepEqual(await read(), { state: "invalid" });
    });
    it("a record with a second hard link refuses", async () => {
      const { dir, file } = await withRecord();
      await fsp.link(file, path.join(dir, "elsewhere"));
      assert.deepEqual(await read(), { state: "invalid" });
    });
    it("an oversize record refuses", async () => {
      const { file, raw } = await withRecord();
      await fsp.writeFile(file, raw + " ".repeat(9000));
      assert.deepEqual(await read(), { state: "invalid" });
    });
    it("a boundaries directory that is not private refuses", async () => {
      const { dir } = await withRecord();
      await fsp.chmod(dir, 0o777);
      assert.deepEqual(await read(), { state: "invalid" });
    });
  });
  it("rejects a tampered body, a wrong token and a record copied under another name", async () => {
    await writeBoundaryRecord(base());
    const dir = path.join(dataDir, ".uzi-authority/boundaries");
    const file = path.join(dir, boundaryFileName("inc-S"));
    const raw = await fsp.readFile(file, "utf8");
    await fsp.writeFile(file, raw.replace(CID.slice(0, 12), "ffffffffffff"));
    assert.deepEqual(await readBoundaryRecord({ dataDir, workerToken: TOKEN }, "inc-S"), { state: "invalid" });
    await fsp.writeFile(file, raw);
    assert.equal((await readBoundaryRecord({ dataDir, workerToken: "other-token" }, "inc-S")).state, "invalid");
    await fsp.writeFile(path.join(dir, boundaryFileName("inc-X")), raw);
    assert.equal((await readBoundaryRecord({ dataDir, workerToken: TOKEN }, "inc-X")).state, "invalid");
  });
  it("rejects unknown keys and bad shapes", () => {
    const rec: BoundaryRecord = { version: 1, incarnation: "i", slot: { kind: "compose", hostname: "h", containerId: CID }, crossCheck: "ok",
      execution: { nsDev: "1", nsIno: "2", initStartTicks: "3", bootId: BOOT } };
    const sealed = JSON.parse(sealBoundaryRecord(TOKEN, rec));
    assert.ok(unsealBoundaryRecord(TOKEN, JSON.stringify(sealed)));
    assert.equal(unsealBoundaryRecord(TOKEN, JSON.stringify({ ...sealed, extra: 1 })), undefined);
    assert.equal(unsealBoundaryRecord(TOKEN, "not json"), undefined);
    assert.equal(unsealBoundaryRecord(TOKEN, JSON.stringify({ ...sealed, mac: "00" })), undefined);
  });
  it("a refused cross-check writes no record", async () => {
    const w = await writeBoundaryRecord(base({ hostname: () => "otherhost123" }));
    assert.deepEqual(w, { ok: false, reason: "cross_check_failed" });
    await assert.rejects(fsp.readdir(path.join(dataDir, ".uzi-authority/boundaries")));
  });

  describe("durability seams", () => {
    const real = realBoundaryOps();
    const failing = (seam: keyof BoundaryFsOps, nth = 1): { ops: BoundaryFsOps; calls: string[] } => {
      const calls: string[] = [];
      const counts: Record<string, number> = {};
      const wrap = <K extends keyof BoundaryFsOps>(k: K): BoundaryFsOps[K] =>
        (async (...args: unknown[]) => {
          calls.push(`${k}`);
          counts[k] = (counts[k] ?? 0) + 1;
          if (k === seam && counts[k] === nth) throw Object.assign(new Error(`${k} failed`), { code: "EIO" });
          return (real[k] as (...a: unknown[]) => unknown)(...args);
        }) as BoundaryFsOps[K];
      return { calls, ops: { syncDir: wrap("syncDir"), mkdirPrivate: wrap("mkdirPrivate"), writeTmp: wrap("writeTmp"),
        link: wrap("link"), unlink: wrap("unlink"), readFile: wrap("readFile") } };
    };
    it("runs the steps in the documented order", async () => {
      const { ops, calls } = failing("readFile", 99);
      assert.ok((await writeBoundaryRecord(base({ ops }))).ok);
      assert.deepEqual(calls, ["syncDir", "mkdirPrivate", "syncDir", "writeTmp", "link", "unlink", "syncDir", "readFile"]);
    });
    for (const [seam, nth] of [["syncDir", 1], ["mkdirPrivate", 1], ["syncDir", 2], ["writeTmp", 1], ["link", 1], ["unlink", 1], ["syncDir", 3], ["readFile", 1]] as const) {
      it(`fails closed when ${seam} #${nth} fails`, async () => {
        const { ops } = failing(seam, nth);
        assert.deepEqual(await writeBoundaryRecord(base({ ops })), { ok: false, reason: "durability_failed" });
      });
    }
    it("a failed link leaves no tmp file and no record", async () => {
      const { ops } = failing("link");
      await writeBoundaryRecord(base({ ops }));
      assert.deepEqual(await fsp.readdir(path.join(dataDir, ".uzi-authority/boundaries")), []);
    });
    it("a read-back that differs fails", async () => {
      const ops: BoundaryFsOps = { ...real, readFile: async () => "{}" };
      assert.deepEqual(await writeBoundaryRecord(base({ ops })), { ok: false, reason: "durability_failed" });
    });
  });

  it("process state: success records, failure disables", async () => {
    assert.equal(processBoundaryDisabledReason(), "not_attempted");
    const bad = await establishProcessBoundary(base({ hostname: () => "otherhost123" }));
    assert.equal(bad.ok, false);
    assert.equal(processBoundary(), undefined);
    assert.equal(processBoundaryDisabledReason(), "cross_check_failed");
    const ok = await establishProcessBoundary(base());
    assert.ok(ok.ok);
    assert.deepEqual(processBoundary(), ok.record);
    assert.equal(processBoundaryDisabledReason(), undefined);
  });
});

// ---- predicate ------------------------------------------------------------------------------------------------------

const slotS = { kind: "compose", hostname: "abababababab", containerId: CID } as const;
const execP = { nsDev: "4", nsIno: "100", initStartTicks: "500", bootId: BOOT };
const execS = { nsDev: "4", nsIno: "200", initStartTicks: "900", bootId: BOOT };
const rec = (incarnation: string, execution = execS, over: Partial<BoundaryRecord> = {}): BoundaryRecord =>
  ({ version: 1, incarnation, slot: slotS, crossCheck: "ok", execution, ...over });
const slot = (over: Partial<SlotSnapshot> = {}): SlotSnapshot => ({ version: "v", key: "k", state: "active", epoch: "e", seq: 1,
  owner: { worker: "inc-P", executor: null }, pending: null, retiring: null, publication: null, codexMarked: false, ...over });
const clean: CensusResult = { ok: true, paths: [] };

function input(over: Partial<RestoreEvalInput> & { pRecord?: BoundaryRecord | "missing" | "invalid"; sRecord?: BoundaryRecord } = {}): { i: RestoreEvalInput; reads: string[] } {
  const reads: string[] = [];
  const sRec = over.sRecord ?? rec("inc-S");
  const pRec = over.pRecord ?? rec("inc-P", execP);
  const live: LiveBoundaryIdentity = { slot: sRec.slot, crossCheck: sRec.crossCheck === "failed" ? "ok" : sRec.crossCheck, execution: sRec.execution };
  const i: RestoreEvalInput = {
    selfIncarnation: "inc-S", selfRead: { state: "ok", record: sRec }, selfLive: live, predecessor: slot(), predecessorFenced: false,
    readBoundary: async (inc) => { reads.push(inc); return pRec === "missing" ? { state: "missing" } : pRec === "invalid" ? { state: "invalid" } : { state: "ok", record: pRec }; },
    bareBusy: false, bareSlot: null, census: clean, ...over,
  };
  return { i, reads };
}
const decide = async (over: Parameters<typeof input>[0] = {}) => (await evaluateComposeRestore(input(over).i));

describe("evaluateComposeRestore", () => {
  it("admits a distinct older execution in the same compose slot and reads only P's file", async () => {
    const { i, reads } = input();
    const d = await evaluateComposeRestore(i);
    assert.ok(d.admit);
    assert.equal(d.verdict.boot, "same_boot");
    assert.deepEqual(reads, ["inc-P"]);
  });
  it("admits a different boot in the same slot even with higher start ticks", async () => {
    const d = await decide({ pRecord: rec("inc-P", { ...execP, bootId: "99999999-2222-3333-4444-555555555555", initStartTicks: "99999" }) });
    assert.ok(d.admit && d.verdict.boot === "different_boot");
  });
  it("self_record_invalid: missing, invalid, mismatching live identity, other incarnation, no live identity", async () => {
    for (const over of [
      { selfRead: { state: "missing" } as const }, { selfRead: { state: "invalid" } as const }, { selfLive: undefined },
      { sRecord: rec("inc-OTHER") },
      { selfLive: { slot: slotS, crossCheck: "ok", execution: { ...execS, nsIno: "999" } } as LiveBoundaryIdentity },
      { selfLive: { slot: { ...slotS, hostname: "cdcdcdcdcdcd" }, crossCheck: "ok", execution: execS } as LiveBoundaryIdentity },
    ]) assert.deepEqual(await decide(over as never), { admit: false, reason: "self_record_invalid" });
  });
  it("a missing predecessor record refuses and is never treated as clean", async () => {
    assert.deepEqual(await decide({ pRecord: "missing" }), { admit: false, reason: "predecessor_record_missing" });
  });
  it("an invalid predecessor record, or one naming another incarnation, refuses", async () => {
    assert.deepEqual(await decide({ pRecord: "invalid" }), { admit: false, reason: "predecessor_record_invalid" });
    assert.deepEqual(await decide({ pRecord: rec("inc-Q", execP) }), { admit: false, reason: "predecessor_record_invalid" });
  });
  it("hosted slots are not delivered", async () => {
    const hosted = { kind: "hosted", podUid: "u", container: "c" } as const;
    assert.deepEqual(await decide({ pRecord: rec("inc-P", execP, { slot: hosted, crossCheck: "na" }) }), { admit: false, reason: "hosted_restore_not_delivered" });
    assert.deepEqual(await decide({ sRecord: rec("inc-S", execS, { slot: hosted, crossCheck: "na" }), pRecord: rec("inc-P", execP, { slot: hosted, crossCheck: "na" }) }),
      { admit: false, reason: "hosted_restore_not_delivered" });
  });
  it("slot_mismatch refuses (no slot comparison would admit it)", async () => {
    assert.deepEqual(await decide({ pRecord: rec("inc-P", execP, { slot: { ...slotS, containerId: "cd".repeat(32) } }) }), { admit: false, reason: "slot_mismatch" });
    assert.deepEqual(await decide({ pRecord: rec("inc-P", execP, { slot: { ...slotS, hostname: "cdcdcdcdcdcd" } }) }), { admit: false, reason: "slot_mismatch" });
  });
  it("cross_check_failed when P's record is not ok", async () => {
    assert.deepEqual(await decide({ pRecord: rec("inc-P", execP, { crossCheck: "failed" }) }), { admit: false, reason: "cross_check_failed" });
  });
  it("same_execution refuses (no execution comparison would admit it)", async () => {
    assert.deepEqual(await decide({ pRecord: rec("inc-P", execS) }), { admit: false, reason: "same_execution" });
  });
  it("predecessor_not_older refuses a same-boot predecessor that started later or at the same tick", async () => {
    assert.deepEqual(await decide({ pRecord: rec("inc-P", { ...execP, initStartTicks: "900", nsIno: "300" }) }), { admit: false, reason: "predecessor_not_older" });
    assert.deepEqual(await decide({ pRecord: rec("inc-P", { ...execP, initStartTicks: "901" }) }), { admit: false, reason: "predecessor_not_older" });
  });
  it("predecessor_pending for a pending footprint or a publication", async () => {
    const pending: PendingFootprint = { operation: "o", kind: "bare_mutation", footprint: "f", status: "running", residue: [] };
    assert.deepEqual(await decide({ predecessor: slot({ pending }) }), { admit: false, reason: "predecessor_pending" });
    const publication: SlotPublication = { harness: "claude", sourceCoreDigest: "d", cohort: { runId: "r", generation: 1, captureIds: [] } };
    assert.deepEqual(await decide({ predecessor: slot({ publication }) }), { admit: false, reason: "predecessor_pending" });
  });
  it("predecessor_fenced", async () => {
    assert.deepEqual(await decide({ predecessorFenced: true }), { admit: false, reason: "predecessor_fenced" });
  });
  it("bare_busy and bare_slot_foreign_active (even with pending null)", async () => {
    assert.deepEqual(await decide({ bareBusy: true }), { admit: false, reason: "bare_busy" });
    assert.deepEqual(await decide({ bareSlot: slot({ owner: { worker: "inc-Z", executor: null } }) }), { admit: false, reason: "bare_slot_foreign_active" });
    const own = await decide({ bareSlot: slot({ owner: { worker: "inc-S", executor: null } }) });
    assert.ok(own.admit);
  });
  it("census refusals pass through", async () => {
    for (const reason of ["census_foreign_active", "census_pending", "attribution_unverifiable"] as const)
      assert.deepEqual(await decide({ census: { ok: false, reason } }), { admit: false, reason });
  });
});

// ---- census ---------------------------------------------------------------------------------------------------------

const ATT = formatAttemptId(new Date("2026-01-01T00:00:00Z"), 1, "0123456789abcdef");
const P_ATT = formatAttemptId(new Date("2026-01-02T00:00:00Z"), 1, "1123456789abcdef");
const S_ATT = formatAttemptId(new Date("2026-01-03T00:00:00Z"), 2, "2123456789abcdef");
const SHA = "a".repeat(40);
const rbasis = (predecessor: string): RestoreBasis => ({
  version: 1, predecessor: { clonePath: predecessor, attemptId: P_ATT, generation: 1 }, journalDigest: restoreDigest("p"),
  slotOwner: { worker: "worker-incarnation-1", epoch: "epoch-1" }, pin: { captureId: "capture-1", generation: 1, sourceSha: SHA, finalization: true }, restoreTip: SHA,
});
const j = (clonePath: string, extra: object = {}): string => JSON.stringify({ runId: "r1", clonePath, ...extra });
const marker = (clonePath: string): string => JSON.stringify({ version: 1, runId: "r2", generation: 1, branch: "agent/issue-2", key: "k",
  clonePath, journalDigest: "a".repeat(64), reason: "stopped", at: "2026-01-01T00:00:00Z" });
const ledger = (clonePath: string, state = "live"): string => JSON.stringify({ attemptId: ATT, runId: "r1", clonePath, state });
const progress = (source: string, successor: string, run = "r1"): object => ({ version: 1, source: { runId: run, clonePath: source }, successor: { runId: run, clonePath: successor },
  attempts: 1, startedAt: 0, deadline: 300_000, backoffMs: 0, stage: "capturing" });
const retainedKey = (run: string): string => `uzi-retained.${run}.journal`;
const descriptor = (journal: object, extra: object = {}): string => JSON.stringify({ version: 1, branch: "b", key: "k", journal, ...extra });
const PARK_EVIDENCE = { parkAck: { runId: "r1", claimGeneration: 2, workerId: "w", registerNonce: "n", memoryEpisode: 1 }, interventionIds: [], processJoin: "executor",
  commands: "not_required", steering: "joined", auxiliaries: "none", feedback: "drained", sinkReceipts: "joined", parkReap: "not_applicable" };
const parkRecord = (settlementPath: string): object => ({ version: 1, binding: PARK_EVIDENCE.parkAck, ack: "confirmed",
  origin: { flightId: UUID, attemptId: null, executorIncarnation: null, authorityIncarnation: UUID, sourceEpoch: UUID },
  settlement: { version: 1, confirmed: true, sources: [{ path: settlementPath, authorityKey: "1".padStart(64, "a"), attemptId: null, epoch: UUID, seq: 1 }], evidence: PARK_EVIDENCE } });

describe("attribution census", () => {
  const P = "/data/runner/s/issue-1.attempt-x-g1";
  it("enumerates every attribution family, including nested descriptor sources", () => {
    const pPath = `/a/5.attempt-${P_ATT}`, sPath = `/a/6.attempt-${S_ATT}`;
    const r = enumerateAttributedPaths([
      { key: "uzi-recovery.agent/issue-1.clone", value: j("/a/1", { retainedSources: [{ runId: "r1", clonePath: "/a/2" }, { runId: "r1", clonePath: "/a/4" }], recovery: progress("/a/3", "/a/1") }) },
      { key: "uzi-recovery.agent/issue-9.clone", value: "" },
      { key: "uzi-restore.agent/issue-1.journal", value: restoreJournalJson(rbasis(pPath), "ready", { runId: "r1", clonePath: sPath, attemptId: S_ATT }) },
      { key: "uzi-restore.agent/issue-9.journal", value: "" },
      { key: "uzi-restore.agent/issue-2.journal", value: restoreTombstoneJson(rbasis(`/a/7.attempt-${P_ATT}`), { runId: "r", attemptId: S_ATT, clonePath: `/a/8.attempt-${S_ATT}` }) },
      { key: "uzi-attempts.agent/issue-1.entry", value: ledger("/a/9", "retired") },
      { key: "uzi-stopped.r2.entry", value: marker("/a/10") },
      { key: retainedKey("r3"), value: descriptor({ runId: "r3", clonePath: "/a/11", retainedSources: [{ runId: "r3", clonePath: "/a/12" }], recovery: progress("/a/13", "/a/11", "r3") }) },
      { key: "uzi-attemptmode.k.enabled", value: "true" }, { key: "core.bare", value: "true" },
    ], true);
    assert.ok(r.ok);
    assert.deepEqual(r.paths, ["/a/1", "/a/10", "/a/11", "/a/12", "/a/13", "/a/2", "/a/3", "/a/4", `/a/5.attempt-${P_ATT}`, `/a/6.attempt-${S_ATT}`, `/a/7.attempt-${P_ATT}`, `/a/8.attempt-${S_ATT}`, "/a/9"]);
  });
  it("a cleared recovery journal (the empty string every retirement writes) is no paths, not an unverifiable value", () => {
    assert.deepEqual(enumerateAttributedPaths([{ key: "uzi-recovery.agent/issue-1.clone", value: "" }], true), { ok: true, paths: [] });
  });
  it("enumerates a memory park settlement's source paths", () => {
    const r = enumerateAttributedPaths([{ key: "uzi-recovery.b.clone", value: j("/a/1", { memoryPark: parkRecord("/a/14") }) }], true);
    assert.deepEqual(r, { ok: true, paths: ["/a/1", "/a/14"] });
    assert.deepEqual(enumerateAttributedPaths([{ key: "uzi-recovery.b.clone", value: j("/a/1", { memoryPark: parkRecord("rel/path") }) }], true), { ok: false, reason: "attribution_unverifiable" });
  });
  const unverifiable: Array<[string, Array<{ key: string; value: string }>, boolean?]> = [
    ["unknown key under a prefix", [{ key: "uzi-recovery.b.other", value: "{}" }]],
    ["unparseable journal", [{ key: "uzi-recovery.b.clone", value: "{not json" }]],
    ["relative path", [{ key: "uzi-recovery.b.clone", value: j("rel/path") }]],
    ["non-normalized path", [{ key: "uzi-recovery.b.clone", value: j("/a/../b") }]],
    ["bad retained source", [{ key: "uzi-recovery.b.clone", value: j("/a", { retainedSources: [{ clonePath: 5 }] }) }]],
    ["journal with an unknown key and a non-string run id", [{ key: "uzi-recovery.b.clone", value: '{"runId":7,"clonePath":"/a","bogus":1}' }]],
    ["journal with an unknown key only", [{ key: "uzi-recovery.b.clone", value: j("/a", { bogus: 1 }) }]],
    ["ledger with a numeric attempt id and an empty run id", [{ key: "uzi-attempts.b.entry", value: '{"attemptId":5,"runId":"","clonePath":"/a","state":"live"}' }]],
    ["ledger with a lax attempt id", [{ key: "uzi-attempts.b.entry", value: JSON.stringify({ attemptId: "issue-1.attempt-x", runId: "r1", clonePath: "/a", state: "live" }) }]],
    ["stopped marker for another run", [{ key: "uzi-stopped.other.entry", value: marker("/a") }]],
    ["descriptor whose journal names another run", [{ key: retainedKey("r9"), value: descriptor({ runId: "r3", clonePath: "/a", retainedSources: [{ runId: "r3", clonePath: "/b" }] }) }]],
    ["bad ledger state", [{ key: "uzi-attempts.b.entry", value: ledger("/a", "weird") }]],
    ["stopped marker with extra key", [{ key: "uzi-stopped.r.entry", value: JSON.stringify({ ...JSON.parse(marker("/a")), extra: 1 }) }]],
    ["descriptor with extra key", [{ key: retainedKey("r"), value: descriptor({ runId: "r", clonePath: "/a", retainedSources: [{ runId: "r", clonePath: "/b" }] }, { x: 1 }) }]],
    ["unknown restore stage", [{ key: "uzi-restore.b.journal", value: JSON.stringify({ restore: { predecessor: { clonePath: "/a" } }, restoreStage: "weird" }) }]],
    ["restore without predecessor", [{ key: "uzi-restore.b.journal", value: JSON.stringify({ restore: {}, restoreStage: "ready", runId: "r", clonePath: "/a" }) }]],
    ["restore entry with a loose remainder", [{ key: "uzi-restore.b.journal", value: JSON.stringify({ restore: rbasis(`/a/5.attempt-${P_ATT}`), restoreStage: "ready", runId: "r", clonePath: "/a/6" }) }]],
    ["incomplete read", [{ key: "uzi-recovery.b.clone", value: j("/a") }], false],
  ];
  for (const [name, pairs, complete] of unverifiable) {
    it(`attribution_unverifiable: ${name}`, () => {
      assert.deepEqual(enumerateAttributedPaths(pairs, complete ?? true), { ok: false, reason: "attribution_unverifiable" });
    });
  }

  const run = (pairs: Array<{ key: string; value: string }>, slots: Record<string, SlotSnapshot | null | "throw">, peeked: string[] = []) =>
    runAttributionCensus({ pairs, complete: true, selfIncarnation: "inc-S", predecessorClonePath: P,
      peekSlot: async (p) => { peeked.push(p); const s = slots[p] ?? null; if (s === "throw") throw new Error("x"); return s; } });

  it("a stopped-canonical source of another run that is foreign-active refuses", async () => {
    assert.deepEqual(await run([{ key: "uzi-stopped.r2.entry", value: marker("/b/canon") }], { "/b/canon": slot({ owner: { worker: "inc-Z", executor: null } }) }),
      { ok: false, reason: "census_foreign_active" });
  });
  it("a nested protected-descriptor source that is foreign-active refuses", async () => {
    const d = descriptor({ runId: "r3", clonePath: "/b/outer", retainedSources: [{ runId: "r3", clonePath: "/b/nested" }] });
    assert.deepEqual(await run([{ key: retainedKey("r3"), value: d }], { "/b/nested": slot({ owner: { worker: "inc-Z", executor: null } }) }),
      { ok: false, reason: "census_foreign_active" });
  });
  it("a non-null pending footprint refuses even when not foreign", async () => {
    const pending: PendingFootprint = { operation: "o", kind: "destructive", footprint: "f", status: "settled_failed", residue: [] };
    assert.deepEqual(await run([{ key: "uzi-attempts.b.entry", value: ledger("/b/x") }], { "/b/x": slot({ state: "retired", pending }) }),
      { ok: false, reason: "census_pending" });
  });
  it("an unreadable slot is attribution_unverifiable, not clean", async () => {
    assert.deepEqual(await run([{ key: "uzi-attempts.b.entry", value: ledger("/b/x") }], { "/b/x": "throw" }), { ok: false, reason: "attribution_unverifiable" });
  });
  it("P's own clone is excused and never peeked; own-incarnation and retired slots pass", async () => {
    const peeked: string[] = [];
    const r = await run([
      { key: "uzi-recovery.b.clone", value: j(P) }, { key: "uzi-attempts.b.entry", value: ledger("/b/own") }, { key: "uzi-attempts.b.entry", value: ledger("/b/ret", "retired") },
    ], { [P]: slot(), "/b/own": slot({ owner: { worker: "inc-S", executor: null } }), "/b/ret": slot({ state: "retired" }) }, peeked);
    assert.ok(r.ok);
    assert.deepEqual(peeked.sort(), ["/b/own", "/b/ret"]);
  });
  it("only peeks slots: the census never touches the source trees", async () => {
    // The named paths do not exist on disk; a stat/readdir of them would be the only way to fail here.
    const r = await run([{ key: "uzi-recovery.b.clone", value: j("/nonexistent/clone/tree", { retainedSources: [{ runId: "r1", clonePath: "/nonexistent/other" }] }) }], {});
    assert.ok(r.ok);
  });
});

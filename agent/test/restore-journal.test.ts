import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { parseRestoreJournal, parseRestoreTombstone, restoreJournalJson, RestoreCompositeBlockedError, RestoreCompositeError, type MemoryParkRecord } from "../src/git.js";
import { parseRestoreBasis, restoreDigest, restoreJournalKey, restoreKeyBranch, restoreTombstoneJson, type RestoreBasis } from "../src/restore-journal.js";
import { formatAttemptId } from "../src/attempt-path.js";
import { serializeSourceFence, type CompletionSourceFence } from "../src/completion-source.js";

// issue #3644 Gate B (plan 3.5): the strict parsers and serializers of the restore journal layout. Pure: no Git, no filesystem.

const RUN = "run-3644";
const P_ID = formatAttemptId(new Date("2026-11-05T10:10:10Z"), 1, "0123456789abcdef");
const S_ID = formatAttemptId(new Date("2026-11-05T11:11:11Z"), 2, "fedcba9876543210");
const P_PATH = `/data/runner/repo.git/issue-3644.attempt-${P_ID}`;
const S_PATH = `/data/runner/repo.git/issue-3644.attempt-${S_ID}`;
const SHA = "a".repeat(40);
const FENCE: CompletionSourceFence = { version: 1, sourceUseEpoch: "0123abcd-0123-4abc-8def-0123456789ab", sourceCoreDigest: "a".repeat(64) };
const UUID = "11111111-1111-4111-8111-111111111111";
const PARK: MemoryParkRecord = {
  version: 1, binding: { runId: RUN, claimGeneration: 2, workerId: "worker-1", registerNonce: "nonce-1", memoryEpisode: 1 },
  ack: "sent_unconfirmed", origin: { flightId: UUID, attemptId: null, executorIncarnation: null, authorityIncarnation: UUID, sourceEpoch: UUID },
};

const pRaw = JSON.stringify({ runId: RUN, clonePath: P_PATH, attemptId: P_ID });
const basis = (): RestoreBasis => ({
  version: 1, predecessor: { clonePath: P_PATH, attemptId: P_ID, generation: 1, root: { dev: "2049", ino: "131072" } }, journalDigest: restoreDigest(pRaw), boundary: { predecessor: "c".repeat(64), successor: "d".repeat(64) },
  slotOwner: { worker: "worker-incarnation-1", epoch: "epoch-1" },
  pin: { captureId: "capture-1", generation: 1, sourceSha: SHA, finalization: true }, restoreTip: SHA,
});
const mutate = (b: RestoreBasis, fn: (b: Record<string, any>) => void): unknown => { const c = structuredClone(b) as unknown as Record<string, any>; fn(c); return c; };
const sEntry = (extra: Record<string, unknown> = {}) => ({ runId: RUN, clonePath: S_PATH, attemptId: S_ID, ...extra });
const value = (stage: unknown, remainder: Record<string, unknown> = sEntry(), b: unknown = basis()): string =>
  JSON.stringify({ restore: b, restoreStage: stage, ...remainder });
const tombstone = (extra: Record<string, unknown> = {}, b: unknown = basis()): string =>
  JSON.stringify({ restore: b, restoreStage: "retired", successor: { runId: RUN, attemptId: S_ID, clonePath: S_PATH }, ...extra });

function rejects(fn: () => unknown, what: string): void {
  assert.throws(fn, (e: unknown) => e instanceof RestoreCompositeError, what);
}

describe("restore key", () => {
  it("is single-valued per branch and parsed back with the documented pattern", () => {
    assert.equal(restoreJournalKey("agent/issue-3644"), "uzi-restore.agent/issue-3644.journal");
    assert.equal(restoreKeyBranch("uzi-restore.agent/issue-3644.journal"), "agent/issue-3644");
    assert.equal(restoreKeyBranch("uzi-restore.a.b.journal"), "a.b");
    for (const key of ["uzi-recovery.a.clone", "uzi-restore.a.entry", "uzi-restore..journal", "x.uzi-restore.a.journal"]) {
      assert.equal(restoreKeyBranch(key), undefined, key);
    }
  });
});

describe("parseRestoreJournal accepts", () => {
  for (const stage of ["seeding", "ready"] as const) {
    it(`a minimal ${stage} entry and reproduces it byte for byte`, () => {
      const raw = value(stage);
      const parsed = parseRestoreJournal(raw);
      assert.equal(parsed.stage, stage);
      assert.deepEqual(parsed.basis, basis());
      assert.deepEqual({ ...parsed.journal }, sEntry());
      assert.equal(restoreJournalJson(parsed.basis, parsed.stage, parsed.journal), raw);
    });
  }

  it("a fence and a memory-park record, emitted in recoveryJournalJson order (park, then the fence last)", () => {
    const raw = value("ready", sEntry({ completionSourceFence: FENCE, memoryPark: PARK }));
    const parsed = parseRestoreJournal(raw);
    assert.deepEqual(parsed.journal.completionSourceFence, FENCE);
    assert.equal(parsed.journal.memoryPark?.ack, "sent_unconfirmed");
    const out = restoreJournalJson(parsed.basis, "ready", parsed.journal);
    assert.deepEqual(Object.keys(JSON.parse(out)), ["restore", "restoreStage", "runId", "clonePath", "attemptId", "memoryPark", "completionSourceFence"]);
    assert.equal(restoreJournalJson(parsed.basis, "ready", parseRestoreJournal(out).journal), out);
    assert.ok(out.endsWith(`"completionSourceFence":${serializeSourceFence(FENCE)}}`));
  });

  it("serializes the envelope keys in the documented order whatever order the basis was built in", () => {
    const shuffled = { restoreTip: SHA, pin: { finalization: true, sourceSha: SHA, generation: 1, captureId: "capture-1" }, slotOwner: { epoch: "epoch-1", worker: "worker-incarnation-1" },
      journalDigest: restoreDigest(pRaw), predecessor: { root: { ino: "131072", dev: "2049" }, generation: 1, attemptId: P_ID, clonePath: P_PATH }, boundary: { successor: "d".repeat(64), predecessor: "c".repeat(64) }, version: 1 as const };
    const out = restoreJournalJson(shuffled, "seeding", { runId: RUN, clonePath: S_PATH, attemptId: S_ID });
    assert.deepEqual(Object.keys(JSON.parse(out)), ["restore", "restoreStage", "runId", "clonePath", "attemptId"]);
    assert.deepEqual(Object.keys(JSON.parse(out).restore), ["version", "predecessor", "journalDigest", "boundary", "slotOwner", "pin", "restoreTip"]);
    assert.deepEqual(Object.keys(JSON.parse(out).restore.predecessor), ["clonePath", "attemptId", "generation", "root"]);
    assert.deepEqual(Object.keys(JSON.parse(out).restore.predecessor.root), ["dev", "ino"]);
    assert.deepEqual(Object.keys(JSON.parse(out).restore.boundary), ["predecessor", "successor"]);
    assert.deepEqual(Object.keys(JSON.parse(out).restore.pin), ["captureId", "generation", "sourceSha", "finalization"]);
  });
});

describe("parseRestoreJournal rejects", () => {
  it("every remainder key the successor's plain journal cannot carry", () => {
    const forbidden: Array<[string, unknown]> = [
      ["recovery", { version: 1, source: { runId: RUN, clonePath: S_PATH, attemptId: S_ID }, attempts: 1, startedAt: 0, deadline: 300000, backoffMs: 0, stage: "capturing" }],
      ["retainedSources", [{ runId: RUN, clonePath: P_PATH, attemptId: P_ID }]],
      ["peeledMarkers", [SHA]],
      ["remediationRecovery", {}],
      ["restoreTip", SHA],
      ["unknown", 1],
      ["restore2", {}],
    ];
    for (const [key, v] of forbidden) rejects(() => parseRestoreJournal(value("ready", sEntry({ [key]: v }))), key);
  });

  it("a missing attempt id, an invalid remainder and an attempt id the path does not carry", () => {
    rejects(() => parseRestoreJournal(value("ready", { runId: RUN, clonePath: S_PATH })), "no attemptId");
    rejects(() => parseRestoreJournal(value("ready", sEntry({ clonePath: "relative/path" }))), "relative path");
    rejects(() => parseRestoreJournal(value("ready", sEntry({ clonePath: P_PATH }))), "attempt id vs path");
    rejects(() => parseRestoreJournal(value("ready", sEntry({ runId: "bad run" }))), "run id");
    rejects(() => parseRestoreJournal(value("ready", sEntry({ completionSourceFence: { version: 1 } }))), "fence");
    rejects(() => parseRestoreJournal(value("ready", sEntry({ memoryPark: { version: 2 } }))), "memory park");
  });

  it("a wrong, missing or tombstone stage", () => {
    for (const stage of ["retired", "Ready", "", "done", null, 1, undefined]) {
      rejects(() => parseRestoreJournal(value(stage)), `stage ${String(stage)}`);
    }
    rejects(() => parseRestoreJournal(tombstone()), "a tombstone is not an entry");
  });

  it("anything that is not one JSON object", () => {
    for (const raw of ["", "{", "[]", "null", "\"x\"", "1"]) rejects(() => parseRestoreJournal(raw), raw);
  });
});

describe("the restore basis is validated strictly", () => {
  const bad: Array<[string, (b: Record<string, any>) => void]> = [
    ["version 2", b => { b.version = 2; }],
    ["no version", b => { delete b.version; }],
    ["extra basis key", b => { b.extra = 1; }],
    ["no predecessor", b => { delete b.predecessor; }],
    ["extra predecessor key", b => { b.predecessor.extra = 1; }],
    ["relative-empty predecessor path", b => { b.predecessor.clonePath = ""; }],
    ["predecessor attempt id malformed", b => { b.predecessor.attemptId = "nope"; }],
    ["predecessor generation not its attempt's", b => { b.predecessor.generation = 2; }],
    ["predecessor generation zero", b => { b.predecessor.generation = 0; }],
    ["predecessor generation string", b => { b.predecessor.generation = "1"; }],
    ["digest too short", b => { b.journalDigest = "ab"; }],
    ["digest upper case", b => { b.journalDigest = "A".repeat(64); }],
    ["digest missing", b => { delete b.journalDigest; }],
    ["root missing", b => { delete b.predecessor.root; }],
    ["root dev not digits", b => { b.predecessor.root.dev = "0x1"; }],
    ["root ino number", b => { b.predecessor.root.ino = 5; }],
    ["root ino too long", b => { b.predecessor.root.ino = "1".repeat(31); }],
    ["root ino empty", b => { b.predecessor.root.ino = ""; }],
    ["extra root key", b => { b.predecessor.root.extra = "1"; }],
    ["boundary missing", b => { delete b.boundary; }],
    ["boundary predecessor short", b => { b.boundary.predecessor = "ab"; }],
    ["boundary successor upper case", b => { b.boundary.successor = "D".repeat(64); }],
    ["extra boundary key", b => { b.boundary.extra = "c".repeat(64); }],
    ["slot owner worker empty", b => { b.slotOwner.worker = ""; }],
    ["slot owner epoch number", b => { b.slotOwner.epoch = 1; }],
    ["extra slot owner key", b => { b.slotOwner.extra = 1; }],
    ["pin generation differs from the predecessor's", b => { b.pin.generation = 2; }],
    ["pin sha short", b => { b.pin.sourceSha = "abc"; }],
    ["pin sha upper case", b => { b.pin.sourceSha = "A".repeat(40); }],
    ["pin finalization not boolean", b => { b.pin.finalization = "true"; }],
    ["pin capture id empty", b => { b.pin.captureId = ""; }],
    ["extra pin key", b => { b.pin.extra = 1; }],
    ["restore tip malformed", b => { b.restoreTip = "zz"; }],
    ["restore tip missing", b => { delete b.restoreTip; }],
  ];
  for (const [name, fn] of bad) {
    it(`rejects ${name} in an entry, a tombstone and on its own`, () => {
      const b = mutate(basis(), fn);
      rejects(() => parseRestoreJournal(value("ready", sEntry(), b)), name);
      rejects(() => parseRestoreTombstone(tombstone({}, b)), name);
      rejects(() => parseRestoreBasis(b), name);
    });
  }
  it("accepts the unmutated basis everywhere (the controls for the rows above)", () => {
    assert.doesNotThrow(() => parseRestoreJournal(value("ready")));
    assert.doesNotThrow(() => parseRestoreTombstone(tombstone()));
    assert.deepEqual(parseRestoreBasis(basis()), basis());
    const unfinalized = { ...basis(), pin: { ...basis().pin, finalization: false } };
    assert.equal(parseRestoreBasis(unfinalized).pin.finalization, false);
  });
});

describe("parseRestoreTombstone", () => {
  it("accepts exactly {restore, restoreStage:retired, successor} and reproduces it", () => {
    const parsed = parseRestoreTombstone(tombstone());
    assert.deepEqual(parsed.successor, { runId: RUN, attemptId: S_ID, clonePath: S_PATH });
    assert.equal(restoreTombstoneJson(parsed.basis, parsed.successor), tombstone());
  });

  it("rejects a fence, a source or any other key, and a changed shape", () => {
    rejects(() => parseRestoreTombstone(tombstone({ completionSourceFence: FENCE })), "fence");
    rejects(() => parseRestoreTombstone(tombstone({ retainedSources: [] })), "sources");
    rejects(() => parseRestoreTombstone(tombstone({ runId: RUN })), "run id");
    rejects(() => parseRestoreTombstone(JSON.stringify({ restore: basis(), restoreStage: "retired" })), "no successor");
    rejects(() => parseRestoreTombstone(JSON.stringify({ restore: basis(), restoreStage: "retired", successor: { runId: RUN, attemptId: S_ID } })), "short successor");
    rejects(() => parseRestoreTombstone(JSON.stringify({ restore: basis(), restoreStage: "retired", successor: { runId: RUN, attemptId: "x", clonePath: S_PATH } })), "attempt id");
    rejects(() => parseRestoreTombstone(JSON.stringify({ restore: basis(), restoreStage: "retired", successor: { runId: RUN, attemptId: S_ID, clonePath: S_PATH, x: 1 } })), "extra successor key");
    for (const stage of ["ready", "seeding", "", undefined]) {
      rejects(() => parseRestoreTombstone(JSON.stringify({ restore: basis(), restoreStage: stage, successor: { runId: RUN, attemptId: S_ID, clonePath: S_PATH } })), String(stage));
    }
    rejects(() => parseRestoreTombstone(value("ready")), "an entry is not a tombstone");
    rejects(() => parseRestoreTombstone("not json"), "garbage");
  });
});

describe("restoreDigest and the error classes", () => {
  it("is the sha256 hex of the raw value", () => {
    assert.equal(restoreDigest(pRaw), createHash("sha256").update(pRaw).digest("hex"));
    assert.equal(restoreDigest(""), "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855");
  });

  it("the blocked error is a composite error that names the holding run", () => {
    const e = new RestoreCompositeBlockedError("other run", "run-x");
    assert.ok(e instanceof RestoreCompositeError);
    assert.equal(e.restoreRunId, "run-x");
    assert.equal(e.name, "RestoreCompositeBlockedError");
    assert.equal(new RestoreCompositeError("x").name, "RestoreCompositeError");
  });
});

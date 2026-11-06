// issue #3644 Gate B: the pure part of the independent-restore journal layout (plan 3.5).
//
// A restore leaves the killed predecessor P's `uzi-recovery.<branch>.clone` journal byte-identical and puts the
// successor S's own state at `uzi-restore.<branch>.journal` (single-valued, "" = cleared). This module owns the
// key, the strict envelope/tombstone/basis parsers and serializers, and the two error classes. The remainder of an
// entry (S's runId/clonePath/attemptId/memoryPark?/completionSourceFence?) is validated by git.ts's
// parseRecoveryJournal; nothing here touches the filesystem or Git.
import { createHash } from "node:crypto";
import { ATTEMPT_ID_RE, attemptGeneration } from "./attempt-path.js";

const KEY_RE = /^uzi-restore\.(.+)\.journal$/;
const SHA40_RE = /^[0-9a-f]{40}$/;
const SHA256_RE = /^[0-9a-f]{64}$/;

export function restoreJournalKey(branch: string): string {
  return `uzi-restore.${branch}.journal`;
}

/** The branch a restore key names, or undefined when `key` is not a restore key. */
export function restoreKeyBranch(key: string): string | undefined {
  return KEY_RE.exec(key)?.[1];
}

/** A restore entry exists (or its state is unreadable) and the requested effect would have touched P's journal key
 *  or relied on a state this layout does not allow. Nothing was written. */
export class RestoreCompositeError extends Error {
  constructor(readonly reason: string) {
    super(`restore composite: ${reason}`);
    this.name = "RestoreCompositeError";
  }
}

/** A restore entry of ANOTHER run holds the branch: the caller must stop nonterminally (plan 3.5 "Other runs on the
 *  branch"); nothing was written. */
export class RestoreCompositeBlockedError extends RestoreCompositeError {
  constructor(reason: string, readonly restoreRunId?: string) {
    super(reason);
    this.name = "RestoreCompositeBlockedError";
  }
}

export interface RestoreBasis {
  version: 1;
  predecessor: { clonePath: string; attemptId: string; generation: number };
  journalDigest: string;
  slotOwner: { worker: string; epoch: string };
  pin: { captureId: string; generation: number; sourceSha: string; finalization: boolean };
  restoreTip: string;
}

export type RestoreStage = "seeding" | "ready";

export interface RestoreSuccessor { runId: string; attemptId: string; clonePath: string }

export type RestoreValue =
  | { kind: "entry"; basis: RestoreBasis; stage: RestoreStage; remainder: Record<string, unknown> }
  | { kind: "tombstone"; basis: RestoreBasis; successor: RestoreSuccessor };

/** sha256 hex of a raw journal value: the `journalDigest` of the basis. */
export function restoreDigest(raw: string): string {
  return createHash("sha256").update(raw).digest("hex");
}

function bad(what: string): never {
  throw new RestoreCompositeError(`invalid ${what}`);
}

function obj(v: unknown, keys: readonly string[], what: string): Record<string, unknown> {
  if (typeof v !== "object" || v === null || Array.isArray(v)) return bad(what);
  const o = v as Record<string, unknown>;
  const have = Object.keys(o);
  if (have.length !== keys.length || keys.some(k => !Object.prototype.hasOwnProperty.call(o, k))) return bad(what);
  return o;
}

function str(v: unknown, what: string, max = 4096): string {
  if (typeof v !== "string" || v.length === 0 || v.length > max) return bad(what);
  return v;
}

function gen(v: unknown, what: string): number {
  if (typeof v !== "number" || !Number.isSafeInteger(v) || v < 1) return bad(what);
  return v;
}

function attempt(v: unknown, what: string): string {
  if (typeof v !== "string" || !ATTEMPT_ID_RE.test(v)) return bad(what);
  return v;
}

function successor(v: unknown): RestoreSuccessor {
  const o = obj(v, ["runId", "attemptId", "clonePath"], "restore successor");
  const out = { runId: str(o.runId, "restore successor run", 256), attemptId: attempt(o.attemptId, "restore successor attempt"),
    clonePath: str(o.clonePath, "restore successor path") };
  if (!out.clonePath.endsWith(`.attempt-${out.attemptId}`)) bad("restore successor path");
  return out;
}

/** Strict basis: exact keys, no extras, no coercion. */
export function parseRestoreBasis(v: unknown): RestoreBasis {
  const o = obj(v, ["version", "predecessor", "journalDigest", "slotOwner", "pin", "restoreTip"], "restore basis");
  if (o.version !== 1) bad("restore basis version");
  const p = obj(o.predecessor, ["clonePath", "attemptId", "generation"], "restore predecessor");
  const predecessor = { clonePath: str(p.clonePath, "restore predecessor path"), attemptId: attempt(p.attemptId, "restore predecessor attempt"),
    generation: gen(p.generation, "restore predecessor generation") };
  if (!predecessor.clonePath.endsWith(`.attempt-${predecessor.attemptId}`)) bad("restore predecessor path");
  // P's attempt id must carry g<G> (a `gx` id has no generation and is refused); the basis generation must be exactly that G.
  if (attemptGeneration(predecessor.attemptId) !== predecessor.generation) bad("restore predecessor generation");
  if (typeof o.journalDigest !== "string" || !SHA256_RE.test(o.journalDigest)) bad("restore journal digest");
  const s = obj(o.slotOwner, ["worker", "epoch"], "restore slot owner");
  const slotOwner = { worker: str(s.worker, "restore slot worker", 256), epoch: str(s.epoch, "restore slot epoch", 256) };
  const n = obj(o.pin, ["captureId", "generation", "sourceSha", "finalization"], "restore pin");
  if (typeof n.sourceSha !== "string" || !SHA40_RE.test(n.sourceSha) || typeof n.finalization !== "boolean") bad("restore pin");
  const pin = { captureId: str(n.captureId, "restore pin capture", 256), generation: gen(n.generation, "restore pin generation"),
    sourceSha: n.sourceSha as string, finalization: n.finalization as boolean };
  if (pin.generation !== predecessor.generation) bad("restore pin generation");
  if (typeof o.restoreTip !== "string" || !SHA40_RE.test(o.restoreTip)) bad("restore tip");
  return { version: 1, predecessor, journalDigest: o.journalDigest as string, slotOwner, pin, restoreTip: o.restoreTip as string };
}

/** Fixed key order of the basis: the persisted spelling. */
function basisOrdered(b: RestoreBasis): RestoreBasis {
  return { version: 1,
    predecessor: { clonePath: b.predecessor.clonePath, attemptId: b.predecessor.attemptId, generation: b.predecessor.generation },
    journalDigest: b.journalDigest, slotOwner: { worker: b.slotOwner.worker, epoch: b.slotOwner.epoch },
    pin: { captureId: b.pin.captureId, generation: b.pin.generation, sourceSha: b.pin.sourceSha, finalization: b.pin.finalization },
    restoreTip: b.restoreTip };
}

/**
 * Strictly decode one `uzi-restore.<branch>.journal` value (non-empty). A `seeding`/`ready` entry returns the
 * remainder object (every key but `restore`/`restoreStage`) for the caller to run through parseRecoveryJournal; the
 * remainder's own key set is checked by the caller (parseRestoreJournal in git.ts). A `retired` value must be exactly
 * the tombstone shape. Anything else throws {@link RestoreCompositeError}.
 */
export function parseRestoreValue(value: string): RestoreValue {
  let parsed: unknown;
  try { parsed = JSON.parse(value); } catch { return bad("restore journal JSON"); }
  if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) return bad("restore journal");
  const o = parsed as Record<string, unknown>;
  const basis = parseRestoreBasis(o.restore);
  if (o.restoreStage === "retired") {
    const t = obj(o, ["restore", "restoreStage", "successor"], "restore tombstone");
    return { kind: "tombstone", basis, successor: successor(t.successor) };
  }
  if (o.restoreStage !== "seeding" && o.restoreStage !== "ready") return bad("restore stage");
  const { restore: _restore, restoreStage: _stage, ...remainder } = o;
  void _restore; void _stage;
  return { kind: "entry", basis, stage: o.restoreStage, remainder };
}

/** `{"restore":B,"restoreStage":stage,` followed by the remainder journal's own JSON (recoveryJournalJson order). */
export function restoreEnvelopeJson(basis: RestoreBasis, stage: RestoreStage, remainderJson: string): string {
  if (!remainderJson.startsWith("{") || remainderJson.length < 3) throw new RestoreCompositeError("invalid restore remainder");
  const head = JSON.stringify({ restore: basisOrdered(parseRestoreBasis(basis)), restoreStage: stage });
  return `${head.slice(0, -1)},${remainderJson.slice(1)}`;
}

export function restoreTombstoneJson(basis: RestoreBasis, succ: RestoreSuccessor): string {
  const s = successor(succ);
  return JSON.stringify({ restore: basisOrdered(parseRestoreBasis(basis)), restoreStage: "retired",
    successor: { runId: s.runId, attemptId: s.attemptId, clonePath: s.clonePath } });
}

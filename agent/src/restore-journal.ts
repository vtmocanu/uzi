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
  /** `root` is the decimal dev/ino of P's clone root (bigint-safe strings), as the authority's slot-key derivation lstat'd it. */
  predecessor: { clonePath: string; attemptId: string; generation: number; root: { dev: string; ino: string } };
  journalDigest: string;
  /** Lowercase sha256 hex of the RAW bytes of P's and S's boundary record files. */
  boundary: { predecessor: string; successor: string };
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

/** A decimal digit string of up to 30 characters (a stat dev/ino rendered bigint-safe). */
function digits(v: unknown, what: string): string {
  if (typeof v !== "string" || !/^[0-9]{1,30}$/.test(v)) return bad(what);
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
  const o = obj(v, ["version", "predecessor", "journalDigest", "boundary", "slotOwner", "pin", "restoreTip"], "restore basis");
  if (o.version !== 1) bad("restore basis version");
  const p = obj(o.predecessor, ["clonePath", "attemptId", "generation", "root"], "restore predecessor");
  const r = obj(p.root, ["dev", "ino"], "restore predecessor root");
  const predecessor = { clonePath: str(p.clonePath, "restore predecessor path"), attemptId: attempt(p.attemptId, "restore predecessor attempt"),
    generation: gen(p.generation, "restore predecessor generation"), root: { dev: digits(r.dev, "restore root dev"), ino: digits(r.ino, "restore root ino") } };
  const bd = obj(o.boundary, ["predecessor", "successor"], "restore boundary");
  if (typeof bd.predecessor !== "string" || !SHA256_RE.test(bd.predecessor) || typeof bd.successor !== "string" || !SHA256_RE.test(bd.successor)) bad("restore boundary");
  const boundary = { predecessor: bd.predecessor as string, successor: bd.successor as string };
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
  return { version: 1, predecessor, journalDigest: o.journalDigest as string, boundary, slotOwner, pin, restoreTip: o.restoreTip as string };
}

/** Fixed key order of the basis: the persisted spelling. */
function basisOrdered(b: RestoreBasis): RestoreBasis {
  return { version: 1,
    predecessor: { clonePath: b.predecessor.clonePath, attemptId: b.predecessor.attemptId, generation: b.predecessor.generation,
      root: { dev: b.predecessor.root.dev, ino: b.predecessor.root.ino } },
    journalDigest: b.journalDigest, boundary: { predecessor: b.boundary.predecessor, successor: b.boundary.successor }, slotOwner: { worker: b.slotOwner.worker, epoch: b.slotOwner.epoch },
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

// ---- the rc.2 holding-root grammar and the attempt-id clock (issue #3644 amendment 14) -----------------------------

const HOLDING_UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;

/** The name prefix of the worker-only holding directories rc.2's `retireRunnerClone(discard:false)` creates for `runId`:
 *  the run id with every character outside `[A-Za-z0-9_-]` replaced by `_`, then `-`. Pure; no filesystem access. */
export function holdingNamePrefix(runId: string): string {
  return `${runId.replace(/[^A-Za-z0-9_-]/g, "_")}-`;
}

/** True when `name` is exactly `<holdingNamePrefix(runId)><uuid v4>`, the spelling rc.2 gives a holding directory. */
export function isHoldingName(name: string, runId: string): boolean {
  const prefix = holdingNamePrefix(runId);
  return name.startsWith(prefix) && HOLDING_UUID_RE.test(name.slice(prefix.length));
}

/** The UTC instant (ms) an attempt id encodes (`yyyymmddThhmmssZ-...`), or undefined for anything else. */
export function attemptIdStartMs(attemptId: string): number | undefined {
  const m = /^([0-9]{4})([0-9]{2})([0-9]{2})T([0-9]{2})([0-9]{2})([0-9]{2})Z-/.exec(attemptId);
  if (!m) return undefined;
  const [y, mo, d, h, mi, s] = m.slice(1).map(Number) as [number, number, number, number, number, number];
  const ms = Date.UTC(y, mo - 1, d, h, mi, s);
  return Number.isFinite(ms) ? ms : undefined;
}

// ---- the crash windows of the seed (plan 3.9) --------------------------------------------------------------------

/**
 * What a refused or interrupted seed left behind (plan 3.9), named by the typed error:
 *   none         nothing was written (every refusal before the marker)
 *   marker_only  the retained-home marker may exist; the bare is untouched
 *   ledger_live  S's `live` ledger entry may exist, no restore entry (exit: the plain discard of P retires it)
 *   seeding      the restore entry (`seeding`, or `ready` whose read-back failed) and S's ledger entry exist, maybe a partial S
 */
export type RestoreWindow = "none" | "marker_only" | "ledger_live" | "seeding";

/** A seed that refused or failed. Nothing beyond `window` was written, and `cause` carries the underlying error. */
export class RestoreSeedError extends Error {
  constructor(readonly reason: string, readonly window: RestoreWindow, readonly step: string, cause?: unknown) {
    super(`independent restore seed refused (${reason}) at ${step}; left behind: ${window}`, cause === undefined ? undefined : { cause });
    this.name = "RestoreSeedError";
  }
}

// ---- ref protection while a restore entry exists (plan 3.7) -------------------------------------------------------

/** What the restore entries (and tombstones) of a bare keep alive, per run: every generation up to the predecessor's, and its pin. */
export interface RestoreRefProtection {
  /** A restore value that cannot be decoded names no run: everything is kept. */
  all: boolean;
  runs: Map<string, { generation: number; shas: Set<string> }>;
}

/** Fold the raw values of every `uzi-restore.<branch>.journal` key (empty = cleared) into the protection they imply. */
export function restoreRefProtection(values: readonly string[]): RestoreRefProtection {
  const out: RestoreRefProtection = { all: false, runs: new Map() };
  for (const value of values) {
    if (value === "") continue;
    try {
      const v = parseRestoreValue(value);
      const runId = v.kind === "entry" ? v.remainder.runId : v.successor.runId;
      if (typeof runId !== "string" || runId === "") { out.all = true; continue; }
      const held = out.runs.get(runId) ?? { generation: 0, shas: new Set<string>() };
      held.generation = Math.max(held.generation, v.basis.predecessor.generation);
      held.shas.add(v.basis.pin.sourceSha);
      held.shas.add(v.basis.restoreTip);
      out.runs.set(runId, held);
    } catch { out.all = true; }
  }
  return out;
}

/** True when a deleter must keep a ref of `runId`: a generation at or below the predecessor's, or the pin's SHA. */
export function restoreProtects(p: RestoreRefProtection, runId: string, what: { generation?: number | null; sha?: string }): boolean {
  if (p.all) return true;
  const held = p.runs.get(runId);
  if (!held) return false;
  if (what.sha !== undefined && held.shas.has(what.sha)) return true;
  return what.generation !== undefined && what.generation !== null && what.generation <= held.generation;
}

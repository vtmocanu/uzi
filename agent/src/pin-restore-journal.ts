import { createHash } from "node:crypto";
import path from "node:path";
import { ATTEMPT_ID_RE, attemptGeneration } from "./attempt-path.js";

export interface PinRestoreLink {
  version: 1;
  intentId: string;
  predecessorJournalDigest: string;
  predecessorGeneration: number;
  predecessorEpoch: string;
  successor: {
    runId: string;
    claimGeneration: number;
    workerId: string;
    registerNonce: string;
    authorityIncarnation: string;
    executorIncarnation: string | null;
    sourceEpoch: string;
    attemptId: string;
    clonePath: string;
  };
  restore: {
    captureId: string;
    authenticatedRecordDigest: string;
    predecessorGeneration: number;
    bareDir: string;
    pinRef: string;
    tip: string;
    contentSource: string;
    expectedHead: string;
    expectedTree: string;
  };
}

export interface RetainedPinEnvelope {
  version: 2;
  runId: string;
  branch: string;
  key: string;
  predecessors: {
    journalDigest: string;
    rawJournal: string;
    generation: number;
    sourceEpoch: string;
  }[];
  intent: { version: 1; phase: "prepared" | "ready"; link: PinRestoreLink };
}

export interface PinJournalIdentity {
  runId: string;
  clonePath: string;
  attemptId?: string;
  sourceGeneration?: number;
  pinRestore?: PinRestoreLink;
}

const ID = /^[A-Za-z0-9_-]{1,160}$/;
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;
const HEX64 = /^[0-9a-f]{64}$/;
const SHA40 = /^[0-9a-f]{40}$/;

function fail(): never { throw new Error("invalid pin restore journal"); }
function keys(value: unknown, names: string[]): Record<string, unknown> {
  if (typeof value !== "object" || value === null || Array.isArray(value) ||
      Object.getPrototypeOf(value) !== Object.prototype) return fail();
  const o = value as Record<string, unknown>;
  if (Object.keys(o).length !== names.length || names.some(k => !Object.hasOwn(o, k))) return fail();
  return o;
}
function text(value: unknown, max: number): string {
  if (typeof value !== "string" || value.length === 0 || Buffer.byteLength(value) > max) return fail();
  // Display identities must not carry terminal controls, bidi overrides or invalid Unicode.
  for (const ch of value) {
    const c = ch.codePointAt(0)!;
    if (c < 32 || (c >= 127 && c <= 159) || (c >= 0xd800 && c <= 0xdfff) ||
        c === 0x061c || c === 0x200e || c === 0x200f || (c >= 0x2028 && c <= 0x202e) ||
        (c >= 0x2066 && c <= 0x2069)) return fail();
  }
  return value;
}
function match(value: unknown, re: RegExp): string {
  const clean = text(value, 4096);
  if (!re.test(clean)) return fail();
  return clean;
}
function positive(value: unknown): number {
  if (typeof value !== "number" || !Number.isSafeInteger(value) || value < 1) return fail();
  return value;
}
function absolute(value: unknown): string {
  const p = text(value, 4096);
  if (!path.isAbsolute(p) || path.resolve(p) !== p) return fail();
  return p;
}

/** Strict lexical decoding, with decoded-key uniqueness in every object. */
export function parsePinRestoreJson(raw: string, maxBytes: number): unknown {
  if (typeof raw !== "string" || !Number.isSafeInteger(maxBytes) || maxBytes < 0 ||
      Buffer.byteLength(raw) > maxBytes || Buffer.from(raw, "utf8").toString("utf8") !== raw) return fail();
  const stack: ({ keys: Set<string>; expectingKey: boolean } | null)[] = [];
  // Scan incrementally: generic legacy journals may be much larger than pin envelopes.
  const tokens = /"(?:[^"\\]|\\[\s\S])*"|[{}[\]:,]|[^\s{}[\]:,]+/g;
  for (const token of raw.matchAll(tokens)) {
    const t = token[0];
    const top = stack.at(-1);
    if (t === "{" || t === "[") {
      if (stack.length >= 32) return fail();
      stack.push(t === "{" ? { keys: new Set(), expectingKey: true } : null);
    } else if (t === "}" || t === "]") stack.pop();
    else if (t === "," && top) top.expectingKey = true;
    else if (t.startsWith('"') && top?.expectingKey) {
      const key = JSON.parse(t) as string;
      if (top.keys.has(key)) return fail();
      top.keys.add(key);
      top.expectingKey = false;
    }
  }
  return JSON.parse(raw) as unknown;
}

export function parsePinRestoreLink(value: unknown): PinRestoreLink {
  const o = keys(value, ["version", "intentId", "predecessorJournalDigest", "predecessorGeneration",
    "predecessorEpoch", "successor", "restore"]);
  if (o.version !== 1) return fail();
  const s = keys(o.successor, ["runId", "claimGeneration", "workerId", "registerNonce",
    "authorityIncarnation", "executorIncarnation", "sourceEpoch", "attemptId", "clonePath"]);
  const r = keys(o.restore, ["captureId", "authenticatedRecordDigest", "predecessorGeneration",
    "bareDir", "pinRef", "tip", "contentSource", "expectedHead", "expectedTree"]);
  const predecessorGeneration = positive(o.predecessorGeneration);
  const successor: PinRestoreLink["successor"] = {
    runId: match(s.runId, ID), claimGeneration: positive(s.claimGeneration),
    workerId: text(s.workerId, 128), registerNonce: text(s.registerNonce, 256),
    authorityIncarnation: match(s.authorityIncarnation, UUID),
    executorIncarnation: s.executorIncarnation === null ? null
      : match(s.executorIncarnation, /^[A-Za-z0-9._:-]{1,128}$/),
    sourceEpoch: match(s.sourceEpoch, UUID), attemptId: match(s.attemptId, ATTEMPT_ID_RE),
    clonePath: absolute(s.clonePath),
  };
  if (successor.claimGeneration <= predecessorGeneration ||
      attemptGeneration(successor.attemptId) !== successor.claimGeneration ||
      !successor.clonePath.endsWith(`.attempt-${successor.attemptId}`)) return fail();
  const bareDir = match(r.bareDir, /^[A-Za-z0-9._+-]{1,255}$/);
  if (bareDir === "." || bareDir === "..") return fail();
  const restore: PinRestoreLink["restore"] = {
    captureId: match(r.captureId, ID), authenticatedRecordDigest: match(r.authenticatedRecordDigest, HEX64),
    predecessorGeneration: positive(r.predecessorGeneration), bareDir,
    pinRef: text(r.pinRef, 512), tip: match(r.tip, SHA40),
    contentSource: match(r.contentSource, SHA40), expectedHead: match(r.expectedHead, SHA40),
    expectedTree: match(r.expectedTree, SHA40),
  };
  if (restore.predecessorGeneration !== predecessorGeneration ||
      restore.pinRef !== `refs/uzi-recovery-pin/${successor.runId}/${predecessorGeneration}`) return fail();
  return { version: 1, intentId: match(o.intentId, ID),
    predecessorJournalDigest: match(o.predecessorJournalDigest, HEX64), predecessorGeneration,
    predecessorEpoch: match(o.predecessorEpoch, UUID), successor, restore };
}

/** Share one instance across all descriptors decoded by an operation. Failed decodes also consume bytes. */
export class PinRestoreDecodeBudget {
  decodedBytes = 0;
  consume(bytes: number): void {
    if (!Number.isSafeInteger(bytes) || bytes < 0 ||
        !Number.isSafeInteger(this.decodedBytes) || this.decodedBytes < 0) return fail();
    this.decodedBytes += bytes;
    if (this.decodedBytes > 1048576) return fail();
  }
}

/** The callback validates the already strictly decoded value; it need not decode raw a second time. */
type ParseJournal = (raw: string, value: unknown) => PinJournalIdentity;

function journal(raw: string, parseJournal: ParseJournal, budget: PinRestoreDecodeBudget): PinJournalIdentity {
  const bytes = Buffer.byteLength(raw);
  if (bytes > 16384) return fail();
  budget.consume(bytes);
  const value = parsePinRestoreJson(raw, 16384);
  // An entry is a raw journal, never another history envelope.
  if (typeof value !== "object" || value === null || Array.isArray(value) ||
      Object.hasOwn(value, "predecessors") || Object.hasOwn(value, "intent")) return fail();
  const j = parseJournal(raw, value);
  match(j.runId, ID);
  absolute(j.clonePath);
  const generation = positive(j.sourceGeneration);
  if (j.attemptId !== undefined &&
      (attemptGeneration(match(j.attemptId, ATTEMPT_ID_RE)) !== generation ||
       !j.clonePath.endsWith(`.attempt-${j.attemptId}`))) return fail();
  return j;
}

function tuple(j: PinJournalIdentity, link: PinRestoreLink): void {
  const s = link.successor;
  if (j.runId !== s.runId || j.clonePath !== s.clonePath ||
      j.attemptId !== s.attemptId || j.sourceGeneration !== s.claimGeneration) return fail();
}

export function parseRetainedPinEnvelope(
  raw: string, parseJournal: ParseJournal, budget = new PinRestoreDecodeBudget(),
): RetainedPinEnvelope {
  const o = keys(parsePinRestoreJson(raw, 65536), ["version", "runId", "branch", "key", "predecessors", "intent"]);
  if (o.version !== 2) return fail();
  const runId = match(o.runId, ID);
  const branch = text(o.branch, 256);
  const key = match(o.key, /^[A-Za-z0-9_-]{1,255}$/);
  const intent = keys(o.intent, ["version", "phase", "link"]);
  if (intent.version !== 1 || (intent.phase !== "prepared" && intent.phase !== "ready")) return fail();
  const link = parsePinRestoreLink(intent.link);
  if (link.successor.runId !== runId) return fail();
  if (!Array.isArray(o.predecessors) || o.predecessors.length < 1 || o.predecessors.length > 8) return fail();
  const predecessors: RetainedPinEnvelope["predecessors"] = [];
  const seen = new Map<string, RetainedPinEnvelope["predecessors"][number]>();
  const paths = new Set<string>();
  const attempts = new Set<string>();
  let rawSum = 0;
  // At most eight entries; any invalid entry fails the entire descriptor.
  for (const value of o.predecessors) {
    const e = keys(value, ["journalDigest", "rawJournal", "generation", "sourceEpoch"]);
    if (typeof e.rawJournal !== "string") return fail();
    rawSum += Buffer.byteLength(e.rawJournal);
    if (rawSum > 32768) return fail();
    const j = journal(e.rawJournal, parseJournal, budget);
    const entry = { journalDigest: match(e.journalDigest, HEX64), rawJournal: e.rawJournal,
      generation: positive(e.generation), sourceEpoch: match(e.sourceEpoch, UUID) };
    if (createHash("sha256").update(entry.rawJournal, "utf8").digest("hex") !== entry.journalDigest ||
        j.runId !== runId || j.sourceGeneration !== entry.generation ||
        seen.has(entry.journalDigest) || paths.has(j.clonePath) ||
        (j.attemptId !== undefined && attempts.has(j.attemptId))) return fail();
    const last = predecessors.at(-1);
    if (last !== undefined && (entry.generation <= last.generation || j.pinRestore === undefined)) return fail();
    if (j.pinRestore !== undefined) {
      const previous = parsePinRestoreLink(j.pinRestore);
      const referenced = seen.get(previous.predecessorJournalDigest);
      if (!referenced || referenced.generation !== previous.predecessorGeneration ||
          referenced.sourceEpoch !== previous.predecessorEpoch ||
          previous.successor.sourceEpoch !== entry.sourceEpoch) return fail();
      tuple(j, previous);
    }
    seen.set(entry.journalDigest, entry);
    paths.add(j.clonePath);
    if (j.attemptId !== undefined) attempts.add(j.attemptId);
    predecessors.push(entry);
  }
  const last = predecessors.at(-1)!;
  if (link.predecessorJournalDigest !== last.journalDigest ||
      link.predecessorGeneration !== last.generation || link.predecessorEpoch !== last.sourceEpoch ||
      paths.has(link.successor.clonePath) || attempts.has(link.successor.attemptId)) return fail();
  return { version: 2, runId, branch, key, predecessors,
    intent: { version: 1, phase: intent.phase, link } };
}

export function serializeRetainedPinEnvelope(
  envelope: RetainedPinEnvelope, parseJournal: ParseJournal, budget = new PinRestoreDecodeBudget(),
): string {
  return JSON.stringify(parseRetainedPinEnvelope(JSON.stringify(envelope), parseJournal, budget));
}

/** "absent" describes metadata only; callers must certify retirement before execution. */
export function validateRetainedPinActive(
  envelope: RetainedPinEnvelope, rawActive: string | undefined,
  parseJournal: ParseJournal, budget = new PinRestoreDecodeBudget(),
): "prepared-old" | "prepared-new" | "ready-new" | "absent" {
  return parseRetainedPinSnapshot(JSON.stringify(envelope), rawActive, parseJournal, budget).relation;
}

/** One strict decode per nested source occurrence, sharing the caller’s operation budget. */
export function parseRetainedPinSnapshot(
  raw: string, rawActive: string | undefined, parseJournal: ParseJournal, budget = new PinRestoreDecodeBudget(),
): { envelope: RetainedPinEnvelope; relation: "prepared-old" | "prepared-new" | "ready-new" | "absent" } {
  const e = parseRetainedPinEnvelope(raw, parseJournal, budget);
  if (rawActive === undefined) return { envelope: e, relation: "absent" };
  if (rawActive === e.predecessors.at(-1)!.rawJournal) {
    if (e.intent.phase !== "prepared") return fail();
    return { envelope: e, relation: "prepared-old" };
  }
  const nestedBytes = e.predecessors.reduce((sum, entry) => sum + Buffer.byteLength(entry.rawJournal), 0)
    + Buffer.byteLength(rawActive);
  if (nestedBytes > 32768) return fail();
  const active = journal(rawActive, parseJournal, budget);
  tuple(active, e.intent.link);
  if (active.pinRestore === undefined ||
      JSON.stringify(parsePinRestoreLink(active.pinRestore)) !== JSON.stringify(e.intent.link)) return fail();
  return { envelope: e, relation: e.intent.phase === "prepared" ? "prepared-new" : "ready-new" };
}

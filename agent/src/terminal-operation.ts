/**
 * Terminal-preservation operation record and engine (#4007 slice B, production-dark).
 *
 * The record is the worker's durable, monotonic account of one terminal operation (one run generation): what was
 * captured, what was prepared, and every send attempt. It lives under `<dataDir>/terminal-preservation/<runId>/`,
 * a sibling of the recovery root, so the generic recovery readers never see it. The file IO and MAC live on
 * RecoveryCoordinator; this module owns the schema, canonical bytes, the pure monotonic merge and the engine, whose
 * every effect is injected so the caller wiring is a later, separate change.
 */
import type { CheckpointRange } from "./git.js";
import type { TerminalPreservationRequest } from "./protocol.js";
import type { TerminalPreservationResult } from "./client.js";
import { TERMINAL_SNAPSHOT_SUFFIX, type TerminalCoverageResult, type TerminalUniverse } from "./terminal-inventory.js";
import { parseCheckpointRange, serializeCheckpointRange } from "./terminal-range.js";

export type TerminalManifest = TerminalPreservationRequest["manifest"];
export type TerminalAuthorityClass = "current_flight" | "open_guarded_hold";
export type TerminalPhase = "capturing" | "captured" | "prepared" | "incomplete" | "closed";
export type TerminalInvocationOutcome = "invoked" | "published" | "recorded" | "publish_unknown" | "refused" | "conflict";

export interface TerminalInvocation { at: string; outcome: TerminalInvocationOutcome; reason?: string }

export interface TerminalOperationRecord {
  runId: string;
  workerId: string;
  generation: number;
  authorityClass: TerminalAuthorityClass;
  universeDigest: string;
  manifest?: TerminalManifest;
  tip?: string;
  priorTip?: string;
  /** serializeCheckpointRange output; presence is preserved (absent differs from empty). */
  range?: string;
  scanDigest?: string;
  transportDigest?: string;
  verdicts: Record<string, string>;
  cloneRef: string;
  bareRef: string;
  clonePath: string;
  flaggedCommits: string[];
  phase: TerminalPhase;
  invocations: TerminalInvocation[];
  localBlock: string[];
  closedReason?: string;
}

export const TERMINAL_RECORD_MAX_BYTES = 256 * 1024;
const MAX_INVOCATIONS = 256;
const MAX_BLOCKS = 64;
const MAX_FLAGGED = 256;
const MAX_VERDICTS = 64;
const MAX_STR = 4096;

const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;
const SHA_RE = /^[0-9a-f]{40}$/;
const PHASES: readonly TerminalPhase[] = ["capturing", "captured", "prepared", "incomplete", "closed"];
const OUTCOMES: readonly TerminalInvocationOutcome[] = ["invoked", "published", "recorded", "publish_unknown", "refused", "conflict"];
const PHASE_RANK: Record<TerminalPhase, number> = { capturing: 0, captured: 1, prepared: 2, incomplete: 2, closed: 3 };

export const isTerminalRunId = (v: unknown): v is string => typeof v === "string" && UUID_RE.test(v);
export const isTerminalGeneration = (v: unknown): v is number => typeof v === "number" && Number.isSafeInteger(v) && v >= 1;

/** Key-sorted JSON (undefined omitted) so the MAC bytes never depend on insertion order. */
export function canonicalTerminalJson(value: unknown): string {
  if (value === null || typeof value !== "object") return JSON.stringify(value) ?? "null";
  if (Array.isArray(value)) return `[${value.map(canonicalTerminalJson).join(",")}]`;
  const obj = value as Record<string, unknown>;
  const parts: string[] = [];
  for (const k of Object.keys(obj).sort()) {
    if (obj[k] !== undefined) parts.push(`${JSON.stringify(k)}:${canonicalTerminalJson(obj[k])}`);
  }
  return `{${parts.join(",")}}`;
}

const isObj = (v: unknown): v is Record<string, unknown> => !!v && typeof v === "object" && !Array.isArray(v);
const str = (v: unknown, max = MAX_STR): v is string => typeof v === "string" && v.length > 0 && v.length <= max;
const strArray = (v: unknown, max: number, each: (s: unknown) => boolean): v is string[] =>
  Array.isArray(v) && v.length <= max && v.every(each);
const onlyKeys = (o: Record<string, unknown>, keys: readonly string[]) => Object.keys(o).every(k => keys.includes(k));

function validManifest(v: unknown): v is TerminalManifest {
  if (!isObj(v) || !onlyKeys(v, ["run_id", "generation", "worker_id", "roots", "abandoned"])) return false;
  if (!str(v.run_id) || !isTerminalGeneration(v.generation) || !str(v.worker_id)) return false;
  if (!Array.isArray(v.roots) || v.roots.length > 1024 || !Array.isArray(v.abandoned) || v.abandoned.length > 1024) return false;
  const rootOk = (r: unknown) => isObj(r) && onlyKeys(r, ["source_id", "attempt_id", "commit", "tree"]) &&
    str(r.source_id) && str(r.attempt_id) && str(r.commit) && str(r.tree);
  const abandonedOk = (a: unknown) => isObj(a) && onlyKeys(a, ["class", "source_id", "extents"]) &&
    (a.class === "ignored" || a.class === "cold_dirty") && str(a.source_id) && isObj(a.extents) &&
    onlyKeys(a.extents, ["files", "bytes"]) && Number.isSafeInteger(a.extents.files) && Number.isSafeInteger(a.extents.bytes);
  return v.roots.every(rootOk) && v.abandoned.every(abandonedOk);
}

const RECORD_KEYS = ["runId", "workerId", "generation", "authorityClass", "universeDigest", "manifest", "tip", "priorTip",
  "range", "scanDigest", "transportDigest", "verdicts", "cloneRef", "bareRef", "clonePath", "flaggedCommits", "phase",
  "invocations", "localBlock", "closedReason"] as const;

/** Strict decode of an untrusted object into a record; undefined on any deviation (unknown key, type, bound). */
export function decodeTerminalRecord(raw: unknown): TerminalOperationRecord | undefined {
  if (!isObj(raw) || !onlyKeys(raw, RECORD_KEYS)) return undefined;
  if (!isTerminalRunId(raw.runId) || !str(raw.workerId, 256) || !isTerminalGeneration(raw.generation)) return undefined;
  if (raw.authorityClass !== "current_flight" && raw.authorityClass !== "open_guarded_hold") return undefined;
  if (!str(raw.universeDigest, 256) || !str(raw.cloneRef) || !str(raw.bareRef) || !str(raw.clonePath)) return undefined;
  if (raw.manifest !== undefined && !validManifest(raw.manifest)) return undefined;
  if (raw.tip !== undefined && !(typeof raw.tip === "string" && SHA_RE.test(raw.tip))) return undefined;
  if (raw.priorTip !== undefined && !(typeof raw.priorTip === "string" && SHA_RE.test(raw.priorTip))) return undefined;
  if (raw.range !== undefined) {
    if (typeof raw.range !== "string") return undefined;
    const parsed = parseCheckpointRange(raw.range);
    if (!parsed || serializeCheckpointRange(parsed) !== raw.range) return undefined;
  }
  if (raw.scanDigest !== undefined && !str(raw.scanDigest, 256)) return undefined;
  if (raw.transportDigest !== undefined && !str(raw.transportDigest, 256)) return undefined;
  if (!isObj(raw.verdicts) || Object.keys(raw.verdicts).length > MAX_VERDICTS ||
      !Object.entries(raw.verdicts).every(([k, v]) => k.length > 0 && k.length <= 256 && typeof v === "string" && v.length <= 256)) return undefined;
  if (!strArray(raw.flaggedCommits, MAX_FLAGGED, s => typeof s === "string" && SHA_RE.test(s))) return undefined;
  if (!PHASES.includes(raw.phase as TerminalPhase)) return undefined;
  if (!Array.isArray(raw.invocations) || raw.invocations.length > MAX_INVOCATIONS) return undefined;
  const invocations: TerminalInvocation[] = [];
  for (const i of raw.invocations) {
    if (!isObj(i) || !onlyKeys(i, ["at", "outcome", "reason"]) || !str(i.at, 64) ||
        !OUTCOMES.includes(i.outcome as TerminalInvocationOutcome) ||
        (i.reason !== undefined && !str(i.reason, 256))) return undefined;
    invocations.push({ at: i.at, outcome: i.outcome as TerminalInvocationOutcome, ...(i.reason !== undefined ? { reason: i.reason } : {}) });
  }
  if (!strArray(raw.localBlock, MAX_BLOCKS, s => str(s, 128))) return undefined;
  if (new Set(raw.localBlock).size !== raw.localBlock.length) return undefined;
  if (raw.closedReason !== undefined && !str(raw.closedReason, 256)) return undefined;
  return raw as unknown as TerminalOperationRecord;
}

/** True once an invocation reached a final server outcome. */
const terminalFinalOutcome = (r: Pick<TerminalOperationRecord, "invocations">): "published" | "recorded" | undefined => {
  for (const i of r.invocations) if (i.outcome === "published" || i.outcome === "recorded") return i.outcome;
  return undefined;
};
const terminalInvoked = (r: Pick<TerminalOperationRecord, "invocations">): boolean => r.invocations.some(i => i.outcome === "invoked");

// ---- monotonic merge -------------------------------------------------------------------------------------

/** Set-once identity fields. A differing value once set is a rejection. */
export interface TerminalIdentityPatch {
  manifest?: TerminalManifest; tip?: string; priorTip?: string; range?: string; scanDigest?: string; transportDigest?: string;
}
const IDENTITY_KEYS = ["manifest", "tip", "priorTip", "range", "scanDigest", "transportDigest"] as const;

/** A delta against the current record. Writers never send a whole record, so a stale writer cannot overwrite what a
 *  concurrent writer added: every field only appends, grows or advances. */
export interface TerminalDelta extends TerminalIdentityPatch {
  phase?: TerminalPhase;
  appendInvocations?: TerminalInvocation[];
  addLocalBlock?: string[];
  addFlaggedCommits?: string[];
  verdicts?: Record<string, string>;
  closedReason?: string;
}

export type TerminalMergeResult = { ok: true; record: TerminalOperationRecord } | { ok: false; reason: string };

/**
 * Pure monotonic merge of `delta` into `current`. Rules: invocations are append-only; localBlock and flaggedCommits only
 * grow; identity fields are set-once; the phase never moves backward (capturing < captured < prepared < closed, with
 * `incomplete` reachable only from capturing/captured, and `closed` terminal); `invoked` can never be appended while any
 * local block exists or after close; `closed` needs a final outcome, or no `invoked` entry at all; a `conflict` outcome
 * always adds the `server_conflict` block.
 */
export function mergeTerminalRecord(current: TerminalOperationRecord, delta: TerminalDelta): TerminalMergeResult {
  const no = (reason: string): TerminalMergeResult => ({ ok: false, reason });
  // Append-only history ends at close: any non-empty delta against a closed record is refused (an empty one, or a
  // restatement of the closed phase, stays an idempotent no-op).
  if (current.phase === "closed") {
    const nonEmpty = IDENTITY_KEYS.some(k => delta[k] !== undefined) || (delta.phase !== undefined && delta.phase !== "closed") ||
      (delta.appendInvocations?.length ?? 0) > 0 || (delta.addLocalBlock?.length ?? 0) > 0 ||
      (delta.addFlaggedCommits?.length ?? 0) > 0 || Object.keys(delta.verdicts ?? {}).length > 0 || delta.closedReason !== undefined;
    if (nonEmpty) return no("closed");
  }
  const next: TerminalOperationRecord = {
    ...current, verdicts: { ...current.verdicts }, flaggedCommits: [...current.flaggedCommits],
    invocations: [...current.invocations], localBlock: [...current.localBlock],
  };
  for (const key of IDENTITY_KEYS) {
    const value = delta[key];
    if (value === undefined) continue;
    const have = current[key];
    if (have !== undefined && canonicalTerminalJson(have) !== canonicalTerminalJson(value)) return no("identity_changed");
    (next as unknown as Record<string, unknown>)[key] = value;
  }
  for (const [k, v] of Object.entries(delta.verdicts ?? {})) {
    if (current.verdicts[k] !== undefined && current.verdicts[k] !== v) return no("verdict_changed");
    next.verdicts[k] = v;
  }
  for (const sha of delta.addFlaggedCommits ?? []) if (!next.flaggedCommits.includes(sha)) next.flaggedCommits.push(sha);
  const addBlock = (b: string) => { if (!next.localBlock.includes(b)) next.localBlock.push(b); };
  for (const b of delta.addLocalBlock ?? []) addBlock(b);
  const appended = delta.appendInvocations ?? [];
  for (const inv of appended) {
    if (inv.outcome === "invoked") {
      if (next.localBlock.length > 0) return no("local_block");
      if (next.phase !== "prepared") return no("not_prepared");
    }
    if (inv.outcome === "conflict") addBlock("server_conflict");
    next.invocations.push(inv);
  }
  if (next.invocations.length > MAX_INVOCATIONS || next.localBlock.length > MAX_BLOCKS ||
      next.flaggedCommits.length > MAX_FLAGGED) return no("bound");
  if (delta.closedReason !== undefined) next.closedReason = delta.closedReason;
  if (delta.phase !== undefined && delta.phase !== current.phase) {
    const from = current.phase, to = delta.phase;
    if (from === "closed") return no("closed");
    if (to === "incomplete") {
      if (from !== "capturing" && from !== "captured") return no("incomplete_only_from_capture");
    } else if (from === "incomplete" || PHASE_RANK[to] < PHASE_RANK[from]) return no("phase_regression");
    next.phase = to;
    if (to === "prepared" && (next.tip === undefined || next.range === undefined || next.manifest === undefined ||
        next.scanDigest === undefined || next.transportDigest === undefined)) return no("prepared_incomplete");
  }
  if (next.phase === "closed" && current.phase !== "closed") {
    if (!terminalFinalOutcome(next) && terminalInvoked(next)) return no("close_requires_final");
    if (!terminalFinalOutcome(next) && next.closedReason === undefined) return no("close_requires_reason");
  }
  // The merge enforces the same bounds the strict reader does, so it can never produce a record its own reader rejects.
  if (!decodeTerminalRecord(JSON.parse(JSON.stringify(next)))) return no("bound");
  return { ok: true, record: next };
}

// ---- store contract (implemented by RecoveryCoordinator) -------------------------------------------------

export type TerminalReadResult = { kind: "ok"; record: TerminalOperationRecord } | { kind: "absent" } | { kind: "corrupt" };
export type TerminalUpdateResult =
  | { kind: "ok"; record: TerminalOperationRecord }
  | { kind: "rejected"; reason: string }
  | { kind: "absent" } | { kind: "corrupt" } | { kind: "disabled" } | { kind: "error"; reason: string };
export type TerminalInvokeResult =
  | { kind: "invoked"; record: TerminalOperationRecord }
  | { kind: "blocked"; reason: string }
  | { kind: "rejected"; reason: string } | { kind: "absent" } | { kind: "corrupt" } | { kind: "disabled" } | { kind: "error"; reason: string };

export interface TerminalOperationStore {
  read(runId: string, generation: number): Promise<TerminalReadResult>;
  /** Create the record when absent; `exists` carries the winner. */
  create(record: TerminalOperationRecord): Promise<{ kind: "created"; record: TerminalOperationRecord } | { kind: "exists" } | { kind: "disabled" } | { kind: "error"; reason: string }>;
  /** Read-merge-write under the run's journal lock. Safe to call while the operation owner holds the lane. */
  update(runId: string, generation: number, delta: TerminalDelta): Promise<TerminalUpdateResult>;
  /** Latch writer: only the journal lock, so it lands while a send is in flight. */
  addLocalBlock(runId: string, generation: number, reasons: string[], flaggedCommits?: string[]): Promise<TerminalUpdateResult>;
  /** The pre-send step, atomic under the journal lock: refuse when any local block or secret record exists for the run,
   *  else append `invoked` durably. */
  invoke(runId: string, generation: number, at: string): Promise<TerminalInvokeResult>;
  /** Run-wide head-authority check (any generation): clear only when no record of the run carries a local block, a
   *  flagged commit or a secret closure. Disabled, unreadable or corrupt state is never `clear`. */
  runBlock(runId: string): Promise<{ kind: "clear" } | { kind: "blocked"; reason: string } | { kind: "error"; reason: string }>;
}

/** Result of RecoveryCoordinator.resumeTerminalOperations: in-memory only, nothing here was sent or touched in git. */
export interface TerminalRediscovery {
  resumePending: Array<{ runId: string; generation: number; record: TerminalOperationRecord }>;
  incomplete: number;
  corrupt: number;
  truncated: boolean;
}

// ---- engine ----------------------------------------------------------------------------------------------

export type TerminalOperationResult =
  | { outcome: "published" | "recorded"; tip: string; manifest: TerminalManifest }
  | { outcome: "kept"; reason: string }
  | { outcome: "unknown"; reason: string };

export type CaptureOutcome =
  | { kind: "ok"; base: string; extraRoots: string[]; manifest: TerminalManifest; snapshot?: { commit: string; tree: string } }
  | { kind: "refused"; reason: string };
export type PrepareOutcome =
  | { pack: Buffer; range: CheckpointRange; digests: { scan: string; transport: string } }
  | { reason: string; range?: CheckpointRange; flaggedCommits?: string[] };

export interface TerminalOperationDeps {
  enabled(): boolean;
  store: TerminalOperationStore;
  /** The operation owner lane (one operation per run generation). */
  owner<T>(fn: () => Promise<T>): Promise<T>;
  /** A refusal reason when a prior failure forbids any preservation (push_secret_blocked, worker_residue_blocked). */
  failureFirst(): string | undefined;
  universe(): Promise<TerminalUniverse>;
  capture(onPhase: (phase: "capturing" | "captured", info: { cloneRef: string; bareRef: string; commit?: string }) => Promise<void>): Promise<CaptureOutcome>;
  aggregate(input: { base: string; extraRoots: string[] }): Promise<{ tip: string } | { refused: string }>;
  prepare(range: CheckpointRange | "resolve", expectDigests: { scan: string; transport: string } | undefined, tip: string): Promise<PrepareOutcome>;
  /** `snapshot` is the capture's own snapshot root, the one extra root a manifest may declare. */
  coverage(universe: Extract<TerminalUniverse, { kind: "proved" }>, manifest: TerminalManifest, snapshot?: { commit: string; tree: string }): TerminalCoverageResult;
  /** A late local failure (secret remediation, residue) discovered after preparation; undefined when none. */
  recheck(): Promise<string | undefined>;
  boundaryValid(): boolean;
  send(req: TerminalPreservationRequest, signal?: AbortSignal): Promise<TerminalPreservationResult>;
  now(): number;
}

export interface TerminalOperationInput {
  runId: string;
  workerId: string;
  generation: number;
  authorityClass: TerminalAuthorityClass;
  clonePath: string;
  priorTip?: string;
  mode: "initial" | "resume";
  signal?: AbortSignal;
}

const kept = (reason: string): TerminalOperationResult => ({ outcome: "kept", reason });
/** Clamp an injected or server-supplied reason to `[a-z0-9_]` and a length well inside the record's string bounds before
 *  it can reach a persisted field. */
const reasonToken = (reason: string): string => reason.toLowerCase().replace(/[^a-z0-9_]+/g, "_").slice(0, 120) || "unknown";
const iso = (deps: TerminalOperationDeps): string => new Date(deps.now()).toISOString();

/** The snapshot root the record's (MAC-protected) manifest declared at capture, re-presented to the coverage check on
 *  resume and on the late check. */
function recordedSnapshot(manifest: TerminalManifest): { commit: string; tree: string } | undefined {
  const root = manifest.roots.find(r => r.source_id.endsWith(TERMINAL_SNAPSHOT_SUFFIX));
  return root ? { commit: root.commit, tree: root.tree } : undefined;
}

async function finalResult(deps: TerminalOperationDeps, record: TerminalOperationRecord): Promise<TerminalOperationResult> {
  const outcome = terminalFinalOutcome(record);
  // A block that landed while the send was in flight keeps the factual receipt but never yields head authority.
  if (outcome && record.localBlock.length > 0) return kept(record.localBlock[0]!);
  if (outcome && record.tip && record.manifest) {
    // Head authority is run-wide: a block on any other generation of the run also withholds it.
    const run = await deps.store.runBlock(record.runId);
    if (run.kind !== "clear") return kept(run.reason);
    return { outcome, tip: record.tip, manifest: record.manifest };
  }
  return kept(record.closedReason ?? "closed");
}

/** Run one terminal operation (or its resume) in the approved order. Every effect is injected; with the gate off it does
 *  nothing at all. Typed results; never throws for an expected refusal. */
export async function runTerminalOperation(deps: TerminalOperationDeps, input: TerminalOperationInput): Promise<TerminalOperationResult> {
  if (!deps.enabled()) return kept("disabled");
  const failure = deps.failureFirst();
  if (failure !== undefined) return kept(failure);
  if (!isTerminalRunId(input.runId) || !isTerminalGeneration(input.generation)) return kept("invalid_input");
  try {
    return await deps.owner(() => operate(deps, input));
  } catch (error) {
    return kept(`operation_error:${error instanceof Error ? error.message.slice(0, 120) : "unknown"}`);
  }
}

async function operate(deps: TerminalOperationDeps, input: TerminalOperationInput): Promise<TerminalOperationResult> {
  const { store } = deps;
  const existing = await store.read(input.runId, input.generation);
  if (existing.kind === "corrupt") return kept("record_corrupt");
  if (existing.kind === "ok") {
    const rec = existing.record;
    if (rec.phase === "closed") return finalResult(deps, rec);
    if (input.mode !== "resume") return kept("existing_record");
    return resume(deps, input, rec);
  }
  if (input.mode === "resume") return kept("not_resumable");
  return initial(deps, input);
}

type Proved = Extract<TerminalUniverse, { kind: "proved" }>;
async function proveUniverse(deps: TerminalOperationDeps): Promise<{ ok: true; universe: Proved } | { ok: false; reason: string }> {
  const universe = await deps.universe();
  return universe.kind === "proved" ? { ok: true, universe } : { ok: false, reason: `universe_${universe.reason}` };
}

/** Mark a capture-phase record incomplete (never from prepared or later); a rejection is not an error. */
async function markIncomplete(deps: TerminalOperationDeps, input: TerminalOperationInput, reason: string): Promise<TerminalOperationResult> {
  const clamped = reasonToken(reason);
  await deps.store.update(input.runId, input.generation, { phase: "incomplete", closedReason: clamped });
  return kept(clamped);
}

async function initial(deps: TerminalOperationDeps, input: TerminalOperationInput): Promise<TerminalOperationResult> {
  const { store } = deps;
  const proof = await proveUniverse(deps);
  if (!proof.ok) return kept(proof.reason);
  let recordCreated = false;
  const captured = await deps.capture(async (phase, info) => {
    if (phase === "capturing") {
      const created = await store.create({
        runId: input.runId, workerId: input.workerId, generation: input.generation, authorityClass: input.authorityClass,
        universeDigest: proof.universe.universeDigest, verdicts: {}, cloneRef: info.cloneRef, bareRef: info.bareRef,
        clonePath: input.clonePath, flaggedCommits: [], phase: "capturing", invocations: [], localBlock: [],
      });
      if (created.kind !== "created") throw new Error(`record_${created.kind}`);
      recordCreated = true;
      return;
    }
    const updated = await store.update(input.runId, input.generation, { phase: "captured" });
    if (updated.kind !== "ok") throw new Error(`record_${updated.kind}`);
  });
  if (captured.kind === "refused") return recordCreated ? markIncomplete(deps, input, captured.reason) : kept(captured.reason);
  if (!recordCreated) return kept("record_not_created");
  const cov = deps.coverage(proof.universe, captured.manifest, captured.snapshot);
  if (!cov.ok) return markIncomplete(deps, input, `coverage_${cov.reason}`);
  const agg = await deps.aggregate({ base: captured.base, extraRoots: captured.extraRoots });
  if ("refused" in agg) return markIncomplete(deps, input, `aggregate_${agg.refused}`);
  const prepared = await deps.prepare("resolve", undefined, agg.tip);
  if (!("pack" in prepared)) return prepareFailure(deps, input, prepared, true);
  const wrote = await store.update(input.runId, input.generation, {
    phase: "prepared", manifest: captured.manifest, tip: agg.tip,
    ...(input.priorTip !== undefined ? { priorTip: input.priorTip } : {}),
    range: serializeCheckpointRange(prepared.range), scanDigest: prepared.digests.scan,
    transportDigest: prepared.digests.transport, verdicts: { coverage: "ok", prepare: "ok" },
  });
  if (wrote.kind !== "ok") return kept(`record_${wrote.kind}`);
  return finishAndSend(deps, input, wrote.record, prepared.pack);
}

async function resume(deps: TerminalOperationDeps, input: TerminalOperationInput, rec: TerminalOperationRecord): Promise<TerminalOperationResult> {
  if (rec.phase !== "prepared" || rec.tip === undefined || rec.manifest === undefined || rec.range === undefined ||
      rec.scanDigest === undefined || rec.transportDigest === undefined) return kept("not_resumable");
  if (rec.localBlock.length > 0) return kept("local_block");
  const range = parseCheckpointRange(rec.range);
  if (!range) return kept("record_range_invalid");
  const proof = await proveUniverse(deps);
  if (!proof.ok) return kept(proof.reason);
  if (proof.universe.universeDigest !== rec.universeDigest) return kept("universe_changed");
  const cov = deps.coverage(proof.universe, rec.manifest, recordedSnapshot(rec.manifest));
  if (!cov.ok) return kept(`coverage_${cov.reason}`);
  const prepared = await deps.prepare(range, { scan: rec.scanDigest, transport: rec.transportDigest }, rec.tip);
  if (!("pack" in prepared)) return prepareFailure(deps, input, prepared, false);
  // The re-prepared pack must be the one the record describes, and the request rebuilt from the record must name this
  // operation: any difference sends nothing.
  const m = rec.manifest;
  if (prepared.digests.scan !== rec.scanDigest || prepared.digests.transport !== rec.transportDigest ||
      serializeCheckpointRange(prepared.range) !== rec.range ||
      m.run_id !== input.runId || m.generation !== input.generation || m.worker_id !== input.workerId ||
      (input.priorTip !== undefined && input.priorTip !== rec.priorTip)) return kept("digest_mismatch");
  return finishAndSend(deps, input, rec, prepared.pack);
}

/** A preparation refusal. A secret finding latches (and closes only a positively uninvoked operation); anything else is a
 *  kept local failure that never closes. */
async function prepareFailure(deps: TerminalOperationDeps, input: TerminalOperationInput,
  failed: Extract<PrepareOutcome, { reason: string }>, fresh: boolean): Promise<TerminalOperationResult> {
  if (failed.reason !== "secret_found") return fresh ? markIncomplete(deps, input, `prepare_${failed.reason}`) : kept(`prepare_${failed.reason}`);
  // The block is written on its own first, so a malformed or oversized flagged-commit list can never cost the latch;
  // the flagged commits follow as a best-effort, normalised second write. A block that does not persist is reported as
  // such (the caller still latches the flight in memory) rather than silently treated as recorded.
  const latched = await deps.store.addLocalBlock(input.runId, input.generation, ["secret_found"]);
  if (latched.kind !== "ok") return kept("secret_found_unlatched");
  const room = Math.max(0, MAX_FLAGGED - latched.record.flaggedCommits.length);
  const flagged = [...new Set((failed.flaggedCommits ?? []).filter(sha => SHA_RE.test(sha)))]
    .filter(sha => !latched.record.flaggedCommits.includes(sha)).slice(0, room);
  if (flagged.length > 0) await deps.store.addLocalBlock(input.runId, input.generation, ["secret_found"], flagged);
  if (latched.kind === "ok" && !terminalInvoked(latched.record) && latched.record.phase !== "closed" &&
      latched.record.phase !== "incomplete") {
    await deps.store.update(input.runId, input.generation, {
      phase: "closed", closedReason: "secret_found", appendInvocations: [{ at: iso(deps), outcome: "refused", reason: "secret_found" }],
    });
  }
  return kept("secret_found");
}

async function finishAndSend(deps: TerminalOperationDeps, input: TerminalOperationInput, rec: TerminalOperationRecord,
  pack: Buffer): Promise<TerminalOperationResult> {
  const { store } = deps;
  const proof = await proveUniverse(deps);
  if (!proof.ok) return lateFailure(deps, input, proof.reason);
  if (proof.universe.universeDigest !== rec.universeDigest) return lateFailure(deps, input, "universe_changed");
  const cov = deps.coverage(proof.universe, rec.manifest!, recordedSnapshot(rec.manifest!));
  if (!cov.ok) return lateFailure(deps, input, `coverage_${cov.reason}`);
  const late = await deps.recheck();
  if (late !== undefined) return lateFailure(deps, input, late);
  if (!deps.boundaryValid()) return kept("boundary_invalid");
  const invoked = await store.invoke(input.runId, input.generation, iso(deps));
  if (invoked.kind !== "invoked") return kept(invoked.kind === "blocked" ? invoked.reason : `invoke_${invoked.kind}`);
  // The boundary can lapse while `invoke` is in flight: nothing is sent. The lapse is NOT latched: nothing left the
  // worker, and a later resume re-proves universe, coverage, digests and its own boundary before it may send the
  // identical request (bounded by MAX_INVOCATIONS). A run-wide block here would strand the run's custody for good.
  if (!deps.boundaryValid()) return kept("boundary_lost");
  const req: TerminalPreservationRequest = {
    generation: input.generation, tip: rec.tip!, ...(rec.priorTip !== undefined ? { prior_tip: rec.priorTip } : {}),
    manifest: rec.manifest!, disposition: "preserve", ...(pack.length > 0 ? { pack: pack.toString("base64") } : {}),
  };
  let result: TerminalPreservationResult;
  try {
    result = await deps.send(req, input.signal);
  } catch {
    result = { kind: "unknown", cause: "transport" };
  }
  const at = iso(deps);
  if (result.kind === "final") {
    const done = await store.update(input.runId, input.generation, {
      phase: "closed", appendInvocations: [{ at, outcome: result.outcome }], closedReason: result.outcome,
    });
    if (done.kind !== "ok") return { outcome: "unknown", reason: `record_${done.kind}` };
    // The factual receipt is recorded above; head authority is returned only when nothing blocked or lapsed meanwhile.
    const blocked = done.record.localBlock[0] ?? deps.failureFirst();
    if (blocked !== undefined) return kept(blocked);
    const run = await store.runBlock(input.runId);
    if (run.kind !== "clear") return kept(run.reason);
    if (!deps.boundaryValid()) return kept("boundary_lost");
    return { outcome: result.outcome, tip: rec.tip!, manifest: rec.manifest! };
  }
  if (result.kind === "refused") {
    const conflict = result.reason === "generation_bound" || result.reason === "prior_tip_mismatch";
    // A non-conflict server refusal after `invoke` leaves the record prepared and resumable (the next resume may send
    // again); the loop is bounded by MAX_INVOCATIONS, after which every append is rejected as `bound`.
    await store.update(input.runId, input.generation, {
      appendInvocations: [{ at, outcome: conflict ? "conflict" : "refused", reason: reasonToken(result.reason) }],
      ...(conflict ? { addLocalBlock: ["server_conflict"] } : {}),
    });
    return kept(result.reason);
  }
  const reason = result.cause === "retryable" ? result.reason : result.cause;
  await store.update(input.runId, input.generation, { appendInvocations: [{ at, outcome: "publish_unknown", reason: reasonToken(reason) }] });
  return { outcome: "unknown", reason };
}

/** A failure found after preparation: close as refused only when positively uninvoked, else latch without closing. */
async function lateFailure(deps: TerminalOperationDeps, input: TerminalOperationInput, reason: string): Promise<TerminalOperationResult> {
  const latched = await deps.store.addLocalBlock(input.runId, input.generation, ["local_failure_late"]);
  if (latched.kind === "ok" && !terminalInvoked(latched.record) && latched.record.phase === "prepared") {
    await deps.store.update(input.runId, input.generation, {
      phase: "closed", closedReason: "local_failure_late",
      appendInvocations: [{ at: iso(deps), outcome: "refused", reason: "local_failure_late" }],
    });
  }
  return kept(reason === "local_failure_late" ? reason : `late_${reason}`);
}

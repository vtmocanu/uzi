/** Controller evidence, versions 1 (legacy) and 2 (adds successor containers and attestation).
 * Parsing grants no epoch or deletion authority. */
export interface PodEpochContainer {
  name: string; containerID: string; exitCode: number; restartCount: number;
}
export interface PodEpochClaim { name: string; uid: string; volumeName: string }
interface PodEpochEvidenceBase {
  session: string; predecessor: string; successor: string;
  containers: PodEpochContainer[]; initContainers: PodEpochContainer[];
  data: PodEpochClaim; dind: PodEpochClaim;
  maintenance?: { operationID: string; oldClaimUID: string; newClaimUID: string };
  runtime: "containerd-runc" | "crio-crun";
  sessionStarted: string; terminalObserved: string; successorAdded: string;
}
export interface PodEpochEvidenceV1 extends PodEpochEvidenceBase { version: 1 }
export interface PodEpochEvidenceV2 extends PodEpochEvidenceBase {
  version: 2;
  successorContainers: Array<{ name: string; containerID: string }>;
  attestation: { result: "consistent" | "contradicted"; reason: string };
}
export type PodEpochEvidence = PodEpochEvidenceV1 | PodEpochEvidenceV2;
export interface PodEpochNegative { handoff: "none"; reason: string }
export type PodEpochDocument = PodEpochEvidence | PodEpochNegative;
export class PodEpochEvidenceError extends Error {
  constructor() { super("pod_epoch_evidence_invalid"); this.name = "PodEpochEvidenceError"; }
}
function requireValid(ok: unknown): asserts ok { if (!ok) throw new PodEpochEvidenceError(); }
const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;
function nonempty(v: unknown): v is string { return typeof v === "string" && v.length > 0; }
function identity(v: unknown): v is string { return typeof v === "string" && uuid.test(v); }
function shape(v: unknown, keys: string[], optional: string[] = []): asserts v is Record<string, unknown> {
  requireValid(typeof v === "object" && v !== null && !Array.isArray(v));
  requireValid(keys.every(k => Object.hasOwn(v, k)) &&
    Object.keys(v).every(k => keys.includes(k) || optional.includes(k)));
}
/** Raw token walk: decoded member names are unique at every depth, including escaped names.
 * Depth and input bytes bound recursion and work. Only integer number syntax is used by v1.
 */
function tokens(source: string): void {
  let i = 0;
  const whitespace = () => { while (/[\x20\t\r\n]/.test(source[i] ?? "") && i < source.length) i++; };
  const string = (): string => {
    requireValid(source[i++] === '"');
    const start = i - 1;
    for (; i < source.length; i++) {
      if (source[i] === "\\") { i++; continue; }
      if (source[i] === '"') return JSON.parse(source.slice(start, ++i)) as string;
    }
    throw new PodEpochEvidenceError();
  };
  const value = (depth: number): void => {
    requireValid(depth <= 64); whitespace();
    if (source[i] === "{") {
      i++; whitespace(); const seen = new Set<string>();
      if (source[i] !== "}") for (;;) {
        whitespace(); const key = string();
        requireValid(!seen.has(key)); seen.add(key); whitespace();
        requireValid(source[i++] === ":"); value(depth + 1); whitespace();
        if (source[i] !== ",") break;
        i++;
      }
      requireValid(source[i++] === "}");
    } else if (source[i] === "[") {
      i++; whitespace();
      if (source[i] !== "]") for (;;) {
        value(depth + 1); whitespace(); if (source[i] !== ",") break; i++;
      }
      requireValid(source[i++] === "]");
    } else if (source[i] === '"') string();
    else {
      const match = /^(?:-?(?:0|[1-9][0-9]*)|true|false)/.exec(source.slice(i));
      requireValid(match); i += match[0].length;
    }
  };
  value(0); whitespace(); requireValid(i === source.length);
}
function timestamp(v: unknown): bigint {
  requireValid(typeof v === "string");
  const m = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(?:\.(\d{1,9}))?(Z|[+-]\d{2}:\d{2})$/.exec(v);
  requireValid(m);
  const [year, month, day, hour, minute, second] = m.slice(1, 7).map(Number);
  requireValid(year !== undefined && month !== undefined && day !== undefined &&
    hour !== undefined && minute !== undefined && second !== undefined);
  const d = new Date(0);
  d.setUTCFullYear(year, month - 1, day); d.setUTCHours(hour, minute, second, 0);
  requireValid(month >= 1 && month <= 12 && day >= 1 &&
    d.getUTCFullYear() === year && d.getUTCMonth() === month - 1 && d.getUTCDate() === day &&
    hour <= 23 && minute <= 59 && second <= 59);
  let offset = 0;
  if (m[8] !== "Z") {
    const zone = m[8]!;
    const h = Number(zone.slice(1, 3)), min = Number(zone.slice(4));
    requireValid(h <= 23 && min <= 59);
    offset = (h * 60 + min) * (zone[0] === "+" ? 1 : -1);
  }
  const ns = BigInt(d.getTime() - offset * 60_000) * 1_000_000n +
    BigInt((m[7] ?? "").padEnd(9, "0"));
  requireValid(ns !== -62135596800000000000n); // Go time.Time zero instant.
  return ns;
}
function entries(v: unknown, required: boolean): void {
  requireValid(Array.isArray(v) && (!required || v.length > 0));
  const names = new Set<string>();
  for (const e of v) {
    shape(e, ["name", "containerID", "exitCode", "restartCount"]);
    requireValid(nonempty(e.name) && !names.has(e.name) && nonempty(e.containerID));
    names.add(e.name);
    for (const n of [e.exitCode, e.restartCount])
      requireValid(typeof n === "number" && Number.isInteger(n) && n >= 0 && n <= 2147483647);
    requireValid(e.exitCode !== 137 && e.exitCode !== 255);
  }
}
function claim(v: unknown): asserts v is unknown & PodEpochClaim {
  shape(v, ["name", "uid", "volumeName"]);
  requireValid(nonempty(v.name) && identity(v.uid) && nonempty(v.volumeName));
}
export function parsePodEpochEvidence(input: string | Uint8Array): PodEpochDocument {
  try {
    const size = typeof input === "string" ? Buffer.byteLength(input, "utf8") : input.byteLength;
    requireValid(size > 0 && size <= 8192);
    const bytes = typeof input === "string" ? Buffer.from(input, "utf8") : input;
    const source = new TextDecoder("utf-8", { fatal: true, ignoreBOM: true }).decode(bytes);
    // Reject lone UTF-16 surrogates supplied directly by a JS caller.
    requireValid(typeof input !== "string" || source === input);
    tokens(source);
    const v: unknown = JSON.parse(source);
    if (typeof v === "object" && v !== null && Object.hasOwn(v, "handoff")) {
      shape(v, ["handoff", "reason"]);
      requireValid(v.handoff === "none" && nonempty(v.reason) && v.reason.trim() !== "");
      return v as unknown as PodEpochNegative;
    }
    const version = typeof v === "object" && v !== null ? (v as { version?: unknown }).version : undefined;
    requireValid(version === 1 || version === 2);
    const v2 = version === 2;
    shape(v, ["version", "session", "predecessor", "successor", "containers", "initContainers",
      "data", "dind", "runtime", "sessionStarted", "terminalObserved", "successorAdded",
      ...(v2 ? ["successorContainers", "attestation"] : [])], ["maintenance"]);
    requireValid(identity(v.session) && identity(v.predecessor) &&
      identity(v.successor) && v.predecessor !== v.successor &&
      (v.runtime === "containerd-runc" || v.runtime === "crio-crun"));
    claim(v.data); claim(v.dind); requireValid(v.data.name !== v.dind.name);
    entries(v.containers, true); entries(v.initContainers, false);
    const start = timestamp(v.sessionStarted), terminal = timestamp(v.terminalObserved),
      successor = timestamp(v.successorAdded);
    requireValid(start <= terminal && terminal <= successor);
    if (Object.hasOwn(v, "maintenance")) {
      shape(v.maintenance, ["operationID", "oldClaimUID", "newClaimUID"]);
      const m = v.maintenance;
      requireValid(nonempty(m.operationID) && m.operationID.trim() !== "" &&
        identity(m.oldClaimUID) && identity(m.newClaimUID) &&
        m.oldClaimUID !== m.newClaimUID && m.newClaimUID === v.dind.uid);
    }
    if (v2) {
      requireValid(Array.isArray(v.successorContainers) && v.successorContainers.length > 0);
      const names = new Set<string>();
      for (const c of v.successorContainers as unknown[]) {
        shape(c, ["name", "containerID"]);
        requireValid(nonempty(c.name) && !names.has(c.name) && nonempty(c.containerID));
        names.add(c.name);
      }
      const a = v.attestation;
      shape(a, ["result", "reason"]);
      requireValid((a.result === "consistent" || a.result === "contradicted") &&
        nonempty(a.reason) && a.reason.trim() !== "");
    }
    return v as unknown as PodEpochEvidence;
  } catch { throw new PodEpochEvidenceError(); }
}
/** Additional startup binding; volume-entry ownership is checked by the EX orchestrator. */
export function assertPodEpochEvidenceBinding(
  evidence: PodEpochEvidence, successor: string, runtime: string,
): void {
  requireValid(evidence.successor === successor && evidence.runtime === runtime);
}

import { recoverySource, sameRecoverySource, type RecoveryProgress, type RecoverySource } from "./recovery-progress.js";

// Accounting only: this namespace never supplies custody, deletion or model authority.
export interface OwnerReservation {
  episode: number;
  claimGeneration: number;
  startedAt: number;
  deadline: number;
  attempts: number;
}
export interface RecoveryOwnerRecord {
  version: 1;
  bare: string;
  runId: string;
  branch: string;
  key: string;
  source: RecoverySource;
  root: RecoverySource;
  baseline: number;
  consumed: number;
  state: "pending" | "committed";
  before: OwnerReservation | null;
  after: OwnerReservation;
  successor?: RecoverySource;
}
const invalid = (): never => { throw new Error("invalid recovery owner record"); };
export const ownerGrant = (value: unknown): value is number =>
  Number.isSafeInteger(value) && (value as number) >= 0;
export function ownerReservation(p: RecoveryProgress): OwnerReservation {
  return { episode: p.episode ?? 0, claimGeneration: p.claimGeneration ?? 0,
    startedAt: p.startedAt, deadline: p.deadline, attempts: p.attempts };
}
export function sameOwnerReservation(a: OwnerReservation | null, b: OwnerReservation | null): boolean {
  return a === null || b === null ? a === b : a.episode === b.episode &&
    a.claimGeneration === b.claimGeneration && a.startedAt === b.startedAt &&
    a.deadline === b.deadline && a.attempts === b.attempts;
}
export function parseRecoveryOwner(raw: string): RecoveryOwnerRecord {
  if (Buffer.byteLength(raw) > 32_768) invalid();
  const o = JSON.parse(raw) as RecoveryOwnerRecord;
  // Only our canonical serialization is accepted; this also rejects duplicate JSON keys.
  if (!o || Array.isArray(o) || JSON.stringify(o) !== raw ||
      Object.keys(o).some(k => !["version", "bare", "runId", "branch", "key", "source", "root",
        "baseline", "consumed", "state", "before", "after", "successor"].includes(k)) ||
      o.version !== 1 || typeof o.bare !== "string" || o.bare.length > 4096 ||
      typeof o.branch !== "string" || o.branch.length > 1024 || !o.branch ||
      typeof o.key !== "string" || !/^[A-Za-z0-9_-]{1,160}$/.test(o.key) ||
      !ownerGrant(o.baseline) || !ownerGrant(o.consumed) || o.consumed < o.baseline ||
      !["pending", "committed"].includes(o.state)) invalid();
  for (const s of [o.source, o.root, ...(o.successor !== undefined ? [o.successor] : [])]) {
    recoverySource(s);
    if (s.runId !== o.runId) invalid();
  }
  for (const [index, r] of [o.before, o.after].entries()) {
    if (r === null && index === 0) continue;
    if (!r || Object.keys(r).sort().join(",") !== "attempts,claimGeneration,deadline,episode,startedAt" ||
        !ownerGrant(r.episode) || !ownerGrant(r.claimGeneration) || (index === 1 && r.claimGeneration < 1) ||
        !ownerGrant(r.startedAt) || r.deadline !== r.startedAt + 300_000 ||
        !Number.isSafeInteger(r.deadline) || !Number.isSafeInteger(r.attempts) ||
        r.attempts < 1 || r.attempts > 3) invalid();
  }
  return o;
}
export function ownerJournalMatches(record: RecoveryOwnerRecord, journal: RecoverySource & { recovery?: RecoveryProgress }): boolean {
  const p = journal.recovery;
  return !!p && JSON.stringify(p.source) === JSON.stringify(record.source) &&
    sameOwnerReservation(record.after, ownerReservation(p)) &&
    (sameRecoverySource(journal, record.root) ||
      (!!record.successor && sameRecoverySource(journal, record.successor) &&
       !!p.successor && JSON.stringify(p.successor) === JSON.stringify(record.successor)));
}

/** The journal still names the exact reservation preceding an interrupted charge. */
export function ownerJournalBeforeMatches(record: RecoveryOwnerRecord, journal: RecoverySource & { recovery?: RecoveryProgress }): boolean {
  if (!sameRecoverySource(journal, record.root)) return false;
  const p = journal.recovery;
  if (record.before === null) return !p && sameRecoverySource(journal, record.source);
  return !!p && JSON.stringify(p.source) === JSON.stringify(record.source) &&
    sameOwnerReservation(record.before, ownerReservation(p));
}

export class RecoveryChargeInterruptedError extends Error {
  constructor() { super("recovery owner pending charge; work refused"); }
}

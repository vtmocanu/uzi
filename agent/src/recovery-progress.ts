import path from "node:path";
import { ATTEMPT_ID_RE } from "./attempt-path.js";

export interface RecoverySource {
  runId: string;
  clonePath: string;
  attemptId?: string;
  restoreTip?: string;
}

export type RecoveryBlocker = "capture_failed" | "source_missing" | "adoption_failed" | "budget_exhausted" | "clock_invalid" | "oversize" | "prerequisites_unavailable" | "quiescence_failed" | "preservation_failed" | "decoded_history_limit";

export interface RecoveryProgress {
  version: 1;
  source: RecoverySource;
  attempts: number;
  startedAt: number;
  deadline: number;
  backoffMs: number;
  stage: "capturing" | "captured" | "adopting" | "ready-for-model" | "blocked";
  restoreTip?: string;
  successor?: RecoverySource;
  blocker?: RecoveryBlocker;
}

export class InvalidRecoveryClonePathError extends Error {
  constructor() { super("invalid retained recovery clone path"); }
}

const invalid = (): never => { throw new Error("invalid retained recovery progress"); };
const object = (v: unknown): Record<string, unknown> => {
  if (!v || typeof v !== "object" || Array.isArray(v)) return invalid();
  return v as Record<string, unknown>;
};
const keys = (v: Record<string, unknown>, allowed: string[]): void => {
  if (Object.keys(v).some(k => !allowed.includes(k))) invalid();
};

export function recoverySource(v: unknown): RecoverySource {
  const o = object(v);
  keys(o, ["runId", "clonePath", "attemptId", "restoreTip"]);
  if (typeof o.runId !== "string" || !/^[A-Za-z0-9_-]{1,160}$/.test(o.runId)) invalid();
  if (typeof o.clonePath !== "string" || !path.isAbsolute(o.clonePath) ||
      path.resolve(o.clonePath) !== o.clonePath || [...o.clonePath].some(c => c.charCodeAt(0) < 32 || c.charCodeAt(0) === 127)) {
    throw new InvalidRecoveryClonePathError();
  }
  if (o.attemptId !== undefined && (typeof o.attemptId !== "string" || !ATTEMPT_ID_RE.test(o.attemptId))) invalid();
  if (o.restoreTip !== undefined && (typeof o.restoreTip !== "string" || !/^[0-9a-f]{40}$/.test(o.restoreTip))) invalid();
  if (o.attemptId !== undefined && !(o.clonePath as string).endsWith(`.attempt-${o.attemptId}`)) invalid();
  return o as unknown as RecoverySource;
}

export function recoveryProgress(v: unknown): RecoveryProgress {
  const o = object(v);
  keys(o, ["version", "source", "attempts", "startedAt", "deadline", "backoffMs", "stage", "restoreTip", "successor", "blocker"]);
  if (o.version !== 1 || !Number.isSafeInteger(o.attempts) || (o.attempts as number) < 1 || (o.attempts as number) > 3 ||
      !Number.isSafeInteger(o.startedAt) || (o.startedAt as number) < 0 ||
      !Number.isSafeInteger(o.deadline) || o.deadline !== (o.startedAt as number) + 300_000 ||
      !Number.isSafeInteger(o.backoffMs) || (o.backoffMs as number) < 0 || (o.backoffMs as number) > 480_000 ||
      !["capturing", "captured", "adopting", "ready-for-model", "blocked"].includes(o.stage as string)) invalid();
  recoverySource(o.source);
  if (o.successor !== undefined) recoverySource(o.successor);
  if (o.restoreTip !== undefined && (typeof o.restoreTip !== "string" || !/^[0-9a-f]{40}$/.test(o.restoreTip))) invalid();
  if (["captured", "adopting", "ready-for-model"].includes(o.stage as string) && o.restoreTip === undefined) invalid();
  if (o.stage === "ready-for-model" && o.successor === undefined) invalid();
  if (o.blocker !== undefined && (!["capture_failed", "source_missing", "adoption_failed", "budget_exhausted", "clock_invalid", "oversize", "prerequisites_unavailable", "quiescence_failed", "preservation_failed", "decoded_history_limit"].includes(o.blocker as string))) invalid();
  if (o.stage === "blocked" && o.blocker === undefined) invalid();
  return o as unknown as RecoveryProgress;
}

export function sameRecoverySource(a: RecoverySource, b: RecoverySource): boolean {
  return a.runId === b.runId && a.clonePath === b.clonePath && a.attemptId === b.attemptId;
}

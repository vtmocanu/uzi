// Captured verbatim from the base revision a1454259ded86a8eb4c1d48f68113d1d56c302b0
// (`git show a1454259:agent/src/git.ts`, function parseRecoveryJournal). It stands for the strict-allowlist
// parser of a worker built BEFORE the completion-source fence existed: the accepted rollback fence is that
// such a worker refuses a journal carrying any key it does not know. Do not edit the function body.
import { ATTEMPT_ID_RE } from "../../src/attempt-path.js";
import { recoveryProgress, recoverySource, sameRecoverySource, type RecoveryProgress, type RecoverySource } from "../../src/recovery-progress.js";

const SHA40_RE = /^[0-9a-f]{40}$/;
const MAX_PEELED_MARKERS = 64;

export interface BaseRecoveryJournalEntry extends RecoverySource {
  recovery?: RecoveryProgress;
  retainedSources?: RecoverySource[];
  peeledMarkers?: string[];
}
type RecoveryJournalEntry = BaseRecoveryJournalEntry;

export function parseRecoveryJournal(value: string): RecoveryJournalEntry {
  const parsed: unknown = JSON.parse(value);
  if (typeof parsed !== "object" || parsed === null || !("runId" in parsed) || !("clonePath" in parsed)
      || typeof parsed.runId !== "string" || typeof parsed.clonePath !== "string") {
    throw new Error("invalid retained recovery clone journal");
  }
  const attemptId = "attemptId" in parsed ? parsed.attemptId : undefined;
  if (attemptId !== undefined && (typeof attemptId !== "string" || !ATTEMPT_ID_RE.test(attemptId))) {
    throw new Error("invalid retained recovery clone journal");
  }
  const o = parsed as Record<string, unknown>;
  if (Object.keys(o).some(k => !["runId", "clonePath", "attemptId", "restoreTip", "recovery", "retainedSources", "peeledMarkers"].includes(k))) {
    throw new Error("invalid retained recovery clone journal");
  }
  // Legacy callers classify a mismatched attempt id as CapturePathMismatchError.
  // Episode descriptors require strict identity before they can authorize any write.
  const source: RecoverySource = { ...recoverySource({ runId: parsed.runId, clonePath: parsed.clonePath,
    ...(o.recovery !== undefined || o.retainedSources !== undefined ? { attemptId } : {}),
    ...(o.restoreTip === undefined ? {} : { restoreTip: o.restoreTip }) }),
    ...(attemptId === undefined ? {} : { attemptId }) };
  const recovery = o.recovery === undefined ? undefined : recoveryProgress(o.recovery);
  let retainedSources: RecoverySource[] | undefined;
  if (o.retainedSources !== undefined) {
    if (!Array.isArray(o.retainedSources)) throw new Error("invalid retained sources");
    retainedSources = o.retainedSources.map(recoverySource);
  }
  if (recovery && !sameRecoverySource(source, recovery.source) &&
      !(recovery.successor && sameRecoverySource(source, recovery.successor)) &&
      !retainedSources?.some(s => sameRecoverySource(source, s))) throw new Error("recovery source identity mismatch");
  if (recovery?.stage === "ready-for-model" &&
      (!recovery.successor || !sameRecoverySource(source, recovery.successor))) throw new Error("recovery successor identity mismatch");
  if (recovery && ["captured", "adopting", "ready-for-model"].includes(recovery.stage) &&
      source.restoreTip !== recovery.restoreTip) throw new Error("recovery restore tip mismatch");
  if (recovery && ["adopting", "ready-for-model"].includes(recovery.stage) &&
      recovery.successor?.restoreTip !== recovery.restoreTip) throw new Error("recovery successor tip mismatch");
  if (recovery && (recovery.source.runId !== source.runId ||
      (recovery.successor && recovery.successor.runId !== source.runId))) throw new Error("recovery run identity mismatch");
  if (retainedSources?.some(s => s.runId !== source.runId)) throw new Error("retained run identity mismatch");
  let peeledMarkers: string[] | undefined;
  if (o.peeledMarkers !== undefined) {
    if (!Array.isArray(o.peeledMarkers) || o.peeledMarkers.length > MAX_PEELED_MARKERS ||
        !o.peeledMarkers.every((m): m is string => typeof m === "string" && SHA40_RE.test(m)) ||
        new Set(o.peeledMarkers).size !== o.peeledMarkers.length) throw new Error("invalid peeled markers");
    peeledMarkers = [...o.peeledMarkers];
  }
  return { ...source, ...(recovery ? { recovery } : {}), ...(retainedSources ? { retainedSources } : {}),
    ...(peeledMarkers?.length ? { peeledMarkers } : {}) };
}

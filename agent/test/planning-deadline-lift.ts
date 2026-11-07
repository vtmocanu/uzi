// Issue #3543 validation-only support for the base-discovery measurement harness
// (planning-base-discovery.measure.ts): lifts the planning capture helper's wall-clock bounds in
// the helper SOURCE the harness hands to its spawner, so a near-both-caps base can run to its
// cap under the unchanged production caps and memory bounds. Nothing in agent/src changes and no
// production flag exists: the lift is a text transform applied only inside the harness spawn
// wrapper, and it fails closed unless the source matches the expected shape exactly.
//
// This module is pure (no CLI, no I/O) so the unit test can import it; the measure harness
// runs its CLI at import time and cannot be imported.

export const LIFTED_HELPER_DEADLINE_MS = 1_800_000;
export const LIFTED_COMMAND_TIMER_MS = 1_800_000;
export const LIFTED_OUTER_TIMEOUT_MS = 1_830_000;
export const LIFTED_SCOPE_TIMEOUT_MS = 1_860_000;
export const PRODUCTION_OUTER_TIMEOUT_MS = 30_000;

const DEADLINE_ORIGINAL = "const deadline = Date.now() + 28000;";
const DEADLINE_LIFTED = `const deadline = Date.now() + ${LIFTED_HELPER_DEADLINE_MS};`;
const COMMAND_TIMER_ORIGINAL = "Math.max(0, Math.min(5000, deadline - Date.now()))";
const COMMAND_TIMER_LIFTED = `Math.max(0, Math.min(${LIFTED_COMMAND_TIMER_MS}, deadline - Date.now()))`;
const CAP_MARKER = "base_discovery_entry_cap";

function occurrences(source: string, needle: string): number {
  return source.split(needle).length - 1;
}

/**
 * Returns the helper source with its 28 s deadline and its per-git-command 5 s timer ceiling
 * lifted. Throws unless each of the two markers occurs exactly once and the source carries the
 * base-discovery cap token (so a stale or unrelated program is never "lifted" silently), and
 * unless undoing the two replacements reproduces the input byte for byte.
 */
export function liftPlanningHelperDeadline(source: string): string {
  if (typeof source !== "string") throw new Error("planning helper source must be a string");
  for (const [name, marker] of [["deadline", DEADLINE_ORIGINAL], ["per-command timer", COMMAND_TIMER_ORIGINAL]] as const) {
    const n = occurrences(source, marker);
    if (n !== 1) throw new Error(`planning helper ${name} marker must occur exactly once, found ${n}`);
  }
  if (!source.includes(CAP_MARKER)) throw new Error(`planning helper source lacks ${CAP_MARKER}: not the base-discovery helper`);
  const lifted = source.replace(DEADLINE_ORIGINAL, () => DEADLINE_LIFTED).replace(COMMAND_TIMER_ORIGINAL, () => COMMAND_TIMER_LIFTED);
  const expectedDelta = (DEADLINE_LIFTED.length - DEADLINE_ORIGINAL.length) + (COMMAND_TIMER_LIFTED.length - COMMAND_TIMER_ORIGINAL.length);
  if (lifted.length - source.length !== expectedDelta) throw new Error("planning helper lift changed an unexpected number of bytes");
  const undone = lifted.split(DEADLINE_LIFTED).join(DEADLINE_ORIGINAL).split(COMMAND_TIMER_LIFTED).join(COMMAND_TIMER_ORIGINAL);
  if (undone !== source) throw new Error("planning helper lift is not reversible: the source changed somewhere else");
  return lifted;
}

// ---------------------------------------------------------------------------------------------
// Phase evidence (lift rows have no helper instrumentation in control mode, so a parent-side
// /proc observer records when the sizing batch ran and when the index open or the next git
// spawn followed). Times are ms relative to the job start, or null when never observed.

export interface Phases {
  sizingSeenMs: number | null;
  sizingGoneMs: number | null;
  indexOpenSeenMs: number | null;
  postSizingSpawnSeenMs: number | null;
  sizingPid: number | null;
}

export function emptyPhases(): Phases {
  return { sizingSeenMs: null, sizingGoneMs: null, indexOpenSeenMs: null, postSizingSpawnSeenMs: null, sizingPid: null };
}

export type PhaseEvidence = "completed" | "post_sizing_observed";

export const SIZING_BATCH_ARG = "--batch-check=%(objectname) %(objecttype) %(objectsize)";

export function isSizingArgv(argv: readonly string[]): boolean {
  const at = argv.indexOf("cat-file");
  return at >= 0 && argv[at + 1] === SIZING_BATCH_ARG;
}

export function isGitArgv(argv: readonly string[]): boolean {
  const exe = argv[0] ?? "";
  return exe.slice(exe.lastIndexOf("/") + 1) === "git";
}

/**
 * Completed capture, or a refusal observed AFTER the sizing batch ended: the batch was seen before
 * it was gone, the index open or a later non-cat-file git spawn was seen at or after it was gone,
 * and every time is no later than the outcome. Anything else is null: the row proves nothing
 * about the post-sizing window.
 */
export function phaseEvidenceOf(outcomeKind: unknown, phases: Phases | null, outcomeMs: number | null): PhaseEvidence | null {
  if (outcomeKind === "completed") return "completed";
  if (outcomeKind !== "refused" || phases === null || outcomeMs === null) return null;
  const { sizingSeenMs, sizingGoneMs, indexOpenSeenMs, postSizingSpawnSeenMs } = phases;
  if (sizingSeenMs === null || sizingGoneMs === null || sizingSeenMs > sizingGoneMs) return null;
  const after = [indexOpenSeenMs, postSizingSpawnSeenMs].filter((v): v is number => v !== null);
  if (after.length === 0 || after.some((v) => v < sizingGoneMs)) return null;
  return [sizingSeenMs, sizingGoneMs, ...after].every((v) => v <= outcomeMs) ? "post_sizing_observed" : null;
}

export interface HelperPhaseMetrics {
  t0?: unknown; sizingBytes?: unknown; sizingCloseMs?: unknown; boundaryMs?: unknown;
}

export const OBSERVER_SLACK_MS = 60;

/**
 * Contradictions between the /proc observer and the helper's own metering preamble (full lift
 * rows only). A phase the observer missed is never a mismatch; only an observation the preamble
 * rules out is. Observer times are relative to jobStartedAtMs (epoch); preamble times are
 * relative to the helper's t0 (epoch).
 */
export function phaseObserverMismatches(phases: Phases, jobStartedAtMs: number, helper: HelperPhaseMetrics | null | undefined): string[] {
  if (!helper || typeof helper.t0 !== "number") return [];
  const abs = (rel: number | null): number | null => (rel === null ? null : jobStartedAtMs + rel);
  const helperAbs = (rel: unknown): number | null => (typeof rel === "number" ? (helper.t0 as number) + rel : null);
  const out: string[] = [];
  const sizingClose = helperAbs(helper.sizingCloseMs);
  const boundary = helperAbs(helper.boundaryMs);
  if (phases.sizingSeenMs !== null && sizingClose === null && boundary !== null && helper.sizingBytes === 0) {
    out.push("PHASE_OBSERVER_MISMATCH: observer saw the sizing batch but the preamble recorded no sizing spawn although discovery completed");
  }
  if (sizingClose !== null) {
    for (const [name, rel] of [["index open", phases.indexOpenSeenMs], ["post-sizing spawn", phases.postSizingSpawnSeenMs]] as const) {
      const at = abs(rel);
      if (at !== null && at < sizingClose - OBSERVER_SLACK_MS) {
        out.push(`PHASE_OBSERVER_MISMATCH: observer saw the ${name} ${sizingClose - at} ms before the preamble's sizing close`);
      }
    }
  }
  const gone = abs(phases.sizingGoneMs);
  if (gone !== null && boundary !== null && gone > boundary + OBSERVER_SLACK_MS) {
    out.push(`PHASE_OBSERVER_MISMATCH: observer saw the sizing batch still running ${gone - boundary} ms after the preamble's index-open boundary`);
  }
  return out;
}

// ---------------------------------------------------------------------------------------------
// Row validity: the only record a lift row may be reported from.

export interface RowVerdict { valid: boolean; reasons: string[] }

type Rec = Record<string, unknown>;
const isRec = (v: unknown): v is Rec => typeof v === "object" && v !== null && !Array.isArray(v);

export const LIFT_LABEL_MARKER = "__validation-lift";

/**
 * expect: "completed" | "phase-evidence" | a refusal token (e.g. base_discovery_entry_cap).
 * Valid only for a clean container exit, a clean record and an outcome matching the expectation.
 */
export function verifyRow(result: unknown, term: unknown, label: string, expect: string): RowVerdict {
  const reasons: string[] = [];
  if (!isRec(result)) reasons.push("result record missing or unreadable");
  if (!isRec(term)) reasons.push("term record missing or unreadable");
  if (!isRec(result) || !isRec(term)) return { valid: false, reasons };

  if (term.exitCode !== "0") reasons.push(`container exitCode ${JSON.stringify(term.exitCode)} is not "0"`);
  if (term.oomKilled !== false) reasons.push("container was OOM killed (or the flag is absent)");
  if (term.killedByHarness !== false) reasons.push("container was killed by the harness (or the flag is absent)");
  if (term.label !== label) reasons.push(`term label ${JSON.stringify(term.label)} is not ${JSON.stringify(label)}`);
  if (result.label !== label) reasons.push(`result label ${JSON.stringify(result.label)} is not ${JSON.stringify(label)}`);
  if (result.failed !== false) reasons.push("result.failed is not false");
  if (!Array.isArray(result.failures)) reasons.push("result.failures is not an array");
  else if (result.failures.length > 0) {
    reasons.push(`result carries failures: ${result.failures.map(String).join("; ")}`);
    if (result.failures.some((f) => String(f).startsWith("PHASE_OBSERVER_MISMATCH"))) reasons.push("PHASE_OBSERVER_MISMATCH");
  }

  const lift = result.deadlineLift;
  if (label.includes(LIFT_LABEL_MARKER)) {
    const want = { helperDeadlineMs: LIFTED_HELPER_DEADLINE_MS, commandTimerMs: LIFTED_COMMAND_TIMER_MS,
      outerTimeoutMs: LIFTED_OUTER_TIMEOUT_MS, scopeTimeoutMs: LIFTED_SCOPE_TIMEOUT_MS };
    if (!isRec(lift)) reasons.push("lift row has no deadlineLift record");
    else {
      for (const [k, v] of Object.entries(want)) if (lift[k] !== v) reasons.push(`deadlineLift.${k} is ${JSON.stringify(lift[k])}, expected ${v}`);
      if (Object.keys(lift).length !== Object.keys(want).length) reasons.push("deadlineLift has unexpected keys");
    }
    if (result.validation !== true) reasons.push("lift row is not marked validation");
  } else if (lift !== undefined && lift !== null) reasons.push("non-lift row carries a deadlineLift record");

  const outcome = isRec(result.outcome) ? result.outcome : undefined;
  if (!outcome) reasons.push("result.outcome missing");
  if (expect === "completed") {
    if (outcome?.kind !== "completed") reasons.push(`outcome kind ${JSON.stringify(outcome?.kind)} is not completed`);
  } else if (expect === "phase-evidence") {
    const pe = result.phaseEvidence;
    if (pe !== "completed" && pe !== "post_sizing_observed") {
      reasons.push("PHASE_UNPROVEN: no phase evidence recorded for this row");
    } else if (pe === "completed" && outcome?.kind !== "completed") {
      reasons.push(`PHASE_UNPROVEN: phaseEvidence completed but outcome kind is ${JSON.stringify(outcome?.kind)}`);
    } else if (pe === "post_sizing_observed" && outcome?.kind !== "refused") {
      reasons.push(`PHASE_UNPROVEN: phaseEvidence post_sizing_observed but outcome kind is ${JSON.stringify(outcome?.kind)}`);
    }
  } else {
    if (outcome?.kind !== "refused" || outcome.token !== expect) {
      reasons.push(`outcome ${JSON.stringify(outcome?.kind)}/${JSON.stringify(outcome?.token)} is not a refusal with token ${expect}`);
    }
    if (expect.startsWith("base_discovery_")) {
      const derived = isRec(result.derived) ? result.derived : undefined;
      if (!derived || !isRec(derived.charge)) reasons.push("derived.charge is null for a base-discovery cap refusal");
    }
    if (result.mode === "full") {
      const fetched = isRec(result.treeEntriesFetched) ? result.treeEntriesFetched : undefined;
      if (!fetched || typeof fetched.value !== "number") reasons.push("treeEntriesFetched.value is not a number for a full-mode refusal");
    }
  }
  return { valid: reasons.length === 0, reasons };
}

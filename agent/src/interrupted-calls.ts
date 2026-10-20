import type { RunnerClone } from "./git.js";
import { INVISIBLE_SRC, type TextRedactor } from "./redact.js";

/** Registration evidence is independent of negotiated capabilities and completion custody. */
export type InterruptedRegistration = Readonly<{ workerId: string; nonce: string; epoch: number }>;
type Entry = Readonly<{
  seq: number; source_generation: number; agent: string; agent_instance: string | null;
  agent_label: string | null; tool_name: string; command: string; display_omitted: boolean;
}>;
export type InterruptedCalls = Readonly<{
  format: 1; run_id: string; worker_id: string; register_nonce: string; claim_generation: number;
  source_generation: number | null; inspected_count: number; history_incomplete: true;
  unavailable: boolean; truncated: boolean; entries: readonly Entry[];
}>;
export type InterruptedSnapshot = Readonly<{
  base: "interrupted" | "generic" | "default";
  interruptedEdits: boolean;
  commits: number | null;
}>;

const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
export function interruptedRegistration(workerId: unknown, nonce: unknown, epoch: number): InterruptedRegistration | undefined {
  if (typeof workerId !== "string" || !uuid.test(workerId) || typeof nonce !== "string" ||
      !nonce || Buffer.byteLength(nonce) > 128 || !wellFormed(nonce) || /[\p{Cc}\p{Cf}]/u.test(nonce)) return;
  return Object.freeze({ workerId: workerId.toLowerCase(), nonce, epoch });
}
export function interruptedEligible(kind: string, generation: number): boolean {
  return Number.isSafeInteger(generation) && generation >= 2 &&
    ["issue", "ci_fix", "self_improve", "prompt", "task", "mr_rework"].includes(kind);
}
export function interruptedSnapshot(generation: number, clone: Pick<RunnerClone, "seededFrom" | "priorCommits" | "wipRecovered">): InterruptedSnapshot | undefined {
  if (!Number.isSafeInteger(generation) || generation < 2) return;
  const base = clone.seededFrom === "tracking" || clone.seededFrom === "checkpoint" ? "interrupted" :
    clone.seededFrom === "origin" ? "generic" : "default";
  if (base === "default" && !clone.wipRecovered) return;
  return Object.freeze({
    base, interruptedEdits: clone.wipRecovered === true,
    commits: base !== "generic" && Number.isSafeInteger(clone.priorCommits) && clone.priorCommits >= 0 ? clone.priorCommits : null,
  });
}
function record(v: unknown): v is Record<string, unknown> { return v !== null && typeof v === "object" && !Array.isArray(v); }
function keys(v: Record<string, unknown>, expected: string): boolean {
  const names = expected.split(",");
  return Object.keys(v).length === names.length && names.every(k => Object.hasOwn(v, k));
}
function integer(v: unknown, min: number, max: number): v is number { return Number.isSafeInteger(v) && (v as number) >= min && (v as number) <= max; }
function wellFormed(v: string): boolean { return !/[\uD800-\uDFFF]/u.test(v); }
function display(v: unknown): v is string { return typeof v === "string" && wellFormed(v) && [...v].length <= 128; }

/** No coercion, partial salvage, raw IDs, or unknown extensions at this untrusted seam. */
export function decodeInterruptedCalls(value: unknown, runId: string, generation: number, identity: InterruptedRegistration): InterruptedCalls | undefined {
  if (!uuid.test(runId) || !interruptedEligible("issue", generation) || !record(value) ||
      !keys(value, "format,run_id,worker_id,register_nonce,claim_generation,source_generation,inspected_count,history_incomplete,unavailable,truncated,entries") ||
      value.format !== 1 || value.run_id !== runId || value.worker_id !== identity.workerId ||
      value.register_nonce !== identity.nonce || value.claim_generation !== generation ||
      !integer(value.claim_generation, 2, Number.MAX_SAFE_INTEGER) ||
      !(value.source_generation === null || integer(value.source_generation, 1, generation - 1)) ||
      !integer(value.inspected_count, 0, 256) || value.history_incomplete !== true ||
      typeof value.unavailable !== "boolean" || typeof value.truncated !== "boolean" ||
      !Array.isArray(value.entries) || value.entries.length > 3 || value.entries.length > value.inspected_count ||
      (value.source_generation === null ? value.entries.length !== 0 : value.inspected_count === 0)) return;
  let previous = 2147483648;
  const entries: Entry[] = [];
  for (const e of value.entries) {
    if (!record(e) || !keys(e, "seq,source_generation,agent,agent_instance,agent_label,tool_name,command,display_omitted") ||
        !integer(e.seq, 1, 2147483647) || e.seq >= previous || e.source_generation !== value.source_generation ||
        !display(e.agent) || !(e.agent_instance === null || display(e.agent_instance)) ||
        !(e.agent_label === null || display(e.agent_label)) || !display(e.tool_name) ||
        typeof e.command !== "string" || !wellFormed(e.command) || Buffer.byteLength(e.command) > 2048 ||
        (!e.command && !value.unavailable) || typeof e.display_omitted !== "boolean") return;
    previous = e.seq;
    entries.push(Object.freeze({ ...e }) as Entry);
  }
  return Object.freeze({ ...value, entries: Object.freeze(entries) }) as InterruptedCalls;
}

// Normalize the COMPLETE bounded source before whole-string redaction. Sanitation may join
// characters, so redact again before enforcing display bytes (including the omission marker).
function project(text: string, redact: TextRedactor, cap: number): string {
  const normalized = text.normalize("NFKC").replace(/\r\n?|\u2028|\u2029/gu, "\n");
  const safe = redact(redact(normalized).replace(new RegExp(INVISIBLE_SRC, "gu"), ""));
  if (Buffer.byteLength(safe) <= cap) return safe;
  const marker = "[omitted]";
  let out = "";
  for (const cp of safe) {
    if (Buffer.byteLength(out) + Buffer.byteLength(cp) + Buffer.byteLength(marker) > cap) break;
    out += cp;
  }
  return out + marker;
}
function projectCalls(calls: InterruptedCalls, redact: TextRedactor): InterruptedCalls {
  return Object.freeze({ ...calls, entries: Object.freeze(calls.entries.map(e => Object.freeze({
    ...e, agent: project(e.agent, redact, 512),
    agent_instance: e.agent_instance === null ? null : project(e.agent_instance, redact, 512),
    agent_label: e.agent_label === null ? null : project(e.agent_label, redact, 512),
    tool_name: project(e.tool_name, redact, 512), command: project(e.command, redact, 2048),
  }))) });
}

/** One setup read; checks live authority outside the optional-IO catch, including on failure.
 * The getter repeats all checks and current redaction after any future planning wait. */
export async function prepareInterruptedCalls(options: {
  kind: string; generation: number; fetch(): Promise<InterruptedCalls | undefined>;
  check(): boolean; redact: TextRedactor;
}): Promise<() => InterruptedCalls | undefined> {
  let evidence: InterruptedCalls | undefined;
  if (interruptedEligible(options.kind, options.generation) && options.check()) {
    try { evidence = await options.fetch(); } catch { /* Optional observation; no error content escapes. */ }
    if (!options.check()) evidence = undefined;
  }
  return () => {
    try {
      if (!options.check()) evidence = undefined;
    } catch (err) {
      evidence = undefined;
      throw err;
    }
    return evidence ? projectCalls(evidence, options.redact) : undefined;
  };
}

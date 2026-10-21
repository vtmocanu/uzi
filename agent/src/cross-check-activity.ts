import path from "node:path";
import { isUnsafeIdentifierChar } from "./codex/broker.js";
import { boundUtf8 } from "./codex/projection.js";

/**
 * Cross-check activity evidence (PRD #3166): a bounded, fixed-schema record of which repository
 * paths a checker read or searched. Payloads carry paths, line ranges and counts only: no
 * query, pattern, glob or file content ever reaches the recorder, so none can reach a row.
 */

const EVENT_ACTIVITY_START = "check_activity_start";
const EVENT_ACTIVITY = "check_activity";
const EVENT_ACTIVITY_SUMMARY = "check_activity_summary";

export type ActivityOp = "read" | "search";
export type ActivityOutcome = "ok" | "denied" | "oversize" | "limit" | "error";
export type ActivityCategory = "outside_checkout" | "tool_output" | "policy";

const ACTIVITY_OUTCOMES: readonly ActivityOutcome[] = ["ok", "denied", "oversize", "limit", "error"];
const ACTIVITY_CATEGORIES: readonly ActivityCategory[] = ["outside_checkout", "tool_output", "policy"];
const ACTIVITY_OPS: readonly ActivityOp[] = ["read", "search"];

const ACTIVITY_EVENT_CAP = 200;
const ACTIVITY_PENDING_CAP = 256;
const ACTIVITY_ID_MAX_BYTES = 128;
const ACTIVITY_PATH_MAX_BYTES = 512;
export const ACTIVITY_PAYLOAD_MAX_BYTES = 1024;

export type OutcomeCounts = Record<ActivityOutcome, number>;

export interface ActivitySettle {
  outcome: ActivityOutcome;
  /** Inclusive 1-based line range actually returned; omit when unknown, binary, replayed or empty. */
  lines?: readonly [number, number];
  matches?: number;
  matchesTruncated?: boolean;
  /** Denial category when the recorder's own path resolution does not decide one. */
  category?: ActivityCategory;
}

export interface ActivityRecorderOptions {
  /** The checkout root; may be supplied later through {@link CheckActivityRecorder.attach}. */
  checkout?: string;
  /** The checker HOME directory that holds SDK tool-output spill files. */
  toolOutputDir?: string;
  cap?: number;
  generation: number;
}

const FORMAT_CHAR = /\p{Cf}/u;

function isUnsafeChar(ch: string): boolean {
  return isUnsafeIdentifierChar(ch.codePointAt(0) ?? 0) || FORMAT_CHAR.test(ch);
}

function stripUnsafe(raw: string): string {
  let out = "";
  for (const ch of raw) if (!isUnsafeChar(ch)) out += ch;
  return out;
}

function hasUnsafe(raw: string): boolean {
  for (const ch of raw) if (isUnsafeChar(ch)) return true;
  return false;
}

function sanitizePath(raw: string): string {
  return boundUtf8(stripUnsafe(raw), ACTIVITY_PATH_MAX_BYTES);
}

function within(root: string, target: string): string | undefined {
  const rel = path.relative(root, target);
  if (rel === "") return ".";
  if (rel === ".." || rel.startsWith(".." + path.sep) || path.isAbsolute(rel)) return undefined;
  return rel.split(path.sep).join("/");
}

function zeroCounts(): OutcomeCounts {
  return { ok: 0, denied: 0, oversize: 0, limit: 0, error: 0 };
}

interface Pending {
  op: ActivityOp;
  path?: string;
  category?: ActivityCategory;
}

export class CheckActivityRecorder {
  private checkout: string | undefined;
  private toolOutputDir: string | undefined;
  private readonly cap: number;
  private readonly generation: number;
  private readonly pending = new Map<string, Pending>();
  private readonly reads = zeroCounts();
  private readonly searches = zeroCounts();
  private emitted = 0;
  private dropped = 0;
  private untracked = 0;
  private started = false;
  private modelRan = false;
  private summarized = false;

  constructor(private readonly emit: (payload: Record<string, unknown>) => void, opts: ActivityRecorderOptions) {
    this.attach(opts.checkout, opts.toolOutputDir);
    this.cap = opts.cap ?? ACTIVITY_EVENT_CAP;
    this.generation = opts.generation;
  }

  /** Set the roots paths resolve against (the checkout exists only after the marker is emitted). */
  attach(checkout: string | undefined, toolOutputDir?: string): void {
    this.checkout = checkout ? path.resolve(checkout) : undefined;
    this.toolOutputDir = toolOutputDir ? path.resolve(toolOutputDir) : undefined;
  }

  /** Emit the start marker once. Telemetry never throws into the check. */
  start(): void {
    if (this.started) return;
    try {
      this.emit({ event: EVENT_ACTIVITY_START, v: 1, cap: this.cap, generation: this.generation });
      this.started = true;
    } catch { /* telemetry only */ }
  }

  /** Record that the checker's model session has started: the Claude session's validated init
   *  frame, or a confirmed Codex turn start. A check that ends before this reports an incomplete
   *  capture. Never throws. */
  modelStarted(): void {
    this.modelRan = true;
  }

  /** Register a tool call. Nothing is emitted until it settles. */
  begin(id: unknown, op: ActivityOp, rawPath?: unknown): void {
    const key = this.validId(id);
    if (key === undefined || this.pending.size >= ACTIVITY_PENDING_CAP) {
      if (key === undefined || !this.pending.has(key)) this.untracked++;
      return;
    }
    if (this.pending.has(key)) return;
    this.pending.set(key, { op, ...this.resolve(op, rawPath) });
  }

  settle(id: unknown, result: ActivitySettle): void {
    const key = this.validId(id);
    if (key === undefined) return;
    const entry = this.pending.get(key);
    if (!entry) return;
    this.pending.delete(key);
    // An outcome outside the closed set is counted and emitted as `error`, never as the raw value.
    const outcome: ActivityOutcome = ACTIVITY_OUTCOMES.includes(result.outcome) ? result.outcome : "error";
    const counts = entry.op === "read" ? this.reads : this.searches;
    counts[outcome]++;
    if (this.emitted >= this.cap) { this.dropped++; return; }
    const payload = this.build(entry, result, outcome);
    if (!payload) { this.dropped++; return; }
    try {
      this.emit(payload);
      this.emitted++;
    } catch { this.dropped++; }
  }

  /** Emit the final summary once. Calls still pending are counted `unsettled`. */
  summary(family: string, stage: string, sessionEnded: boolean): void {
    if (this.summarized) return;
    this.summarized = true;
    const unsettled = this.pending.size;
    const complete = this.started && this.modelRan && sessionEnded && unsettled === 0 && this.untracked === 0;
    try {
      this.emit({
        event: EVENT_ACTIVITY_SUMMARY, generation: this.generation,
        family: sanitizePath(family).slice(0, 32) || "unknown", stage: sanitizePath(stage).slice(0, 32) || "unknown",
        reads: { ...this.reads }, searches: { ...this.searches },
        dropped: this.dropped, untracked: this.untracked, unsettled, complete,
      });
    } catch { /* telemetry only */ }
  }

  private validId(id: unknown): string | undefined {
    if (typeof id !== "string") return undefined;
    const bytes = Buffer.byteLength(id, "utf8");
    return bytes >= 1 && bytes <= ACTIVITY_ID_MAX_BYTES ? id : undefined;
  }

  private resolve(op: ActivityOp, raw: unknown): { path?: string; category?: ActivityCategory } {
    if (typeof raw !== "string" || raw.length === 0) return op === "search" ? { path: "." } : {};
    // Decide on the raw string first: a path carrying any character the sanitizer would strip is
    // never emitted (stripping afterwards could turn an outside path into an inside-looking one).
    if (hasUnsafe(raw)) return { category: "policy" };
    const abs = path.resolve(this.checkout ?? "/", raw);
    const inside = this.checkout ? within(this.checkout, abs) : undefined;
    if (inside !== undefined) {
      // Never truncate: a cut path could leave part of a secret in clear past the redactor.
      if (Buffer.byteLength(inside, "utf8") > ACTIVITY_PATH_MAX_BYTES) return { category: "policy" };
      return { path: inside.length > 0 ? inside : "." };
    }
    if (this.toolOutputDir && within(this.toolOutputDir, abs) !== undefined) return { category: "tool_output" };
    return { category: "outside_checkout" };
  }

  private build(entry: Pending, result: ActivitySettle, outcome: ActivityOutcome): Record<string, unknown> | undefined {
    const payload: Record<string, unknown> = { event: EVENT_ACTIVITY, op: entry.op, outcome };
    const category = entry.category ?? result.category;
    // A guard-denied or out-of-checkout attempt carries a category, never the path (PRD #3166).
    if (entry.path !== undefined && category === undefined) payload["path"] = entry.path;
    const lines = result.lines;
    if (entry.op === "read" && lines && Number.isInteger(lines[0]) && Number.isInteger(lines[1])
      && lines[0] >= 1 && lines[1] >= lines[0]) payload["lines"] = [lines[0], lines[1]];
    if (entry.op === "search" && typeof result.matches === "number" && Number.isInteger(result.matches) && result.matches >= 0) {
      payload["matches"] = result.matches;
      if (result.matchesTruncated === true) payload["matches_truncated"] = true;
    }
    if (category) payload["category"] = category;
    if (Buffer.byteLength(JSON.stringify(payload), "utf8") > ACTIVITY_PAYLOAD_MAX_BYTES) {
      // A path whose JSON escapes expand past the bound is omitted rather than the event dropped.
      delete payload["path"];
      if (Buffer.byteLength(JSON.stringify(payload), "utf8") > ACTIVITY_PAYLOAD_MAX_BYTES) return undefined;
    }
    return payload;
  }
}

// ---- Shared wording (the web page, `run logs` and `run get` render these same strings) ----

const CATEGORY_LABEL: Record<ActivityCategory, string> = {
  outside_checkout: "outside checkout",
  tool_output: "tool output",
  policy: "policy",
};

/** One activity line, e.g. `read api/x.go L1-200 · ok`, `search api/internal/forge · 3 matches · ok`,
 *  `read denied (outside checkout)`. Pure; the payload is treated as untrusted. */
export function formatActivityLine(p: Record<string, unknown>): string {
  const op: ActivityOp = ACTIVITY_OPS.includes(p["op"] as ActivityOp) ? p["op"] as ActivityOp : "read";
  const outcome: ActivityOutcome = ACTIVITY_OUTCOMES.includes(p["outcome"] as ActivityOutcome) ? p["outcome"] as ActivityOutcome : "error";
  const rawCategory = p["category"];
  const category = typeof rawCategory === "string" && ACTIVITY_CATEGORIES.includes(rawCategory as ActivityCategory)
    ? CATEGORY_LABEL[rawCategory as ActivityCategory] : undefined;
  const pathText = typeof p["path"] === "string" ? stripUnsafe(p["path"]) || undefined : undefined;
  if (outcome === "denied" && pathText === undefined) return `${op} denied${category ? ` (${category})` : ""}`;
  let subject = `${op} ${pathText ?? (category ? `(${category})` : "")}`.trimEnd();
  const lines = p["lines"];
  if (Array.isArray(lines) && typeof lines[0] === "number" && typeof lines[1] === "number") {
    subject += ` L${lines[0]}-${lines[1]}`;
  }
  const parts = [subject];
  if (typeof p["matches"] === "number") {
    parts.push(`${p["matches"]}${p["matches_truncated"] === true ? "+" : ""} ${p["matches"] === 1 && p["matches_truncated"] !== true ? "match" : "matches"}`);
  }
  parts.push(outcome);
  return parts.join(" · ");
}

function total(counts: unknown): number {
  if (!counts || typeof counts !== "object") return 0;
  let sum = 0;
  for (const v of Object.values(counts as Record<string, unknown>)) if (typeof v === "number") sum += v;
  return sum;
}

/** The summary text, e.g. `4 reads, 0 searches, 0 dropped, complete`. */
export function formatActivitySummary(p: Record<string, unknown>): string {
  const reads = total(p["reads"]);
  const searches = total(p["searches"]);
  const dropped = typeof p["dropped"] === "number" ? p["dropped"] : 0;
  return `${reads} ${reads === 1 ? "read" : "reads"}, ${searches} ${searches === 1 ? "search" : "searches"}, ${dropped} dropped, ${p["complete"] === true ? "complete" : "incomplete"}`;
}

import type { WorkerRunMessage } from "./protocol.js";

// Read-side evidence only. No inferred shell gates, ownership, timeout or budget.
const INTERRUPTIONS = new Set([
  "delegation stopped by the run; child settlement not confirmed",
  "delegation still open at turn end; child settlement not confirmed",
  "delegation ended by a provider stream failure; child settlement not confirmed",
]);
// 60 single-event slots plus six atomic failed pairs (12 snippets): <= 57,671 prose chars.
const QUOTAS = { opening: 6, tail: 12, plan: 4, revision: 4, steering: 4, failure: 8, review: 10, signals: 12 } as const;
type Class = keyof typeof QUOTAS;
type Outcome = "success" | "failed" | "interrupted" | "unknown";
type Observation = { seq: number; text: string };
type Row = {
  key: string; id: string; role: string; instance: string; start: number; time: string;
  end?: number; endTime?: string; outcome: Outcome | "incomplete"; ambiguous: boolean;
  childTools: number; startText: string; cause?: string;
};
type History = { tainted: boolean; evicted: boolean; outcome?: Outcome };
type Pending = { ambiguous?: boolean; row?: Row; result?: { seq: number; time: string; outcome: Outcome; text: string; cause?: string } };
type ToolCounts = { lead: number; child: number; unknown: number };

export interface JudgeFetchMeta {
  pages: number;
  complete: boolean;
  reason: string;
  cap?: number;
}
export interface JudgeEvidence {
  sections: { coverage: string; ledger: string; aggregates: string; prose: string };
  counts: {
    fetched: number; excluded: number; eligible: number; retained: number; omitted: number;
    toolUses: number; toolResults: number; dispatches: number;
    resultErrorFlags: { true: number; false: number; unknown: number };
    confirmed: Record<Outcome, number>; provisional: number; collisions: number;
    oversizedKeys: number; evictions: number; extraResults: number;
    unmatchedResults: number; incomplete: number; omittedRows: number;
  };
  metrics: { pending: number; history: number; historySaturated: boolean; rows: number;
    toolIdentities: number; reservoirEvents: number; retainedChars: number; stateStringChars: number;
    pendingRows: number; failedPairs: number };
}

/** Own bounded string storage: V8 slices can otherwise retain an entire payload.
 * JSON round-tripping preserves exact UTF-16 identities, including lone surrogates;
 * UTF-8 conversion would silently change those keys. */
function ownedPrefix(s: string, cap: number): string {
  return JSON.parse(JSON.stringify(s.slice(0, cap))) as string;
}

/** Escapes at the boundary, before enforcing the rendered character budget. */
function clip(s: string, cap: number): string {
  return s.length <= cap ? ownedPrefix(s, cap) : ownedPrefix(s, cap - 12) + "…[truncated]";
}
function quoted(s: string, cap: number): string {
  return clip(JSON.stringify(s.slice(0, cap)).replace(/[\u007f-\u009f\u061c\u200e-\u200f\u2028-\u202e\u2066-\u2069]/g,
    c => "\\u" + c.charCodeAt(0).toString(16).padStart(4, "0")), cap);
}
function record(v: unknown): Record<string, unknown> | undefined {
  return v !== null && typeof v === "object" && !Array.isArray(v) ? v as Record<string, unknown> : undefined;
}
function str(v: unknown): string { return typeof v === "string" ? v : ""; }

/** Visits at most 48 entries, depth 3 and 1200 source characters. Never serializes a raw object. */
function preview(value: unknown): string {
  let entries = 48, chars = 1200;
  function visit(v: unknown, depth: number): string {
    if (chars <= 0 || entries-- <= 0) return "[omitted]";
    if (typeof v === "string") {
      const part = v.slice(0, Math.min(chars, 800));
      chars -= part.length;
      return quoted(part, 800);
    }
    if (v === null || typeof v === "boolean" || typeof v === "number") return String(v);
    if (typeof v !== "object" || depth === 0) return "[omitted]";
    const parts: string[] = [];
    for (const key in v) {
      if (!Object.hasOwn(v, key)) continue;
      if (parts.length >= 12 || entries <= 0 || chars <= 0) { parts.push("[omitted]"); break; }
      chars -= Math.min(key.length, 80);
      parts.push(quoted(key, 80) + ":" + visit((v as Record<string, unknown>)[key], depth - 1));
    }
    return (Array.isArray(v) ? "[" : "{") + parts.join(",") + (Array.isArray(v) ? "]" : "}");
  }
  return clip(visit(value, 3), 600);
}
function snippet(m: WorkerRunMessage): string {
  const time = timestamp(str(m.created_at));
  return clip("#" + m.seq + " time=" + quoted(Number.isFinite(time) ? m.created_at : "unknown", 48) + " " + quoted(m.kind, 40) + " role=" + quoted(m.agent ?? "unknown", 50)
    + " instance=" + quoted(m.agent_instance ?? "unknown", 90) + " " + preview(m.payload), 800);
}
function outcome(p: Record<string, unknown>): Outcome {
  if (p["is_error"] !== true && p["is_error"] !== false) return "unknown";
  const content = p["content"];
  if (typeof content === "string" && content.length <= 100 && INTERRUPTIONS.has(content)) return "interrupted";
  // Persisted tool_result content is text or an SDK array of text blocks.
  const recognized = typeof content === "string" || (Array.isArray(content) && content.length > 0
    && content.length <= 32 && content.every(v => {
      const r = record(v);
      return r?.["type"] === "text" && typeof r["text"] === "string";
    }));
  return recognized ? (p["is_error"] ? "failed" : "success") : "unknown";
}
// Exact accounting capsule keys from harness-messages/projectResult and usage stamps.
// Any additional field keeps the status eligible; in particular failure facts survive.
const ACCOUNTING_KEYS = new Set(["event", "subtype", "num_turns", "duration_ms", "total_cost_usd",
  "usage", "modelUsage", "usage_basis", "leg_id", "usage_through"]);
function accountingOnly(m: WorkerRunMessage, p: Record<string, unknown>): boolean {
  if (m.kind !== "status") return false;
  if (p["event"] === "codex_response_usage") {
    const keys = new Set(["event", "usage_response_id", "usage", "model"]);
    let visited = 0;
    for (const key in p) if (Object.hasOwn(p, key) && (++visited > 4 || !keys.has(key))) return false;
    return true;
  }
  if (p["event"] !== "result" || p["subtype"] !== "success") return false;
  let visited = 0;
  for (const key in p) if (Object.hasOwn(p, key) && (++visited > 12 || !ACCOUNTING_KEYS.has(key))) return false;
  return true;
}
function lane(m: WorkerRunMessage): keyof ToolCounts {
  return m.agent_instance ? "child" : m.agent === "lead" ? "lead" : "unknown";
}
function timestamp(s: string): number {
  const iso = /^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d+)?(?:Z|[+-]\d\d:\d\d)$/;
  function parse(s: string): number {
    if (!iso.test(s)) return NaN;
    const day = new Date(s.slice(0, 10) + "T00:00:00Z");
    if (!Number.isFinite(day.getTime()) || day.toISOString().slice(0, 10) !== s.slice(0, 10)) return NaN;
    return Date.parse(s);
  }
  return parse(s);
}
function interval(row: Row): string {
  const a = timestamp(row.time), b = timestamp(row.endTime ?? "");
  return Number.isFinite(a) && Number.isFinite(b) && b >= a
    ? (b - a) + "ms persisted-event interval" : "unknown persisted-event interval";
}
function priority(r: Row): number {
  return r.ambiguous || r.outcome === "incomplete" || r.outcome === "failed" || r.outcome === "interrupted" ? 0
    : r.outcome === "unknown" ? 1 : 2;
}
function compare(a: Row, b: Row): number {
  return priority(a) - priority(b) || a.start - b.start || (a.key < b.key ? -1 : a.key > b.key ? 1 : 0);
}

/**
 * Feed persisted messages once, in increasing seq order; page boundaries have no effect.
 * Overlapping/replayed seqs are ignored, counted, and never imply a second dispatch.
 * Retained state: 256 history keys, 128 pending entries, 48 ledger rows, 64 tool
 * keys, 72 prose snippets; string slots stay below 600,000 chars (including copies).
 * Identity replay at a NEW seq is a collision. Pending FIFO eviction never forgets a
 * remembered dispatch tombstone. After history saturation unremembered IDs are provisional.
 */
export class JudgeEvidenceReducer {
  private first?: number;
  private last?: number;
  private duplicates = 0;
  private fetched = 0;
  private excluded = 0;
  private eligible = 0;
  private toolUses = 0;
  private toolResults = 0;
  private resultErrorFlags = { true: 0, false: 0, unknown: 0 };
  private dispatches = 0;
  private collisions = 0;
  private oversizedKeys = 0;
  private evictions = 0;
  private extraResults = 0;
  private orphanResultEvictions = 0;
  private resultPairs = 0;
  private matchedStarts = 0;
  private confirmed: Record<Outcome, number> = { success: 0, failed: 0, interrupted: 0, unknown: 0 };
  private history = new Map<string, History>();
  private pending = new Map<string, Pending>();
  private rows: Row[] = [];
  private tools = new Map<string, ToolCounts>();
  private overflow: ToolCounts = { lead: 0, child: 0, unknown: 0 };
  private reservoirs: Record<Class, Observation[]> = {
    opening: [], tail: [], plan: [], revision: [], steering: [], failure: [], review: [], signals: [],
  };
  private failedPairs: { start: Observation; end: Observation }[] = [];
  private boundary?: { seq: number; time: string; id: string };
  private afterDispatch = 0;

  accept(m: WorkerRunMessage): void {
    if (this.last !== undefined && m.seq <= this.last) { this.duplicates++; return; }
    this.first ??= m.seq;
    this.last = m.seq;
    this.fetched++;
    const p = m.payload ?? {};
    const name = str(p["name"]);
    const dispatch = m.kind === "tool_use" && (name === "Agent" || name === "Task");
    const text = snippet(m);
    const accounting = accountingOnly(m, p);
    if (m.kind === "progress_note" || m.kind === "summary_usage" || accounting) this.excluded++;
    else {
      this.eligible++;
      const obs = { seq: m.seq, text };
      this.keep("opening", obs);
      this.keep("tail", obs);
      if (m.kind === "plan" || p["event"] === "draft_plan_capture") this.keep("plan", obs);
      if (m.kind === "plan_feedback" || m.kind === "plan_revising") this.keep("revision", obs);
      if (["user_message", "steer_ack", "answer"].includes(m.kind)) this.keep("steering", obs);
      if (m.kind === "error" || (m.kind === "tool_result" && outcome(p) !== "success")) this.keep("failure", obs);
      // Reviews are available child reports, not a role-name severity classifier.
      if (m.kind === "text" && m.agent_instance) this.keep("review", obs);
      // Runner progress/completion often persist as status text, without structured codes.
      // Preserve status observations; never promote their prose to a gate verdict.
      if (["status", "question", "paused", "pause_failed", "limit_hit"].includes(m.kind)) this.keep("signals", obs);
    }
    if (m.kind === "tool_use") {
      this.toolUses++;
      const category = lane(m);
      let counts = name.length <= 256 ? this.tools.get(name) : undefined;
      if (!counts && name.length > 0 && name.length <= 256 && this.tools.size < 64) {
        counts = { lead: 0, child: 0, unknown: 0 }; this.tools.set(ownedPrefix(name, 256), counts);
      }
      (counts ?? this.overflow)[category]++;
      if (category === "lead" && this.boundary) this.afterDispatch++;
      if (m.agent_instance) {
        // A retained duplicate dispatch ID cannot establish unique lane attribution.
        const candidates = this.activeRows().filter(row => row.id === m.agent_instance);
        if (candidates.length === 1 && !candidates[0]!.ambiguous) candidates[0]!.childTools++;
      }
    }
    if (m.kind === "tool_result") {
      this.toolResults++;
      const flag = p["is_error"] === true ? "true" : p["is_error"] === false ? "false" : "unknown";
      this.resultErrorFlags[flag]++;
    }
    if (dispatch) {
      this.dispatches++;
      this.boundary = { seq: m.seq, time: ownedPrefix(str(m.created_at), 64), id: ownedPrefix(str(p["id"]), 256) };
      this.afterDispatch = 0;
    }
    if (!dispatch && m.kind !== "tool_result") return;
    const id = str(p[dispatch ? "id" : "tool_use_id"]);
    const instance = m.agent_instance ?? "";
    // Length-prefixed lane prevents delimiter collisions. Never truncate a matching key.
    const category = lane(m);
    const keyLength = category.length + 1 + String(instance.length).length + 1 + instance.length + id.length;
    if (!id || id.length > 256 || instance.length > 256 || keyLength > 256) {
      this.oversizedKeys++;
      if (dispatch) this.addRow({ key: "#" + m.seq, id: clip(id, 80),
        role: ownedPrefix(str(record(p["input"])?.["subagent_type"]), 256) || "unknown",
        instance: clip(instance, 80), start: m.seq, time: ownedPrefix(str(m.created_at), 64),
        outcome: "incomplete", ambiguous: true, childTools: 0, startText: text });
      return;
    }
    const key = ownedPrefix(category + ":" + instance.length + ":" + instance + id, 256);
    let h = this.history.get(key);
    if (dispatch) {
      if (h) {
        this.collisions++;
        h.tainted = true;
        if (h.outcome) { this.confirmed[h.outcome]--; delete h.outcome; }
        for (const row of this.activeRows()) if (row.key === key) row.ambiguous = true;
      } else if (this.history.size < 256) {
        h = { tainted: false, evicted: this.orphanResultEvictions > 0 }; this.history.set(key, h);
      }
      const old = this.pending.get(key);
      if (old?.ambiguous && h) h.tainted = true;
      const row: Row = { key, id: ownedPrefix(id, 256), instance: ownedPrefix(instance, 256),
        role: ownedPrefix(str(record(p["input"])?.["subagent_type"]), 256) || "unknown",
        start: m.seq, time: ownedPrefix(str(m.created_at), 64), outcome: "incomplete",
        ambiguous: category === "unknown" || !h || h.tainted || h.evicted, childTools: 0, startText: text };
      this.addRow(row);
      if (old?.result && !old.row) {
        this.match(row, old.result, h); this.pending.delete(key);
      } else {
        this.pending.delete(key);
        this.put(key, { row });
      }
    } else {
      const result = { seq: m.seq, time: ownedPrefix(str(m.created_at), 64), outcome: outcome(p), text,
        cause: typeof p["content"] === "string" && INTERRUPTIONS.has(p["content"])
          ? p["content"] === "delegation stopped by the run; child settlement not confirmed" ? "run stopped; settlement unconfirmed"
            : p["content"] === "delegation still open at turn end; child settlement not confirmed" ? "open at turn end; settlement unconfirmed"
              : "provider stream failure; settlement unconfirmed" : undefined };
      const old = this.pending.get(key);
      if (old?.row) {
        this.match(old.row, result, h); this.pending.delete(key);
      } else if (h || old?.result) {
        this.extraResults++;
        if (old) old.ambiguous = true;
        if (h) {
          if (h.outcome) { this.confirmed[h.outcome]--; delete h.outcome; }
          h.tainted = true;
          for (const row of this.activeRows()) if (row.key === key) row.ambiguous = true;
        }
      } else this.put(key, { result });
    }
  }

  private activeRows(): Row[] {
    return [...new Set([...this.rows, ...Array.from(this.pending.values(), p => p.row).filter((r): r is Row => !!r)])];
  }
  private put(key: string, value: Pending): void {
    if (this.pending.size >= 128) {
      const oldest = this.pending.keys().next().value!;
      const evicted = this.pending.get(oldest);
      if (evicted?.row) evicted.row.ambiguous = true;
      this.pending.delete(oldest);
      const h = this.history.get(oldest);
      if (h) h.evicted = true;
      else if (evicted?.result) this.orphanResultEvictions++;
      this.evictions++;
    }
    this.pending.set(key, value);
  }
  private match(row: Row, result: NonNullable<Pending["result"]>, h: History | undefined): void {
    this.resultPairs++;
    this.matchedStarts++;
    row.end = result.seq; row.endTime = result.time; row.outcome = result.outcome; row.cause = result.cause;
    row.ambiguous ||= !h || h.tainted || h.evicted;
    if (!row.ambiguous && result.outcome !== "unknown") {
      this.confirmed[result.outcome]++; h!.outcome = result.outcome;
    }
    // Successful dispatch results can be the only available review/validation report.
    // Preserve the correlated brief/result, without parsing its prose for findings.
    this.keep("review", { seq: row.start, text: row.startText });
    this.keep("review", { seq: result.seq, text: result.text });
    if (result.outcome !== "success") {
      // Six earliest failed pairs, kept atomically, independent of generic error floods.
      this.failedPairs.push({ start: { seq: row.start, text: row.startText },
        end: { seq: result.seq, text: result.text } });
      this.failedPairs.sort((a, b) => a.start.seq - b.start.seq);
      this.failedPairs = this.failedPairs.slice(0, 6);
    }
    this.addRow(row);
  }
  private addRow(row: Row): void {
    const candidates = [...this.rows.filter(r => r.start !== row.start), row].sort(compare);
    // Reserve a few representative successes even under a failure flood.
    const successes = candidates.filter(r => r.outcome === "success" && !r.ambiguous).slice(0, 4);
    const rest = candidates.filter(r => !successes.includes(r)).slice(0, 48 - successes.length);
    this.rows = [...rest, ...successes].sort(compare);
  }
  private keep(cls: Class, obs: Observation): void {
    const list = this.reservoirs[cls];
    if (list.some(o => o.seq === obs.seq)) return;
    list.push(obs);
    list.sort((a, b) => a.seq - b.seq);
    if (list.length > QUOTAS[cls]) {
      if (cls === "opening" || cls === "failure") list.pop(); else list.shift();
    }
  }

  private stateStringChars(): number {
    let chars = 0;
    for (const key of this.history.keys()) chars += key.length;
    for (const [key, p] of this.pending) {
      chars += key.length;
      if (p.result) chars += p.result.time.length + p.result.text.length + (p.result.cause?.length ?? 0);
    }
    for (const r of this.activeRows()) {
      chars += r.key.length + r.id.length + r.role.length + r.instance.length + r.time.length
        + (r.endTime?.length ?? 0) + r.startText.length + (r.cause?.length ?? 0);
    }
    for (const key of this.tools.keys()) chars += key.length;
    for (const list of Object.values(this.reservoirs)) for (const o of list) chars += o.text.length;
    for (const pair of this.failedPairs) chars += pair.start.text.length + pair.end.text.length;
    return chars + (this.boundary?.id.length ?? 0) + (this.boundary?.time.length ?? 0);
  }

  render(meta?: JudgeFetchMeta): JudgeEvidence {
    const observations = new Map<number, string>();
    for (const list of Object.values(this.reservoirs)) for (const obs of list) observations.set(obs.seq, obs.text);
    for (const pair of this.failedPairs) {
      observations.set(pair.start.seq, pair.start.text); observations.set(pair.end.seq, pair.end.text);
    }
    const retained = observations.size;
    const provisional = this.resultPairs - Object.values(this.confirmed).reduce((a, b) => a + b, 0);
    const incomplete = this.dispatches - this.matchedStarts;
    const counts = {
      fetched: this.fetched, excluded: this.excluded, eligible: this.eligible, retained,
      omitted: this.eligible - retained, toolUses: this.toolUses, toolResults: this.toolResults,
      dispatches: this.dispatches, resultErrorFlags: { ...this.resultErrorFlags },
      confirmed: { ...this.confirmed }, provisional,
      collisions: this.collisions, oversizedKeys: this.oversizedKeys, evictions: this.evictions,
      extraResults: this.extraResults, unmatchedResults: this.toolResults - this.resultPairs,
      incomplete, omittedRows: this.dispatches - this.rows.length,
    };
    const ledger = this.rows.slice().sort(compare).map(r =>
      clip("start=#" + r.start + " end=" + (r.end === undefined ? "unknown" : "#" + r.end)
        + " role=" + quoted(r.role, 20) + " instance=" + quoted(r.id, 32)
        + " outcome=" + (r.ambiguous ? "unknown ambiguous" : r.outcome)
        + " startTime=" + quoted(Number.isFinite(timestamp(r.time)) ? r.time : "unknown", 44)
        + " endTime=" + quoted(Number.isFinite(timestamp(r.endTime ?? "")) ? r.endTime! : "unknown", 44)
        + " " + interval(r) + " child_tool_uses=" + r.childTools
        + " deadline=unknown cause=" + (r.ambiguous || !r.cause ? "unknown" : quoted(r.cause, 50)), 374)).join("\n");
    const aggregates = clip("Raw observations; tool uses and result-error flags are not gate, ownership or delegation-success proof.\n"
      + JSON.stringify(counts) + "\nTool identity counts:\n"
      + [...this.tools].map(([name, c]) => quoted(name, 60) + " " + JSON.stringify(c)).join("\n")
      + "\noverflow " + JSON.stringify(this.overflow)
      + "\nLead tool uses after LAST observed dispatch=" + this.afterDispatch
      + " boundary=" + (this.boundary ? "#" + this.boundary.seq + " time=" + quoted(this.boundary.time, 80)
        + " id=" + quoted(this.boundary.id, 100) : "unknown"), 8000);
    const coverage = clip("Fetched seq=" + (this.first ?? "unknown") + ".." + (this.last ?? "unknown")
      + " count=" + this.fetched + " pages=" + (meta?.pages ?? "unknown")
      + " completion-known=" + (meta?.complete ? "yes" : "no") + " complete=" + (meta?.complete ?? "unknown")
      + " reason=" + quoted(meta?.reason ?? "unknown", 180) + " cap=" + (meta?.cap ?? "unknown")
      + "\nSampling eligible=" + this.eligible + " excluded=" + this.excluded + " retained=" + retained
      + " omitted=" + counts.omitted + "; sample omissions are not unfetched events."
      + "\nPairing confirmed=" + JSON.stringify(this.confirmed) + " provisional=" + provisional
      + " incomplete=" + incomplete + " unmatched raw results=" + counts.unmatchedResults
      + " observedExtraResults=" + this.extraResults + " orphanResultEvictions=" + this.orphanResultEvictions
      + (this.orphanResultEvictions ? " (later untracked identities provisional)" : "") + " pending=" + this.pending.size + " evictions=" + this.evictions
      + " tombstones=" + this.history.size + " historySaturated=" + (this.history.size === 256)
      + " collisions=" + this.collisions + " oversizedKeys=" + this.oversizedKeys + " ignoredSeqs=" + this.duplicates
      + "\nLedger rows=" + this.rows.length + " omittedRows=" + counts.omittedRows
      + "\nTail is latest fetched eligible evidence; " + (meta?.complete ? "fetch complete." : "final delivery unknown."), 2000);
    const prose = [...observations].sort(([a], [b]) => a - b).map(([, s]) => s).join("\n");
    return { sections: { coverage, ledger, aggregates, prose }, counts,
      metrics: { pending: this.pending.size, history: this.history.size, historySaturated: this.history.size === 256,
        rows: this.rows.length, toolIdentities: this.tools.size,
        reservoirEvents: Object.values(this.reservoirs).reduce((n, a) => n + a.length, 0) + this.failedPairs.length * 2,
        pendingRows: Array.from(this.pending.values()).filter(p => p.row).length,
        failedPairs: this.failedPairs.length,
        // Count retained string slots, including duplicates, rather than raw-payload size.
        stateStringChars: this.stateStringChars(),
        retainedChars: prose.length + ledger.length + aggregates.length + coverage.length } };
  }
}

import { memo, useEffect, useState } from "react";
import type { RunMessage } from "../lib/api";
import { Markdown } from "./Markdown";

// Terse, per-kind rendering of a run's event stream — one readable line per
// event instead of a JSON dump. Kinds come from agent/src/sdk-messages.ts (the
// SDK mapping) plus the runner's own worker emissions:
//   text | thinking | tool_use | tool_result | status | error | plan
// Anything else falls through to a muted unknown-kind line (RunMessage.kind is a
// free string; the raw frame is always available in the worker's debug log).
//
// The pure helpers (toolSummary, resultToText, formatDuration, describeStatus,
// describeError, buildToolIndex) are exported and unit-tested in isolation.

// ── payload probes ──────────────────────────────────────────────────────────

function asRecord(v: unknown): Record<string, unknown> | undefined {
  return v && typeof v === "object" ? (v as Record<string, unknown>) : undefined;
}
function asString(v: unknown): string | undefined {
  return typeof v === "string" ? v : undefined;
}
function firstLine(s: string): string {
  const i = s.indexOf("\n");
  return i === -1 ? s : s.slice(0, i);
}
const SUMMARY_MAX = 160;
export function truncate(s: string, max = SUMMARY_MAX): string {
  return s.length > max ? `${s.slice(0, max - 1).trimEnd()}…` : s;
}

// compactValue renders a single tool-input value inline (no whole-frame JSON).
function compactValue(v: unknown): string {
  if (typeof v === "string") return v;
  if (typeof v === "number" || typeof v === "boolean") return String(v);
  if (v == null) return "";
  try {
    return JSON.stringify(v);
  } catch {
    return String(v);
  }
}

// ── tool summaries ──────────────────────────────────────────────────────────

// toolSummary derives a one-line argument gist for a tool call. Sub-agent spawns
// (Task) surface their subagent_type so they are recognizable. Unknown tools
// degrade to compact key: value pairs. The result is NOT truncated — callers
// truncate for display and offer expansion.
export function toolSummary(name: string | undefined, input: unknown): string {
  const rec = asRecord(input) ?? {};
  const s = (k: string): string => asString(rec[k]) ?? "";
  switch (name) {
    case "Bash":
      return firstLine(s("command"));
    case "Read":
    case "Write":
    case "Edit":
    case "MultiEdit":
      return s("file_path") || s("path");
    case "NotebookEdit":
      return s("notebook_path") || s("file_path");
    case "Glob":
    case "Grep": {
      const pat = s("pattern");
      const where = s("path");
      return where ? `${pat} in ${where}` : pat;
    }
    case "Task": {
      const kind = s("subagent_type");
      const gist = firstLine(s("description") || s("prompt"));
      return kind ? (gist ? `${kind}: ${gist}` : kind) : gist;
    }
    case "WebFetch":
      return s("url");
    case "WebSearch":
      return s("query");
    default: {
      const pairs = Object.entries(rec).map(([k, v]) => `${k}: ${compactValue(v)}`);
      return pairs.join(", ");
    }
  }
}

// ── tool results ────────────────────────────────────────────────────────────

// resultToText flattens a tool_result's content — a string or an array of blocks
// (mapUser passes SDK content through as-is) — into displayable text, reporting
// whether a non-text block (e.g. an image) was dropped as a known lost signal.
export function resultToText(content: unknown): { text: string; hadNonText: boolean } {
  if (typeof content === "string") return { text: content, hadNonText: false };
  if (Array.isArray(content)) {
    const parts: string[] = [];
    let hadNonText = false;
    for (const block of content) {
      const rec = asRecord(block);
      const t = rec && rec["type"] === "text" ? asString(rec["text"]) : undefined;
      if (t !== undefined) parts.push(t);
      else hadNonText = true;
    }
    return { text: parts.join("\n"), hadNonText };
  }
  return { text: "", hadNonText: false };
}

// ── durations ───────────────────────────────────────────────────────────────

// formatDuration renders a millisecond span terse: "0.8s", "12.4s", "1m 05s".
export function formatDuration(ms: number): string {
  if (!Number.isFinite(ms) || ms < 0) ms = 0;
  const secs = ms / 1000;
  if (secs < 60) return `${secs.toFixed(1)}s`;
  const m = Math.floor(secs / 60);
  const s = Math.round(secs - m * 60);
  return `${m}m ${String(s).padStart(2, "0")}s`;
}

function elapsedMs(startIso: string, now: number): number {
  const start = new Date(startIso).getTime();
  return Number.isFinite(start) ? Math.max(0, now - start) : 0;
}

// ── status / error text ─────────────────────────────────────────────────────

// describeStatus maps a `status` payload to a human line. Two shapes reach here:
// worker progress `{text}` (already human-readable) and SDK frames `{event}`
// (init heartbeat, or the terminal result discriminated by `subtype`).
export function describeStatus(payload: unknown): string {
  const rec = asRecord(payload);
  if (!rec) return "status";
  const text = asString(rec["text"]);
  if (text !== undefined) return text;
  const event = asString(rec["event"]);
  if (event === "init") {
    const model = asString(rec["model"]);
    return model ? `agent started (${model})` : "agent started";
  }
  if (event === "result") {
    const subtype = asString(rec["subtype"]) ?? "unknown";
    if (subtype === "success") {
      const bits: string[] = [];
      if (typeof rec["duration_ms"] === "number") bits.push(formatDuration(rec["duration_ms"]));
      if (typeof rec["num_turns"] === "number") bits.push(`${rec["num_turns"]} turns`);
      if (typeof rec["total_cost_usd"] === "number")
        bits.push(`$${(rec["total_cost_usd"] as number).toFixed(2)}`);
      return bits.length ? `agent finished (${bits.join(", ")})` : "agent finished";
    }
    return `status: result/${subtype}`;
  }
  return event ? `status: ${event}` : "status";
}

// describeError maps an `error` payload to a line. Two shapes: worker `{text}`
// and the SDK result `{event, subtype, errors[]}` (subtype is the useful signal:
// error_max_turns vs error_during_execution).
export function describeError(payload: unknown): string {
  const rec = asRecord(payload);
  if (!rec) return "error";
  const text = asString(rec["text"]);
  if (text !== undefined) return text;
  const subtype = asString(rec["subtype"]);
  const errors = Array.isArray(rec["errors"]) ? rec["errors"].map(String).filter(Boolean) : [];
  const joined = errors.join("; ");
  if (subtype && joined) return `${subtype}: ${joined}`;
  return subtype || joined || "error";
}

// ── tool_use ↔ tool_result pairing ──────────────────────────────────────────

export interface ToolIndex {
  /** tool_use.id → its tool_result message. */
  resultByUseId: Map<string, RunMessage>;
  /** Every tool_use.id present in the stream (to detect folded vs orphan results). */
  toolUseIds: Set<string>;
}

// buildToolIndex pairs results to calls by id — never by adjacency, so parallel
// calls (useA, useB, resA, resB) map A→resA and B→resB correctly. A result whose
// tool_use_id is not in toolUseIds is an orphan (rendered standalone).
export function buildToolIndex(messages: RunMessage[]): ToolIndex {
  const resultByUseId = new Map<string, RunMessage>();
  const toolUseIds = new Set<string>();
  for (const m of messages) {
    if (m.kind === "tool_use") {
      const id = asString(asRecord(m.payload)?.["id"]);
      if (id) toolUseIds.add(id);
    } else if (m.kind === "tool_result") {
      const id = asString(asRecord(m.payload)?.["tool_use_id"]);
      if (id && !resultByUseId.has(id)) resultByUseId.set(id, m);
    }
  }
  return { resultByUseId, toolUseIds };
}

// ── collapsible primitive ───────────────────────────────────────────────────

function Expander({
  open,
  onToggle,
  label,
}: {
  open: boolean;
  onToggle: () => void;
  label: string;
}) {
  return (
    <button
      type="button"
      aria-expanded={open}
      onClick={onToggle}
      className="text-[11px] font-medium text-slate-500 hover:text-slate-300"
    >
      {label}
    </button>
  );
}

const RESULT_INLINE_LINES = 8;

// ── per-kind rows ───────────────────────────────────────────────────────────

function ToolResultBody({ result }: { result: RunMessage }) {
  const rec = asRecord(result.payload) ?? {};
  const isError = rec["is_error"] === true;
  const { text, hadNonText } = resultToText(rec["content"]);
  const lines = text === "" ? 0 : text.split("\n").length;
  const large = lines > RESULT_INLINE_LINES;
  // Errors auto-expand — a failed tool is exactly what the user wants to see.
  const [open, setOpen] = useState(isError || !large);

  const mark = isError ? "✗" : "✓";
  const markClass = isError ? "text-rose-400" : "text-emerald-400";
  const body = text || (hadNonText ? "" : "(no output)");

  return (
    <div className={`mt-1 rounded-md border px-2 py-1 ${isError ? "border-rose-900/70 bg-rose-950/30" : "border-slate-800 bg-slate-900/50"}`}>
      <div className="flex items-center gap-2">
        <span className={`text-xs font-semibold ${markClass}`}>
          <span aria-hidden="true">{mark}</span>{" "}
          <span>{isError ? "error" : "result"}</span>
        </span>
        {large && (
          <Expander
            open={open}
            onToggle={() => setOpen((o) => !o)}
            label={open ? "hide" : `show ${lines} lines`}
          />
        )}
      </div>
      {open && body !== "" && (
        <pre className="mt-1 max-h-72 overflow-auto whitespace-pre-wrap break-words text-xs text-slate-300">
          {body}
        </pre>
      )}
      {hadNonText && <div className="mt-1 text-[11px] italic text-slate-500">non-text result (e.g. image) omitted</div>}
    </div>
  );
}

function RunningIndicator({ start }: { start: string }) {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const id = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(id);
  }, []);
  return (
    <span className="inline-flex items-center gap-1.5 text-xs text-amber-300">
      <span
        aria-hidden="true"
        className="inline-block h-3 w-3 animate-spin rounded-full border-2 border-amber-800 border-t-amber-300"
      />
      running… {formatDuration(elapsedMs(start, now))}
    </span>
  );
}

function ToolUseRow({ msg, result, live }: { msg: RunMessage; result?: RunMessage; live: boolean }) {
  const rec = asRecord(msg.payload) ?? {};
  const name = asString(rec["name"]) ?? "tool";
  const full = toolSummary(name, rec["input"]);
  const [open, setOpen] = useState(false);
  const truncated = full.length > SUMMARY_MAX;

  return (
    <div className="text-sm">
      <div className="flex flex-wrap items-baseline gap-x-2 gap-y-1">
        <span className="shrink-0 font-medium text-slate-200">
          <span aria-hidden="true">⚙</span> {name}
        </span>
        {full && (
          <span className="min-w-0 break-words font-mono text-xs text-slate-400">
            {open ? full : truncate(full)}
          </span>
        )}
        {truncated && (
          <Expander open={open} onToggle={() => setOpen((o) => !o)} label={open ? "less" : "more"} />
        )}
        <span className="ml-auto shrink-0">
          {result ? (
            <span className="text-xs text-slate-500">
              {formatDuration(new Date(result.created_at).getTime() - new Date(msg.created_at).getTime())}
            </span>
          ) : live ? (
            <RunningIndicator start={msg.created_at} />
          ) : (
            <span className="text-xs italic text-slate-600">no result</span>
          )}
        </span>
      </div>
      {result && <ToolResultBody result={result} />}
    </div>
  );
}

function ThinkingRow({ text }: { text: string }) {
  const [open, setOpen] = useState(false);
  return (
    <div className="text-sm text-slate-500">
      <div className="flex items-baseline gap-2">
        <span className="italic">💭 thinking… {truncate(firstLine(text), 100)}</span>
        <Expander open={open} onToggle={() => setOpen((o) => !o)} label={open ? "hide" : "show"} />
      </div>
      {open && (
        <pre className="mt-1 whitespace-pre-wrap break-words text-xs italic text-slate-500">{text}</pre>
      )}
    </div>
  );
}

function StandaloneResult({ result }: { result: RunMessage }) {
  const id = asString(asRecord(result.payload)?.["tool_use_id"]);
  return (
    <div className="text-sm">
      <div className="text-xs text-slate-500">
        result{id ? ` for ${truncate(id, 24)}` : ""}
      </div>
      <ToolResultBody result={result} />
    </div>
  );
}

// RunEventRow renders one message. Memoized (props: msg is immutable per seq;
// result flips undefined→object once when a tool call completes; live flips once
// when the run leaves running) so an append never re-renders settled rows.
export const RunEventRow = memo(function RunEventRow({
  msg,
  result,
  live,
}: {
  msg: RunMessage;
  result?: RunMessage;
  live: boolean;
}) {
  const rec = asRecord(msg.payload);
  switch (msg.kind) {
    case "text": {
      const text = asString(rec?.["text"]) ?? "";
      return text ? <Markdown content={text} /> : null;
    }
    case "thinking":
      return <ThinkingRow text={asString(rec?.["text"]) ?? ""} />;
    case "tool_use":
      return <ToolUseRow msg={msg} result={result} live={live} />;
    case "tool_result":
      // Only reached for orphan results; folded ones are skipped by the parent.
      return <StandaloneResult result={msg} />;
    case "status":
      return <div className="text-xs italic text-slate-500">{describeStatus(msg.payload)}</div>;
    case "error":
      return (
        <div className="rounded-md border border-rose-900/70 bg-rose-950/40 px-2 py-1 text-sm text-rose-200">
          <span aria-hidden="true">✗</span> {describeError(msg.payload)}
        </div>
      );
    case "plan":
      return <div className="text-xs italic text-slate-500">📋 plan submitted (awaiting approval)</div>;
    default: {
      const extract = asString(rec?.["text"]) ?? asString(rec?.["message"]) ?? "";
      return (
        <div className="text-xs text-slate-500">
          <span className="italic">unrenderable {msg.kind} event</span>
          {extract && <span className="ml-1 text-slate-400">— {truncate(extract)}</span>}
        </div>
      );
    }
  }
});

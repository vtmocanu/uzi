import { memo, useEffect, useRef, useState, type ReactNode } from "react";
import type { RunMessage } from "../lib/api";
import { Markdown } from "./Markdown";
import { cx } from "./ui";
import {
  BotIcon,
  CircleIcon,
  ExternalLinkIcon,
  FileTextIcon,
  SearchIcon,
  TerminalIcon,
  ThoughtIcon,
} from "./icons";

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
      // The full command is the source of truth (PRD #38 Decision 1); the
      // display layer (CommandBlock) clamps for presentation. firstLine() used
      // to truncate here, silently losing lines 2+ of any multi-line command.
      return s("command");
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

// ── shell highlighting + command block ──────────────────────────────────────

// Tailwind classes for each shell token class. Colours are the --syn-* tokens
// (index.css), which alias the palette so a theme recolours them for free.
const SYN_CLASS = {
  cmd: "text-syn-cmd",
  flag: "text-syn-flag",
  str: "text-syn-str",
  op: "text-syn-op",
  comment: "italic text-syn-comment",
  arg: "text-syn-arg",
} as const;
type SynClass = keyof typeof SYN_CLASS;

const SHELL_SPACE = new Set([" ", "\t", "\n", "\r", "\f", "\v"]);
const SHELL_OP_START = new Set(["&", "|", ";", "<", ">"]);

// Commands are untrusted and have no upstream length cap (PRD threat model), and
// this emits one DOM node per token — so a multi-MB command would freeze the tab
// (line-clamp is CSS-only and does not reduce node count). Highlight only the
// first HIGHLIGHT_MAX_CHARS / HIGHLIGHT_MAX_NODES, then append the exact
// remainder as ONE plain (unhighlighted) text node: text is preserved verbatim,
// DOM fan-out is bounded.
const HIGHLIGHT_MAX_CHARS = 8 * 1024;
const HIGHLIGHT_MAX_NODES = 2000;

// highlightShell is a pure, best-effort shell tokenizer (PRD #38 Decision 2). It
// splits a command into command / flag / string / operator / comment / arg spans
// and returns React elements only — never dangerouslySetInnerHTML — so untrusted
// LLM command text (e.g. a literal "<script>") renders inert as escaped text.
// Accuracy is cosmetic: a mis-classified token only mis-colours; the invariant
// that MATTERS and is tested is that the concatenated text equals the input
// exactly (no character is ever dropped or added).
export function highlightShell(command: string): ReactNode[] {
  const nodes: ReactNode[] = [];
  let key = 0;
  const emit = (cls: SynClass, text: string) => {
    nodes.push(
      <span key={key++} className={SYN_CLASS[cls]}>
        {text}
      </span>,
    );
  };

  const n = command.length;
  let i = 0;
  // The next bare word is a command name at the start of the string and after
  // any control operator (|, &&, ;, newline, …); otherwise it is an argument.
  let expectCommand = true;

  while (i < n) {
    // Fan-out guard: once the char or node budget is spent, dump the untouched
    // tail as a single text node and stop tokenizing (bounds DOM for a huge cmd).
    if (i >= HIGHLIGHT_MAX_CHARS || nodes.length >= HIGHLIGHT_MAX_NODES) {
      nodes.push(command.slice(i));
      break;
    }
    const c = command[i];

    // Whitespace run (preserved verbatim as a plain text node so pre-wrap keeps
    // the original spacing). A newline starts a fresh command context.
    if (SHELL_SPACE.has(c)) {
      let j = i + 1;
      while (j < n && SHELL_SPACE.has(command[j])) j++;
      const ws = command.slice(i, j);
      nodes.push(ws);
      if (ws.includes("\n")) expectCommand = true;
      i = j;
      continue;
    }

    // Comment: '#' only reaches this branch at a token boundary (a mid-word '#'
    // is consumed by the word scanner), so it runs to end of line.
    if (c === "#") {
      let j = i + 1;
      while (j < n && command[j] !== "\n") j++;
      emit("comment", command.slice(i, j));
      i = j;
      continue;
    }

    // Control / redirection operators (&& || | ; << < >> > &). Reset to command.
    if (SHELL_OP_START.has(c)) {
      const next = command[i + 1];
      const isDouble =
        (c === "&" && next === "&") ||
        (c === "|" && next === "|") ||
        (c === ">" && next === ">") ||
        (c === "<" && next === "<") ||
        (c === ";" && next === ";");
      const op = isDouble ? c + next : c;
      emit("op", op);
      i += op.length;
      expectCommand = true;
      continue;
    }

    // Single-quoted string (no escapes inside).
    if (c === "'") {
      let j = i + 1;
      while (j < n && command[j] !== "'") j++;
      if (j < n) j++; // include the closing quote
      emit("str", command.slice(i, j));
      i = j;
      expectCommand = false;
      continue;
    }

    // Double-quoted string (honours \" escapes).
    if (c === '"') {
      let j = i + 1;
      while (j < n) {
        if (command[j] === "\\" && j + 1 < n) {
          j += 2;
          continue;
        }
        if (command[j] === '"') {
          j++;
          break;
        }
        j++;
      }
      emit("str", command.slice(i, j));
      i = j;
      expectCommand = false;
      continue;
    }

    // A bare word: run until whitespace, an operator start, or a quote (so an
    // embedded quote becomes its own string token). A '#' inside a word stays.
    let j = i;
    while (j < n) {
      const cj = command[j];
      if (SHELL_SPACE.has(cj) || SHELL_OP_START.has(cj) || cj === "'" || cj === '"') break;
      j++;
    }
    const word = command.slice(i, j);
    if (expectCommand) {
      emit("cmd", word);
      expectCommand = false;
    } else if (word.startsWith("-")) {
      emit("flag", word);
    } else {
      emit("arg", word);
    }
    i = j;
  }

  return nodes;
}

// CommandBlock renders a Bash command on a dark code surface with a ❯ prompt and
// shell highlighting (PRD #38 Decisions 1–3). Long commands clamp to two lines
// behind a "Show full command" toggle. Overflow is decided by a CONTENT
// heuristic (a newline, or > SUMMARY_MAX chars), never layout measurement:
// jsdom reports no layout (scrollHeight === 0), so a measured toggle would be
// both untestable and flaky.
export function CommandBlock({ command }: { command: string }) {
  const clampable = command.includes("\n") || command.length > SUMMARY_MAX;
  const [expanded, setExpanded] = useState(false);
  const clamped = clampable && !expanded;
  return (
    <div className="mt-1.5">
      <div className="whitespace-pre-wrap break-words rounded-md border border-edge bg-ink px-2.5 py-2 font-mono text-xs leading-relaxed">
        <span aria-hidden="true" className="mr-2 select-none text-faint">
          ❯
        </span>
        <code className={clamped ? "line-clamp-2" : undefined}>{highlightShell(command)}</code>
      </div>
      {clampable && (
        <button
          type="button"
          aria-expanded={expanded}
          onClick={() => setExpanded((e) => !e)}
          className="mt-1 inline-flex min-h-[24px] items-center gap-1 rounded-md px-1.5 py-0.5 text-[11px] font-medium text-muted hover:bg-raised/60 hover:text-fg"
        >
          {expanded ? "Show less" : "Show full command"}
        </button>
      )}
    </div>
  );
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
      // Interactive label: --muted (~6.9:1, not the ~3.65:1 --faint) and a ≥24px
      // hit target with padding (WCAG 1.4.3 + 2.5.8, PRD #38 Decisions 9 + M4).
      className="inline-flex min-h-[24px] items-center rounded-md px-1.5 py-0.5 text-[11px] font-medium text-muted hover:bg-raised/60 hover:text-fg"
    >
      {label}
    </button>
  );
}

// MetaLine renders a status/meta message as a full-width, hairline-flanked divider
// (PRD #38 Decision 12). Single source of truth for status rendering: the feed no
// longer intercepts `status` — RunEventRow's status branch delegates here.
//
// Two-tone like the mock's `.meta .k`: the event-type key renders mono, text-fg,
// non-italic; the trailing parenthetical detail (model, finish stats) stays
// italic text-muted. describeStatus returns a flat "key (detail)" string, so we
// split on the first "(" — a worker progress line has no parenthetical, so the
// whole line is the key (still a scannable mono-fg anchor).
function MetaLine({ text }: { text: string }) {
  const paren = text.indexOf("(");
  const key = paren === -1 ? text : text.slice(0, paren).trimEnd();
  const detail = paren === -1 ? "" : text.slice(paren);
  return (
    <div className="flex items-center gap-2.5 py-0.5 text-xs italic text-muted">
      <span aria-hidden="true" className="h-px flex-1 bg-edge" />
      <span>
        <span className="font-mono text-[11px] not-italic text-fg">{key}</span>
        {detail ? ` ${detail}` : ""}
      </span>
      <span aria-hidden="true" className="h-px flex-1 bg-edge" />
    </div>
  );
}

// ── tool icons + duration ─────────────────────────────────────────────────────

// One type-appropriate icon per tool (PRD #38 Decision, "per-tool icons"); an
// unknown tool gets a neutral dot rather than masquerading as a shell command.
function toolIcon(name: string): ReactNode {
  switch (name) {
    case "Bash":
      return <TerminalIcon />;
    case "Read":
    case "Write":
    case "Edit":
    case "MultiEdit":
    case "NotebookEdit":
      return <FileTextIcon />;
    case "Grep":
    case "Glob":
      return <SearchIcon />;
    case "Task":
      return <BotIcon />;
    case "WebFetch":
    case "WebSearch":
      return <ExternalLinkIcon />;
    default:
      return <CircleIcon />;
  }
}

// Tool-duration thresholds. These live ONLY in the tool render path — the shared
// formatDuration (used by RunView's header/terminal line) is left untouched
// (PRD #38 Decision 6): sub-100ms collapses to "instant", and a span past a
// minute tints --warn.
const INSTANT_MS = 100;
const SLOW_MS = 60_000;

function ToolDuration({ msg, result, live }: { msg: RunMessage; result?: RunMessage; live: boolean }) {
  if (result) {
    const raw = new Date(result.created_at).getTime() - new Date(msg.created_at).getTime();
    const ms = Number.isFinite(raw) && raw >= 0 ? raw : 0;
    if (ms < INSTANT_MS) {
      // Suppress the meaningless "0.0s"; the raw value stays available in the title.
      return (
        <span className="ml-auto shrink-0 text-[11px] tabular-nums text-muted" title={`${Math.round(ms)}ms`}>
          instant
        </span>
      );
    }
    return (
      <span
        className={cx("ml-auto shrink-0 text-[11px] tabular-nums", ms >= SLOW_MS ? "text-warn" : "text-muted")}
      >
        {formatDuration(ms)}
      </span>
    );
  }
  if (live) {
    return (
      <span className="ml-auto shrink-0">
        <RunningIndicator start={msg.created_at} />
      </span>
    );
  }
  return <span className="ml-auto shrink-0 text-[11px] italic text-muted">no result</span>;
}

// ── per-kind rows ───────────────────────────────────────────────────────────

// ToolResultBody renders a result as a collapsed inline chip that expands into a
// focusable code block (PRD #38 Decision 13). Labels: "✗ error" / "✓ ok"
// (empty or whitespace-only output) / "✓ N lines". The body stays MOUNTED but
// hidden while collapsed, so its text is always in the DOM (pairing/test
// assertions); a dropped non-text block surfaces as a "[image omitted]" first
// line. Errors auto-expand with a danger tint.
function ToolResultBody({ result, toolName }: { result: RunMessage; toolName?: string }) {
  const rec = asRecord(result.payload) ?? {};
  const isError = rec["is_error"] === true;
  const { text, hadNonText } = resultToText(rec["content"]);
  const empty = text.trim() === "";
  const lineCount = empty ? 0 : text.split("\n").length;

  const [open, setOpen] = useState(isError);
  const bodyRef = useRef<HTMLPreElement>(null);
  const userToggled = useRef(false);
  // Move keyboard focus into the body only when the USER opens it — never on the
  // error auto-expand, which would steal focus as the feed renders.
  useEffect(() => {
    if (open && userToggled.current) bodyRef.current?.focus();
  }, [open]);

  const label = isError ? "error" : empty ? "ok" : `${lineCount} line${lineCount === 1 ? "" : "s"}`;
  // Name the tool in the a11y label when the call is known (paired), like the mock
  // ("Show 3 lines of Bash output"). An orphan result has no call, so it keeps the
  // tool-agnostic wording (PRD #38 M6 NIT).
  const of = toolName ? `${toolName} ` : "";
  const showLabel = isError
    ? `Show ${of}error output`
    : empty
      ? `Show ${of}output`
      : `Show ${label} of ${of}output`;
  const ariaLabel = open
    ? isError
      ? `Hide ${of}error output`
      : `Hide ${of}output`
    : showLabel;

  const bodyText = empty ? (hadNonText ? "" : "(no output)") : text;
  const body = [hadNonText ? "[image omitted]" : "", bodyText].filter(Boolean).join("\n");

  return (
    <div className="mt-1.5">
      <button
        type="button"
        aria-expanded={open}
        aria-label={ariaLabel}
        onClick={() => {
          userToggled.current = true;
          setOpen((o) => !o);
        }}
        className={cx(
          "inline-flex min-h-[24px] items-center gap-1.5 rounded-md border px-2 py-0.5 text-[11px] font-medium",
          isError
            ? "border-danger/40 bg-danger/10 text-danger"
            : "border-edge bg-raised/50 text-muted hover:border-edge-strong",
        )}
      >
        <span aria-hidden="true" className={cx("font-bold", isError ? "text-danger" : "text-ok")}>
          {isError ? "✗" : "✓"}
        </span>
        {label}
      </button>
      <pre
        ref={bodyRef}
        hidden={!open}
        tabIndex={0}
        role="group"
        aria-label={isError ? "Tool error output" : "Tool output"}
        className={cx(
          "mt-1.5 max-h-64 overflow-auto whitespace-pre-wrap break-words rounded-md border px-2.5 py-2 font-mono text-xs text-muted",
          isError ? "border-danger/40 bg-danger/[0.08]" : "border-edge bg-ink",
        )}
      >
        {body}
      </pre>
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
    <span className="inline-flex items-center gap-1.5 text-xs text-warn">
      <span
        aria-hidden="true"
        className="inline-block h-3 w-3 animate-spin rounded-full border-2 border-warn/30 border-t-warn"
      />
      running… {formatDuration(elapsedMs(start, now))}
    </span>
  );
}

function ToolUseRow({ msg, result, live }: { msg: RunMessage; result?: RunMessage; live: boolean }) {
  const rec = asRecord(msg.payload) ?? {};
  const name = asString(rec["name"]) ?? "tool";
  const full = toolSummary(name, rec["input"]);
  const isBash = name === "Bash";
  const [open, setOpen] = useState(false);
  // Bash routes its full (possibly multi-line) command through CommandBlock; a
  // non-Bash arg stays inline and keeps the >160-char "more" affordance so it is
  // never ellipsis-only (PRD #38 Decision 13). The header does NOT wrap, so the
  // duration (ml-auto shrink-0) can never float to its own line (Decision 6).
  const argOverflow = !isBash && full.length > SUMMARY_MAX;

  return (
    // The tool rail (border-l) is drawn once by the feed around a RUN of
    // consecutive tool/thinking rows (PRD #38 M4 rail consolidation), so the row
    // itself no longer carries its own border — that produced a segmented rail.
    <div className="text-sm">
      <div className="flex items-center gap-2">
        <span aria-hidden="true" className="inline-flex shrink-0 text-faint [font-size:14px]">
          {toolIcon(name)}
        </span>
        <span className="shrink-0 text-[11px] font-semibold uppercase tracking-wider text-muted">
          {name}
        </span>
        {!isBash && full && (
          <span className="min-w-0 break-words font-mono text-xs text-fg">
            {open ? full : truncate(full)}
          </span>
        )}
        {argOverflow && (
          <Expander open={open} onToggle={() => setOpen((o) => !o)} label={open ? "less" : "more"} />
        )}
        <ToolDuration msg={msg} result={result} live={live} />
      </div>
      {isBash && full && <CommandBlock command={full} />}
      {result && <ToolResultBody result={result} toolName={name} />}
    </div>
  );
}

function ThinkingRow({ text }: { text: string }) {
  const [open, setOpen] = useState(false);
  // Muted, not faint (Decision 12 + M4 contrast): thinking is de-emphasized but
  // still conveys meaning, so it must clear 4.5:1. The rail border is drawn by
  // the feed's rail wrapper, not here.
  return (
    <div className="text-sm text-muted">
      <div className="flex items-baseline gap-2">
        <span className="inline-flex items-baseline gap-1.5 italic">
          <span aria-hidden="true" className="self-center">
            <ThoughtIcon />
          </span>
          thinking… {truncate(firstLine(text), 100)}
        </span>
        <Expander open={open} onToggle={() => setOpen((o) => !o)} label={open ? "hide" : "show"} />
      </div>
      {open && (
        <pre className="mt-1 whitespace-pre-wrap break-words text-xs italic text-muted">{text}</pre>
      )}
    </div>
  );
}

function StandaloneResult({ result }: { result: RunMessage }) {
  const id = asString(asRecord(result.payload)?.["tool_use_id"]);
  return (
    <div className="text-sm">
      <div className="text-[11px] font-semibold uppercase tracking-wider text-muted">
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
      return <MetaLine text={describeStatus(msg.payload)} />;
    case "error":
      return (
        <div className="rounded-md border border-danger/40 bg-danger/10 px-2 py-1 text-sm text-danger">
          <span aria-hidden="true">✗</span> {describeError(msg.payload)}
        </div>
      );
    case "plan":
      return (
        <div className="inline-flex items-center gap-1.5 rounded-md border border-warn/40 bg-warn/10 px-2 py-1 text-xs text-warn">
          <span aria-hidden="true">
            <FileTextIcon />
          </span>
          plan submitted (awaiting approval)
        </div>
      );
    default: {
      const extract = asString(rec?.["text"]) ?? asString(rec?.["message"]) ?? "";
      return (
        <div className="text-xs text-muted">
          <span className="italic">unrenderable {msg.kind} event</span>
          {extract && <span className="ml-1 text-fg">— {truncate(extract)}</span>}
        </div>
      );
    }
  }
});

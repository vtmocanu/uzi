import { memo, useEffect, useRef, useState, type ReactNode } from "react";
import type { RunMessage } from "../lib/api";
import type { PhaseUsage } from "../lib/runUsage";
import { formatTokens, formatCost } from "../lib/formatTokens";
import { parseAnswerPayload, parseQuestionPayload } from "../lib/runQuestion";
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
function asNumber(v: unknown): number | undefined {
  return typeof v === "number" && Number.isFinite(v) ? v : undefined;
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
// surface their subagent_type so they are recognizable. Unknown tools degrade to
// compact key: value pairs. The result is NOT truncated — callers truncate for
// display and offer expansion.
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
    // "Agent" is the tool a LIVE run actually spawns subagents with (the name the
    // guardrails deny for nested spawns, guardrails.ts NESTED_AGENT_TOOL); "Task" is
    // its legacy name, kept because historical runs persisted frames under it. Matching
    // only "Task" left every live spawn falling through to the default key:value branch.
    case "Agent":
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
      <div className="rounded-md border border-edge bg-ink px-2.5 py-2 font-mono text-xs leading-relaxed">
        {/* Clamp on this padding-free inner wrapper via max-height (2 line boxes),
            NOT line-clamp: line-clamp forces display:-webkit-box on its target,
            which drops the inline ❯ prompt onto its own line above the text. Here
            ❯ stays a plain inline sibling of <code>, so it reads inline in BOTH
            states (user-approved deviation from the mock's line-clamp quirk). The
            cap is 2 × leading-relaxed(1.625) = 3.25em (em == the inherited
            text-xs), i.e. exactly two lines regardless of the outer padding. */}
        <div className={cx("whitespace-pre-wrap break-words", clamped && "max-h-[3.25em] overflow-hidden")}>
          <span aria-hidden="true" className="mr-2 select-none text-faint">
            ❯
          </span>
          <code>{highlightShell(command)}</code>
        </div>
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

// GUARDRAIL_DENY_MARK is the stable, deliberate phrase every one of the 15
// guardrail deny reasons carries (agent/src/guardrails.ts:92-111), plus the two
// colon-less `?? "denied by guardrail"` fallbacks (:539/:786). `web/` and `agent/`
// are separate npm packages, so the phrase cannot be imported — the contract is
// pinned from the agent side by agent/test/guardrails.test.ts.
const GUARDRAIL_DENY_MARK = "denied by guardrail";

// The SDK may wrap a tool error in `<tool_use_error>…</tool_use_error>`; the open
// tag is stripped before the anchor test so a wrapped denial still reads as one.
const TOOL_USE_ERROR_OPEN = "<tool_use_error>";

// …and it builds a `PreToolUse:Bash hook error: <reason>` form of the same denial
// (see classifyResultState). Stripped up to and including this preamble.
const HOOK_ERROR_PREAMBLE = "hook error: ";

export type ResultState = "ok" | "error" | "blocked";

// classifyResultState maps a tool_result to its presentation state (PRD #116).
//
// The deny phrase must START one of the text's LINES (after leading whitespace and
// an optional `<tool_use_error>` open tag). Anchoring per line, rather than
// `.includes` over the whole text, is what keeps both halves honest:
//   - a real denial still matches in every shape it ships in — the plain reason, the
//     colon-less `?? "denied by guardrail"` fallback (no colon to anchor on, which is
//     why this is not an exact-string or "phrase + colon" test), the `<tool_use_error>`
//     wrapper, and a denial that is one of several content blocks joined with "\n" by
//     resultToText, wherever in that join it lands;
//   - a MID-LINE mention no longer disarms the red chip. A genuinely failing command
//     that greps guardrails.ts, or a failing `npm test` whose node --test output echoes
//     a test title quoting the phrase, is a real problem and must stay "✗ error"
//     (under-alarming it is the risk PRD #116 called out).
//
// The one prefix stripped beyond the wrapper is a hook-error preamble. The pinned SDK
// builds `${hookName} hook error: ${reason}` for the SAME denial and yields it just
// BEFORE the raw-reason result; the bare reason only wins because the consumer keeps
// the last yield. That is one reordering away from every denial arriving prefixed, so
// the preamble is stripped too — narrowly (it must end in "hook error: "), which no
// incidental mention of the phrase carries.
//
// "blocked" is gated on is_error: a successful result quoting the phrase is untouched.
// The state is purely presentational — `is_error` stays true on the persisted frame,
// so nothing downstream shifts. That guarantee comes from the frame being UNCHANGED,
// not from nobody reading it: the judge's missing-tool prescan does read this exact
// field (api/internal/workersvc/judge.go, toolResultOutcome → observedGreenTools), and
// flipping a denial to non-error at the source would make a denied command count as
// green evidence that it ran (PRD #116 Decision 2).
export function classifyResultState(isError: boolean, text: string): ResultState {
  if (!isError) return "ok";
  for (const line of text.split("\n")) {
    let s = line.trimStart();
    if (s.startsWith(TOOL_USE_ERROR_OPEN)) s = s.slice(TOOL_USE_ERROR_OPEN.length).trimStart();
    const preamble = s.indexOf(HOOK_ERROR_PREAMBLE);
    if (preamble !== -1) s = s.slice(preamble + HOOK_ERROR_PREAMBLE.length).trimStart();
    if (s.startsWith(GUARDRAIL_DENY_MARK)) return "blocked";
  }
  return "error";
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
      // Duration + turns only — the per-phase token/cost figures ride the finish
      // line separately (PhaseUsage), because the frame's own total_cost_usd is
      // CUMULATIVE-across-resume (PRD #40 verdict b) and would over-read per phase.
      const bits: string[] = [];
      if (typeof rec["duration_ms"] === "number") bits.push(formatDuration(rec["duration_ms"]));
      if (typeof rec["num_turns"] === "number") bits.push(`${rec["num_turns"]} turns`);
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
function MetaLine({ text, usage }: { text: string; usage?: PhaseUsage }) {
  const paren = text.indexOf("(");
  const key = paren === -1 ? text : text.slice(0, paren).trimEnd();
  const detail = paren === -1 ? "" : text.slice(paren);
  return (
    <div className="flex items-center gap-2.5 py-0.5 text-xs italic text-muted">
      <span aria-hidden="true" className="h-px flex-1 bg-edge" />
      <span>
        <span className="font-mono text-[11px] not-italic text-fg">{key}</span>
        {detail ? ` ${detail}` : ""}
        {usage && (
          <>
            {" · "}
            <FinishTokens usage={usage} />
          </>
        )}
      </span>
      <span aria-hidden="true" className="h-px flex-1 bg-edge" />
    </div>
  );
}

// FinishTokens renders a result frame's PER-PHASE token/cost figures on its finish
// line (PRD #40 §1). Tokens are the delta this phase spent (fresh in · cached · out,
// derived in lib/runUsage.ts); cost too. A $0 cost (subscription auth) is dropped
// rather than shown as "$0.00". Mono + tabular so figures line up down the log.
function FinishTokens({ usage }: { usage: PhaseUsage }) {
  return (
    <span className="font-mono not-italic tabular-nums text-faint">
      {formatTokens(usage.fresh)} in · {formatTokens(usage.cached)} cached · {formatTokens(usage.out)} out
      {usage.costUsd > 0 && <span className="text-brand"> · {formatCost(usage.costUsd)}</span>}
    </span>
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
    // Both spawn names get the bot icon — see toolSummary for why "Agent" is the live
    // one and "Task" the legacy one.
    case "Agent":
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

// The neutral chip/body frame, shared by "ok" and "blocked" so a future tweak to
// one cannot silently drift from the other.
const NEUTRAL_CHIP = "border-edge bg-raised/50 text-muted hover:border-edge-strong";
const NEUTRAL_BODY = "border-edge bg-ink";

// Per-state chip/body presentation (PRD #116 Decision 4). "blocked" reuses the
// NEUTRAL success frame — only its ⊘ glyph is warn-tinted, because --warn is
// already spoken for by the plan-submitted chip and the slow-duration tint, so a
// fully warn chip would collide semantically.
//
// The weight lives in glyphClass, per state, rather than on the shared span: at
// 11px, `font-bold` closes the counter of ⊘ and it rasterises as a small amber
// blob, confusable with the feed's agent-status dots. ✓/✗ read fine bold at that
// size and keep it; ⊘ goes non-bold and a touch larger instead.
const RESULT_TONE: Record<ResultState, { glyph: string; glyphClass: string; chip: string; body: string; aria: string }> = {
  ok: {
    glyph: "✓",
    glyphClass: "font-bold text-ok",
    chip: NEUTRAL_CHIP,
    body: NEUTRAL_BODY,
    aria: "Tool output",
  },
  error: {
    glyph: "✗",
    glyphClass: "font-bold text-danger",
    chip: "border-danger/40 bg-danger/10 text-danger",
    body: "border-danger/40 bg-danger/[0.08]",
    aria: "Tool error output",
  },
  blocked: {
    glyph: "⊘",
    glyphClass: "text-warn text-[13px] leading-none",
    chip: NEUTRAL_CHIP,
    body: NEUTRAL_BODY,
    aria: "Tool blocked output",
  },
};

// ToolResultBody renders a result as a collapsed inline chip that expands into a
// focusable code block (PRD #38 Decision 13). Labels: "✗ error" / "⊘ blocked"
// (a guardrail denial, PRD #116) / "✓ ok" (empty or whitespace-only output) /
// "✓ N lines". The body stays MOUNTED but hidden while collapsed, so its text is
// always in the DOM (pairing/test assertions); a dropped non-text block surfaces
// as a "[image omitted]" first line. Errors auto-expand with a danger tint; a
// blocked result is handled-and-recovered, so it stays neutral and collapsed.
function ToolResultBody({ result, toolName }: { result: RunMessage; toolName?: string }) {
  const rec = asRecord(result.payload) ?? {};
  const isError = rec["is_error"] === true;
  const { text, hadNonText } = resultToText(rec["content"]);
  const empty = text.trim() === "";
  const lineCount = empty ? 0 : text.split("\n").length;
  const state = classifyResultState(isError, text);
  const tone = RESULT_TONE[state];

  const [open, setOpen] = useState(state === "error");
  const bodyRef = useRef<HTMLPreElement>(null);
  const userToggled = useRef(false);
  // Move keyboard focus into the body only when the USER opens it — never on the
  // error auto-expand, which would steal focus as the feed renders.
  useEffect(() => {
    if (open && userToggled.current) bodyRef.current?.focus();
  }, [open]);

  const label =
    state === "error"
      ? "error"
      : state === "blocked"
        ? "blocked"
        : empty
          ? "ok"
          : `${lineCount} line${lineCount === 1 ? "" : "s"}`;
  // Name the tool in the a11y label when the call is known (paired), like the mock
  // ("Show 3 lines of Bash output"). An orphan result has no call, so it keeps the
  // tool-agnostic wording (PRD #38 M6 NIT).
  const of = toolName ? `${toolName} ` : "";
  const showLabel =
    state === "error"
      ? `Show ${of}error output`
      : state === "blocked"
        ? `Show ${of}blocked output`
        : empty
          ? `Show ${of}output`
          : `Show ${label} of ${of}output`;
  const ariaLabel = open
    ? state === "error"
      ? `Hide ${of}error output`
      : state === "blocked"
        ? `Hide ${of}blocked output`
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
          tone.chip,
        )}
      >
        <span aria-hidden="true" className={tone.glyphClass}>
          {tone.glyph}
        </span>
        {label}
      </button>
      <pre
        ref={bodyRef}
        hidden={!open}
        tabIndex={0}
        role="group"
        aria-label={tone.aria}
        className={cx(
          "mt-1.5 max-h-64 overflow-auto whitespace-pre-wrap break-words rounded-md border px-2.5 py-2 font-mono text-xs text-muted",
          tone.body,
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

// The unknown-kind fallback line. Extracted from the `default:` arm because PRD #88's
// `question` row also lands here when its payload is unusable — a question with no
// question_id can be shown but never answered (the api rejects an id-less answer), so
// it degrades to the same muted line every other unrenderable frame gets rather than
// rendering a composer whose every submit would 400.
function UnrenderableRow({ kind, rec }: { kind: string; rec?: Record<string, unknown> }) {
  const extract = asString(rec?.["text"]) ?? asString(rec?.["message"]) ?? "";
  return (
    <div className="text-xs text-muted">
      <span className="italic">unrenderable {kind} event</span>
      {extract && <span className="ml-1 text-fg">— {truncate(extract)}</span>}
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
  phaseUsage,
}: {
  msg: RunMessage;
  result?: RunMessage;
  live: boolean;
  // PRD #40: the per-phase token/cost delta for a result frame (status/error),
  // looked up by seq upstream. undefined for every non-result row.
  phaseUsage?: PhaseUsage;
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
      return <MetaLine text={describeStatus(msg.payload)} usage={phaseUsage} />;
    case "error":
      return (
        <div className="rounded-md border border-danger/40 bg-danger/10 px-2 py-1 text-sm text-danger">
          <span aria-hidden="true">✗</span> {describeError(msg.payload)}
          {phaseUsage && (
            <span className="ml-2 text-xs">
              <FinishTokens usage={phaseUsage} />
            </span>
          )}
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
    // PRD #41: the planner is reworking the plan after the user requested changes.
    // A terse, info-toned one-liner — the plan body itself re-arrives as a `plan`
    // event, so this row only marks the round transition.
    case "plan_revising": {
      const round = asNumber(rec?.["round"]);
      return (
        <div className="inline-flex items-center gap-1.5 rounded-md border border-info/40 bg-info/10 px-2 py-1 text-xs text-info">
          <span aria-hidden="true">
            <FileTextIcon />
          </span>
          revising plan{round != null ? ` (round ${round})` : ""}
        </div>
      );
    }
    // PRD #41: the user's steering text for a revision. UNTRUSTED, so it is rendered
    // through the hardened <Markdown> (react-markdown, no raw-HTML sink) — never a
    // raw injection point.
    case "plan_feedback": {
      const feedback = asString(rec?.["feedback"]) ?? "";
      return (
        <div className="rounded-md border border-brand/30 bg-brand/[0.08] px-2.5 py-1.5 text-sm">
          <div className="mb-1 text-[11px] font-semibold uppercase tracking-wider text-brand">requested changes</div>
          {feedback ? <Markdown content={feedback} /> : <span className="text-xs text-faint">(no feedback text)</span>}
        </div>
      );
    }
    // PRD #88: the lead asked the human a question and the run parked. The row is the
    // durable record — the ANSWER affordance lives on the run panel (QuestionPanel), so
    // this stays a read-only transcript entry and still renders long after the run
    // resumed.
    //
    // Every string in the payload is model-authored from repo/issue content the agent
    // read, i.e. attacker-influenceable. `question` goes through the hardened <Markdown>
    // (no rehype-raw, so raw HTML is inert text); the header and each option render as
    // React text children. Nothing here reaches href/title/style — option `label` in a
    // `title` attribute would be the one channel a subtree-text assertion cannot see.
    case "question": {
      const q = parseQuestionPayload(msg.payload);
      if (!q) return <UnrenderableRow kind={msg.kind} rec={rec} />;
      return (
        <div className="rounded-md border border-warn/40 bg-warn/[0.08] px-2.5 py-1.5 text-sm">
          <div className="mb-1 text-[11px] font-semibold uppercase tracking-wider text-warn">
            question for you{q.generation > 1 ? ` · #${q.generation}` : ""}
          </div>
          <ul className="space-y-2">
            {q.questions.map((item, i) => (
              <li key={i} className="space-y-1">
                {item.header.trim() !== "" && (
                  <div className="text-xs font-semibold text-fg">{item.header}</div>
                )}
                <Markdown content={item.question} />
                {item.options.length > 0 && (
                  <div className="flex flex-wrap gap-1.5">
                    {item.options.map((o, j) => (
                      <span
                        key={j}
                        className="rounded-full border border-edge-strong bg-raised px-2 py-[2px] text-[11px] text-muted"
                      >
                        {o.label}
                      </span>
                    ))}
                  </div>
                )}
              </li>
            ))}
          </ul>
        </div>
      );
    }
    // PRD #88: the human's answer, echoed once applied so the round-trip is auditable
    // — the same role plan_feedback plays for a revision, and the same brand tone, so
    // "this line is the user speaking" reads identically for both.
    case "answer": {
      const { answers } = parseAnswerPayload(msg.payload);
      return (
        <div className="rounded-md border border-brand/30 bg-brand/[0.08] px-2.5 py-1.5 text-sm">
          <div className="mb-1 text-[11px] font-semibold uppercase tracking-wider text-brand">your answer</div>
          {answers.length === 0 ? (
            <span className="text-xs text-faint">(no answer text)</span>
          ) : (
            <ul className="space-y-1.5">
              {answers.map((a, i) => (
                <li key={i}>
                  {a.trim() === "" ? (
                    <span className="text-xs text-faint">(no answer given)</span>
                  ) : (
                    // The user's own text, but it round-trips through the server (and can
                    // arrive from a Slack reply), so it is rendered through the same
                    // escaped sink as everything else in the feed.
                    <Markdown content={a} />
                  )}
                </li>
              ))}
            </ul>
          )}
        </div>
      );
    }
    default:
      return <UnrenderableRow kind={msg.kind} rec={rec} />;
  }
});

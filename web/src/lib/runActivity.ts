// runActivity mirrors api/internal/runactivity (PRD #1064 D3) in TypeScript: it derives
// a run's "now" line — who is acting, on what, right now — from the run's newest
// tool_use frame. The run view computes it CLIENT-SIDE off the live WS frames
// (useRunStream) so the line updates without waiting for a DTO re-read (D6); the board
// and runs list instead read the server-computed run.current_activity DTO directly.
//
// The Go copy (Latest + FromFrame) and this copy are pinned against each other by the
// shared golden fixture fixtures/run-activity/cases.json, asserted from BOTH modules
// (runActivity.test.ts here, runactivity_test.go there). Change the selection or fold
// rule in one and the fixture reddens the other — that is the whole point (R5).

import type { RunActivity, RunMessage } from "./apiTypes";
import { formatElapsed } from "./runBadge";
import { stripUnsafeChars } from "./safeText";

// DETAIL_CAP_RUNES is the code-point cap applied to the two model-authored display
// fields (agent_label, detail), matching the Go detailCapRunes (200) and, through it,
// workersvc.sanitizePlanChangedLine's cap. It is a CODE-POINT cap, not a UTF-16 unit
// cap — Array.from splits on code points so an astral char counts once and the cut
// never lands inside a surrogate pair (the Go rule caps by rune).
const DETAIL_CAP_RUNES = 200;

// sanitize strips the terminal-unsafe characters and then caps at DETAIL_CAP_RUNES
// code points — the same strip-then-cap order the Go sanitize applies. stripUnsafeChars
// is the shared web copy of the termsafe/sanitizeTTY predicate (safeText.ts), so the
// stripped set matches what the server strips.
function sanitize(s: string): string {
  return Array.from(stripUnsafeChars(s)).slice(0, DETAIL_CAP_RUNES).join("");
}

// asRecord coerces an unknown payload/input to a plain object for keyed access; a
// non-object (a malformed frame the worker still persisted) yields {} so the fold
// degrades to empty fields rather than throwing — the now line is advisory, mirroring
// the Go fold's tolerance of a malformed payload.
function asRecord(v: unknown): Record<string, unknown> {
  return typeof v === "object" && v !== null ? (v as Record<string, unknown>) : {};
}

function str(v: unknown): string {
  return typeof v === "string" ? v : "";
}

// latestActivity folds the newest tool_use frame in messages via fromMessage, skipping
// every other kind (tool_result, status, text, thinking, …). It returns null when no
// tool_use frame exists. "Newest" is the greatest seq — the deterministic tiebreak
// across interleaved subagents (R9) — so the caller need not pre-sort.
export function latestActivity(messages: RunMessage[]): RunActivity | null {
  let best: RunMessage | null = null;
  for (const m of messages) {
    if (m.kind !== "tool_use") continue;
    if (best === null || m.seq > best.seq) best = m;
  }
  if (best === null) return null;
  return fromMessage(best);
}

// fromMessage folds a single tool_use frame into a RunActivity, mirroring the Go
// FromFrame. tool is payload.name. For an "Agent" tool_use (the lead's own subagent
// dispatch, often the newest frame before the subagent has written anything) the acting
// agent is input.subagent_type (falling back to the frame's own agent) and the label +
// detail are input.description — the dispatch names the lane about to work. For every
// other tool the frame's own agent/agent_label are used verbatim. detail is the
// repo-relative file_path for the file tools, the description for Agent and Bash (NEVER
// Bash's command), and "" otherwise. agent_label and detail are stripped-and-capped;
// agent and tool are NOT sanitized here (the server does not cap them either) — the
// render layer folds all four through stripUnsafeChars for defense-in-depth.
function fromMessage(m: RunMessage): RunActivity {
  const payload = asRecord(m.payload);
  const name = str(payload.name);
  const input = asRecord(payload.input);

  let agent = m.agent ?? "";
  let agentLabel = m.agent_label ?? "";
  let detail = "";

  switch (name) {
    case "Agent": {
      const sub = str(input.subagent_type);
      if (sub !== "") agent = sub;
      agentLabel = str(input.description);
      detail = str(input.description);
      break;
    }
    case "Bash":
      // The command is deliberately never surfaced (D3); only the description.
      detail = str(input.description);
      break;
    case "Read":
    case "Edit":
    case "Write":
    case "MultiEdit":
      detail = str(input.file_path);
      break;
    default:
      detail = "";
  }

  return {
    agent,
    agent_label: sanitize(agentLabel),
    tool: name,
    detail: sanitize(detail),
    at: m.created_at,
    seq: m.seq,
  };
}

// activityAge renders a RunActivity's client-side age from its `at` instant — the R7
// mitigation: a stalled lane reads its real age ("12m") rather than a lie, because the
// age is computed at render time, not stamped server-side. Returns "" for an
// unparseable timestamp so a caller renders nothing rather than a fabricated token.
export function activityAge(at: string, nowMs: number): string {
  const t = Date.parse(at);
  if (Number.isNaN(t)) return "";
  return formatElapsed(nowMs - t);
}

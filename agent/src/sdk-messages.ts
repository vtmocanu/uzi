// SDK stream events → run_messages (PRD #4 §Message mapping).
//
// Maps the SDK's discriminated `SDKMessage` union to the worker's run-message
// kinds (text|thinking|tool_use|tool_result|status|error) with agent
// attribution (lead vs a named subagent). Pure and defensive: field access
// goes through narrow probes rather than the full SDK types, so an SDK minor
// bump that reshapes a variant degrades to "skip" instead of a type error or a
// crash (the SDK-drift risk is contained to this file + sdk-executor.ts).
//
// Partial/streaming events (`type: 'stream_event'`) are intentionally NOT
// mapped: they are token deltas, and persisting them would flood the gapless
// seq stream. M3 persists discrete blocks; the live-UI partial channel is M5.

import type { EmittedMessage } from "./executor.js";

/** The lead runs on the main thread; subagents carry a `subagent_type`. */
const LEAD = "lead";

function asRecord(v: unknown): Record<string, unknown> | undefined {
  return v && typeof v === "object" ? (v as Record<string, unknown>) : undefined;
}

function asString(v: unknown): string | undefined {
  return typeof v === "string" ? v : undefined;
}

/** Attribution for an assistant/user frame: a named subagent, else the lead. */
function agentOf(msg: Record<string, unknown>): string {
  return asString(msg["subagent_type"]) ?? LEAD;
}

/** Content blocks of an assistant/user message (or [] if not an array). */
function contentBlocks(message: unknown): Record<string, unknown>[] {
  const rec = asRecord(message);
  const content = rec?.["content"];
  if (!Array.isArray(content)) return [];
  return content.filter((b): b is Record<string, unknown> => asRecord(b) !== undefined);
}

/** Map an assistant frame's content blocks (text / thinking / tool_use). */
function mapAssistant(msg: Record<string, unknown>): EmittedMessage[] {
  const agent = agentOf(msg);
  const out: EmittedMessage[] = [];
  for (const block of contentBlocks(msg["message"])) {
    switch (block["type"]) {
      case "text": {
        const text = asString(block["text"]);
        if (text) out.push({ kind: "text", agent, payload: { text } });
        break;
      }
      case "thinking": {
        const thinking = asString(block["thinking"]);
        if (thinking) out.push({ kind: "thinking", agent, payload: { text: thinking } });
        break;
      }
      case "tool_use": {
        out.push({
          kind: "tool_use",
          agent,
          payload: {
            id: asString(block["id"]),
            name: asString(block["name"]),
            input: block["input"],
          },
        });
        break;
      }
      default:
        break; // redacted_thinking, server_tool_use, etc. — not surfaced in M3.
    }
  }
  return out;
}

/**
 * Map a user frame — only tool_result blocks are surfaced (the agent's own
 * prompt echoes and synthetic user turns are noise for the run stream). A
 * tool_result may carry structured content; it is passed through as-is.
 */
function mapUser(msg: Record<string, unknown>): EmittedMessage[] {
  const agent = agentOf(msg);
  const out: EmittedMessage[] = [];
  for (const block of contentBlocks(msg["message"])) {
    if (block["type"] !== "tool_result") continue;
    out.push({
      kind: "tool_result",
      agent,
      payload: {
        tool_use_id: asString(block["tool_use_id"]),
        content: block["content"],
        is_error: block["is_error"] === true,
      },
    });
  }
  return out;
}

/** Map the terminal `result` frame to a status (success) or error message. */
function mapResult(msg: Record<string, unknown>): EmittedMessage[] {
  const subtype = asString(msg["subtype"]) ?? "unknown";
  if (subtype === "success" && msg["is_error"] !== true) {
    // duration_ms/total_cost_usd are forwarded additively so the run view can
    // show the finish line's duration and cost (PRD #11); num_turns is unchanged.
    // Added only when the SDK frame carries them, so the payload stays minimal
    // (and absent fields never surface as null in the persisted stream).
    const payload: Record<string, unknown> = {
      event: "result",
      subtype,
      num_turns: msg["num_turns"],
    };
    if (typeof msg["duration_ms"] === "number") payload["duration_ms"] = msg["duration_ms"];
    if (typeof msg["total_cost_usd"] === "number") payload["total_cost_usd"] = msg["total_cost_usd"];
    return [{ kind: "status", agent: LEAD, payload }];
  }
  // error_during_execution / error_max_turns / error_max_budget_usd / etc.
  const errors = Array.isArray(msg["errors"]) ? (msg["errors"] as unknown[]).map(String) : [];
  return [
    {
      kind: "error",
      agent: LEAD,
      payload: { event: "result", subtype, errors },
    },
  ];
}

/**
 * Map one SDK message to zero or more run messages. Returns [] for message
 * types M3 does not persist (partials, hook lifecycle, telemetry chatter).
 */
export function mapSdkMessage(message: unknown): EmittedMessage[] {
  const msg = asRecord(message);
  if (!msg) return [];
  switch (msg["type"]) {
    case "assistant":
      return mapAssistant(msg);
    case "user":
      return mapUser(msg);
    case "result":
      return mapResult(msg);
    case "system":
      // Only the init frame is useful as a status heartbeat; other system
      // subtypes (task_*, hook_*, status) are not persisted in M3.
      if (msg["subtype"] === "init") {
        return [
          {
            kind: "status",
            agent: LEAD,
            payload: { event: "init", model: asString(msg["model"]) },
          },
        ];
      }
      return [];
    default:
      return [];
  }
}

/**
 * Whether an SDK message signals the turn ended in error (used by the executor
 * to fail the run). Mirrors mapResult's success test.
 */
export function isErrorResult(message: unknown): boolean {
  const msg = asRecord(message);
  if (!msg || msg["type"] !== "result") return false;
  return msg["subtype"] !== "success" || msg["is_error"] === true;
}

/** Whether an SDK message is the terminal `result` frame. */
export function isResult(message: unknown): boolean {
  return asRecord(message)?.["type"] === "result";
}

/** Extract the session_id an SDK message carries, if any (first one wins). */
export function sessionIdOf(message: unknown): string | undefined {
  return asString(asRecord(message)?.["session_id"]);
}

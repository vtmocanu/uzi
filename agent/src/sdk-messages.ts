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
    // duration_ms/total_cost_usd are forwarded for the finish line's duration and
    // cost (PRD #11); usage/modelUsage carry the full token accounting the API
    // folds into run_usage (PRD #40 M1). Unguarded passthrough, same style as
    // num_turns: when the SDK frame omits a field it lands as undefined and
    // JSON-serialization drops it, so nothing surfaces on the wire. These result
    // totals are CUMULATIVE-across-resume (PRD #40 Decision 3 verdict b), so the
    // server rollup takes them latest-wins per (run_id, model), never a sum.
    return [
      {
        kind: "status",
        agent: LEAD,
        payload: {
          event: "result",
          subtype,
          num_turns: msg["num_turns"],
          duration_ms: msg["duration_ms"],
          total_cost_usd: msg["total_cost_usd"],
          usage: msg["usage"],
          modelUsage: msg["modelUsage"],
        },
      },
    ];
  }
  // error_during_execution / error_max_turns / error_max_budget_usd / etc. The
  // SDK's SDKResultError carries the same accounting as a success frame, so a
  // failed/cancelled run's pre-death spend is still forwarded (PRD #40 Decision 4)
  // — the runs most worth auditing must not report nothing.
  const errors = Array.isArray(msg["errors"]) ? (msg["errors"] as unknown[]).map(String) : [];
  return [
    {
      kind: "error",
      agent: LEAD,
      payload: {
        event: "result",
        subtype,
        errors,
        usage: msg["usage"],
        modelUsage: msg["modelUsage"],
        total_cost_usd: msg["total_cost_usd"],
        num_turns: msg["num_turns"],
        duration_ms: msg["duration_ms"],
      },
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

/**
 * The per-API-call token usage an assistant frame carries
 * (`SDKAssistantMessage.message.usage` — a `BetaUsage`, sdk.d.ts:2762). Returned
 * raw for the executor to attach to EXACTLY ONE emitted message that survives its
 * signal filter (PRD #40 Decision 11 — the attach is executor-side, not here,
 * because mapAssistant explodes one frame into N messages and cannot see that
 * later drop). This is PER-CALL usage: it is what the per-agent table sums, and is
 * a DELIBERATELY different data path from the terminal result frame's usage, which
 * the CLI reports from a cumulative-across-resume accumulator (Decision 3 verdict
 * b). Undefined for any non-assistant frame, or one with no object-shaped usage.
 */
export function assistantUsageOf(message: unknown): Record<string, unknown> | undefined {
  const msg = asRecord(message);
  if (!msg || msg["type"] !== "assistant") return undefined;
  return asRecord(asRecord(msg["message"])?.["usage"]);
}

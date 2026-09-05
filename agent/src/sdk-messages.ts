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
//
// ── Replay frames and the missing role (PRD #99 M2 decision, 2026-07-20) ──────
// `SDKUserMessageReplay` (sdk.d.ts:4334) is the transcript re-delivery variant.
// It carries `parent_tool_use_id` but has NEITHER `subagent_type` NOR
// `task_description`, and its `type` is `'user'` exactly like `SDKUserMessage`,
// so it maps through `mapUser`. A replayed subagent tool_result therefore emits
// `agentInstance` SET while `agent` falls back to "lead" and `agentLabel` is
// absent — a message attributed to the lead but keyed to a subagent's lane.
//
// This is left UNCORRECTED in the worker, for two reasons:
//
//  1. It is not reliably detectable here. The two variants are structurally
//     identical apart from `uuid`/`session_id` being REQUIRED on the replay and
//     OPTIONAL on the normal frame (verified against the pinned 0.3.201
//     typings). That distinction does not exist at runtime — ordinary user
//     frames do carry both fields in practice (`sessionIdOf` reads `session_id`
//     off any message) — so there is no honest `isReplay` predicate to write.
//     A guess would be a silent mis-tag, which is worse than the gap.
//  2. Even given perfect detection, the worker has nothing better to put in
//     `agent`: the replay frame genuinely does not carry the role. Inventing one
//     (a "subagent" sentinel, or reusing the previous frame's role) would need
//     the correlation state this design exists to avoid, and would PERSIST the
//     guess — `run_messages.agent` is denormalized with no backfill, so a wrong
//     value is permanent.
//
// The fix therefore belongs on the read side, where it is free and self-
// correcting: the pane derives a lane's role and label from the first message in
// the lane with a NON-NULL value rather than from the lane's first message, so
// the lane heals as soon as any frame in it carries the role. Recorded as
// Decision 8's fourth degradation case in prds/99-activity-instance-lanes.md.

import { query as sdkQuery } from "@anthropic-ai/claude-agent-sdk";
import type { EmittedMessage } from "./executor.js";
import type { SdkQueryFn } from "./sdk-executor.js"; // type-only — erased at runtime, so no import cycle
import type { HarnessAttribution, HarnessItem } from "./harness.js";
import { projectInit, projectItem, projectResult } from "./harness-messages.js";

/** One-shot user-turn prompt stream: the SDK consumes a single user message. */
export async function* promptStream(text: string): AsyncGenerator<unknown> {
  yield { type: "user", message: { role: "user", content: text }, parent_tool_use_id: null };
}

// The real SDK `Query` has a required getContextUsage() returning the wider
// SDKControlGetContextUsageResponse; it satisfies the optional, narrower seam
// type by covariance, so no cast is needed here.
export const defaultQueryFn: SdkQueryFn = (params) =>
  sdkQuery({ prompt: params.prompt as never, options: params.options });

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

/**
 * Per-frame attribution (PRD #99): WHO produced the frame, WHICH invocation of
 * them, and WHAT that invocation was asked to do.
 *
 * All three are plain top-level fields on the SDK's complete assistant/user
 * frames — `subagent_type` (sdk.d.ts:2777), `parent_tool_use_id` (:2765 /
 * :4295), `task_description` (:2781) — read through the same narrow `asString`
 * probe, so an SDK reshape degrades each to `undefined` (→ SQL NULL → the web's
 * role-name fallback) rather than throwing. There is deliberately NO correlation
 * state here: nothing is remembered between frames, which is what makes this
 * correct across resume and across two subagents running at once.
 *
 * `parent_tool_use_id` is `string | null` and is null on the ORCHESTRATOR's own
 * turns, so `asString` collapses null and absent to the same `undefined`. Read
 * that as "this frame has no parent invocation", NOT as `agent === "lead"`: a
 * repo roster may ship an agent NAMED `lead`, which is a real subagent and does
 * carry a `parent_tool_use_id` (see `orphanInstanceKind` below for why the two
 * must never be conflated).
 */
/**
 * Decode a frame's attribution into the neutral {@link HarnessAttribution} (PRD
 * #1146 M2). `agent` is always present (agentOf's string-or-lead projection);
 * `agentInstance`/`agentLabel` are assigned conditionally, never as an explicit
 * `undefined` — the batcher copies a field onto the wire only when it is
 * `!== undefined`, and the API maps an absent field to SQL NULL. Writing
 * `agentInstance: undefined` would still be absent on the JSON wire, but the
 * conditional keeps the emitted object's shape honest for the node --test
 * assertions that check key presence. Exported so the run-lane adapter
 * (claude-harness.ts) reuses this exact decode rather than a second copy.
 */
export function decodeAttribution(
  msg: Record<string, unknown>,
): HarnessAttribution {
  const at: HarnessAttribution = { agent: agentOf(msg) };
  const instance = asString(msg["parent_tool_use_id"]);
  if (instance !== undefined) at.agentInstance = instance;
  const label = asString(msg["task_description"]);
  if (label !== undefined) at.agentLabel = label;
  return at;
}

/** Content blocks of an assistant/user message (or [] if not an array). */
function contentBlocks(message: unknown): Record<string, unknown>[] {
  const rec = asRecord(message);
  const content = rec?.["content"];
  if (!Array.isArray(content)) return [];
  return content.filter((b): b is Record<string, unknown> => asRecord(b) !== undefined);
}

/**
 * Decode an assistant frame's content blocks into neutral {@link HarnessItem}s
 * (text / thinking / started-tool). Empty text/thinking blocks are omitted here
 * (the emptiness test that mapAssistant applied); unknown block types
 * (redacted_thinking, server_tool_use, etc.) are dropped — not surfaced in M3.
 * Exported so the run-lane adapter reuses the SAME decode as chat/tests. The
 * neutral items are projected to EmittedMessages by harness-messages.projectItem.
 */
export function decodeAssistantItems(msg: Record<string, unknown>): HarnessItem[] {
  const out: HarnessItem[] = [];
  for (const block of contentBlocks(msg["message"])) {
    switch (block["type"]) {
      case "text": {
        const text = asString(block["text"]);
        if (text) out.push({ kind: "text", text });
        break;
      }
      case "thinking": {
        const thinking = asString(block["thinking"]);
        if (thinking) out.push({ kind: "thinking", text: thinking });
        break;
      }
      case "tool_use": {
        out.push({
          kind: "tool",
          phase: "started",
          id: asString(block["id"]),
          name: asString(block["name"]),
          input: block["input"],
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
 * Decode a user frame's tool_result blocks into neutral finished-tool
 * {@link HarnessItem}s. Only tool_result blocks are surfaced (the agent's own
 * prompt echoes and synthetic user turns are noise for the run stream); a
 * tool_result may carry structured content and it is passed through as-is.
 */
export function decodeUserItems(msg: Record<string, unknown>): HarnessItem[] {
  const out: HarnessItem[] = [];
  for (const block of contentBlocks(msg["message"])) {
    if (block["type"] !== "tool_result") continue;
    out.push({
      kind: "tool",
      phase: "finished",
      id: asString(block["tool_use_id"]),
      output: block["content"],
      isError: block["is_error"] === true,
    });
  }
  return out;
}

/**
 * The input to harness-messages.projectResult — the terminal `result` frame's
 * outcome, display subtype, String-mapped error array and the opaque uzi wire
 * capsule. The accounting fields are forwarded UNGUARDED (duration_ms/
 * total_cost_usd feed the finish line's duration and cost, PRD #11; usage/
 * modelUsage carry the token accounting the API folds into run_usage, PRD #40 M1)
 * — when the SDK frame omits one it lands as `undefined` and projectResult still
 * constructs the key, so the shape is preserved before JSON serialization drops
 * it. Result totals are CUMULATIVE-across-resume (PRD #40 Decision 3 verdict b);
 * the server merges with GREATEST per (run_id, session_id, model), never a sum
 * across a model's sessions, then SUMs across models. The SDKResultError carries
 * the same accounting as a success frame, so a failed run's pre-death spend is
 * still forwarded (Decision 4). Exported so the adapter reuses this exact decode.
 */
export function decodeResult(msg: Record<string, unknown>): {
  outcome: "success" | "failed";
  subtype: string;
  errors: readonly string[];
  wire: {
    usage: unknown;
    modelUsage: unknown;
    num_turns: unknown;
    duration_ms: unknown;
    total_cost_usd: unknown;
  };
} {
  const subtype = asString(msg["subtype"]) ?? "unknown";
  const outcome: "success" | "failed" =
    subtype === "success" && msg["is_error"] !== true ? "success" : "failed";
  const errors = Array.isArray(msg["errors"])
    ? (msg["errors"] as unknown[]).map(String)
    : [];
  return {
    outcome,
    subtype,
    errors,
    wire: {
      usage: msg["usage"],
      modelUsage: msg["modelUsage"],
      num_turns: msg["num_turns"],
      duration_ms: msg["duration_ms"],
      total_cost_usd: msg["total_cost_usd"],
    },
  };
}

/** Map an assistant frame's content blocks (text / thinking / tool_use). */
function mapAssistant(msg: Record<string, unknown>): EmittedMessage[] {
  const at = decodeAttribution(msg);
  return decodeAssistantItems(msg).map((item) => projectItem(item, at));
}

/**
 * Map a user frame — only tool_result blocks are surfaced (the agent's own
 * prompt echoes and synthetic user turns are noise for the run stream). A
 * tool_result may carry structured content; it is passed through as-is.
 */
function mapUser(msg: Record<string, unknown>): EmittedMessage[] {
  const at = decodeAttribution(msg);
  return decodeUserItems(msg).map((item) => projectItem(item, at));
}

/** Map the terminal `result` frame to a status (success) or error message. */
function mapResult(msg: Record<string, unknown>): EmittedMessage[] {
  return [projectResult(decodeResult(msg))];
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
      // NOTE (PRD #99): `SDKUserMessageReplay` (sdk.d.ts:4334) also has
      // `type: 'user'` and lands here. It carries `parent_tool_use_id` but has
      // NEITHER `subagent_type` NOR `task_description`, so such a frame emits
      // agentInstance SET while `agent` falls back to "lead" and agentLabel is
      // absent. That is DELIBERATELY not corrected here — see the decision note
      // in this file's header. Do not add a replay guard without reading it.
      return mapUser(msg);
    case "result":
      return mapResult(msg);
    case "system":
      // Only the init frame is useful as a status heartbeat; other system
      // subtypes (task_*, hook_*, status) are not persisted in M3.
      if (msg["subtype"] === "init") {
        return [projectInit(asString(msg["model"]))];
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

/**
 * The frame type when a frame carries a `parent_tool_use_id` but NO
 * `subagent_type` field — otherwise undefined. This is the `SDKUserMessageReplay`
 * signature (sdk.d.ts:4334) and it should be IMPOSSIBLE on a well-formed frame,
 * which is why it is worth an alarm rather than a silent normalization.
 *
 * The test is on the field's PRESENCE, never on `agentOf`'s output. That
 * distinction is the whole point and it is easy to get wrong: `agentOf`'s
 * `subagent_type ?? LEAD` collapses two genuinely different states into the same
 * string `"lead"` —
 *   - field ABSENT            → "lead"   (the replay case: actually anomalous)
 *   - field PRESENT == "lead" → "lead"   (a repo-authored `lead` subagent, which
 *                                         this repo explicitly supports:
 *                                         repoagents.ts:46, and
 *                                         `selectSubagents(source="repo")` applies
 *                                         only an `exclude` check, so a repo
 *                                         `lead` IS registered as a subagent)
 * Anything keying off "is this the lead?" must decide which of the two it means.
 * A value test here would fire on every frame of a healthy repo-`lead` subagent,
 * making the signal mean "working as intended" and "replay artifact" at once.
 *
 * This is a DETECTOR, not a mutator: nothing is dropped or rewritten on the back
 * of it. Its purpose is to keep the question observable — the decision to leave
 * replay frames uncorrected (see this file's header) rests on the absence of
 * these frames in practice, and without this line that absence could never be
 * confirmed, only assumed. If it never fires, that reasoning stands on evidence;
 * if it starts firing, the analysis is already waiting in the PRD's Decision 8.
 * Do not delete it as defensive noise.
 */
export function orphanInstanceKind(message: unknown): string | undefined {
  const msg = asRecord(message);
  if (!msg) return undefined;
  if (asString(msg["subagent_type"]) !== undefined) return undefined;
  if (asString(msg["parent_tool_use_id"]) === undefined) return undefined;
  return asString(msg["type"]) ?? "unknown";
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

/**
 * The model an assistant frame's API call actually ran on
 * (`SDKAssistantMessage.message.model`, e.g. "claude-opus-4-8"). Read here but
 * attached by the executor for the SAME reason `assistantUsageOf` is (PRD #93
 * Decision 2, which inherits PRD #40 Decision 11's argument verbatim): mapAssistant
 * explodes one frame into N messages and cannot see the executor's later signal
 * filter, and every phase terminates on a signal frame — so attaching in the mapper
 * would systematically lose the lead's terminating-frame attribution.
 *
 * The value is CO-GATED with usage at that seam: it rides the SAME surviving
 * message under the SAME `usageAttached` latch, so a model is recorded only where
 * that agent's tokens are. That is what lets the web derive read `model` inside its
 * existing `"usage" in payload` branch and never manufacture a zero-token agent row
 * out of a model-only frame. A frame whose messages are ALL filtered loses both
 * (accepted, same as usage).
 *
 * This is per-CALL model, which is the only source of the agent→model mapping the
 * per-agent table needs: the result frame's `modelUsage` map is keyed by model, so
 * it cannot say which agent used which when several agents share one (PRD #93
 * Decision 1). Undefined for any non-assistant frame, or one whose `model` is
 * absent or not a string.
 */
export function assistantModelOf(message: unknown): string | undefined {
  const msg = asRecord(message);
  if (!msg || msg["type"] !== "assistant") return undefined;
  return asString(asRecord(msg["message"])?.["model"]);
}

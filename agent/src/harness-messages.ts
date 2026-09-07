// Pure, SDK-free projection core (PRD #1146 / #1106 M2, milestone m1).
//
// The ONE projection that turns neutral harness records into the worker's
// `EmittedMessage` wire objects. It is shared by two callers so there is never a
// second, drifting mapper:
//   - `sdk-messages.ts` mapAssistant/mapUser/mapResult/mapSdkMessage delegate here
//     (so chat-executor and the existing mapper tests exercise this exact code),
//   - `harness-reducer.ts` projects the run lane's surviving items and terminal.
//
// It imports NO SDK, NOT sdk-messages.ts's SDK path, signals.ts or limit.ts. All
// SDK-shaped decoding (reading raw frame fields into HarnessItem/HarnessAttribution
// and the result wire capsule) stays in sdk-messages.ts, which is SDK-aware; this
// module only projects the already-decoded neutral records. The payload shapes
// below reproduce the pre-extraction mapAssistant/mapUser/mapResult output
// byte-for-byte, including which keys are always present and the exact key order
// per branch (existing node --test assertions check both before JSON serialization).

import type { EmittedMessage } from "./executor.js";
import type { HarnessAttribution, HarnessItem } from "./harness.js";

/** The lead runs on the main thread; subagents carry a `subagent_type`. */
const LEAD = "lead";

/**
 * Project one neutral item to its EmittedMessage, reproducing today's
 * mapAssistant/mapUser output. `at` is spread with the SAME conditional
 * key-presence attributionOf/agentOf emit (agent always present; agentInstance /
 * agentLabel present only when decode set them), which the node --test assertions
 * check. Empty text/thinking omission and the signal-tool_use drop are DECODE /
 * REDUCER concerns respectively — this projects whatever item it is handed.
 */
export function projectItem(
  item: HarnessItem,
  at: HarnessAttribution,
): EmittedMessage {
  switch (item.kind) {
    case "text":
      return { kind: "text", ...at, payload: { text: item.text } };
    case "thinking":
      return { kind: "thinking", ...at, payload: { text: item.text } };
    case "tool":
      if (item.phase === "started") {
        // All three keys ALWAYS present (mapAssistant L138-149): an absent id/name
        // rides as `undefined`, not an omitted key.
        return {
          kind: "tool_use",
          ...at,
          payload: { id: item.id, name: item.name, input: item.input },
        };
      }
      // Finished tool = a user-frame tool_result (mapUser L167-176). Structured
      // content passes through as-is; is_error is the decoded strict-boolean.
      return {
        kind: "tool_result",
        ...at,
        payload: {
          tool_use_id: item.id,
          content: item.output,
          is_error: item.isError === true,
        },
      };
  }
}

/**
 * Project the terminal result frame, reproducing mapResult (sdk-messages.ts
 * L181-236) VERBATIM. BOTH branches construct every accounting key even when its
 * value is `undefined`, unknown members survive inside the wire capsule, and the
 * per-branch KEY ORDER is preserved exactly (success and failed differ, and the
 * existing tests assert the object shape before JSON serialization).
 */
export function projectResult(f: {
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
}): EmittedMessage {
  if (f.outcome === "success") {
    return {
      kind: "status",
      agent: LEAD,
      payload: {
        event: "result",
        subtype: f.subtype,
        num_turns: f.wire.num_turns,
        duration_ms: f.wire.duration_ms,
        total_cost_usd: f.wire.total_cost_usd,
        usage: f.wire.usage,
        modelUsage: f.wire.modelUsage,
      },
    };
  }
  return {
    kind: "error",
    agent: LEAD,
    payload: {
      event: "result",
      subtype: f.subtype,
      errors: f.errors,
      usage: f.wire.usage,
      modelUsage: f.wire.modelUsage,
      total_cost_usd: f.wire.total_cost_usd,
      num_turns: f.wire.num_turns,
      duration_ms: f.wire.duration_ms,
    },
  };
}

/** The system/init persisted status message (mapSdkMessage init case). */
export function projectInit(model: string | undefined): EmittedMessage {
  return {
    kind: "status",
    agent: LEAD,
    payload: { event: "init", model },
  };
}

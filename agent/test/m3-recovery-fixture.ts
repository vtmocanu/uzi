import type { InterruptedCalls } from "../src/interrupted-calls.js";

export function m3Calls(command = "printf interrupted-observation"): InterruptedCalls {
  return {
    format: 1, run_id: "ENVELOPE-RUN-NEVER-RENDER", worker_id: "ENVELOPE-WORKER-NEVER-RENDER",
    register_nonce: "ENVELOPE-NONCE-NEVER-RENDER", claim_generation: 3, source_generation: 2,
    inspected_count: 7, history_incomplete: true, unavailable: false, truncated: false,
    entries: [{
      seq: 19, source_generation: 2, agent: "OLD-AGENT", agent_instance: "OLD-INSTANCE",
      agent_label: "OLD-LABEL", tool_name: "Bash", command, display_omitted: false,
    }],
  };
}

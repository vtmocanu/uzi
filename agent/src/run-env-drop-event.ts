// The run event for a repo/user variable the worker did not add to the environment
// (PRD #4416). One `status` message per dropped name; the name goes through sanitizeName so a
// hostile name cannot forge or rewrite a log line, and the value is never part of it. The rule text is
// worker-authored (it echoes a name only for an exact match of a fixed denied name).

import type { EmittedMessage } from "./executor.js";
import { sanitizeName } from "./codex/render.js";
import type { RunEnvDrop, RunEnvDropHandler } from "./run-env-merge.js";

export function runEnvDropMessage(drop: RunEnvDrop): EmittedMessage {
  return {
    kind: "status",
    agent: "worker",
    payload: {
      event: "run_env_dropped",
      text: `Environment variable ${sanitizeName(drop.name)} was not set for this run: ${drop.rule}.`,
    },
  };
}

/** A drop handler that emits one run event per dropped name through the run's `emit`. */
export function runEnvDropEmitter(emit: (msg: EmittedMessage) => void): RunEnvDropHandler {
  return (drop) => emit(runEnvDropMessage(drop));
}

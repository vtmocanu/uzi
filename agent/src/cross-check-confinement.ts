import { CrossCheckMalformedError } from "./codex/cross-check.js";
import { CrossCheckCheckerUnavailableError } from "./codex/model-rejection.js";

import type { IsolatedRefusalCode } from "./isolated-executor.js";

const INIT_REFUSALS = {
  init_tools: "init tools refused",
  init_mcp: "init MCP servers refused",
  init_plugin: "init plugin refused",
  init_agents: "init agents refused",
  init_skills: "init skills refused",
  init_malformed: "malformed init frame",
  content_before_init: "content before init verification",
} satisfies Record<IsolatedRefusalCode, string>;

const REFUSALS = {
  ...INIT_REFUSALS,
  no_init: "the SDK never reported its tool set (no init frame)",
  tool_unallowed: "unallowed tool block",
  tool_repeated: "repeated tool block",
  tool_budget: "tool budget exceeded",
  cleanup_unconfirmed: "cleanup unconfirmed",
  cleanup_callback_missing: "cleanup confirmation callback missing",
} as const;
type Subreason = keyof typeof REFUSALS;
type PriorFailure = Subreason | "malformed" | "checker_unavailable" | "unknown";

/** Only worker-owned categories cross the logger boundary, never SDK/frame/error text. */
export class CrossCheckConfinementError extends Error {
  constructor(readonly subreason: Subreason, readonly priorFailure?: PriorFailure) {
    super(`cross-check confinement refused: ${REFUSALS[subreason]}`);
    this.name = "CrossCheckConfinementError";
  }
}

export function priorCrossCheckFailure(error: unknown): PriorFailure {
  if (error instanceof CrossCheckConfinementError) return error.subreason;
  if (error instanceof CrossCheckMalformedError) return "malformed";
  if (error instanceof CrossCheckCheckerUnavailableError) return "checker_unavailable";
  return "unknown";
}

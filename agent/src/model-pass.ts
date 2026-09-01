import type { HookInput, HookJSONOutput } from "@anthropic-ai/claude-agent-sdk";

/** A PreToolUse deny for EVERY tool: the advice runners (judge/review/summary) are
 *  read-only. A deny is authoritative even under bypassPermissions (the same property
 *  guardrails.ts relies on). */
export function buildDenyAllHook(reason: string) {
  return async (_input: HookInput): Promise<HookJSONOutput> => ({
    hookSpecificOutput: {
      hookEventName: "PreToolUse",
      permissionDecision: "deny",
      permissionDecisionReason: reason,
    },
  });
}

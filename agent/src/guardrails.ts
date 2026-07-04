// Layered SDK guardrails (PRD #4 §Guardrails, primary directive).
//
// This module is the defense-in-depth layer: the agent already has no push
// credential (the worker holds the PAT and performs every authenticated git
// op), so these hooks/allowlists exist so that even a prompt-injected model
// that *tries* to push, force-mutate the repo, read credentials, or snoop the
// process table is denied at the tool boundary — before GitLab would reject it.
//
// A PreToolUse `permissionDecision: 'deny'` blocks the tool even under
// `bypassPermissions` (a deny from any hook is authoritative), which is exactly
// why the worker runs `bypassPermissions` (allow-by-default, deny-specific) plus
// this deny-hook rather than `default` (which hangs headless) or `dontAsk`
// (deny-by-default, too tight for the coder subagent's broad file/bash needs).

import type { HookInput, HookJSONOutput } from "@anthropic-ai/claude-agent-sdk";
import type { Logger } from "./log.js";

/**
 * The subagent-invocation tool. Blocking it on each mapped subagent (see
 * agents.ts) is what enforces "the defined subagents can be invoked by the lead,
 * but no agent can spawn beyond them" — i.e. no nested/unbounded Agent spawning.
 * The lead keeps this tool so it can delegate to coder/reviewer/tester.
 */
export const NESTED_AGENT_TOOL = "Agent";

/** Result of screening one Bash command against the deny-list. */
export interface BashScreenResult {
  denied: boolean;
  /** Static (content-free) reason when denied — safe to persist/log. */
  reason?: string;
}

// Each rule pairs a matcher against the (whitespace-normalized, lowercased)
// command with a STATIC reason. Reasons never echo the command, so a denial can
// be surfaced to the run stream without leaking attacker-influenced text.
interface DenyRule {
  test: (normalized: string) => boolean;
  reason: string;
}

/** A git subcommand appears anywhere in the (possibly chained) command line. */
function hasGit(normalized: string): boolean {
  return /(^|[\s;|&(])git(\s|$)/.test(normalized);
}

const DENY_RULES: DenyRule[] = [
  // Any push — the primary directive. Covers `git push`, `git push --force`,
  // `git push origin main`, force-with-lease, etc.
  {
    test: (c) => /(^|[\s;|&(])git\s+push(\s|$|;|&|\|)/.test(c),
    reason: "denied by guardrail: git push is not permitted (the worker opens MRs; the agent never pushes)",
  },
  // Remote mutation — repointing origin could exfiltrate or bypass the MR flow.
  {
    test: (c) => /(^|[\s;|&(])git\s+remote\s+set-url(\s|$)/.test(c),
    reason: "denied by guardrail: git remote mutation is not permitted",
  },
  // Any force flag in a git command. Scoped to git so ordinary non-git `-f`
  // (e.g. `rm -f`, `grep -f`) still works; the intent here is force-push and
  // force repo mutation (git push -f, git branch -f, git checkout -f, …).
  {
    test: (c) => hasGit(c) && /(^|\s)(--force(-with-lease)?|-f)(\s|=|$)/.test(c),
    reason: "denied by guardrail: forced git operations are not permitted",
  },
  // Credential / secret reads. `git config --get`/`--list` and a bare
  // `env`/`printenv` could dump configured values or the environment. (The PAT
  // is off-disk and off-env for the agent, but the auditor requires the read
  // itself be denied as defense-in-depth.)
  {
    test: (c) => /(^|[\s;|&(])git\s+config\s+([^\n]*\s)?(--get|--get-all|--list|-l)(\s|$)/.test(c),
    reason: "denied by guardrail: reading git config values is not permitted",
  },
  {
    test: (c) => /(^|[\s;|&(])(env|printenv)(\s|$|;|&|\|)/.test(c),
    reason: "denied by guardrail: reading the process environment is not permitted",
  },
  // Process-table / /proc snooping (auditor requirement): argv/environ of any
  // process could in principle reveal secrets, so both are denied.
  {
    test: (c) => /(^|[\s;|&(])ps(\s|$|;|&|\|)/.test(c),
    reason: "denied by guardrail: listing processes is not permitted",
  },
  {
    test: (c) => /\/proc\//.test(c),
    reason: "denied by guardrail: reading /proc is not permitted",
  },
];

/** Collapse runs of whitespace and lowercase, so matchers stay simple. */
function normalizeCommand(command: string): string {
  return command.replace(/\s+/g, " ").trim().toLowerCase();
}

/**
 * Screen a single Bash command string against the deny-list. Pure and
 * synchronous so the guardrail suite can assert it directly with NO live
 * Anthropic session.
 */
export function screenBashCommand(command: string): BashScreenResult {
  const normalized = normalizeCommand(command);
  if (!normalized) return { denied: false };
  for (const rule of DENY_RULES) {
    if (rule.test(normalized)) return { denied: true, reason: rule.reason };
  }
  return { denied: false };
}

/** Extract the `command` field from a Bash tool_input, if present. */
function bashCommandOf(toolInput: unknown): string | undefined {
  if (toolInput && typeof toolInput === "object" && "command" in toolInput) {
    const cmd = (toolInput as { command?: unknown }).command;
    if (typeof cmd === "string") return cmd;
  }
  return undefined;
}

/**
 * Build the PreToolUse hook callback. Fires (with `matcher: 'Bash'`) before any
 * Bash tool runs; a matching command is denied with a static reason. Anything
 * that is not a Bash tool call, or a Bash command that passes the deny-list,
 * returns no decision (the tool proceeds under `bypassPermissions`).
 */
export function buildPreToolUseHook(log: Logger): (input: HookInput) => Promise<HookJSONOutput> {
  return async (input: HookInput): Promise<HookJSONOutput> => {
    if (input.hook_event_name !== "PreToolUse" || input.tool_name !== "Bash") {
      return {};
    }
    const command = bashCommandOf(input.tool_input);
    if (command === undefined) return {};

    const screen = screenBashCommand(command);
    if (!screen.denied) return {};

    // Log the denial (reason only — never the command) so an operator can see
    // the guardrail firing without the attacker-influenced text reaching logs.
    log.warn("guardrail denied a Bash command", { reason: screen.reason });
    return {
      hookSpecificOutput: {
        hookEventName: "PreToolUse",
        permissionDecision: "deny",
        permissionDecisionReason: screen.reason ?? "denied by guardrail",
      },
    };
  };
}

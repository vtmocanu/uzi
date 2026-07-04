// PRD #3 templates → programmatic SDK subagents (PRD #4 §Agent assembly).
//
// The claim delivers each template as structured fields
// (name/description/prompt_body/tools/model). We map them to programmatic
// `AgentDefinition`s passed via `query({ options: { agents } })` — NOT to
// `.claude/agents/*.md` files, which `settingSources: []` would never load
// anyway (that isolation is the repo-borne prompt-injection defense).
//
// Lead vs subagents: the templates model both the lead orchestrator and its
// worker subagents (coder/reviewer/tester). The wire contract carries no
// explicit role flag, so the lead is identified by name convention
// (`lead`/`orchestrator`); its prompt_body becomes the main-thread system
// prompt and it is NOT also registered as an invokable subagent. Every other
// template becomes an invokable subagent.
//
// Read-only roles (reviewer/tester) are enforced via each AgentDefinition's
// `tools` allowlist — subagents inherit the parent's `bypassPermissions` mode
// and cannot override it, so the allowlist (already excluding Edit/Write in the
// PRD #3 templates) is what makes them read-only. Every subagent additionally
// disallows the Agent tool, so no subagent can spawn further nested agents.

import type { AgentDefinition } from "@anthropic-ai/claude-agent-sdk";
import type { AgentTemplate } from "./protocol.js";
import { NESTED_AGENT_TOOL } from "./guardrails.js";

const LEAD_NAME_RE = /^(lead|orchestrator)$/i;

export interface AssembledAgents {
  /** Invokable subagents, keyed by name, for `options.agents`. */
  subagents: Record<string, AgentDefinition>;
  /** The lead template's prompt_body, if a lead template was provided. */
  leadSystemPrompt?: string;
  /** The lead template's model override, if any. */
  leadModel?: string;
}

/** Map one template's structured fields onto an SDK AgentDefinition. */
function toDefinition(t: AgentTemplate): AgentDefinition {
  const def: AgentDefinition = {
    description: t.description,
    prompt: t.prompt_body,
    // No subagent may spawn nested agents (defense-in-depth over the fact that
    // `agents` + settingSources:[] already limit spawnable agents to these).
    disallowedTools: [NESTED_AGENT_TOOL],
  };
  // A non-empty allowlist makes the role read-only when Edit/Write are absent
  // (PRD #3 excludes them from reviewer/tester). Absent/empty ⇒ inherit all.
  if (t.tools && t.tools.length > 0) def.tools = [...t.tools];
  if (t.model) def.model = t.model;
  return def;
}

/**
 * Partition templates into the lead (by name convention) and invokable
 * subagents, mapping each subagent to an SDK AgentDefinition. Duplicate names
 * collapse (last wins) — the SDK `agents` map is name-keyed regardless.
 */
export function assembleAgents(templates: AgentTemplate[]): AssembledAgents {
  const result: AssembledAgents = { subagents: {} };
  let leadSeen = false;
  for (const t of templates) {
    if (!leadSeen && LEAD_NAME_RE.test(t.name)) {
      leadSeen = true;
      result.leadSystemPrompt = t.prompt_body;
      if (t.model) result.leadModel = t.model;
      continue;
    }
    result.subagents[t.name] = toDefinition(t);
  }
  return result;
}

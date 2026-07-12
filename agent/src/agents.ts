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
//
// null/absent/empty `tools` = INHERIT ALL (PRD #3 wire contract). We honor it by
// leaving the allowlist UNSET, so the SDK grants the full toolset — the built-in
// `coder` template deliberately ships with no `tools:` line for exactly this
// reason (pinned by the api-side agenttmpl tests), and downgrading it to a
// read-only default would silently break the implement role. The read-only
// roles restrict themselves by DECLARING a toolset, not by us defaulting.
//
// ACCEPTED RESIDUAL (auditor F4, team-lead ruling 2026-07-04): a *malformed*
// custom template with null tools therefore inherits all tools. That is bounded
// by the global Bash PreToolUse deny-hook, the file-tool worktree jail, and the
// agent holding no credentials — and is no looser than the built-in
// general-purpose subagent, which our allowlist never constrained either. The
// per-template allowlist is a correctness/cost shaper, not the primary-directive
// boundary; do NOT "re-fix" this into a fail-closed default (it breaks coder).

import type { AgentDefinition } from "@anthropic-ai/claude-agent-sdk";
import type { AgentSource, AgentTemplate } from "./protocol.js";
import { NESTED_AGENT_TOOL } from "./guardrails.js";
import { SIGNAL_SERVER_NAME } from "./signals.js";
import { qualifiedSkillName } from "./skills-plugin.js";

// Server-level MCP denial (PRD #43 M2 / Decision 3). A `mcp__<server>` entry in
// disallowedTools removes EVERY tool the named in-process MCP server exposes
// (sdk.d.ts:48), so this denies both workflow-signal tools
// (mcp__uzi__submit_plan / mcp__uzi__signal_done) to every subagent. The run's
// plan gate and its done→MR handoff are the lead's alone: a subagent (coder,
// reviewer, …) — buggy or prompt-injected — must never reach them and end the
// loop with a partial, unreviewed tree. Belt-and-suspenders with the
// main-thread-only signal scan in signals.ts: this stops the tool_use from ever
// being made; that scan ignores it even if it somehow were.
const SIGNAL_SERVER_DENY = `mcp__${SIGNAL_SERVER_NAME}`;

/**
 * A template is the lead orchestrator (routed to the main thread, not registered
 * as an invokable subagent) when its name matches this convention. The product
 * ships exactly one such builtin (`lead`, in api/internal/agenttmpl/builtins/);
 * a test pins that shipped name to this regex so the two never drift.
 */
export const LEAD_NAME_RE = /^(lead|orchestrator)$/i;

export interface AssembledAgents {
  /** Invokable subagents, keyed by name, for `options.agents`. */
  subagents: Record<string, AgentDefinition>;
  /** The lead template's prompt_body, if a lead template was provided. */
  leadSystemPrompt?: string;
  /** The lead template's model override, if any. */
  leadModel?: string;
}

/** Map one template's structured fields onto an SDK AgentDefinition. availableSkills
 *  (when given) restricts the template's ALLOCATED skills to the worker's
 *  materialized survivors; repoSkillNames are the repo-borne skills, which carry no
 *  allocation and so attach to EVERY subagent. */
function toDefinition(
  t: AgentTemplate,
  availableSkills?: ReadonlySet<string>,
  repoSkillNames: readonly string[] = [],
): AgentDefinition {
  const def: AgentDefinition = {
    description: t.description,
    prompt: t.prompt_body,
    // No subagent may spawn nested agents (defense-in-depth over the fact that
    // `agents` + settingSources:[] already limit spawnable agents to these), and
    // no subagent may reach the run's workflow-signal MCP tools (SIGNAL_SERVER_DENY
    // above — the plan gate and done→MR handoff are the lead's alone).
    disallowedTools: [NESTED_AGENT_TOOL, SIGNAL_SERVER_DENY],
  };
  // A non-empty template list is an explicit allowlist honored verbatim (and is
  // what makes reviewer/tester read-only). null/absent/empty ⇒ leave `tools`
  // unset so the SDK inherits all — the PRD #3 contract (see header). Repo skills
  // do NOT touch this allowlist: enabling a skill via `skills` is sufficient (per
  // sdk.d.ts:44 a tools-`Skill` grant is deprecated), so a read-only subagent
  // (reviewer/tester) still expands a repo skill without any tools widening.
  if (t.tools && t.tools.length > 0) def.tools = [...t.tools];
  if (t.model) def.model = t.model;
  // Skill scoping (PRD #16): a subagent preloads its own ALLOCATED delivered
  // skills (filtered to the materialized survivors, so it never lists a skill
  // dropped by cap/collision) PLUS every repo-borne skill — repo skills carry no
  // allocation and are enabled for ALL templates in the run (PRD §Worker point 3).
  // Delivered skills stay per-template; only repo skills are all-templates. Always
  // set explicitly (possibly []). The lead is the main thread, covered by the
  // top-level union, so it is not registered here.
  const allocated = (t.skills ?? []).filter((n) => !availableSkills || availableSkills.has(n));
  const names = repoSkillNames.length > 0 ? [...new Set([...allocated, ...repoSkillNames])] : allocated;
  def.skills = names.map(qualifiedSkillName);
  return def;
}

/**
 * Partition templates into the lead (by name convention) and invokable
 * subagents, mapping each subagent to an SDK AgentDefinition. Duplicate names
 * collapse (last wins) — the SDK `agents` map is name-keyed regardless.
 * availableSkills, when given, restricts each subagent's allocated skills to the
 * worker's materialized survivor set (bare names); repoSkillNames are the
 * materialized repo-borne survivors, attached to every subagent (all-templates).
 */
export function assembleAgents(
  templates: AgentTemplate[],
  availableSkills?: ReadonlySet<string>,
  repoSkillNames: readonly string[] = [],
): AssembledAgents {
  const result: AssembledAgents = { subagents: {} };
  let leadSeen = false;
  for (const t of templates) {
    if (!leadSeen && LEAD_NAME_RE.test(t.name)) {
      leadSeen = true;
      result.leadSystemPrompt = t.prompt_body;
      if (t.model) result.leadModel = t.model;
      continue;
    }
    result.subagents[t.name] = toDefinition(t, availableSkills, repoSkillNames);
  }
  return result;
}

/**
 * Map a template list to a name-keyed subagent map WITHOUT the lead partition:
 * EVERY template becomes an invokable subagent, including one named `lead`. This
 * is the repo-source path (PRD #37 Decision 3): a repo file named `lead` is just
 * another subagent candidate, never the main-thread orchestrator — that always
 * comes from the claim payload. Feeding the repo roster through `assembleAgents`
 * instead would route a repo-authored `lead.prompt_body` into the lead system
 * prompt, the exact repo-borne injection `settingSources: []` exists to prevent.
 * Excluded names are dropped. Skill scoping is identical to assembleAgents.
 */
export function subagentsFromTemplates(
  templates: AgentTemplate[],
  exclude: ReadonlySet<string>,
  availableSkills?: ReadonlySet<string>,
  repoSkillNames: readonly string[] = [],
): Record<string, AgentDefinition> {
  const subagents: Record<string, AgentDefinition> = {};
  for (const t of templates) {
    if (exclude.has(t.name)) continue;
    subagents[t.name] = toDefinition(t, availableSkills, repoSkillNames);
  }
  return subagents;
}

/**
 * The subagent roster to run the IMPLEMENT phase with, given the approved
 * selection (PRD #37 Decision 5). The `lead` is NOT here — it stays uzi's builtin
 * from the claim payload under either source, so callers keep using
 * assembleAgents' leadSystemPrompt/leadModel.
 *
 *   - "own":  the owner's already-assembled subagents (lead already partitioned
 *             out by assembleAgents), minus the excluded names.
 *   - "repo": the detected repo roster mapped subagents-only (a repo `lead` stays
 *             a subagent), minus the excluded names.
 *
 * Exclusions are re-applied here worker-side; the API validated membership (M2)
 * but the worker owns what actually reaches the SDK. An exclusion naming an agent
 * not in the chosen source is a harmless no-op (nothing to remove).
 */
export function selectSubagents(
  source: AgentSource,
  ownSubagents: Record<string, AgentDefinition>,
  repoTemplates: AgentTemplate[],
  exclusions: readonly string[],
  availableSkills?: ReadonlySet<string>,
  repoSkillNames: readonly string[] = [],
): Record<string, AgentDefinition> {
  const exclude = new Set(exclusions);
  if (source === "repo") {
    return subagentsFromTemplates(repoTemplates, exclude, availableSkills, repoSkillNames);
  }
  const out: Record<string, AgentDefinition> = {};
  for (const [name, def] of Object.entries(ownSubagents)) {
    if (!exclude.has(name)) out[name] = def;
  }
  return out;
}

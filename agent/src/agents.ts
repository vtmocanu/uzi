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
import { MEMORY_SERVER_NAME } from "./memory-tools.js";
import { qualifiedSkillName } from "./skills-plugin.js";

// Server-level MCP denial (PRD #43 M2 / Decision 3). A `mcp__<server>` entry in
// disallowedTools removes EVERY tool the named in-process MCP server exposes
// (sdk.d.ts:48), so this denies both workflow-signal tools
// (mcp__uzi__submit_plan / mcp__uzi__signal_done) to every subagent. The run's
// plan gate and its done→MR handoff are the lead's alone: a subagent (coder,
// reviewer, …) — buggy or prompt-injected — must never reach them and end the
// loop with a partial, unreviewed tree. This is DEFENSE-IN-DEPTH: it should stop
// the tool_use from ever being made, but whether disallowedTools wins over a
// custom template's explicit `tools` allowlist is unproven from the SDK types, so
// the load-bearing guarantee is the main-thread-only signal scan in signals.ts
// (scanSignals ignores a subagent-borne signal even if the SDK let the call
// through). Keep both.
const SIGNAL_SERVER_DENY = `mcp__${SIGNAL_SERVER_NAME}`;

// PRD #90: save_memory is the LEAD's alone (OQ-B — one writer, clean provenance).
// Denying the whole `mcp__memory` server to every subagent (same mechanism as
// SIGNAL_SERVER_DENY) keeps a coder/reviewer — buggy or prompt-injected — from
// writing cross-run memory. Defense-in-depth: the entry is still per-(user,repo)
// scoped and server-capped, but provenance stays "the lead saved this".
const MEMORY_SERVER_DENY = `mcp__${MEMORY_SERVER_NAME}`;

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
 *  materialized survivors; allTemplateSkillNames are the skills that carry NO
 *  allocation to honor and so attach to EVERY subagent — repo-borne skills always
 *  (assembleAgents), plus the run's whole surviving delivered set on the
 *  repo-source path (subagentsFromTemplates, PRD #72 M1 / Decision 6). Callers
 *  pass survivors only; this function does not re-filter that list. */
function toDefinition(
  t: AgentTemplate,
  availableSkills?: ReadonlySet<string>,
  allTemplateSkillNames: readonly string[] = [],
): AgentDefinition {
  const def: AgentDefinition = {
    description: t.description,
    prompt: t.prompt_body,
    // No subagent may spawn nested agents (defense-in-depth over the fact that
    // `agents` + settingSources:[] already limit spawnable agents to these), reach
    // the run's workflow-signal MCP tools (SIGNAL_SERVER_DENY — the plan gate and
    // done→MR handoff are the lead's alone), or write cross-run memory
    // (MEMORY_SERVER_DENY — save_memory is lead-only, PRD #90 OQ-B).
    disallowedTools: [NESTED_AGENT_TOOL, SIGNAL_SERVER_DENY, MEMORY_SERVER_DENY],
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
  // dropped by cap/collision) PLUS every all-templates skill — those carry no
  // allocation to honor and are enabled for ALL templates in the run (repo-borne
  // skills, PRD §Worker point 3; on a repo-source roster also the delivered
  // survivors, PRD #72 M1). On the own-template path the all-templates list is the
  // repo skills alone, so delivered skills stay per-template there. Always set
  // explicitly (possibly []). The lead is the main thread, covered by the
  // top-level union, so it is not registered here.
  //
  // De-dup: since PRD #72 the two lists can OVERLAP on the repo path (the repo
  // survivors are members of the run's surviving set), and a plain concat would
  // put a duplicate `uzi:<name>` on the SDK. This Set and the one
  // subagentsFromTemplates builds its list with are REDUNDANT WITH EACH OTHER —
  // measured 2026-07-26 by mutation, removing either ALONE changes no output and
  // only removing BOTH reddens the tests. Both are kept deliberately: this one
  // predates #72 and also covers allocated ∩ all-templates, that one collapses the
  // overlap at the point where #72 creates it. Do not "simplify" one away on the
  // grounds that a test still passes — no single test can hold it.
  const allocated = (t.skills ?? []).filter((n) => !availableSkills || availableSkills.has(n));
  const names =
    allTemplateSkillNames.length > 0 ? [...new Set([...allocated, ...allTemplateSkillNames])] : allocated;
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
 * Excluded names are dropped.
 *
 * Skill scoping DIFFERS from assembleAgents (PRD #72 M1 / Decision 6): every
 * subagent here gets the run's whole SURVIVING skill set, not a per-template
 * slice. Allocations key on `template_id` and a repo roster has no template rows,
 * so `t.skills` is always absent (repoagents.ts builds name/description/
 * prompt_body only) and per-template scoping would deliver nothing at all — a run
 * started with agents from git would silently lose every delivered skill its owner
 * allocated. With no allocation signal to honor, honoring none is the honest
 * reading, and it is exactly the all-templates rule repo-borne skills already
 * follow. `availableSkills` IS that surviving set (built from the materialized run
 * union), so a skill dropped by the per-run cap or a name collision is not in it
 * and therefore reaches nobody. Passing no survivor set (a caller with no skills)
 * degrades to the repo-borne names alone.
 */
export function subagentsFromTemplates(
  templates: AgentTemplate[],
  exclude: ReadonlySet<string>,
  availableSkills?: ReadonlySet<string>,
  repoSkillNames: readonly string[] = [],
): Record<string, AgentDefinition> {
  // Union order follows the run union (delivered survivors, then repo survivors),
  // since availableSkills is built from it; the repo names are already members in
  // production and are unioned in only so a caller passing a delivered-only set
  // still gets them. That membership is exactly why this de-dups — see the
  // matching note in toDefinition for why BOTH Sets stay despite being redundant.
  const allTemplateSkillNames = availableSkills ? [...new Set([...availableSkills, ...repoSkillNames])] : repoSkillNames;
  const subagents: Record<string, AgentDefinition> = {};
  for (const t of templates) {
    if (exclude.has(t.name)) continue;
    subagents[t.name] = toDefinition(t, availableSkills, allTemplateSkillNames);
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
 *             out by assembleAgents), minus the excluded names. Skills stay
 *             per-template — the allocations ARE the admin's scoping surface here,
 *             so this path is deliberately untouched by PRD #72 M1.
 *   - "repo": the detected repo roster mapped subagents-only (a repo `lead` stays
 *             a subagent), minus the excluded names. Every subagent gets the run's
 *             whole surviving skill set (PRD #72 M1) — see subagentsFromTemplates.
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

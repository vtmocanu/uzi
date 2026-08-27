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
// Read-only roles are enforced via each AgentDefinition's `tools` allowlist —
// subagents inherit the parent's `bypassPermissions` mode and cannot override it,
// so an allowlist that excludes Edit/Write is what makes them read-only. The
// property is the ALLOWLIST, never the role name: reviewer/auditor are the pair
// usually cited, and citing a pair is what made the previous version of this
// sentence false — it said "exactly those two roles" when five qualified
// (reviewer, auditor, fact-checker, researcher, web-ux, measured 2026-08-03).
// Treat that list as a dated sample, not a definition; derive it from the
// frontmatter when you need it, because the roster moves. Every subagent
// additionally disallows the Agent tool, so no subagent can spawn nested agents.
//
// Roles that DECLARE write tools (architect, tester, documenter, spec-keeper)
// keep them on the implement turn and lose them on the PLAN turn — as does
// anything that inherits all, `coder` included, which declares no tools at all.
// See planTurnSubagents (#203).
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
import { NESTED_AGENT_TOOL, WRITE_PATH_TOOLS } from "./guardrails.js";
import { SIGNAL_SERVER_NAME } from "./signals.js";
import { MEMORY_SERVER_NAME } from "./memory-tools.js";
import { qualifiedSkillName } from "./skills-plugin.js";
import { reportIncidentalIssueToolName, FINDINGS_SERVER_NAME } from "./findings-tools.js";
import { FORGE_SERVER_NAME } from "./forge-tools.js";
import { FINDINGS_NUDGE_APPEND, WORKER_RUNTIME_APPEND } from "./prompt.js";

// Server-level MCP denial (PRD #43 M2 / Decision 3). A `mcp__<server>` entry in
// disallowedTools removes EVERY tool the named in-process MCP server exposes
// (sdk.d.ts:48), so this denies both workflow-signal tools
// (mcp__uzi__submit_plan / mcp__uzi__signal_done) to every subagent. The run's
// plan gate and its done→MR handoff are the lead's alone: a subagent (coder,
// reviewer, …) — buggy or prompt-injected — must never reach them and end the
// loop with a partial, unreviewed tree. This is DEFENSE-IN-DEPTH: it should stop
// the tool_use from ever being made. A PRD #158 M0 spike (2026-08-10) MEASURED
// that a server-level disallowedTools entry does override a subagent's explicit
// `tools` allowlist on the pinned SDK, so this deny is effective — but that ran on
// one claude CLI build, and the load-bearing guarantee stays the main-thread-only
// signal scan in signals.ts (scanSignals ignores a subagent-borne signal even if
// the SDK ever let the call through), because it holds regardless of SDK gating.
// Keep both.
const SIGNAL_SERVER_DENY = `mcp__${SIGNAL_SERVER_NAME}`;

// PRD #90: save_memory is the LEAD's alone (OQ-B — one writer, clean provenance).
// Denying the whole `mcp__memory` server to every subagent (same mechanism as
// SIGNAL_SERVER_DENY) keeps a coder/reviewer — buggy or prompt-injected — from
// writing cross-run memory. Defense-in-depth: the entry is still per-(user,repo)
// scoped and server-capped, but provenance stays "the lead saved this".
const MEMORY_SERVER_DENY = `mcp__${MEMORY_SERVER_NAME}`;

// PRD #457: the incidental-findings tool name is a pure constant; compute once.
const FINDINGS_TOOL_NAME = reportIncidentalIssueToolName();

/** Membership form of WRITE_PATH_TOOLS, for planTurnSubagents' filter. Derived,
 *  never re-typed — guardrails.ts owns the list (#203). */
const WRITE_TOOL_SET: ReadonlySet<string> = new Set<string>(WRITE_PATH_TOOLS);

/**
 * Whether a subagent CAN edit files, derived from its own tool allowlist exactly
 * as the SDK will resolve it (PRD #266 M1). An ABSENT or EMPTY `tools` list is the
 * inherit-all contract (PRD #3 — see header), so the SDK grants the full toolset
 * and the agent CAN write; a non-empty list is an explicit allowlist, so the agent
 * can write iff it names at least one of WRITE_PATH_TOOLS.
 *
 * MUST be called on a PRE-STRIP definition (`assembleAgents`' map), never on a
 * plan-turn roster: `planTurnSubagents` subtracts the write tools, so a capability
 * read off that map would report `coder` (inherit-all, hence write-capable) as
 * read-only — the exact bug PRD #266 fixes. `def.disallowedTools` is not consulted
 * here: the plan turn expresses its downgrade there, and the roster line describes
 * the agent's own declared capability, not the plan turn's temporary restriction.
 */
export function subagentCanWrite(def: AgentDefinition): boolean {
  if (!def.tools || def.tools.length === 0) return true;
  return def.tools.some((t) => WRITE_TOOL_SET.has(t));
}

/**
 * Build a name→can-write map over a subagent roster (PRD #266 M1). Feed the
 * PRE-STRIP map (`assembleAgents`' `subagents`) so the capabilities reflect each
 * agent's declared toolset, not a plan turn's write-stripped roster. The result is
 * looked up by roster name where the lead's "Available subagents" line is built.
 */
export function subagentWriteCapabilities(
  subagents: Record<string, AgentDefinition>,
): Record<string, boolean> {
  const caps: Record<string, boolean> = {};
  for (const [name, def] of Object.entries(subagents)) {
    caps[name] = subagentCanWrite(def);
  }
  return caps;
}

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
    // Append two shared constants to every subagent's prompt regardless of its body,
    // so shared-library repo agents get them WITHOUT editing their files:
    //  - FINDINGS_NUDGE_APPEND (PRD #457 M3): the incidental-findings discovery nudge,
    //    same constant as the lead nudge (buildLeadSystemPrompt) — one source of wording.
    //  - WORKER_RUNTIME_APPEND (PRD #702 M5): the subagent-channel mirror of the lead's
    //    deps-provisioning notes, so every subagent carries the worker-runtime dependency
    //    guidance regardless of role body.
    prompt: `${t.prompt_body}\n\n${FINDINGS_NUDGE_APPEND}\n\n${WORKER_RUNTIME_APPEND}`,
    // No subagent may spawn nested agents (defense-in-depth over the fact that
    // `agents` + settingSources:[] already limit spawnable agents to these), reach
    // the run's workflow-signal MCP tools (SIGNAL_SERVER_DENY — the plan gate and
    // done→MR handoff are the lead's alone), or write cross-run memory
    // (MEMORY_SERVER_DENY — save_memory is lead-only, PRD #90 OQ-B).
    disallowedTools: [NESTED_AGENT_TOOL, SIGNAL_SERVER_DENY, MEMORY_SERVER_DENY],
  };
  // A non-empty template list is an explicit allowlist honored verbatim (and is
  // what makes reviewer/auditor read-only). null/absent/empty ⇒ leave `tools`
  // unset so the SDK inherits all — the PRD #3 contract (see header). Repo skills
  // do NOT touch this allowlist: enabling a skill via `skills` is sufficient (per
  // sdk.d.ts:44 a tools-`Skill` grant is deprecated), so a read-only subagent
  // (reviewer/auditor) still expands a repo skill without any tools widening.
  if (t.tools && t.tools.length > 0) def.tools = [...t.tools];
  // PRD #457 M1: grant the incidental-findings tool to any subagent with a non-empty
  // allowlist (the broad-reading read-only roles that would otherwise be unable to
  // call it). When `tools` is unset (inherit-all, e.g. coder) the tool is already
  // available — do NOT materialize an allowlist. Dedup-guarded. NOT a write tool, so
  // it survives the plan-turn write-strip (planTurnSubagents).
  if (def.tools && !def.tools.includes(FINDINGS_TOOL_NAME)) def.tools = [...def.tools, FINDINGS_TOOL_NAME];
  // PRD #158 / issue #581: an in-process MCP server (forge, findings) is registered
  // once in the top-level options.mcpServers (sdk-executor.ts); on the pinned SDK that
  // map reaches only the LEAD session. To let an allowlisted subagent (e.g. the
  // fact-checker, granted the mcp__forge__* read tools) actually reach it, name the
  // server on this AgentDefinition via the string form of mcpServers (sdk.d.ts:59,120)
  // — the only way to attach an in-process instance to a subagent. Derived from the
  // resolved allowlist (which now already includes the findings tool from the line
  // above), so it self-tracks whatever the template declares. Inherit-all subagents
  // (no `tools`) are left untouched. memory/signal are DELIBERATELY excluded — they are
  // server-denied to every subagent above (MEMORY_SERVER_DENY / SIGNAL_SERVER_DENY).
  if (def.tools) {
    const servers: string[] = [];
    for (const server of [FORGE_SERVER_NAME, FINDINGS_SERVER_NAME]) {
      const prefix = `mcp__${server}__`;
      if (def.tools.some((name) => name.startsWith(prefix))) servers.push(server);
    }
    if (servers.length > 0) def.mcpServers = servers;
  }
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
  // NOTE for anyone mutation-testing this: the `new Set` is PROVABLY EQUIVALENT to
  // a plain concat today, so that mutant cannot be killed and its survival is not a
  // gap. Proof that `allocated ∩ allTemplateSkillNames` is empty on both paths:
  //   - repo path: repo templates carry no allocations at all (`t.skills` is never
  //     populated by repoagents.ts), so `allocated` is [];
  //   - own path: `allTemplateSkillNames` is the repo-borne survivors, and a repo
  //     skill whose name collides with a delivered one is dropped upstream at
  //     `agent/src/skills-run.ts` (the DROP_REPO_COLLISION branch), so the two lists
  //     are disjoint by construction.
  // It is kept as a boundary guard — this is the single place `def.skills` is built
  // for the SDK, both invariants live in another file, and a duplicate `uzi:<name>`
  // would go straight to the enable-list. What IS pinned is the CONTENT and ORDER of
  // this list (agents.test.ts, sdk-executor.test.ts), not the de-dup.
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
 * so `t.skills` is always absent (repoagents.ts never populates `.skills`) and
 * per-template scoping would deliver nothing at all — a run started with agents
 * from git would silently lose every delivered skill its owner allocated. With no
 * allocation signal to honor, honoring none is the honest reading, and it is
 * exactly the all-templates rule repo-borne skills already follow. A skill dropped
 * by the per-run cap or a name collision is not in the survivor set and therefore
 * reaches nobody.
 *
 * PRECONDITION: `availableSkills`, when given, is the run's WHOLE survivor set —
 * delivered survivors ∪ repo survivors. Both callers derive it from the same
 * `runSkills` the repo names are filtered out of (`agent/src/skills-run.ts`), so
 * `repoSkillNames ⊆ availableSkills` structurally, and passing a delivered-only
 * set here would silently drop the repo-borne skills.
 *
 * `repoSkillNames` is therefore used ONLY when no survivor set is supplied. That
 * fallback is the pre-#72 behaviour and the reason the parameter survives.
 *
 * NOT run-kind gated, deliberately. Decision 13 gates M3/M4/M5 to `kind ===
 * "issue"`; Decision 6 draws no kind distinction, so a `ci_fix` or `self_improve`
 * run that resolves to the repo roster gets this rule too. Correct as written —
 * do not "fix" it into a gate to match the surrounding milestones.
 */
export function subagentsFromTemplates(
  templates: AgentTemplate[],
  exclude: ReadonlySet<string>,
  availableSkills?: ReadonlySet<string>,
  repoSkillNames: readonly string[] = [],
): Record<string, AgentDefinition> {
  // The survivor set already CONTAINS the repo survivors (see PRECONDITION), so
  // unioning `repoSkillNames` back in would append a subset to its own superset —
  // manufacturing a duplicate hazard that then needs guarding. Order follows the
  // run union: delivered survivors, then repo survivors.
  const allTemplateSkillNames = availableSkills ? [...availableSkills] : repoSkillNames;
  const subagents: Record<string, AgentDefinition> = {};
  for (const t of templates) {
    if (exclude.has(t.name)) continue;
    subagents[t.name] = toDefinition(t, availableSkills, allTemplateSkillNames);
  }
  return subagents;
}

/**
 * PRD #305: when a schedule opts into "apply model also to agents", overwrite every
 * subagent's model with the run's resolved model (the same value the lead runs on),
 * overriding each template's own `model:` pin. Applied to BOTH rosters by the
 * executor — the own roster (assembled.subagents, before the plan-turn copy) and the
 * freshly-built repo roster — which is why it is a post-build mutation here rather
 * than a branch in `toDefinition`: the own roster is built and plan-turn-copied before
 * the run model resolves, so `toDefinition` cannot see it (PRD Decision 2 asks for one
 * mechanism covering both rosters; this helper is that mechanism). Mutates in place;
 * the lead is never in these maps (it is the main thread), so it is untouched.
 */
export function applySubagentModelOverride(
  subagents: Record<string, AgentDefinition>,
  model: string,
): void {
  for (const def of Object.values(subagents)) {
    def.model = model;
  }
}

/** What a planning turn runs with (#203): the write-stripped roster, plus the
 *  names that had to be dropped outright so the caller can report them. */
export interface PlanTurnRoster {
  /** The plan turn's `options.agents` map. */
  subagents: Record<string, AgentDefinition>;
  /** Agents whose declared allowlist consisted ONLY of write tools, in input
   *  order. Empty on every shipped builtin; see planTurnSubagents. */
  dropped: string[];
}

/**
 * The subagent roster to run a PLANNING turn with: the assembled map with every
 * file-write tool (`WRITE_PATH_TOOLS`) taken off every subagent (#203).
 *
 * WHY. Before the human approval gate, a subagent write is an uncommitted
 * worktree change the approver never saw, which the first implement-phase commit
 * then sweeps in. Several roles can write on the plan turn today — architect,
 * tester, documenter and spec-keeper declare write tools, and `coder` ships no
 * `tools:` line at all (inherit-all, deliberate — see the header) while being
 * invokable on that turn. So this operates on the ASSEMBLED MAP rather than on
 * template frontmatter: editing four templates' `tools:` lines would miss coder
 * and would also cost those roles their write tools on the implement turn, where
 * writing is the job.
 *
 * WHAT THIS IS NOT. Not a structural guarantee, and it must not be described as
 * one: every one of those roles also declares `Bash`, the turn runs under
 * `bypassPermissions`, and guardrails.ts denies no filesystem write of any kind
 * (`echo >`, `sed -i`, `tee`, `git apply` all pass). This removes the ergonomic
 * path and the model's awareness of it — defence-in-depth one layer better than
 * the prompt instruction that carried it before. The integrity property is #212.
 *
 * BOTH OPERATIONS, ALWAYS, and that is the whole design. `tools` is filtered
 * where present AND the write tools are unioned into `disallowedTools`,
 * unconditionally. Which of the two the SDK honours, and in what order, is
 * UNPROVEN here: `sdk.d.ts:44-50` documents both fields on `AgentDefinition` with
 * no ordering between them, the precedence sentence people quote
 * (`sdk.d.ts:1391-1393`) is attached to the top-level `Options.disallowedTools`,
 * and resolution happens inside the compiled `claude` binary. Doing both makes
 * the question stop being a dependency instead of betting on an answer.
 *
 * AN ALLOWLIST THAT EMPTIES DROPS THE AGENT, and that case is the one exception
 * to the paragraph above — which is exactly why it is handled rather than left to
 * fall out. A template declaring ONLY write tools (`tools: Edit, Write`, reachable
 * through the documented admin surface: `validateTemplateFields` enforces no tool
 * policy) filters to `[]`, and `[]` re-opens the precedence question this function
 * exists to close. The repo reads an empty list as INHERIT ALL in three places
 * (this file's header, `render.go`, and `agent_templates.go` which normalizes an
 * explicit `[]` to NULL precisely so one never persists), so emitting `tools: []`
 * would either DISCARD the operator's restriction and hand back the full parent
 * toolset, or grant nothing at all — decided inside the compiled binary. Neither
 * is acceptable, and neither alternative repair works: substituting a read set
 * invents capability the operator deliberately excluded, and deleting the key is
 * the inherit-all widening spelled out. An agent whose entire declared toolset is
 * writing has no coherent role in a read-only wave, so it does not attend. Note
 * `toDefinition` never emits `tools: []` either (it requires a non-empty list);
 * this function is the first path in the repo that could, and it does not.
 *
 * The DROPPED names are returned rather than logged here because this module is
 * pure. The caller must feed `Object.keys(subagents)` — the RESULT, not the input
 * map — to the plan turn's Agent-guard allowSet and to the plan prompt's
 * "Available subagents" line, or it advertises an agent the SDK cannot resolve.
 *
 * NEW OBJECTS, NEVER MUTATION. `selectSubagents`'s own-source path copies
 * REFERENCES (`out[name] = def`), so mutating a definition here would leak into
 * the implement map and strip write tools for the whole run, breaking `coder`.
 * Each entry gets a fresh object with fresh `tools`/`disallowedTools` arrays;
 * remaining fields are shared by reference and are never mutated by anyone.
 *
 * The LEAD is deliberately out of scope: it is not in this map (it is the main
 * thread), so it keeps its write tools on the plan turn. The harm argument
 * applies to a lead write identically — the lead is the accountable party the
 * human is gating, and only a `PreToolUse` deny (the recorded upgrade path)
 * would reach it. Stated, not closed.
 */
export function planTurnSubagents(subagents: Record<string, AgentDefinition>): PlanTurnRoster {
  const out: Record<string, AgentDefinition> = {};
  const dropped: string[] = [];
  for (const [name, def] of Object.entries(subagents)) {
    const next: AgentDefinition = { ...def };
    // Filter where present only: an ABSENT `tools` is the inherit-all contract
    // (PRD #3), and materializing an allowlist here would silently narrow coder
    // to whatever this function happened to think the full toolset was.
    if (def.tools) {
      const kept = def.tools.filter((t) => !WRITE_TOOL_SET.has(t));
      // PRD #457 M1 R1: toDefinition now grants the (non-write) findings tool to every
      // allowlist, so a write-ONLY custom agent reaches here as `[Edit, Write,
      // FINDINGS_TOOL_NAME]` and `kept` becomes `[FINDINGS_TOOL_NAME]` — non-empty. Exclude
      // the findings tool from the emptiness test so such an agent still DROPS (unchanged
      // behaviour), while a read-only agent keeps the findings tool through the plan turn.
      const meaningful = kept.filter((t) => t !== FINDINGS_TOOL_NAME);
      if (meaningful.length === 0) {
        dropped.push(name);
        continue;
      }
      next.tools = kept;
    }
    const denied = def.disallowedTools ?? [];
    next.disallowedTools = [...denied, ...WRITE_PATH_TOOLS.filter((t) => !denied.includes(t))];
    out[name] = next;
  }
  return { subagents: out, dropped };
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

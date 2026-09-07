// PRD #1171 (M2): the deterministic Codex prompt / tool / role renderer.
//
// This is NEW, Codex-only code. It takes the provider-neutral run inputs
// (`RunTurnRequest` / `AdviceRequest` from ../harness.ts) and produces the
// immutable per-(thread,turn) grants the {@link RunGrants} broker checks against,
// the resolved model + reasoning effort, and the rendered lead/subagent prompt
// text. It is PURE: no I/O, no clock, no randomness, and it NEVER mutates its
// input or any imported structure — so two calls on the same input are
// byte-identical, and it cannot change the Claude rendering path (agents.ts,
// sdk-executor.ts, claude-harness.ts) by construction. The only things imported
// from the Claude side are read-only constants (server names, the shared prompt
// appends, the skill-name regex) and the `RunGrants` TYPE from the broker.
//
// WHY IT MIRRORS agents.ts RATHER THAN CALLS IT. agents.ts builds Claude
// `AgentDefinition`s (tools allowlist + `disallowedTools` + plugin-qualified
// skills) for the Claude SDK. Codex has a different vocabulary (a fixed set of
// worker CALLBACK names — see broker.ts) and a single `allowedTools` grant with no
// separate deny list. So this module reproduces agents.ts's SEMANTICS against the
// canonical Codex names:
//   - inherit-all (no `tools`) ⇒ the run's full callback vocabulary;
//   - explicit allow ⇒ exactly those mapped names (unknowns stripped + reported);
//   - every subagent loses spawn/signal/cross-run-memory authority (agents.ts's
//     unconditional `disallowedTools` on every subagent);
//   - a non-empty allowlist / inherit gains the incidental-findings tool;
//   - enabling a skill NEVER widens the tool allowlist (skills ride `allowedSkills`).
//
// The canonical tool vocabulary here MIRRORS broker.ts's module-private
// `TOOL_ALIASES` / `SIGNAL_TOOLS` / `DELEGATE_TOOLS` / `canonicalName` /
// `capabilityOf`. Those are not exported, so they are reproduced (not imported);
// codex-render.test.ts pins the mapping so the two cannot drift silently.

import type {
  AdviceRequest,
  HarnessAgent,
  HarnessEffort,
  HarnessToolSet,
  RunTurnRequest,
} from "../harness.js";
import type { RunGrants } from "./broker.js";

import { SIGNAL_SERVER_NAME } from "../signals.js";
import { MEMORY_SERVER_NAME } from "../memory-tools.js";
import { reportIncidentalIssueToolName } from "../findings-tools.js";
import { SKILL_NAME_RE } from "../skills-plugin.js";
import { FINDINGS_NUDGE_APPEND, WORKER_RUNTIME_APPEND } from "../prompt.js";

// --- canonical Codex tool vocabulary (mirrors broker.ts) ----------------------

const BASH_TOOL = "Bash";
const READ_TOOL = "Read";
const SKILL_TOOL = "Skill";
const APPLY_PATCH = "apply_patch";
const SPAWN_AGENT = "spawn_agent";

/** The incidental-findings callback name (`mcp__findings__report_incidental_issue`),
 *  granted to every inherit / non-empty-allow role exactly as agents.ts does. */
const FINDINGS_TOOL_NAME = reportIncidentalIssueToolName();

/** Codex source aliases → the canonical callback name. Mirrors broker.ts
 *  `TOOL_ALIASES`: `Write`/`Edit`/`MultiEdit` are the source aliases for
 *  `apply_patch`; `Agent` is the alias for `spawn_agent`. `MultiEdit` is INCLUDED
 *  for broker parity — broker.ts's `TOOL_ALIASES` maps it too, so both sides collapse
 *  it to `apply_patch`. `NotebookEdit` is DELIBERATELY absent because neither the ADR
 *  nor broker.ts maps it; it therefore falls through to the unknown-tool path and
 *  fails closed to unknown_tool. */
const TOOL_ALIASES: ReadonlyMap<string, string> = new Map([
  ["Write", APPLY_PATCH],
  ["Edit", APPLY_PATCH],
  ["MultiEdit", APPLY_PATCH],
  ["Agent", SPAWN_AGENT],
]);

/** The five workflow signalling tools, bare (agents.ts / signals.ts). Root-only:
 *  the broker latches a signal only from the root origin, and agents.ts denies the
 *  whole `mcp__uzi` server to every subagent. */
const SIGNAL_TOOLS: ReadonlySet<string> = new Set([
  "submit_plan",
  "signal_done",
  "ask_user",
  "report_progress",
  "checkpoint",
]);

/** The delegation family (broker.ts `DELEGATE_TOOLS`): `spawn_agent` plus the
 *  code-mode child-lifecycle callbacks. All root-only and non-nested. */
const DELEGATE_TOOLS: ReadonlySet<string> = new Set([
  "spawn_agent",
  "collaborationspawn_agent",
  "collaborationwait_agent",
  "SubagentStart",
  "SubagentStop",
]);

// --- model + effort contract (adr/1106-codex-harness.md §Model and effort) -----

/** The initial product model picker (ADR :266). Deliberately narrower than the
 *  server catalog; an out-of-picker model is dropped with a diagnostic. */
const CONTRACT_MODELS: ReadonlySet<string> = new Set(["gpt-6-astra", "gpt-5.6-sol"]);

/** uzi's effort contract, mapped 1:1 to Codex `modelReasoningEffort` (ADR :267).
 *  Provider-only values (`ultra`, `persistent`) are NOT in the uzi contract. */
const CONTRACT_EFFORTS: ReadonlySet<string> = new Set(["low", "medium", "high", "xhigh", "max"]);

/** The plugin-qualifier the SDK skill enable-list uses (`uzi:<name>`). Stripped to
 *  the bare name the broker's `allowedSkills` is checked against. */
const SKILL_QUALIFIER_PREFIX = "uzi:";

/** The role name attributed to request-scoped (lead / advice) diagnostics. */
const LEAD_ROLE = "lead";
const ADVICE_ROLE = "advice";

// --- exported result shapes ---------------------------------------------------

export type CodexRenderDiagnosticKind =
  | "unknown_tool"
  | "unknown_skill"
  | "unknown_effort"
  | "unknown_model";

/** One stripped Claude tool/skill/effort/model reported with the role it was
 *  attached to. `name` is the ORIGINAL (pre-canonicalization) input name, passed
 *  through {@link sanitizeName} so a control/newline/ANSI/bidi sequence in the raw
 *  identifier cannot survive into whichever consumer renders the diagnostic. */
export interface CodexRenderDiagnostic {
  readonly kind: CodexRenderDiagnosticKind;
  readonly role: string;
  readonly name: string;
}

/** A resolved model + reasoning effort. `modelReasoningEffort` is the uzi effort
 *  mapped 1:1 to the Codex field name; both are absent when dropped/unset. */
export interface ResolvedCodexModel {
  readonly model?: string;
  readonly modelReasoningEffort?: HarnessEffort;
}

/** The rendered lead prompt pair. `systemPrompt`/`prompt` are threaded through
 *  verbatim (they are already built upstream by buildLeadSystemPrompt). */
export interface RenderedCodexPrompts {
  readonly systemPrompt: string;
  readonly prompt: string;
}

export interface RenderedCodexRun {
  /** The lead (root/main thread) resolved model + effort (the request's). */
  readonly lead: ResolvedCodexModel;
  /** Per-role resolved model + effort (agent model overrides the request's). */
  readonly perRoleModels: ReadonlyMap<string, ResolvedCodexModel>;
  /** The root grants: full root vocabulary, `isRoot: true`. */
  readonly leadGrants: RunGrants;
  /** Per-role grants (canonical `allowedTools`/`allowedSkills`, `isRoot: false`). */
  readonly perRoleGrants: ReadonlyMap<string, RunGrants>;
  /** The lead system + user prompt, verbatim. */
  readonly leadPrompt: RenderedCodexPrompts;
  /** Per-role rendered subagent prompt (body + the two shared appends). */
  readonly perRolePrompts: ReadonlyMap<string, string>;
  /** Every stripped tool/skill/effort/model, stably ordered. */
  readonly diagnostics: readonly CodexRenderDiagnostic[];
}

export interface RenderedCodexAdvice {
  readonly label: AdviceRequest["label"];
  readonly model?: string;
  readonly modelReasoningEffort?: HarnessEffort;
  readonly systemPrompt: string;
  readonly prompt: string;
  readonly output: AdviceRequest["output"];
  readonly diagnostics: readonly CodexRenderDiagnostic[];
}

// --- pure helpers -------------------------------------------------------------

type MutableDiagnostics = CodexRenderDiagnostic[];

/** Canonicalize one Claude tool name to its Codex callback name — MIRRORS
 *  broker.ts `canonicalName`: alias map first, then `mcp__uzi__<sig>` → bare
 *  signal. Any other name is returned unchanged (its recognition is decided
 *  separately). */
function canonicalizeToolName(name: string): string {
  const alias = TOOL_ALIASES.get(name);
  if (alias !== undefined) return alias;
  const prefix = `mcp__${SIGNAL_SERVER_NAME}__`;
  if (name.startsWith(prefix)) {
    const bare = name.slice(prefix.length);
    if (SIGNAL_TOOLS.has(bare)) return bare;
  }
  return name;
}

/** Whether a canonical name is a recognized Codex callback — MIRRORS broker.ts
 *  `capabilityOf` returning something other than `unknown`. An unrecognized name
 *  is what gets stripped with an `unknown_tool` diagnostic. */
function isRecognizedCanonical(canonical: string): boolean {
  if (canonical === BASH_TOOL || canonical === APPLY_PATCH || canonical === READ_TOOL) return true;
  if (SIGNAL_TOOLS.has(canonical)) return true;
  if (DELEGATE_TOOLS.has(canonical)) return true;
  return canonical === SKILL_TOOL || canonical.startsWith("mcp__");
}

/** The authority a SUBAGENT can never hold — the canonical mirror of the
 *  unconditional `disallowedTools` agents.ts puts on every subagent: no nested
 *  spawn/delegation, no workflow signals, no cross-run memory writes. */
function isSubagentForbidden(canonical: string): boolean {
  if (DELEGATE_TOOLS.has(canonical)) return true;
  if (SIGNAL_TOOLS.has(canonical)) return true;
  return canonical.startsWith(`mcp__${MEMORY_SERVER_NAME}__`);
}

/** The full callback vocabulary an inherit-all role receives. A subagent gets the
 *  base read/write/shell/skill set plus the findings tool; the root additionally
 *  gets delegation and the workflow signals. */
function fullVocabulary(isRoot: boolean): readonly string[] {
  const base = [BASH_TOOL, APPLY_PATCH, READ_TOOL, SKILL_TOOL, FINDINGS_TOOL_NAME];
  if (!isRoot) return base;
  return [...base, SPAWN_AGENT, ...SIGNAL_TOOLS];
}

/** A stable, insertion-ordered set built from a sorted copy, so any serialized
 *  form is byte-identical across calls. */
function sortedSet(values: Iterable<string>): ReadonlySet<string> {
  return new Set([...values].sort());
}

/**
 * Build one role's canonical `allowedTools`. Inherit ⇒ the full role-appropriate
 * vocabulary. Explicit allow ⇒ exactly the mapped names (unknowns stripped +
 * reported). Then: a non-empty allowlist / inherit gains the findings tool
 * (agents.ts parity); `deniedTools` are removed (canonicalized so
 * `Write`/`Agent`/`mcp__uzi__*` line up); a subagent loses all forbidden
 * authority. Skills are handled separately and NEVER added here.
 *
 * Plan-phase note: this does NOT strip write tools on the plan turn (unlike
 * agents.ts's planTurnSubagents). The Codex broker denies every file write while
 * `grants.phase === "plan"`, so the phase field on the grant is the mechanism, and
 * `apply_patch` correctly stays in the allowlist for both phases.
 */
function buildAllowedTools(
  tools: HarnessToolSet,
  deniedTools: readonly string[],
  isRoot: boolean,
  role: string,
  diagnostics: MutableDiagnostics,
): ReadonlySet<string> {
  const allowed = new Set<string>();
  if (tools.kind === "inherit") {
    for (const t of fullVocabulary(isRoot)) allowed.add(t);
  } else {
    for (const raw of tools.names) {
      const canonical = canonicalizeToolName(raw);
      if (!isRecognizedCanonical(canonical)) {
        diagnostics.push({ kind: "unknown_tool", role, name: sanitizeName(raw) });
        continue;
      }
      allowed.add(canonical);
    }
    // Mirror agents.ts: the findings tool is granted to any NON-EMPTY allowlist.
    // An explicit empty allow ([]) means exactly nothing, so it stays empty.
    if (tools.names.length > 0) allowed.add(FINDINGS_TOOL_NAME);
  }
  // Denied tools removed (canonicalized so a denial expressed as a Claude source
  // name — Write/Edit/Agent/mcp__uzi__submit_plan — removes the canonical entry),
  // and a subagent can never spawn, signal, or write cross-run memory even if its
  // allowlist named one (the broker also root-gates these at dispatch). Filtered in
  // one pass into a fresh set rather than deleting from `allowed` while iterating it.
  const denied = new Set(deniedTools.map(canonicalizeToolName));
  const kept: string[] = [];
  for (const t of allowed) {
    if (denied.has(t)) continue;
    if (!isRoot && isSubagentForbidden(t)) continue;
    kept.push(t);
  }
  return sortedSet(kept);
}

/**
 * Build one role's canonical `allowedSkills` — bare names, `uzi:` stripped. An
 * explicit `[]` disables all skills. A name that is not a valid bare skill name
 * (kebab-case, SKILL_NAME_RE) is stripped with an `unknown_skill` diagnostic.
 */
function buildAllowedSkills(
  skills: readonly string[],
  role: string,
  diagnostics: MutableDiagnostics,
): ReadonlySet<string> {
  const allowed = new Set<string>();
  for (const raw of skills) {
    const bare = raw.startsWith(SKILL_QUALIFIER_PREFIX)
      ? raw.slice(SKILL_QUALIFIER_PREFIX.length)
      : raw;
    if (!SKILL_NAME_RE.test(bare)) {
      diagnostics.push({ kind: "unknown_skill", role, name: sanitizeName(raw) });
      continue;
    }
    allowed.add(bare);
  }
  return sortedSet(allowed);
}

/** Validate a model against the uzi picker; an out-of-picker value is dropped
 *  (undefined) with an `unknown_model` diagnostic. Absent ⇒ absent, no diagnostic. */
function resolveModel(
  candidate: string | undefined,
  role: string,
  diagnostics: MutableDiagnostics,
): string | undefined {
  if (candidate === undefined) return undefined;
  if (CONTRACT_MODELS.has(candidate)) return candidate;
  diagnostics.push({ kind: "unknown_model", role, name: sanitizeName(candidate) });
  return undefined;
}

/** Validate an effort against the uzi contract (1:1 to Codex modelReasoningEffort);
 *  an out-of-contract value is dropped (undefined) with an `unknown_effort`
 *  diagnostic. Absent ⇒ absent, no diagnostic. */
function resolveEffort(
  candidate: string | undefined,
  role: string,
  diagnostics: MutableDiagnostics,
): HarnessEffort | undefined {
  if (candidate === undefined) return undefined;
  if (CONTRACT_EFFORTS.has(candidate)) return candidate as HarnessEffort;
  diagnostics.push({ kind: "unknown_effort", role, name: sanitizeName(candidate) });
  return undefined;
}

/** Per-role model resolution: a valid agent model overrides the request model; an
 *  unknown agent model is reported (under the role) and falls back to the request
 *  model. The request model was already validated once, under the lead. */
function resolveRoleModel(
  agentModel: string | undefined,
  requestModel: string | undefined,
  role: string,
  diagnostics: MutableDiagnostics,
): string | undefined {
  if (agentModel === undefined) return requestModel;
  if (CONTRACT_MODELS.has(agentModel)) return agentModel;
  diagnostics.push({ kind: "unknown_model", role, name: sanitizeName(agentModel) });
  return requestModel;
}

/** Render a subagent prompt exactly as agents.ts `toDefinition` composes it: the
 *  role body followed by the two shared appends, so a Codex subagent carries the
 *  same findings nudge and worker-runtime guidance a Claude subagent does. */
function renderSubagentPrompt(agent: HarnessAgent): string {
  return `${agent.prompt}\n\n${FINDINGS_NUDGE_APPEND}\n\n${WORKER_RUNTIME_APPEND}`;
}

/** The character ceiling on a sanitized diagnostic `name`, mirroring broker.ts's
 *  MAX_DIAGNOSTIC_CHARS. An over-long name is truncated with an ellipsis. */
const MAX_DIAGNOSTIC_NAME_CHARS = 120;

/** True for a code point that must not survive in a human-facing diagnostic `name`:
 *  a C0 control (0x00-0x1F, incl. TAB/LF/CR/ESC), DEL (0x7F), a C1 control
 *  (0x80-0x9F), or a Unicode bidi/format control (zero-width, line/paragraph
 *  separators, bidi embeddings/overrides/isolates, word joiner, BOM). MIRRORS
 *  broker.ts `isUnsafeIdentifierChar`. A code-point scan on purpose: oxlint's
 *  `no-control-regex` is denied, so a control-char regex is not an option. */
function isUnsafeNameChar(cp: number): boolean {
  if (cp <= 0x1f) return true; // C0 controls incl. TAB/LF/CR/ESC
  if (cp === 0x7f) return true; // DEL
  if (cp >= 0x80 && cp <= 0x9f) return true; // C1 controls
  if (cp >= 0x200b && cp <= 0x200f) return true; // ZWSP..RLM
  if (cp === 0x2028 || cp === 0x2029) return true; // line / paragraph separators
  if (cp >= 0x202a && cp <= 0x202e) return true; // bidi embeddings / overrides
  if (cp >= 0x2060 && cp <= 0x2064) return true; // word joiner..invisible separator
  if (cp >= 0x2066 && cp <= 0x206f) return true; // bidi isolates + deprecated format
  return cp === 0xfeff; // BOM / ZWNBSP
}

/** Sanitize an untrusted, user-authored diagnostic `name` (an unknown tool/skill/
 *  effort/model identifier) so a control/newline/ANSI/bidi sequence can never forge
 *  or terminal-rewrite a downstream log/report line, whichever future consumer renders
 *  the diagnostic. Replaces every control and bidi/format code point (see
 *  {@link isUnsafeNameChar}) with the visible replacement char U+FFFD, then bounds the
 *  length. MIRRORS broker.ts `safeId`; pure and allocation-bounded (one code-point pass
 *  + one slice). Only the human `name` is routed through this — never the machine-facing
 *  `kind`/`role` shape or the tool canonicalization. */
function sanitizeName(s: string): string {
  let out = "";
  for (const ch of s) {
    const cp = ch.codePointAt(0) ?? 0;
    out += isUnsafeNameChar(cp) ? "�" : ch;
  }
  return out.length > MAX_DIAGNOSTIC_NAME_CHARS
    ? `${out.slice(0, MAX_DIAGNOSTIC_NAME_CHARS)}…`
    : out;
}

/** Code-unit comparator (UTF-16), IDENTICAL to what `sortedSet`'s default `.sort()`
 *  applies. Locale-independent, so the ordering is byte-identical across environments. */
function byCodeUnit(a: string, b: string): number {
  return a < b ? -1 : a > b ? 1 : 0;
}

/** Stable diagnostic ordering (kind, role, name), so any serialized form of the
 *  result is byte-identical across calls regardless of push order. Sorts by UTF-16
 *  code unit (the SAME comparator `sortedSet` uses) rather than `localeCompare`, whose
 *  ICU/locale-dependence would make the order vary across environments for a non-ASCII
 *  `name` (an arbitrary unknown-tool/model string). */
function sortDiagnostics(diagnostics: MutableDiagnostics): readonly CodexRenderDiagnostic[] {
  return [...diagnostics].sort(
    (a, b) => byCodeUnit(a.kind, b.kind) || byCodeUnit(a.role, b.role) || byCodeUnit(a.name, b.name),
  );
}

// --- public builders ----------------------------------------------------------

/**
 * Render a Codex run turn: resolved model/effort, the lead + per-role grants, and
 * the lead + subagent prompt text. Pure and deterministic; never mutates `request`.
 */
export function renderCodexRun(request: RunTurnRequest): RenderedCodexRun {
  const diagnostics: MutableDiagnostics = [];

  // Resolve the request-scoped model + effort once, attributed to the lead (the
  // root thread that runs on them). Per-role fallbacks reuse these validated values.
  const requestModel = resolveModel(request.model, LEAD_ROLE, diagnostics);
  const requestEffort = resolveEffort(request.effort, LEAD_ROLE, diagnostics);
  const lead: ResolvedCodexModel = { model: requestModel, modelReasoningEffort: requestEffort };

  const leadGrants: RunGrants = {
    role: LEAD_ROLE,
    phase: request.phase,
    allowedTools: sortedSet(fullVocabulary(true)),
    allowedSkills: buildAllowedSkills(request.leadSkills, LEAD_ROLE, diagnostics),
    isRoot: true,
  };

  // Sort roles so the maps and any serialized form are deterministic.
  const roleEntries = Object.entries(request.agents).sort((a, b) => a[0].localeCompare(b[0]));

  const perRoleModels = new Map<string, ResolvedCodexModel>();
  const perRoleGrants = new Map<string, RunGrants>();
  const perRolePrompts = new Map<string, string>();

  for (const [role, agent] of roleEntries) {
    perRoleModels.set(role, {
      model: resolveRoleModel(agent.model, requestModel, role, diagnostics),
      // HarnessAgent carries no per-role effort in the neutral contract, so every
      // role runs on the request-scoped effort (already validated under the lead).
      modelReasoningEffort: requestEffort,
    });
    perRoleGrants.set(role, {
      role,
      phase: request.phase,
      allowedTools: buildAllowedTools(agent.tools, agent.deniedTools, false, role, diagnostics),
      allowedSkills: buildAllowedSkills(agent.skills, role, diagnostics),
      isRoot: false,
    });
    perRolePrompts.set(role, renderSubagentPrompt(agent));
  }

  return {
    lead,
    perRoleModels,
    leadGrants,
    perRoleGrants,
    leadPrompt: { systemPrompt: request.systemPrompt, prompt: request.prompt },
    perRolePrompts,
    diagnostics: sortDiagnostics(diagnostics),
  };
}

/**
 * Render a Codex advice pass. SEPARATE from renderCodexRun by design: advice is
 * model + prompt only (the shared advice ceiling — no run workspace, no agents,
 * no tools, no servers, no cwd). It resolves model/effort against the same uzi
 * contract and carries the output shape through verbatim.
 *
 * The runtime guard enforces the ceiling even against a cast/malformed input: an
 * advice request that carries any run-tool surface is a programming error, not a
 * silently-ignored field.
 */
export function renderCodexAdvice(request: AdviceRequest): RenderedCodexAdvice {
  const carrier = request as unknown as Record<string, unknown>;
  for (const forbidden of ["agents", "tools", "toolServers", "cwd"]) {
    if (forbidden in carrier) {
      throw new Error(`renderCodexAdvice: an advice request must not carry "${forbidden}" (advice ceiling)`);
    }
  }
  const diagnostics: MutableDiagnostics = [];
  return {
    label: request.label,
    model: resolveModel(request.model, ADVICE_ROLE, diagnostics),
    modelReasoningEffort: resolveEffort(request.effort, ADVICE_ROLE, diagnostics),
    systemPrompt: request.systemPrompt,
    prompt: request.prompt,
    output: request.output,
    diagnostics: sortDiagnostics(diagnostics),
  };
}

// PRD #1171 (M3, milestone 2) — the deterministic Codex run/advice BUILDERS: the pure
// composition layer that turns a provider-neutral request into the exact construction
// inputs milestone 3's CodexExecutor consumes, WITHOUT wiring a dead branch into
// runner.ts.
//
// `renderCodexRun`/`renderCodexAdvice` (render.ts) already own the Claude-tool-
// vocabulary → Codex mapping, unknown-tool/skill/effort/model stripping+reporting, the
// per-role model/effort fallback, sanitized skill delivery and the run-vs-advice split.
// These builders sit one layer above them: they JOIN the renderer's three per-role maps
// (grants + model/effort + prompt) into the single {@link DelegationRole} table the
// {@link CodexDelegationRunner} needs, derive the broker's `allowedRoles` set from the
// KNOWN roles, and surface the lead's broker grants + prompt + resolved model/effort as
// one flat object the harness/broker are constructed from. Run and advice stay SEPARATE
// entry points (advice is tool-less/isolated by construction — no roles, no grants).
//
// PURE + DETERMINISTIC: no I/O, no clock, no randomness. Two calls on the same input
// produce byte-identical structures (the renderer already sorts every map/set/diagnostic
// by UTF-16 code unit), and the builders never mutate their input or the render output.
// They add NO new authority: every grant/skill/model here is exactly what the renderer
// resolved, only reshaped for the consumer.

import { renderCodexAdvice, renderCodexRun } from "./render.js";
import type { CodexRenderDiagnostic, RenderedCodexAdvice, RenderedCodexRun } from "./render.js";
import type { DelegationRole } from "./delegation.js";
import type { RunGrants } from "./broker.js";
import type { AdviceRequest, HarnessEffort, RunTurnRequest } from "../harness.js";

/** The lead (root/main thread) construction inputs: its IMMUTABLE root grants
 *  (`isRoot:true`, full root vocabulary), the verbatim system+user prompt and the
 *  resolved model/effort. This is what the root broker + harness turn are built from. */
export interface CodexLeadPlan {
  readonly grants: RunGrants;
  readonly systemPrompt: string;
  readonly prompt: string;
  readonly model?: string;
  readonly modelReasoningEffort?: HarnessEffort;
}

/** Everything milestone 3 needs to construct a Codex RUN turn: the lead plan, the
 *  delegation role table (each role's isRoot:false grants + prompt + model/effort), the
 *  KNOWN-role set the broker admits delegation against, and the stripped-input
 *  diagnostics. */
export interface CodexRunPlan {
  readonly lead: CodexLeadPlan;
  /** The delegation targets, keyed by role — the {@link CodexDelegationRunner} input.
   *  Every entry is a subagent (`grants.isRoot === false`). */
  readonly roles: ReadonlyMap<string, DelegationRole>;
  /** The known delegation-target role names (== `roles` keys); the broker's
   *  `allowedRoles`. A delegation to any other role is denied. */
  readonly allowedRoles: ReadonlySet<string>;
  readonly diagnostics: readonly CodexRenderDiagnostic[];
}

/** The advice construction inputs — the SEPARATELY-GATED, tool-less/isolated ceiling.
 *  Deliberately carries NO roles, NO grants and NO tool surface: model + effort +
 *  prompt + output only, mirroring {@link RenderedCodexAdvice}. */
export interface CodexAdvicePlan {
  readonly label: AdviceRequest["label"];
  readonly systemPrompt: string;
  readonly prompt: string;
  readonly model?: string;
  readonly modelReasoningEffort?: HarnessEffort;
  readonly output: AdviceRequest["output"];
  readonly diagnostics: readonly CodexRenderDiagnostic[];
}

/**
 * Build the RUN construction inputs from a neutral {@link RunTurnRequest}. Pure; the
 * `render` seam defaults to {@link renderCodexRun} and is injectable for tests. The
 * per-role maps are joined in the renderer's already-sorted order, so the `roles` map
 * and the `allowedRoles` set are deterministic.
 */
export function buildCodexRunPlan(
  request: RunTurnRequest,
  render: (request: RunTurnRequest) => RenderedCodexRun = renderCodexRun,
): CodexRunPlan {
  const rendered = render(request);

  const roles = new Map<string, DelegationRole>();
  // Iterate the renderer's per-role grants map (deterministically ordered). Grants are
  // the authority; the model/effort and prompt are joined from the sibling maps. A role
  // present in grants is always present in the other two (the renderer builds all three
  // in lockstep), so a missing sibling is a programming error surfaced as an empty prompt
  // rather than a silent role drop — but it cannot happen for renderCodexRun output.
  for (const [role, grants] of rendered.perRoleGrants) {
    const model = rendered.perRoleModels.get(role);
    roles.set(role, {
      grants,
      systemPrompt: rendered.perRolePrompts.get(role) ?? "",
      model: model?.model,
      effort: model?.modelReasoningEffort,
    });
  }

  return {
    lead: {
      grants: rendered.leadGrants,
      systemPrompt: rendered.leadPrompt.systemPrompt,
      prompt: rendered.leadPrompt.prompt,
      model: rendered.lead.model,
      modelReasoningEffort: rendered.lead.modelReasoningEffort,
    },
    roles,
    allowedRoles: new Set(roles.keys()),
    diagnostics: rendered.diagnostics,
  };
}

/**
 * Build the ADVICE construction inputs from a neutral {@link AdviceRequest}. SEPARATE
 * from {@link buildCodexRunPlan} by design: advice has no roles, grants or tool surface.
 * `render` defaults to {@link renderCodexAdvice}, which ALSO enforces the advice ceiling
 * at runtime (it throws if the request carries agents/tools/toolServers/cwd), so this
 * builder can never accidentally admit a run-tool surface.
 */
export function buildCodexAdvicePlan(
  request: AdviceRequest,
  render: (request: AdviceRequest) => RenderedCodexAdvice = renderCodexAdvice,
): CodexAdvicePlan {
  const rendered = render(request);
  return {
    label: rendered.label,
    systemPrompt: rendered.systemPrompt,
    prompt: rendered.prompt,
    model: rendered.model,
    modelReasoningEffort: rendered.modelReasoningEffort,
    output: rendered.output,
    diagnostics: rendered.diagnostics,
  };
}

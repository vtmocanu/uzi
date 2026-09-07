import { describe, it } from "node:test";
import assert from "node:assert/strict";

import {
  renderCodexRun,
  renderCodexAdvice,
  type CodexRenderDiagnostic,
  type CodexRenderDiagnosticKind,
  type RenderedCodexAdvice,
  type RenderedCodexPrompts,
  type RenderedCodexRun,
  type ResolvedCodexModel,
} from "../src/codex/render.js";
import type {
  AdviceRequest,
  HarnessAgent,
  HarnessEffort,
  HarnessToolSet,
  RunTurnRequest,
} from "../src/harness.js";
import { reportIncidentalIssueToolName } from "../src/findings-tools.js";
import { FINDINGS_NUDGE_APPEND, WORKER_RUNTIME_APPEND } from "../src/prompt.js";

// PRD #1171 (M2) — the deterministic Codex prompt/tool/role renderer. Pure input →
// output, no I/O, no process, no clock. These tests pin the tool/skill/effort/model
// mapping tables, inherit-vs-allow, skill non-widening, the diagnostics surface,
// determinism, and Claude-path independence.

const FINDINGS_TOOL = reportIncidentalIssueToolName();

/** A HarnessAgent with sane defaults, overridable per test. */
function agent(overrides: Partial<HarnessAgent> = {}): HarnessAgent {
  return {
    description: "an agent",
    prompt: "do the thing",
    tools: { kind: "inherit" },
    deniedTools: [],
    toolServers: [],
    skills: [],
    ...overrides,
  };
}

/** A RunTurnRequest with a real AbortSignal (never structured-cloned). */
function runRequest(overrides: Partial<RunTurnRequest> = {}): RunTurnRequest {
  return {
    prompt: "user turn",
    systemPrompt: "lead system prompt",
    signal: new AbortController().signal,
    phase: "implement",
    agents: {},
    leadSkills: [],
    ...overrides,
  };
}

function adviceRequest(overrides: Partial<AdviceRequest> = {}): AdviceRequest {
  return {
    label: "judge",
    systemPrompt: "advice system",
    prompt: "advice prompt",
    output: { kind: "text" },
    signal: new AbortController().signal,
    timeoutMs: 1000,
    ...overrides,
  };
}

const allow = (names: readonly string[]): HarnessToolSet => ({ kind: "allow", names });

function grantFor(run: RenderedCodexRun, role: string) {
  const g = run.perRoleGrants.get(role);
  assert.ok(g, `expected grants for role "${role}"`);
  return g;
}

function toolsOf(run: RenderedCodexRun, role: string): Set<string> {
  return new Set(grantFor(run, role).allowedTools);
}

function hasDiagnostic(
  diagnostics: readonly CodexRenderDiagnostic[],
  kind: CodexRenderDiagnosticKind,
  role: string,
  name: string,
): boolean {
  return diagnostics.some((d) => d.kind === kind && d.role === role && d.name === name);
}

/** Deterministic serializer: Sets → sorted arrays, Maps → sorted [k,v] arrays,
 *  recursively, then JSON. Two byte-identical strings ⇒ byte-identical output. */
function serialize(value: unknown): string {
  return JSON.stringify(normalize(value));
}
function normalize(value: unknown): unknown {
  if (value instanceof Set) return [...value].map(normalize).sort(compareJson);
  if (value instanceof Map) {
    return [...value.entries()]
      .map(([k, v]) => [k, normalize(v)] as const)
      .sort((a, b) => compareJson(a[0], b[0]));
  }
  if (Array.isArray(value)) return value.map(normalize);
  if (value && typeof value === "object") {
    const out: Record<string, unknown> = {};
    for (const k of Object.keys(value as Record<string, unknown>).sort()) {
      out[k] = normalize((value as Record<string, unknown>)[k]);
    }
    return out;
  }
  return value;
}
function compareJson(a: unknown, b: unknown): number {
  return JSON.stringify(a).localeCompare(JSON.stringify(b));
}

describe("renderCodexRun — tool mapping", () => {
  it("maps Bash→Bash and Write/Edit/MultiEdit→apply_patch (collapsed)", () => {
    const run = renderCodexRun(
      runRequest({ agents: { coder: agent({ tools: allow(["Bash", "Write", "Edit", "MultiEdit", "Read"]) }) } }),
    );
    const tools = toolsOf(run, "coder");
    assert.equal(tools.has("Bash"), true);
    assert.equal(tools.has("apply_patch"), true);
    assert.equal(tools.has("Read"), true);
    // The three write aliases collapse to exactly one apply_patch, none passed through.
    assert.equal(tools.has("Write"), false);
    assert.equal(tools.has("Edit"), false);
    assert.equal(tools.has("MultiEdit"), false);
    // A non-empty allowlist gains the findings tool (agents.ts parity).
    assert.equal(tools.has(FINDINGS_TOOL), true);
    assert.equal(run.diagnostics.length, 0);
  });

  it("maps Agent→spawn_agent and mcp__uzi__<sig>→bare signal, observable on the root", () => {
    const run = renderCodexRun(runRequest());
    const lead = new Set(run.leadGrants.allowedTools);
    // Canonical delegate + signal names are BARE on the root vocabulary.
    assert.equal(lead.has("spawn_agent"), true);
    assert.equal(lead.has("Agent"), false);
    assert.equal(lead.has("submit_plan"), true);
    assert.equal(lead.has("mcp__uzi__submit_plan"), false);
  });

  it("recognizes Agent/signal aliases on a subagent (no unknown_tool) but strips them as root-only", () => {
    const run = renderCodexRun(
      runRequest({ agents: { helper: agent({ tools: allow(["Agent", "mcp__uzi__submit_plan", "Read"]) }) } }),
    );
    const tools = toolsOf(run, "helper");
    // Recognized (mapped), so NOT reported as unknown…
    assert.equal(hasDiagnostic(run.diagnostics, "unknown_tool", "helper", "Agent"), false);
    assert.equal(hasDiagnostic(run.diagnostics, "unknown_tool", "helper", "mcp__uzi__submit_plan"), false);
    // …but a subagent can never hold delegation or a workflow signal.
    assert.equal(tools.has("spawn_agent"), false);
    assert.equal(tools.has("submit_plan"), false);
    // The ordinary tool survives.
    assert.equal(tools.has("Read"), true);
  });

  it("strips an unknown tool and reports unknown_tool with the role named", () => {
    const run = renderCodexRun(
      runRequest({ agents: { reviewer: agent({ tools: allow(["Read", "Grep", "FrobnicateXYZ"]) }) } }),
    );
    const tools = toolsOf(run, "reviewer");
    assert.equal(tools.has("Read"), true);
    assert.equal(tools.has("Grep"), false);
    assert.equal(tools.has("FrobnicateXYZ"), false);
    assert.equal(hasDiagnostic(run.diagnostics, "unknown_tool", "reviewer", "Grep"), true);
    assert.equal(hasDiagnostic(run.diagnostics, "unknown_tool", "reviewer", "FrobnicateXYZ"), true);
  });
});

describe("renderCodexRun — inherit vs allow, denied, skills", () => {
  it("inherit and explicit allow produce distinct grants", () => {
    const run = renderCodexRun(
      runRequest({
        agents: {
          coder: agent({ tools: { kind: "inherit" } }),
          reviewer: agent({ tools: allow(["Read"]) }),
        },
      }),
    );
    const coder = toolsOf(run, "coder");
    const reviewer = toolsOf(run, "reviewer");
    // Inherit ⇒ the full (subagent) callback vocabulary.
    assert.deepEqual([...coder].sort(), ["Bash", "Read", "Skill", "apply_patch", FINDINGS_TOOL].sort());
    // Allow ⇒ exactly the mapped names (+ findings for a non-empty allowlist).
    assert.deepEqual([...reviewer].sort(), ["Read", FINDINGS_TOOL].sort());
    assert.notDeepEqual([...coder].sort(), [...reviewer].sort());
  });

  it("removes deniedTools, canonicalizing a Claude source name", () => {
    const run = renderCodexRun(
      runRequest({
        agents: {
          a: agent({ tools: { kind: "inherit" }, deniedTools: ["Bash"] }),
          // A denial expressed as the Claude source name removes the canonical entry.
          b: agent({ tools: { kind: "inherit" }, deniedTools: ["Write"] }),
        },
      }),
    );
    assert.equal(toolsOf(run, "a").has("Bash"), false);
    assert.equal(toolsOf(run, "b").has("apply_patch"), false);
  });

  it("explicit skills [] disables all; a listed skill is kept bare", () => {
    const run = renderCodexRun(
      runRequest({
        agents: {
          none: agent({ skills: [] }),
          some: agent({ skills: ["foo-skill", "uzi:bar-skill"] }),
        },
      }),
    );
    assert.equal(grantFor(run, "none").allowedSkills.size, 0);
    assert.deepEqual([...grantFor(run, "some").allowedSkills].sort(), ["bar-skill", "foo-skill"]);
  });

  it("reports unknown_skill for a malformed skill name and strips it", () => {
    const run = renderCodexRun(runRequest({ agents: { r: agent({ skills: ["Bad Name!"] }) } }));
    assert.equal(grantFor(run, "r").allowedSkills.size, 0);
    assert.equal(hasDiagnostic(run.diagnostics, "unknown_skill", "r", "Bad Name!"), true);
  });

  it("skill enablement does NOT widen the tool allowlist", () => {
    const run = renderCodexRun(
      runRequest({ agents: { r: agent({ tools: allow(["Read"]), skills: ["foo-skill", "bar-skill"] }) } }),
    );
    const tools = toolsOf(run, "r");
    assert.equal(tools.has("Skill"), false); // no Skill tool added by enabling skills
    assert.deepEqual([...tools].sort(), ["Read", FINDINGS_TOOL].sort());
    assert.deepEqual([...grantFor(run, "r").allowedSkills].sort(), ["bar-skill", "foo-skill"]);
  });

  it("an explicit empty allowlist grants exactly nothing (no findings)", () => {
    const run = renderCodexRun(runRequest({ agents: { r: agent({ tools: allow([]) }) } }));
    assert.equal(toolsOf(run, "r").size, 0);
  });
});

describe("renderCodexRun — model + effort contract", () => {
  it("maps an in-contract effort 1:1 to modelReasoningEffort", () => {
    for (const effort of ["low", "medium", "high", "xhigh", "max"] as const) {
      const run = renderCodexRun(runRequest({ effort }));
      assert.equal(run.lead.modelReasoningEffort, effort);
      assert.equal(run.diagnostics.length, 0);
    }
  });

  it("keeps an in-contract model and reports nothing", () => {
    for (const model of ["gpt-6-astra", "gpt-5.6-sol"]) {
      const run = renderCodexRun(runRequest({ model }));
      assert.equal(run.lead.model, model);
      assert.equal(run.diagnostics.length, 0);
    }
  });

  it("strips an out-of-contract effort (ultra) with a diagnostic and falls back to default", () => {
    const run = renderCodexRun(runRequest({ effort: "ultra" as unknown as HarnessEffort }));
    assert.equal(run.lead.modelReasoningEffort, undefined);
    assert.equal(hasDiagnostic(run.diagnostics, "unknown_effort", "lead", "ultra"), true);
  });

  it("strips an unknown model with a diagnostic and falls back to default", () => {
    const run = renderCodexRun(runRequest({ model: "gpt-9-imaginary" }));
    assert.equal(run.lead.model, undefined);
    assert.equal(hasDiagnostic(run.diagnostics, "unknown_model", "lead", "gpt-9-imaginary"), true);
  });
});

describe("renderCodexRun — per-role resolution and root-ness", () => {
  it("an agent model overrides the request model; absent inherits it; effort is request-scoped", () => {
    const run = renderCodexRun(
      runRequest({
        model: "gpt-6-astra",
        effort: "low",
        agents: {
          override: agent({ model: "gpt-5.6-sol" }),
          inherit: agent(),
        },
      }),
    );
    const override = run.perRoleModels.get("override");
    const inherit = run.perRoleModels.get("inherit");
    const expectOverride: ResolvedCodexModel = { model: "gpt-5.6-sol", modelReasoningEffort: "low" };
    const expectInherit: ResolvedCodexModel = { model: "gpt-6-astra", modelReasoningEffort: "low" };
    assert.deepEqual(override, expectOverride);
    assert.deepEqual(inherit, expectInherit);
  });

  it("an unknown agent model is reported under the role and falls back to the request model", () => {
    const run = renderCodexRun(
      runRequest({ model: "gpt-6-astra", agents: { bad: agent({ model: "not-a-model" }) } }),
    );
    assert.equal(run.perRoleModels.get("bad")?.model, "gpt-6-astra");
    assert.equal(hasDiagnostic(run.diagnostics, "unknown_model", "bad", "not-a-model"), true);
  });

  it("the lead is root and every subagent is not", () => {
    const run = renderCodexRun(runRequest({ agents: { a: agent(), b: agent() } }));
    assert.equal(run.leadGrants.isRoot, true);
    assert.equal(run.leadGrants.role, "lead");
    assert.equal(grantFor(run, "a").isRoot, false);
    assert.equal(grantFor(run, "b").isRoot, false);
  });

  it("threads the phase onto every grant (the broker's write gate)", () => {
    const run = renderCodexRun(runRequest({ phase: "plan", agents: { a: agent() } }));
    assert.equal(run.leadGrants.phase, "plan");
    assert.equal(grantFor(run, "a").phase, "plan");
    // apply_patch stays in the allowlist even in plan phase; the broker's phase gate
    // denies the actual write, not a stripped tool.
    assert.equal(grantFor(run, "a").allowedTools.has("apply_patch"), true);
  });
});

describe("renderCodexRun — prompts", () => {
  it("threads the lead system + user prompt verbatim", () => {
    const run = renderCodexRun(runRequest({ systemPrompt: "SYS", prompt: "USR" }));
    const lead: RenderedCodexPrompts = run.leadPrompt;
    assert.deepEqual(lead, { systemPrompt: "SYS", prompt: "USR" });
  });

  it("renders a subagent prompt as body + the two shared appends (agents.ts parity)", () => {
    const run = renderCodexRun(runRequest({ agents: { a: agent({ prompt: "BODY" }) } }));
    assert.equal(run.perRolePrompts.get("a"), `BODY\n\n${FINDINGS_NUDGE_APPEND}\n\n${WORKER_RUNTIME_APPEND}`);
  });
});

describe("renderCodexAdvice — advice ceiling", () => {
  it("carries model + effort + prompt + output only, with no grants/agents", () => {
    const rendered: RenderedCodexAdvice = renderCodexAdvice(
      adviceRequest({ model: "gpt-6-astra", effort: "high", output: { kind: "json", schema: {} } }),
    );
    assert.equal(rendered.label, "judge");
    assert.equal(rendered.model, "gpt-6-astra");
    assert.equal(rendered.modelReasoningEffort, "high");
    assert.equal(rendered.systemPrompt, "advice system");
    assert.equal(rendered.prompt, "advice prompt");
    assert.deepEqual(rendered.output, { kind: "json", schema: {} });
    // No run-tool surface leaks into advice.
    assert.equal("leadGrants" in (rendered as object), false);
    assert.equal("perRoleGrants" in (rendered as object), false);
    assert.equal("agents" in (rendered as object), false);
  });

  it("applies the same model/effort contract (out-of-contract stripped + diagnostic)", () => {
    const rendered = renderCodexAdvice(
      adviceRequest({ model: "nope", effort: "persistent" as unknown as HarnessEffort }),
    );
    assert.equal(rendered.model, undefined);
    assert.equal(rendered.modelReasoningEffort, undefined);
    assert.equal(hasDiagnostic(rendered.diagnostics, "unknown_model", "advice", "nope"), true);
    assert.equal(hasDiagnostic(rendered.diagnostics, "unknown_effort", "advice", "persistent"), true);
  });

  it("does NOT accept an agents/tools surface — type-level and runtime", () => {
    // Type-level: AdviceRequest carries no `agents` field. This const compiles only
    // while that holds (it becomes `never` and fails assignment otherwise).
    type AssertNoAgents = AdviceRequest extends { agents: unknown } ? never : true;
    const noAgents: AssertNoAgents = true;
    assert.equal(noAgents, true);

    // Runtime: a cast request that smuggles a run-tool surface is rejected.
    const smuggled = adviceRequest() as unknown as Record<string, unknown>;
    smuggled["agents"] = { coder: {} };
    assert.throws(() => renderCodexAdvice(smuggled as unknown as AdviceRequest), /advice ceiling/);

    const withTools = adviceRequest() as unknown as Record<string, unknown>;
    withTools["tools"] = { kind: "allow", names: ["Bash"] };
    assert.throws(() => renderCodexAdvice(withTools as unknown as AdviceRequest), /advice ceiling/);
  });
});

describe("renderCodexRun — determinism", () => {
  it("two calls on the same input deep-equal and serialize byte-identically", () => {
    const build = (): RunTurnRequest =>
      runRequest({
        model: "gpt-6-astra",
        effort: "medium",
        phase: "implement",
        leadSkills: ["lead-skill", "uzi:another"],
        agents: {
          // Deliberately out of sorted order to prove the renderer sorts.
          zeta: agent({ tools: allow(["Read", "Bash"]), skills: ["s1"], model: "gpt-5.6-sol" }),
          alpha: agent({ tools: { kind: "inherit" }, deniedTools: ["Write"], skills: [] }),
        },
      });
    const a = renderCodexRun(build());
    const b = renderCodexRun(build());
    assert.deepEqual(a, b);
    assert.equal(serialize(a), serialize(b));
    // Map key order is stable (sorted).
    assert.deepEqual([...a.perRoleGrants.keys()], ["alpha", "zeta"]);
    assert.deepEqual([...a.perRoleModels.keys()], ["alpha", "zeta"]);
    assert.deepEqual([...a.perRolePrompts.keys()], ["alpha", "zeta"]);
  });
});

describe("renderCodexRun — Claude-path independence (guard)", () => {
  it("does not mutate the input request or shared imported constants", () => {
    const nudgeBefore = FINDINGS_NUDGE_APPEND;
    const runtimeBefore = WORKER_RUNTIME_APPEND;

    const req = runRequest({
      leadSkills: ["one", "two"],
      agents: {
        coder: agent({ tools: allow(["Bash", "Write"]), deniedTools: ["Read"], skills: ["s1"] }),
      },
    });
    // Snapshot the mutable sub-structures (the AbortSignal is not JSON-cloneable).
    const agentsBefore = JSON.stringify(req.agents);
    const leadSkillsBefore = JSON.stringify(req.leadSkills);

    const run = renderCodexRun(req);

    // The input is untouched — the renderer builds fresh sets/maps, never aliasing input.
    assert.equal(JSON.stringify(req.agents), agentsBefore);
    assert.equal(JSON.stringify(req.leadSkills), leadSkillsBefore);
    // The shared prompt constants are unchanged (never imported to be mutated).
    assert.equal(FINDINGS_NUDGE_APPEND, nudgeBefore);
    assert.equal(WORKER_RUNTIME_APPEND, runtimeBefore);

    // Mutating the OUTPUT does not reach back into the input.
    (run.leadGrants.allowedTools as Set<string>).add("INJECTED");
    assert.equal(JSON.stringify(req.agents), agentsBefore);
  });
});

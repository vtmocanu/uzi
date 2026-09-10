import { describe, it } from "node:test";
import assert from "node:assert/strict";

import { buildCodexAdvicePlan, buildCodexRunPlan } from "../src/codex/run-builder.js";
import type { AdviceRequest, HarnessAgent, HarnessToolSet, RunTurnRequest } from "../src/harness.js";
import { reportIncidentalIssueToolName } from "../src/findings-tools.js";

// PRD #1171 (M3, milestone 2) — the deterministic run/advice BUILDERS that reshape the
// renderer output into m3's construction inputs. Pure input → output; these pin the
// lead/role join, the allowedRoles derivation, the run-vs-advice split and determinism.

const FINDINGS_TOOL = reportIncidentalIssueToolName();
const allow = (names: readonly string[]): HarnessToolSet => ({ kind: "allow", names });

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

/** Deterministic serializer (Sets → sorted, Maps → sorted), mirroring codex-render.test. */
function serialize(value: unknown): string {
  return JSON.stringify(normalize(value));
}
function normalize(value: unknown): unknown {
  if (value instanceof Set) return [...value].map(normalize).sort(cmp);
  if (value instanceof Map) return [...value.entries()].map(([k, v]) => [k, normalize(v)] as const).sort((a, b) => cmp(a[0], b[0]));
  if (Array.isArray(value)) return value.map(normalize);
  if (value && typeof value === "object") {
    const out: Record<string, unknown> = {};
    for (const k of Object.keys(value as Record<string, unknown>).sort()) out[k] = normalize((value as Record<string, unknown>)[k]);
    return out;
  }
  return value;
}
function cmp(a: unknown, b: unknown): number {
  return JSON.stringify(a).localeCompare(JSON.stringify(b));
}

describe("buildCodexRunPlan", () => {
  it("exposes the lead as a root grant with the verbatim prompt + resolved model/effort", () => {
    const plan = buildCodexRunPlan(
      runRequest({ model: "gpt-6-astra", effort: "high", leadSkills: ["uzi:issue-triage"] }),
    );
    assert.equal(plan.lead.grants.isRoot, true);
    assert.equal(plan.lead.grants.role, "lead");
    assert.equal(plan.lead.systemPrompt, "lead system prompt");
    assert.equal(plan.lead.prompt, "user turn");
    assert.equal(plan.lead.model, "gpt-6-astra");
    assert.equal(plan.lead.modelReasoningEffort, "high");
    // The lead's skills survived render sanitization (uzi: stripped).
    assert.equal(plan.lead.grants.allowedSkills.has("issue-triage"), true);
  });

  it("joins each role's grants + prompt + model/effort into the delegation table", () => {
    const plan = buildCodexRunPlan(
      runRequest({
        model: "gpt-6-astra",
        effort: "medium",
        agents: {
          coder: agent({ tools: allow(["Bash", "Write"]), deniedTools: ["Read"], skills: ["s1"], model: "gpt-5.6-sol" }),
        },
      }),
    );
    const coder = plan.roles.get("coder");
    assert.ok(coder);
    // Subagent grant (never root), with the Claude-parity tool set the renderer built.
    assert.equal(coder.grants.isRoot, false);
    assert.equal(coder.grants.role, "coder");
    assert.equal(coder.grants.allowedTools.has("apply_patch"), true);
    assert.equal(coder.grants.allowedTools.has(FINDINGS_TOOL), true);
    assert.equal(coder.grants.allowedTools.has("Read"), false); // denied
    assert.equal(coder.grants.allowedSkills.has("s1"), true);
    assert.equal(coder.grants.allowedTools.has("s1"), false); // a skill never widens tools
    // The subagent prompt carries the shared appends (rendered, not the bare body).
    assert.match(coder.systemPrompt, /do the thing/);
    // Per-role model override + request-scoped effort fallback.
    assert.equal(coder.model, "gpt-5.6-sol");
    assert.equal(coder.effort, "medium");
  });

  it("derives allowedRoles as exactly the known role names", () => {
    const plan = buildCodexRunPlan(
      runRequest({ agents: { coder: agent(), reviewer: agent(), zeta: agent() } }),
    );
    assert.deepEqual([...plan.allowedRoles].sort(), ["coder", "reviewer", "zeta"]);
    assert.deepEqual([...plan.roles.keys()].sort(), ["coder", "reviewer", "zeta"]);
    // allowedRoles and the role table agree exactly.
    for (const r of plan.allowedRoles) assert.ok(plan.roles.has(r));
  });

  it("surfaces the renderer's stripped-input diagnostics", () => {
    const plan = buildCodexRunPlan(
      runRequest({ agents: { coder: agent({ tools: allow(["Bash", "TotallyFakeTool"]) }) } }),
    );
    assert.ok(plan.diagnostics.some((d) => d.kind === "unknown_tool" && d.role === "coder" && d.name === "TotallyFakeTool"));
  });

  it("is deterministic (two builds serialize byte-identically)", () => {
    const build = (): RunTurnRequest =>
      runRequest({
        model: "gpt-6-astra",
        effort: "medium",
        agents: { zeta: agent({ tools: allow(["Read", "Bash"]) }), alpha: agent({ deniedTools: ["Write"] }) },
      });
    const a = buildCodexRunPlan(build());
    const b = buildCodexRunPlan(build());
    assert.equal(serialize(a), serialize(b));
    // Role order is the renderer's sorted order.
    assert.deepEqual([...a.roles.keys()], ["alpha", "zeta"]);
  });
});

describe("buildCodexAdvicePlan", () => {
  it("carries model/effort/prompt/output through and has NO role/tool surface", () => {
    const plan = buildCodexAdvicePlan(adviceRequest({ model: "gpt-6-astra", effort: "low" }));
    assert.equal(plan.label, "judge");
    assert.equal(plan.systemPrompt, "advice system");
    assert.equal(plan.prompt, "advice prompt");
    assert.equal(plan.model, "gpt-6-astra");
    assert.equal(plan.modelReasoningEffort, "low");
    assert.deepEqual(plan.output, { kind: "text" });
    // The advice plan shape carries no roles/grants (tool-less ceiling by construction).
    assert.equal("roles" in plan, false);
    assert.equal("allowedRoles" in plan, false);
  });

  it("reports an unknown advice model as a diagnostic and drops it", () => {
    const plan = buildCodexAdvicePlan(adviceRequest({ model: "no-such-model" }));
    assert.equal(plan.model, undefined);
    assert.ok(plan.diagnostics.some((d) => d.kind === "unknown_model" && d.role === "advice"));
  });

  it("enforces the advice ceiling (a run-tool surface throws)", () => {
    const bad = adviceRequest() as unknown as Record<string, unknown>;
    bad.agents = { coder: {} };
    assert.throws(() => buildCodexAdvicePlan(bad as unknown as AdviceRequest), /advice ceiling/);
  });
});

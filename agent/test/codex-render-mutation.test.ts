import { describe, it } from "node:test";
import assert from "node:assert/strict";

import { renderCodexRun, type RenderedCodexRun } from "../src/codex/render.js";
import type { CodexRenderDiagnostic } from "../src/codex/render.js";
import type { RunGrants } from "../src/codex/broker.js";
import type { HarnessAgent, HarnessToolSet, RunTurnRequest } from "../src/harness.js";
import { reportIncidentalIssueToolName } from "../src/findings-tools.js";

// PRD #1171 (M2/M5) — the CLAUDE-RENDERER BYTE-IDENTITY MUTATION test.
//
// The Codex renderer (render.ts) reproduces the Claude renderer's (agents.ts)
// grant SEMANTICS against the canonical Codex vocabulary; the run's determinism
// control (codex-render.test.ts) asserts two calls serialize byte-identically. That
// control is only meaningful if the byte-identity comparison actually DISCRIMINATES —
// if a renderer that mis-derived a grant would be CAUGHT. This test proves exactly
// that with fail-old / pass-fixed mutations:
//   - pass-fixed: the real renderer's output equals a captured golden across calls;
//   - fail-old: each mutation modelling a specific Claude-parity renderer BUG (findings
//     tool dropped, a subagent keeping spawn/signal authority, a denied tool not
//     removed, a skill leaking into the tool allowlist, the lead losing root, a stray
//     diagnostic, a wrong per-role model) produces output the byte-identity assertion
//     REJECTS (serialize(mutant) !== golden).
// It does NOT edit render.ts or agents.ts; a mutation is an OUTPUT transform modelling
// what a buggy renderer would emit, which is exactly what the byte-identity check must
// catch. A NON-discriminating change (reordering a set) must NOT be flagged — proving
// the assertion keys on CONTENT, not incidental order.

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

/** The SAME normalize+JSON serializer the run's determinism control uses: Sets → sorted
 *  arrays, Maps → sorted [k,v] arrays, recursively, then JSON. Two byte-identical
 *  structures ⇒ identical output regardless of set/map insertion order. This IS the
 *  byte-identity assertion under test. */
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

/** The fixed input: a subagent with an explicit non-empty allow-list, a denied tool and
 *  a skill — the case that exercises every Claude-parity property at once. */
function buildInput(): RunTurnRequest {
  return runRequest({
    model: "gpt-6-astra",
    effort: "medium",
    leadSkills: ["uzi:issue-triage"],
    agents: {
      coder: agent({ tools: allow(["Bash", "Write", "Read"]), deniedTools: ["Read"], skills: ["s1"], model: "gpt-5.6-sol" }),
    },
  });
}

interface MutableGrant {
  role: string;
  phase: string;
  allowedTools: Set<string>;
  allowedSkills: Set<string>;
  isRoot: boolean;
}
interface MutableRun {
  lead: { model?: string; modelReasoningEffort?: string };
  perRoleModels: Map<string, { model?: string; modelReasoningEffort?: string }>;
  leadGrants: MutableGrant;
  perRoleGrants: Map<string, MutableGrant>;
  leadPrompt: { systemPrompt: string; prompt: string };
  perRolePrompts: Map<string, string>;
  diagnostics: CodexRenderDiagnostic[];
}

/** A deep clone reproducing the FULL RenderedCodexRun shape (every field, in order) so
 *  an UNMUTATED clone serializes byte-identically to the golden; a mutation then changes
 *  exactly one thing. Sets/Maps are rebuilt so a mutation never touches the golden. */
function cloneRun(run: RenderedCodexRun): MutableRun {
  const cloneGrant = (g: RunGrants): MutableGrant => ({
    role: g.role,
    phase: g.phase,
    allowedTools: new Set(g.allowedTools),
    allowedSkills: new Set(g.allowedSkills),
    isRoot: g.isRoot,
  });
  return {
    lead: { ...run.lead },
    perRoleModels: new Map([...run.perRoleModels].map(([k, v]) => [k, { ...v }])),
    leadGrants: cloneGrant(run.leadGrants),
    perRoleGrants: new Map([...run.perRoleGrants].map(([k, v]) => [k, cloneGrant(v)])),
    leadPrompt: { ...run.leadPrompt },
    perRolePrompts: new Map(run.perRolePrompts),
    diagnostics: [...run.diagnostics],
  };
}

describe("Claude-renderer byte identity — mutation controls", () => {
  const input = buildInput();
  const golden = serialize(renderCodexRun(input));

  it("PASS-FIXED: the real renderer matches the golden across calls (assertion holds)", () => {
    assert.equal(serialize(renderCodexRun(input)), golden);
  });

  it("the golden actually carries the Claude-parity invariants the mutations attack", () => {
    const run = renderCodexRun(input);
    const coder = run.perRoleGrants.get("coder");
    assert.ok(coder);
    assert.equal(coder.allowedTools.has(FINDINGS_TOOL), true, "findings tool on a non-empty allow-list");
    assert.equal(coder.allowedTools.has("spawn_agent"), false, "subagent never has spawn");
    assert.equal(coder.allowedTools.has("submit_plan"), false, "subagent never has signals");
    assert.equal(coder.allowedTools.has("Read"), false, "denied tool removed");
    assert.equal(coder.allowedTools.has("s1"), false, "skill not widened into tools");
    assert.equal(run.leadGrants.isRoot, true, "lead is root");
  });

  // Each mutation models a DISTINCT Claude-parity renderer bug; the byte-identity
  // assertion (serialize === golden) must reject every one of them.
  const mutations: [name: string, mutate: (run: ReturnType<typeof cloneRun>) => void][] = [
    ["findings-tool-dropped", (r) => { r.perRoleGrants.get("coder")!.allowedTools.delete(FINDINGS_TOOL); }],
    ["subagent-keeps-spawn", (r) => { r.perRoleGrants.get("coder")!.allowedTools.add("spawn_agent"); }],
    ["subagent-keeps-signal", (r) => { r.perRoleGrants.get("coder")!.allowedTools.add("submit_plan"); }],
    ["denied-tool-not-removed", (r) => { r.perRoleGrants.get("coder")!.allowedTools.add("Read"); }],
    ["skill-leaks-into-tools", (r) => { r.perRoleGrants.get("coder")!.allowedTools.add("s1"); }],
    ["skill-dropped", (r) => { r.perRoleGrants.get("coder")!.allowedSkills.delete("s1"); }],
    ["lead-loses-root", (r) => { r.leadGrants.isRoot = false; }],
    ["stray-diagnostic", (r) => { r.diagnostics.push({ kind: "unknown_tool", role: "coder", name: "phantom" }); }],
    ["wrong-per-role-model", (r) => { r.perRoleModels.get("coder")!.model = "gpt-6-astra"; }],
    ["prompt-tampered", (r) => { r.perRolePrompts.set("coder", "TAMPERED"); }],
  ];

  for (const [name, mutate] of mutations) {
    it(`FAIL-OLD: mutation "${name}" is caught by the byte-identity assertion`, () => {
      const mutant = cloneRun(renderCodexRun(input));
      mutate(mutant);
      assert.notEqual(
        serialize(mutant),
        golden,
        `the byte-identity assertion did NOT discriminate the "${name}" renderer bug`,
      );
    });
  }

  it("CONTROL: a non-discriminating reordering of a grant's tools is NOT flagged", () => {
    // Rebuild the coder's allowedTools set in a DIFFERENT insertion order but with the
    // SAME members: the serializer normalizes order, so byte identity must still hold.
    const reordered = cloneRun(renderCodexRun(input));
    const coder = reordered.perRoleGrants.get("coder")!;
    const members = [...coder.allowedTools].reverse();
    coder.allowedTools = new Set(members);
    assert.equal(serialize(reordered), golden, "reordering set members must not break byte identity");
  });
});

import { it } from "node:test";
import assert from "node:assert/strict";
import { buildSignalMcpServer, scanSignals } from "../src/signals.js";
import { RunTurnReducerImpl } from "../src/harness-reducer.js";
import { buildCodexDynamicTools } from "../src/codex/dynamic-tools.js";
import { renderCodexRun } from "../src/codex/render.js";
import { enrollment, proposal } from "./semantic-coverage-fixture.js";
import type { HarnessEvent, RunTurnRequest } from "../src/harness.js";

const block = (semantic_proposal?: unknown) => ({ type: "tool_use", name: "mcp__uzi__submit_plan",
  input: { plan_md: "plan", ...(semantic_proposal === undefined ? {} : { semantic_proposal }) } });
const frame = (content: unknown[], subagent = false) => ({ type: "assistant",
  ...(subagent ? { parent_tool_use_id: "child" } : {}), message: { content } });

it("M2 proposals are opt-in, root-only, strict and atomic with last submission clearing omission", () => {
  assert.equal(scanSignals(frame([block(proposal())])).semanticSubmission, undefined);
  assert.deepEqual(scanSignals(frame([block(proposal())]), { semanticPlanning: enrollment }).semanticSubmission, proposal());
  assert.deepEqual(scanSignals(frame([block(proposal())], true), { semanticPlanning: enrollment }), {});
  for (const bad of [null, {}, { criteria: [...proposal().criteria, { bad: true }] }, { ...proposal(), extra: true }]) {
    assert.deepEqual(scanSignals(frame([block(bad)]), { semanticPlanning: enrollment }).semanticSubmission, { invalid: true });
  }
  assert.equal(scanSignals(frame([block(proposal()), block()]), { semanticPlanning: enrollment }).semanticSubmission, undefined);
  assert.deepEqual(scanSignals(frame([block(null), block(proposal())]), { semanticPlanning: enrollment }).semanticSubmission, proposal());
});

it("M2 Claude tool schema exposes optional strict proposals only for injected context", () => {
  for (const semanticPlanning of [undefined, enrollment]) {
    const server = buildSignalMcpServer({ semanticPlanning });
    const tools = (server.instance as unknown as { _registeredTools: Record<string, {
      inputSchema: { shape: Record<string, { safeParse(v: unknown): { success: boolean } }> };
    }> })._registeredTools;
    const shape = tools.submit_plan!.inputSchema.shape;
    assert.equal(Object.hasOwn(shape, "semantic_proposal"), !!semanticPlanning);
    if (semanticPlanning) {
      assert.equal(shape.semantic_proposal!.safeParse(undefined).success, true);
      assert.equal(shape.semantic_proposal!.safeParse(proposal()).success, true);
      assert.equal(shape.semantic_proposal!.safeParse({ criteria: [] }).success, false);
    }
  }
});

it("M2 reducer replaces proposals across frames and ignores child signals", async () => {
  const reducer = new RunTurnReducerImpl({ request() {}, async get() { return undefined; } });
  reducer.beginTurn();
  const event = (signals: object, child = false): HarnessEvent => ({
    kind: "frame", origin: { kind: child ? "subagent" : "main" }, attribution: {}, items: [], signals,
  });
  await reducer.accept(event({ plan: "first", semanticSubmission: proposal() }));
  await reducer.accept(event({ plan: "child", semanticSubmission: { invalid: true } }, true));
  assert.deepEqual(reducer.finish({ kind: "exhausted" }).result.semanticSubmission, proposal());
  await reducer.accept(event({ plan: "second" }));
  assert.equal(reducer.finish({ kind: "exhausted" }).result.semanticSubmission, undefined);
  await reducer.accept(event({ plan: "third", semanticSubmission: { invalid: true } }));
  assert.deepEqual(reducer.finish({ kind: "exhausted" }).result.semanticSubmission, { invalid: true });
});

it("M2 Codex rendering grants semantic schema only to the enrolled root", () => {
  for (const semanticPlanning of [undefined, enrollment]) {
    const request: RunTurnRequest = { semanticPlanning, prompt: "plan", systemPrompt: "rules", phase: "plan",
      agents: {}, leadSkills: [], signal: new AbortController().signal };
    const rendered = renderCodexRun(request);
    const grants = rendered.leadGrants;
    for (const isRoot of [true, false]) {
      const tool = buildCodexDynamicTools({ ...grants, isRoot }).find(t => t.name === "submit_plan")!;
      const properties = tool.inputSchema.properties as Record<string, unknown>;
      assert.equal(Object.hasOwn(properties, "semantic_proposal"), !!semanticPlanning && isRoot);
    }
  }
});

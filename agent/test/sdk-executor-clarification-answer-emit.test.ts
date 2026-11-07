// Issue #3985: the PRODUCTION SdkExecutor ask path (askUserOrContinue: ctx.askUser, then the lead
// `answer` feed message) driven by a real RunRunner, whose real parkQuestion supplies ctx.askUser.
// A fake claude queryFn plans, the human revises, the revision turn asks a question, and the
// clarification settles through `clarification_settled`. The server's settlement answer is the only
// scripted part (api.stateAckWire). An `answer` message must be emitted only after the settlement is
// confirmed; a refusal or an ambiguous ACK emits none and the error propagates out of SdkExecutor.run.
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { afterEach, beforeEach, describe, it } from "node:test";
import type { SDKMessage } from "@anthropic-ai/claude-agent-sdk";
import { SdkExecutor, type SdkQueryFn } from "../src/sdk-executor.js";
import { nullLogger } from "./helpers.js";
import { api, client, fakeGitlab, installHarness, runner } from "./runner-harness.js";
import { freshClaim, newApi, row, send, until } from "./gate-revision-harness.js";

installHarness();

const FEATURE = "clarification_revision_settlement_v1";

let homeDir: string;
beforeEach(() => { homeDir = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-answer-emit-")); });
afterEach(() => { fs.rmSync(homeDir, { recursive: true, force: true }); });

const submitPlan = (plan: string): SDKMessage => ({
  type: "assistant", session_id: "sess-1",
  message: { content: [{ type: "tool_use", id: "t1", name: "mcp__uzi__submit_plan", input: { plan_md: plan } }] },
}) as unknown as SDKMessage;
const askUser = (): SDKMessage => ({
  type: "assistant", session_id: "sess-1",
  message: { content: [{ type: "tool_use", id: "t3", name: "mcp__uzi__ask_user", input: { questions: [{ question: "Which database?", header: "DB" }] } }] },
}) as unknown as SDKMessage;
const resultSuccess = (): SDKMessage => ({ type: "result", subtype: "success", is_error: false, num_turns: 1, session_id: "sess-1" }) as unknown as SDKMessage;

/** Plan v1, then (after the human revise) a revision turn that asks, then plan v2. */
function scriptedQuery(): { queryFn: SdkQueryFn; turns: () => number } {
  const scripts: SDKMessage[][] = [
    [submitPlan("# v1"), resultSuccess()],
    [askUser(), resultSuccess()],
    [submitPlan("# v2"), resultSuccess()],
  ];
  let i = 0;
  const queryFn: SdkQueryFn = (params) => {
    const script = scripts[Math.min(i, scripts.length - 1)]!;
    i++;
    return (async function* () {
      for await (const _ of params.prompt) void _;
      for (const m of script) yield m;
    })();
  };
  return { queryFn, turns: () => i };
}

type Ack = "applied" | "no_disposition" | "conflict" | "applied_false";

async function driveSdk(t: { after: (fn: () => Promise<void>) => void }, ack: Ack): Promise<{
  answers: number; error: unknown; settlements: number; runningAfterQuestion: number; finished: boolean; gates: number;
}> {
  newApi();
  client.protocolFeatures = [...client.protocolFeatures, FEATURE];
  api.stateAckWire = (body, run) => {
    if (body.status !== "clarification_settled") return undefined;
    switch (ack) {
      case "applied": return { status: 200, body: { run: { ...run, status: "awaiting_approval" }, disposition: "clarification_settled" } };
      case "no_disposition": return { status: 200, body: { run: { ...run, status: "awaiting_approval" } } };
      case "applied_false": return { status: 409, body: { run: { ...run, status: "awaiting_approval" }, disposition: "clarification_settled" } };
      case "conflict": return { status: 409, body: { run: { ...run, status: "running" } } };
    }
  };
  const claim = freshClaim(1, { plan_cross_check_required: true, auto_approve: false });
  const id = claim.run_id;
  let gates = 0;
  api.onState(id, body => {
    if (body.status === "awaiting_approval") {
      if (++gates === 1) send(id, row("revise_plan", "clarify before revising"));
      else send(id, row("cancel"));
    }
    if (body.status === "awaiting_input" && body.open_question_id)
      send(id, row("answer", JSON.stringify({ question_id: body.open_question_id, answers: ["postgres"] })));
  });
  const { queryFn } = scriptedQuery();
  const executor = new SdkExecutor(nullLogger(), homeDir, { queryFn });
  let error: unknown;
  let finished = false;
  const done = runner(executor, fakeGitlab().gitlab).execute(claim).then(
    () => undefined, (e: unknown) => { error = e; },
  ).finally(() => { finished = true; });
  t.after(async () => { send(id, row("cancel")); await done; });
  await until(() => finished, 20_000);
  await new Promise(r => setTimeout(r, 100));
  const bodies = api.states.filter(s => s.runId === id).map(s => s.body);
  const states = bodies.map(b => b.status);
  const parked = states.indexOf("awaiting_input");
  // The runner turns an executor failure into a terminal report instead of rejecting execute().
  const terminal = bodies.at(-1);
  return {
    answers: api.messages(id).filter(m => m.kind === "answer").length,
    error: error ?? JSON.stringify({ status: terminal?.status, reason: (terminal as { failure_reason?: unknown } | undefined)?.failure_reason, states }),
    settlements: states.filter(s => s === "clarification_settled").length,
    runningAfterQuestion: parked < 0 ? 0 : states.slice(parked).filter(s => s === "running").length,
    finished,
    gates,
  };
}

describe("#3985 SdkExecutor emits the lead answer only after a confirmed clarification settlement", () => {
  it("a confirmed settlement emits exactly one answer event and the run continues to the next plan", async t => {
    const o = await driveSdk(t, "applied");
    assert.equal(o.settlements, 1);
    assert.equal(o.answers, 1, "exactly one answer feed message");
    assert.equal(o.runningAfterQuestion, 0, "an answer to a revise clarification is not plan approval");
    assert.equal(o.gates, 2, "the loop went on to present plan v2 after the answer");
  });

  for (const ack of ["conflict", "applied_false"] as const) {
    it(`a ${ack} ACK emits zero answer events and the settlement error propagates`, async t => {
      const o = await driveSdk(t, ack);
      assert.equal(o.settlements, 1);
      assert.equal(o.answers, 0);
      assert.match(String(o.error), /clarification revision settlement was not confirmed/);
      assert.equal(o.runningAfterQuestion, 0);
    });
  }

  it("an ambiguous ACK (200 without the clarification_settled disposition) emits zero answer events and the error propagates", async t => {
    const o = await driveSdk(t, "no_disposition");
    assert.equal(o.settlements, 1);
    assert.equal(o.answers, 0);
    assert.match(String(o.error), /clarification revision settlement was not confirmed/);
    assert.equal(o.runningAfterQuestion, 0);
  });
});

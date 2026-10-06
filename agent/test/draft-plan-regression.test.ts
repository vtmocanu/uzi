// This primary regression imports BASE-existing entrypoints only. Copy this file and
// draft-plan-script.ts onto BASE to prove the missing capture through ordinary runner close.
import test from "node:test";
import assert from "node:assert/strict";
import { RunTurnReducerImpl } from "../src/harness-reducer.js";
import type { Executor } from "../src/executor.js";
import { SdkExecutor, type SdkQueryFn } from "../src/sdk-executor.js";
import { nullLogger } from "./helpers.js";
import { api, assistant, fakeGitlab, gitlabClaim, homeDir, input, installHarness, resultOk, runner, simulateCommittedWork } from "./runner-harness.js";
import { capture, claudeScript, codexScript, request, toolNote } from "./draft-plan-script.js";

installHarness();
const draft = "# Draft\nImplement the bounded feature.";
const refusal = "provider error: 400 policy refusal before submit (#1831)";

test("submitted plan is still the plan record, with earlier draft separately labelled", async () => {
  let turn = 0;
  const queryFn: SdkQueryFn = (params) => (async function* () {
    for await (const _ of params.prompt) { /* drain existing input stream */ }
    if (turn++ === 0) {
      yield capture(draft) as ReturnType<typeof assistant>;
      yield assistant([{ type: "tool_use", id: "submitted", name: "mcp__uzi__submit_plan",
        input: { plan_md: "# Approved candidate" } }]);
    } else {
      yield assistant([{ type: "tool_use", id: "done", name: "mcp__uzi__signal_done", input: {} }]);
    }
    yield resultOk();
  })();
  simulateCommittedWork();
  const claim = gitlabClaim(23230);
  api.setInputs(claim.run_id, [input("approve_plan")]);
  await runner(new SdkExecutor(nullLogger(), homeDir, { queryFn }), fakeGitlab().gitlab).execute(claim);
  const gate = api.states.find((s) => s.body.status === "awaiting_approval")?.body;
  assert.equal(gate?.plan_md, "# Approved candidate");
  const messages = api.messages(claim.run_id);
  const captured = messages.find((m) => m.payload.event === "draft_plan_capture");
  assert.equal(captured?.payload.plan_md, draft);
  const plans = messages.filter((m) => m.kind === "plan");
  assert.equal(plans.length, 1);
  assert.ok(JSON.stringify(plans[0]?.payload).includes("# Approved candidate"));
  assert.ok(captured!.seq < plans[0]!.seq);
});

for (const adapter of ["claude", "codex"] as const) {
  for (const end of ["provider-result", "iterator-throw"] as const) {
    test(`${adapter}: draft survives ${end} before submit through ordinary runner close`, async () => {
      const error = new Error(refusal);
      const validators = ["reviewer: approved", "tester: approved"];
      const frames = [
        capture(draft),
        { type: "user", message: { content: [{ type: "tool_result", tool_use_id: "draft-call", content: "Draft capture requested; this is not plan submission or approval." }] } },
        ...validators.map((text) => ({ type: "assistant", subagent_type: text.split(":")[0],
          parent_tool_use_id: "validator", message: { content: [{ type: "text", text }] } })),
      ];
      const script = adapter === "claude"
        ? { harness: claudeScript(end === "provider-result" ? [...frames, {
          type: "result", subtype: "error_during_execution", is_error: true,
          errors: [refusal], result: "400 policy refusal before submit (#1831)", terminal_reason: "api_error", api_error_status: 400,
        }] : frames, end === "iterator-throw" ? error : undefined) }
        : codexScript([
          toolNote("save_draft_plan", { plan_md: draft }),
          ...["reviewer", "tester"].map((role) => toolNote("spawn_agent",
            { subagent_type: role, description: `${role} validation`, prompt: "Validate the draft" }, `validate-${role}`)),
          ...(end === "provider-result" ? [{
            kind: "turn_completed" as const, method: "turn/completed", threadId: "th-1", turnId: "tn-1",
            status: "failed", params: { threadId: "th-1", turn: { id: "tn-1", status: "failed",
              error: { message: refusal, codexErrorInfo: "other" } } },
          }] : []),
        ], end === "iterator-throw" ? error : undefined);
      const executor: Executor = {
        async run(ctx) {
          const reducer = new RunTurnReducerImpl({ request() {}, get: async () => undefined });
          reducer.beginTurn();
          for await (const event of script.harness.startTurn(request).events) {
            for (const message of (await reducer.accept(event)).messages) ctx.emit(message);
            if (event.kind === "turn_finished" && event.terminal.outcome === "failed") {
              throw event.terminal.failure?.materialize().original ?? error;
            }
          }
          throw error;
        },
      };
      const claim = gitlabClaim(2323);
      await runner(executor, fakeGitlab().gitlab).execute(claim);
      const persisted = api.messages(claim.run_id);
      const captures = persisted.filter((m) => m.payload.event === "draft_plan_capture");
      assert.equal(captures.length, 1, "ordinary close must persist the captured draft");
      assert.deepEqual(captures[0]?.payload, {
        event: "draft_plan_capture", version: 1, label: "draft, unapproved, possibly incomplete",
        plan_md: draft, truncated: false,
      });
      assert.equal(captures[0]?.kind, "status");
      assert.equal(captures[0]?.agent, "lead");
      const reports = persisted.filter((m) => validators.includes(String(m.payload.text)));
      assert.deepEqual(reports.map((m) => m.payload.text), validators);
      assert.deepEqual(reports.map((m) => m.agent), ["reviewer", "tester"]);
      for (const report of reports) {
        assert.ok(report.agent_instance, "validator report has invocation attribution");
        if (adapter === "codex") {
          const activity = persisted.filter((m) => m.agent_instance === report.agent_instance && m.kind === "tool_use");
          assert.equal(activity.length, 1, "validator performed projected tool activity");
          assert.ok(captures[0]!.seq < activity[0]!.seq && activity[0]!.seq < report.seq);
        }
      }
      assert.ok(captures[0]!.seq < reports[0]!.seq && reports[0]!.seq < reports[1]!.seq);
      assert.equal(persisted.some((m) => JSON.stringify(m.payload).includes("draft-call")), false);
      const failed = api.states.filter((s) => s.body.status === "failed").at(-1)?.body;
      assert.ok(failed?.failure_reason);
      if (end === "iterator-throw" || adapter === "claude") assert.equal(failed.failure_reason, refusal);
      else assert.equal(failed.failure_reason, "codex turn failed: failed (other)");
      assert.equal(api.states.some((s) => s.body.status === "awaiting_approval"), false);
    });
  }
}

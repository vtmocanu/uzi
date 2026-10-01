// Issue #1514: a non-interlocked milestone issue run whose lead finishes the operator's last
// permitted milestone and calls signal_done in the SAME turn. The loop-top scope gate never ran
// again, so the run used to finalize as a full delivery (`Closes #N`, no scope_capped on the
// terminal report). Driven through the REAL RunRunner + SdkExecutor against the FakeApi.
import { describe, it } from "node:test";
import assert from "node:assert/strict";
import type { SDKMessage } from "@anthropic-ai/claude-agent-sdk";
import { SdkExecutor, type SdkQueryFn } from "../src/sdk-executor.js";
import { nullLogger } from "./helpers.js";
import {
  api,
  assistant,
  fakeGitlab,
  gitlabClaim,
  homeDir,
  input,
  installHarness,
  resultOk,
  runnerWith,
  simulateCommittedWork,
} from "./runner-harness.js";

installHarness();

const IID = 7;
const MILESTONES = [1, 2, 3, 4, 5, 6, 7].map((n) => ({ id: `m${n}`, title: `milestone ${n}` }));

/** A planning turn freezing the 7 milestones, then ONE implement turn that declares only m3
 *  (the milestone it just finished; the armed ACK carries m1, m2) and signals done. */
function planThenDoneDeclaringThree(): SdkQueryFn {
  const scripts: SDKMessage[][] = [
    [
      assistant([
        { type: "tool_use", id: "p", name: "mcp__uzi__submit_plan", input: { plan_md: "# PLAN", milestones: MILESTONES } },
      ]),
      resultOk(),
    ],
    [
      assistant([
        { type: "text", text: "m3 finished" },
        { type: "tool_use", id: "d", name: "mcp__uzi__signal_done", input: { milestones_completed: ["m3"] } },
      ]),
      resultOk(),
    ],
  ];
  let i = 0;
  return (params) => {
    const script = scripts[Math.min(i, scripts.length - 1)]!;
    i++;
    return (async function* (): AsyncGenerator<SDKMessage> {
      for await (const _ of params.prompt) {
        /* drain */
      }
      for (const m of script) yield m;
    })();
  };
}

describe("Issue #1514: scope-capped signal_done delivers a non-closing partial", () => {
  it("reports scope_capped and renders a [partial], non-closing MR", async () => {
    simulateCommittedWork();
    const claim = gitlabClaim(IID);
    api.setInputs(claim.run_id, [input("approve_plan")]);
    api.setStateAckScope(claim.run_id, { scope_ceiling: 3, milestones_completed: ["m1", "m2"] });
    const forge = fakeGitlab();
    const r = runnerWith(
      () => ({ executor: new SdkExecutor(nullLogger(), homeDir, { queryFn: planThenDoneDeclaringThree() }) }),
      forge.gitlab,
    );
    await r.execute(claim);

    const terminal = api.states.filter((s) => s.runId === claim.run_id && s.body.status === "completed").at(-1)?.body;
    assert.ok(terminal, "the run reported completed");
    assert.equal(terminal!.scope_capped, true, "the completed report carries scope_capped");

    const create = forge.calls.find((c) => c.method === "POST");
    assert.ok(create, "an MR was created");
    const body = JSON.parse(create!.body ?? "{}") as { title?: string; description?: string };
    assert.ok(!new RegExp(`Closes #${IID}\\b`).test(body.description ?? ""), "the MR must not close the issue");
    assert.match(body.description ?? "", new RegExp(`Implements part of #${IID}`));
    assert.ok((body.title ?? "").startsWith("[partial] "), `title was ${body.title}`);
  });
});

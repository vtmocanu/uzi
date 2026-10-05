// Issue #1514: a non-interlocked milestone issue run whose lead finishes the operator's last
// permitted milestone and calls signal_done in the SAME turn. The loop-top scope gate never ran
// again, so the run used to finalize as a full delivery (`Closes #N`, no scope_capped on the
// terminal report). Driven through the REAL RunRunner + SdkExecutor against the FakeApi.
import { describe, it } from "node:test";
import assert from "node:assert/strict";
import path from "node:path";
import { Outbox } from "../src/outbox.js";
import type { StateRequest } from "../src/protocol.js";
import type { Executor } from "../src/executor.js";
import type { SDKMessage } from "@anthropic-ai/claude-agent-sdk";
import { SdkExecutor, type SdkQueryFn } from "../src/sdk-executor.js";
import { nullLogger } from "./helpers.js";
import {
  api,
  client,
  git,
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


/** Delivers a genuinely empty capped slice through the public executor seam. */
const emptyCappedExecutor = (): Executor => ({
  run: async (ctx) => ({ branch: ctx.branch, scopeCapped: { completedCount: 0, total: 3 }, summary: "Stopped before m1; nothing to land." }),
});

async function cappedOutbox(): Promise<Outbox> {
  const outbox = new Outbox({
    root: path.join(homeDir, "outbox"), log: nullLogger(),
    runMaxBytes: 64 * 1024 * 1024, maxBytes: 512 * 1024 * 1024, retentionMs: 86_400_000,
  });
  await outbox.init();
  return outbox;
}

describe("interlocked zero capped slice", () => {
  it("binds the committed base head and latest ACK revision before durable report-only completion, without push or MR", async () => {
    const claim = gitlabClaim(2080, {
      claim_generation: 3,
      config: { completion_contract_version: 1, contract_revision: 1 },
    });
    api.setStateAckCompletionRevision(claim.run_id, 7);
    api.setCompletionPermitResponse(true);
    api.failStateWhen(claim.run_id, (b) => b.status === "completed", { httpStatus: 409, runStatus: "running" });
    await client.register("cap", undefined, 1, undefined, ["credential_switch_v1"]);
    const outbox = await cappedOutbox();
    const forge = fakeGitlab();
    let pushes = 0;
    git.pushBranch = async () => { pushes++; throw new Error("empty slice must not push"); };
    const branchTip = git.branchTip.bind(git);
    let baseHead: string | null = null;
    git.branchTip = async (clone, branch) => { baseHead = await branchTip(clone, branch); return baseHead; };
    let journalAtSend: unknown;
    let terminal: StateRequest | undefined;
    const reportState = client.reportState.bind(client);
    client.reportState = async (id, body, ...rest) => {
      if (body.status === "completed") {
        terminal = body;
        journalAtSend = await outbox.readTerminalJournal(id, 3);
      }
      return reportState(id, body, ...rest);
    };
    await runnerWith(() => ({ executor: emptyCappedExecutor() }), forge.gitlab, undefined, undefined, {
      outbox, outboxTerminalMaxBytes: 1 << 20, gapFillMax: 100,
    }).execute(claim);
    assert.equal(api.completionPermitRequests.length, 1);
    const permit = api.completionPermitRequests[0]!.body;
    assert.ok(baseHead, "the clone's committed base is resolvable");
    assert.deepEqual(permit, {
      contract_revision: 7, branch: "agent/issue-2080", head: baseHead, claim_generation: 3, scope_capped: true,
    });
    assert.ok(terminal, "the completed report was sent, even though its ACK was refused");
    assert.equal(terminal.head, baseHead);
    assert.equal(terminal.branch, permit.branch);
    assert.equal(terminal.report_only, true);
    assert.equal(terminal.scope_capped, true);
    assert.equal(terminal.report_md, "Stopped before m1; nothing to land.");
    assert.ok(journalAtSend, "completion was durably journaled before the send");
    const journal = await outbox.readTerminalJournal(claim.run_id, 3);
    assert.equal(journal?.body.status, "completed");
    assert.equal(journal?.body.head, baseHead);
    assert.equal(journal?.body.branch, permit.branch);
    assert.equal(journal?.body.scope_capped, true);
    assert.equal(journal?.body.report_only, true);
    assert.equal(journal?.body.report_md, terminal.report_md);
    assert.equal(pushes, 0);
    assert.equal(forge.all.length, 0);
  });

  for (const failure of ["denied", "missing head", "missing revision"] as const) {
    it(`${failure} never journals a successful empty capped completion`, async () => {
      const claim = gitlabClaim(2081, {
        claim_generation: 3,
        config: { completion_contract_version: 1, ...(failure === "missing revision" ? {} : { contract_revision: 1 }) },
      });
      api.setCompletionPermitResponse(false, { denyReason: "scope_cap_not_reached" });
      if (failure === "missing head") git.branchTip = async () => null;
      let pushes = 0;
      git.pushBranch = async () => { pushes++; throw new Error("empty slice must not push"); };
      const forge = fakeGitlab();
      const outbox = await cappedOutbox();
      api.failStateWhen(claim.run_id, (b) => b.status === "failed", { httpStatus: 409, runStatus: "running" });
      await runnerWith(() => ({ executor: emptyCappedExecutor() }), forge.gitlab, undefined, undefined, {
        outbox, outboxTerminalMaxBytes: 1 << 20, gapFillMax: 100, recoveryRetryMs: 1,
      }).execute(claim);
      assert.ok(!api.states.some((s) => s.runId === claim.run_id && s.body.status === "completed"));
      const journal = await outbox.readTerminalJournal(claim.run_id, 3);
      assert.notEqual(journal?.body.status, "completed");
      assert.ok(api.completionHoldRequests.length > 0 || api.states.some((s) => s.runId === claim.run_id && s.body.status === "failed"), "uses the existing hold/fail path");
      assert.equal(api.completionPermitRequests.length, failure === "denied" ? 1 : 0);
      if (failure === "denied") assert.equal(api.completionPermitRequests[0]!.body.scope_capped, true);
      assert.equal(pushes, 0);
      assert.equal(forge.all.length, 0);
    });
  }

  it("preserves legacy zero-slice completion without a permit or identity fields", async () => {
    const claim = gitlabClaim(2082);
    const forge = fakeGitlab();
    await runnerWith(() => ({ executor: emptyCappedExecutor() }), forge.gitlab).execute(claim);
    const terminal = api.states.find((s) => s.runId === claim.run_id && s.body.status === "completed")?.body;
    assert.equal(terminal?.report_only, true);
    assert.equal(terminal?.scope_capped, true);
    assert.equal(terminal?.head, undefined);
    assert.equal(terminal?.branch, undefined);
    assert.equal(api.completionPermitRequests.length, 0);
    assert.equal(forge.all.length, 0);
  });
});

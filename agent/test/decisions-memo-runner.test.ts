import { describe, it } from "node:test";
import assert from "node:assert/strict";
import type { SDKMessage } from "@anthropic-ai/claude-agent-sdk";
import { recordingLogger, nullLogger } from "./helpers.js";
import { SdkExecutor, type SdkQueryFn } from "../src/sdk-executor.js";
import type { Executor } from "../src/executor.js";
import { DECISIONS_MEMO_MAX_BYTES } from "../src/decisions-memo.js";
import type { ClaimResponse } from "../src/protocol.js";
import {
  api,
  assistant,
  fakeGitlab,
  git,
  gitlabClaim,
  homeDir,
  input,
  installHarness,
  resultOk,
  runnerWith,
  simulateCommittedWork,
} from "./runner-harness.js";

installHarness();

// Issue #2083 M2: the decisions memo, runner end to end. A raw Claude assistant message
// carries `decisions_memo` on signal_done; it must travel Claude adapter -> reducer ->
// ExecutorResult -> one POST, and appear nowhere else (run messages, reports, logs).

// Assembled at runtime so no secret-shaped literal sits in source; it also proves the
// redactor sees the joined value (the redaction case below registers it as a claim secret).
const CANARY = ["CANARY", "decisions", "memo", "7f3a9c"].join("-");

interface Driven {
  prompts: string[];
  queryFn: SdkQueryFn;
}

/** A scripted lead: plan turn, then a done turn whose signal_done carries `memo` (when given).
 *  Records the text of every prompt the model was sent, to prove injection. */
function scriptedLead(memo: unknown, withDone = true): Driven {
  const prompts: string[] = [];
  const done: Record<string, unknown> = memo === undefined ? {} : { decisions_memo: memo };
  const scripts: SDKMessage[][] = [
    [assistant([{ type: "tool_use", id: "p", name: "mcp__uzi__submit_plan", input: { plan_md: "# PLAN\n- do it" } }]), resultOk()],
    withDone
      ? [assistant([{ type: "text", text: "done" }, { type: "tool_use", id: "d", name: "mcp__uzi__signal_done", input: done }]), resultOk()]
      : [assistant([{ type: "text", text: "no signal" }]), resultOk()],
  ];
  let i = 0;
  const queryFn: SdkQueryFn = (params) => {
    const script = scripts[Math.min(i, scripts.length - 1)]!;
    i++;
    return (async function* () {
      for await (const m of params.prompt) {
        const content = (m as { message?: { content?: unknown } }).message?.content;
        prompts.push(typeof content === "string" ? content : JSON.stringify(content));
      }
      for (const m of script) yield m;
    })();
  };
  return { prompts, queryFn };
}

const KINDS: Array<[string, (overrides?: Partial<ClaimResponse>) => ClaimResponse]> = [
  ["issue", (o) => gitlabClaim(7, o)],
  ["prompt", (o) => gitlabClaim(23, { kind: "prompt", ...o })],
  ["self_improve", (o) => gitlabClaim(77, { kind: "self_improve", ...o })],
  ["mr_rework", (o) => gitlabClaim(42, { kind: "mr_rework", issue_iid: null, branch: "agent/issue-42", ...o })],
];

const enabledMemo = (body: unknown = { format: 1, body: "earlier-run-notes", source_run_id: "r0" }) => ({
  status: 200,
  body: { enabled: true, memo: body },
});

async function drive(claim: ClaimResponse, lead: Driven, exec?: Executor) {
  const { gitlab, calls } = fakeGitlab();
  const { logger, lines } = recordingLogger();
  api.setInputs(claim.run_id, [input("approve_plan")]);
  const executor = exec ?? new SdkExecutor(nullLogger(), homeDir, { queryFn: lead.queryFn });
  await runnerWith(() => ({ executor }), gitlab, undefined, logger).execute(claim);
  const completed = api.states.find((s) => s.runId === claim.run_id && s.body.status === "completed");
  return { calls, lines, completed };
}

const persisted = (claim: ClaimResponse, lines: unknown[]): string =>
  JSON.stringify({
    messages: api.messages(claim.run_id),
    states: api.states.filter((s) => s.runId === claim.run_id),
    lines,
  });

describe("decisions memo: end-to-end save", () => {
  for (const [kind, mk] of KINDS) {
    it(`${kind}: the canary reaches exactly one POST with the claim generation and nowhere else`, async () => {
      simulateCommittedWork();
      api.decisionsMemo.get = enabledMemo(null);
      const claim = mk({ claim_generation: 3 });
      const { completed, lines } = await drive(claim, scriptedLead(CANARY));
      assert.ok(completed, "the run completed");
      assert.ok(completed.body.mr_iid !== undefined, "an MR-path completion");
      assert.strictEqual(api.decisionsMemoGets.length, 1);
      assert.strictEqual(api.decisionsMemoGets[0]!.claimGeneration, "3");
      assert.strictEqual(api.decisionsMemoPosts.length, 1, "exactly one save");
      const post = api.decisionsMemoPosts[0]!;
      assert.strictEqual(post.runId, claim.run_id);
      assert.strictEqual(post.body.claim_generation, 3);
      assert.strictEqual(post.body.body, CANARY);
      const everything = persisted(claim, lines);
      // The scan is meaningful: the same assistant frame's text IS persisted, while the
      // signal_done call (marked as a signal by the Claude adapter) is filtered from output.
      assert.ok(api.messages(claim.run_id).some((m) => JSON.stringify(m.payload).includes("done")));
      assert.ok(!everything.includes(CANARY), "the memo body never appears in a run message, report or log");
      const statusTexts = api.messages(claim.run_id).filter((m) => m.kind === "status").map((m) => JSON.stringify(m.payload));
      assert.ok(statusTexts.some((t) => t.includes(`decisions memo saved (${CANARY.length} bytes)`)));
    });
  }

  it("redacts a claim secret out of the body before saving", async () => {
    simulateCommittedWork();
    api.decisionsMemo.get = enabledMemo(null);
    const claim = gitlabClaim(7, { secrets: { forge_pat: CANARY, anthropic_oauth_token: "dummy-oauth-do-not-scan" } });
    await drive(claim, scriptedLead(`note ${CANARY} end`));
    assert.strictEqual(api.decisionsMemoPosts.length, 1);
    assert.ok(!api.decisionsMemoPosts[0]!.body.body!.includes(CANARY));
  });

  it("re-clamps an executor-provided oversize memo to the cap", async () => {
    simulateCommittedWork();
    api.decisionsMemo.get = enabledMemo(null);
    const exec: Executor = { run: async (ctx) => ({ branch: ctx.branch, decisionsMemo: "é".repeat(DECISIONS_MEMO_MAX_BYTES) }) };
    await drive(gitlabClaim(7), scriptedLead(undefined), exec);
    assert.strictEqual(api.decisionsMemoPosts.length, 1);
    const body = api.decisionsMemoPosts[0]!.body.body!;
    assert.ok(Buffer.byteLength(body, "utf8") <= DECISIONS_MEMO_MAX_BYTES);
    assert.ok(!body.includes("�"));
  });
});

describe("decisions memo: no save", () => {
  it("report-only completion posts nothing", async () => {
    simulateCommittedWork();
    api.decisionsMemo.get = enabledMemo(null);
    const exec: Executor = { run: async (ctx) => ({ branch: ctx.branch, reportOnly: true, summary: "evidence", decisionsMemo: CANARY }) };
    const { completed } = await drive(gitlabClaim(7), scriptedLead(undefined), exec);
    assert.strictEqual(completed?.body.report_only, true);
    assert.strictEqual(api.decisionsMemoPosts.length, 0);
  });

  it("an empty-diff prompt run posts nothing", async () => {
    git.changedFiles = (async () => []) as typeof git.changedFiles;
    api.decisionsMemo.get = enabledMemo(null);
    const exec: Executor = { run: async (ctx) => ({ branch: ctx.branch, decisionsMemo: CANARY }) };
    const { completed } = await drive(gitlabClaim(23, { kind: "prompt" }), scriptedLead(undefined), exec);
    assert.strictEqual(completed?.body.report_only, true);
    assert.strictEqual(api.decisionsMemoPosts.length, 0);
  });

  it("a failed run posts nothing", async () => {
    simulateCommittedWork();
    api.decisionsMemo.get = enabledMemo(null);
    const exec: Executor = { run: async () => { throw new Error("boom"); } };
    await drive(gitlabClaim(7), scriptedLead(undefined), exec);
    assert.ok(api.states.some((s) => s.body.status === "failed"));
    assert.strictEqual(api.decisionsMemoPosts.length, 0);
  });

  it("a completion with no memo from the lead posts nothing", async () => {
    simulateCommittedWork();
    api.decisionsMemo.get = enabledMemo(null);
    await drive(gitlabClaim(7), scriptedLead(undefined));
    assert.strictEqual(api.decisionsMemoPosts.length, 0);
  });

  it("a non-memo kind (ci_fix) neither reads nor saves", async () => {
    simulateCommittedWork();
    api.decisionsMemo.get = enabledMemo(null);
    const exec: Executor = { run: async (ctx) => ({ branch: ctx.branch, decisionsMemo: CANARY }) };
    await drive(gitlabClaim(7, { kind: "ci_fix", pipeline: { id: 1, ref: "agent/issue-7", web_url: "https://x/1", failed_jobs: [] } as never }), scriptedLead(undefined), exec);
    assert.strictEqual(api.decisionsMemoGets.length, 0);
    assert.strictEqual(api.decisionsMemoPosts.length, 0);
  });

  it("enabled=false: the param is not exposed and nothing is saved", async () => {
    simulateCommittedWork();
    api.decisionsMemo.get = { status: 200, body: { enabled: false, memo: null } };
    let sawParam = false;
    const exec: Executor = {
      run: async (ctx) => {
        sawParam = ctx.decisionsMemoEnabled === true;
        return { branch: ctx.branch, decisionsMemo: CANARY };
      },
    };
    await drive(gitlabClaim(7), scriptedLead(undefined), exec);
    assert.strictEqual(sawParam, false);
    assert.strictEqual(api.decisionsMemoPosts.length, 0);
  });

  it("a rejected save (409) still completes the run normally, logging no content", async () => {
    simulateCommittedWork();
    api.decisionsMemo.get = enabledMemo(null);
    api.decisionsMemo.postStatus = 409;
    const claim = gitlabClaim(7);
    const { completed, lines } = await drive(claim, scriptedLead(CANARY));
    assert.ok(completed?.body.mr_iid !== undefined);
    assert.strictEqual(api.decisionsMemoPosts.length, 1);
    const warn = (lines as Array<{ msg: string }>).find((l) => l.msg === "decisions memo not saved");
    assert.ok(warn, "a fixed warning is logged");
    assert.ok(!persisted(claim, lines).includes(CANARY));
  });

  it("an unreachable save endpoint (500) still completes the run", async () => {
    simulateCommittedWork();
    api.decisionsMemo.get = enabledMemo(null);
    api.decisionsMemo.postStatus = 500;
    const { completed } = await drive(gitlabClaim(7), scriptedLead(CANARY));
    assert.ok(completed?.body.mr_iid !== undefined);
  });
});

describe("decisions memo: claim-time read", () => {
  const rework = () => gitlabClaim(42, { kind: "mr_rework", issue_iid: null, branch: "agent/issue-42" });

  it("mr_rework injects a valid memo into the plan prompt and reports only its byte count", async () => {
    simulateCommittedWork();
    api.decisionsMemo.get = enabledMemo({ format: 1, body: "REMEMBER-THE-FIX", source_run_id: "r0" });
    const claim = rework();
    const lead = scriptedLead("next-memo");
    await drive(claim, lead);
    assert.ok(lead.prompts[0]!.includes("REMEMBER-THE-FIX"));
    assert.match(lead.prompts[0]!, /<untrusted_decisions_memo_[0-9a-f]+>/);
    const statuses = api.messages(claim.run_id).filter((m) => m.kind === "status").map((m) => JSON.stringify(m.payload));
    assert.ok(statuses.some((t) => t.includes("decisions memo injected (16 bytes)")));
    assert.ok(!persisted(claim, []).includes("REMEMBER-THE-FIX"), "the earlier memo is not persisted either");
  });

  it("a non-rework memo kind learns enabled but injects nothing", async () => {
    simulateCommittedWork();
    api.decisionsMemo.get = enabledMemo({ format: 1, body: "REMEMBER-THE-FIX" });
    const lead = scriptedLead(CANARY);
    await drive(gitlabClaim(7), lead);
    assert.ok(!lead.prompts[0]!.includes("REMEMBER-THE-FIX"));
    assert.strictEqual(api.decisionsMemoPosts.length, 1, "enabled was learned, so the save happens");
  });

  const bad: Array<[string, { status: number; body: unknown } | undefined]> = [
    ["404", { status: 404, body: { error: "not found" } }],
    ["409 claim_not_current", { status: 409, body: { error: "claim_not_current" } }],
    ["500", { status: 500, body: { error: "x" } }],
    ["no route", undefined],
    ["enabled false", { status: 200, body: { enabled: false, memo: { format: 1, body: "STALE-MEMO" } } }],
    ["format 2", enabledMemo({ format: 2, body: "STALE-MEMO" })],
    ["empty body", enabledMemo({ format: 1, body: "" })],
    ["oversize body", enabledMemo({ format: 1, body: "STALE-MEMO".padEnd(DECISIONS_MEMO_MAX_BYTES + 1, "x") })],
    ["non-string body", enabledMemo({ format: 1, body: 12 })],
  ];
  for (const [name, get] of bad) {
    it(`falls back to no memo and still completes: ${name}`, async () => {
      simulateCommittedWork();
      api.decisionsMemo.get = get;
      const claim = rework();
      const lead = scriptedLead(undefined);
      const { completed, lines } = await drive(claim, lead);
      assert.ok(completed, "the run proceeded to completion");
      assert.ok(!lead.prompts[0]!.includes("untrusted_decisions_memo"), "no memo block injected");
      assert.ok(!lead.prompts[0]!.includes("STALE-MEMO"));
      assert.ok(!JSON.stringify(lines).includes("STALE-MEMO"), "a rejected body is never logged");
    });
  }
});

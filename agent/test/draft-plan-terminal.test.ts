import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { Outbox } from "../src/outbox.js";
import { nullLogger } from "./helpers.js";
import { RunTurnReducerImpl } from "../src/harness-reducer.js";
import { RunRunner } from "../src/runner.js";
import type { Executor } from "../src/executor.js";
import { api, client, git, fakeGitlab, gitlabClaim, installHarness, runner } from "./runner-harness.js";
import { commitInTree, fixture } from "./codex-reap-fixture.js";
import { capture, claudeScript, request } from "./draft-plan-script.js";

installHarness();

for (const [name, status, live] of [
  ["transient close exhaustion", 503, false],
  ["permanent message failure during provider", 401, true],
  ["permanent message failure during ordinary close", 401, false],
] as const) {
  test(`draft capture preserves existing terminal winner and custody on ${name}`, async () => {
    const events: string[] = [];
    const claim = gitlabClaim(23231, { claim_generation: 9 });
    const { coord, safety, root } = fixture(events, () =>
      live || status === 503
        ? api.states.some((s) => s.runId === claim.run_id && s.body.status === "failed")
        : false);
    const original = new Error("provider policy refusal before submit");
    const outbox = new Outbox({ root: path.join(root, "outbox"), log: nullLogger(),
      runMaxBytes: 64 * 1024 * 1024, maxBytes: 512 * 1024 * 1024, retentionMs: 7 * 86_400_000 });
    await outbox.init();
    const closePermanent = status === 401 && !live;
    const journals: ReturnType<Outbox["readTerminalJournal"]>[] = [];
    try {
      api.failMessagesNext(100, status);
      api.onState(claim.run_id, (body) => {
        if (body.status === "failed") {
          events.push("failed");
          journals.push(outbox.readTerminalJournal(claim.run_id, 9));
        }
      });
      const executor: Executor = {
        safety,
        async run(ctx) {
          commitInTree(ctx.worktreePath, "WORK.txt", "existing custody fixture\n");
          const harness = claudeScript([capture("# draft before refusal")]);
          const reducer = new RunTurnReducerImpl({ request() {}, get: async () => undefined });
          reducer.beginTurn();
          for await (const event of harness.startTurn(request).events) {
            for (const message of (await reducer.accept(event)).messages) ctx.emit(message);
            if (live) {
              // Match the existing permanent-failure regression: the transport wins
              // while the provider is active, then its abort unwinds the provider.
              await new Promise<never>((_resolve, reject) => {
                const aborted = () => reject(original);
                if (ctx.signal?.aborted) aborted();
                else ctx.signal?.addEventListener("abort", aborted, { once: true });
              });
            }
          }
          events.push("provider-refused");
          throw original;
        },
      };
      const gitlab = fakeGitlab().gitlab;
      const options = { recovery: coord, outbox, outboxTerminalMaxBytes: 1 << 20, gapFillMax: 100 };
      // Defer timer delivery beyond this test\'s deadline: only ordinary close can
      // initiate this case\'s first post, after generic failure\'s suppression check.
      const run = closePermanent
        ? new RunRunner(client, git, () => ({ executor }), nullLogger(), 1_000_000_000, undefined, {
          pollMs: 5, planApprovalTimeoutMs: 0, questionTimeoutMs: 600, prDescriptionHeadLagMs: 0, gitlab, ...options,
        })
        : runner(executor, gitlab, undefined, options);
      await run.execute(claim);
      const failed = api.states.filter((s) => s.runId === claim.run_id && s.body.status === "failed");
      assert.equal(failed.length, closePermanent ? 2 : 1);
      if (status === 503) assert.equal(failed[0]?.body.failure_reason, original.message);
      else assert.match(failed[0]?.body.failure_reason ?? "", /message persistence failed permanently/);
      if (closePermanent) {
        assert.ok(events.indexOf("provider-refused") < events.indexOf("reconcile"), "callback reaps after provider refusal");
        const selected = await Promise.all(journals);
        assert.deepEqual(selected.map((journal) => journal?.body.failure_reason), failed.map((attempt) => attempt.body.failure_reason),
          "each report uses the selected write-ahead journal; the first ACK retires the transport winner");
        assert.equal(outbox.hasPendingTerminal(claim.run_id, 9), false, "both acknowledged journals retired");
        assert.equal(failed[1]?.body.failure_reason, original.message, "ordinary failure attempts its own report after close");
      }
      assert.ok(events.indexOf("reconcile") >= 0 && events.indexOf("reconcile") < events.indexOf("failed"));
      assert.ok(events.indexOf("failed") < events.indexOf("reserve"), "committed custody captured after terminal report");
      assert.equal(events.filter((event) => event === "reconcile").length, 1);
      assert.equal(api.messages(claim.run_id).some((m) => m.payload.event === "draft_plan_capture"), false,
        "an unavailable sink makes no capture persistence promise");
    } finally {
      fs.rmSync(root, { recursive: true, force: true });
    }
  });
}

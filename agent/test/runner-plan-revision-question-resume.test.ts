import assert from "node:assert/strict";
import { setImmediate as nextTick } from "node:timers/promises";
import { it } from "node:test";
import type { Executor } from "../src/executor.js";
import { api, client, fakeGitlab, installHarness, runner } from "./runner-harness.js";
import { freshClaim, newApi, row, send } from "./gate-revision-harness.js";

installHarness();

function barrier() {
  let release!: () => void;
  const promise = new Promise<void>(resolve => { release = resolve; });
  return { promise, release };
}

for (const scenario of [
  { name: "plan-missing fallback", missing: true, interactive: false, kind: "issue" },
  { name: "ordinary interactive issue", missing: false, interactive: true, kind: "issue" },
  { name: "ordinary non-issue", missing: false, interactive: false, kind: "prompt" },
  { name: "ordinary issue control", missing: false, interactive: false, kind: "issue" },
] as const) {
  it(`revision ${scenario.name} confirms running before the next source check`, async t => {
    newApi();
    api.planRevisionRetries = true;
    client.protocolFeatures = [...client.protocolFeatures, "plan_revision_retry_v1"];
    assert.equal(client.hasFeature("plan_revision_retry_v1"), true);
    const claim = freshClaim(1, { kind: scenario.kind, interactive: scenario.interactive });
    const id = claim.run_id;
    const requestedRunning = barrier();
    const sourcePublished = barrier();
    const sourceRefused = barrier();
    const releaseRunning = barrier();
    const continued = barrier();
    let questionSeen = false;
    let answerReturned = false;
    let continuedAfterConfirmation = false;
    let sourceFailure: unknown;
    const realReport = client.reportState.bind(client);
    t.mock.method(client, "reportState", async (...args: Parameters<typeof client.reportState>) => {
      if (args[0] === id && args[1].status === "running" && questionSeen) {
        requestedRunning.release();
        await releaseRunning.promise;
      }
      return realReport(...args);
    });
    api.onState(id, body => {
      if (body.status === "awaiting_approval") sourcePublished.release();
      if (body.status !== "awaiting_input" || !body.open_question_id) return;
      questionSeen = true;
      send(id, row("answer", JSON.stringify({ question_id: body.open_question_id, answers: ["continue"] })));
    });
    const executor: Executor = {
      async run(ctx) {
        const verdict = await ctx.gatePlan!("# Source plan");
        assert.equal(verdict.kind, "revise");
        await ctx.beforePlanRevisionTurn!();
        assert.ok(api.ownershipRequests.includes(id), "active revision source check is negotiated");
        const answer = scenario.missing
          ? await ctx.askPlanMissing!()
          : await ctx.askUser!([{ question: "Continue planning?", header: "Plan" }]);
        assert.equal(answer.kind, "answer");
        answerReturned = true;
        try { await ctx.beforePlanRevisionTurn!(); }
        catch (error) { sourceFailure = error; sourceRefused.release(); throw error; }
        continuedAfterConfirmation = true;
        continued.release();
        throw new Error("test reached the confirmed revision continuation");
      },
    };
    const done = runner(executor, fakeGitlab().gitlab).execute(claim);
    t.after(async () => {
      releaseRunning.release();
      send(id, row("cancel"));
      await done.catch(() => undefined);
    });
    await Promise.race([
      sourcePublished.promise,
      done.then(() => { throw new Error("run ended before publishing its source plan"); }),
    ]);
    send(id, row("revise_plan", "clarify before revising"));
    await Promise.race([
      requestedRunning.promise,
      done.then(() => { throw new Error("run ended before attempting the answer's running report"); }),
    ]);
    // Flush the answer promise's continuation while the real running request is
    // still held. This is an event-loop turn, not a wall-clock delay.
    await nextTick();
    if (answerReturned) await Promise.race([sourceRefused.promise, done]);
    assert.equal(answerReturned, false,
      `an unanswered running report must not release the revision continuation; source check: ${String(sourceFailure)}`);
    assert.equal(continuedAfterConfirmation, false);
    releaseRunning.release();
    await Promise.race([
      continued.promise,
      done.then(() => { if (!continuedAfterConfirmation) throw new Error("the revision stopped before its next provider boundary"); }),
    ]);
    await done;
    assert.equal(continuedAfterConfirmation, true);
  });
}

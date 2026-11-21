import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import http from "node:http";
import type { AddressInfo } from "node:net";
import { WorkerClient } from "../src/client.js";
import type { ExecutorResult, RunContext } from "../src/executor.js";
import { PhaseSwitchUnverifiedError } from "../src/phase-switch-unverified.js";
import type { PhaseSwitchHoldOutcome } from "../src/protocol.js";
import { PHASE_SWITCH_HOLD_REASON } from "../src/protocol.js";
import { nullLogger } from "./helpers.js";
import { api, client, fakeGitlab, gitlabClaim, installHarness, runnerWith, worktreeDirFor } from "./runner-harness.js";

installHarness();

// PRD #3252 D8 (m4): the runner side of the phase-switch hold. The executor is a scripted stand-in that
// either returns ExecutorResult.phaseSwitchHeld (implement) or throws PhaseSwitchUnverifiedError (plan);
// the hold request is a scripted client call and the ownership read is the fake api's wire override, so
// every answer the retry loop classifies is exactly the one the test names.

type Post = { phase: "plan" | "implement"; generation: number; head?: string; sawStatus: boolean };
interface Own { status: number; body?: Record<string, unknown> }

const PAUSED_TEXT = /^Paused: /;
const ownRow = (generation: number, over: Record<string, unknown> = {}): Own => ({
  status: 200,
  body: { status: "running", claim_generation: generation, claim_released: false, hold_reason: null, cancellation_requested: false, ...over },
});
const held: PhaseSwitchHoldOutcome = { outcome: "held", status: "paused" };
const unknown: PhaseSwitchHoldOutcome = { outcome: "unknown", detail: "socket hang up" };

interface HoldOptions {
  /** What the executor does: an implement result, or a plan-phase throw. */
  throws?: unknown;
  held?: NonNullable<ExecutorResult["phaseSwitchHeld"]>;
  /** The nth (1-based) hold request answer; the last entry repeats. */
  post: PhaseSwitchHoldOutcome[];
  /** The nth (1-based) ownership read; the last entry repeats. */
  ownership?: Own[];
  generation?: number;
  /** Reclaim of an earlier flight: its run id, so the retained clone is the same run's. */
  runId?: string;
  iid: number;
  /** Queue an owner cancel input while the nth (1-based) hold request is in flight. */
  cancelDuringPost?: number;
  /** Production configuration: no independent hold deadline (the default tests set a TEST-ONLY one). */
  noDeadline?: boolean;
}

const IMPLEMENT_HELD = {
  cause: "model_mismatch" as const, phase: "implement" as const,
  requested: { model: "sonnet", effort: "medium" }, observed: { model: "claude-opus-5-5", effort: "medium" },
};

async function runHold(opts: HoldOptions) {
  const { gitlab } = fakeGitlab();
  const generation = opts.generation ?? 1;
  const claim = gitlabClaim(opts.iid, {
    plan_approved: true, plan_source: "seeded", plan_md: "# Seeded plan\n- do it", session_id: null, claim_generation: generation,
    ...(opts.runId !== undefined ? { run_id: opts.runId } : {}),
  });
  const order: string[] = [];
  const posts: Post[] = [];
  let runs = 0;
  let ownershipReads = 0;
  const executor = {
    run: async (ctx: RunContext): Promise<ExecutorResult> => {
      runs++;
      order.push("run");
      if (opts.throws !== undefined) throw opts.throws;
      return { branch: ctx.branch, phaseSwitchHeld: opts.held ?? IMPLEMENT_HELD };
    },
    closeAndSettlePlanSummary: async () => { order.push("settle"); },
    killAgentTree: () => { order.push("kill"); },
  };
  const original = client.requestPhaseSwitchHold.bind(client);
  (client as unknown as { requestPhaseSwitchHold: unknown }).requestPhaseSwitchHold = async (
    runId: string, args: { head?: string; claimGeneration: number; phase: "plan" | "implement" },
  ) => {
    order.push("post");
    posts.push({
      phase: args.phase, generation: args.claimGeneration, ...(args.head !== undefined ? { head: args.head } : {}),
      sawStatus: api.messages(runId).some((m) => PAUSED_TEXT.test(String((m.payload as { text?: unknown } | undefined)?.text ?? ""))),
    });
    if (opts.cancelDuringPost === posts.length) api.setInputs(runId, [{ id: 1, kind: "cancel" }]);
    return opts.post[Math.min(posts.length, opts.post.length) - 1]!;
  };
  if (opts.ownership) {
    api.ownershipWire.set(claim.run_id, (res: http.ServerResponse) => {
      const answer = opts.ownership![Math.min(++ownershipReads, opts.ownership!.length) - 1]!;
      res.writeHead(answer.status, { "content-type": "application/json" });
      res.end(JSON.stringify(answer.body ?? { error: "run not found for this worker" }));
    });
  }
  const t0 = Date.now();
  let finished = false;
  const flight = runnerWith(() => ({ executor }), gitlab, undefined, nullLogger(),
    { recoveryRetryMs: 5, ...(opts.noDeadline ? {} : { phaseSwitchHoldDeadlineMs: 3_000 }) }).execute(claim).finally(() => { finished = true; });
  try {
    await flight;
  } finally {
    (client as unknown as { requestPhaseSwitchHold: unknown }).requestPhaseSwitchHold = original;
  }
  const states = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body.status);
  const cancelReports = api.states.filter((s) => s.runId === claim.run_id && s.body.status === "failed" && s.body.failure_reason === "run cancelled").length;
  const feed = api.messages(claim.run_id).map((m) => String((m.payload as { text?: unknown } | undefined)?.text ?? ""));
  return {
    runId: claim.run_id, order, posts, runs, cancelReports, ownershipReads: () => ownershipReads, states, feed, elapsedMs: Date.now() - t0,
    finished: () => finished, cloneKept: fs.existsSync(worktreeDirFor(opts.iid)),
  };
}

const terminal = (states: string[]): string[] => states.filter((s) => ["completed", "failed", "cancelled"].includes(s));

describe("phase-switch hold (issue #3252 m4)", () => {
  it("an implement hold result quiesces, preserves, says why, then holds: no finalize, no terminal report, one turn", async () => {
    const r = await runHold({ iid: 32521, post: [held] });
    assert.equal(r.runs, 1, "the executor ran one turn and was never revived");
    assert.deepEqual(r.posts.map((p) => [p.phase, p.generation]), [["implement", 1]]);
    assert.equal(r.posts[0]!.sawStatus, true, "the status line was flushed before the hold request");
    const afterRun = r.order.slice(r.order.indexOf("run"));
    const beforePost = afterRun.slice(0, afterRun.indexOf("post"));
    assert.ok(beforePost.includes("settle") && beforePost.includes("kill"), `quiesce precedes the post: ${r.order.join(",")}`);
    assert.ok(beforePost.indexOf("settle") < beforePost.indexOf("kill"), "plan summary settles before the tree is reaped");
    assert.ok(r.feed.includes("Paused: the runtime kept the planning model claude-opus-5-5 instead of switching to sonnet. Resume retries on a new claim; Cancel ends the run."), r.feed.join(" | "));
    assert.deepEqual(terminal(r.states), [], "no terminal report");
    assert.ok(!r.states.includes("completed"), "nothing was published");
    assert.equal(r.cloneKept, true, "the clone is preserved");
  });

  it("an implement hold refused as plan_not_approved is re-posted once for the plan phase", async () => {
    const r = await runHold({ iid: 32522, post: [{ outcome: "refused", status: "awaiting_approval", holdReason: null, reason: "plan_not_approved" }, held] });
    assert.deepEqual(r.posts.map((p) => p.phase), ["implement", "plan"]);
    assert.deepEqual(terminal(r.states), []);
  });

  it("a plan-phase switch-back mismatch holds the run instead of failing it", async () => {
    const err = new PhaseSwitchUnverifiedError("model_mismatch", { model: "opus", effort: "high" }, { model: "claude-sonnet-5-5", effort: "high" });
    const r = await runHold({ iid: 32523, throws: err, post: [held] });
    assert.deepEqual(r.posts.map((p) => p.phase), ["plan"]);
    assert.ok(r.feed.includes("Paused: the runtime kept the implementation model claude-sonnet-5-5 instead of switching back to the planning model opus. Resume retries on a new claim; Cancel ends the run."), r.feed.join(" | "));
    assert.deepEqual(terminal(r.states), [], "held, not failed");
    assert.equal(r.runs, 1);
  });

  it("the status line names the real remedy for a deterministic cause and the transient text for a timeout", async () => {
    const deterministic = await runHold({
      iid: 32524, post: [held],
      held: { cause: "no_characterized_expectation", phase: "implement", requested: { model: "opusplan" }, observed: {} },
    });
    assert.ok(deterministic.feed.includes("Paused: this worker cannot verify a switch to opusplan. Choose a supported model (opus, sonnet, haiku, fable) or a full model id, or clear the Claude planning setting, then Resume; Cancel ends the run."), deterministic.feed.join(" | "));
    const transient = await runHold({
      iid: 32525, post: [held],
      held: { cause: "timeout", phase: "implement", requested: { model: "sonnet" }, observed: {} },
    });
    assert.ok(transient.feed.includes("Paused: the runtime's settings could not be verified before switching models. Resume retries on a new claim; Cancel ends the run."), transient.feed.join(" | "));
    const effort = await runHold({
      iid: 32526, post: [held],
      held: { cause: "effort_mismatch", phase: "implement", requested: { model: "sonnet", effort: "medium" }, observed: { effort: "high" } },
    });
    assert.ok(effort.feed.includes("Paused: the runtime kept the planning effort high instead of switching to medium. Resume retries on a new claim; Cancel ends the run."), effort.feed.join(" | "));
  });

  it("an unapplied request and a failed ownership read, then restored connectivity: held, no further turn, no terminal report", async () => {
    const r = await runHold({
      iid: 32527, post: [unknown, unknown, held],
      ownership: [{ status: 503 }, { status: 503 }, ownRow(1), ownRow(1)],
    });
    assert.equal(r.runs, 1, "no further turn while the hold was unconfirmed");
    assert.equal(r.posts.length, 3, "re-posted only after a live observation");
    assert.ok(r.ownershipReads() >= 3);
    assert.deepEqual(terminal(r.states), []);
    assert.equal(r.cloneKept, true);
  });

  it("a lost ACK followed by an owner Resume: queued with the hold reason at our generation ends the flight with no further post or report", async () => {
    const r = await runHold({
      iid: 32528, post: [unknown, held], noDeadline: true,
      ownership: [ownRow(1, { status: "queued", hold_reason: PHASE_SWITCH_HOLD_REASON })],
    });
    assert.equal(r.posts.length, 1, "no post after the observation");
    assert.equal(r.ownershipReads(), 1, "the queued observation settled the outcome at once");
    assert.ok(r.elapsedMs < 20_000, `resumed is recognised promptly, not at the hold deadline (${r.elapsedMs} ms)`);
    assert.equal(r.runs, 1);
    assert.deepEqual(terminal(r.states), []);
    assert.deepEqual([...new Set(r.states)], ["running"], "no report changed the row after the observation");
    assert.equal(r.cloneKept, true);
    // The slot is free: the next claim (a new generation) runs the executor, hence the barrier, again.
    const next = await runHold({ iid: 32528, runId: r.runId, generation: 2, post: [held], ownership: [ownRow(2)] });
    assert.equal(next.runs, 1);
    assert.deepEqual(next.posts.map((p) => p.generation), [2]);
  });

  it("a 409 whose ownership shows queued with the hold reason is confirmed by the read, with no second post", async () => {
    const r = await runHold({
      iid: 32529, post: [{ outcome: "refused", status: "queued", holdReason: PHASE_SWITCH_HOLD_REASON }, held], noDeadline: true,
      ownership: [ownRow(1, { status: "queued", hold_reason: PHASE_SWITCH_HOLD_REASON })],
    });
    assert.equal(r.posts.length, 1);
    assert.equal(r.ownershipReads(), 1, "one read confirmed it");
    assert.ok(r.elapsedMs < 20_000, `confirmed promptly, not at the hold deadline (${r.elapsedMs} ms)`);
    assert.deepEqual(terminal(r.states), []);
  });

  it("a partial ownership answer and a 404 on the post are retried, never read as success", async () => {
    const r = await runHold({
      iid: 32530, post: [unknown, unknown, held],
      ownership: [{ status: 200, body: { status: "running" } }, { status: 200, body: { status: "running", claim_generation: 1 } }, ownRow(1)],
    });
    assert.equal(r.posts.length, 3);
    assert.ok(r.ownershipReads() >= 3);
    assert.deepEqual(terminal(r.states), []);
  });

  it("an ownership read of paused with the reason at our generation confirms the hold without another post", async () => {
    const r = await runHold({
      iid: 32531, post: [unknown, held],
      ownership: [ownRow(1, { status: "paused", hold_reason: PHASE_SWITCH_HOLD_REASON })],
    });
    assert.equal(r.posts.length, 1);
  });

  it("a refused implement hold with the run live and unheld is re-posted once for the plan phase", async () => {
    const r = await runHold({
      iid: 32532, post: [{ outcome: "refused", status: "awaiting_approval", holdReason: null }, held],
      ownership: [ownRow(1, { status: "awaiting_approval" })],
    });
    assert.deepEqual(r.posts.map((p) => p.phase), ["implement", "plan"]);
  });

  for (const [name, own] of [
    ["ownership 404", { status: 404 }],
    ["the claim released", ownRow(1, { claim_released: true })],
    ["a different claim generation", ownRow(2)],
    ["a terminal status", ownRow(1, { status: "completed" })],
  ] as Array<[string, Own]>) {
    it(`definitive non-live (${name}) stops at once: one post, one read, no report`, async () => {
      const r = await runHold({ iid: 32540 + name.length, post: [unknown], ownership: [own], noDeadline: true });
      assert.equal(r.posts.length, 1);
      assert.equal(r.ownershipReads(), 1);
      assert.deepEqual(terminal(r.states), []);
      assert.ok(r.elapsedMs < 20_000, "returned well before the retry deadline");
      assert.ok(r.feed.some((t) => PAUSED_TEXT.test(t)), "the Paused line was posted");
      assert.ok(r.feed.includes("This worker no longer holds the run and has stopped working on it. This worker keeps its copy of the work."),
        `the feed corrects the Paused line: ${r.feed.join(" | ")}`);
    });
  }

  it("a server-side park (paused with another hold reason at our generation) ends the flight promptly with no post, report or deadline wait", async () => {
    for (const [i, reason] of ["budget_exhausted", "wall"].entries()) {
      const r = await runHold({
        iid: 32580 + i, post: [unknown, held],
        ownership: [{ status: 503 }, ownRow(1, { status: "paused", hold_reason: reason })],
        noDeadline: true,
      });
      assert.equal(r.posts.length, 1, "no post after the park observation");
      assert.equal(r.ownershipReads(), 2);
      assert.ok(r.elapsedMs < 20_000, `recognised promptly (${r.elapsedMs} ms)`);
      assert.equal(r.runs, 1);
      assert.deepEqual(terminal(r.states), [], "no terminal report");
      assert.deepEqual([...new Set(r.states)], ["running"], "no report changed the row");
      assert.equal(r.cloneKept, true, "the clone is kept");
      assert.ok(r.feed.includes(`The server paused the run first (${reason}); this worker keeps the work.`), r.feed.join(" | "));
      assert.ok(!r.feed.some((t) => t.includes("no longer holds the run")));
    }
  });

  it("TEST-ONLY phaseSwitchHoldDeadlineMs: a hold request whose reply and every ownership read are lost ends at the explicit test deadline without claiming the run is not paused", async () => {
    const r = await runHold({ iid: 32575, post: [unknown], ownership: [{ status: 503 }] });
    assert.deepEqual(terminal(r.states), [], "no terminal report");
    assert.ok(r.elapsedMs >= 2_500, `waited for the explicit test deadline (${r.elapsedMs} ms)`);
    assert.ok(r.feed.includes("The pause could not be confirmed; the run may not be paused. This worker keeps the work."),
      `the feed hedges rather than asserting the run is not paused: ${r.feed.join(" | ")}`);
    assert.ok(!r.feed.some((t) => t.includes("no longer holds the run")), "the definitive non-live line is not used for an unknown outcome");
    assert.equal(r.cloneKept, true, "the work is kept");
  });

  it("an owner Cancel queued during an unknown hold request reports cancelled and cleans up like any cancel", async () => {
    const r = await runHold({ iid: 32560, post: [unknown], cancelDuringPost: 1, ownership: [ownRow(1)], noDeadline: true });
    assert.equal(r.cancelReports, 1, `one cancelled report was sent: ${r.states.join(",")}`);
    assert.equal(r.cloneKept, false, "the normal cancelled cleanup removed the clone");
    assert.ok(!r.feed.some((t) => t.includes("could not be confirmed") || t.includes("no longer holds the run")));
    assert.ok(r.elapsedMs < 20_000, "returned well before the retry deadline");
  });

  it("an ownership read naming a cancellation request reports cancelled and cleans up like any cancel", async () => {
    const r = await runHold({
      iid: 32561, post: [unknown], cancelDuringPost: 1, noDeadline: true,
      ownership: [ownRow(1, { cancellation_requested: true })],
    });
    assert.ok(r.ownershipReads() >= 1);
    assert.equal(r.cancelReports, 1, `one cancelled report was sent: ${r.states.join(",")}`);
    assert.equal(r.cloneKept, false, "the normal cancelled cleanup removed the clone");
    assert.ok(r.elapsedMs < 20_000, "returned well before the retry deadline");
  });

  it("a pending cancellation the input read cannot confirm is retried, never reported as a lost claim", async () => {
    const r = await runHold({ iid: 32563, post: [unknown], ownership: [ownRow(1, { cancellation_requested: true })] });
    assert.ok(r.ownershipReads() > 1, "the unconfirmed cancellation is re-read, not taken as definitive");
    assert.ok(!r.feed.some((t) => t.includes("no longer holds the run")), `the claim is still ours: ${r.feed.join(" | ")}`);
    assert.ok(r.feed.includes("The pause could not be confirmed; the run may not be paused. This worker keeps the work."));
  });

  it("a plan-phase hold interrupted by an owner Cancel also reports cancelled", async () => {
    const err = new PhaseSwitchUnverifiedError("model_mismatch", { model: "opus", effort: "high" }, { model: "claude-sonnet-5-5", effort: "high" });
    const r = await runHold({ iid: 32562, throws: err, post: [unknown], cancelDuringPost: 1, ownership: [ownRow(1)] });
    assert.equal(r.cancelReports, 1, `one cancelled report was sent: ${r.states.join(",")}`);
    assert.equal(r.cloneKept, false);
  });
});

describe("WorkerClient.requestPhaseSwitchHold classification (issue #3252 m4)", () => {
  async function answer(
    respond: (req: http.IncomingMessage, res: http.ServerResponse) => void,
    args: { head?: string; claimGeneration: number; phase: "plan" | "implement" } = { head: "a".repeat(40), claimGeneration: 7, phase: "implement" },
  ): Promise<{ outcome: PhaseSwitchHoldOutcome; bodies: string[]; paths: string[] }> {
    const bodies: string[] = [];
    const paths: string[] = [];
    const server = http.createServer((req, res) => {
      let body = "";
      req.on("data", (c) => { body += c; });
      req.on("end", () => { bodies.push(body); paths.push(req.url ?? ""); respond(req, res); });
    });
    await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
    const { port } = server.address() as AddressInfo;
    try {
      const c = new WorkerClient(`http://127.0.0.1:${port}`, "worker-join-token-0123456789", "0.1.0-test", nullLogger(), { sleep: async () => {} });
      return { outcome: await c.requestPhaseSwitchHold("run-1", args), bodies, paths };
    } finally {
      server.closeAllConnections();
      await new Promise<void>((resolve) => server.close(() => resolve()));
    }
  }
  const json = (res: http.ServerResponse, code: number, body: unknown) => {
    res.writeHead(code, { "content-type": "application/json" });
    res.end(JSON.stringify(body));
  };

  it("held only on a 200 naming paused plus the phase-switch hold, with generation and phase on the wire", async () => {
    const r = await answer((_q, res) => json(res, 200, { run: { id: "run-1", status: "paused", hold_reason: PHASE_SWITCH_HOLD_REASON } }));
    assert.deepEqual(r.outcome, { outcome: "held", status: "paused" });
    assert.equal(r.paths[0], "/api/worker/runs/run-1/phase-switch-hold");
    assert.deepEqual(JSON.parse(r.bodies[0]!), { head: "a".repeat(40), claim_generation: 7, phase: "implement" });
  });
  it("an absent head is omitted from the body", async () => {
    const r = await answer((_q, res) => json(res, 200, { run: { status: "paused", hold_reason: PHASE_SWITCH_HOLD_REASON } }), { claimGeneration: 3, phase: "plan" });
    assert.deepEqual(JSON.parse(r.bodies[0]!), { claim_generation: 3, phase: "plan" });
  });
  it("a 200 for some other paused state is not a confirmation", async () => {
    const r = await answer((_q, res) => json(res, 200, { run: { status: "paused", hold_reason: "budget_exhausted" } }));
    assert.equal(r.outcome.outcome, "unknown");
  });
  it("a 409 is a refusal carrying the row's status, hold reason and the refusal reason", async () => {
    const r = await answer((_q, res) => json(res, 409, { run: { status: "awaiting_approval", hold_reason: null }, reason: "plan_not_approved" }));
    assert.deepEqual(r.outcome, { outcome: "refused", status: "awaiting_approval", holdReason: null, reason: "plan_not_approved" });
    const queued = await answer((_q, res) => json(res, 409, { run: { status: "queued", hold_reason: PHASE_SWITCH_HOLD_REASON } }));
    assert.deepEqual(queued.outcome, { outcome: "refused", status: "queued", holdReason: PHASE_SWITCH_HOLD_REASON });
  });
  for (const code of [404, 400, 500, 503]) {
    it(`an HTTP ${code} is unknown, never held`, async () => {
      const r = await answer((_q, res) => json(res, code, { error: "nope" }));
      assert.equal(r.outcome.outcome, "unknown");
      assert.equal(r.bodies.length, 1, "one attempt");
    });
  }
  it("a dropped connection is unknown, never thrown", async () => {
    const r = await answer((req) => { req.socket.destroy(); });
    assert.equal(r.outcome.outcome, "unknown");
  });
  it("no claim generation means no request at all", async () => {
    const c = new WorkerClient("http://127.0.0.1:9", "worker-join-token-0123456789", "0.1.0-test", nullLogger(), { sleep: async () => {} });
    assert.equal((await c.requestPhaseSwitchHold("run-1", { claimGeneration: 0, phase: "plan" })).outcome, "unknown");
  });
});

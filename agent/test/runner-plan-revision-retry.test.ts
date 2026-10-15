// M2 runner wiring only: typed transient hooks do not prove real provider exhaustion.
import assert from "node:assert/strict";
import { it } from "node:test";
import fs from "node:fs";
import path from "node:path";
import { RequestError } from "../src/client.js";
import { recordingLogger } from "./helpers.js";
import { PlanRejectedError, PlanRevisionReplacedError, type Executor, type RunContext } from "../src/executor.js";
import { TransientRecoveryError } from "../src/sdk-executor.js";
import type { PlanVerdict } from "../src/steering.js";
import { api, client, fakeGitlab, git, homeDir, installHarness, runner, runnerWith } from "./runner-harness.js";
import { feed, freshClaim, gates, holdGateAck, newApi, row, send, statuses, until } from "./gate-revision-harness.js";

installHarness();
const SOURCE = "# Source plan";
const REVISED = "# Revised for J";

function enable(): void {
  newApi();
  api.planRevisionRetries = true;
  client.protocolFeatures = [...client.protocolFeatures, "plan_revision_retry_v1"];
}

function barrier(): { promise: Promise<void>; release: () => void } {
  let release!: () => void;
  const promise = new Promise<void>(resolve => { release = resolve; });
  return { promise, release };
}

/** Mimics the human planning driver's hook/catch seam; all input routing remains real. */
class RevisionDriver implements Executor {
  readonly turns: number[] = [];
  replacement?: PlanVerdict;
  readonly publish = barrier();
  async run(ctx: RunContext): Promise<{ branch: string }> {
    let verdict = await ctx.gatePlan!(SOURCE);
    for (;;) {
      if (verdict.kind === "cancel") throw new Error("run cancelled");
      if (verdict.kind === "reject") throw new PlanRejectedError(verdict.reason);
      if (verdict.kind === "approve") return { branch: ctx.branch };
      await ctx.beforePlanRevisionTurn!();
      this.turns.push(verdict.inputId!);
      try {
        if (this.turns.length === 1) {
          await ctx.retryPlanRevisionTurn!(new TransientRecoveryError("inline transient", { providerOrigin: true }));
          // A returned retry allows the same turn to start again only through beforeTurn.
          await ctx.beforePlanRevisionTurn!();
          this.turns.push(verdict.inputId!);
        }
        await this.publish.promise;
        verdict = await ctx.gatePlan!(REVISED, undefined, undefined, verdict.inputId);
      } catch (error) {
        if (!(error instanceof PlanRevisionReplacedError)) throw error;
        this.replacement = error.verdict;
        verdict = error.verdict;
      }
    }
  }
}

it("expiry keeps the source live; ordinary J replaces I and its publication atomically retires both", async () => {
  enable();
  const claim = freshClaim();
  const id = claim.run_id;
  const driver = new RevisionDriver();
  const r = runner(driver, fakeGitlab().gitlab, undefined, { planRevisionRetryPolicy: { redrives: 0 } });
  const held = holdGateAck(id, REVISED);
  const done = r.execute(claim);
  try {
    assert.ok(await until(() => gates(id).length === 1));
    const source = api.gateOf(id);
    const [i] = send(id, row("revise_plan", "I"));
    assert.ok(await until(() => feed(id).some(text => text.includes("retry allowance exhausted"))));
    const approve = row("approve_plan");
    const followup = row("follow_up", "must not restart");
    send(id, approve, followup);
    assert.ok(await until(() => api.isAcked(id, approve.id) && api.isAcked(id, followup.id)));
    assert.deepEqual(driver.turns, [i!.id]);
    assert.deepEqual(api.gateOf(id), source);
    assert.deepEqual(gates(id).map(g => g.plan_md), [SOURCE]);
    assert.equal(api.isApplied(id, i!.id), false);
    assert.equal(api.isApplied(id, approve.id), false);
    assert.equal(api.isApplied(id, followup.id), false);

    const [j] = send(id, row("revise_plan", "J"));
    assert.ok(await until(() => driver.turns.length === 2));
    assert.deepEqual(driver.turns, [i!.id, j!.id]);
    assert.equal(driver.replacement?.kind, "revise");
    assert.equal(api.isApplied(id, i!.id), false, "I remains pending during J's turn");
    assert.equal(api.isApplied(id, j!.id), false);
    driver.publish.release();
    assert.ok(await until(() => gates(id).some(g => g.plan_md === REVISED)));
    const publication = gates(id).find(g => g.plan_md === REVISED)!;
    assert.equal(publication.expected_gate_revision, source.revision);
    assert.equal(publication.revision_input_id, j!.id);
    assert.deepEqual(publication.superseded_revision_input_ids, [i!.id]);
    assert.equal(api.isApplied(id, j!.id), true);
    assert.equal(api.isApplied(id, i!.id), true);
    assert.equal(api.isInputSuperseded(id, i!.id), true);
    assert.equal(api.isInputSuperseded(id, j!.id), false);
    held.release();
    assert.ok(await until(() => feed(id).some(text => text.includes("were superseded"))));
    send(id, row("cancel"));
    await done;
    assert.equal(api.inputReceiptCalls.some(c => c.kind === "applied" && c.ids.some(n => n === i!.id || n === j!.id)), false,
      "the camel-case state receipt retires local revision work without APPLIED");
    assert.equal(statuses(id).includes("recovery_wait"), false);
  } finally {
    held.release();
    driver.publish.release();
    r.shutdown();
    await done;
  }
});

it("an underclaimed cap cannot veto server-accepted J; one queued cancel ends the wait", async () => {
  enable();
  const claim = freshClaim(1, { config: { plan_max_revisions: 1 } });
  const id = claim.run_id;
  const driver = new RevisionDriver();
  const r = runner(driver, fakeGitlab().gitlab, undefined, { planRevisionRetryPolicy: { redrives: 0 } });
  const done = r.execute(claim);
  try {
    assert.ok(await until(() => gates(id).length === 1));
    const [i] = send(id, row("revise_plan", "I"));
    assert.ok(await until(() => feed(id).some(text => text.includes("retry allowance exhausted"))));
    const notice = feed(id).find(text => text.includes("retry allowance exhausted"))!;
    assert.match(notice, /Cancel and source-bound reject remain available/);
    assert.match(notice, /dispatch a new run/);
    const [j] = send(id, row("revise_plan", "server accepted J"));
    assert.ok(await until(() => driver.turns.length === 2));
    assert.deepEqual(driver.turns, [i!.id, j!.id]);
    assert.match(notice, /if the server's PLAN_MAX_REVISIONS permits/);
    const [cancel] = send(id, row("cancel"));
    driver.publish.release();
    await done;
    assert.ok(api.isAcked(id, cancel!.id));
    assert.equal(statuses(id).includes("completed"), false);
  } finally {
    driver.publish.release();
    r.shutdown();
    await done;
  }
});

for (const control of ["cancel", "reject", "paused", "generation", "source", "released", "worker"] as const) {
  it(`backoff suppresses redrive after ${control}`, async t => {
    enable();
    const claim = freshClaim();
    const id = claim.run_id;
    const driver = new RevisionDriver();
    const sleeping = barrier();
    const release = barrier();
    const r = runner(driver, fakeGitlab().gitlab, undefined, {
      planRevisionRetryPolicy: {
        redrives: 1, random: () => 0,
        sleep: async () => { sleeping.release(); await release.promise; },
      },
    });
    const done = r.execute(claim);
    try {
      assert.ok(await until(() => gates(id).length === 1));
      send(id, row("revise_plan", "I"));
      assert.ok(await until(() => driver.turns.length === 1));
      await sleeping.promise;
      if (control === "cancel" || control === "reject") {
        const [cancel] = send(id, row(control === "cancel" ? "cancel" : "reject_plan", "stop this source"));
        assert.ok(await until(() => api.isAcked(id, cancel!.id)));
      } else if (control === "paused") api.setOwnershipStatus(id, "paused", 1);
      else if (control === "generation") api.setInputClaimGeneration(id, 2);
      else if (control === "source") api.seedGate(id, { revision: 2, presentationId: "external", payload: { plan_md: "# external" } });
      else if (control === "released") t.mock.method(client, "getRunOwnership", async () => ({
        status: "awaiting_approval", claim_generation: 1, gate_revision: 1, claim_released: true,
      }));
      else t.mock.method(client, "getRunOwnership", async () => { throw new RequestError("GET", "/ownership", 404, "worker no longer owns run"); });
      release.release();
      await done;
      assert.equal(driver.turns.length, 1);
      assert.equal(gates(id).length, 1, "no revised publication or restoration overwrites the moved source");
      assert.equal(statuses(id).includes("recovery_wait"), false);
      assert.equal(statuses(id).includes("completed"), false);
      if (control === "reject") {
        const terminal = api.states.find(s => s.runId === id && s.body.status === "failed");
        assert.equal(terminal?.body.expected_gate_revision, 1, "backoff reject keeps its exact source fence");
      }
    } finally {
      release.release();
      driver.publish.release();
      r.shutdown();
      await done;
    }
  });
}

for (const outcome of ["stale source", "unknown", "capability loss"] as const) {
it(`a recovered rejection with ${outcome} preserves the clone and HOME`, async t => {
  enable();
  const claim = freshClaim();
  const id = claim.run_id;
  const ownedHome = path.join(homeDir, "source-rejection-home");
  fs.mkdirSync(ownedHome);
  const breadcrumb = path.join(ownedHome, "session-proof");
  fs.writeFileSync(breadcrumb, "session must survive a source refusal");
  const remove = t.mock.method(git, "removeRunnerClone");
  const { logger, lines } = recordingLogger();
  const executor: Executor = { run: async ctx => {
    const verdict = await ctx.gatePlan!(SOURCE);
    assert.equal(verdict.kind, "revise");
    await ctx.beforePlanRevisionTurn!();
    try {
      await ctx.retryPlanRevisionTurn!(new TransientRecoveryError("inline transient", { providerOrigin: true }));
    } catch (error) {
      assert.ok(error instanceof PlanRejectedError);
      assert.equal(error.sourceGateRevision, 1);
      if (outcome === "unknown") api.sendRawState(id, 409, JSON.stringify({ run: { status: "awaiting_approval" } }));
      else api.seedGate(id, { revision: 2, presentationId: "newer-presentation", payload: { plan_md: "# newer" } });
      if (outcome === "capability loss") client.clearFeatures();
      throw error;
    }
    throw new Error("expiry must not return for another provider turn");
  } };
  const r = runnerWith(() => ({ executor, homeDir: ownedHome }), fakeGitlab().gitlab, undefined, logger,
    { planRevisionRetryPolicy: { redrives: 0 } });
  const done = r.execute(claim);
  try {
    assert.ok(await until(() => gates(id).length === 1));
    const [i] = send(id, row("revise_plan", "I"));
    assert.ok(await until(() => feed(id).some(text => text.includes("retry allowance exhausted"))));
    const [reject] = send(id, row("reject_plan", "owner rejection of source 1"));
    await done;
    assert.equal(api.gateOf(id).revision, outcome === "unknown" ? 1 : 2);
    assert.equal(statuses(id).includes("failed"), false);
    assert.equal(api.isApplied(id, i!.id), false);
    assert.equal(api.isApplied(id, reject!.id), false);
    assert.equal(remove.mock.callCount(), 0, "source refusal grants no clone disposal authority");
    assert.equal(fs.existsSync(breadcrumb), true);
    assert.match(JSON.stringify(lines), outcome === "stale source"
      ? /recovered rejection refused because its source gate changed/
      : /source-fenced rejection outcome is unconfirmed/);
  } finally {
    r.shutdown();
    await done;
  }
});
}

it("an id-less resumed gate still refuses a human provider start on paused ownership", async () => {
  enable();
  const at = new Date(Date.now() - 1000).toISOString();
  const claim = freshClaim(2, { resume_phase: "awaiting_approval", plan_md: SOURCE, resume_plan_at: at,
    resume_gate_revision: 2, resume_gate_presentation_id: undefined });
  const id = claim.run_id;
  api.seedGate(id, { revision: 2, presentationId: null, payload: { plan_md: SOURCE } });
  let starts = 0;
  const executor: Executor = { resumesAtGate: true, run: async ctx => {
    const verdict = await ctx.takeResumedGateEvent!();
    assert.equal(verdict?.kind, "revise");
    api.setOwnershipStatus(id, "paused", 2);
    await ctx.beforePlanRevisionTurn!();
    starts++;
    return { branch: ctx.branch };
  } };
  const [i] = send(id, row("revise_plan", "I", { gate_binding: "bound", gate_revision: 2 }));
  await runner(executor, fakeGitlab().gitlab).execute(claim);
  assert.equal(starts, 0);
  assert.equal(statuses(id).includes("failed"), false);
  assert.equal(api.isApplied(id, i!.id), false);
});

it("a milestone pause during backoff remains a milestone pause", async () => {
  enable();
  const claim = freshClaim();
  const id = claim.run_id;
  const driver = new RevisionDriver();
  const sleeping = barrier();
  const release = barrier();
  const r = runner(driver, fakeGitlab().gitlab, undefined, { planRevisionRetryPolicy: {
    redrives: 1, random: () => 0, sleep: async () => { sleeping.release(); await release.promise; },
  } });
  const done = r.execute(claim);
  try {
    assert.ok(await until(() => gates(id).length === 1));
    send(id, row("revise_plan", "I"));
    await sleeping.promise;
    const [pause] = send(id, row("pause", JSON.stringify({ mode: "milestone" })));
    assert.ok(await until(() => api.isAcked(id, pause!.id)));
    release.release();
    assert.ok(await until(() => driver.turns.length === 2), "the next human provider turn remains allowed");
    assert.equal(statuses(id).includes("paused"), false);
    send(id, row("cancel"));
    driver.publish.release();
    await done;
  } finally {
    release.release(); driver.publish.release(); r.shutdown(); await done;
  }
});

import assert from "node:assert/strict";
import { test } from "node:test";
import path from "node:path";
import fs from "node:fs";
import { execFileSync } from "node:child_process";
import { Outbox } from "../src/outbox.js";
import { StaleClaimError } from "../src/runner.js";
import { PolicyHoldUnsupported } from "../src/client.js";
import { randomUUID } from "node:crypto";
import { ProviderPolicyRefusal, admitPolicyTurn } from "../src/provider-policy-refusal.js";
import type { Executor, RunContext } from "../src/executor.js";
import { nullLogger } from "./helpers.js";
import { installHarness, api, client, homeDir, git, gitlabClaim, fakeGitlab, runner, worktreeDirFor, fx } from "./runner-harness.js";

installHarness();

function refusal() {
  return new ProviderPolicyRefusal({ ...admitPolicyTurn("implement"), event: "provider_policy_refusal",
    provider: "codex", category: "policy_refusal", origin: "root", policy_tag: "cyberPolicy" });
}

test("missing preserved session rejects without executor work even with negotiation off", async () => {
  const { outbox, claim } = await setup();
  claim.policy_hold_resume = true;
  client.supportsPolicyHold = () => false;
  let calls = 0;
  const executor: Executor = { async run() { calls++; throw new Error("forbidden model"); } };
  await runner(executor, fakeGitlab().gitlab, undefined, { outbox }).execute(claim);
  assert.equal(calls, 0);
  assert.equal(api.policyHoldRequests.length, 1);
  const body = api.policyHoldRequests[0]!.body;
  assert.equal(body.detail, "resume_rejected");
  assert.equal(body.tag, undefined);
  assert.equal(body.correlation_id, undefined);
  assert.equal(api.states.some(s => s.body.status === "failed"), false);
});

for (const full of [false, true]) for (const fresh of [false, true]) for (const recovered of [false, true]) {
  test(`M2 Stop full=${full} fresh=${fresh} recovered=${recovered}`, async () => {
    const { outbox, claim } = await setup();
    claim.policy_hold_resume = true;
    claim.policy_hold_fresh_session = fresh;
    claim.session_id = fresh ? null : "missing-session";
    claim.plan_approved = true;
    claim.plan_md = "approved delivery";
    claim.issue_title = "Deliver approved milestones";
    claim.milestones = [{ id: "m1", title: "one" }, { id: "m2", title: "two" }];
    api.setStateAckScope(claim.run_id, { scope_ceiling: full ? 2 : 1,
      milestones_completed: full ? ["m1", "m2"] : ["m1"] });
    let recoveredHead: string | undefined;
    if (recovered) {
      const runGit = (...args: string[]) => execFileSync("git", ["-C", fx.originPath, ...args],
        { encoding: "utf8" }).trim();
      runGit("checkout", "-b", "agent/issue-3068");
      fs.writeFileSync(path.join(fx.originPath, "delivered.txt"), "completed milestone\n");
      runGit("add", "delivered.txt");
      runGit("-c", "user.name=fixture", "-c", "user.email=fixture@example.test", "commit", "-m", "completed work");
      recoveredHead = runGit("rev-parse", "HEAD");
      runGit("checkout", "main");
    }
    let calls = 0;
    const forge = fakeGitlab({ head: recoveredHead });
    const executor: Executor = { async run(ctx) {
      calls++;
      assert.equal(ctx.policyHoldResume, true);
      assert.equal(ctx.policyHoldFreshSession, fresh);
      assert.equal(fs.readFileSync(path.join(ctx.worktreePath, "delivered.txt"), "utf8"), "completed milestone\n");
      return { branch: ctx.branch, ...(full ? {} : { scopeCapped: { completedCount: 1, total: 2 } }) };
    } };
    await runner(executor, forge.gitlab, undefined, { outbox }).execute(claim);
    assert.equal(calls, recovered ? 1 : 0, "ordinary executor only receives recovered delivery");
    assert.equal(api.states.some(s => s.body.seeded_from_default), false, "Stop never clears completion");
    if (!recovered) {
      assert.equal(api.policyHoldRequests[0]?.body.detail, "resume_rejected");
      assert.equal(forge.calls.length, 0);
      assert.equal(api.states.some(s => s.body.status === "completed" || s.body.status === "failed"), false);
      assert.ok(JSON.stringify(api.messages(claim.run_id)).includes("completed work is unavailable"));
    } else {
      assert.equal(api.policyHoldRequests.length, 0);
      assert.ok(api.states.some(s => s.body.status === "completed"), JSON.stringify(api.states));
    }
  });
}

test("degraded capture retains the clone and retries ACK loss with the same outcome UUID", async () => {
  const { outbox, claim } = await setup();
  git.worktreeStatus = async () => null;
  api.policyHoldLoseReplies = 1;
  const executor: Executor = { async run() { throw refusal(); } };
  await runner(executor, fakeGitlab().gitlab, undefined, { outbox, recoveryRetryMs: 1 }).execute(claim);
  assert.equal(api.policyHoldRequests.length, 2);
  assert.deepEqual(api.policyHoldRequests[0]!.body, api.policyHoldRequests[1]!.body);
  assert.equal(api.policyHoldRequests[0]!.body.head, "");
  assert.equal(api.policyHoldRequests[0]!.body.published, false);
  assert.equal(fs.existsSync(worktreeDirFor(3068)), true, "degraded hold preserves the actual clone");
  assert.equal(api.states.some(s => s.body.status === "failed" || s.body.status === "completed"), false);
});

test("journal failure blocks sending and reuses the minted UUID after disk recovery", async () => {
  const { outbox, claim } = await setup();
  const journal = outbox.journalPolicy.bind(outbox);
  const ids: string[] = [];
  outbox.journalPolicy = async (runId, outcome) => {
    ids.push(outcome.outcome_id);
    assert.equal(api.policyHoldRequests.length, 0);
    if (ids.length === 1) return { journaled: false, reason: "not_durable" };
    return journal(runId, outcome);
  };
  let calls = 0;
  const executor: Executor = { async run() { calls++; throw refusal(); } };
  await runner(executor, fakeGitlab().gitlab, undefined, { outbox, recoveryRetryMs: 1 }).execute(claim);
  assert.equal(calls, 1);
  assert.equal(ids.length, 2);
  assert.equal(ids[0], ids[1]);
  assert.equal(api.policyHoldRequests[0]!.body.outcome_id, ids[0]);
});

test("summary cleanup failure preserves the admitted policy outcome", async () => {
  const { outbox, claim } = await setup();
  const executor: Executor = {
    async run() { throw refusal(); },
    async closeAndSettlePlanSummary() { throw new Error("cleanup failed"); },
  };
  await runner(executor, fakeGitlab().gitlab, undefined, { outbox }).execute(claim);
  assert.equal(api.policyHoldRequests[0]!.body.detail, "refused");
  assert.equal(api.policyHoldRequests[0]!.body.head, "");
  assert.equal(api.states.some(s => s.body.status === "failed"), false);
  assert.equal(fs.existsSync(worktreeDirFor(3068)), true);
});

for (const disposition of ["applied", "already_applied", "resolved_newer_policy", "pending", "terminal", "recorded", "unauthenticated_historical_outcome"] as const) {
  test(`older generation reconciliation: ${disposition}`, async () => {
    const { outbox, claim } = await setup();
    const outcome = { outcome_id: randomUUID(), source_generation: 1, detail: "resume_rejected" as const,
      head: "", published: false };
    assert.equal((await outbox.journalPolicy(claim.run_id, outcome)).journaled, true);
    client.reportPolicyHold = async (_id, body) => {
      api.policyHoldRequests.push({ runId: claim.run_id, body: { ...body } });
      return {
        disposition, outcome_id: body.outcome_id, claim_generation: body.claim_generation,
        status: disposition === "terminal" ? "cancelled" : disposition === "applied" ? "paused" : "running",
        ...(disposition === "applied" ? { hold_reason: "provider_policy_refusal" } : {}),
      };
    };
    let calls = 0;
    const executor: Executor = { async run() { calls++; throw new Error("test execution reached"); } };
    const forge = fakeGitlab();
    await runner(executor, forge.gitlab, undefined, { outbox }).execute(claim);
    assert.equal(calls, disposition === "already_applied" || disposition === "resolved_newer_policy" ? 1 : 0);
    assert.equal(api.policyHoldRequests.length, 1);
    assert.equal(api.policyHoldRequests[0]!.body.outcome_id, outcome.outcome_id);
    assert.equal(api.policyHoldRequests[0]!.body.source_generation, 1);
    assert.equal(api.policyHoldRequests[0]!.body.claim_generation, 2);
    assert.equal(forge.calls.length, 0);
    assert.equal(outbox.hasPendingPolicy(claim.run_id), true, "physical retirement belongs to custody proof");
    if (calls === 0) assert.equal(api.states.some(s => s.body.status === "failed"), false);
    if (disposition === "unauthenticated_historical_outcome") {
      assert.equal(outbox.listPendingPolicies()[0]!.server_settled, undefined);
    }

  });
}

// Probe the admission boundary directly so a blocked claim never needs an unbounded execute().
async function policyReconciliationProbe() {
  const { outbox, claim } = await setup();
  claim.claim_generation = 3;
  const outcome = { outcome_id: randomUUID(), source_generation: 1,
    detail: "resume_rejected" as const, head: "", published: false };
  assert.equal((await outbox.journalPolicy(claim.run_id, outcome)).journaled, true);
  let cancelled = false, fenced = false, closed = 0;
  const active = { shuttingDown: false };
  const flight = {
    runId: claim.run_id, claimGeneration: 3, runLog: nullLogger(),
    cancel: new AbortController(), active,
    steering: { isCancelled: () => cancelled, claimFence: () => fenced },
    batcher: { async close() { closed++; } },
    policyReconciliationPending: false, preserveRecoveryClone: false, preserveSession: false,
  };
  const forge = fakeGitlab();
  const subject = runner({ async run() { assert.fail("forbidden model work"); } },
    forge.gitlab, undefined, { outbox, recoveryRetryMs: 1 }) as unknown as {
      reconcileClaimPolicies(flight: unknown): Promise<boolean>;
      waitRecoveryRetry(flight: unknown, cancelStopsWait: boolean, waitMs: number): Promise<void>;
    };
  const unknownFile = path.join(homeDir, "outbox", claim.run_id, "policy-2.json");
  return { outbox, claim, outcome, flight, active, subject, forge,
    addUnknown: () => fs.writeFileSync(unknownFile, "unreadable"),
    cancel: () => { cancelled = true; },
    fence: () => { fenced = true; },
    assertBlocked() {
      assert.equal(flight.policyReconciliationPending, true);
      assert.equal(flight.preserveRecoveryClone, true);
      assert.equal(flight.preserveSession, true);
      assert.equal(flight.active, active);
      assert.equal(closed, 0, "blocked admission must retain its open active flight");
      assert.equal(forge.calls.length, 0);
      assert.equal(api.states.length, 0);
      assert.equal(fs.readFileSync(unknownFile, "utf8"), "unreadable");
    },
  };
}

for (const disposition of ["already_applied", "resolved_newer_policy"] as const) {
  for (const timing of ["existing", "cached ACK", "during ACK"] as const) {
    test(`mixed policy inventory blocks ${disposition} with unknown sibling ${timing}`, async (t) => {
      const probe = await policyReconciliationProbe();
      const { subject, flight, outbox, claim, outcome } = probe;
      api.policyHoldDisposition = disposition;
      if (timing === "cached ACK") {
        assert.equal(await subject.reconcileClaimPolicies(flight), false, "clean settled evidence permits admission");
        assert.equal(outbox.hasUnresolvedPolicy(claim.run_id), false);
      }
      if (timing === "during ACK") {
        const report = client.reportPolicyHold.bind(client);
        t.mock.method(client, "reportPolicyHold", async (...args: Parameters<typeof report>) => {
          const ack = await report(...args);
          probe.addUnknown();
          return ack;
        });
      } else probe.addUnknown();
      const blocked = new Error("observed delayed blocked wait");
      let waits = 0;
      subject.waitRecoveryRetry = async (waiting, cancelStopsWait, waitMs) => {
        waits++;
        assert.equal(waiting, flight);
        assert.equal(cancelStopsWait, true);
        assert.ok(waitMs >= 250);
        probe.assertBlocked();
        assert.equal(outbox.hasUnresolvedPolicy(claim.run_id), true);
        throw blocked;
      };
      await assert.rejects(subject.reconcileClaimPolicies(flight), error => error === blocked);
      assert.equal(waits, 1);
      assert.equal(api.policyHoldRequests.length, 1, "unknown evidence must not fabricate a hold");
      assert.equal(api.policyHoldRequests[0]!.body.outcome_id, outcome.outcome_id);
      assert.equal(api.policyHoldRequests[0]!.body.source_generation, 1);
      assert.equal(api.policyHoldRequests[0]!.body.claim_generation, 3);
      assert.equal(outbox.listPendingPolicies(claim.run_id)[0]!.server_settled, true);
    });
  }
}

for (const control of ["Cancel", "fence", "shutdown"] as const) {
  test(`mixed policy blocked wait observes ${control}`, async () => {
    const probe = await policyReconciliationProbe();
    api.policyHoldDisposition = "already_applied";
    probe.addUnknown();
    let waits = 0;
    const shutdown = new Error("worker shutdown");
    probe.subject.waitRecoveryRetry = async () => {
      assert.equal(++waits, 1, "control must stop the next retry");
      probe.assertBlocked();
      if (control === "Cancel") probe.cancel();
      else if (control === "fence") probe.fence();
      else {
        probe.active.shuttingDown = true;
        probe.flight.cancel.abort(shutdown);
      }
    };
    await assert.rejects(probe.subject.reconcileClaimPolicies(probe.flight), error =>
      control === "fence" ? error instanceof StaleClaimError :
        control === "shutdown" ? error === shutdown :
          error instanceof Error && error.message === "run cancelled");
    assert.equal(waits, 1);
    probe.assertBlocked();
  });
}

async function setup() {
  const outbox = new Outbox({ root: path.join(homeDir, "outbox"), log: nullLogger(),
    runMaxBytes: 64 << 20, maxBytes: 128 << 20, retentionMs: 86400000 });
  await outbox.init();
  client.supportsPolicyHold = () => true;
  const claim = gitlabClaim(3068, { claim_generation: 2,
    kind: "issue", interactive: false,
    secrets: { forge_pat: "fixture-forge-pat-000000", codex: { auth_mode: "api_key", access_token: "fixture-key", capability: "fixture-capability" } } });
  return { outbox, claim };
}

test("an already admitted root outcome survives a feature-cache loss", async () => {
  const { outbox, claim } = await setup();
  const executor: Executor = { async run(ctx) {
    assert.equal(ctx.policyHoldSupported, true);
    client.supportsPolicyHold = () => false;
    throw refusal();
  } };
  await runner(executor, fakeGitlab().gitlab, undefined, { outbox }).execute(claim);
  assert.equal(api.policyHoldRequests.length, 1);
  assert.equal(api.states.some(s => s.body.status === "failed"), false);
});

test("missing kind is not admitted by a legacy default", async () => {
  const { outbox, claim } = await setup();
  claim.kind = undefined;
  let supported: boolean | undefined;
  const executor: Executor = { async run(ctx) { supported = ctx.policyHoldSupported; throw refusal(); } };
  await runner(executor, fakeGitlab().gitlab, undefined, { outbox }).execute(claim);
  assert.equal(supported, false);
  assert.equal(api.policyHoldRequests.length, 0);
  assert.equal(api.states.some(s => s.body.status === "failed"), true);
});

test("unsupported hold fallback settles delivery only after its real terminal ACK", async () => {
  const { outbox, claim } = await setup();
  client.reportPolicyHold = async () => { throw new PolicyHoldUnsupported(); };
  const executor: Executor = { async run() { throw refusal(); } };
  const forge = fakeGitlab();
  await runner(executor, forge.gitlab, undefined, { outbox }).execute(claim);
  assert.equal(api.states.some(s => s.body.status === "failed"), true);
  assert.equal(outbox.listPendingPolicies(claim.run_id)[0]?.server_settled, true);
  assert.equal(outbox.hasPendingPolicy(claim.run_id), true, "terminal ACK is not source deletion authority");
  assert.equal(forge.calls.length, 0);
});

test("root refusal journals a flat hold and parks without terminal failure or MR", async () => {
  const { outbox, claim } = await setup();
  client.publishCheckpoint = async (_runId, _tip, pack) => {
    for await (const chunk of pack) void chunk;
    return { ok: true, body: { published: true, ref: "refs/uzi-checkpoints/agent/issue-3068" } };
  };
  let calls = 0;
  const executor: Executor = { async run(ctx: RunContext) {
    calls++;
    assert.equal(ctx.policyHoldSupported, true);
    throw new ProviderPolicyRefusal({ ...admitPolicyTurn("implement"), event: "provider_policy_refusal",
      provider: "codex", category: "policy_refusal", origin: "root", policy_tag: "cyberPolicy" });
  } };
  const forge = fakeGitlab();
  await runner(executor, forge.gitlab, undefined, { outbox }).execute(claim);
  assert.equal(calls, 1);
  assert.equal(api.policyHoldRequests.length, 1);
  assert.equal(api.policyHoldRequests[0]!.body.detail, "refused");
  assert.equal(api.policyHoldRequests[0]!.body.tag, "cyberPolicy");
  assert.equal(forge.calls.length, 0);
  assert.equal(api.states.some(s => s.body.status === "failed" || s.body.status === "completed"), false);
  assert.match(String(api.policyHoldRequests[0]!.body.head), /^[0-9a-f]{40}$/);
  assert.equal(api.policyHoldRequests[0]!.body.published, true);
  assert.equal(outbox.listPendingPolicies(claim.run_id)[0]?.server_settled, true);
});

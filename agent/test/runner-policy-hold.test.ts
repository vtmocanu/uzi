import assert from "node:assert/strict";
import { test } from "node:test";
import path from "node:path";
import fs from "node:fs";
import { execFileSync } from "node:child_process";
import { CodexSessionStore } from "../src/codex/session-state.js";
import { Outbox } from "../src/outbox.js";
import { RunRunner, StaleClaimError } from "../src/runner.js";
import { PolicyHoldUnsupported } from "../src/client.js";
import { randomUUID } from "node:crypto";
import { ProviderPolicyRefusal, admitPolicyTurn } from "../src/provider-policy-refusal.js";
import type { Executor, RunContext } from "../src/executor.js";
import { nullLogger } from "./helpers.js";
import { installHarness, api, client, homeDir, git, TOKEN, gitlabClaim, fakeGitlab, runner, runnerWith, worktreeDirFor, fx } from "./runner-harness.js";

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

test("M2 required Stop callback rejects a failed ACK before continuing", async (t) => {
  const { outbox, claim } = await setup();
  Object.assign(claim, { policy_hold_resume: true, policy_hold_fresh_session: true,
    session_id: null, plan_approved: true, plan_md: "approved delivery",
    milestones: [{ id: "m1", title: "one" }, { id: "m2", title: "two" }] });
  api.setStateAckScope(claim.run_id, { scope_ceiling: 1, milestones_completed: ["m1"] });
  const runGit = (...args: string[]) => execFileSync("git", ["-C", fx.originPath, ...args], { encoding: "utf8" }).trim();
  runGit("checkout", "-b", "agent/issue-3068");
  fs.writeFileSync(path.join(fx.originPath, "delivered.txt"), "completed milestone\n");
  runGit("add", "delivered.txt");
  runGit("-c", "user.name=fixture", "-c", "user.email=fixture@example.test", "commit", "-m", "completed work");
  const head = runGit("rev-parse", "HEAD");
  runGit("checkout", "main");
  const report = client.reportState.bind(client);
  let boundaries = 0;
  t.mock.method(client, "reportState", async (...args: Parameters<typeof report>) => {
    if (args[1].iteration_count !== undefined) {
      boundaries++;
      throw new Error("iteration transport unavailable");
    }
    return report(...args);
  });
  let crossedBoundary = false;
  const executor: Executor = { async run(ctx) {
    await ctx.reportIteration?.(1);
    crossedBoundary = true;
    throw refusal();
  } };
  await runner(executor, fakeGitlab({ head }).gitlab, undefined, { outbox }).execute(claim);
  assert.equal(boundaries, 1);
  assert.equal(crossedBoundary, false);
  assert.equal(api.policyHoldRequests[0]?.body.detail, "resume_rejected");
  assert.equal(api.states.some(s => s.body.status === "completed" || s.body.status === "failed"), false);
});

for (const fresh of [false, true]) {
  test(`M2 constrained lost tree below Stop fresh=${fresh}`, async (t) => {
    const { outbox, claim } = await setup();
    Object.assign(claim, { policy_hold_resume: true, policy_hold_fresh_session: fresh,
      session_id: fresh ? null : "usable-session", plan_approved: true, plan_md: "approved delivery",
      milestones: [{ id: "m1", title: "one" }, { id: "m2", title: "two" }] });
    // A usable transcript does not recover completed delivery files.
    t.mock.method(CodexSessionStore, "inspectSession", async () => "present" as const);
    api.setStateAckScope(claim.run_id, { scope_ceiling: 2, milestones_completed: ["m1"] });
    let calls = 0;
    const forge = fakeGitlab();
    await runnerWith(() => ({ executor: { async run() { calls++; throw new Error("forbidden model"); } }, homeDir }),
      forge.gitlab, undefined, nullLogger(), { outbox }).execute(claim);
    assert.equal(calls, 0);
    assert.equal(api.states.some(s => s.body.seeded_from_default), false);
    assert.equal(api.policyHoldRequests[0]?.body.detail, "resume_rejected");
    assert.equal(forge.calls.length, 0);
    assert.equal(api.states.some(s => s.body.status === "completed" || s.body.status === "failed"), false);
    assert.equal(fs.existsSync(worktreeDirFor(3068)), true, "hold retains custody");
  });
}

test("M2 constrained recovered WIP keeps completed scope", async (t) => {
  const { outbox, claim } = await setup();
  Object.assign(claim, { policy_hold_resume: true, policy_hold_fresh_session: true,
    session_id: null, plan_approved: true, plan_md: "approved delivery",
    milestones: [{ id: "m1", title: "one" }, { id: "m2", title: "two" }] });
  api.setStateAckScope(claim.run_id, { scope_ceiling: 2, milestones_completed: ["m1"] });
  const clone = git.runnerCloneForBranch.bind(git);
  t.mock.method(git, "runnerCloneForBranch", async (...args: Parameters<typeof clone>) => {
    const recovered = await clone(...args);
    assert.equal(recovered.seededFrom, "default");
    fs.writeFileSync(path.join(recovered.path, "delivered.txt"), "recovered WIP\n");
    return { ...recovered, wipRecovered: true };
  });
  let calls = 0;
  const executor: Executor = { async run(ctx) {
    calls++;
    assert.equal(fs.readFileSync(path.join(ctx.worktreePath, "delivered.txt"), "utf8"), "recovered WIP\n");
    throw refusal();
  } };
  await runner(executor, fakeGitlab().gitlab, undefined, { outbox }).execute(claim);
  assert.equal(calls, 1);
  assert.equal(api.states.some(s => s.body.seeded_from_default), false);
  assert.equal(api.policyHoldRequests[0]?.body.detail, "refused");
});

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

// M1 observes policy-catch settlement ENTRY only. The intercepted method returns without
// inventory freeze, archive upload, backend custody ACK or physical source retirement.
// Real outbox/recovery tests own those deeper proofs; this fixture supplies no cleanup authority.
type PolicyHoldBoundaries = {
  enterPolicyHold: RunRunner["enterPolicyHold"];
  quiesceRun: RunRunner["quiesceRun"];
  captureHoldContext: RunRunner["captureHoldContext"];
  pinRecoveryGeneration: RunRunner["pinRecoveryGeneration"];
  retireFinalizeRecord: RunRunner["retireFinalizeRecord"];
  settleGuardedInventory: RunRunner["settleGuardedInventory"];
  waitRecoveryRetry: RunRunner["waitRecoveryRetry"];
};

const m1Cases = [
  { name: "verified published guarded capture", published: true },
  { name: "verified local-only guarded capture" },
  { name: "unverified head", noHead: true },
  { name: "blocked quiescence", blocked: true },
  { name: "pending ACK", disposition: "pending" },
  { name: "unknown ACK after real HTTP", unknownAck: true },
  { name: "unguarded claim", unguarded: true },
  { name: "already_applied preserves owner action", disposition: "already_applied" },
  { name: "resolved_newer_policy preserves owner action", disposition: "resolved_newer_policy" },
] as const;

for (const variant of m1Cases) {
  test(`M1 policy catch intercepted settlement entry: ${variant.name}`, async (t) => {
    const options: {
      name: string; published?: boolean; noHead?: boolean; blocked?: boolean; unknownAck?: boolean;
      unguarded?: boolean; disposition?: "pending" | "already_applied" | "resolved_newer_policy";
    } = variant;
    const { outbox, claim } = await setup();
    claim.inventory_guarded = !options.unguarded;
    api.policyHoldDisposition = options.disposition ?? "applied";
    const forge = fakeGitlab();
    const sessionId = "a839f482-b752-42e3-9517-71346f3a891c";
    const markerBytes = Buffer.from("M1 retained HOME and transcript bytes\n");
    const homeMarker = path.join(homeDir, "m1-home-marker");
    const sessionMarker = path.join(homeDir, "sessions", `rollout-${sessionId}.jsonl`);
    fs.mkdirSync(path.dirname(sessionMarker), { recursive: true });
    fs.writeFileSync(homeMarker, markerBytes);
    fs.writeFileSync(sessionMarker, markerBytes);
    const storePath = path.join(homeDir, "codex-session-store");
    await CodexSessionStore.persist(homeDir, storePath);
    assert.equal(await CodexSessionStore.inspectSession(storePath, sessionId), "present");
    const storeGeneration = fs.readFileSync(path.join(storePath, "current"), "utf8").trim();
    const storedMarker = path.join(storePath, "generations", storeGeneration, "sessions", path.basename(sessionMarker));
    assert.deepEqual(fs.readFileSync(storedMarker), markerBytes);
    const trace: string[] = [];
    let flight: Parameters<PolicyHoldBoundaries["enterPolicyHold"]>[1] | undefined;
    let clonePath = "";
    let calls = 0, settlements = 0, retries = 0, sends = 0;
    let settlementArgs: Parameters<PolicyHoldBoundaries["settleGuardedInventory"]> | undefined;
    let callbackError: unknown;
    let journalBytes: Buffer | undefined;
    let finalizeBytes: Buffer | undefined;
    let pinBytes: Buffer | undefined;
    let pinPath: string | undefined;
    const policyPath = path.join(homeDir, "outbox", claim.run_id, "policy-2.json");
    const finalizePath = path.join(homeDir, "outbox", claim.run_id, "finalize-2.json");
    const flags = () => {
      assert.ok(flight, "production execute reached its policy catch");
      assert.equal(flight.preserveRecoveryClone, true);
      assert.equal(flight.preserveSession, true);
    };
    const boundary = (name: string) => { flags(); trace.push(name); };
    const localHead = () => execFileSync("git", ["-C", clonePath, "rev-parse", "HEAD"],
      { encoding: "utf8" }).trim();
    const executor: Executor = {
      async run(ctx) {
        calls++;
        clonePath = ctx.worktreePath;
        ctx.onSessionId?.(sessionId);
        fs.writeFileSync(path.join(clonePath, "m1-work.txt"), markerBytes);
        assert.equal((await outbox.journalFinalize(claim.run_id, 2)).written, true);
        finalizeBytes = fs.readFileSync(finalizePath);
        throw refusal();
      },
      async closeAndSettlePlanSummary() {
        if (flight) { flags(); if (!trace.includes("summary")) trace.push("summary"); }
      },
      killAgentTree() {
        // quiesceRun may reap again; assert every reap, record the initial explicit kill once.
        if (flight) { flags(); if (!trace.includes("kill")) trace.push("kill"); }
      },
    };
    const subject = runnerWith(() => ({ executor, homeDir }), forge.gitlab, TOKEN,
      nullLogger(), { outbox });
    const observed = subject as unknown as PolicyHoldBoundaries;
    const enter = observed.enterPolicyHold.bind(subject);
    t.mock.method(observed, "enterPolicyHold", async (...args: Parameters<typeof enter>) => {
      assert.equal(args[0], claim);
      flight = args[1];
      const close = flight.batcher.close.bind(flight.batcher);
      t.mock.method(flight.batcher, "close", async () => {
        flags();
        if (!trace.includes("close")) trace.push("close");
        return close();
      });
      return enter(...args);
    });
    const quiesce = observed.quiesceRun.bind(subject);
    t.mock.method(observed, "quiesceRun", async (...args: Parameters<typeof quiesce>) => {
      if (flight) flags();
      if (args[2].site === "wall_park") {
        boundary("quiesce");
        if (options.blocked) return {
          blocked: true,
          outcome: {
            process: { state: "unverified" as const, processes: [], killed: [], detail: "M1 negative proof" },
            docker: { state: "not_wired" as const, removed: [], detail: "M1 negative proof" },
          },
        };
      }
      return quiesce(...args);
    });
    const capture = observed.captureHoldContext.bind(subject);
    t.mock.method(observed, "captureHoldContext", async (...args: Parameters<typeof capture>) => {
      boundary("capture");
      assert.ok(args[0] === claim, "capture uses the exact claim");
      assert.ok(args[1] === flight, "capture uses the exact flight");
      if (options.noHead) return {
        verified: false, published: false, mode: "same_worker_only" as const, head: localHead(),
      };
      const result = await capture(...args);
      assert.equal(result.verified, true);
      assert.equal(result.head, localHead(), "capture equals the actual clone HEAD");
      assert.equal(result.published, options.published === true);
      return result;
    });
    if (options.published) {
      // FakeApi has no checkpoint publisher. Only its publish response is supplied here;
      // capture/verification, Git pack creation, generation pin and policy HTTP remain real.
      t.mock.method(client, "publishCheckpoint", async (...args: Parameters<typeof client.publishCheckpoint>) => {
        const tip = args[1], pack = args[2];
        flags();
        assert.equal(tip, localHead());
        let bytes = 0;
        for await (const chunk of pack) bytes += chunk.length;
        assert.ok(bytes > 0, "actual checkpoint pack was consumed");
        return { ok: true, body: { published: true, ref: "refs/uzi-checkpoints/agent/issue-3068" } };
      });
    }
    const pin = observed.pinRecoveryGeneration.bind(subject);
    t.mock.method(observed, "pinRecoveryGeneration", async (...args: Parameters<typeof pin>) => {
      if (!flight) return pin(...args); // ordinary start-tip evidence stays real
      try {
        boundary("pin");
        assert.ok(args[0] === claim, "pin uses the exact claim");
        assert.ok(args[1] === flight, "pin uses the exact flight");
        assert.equal(args[2], options.noHead || options.blocked ? null : localHead());
        const record = await pin(...args);
        if (args[2] && options.unguarded) {
          assert.ok(record, "real legacy generation pin returned a record");
          assert.equal(record.sourceSha, localHead());
          assert.equal(record.generation, 2);
          assert.equal(record.inventoryGuarded, claim.inventory_guarded);
          pinPath = path.join(git.recoveryRoot, claim.run_id, `${record.captureId}.json`);
          pinBytes = fs.readFileSync(pinPath);
          assert.equal(JSON.parse(pinBytes.toString()).sourceSha, localHead());
        } else {
          // FakeApi supplies no exact open guarded hold. Preserve that real refusal rather
          // than fabricate backend custody authority to obtain a guarded journal.
          assert.equal(record, undefined);
        }
        return record;
      } catch (error) {
        callbackError ??= error;
        flight.steering.markCancelled();
        flight.cancel.abort(error);
        throw error;
      }
    });
    const journal = outbox.journalPolicy.bind(outbox);
    t.mock.method(outbox, "journalPolicy", async (...args: Parameters<typeof journal>) => {
      try {
        boundary("journal");
        assert.equal(sends, 0);
        assert.equal(Object.isFrozen(args[1]), true);
        assert.equal(args[1].source_generation, 2);
        assert.equal(args[1].session_id, sessionId);
        const result = await journal(...args);
        assert.equal(result.journaled, true);
        journalBytes = fs.readFileSync(policyPath);
        assert.deepEqual(JSON.parse(journalBytes.toString()).outcome, args[1]);
        return result;
      } catch (error) {
        callbackError ??= error;
        if (flight) {
          // Stop only this already-failed fixture; no successful scenario supplies an owner action.
          flight.steering.markCancelled();
          flight.cancel.abort(error);
        }
        throw error;
      }
    });
    const report = client.reportPolicyHold.bind(client);
    t.mock.method(client, "reportPolicyHold", async (...args: Parameters<typeof report>) => {
      boundary("send");
      sends++;
      assert.ok(journalBytes, "durable journal precedes first HTTP send");
      const authenticated = await outbox.readPolicyForRetirement(claim.run_id, 2);
      assert.ok(authenticated?.context.expectedIdentity, "real journal authentication succeeded");
      assert.deepEqual(authenticated.request, args[1]);
      assert.deepEqual(authenticated.policy.outcome, JSON.parse(journalBytes.toString()).outcome);
      assert.equal(args[1].claim_generation, 2);
      assert.equal(args[1].source_generation, 2);
      assert.equal(args[1].session_id, sessionId);
      const ack = await report(...args);
      boundary("ACK");
      if (options.unknownAck) throw new Error("M1 reply unavailable after actual HTTP");
      return ack;
    });
    const retire = observed.retireFinalizeRecord.bind(subject);
    t.mock.method(observed, "retireFinalizeRecord", async (...args: Parameters<typeof retire>) => {
      try {
        boundary("finalize");
        assert.ok(args[0] === flight, "retirement uses the exact flight");
        assert.equal(args[1], "policy_hold");
        assert.equal(outbox.listPendingPolicies(claim.run_id)[0]?.server_settled, true);
        assert.deepEqual(fs.readFileSync(finalizePath), finalizeBytes);
        await retire(...args);
        // Real legacy journals retain their existing cleanup authority; guarded journals do not.
        if (options.unguarded) assert.equal(fs.existsSync(finalizePath), false,
          "existing legacy authority permits finalize retirement after the policy ACK");
        else assert.deepEqual(fs.readFileSync(finalizePath), finalizeBytes,
          "guarded finalize record remains without independent retirement authority");
      } catch (error) {
        callbackError ??= error;
        if (flight) {
          // Stop only this already-failed fixture; no successful scenario supplies an owner action.
          flight.steering.markCancelled();
          flight.cancel.abort(error);
        }
        throw error;
      }
    });
    t.mock.method(observed, "settleGuardedInventory", async (...args: Parameters<PolicyHoldBoundaries["settleGuardedInventory"]>) => {
      boundary("settle");
      settlements++;
      settlementArgs = args; // Assert outside the production catch after execute settles.
    });
    // Policy capture and delivery retries are finite in enterPolicyHold (three/two).
    // This barrier yields immediately; it does not poll readiness or alter production timers.
    t.mock.method(observed, "waitRecoveryRetry", async () => { flags(); retries++; });
    try { await subject.execute(claim); }
    finally { assert.ifError(callbackError); }

    const unresolved = options.disposition === "pending" || options.unknownAck;
    const shouldSettle = !options.noHead && !options.blocked && !unresolved &&
      !options.unguarded && !options.disposition;
    assert.equal(calls, 1, "no executor retry");
    assert.equal(settlements, shouldSettle ? 1 : 0);
    if (shouldSettle) {
      assert.ok(settlementArgs, "settlement arguments were observed");
      assert.equal(settlementArgs.length, 6);
      assert.ok(settlementArgs[0] === claim, "settlement uses the exact claim");
      assert.ok(settlementArgs[1] === flight, "settlement uses the exact flight");
      assert.deepEqual(settlementArgs.slice(2), [undefined, undefined, undefined, true]);
    } else assert.equal(settlementArgs, undefined);
    assert.equal(sends, unresolved ? 2 : 1);
    assert.equal(retries, options.noHead ? 2 : unresolved ? 1 : 0);
    assert.deepEqual(trace, [
      "summary", "kill", "quiesce",
      ...(options.blocked ? [] : Array(options.noHead ? 3 : 1).fill("capture")),
      "pin", "journal", "send", "ACK",
      ...(unresolved ? ["send", "ACK"] : ["finalize"]),
      ...(shouldSettle ? ["settle"] : []),
      "close",
    ]);
    assert.equal(api.policyHoldRequests.length, sends);
    const body = api.policyHoldRequests[0]!.body;
    assert.equal(body.head, options.noHead || options.blocked ? "" : localHead());
    assert.equal(body.published, options.published === true);
    assert.equal(body.checkpoint_contains_latest, options.published ? true : undefined);
    assert.equal(body.session_id, sessionId);
    assert.equal(body.source_generation, 2);
    for (const request of api.policyHoldRequests) assert.deepEqual(request.body, body);
    const pending = outbox.listPendingPolicies(claim.run_id)[0]!;
    assert.deepEqual(pending.outcome, JSON.parse(journalBytes!.toString()).outcome,
      "delivery settlement does not rewrite immutable outcome identity");
    assert.equal(pending.server_settled === true, !unresolved);
    if (options.unguarded) assert.equal(fs.existsSync(finalizePath), false);
    else assert.deepEqual(fs.readFileSync(finalizePath), finalizeBytes);
    if (pinPath) assert.deepEqual(fs.readFileSync(pinPath), pinBytes);
    assert.deepEqual(fs.readFileSync(homeMarker), markerBytes);
    assert.deepEqual(fs.readFileSync(sessionMarker), markerBytes);
    assert.deepEqual(fs.readFileSync(storedMarker), markerBytes);
    assert.equal(await CodexSessionStore.inspectSession(storePath, sessionId), "present");
    assert.deepEqual(fs.readFileSync(path.join(clonePath, "m1-work.txt")), markerBytes);
    flags();
    assert.equal(forge.calls.length, 0, "no MR publication");
    assert.equal(api.states.some(s => s.body.status === "failed" || s.body.status === "completed"), false);
  });
}

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

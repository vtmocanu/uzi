// PRD #3708 U2: the gated plan cross-check agent path. A human-gated run (auto_approve false,
// plan_cross_check_gated true) is checked BEFORE its first human gate; every checker outcome,
// APPROVE included, then opens the ordinary human gate. These drive the real runner gate
// (ctx.gatePlan) against the fake api; nothing here mints an approval from a checker verdict.
import { beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { createHash, randomUUID } from "node:crypto";
import { PlanRejectedError, type Executor, type ExecutorResult, type RunContext } from "../src/executor.js";
import { MessageBatcher } from "../src/batcher.js";
import type { ExecutorFactory } from "../src/runner.js";
import { LimitReachedError } from "../src/limit.js";
import { CI_CONFIG_MARKER } from "../src/prompt.js";
import type { ClaimResponse, StateRequest } from "../src/protocol.js";
import type { PlanVerdict } from "../src/steering.js";
import type { PlanCrossCheckStateRequest } from "../src/client.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, installHarness, runner, runnerWith } from "./runner-harness.js";
import { feed, freshClaim, gates, newApi, row, send, until } from "./gate-revision-harness.js";
import { enrollment, proposal, boundedTransport, hash } from "./semantic-coverage-fixture.js";
import { semanticServer } from "./semantic-server-fixture.js";

installHarness();
beforeEach(() => {
  newApi();
  api.checkedTransport = true;
  api.usageHandler = () => ({ status: 200, body: {} });
});

const PLAN = "# PLAN\n- local proposal";
const NORMALIZED = "# PLAN\n- server normalized proposal";
const digest = "b".repeat(64);
const child = "22222222-2222-4222-8222-222222222222";
const timing = { preparationMs: 100, backoffMs: [1, 2] as const, requestMs: 100, pollMs: 5 };
// Runner paths that reach native planning capture walk Linux /proc; Linux CI runs them.
const LINUX_CAPTURE = process.platform !== "linux" && "Linux /proc capture required";
const PASS_LINE = /^plan cross-check: passed after (\d+) rounds?$/;

function gatedClaim(generation = 1, overrides: Partial<ClaimResponse> = {}): ClaimResponse {
  return freshClaim(generation, { kind: "issue", auto_approve: false, plan_cross_check_gated: true, ...overrides });
}
function proof(runId: string, generation = 1) {
  return { lead_last_seq: Math.max(0, ...api.messages(runId).map((m) => m.seq)),
    claim_generation: generation, plan_cross_check_settled: true, gate_revision: 0,
    current_plan_sha256: createHash("sha256").update("").digest("hex") };
}
function canonicalOf(body: Record<string, unknown>, planMd = NORMALIZED) {
  return { plan_md: planMd, milestones: [{ title: "canonical", done: false }],
    required_capabilities: [], required_tools: [], size_class: "s",
    base_commit: body.base_commit, planning_diff: body.planning_diff };
}
function answer(runId: string, body: Record<string, unknown>, verdict = "approve", reason = verdict, overrides = {}) {
  const { stage: _stage, claim_generation: _generation, round = 1, ...candidate } = body;
  return { result: "candidate", round, checker_run_id: child, candidate_digest: digest,
    candidate_generation: 1, candidate, verdict, reason_class: reason, findings: null,
    deadline_at: new Date(Date.now() + 60_000).toISOString(), ...proof(runId), ...overrides };
}
/** The checker answers every submission with one verdict and a canonical (server-normalized) candidate. */
function checkerSays(verdict: string, reason = verdict, overrides: Record<string, unknown> = {}) {
  api.crossCheckHandler = ({ runId, body }) => ({ status: 200, body: answer(runId, body, verdict, reason, {
    candidate: canonicalOf(body), automatic_revision_limit: 2, automatic_rounds_enabled: true, ...overrides }) });
}
/** A human approves each presented gate. */
function humanApproves(runId: string) {
  api.onState(runId, (b) => { if (b.status === "awaiting_approval") send(runId, row("approve_plan")); });
}
function gateReason(g: StateRequest): string | undefined {
  return (g as PlanCrossCheckStateRequest).plan_cross_check_gate_reason ?? undefined;
}
/** Plans are presented in order; an automatic REVISE moves to the next plan, anything else ends. */
function gatedExec(plans: string[] = [PLAN]) {
  const verdicts: PlanVerdict[] = [];
  const exec: Executor = { run: async (ctx) => {
    for (const plan of plans) {
      const v = await ctx.gatePlan!(plan);
      verdicts.push(v);
      if (v.kind === "reject") throw new PlanRejectedError(v.reason);
      if (!(v.kind === "revise" && v.automatic)) break;
    }
    return { branch: ctx.branch };
  } };
  return { exec, verdicts };
}
function start(exec: Executor, c: ClaimResponse) {
  const forge = fakeGitlab();
  const run = runner(exec, forge.gitlab, undefined, { planCrossCheckTiming: timing, planApprovalTimeoutMs: 4000 });
  return Object.assign(run.execute(c).then(() => forge), { shutdown: () => run.shutdown() });
}
function passLine(runId: string): string[] { return feed(runId).filter((t) => PASS_LINE.test(t)); }
function planMessages(runId: string): string[] {
  return api.messages(runId).filter((m) => m.kind === "plan").map((m) => String((m.payload as { plan_md?: unknown }).plan_md));
}
/** No checker verdict ever released an approval or stored a plan outside the human gate. */
function assertNoCheckedStorage(runId: string) {
  assert.ok(!api.states.some((s) => s.runId === runId && s.body.status === "running" && s.body.candidate_digest),
    "no checked (cross_check) storage report");
}

describe("gated plan cross-check: checker outcomes all open the human gate", () => {
  it("APPROVE presents the human gate from the canonical candidate (gated_pass), never an approval", { skip: LINUX_CAPTURE }, async () => {
    const c = gatedClaim();
    checkerSays("approve");
    humanApproves(c.run_id);
    const { exec, verdicts } = gatedExec();
    await start(exec, c);
    assert.equal(api.crossCheckRequests.length, 1);
    const [gate, ...rest] = gates(c.run_id);
    assert.ok(gate && rest.length === 0, "exactly one human gate");
    assert.equal(gate.plan_md, NORMALIZED, "the gate reads the plan the checker read");
    assert.equal(gate.candidate_digest, digest);
    assert.equal(Object.hasOwn(gate, "plan_cross_check_gate_reason"), false, "no worker-declared reason on a pass");
    assert.deepEqual(gate.milestones, [{ title: "canonical", done: false }]);
    assert.deepEqual(gate.required_capabilities, []);
    assert.deepEqual(gate.required_tools, []);
    assert.equal(gate.size_class, "s");
    assert.equal(verdicts.length, 1);
    assert.equal(verdicts[0]!.kind, "approve");
    assert.notEqual((verdicts[0] as { approval?: string }).approval, "cross_check", "the human approved, not the checker");
    assert.deepEqual((verdicts[0] as { canonical?: unknown }).canonical, {
      plan: gate.plan_md, milestones: gate.milestones, candidate_digest: digest, claimGeneration: c.claim_generation,
    }, "the human verdict carries exactly the presented candidate to implementation");
    assertNoCheckedStorage(c.run_id);
    assert.deepEqual(passLine(c.run_id), ["plan cross-check: passed after 1 round"]);
  });

  it("REVISE then APPROVE returns one automatic revise, then opens the gate after two rounds", { skip: LINUX_CAPTURE }, async () => {
    const c = gatedClaim();
    api.crossCheckHandler = ({ runId, body }) => ({ status: 200, body: answer(runId, body,
      body.round === 1 ? "revise" : "approve", body.round === 1 ? "revise" : "approve", {
        candidate: canonicalOf(body), automatic_revision_limit: 2, automatic_rounds_enabled: true,
        findings: body.round === 1 ? { summary: "tighten", items: [] } : null }) });
    humanApproves(c.run_id);
    const { exec, verdicts } = gatedExec([PLAN, `${PLAN} 2`]);
    await start(exec, c);
    assert.deepEqual(api.crossCheckRequests.map((r) => r.body.round), [1, 2]);
    assert.deepEqual(verdicts.map((v) => v.kind), ["revise", "approve"]);
    assert.equal(gates(c.run_id).length, 1, "the automatic revise published no gate");
    assert.equal(gates(c.run_id)[0]!.candidate_digest, digest);
    assert.deepEqual(passLine(c.run_id), ["plan cross-check: passed after 2 rounds"]);
    assertNoCheckedStorage(c.run_id);
  });

  const forced: { name: string; verdict: string; reason: string; expectReason: string; line: string }[] = [
    { name: "BLOCK", verdict: "block", reason: "block", expectReason: "block", line: "plan cross-check: blocked" },
    { name: "failure", verdict: "failed", reason: "model_error", expectReason: "model_error", line: "plan cross-check: model error" },
    { name: "checker-recorded timeout", verdict: "failed", reason: "timed_out", expectReason: "timed_out", line: "plan cross-check: timed out" },
  ];
  for (const f of forced) {
    it(`${f.name} opens the human gate with its checker-derived reason and no digest`, { skip: LINUX_CAPTURE }, async () => {
      const c = gatedClaim();
      checkerSays(f.verdict, f.reason);
      humanApproves(c.run_id);
      const { exec, verdicts } = gatedExec();
      await start(exec, c);
      const [gate, ...rest] = gates(c.run_id);
      assert.ok(gate && rest.length === 0);
      assert.equal(gateReason(gate), f.expectReason);
      assert.equal(Object.hasOwn(gate, "candidate_digest"), false, "a non-pass never carries the pass binding");
      assert.equal(gate.plan_md, PLAN, "the human fallback presents the lead's own plan");
      assert.ok(feed(c.run_id).includes(f.line), `status line ${f.line}; got ${JSON.stringify(feed(c.run_id))}`);
      assert.equal(passLine(c.run_id).length, 0);
      assert.equal(verdicts[0]!.kind, "approve");
      assertNoCheckedStorage(c.run_id);
    });
  }

  it("a pending checker that outlives its stored deadline opens the gate as timed_out", { skip: LINUX_CAPTURE }, async () => {
    const c = gatedClaim();
    api.crossCheckHandler = ({ runId, method, body }) => method === "GET"
      ? { status: 503, body: { error: "temporary" } }
      : { status: 200, body: answer(runId, body, "pending", "", { deadline_at: new Date(Date.now() + 300).toISOString() }) };
    humanApproves(c.run_id);
    const { exec } = gatedExec();
    await start(exec, c);
    const [gate] = gates(c.run_id);
    assert.equal(gateReason(gate!), "timed_out");
    assert.equal(Object.hasOwn(gate!, "candidate_digest"), false);
  });

  it("exhausted automatic revisions open the gate as revisions_exhausted", { skip: LINUX_CAPTURE }, async () => {
    const c = gatedClaim();
    checkerSays("revise", "revise", { automatic_revision_limit: 1 });
    humanApproves(c.run_id);
    const { exec, verdicts } = gatedExec([PLAN, `${PLAN} 2`]);
    await start(exec, c);
    assert.deepEqual(api.crossCheckRequests.map((r) => r.body.round), [1, 2]);
    assert.deepEqual(verdicts.map((v) => v.kind), ["revise", "approve"]);
    assert.equal(gateReason(gates(c.run_id)[0]!), "revisions_exhausted");
  });

  it("an unavailable checker opens the gate as checker_unavailable", { skip: LINUX_CAPTURE }, async () => {
    const c = gatedClaim();
    api.crossCheckHandler = () => ({ status: 409, body: { reason: "checker_unavailable" } });
    humanApproves(c.run_id);
    const { exec } = gatedExec();
    await start(exec, c);
    assert.equal(gates(c.run_id).length, 1);
    assert.equal(gateReason(gates(c.run_id)[0]!), "checker_unavailable");
    assert.ok(feed(c.run_id).includes("plan cross-check: checker unavailable"));
  });

  it("a preparation failure throws before any candidate or human publication", { skip: LINUX_CAPTURE }, async () => {
    const c = gatedClaim();
    const reserve = MessageBatcher.prototype.reserveCandidateTransport;
    let attempts = 0;
    MessageBatcher.prototype.reserveCandidateTransport = function () {
      attempts++;
      throw new Error("fixture reservation acquisition failure");
    };
    const { exec, verdicts } = gatedExec();
    try {
      await start(exec, c);
      assert.equal(attempts, 1);
      assert.equal(verdicts.length, 0);
      assert.equal(api.crossCheckRequests.length, 0);
      assert.equal(gates(c.run_id).length, 0);
      const states = api.states.filter((s) => s.runId === c.run_id).map((s) => s.body);
      assert.ok(states.some((s) => s.status === "failed" && s.failure_reason?.includes("transport reservation unavailable")),
        JSON.stringify(states.map((s) => [s.status, s.failure_reason])));
      assert.ok(!states.some((s) => s.status === "running" && s.plan_md), "no unchecked approval");
    } finally { MessageBatcher.prototype.reserveCandidateTransport = reserve; }
  });
});

describe("gated plan cross-check: canonical text", () => {
  it("a scrubbed canonical candidate reaches the feed plan and the gate report, not the raw text", { skip: LINUX_CAPTURE }, async () => {
    const c = gatedClaim();
    // Assembled at runtime so no complete provider-token-shaped literal exists in source.
    const secret = ["ghp", "_", "Zx9Qw8Er7Ty6Ui5Op4As3Df2Gh1Jk0Lm9Nb8"].join("");
    const raw = `# PLAN\n- rotate ${secret} before the deploy`;
    const canonical = "# PLAN\n- rotate [redacted] before the deploy";
    api.crossCheckHandler = ({ runId, body }) => ({ status: 200, body: answer(runId, body, "approve", "approve", {
      candidate: canonicalOf(body, canonical) }) });
    humanApproves(c.run_id);
    const { exec } = gatedExec([raw]);
    await start(exec, c);
    assert.deepEqual(planMessages(c.run_id), [canonical], "the feed plan is the text the checker read");
    const [gate] = gates(c.run_id);
    assert.equal(gate!.plan_md, canonical, "the stored plan equals the feed plan");
    assert.ok(!JSON.stringify(api.messages(c.run_id)).includes(secret), "raw text never reaches the feed");
    assert.ok(!JSON.stringify(gates(c.run_id)).includes(secret), "raw text never reaches the gate report");
  });
});

describe("gated plan cross-check: canonical requirement set", () => {
  it("a gated pass reports the checker's canonical requirements, not the raw detection", { skip: LINUX_CAPTURE }, async () => {
    const c = gatedClaim();
    // Raw detection of the clone: a Dockerfile infers the docker capability and size class s.
    fs.writeFileSync(path.join(fx.originPath, "Dockerfile"), "FROM alpine\n");
    const env: NodeJS.ProcessEnv = { ...process.env };
    for (const key of Object.keys(env)) if (key.startsWith("GIT_")) delete env[key];
    env.GIT_CONFIG_GLOBAL = "/dev/null";
    env.GIT_CONFIG_SYSTEM = "/dev/null";
    execFileSync("git", ["-C", fx.originPath, "add", "Dockerfile"], { env, stdio: "pipe" });
    execFileSync("git", ["-C", fx.originPath, "commit", "-m", "detector fixture"], { env, stdio: "pipe" });
    api.crossCheckHandler = ({ runId, body }) => ({ status: 200, body: answer(runId, body, "approve", "approve", {
      candidate: { ...canonicalOf(body), required_capabilities: ["jvm"], size_class: "m" } }) });
    humanApproves(c.run_id);
    const { exec, verdicts } = gatedExec();
    await start(exec, c);
    const submitted = api.crossCheckRequests[0]!.body as { required_capabilities: string[]; size_class: string };
    assert.deepEqual(submitted.required_capabilities, ["docker"], "the checker was shown the raw detection");
    assert.equal(submitted.size_class, "s");
    const [gate, ...rest] = gates(c.run_id);
    assert.ok(gate && rest.length === 0, "exactly one human gate");
    assert.deepEqual(gate.required_capabilities, ["jvm"], "the canonical capability set wins");
    assert.equal(gate.size_class, "m", "the canonical size class wins");
    assert.equal(gate.candidate_digest, digest, "bound to the checked candidate");
    assert.equal(Object.hasOwn(gate, "plan_cross_check_gate_reason"), false, "a real pass declares no reason");
    assert.deepEqual(passLine(c.run_id), ["plan cross-check: passed after 1 round"]);
    assert.equal(verdicts[0]!.kind, "approve");
  });
});

describe("gated plan cross-check: human gate settlement", () => {
  it("the applied gate releases the transport reservation before the state barrier", { skip: LINUX_CAPTURE }, async () => {
    const c = gatedClaim();
    checkerSays("approve");
    humanApproves(c.run_id);
    type Flight = { checkedStateBarrier?: Promise<void> };
    let flight: Flight | undefined;
    const barrierHeldAtRelease: boolean[] = [];
    const reserve = MessageBatcher.prototype.reserveCandidateTransport;
    MessageBatcher.prototype.reserveCandidateTransport = function (this: MessageBatcher) {
      const reservation = reserve.call(this);
      const release = reservation.releaseAppliedGate.bind(reservation);
      reservation.releaseAppliedGate = (ack) => {
        barrierHeldAtRelease.push(flight?.checkedStateBarrier !== undefined);
        return release(ack);
      };
      return reservation;
    };
    try {
      const forge = fakeGitlab();
      const run = runner(gatedExec().exec, forge.gitlab, undefined, { planCrossCheckTiming: timing, planApprovalTimeoutMs: 4000 });
      const seam = run as unknown as { prepareGateCheckpoint: (claim: ClaimResponse, f: Flight, ...rest: unknown[]) => Promise<unknown> };
      const prepare = seam.prepareGateCheckpoint.bind(run);
      seam.prepareGateCheckpoint = (claim, f, ...rest) => { flight = f; return prepare(claim, f, ...rest); };
      await run.execute(c);
      assert.deepEqual(barrierHeldAtRelease, [true], "the barrier is still held when the reservation is released");
      assert.equal(flight?.checkedStateBarrier, undefined, "and is released afterwards");
    } finally { MessageBatcher.prototype.reserveCandidateTransport = reserve; }
  });

  it("an executor re-entry resumes the existing human wait without re-planning or re-submitting", { skip: LINUX_CAPTURE }, async () => {
    const c = gatedClaim();
    checkerSays("approve");
    let otherwise = 0;
    let resumed: PlanVerdict | undefined;
    // The fake api records a state before it replies, so a gate visible in api.states may still
    // have its ACK in flight. Hold the first gate's ACK briefly so that window is open on every
    // run. The timer starts inside the hook: one armed up front would expire during the checker.
    let held = false;
    api.onState(c.run_id, (b) => {
      if (b.status !== "awaiting_approval" || held) return;
      held = true;
      api.afterPersistState(c.run_id, (s) => s.status === "awaiting_approval", new Promise((r) => setTimeout(r, 100)));
    });
    const exec: Executor = { run: async (ctx) => {
      assert.ok(ctx.continueExistingPlanGate, "a gated run offers the re-entry hook");
      // Re-enter only once the worker has applied the gate's ACK: it calls onPlanPersisted then.
      let persisted!: () => void;
      const confirmed = new Promise<void>((r) => { persisted = r; });
      void ctx.gatePlan!(PLAN, undefined, async () => { persisted(); }).catch(() => undefined);
      let deadline: NodeJS.Timeout | undefined;
      const presented = await Promise.race([confirmed.then(() => true),
        new Promise<false>((r) => { deadline = setTimeout(() => r(false), 4000); })]);
      clearTimeout(deadline);
      assert.ok(presented, "the worker confirmed the gate before the re-entry");
      assert.equal(gates(c.run_id).length, 1, "the gate is presented before the re-entry");
      const pending = ctx.continueExistingPlanGate!(async () => { otherwise++; return { kind: "reject", reason: "re-planned" } as PlanVerdict; });
      send(c.run_id, row("approve_plan"));
      resumed = await pending;
      return { branch: ctx.branch };
    } };
    await start(exec, c);
    assert.equal(otherwise, 0, "the fallback planner was not used");
    assert.equal(resumed?.kind, "approve");
    assert.equal(api.crossCheckRequests.length, 1, "nothing re-submitted");
    assert.equal(gates(c.run_id).length, 1, "the gate was not re-presented");
    assert.equal(planMessages(c.run_id).length, 1, "the plan was not re-emitted");
  });
});

describe("gated plan cross-check: semantic candidate", () => {
  for (const redacted of [false, true]) it(`a compiled semantic gated pass presents the matching bundle (${redacted ? "redacted title" : "unchanged title"})`, { skip: LINUX_CAPTURE }, async () => {
    git.capturePlanningDiff = async () => Buffer.from("");
    const rawPlan = redacted ? "Implement A\u202e" : "Implement A";
    const rawMilestones = [{ id: "m1", title: redacted ? "A glpat-" + "a".repeat(20) : "A" }];
    const checkedMilestones = [{ id: "m1", title: redacted ? "A [redacted]" : "A" }];
    const c = gatedClaim(1, { issue_description: "Implement A", semantic_planning: enrollment });
    const events: string[] = [];
    const server = semanticServer(client, events);
    api.stateAckWire = (body, run) => {
      if (body.status !== "awaiting_approval") return undefined;
      assert.equal(body.candidate_digest, server.compiled().candidate.digest);
      const ack = server.gateAck(body.claim_generation!, api.gateResumeFields(String(run.id)).resume_gate_revision!,
        body.semantic_park);
      // The reconciliation group the applied-gate release requires, as the real api returns it.
      return { status: 200, body: { run, ...ack, claim_generation: body.claim_generation,
        plan_cross_check_settled: true, lead_last_seq: Math.max(0, ...api.messages(String(run.id)).map((m) => m.seq)),
        current_plan_sha256: hash(body.plan_md ?? ""), gate_presentation_id: body.presentation_id,
        gate_payload_digest: hash("payload") } };
    };
    api.crossCheckHandler = ({ runId, body }) => {
      const { stage: _stage, claim_generation: _generation, round = 1, ...candidate } = body;
      const identity = server.compiled().candidate;
      return { status: 200, body: { result: "candidate", round, checker_run_id: child,
        candidate_digest: identity.digest, candidate_generation: 1,
        candidate: { ...candidate, plan_md: "Implement A", milestones: checkedMilestones, semantic_candidate: identity },
        verdict: "approve", reason_class: "approve", findings: null,
        automatic_revision_limit: 2, automatic_rounds_enabled: true,
        deadline_at: new Date(Date.now() + 60_000).toISOString(),
        lead_last_seq: Math.max(0, ...api.messages(runId).map((m) => m.seq)),
        claim_generation: 1, plan_cross_check_settled: true, gate_revision: 0, current_plan_sha256: hash("") } };
    };
    humanApproves(c.run_id);
    const transport = await boundedTransport("claude", server.report, events);
    const p = proposal();
    p.criteria[0]!.citations[0]!.source_id = "issue:" + c.issue_iid;
    let humanVerdict: PlanVerdict | undefined;
    const exec: Executor = { run: async (ctx) => {
      humanVerdict = await ctx.gatePlan!(rawPlan, rawMilestones, undefined, undefined, p);
      return { branch: ctx.branch };
    } };
    await runner(exec, fakeGitlab().gitlab, undefined, { semanticCoverageTestTransport: transport,
      planCrossCheckTiming: timing, planApprovalTimeoutMs: 4000 }).execute(c);
    const [gate, ...rest] = gates(c.run_id);
    assert.ok(gate && rest.length === 0, JSON.stringify(api.states));
    assert.equal(gate.plan_md, rawPlan, "semantic presentation preserves the raw plan identity");
    assert.deepEqual(gate.milestones, checkedMilestones);
    assert.equal(gate.candidate_digest, server.compiled().candidate.digest);
    assert.equal(humanVerdict?.kind, "approve", "a human verdict reached the executor");
    assert.notEqual((humanVerdict as { approval?: string }).approval, "cross_check");
    assert.deepEqual((humanVerdict as { canonical?: unknown }).canonical, {
      plan: gate.plan_md, milestones: gate.milestones, candidate_digest: gate.candidate_digest, claimGeneration: 1,
    }, "implementation receives the exact presented plan, milestones and semantic identity");
    assertNoCheckedStorage(c.run_id);
    assert.equal(Object.hasOwn(gate, "plan_cross_check_gate_reason"), false);
    assert.equal(Object.hasOwn(gate, "semantic_park"), false, "a human-gated run supplies no autopilot park");
    assert.deepEqual(passLine(c.run_id), ["plan cross-check: passed after 1 round"]);
  });
});

describe("gated plan cross-check: R4 behaviours retained at the human gate", () => {
  it("a ci_fix CI-config plan keeps its marker plan and the pass line, and the gate is human", { skip: LINUX_CAPTURE }, async () => {
    const c = gatedClaim(1, { kind: "ci_fix" });
    const marked = `${CI_CONFIG_MARKER}\nEdit .gitlab-ci.yml to add the missing job`;
    api.crossCheckHandler = ({ runId, body }) => ({ status: 200, body: answer(runId, body, "approve", "approve", {
      candidate: canonicalOf(body, marked) }) });
    humanApproves(c.run_id);
    const { exec, verdicts } = gatedExec([marked]);
    await start(exec, c);
    assert.equal(api.crossCheckRequests.length, 1, "a CI-config plan is still checked");
    assert.deepEqual(planMessages(c.run_id), [marked]);
    assert.deepEqual(passLine(c.run_id), ["plan cross-check: passed after 1 round"]);
    assert.equal(gates(c.run_id).length, 1);
    assert.equal(gates(c.run_id)[0]!.plan_md, marked);
    assert.equal(verdicts[0]!.kind, "approve");
    assertNoCheckedStorage(c.run_id);
  });

  const GIT_ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };
  const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];
  const gitIn = (dir: string, args: string[]) => execFileSync("git", ["-C", dir, ...args], { env: GIT_ENV, encoding: "utf8" }).trim();
  function publishBranch(name: string): string {
    gitIn(fx.originPath, ["checkout", "-b", name]);
    fs.writeFileSync(path.join(fx.originPath, "PUBLISHED.md"), `# published work on ${name}\n`);
    gitIn(fx.originPath, ["add", "PUBLISHED.md"]);
    gitIn(fx.originPath, [...IDENT, "commit", "-m", `published commit on ${name}`]);
    const sha = gitIn(fx.originPath, ["rev-parse", "HEAD"]);
    gitIn(fx.originPath, ["checkout", "main"]);
    return sha;
  }
  for (const verdict of ["approve", "block"] as const) {
    it(`a history-rewrite plan on a published branch emits the warn-only nudge (checker ${verdict}) with no forced reason`, { skip: LINUX_CAPTURE }, async () => {
      const c = gatedClaim();
      const tip = publishBranch(`agent/issue-${c.issue_iid}`);
      const rewrite = "# PLAN\n- git rebase the 23 published commits onto main";
      api.crossCheckHandler = ({ runId, body }) => ({ status: 200, body: answer(runId, body, verdict, verdict, {
        candidate: canonicalOf(body, rewrite) }) });
      humanApproves(c.run_id);
      const { exec, verdicts } = gatedExec([rewrite]);
      await start(exec, c);
      const nudges = feed(c.run_id).filter((t) => t.includes("the plan proposes rewriting history on a branch published at"));
      assert.equal(nudges.length, 1);
      assert.ok(nudges[0]!.includes(tip.slice(0, 12)));
      const [gate] = gates(c.run_id);
      assert.equal(gateReason(gate!), verdict === "block" ? "block" : undefined, "the reason is the checker's, never worker-forced");
      assert.equal(verdicts[0]!.kind, "approve", "the nudge never rejects");
    });
  }

  it("a reclaim with incomplete owner history keeps the owner-history note and never calls the checker", async () => {
    const M4_SID = "aaaaaaaa-bbbb-cccc-dddd-ffffffffffff";
    const iid = 37081;
    const runId = randomUUID();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-3708-history-"));
    const { gitlab } = fakeGitlab();
    const factoryOf = (run: Executor["run"]): ExecutorFactory => (id) => ({ homeDir: path.join(homeRoot, id), executor: { run } });
    try {
      // Park with a commit so the reclaim recovers its tree (otherwise a human-approved run re-gates
      // for a different reason and the owner-history clause is never reached).
      await runnerWith(factoryOf(async (ctx: RunContext): Promise<ExecutorResult> => {
        fs.writeFileSync(path.join(ctx.worktreePath, "WORK.txt"), "committed before the park\n");
        gitIn(ctx.worktreePath, ["add", "WORK.txt"]);
        gitIn(ctx.worktreePath, [...IDENT, "commit", "-m", "add WORK.txt"]);
        fs.mkdirSync(path.join(homeRoot, runId), { recursive: true });
        throw new LimitReachedError({ resetsAtMs: Date.now() + 5 * 3600_000, rateLimitType: "five_hour" });
      }), gitlab).execute(gitlabClaim(iid, { run_id: runId, wait_on_limit: true, claim_generation: 1 }));
      api.setInputClaimGeneration(runId, 2);
      api.overrideFollowUps(runId, 404, { error: "not found" });
      humanApproves(runId);
      api.crossCheckHandler = () => { throw new Error("a reclaim must never submit"); };
      const verdicts: string[] = [];
      await runnerWith(factoryOf(async (ctx: RunContext): Promise<ExecutorResult> => {
        verdicts.push((await ctx.gatePlan!("# reviewed plan\n- ship it")).kind);
        return { branch: ctx.branch };
      }), gitlab).execute(gitlabClaim(iid, { run_id: runId, wait_on_limit: true, claim_generation: 2, session_id: M4_SID, last_seq: 1000,
        plan_approved: true, plan_source: "agent", plan_md: "# reviewed plan\n- ship it", auto_approve: false,
        plan_cross_check_gated: true }));
      assert.deepEqual(verdicts, ["approve"]);
      assert.equal(api.crossCheckRequests.length, 0, "no submit");
      assert.equal(api.crossCheckLatestRequests.length, 0, "no latest discovery");
      assert.equal(feed(runId).filter((t) => /earlier owner follow-ups on this run could not be restored/.test(t)).length, 1);
      const [gate] = gates(runId);
      assert.ok(gate, "the human gate opens");
      assert.equal(Object.hasOwn(gate, "plan_cross_check_gate_reason"), false, "no checker reason");
    } finally { fs.rmSync(homeRoot, { recursive: true, force: true }); }
  });
});

describe("gated plan cross-check: never re-checked", () => {
  it("a human revise after the gate never re-checks", { skip: LINUX_CAPTURE }, async () => {
    const c = gatedClaim();
    checkerSays("approve");
    let presentations = 0;
    api.onState(c.run_id, (b) => {
      if (b.status === "awaiting_approval")
        send(c.run_id, row(++presentations === 1 ? "revise_plan" : "approve_plan", "human feedback"));
    });
    const seen: PlanVerdict[] = [];
    const exec: Executor = { run: async (ctx) => {
      const first = await ctx.gatePlan!(PLAN);
      seen.push(first);
      assert.ok(first.kind === "revise" && !first.automatic);
      seen.push(await ctx.gatePlan!("# PLAN human revised", undefined, undefined, first.kind === "revise" ? first.inputId : undefined));
      return { branch: ctx.branch };
    } };
    await start(exec, c);
    assert.deepEqual(seen.map((v) => v.kind), ["revise", "approve"]);
    assert.equal(api.crossCheckRequests.length, 1, "one candidate for the whole run");
    assert.equal(api.crossCheckLatestRequests.length, 0);
    const presented = gates(c.run_id);
    assert.equal(presented.length, 2);
    assert.equal(presented[0]!.candidate_digest, digest);
    assert.equal(Object.hasOwn(presented[1]!, "candidate_digest"), false, "the revised plan carries no pass binding");
    assert.equal(passLine(c.run_id).length, 1, "the pass line is not repeated");
  });

  const reclaims: { name: string; overrides: Partial<ClaimResponse> }[] = [
    { name: "plan_md set", overrides: { plan_md: "# prior\n- presented plan", plan_source: "agent" } },
    { name: "resume_gate_revision > 0", overrides: { resume_gate_revision: 2 } },
  ];
  for (const r of reclaims) {
    it(`a reclaim with ${r.name} never calls submit or latest`, { skip: LINUX_CAPTURE }, async () => {
      const c = gatedClaim(2, r.overrides);
      api.crossCheckHandler = () => { throw new Error("a presented gate is never re-checked"); };
      api.crossCheckLatestHandler = () => { throw new Error("a presented gate never runs discovery"); };
      humanApproves(c.run_id);
      const { exec, verdicts } = gatedExec();
      await start(exec, c);
      assert.equal(api.crossCheckRequests.length, 0);
      assert.equal(api.crossCheckLatestRequests.length, 0);
      assert.equal(verdicts.length, 1);
      assert.ok(gates(c.run_id).length >= 1);
      assert.equal(Object.hasOwn(gates(c.run_id)[0]!, "candidate_digest"), false);
    });
  }

  it("a human revise queued before the first gate opens is not lost and triggers no extra check", { skip: LINUX_CAPTURE }, async () => {
    const c = gatedClaim();
    let queuedId = 0;
    api.crossCheckHandler = ({ runId, body }) => {
      // The human acts while the check is still pending: the gate has not opened yet.
      if (queuedId === 0) queuedId = send(c.run_id, row("revise_plan", "early human feedback"))[0]!.id;
      return { status: 200, body: answer(runId, body, "approve", "approve", { candidate: canonicalOf(body),
        automatic_revision_limit: 2, automatic_rounds_enabled: true }) };
    };
    api.onState(c.run_id, (b) => {
      if (b.status === "awaiting_approval" && b.plan_md === "# PLAN human revised") send(c.run_id, row("approve_plan", "final"));
    });
    const seen: PlanVerdict[] = [];
    const exec: Executor = { run: async (ctx) => {
      const first = await ctx.gatePlan!(PLAN);
      seen.push(first);
      assert.ok(first.kind === "revise" && !first.automatic, "the early revise is the human's, not automatic");
      seen.push(await ctx.gatePlan!("# PLAN human revised", undefined, undefined, first.kind === "revise" ? first.inputId : undefined));
      return { branch: ctx.branch };
    } };
    await start(exec, c);
    assert.ok(queuedId > 0);
    assert.deepEqual(seen.map((v) => v.kind), ["revise", "approve"], "the early revise was consumed, not lost");
    assert.equal((seen[0] as { feedback?: string }).feedback, "early human feedback");
    assert.equal(api.crossCheckRequests.length, 1, "no extra check for the human revise");
    assert.equal(api.crossCheckLatestRequests.length, 0);
    assert.equal(gates(c.run_id).length, 2, "the check's gate opened once, then the revised plan");
    assert.equal(gates(c.run_id)[0]!.candidate_digest, digest);
    assert.equal(Object.hasOwn(gates(c.run_id)[1]!, "candidate_digest"), false);
    assert.ok(api.isApplied(c.run_id, queuedId), "the early revise input settled");
    assertNoCheckedStorage(c.run_id);
  });

  it("a lost submit ACK: a same-generation retry resubmits the same round and creates no second round", { skip: LINUX_CAPTURE }, async () => {
    // Generation 2 with no stored plan: the first execution submitted round 1 and its response was lost.
    const c = gatedClaim(2);
    api.crossCheckLatestHandler = () => ({ status: 200, body: {
      result: "latest", round: 1, candidate_generation: 2, automatic_revision_limit: 2,
      automatic_rounds_enabled: true, next_round: null, next_round_eligible: false, fallback_reason: "" } });
    api.crossCheckHandler = ({ runId, body }) => ({ status: 200, body: answer(runId, body, "approve", "approve", {
      ...proof(runId, 2), candidate: canonicalOf(body), candidate_generation: 2,
      automatic_revision_limit: 2, automatic_rounds_enabled: true }) });
    humanApproves(c.run_id);
    const { exec, verdicts } = gatedExec();
    await start(exec, c);
    assert.deepEqual(api.crossCheckLatestRequests, [{ runId: c.run_id, generation: 2 }], "discovery ran once");
    assert.deepEqual(api.crossCheckRequests.map((r) => r.body.round), [1], "the same round is resubmitted, never round 2");
    assert.equal(gates(c.run_id).length, 1);
    assert.equal(gates(c.run_id)[0]!.candidate_digest, digest);
    assert.equal(verdicts.length, 1);
    assert.equal(verdicts[0]!.kind, "approve");
    assert.notEqual((verdicts[0] as { approval?: string }).approval, "cross_check");
    assertNoCheckedStorage(c.run_id);
  });

  it("owner cancellation while the check is pending presents no gate and mints no approval", { skip: LINUX_CAPTURE }, async () => {
    const c = gatedClaim();
    let context!: RunContext;
    api.crossCheckHandler = async ({ runId, body }) => {
      send(c.run_id, row("cancel"));
      assert.ok(await until(() => context.cancelRequested!()));
      return { status: 200, body: answer(runId, body, "approve", "approve", { candidate: canonicalOf(body),
        automatic_revision_limit: 2, automatic_rounds_enabled: true }) };
    };
    humanApproves(c.run_id);
    const verdicts: PlanVerdict[] = [];
    const exec: Executor = { run: async (ctx) => {
      context = ctx;
      verdicts.push(await ctx.gatePlan!(PLAN));
      return { branch: ctx.branch };
    } };
    await start(exec, c);
    assert.equal(verdicts.length, 0, "no verdict reaches the executor");
    assert.equal(gates(c.run_id).length, 0, "no human gate presented");
    const states = api.states.filter((s) => s.runId === c.run_id).map((s) => s.body);
    assert.ok(!states.some((s) => s.status === "awaiting_approval"), "never awaiting_approval");
    assert.ok(!states.some((s) => s.status === "running" && s.plan_md), "no approval minted");
    assert.ok(!states.some((s) => s.status === "completed" || s.status === "recovery_wait"));
    assertNoCheckedStorage(c.run_id);
  });

  it("a claim without the gated field never submits and opens the ordinary gate", { skip: LINUX_CAPTURE }, async () => {
    const c = freshClaim(1, { kind: "issue", auto_approve: false });
    api.crossCheckHandler = () => { throw new Error("an ungated run is never checked"); };
    humanApproves(c.run_id);
    const { exec, verdicts } = gatedExec();
    await start(exec, c);
    assert.equal(api.crossCheckRequests.length, 0);
    assert.equal(api.crossCheckLatestRequests.length, 0);
    assert.equal(gates(c.run_id).length, 1);
    assert.equal(Object.hasOwn(gates(c.run_id)[0]!, "plan_cross_check_gate_reason"), false);
    assert.equal(verdicts[0]!.kind, "approve");
  });
});

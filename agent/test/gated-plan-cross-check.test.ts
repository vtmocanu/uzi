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
import { feed, freshClaim, gates, newApi, row, send } from "./gate-revision-harness.js";
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

describe("gated plan cross-check: semantic candidate", () => {
  it("a gated pass of a compiled semantic candidate binds the semantic digest and opens the human gate", { skip: LINUX_CAPTURE }, async () => {
    git.capturePlanningDiff = async () => Buffer.from("");
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
        candidate: { ...candidate, semantic_candidate: identity },
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
    const exec: Executor = { run: async (ctx) => {
      const verdict = await ctx.gatePlan!("Implement A", [{ id: "m1", title: "A" }], undefined, undefined, p);
      assert.equal(verdict.kind, "approve");
      assert.notEqual((verdict as { approval?: string }).approval, "cross_check");
      return { branch: ctx.branch };
    } };
    await runner(exec, fakeGitlab().gitlab, undefined, { semanticCoverageTestTransport: transport,
      planCrossCheckTiming: timing, planApprovalTimeoutMs: 4000 }).execute(c);
    const [gate, ...rest] = gates(c.run_id);
    assert.ok(gate && rest.length === 0, JSON.stringify(api.states));
    assert.equal(gate.candidate_digest, server.compiled().candidate.digest);
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

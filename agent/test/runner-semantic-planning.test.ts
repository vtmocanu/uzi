import { beforeEach, it } from "node:test";
import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
import type { Executor } from "../src/executor.js";
import { RequestError, type PlanCrossCheckCandidate } from "../src/client.js";
import { api, client, git, installHarness, runner, fakeGitlab } from "./runner-harness.js";
import { freshClaim, gates, newApi, row, send } from "./gate-revision-harness.js";
import { enrollment, proposal, boundedTransport, hash } from "./semantic-coverage-fixture.js";
import { semanticServer } from "./semantic-server-fixture.js";
installHarness();
beforeEach(() => {
  newApi(); api.checkedTransport = true;
  api.usageHandler = () => ({ status: 200, body: {} });
  // Isolate gate ordering from native alternate-object capture; no repository mutation.
  git.capturePlanningDiff = async (clone, base, bare) => {
    assert.ok(bare, "runner forwards explicit worker bare authority");
    assert.match(base, /^[a-f0-9]{40}$/);
    assert.ok(clone);
    return Buffer.from("");
  };
});
const timing = { preparationMs: 100, backoffMs: [1, 2] as const, requestMs: 1000, pollMs: 5 };
const milestones = [{ id: "m1", title: "A" }];
const LINUX_CAPTURE = process.platform !== "linux" && "Linux /proc capture required";
function semanticClaim(overrides = {}) {
  return freshClaim(1, { kind: "issue", auto_approve: true, issue_description: "Implement A",
    semantic_planning: enrollment, ...overrides });
}
function serverWithAck(events: string[]) {
  const server = semanticServer(client, events);
  const presentations: ReturnType<typeof server.gateAck>[] = [];
  api.stateAckWire = (body, run) => {
    if (body.status !== "awaiting_approval") return undefined;
    assert.equal(body.candidate_digest, server.compiled().candidate.digest);
    assert.ok(body.presentation_id);
    const ack = server.gateAck(body.claim_generation!, api.gateResumeFields(String(run.id)).resume_gate_revision!,
      body.semantic_park);
    presentations.push(ack);
    return { status: 200, body: { run, ...ack } };
  };
  return { ...server, presentations };
}
function submitted(issueIid: number | null) {
  const p = proposal();
  p.criteria[0]!.citations[0]!.source_id = "issue:" + issueIid;
  return p;
}

for (const harness of ["claude", "codex"] as const) {
  it("M2 runner no-checker approval point executes bounded " + harness + " coverage", { skip: LINUX_CAPTURE }, async () => {
    const c = semanticClaim();
    const events: string[] = [];
    const server = serverWithAck(events);
    const transport = await boundedTransport(harness, server.report, events);
    const exec: Executor = { run: async ctx => {
      assert.deepEqual(ctx.semanticPlanning, enrollment);
      const verdict = await ctx.gatePlan!("Implement A", milestones, undefined, undefined, submitted(c.issue_iid));
      assert.equal(verdict.kind, "approve");
      assert.equal(events.filter(e => e === "invoke").length, 1);
      return { branch: ctx.branch };
    } };
    await runner(exec, fakeGitlab().gitlab, undefined, { semanticCoverageTestTransport: transport,
      planApprovalTimeoutMs: 4000 }).execute(c);
    assert.deepEqual(events.slice(0, 6), ["compile", "status", "reserve", "invoke", "settle:success", "publish"], JSON.stringify(api.states));
    assert.equal(api.crossCheckRequests.length, 0, "coverage introduces no cross-family requirement");
    assert.ok(api.messages(c.run_id).some(m => m.payload.event === "semantic_plan_coverage" && m.payload.outcome === "complete"));
  });
}

it("M2 runner four automatic checker REVISE rounds compile but consume zero coverage calls", { skip: LINUX_CAPTURE }, async () => {
  const c = semanticClaim({ plan_cross_check_required: true });
  const events: string[] = [];
  const server = serverWithAck(events);
  const transport = await boundedTransport("claude", server.report, events);
  api.crossCheckHandler = ({ runId, body }) => {
    const { stage: _stage, claim_generation: _generation, round = 1, ...candidate } = body;
    assert.equal((candidate as unknown as PlanCrossCheckCandidate).semantic_candidate, undefined);
    assert.equal((candidate as unknown as PlanCrossCheckCandidate).candidate_identity_version, "semantic_v1");
    // Like the API, resolve the server-stored candidate rather than accepting
    // a redundant worker-supplied copy of canonical criteria.
    const identity = server.compiled().candidate;
    events.push("checker:" + round);
    assert.equal(events.includes("invoke"), false, "checker decision always precedes coverage");
    return { status: 200, body: {
      result: "candidate", round, checker_run_id: "11111111-1111-4111-8111-111111111111",
      candidate_digest: identity.digest, candidate_generation: 1,
      candidate: { ...candidate, semantic_candidate: identity },
      verdict: Number(round) < 5 ? "revise" : "approve", reason_class: Number(round) < 5 ? "revise" : "approve",
      findings: Number(round) < 5 ? { summary: "revise", items: [] } : null,
      automatic_revision_limit: 4, automatic_rounds_enabled: true,
      deadline_at: new Date(Date.now() + 60000).toISOString(),
      lead_last_seq: Math.max(0, ...api.messages(runId).map(m => m.seq)),
      claim_generation: 1, plan_cross_check_settled: true, gate_revision: 0, current_plan_sha256: hash(""),
    } };
  };
  const exec: Executor = { run: async ctx => {
    for (let n = 1; n <= 5; n++) {
      const verdict = await ctx.gatePlan!("Implement A " + n, milestones, undefined, undefined, submitted(c.issue_iid));
      assert.equal(verdict.kind, n < 5 ? "revise" : "approve");
      assert.equal(events.filter(e => e === "invoke").length, n < 5 ? 0 : 1);
      assert.equal(events.filter(e => e === "compile").length, n);
    }
    return { branch: ctx.branch };
  } };
  await runner(exec, fakeGitlab().gitlab, undefined, { semanticCoverageTestTransport: transport,
    planCrossCheckTiming: timing, planApprovalTimeoutMs: 4000 }).execute(c);
  assert.equal(events.filter(e => e === "checker:5").length, 1, JSON.stringify(api.states));
  assert.equal(events.filter(e => e === "invoke").length, 1);
  assert.ok(events.indexOf("checker:5") < events.indexOf("reserve"));
});

for (const harness of ["claude", "codex"] as const) it("M3 " + harness + " human revision binds both exact candidates without a checker", { skip: LINUX_CAPTURE }, async () => {
  const c = semanticClaim({ auto_approve: false, plan_cross_check_required: true });
  const events: string[] = [];
  const server = serverWithAck(events);
  const transport = await boundedTransport(harness, server.report, events);
  let presentations = 0;
  api.onState(c.run_id, body => {
    if (body.status === "awaiting_approval") send(c.run_id,
      row(++presentations === 1 ? "revise_plan" : "approve_plan", "owner decision"));
  });
  const exec: Executor = { run: async ctx => {
    const p = submitted(c.issue_iid);
    const first = await ctx.gatePlan!("Implement A", milestones, undefined, undefined, p);
    assert.equal(first.kind, "revise");
    const next = await ctx.gatePlan!("Implement A with tests", milestones, undefined,
      first.kind === "revise" && !first.automatic ? first.inputId : undefined, p);
    assert.equal(next.kind, "approve");
    return { branch: ctx.branch };
  } };
  await runner(exec, fakeGitlab().gitlab, undefined, { semanticCoverageTestTransport: transport,
    planApprovalTimeoutMs: 4000 }).execute(c);
  assert.equal(events.filter(e => e === "invoke").length, 2);
  assert.equal(api.crossCheckRequests.length, 0);
  assert.equal(presentations, 2);
  assert.equal(server.presentations.length, 2);
  const [first, revised] = server.presentations;
  assert.notEqual(first!.semantic_approval.candidate_digest, revised!.semantic_approval.candidate_digest);
  for (const ack of server.presentations) {
    assert.equal(ack.semantic_approval.claim_generation, c.claim_generation);
    assert.equal(ack.semantic_presentation.coverage.candidate_digest, ack.semantic_approval.candidate_digest);
    assert.equal(ack.semantic_presentation.coverage.report?.candidate_digest, ack.semantic_approval.candidate_digest);
  }
});


for (const harness of ["claude", "codex"] as const) {
  for (const changed of ["criteria", "source", "coverage", "generation", "queued revision"] as const) {
    it("M3 " + harness + " resumed semantic " + changed + " refreshes presentation and certificate", { skip: LINUX_CAPTURE }, async () => {
      const c = semanticClaim({ auto_approve: false, plan_cross_check_required: false });
      const events: string[] = [];
      const server = serverWithAck(events);
      const transport = await boundedTransport(harness, server.report, events);
      const oldId = randomUUID();
      const oldRevision = 3;
      api.seedGate(c.run_id, { revision: oldRevision, presentationId: oldId,
        payload: { plan_md: "Implement A", milestones, required_capabilities: [], required_tools: [], size_class: "s" } });
      // Seed the prior server candidate and report, without giving the resumed claim
      // a certificate it cannot carry on the real protocol.
      await client.submitSemanticCandidate(c.run_id, 1, {
        plan_md: "Implement A", milestones, required_capabilities: [], required_tools: [],
        size_class: "s", base_commit: "a".repeat(40), planning_diff: "",
      }, submitted(c.issue_iid), [{ id: "issue:" + c.issue_iid, kind: "issue", provenance: "server:issue",
        completeness: "complete", bytes: Buffer.from("Implement A").toString("base64") }]);
      await client.publishSemanticCoverage(c.run_id, 1, {
        review_id: randomUUID(), invocation_id: randomUUID(), report: server.report(),
      });
      const old = server.gateAck(1, oldRevision);
      const generation = changed === "generation" ? 2 : 1;
      api.setInputClaimGeneration(c.run_id, generation);
      api.setOwnershipStatus(c.run_id, "awaiting_approval", generation);
      const [queued] = changed === "queued revision"
        ? send(c.run_id, row("revise_plan", "keep the plan text; revise acceptance",
          { gate_binding: "bound", gate_revision: oldRevision }))
        : [];
      const claim = { ...c, claim_generation: generation, plan_md: "Implement A",
        plan_source: "agent" as const, plan_approved: false, resume_phase: "awaiting_approval" as const,
        resume_plan_at: "2000-01-01T00:00:00.000000Z", ...api.gateResumeFields(c.run_id),
        ...(changed === "source" ? { issue_description: "Implement A and verify B" } : {}),
      };
      if (changed === "coverage") server.deny();
      api.onState(c.run_id, body => {
        if (body.status === "awaiting_approval") send(c.run_id, row("approve_plan"));
      });
      let entered = false;
      const exec: Executor = { resumesAtGate: true, run: async ctx => {
        entered = true;
        const pending = await ctx.takeResumedGateEvent!(ctx.signal);
        assert.equal(pending?.kind, queued ? "revise" : undefined);
        if (queued && pending?.kind === "revise") assert.equal(pending.inputId, queued.id);
        const proposal = submitted(c.issue_iid);
        if (changed === "source") proposal.criteria[0]!.citations[0]!.hash = hash(claim.issue_description);
        if (changed === "criteria" || queued) proposal.criteria[0]!.done_when = "A and its acceptance are verified";
        const verdict = await ctx.gatePlan!("Implement A", milestones, undefined,
          pending?.kind === "revise" ? pending.inputId : undefined, proposal);
        assert.equal(verdict.kind, "approve");
        return { branch: ctx.branch };
      } };
      await runner(exec, fakeGitlab().gitlab, undefined, { semanticCoverageTestTransport: transport,
        planApprovalTimeoutMs: 4000 }).execute(claim);
      assert.equal(entered, true);
      const [gate] = gates(c.run_id);
      assert.ok(gate, JSON.stringify(api.states));
      assert.equal(gate.plan_md, "Implement A");
      assert.notEqual(gate.presentation_id, oldId);
      assert.equal(server.presentations.length, 1);
      const current = server.presentations[0]!;
      assert.equal(current.gate_revision, oldRevision + 1);
      assert.equal(current.semantic_approval.claim_generation, generation);
      assert.notEqual(current.semantic_approval.presentation_digest, old.semantic_approval.presentation_digest);
      assert.equal(current.semantic_approval.candidate_digest, server.compiled().candidate.digest);
      assert.deepEqual(current.semantic_presentation.coverage, server.status());
      if (changed === "criteria" || queued)
        assert.notEqual(current.semantic_approval.criteria_digest, old.semantic_approval.criteria_digest);
      if (changed === "source")
        assert.notEqual(current.semantic_approval.source_digest, old.semantic_approval.source_digest);
      if (changed === "coverage") {
        assert.equal(current.semantic_presentation.coverage.outcome, "unavailable");
        assert.equal(current.semantic_presentation.coverage.report, undefined);
        assert.equal(events.includes("invoke"), false);
      }
      if (queued) assert.equal(api.isApplied(c.run_id, queued.id), true);
      assert.equal(api.crossCheckRequests.length, 0);
    });
  }
}

it("M2 runner reports coverage exhaustion independently of remaining checker allowance", { skip: LINUX_CAPTURE }, async () => {
  const c = semanticClaim();
  const events: string[] = [];
  const server = serverWithAck(events); server.deny();
  const transport = await boundedTransport("claude", server.report, events);
  // PRD #1230 M4: exhausted coverage no longer auto-approves; the owner decides.
  api.onState(c.run_id, body => { if (body.status === "awaiting_approval") send(c.run_id, row("approve_plan")); });
  const exec: Executor = { run: async ctx => {
    assert.equal((await ctx.gatePlan!("Implement A", milestones, undefined, undefined, submitted(c.issue_iid))).kind, "approve");
    return { branch: ctx.branch };
  } };
  await runner(exec, fakeGitlab().gitlab, undefined, { semanticCoverageTestTransport: transport,
    planApprovalTimeoutMs: 4000 }).execute(c);
  assert.equal(gates(c.run_id)[0]?.semantic_park, "coverage_unavailable");
  assert.equal(events.includes("invoke"), false);
  assert.ok(api.messages(c.run_id).some(m => m.payload.event === "semantic_plan_coverage" && m.payload.reason === "coverage_exhausted"), JSON.stringify(api.states));
});
it("M2 runner structural production claims expose no semantic context or calls", { skip: LINUX_CAPTURE }, async () => {
  const c = freshClaim(1, { kind: "issue", auto_approve: true });
  const events: string[] = [];
  const server = serverWithAck(events);
  const transport = await boundedTransport("claude", server.report, events);
  const exec: Executor = { run: async ctx => {
    assert.equal(ctx.semanticPlanning, undefined);
    await ctx.gatePlan!("Implement A", milestones);
    return { branch: ctx.branch };
  } };
  await runner(exec, fakeGitlab().gitlab, undefined, { semanticCoverageTestTransport: transport }).execute(c);
  assert.deepEqual(events, []);
});

// PRD #1230 M4 (D3/D4): the autopilot coverage park. The server's single coverage authority decides;
// every non-complete outcome, and any maintainer criterion, takes the human gate with the exact
// park reason the api validates, without a replan, a synthetic REVISE or a second coverage call.
function maintainerProposal(issueIid: number | null) {
  const p = submitted(issueIid);
  Object.assign(p.criteria[0]!, { kind: "maintainer", maintainer: {
    reason: "Owner observes the hosted rollout", conditions: ["Rollout reached the hosted cluster"],
    verification_steps: ["Open the hosted dashboard and confirm the new revision"],
  } });
  return p;
}
const parkCases: [string, string, (s: ReturnType<typeof serverWithAck>) => void][] = [
  ["incomplete", "coverage_incomplete", s => s.force("incomplete")],
  ["uncertain", "coverage_uncertain", s => s.force("uncertain")],
  ["unavailable", "coverage_unavailable", s => s.force("unavailable")],
  ["exhausted", "coverage_unavailable", s => s.deny()],
  ["status transport throw", "coverage_unavailable", s => s.failStatus(new RequestError("GET", "/semantic/coverage", 503, "down"))],
  ["publish transport throw", "coverage_unavailable", s => s.failPublish(new RequestError("POST", "/semantic/coverage/publish", 502, "down"))],
  ["network throw", "coverage_unavailable", s => s.failStatus(Object.assign(new TypeError("fetch failed"), { cause: { code: "ECONNRESET" } }))],
];
for (const harness of ["claude", "codex"] as const) {
  for (const [name, park, arrange] of parkCases) {
    it("M4 autopilot " + harness + " parks " + name + " coverage with " + park + " and no second coverage call", { skip: LINUX_CAPTURE }, async () => {
      const c = semanticClaim();
      const events: string[] = [];
      const server = serverWithAck(events);
      arrange(server);
      const transport = await boundedTransport(harness, server.report, events);
      api.onState(c.run_id, body => { if (body.status === "awaiting_approval") send(c.run_id, row("approve_plan")); });
      const exec: Executor = { run: async ctx => {
        const verdict = await ctx.gatePlan!("Implement A", milestones, undefined, undefined, submitted(c.issue_iid));
        assert.equal(verdict.kind, "approve");
        assert.ok(verdict.kind === "approve" && verdict.approval === undefined, "a human, never the checker or auto-approval");
        return { branch: ctx.branch };
      } };
      await runner(exec, fakeGitlab().gitlab, undefined, { semanticCoverageTestTransport: transport,
        planApprovalTimeoutMs: 4000 }).execute(c);
      const parked = gates(c.run_id);
      assert.equal(parked.length, 1, JSON.stringify(api.states));
      assert.equal(parked[0]!.semantic_park, park);
      assert.equal(events.filter(e => e === "compile").length, 1, "no replan");
      assert.ok(events.filter(e => e === "reserve").length <= 1, "no second coverage admission for the same candidate");
      assert.ok(events.filter(e => e === "invoke").length <= 1);
      assert.equal(api.crossCheckRequests.length, 0, "no synthetic checker round");
      const planReports = api.states.filter(s => s.runId === c.run_id && s.body.status === "running" && s.body.plan_md !== undefined);
      assert.equal(planReports.length, 0, "an autopilot plan write never precedes the human decision");
      assert.ok(api.messages(c.run_id).some(m => m.payload.event === "semantic_plan_coverage"));
    });
  }
  it("M4 autopilot " + harness + " parks a complete candidate with a maintainer criterion as maintainer_authorization", { skip: LINUX_CAPTURE }, async () => {
    const c = semanticClaim();
    const events: string[] = [];
    const server = serverWithAck(events);
    const transport = await boundedTransport(harness, server.report, events);
    api.onState(c.run_id, body => { if (body.status === "awaiting_approval") send(c.run_id, row("approve_plan")); });
    const exec: Executor = { run: async ctx => {
      const verdict = await ctx.gatePlan!("Implement A", milestones, undefined, undefined, maintainerProposal(c.issue_iid));
      assert.equal(verdict.kind, "approve");
      return { branch: ctx.branch };
    } };
    await runner(exec, fakeGitlab().gitlab, undefined, { semanticCoverageTestTransport: transport,
      planApprovalTimeoutMs: 4000 }).execute(c);
    assert.equal(server.status().outcome, "complete");
    assert.equal(gates(c.run_id).length, 1, JSON.stringify(api.states));
    assert.equal(gates(c.run_id)[0]!.semantic_park, "maintainer_authorization");
    assert.equal(events.filter(e => e === "invoke").length, 1);
  });
}

it("M4 a foreign or malformed coverage response stays fail-closed and never parks as unavailable", { skip: LINUX_CAPTURE }, async () => {
  const c = semanticClaim();
  const events: string[] = [];
  const server = serverWithAck(events);
  server.failStatus(new Error("Foreign semantic plan"));
  const transport = await boundedTransport("claude", server.report, events);
  let thrown: unknown;
  const exec: Executor = { run: async ctx => {
    try { await ctx.gatePlan!("Implement A", milestones, undefined, undefined, submitted(c.issue_iid)); }
    catch (error) { thrown = error; throw error; }
    return { branch: ctx.branch };
  } };
  await runner(exec, fakeGitlab().gitlab, undefined, { semanticCoverageTestTransport: transport,
    planApprovalTimeoutMs: 4000 }).execute(c);
  assert.match(String(thrown), /Foreign semantic plan/);
  assert.equal(gates(c.run_id).length, 0);
});

it("M4 a 403 from the coverage status endpoint stays fail-closed and never parks as unavailable", { skip: LINUX_CAPTURE }, async () => {
  const c = semanticClaim();
  const events: string[] = [];
  const server = serverWithAck(events);
  server.failStatus(new RequestError("GET", "/semantic/coverage", 403, "denied"));
  const transport = await boundedTransport("claude", server.report, events);
  let thrown: unknown;
  const exec: Executor = { run: async ctx => {
    try { await ctx.gatePlan!("Implement A", milestones, undefined, undefined, submitted(c.issue_iid)); }
    catch (error) { thrown = error; throw error; }
    return { branch: ctx.branch };
  } };
  await runner(exec, fakeGitlab().gitlab, undefined, { semanticCoverageTestTransport: transport,
    planApprovalTimeoutMs: 4000 }).execute(c);
  assert.ok(thrown instanceof RequestError && thrown.status === 403, String(thrown));
  assert.equal(gates(c.run_id).length, 0);
});

it("M4 a status verdict for another candidate is foreign, not a park", { skip: LINUX_CAPTURE }, async () => {
  const c = semanticClaim();
  const events: string[] = [];
  const server = serverWithAck(events);
  const transport = await boundedTransport("claude", server.report, events);
  const real = client.semanticCoverageStatus.bind(client);
  let calls = 0;
  client.semanticCoverageStatus = async (...args) => {
    const status = await real(...args);
    // The review's own read passes; the authoritative post-review read names a different candidate.
    return ++calls < 2 ? status : { ...status, candidate_digest: "f".repeat(64) };
  };
  let thrown: unknown;
  const exec: Executor = { run: async ctx => {
    try { await ctx.gatePlan!("Implement A", milestones, undefined, undefined, submitted(c.issue_iid)); }
    catch (error) { thrown = error; throw error; }
    return { branch: ctx.branch };
  } };
  await runner(exec, fakeGitlab().gitlab, undefined, { semanticCoverageTestTransport: transport,
    planApprovalTimeoutMs: 4000 }).execute(c);
  assert.match(String(thrown), /Foreign semantic coverage status/);
  assert.equal(gates(c.run_id).length, 0);
});

function checkerAnswering(server: ReturnType<typeof serverWithAck>, verdict: "approve" | "block") {
  api.crossCheckHandler = ({ runId, body }) => {
    const { stage: _stage, claim_generation: _generation, round = 1, ...candidate } = body;
    const identity = server.compiled().candidate;
    return { status: 200, body: {
      result: "candidate", round, checker_run_id: "11111111-1111-4111-8111-111111111111",
      candidate_digest: identity.digest, candidate_generation: 1,
      candidate: { ...candidate, semantic_candidate: identity },
      verdict, reason_class: verdict, findings: null,
      automatic_revision_limit: 4, automatic_rounds_enabled: true,
      deadline_at: new Date(Date.now() + 60000).toISOString(),
      lead_last_seq: Math.max(0, ...api.messages(runId).map(m => m.seq)),
      claim_generation: 1, plan_cross_check_settled: true, gate_revision: 0, current_plan_sha256: hash(""),
    } };
  };
}

it("M4 complete coverage, no maintainers and a checker that did not approve parks with NO semantic_park", { skip: LINUX_CAPTURE }, async () => {
  const c = semanticClaim({ plan_cross_check_required: true });
  const events: string[] = [];
  const server = serverWithAck(events);
  checkerAnswering(server, "block");
  const transport = await boundedTransport("claude", server.report, events);
  api.onState(c.run_id, body => { if (body.status === "awaiting_approval") send(c.run_id, row("approve_plan")); });
  const exec: Executor = { run: async ctx => {
    const verdict = await ctx.gatePlan!("Implement A", milestones, undefined, undefined, submitted(c.issue_iid));
    assert.equal(verdict.kind, "approve");
    return { branch: ctx.branch };
  } };
  await runner(exec, fakeGitlab().gitlab, undefined, { semanticCoverageTestTransport: transport,
    planCrossCheckTiming: timing, planApprovalTimeoutMs: 4000 }).execute(c);
  assert.equal(server.status().outcome, "complete");
  const [gate] = gates(c.run_id);
  assert.ok(gate, JSON.stringify(api.states));
  assert.equal((gate as { plan_cross_check_gate_reason?: string }).plan_cross_check_gate_reason, "block");
  assert.equal(Object.hasOwn(gate, "semantic_park"), false);
});

it("M4 complete coverage with a checker APPROVE and no maintainers still auto-approves", { skip: LINUX_CAPTURE }, async () => {
  const c = semanticClaim({ plan_cross_check_required: true });
  const events: string[] = [];
  const server = serverWithAck(events);
  checkerAnswering(server, "approve");
  const transport = await boundedTransport("claude", server.report, events);
  const exec: Executor = { run: async ctx => {
    const verdict = await ctx.gatePlan!("Implement A", milestones, undefined, undefined, submitted(c.issue_iid));
    assert.ok(verdict.kind === "approve" && verdict.approval === "cross_check");
    return { branch: ctx.branch };
  } };
  await runner(exec, fakeGitlab().gitlab, undefined, { semanticCoverageTestTransport: transport,
    planCrossCheckTiming: timing, planApprovalTimeoutMs: 4000 }).execute(c);
  assert.equal(gates(c.run_id).length, 0, JSON.stringify(api.states));
});

it("M4 complete coverage without a required checker auto-approves and supplies no park", { skip: LINUX_CAPTURE }, async () => {
  const c = semanticClaim();
  const events: string[] = [];
  const server = serverWithAck(events);
  const transport = await boundedTransport("claude", server.report, events);
  const exec: Executor = { run: async ctx => {
    const verdict = await ctx.gatePlan!("Implement A", milestones, undefined, undefined, submitted(c.issue_iid));
    assert.equal(verdict.kind, "approve");
    return { branch: ctx.branch };
  } };
  await runner(exec, fakeGitlab().gitlab, undefined, { semanticCoverageTestTransport: transport,
    planApprovalTimeoutMs: 4000 }).execute(c);
  assert.equal(gates(c.run_id).length, 0);
  assert.ok(api.states.some(s => s.runId === c.run_id && s.body.status === "running" && s.body.plan_md !== undefined));
});

it("M4 a checker APPROVE never outranks incomplete coverage: it stays evidence and the human decides", { skip: LINUX_CAPTURE }, async () => {
  const c = semanticClaim({ plan_cross_check_required: true });
  const events: string[] = [];
  const server = serverWithAck(events);
  server.force("incomplete");
  checkerAnswering(server, "approve");
  const transport = await boundedTransport("claude", server.report, events);
  api.onState(c.run_id, body => { if (body.status === "awaiting_approval") send(c.run_id, row("approve_plan")); });
  const exec: Executor = { run: async ctx => {
    const verdict = await ctx.gatePlan!("Implement A", milestones, undefined, undefined, submitted(c.issue_iid));
    assert.ok(verdict.kind === "approve" && verdict.approval === undefined, "not a cross_check approval");
    return { branch: ctx.branch };
  } };
  await runner(exec, fakeGitlab().gitlab, undefined, { semanticCoverageTestTransport: transport,
    planCrossCheckTiming: timing, planApprovalTimeoutMs: 4000 }).execute(c);
  assert.equal(api.crossCheckRequests.length, 1, "the verdict is retained as historical evidence");
  const [gate] = gates(c.run_id);
  assert.equal(gate!.semantic_park, "coverage_incomplete");
  assert.equal((gate as { plan_cross_check_gate_reason?: unknown }).plan_cross_check_gate_reason, undefined,
    "the coverage reason is never recorded as checker provenance");
  assert.equal(events.filter(e => e === "compile").length, 1);
});

it("M4 the human-gate latch survives a human REVISE: a later complete candidate re-parks and never auto-approves", { skip: LINUX_CAPTURE }, async () => {
  const c = semanticClaim();
  const events: string[] = [];
  const server = serverWithAck(events);
  server.force("incomplete");
  const transport = await boundedTransport("claude", server.report, events);
  let presentations = 0;
  api.onState(c.run_id, body => {
    if (body.status !== "awaiting_approval") return;
    if (++presentations === 1) server.force(undefined);
    send(c.run_id, row(presentations === 1 ? "revise_plan" : "approve_plan", "owner decision"));
  });
  const exec: Executor = { run: async ctx => {
    const first = await ctx.gatePlan!("Implement A", milestones, undefined, undefined, submitted(c.issue_iid));
    assert.equal(first.kind, "revise");
    const next = await ctx.gatePlan!("Implement A with tests", milestones, undefined,
      first.kind === "revise" && !first.automatic ? first.inputId : undefined, submitted(c.issue_iid));
    assert.equal(next.kind, "approve");
    assert.ok(next.kind === "approve" && next.approval === undefined);
    return { branch: ctx.branch };
  } };
  await runner(exec, fakeGitlab().gitlab, undefined, { semanticCoverageTestTransport: transport,
    planApprovalTimeoutMs: 4000 }).execute(c);
  const parked = gates(c.run_id);
  assert.equal(parked.length, 2, JSON.stringify(api.states));
  assert.equal(parked[0]!.semantic_park, "coverage_incomplete");
  assert.equal(Object.hasOwn(parked[1]!, "semantic_park"), false, "the api cleared auto_approve at the first park");
  assert.equal(parked[1]!.semantic_park, undefined, "the second gate report carries no semantic_park");
  assert.equal(server.status().outcome, "complete", "the revised candidate itself had complete coverage");
  assert.equal(events.filter(e => e === "compile").length, 2);
});

it("M4 a manual run never supplies semantic_park", { skip: LINUX_CAPTURE }, async () => {
  const c = semanticClaim({ auto_approve: false });
  const events: string[] = [];
  const server = serverWithAck(events);
  server.force("incomplete");
  const transport = await boundedTransport("claude", server.report, events);
  api.onState(c.run_id, body => { if (body.status === "awaiting_approval") send(c.run_id, row("approve_plan")); });
  const exec: Executor = { run: async ctx => {
    assert.equal((await ctx.gatePlan!("Implement A", milestones, undefined, undefined, submitted(c.issue_iid))).kind, "approve");
    return { branch: ctx.branch };
  } };
  await runner(exec, fakeGitlab().gitlab, undefined, { semanticCoverageTestTransport: transport,
    planApprovalTimeoutMs: 4000 }).execute(c);
  const [gate] = gates(c.run_id);
  assert.ok(gate);
  assert.equal(Object.hasOwn(gate, "semantic_park"), false);
});

it("M4 a structural autopilot run never reads coverage status or supplies a park", { skip: LINUX_CAPTURE }, async () => {
  const c = freshClaim(1, { kind: "issue", auto_approve: true });
  const events: string[] = [];
  const server = serverWithAck(events);
  const transport = await boundedTransport("claude", server.report, events);
  const exec: Executor = { run: async ctx => {
    assert.equal((await ctx.gatePlan!("Implement A", milestones)).kind, "approve");
    return { branch: ctx.branch };
  } };
  await runner(exec, fakeGitlab().gitlab, undefined, { semanticCoverageTestTransport: transport }).execute(c);
  assert.deepEqual(events, []);
  assert.equal(gates(c.run_id).length, 0);
});

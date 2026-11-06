// Issue #3953 m3: a credential switch that arrives while the plan cross-check holds its candidate
// transport reservation is LATCHED (SteeringChannel.holdCredentialSwitch), never tripped into the
// reservation. It is serviced only after the reservation is verifiably released (S1 approve, S2
// automatic revise, S3 applied human gate), and the checker's decision is stashed for the give-up
// re-run. Driven through the real runner with the real SdkExecutor, as runner-plan-cross-check.test.ts.
import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import os from "node:os";
import { Outbox } from "../src/outbox.js";
import { RecoveryCoordinator, type RecoveryArchiveClient } from "../src/recovery.js";
import { CredentialSwitchSignal } from "../src/steering.js";
import { createHash } from "node:crypto";
import type { Executor, RunContext } from "../src/executor.js";
import { SdkExecutor, type SdkQueryFn } from "../src/sdk-executor.js";
import { MessageBatcher } from "../src/batcher.js";
import { CandidateTransportInvariantError, RunRunner } from "../src/runner.js";
import { SteeringChannel } from "../src/steering.js";
import type { PlanCrossCheckStateRequest } from "../src/client.js";
import type { OutgoingMessage } from "../src/protocol.js";
import type { Logger } from "../src/log.js";
import { nullLogger } from "./helpers.js";
import { mockCommittedTracking } from "./runner-tracking-fixture.js";
import { enrollment, proposal, boundedTransport, hash } from "./semantic-coverage-fixture.js";
import { semanticServer } from "./semantic-server-fixture.js";
import { api, client, fx, git, homeDir, installHarness, runner, runnerWith, fakeGitlab, worktreeDirFor,
  assistant, resultOk } from "./runner-harness.js";
import { freshClaim, gates, holdGateAck, newApi, routed, row, send, statuses, until } from "./gate-revision-harness.js";

installHarness();
beforeEach(() => {
  newApi();
  api.checkedTransport = true;
  api.usageHandler = () => ({ status: 200, body: {} });
});
// Prototype wrappers installed by watchSwitchDelivery are undone after every test.
const restorers: (() => void)[] = [];
// Waits inside api-fake handlers must not outlive their test (they would poll the next test's api).
let testOver = false;
beforeEach(() => { testOver = false; });
afterEach(() => { testOver = true; while (restorers.length) restorers.pop()!(); });
const handlerWait = (pred: () => boolean): Promise<boolean> => until(() => testOver || pred());

const PLAN = "# PLAN\n- local proposal";
const NORMALIZED = "# PLAN\n- server normalized proposal";
const digest = "a".repeat(64);
const child = "11111111-1111-4111-8111-111111111111";
const timing = { preparationMs: 100, backoffMs: [1, 2] as const, requestMs: 100, pollMs: 5 };
const SHA = "cafef00dcafef00dcafef00dcafef00dcafef00d";
// Runner paths that reach native planning capture walk Linux /proc; Linux CI runs them.
const LINUX_CAPTURE = process.platform !== "linux" && "Linux /proc capture required";

function claim(generation = 1) {
  return freshClaim(generation, { kind: "issue", auto_approve: true, plan_cross_check_required: true });
}
function proof(runId: string, generation = 1) {
  return { lead_last_seq: Math.max(0, ...api.messages(runId).map((m) => m.seq)),
    claim_generation: generation, plan_cross_check_settled: true, gate_revision: 0,
    current_plan_sha256: createHash("sha256").update("").digest("hex") };
}
function answer(runId: string, body: Record<string, unknown>, verdict = "approve", reason = verdict, overrides = {}) {
  const { stage: _stage, claim_generation: _generation, round = 1, ...candidate } = body;
  return { result: "candidate", round, checker_run_id: child, candidate_digest: digest,
    candidate_generation: 1, candidate, verdict, reason_class: reason, findings: null,
    deadline_at: new Date(Date.now() + 60_000).toISOString(), ...proof(runId), ...overrides };
}
const canonical = (body: Record<string, unknown>) => ({
  candidate: { ...body, plan_md: NORMALIZED, milestones: [], required_capabilities: [], required_tools: [], size_class: "s" } });

/** Terminal failure with `condition` in its reason, and no approval-bearing running report. */
function failedWith(runId: string, condition: string) {
  const states = api.states.filter((s) => s.runId === runId).map((s) => s.body);
  assert.ok(states.some((s) => s.status === "failed" && s.failure_reason?.includes(condition)),
    `terminal failure ${condition}; got ${JSON.stringify(states.map((s) => [s.status, s.failure_reason]))}`);
}
const everyState = (runId: string) => api.states.filter((s) => s.runId === runId).map((s) => s.body);
const texts = (runId: string): string[] =>
  api.messages(runId).map((m) => (m.payload as { text?: string }).text ?? "");
function assertSeqsUnique(runId: string): void {
  const seqs = api.messages(runId).map((m) => m.seq);
  assert.equal(new Set(seqs).size, seqs.length, `duplicate seqs in the message log: ${JSON.stringify(seqs)}`);
}

/** Clean-tree capture doubles: a verified capture releases the switch (runner-credential-switch.test.ts). */
function healthyCapture(): void {
  git.worktreeStatus = (async () => []) as typeof git.worktreeStatus;
  mockCommittedTracking(git, SHA);
  git.verifyRunnerTrackingCovers = (async () => true) as typeof git.verifyRunnerTrackingCovers;
  git.trackingTip = (async () => SHA) as typeof git.trackingTip;
  git.checkpointPack = (async () => null) as typeof git.checkpointPack;
}
/** Dirty-tree capture that never verifies: the switch gives up and the run continues in place. */
function failingCapture(): void {
  git.worktreeStatus = async () => ["M src/impl.ts"];
  git.commitWipMarker = async () => false;
}

interface Timeline { events: string[]; callsAt: Record<string, number>; counter: { calls: number } }
/** Observe the candidate reservation (reserve/release), landed state reports and landed messages in
 *  ONE ordered list. Returns the restore function for the shared batcher prototype. */
function observe(runId: string, tl: Timeline, opts: { prefix?: string; failAfterRelease?: boolean } = {}) {
  const reserve = MessageBatcher.prototype.reserveCandidateTransport;
  const reservations = { installed: 0, released: 0 };
  const noteRelease = (): void => {
    reservations.released++; tl.events.push("release");
    // The held events were assigned seqs by the release: fail their first flush (assigned replay).
    if (opts.failAfterRelease) api.failMessagesNext(1);
  };
  MessageBatcher.prototype.reserveCandidateTransport = function () {
    if (opts.prefix) {
      this.emit({ kind: "status", payload: { text: opts.prefix } });
      api.failMessagesNext(1);
    }
    const reservation = reserve.call(this);
    reservations.installed++;
    tl.events.push("reserve");
    return { ...reservation,
      release: (response) => {
        const released = reservation.release(response);
        if (released) noteRelease();
        return released;
      },
      releaseAppliedGate: (ack) => {
        const released = reservation.releaseAppliedGate(ack);
        if (released) noteRelease();
        return released;
      } };
  };
  const post = client.postMessages.bind(client);
  client.postMessages = async (id, msgs, generation, signal) => {
    const result = await post(id, msgs, generation, signal);
    if (id === runId) for (const m of msgs) {
      const text = (m.payload as { text?: string }).text;
      if (text) tl.events.push(`msg:${text}`);
    }
    return result;
  };
  const emit = MessageBatcher.prototype.emit;
  const refused: string[] = [];
  MessageBatcher.prototype.emit = function (...args: Parameters<typeof emit>) {
    try { return emit.apply(this, args); }
    catch (error) { refused.push(error instanceof Error ? error.name : String(error)); throw error; }
  };
  api.onState(runId, (b) => {
    tl.events.push(`state:${b.status}`);
    tl.callsAt[b.status] ??= tl.counter.calls;
  });
  return { reservations, refused, restore: () => {
    MessageBatcher.prototype.reserveCandidateTransport = reserve;
    MessageBatcher.prototype.emit = emit;
    client.postMessages = post;
  } };
}

/** Watch the switch reach the worker: `delivered` once an inputs poll carries it, `trips` counts the
 *  steering channel's completed handling of a switch signal, and `latched()` is the hold's own latch. */
function watchSwitchDelivery(generation = 1) {
  const getInputs = client.getInputs.bind(client);
  const handles: { latched(): boolean }[] = [];
  const seen = { delivered: false, trips: 0, latched: () => handles.some((h) => h.latched()) };
  client.getInputs = async (runId) => {
    const result = await getInputs(runId);
    if (result.credentialSwitch?.generation === generation) seen.delivered = true;
    return result;
  };
  const proto = SteeringChannel.prototype as unknown as {
    holdCredentialSwitch: () => { latched(): boolean }; maybeTripCredentialSwitch: (g: number) => void };
  const hold = proto.holdCredentialSwitch;
  const trip = proto.maybeTripCredentialSwitch;
  proto.holdCredentialSwitch = function (this: unknown) {
    const handle = hold.call(this);
    handles.push(handle);
    return handle;
  };
  proto.maybeTripCredentialSwitch = function (this: unknown, g: number) {
    try { return trip.call(this, g); } finally { seen.trips++; }
  };
  restorers.push(() => { proto.holdCredentialSwitch = hold; proto.maybeTripCredentialSwitch = trip; });
  return seen;
}

/** The reclaim lands on a worker with an empty disk (a cold reassignment): drop the retained clone and
 *  bare repository the released claim left, so the next claim seeds afresh instead of recovering them. */
function coldWorker(): void {
  // The capture doubles belong to the old claim; the new worker uses real Git again.
  for (const method of ["worktreeStatus", "commitWipMarker", "fetchAgentBranch", "committedTrackingOwnership",
    "verifyRunnerTrackingCovers", "trackingTip", "checkpointPack"])
    delete (git as unknown as Record<string, unknown>)[method];
  fs.rmSync(path.join(fx.dataDir, "runner"), { recursive: true, force: true });
  fs.rmSync(git.barePathFor(fx.originPath), { recursive: true, force: true });
}

/** The next claim (generation `generation`) of a released run: the server's last message seq rides it
 *  (claim.last_seq), the receipts move to the new generation, and the disk is cold. */
function reclaim(c: ReturnType<typeof claim>, generation = 2) {
  api.setInputClaimGeneration(c.run_id, generation);
  coldWorker();
  return { ...c, claim_generation: generation, last_seq: Math.max(0, ...api.messages(c.run_id).map((m) => m.seq)) };
}

/** A scripted SDK: turn n submits plans[n] when present, else finishes. */
function scriptedQuery(plans: string[], counter: { calls: number }): SdkQueryFn {
  return (params) => {
    const turn = counter.calls++;
    return (async function* () {
      for await (const _ of params.prompt) { /* drain */ }
      const plan = plans[turn];
      if (plan !== undefined) {
        yield assistant([{ type: "tool_use", id: `p${turn}`, name: "mcp__uzi__submit_plan", input: { plan_md: plan, milestones: [] } }]);
      } else {
        yield assistant([{ type: "text", text: "done implementing" },
          { type: "tool_use", id: `d${turn}`, name: "mcp__uzi__signal_done", input: {} }]);
      }
      yield resultOk();
    })();
  };
}

const PLAN2 = "# PLAN\n- revised proposal";
type Verdict = "approve" | "revise" | "block";
interface DriveOpts {
  gated?: boolean;
  verdicts?: Verdict[];
  /** Rounds during whose pending checker request a switch becomes visible to the worker. */
  switchRounds?: number[];
  /** poll: the inputs poll carries the switch. ack: the awaiting_approval state ACK carries it. */
  entry?: "poll" | "ack";
  capture: "release" | "giveup";
  plans?: string[];
  emitHeld?: boolean;
  failAfterRelease?: boolean;
  dropFirstPost?: boolean;
  /** Reject the human gate once the give-up is confirmed (a gate left presented by the old claim). */
  rejectAfterGiveUp?: boolean;
  /** Rewrite the plan markdown of the n-th (1-based) ctx.gatePlan call. */
  gateMd?: (n: number, md: string) => string;
  /** Rewrite the milestones of the n-th (1-based) ctx.gatePlan call. */
  gateMilestones?: (n: number, milestones: Parameters<NonNullable<RunContext["gatePlan"]>>[1]) => Parameters<NonNullable<RunContext["gatePlan"]>>[1];
  /** A forced human gate for the checker's revise/block instead of an automatic revision. */
  manual?: boolean;
  /** Run first on the pending checker request, whether or not a switch is requested. */
  duringPending?: (c: ReturnType<typeof claim>) => Promise<void>;
  /** Run once the switch is visible to the worker and the checker is still pending. */
  afterSwitch?: (c: ReturnType<typeof claim>) => Promise<void>;
  /** Arrange the api for this claim before the run starts. */
  setup?: (c: ReturnType<typeof claim>) => void;
  /** Extra runner options. */
  runnerOpts?: Record<string, unknown>;
}
/** Run one claim to its end through the real runner + real SdkExecutor + the cross-check api fake. */
async function drive(o: DriveOpts) {
  const c = o.gated
    ? freshClaim(1, { kind: "issue", auto_approve: false, plan_cross_check_gated: true })
    : claim();
  const counter = { calls: 0 };
  const tl: Timeline = { events: [], callsAt: {}, counter };
  const probe = observe(c.run_id, tl, { failAfterRelease: o.failAfterRelease });
  const switchSeen = watchSwitchDelivery();
  const decided = new Map<number, ReturnType<typeof answer>>();
  let lastRound = 0;
  let dropped = false;
  let emit!: RunContext["emit"];
  const trace = { queriesAtRequest: [] as number[], requests: 0 };
  api.crossCheckHandler = async ({ runId, method, body }) => {
    if (method === "GET") return { status: 200, body: decided.get(lastRound) };
    const round = Number(body.round);
    lastRound = round;
    if (!decided.has(round)) {
      await o.duringPending?.(c);
      if ((o.switchRounds ?? []).includes(round)) {
        trace.requests++;
        if (o.emitHeld) {
          emit({ kind: "status", payload: { text: `held during reservation ${round}A` } });
          emit({ kind: "status", payload: { text: `held during reservation ${round}B` } });
        }
        trace.queriesAtRequest.push(counter.calls);
        tl.events.push("switch-requested");
        if ((o.entry ?? "poll") === "poll") {
          api.requestCredentialSwitch(runId, 1);
          assert.ok(await handlerWait(() => switchSeen.delivered), "the worker polled the switch while the checker is pending");
          switchSeen.delivered = false;
          assert.ok(await handlerWait(() => switchSeen.latched()), "the hold latched the switch");
        } else api.armStateAckCredentialSwitchOnce(runId, 1, "awaiting_approval");
        await o.afterSwitch?.(c);
      }
      const verdict = (o.verdicts ?? ["approve"])[round - 1] ?? "approve";
      const generation = Number(body.claim_generation ?? 1);
      decided.set(round, answer(runId, body, verdict, verdict, {
        ...proof(runId, generation), candidate_generation: generation,
        ...(o.manual ? {} : { automatic_revision_limit: 3, automatic_rounds_enabled: true }),
        ...(verdict === "revise" ? { findings: { summary: `fix round ${round}`, items: [] } } : {}),
        ...(verdict === "approve" ? canonical(body) : {}),
      }));
    }
    const first = !dropped && (o.dropFirstPost ?? false);
    if (first) dropped = true;
    return { status: 200, body: decided.get(round), ...(first ? { drop: true } : {}) };
  };
  // Observe canonical storage (a running report carrying the checker's digest).
  const storage: PlanCrossCheckStateRequest[] = [];
  const report = client.reportPlanCrossCheckGateState.bind(client);
  client.reportPlanCrossCheckGateState = async (id, fields, signal) => {
    if (id === c.run_id && fields.status === "running" && fields.candidate_digest === digest) {
      storage.push(fields);
      tl.events.push("storage");
    }
    return report(id, fields, signal);
  };
  let gaveUp = false;
  const prior = api.onState.bind(api);
  const hook = (b: Parameters<Parameters<typeof prior>[1]>[0]) => {
    tl.events.push(`state:${b.status}`);
    tl.callsAt[b.status] ??= tl.counter.calls;
    if (b.status === "credential_switch_failed") {
      gaveUp = true;
      if (o.rejectAfterGiveUp) send(c.run_id, row("reject_plan", "human after give-up"));
    }
  };
  prior(c.run_id, hook);
  const attempts: string[] = [];
  let gateCalls = 0;
  const exec: Executor = { run: async (ctx) => {
    emit = ctx.emit;
    if (o.capture === "release") healthyCapture(); else failingCapture();
    const wip = git.worktreeStatus.bind(git);
    git.worktreeStatus = (async (...a: Parameters<typeof wip>) => { tl.events.push("capture"); return wip(...a); }) as typeof git.worktreeStatus;
    const attempt = ctx.attemptCredentialSwitch!;
    ctx.attemptCredentialSwitch = async () => {
      attempts.push("attempt"); tl.events.push("attempt");
      const outcome = await attempt();
      attempts.push(outcome);
      return outcome;
    };
    if (o.gateMd || o.gateMilestones) {
      const gate = ctx.gatePlan!;
      ctx.gatePlan = (md, ms, ...rest) => {
        const n = ++gateCalls;
        return gate(o.gateMd ? o.gateMd(n, md) : md, o.gateMilestones ? o.gateMilestones(n, ms) : ms, ...rest);
      };
    }
    return new SdkExecutor(nullLogger(), homeDir, { queryFn: scriptedQuery(o.plans ?? [PLAN], counter) }).run(ctx);
  } };
  o.setup?.(c);
  const forge = fakeGitlab();
  const sentinel = path.join(homeDir, "SESSION");
  fs.writeFileSync(sentinel, "resumable session\n");
  try {
    await runner(exec, forge.gitlab, undefined, { planCrossCheckTiming: timing, recoveryRetryMs: 1,
      planApprovalTimeoutMs: 3000, ...o.runnerOpts }).execute(c);
  } finally { probe.restore(); client.reportPlanCrossCheckGateState = report; }
  const heldSeqs = api.messages(c.run_id).filter((m) => (m.payload as { text?: string }).text?.startsWith("held during"))
    .map((m) => m.seq);
  return { c, tl, counter, forge, probe, storage, attempts, sentinel, trace, gaveUp: () => gaveUp, heldSeqs,
    posts: () => api.crossCheckRequests.filter((r) => r.method === "POST") };
}
type Driven = Awaited<ReturnType<typeof drive>>;

/** The held switch was serviced only after the candidate transport reservation was released. */
function assertServicedAfterRelease(d: Driven): void {
  const ev = d.tl.events;
  const release = ev.indexOf("release");
  assert.ok(release >= 0, `the reservation was released: ${JSON.stringify(ev)}`);
  assert.ok(ev.indexOf("attempt") > release, `switch attempt after release: ${JSON.stringify(ev)}`);
  const capture = ev.indexOf("capture");
  assert.ok(capture > release, `no capture while the reservation is installed: ${JSON.stringify(ev)}`);
  for (const status of ["credential_switch", "credential_switch_failed"]) {
    const at = ev.indexOf(`state:${status}`);
    if (at >= 0) assert.ok(at > capture, `${status} report only after capture: ${JSON.stringify(ev)}`);
  }
  assert.ok(d.probe.reservations.installed >= 1);
  assert.equal(d.probe.reservations.released, d.probe.reservations.installed, "every installed reservation was released before service");
  assert.deepEqual(d.probe.refused, [], "no emit was refused by a reservation");
}
const switchReports = (runId: string) => statuses(runId).filter((s) => s.startsWith("credential_switch"));

describe("issue #3953 m3: a held credential switch during the plan cross-check", () => {
  it("S1 release: the approve is serviced after the reservation releases, before any storage, and the next claim re-presents a human gate", { skip: LINUX_CAPTURE, timeout: 60_000 }, async () => {
    const d = await drive({ capture: "release", switchRounds: [1], emitHeld: true, failAfterRelease: true });
    const { c, tl } = d;
    const ev = tl.events;
    assertServicedAfterRelease(d);
    assert.ok(ev.indexOf("switch-requested") < ev.indexOf("release"));
    assert.deepEqual(d.attempts, ["attempt", "released"], "the held switch was serviced exactly once");
    assert.equal(ev.filter((e) => e === "state:credential_switch").length, 1);
    assert.deepEqual(switchReports(c.run_id), ["credential_switch"]);
    // Serviced before any storage / implementation.
    assert.deepEqual(d.storage, [], "the approve is never stored once the held switch is serviced");
    assert.equal(d.counter.calls, 1, "no model call after the switch request");
    assert.equal(d.trace.queriesAtRequest[0], 1);
    assert.equal(gates(c.run_id).length, 0, "no human gate on the old claim");
    assert.ok(!statuses(c.run_id).includes("failed") && !statuses(c.run_id).includes("completed"));
    // Held events: delivered exactly once, in order, before the release report, with unique seqs.
    for (const text of ["held during reservation 1A", "held during reservation 1B"]) {
      assert.equal(texts(c.run_id).filter((t) => t === text).length, 1, `${text} delivered exactly once`);
      assert.ok(ev.indexOf(`msg:${text}`) >= 0 && ev.indexOf(`msg:${text}`) < ev.indexOf("state:credential_switch"),
        `${text} precedes the credential_switch report: ${JSON.stringify(ev)}`);
    }
    assert.equal(d.heldSeqs.length, 2);
    assert.ok(d.heldSeqs[0]! < d.heldSeqs[1]!, "held events keep their order");
    assertSeqsUnique(c.run_id);
    assert.equal(d.forge.calls.length, 0);
    assert.equal(fs.existsSync(worktreeDirFor(Number(c.issue_iid))), true, "clone retained for recapture");
    assert.equal(fs.existsSync(d.sentinel), true, "HOME retained for the reclaim");
    assert.equal(d.posts().length, 1);

    // The NEXT claim (generation 2) on the same api never reuses the generation-1 approval.
    const before = api.messages(c.run_id).map((m) => `${m.seq}:${(m.payload as { text?: string }).text ?? m.kind}`);
    const c2 = reclaim(c);
    api.crossCheckLatestHandler = () => ({ status: 200, body: {
      result: "latest", round: 1, candidate_generation: 1, automatic_revision_limit: 2,
      automatic_rounds_enabled: true, next_round: null, next_round_eligible: false, fallback_reason: "approve" } });
    api.onState(c.run_id, (b) => { if (b.status === "awaiting_approval") send(c.run_id, row("reject_plan", "next claim human")); });
    const counter2 = { calls: 0 };
    const exec2: Executor = { run: async (ctx) =>
      new SdkExecutor(nullLogger(), homeDir, { queryFn: scriptedQuery([PLAN], counter2) }).run(ctx) };
    await runner(exec2, fakeGitlab().gitlab, undefined, { planCrossCheckTiming: timing, recoveryRetryMs: 1,
      planApprovalTimeoutMs: 3000 }).execute(c2);
    assert.equal(api.crossCheckLatestRequests.length, 1, "the next claim discovers once");
    assert.equal(d.posts().length, 1, "no second submit");
    const humanGates = gates(c.run_id) as PlanCrossCheckStateRequest[];
    assert.equal(humanGates.length, 1, "a human gate, not the generation-1 approval");
    assert.equal(humanGates[0]!.plan_cross_check_gate_reason, "interrupted");
    assert.ok(!everyState(c.run_id).some((s) => s.status === "running" && s.candidate_digest === digest),
      "the generation-1 approval was never stored");
    assertSeqsUnique(c.run_id);
    const after = api.messages(c.run_id).map((m) => `${m.seq}:${(m.payload as { text?: string }).text ?? m.kind}`);
    assert.deepEqual(after.slice(0, before.length), before, "earlier events are intact");
  });

  it("S1 give-up: the re-run adopts the stashed approve: no second submit, canonical storage with its digest, implementation starts", { skip: LINUX_CAPTURE, timeout: 60_000 }, async () => {
    const d = await drive({ capture: "giveup", switchRounds: [1], emitHeld: true });
    const { c, tl } = d;
    assertServicedAfterRelease(d);
    assert.deepEqual(d.attempts, ["attempt", "gave_up"]);
    assert.deepEqual(switchReports(c.run_id), ["credential_switch_failed"], "server-confirmed give-up, no release");
    assert.equal(d.counter.calls, 1 + 1, "planning turn, then implementation only");
    assert.equal(tl.callsAt["credential_switch_failed"], 1, "no model call between the request and the give-up");
    assert.equal(d.posts().length, 1, "the adopted decision is never resubmitted");
    assert.equal(d.storage.length, 1, "canonical plan stored once");
    assert.equal(d.storage[0]!.plan_md, NORMALIZED);
    assert.equal(d.storage[0]!.candidate_digest, digest);
    assert.ok(tl.events.indexOf("storage") > tl.events.indexOf("state:credential_switch_failed"));
    assert.equal(gates(c.run_id).length, 0);
    assert.ok(statuses(c.run_id).includes("completed"), JSON.stringify(statuses(c.run_id)));
    assert.ok(!statuses(c.run_id).includes("failed"));
    for (const text of ["held during reservation 1A", "held during reservation 1B"])
      assert.equal(texts(c.run_id).filter((t) => t === text).length, 1);
    assertSeqsUnique(c.run_id);
  });

  it("S2 give-up: an automatic revise decided under a held switch is serviced before the revision turn, which runs at nextRound", { skip: LINUX_CAPTURE, timeout: 60_000 }, async () => {
    const d = await drive({ capture: "giveup", switchRounds: [1], verdicts: ["revise", "approve"], plans: [PLAN, PLAN2] });
    const { c, tl } = d;
    assertServicedAfterRelease(d);
    assert.deepEqual(d.attempts, ["attempt", "gave_up"]);
    assert.equal(tl.callsAt["credential_switch_failed"], 1, "serviced before the revision turn's model call");
    assert.deepEqual(d.posts().map((r) => r.body.round), [1, 2], "the revision candidate is round two");
    assert.equal(d.posts()[1]!.body.plan_md, PLAN2, "round two carries the revision turn's plan");
    assert.equal(d.counter.calls, 3, "plan, one revision turn, implementation");
    assert.equal(d.storage.length, 1);
    assert.ok(statuses(c.run_id).includes("completed"));
    assert.deepEqual(switchReports(c.run_id), ["credential_switch_failed"]);
  });

  it("S2 release: the next claim selects the next round by discovery without replaying the consumed round", { skip: LINUX_CAPTURE, timeout: 60_000 }, async () => {
    const d = await drive({ capture: "release", switchRounds: [1], verdicts: ["revise", "approve"] });
    const { c } = d;
    assertServicedAfterRelease(d);
    assert.deepEqual(d.attempts, ["attempt", "released"]);
    assert.equal(d.counter.calls, 1, "no revision turn on the released claim");
    assert.deepEqual(switchReports(c.run_id), ["credential_switch"]);
    assert.deepEqual(d.posts().map((r) => r.body.round), [1]);
    // The reclaim: the server's latest metadata offers round two.
    const c2 = reclaim(c);
    api.crossCheckLatestHandler = () => ({ status: 200, body: {
      result: "latest", round: 1, candidate_generation: 1, automatic_revision_limit: 3,
      automatic_rounds_enabled: true, next_round: 2, next_round_eligible: true, fallback_reason: "" } });
    const counter2 = { calls: 0 };
    api.crossCheckHandler = ({ runId, body }) => ({ status: 200, body: answer(runId, body, "approve", "approve", {
      ...proof(runId, 2), candidate_generation: 2, automatic_revision_limit: 3, automatic_rounds_enabled: true,
      ...canonical(body) }) });
    const exec2: Executor = { run: async (ctx) => {
      return new SdkExecutor(nullLogger(), homeDir, { queryFn: scriptedQuery([PLAN2], counter2) }).run(ctx);
    } };
    await runner(exec2, fakeGitlab().gitlab, undefined, { planCrossCheckTiming: timing, recoveryRetryMs: 1,
      planApprovalTimeoutMs: 3000 }).execute(c2);
    assert.deepEqual(api.crossCheckLatestRequests, [{ runId: c.run_id, generation: 2 }]);
    assert.deepEqual(api.crossCheckRequests.filter((r) => r.method === "POST").map((r) => r.body.round), [1, 2],
      "round one is not replayed on the reclaim");
    assert.equal(counter2.calls, 2, `planning turn and implementation ${JSON.stringify(statuses(c.run_id))}`);
    assert.ok(statuses(c.run_id).includes("completed"), JSON.stringify(statuses(c.run_id)));
    assertSeqsUnique(c.run_id);
  });
});

/** Re-present check shared by the S3 release cases: the reclaim's discovery re-presents a human gate. */
async function reclaimAndExpectRepresent(c: ReturnType<typeof claim>, fallback: string, reason: string, gatedRun: boolean) {
  const c2 = reclaim(c);
  api.crossCheckLatestHandler = () => ({ status: 200, body: {
    result: "latest", round: 1, candidate_generation: 1, automatic_revision_limit: 3,
    automatic_rounds_enabled: true, next_round: null, next_round_eligible: false, fallback_reason: fallback } });
  api.onState(c.run_id, (b) => { if (b.status === "awaiting_approval") send(c.run_id, row("reject_plan", "next claim human")); });
  const counter2 = { calls: 0 };
  const exec2: Executor = { run: async (ctx) =>
    new SdkExecutor(nullLogger(), homeDir, { queryFn: scriptedQuery([PLAN], counter2) }).run(ctx) };
  await runner(exec2, fakeGitlab().gitlab, undefined, { planCrossCheckTiming: timing, recoveryRetryMs: 1,
    planApprovalTimeoutMs: 3000 }).execute(c2);
  const all = gates(c.run_id) as PlanCrossCheckStateRequest[];
  assert.equal(all.length, 2, `the old claim presented once, the reclaim re-presents once (gated=${gatedRun})`);
  assert.equal(all[1]!.plan_cross_check_gate_reason, reason);
  assert.equal(api.crossCheckRequests.filter((r) => r.method === "POST").length, 1, "the reclaim submits no second candidate");
  assertSeqsUnique(c.run_id);
}

describe("issue #3953 m3: S3 applied human gate", () => {
  for (const [name, gated, verdict, reason] of [
    ["forced human gate (checker block)", false, "block", "block"],
    ["gated_pass (checker approved, human still approves)", true, "approve", "interrupted"],
  ] as const) {
    it(`${name}: give-up keeps the presented gate, its binding and its verdict path with no republish`, { skip: LINUX_CAPTURE, timeout: 60_000 }, async () => {
      const d = await drive({ gated, capture: "giveup", verdicts: [verdict], switchRounds: [1], rejectAfterGiveUp: true });
      const { c, tl } = d;
      assertServicedAfterRelease(d);
      assert.deepEqual(d.attempts, ["attempt", "gave_up"]);
      assert.deepEqual(switchReports(c.run_id), ["credential_switch_failed"]);
      // Service only after the applied gate released the reservation (releaseAppliedGate).
      assert.ok(tl.events.indexOf("state:awaiting_approval") < tl.events.indexOf("release"));
      const presented = gates(c.run_id) as PlanCrossCheckStateRequest[];
      assert.equal(presented.length, 1, "no republish after the give-up");
      assert.ok(presented[0]!.presentation_id, "the presentation identity is intact");
      const rejects = api.inputRows(c.run_id).filter((r) => r.kind === "reject_plan");
      assert.equal(rejects.length, 1);
      assert.ok(api.isApplied(c.run_id, rejects[0]!.id), "the human verdict bound to the original gate revision applied");
      assert.ok(everyState(c.run_id).some((s) => s.status === "failed" && s.failure_reason?.includes("human after give-up")),
        JSON.stringify(everyState(c.run_id).map((s) => [s.status, s.failure_reason])));
      assert.equal(d.posts().length, 1, "no second candidate");
      assert.equal(d.counter.calls, 1, "no model call after the switch request");
      assert.deepEqual(d.storage, []);
      assertSeqsUnique(c.run_id);
    });

    it(`${name}: release lets the next claim re-present`, { skip: LINUX_CAPTURE, timeout: 60_000 }, async () => {
      const d = await drive({ gated, capture: "release", verdicts: [verdict], switchRounds: [1], emitHeld: true });
      const { c } = d;
      assertServicedAfterRelease(d);
      assert.deepEqual(d.attempts, ["attempt", "released"]);
      assert.deepEqual(switchReports(c.run_id), ["credential_switch"]);
      assert.equal((gates(c.run_id) as PlanCrossCheckStateRequest[]).length, 1);
      for (const text of ["held during reservation 1A", "held during reservation 1B"])
        assert.equal(texts(c.run_id).filter((t) => t === text).length, 1, `${text} delivered once`);
      await reclaimAndExpectRepresent(c, verdict === "block" ? "block" : "approve", reason, gated);
    });
  }

  for (const capture of ["giveup", "release"] as const) {
    it(`retryable preparation (held human gate): a switch visible while the forced gate ACK is held is serviced after the release (${capture})`, { skip: LINUX_CAPTURE, timeout: 60_000 }, async () => {
      const c = claim();
      const counter = { calls: 0 };
      const tl: Timeline = { events: [], callsAt: {}, counter };
      const probe = observe(c.run_id, tl, { prefix: "switch prefix" });
      // The switch becomes visible while the forced gate's ACK is held, i.e. with the reservation installed.
      const held = holdGateAck(c.run_id, PLAN);
      const switchSeen = watchSwitchDelivery();
      let requested = false;
      api.onState(c.run_id, (b) => {
        tl.events.push(`state:${b.status}`);
        if (b.status === "awaiting_approval" && !requested) {
          requested = true;
          tl.events.push("switch-requested");
          api.requestCredentialSwitch(c.run_id, 1);
        }
        if (b.status === "credential_switch_failed") send(c.run_id, row("reject_plan", "reject restored preparation"));
      });
      const attempts: string[] = [];
      const exec: Executor = { run: async (ctx) => {
        if (capture === "release") healthyCapture(); else failingCapture();
        const wip = git.worktreeStatus.bind(git);
        git.worktreeStatus = (async (...a: Parameters<typeof wip>) => { tl.events.push("capture"); return wip(...a); }) as typeof git.worktreeStatus;
        const attempt = ctx.attemptCredentialSwitch!;
        ctx.attemptCredentialSwitch = async () => { tl.events.push("attempt"); const o = await attempt(); attempts.push(o); return o; };
        return new SdkExecutor(nullLogger(), homeDir, { queryFn: scriptedQuery([PLAN], counter) }).run(ctx);
      } };
      const done = runner(exec, fakeGitlab().gitlab, undefined, { planCrossCheckTiming: timing, recoveryRetryMs: 1,
        planApprovalTimeoutMs: 3000 }).execute(c);
      try {
        assert.ok(await until(() => requested));
        assert.ok(await until(() => switchSeen.delivered), "the worker polled the switch while the gate ACK is held");
        assert.ok(await until(() => switchSeen.latched()), "the hold latched the switch");
        held.release();
        await done;
      } finally { held.release(); probe.restore(); }
      const ev = tl.events;
      assert.deepEqual(attempts, [capture === "giveup" ? "gave_up" : "released"]);
      assert.ok(ev.indexOf("release") > ev.indexOf("state:awaiting_approval"), "the applied forced gate releases the reservation");
      assert.ok(ev.indexOf("capture") > ev.indexOf("release"), `no capture while the reservation is installed: ${JSON.stringify(ev)}`);
      assert.equal(texts(c.run_id).filter((t) => t === "switch prefix").length, 1, "assigned prefix delivered exactly once");
      assert.ok(ev.indexOf("msg:switch prefix") < ev.indexOf(capture === "giveup" ? "state:credential_switch_failed" : "state:credential_switch"));
      assert.equal((gates(c.run_id) as PlanCrossCheckStateRequest[]).length, 1, "no second publication");
      assert.equal(api.crossCheckRequests.length, 0, "failed preparation never submits");
      assert.deepEqual(probe.refused, []);
      if (capture === "giveup") {
        assert.ok(everyState(c.run_id).some((s) => s.status === "failed" && s.failure_reason?.includes("reject restored preparation")));
      } else {
        assert.deepEqual(switchReports(c.run_id), ["credential_switch"]);
        assert.ok(!statuses(c.run_id).includes("failed"));
      }
      assertSeqsUnique(c.run_id);
    });
  }
});

describe("issue #3953 m3: stash lifetime, ambiguous submit and pre-reserve switches", () => {
  it("a switch during reclaimed-claim discovery (before any reservation) re-selects and re-discovers; no state barrier hangs", { skip: LINUX_CAPTURE, timeout: 60_000 }, async () => {
    const c = freshClaim(2, { kind: "issue", auto_approve: true, plan_cross_check_required: true });
    const counter = { calls: 0 };
    const tl: Timeline = { events: [], callsAt: {}, counter };
    const probe = observe(c.run_id, tl);
    const switchSeen = watchSwitchDelivery(2);
    let latestCalls = 0;
    api.crossCheckLatestHandler = async ({ runId }) => {
      latestCalls++;
      if (latestCalls === 1) {
        tl.events.push("switch-requested");
        api.requestCredentialSwitch(runId, 2);
        assert.ok(await handlerWait(() => switchSeen.delivered), "the worker polled the switch during discovery");
        assert.ok(await handlerWait(() => switchSeen.trips > 0), "the channel tripped the switch (no hold is open yet)");
      }
      return { status: 200, body: { result: "no_row", round: 0, candidate_generation: 0,
        automatic_revision_limit: 0, automatic_rounds_enabled: false, next_round: 1, next_round_eligible: true, fallback_reason: "" } };
    };
    api.crossCheckHandler = ({ runId, body }) => ({ status: 200, body: answer(runId, body, "approve", "approve", {
      ...proof(runId, 2), candidate_generation: 2, ...canonical(body) }) });
    const attempts: string[] = [];
    const exec: Executor = { run: async (ctx) => {
      failingCapture();
      const attempt = ctx.attemptCredentialSwitch!;
      ctx.attemptCredentialSwitch = async () => { const o = await attempt(); attempts.push(o); return o; };
      return new SdkExecutor(nullLogger(), homeDir, { queryFn: scriptedQuery([PLAN], counter) }).run(ctx);
    } };
    try {
      await runner(exec, fakeGitlab().gitlab, undefined, { planCrossCheckTiming: timing, recoveryRetryMs: 1,
        planApprovalTimeoutMs: 3000 }).execute(c);
    } finally { probe.restore(); }
    assert.deepEqual(attempts, ["gave_up"]);
    assert.equal(latestCalls, 2, "the give-up re-run re-discovers");
    assert.equal(api.crossCheckRequests.filter((r) => r.method === "POST").length, 1, "one submit, after the second discovery");
    assert.deepEqual(switchReports(c.run_id), ["credential_switch_failed"]);
    assert.ok(statuses(c.run_id).includes("completed"), JSON.stringify(statuses(c.run_id)));
    assert.ok(!statuses(c.run_id).includes("failed"));
    assert.equal(gates(c.run_id).length, 0);
    assert.equal(probe.reservations.installed, 1, "the reservation only exists after the re-run's discovery");
    assert.ok(tl.events.indexOf("reserve") > tl.events.indexOf("state:credential_switch_failed"));
  });

  for (const switchInRoundOne of [false, true]) {
    it(`a switch in the later round does not replay the consumed round (round-one switch: ${switchInRoundOne})`, { skip: LINUX_CAPTURE, timeout: 60_000 }, async () => {
      const d = await drive({ capture: "giveup", verdicts: ["revise", "approve"], plans: [PLAN, PLAN2],
        switchRounds: switchInRoundOne ? [1, 2] : [2] });
      const { c } = d;
      assert.deepEqual(d.attempts, switchInRoundOne ? ["attempt", "gave_up", "attempt", "gave_up"] : ["attempt", "gave_up"]);
      assert.deepEqual(d.posts().map((r) => r.body.round), [1, 2], "each round is submitted exactly once");
      assert.deepEqual(d.posts().map((r) => r.body.plan_md), [PLAN, PLAN2]);
      assert.equal(d.counter.calls, 3, "plan, one revision turn, implementation: the consumed revise is not replayed");
      assert.equal(d.storage.length, 1);
      assert.equal(d.storage[0]!.plan_md, NORMALIZED);
      assert.ok(statuses(c.run_id).includes("completed"), JSON.stringify(statuses(c.run_id)));
      assert.equal(gates(c.run_id).length, 0);
      assertSeqsUnique(c.run_id);
    });
  }

  it("an ordinary automatic revise with no switch adopts no stash and replays nothing: one revision turn, each round submitted once", { skip: LINUX_CAPTURE, timeout: 60_000 }, async () => {
    const d = await drive({ capture: "giveup", verdicts: ["revise", "approve"], plans: [PLAN, PLAN2], switchRounds: [] });
    const { c } = d;
    assert.deepEqual(d.attempts, [], "no switch was ever serviced");
    assert.deepEqual(d.posts().map((r) => r.body.round), [1, 2], "each round is submitted exactly once");
    assert.deepEqual(d.posts().map((r) => r.body.plan_md), [PLAN, PLAN2]);
    assert.equal(d.counter.calls, 3, "plan, exactly one revision turn, implementation");
    assert.equal(d.storage.length, 1, "canonical plan stored once, from the round-two checker answer");
    assert.equal(d.storage[0]!.plan_md, NORMALIZED);
    assert.deepEqual(switchReports(c.run_id), []);
    assert.ok(statuses(c.run_id).includes("completed"), JSON.stringify(statuses(c.run_id)));
    assert.ok(!statuses(c.run_id).includes("failed"));
    assert.equal(gates(c.run_id).length, 0);
    assertSeqsUnique(c.run_id);
  });

  it("a non-switch failure after a stashed revise was consumed never replays the stale revise", { skip: LINUX_CAPTURE, timeout: 60_000 }, async () => {
    // Round one's revise is decided under a latched switch (stashed), serviced by a give-up and consumed by
    // the continuation. Round two's checker request then fails outright (a non-switch exit) and the run
    // falls back to a human gate; a second switch there must continue that gate, not replay round one's revise.
    let gateSwitch = false;
    const d = await drive({ capture: "giveup", switchRounds: [1], verdicts: ["revise", "approve"], plans: [PLAN, PLAN2],
      duringPending: async () => { if (api.crossCheckRequests.some((r) => r.method === "POST" && Number(r.body.round) === 2)) throw new Error("checker transport down"); },
      setup: (c) => api.onState(c.run_id, (b) => {
        if (b.status === "awaiting_approval" && !gateSwitch) { gateSwitch = true; api.requestCredentialSwitch(c.run_id, 1); }
      }) });
    const { c } = d;
    assert.ok(gateSwitch, "the human gate was presented");
    assert.deepEqual(d.attempts, ["attempt", "gave_up", "attempt", "gave_up"], "the round-one switch and the human-gate switch");
    const rounds = d.posts().map((r) => Number(r.body.round));
    assert.equal(rounds.filter((r) => r === 1).length, 1, `round one is submitted once: ${JSON.stringify(rounds)}`);
    assert.ok(rounds.every((r) => r === 1 || r === 2), JSON.stringify(rounds));
    assert.equal(d.counter.calls, 2, "plan and one revision turn; the failed round neither replays the stale revise nor implements");
    assert.deepEqual(d.storage, []);
    assert.ok(!statuses(c.run_id).includes("completed"));
    assert.equal(gates(c.run_id).length, 1, "the failed round falls back to one human gate, not the stale decision");
    failedWith(c.run_id, "plan approval timed out");
    assert.ok(!everyState(c.run_id).some((s) => s.failure_reason?.includes("invalid automatic plan revision round")),
      "the stale round-one revise was not replayed at the human gate");
    assertSeqsUnique(c.run_id);
  });

  it("a stashed approval that does not match the re-presented candidate fails closed",{ skip: LINUX_CAPTURE, timeout: 60_000 }, async () => {
    const d = await drive({ capture: "giveup", switchRounds: [1], gateMd: (n, md) => n === 1 ? md : `${md}\n- changed after the give-up` });
    const { c } = d;
    failedWith(c.run_id, "stashed decision does not match the re-presented candidate");
    assert.deepEqual(d.storage, [], "the stale approval authorizes nothing");
    assert.equal(d.posts().length, 1);
    assert.ok(!statuses(c.run_id).includes("completed"));
    assert.equal(d.counter.calls, 1, "no implementation");
    assert.equal(gates(c.run_id).length, 0);
  });

  it("a stashed approval whose milestones differ from the re-presented candidate fails closed", { skip: LINUX_CAPTURE, timeout: 60_000 }, async () => {
    const d = await drive({ capture: "giveup", switchRounds: [1],
      gateMilestones: (n, ms) => n === 1 ? ms : [...(ms ?? []), { id: "m-extra", title: "added after the give-up" }] });
    const { c } = d;
    failedWith(c.run_id, "stashed decision does not match the re-presented candidate");
    assert.deepEqual(d.storage, [], "the stale approval authorizes nothing");
    assert.equal(d.posts().length, 1);
    assert.ok(!statuses(c.run_id).includes("completed"));
    assert.equal(d.counter.calls, 1, "no implementation");
    assert.equal(gates(c.run_id).length, 0);
  });

  for (const capture of ["giveup", "release"] as const) {
    it(`an accepted submit whose response is lost, recovered by status while a switch is pending (${capture})`, { skip: LINUX_CAPTURE, timeout: 60_000 }, async () => {
      const d = await drive({ capture, switchRounds: [1], dropFirstPost: true, emitHeld: true });
      const { c } = d;
      assertServicedAfterRelease(d);
      assert.equal(api.crossCheckReplies.filter((r) => r.acceptedCandidate && r.dropped).length, 1);
      assert.deepEqual(api.crossCheckRequests.map((r) => r.method), ["POST", "GET"], "status recovery, never a second submit");
      assert.equal(d.counter.calls, capture === "giveup" ? 2 : 1);
      for (const text of ["held during reservation 1A", "held during reservation 1B"])
        assert.equal(texts(c.run_id).filter((t) => t === text).length, 1);
      assertSeqsUnique(c.run_id);
      if (capture === "giveup") {
        assert.equal(d.storage.length, 1);
        assert.equal(d.storage[0]!.candidate_digest, digest);
        assert.ok(statuses(c.run_id).includes("completed"));
      } else {
        assert.deepEqual(d.storage, []);
        assert.deepEqual(switchReports(c.run_id), ["credential_switch"]);
      }
    });
  }
});

describe("issue #3953 m3: boundaries keep their dispositions; the latch is never serviced", () => {
  const noSwitchReports = (runId: string) => assert.deepEqual(switchReports(runId), [], "no credential_switch report of any kind");

  it("an owner cancel row sent during the hold, with no switch, ends the run as cancelled; nothing is stored", { skip: LINUX_CAPTURE, timeout: 60_000 }, async () => {
    const d = await drive({ capture: "release", switchRounds: [], duringPending: async (c) => {
      const [cancel] = send(c.run_id, row("cancel"));
      await routed(c.run_id, cancel!.id);
    } });
    assert.deepEqual(d.storage, [], "no approval is stored after a cancel");
    assert.equal(d.counter.calls, 1);
    assert.ok(!statuses(d.c.run_id).includes("completed"));
    failedWith(d.c.run_id, "run cancelled");
    noSwitchReports(d.c.run_id);
    assert.deepEqual(d.attempts, []);
    assert.deepEqual(d.tl.events.filter((e) => e === "capture"), [], "the latch is never serviced");
  });

  it("an owner cancel row sent while a latched switch is pending is withheld until the switch is serviced; the release leaves it for the reclaim", { skip: LINUX_CAPTURE, timeout: 60_000 }, async () => {
    let cancelId = 0;
    // The cancel row is only sent; nothing waits on it (the api withholds every row while a switch is pending).
    const d = await drive({ capture: "release", switchRounds: [1], afterSwitch: async (c) => {
      cancelId = send(c.run_id, row("cancel"))[0]!.id;
    } });
    const { c } = d;
    assert.ok(cancelId > 0);
    assert.deepEqual(switchReports(c.run_id), ["credential_switch"]);
    assert.ok(!statuses(c.run_id).includes("failed"), JSON.stringify(statuses(c.run_id)));
    const releaseAt = api.timeline.findIndex((e) => e.type === "state" && e.runId === c.run_id && e.status === "credential_switch");
    assert.ok(releaseAt >= 0);
    assert.deepEqual(api.timeline.slice(0, releaseAt).filter((e) => e.type !== "state" && e.runId === c.run_id && e.ids.includes(cancelId)), [],
      "the withheld row was never ACKed or receipted before the credential_switch report");
    assert.equal(api.isAcked(c.run_id, cancelId), false);
    assert.equal(api.isApplied(c.run_id, cancelId), false, "the row stays unapplied for the reclaim");
    assert.deepEqual(d.storage, []);
    assert.equal(d.counter.calls, 1);
    // The reclaim receives the row and ends cancelled.
    const c2 = reclaim(c);
    api.crossCheckLatestHandler = () => ({ status: 200, body: {
      result: "latest", round: 1, candidate_generation: 1, automatic_revision_limit: 2,
      automatic_rounds_enabled: true, next_round: null, next_round_eligible: false, fallback_reason: "approve" } });
    const exec2: Executor = { run: async (ctx) =>
      new SdkExecutor(nullLogger(), homeDir, { queryFn: scriptedQuery([PLAN], { calls: 0 }) }).run(ctx) };
    await runner(exec2, fakeGitlab().gitlab, undefined, { planCrossCheckTiming: timing, recoveryRetryMs: 1,
      planApprovalTimeoutMs: 3000 }).execute(c2);
    failedWith(c.run_id, "run cancelled");
    assert.equal(api.isApplied(c.run_id, cancelId), true, "the reclaim applied the very row that was queued during the hold");
  });

  it("an owner cancel row queued during the hold is delivered and applied after a give-up; the run ends cancelled with no approval stored", { skip: LINUX_CAPTURE, timeout: 60_000 }, async () => {
    let cancelId = 0;
    // The adopted approve's canonical storage is held until the queued cancel row has been ACKed, so the
    // order "cancel before storage" is fixed rather than a race between two polls.
    const base = client.reportPlanCrossCheckGateState.bind(client);
    client.reportPlanCrossCheckGateState = async (id, fields, signal) => {
      if (fields.status === "running" && fields.candidate_digest === digest)
        await until(() => cancelId > 0 && api.isAcked(id, cancelId));
      return base(id, fields, signal);
    };
    const d = await drive({ capture: "giveup", switchRounds: [1], afterSwitch: async (c) => {
      cancelId = send(c.run_id, row("cancel"))[0]!.id;
    } });
    const { c } = d;
    const ev = d.tl.events;
    assert.ok(cancelId > 0);
    assert.deepEqual(d.attempts, ["attempt", "gave_up"]);
    assert.deepEqual(switchReports(c.run_id), ["credential_switch_failed"]);
    const gaveUpAt = api.timeline.findIndex((e) => e.type === "state" && e.runId === c.run_id && e.status === "credential_switch_failed");
    assert.ok(gaveUpAt >= 0);
    const receipts = api.timeline.map((e, i) => ({ e, i })).filter(({ e }) => e.type !== "state" && e.runId === c.run_id && e.ids.includes(cancelId));
    assert.ok(receipts.length > 0, "the queued row was delivered after the give-up");
    assert.ok(receipts.every(({ i }) => i > gaveUpAt), "the row was not ACKed or receipted before the give-up report");
    assert.ok(api.isAcked(c.run_id, cancelId), "ACKed after the give-up");
    assert.ok(api.isApplied(c.run_id, cancelId), "applied after the give-up");
    failedWith(c.run_id, "run cancelled");
    assert.ok(!statuses(c.run_id).includes("completed"));
    assert.ok(!everyState(c.run_id).some((x) => x.status === "running" && x.candidate_digest === digest),
      "no approval is stored once the cancel is applied");
    assert.equal(d.counter.calls, 1, "no implementation turn starts");
    assert.equal(d.posts().length, 1);
    assert.ok(ev.indexOf("capture") > ev.indexOf("release"), JSON.stringify(ev));
    // A cancel ends the run: its clone is not kept for a reclaim (no release happened).
    assert.equal(fs.existsSync(worktreeDirFor(Number(c.issue_iid))), false, "the cancelled run's clone is cleaned up");
  });

  it("a server terminal cancel (getInputs terminalCancelled) during a hold with a latched switch ends the flight: no switch attempt, capture, gate or storage", { skip: LINUX_CAPTURE, timeout: 60_000 }, async () => {
    const injected = { calls: 0 };
    const getInputs = client.getInputs.bind(client);
    const d = await drive({ capture: "release", switchRounds: [1], afterSwitch: async (c) => {
      // The switch is latched under the hold; now the server reports the run terminally cancelled.
      client.getInputs = async (runId, ...rest) => {
        if (runId === c.run_id) { injected.calls++; return { inputs: [], terminalCancelled: { run_id: runId, generation: 1 } }; }
        return getInputs(runId, ...rest);
      };
      await handlerWait(() => injected.calls > 0);
    } });
    assert.ok(injected.calls > 0);
    const states = statuses(d.c.run_id);
    assert.deepEqual(switchReports(d.c.run_id), []);
    assert.deepEqual(d.attempts, [], "the latch is never serviced");
    assert.deepEqual(d.tl.events.filter((e) => e === "capture"), []);
    assert.equal(gates(d.c.run_id).length, 0);
    assert.deepEqual(d.storage, []);
    assert.equal(d.counter.calls, 1);
    assert.ok(!states.includes("completed") && !states.includes("failed"), `fenced flight reports no terminal: ${JSON.stringify(states)}`);
  });

  it("a claim fence during the hold ends the flight quietly; the latch is never serviced", { skip: LINUX_CAPTURE, timeout: 60_000 }, async () => {
    const d = await drive({ capture: "release", switchRounds: [], duringPending: async (c) => {
      api.setInputClaimGeneration(c.run_id, 7);
      send(c.run_id, row("revise_plan", "fence probe"));
      assert.ok(await until(() => api.timeline.some((e) => e.type === "receipt_reply" && e.httpStatus === 409)),
        "the fenced receipt was refused");
    } });
    noSwitchReports(d.c.run_id);
    assert.deepEqual(d.attempts, []);
    assert.deepEqual(d.storage, []);
    assert.ok(!statuses(d.c.run_id).includes("completed"));
  });

  it("an unrecoverable presentation ACK keeps its terminal failure with a latched switch; no switch report", { skip: LINUX_CAPTURE, timeout: 60_000 }, async () => {
    const d = await drive({ capture: "release", switchRounds: [1], verdicts: ["revise"], manual: true,
      setup: (c) => api.failStateWhen(c.run_id, (b) => b.status === "awaiting_approval", { httpStatus: 400 }) });
    failedWith(d.c.run_id, "human-presentation ACK unrecoverable");
    noSwitchReports(d.c.run_id);
    assert.deepEqual(d.attempts, []);
    assert.equal(d.posts().length, 1);
    assert.deepEqual(d.tl.events.filter((e) => e === "capture"), []);
    assert.equal(d.probe.reservations.installed, 1);
    assert.equal(d.probe.reservations.released, 0, "the reservation is never released, so the latch is never serviced");
  });

  it("exhausted preparation ACKs keep their terminal disposition with a latched switch; assigned replay preserved, no switch report", { skip: LINUX_CAPTURE, timeout: 60_000 }, async () => {
    const c = claim();
    const root = fs.mkdtempSync(path.join(fs.realpathSync(os.tmpdir()), "switch-preparation-"));
    const outbox = new Outbox({ root: path.join(root, "outbox"), log: nullLogger(),
      runMaxBytes: 64 * 1024 * 1024, maxBytes: 512 * 1024 * 1024, retentionMs: 86_400_000 });
    await outbox.init();
    const archive: RecoveryArchiveClient = {
      reserveRecoveryCapture: async () => ({ capture_id: "preparation-capture", state: "preparing" }),
      getRecoveryCaptureStatus: async (_id, captureId) => ({ capture_id: captureId, state: "preparing", manifest_bound: false }),
      uploadRecoveryBundle: async (_id, captureId, _manifest, bundle) => {
        for await (const _chunk of bundle) { /* Consume the real Git bundle. */ }
        return { capture_id: captureId, state: "available", manifest_bound: true };
      },
      releaseRecoveryCustody: async (id) => ({ run_id: id, released: true, holds_released: 1 }),
      listRecoveryHolds: async (id) => ({ run_id: id, holds: [] }),
    };
    const recovery = new RecoveryCoordinator({ client: archive, git, log: nullLogger(),
      recoveryRoot: path.join(root, "recovery"), workerToken: "preparation-worker-fixture" });
    const tl: Timeline = { events: [], callsAt: {}, counter: { calls: 0 } };
    const original = MessageBatcher.prototype.reserveCandidateTransport;
    const probe = observe(c.run_id, tl);
    const observed = MessageBatcher.prototype.reserveCandidateTransport;
    MessageBatcher.prototype.reserveCandidateTransport = function () {
      this.emit({ kind: "status", payload: { text: "persistent prefix" } });
      api.failMessagesNext(100);
      return observed.call(this);
    };
    api.onState(c.run_id, (b) => {
      tl.events.push(`state:${b.status}`);
      if (b.status === "awaiting_approval") api.requestCredentialSwitch(c.run_id, 1);
    });
    const exec: Executor = { run: async (ctx) => {
      failingCapture();
      const wip = git.worktreeStatus.bind(git);
      git.worktreeStatus = (async (...a: Parameters<typeof wip>) => { tl.events.push("capture"); return wip(...a); }) as typeof git.worktreeStatus;
      const attempt = ctx.attemptCredentialSwitch!;
      ctx.attemptCredentialSwitch = async () => { tl.events.push("attempt"); return attempt(); };
      return new SdkExecutor(nullLogger(), homeDir, { queryFn: scriptedQuery([PLAN], tl.counter) }).run(ctx);
    } };
    try {
      await runner(exec, fakeGitlab().gitlab, undefined, { outbox, recovery, planCrossCheckTiming: timing,
        planApprovalTimeoutMs: 3000 }).execute(c);
      failedWith(c.run_id, "plan cross-check: preparation ACKs unrecoverable");
      assert.deepEqual(switchReports(c.run_id), []);
      assert.deepEqual(tl.events.filter((e) => e === "attempt"), [], "the latch is never serviced");
      assert.equal(api.crossCheckRequests.length, 0);
      assert.ok(outbox.hasUndrainedMessages(c.run_id), "close spills the reserved assigned prefix");
      const recovered: OutgoingMessage[] = [];
      await outbox.drainRun(c.run_id, async (msgs) => { recovered.push(...msgs); });
      assert.ok(recovered.some((m) => (m.payload as { text?: string }).text === "persistent prefix"));
    } finally {
      probe.restore();
      MessageBatcher.prototype.reserveCandidateTransport = original;
      fs.rmSync(root, { recursive: true, force: true });
    }
  });
});

describe("issue #3953 m3: every trip entry reaches neither guard (the state-ack entries prove ordering only)", () => {
  const invariant = "candidate transport invariant violated";
  const noInvariant = (runId: string) => {
    for (const s of everyState(runId))
      assert.ok(!s.failure_reason?.includes(invariant), `the invariant guard fired: ${s.failure_reason}`);
  };
  // The state-ack entries ride the awaiting_approval ACK, which the runner handles after the applied-gate
  // release; the ack therefore trips with no reservation installed. These two prove ordering only, not
  // that a latch was held (a ack landing under an installed reservation is not reachable here).
  const entries: [string, DriveOpts][] = [
    ["inputs poll credentialSwitch, approve", { entry: "poll", verdicts: ["approve"], capture: "giveup" }],
    ["inputs poll credentialSwitch, automatic revise", { entry: "poll", verdicts: ["revise", "approve"], plans: [PLAN, PLAN2], capture: "giveup" }],
    ["inputs poll credentialSwitch, forced gate", { entry: "poll", verdicts: ["block"], capture: "giveup", rejectAfterGiveUp: true }],
    ["state-ack switch (tripCredentialSwitch), forced gate (ordering only)", { entry: "ack", verdicts: ["block"], capture: "giveup", rejectAfterGiveUp: true }],
    ["state-ack switch (tripCredentialSwitch), gated_pass (ordering only)", { entry: "ack", gated: true, verdicts: ["approve"], capture: "giveup", rejectAfterGiveUp: true }],
  ];
  for (const [name, opts] of entries) {
    it(`${name}`, { skip: LINUX_CAPTURE, timeout: 60_000 }, async () => {
      const d = await drive({ ...opts, switchRounds: [1] });
      noInvariant(d.c.run_id);
      assertServicedAfterRelease(d);
      assert.deepEqual(d.attempts, ["attempt", "gave_up"], "serviced once, through the ordinary hook");
      assert.deepEqual(switchReports(d.c.run_id), ["credential_switch_failed"]);
      assert.deepEqual(d.probe.refused, []);
    });
  }

  it("a human verdict acked while the forced gate's ACK is held, with a switch pending after that ACK, is serviced after the release and the verdict then applies", { skip: LINUX_CAPTURE, timeout: 60_000 }, async () => {
    // The receipt switch_pending refusal is only reachable while a hold is open if the APPLIED receipt is
    // sent before the applied-gate release; the runner sends it after (see report), so this proves the
    // ordering and the absence of the invariant, not the refusal arm itself.
    const c = claim();
    const held = holdGateAck(c.run_id, PLAN);
    api.crossCheckHandler = ({ runId, body }) => ({ status: 200, body: answer(runId, body, "block", "block") });
    const counter = { calls: 0 };
    const tl: Timeline = { events: [], callsAt: {}, counter };
    const probe = observe(c.run_id, tl);
    let approveId = 0;
    api.onState(c.run_id, (b) => {
      tl.events.push(`state:${b.status}`);
      if (b.status === "awaiting_approval" && b.plan_md === PLAN) {
        // The next ACK of a routed row makes the switch pending on the server.
        api.pendSwitchAfterNextAck(c.run_id, 1);
        approveId = send(c.run_id, row("approve_plan", "human ok"))[0]!.id;
      }
    });
    const attempts: string[] = [];
    const exec: Executor = { run: async (ctx) => {
      failingCapture();
      const wip = git.worktreeStatus.bind(git);
      git.worktreeStatus = (async (...a: Parameters<typeof wip>) => { tl.events.push("capture"); return wip(...a); }) as typeof git.worktreeStatus;
      const attempt = ctx.attemptCredentialSwitch!;
      ctx.attemptCredentialSwitch = async () => { const o = await attempt(); attempts.push(o); return o; };
      return new SdkExecutor(nullLogger(), homeDir, { queryFn: scriptedQuery([PLAN], counter) }).run(ctx);
    } };
    const done = runner(exec, fakeGitlab().gitlab, undefined, { planCrossCheckTiming: timing, recoveryRetryMs: 1,
      planApprovalTimeoutMs: 3000 }).execute(c);
    try {
      assert.ok(await until(() => approveId > 0));
      await routed(c.run_id, approveId);
      assert.deepEqual(tl.events.filter((e) => e === "capture"), [], "nothing is serviced while the gate ACK is held");
      held.release();
      await done;
    } finally { held.release(); probe.restore(); }
    noInvariant(c.run_id);
    const ev = tl.events;
    assert.deepEqual(attempts, ["gave_up"]);
    assert.ok(ev.indexOf("capture") > ev.indexOf("release"), JSON.stringify(ev));
    assert.deepEqual(switchReports(c.run_id), ["credential_switch_failed"]);
    assert.equal((gates(c.run_id) as PlanCrossCheckStateRequest[]).length, 1, "no republish");
    assert.ok(api.isApplied(c.run_id, approveId), "the human verdict applied after the give-up");
    assert.ok(statuses(c.run_id).includes("completed"), JSON.stringify(statuses(c.run_id)));
    assert.equal(api.crossCheckRequests.filter((r) => r.method === "POST").length, 1);
  });
});

describe("issue #3953 m3: the invariant guards (hold bypassed, reservation installed)", () => {
  for (const guard of ["G1 (the attemptCredentialSwitch hook)", "G2 (dispatchFailure's CredentialSwitchSignal arm)"] as const) {
    it(`${guard}: fails the run terminally, reports no switch, persists the assigned replay and logs the loss`, { skip: LINUX_CAPTURE, timeout: 60_000 }, async () => {
      const c = claim();
      const root = fs.mkdtempSync(path.join(fs.realpathSync(os.tmpdir()), "switch-invariant-"));
      const outbox = new Outbox({ root: path.join(root, "outbox"), log: nullLogger(),
        runMaxBytes: 64 * 1024 * 1024, maxBytes: 512 * 1024 * 1024, retentionMs: 86_400_000 });
      await outbox.init();
      const logged: { level: string; message: string; fields?: Record<string, unknown> }[] = [];
      const log: Logger = { ...nullLogger(),
        error: (message, fields) => { logged.push({ level: "error", message, fields }); },
        warn: (message, fields) => { logged.push({ level: "warn", message, fields }); },
        child: () => log };
      const counter = { calls: 0 };
      const tl: Timeline = { events: [], callsAt: {}, counter };
      // The reservation is installed with an assigned prefix whose delivery keeps failing, so the
      // forced gate is published while the reservation stays installed (as the 3-attempt case).
      const original = MessageBatcher.prototype.reserveCandidateTransport;
      const probe = observe(c.run_id, tl);
      const observed = MessageBatcher.prototype.reserveCandidateTransport;
      MessageBatcher.prototype.reserveCandidateTransport = function () {
        this.emit({ kind: "status", payload: { text: "assigned prefix" } });
        api.failMessagesNext(100);
        return observed.call(this);
      };
      const held = holdGateAck(c.run_id, PLAN);
      api.onState(c.run_id, (b) => {
        tl.events.push(`state:${b.status}`);
        // The forced gate is persisted and its ACK is held: the switch arrives with the reservation installed.
        if (b.status === "awaiting_approval") api.requestCredentialSwitch(c.run_id, 1);
      });
      // Bypass the hold: the switch trips into the installed reservation.
      const hold = SteeringChannel.prototype.holdCredentialSwitch;
      SteeringChannel.prototype.holdCredentialSwitch = () => ({ latched: () => false, close: () => {} });
      let captured: unknown;
      const runnerProto = RunRunner.prototype as unknown as { reportGenericFailure: (...args: unknown[]) => Promise<unknown> };
      const generic = runnerProto.reportGenericFailure;
      runnerProto.reportGenericFailure = function (this: unknown, ...args: unknown[]) {
        if (args[2] instanceof Error) captured ??= args[2];
        return generic.apply(this, args);
      };
      const exec: Executor = guard.startsWith("G1")
        ? { run: async (ctx) => {
            failingCapture();
            const attempt = ctx.attemptCredentialSwitch!;
            ctx.attemptCredentialSwitch = async () => {
              tl.events.push("attempt");
              try { return await attempt(); } catch (error) { captured = error; throw error; }
            };
            return new SdkExecutor(nullLogger(), homeDir, { queryFn: scriptedQuery([PLAN], counter) }).run(ctx);
          } }
        : { run: async (ctx) => {
            failingCapture();
            await ctx.gatePlan!(PLAN); // rejects with the CredentialSwitchSignal; the executor does not handle it
            return { branch: ctx.branch };
          } };
      const forge = fakeGitlab();
      const done = runnerWith(() => ({ executor: exec }), forge.gitlab, undefined, log, { outbox, planCrossCheckTiming: timing,
        recoveryRetryMs: 1, planApprovalTimeoutMs: 3000 }).execute(c);
      try {
        await done;
        failedWith(c.run_id, "candidate transport invariant violated");
        assert.deepEqual(switchReports(c.run_id), [], "no credential_switch / credential_switch_failed report");
        assert.deepEqual(tl.events.filter((e) => e === "capture" || e === "release"), []);
        assert.ok(captured instanceof CandidateTransportInvariantError, `the guard's error: ${String(captured)}`);
        assert.equal(tl.events.includes("attempt"), guard.startsWith("G1"), "G1 is reached through the hook, G2 only without it");
        assert.ok(logged.some((l) => l.level === "error" && l.message.includes("candidate transport invariant violated")),
          "the invariant is logged");
        assert.ok(logged.some((l) => l.level === "warn" && l.message.includes("closed with candidate transport reservation held")),
          "the close logs the reservation it could not release");
        assert.ok(!everyState(c.run_id).some((x) => x.status === "running" && x.candidate_digest === digest), "no approval stored");
        assert.equal(api.crossCheckRequests.length, 0);
        assert.ok(outbox.hasUndrainedMessages(c.run_id), "the batcher close persisted the assigned replay");
        const recovered: OutgoingMessage[] = [];
        await outbox.drainRun(c.run_id, async (msgs) => { recovered.push(...msgs); });
        assert.ok(recovered.some((m) => (m.payload as { text?: string }).text === "assigned prefix"),
          JSON.stringify(recovered.map((m) => m.payload)));
        assert.equal(forge.calls.length, 0);
      } finally {
        held.release();
        SteeringChannel.prototype.holdCredentialSwitch = hold;
        runnerProto.reportGenericFailure = generic;
        probe.restore();
        MessageBatcher.prototype.reserveCandidateTransport = original;
        fs.rmSync(root, { recursive: true, force: true });
      }
    });
  }
});

describe("issue #3953 m3: semantic coverage forces a human gate after the checker approve (real SemanticPlanningGate)", () => {
  const milestones = [{ id: "m1", title: "A" }];
  const submitted = (issueIid: number | null) => {
    const p = proposal();
    p.criteria[0]!.citations[0]!.source_id = "issue:" + issueIid;
    return p;
  };
  function semanticRig(c: ReturnType<typeof claim>, events: string[]) {
    // Isolate gate ordering from native alternate-object capture; no repository mutation.
    git.capturePlanningDiff = async () => Buffer.from("");
    const server = semanticServer(client, events);
    api.stateAckWire = (body, run) => {
      if (body.status !== "awaiting_approval") return undefined;
      const ack = server.gateAck(body.claim_generation!, api.gateResumeFields(String(run.id)).resume_gate_revision!,
        body.semantic_park);
      return { status: 200, body: { run, ...ack } };
    };
    const switchSeen = watchSwitchDelivery();
    api.crossCheckHandler = async ({ runId, body }) => {
      const { stage: _stage, claim_generation: _generation, round = 1, ...candidate } = body;
      events.push("switch-requested");
      api.requestCredentialSwitch(runId, 1);
      assert.ok(await handlerWait(() => switchSeen.delivered), "the worker polled the switch while the checker is pending");
      assert.ok(await handlerWait(() => switchSeen.latched()), "the hold latched the switch");
      const identity = server.compiled().candidate;
      return { status: 200, body: {
        result: "candidate", round, checker_run_id: child, candidate_digest: identity.digest, candidate_generation: 1,
        candidate: { ...candidate, semantic_candidate: identity }, verdict: "approve", reason_class: "approve", findings: null,
        automatic_revision_limit: 4, automatic_rounds_enabled: true,
        deadline_at: new Date(Date.now() + 60000).toISOString(),
        lead_last_seq: Math.max(0, ...api.messages(runId).map((m) => m.seq)),
        claim_generation: 1, plan_cross_check_settled: true, gate_revision: 0, current_plan_sha256: hash(""),
      } };
    };
    api.onState(c.run_id, (b) => {
      if (b.status === "credential_switch_failed") events.push("gave-up");
      if (b.status === "awaiting_approval") events.push("gate");
    });
    return server;
  }
  const semanticClaim = () => freshClaim(1, { kind: "issue", auto_approve: true, issue_description: "Implement A",
    semantic_planning: enrollment, plan_cross_check_required: true });
  function semanticExec(c: ReturnType<typeof claim>, verdicts: unknown[]): Executor {
    return { run: async (ctx) => {
      failingCapture();
      const call = () => ctx.gatePlan!("Implement A", milestones, undefined, undefined, submitted(c.issue_iid));
      let verdict;
      try { verdict = await call(); }
      catch (error) {
        assert.ok(error instanceof CredentialSwitchSignal, String(error));
        assert.equal(await ctx.attemptCredentialSwitch!(), "gave_up", "the held switch gives up on the failing capture");
        verdict = await ctx.continueExistingPlanGate!(call);
      }
      verdicts.push(verdict);
      return { branch: ctx.branch };
    } };
  }

  it("the give-up re-run adopts the approve, its semantic calls succeed on the rearmed lifecycle, and the gate is presented once with its park", { skip: LINUX_CAPTURE, timeout: 60_000 }, async () => {
    const c = semanticClaim();
    const events: string[] = [];
    const server = semanticRig(c, events);
    server.force("incomplete");
    const transport = await boundedTransport("claude", server.report, events);
    api.onState(c.run_id, (b) => { events.push(`state:${b.status}`); if (b.status === "awaiting_approval") send(c.run_id, row("approve_plan")); });
    // The real client honours its signal; the fake transport does not, so make every semantic call
    // throw on an aborted signal. A gate holding the lifecycle signal of the released (aborted)
    // lifecycle instead of the rearmed one then fails its calls on the re-run.
    const original = {
      submit: client.submitSemanticCandidate, status: client.semanticCoverageStatus, reserve: client.reserveSemanticCoverage,
      settle: client.settleSemanticCoverage, publish: client.publishSemanticCoverage,
    };
    client.submitSemanticCandidate = async (...a: Parameters<typeof original.submit>) => { a[5]?.throwIfAborted(); return original.submit.apply(client, a); };
    client.semanticCoverageStatus = async (...a: Parameters<typeof original.status>) => { a[2]?.throwIfAborted(); return original.status.apply(client, a); };
    client.reserveSemanticCoverage = async (...a: Parameters<typeof original.reserve>) => { a[3]?.throwIfAborted(); return original.reserve.apply(client, a); };
    client.settleSemanticCoverage = async (...a: Parameters<typeof original.settle>) => { a[3]?.throwIfAborted(); return original.settle.apply(client, a); };
    client.publishSemanticCoverage = async (...a: Parameters<typeof original.publish>) => { a[3]?.throwIfAborted(); return original.publish.apply(client, a); };
    const verdicts: unknown[] = [];
    try {
      await runner(semanticExec(c, verdicts), fakeGitlab().gitlab, undefined, { semanticCoverageTestTransport: transport,
        planCrossCheckTiming: timing, recoveryRetryMs: 1, planApprovalTimeoutMs: 4000 }).execute(c);
    } finally {
      client.submitSemanticCandidate = original.submit; client.semanticCoverageStatus = original.status;
      client.reserveSemanticCoverage = original.reserve; client.settleSemanticCoverage = original.settle;
      client.publishSemanticCoverage = original.publish;
    }
    assert.equal(api.crossCheckRequests.filter((r) => r.method === "POST").length, 1, "the checked decision is adopted, never resubmitted");
    const gave = events.indexOf("state:credential_switch_failed");
    assert.ok(gave > events.indexOf("switch-requested"), JSON.stringify(events));
    const afterGiveUp = events.slice(gave);
    for (const step of ["status", "reserve", "invoke", "settle:success", "publish"])
      assert.ok(afterGiveUp.includes(step), `${step} ran on the re-run: ${JSON.stringify(events)}`);
    assert.deepEqual(events.slice(0, gave).filter((e) => ["invoke", "publish", "reserve"].includes(e)), [],
      "no semantic call before the held switch is serviced");
    assert.equal(events.filter((e) => e === "invoke").length, 1);
    const presented = gates(c.run_id);
    assert.equal(presented.length, 1, JSON.stringify(everyState(c.run_id).map((s) => s.status)));
    assert.equal(presented[0]!.semantic_park, "coverage_incomplete");
    const [verdict] = verdicts as { kind: string; approval?: string }[];
    assert.equal(verdict!.kind, "approve");
    assert.equal(verdict!.approval, undefined, "a human decided; the checker approve is evidence only");
    assert.ok(!everyState(c.run_id).some((s) => s.status === "running" && s.candidate_digest !== undefined),
      "the checker approve was never stored as an auto approval");
  });

  it("a stashed approve whose candidate digest differs from the re-run's compile fails closed", { skip: LINUX_CAPTURE, timeout: 60_000 }, async () => {
    const c = semanticClaim();
    const events: string[] = [];
    const server = semanticRig(c, events);
    server.force("incomplete");
    const transport = await boundedTransport("claude", server.report, events);
    // The give-up re-run compiles a candidate whose digest differs (same plan and milestones).
    const compile = client.submitSemanticCandidate.bind(client);
    let compiles = 0;
    client.submitSemanticCandidate = async (id, gen, structural, ...rest) =>
      compile(id, gen, ++compiles === 1 ? structural : { ...structural, size_class: structural.size_class === "l" ? "m" : "l" }, ...rest);
    const verdicts: unknown[] = [];
    await runner(semanticExec(c, verdicts), fakeGitlab().gitlab, undefined, { semanticCoverageTestTransport: transport,
      planCrossCheckTiming: timing, recoveryRetryMs: 1, planApprovalTimeoutMs: 4000 }).execute(c);
    assert.ok(compiles >= 2, "the re-run compiled again");
    failedWith(c.run_id, "stashed decision does not match");
    assert.deepEqual(verdicts, []);
    assert.equal(api.crossCheckRequests.filter((r) => r.method === "POST").length, 1);
    assert.equal(events.filter((e) => e === "invoke").length, 0, "no coverage review runs on a mismatched adoption");
    assert.equal(gates(c.run_id).length, 0);
  });

  it("an owner cancel during the re-run's coverage review still aborts it", { skip: LINUX_CAPTURE, timeout: 60_000 }, async () => {
    const c = semanticClaim();
    const events: string[] = [];
    const server = semanticRig(c, events);
    server.force("incomplete");
    const transport = await boundedTransport("claude", server.report, events);
    api.onState(c.run_id, (b) => { events.push(`state:${b.status}`); });
    const status = client.semanticCoverageStatus.bind(client);
    let cancelled = false;
    client.semanticCoverageStatus = async (...args) => {
      if (events.includes("state:credential_switch_failed") && !cancelled) {
        cancelled = true;
        send(c.run_id, row("cancel"));
        const signal = args[2];
        assert.ok(await until(() => signal?.aborted === true), "the owner cancel aborts the review's signal");
      }
      return status(...args);
    };
    const verdicts: unknown[] = [];
    const exec = semanticExec(c, verdicts);
    await runner(exec, fakeGitlab().gitlab, undefined, { semanticCoverageTestTransport: transport,
      planCrossCheckTiming: timing, recoveryRetryMs: 1, planApprovalTimeoutMs: 4000 }).execute(c);
    assert.equal(cancelled, true, "the re-run reached its coverage review");
    failedWith(c.run_id, "run cancelled");
    assert.deepEqual(verdicts, [], "no verdict escapes the cancelled review");
    assert.equal(gates(c.run_id).length, 0, "no gate is presented after the cancel");
  });
});

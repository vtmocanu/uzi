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
import { execFileSync, spawn } from "node:child_process";
import { processGroupAlive } from "../src/tick-spawner.js";
import { Outbox } from "../src/outbox.js";
import { RecoveryCoordinator, type RecoveryArchiveClient } from "../src/recovery.js";
import { CredentialSwitchSignal } from "../src/steering.js";
import { createHash } from "node:crypto";
import type { Executor, RunContext } from "../src/executor.js";
import { TrustedExecutionRefusal } from "../src/trusted-execution-refusal.js";
import { SdkExecutor, TransientRecoveryError, type SdkExecutorOptions, type SdkQueryFn } from "../src/sdk-executor.js";
import { MessageBatcher } from "../src/batcher.js";
import { CandidateTransportInvariantError, RunRunner } from "../src/runner.js";
import { quiesceRunAttempt, type QuiesceRunRequest } from "../src/run-quiescence.js";
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
/** Issue #3974: the levers a drive callback gets. */
interface DriveControl {
  tl: Timeline;
  switchSeen: ReturnType<typeof watchSwitchDelivery>;
  /** Make the switch visible to the worker and wait until the hold has latched it. */
  requestSwitch(): Promise<void>;
  /** Queue an owner cancel row (the fake stamps stop_kind 'cancelled'); records the "cancel-sent" event. */
  cancel(): number;
  /** Observe state reports without replacing the drive's own recording hook (FakeApi.onState is last-wins). */
  onState(fn: (body: Parameters<Parameters<typeof api.onState>[1]>[0]) => void): void;
  /** Flip the pending checker answer of `round` to `verdict`, keeping its identity. */
  flip(round: number, verdict: "approve" | "block"): void;
  /** Issue #3980: the candidate reservation's live counters. */
  reservations: { installed: number; released: number };
  /** Issue #3980: the one long-lived real SdkExecutor the run uses when `DriveOpts.forward` is set. */
  sdk?: SdkExecutor;
}
/** Observe the candidate reservation (reserve/release), landed state reports and landed messages in
 *  ONE ordered list. Returns the restore function for the shared batcher prototype. */
function observe(runId: string, tl: Timeline, opts: { prefix?: string; failAfterRelease?: boolean; healAtReserve?: boolean } = {}) {
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
    // Deliveries failed until now (the owed assigned replay); the reservation's prepare can land them.
    if (opts.healAtReserve) api.failMessagesNext(0);
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
  const channels: SteeringChannel[] = [];
  const seen = { delivered: false, trips: 0, latched: () => handles.some((h) => h.latched()),
    /** Issue #3974: an owner cancel was committed under a withheld switch (the channel's sticky flag). */
    cancelCommitted: () => channels.some((ch) => ch.cancelCommittedUnderWithheldSwitch()),
    isCancelled: () => channels.some((ch) => ch.isCancelled()),
    /** A deliberate service-close of every open hold, as the runner does at a verified release. */
    serviceHolds: () => { for (const h of handles) (h as unknown as { close(o: { service: boolean }): void }).close({ service: true }); } };
  client.getInputs = async (runId) => {
    const result = await getInputs(runId);
    if (result.credentialSwitch?.generation === generation) seen.delivered = true;
    return result;
  };
  const proto = SteeringChannel.prototype as unknown as {
    holdCredentialSwitch: () => { latched(): boolean }; maybeTripCredentialSwitch: (g: number) => void;
    beginCredentialSwitchDefer: () => void };
  const hold = proto.holdCredentialSwitch;
  const trip = proto.maybeTripCredentialSwitch;
  const defer = proto.beginCredentialSwitchDefer;
  proto.beginCredentialSwitchDefer = function (this: unknown) {
    channels.push(this as SteeringChannel);
    return defer.call(this);
  };
  proto.holdCredentialSwitch = function (this: unknown) {
    const handle = hold.call(this);
    handles.push(handle);
    channels.push(this as SteeringChannel);
    return handle;
  };
  proto.maybeTripCredentialSwitch = function (this: unknown, g: number) {
    try { return trip.call(this, g); } finally { seen.trips++; }
  };
  restorers.push(() => { proto.holdCredentialSwitch = hold; proto.maybeTripCredentialSwitch = trip; proto.beginCredentialSwitchDefer = defer; });
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
function scriptedQuery(plans: string[], counter: { calls: number }, milestones: { id: string; title: string }[] = []): SdkQueryFn {
  return (params) => {
    const turn = counter.calls++;
    return (async function* () {
      for await (const _ of params.prompt) { /* drain */ }
      const plan = plans[turn];
      if (plan !== undefined) {
        yield assistant([{ type: "tool_use", id: `p${turn}`, name: "mcp__uzi__submit_plan", input: { plan_md: plan, milestones } }]);
      } else {
        yield assistant([{ type: "text", text: "done implementing" },
          { type: "tool_use", id: `d${turn}`, name: "mcp__uzi__signal_done", input: {} }]);
      }
      yield resultOk();
    })();
  };
}

const PLAN2 = "# PLAN\n- revised proposal";
type Verdict = "approve" | "revise" | "block" | "pending";
interface DriveOpts {
  gated?: boolean;
  verdicts?: Verdict[];
  /** Rounds during whose pending checker request a switch becomes visible to the worker. */
  switchRounds?: number[];
  /** poll: the inputs poll carries the switch. ack: the awaiting_approval state ACK carries it. */
  entry?: "poll" | "ack";
  /** "real" (issue #3980) installs neither capture double: the runner captures through the real GitCache. */
  capture: "release" | "giveup" | "real";
  /** Issue #3980: run through ONE real SdkExecutor built with these options, and forward the Executor
   *  hooks the runner's quiescence uses (reapAttributedProcesses, with its options) to it. */
  forward?: Partial<SdkExecutorOptions>;
  plans?: string[];
  /** The milestones every scripted submit_plan carries (default none). */
  planMilestones?: { id: string; title: string }[];
  emitHeld?: boolean;
  failAfterRelease?: boolean;
  /** An assigned replay: a status emitted when the reservation installs whose first delivery fails. */
  prefix?: string;
  /** An owed assigned replay: a status emitted (and so seq-assigned) during planning whose delivery keeps
   *  failing until the reservation installs, whose prepare then delivers it before the candidate is submitted. */
  owed?: string;
  dropFirstPost?: boolean;
  /** Reject the human gate once the give-up is confirmed (a gate left presented by the old claim). */
  rejectAfterGiveUp?: boolean;
  /** Rewrite the plan markdown of the n-th (1-based) ctx.gatePlan call. */
  gateMd?: (n: number, md: string) => string;
  /** Rewrite the milestones of the n-th (1-based) ctx.gatePlan call. */
  gateMilestones?: (n: number, milestones: Parameters<NonNullable<RunContext["gatePlan"]>>[1]) => Parameters<NonNullable<RunContext["gatePlan"]>>[1];
  /** A forced human gate for the checker's revise/block instead of an automatic revision. */
  manual?: boolean;
  /** The checker never decides: its answer stays pending past a short deadline (a timed-out checker). */
  pending?: boolean;
  /** Run first on the pending checker request, whether or not a switch is requested. */
  duringPending?: (c: ReturnType<typeof claim>, x: DriveControl) => Promise<void>;
  /** Run once the switch is visible to the worker and the checker is still pending. */
  afterSwitch?: (c: ReturnType<typeof claim>, x: DriveControl) => Promise<void>;
  /** Run inside the first transport preparation of checkPlan (a reservation is installed, no round
   *  has been submitted yet). Settles before the preparation continues. */
  duringPreparation?: (c: ReturnType<typeof claim>, x: DriveControl, signal: AbortSignal) => Promise<void>;
  /** Arrange the api for this claim before the run starts. */
  setup?: (c: ReturnType<typeof claim>, x: DriveControl) => void;
  /** Issue #3974: serve cancellation_requested the way the server derives it (FakeApi.deriveCancellationRequested). */
  serveCancellation?: boolean;
  /** Issue #3974: how long the checker's pending answer stays pending (default 40 ms). */
  pendingDeadlineMs?: number;
  /** A status emitted before the reservation whose delivery keeps failing, so the first preparation is
   *  held (a forced human gate whose completePreparation owns the remaining attempts). */
  persistentPrefix?: boolean;
  /** Claim overrides (an mr_rework lead, ...). */
  claimOver?: (base: ReturnType<typeof claim>) => Partial<ReturnType<typeof claim>>;
  /** Extra runner options. */
  runnerOpts?: Record<string, unknown>;
}
/** Run one claim to its end through the real runner + real SdkExecutor + the cross-check api fake. */
async function drive(o: DriveOpts) {
  const base = o.gated
    ? freshClaim(1, { kind: "issue", auto_approve: false, plan_cross_check_gated: true })
    : claim();
  const c = { ...base, ...o.claimOver?.(base) };
  if (o.serveCancellation) api.deriveCancellationRequested = true;
  const counter = { calls: 0 };
  const tl: Timeline = { events: [], callsAt: {}, counter };
  const failedPosts = { count: 0 };
  const realPost = client.postMessages;
  if (o.owed) client.postMessages = async (...args: Parameters<typeof realPost>) => {
    try { return await realPost.apply(client, args); }
    catch (error) { if (args[0] === c.run_id) failedPosts.count++; throw error; }
  };
  const probe = observe(c.run_id, tl, { failAfterRelease: o.failAfterRelease, prefix: o.prefix, healAtReserve: o.owed !== undefined });
  const switchSeen = watchSwitchDelivery();
  const decided = new Map<number, ReturnType<typeof answer>>();
  const leadSeqs: number[] = [];
  let lastRound = 0;
  let dropped = false;
  let emit!: RunContext["emit"];
  const trace = { queriesAtRequest: [] as number[], requests: 0 };
  const cancelIds: number[] = [];
  const stateHooks: Parameters<DriveControl["onState"]>[0][] = [];
  const sdk = o.forward ? new SdkExecutor(nullLogger(), homeDir, { queryFn: scriptedQuery(o.plans ?? [PLAN], counter, o.planMilestones), ...o.forward }) : undefined;
  const ctl: DriveControl = {
    tl, switchSeen, reservations: probe.reservations, sdk,
    onState: (fn) => { stateHooks.push(fn); },
    requestSwitch: async () => {
      tl.events.push("switch-requested");
      api.requestCredentialSwitch(c.run_id, 1);
      assert.ok(await handlerWait(() => switchSeen.delivered), "the worker polled the switch");
      switchSeen.delivered = false;
      assert.ok(await handlerWait(() => switchSeen.latched()), "the hold latched the switch");
    },
    cancel: () => {
      const id = send(c.run_id, row("cancel"))[0]!.id;
      cancelIds.push(id);
      tl.events.push("cancel-sent");
      return id;
    },
    flip: (round, verdict) => {
      decided.set(round, { ...decided.get(round)!, verdict, reason_class: verdict });
    },
  };
  // Issue #3974: every ownership answer for this run, in the one ordered event list.
  const ownership = client.getRunOwnership.bind(client);
  client.getRunOwnership = (async (...args: Parameters<typeof ownership>) => {
    const answered = await ownership(...args);
    if (args[0] === c.run_id) tl.events.push(`ownership:${answered.cancellation_requested === true}`);
    return answered;
  }) as typeof client.getRunOwnership;
  const reserveBeforePreparation = MessageBatcher.prototype.reserveCandidateTransport;
  if (o.persistentPrefix) {
    const observed = MessageBatcher.prototype.reserveCandidateTransport;
    MessageBatcher.prototype.reserveCandidateTransport = function () {
      this.emit({ kind: "status", payload: { text: "persistent prefix" } });
      api.failMessagesNext(100);
      return observed.call(this);
    };
  }
  if (o.duringPreparation) {
    let first = true;
    const reserveWrapped = MessageBatcher.prototype.reserveCandidateTransport;
    MessageBatcher.prototype.reserveCandidateTransport = function () {
      const reservation = reserveWrapped.call(this);
      return { ...reservation, prepare: async (...args: Parameters<typeof reservation.prepare>) => {
        if (first) { first = false; await o.duringPreparation!(c, ctl, args[1] ?? new AbortController().signal); }
        return reservation.prepare(...args);
      } };
    };
  }
  api.crossCheckHandler = async ({ runId, method, body }) => {
    if (method === "GET") return { status: 200, body: decided.get(lastRound) };
    const round = Number(body.round);
    lastRound = round;
    if (!decided.has(round)) {
      await o.duringPending?.(c, ctl);
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
        await o.afterSwitch?.(c, ctl);
      }
      const verdict = o.pending ? "pending" : (o.verdicts ?? ["approve"])[round - 1] ?? "approve";
      const generation = Number(body.claim_generation ?? 1);
      decided.set(round, answer(runId, body, verdict, verdict === "pending" ? "" : verdict, {
        ...proof(runId, generation), candidate_generation: generation,
        ...(o.pending ? { deadline_at: new Date(Date.now() + (o.pendingDeadlineMs ?? 40)).toISOString() } : {}),
        ...(o.manual ? {} : { automatic_revision_limit: 3, automatic_rounds_enabled: true }),
        ...(verdict === "revise" ? { findings: { summary: `fix round ${round}`, items: [] } } : {}),
        ...(verdict === "approve" ? canonical(body) : {}),
      }));
      leadSeqs.push(decided.get(round)!.lead_last_seq);
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
    for (const fn of stateHooks) fn(b);
  };
  prior(c.run_id, hook);
  const attempts: string[] = [];
  let gateCalls = 0;
  const exec: Executor = { run: async (ctx) => {
    emit = ctx.emit;
    if (o.owed) {
      api.failMessagesNext(1000);
      ctx.emit({ kind: "status", payload: { text: o.owed } });
      assert.ok(await until(() => failedPosts.count > 0), "the assigned replay's first delivery failed");
    }
    if (o.capture === "release") healthyCapture(); else if (o.capture === "giveup") failingCapture();
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
    return (sdk ?? new SdkExecutor(nullLogger(), homeDir, { queryFn: scriptedQuery(o.plans ?? [PLAN], counter, o.planMilestones) })).run(ctx);
  }, ...(sdk ? { closeAndSettlePlanSummary: (ms?: number) => sdk.closeAndSettlePlanSummary(ms), reapAttributedProcesses: (opts?: { deadlineAt?: number }) => sdk.reapAttributedProcesses(opts) } : {}) };
  o.setup?.(c, ctl);
  const forge = fakeGitlab();
  const sentinel = path.join(homeDir, "SESSION");
  fs.writeFileSync(sentinel, "resumable session\n");
  try {
    await runner(exec, forge.gitlab, undefined, { planCrossCheckTiming: timing, recoveryRetryMs: 1,
      planApprovalTimeoutMs: 3000, ...o.runnerOpts }).execute(c);
  } finally {
    MessageBatcher.prototype.reserveCandidateTransport = reserveBeforePreparation;
    probe.restore(); client.postMessages = realPost; client.reportPlanCrossCheckGateState = report; api.failMessagesNext(0);
    client.getRunOwnership = ownership;
  }
  const heldSeqs = api.messages(c.run_id).filter((m) => (m.payload as { text?: string }).text?.startsWith("held during"))
    .map((m) => m.seq);
  return { c, tl, counter, forge, probe, storage, attempts, sentinel, trace, cancelIds, gaveUp: () => gaveUp, heldSeqs, leadSeqs,
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

  it("forced human gate (checker timed out): give-up keeps the presented gate, its binding and its verdict path with no republish", { skip: LINUX_CAPTURE, timeout: 60_000 }, async () => {
    const d = await drive({ capture: "giveup", pending: true, switchRounds: [1], rejectAfterGiveUp: true });
    const { c, tl } = d;
    assertServicedAfterRelease(d);
    assert.deepEqual(d.attempts, ["attempt", "gave_up"]);
    assert.deepEqual(switchReports(c.run_id), ["credential_switch_failed"]);
    assert.ok(tl.events.indexOf("state:awaiting_approval") < tl.events.indexOf("release"),
      "service only after the applied forced gate released the reservation");
    const presented = gates(c.run_id) as PlanCrossCheckStateRequest[];
    assert.equal(presented.length, 1, "no republish after the give-up");
    assert.equal(presented[0]!.plan_cross_check_gate_reason, "timed_out");
    assert.ok(presented[0]!.presentation_id, "the presentation identity is intact");
    const rejects = api.inputRows(c.run_id).filter((r) => r.kind === "reject_plan");
    assert.equal(rejects.length, 1);
    assert.ok(api.isApplied(c.run_id, rejects[0]!.id), "the human verdict bound to the original gate revision applied");
    assert.ok(everyState(c.run_id).some((s) => s.status === "failed" && s.failure_reason?.includes("human after give-up")),
      JSON.stringify(everyState(c.run_id).map((s) => [s.status, s.failure_reason])));
    assert.equal(d.posts().length, 1, "no second candidate");
    assert.deepEqual(d.storage, []);
    assertSeqsUnique(c.run_id);
  });

  it("forced human gate (checker timed out): release lets the next claim re-present", { skip: LINUX_CAPTURE, timeout: 60_000 }, async () => {
    const d = await drive({ capture: "release", pending: true, switchRounds: [1], emitHeld: true });
    const { c } = d;
    assertServicedAfterRelease(d);
    assert.deepEqual(d.attempts, ["attempt", "released"]);
    assert.deepEqual(switchReports(c.run_id), ["credential_switch"]);
    const presented = gates(c.run_id) as PlanCrossCheckStateRequest[];
    assert.equal(presented.length, 1);
    assert.equal(presented[0]!.plan_cross_check_gate_reason, "timed_out");
    for (const text of ["held during reservation 1A", "held during reservation 1B"])
      assert.equal(texts(c.run_id).filter((t) => t === text).length, 1, `${text} delivered once`);
    await reclaimAndExpectRepresent(c, "timed_out", "timed_out", false);
  });

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

/** Assigned replay (`prefix`: seq-assigned, first delivery failed, owed to the checker's proof) plus held
 *  unassigned events emitted while the reservation was installed: the checker's proof covers the replay and
 *  nothing else, the held events are assigned only by the release, after it, and every message lands once. */
function assertReplayThenHeld(d: Driven, prefix: string): void {
  const { c } = d;
  const log = api.messages(c.run_id);
  const seqOf = (text: string): number[] => log.filter((m) => (m.payload as { text?: string }).text === text).map((m) => m.seq);
  const [replaySeq, ...extra] = seqOf(prefix);
  assert.ok(replaySeq !== undefined && extra.length === 0, `assigned replay delivered exactly once: ${JSON.stringify(seqOf(prefix))}`);
  const held = ["held during reservation 1A", "held during reservation 1B"].map((t) => {
    const seqs = seqOf(t);
    assert.equal(seqs.length, 1, `${t} delivered exactly once: ${JSON.stringify(seqs)}`);
    return seqs[0]!;
  });
  assert.ok(replaySeq < held[0]! && held[0]! < held[1]!, `replay then held, in order: ${JSON.stringify([replaySeq, ...held])}`);
  assert.deepEqual(d.heldSeqs, held);
  const ev = d.tl.events;
  assert.ok(ev.indexOf("reserve") >= 0 && ev.indexOf("msg:assigned replay") > ev.indexOf("reserve"),
    `the replay was still owed when the reservation installed, and its prepare delivered it: ${JSON.stringify(ev)}`);
  // Reconciliation: the checker's proof (lead_last_seq) covers the assigned replay and none of the held events.
  assert.equal(d.leadSeqs.length, 1);
  assert.ok(d.leadSeqs[0]! >= replaySeq && d.leadSeqs[0]! < held[0]!,
    `lead_last_seq ${d.leadSeqs[0]} covers the replay (${replaySeq}) and precedes the held events (${held[0]})`);
  assertSeqsUnique(c.run_id);
}

describe("issue #3953 m3: an assigned replay and held unassigned events together", () => {
  const combined: Omit<DriveOpts, "capture"> = { switchRounds: [1], emitHeld: true, owed: "assigned replay", failAfterRelease: true };

  it("S1 release: replay then held events are delivered once before the release report; the next claim continues the seqs", { skip: LINUX_CAPTURE, timeout: 60_000 }, async () => {
    const d = await drive({ capture: "release", ...combined });
    const { c, tl } = d;
    assertServicedAfterRelease(d);
    assert.deepEqual(d.attempts, ["attempt", "released"]);
    assertReplayThenHeld(d, "assigned replay");
    for (const text of ["assigned replay", "held during reservation 1A", "held during reservation 1B"])
      assert.ok(tl.events.indexOf(`msg:${text}`) >= 0 && tl.events.indexOf(`msg:${text}`) < tl.events.indexOf("state:credential_switch"),
        `${text} landed before the release report: ${JSON.stringify(tl.events)}`);
    assert.equal(tl.events.filter((e) => e === "msg:assigned replay").length, 1, "the replay is posted successfully once");
    assert.deepEqual(d.storage, []);
    const before = api.messages(c.run_id).map((m) => m.seq);
    // The next claim re-presents a human gate and its messages continue after every earlier seq.
    const c2 = reclaim(c);
    api.crossCheckLatestHandler = () => ({ status: 200, body: {
      result: "latest", round: 1, candidate_generation: 1, automatic_revision_limit: 2,
      automatic_rounds_enabled: true, next_round: null, next_round_eligible: false, fallback_reason: "approve" } });
    api.onState(c.run_id, (b) => { if (b.status === "awaiting_approval") send(c.run_id, row("reject_plan", "next claim human")); });
    await runner({ run: async (ctx) => new SdkExecutor(nullLogger(), homeDir, { queryFn: scriptedQuery([PLAN], { calls: 0 }) }).run(ctx) },
      fakeGitlab().gitlab, undefined, { planCrossCheckTiming: timing, recoveryRetryMs: 1, planApprovalTimeoutMs: 3000 }).execute(c2);
    assert.equal((gates(c.run_id) as PlanCrossCheckStateRequest[]).length, 1, "the next claim re-presents a human gate");
    assert.equal(d.posts().length, 1, "no second submit");
    const all = api.messages(c.run_id).map((m) => m.seq);
    assert.deepEqual(all.slice(0, before.length), before, "earlier events are intact");
    assert.ok(all.length > before.length, "the next claim delivered its own events");
    assert.ok(all.slice(before.length).every((seq) => seq > Math.max(...before)), "the next claim's seqs follow the released claim's");
    for (const text of ["assigned replay", "held during reservation 1A", "held during reservation 1B"])
      assert.equal(texts(c.run_id).filter((t) => t === text).length, 1, `${text} is not redelivered by the next claim`);
  });

  it("S1 give-up: replay then held events are delivered once and the adopted approve continues the run", { skip: LINUX_CAPTURE, timeout: 60_000 }, async () => {
    const d = await drive({ capture: "giveup", ...combined });
    assertServicedAfterRelease(d);
    assert.deepEqual(d.attempts, ["attempt", "gave_up"]);
    assertReplayThenHeld(d, "assigned replay");
    assert.equal(d.posts().length, 1, "the adopted decision is never resubmitted");
    assert.equal(d.storage.length, 1);
    assert.equal(d.storage[0]!.candidate_digest, digest);
    assert.ok(statuses(d.c.run_id).includes("completed"), JSON.stringify(statuses(d.c.run_id)));
  });

  for (const capture of ["giveup", "release"] as const) {
    it(`ambiguous submit (accepted, response lost): replay then held events, status recovery and no second submit (${capture})`, { skip: LINUX_CAPTURE, timeout: 60_000 }, async () => {
      const d = await drive({ capture, dropFirstPost: true, ...combined });
      assertServicedAfterRelease(d);
      assert.equal(api.crossCheckReplies.filter((r) => r.acceptedCandidate && r.dropped).length, 1);
      assert.deepEqual(api.crossCheckRequests.map((r) => r.method), ["POST", "GET"], "status recovery, never a second submit");
      assertReplayThenHeld(d, "assigned replay");
      if (capture === "giveup") {
        assert.deepEqual(d.attempts, ["attempt", "gave_up"]);
        assert.equal(d.storage.length, 1);
        assert.ok(statuses(d.c.run_id).includes("completed"));
      } else {
        assert.deepEqual(d.attempts, ["attempt", "released"]);
        assert.deepEqual(d.storage, []);
        assert.deepEqual(switchReports(d.c.run_id), ["credential_switch"]);
      }
    });
  }
});

describe("issue #3953 m3: recovery custody across a held switch's release", () => {
  it("S1 release (guarded inventory): the released generation's hold and source binding stay open and untouched; the next claim binds only its own generation", { skip: LINUX_CAPTURE, timeout: 60_000 }, async () => {
    const root = fs.mkdtempSync(path.join(fs.realpathSync(os.tmpdir()), "switch-custody-"));
    const generation = { current: 1 };
    const reserved: { generation?: number }[] = [];
    const releasedGenerations: (number | undefined)[] = [];
    const archive: RecoveryArchiveClient = {
      hasFeature: (feature: string) => feature === "recovery_inventory_v1",
      reserveRecoveryCapture: async (_id, req) => { reserved.push({ generation: req.generation }); return { capture_id: "custody-capture", state: "preparing" }; },
      getRecoveryCaptureStatus: async (_id, captureId) => ({ capture_id: captureId, state: "preparing", manifest_bound: false }),
      uploadRecoveryBundle: async (_id, captureId, _manifest, bundle) => {
        for await (const _chunk of bundle) { /* Consume the real Git bundle. */ }
        return { capture_id: captureId, state: "available", manifest_bound: true };
      },
      releaseRecoveryCustody: async (id, releasedGeneration) => { releasedGenerations.push(releasedGeneration); return { run_id: id, released: true, holds_released: 1 }; },
      // The server's open guarded hold of the generation being executed.
      listRecoveryHolds: async (id) => ({ run_id: id, holds: [{ hold_id: `hold-g${generation.current}`,
        generation: generation.current, inventory_guarded: true, has_available_capture: false }] }),
    };
    const recovery = new RecoveryCoordinator({ client: archive, git, log: nullLogger(),
      recoveryRoot: path.join(root, "recovery"), workerToken: "custody-worker-fixture" });
    try {
      const d = await drive({ capture: "release", switchRounds: [1], runnerOpts: { recovery },
        setup: (c) => { c.inventory_guarded = true; } });
      const { c } = d;
      assert.deepEqual(d.attempts, ["attempt", "released"]);
      assert.deepEqual(switchReports(c.run_id), ["credential_switch"]);
      // The release keeps the claim's custody exactly as it was pinned: generation and source binding.
      const afterRelease = await recovery.inspect(c.run_id);
      assert.equal(afterRelease.length, 1, JSON.stringify(afterRelease));
      const [held] = afterRelease;
      assert.equal(held!.generation, 1);
      assert.equal(held!.inventoryGuarded, true);
      assert.equal(held!.state, "pinned", "the hold is neither captured nor retired by the release");
      assert.match(held!.sourceSha, /^[0-9a-f]{40}$/);
      assert.equal(held!.branch, `agent/issue-${c.issue_iid}`);
      assert.equal(reserved.length, 0, "no capture was reserved for the released generation");
      assert.equal(releasedGenerations.length, 0, "the released generation's custody hold is not released");

      // The next claim (generation 2) binds its own generation and leaves generation 1 alone.
      generation.current = 2;
      const c2 = { ...reclaim(c), inventory_guarded: true };
      api.crossCheckLatestHandler = () => ({ status: 200, body: {
        result: "latest", round: 1, candidate_generation: 1, automatic_revision_limit: 2,
        automatic_rounds_enabled: true, next_round: null, next_round_eligible: false, fallback_reason: "approve" } });
      api.onState(c.run_id, (b) => { if (b.status === "awaiting_approval") send(c.run_id, row("reject_plan", "next claim human")); });
      await runner({ run: async (ctx) => new SdkExecutor(nullLogger(), homeDir, { queryFn: scriptedQuery([PLAN], { calls: 0 }) }).run(ctx) },
        fakeGitlab().gitlab, undefined, { planCrossCheckTiming: timing, recoveryRetryMs: 1, planApprovalTimeoutMs: 3000, recovery }).execute(c2);
      const records = await recovery.inspect(c.run_id);
      assert.deepEqual(records.filter((r) => r.generation === 1), [held], "generation 1's record is byte-identical after the next claim");
      assert.ok(records.some((r) => r.generation === 2 && r.inventoryGuarded === true), "the next claim pinned its own generation");
      assert.ok(records.every((r) => r.generation === 1 || r.generation === 2), JSON.stringify(records.map((r) => r.generation)));
      assert.ok(reserved.length > 0 && reserved.every((r) => r.generation === 2),
        `the next claim's capture is bound to generation 2 only: ${JSON.stringify(reserved)}`);
      assert.ok(releasedGenerations.every((g) => g === 2), `no release names generation 1: ${JSON.stringify(releasedGenerations)}`);
    } finally { fs.rmSync(root, { recursive: true, force: true }); }
  });
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

  it("an owner cancel row sent while a latched switch is pending ends the run cancelled promptly; the latch is never serviced", { skip: LINUX_CAPTURE, timeout: 60_000 }, async () => {
    // Issue #3974: the api serves cancellation_requested on the ownership read, so the worker finds the
    // cancel the withheld inputs hide. The checker's approve is returned only once the cancel is committed.
    const d = await drive({ capture: "release", switchRounds: [1], serveCancellation: true, afterSwitch: async (_c, x) => {
      x.cancel();
      assert.ok(await until(() => x.switchSeen.cancelCommitted()), "the probe committed the cancel");
    } });
    assertCancelledNotSwitched(d);
    assert.equal(d.counter.calls, 1);
    assert.equal(fs.existsSync(worktreeDirFor(Number(d.c.issue_iid))), false, "a cancelled run's clone is cleaned up");
  });

  it("an older api that serves no cancellation_requested: an owner cancel row sent while a latched switch is pending is withheld until the switch is serviced; the release leaves it for the reclaim", { skip: LINUX_CAPTURE, timeout: 60_000 }, async () => {
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

  /** Count the APPLIED receipts a pending switch refused, and any waiter rejection with the switch signal
   *  while a hold is open (the hold must prevent both: a rejection would abort the live reservation). */
  function watchTripPaths() {
    const seen = { refused: 0, rejectedUnderHold: [] as string[] };
    const apply = client.applyInputs;
    client.applyInputs = async (...args: Parameters<typeof apply>) => {
      try { return await apply.apply(client, args); }
      catch (error) {
        if (String((error as { body?: unknown }).body ?? error).includes("switch_pending")) seen.refused++;
        throw error;
      }
    };
    const proto = SteeringChannel.prototype as unknown as { rejectParkedWaiters: (err: Error) => void; credentialSwitchHoldOpen: () => boolean };
    const reject = proto.rejectParkedWaiters;
    proto.rejectParkedWaiters = function (this: typeof proto, err: Error) {
      if (err instanceof CredentialSwitchSignal && this.credentialSwitchHoldOpen()) seen.rejectedUnderHold.push(err.name);
      return reject.call(this, err);
    };
    restorers.push(() => { client.applyInputs = apply; proto.rejectParkedWaiters = reject; });
    return seen;
  }

  for (const [name, opts] of entries) {
    it(`${name}: no waiter is rejected with the switch signal while the hold is open`, { skip: LINUX_CAPTURE, timeout: 60_000 }, async () => {
      const watched = watchTripPaths();
      const d = await drive({ ...opts, switchRounds: [1] });
      noInvariant(d.c.run_id);
      assert.deepEqual(d.attempts, ["attempt", "gave_up"]);
      assert.deepEqual(watched.rejectedUnderHold, [], "rejectParkedWaiters never ran with the switch signal under an open hold");
    });
  }

  // Only the APPLIED refusal reveals the switch here: pendSwitchAfterNextAck without a signal generation
  // keeps the switch off every inputs poll and ACK, so the latch below can only come from the refusal.
  for (const [shape, base] of [
    ["S1 approve", { verdicts: ["approve"] }],
    ["S3 forced gate, whose presentation waits on the held batch's receipts", { verdicts: ["block"] }],
  ] as [string, Partial<DriveOpts>][]) for (const capture of ["giveup", "release"] as const) {
    it(`held lane (${shape}): an operator input ACKed while the reservation is installed has its APPLIED refused switch_pending; the refusal latches and the switch is serviced after the release (${capture})`, { skip: LINUX_CAPTURE, timeout: 60_000 }, async () => {
      const watched = watchTripPaths();
      const delivery = watchSwitchDelivery();
      let rowId = 0;
      const forced = base.verdicts?.[0] === "block";
      const d = await drive({ ...base, capture, switchRounds: [], rejectAfterGiveUp: forced && capture === "giveup", duringPending: async (c) => {
        if (forced) {
          // Hold the APPLIED request back (the fake api refuses a pending switch's APPLIED before it applies
          // any reply delay), so the forced gate's presentation is already waiting on the receipt.
          const apply = client.applyInputs;
          client.applyInputs = async (...args: Parameters<typeof apply>) => {
            await new Promise((resolve) => setTimeout(resolve, 150));
            return apply.apply(client, args);
          };
          restorers.push(() => { client.applyInputs = apply; });
        }
        api.pendSwitchAfterNextAck(c.run_id);
        rowId = send(c.run_id, row("follow_up", "operator note while the checker is pending"))[0]!.id;
        if (forced) {
          // The decision is returned while the batch's APPLIED is still in flight (delayed): the forced gate's
          // presentation report then waits on that receipt, and the refusal arrives during that wait.
          assert.ok(await handlerWait(() => api.isAcked(c.run_id, rowId)), "the operator input was ACKed");
          return;
        }
        assert.ok(await handlerWait(() => watched.refused > 0), "the held batch's APPLIED was refused switch_pending");
        assert.ok(await handlerWait(() => delivery.latched()), "the refusal latched under the hold");
      } });
      assert.ok(watched.refused > 0, "the held batch's APPLIED was refused switch_pending");
      noInvariant(d.c.run_id);
      assertServicedAfterRelease(d);
      assert.deepEqual(watched.rejectedUnderHold, []);
      assert.ok(rowId > 0);
      assert.equal(delivery.delivered, false, "the switch never rode an inputs poll: the refusal was its only transport");
      if (forced) {
        const presented = gates(d.c.run_id) as PlanCrossCheckStateRequest[];
        assert.equal(presented.length, 1, "one presentation, never republished");
        assert.equal(presented[0]!.plan_cross_check_gate_reason, "block");
      }
      if (capture === "giveup") {
        assert.deepEqual(d.attempts, ["attempt", "gave_up"]);
        assert.deepEqual(switchReports(d.c.run_id), ["credential_switch_failed"]);
        assert.ok(api.isApplied(d.c.run_id, rowId), "the give-up returns the refused input to the ready lane and its APPLIED lands");
        if (!forced) {
          assert.ok(statuses(d.c.run_id).includes("completed"), JSON.stringify(statuses(d.c.run_id)));
          assert.equal(d.storage.length, 1, "the stashed approve is adopted");
        }
      } else {
        assert.deepEqual(d.attempts, ["attempt", "released"]);
        assert.deepEqual(switchReports(d.c.run_id), ["credential_switch"]);
        assert.equal(api.isApplied(d.c.run_id, rowId), false, "the refused input stays unapplied for the next claim");
        assert.deepEqual(d.storage, []);
      }
      assert.equal(d.posts().length, 1);
    });
  }

  for (const capture of ["giveup", "release"] as const) {
    it(`ready lane: a gate input disposed while the reservation is installed has its APPLIED refused switch_pending; the refusal latches and the switch is serviced after the applied gate (${capture})`, { skip: LINUX_CAPTURE, timeout: 60_000 }, async () => {
      const watched = watchTripPaths();
      const delivery = watchSwitchDelivery();
      let rowId = 0;
      const d = await drive({ gated: true, capture, verdicts: ["approve"], switchRounds: [], rejectAfterGiveUp: capture === "giveup",
        duringPending: async (c) => {
          api.pendSwitchAfterNextAck(c.run_id);
          // An unreadable gate binding on a human-gated claim is disposed of on arrival: final, so it goes to the ready lane.
          rowId = send(c.run_id, row("reject_plan", "unreadable binding", { gate_binding: "garbage" }))[0]!.id;
          assert.ok(await handlerWait(() => watched.refused > 0), "the ready lane's APPLIED was refused switch_pending");
          assert.ok(await handlerWait(() => delivery.latched()), "the refusal latched under the hold");
        } });
      noInvariant(d.c.run_id);
      assertServicedAfterRelease(d);
      assert.deepEqual(watched.rejectedUnderHold, []);
      assert.ok(rowId > 0);
      assert.equal(delivery.delivered, false, "the switch never rode an inputs poll: the refusal was its only transport");
      assert.equal((gates(d.c.run_id) as PlanCrossCheckStateRequest[]).length, 1, "one presentation, never republished");
      if (capture === "giveup") {
        assert.deepEqual(d.attempts, ["attempt", "gave_up"]);
        assert.deepEqual(switchReports(d.c.run_id), ["credential_switch_failed"]);
        assert.ok(api.isApplied(d.c.run_id, rowId), "the give-up re-arms the parked input and its APPLIED lands");
      } else {
        assert.deepEqual(d.attempts, ["attempt", "released"]);
        assert.deepEqual(switchReports(d.c.run_id), ["credential_switch"]);
        assert.equal(api.isApplied(d.c.run_id, rowId), false, "the parked input stays unapplied for the next claim");
      }
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

  it("a non-switch failure in the re-run's pre-adoption work clears the stashed approve: a later entry never adopts or trips over the stale decision", { skip: LINUX_CAPTURE, timeout: 60_000 }, async () => {
    // The stash (an S1 approve decided under a held switch) is consumed at gatePlan's adoption point, which
    // follows the semantic compile (planning-diff capture). A non-switch error raised there leaves the stash
    // pending in the closure; a later gatePlan entry of the same flight must not meet it. SdkExecutor ends
    // the run on such an error (runThroughSwitch rethrows a non-switch error), so the later entry is driven
    // here by the executor double: the point is that nothing stale survives the failure.
    const c = semanticClaim();
    const events: string[] = [];
    const server = semanticRig(c, events);
    server.force("incomplete");
    const transport = await boundedTransport("claude", server.report, events);
    api.onState(c.run_id, (b) => { events.push(`state:${b.status}`); if (b.status === "awaiting_approval") send(c.run_id, row("approve_plan")); });
    let captures = 0;
    git.capturePlanningDiff = async () => {
      captures++;
      if (captures === 2) throw new Error("planning diff capture failed after the give-up");
      return Buffer.from("");
    };
    const outcome: { rerunError?: unknown; later?: { kind: string }; sessionReported?: boolean } = {};
    const exec: Executor = { run: async (ctx) => {
      failingCapture();
      const call = (md = "Implement A") => ctx.gatePlan!(md, milestones, undefined, undefined, submitted(c.issue_iid));
      try { await call(); assert.fail("the held switch should have been serviced"); }
      catch (error) {
        assert.ok(error instanceof CredentialSwitchSignal, String(error));
        assert.equal(await ctx.attemptCredentialSwitch!(), "gave_up");
        // The give-up re-run fails in its pre-adoption work with a non-switch error.
        outcome.rerunError = await ctx.continueExistingPlanGate!(() => call()).then(() => undefined, (e: unknown) => e);
      }
      // The failed re-run settled the dropped stash's state barrier: a non-checked `running` report (the
      // session-id persist) goes out rather than waiting on it.
      ctx.onSessionId?.("session-after-failed-rerun");
      outcome.sessionReported = await until(() => api.states.some((s) => s.runId === c.run_id &&
        s.body.status === "running" && s.body.session_id === "session-after-failed-rerun"), 4000);
      // A later entry of the same flight presents a different candidate.
      outcome.later = await call("Implement A, as revised after the failure");
      return { branch: ctx.branch };
    } };
    await runner(exec, fakeGitlab().gitlab, undefined, { semanticCoverageTestTransport: transport,
      planCrossCheckTiming: timing, recoveryRetryMs: 1, planApprovalTimeoutMs: 4000 }).execute(c);
    assert.ok(outcome.rerunError instanceof Error && !(outcome.rerunError instanceof CredentialSwitchSignal) &&
      outcome.rerunError.message.includes("planning diff"), `the re-run failed outside the switch path: ${String(outcome.rerunError)}`);
    assert.equal(outcome.sessionReported, true, "a non-checked running report reached the api, not held by the dropped stash's state barrier");
    assert.ok(!everyState(c.run_id).some((s) => s.failure_reason?.includes("stashed decision")),
      "the stale stash was never consulted by the later entry");
    assert.equal(outcome.later?.kind, "approve", `the later entry presented its own gate: ${JSON.stringify(everyState(c.run_id).map((s) => [s.status, s.failure_reason]))}`);
    assert.equal(api.crossCheckRequests.filter((r) => r.method === "POST").length, 1, "the checked decision is never resubmitted or replayed");
    assert.equal(events.filter((e) => e === "invoke").length, 1, "one coverage review: the later entry's");
  });

  it("a second switch in the re-run's pre-adoption work keeps the stashed approve: the next re-run still meets it", { skip: LINUX_CAPTURE, timeout: 60_000 }, async () => {
    const c = semanticClaim();
    const events: string[] = [];
    const server = semanticRig(c, events);
    server.force("incomplete");
    const transport = await boundedTransport("claude", server.report, events);
    const compile = client.submitSemanticCandidate.bind(client);
    let compiles = 0;
    client.submitSemanticCandidate = async (...args) => {
      if (++compiles === 2) throw new CredentialSwitchSignal();
      return compile(...args);
    };
    const outcome: { second?: unknown; third?: unknown } = {};
    const exec: Executor = { run: async (ctx) => {
      failingCapture();
      const call = (md = "Implement A") => ctx.gatePlan!(md, milestones, undefined, undefined, submitted(c.issue_iid));
      try { await call(); assert.fail("the held switch should have been serviced"); }
      catch (error) {
        assert.ok(error instanceof CredentialSwitchSignal, String(error));
        assert.equal(await ctx.attemptCredentialSwitch!(), "gave_up");
        // A second switch interrupts the re-run's pre-adoption work; its service is not repeated here.
        outcome.second = await ctx.continueExistingPlanGate!(() => call()).then(() => undefined, (e: unknown) => e);
        // The stash survived that switch exit: a re-run presenting a different candidate meets it and fails closed.
        outcome.third = await ctx.continueExistingPlanGate!(() => call("Implement A, changed")).then(() => undefined, (e: unknown) => e);
      }
      return { branch: ctx.branch };
    } };
    await runner(exec, fakeGitlab().gitlab, undefined, { semanticCoverageTestTransport: transport,
      planCrossCheckTiming: timing, recoveryRetryMs: 1, planApprovalTimeoutMs: 4000 }).execute(c);
    assert.ok(outcome.second instanceof CredentialSwitchSignal, String(outcome.second));
    assert.ok(outcome.third instanceof Error && outcome.third.message.includes("stashed decision does not match the re-presented candidate"),
      `the stash was kept across the switch exit: ${String(outcome.third)}`);
    assert.equal(api.crossCheckRequests.filter((r) => r.method === "POST").length, 1, "never resubmitted");
    assert.equal(events.filter((e) => e === "invoke").length, 0, "no coverage review runs on a mismatched adoption");
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

/** Issue #3974: while a switch is withheld the api serves no input rows, so an owner Cancel never arrives
 *  through GET inputs; the worker's ownership probe finds it. Every case below ends the run cancelled
 *  without servicing the switch. */
function assertCancelledNotSwitched(d: Driven): void {
  const { c, tl } = d;
  const ev = tl.events;
  failedWith(c.run_id, "run cancelled");
  assert.deepEqual(switchReports(c.run_id), [], "no credential_switch report of any kind");
  assert.deepEqual(ev.filter((e) => e === "capture"), [], "no capture");
  assert.deepEqual(d.attempts, [], "the switch is never attempted");
  assert.deepEqual(d.storage, [], "no plan is stored");
  assert.ok(d.cancelIds.length > 0, "a cancel row was sent");
  const sent = ev.indexOf("cancel-sent");
  assert.ok(sent >= 0, JSON.stringify(ev));
  assert.ok(!ev.slice(sent).includes("state:awaiting_approval"), `no gate is presented after the cancel: ${JSON.stringify(ev)}`);
  for (const id of d.cancelIds) {
    assert.equal(api.isAcked(c.run_id, id), false, "the old flight never ACKed the cancel row");
    assert.equal(api.isApplied(c.run_id, id), false, "the old flight never applied the cancel row");
    assert.deepEqual(api.timeline.filter((e) => e.type !== "state" && e.runId === c.run_id && e.ids.includes(id)), [],
      "the cancel row was never receipted");
  }
  const probe = ev.indexOf("ownership:true", sent);
  const failed = ev.indexOf("state:failed", sent);
  assert.ok(probe > sent, `an ownership probe answered the cancel after it was sent: ${JSON.stringify(ev)}`);
  assert.ok(failed > probe, `the terminal failed report follows the probe: ${JSON.stringify(ev)}`);
  assert.deepEqual(ev.slice(sent, failed).filter((e) => e === "attempt" || e === "capture" || e.startsWith("state:credential_switch")), [],
    "no switch is serviced between the cancel and the terminal report");
  assert.ok(!statuses(c.run_id).includes("completed"));
}

describe("issue #3974: an owner cancel under a withheld credential switch ends the run cancelled", () => {
  it("(a) a hold during checkPlan's pre-submit preparation: no round exists yet", { skip: LINUX_CAPTURE, timeout: 60_000 }, async () => {
    const d = await drive({ capture: "release", switchRounds: [], serveCancellation: true,
      duringPreparation: async (_c, x, signal) => {
        await x.requestSwitch();
        x.cancel();
        assert.ok(await until(() => signal.aborted || x.switchSeen.cancelCommitted()), "the probe committed the cancel");
      } });
    assertCancelledNotSwitched(d);
    assert.equal(d.posts().length, 0, "no round was ever submitted");
    assert.equal(d.counter.calls, 1);
  });

  it("(a2) an in-flight submit is interrupted by the lifecycle signal; a response released later is ignored", { skip: LINUX_CAPTURE, timeout: 60_000 }, async () => {
    let submitSignal: AbortSignal | undefined;
    const submit = client.submitPlanCrossCheck.bind(client);
    client.submitPlanCrossCheck = async (...args: Parameters<typeof submit>) => { submitSignal = args[3]; return submit(...args); };
    restorers.push(() => { client.submitPlanCrossCheck = submit; });
    let interrupted = false;
    const d = await drive({ capture: "release", switchRounds: [], serveCancellation: true,
      duringPending: async (_c, x) => {
        // The round is recorded; the response is held while the switch latches and the cancel is sent.
        await x.requestSwitch();
        x.cancel();
        assert.ok(await until(() => submitSignal?.aborted === true), "the lifecycle abort interrupted the in-flight submit");
        interrupted = true;
      } });
    assert.equal(interrupted, true);
    assertCancelledNotSwitched(d);
    assert.equal(d.posts().length, 1, "the submit was made once and never retried");
  });

  it("(a2) a response released as soon as the probe answered is ignored", { skip: LINUX_CAPTURE, timeout: 60_000 }, async () => {
    const d = await drive({ capture: "release", switchRounds: [], serveCancellation: true,
      duringPending: async (_c, x) => {
        await x.requestSwitch();
        x.cancel();
        assert.ok(await until(() => x.tl.events.includes("ownership:true")), "the probe answered the cancel");
      } });
    assertCancelledNotSwitched(d);
    assert.equal(d.posts().length, 1);
  });

  it("(b) a pending round is interrupted before the checker deadline", { skip: LINUX_CAPTURE, timeout: 60_000 }, async () => {
    const d = await drive({ capture: "release", switchRounds: [1], pending: true, pendingDeadlineMs: 10 * 60_000,
      serveCancellation: true, afterSwitch: async (_c, x) => { x.cancel(); } });
    assertCancelledNotSwitched(d);
    assert.equal(d.posts().length, 1);
    assert.equal(d.counter.calls, 1);
  });

  it("(b) the checker approves at the same instant the probe answers: still nothing stored and no switch", { skip: LINUX_CAPTURE, timeout: 60_000 }, async () => {
    const d = await drive({ capture: "release", switchRounds: [1], pending: true, pendingDeadlineMs: 10 * 60_000,
      serveCancellation: true, afterSwitch: async (_c, x) => {
        x.cancel();
        void until(() => x.tl.events.includes("ownership:true")).then(() => x.flip(1, "approve"));
      } });
    assertCancelledNotSwitched(d);
    assert.equal(d.counter.calls, 1);
  });
});

/** Hold the next gate presentation's ACK (the reservation stays installed, the hold open). Once it is
 *  persisted the switch latches and the cancel is sent; the ACK is released when the cancel has been
 *  committed ("committed") or as soon as the probe answered it ("answered": the commit and the ACK race). */
const holdGateThenCancel = (release: "committed" | "answered") => (c: ReturnType<typeof claim>, x: DriveControl): void => {
  let go!: () => void;
  const held = new Promise<void>((r) => (go = r));
  api.afterPersistState(c.run_id, (b) => b.status === "awaiting_approval", held);
  x.onState((b) => {
    if (b.status !== "awaiting_approval") return;
    void (async () => {
      try {
        await x.requestSwitch();
        x.cancel();
        await until(() => release === "committed" ? x.switchSeen.cancelCommitted() : x.tl.events.includes("ownership:true"));
      } finally { go(); }
    })();
  });
};

describe("issue #3974: an owner cancel after the checker's verdict, with the gate presented", () => {
  const cases: [string, Omit<DriveOpts, "capture">][] = [
    ["gated_pass", { gated: true, verdicts: ["approve"] }],
    ["human path: checker block", { manual: true, verdicts: ["block"] }],
    ["human path: checker timed out", { manual: true, pending: true }],
  ];
  for (const [name, opts] of cases) for (const release of ["committed", "answered"] as const) {
    it(`${name}: the presentation ACK is held until the cancel is ${release}; the gate never services the switch`, { skip: LINUX_CAPTURE, timeout: 60_000 }, async () => {
      const d = await drive({ capture: "release", switchRounds: [], serveCancellation: true, ...opts, setup: holdGateThenCancel(release) });
      assertCancelledNotSwitched(d);
      assert.equal(d.counter.calls, 1, "no implementation");
      assert.equal(gates(d.c.run_id).length, 1, "the one presentation predates the cancel");
    });
  }
  // The gate is presented and applied; completePreparation then owns the remaining preparation attempts.
  it("forced gate during completePreparation at awaiting_approval", { skip: LINUX_CAPTURE, timeout: 60_000 }, async () => {
    const d = await drive({ capture: "release", switchRounds: [], serveCancellation: true, persistentPrefix: true,
      runnerOpts: { planCrossCheckTiming: { ...timing, backoffMs: [600_000, 600_000] as const } },
      setup: (_c, x) => x.onState((b) => {
        if (b.status !== "awaiting_approval") return;
        void (async () => { await x.requestSwitch(); x.cancel(); })();
      }) });
    assertCancelledNotSwitched(d);
    assert.equal(api.crossCheckRequests.length, 0, "the failed preparation never submits");
    assert.equal(d.probe.reservations.released, 0, "the reservation was never released, so nothing was serviced");
  });

  it("re-plan gate: the cancel latched while prepareGateCheckpoint is blocked", { skip: LINUX_CAPTURE, timeout: 60_000 }, async () => {
    client.protocolFeatures = [...client.protocolFeatures, "milestone_reconciliation_v1"];
    const seam = RunRunner.prototype as unknown as { prepareGateCheckpoint: (...args: unknown[]) => Promise<unknown> };
    const prepare = seam.prepareGateCheckpoint;
    restorers.push(() => { seam.prepareGateCheckpoint = prepare; });
    const blocked: { entered: boolean; whileBlocked?: { cancelled: boolean; gates: number; switchReports: string[]; ownerAborted: boolean } } = { entered: false };
    let ownerSignal: AbortSignal | undefined;
    seam.prepareGateCheckpoint = async function (this: unknown, ...args: unknown[]) {
      blocked.entered = true;
      ownerSignal = args[2] as AbortSignal;
      await seamHold(args[1] as { runId?: string } | undefined);
      return prepare.apply(this, args);
    };
    let seamHold!: (flight?: { runId?: string }) => Promise<void>;
    const d = await drive({ capture: "release", switchRounds: [], serveCancellation: true, manual: true, verdicts: ["block"],
      claimOver: () => ({ execution_milestones: [{ id: "frozen", title: "A frozen milestone the candidate drops" }] }),
      setup: (c, x) => {
        seamHold = async () => {
          await x.requestSwitch();
          x.cancel();
          assert.ok(await until(() => x.switchSeen.cancelCommitted()), "the probe committed the cancel");
          blocked.whileBlocked = { cancelled: x.switchSeen.isCancelled(), gates: gates(c.run_id).length,
            switchReports: switchReports(c.run_id), ownerAborted: ownerSignal?.aborted === true };
        };
      } });
    assert.equal(blocked.entered, true, "the run reached prepareGateCheckpoint");
    assert.deepEqual(blocked.whileBlocked, { cancelled: true, gates: 0, switchReports: [], ownerAborted: true },
      "while blocked: cancelled, no gate presented, no switch serviced");
    assertCancelledNotSwitched(d);
    assert.equal(gates(d.c.run_id).length, 0, "the gate was never presented");
  });
});

describe("issue #3974: lead kinds, ordering and the rows a withheld switch hides", () => {
  it("(d) an mr_rework lead enrolled in the required cross-check", { skip: LINUX_CAPTURE, timeout: 60_000 }, async () => {
    const d = await drive({ capture: "release", switchRounds: [1], pending: true, pendingDeadlineMs: 10 * 60_000,
      serveCancellation: true, claimOver: (base) => ({ kind: "mr_rework", branch: `agent/issue-${base.issue_iid}`, base_branch: "main" }), afterSwitch: async (_c, x) => { x.cancel(); } });
    assert.equal(d.c.kind, "mr_rework");
    assertCancelledNotSwitched(d);
    assert.equal(api.crossCheckRequests.filter((r) => r.method === "POST").length, 1, "the mr_rework lead did submit a round");
  });

  it("(e) reverse order: the cancel row is queued before the switch stamp, both before the worker's next poll", { skip: LINUX_CAPTURE, timeout: 60_000 }, async () => {
    // duringPending runs, then the stamp is requested with no await in between: no poll can land in between.
    const d = await drive({ capture: "release", switchRounds: [1], pending: true, pendingDeadlineMs: 10 * 60_000,
      serveCancellation: true, duringPending: async (_c, x) => { x.cancel(); } });
    const ev = d.tl.events;
    assert.ok(ev.indexOf("cancel-sent") >= 0 && ev.indexOf("cancel-sent") < ev.indexOf("switch-requested"), JSON.stringify(ev));
    assertCancelledNotSwitched(d);
    assert.deepEqual(ev.slice(0, ev.indexOf("cancel-sent")).filter((e) => e === "ownership:true"), [], "no probe answered before the cancel existed");
  });

  it("(f) an answer and a follow_up sent after the stamp are never ACKed or receipted by the old flight", { skip: LINUX_CAPTURE, timeout: 60_000 }, async () => {
    const others: number[] = [];
    const d = await drive({ capture: "release", switchRounds: [1], pending: true, pendingDeadlineMs: 10 * 60_000,
      serveCancellation: true, afterSwitch: async (c, x) => {
        others.push(send(c.run_id, row("answer", "an answer to nothing"))[0]!.id);
        others.push(send(c.run_id, row("follow_up", "a note after the stamp"))[0]!.id);
        x.cancel();
      } });
    assertCancelledNotSwitched(d);
    assert.equal(others.length, 2);
    for (const id of others) {
      assert.equal(api.isAcked(d.c.run_id, id), false, `row ${id} was never ACKed`);
      assert.equal(api.isApplied(d.c.run_id, id), false, `row ${id} was never applied`);
      assert.deepEqual(api.timeline.filter((e) => e.type !== "state" && e.runId === d.c.run_id && e.ids.includes(id)), [],
        `row ${id} was never receipted`);
    }
  });
});

describe("issue #3974: recovery custody parity with an ordinary delivered cancel", () => {
  /** Every custody call the run makes, in order, with the arguments that name a generation or capture. */
  async function custodyRun(mode: "withheld" | "delivered"): Promise<{ calls: string[]; d: Driven }> {
    const root = fs.mkdtempSync(path.join(fs.realpathSync(os.tmpdir()), "switch-custody-parity-"));
    const calls: string[] = [];
    const archive: RecoveryArchiveClient = {
      hasFeature: (feature: string) => feature === "recovery_inventory_v1",
      reserveRecoveryCapture: async (_id, req) => { calls.push(`reserve g${req.generation}`); return { capture_id: "custody-capture", state: "preparing" }; },
      getRecoveryCaptureStatus: async (_id, captureId) => { calls.push("status"); return { capture_id: captureId, state: "preparing", manifest_bound: false }; },
      uploadRecoveryBundle: async (_id, captureId, _manifest, bundle) => {
        calls.push("upload");
        for await (const _chunk of bundle) { /* Consume the real Git bundle. */ }
        return { capture_id: captureId, state: "available", manifest_bound: true };
      },
      releaseRecoveryCustody: async (id, generation) => { calls.push(`release g${generation}`); return { run_id: id, released: true, holds_released: 1 }; },
      listRecoveryHolds: async (id) => { calls.push("holds"); return { run_id: id, holds: [{ hold_id: "hold-g1", generation: 1,
        inventory_guarded: true, has_available_capture: false }] }; },
    };
    const recovery = new RecoveryCoordinator({ client: archive, git, log: nullLogger(),
      recoveryRoot: path.join(root, "recovery"), workerToken: "custody-parity-fixture" });
    try {
      const d = mode === "withheld"
        ? await drive({ capture: "release", switchRounds: [1], pending: true, pendingDeadlineMs: 10 * 60_000, serveCancellation: true,
          runnerOpts: { recovery }, setup: (c) => { c.inventory_guarded = true; }, afterSwitch: async (_c, x) => { x.cancel(); } })
        : await drive({ capture: "release", switchRounds: [], runnerOpts: { recovery }, setup: (c) => { c.inventory_guarded = true; },
          duringPending: async (c, x) => { const id = x.cancel(); await routed(c.run_id, id); } });
      return { calls, d };
    } finally { fs.rmSync(root, { recursive: true, force: true }); }
  }

  it("a cancel found under a withheld switch makes exactly the custody calls an ordinary delivered cancel makes, and none switch-specific", { skip: LINUX_CAPTURE, timeout: 120_000 }, async () => {
    const withheld = await custodyRun("withheld");
    const delivered = await custodyRun("delivered");
    failedWith(withheld.d.c.run_id, "run cancelled");
    failedWith(delivered.d.c.run_id, "run cancelled");
    assert.deepEqual(switchReports(withheld.d.c.run_id), []);
    // A recovery-enabled run captures its own work when it ends; the cancel through the new path must capture
    // exactly as often as the ordinary delivered cancel does, so none of it is the switch's.
    const captures = (d: Driven) => d.tl.events.filter((e) => e === "capture").length;
    assert.equal(captures(withheld.d), captures(delivered.d), `captures ${captures(withheld.d)} vs ${captures(delivered.d)}`);
    assert.deepEqual(withheld.d.attempts, []);
    assert.deepEqual(withheld.calls, delivered.calls, "the same custody calls, in the same order");
    // A switch's own custody work would reserve a capture for the claim's generation; neither run may.
    assert.deepEqual(withheld.calls.filter((call) => call.startsWith("reserve") || call.startsWith("release") || call === "upload"), [],
      `no capture is reserved or uploaded and no custody hold is released by the switch path: ${JSON.stringify(withheld.calls)}`);
    assert.ok(withheld.calls.length > 0, "the coordinator was exercised");
  });
});

describe("issue #3974: the runner never enters a switch once a cancel was committed under it", () => {
  /** A plain issue run (no cross-check) whose executor opens a deferred-switch window (the revision-turn
   *  window), sees the stamp, has the cancel committed, and then misbehaves as `after` says. */
  async function deferredRun(after: (ctx: RunContext) => Promise<unknown>) {
    api.deriveCancellationRequested = true;
    const c = freshClaim(1, { kind: "issue", auto_approve: true });
    const seen = watchSwitchDelivery();
    const events: string[] = [];
    api.onState(c.run_id, (b) => events.push(`state:${b.status}`));
    const entered: string[] = [];
    const enter = (RunRunner.prototype as unknown as { enterCredentialSwitch: (...a: unknown[]) => Promise<unknown> }).enterCredentialSwitch;
    (RunRunner.prototype as unknown as { enterCredentialSwitch: unknown }).enterCredentialSwitch = function (this: unknown, ...a: unknown[]) {
      entered.push("enterCredentialSwitch");
      return enter.apply(this, a);
    };
    restorers.push(() => { (RunRunner.prototype as unknown as { enterCredentialSwitch: unknown }).enterCredentialSwitch = enter; });
    healthyCapture();
    const wip = git.worktreeStatus.bind(git);
    git.worktreeStatus = (async (...a: Parameters<typeof wip>) => { events.push("capture"); return wip(...a); }) as typeof git.worktreeStatus;
    let cancelId = 0;
    const exec: Executor = { run: async (ctx) => {
      await ctx.deferCredentialSwitch!(async () => {
        api.requestCredentialSwitch(c.run_id, 1);
        assert.ok(await until(() => seen.delivered), "the worker polled the switch inside the window");
        cancelId = send(c.run_id, row("cancel"))[0]!.id;
        assert.ok(await until(() => seen.cancelCommitted()), "the probe committed the cancel");
        await after(ctx);
      });
      return { branch: ctx.branch };
    } };
    await runner(exec, fakeGitlab().gitlab, undefined, { recoveryRetryMs: 1 }).execute(c);
    assert.ok(cancelId > 0);
    failedWith(c.run_id, "run cancelled");
    assert.deepEqual(entered, [], "no switch attempt (enterCredentialSwitch) was made");
    assert.deepEqual(events.filter((e) => e === "capture"), [], "no capture");
    assert.deepEqual(switchReports(c.run_id), [], "no credential_switch* report");
    assert.equal(api.isAcked(c.run_id, cancelId), false);
    assert.equal(api.isApplied(c.run_id, cancelId), false);
    return { c, events, seen };
  }

  it("a CredentialSwitchSignal that escapes after the commit becomes the ordinary cancelled failure (dispatchFailure)", { skip: LINUX_CAPTURE, timeout: 60_000 }, async () => {
    await deferredRun(async () => { throw new CredentialSwitchSignal(); });
  });

  it("an executor call of ctx.attemptCredentialSwitch after the commit is refused with the cancelled error", { skip: LINUX_CAPTURE, timeout: 60_000 }, async () => {
    let refused: unknown;
    await deferredRun(async (ctx) => {
      refused = await ctx.attemptCredentialSwitch!().then(() => undefined, (e: unknown) => e);
      throw refused;
    });
    assert.ok(refused instanceof Error && refused.message === "run cancelled", String(refused));
  });

  it("a deliberate hold service-close after the commit trips nothing", { skip: LINUX_CAPTURE, timeout: 60_000 }, async () => {
    const d = await drive({ capture: "release", switchRounds: [1], pending: true, pendingDeadlineMs: 10 * 60_000,
      serveCancellation: true, afterSwitch: async (_c, x) => {
        x.cancel();
        assert.ok(await until(() => x.switchSeen.cancelCommitted()), "the probe committed the cancel");
        x.switchSeen.serviceHolds();
      } });
    assertCancelledNotSwitched(d);
  });
});

// ─── Issue #3980: the changed-plan gate checkpoint on a Claude executor is bounded ───────────────────
// prepareGateCheckpoint used to enforce its 60 s deadline only through the Codex capture lane. A Claude
// executor has none, so one hung clone-side git child, a held bare lock, an unsettled executor join or a
// sink gate that never frees could hold the plan gate (and the held credential switch behind it) forever.
// These drive the real runner with a real GitCache capture (capture: "real"), the switch held, and a
// shortened deadline; each regression observes the hang it injected first, then requires the step to
// settle (fail closed, TransientRecoveryError, custody intact) BEFORE a watchdog that would release it.
const GATE_MS = 1500;
/** Scheduling margin: the longest tail past the deadline (children are aborted, then joined). */
const GATE_TAIL_MS = 3000;

interface TickGroupView { pgid: number; identity: string; killConfirmed: boolean }
interface GateFlightView { runId: string; worktreePath?: string; barePath?: string; preserveRecoveryClone: boolean; preserveSession: boolean;
  sinkGate: { run<T>(fn: () => Promise<T>): Promise<T> }; survivingTickGroups: unknown[]; runLog: Logger }
interface Custody { preserveRecoveryClone: boolean; preserveSession: boolean; groups: TickGroupView[]; gates: number; switchReports: string[];
  reservations: { installed: number; released: number }; tree?: TreeView; indexLock?: boolean }
interface GateRec { entered: boolean; startedAt: number; settledAt: number; error: unknown; watchdogFired: boolean;
  before?: Custody; after?: Custody; flight?: GateFlightView }

const runGitSync = (cwd: string, ...args: string[]): string =>
  execFileSync("git", ["-C", cwd, ...args], { encoding: "utf8", env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" } });
interface TreeView { head: string; tracked: string; untracked: string; trackedPath: string; untrackedPath: string }
function viewTree(wt: string, trackedPath: string, untrackedPath: string): TreeView {
  return { head: runGitSync(wt, "rev-parse", "HEAD").trim(), trackedPath, untrackedPath,
    tracked: fs.readFileSync(path.join(wt, trackedPath), "utf8"), untracked: fs.readFileSync(path.join(wt, untrackedPath), "utf8") };
}

/** Synchronous aborts a hang may fire: the owner (lifecycle) signal handed to the step, or the runner's shutdown signal. */
interface GateAborts { abortOwner: (reason: unknown) => void; abortShutdown: (reason: unknown) => void }

/** Run one claim whose human re-plan gate reaches prepareGateCheckpoint on the real Claude path, with the
 *  switch held. `hang` injects the failure and returns its release; `entered` must come true on its own. */
async function gateCheckpointRun(o: {
  hang: (flight: GateFlightView, x: DriveControl, rec: GateRec, abort: GateAborts) => Promise<{ release: () => void; entered: () => boolean }>;
  dirty?: boolean; forward?: Partial<SdkExecutorOptions>; tailMs?: number; deadlineMs?: number;
  runnerOpts?: Record<string, unknown>;
  /** Abort the lifecycle (owner) signal handed to the step this many ms after it starts, with this reason. */
  ownerAbort?: { afterMs: number; reason: Error };
}) {
  const deadlineMs = o.deadlineMs ?? GATE_MS;
  client.protocolFeatures = [...client.protocolFeatures, "milestone_reconciliation_v1"];
  const seam = RunRunner.prototype as unknown as { prepareGateCheckpoint: (...args: unknown[]) => Promise<unknown> };
  const prepare = seam.prepareGateCheckpoint;
  restorers.push(() => { seam.prepareGateCheckpoint = prepare; });
  const rec: GateRec = { entered: false, startedAt: 0, settledAt: 0, error: undefined, watchdogFired: false };
  let release = (): void => {};
  let hangEntered = (): boolean => false;
  let ctl!: DriveControl;
  let treeBefore: TreeView | undefined;
  let trackedPath = "";
  const untrackedPath = "untracked-by-test.txt";
  const custody = (flight: GateFlightView, c: { run_id: string }): Custody => ({
    preserveRecoveryClone: flight.preserveRecoveryClone, preserveSession: flight.preserveSession,
    groups: (flight.survivingTickGroups as TickGroupView[]).map((g) => ({ ...g })), gates: gates(c.run_id).length, switchReports: switchReports(c.run_id),
    reservations: { ...ctl.reservations },
    ...(flight.worktreePath ? { indexLock: fs.existsSync(path.join(flight.worktreePath, ".git", "index.lock")) } : {}),
  });
  let claimId = "";
  seam.prepareGateCheckpoint = async function (this: unknown, ...args: unknown[]) {
    const flight = args[1] as GateFlightView;
    rec.entered = true;
    rec.flight = flight;
    await ctl.requestSwitch();
    if (o.dirty && flight.worktreePath) {
      const wt = flight.worktreePath;
      trackedPath = runGitSync(wt, "ls-files").split("\n").filter(Boolean)[0]!;
      fs.appendFileSync(path.join(wt, trackedPath), "\nedited by the gate checkpoint test\n");
      fs.writeFileSync(path.join(wt, untrackedPath), "untracked work\n");
      treeBefore = viewTree(wt, trackedPath, untrackedPath);
    }
    // The owner signal is merged in before `hang` runs, so a hang can abort it synchronously.
    const owner = new AbortController();
    args[2] = AbortSignal.any([args[2] as AbortSignal, owner.signal]);
    const abort: GateAborts = {
      abortOwner: (reason) => owner.abort(reason),
      abortShutdown: (reason) => (this as unknown as { shutdownSignal: AbortController }).shutdownSignal.abort(reason),
    };
    const injected = await o.hang(flight, ctl, rec, abort);
    release = injected.release;
    hangEntered = injected.entered;
    rec.before = custody(flight, { run_id: claimId });
    // The watchdog frees the hang only after the allowed tail, so a step that never bounds itself
    // fails the settle-time assertion rather than the node test timeout.
    const watchdog = setTimeout(() => { rec.watchdogFired = true; release(); }, deadlineMs + (o.tailMs ?? GATE_TAIL_MS) + 2000);
    rec.startedAt = Date.now();
    let ownerTimer: NodeJS.Timeout | undefined;
    if (o.ownerAbort) {
      ownerTimer = setTimeout(() => owner.abort(o.ownerAbort!.reason), o.ownerAbort.afterMs);
    }
    try { return await prepare.apply(this, args); }
    catch (error) { rec.error = error; throw error; }
    finally {
      rec.settledAt = Date.now();
      clearTimeout(watchdog);
      clearTimeout(ownerTimer);
      rec.after = custody(flight, { run_id: claimId });
      if (treeBefore && flight.worktreePath) {
        const after = viewTree(flight.worktreePath, trackedPath, untrackedPath);
        rec.after!.tree = after;
      }
      // The run's own teardown needs what the injected hang held (the bare lock, the sink gate).
      release();
    }
  };
  const d = await drive({ capture: "real", switchRounds: [], serveCancellation: true, manual: true, verdicts: ["block"],
    ...(o.forward ? { forward: o.forward } : {}),
    planMilestones: [{ id: "changed", title: "A milestone the human has not approved" }],
    claimOver: () => ({ execution_milestones: [{ id: "frozen", title: "A frozen milestone the candidate drops" }] }),
    runnerOpts: { gateCheckpointDeadlineMs: deadlineMs, ...o.runnerOpts },
    setup: (c, x) => { ctl = x; claimId = c.run_id; } });
  release();
  return { rec, d, entered: hangEntered(), treeBefore, trackedPath, untrackedPath, deadlineMs };
}

function assertGateFailedClosed(r: Awaited<ReturnType<typeof gateCheckpointRun>>, tailMs = GATE_TAIL_MS): void {
  const deadlineMs = r.deadlineMs;
  const { rec } = r;
  assert.equal(rec.entered, true, "the run reached prepareGateCheckpoint");
  assert.equal(r.entered, true, "the injected hang was entered");
  assert.equal(rec.watchdogFired, false, `the step settled by itself (took ${rec.settledAt - rec.startedAt} ms): ${String(rec.error)}`);
  assert.ok(rec.settledAt - rec.startedAt <= deadlineMs + tailMs,
    `settled ${rec.settledAt - rec.startedAt} ms after entry; allowed ${deadlineMs + tailMs}`);
  assert.ok(rec.error instanceof TransientRecoveryError, `TransientRecoveryError, got ${String(rec.error)}`);
  assert.equal(rec.after!.preserveRecoveryClone, true, "the recovery clone stays preserved");
  assert.equal(rec.after!.gates, 0, "no gate presented");
  assert.deepEqual(rec.after!.switchReports, [], "no switch serviced");
  assert.deepEqual(rec.after!.reservations, rec.before!.reservations, "the step left the reservation untouched");
  assert.notEqual(rec.after!.indexLock, true, "no .git/index.lock left behind");
}
function assertCustodyIntact(r: Awaited<ReturnType<typeof gateCheckpointRun>>): void {
  const before = r.treeBefore!, after = r.rec.after!.tree!;
  assert.equal(after.tracked, before.tracked, "tracked edit byte-identical");
  assert.equal(after.untracked, before.untracked, "untracked file byte-identical");
  // HEAD is either untouched or a direct child of the recorded HEAD that holds both pieces of work verbatim.
  if (after.head !== before.head) {
    const wt = r.rec.flight!.worktreePath!;
    assert.equal(runGitSync(wt, "rev-parse", `${after.head}^`).trim(), before.head, "a new HEAD is a direct child of the recorded HEAD");
    assert.equal(runGitSync(wt, "show", `${after.head}:${before.trackedPath}`), before.tracked, "the new HEAD holds the tracked edit byte-identically");
    assert.equal(runGitSync(wt, "show", `${after.head}:${before.untrackedPath}`), before.untracked, "and the untracked file byte-identically");
  }
  // The step itself never sets preserveSession on this path: it must not be reset relative to before.
  if (r.rec.before!.preserveSession) assert.equal(r.rec.after!.preserveSession, true, "preserveSession is not reset");
}

describe("issue #3980: the Claude gate checkpoint is bounded", () => {
  it("(1) a hung clone-side git child (a planted clean filter) is killed and joined by the deadline; the filter's descendants may outlive the step and are reaped by later quiesce", { skip: LINUX_CAPTURE, timeout: 60_000 }, async () => {
    const marker = path.join(homeDir, "filter-entered");
    const orphans = (): number[] => (fs.existsSync(marker) ? fs.readFileSync(marker, "utf8").split("\n").map(Number).filter((n) => n > 0) : []);
    const killOrphans = (): void => { for (const pid of orphans()) { try { process.kill(pid, "SIGKILL"); } catch { /* gone */ } } };
    try {
      const r = await gateCheckpointRun({
        dirty: true,
        hang: async (flight) => {
          const wt = flight.worktreePath!;
          const script = path.join(homeDir, "hang-filter.sh");
          fs.writeFileSync(script, `#!/bin/sh\necho $$ >> '${marker}'\nsleep 300 &\necho $! >> '${marker}'\nexec sleep 300\n`, { mode: 0o755 });
          runGitSync(wt, "config", "filter.hang.clean", script);
          fs.writeFileSync(path.join(wt, ".gitattributes"), "* filter=hang\n");
          // Releasing also unplants the filter, so a run that never bounded itself is not re-hung by its
          // later git calls and fails by assertion rather than by the node timeout.
          const release = (): void => {
            try { runGitSync(wt, "config", "--unset", "filter.hang.clean"); } catch { /* already unset */ }
            killOrphans();
            // A git that already loaded the filter starts a new instance per file: keep killing for a while.
            const sweep = setInterval(killOrphans, 100);
            setTimeout(() => clearInterval(sweep), 3000).unref();
            sweep.unref();
          };
          return { release, entered: () => fs.existsSync(marker) };
        },
      });
      assertGateFailedClosed(r);
      assertCustodyIntact(r);
    } finally { killOrphans(); }
  });

  it("(2) a hung bare-lock wait ends at the deadline and fails closed", { skip: LINUX_CAPTURE, timeout: 60_000 }, async () => {
    let releaseLock = (): void => {};
    let queued = 0;
    const r = await gateCheckpointRun({
      dirty: true,
      hang: async (flight) => {
        const bare = flight.barePath!;
        const internals = git as unknown as { withLock: (key: string, fn: () => Promise<unknown>, signal?: AbortSignal) => Promise<unknown> };
        const original = internals.withLock;
        let held = false;
        internals.withLock = function (this: unknown, key, fn, signal) {
          if (held && key === bare) queued++;
          return original.call(this, key, fn, signal);
        };
        void original.call(git, bare, () => new Promise<void>((resolve) => { held = true; releaseLock = resolve; })).catch(() => undefined);
        assert.ok(await until(() => held), "the test-owned holder took the bare lock");
        return { release: () => releaseLock(), entered: () => queued > 0 };
      },
    });
    assertGateFailedClosed(r);
    assertCustodyIntact(r);
  });

  it("(3) an executor join (the guarded-lane disposal) that never settles cannot hold the attributed reap past the deadline", { skip: LINUX_CAPTURE, timeout: 60_000 }, async () => {
    let unblock = (): void => {};
    const reapOptions: unknown[] = [];
    const r = await gateCheckpointRun({
      dirty: true, forward: {},
      hang: async (_flight, x) => {
        const sdk = x.sdk!;
        (sdk as unknown as { laneDisposal?: Promise<void> }).laneDisposal = new Promise<void>((resolve) => { unblock = resolve; });
        const reap = sdk.reapAttributedProcesses.bind(sdk);
        sdk.reapAttributedProcesses = ((opts?: { deadlineAt?: number }) => { reapOptions.push(opts); return reap(opts); }) as typeof sdk.reapAttributedProcesses;
        return { release: () => unblock(), entered: () => reapOptions.length > 0 };
      },
    });
    assertGateFailedClosed(r);
    assertCustodyIntact(r);
    const first = reapOptions[0] as { deadlineAt?: number } | undefined;
    assert.ok(first && Number.isFinite(first.deadlineAt), `the reap carried the gate deadline: ${JSON.stringify(first)}`);
    assert.ok(first!.deadlineAt! <= r.rec.startedAt + r.deadlineMs + 50, "and it is this step's deadline");
  });

  it("(4) a sink gate held by another gated operation cannot hold the capture's admission past its deadline", { skip: LINUX_CAPTURE, timeout: 60_000 }, async () => {
    let releaseGate = (): void => {};
    let queued = 0;
    const r = await gateCheckpointRun({
      dirty: true,
      hang: async (flight) => {
        const gate = flight.sinkGate as unknown as { run: (fn: () => Promise<unknown>, stop?: unknown) => Promise<unknown> };
        const original = gate.run.bind(gate);
        let held = false;
        gate.run = ((fn: () => Promise<unknown>, stop?: unknown) => { if (held) queued++; return original(fn, stop); }) as typeof gate.run;
        void original(() => new Promise<void>((resolve) => { held = true; releaseGate = resolve; })).catch(() => undefined);
        assert.ok(await until(() => held), "the test-owned holder took the sink gate");
        return { release: () => releaseGate(), entered: () => queued > 0 };
      },
    });
    assertGateFailedClosed(r);
    assertCustodyIntact(r);
  });

  it("(5) a reap that consumes the deadline leaves no further proof, helper pass or Docker request admitted", { skip: LINUX_CAPTURE, timeout: 60_000 }, async () => {
    const reaps: { deadlineAt?: number; at: number }[] = [];
    const quiesces: { deadlineAt?: number; at: number; process?: string; docker?: string; dockerHost?: string }[] = [];
    let entered = false;
    let freeReap = (): void => {};
    const held = new Promise<void>((resolve) => { freeReap = resolve; });
    const r = await gateCheckpointRun({
      dirty: true,
      forward: {
        runProcesses: {
          scan: async () => ({ pids: [], complete: true }),
          // Honours a deadline when handed one; otherwise stays pending until the watchdog releases it, so
          // an unbounded step outlives the allowed settle bound rather than finishing early by itself.
          reap: async (_home, _worktree, _pids, opts) => {
            reaps.push({ deadlineAt: opts?.deadlineAt, at: Date.now() });
            entered = true;
            await Promise.race([held, ...(opts?.deadlineAt === undefined ? [] : [(async () => {
              await new Promise((resolve) => setTimeout(resolve, Math.max(0, opts.deadlineAt! - Date.now())));
              // A timer can fire ~1 ms before Date.now() reaches the deadline: wait it out, plus a margin.
              while (Date.now() < opts.deadlineAt!) await new Promise((resolve) => setTimeout(resolve, 1));
              await new Promise((resolve) => setTimeout(resolve, 10));
            })()])]);
            return { killed: [], left: [], complete: true };
          },
        },
      },
      runnerOpts: { quiesceRun: async (req: QuiesceRunRequest) => {
        const outcome = await quiesceRunAttempt(req);
        quiesces.push({ deadlineAt: req.deadlineAt, at: Date.now(), process: outcome.process?.state, docker: outcome.docker.detail, dockerHost: req.dockerHost });
        return outcome;
      } },
      hang: async () => ({ release: () => freeReap(), entered: () => entered }),
    });
    assertGateFailedClosed(r);
    assertCustodyIntact(r);
    assert.equal(reaps.length, 1, `exactly one attributed reap: ${JSON.stringify(reaps)}`);
    assert.ok(Number.isFinite(reaps[0]!.deadlineAt), "the reap carried the gate deadline");
    const inStep = quiesces.filter((q) => q.at >= r.rec.startedAt && q.at <= r.rec.settledAt);
    assert.deepEqual(inStep, [], `no proof, helper pass or Docker request is admitted once the reap consumed the deadline: ${JSON.stringify(inStep)}`);
  });

  it("(5b) a plan-summary settle that honours its cap and expires the deadline leaves no attributed reap or proof admitted", { skip: LINUX_CAPTURE, timeout: 60_000 }, async () => {
    const settles: { deadlineMs: number | undefined; at: number }[] = [];
    const reaps: number[] = [];
    const quiesces: number[] = [];
    let freeSettle = (): void => {};
    const held = new Promise<void>((resolve) => { freeSettle = resolve; });
    const r = await gateCheckpointRun({
      dirty: true, forward: {},
      runnerOpts: { quiesceRun: async (req: QuiesceRunRequest) => { quiesces.push(Date.now()); return quiesceRunAttempt(req); } },
      hang: async (_flight, x) => {
        const sdk = x.sdk!;
        // Honours its deadlineMs argument; with none (an uncapped settle) it stays pending until released.
        sdk.closeAndSettlePlanSummary = (async (deadlineMs?: number) => {
          settles.push({ deadlineMs, at: Date.now() });
          // The first settle is runFreeCapture's own; quiesceRun's settle is the one burning the deadline.
          if (settles.length === 1) return;
          if (deadlineMs === undefined) { await held; return; }
          const end = Date.now() + deadlineMs;
          await new Promise((resolve) => setTimeout(resolve, deadlineMs));
          while (Date.now() < end) await new Promise((resolve) => setTimeout(resolve, 1));
          await new Promise((resolve) => setTimeout(resolve, 10));
        }) as typeof sdk.closeAndSettlePlanSummary;
        const reap = sdk.reapAttributedProcesses.bind(sdk);
        sdk.reapAttributedProcesses = ((opts?: { deadlineAt?: number }) => { reaps.push(Date.now()); return reap(opts); }) as typeof sdk.reapAttributedProcesses;
        return { release: () => freeSettle(), entered: () => settles.length > 1 };
      },
    });
    assertGateFailedClosed(r);
    assertCustodyIntact(r);
    const deadlineAt = r.rec.startedAt + r.deadlineMs;
    const capped = settles.slice(1).filter((s) => s.at < deadlineAt); // later calls are the run's own teardown
    assert.ok(capped.length > 0 && capped.every((s) => s.deadlineMs !== undefined), `quiesceRun's settle was capped: ${JSON.stringify(settles)}`);
    for (const s of capped) assert.ok(s.at + s.deadlineMs! <= deadlineAt + 50, `settle cap ${s.deadlineMs} at ${s.at - r.rec.startedAt} ms outlives the gate deadline`);
    assert.deepEqual(reaps.filter((t) => t >= deadlineAt), [], "no attributed reap starts after expiry");
    assert.deepEqual(quiesces.filter((t) => t >= deadlineAt), [], "no proof is admitted after expiry");
  });

  it("(7) an owner abort during a hung sink-gate wait surfaces the owner's reason, not TransientRecoveryError", { skip: LINUX_CAPTURE, timeout: 60_000 }, async () => {
    const reason = new Error("owner cancelled the run (3980 test)");
    let releaseGate = (): void => {};
    let queued = 0;
    const r = await gateCheckpointRun({
      dirty: true, ownerAbort: { afterMs: 600, reason },
      hang: async (flight) => {
        const gate = flight.sinkGate as unknown as { run: (fn: () => Promise<unknown>, stop?: unknown) => Promise<unknown> };
        const original = gate.run.bind(gate);
        let held = false;
        gate.run = ((fn: () => Promise<unknown>, stop?: unknown) => { if (held) queued++; return original(fn, stop); }) as typeof gate.run;
        void original(() => new Promise<void>((resolve) => { held = true; releaseGate = resolve; })).catch(() => undefined);
        assert.ok(await until(() => held), "the test-owned holder took the sink gate");
        return { release: () => releaseGate(), entered: () => queued > 0 };
      },
    });
    assert.equal(r.entered, true, "the injected hang was entered");
    assert.equal(r.rec.watchdogFired, false, "the step settled by itself");
    assert.ok(!(r.rec.error instanceof TransientRecoveryError), `not remapped to TransientRecoveryError: ${String(r.rec.error)}`);
    assert.equal(r.rec.error, reason, "the owner's own reason is what the step rejects with");
    assertCustodyIntact(r);
    assert.equal(r.rec.after!.preserveRecoveryClone, true, "the recovery clone stays preserved");
    assert.equal(r.rec.after!.gates, 0, "no gate presented");
  });

  /** Codex path (`bound` undefined): the executor has a credential-free capture lane. A stub lane aborts via the given
   *  abort and reports `incomplete` without entering the action, so runFreeCapture returns `incomplete` and only the
   *  explicit rethrow after it can keep the abort's reason. The stub is removed on release. */
  async function abortInCodexLane(which: "abortOwner" | "abortShutdown", reason: Error) {
    let calls = 0;
    return gateCheckpointRun({
      dirty: true,
      hang: async (flight, _x, _rec, abort) => {
        const executor = (flight as unknown as { executor: Record<string, unknown> }).executor;
        const had = Object.prototype.hasOwnProperty.call(executor, "withCredentialFreeCapture");
        const prior = executor.withCredentialFreeCapture;
        executor.withCredentialFreeCapture = async () => { calls++; abort[which](reason); return { kind: "incomplete" }; };
        const release = (): void => { if (had) executor.withCredentialFreeCapture = prior; else delete executor.withCredentialFreeCapture; };
        return { release, entered: () => calls > 0 };
      },
    });
  }

  it("(7d) an owner abort while a Codex-path capture lane reports incomplete surfaces the owner's reason, not TransientRecoveryError", { skip: LINUX_CAPTURE, timeout: 60_000 }, async () => {
    const reason = new Error("owner cancelled in the codex lane (3980 test)");
    const r = await abortInCodexLane("abortOwner", reason);
    assert.equal(r.rec.entered, true, "the run reached prepareGateCheckpoint");
    assert.equal(r.entered, true, "the stub capture lane was entered");
    assert.equal(r.rec.watchdogFired, false, "the step settled by itself");
    assert.ok(!(r.rec.error instanceof TransientRecoveryError), `not remapped to TransientRecoveryError: ${String(r.rec.error)}`);
    assert.equal(r.rec.error, reason, "the owner's own reason is what the step rejects with");
  });

  it("(7e) a shutdown abort while a Codex-path capture lane reports incomplete surfaces the shutdown reason, not TransientRecoveryError", { skip: LINUX_CAPTURE, timeout: 60_000 }, async () => {
    const reason = new Error("worker shutting down in the codex lane (3980 test)");
    const r = await abortInCodexLane("abortShutdown", reason);
    assert.equal(r.rec.entered, true, "the run reached prepareGateCheckpoint");
    assert.equal(r.entered, true, "the stub capture lane was entered");
    assert.equal(r.rec.watchdogFired, false, "the step settled by itself");
    assert.ok(!(r.rec.error instanceof TransientRecoveryError), `not remapped to TransientRecoveryError: ${String(r.rec.error)}`);
    assert.equal(r.rec.error, reason, "the shutdown reason is what the step rejects with");
  });

  /** Claude path: abort during runFreeCapture's own (first) settle, via the given abort; the step must keep that reason. */
  async function abortDuringSettle(which: "abortOwner" | "abortShutdown", reason: Error) {
    let settles = 0;
    return gateCheckpointRun({
      dirty: true, forward: {},
      hang: async (_flight, x, _rec, abort) => {
        const sdk = x.sdk!;
        const original = sdk.closeAndSettlePlanSummary.bind(sdk);
        sdk.closeAndSettlePlanSummary = (async (deadlineMs?: number) => {
          settles++;
          if (settles === 1) { abort[which](reason); return; }
          return original(deadlineMs);
        }) as typeof sdk.closeAndSettlePlanSummary;
        return { release: () => undefined, entered: () => settles > 0 };
      },
    });
  }

  it("(7b) Claude path pin: an owner abort during the plan-summary settle keeps the owner's reason via the existing deadline-family mapping", { skip: LINUX_CAPTURE, timeout: 60_000 }, async () => {
    const reason = new Error("owner cancelled during the settle (3980 test)");
    const r = await abortDuringSettle("abortOwner", reason);
    assert.equal(r.rec.entered, true, "the run reached prepareGateCheckpoint");
    assert.equal(r.entered, true, "the settle was entered");
    assert.equal(r.rec.watchdogFired, false, "the step settled by itself");
    assert.ok(!(r.rec.error instanceof TransientRecoveryError), `not remapped to TransientRecoveryError: ${String(r.rec.error)}`);
    assert.equal(r.rec.error, reason, "the owner's own reason is what the step rejects with");
    assertCustodyIntact(r);
    assert.equal(r.rec.after!.gates, 0, "no gate presented");
  });

  it("(7c) Claude path pin: a shutdown abort during the plan-summary settle keeps the shutdown reason via the existing deadline-family mapping", { skip: LINUX_CAPTURE, timeout: 60_000 }, async () => {
    const reason = new Error("worker shutting down during the settle (3980 test)");
    const r = await abortDuringSettle("abortShutdown", reason);
    assert.equal(r.rec.entered, true, "the run reached prepareGateCheckpoint");
    assert.equal(r.entered, true, "the settle was entered");
    assert.equal(r.rec.watchdogFired, false, "the step settled by itself");
    assert.ok(!(r.rec.error instanceof TransientRecoveryError), `not remapped to TransientRecoveryError: ${String(r.rec.error)}`);
    assert.equal(r.rec.error, reason, "the shutdown reason is what the step rejects with");
    assertCustodyIntact(r);
    assert.equal(r.rec.after!.gates, 0, "no gate presented");
  });

  it("(8) a non-deadline failure from the capture inside the bound is not remapped to TransientRecoveryError", { skip: LINUX_CAPTURE, timeout: 60_000 }, async () => {
    const refusal = new TrustedExecutionRefusal("codex harness is closed");
    const proto = RunRunner.prototype as unknown as { captureRecoveryRestorePoint: (...args: unknown[]) => Promise<unknown> };
    const original = proto.captureRecoveryRestorePoint;
    let calls = 0;
    restorers.push(() => { proto.captureRecoveryRestorePoint = original; });
    const r = await gateCheckpointRun({
      dirty: true,
      hang: async () => {
        proto.captureRecoveryRestorePoint = async function () { calls++; throw refusal; };
        return { release: () => undefined, entered: () => calls > 0 };
      },
    });
    assert.equal(r.entered, true, "the stubbed capture ran inside the bound");
    assert.equal(r.rec.error, refusal, "the refusal passes through unchanged");
    assertCustodyIntact(r);
  });

  for (const [label, makeFailure] of [
    ["a plain git failure", () => new Error("git status failed: fatal: bad object")],
    // The wrapper text of a deadline stop, but with no cause: nothing proves it was this gate's deadline.
    ["the wrapper text of a deadline stop with no cause", () => new Error("git x failed: recovery deadline exhausted")],
  ] as const) {
    it(`(8b) ${label} from the capture passes through unchanged, not remapped to TransientRecoveryError`, { skip: LINUX_CAPTURE, timeout: 60_000 }, async () => {
      const proto = RunRunner.prototype as unknown as { captureRecoveryRestorePoint: (...args: unknown[]) => Promise<unknown> };
      const original = proto.captureRecoveryRestorePoint;
      restorers.push(() => { proto.captureRecoveryRestorePoint = original; });
      const failure = makeFailure();
      let calls = 0;
      const r = await gateCheckpointRun({
        dirty: true,
        hang: async () => {
          proto.captureRecoveryRestorePoint = async function () { calls++; throw failure; };
          return { release: () => undefined, entered: () => calls > 0 };
        },
      });
      assert.equal(r.entered, true, "the stubbed capture ran inside the bound");
      assert.equal(r.rec.error, failure, `${failure.message} passes through by identity`);
      assert.ok(!(r.rec.error instanceof TransientRecoveryError), "not remapped to TransientRecoveryError");
      assertCustodyIntact(r);
    });
  }

  /** Hooks the capture's owed-context object check (`git --no-replace-objects cat-file -t`, GitCache.requireOwedCommit),
   *  a runGit call made outside any bare lock, so the wrapped error is what reaches the step (a call made under
   *  withLock has its failure replaced by the lock's own raw recovery-interruption error). It records what runGit
   *  rejects it with. "spin": waits until just before the gate deadline, then blocks the event loop past it so no
   *  timer fires and the original execScoped throws recoveryRemaining's raw deadline Error. "timeout": throws the
   *  DOMException a fired AbortSignal.timeout raises. "owner": aborts the owner signal, then throws the raw
   *  deadline Error (the deadline stop that raced the owner's Cancel). */
  function hookCaptureGit(mode: "spin" | "timeout" | "owner", rec: GateRec, deadlineMs: number, abort: GateAborts, ownerReason: Error) {
    const internals = git as unknown as {
      execScoped: (cmd: string, args: string[], ...rest: unknown[]) => Promise<unknown>;
      runGit: (...args: unknown[]) => Promise<unknown>;
    };
    const originalExec = internals.execScoped;
    const originalRunGit = internals.runGit;
    const isFetch = (args: unknown): boolean =>
      Array.isArray(args) && args.includes("cat-file") && args.includes("--no-replace-objects");
    let hits = 0;
    const rejections: unknown[] = [];
    internals.execScoped = async function (this: unknown, cmd, args, ...rest) {
      if (!isFetch(args)) return originalExec.call(this, cmd, args, ...rest);
      hits++;
      if (mode === "timeout") throw new DOMException("The operation was aborted due to timeout", "TimeoutError");
      if (mode === "owner") {
        abort.abortOwner(ownerReason);
        throw new Error("recovery deadline exhausted");
      }
      const deadline = rec.startedAt + deadlineMs;
      while (Date.now() < deadline - 30) await new Promise((resolve) => setTimeout(resolve, 5));
      // No await from here to the call: the deadline timers cannot fire, so the raw recoveryRemaining throw is what surfaces.
      while (Date.now() <= deadline + 100) { /* spin past the deadline */ }
      return originalExec.call(this, cmd, args, ...rest);
    } as typeof internals.execScoped;
    internals.runGit = async function (this: unknown, ...args: unknown[]) {
      try { return await originalRunGit.apply(this, args); }
      catch (error) { if (isFetch(args[1])) rejections.push(error); throw error; }
    } as typeof internals.runGit;
    return { rejections, entered: () => hits > 0, release: () => { internals.execScoped = originalExec; internals.runGit = originalRunGit; } };
  }
  const NEVER_OWNER = new Error("unused owner reason");

  it("(9) a recovery deadline that stops the capture's owed-context git check, wrapped by the git wrapper, fails closed as TransientRecoveryError", { skip: LINUX_CAPTURE, timeout: 60_000 }, async () => {
    let hook!: ReturnType<typeof hookCaptureGit>;
    const r = await gateCheckpointRun({
      dirty: true,
      hang: async (_flight, _x, rec, abort) => {
        hook = hookCaptureGit("spin", rec, GATE_MS, abort, NEVER_OWNER);
        return { release: () => hook.release(), entered: hook.entered };
      },
    });
    assert.equal(hook.rejections.length, 1, "runGit rejected the hooked call once");
    const wrapped = hook.rejections[0];
    assert.ok(wrapped instanceof Error);
    assert.match(wrapped.message, /^git --no-replace-objects cat-file -t [0-9a-f]+ failed: recovery deadline exhausted$/, "the raw deadline was wrapped by runGit");
    assertGateFailedClosed(r);
    assertCustodyIntact(r);
  });

  it("(9b) a fired timeout signal at the capture's owed-context git check, wrapped by the git wrapper, fails closed as TransientRecoveryError", { skip: LINUX_CAPTURE, timeout: 60_000 }, async () => {
    let hook!: ReturnType<typeof hookCaptureGit>;
    const r = await gateCheckpointRun({
      dirty: true,
      hang: async (_flight, _x, rec, abort) => {
        hook = hookCaptureGit("timeout", rec, GATE_MS, abort, NEVER_OWNER);
        return { release: () => hook.release(), entered: hook.entered };
      },
    });
    assert.equal(hook.rejections.length, 1, "runGit rejected the hooked call once");
    const wrapped = hook.rejections[0];
    assert.ok(wrapped instanceof Error);
    assert.match(wrapped.message, /^git --no-replace-objects cat-file -t [0-9a-f]+ failed: The operation was aborted due to timeout$/);
    assertGateFailedClosed(r);
    assertCustodyIntact(r);
  });

  it("(9c) an owner abort that races the wrapped deadline at the git check surfaces the owner's reason, not TransientRecoveryError", { skip: LINUX_CAPTURE, timeout: 60_000 }, async () => {
    const reason = new Error("owner cancelled at the capture git check (3980 test)");
    let hook!: ReturnType<typeof hookCaptureGit>;
    const r = await gateCheckpointRun({
      dirty: true,
      hang: async (_flight, _x, rec, abort) => {
        hook = hookCaptureGit("owner", rec, GATE_MS, abort, reason);
        return { release: () => hook.release(), entered: hook.entered };
      },
    });
    assert.equal(r.entered, true, "the git hook was entered");
    assert.equal(hook.rejections.length, 1, "runGit rejected the hooked call once");
    assert.ok(!(r.rec.error instanceof TransientRecoveryError), `not remapped to TransientRecoveryError: ${String(r.rec.error)}`);
    assert.equal(r.rec.error, reason, "the owner's own reason is what the step rejects with");
    assertCustodyIntact(r);
  });

  /** A REAL test-owned process group (`sleep 60`, detached: its pid is its pgid) that never goes. The
   *  runner's own production probe (processGroupAlive) decides its aliveness; the hook only counts probes
   *  and records signals WITHOUT delivering them, so the group survives until `stop()`. */
  function survivorHooks() {
    const child = spawn("sleep", ["60"], { detached: true, stdio: "ignore" });
    child.unref();
    const pgid = child.pid!;
    const seen = { probes: 0, signals: [] as { sig: string; at: number }[] };
    const stop = (): void => { try { process.kill(-pgid, "SIGKILL"); } catch { /* already gone */ } };
    return { seen, pgid, stop, opts: { checkpointTestHooks: { tickSpawn: {
      groupAlive: (g: number) => { seen.probes++; return processGroupAlive(g, "command"); },
      signalGroup: (_pgid: number, sig: string) => { seen.signals.push({ sig, at: Date.now() }); return true; },
    } } } };
  }
  /** After expiry the real group is still tracked, and killConfirmed is what the prelude left it as. */
  function assertSurvivorKept(r: Awaited<ReturnType<typeof gateCheckpointRun>>, pgid: number, killConfirmed: boolean): void {
    assert.deepEqual(r.rec.before!.groups, [{ pgid, identity: "command", killConfirmed: false }], "the group is tracked when the step starts");
    assert.deepEqual(r.rec.after!.groups, [{ pgid, identity: "command", killConfirmed }], "still tracked after expiry, killConfirmed as the prelude left it");
  }

  it("(6a) tick survivors with the sink admitted just before the deadline: the wait is capped, no SIGKILL is sent after expiry and no second wait starts", { skip: LINUX_CAPTURE, timeout: 60_000 }, async () => {
    const { seen, pgid, stop, opts } = survivorHooks();
    try {
      const r = await gateCheckpointRun({
        dirty: true, runnerOpts: opts,
        hang: async (flight) => {
          flight.survivingTickGroups.push({ pgid, identity: "command", killConfirmed: false });
          const gate = flight.sinkGate as unknown as { run: (fn: () => Promise<unknown>) => Promise<unknown> };
          let releaseGate = (): void => {};
          let held = false;
          void gate.run(() => new Promise<void>((resolve) => { held = true; releaseGate = resolve; })).catch(() => undefined);
          assert.ok(await until(() => held), "the injected sink holder is admitted");
          // Freed ~100 ms before the deadline, so the first survivor wait can only start just before expiry.
          const timer = setTimeout(() => releaseGate(), GATE_MS - 100);
          return { release: () => { clearTimeout(timer); stop(); releaseGate(); }, entered: () => seen.probes > 0 };
        },
      });
      assertGateFailedClosed(r);
      assertCustodyIntact(r);
      const deadlineAt = r.rec.startedAt + r.deadlineMs;
      assert.deepEqual(seen.signals.filter((x) => x.at >= deadlineAt - 20), [], "no signal at or after expiry");
      assert.deepEqual(seen.signals, [], "the first wait was capped by the deadline, so there was no SIGKILL to send");
      assertSurvivorKept(r, pgid, false);
    } finally { stop(); }
  });

  it("(6b) expiry during the second survivor wait: one SIGKILL before the deadline, none after, and the step still settles at the deadline", { skip: LINUX_CAPTURE, timeout: 60_000 }, async () => {
    const { seen, pgid, stop, opts } = survivorHooks();
    try {
      const r = await gateCheckpointRun({
        dirty: true, deadlineMs: 2500, tailMs: 700, runnerOpts: opts,
        hang: async (flight) => {
          flight.survivingTickGroups.push({ pgid, identity: "command", killConfirmed: false });
          return { release: () => stop(), entered: () => seen.probes > 0 };
        },
      });
      // The second wait is capped by the deadline: uncapped it would run a further 2 s past it.
      assertGateFailedClosed(r, 700);
      assertCustodyIntact(r);
      const deadlineAt = r.rec.startedAt + r.deadlineMs;
      assert.equal(seen.signals.length, 1, `exactly one SIGKILL: ${JSON.stringify(seen.signals)}`);
      assert.equal(seen.signals[0]!.sig, "SIGKILL");
      assert.ok(seen.signals[0]!.at < deadlineAt, "sent before the deadline");
      assert.ok(seen.signals[0]!.at >= r.rec.startedAt + 1900, "after the first full wait");
      assertSurvivorKept(r, pgid, true); // the recording hook reports the one SIGKILL as delivered
    } finally { stop(); }
  });
});

describe("issue #3980: awaitTickSurvivorsGone with a gate deadline", () => {
  /** A RunRunner whose survivor probe and signal are the test's hooks; no real process is touched. */
  function survivorRunner(hooks: { alive: (pgid: number) => boolean; signal: (pgid: number, sig: string) => boolean }) {
    const warns: string[] = [];
    const runner = Object.create(RunRunner.prototype) as { checkpointTestHooks: unknown;
      awaitTickSurvivorsGone(flight: unknown, deadlineAt?: number): Promise<string> };
    runner.checkpointTestHooks = { tickSpawn: { groupAlive: hooks.alive, signalGroup: hooks.signal } };
    const flight = (pgids: number[]) => ({ runId: "run-x", survivingTickGroups: pgids.map((pgid) => ({ pgid, identity: "command", killConfirmed: false })),
      runLog: { ...nullLogger(), warn: (m: string) => { warns.push(m); } } });
    return { runner, flight, warns };
  }

  it("a deadline that expires between two groups' SIGKILLs sends no later signal and starts no second wait", async (t) => {
    // Mock Date and setTimeout so the 2 s wait and the "slow" signal are exact, not wall-clock dependent.
    t.mock.timers.enable({ apis: ["Date", "setTimeout"] });
    const signals: number[] = [];
    let probesAfterSignal = 0;
    const { runner, flight, warns } = survivorRunner({
      alive: () => { if (signals.length > 0) probesAfterSignal++; return true; },
      // The first signal takes 150 ms of (mock) time, which carries the clock past the deadline.
      signal: (pgid) => { signals.push(pgid); t.mock.timers.tick(150); return true; },
    });
    const started = Date.now();
    // The first full wait is 2 s, so the deadline must outlast it; the first signal then outlasts the deadline.
    let settled = false;
    const pending = runner.awaitTickSurvivorsGone(flight([101, 102]), started + 2000 + 100).finally(() => { settled = true; });
    for (let i = 0; !settled && i < 1000; i++) {
      await new Promise((resolve) => setImmediate(resolve));
      if (!settled) t.mock.timers.tick(25);
    }
    // Mocked time no longer advances past this loop, so fail rather than await a promise that never settles.
    assert.ok(settled, "awaitTickSurvivorsGone settled within the driven mock time");
    const verdict = await pending;
    assert.equal(verdict, "unconfirmed");
    assert.deepEqual(signals, [101], "the second group was not signalled after the deadline passed");
    assert.equal(probesAfterSignal, 0, "no second wait began");
    assert.deepEqual(warns, ["tick survivors unresolved at the gate checkpoint deadline"]);
  });

  it("an expiry during the first wait sends no signal at all", async () => {
    const signals: number[] = [];
    const { runner, flight, warns } = survivorRunner({ alive: () => true, signal: (pgid) => { signals.push(pgid); return true; } });
    const started = Date.now();
    assert.equal(await runner.awaitTickSurvivorsGone(flight([201]), started + 200), "unconfirmed");
    assert.deepEqual(signals, []);
    assert.equal(warns.length, 1);
    assert.ok(Date.now() - started < 1000, "the 2 s wait was capped at the deadline");
  });

  it("a deadline already passed returns at once, unconfirmed, without signalling", async () => {
    const signals: number[] = [];
    const { runner, flight } = survivorRunner({ alive: () => true, signal: (pgid) => { signals.push(pgid); return true; } });
    const started = Date.now();
    assert.equal(await runner.awaitTickSurvivorsGone(flight([301]), started - 1), "unconfirmed");
    assert.deepEqual(signals, []);
    assert.ok(Date.now() - started < 500);
  });

  it("no survivors is gone immediately, with or without a deadline", async () => {
    const { runner, flight } = survivorRunner({ alive: () => false, signal: () => true });
    assert.equal(await runner.awaitTickSurvivorsGone(flight([401]), Date.now() - 1), "gone");
    assert.equal(await runner.awaitTickSurvivorsGone(flight([401])), "gone");
  });

  it("with time left the wait/SIGKILL/wait sequence is the unbounded one: a group that dies after its SIGKILL reads gone", async () => {
    let dead = false;
    const signals: string[] = [];
    const { runner, flight } = survivorRunner({ alive: () => !dead, signal: (_pgid, sig) => { signals.push(sig); dead = true; return true; } });
    assert.equal(await runner.awaitTickSurvivorsGone(flight([501]), Date.now() + 60_000), "gone");
    assert.deepEqual(signals, ["SIGKILL"]);
  });

  it("without a deadline the first wait is the full 2 s and expiry never short-circuits", async () => {
    const signals: string[] = [];
    let dead = false;
    const { runner, flight } = survivorRunner({ alive: () => !dead, signal: (_pgid, sig) => { signals.push(sig); dead = true; return true; } });
    const started = Date.now();
    assert.equal(await runner.awaitTickSurvivorsGone(flight([601])), "gone");
    assert.ok(Date.now() - started >= 1900, "waited the unbounded first wait before signalling");
    assert.deepEqual(signals, ["SIGKILL"]);
  });
});

describe("issue #3980: SdkExecutor.reapAttributedProcesses with a gate deadline", () => {
  function executor() {
    const reaps: unknown[][] = [];
    const sdk = new SdkExecutor(nullLogger(), homeDir, {
      runProcesses: { scan: async () => ({ pids: [], complete: true }),
        reap: async (...args: unknown[]) => { reaps.push(args.slice(3)); return { killed: [], left: [], complete: true }; } },
    });
    const internals = sdk as unknown as { laneDisposal?: Promise<void>; activeDeps?: { abort: AbortController; install: Promise<unknown> }; depsJoined: boolean };
    return { sdk, reaps, internals };
  }
  const never = (): Promise<void> => new Promise<void>(() => undefined);

  it("a lane disposal that never settles reads as an unconfirmed reap at the deadline, and nothing is reaped", async () => {
    const { sdk, reaps, internals } = executor();
    internals.laneDisposal = never();
    const started = Date.now();
    const r = await sdk.reapAttributedProcesses({ deadlineAt: started + 300 });
    assert.deepEqual(r, { killed: [], left: [], complete: false });
    assert.ok(Date.now() - started >= 250 && Date.now() - started < 2000);
    assert.deepEqual(reaps, []);
  });

  it("a deps install that never settles likewise; the abort was requested before the wait, and the install is not cancelled further", async () => {
    const { sdk, reaps, internals } = executor();
    const abort = new AbortController();
    internals.activeDeps = { abort, install: never() };
    internals.depsJoined = false;
    const started = Date.now();
    const r = await sdk.reapAttributedProcesses({ deadlineAt: started + 300 });
    assert.equal(r.complete, false);
    assert.deepEqual(reaps, []);
    assert.equal(internals.depsJoined, false, "the install was not reported joined");
    assert.ok(Date.now() - started < 2000);
  });

  it("past the deadline nothing is admitted: no deps abort, no reap", async () => {
    const { sdk, reaps, internals } = executor();
    const abort = new AbortController();
    internals.activeDeps = { abort, install: Promise.resolve() };
    internals.depsJoined = false;
    const r = await sdk.reapAttributedProcesses({ deadlineAt: Date.now() - 1 });
    assert.equal(r.complete, false);
    assert.equal(abort.signal.aborted, false);
    assert.deepEqual(reaps, []);
  });

  it("with time left the reap runs and carries the deadline; without one it is called exactly as before", async () => {
    const withDeadline = executor();
    const deadlineAt = Date.now() + 60_000;
    assert.equal((await withDeadline.sdk.reapAttributedProcesses({ deadlineAt })).complete, true);
    assert.deepEqual(withDeadline.reaps, [[{ deadlineAt }]]);
    const plain = executor();
    assert.equal((await plain.sdk.reapAttributedProcesses()).complete, true);
    assert.deepEqual(plain.reaps, [[]], "no options argument is passed when none was given");
  });
});

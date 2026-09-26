// Issue #1604: a plan-gate verdict (revise / approve / reject) interrupted between routing and its
// settlement must be neither lost nor applied against the wrong plan. Every case drives the REAL
// SdkExecutor (a scripted query) through the REAL RunRunner against the FakeApi with strict receipt
// generations; an interruption is a second execute() of the SAME run on a fresh runner with the
// next claim generation (the server's reclaim), whose claim carries what the server persisted.
//
// The target contract (the #1604 plan, D1-D6), asserted black-box:
//  - a taken revise stays unapplied until its revised plan is confirmed persisted (the revised
//    awaiting_approval report applied), so an interruption before that replays it;
//  - a taken or buffered reject is applied only with the `failed` transition (fail_origin
//    plan_rejected), so an interruption before that replays it;
//  - a resumed, unapproved claim with a persisted plan reads the inputs sent before the release
//    BEFORE offering any plan, and a replayed revise revises the submitted plan instead of
//    re-presenting it (with no session, the revision prompt carries the prior plan);
//  - stale and superseded verdicts are applied with a feed notice.
import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { SDKMessage } from "@anthropic-ai/claude-agent-sdk";
import { SdkExecutor, type SdkQueryFn } from "../src/sdk-executor.js";
import type { RunRunner } from "../src/runner.js";
import type { AgentTemplate, ClaimResponse, StateRequest, UserInput } from "../src/protocol.js";
import { nullLogger } from "./helpers.js";
import { api, fakeGitlab, gitlabClaim, installHarness, runnerWith, simulateCommittedWork } from "./runner-harness.js";

installHarness();

// --- fixtures ----------------------------------------------------------------------------------

/** The server-assembled claim fields for an unapproved agent plan whose session_id is NULL
 *  (api/internal/workersvc asserts the live-DB claim assembly produces exactly these). */
const FIXTURE = JSON.parse(
  fs.readFileSync(new URL("../../fixtures/claims/resume-unapproved-no-session.json", import.meta.url), "utf8"),
) as { plan_md: string; plan_source: NonNullable<ClaimResponse["plan_source"]>; plan_approved: boolean; session_id: null };

const PLAN_V1 = FIXTURE.plan_md;
const V1_MILESTONES = [{ id: "m1", title: "first cut" }];
const SID = "11111111-2222-4333-8444-555555555555";
const FEEDBACK = "split the migration into its own milestone";
const REVISE_MARK = "The plan reviewer read your proposed plan";
const PLAN_MARK = "Produce a concrete implementation plan";
const AGENTS: AgentTemplate[] = [
  { name: "reviewer", description: "reviews the change", prompt_body: "Review the change." },
  { name: "coder", description: "writes the change", prompt_body: "Write the change." },
];
const SELECTION = JSON.stringify({ source: "own", exclusions: ["reviewer"] });
const SELECTION_STATUS = "implementing with your agent templates (coder)";
const DEFAULT_SELECTION_STATUS = "implementing with your agent templates (reviewer, coder)";

const STATUS_RESUME_WITH_REVISION =
  "resuming at the plan gate with a revision the owner sent before the claim was released — revising the submitted plan instead of re-presenting it";
const STATUS_WAITING_DELIVERY =
  "waiting to read plan-gate inputs sent before the claim was released — the plan is not offered for approval until they are read";
const STALE_APPROVE_NOTICE = "Approval ignored — the plan changed; re-send if you still want it.";
const STALE_REJECT_NOTICE = "Rejection ignored — the plan changed; re-send if you still want it.";
const SUPERSEDED_REJECT_NOTICE = "an earlier plan rejection was superseded by a newer verdict";

const revisedPlan = (n: number): string => `# REVISED PLAN r${n}\n- the owner's feedback, applied`;
const revisedMilestones = (n: number) => [{ id: `rm${n}`, title: `revised milestone ${n}` }];

// --- the scripted model ------------------------------------------------------------------------

type TurnKind = "plan" | "revise" | "implement";
interface Turn { kind: TurnKind; prompt: string; resume: string | undefined }

function assistantMsg(content: unknown[]): SDKMessage {
  return { type: "assistant", session_id: SID, message: { content } } as unknown as SDKMessage;
}
function resultMsg(): SDKMessage {
  return { type: "result", subtype: "success", is_error: false, num_turns: 1, session_id: SID } as unknown as SDKMessage;
}

interface Block { entered: Promise<void>; release: () => void }

/** A query fn that classifies each turn by its prompt: a revision turn (the revise prompt), a
 *  planning turn (the plan prompt), else an implement turn. Plan and revision turns can be held
 *  mid-turn (a held turn ends with no output when its turn is aborted), and a revision turn can ask
 *  the owner a question first (the answer turn then submits the revised plan). */
class Model {
  readonly turns: Turn[] = [];
  revisions = 0;
  private readonly blocks: Record<"plan" | "revise", Array<{ entered: () => void; released: Promise<void> }>> = {
    plan: [],
    revise: [],
  };
  /** The next revision turn calls ask_user first; its answer turn submits the revised plan. */
  askInNextRevise = false;
  private answerTurnPending = false;

  block(kind: "plan" | "revise"): Block {
    let entered!: () => void;
    let release!: () => void;
    const e = new Promise<void>((r) => (entered = r));
    const released = new Promise<void>((r) => (release = r));
    this.blocks[kind].push({ entered, released });
    return { entered: e, release };
  }

  count(kind: TurnKind, from = 0): number {
    return this.turns.slice(from).filter((t) => t.kind === kind).length;
  }

  queryFn(): SdkQueryFn {
    return (params) => this.turn(params);
  }

  private async *turn(params: Parameters<SdkQueryFn>[0]): AsyncGenerator<SDKMessage> {
    let prompt = "";
    for await (const p of params.prompt) prompt += JSON.stringify(p);
    let kind: TurnKind = prompt.includes(REVISE_MARK) ? "revise" : prompt.includes(PLAN_MARK) ? "plan" : "implement";
    if (this.answerTurnPending) kind = "revise";
    this.turns.push({ kind, prompt, resume: (params.options as { resume?: string }).resume });
    const signal = params.options.abortController!.signal;
    if (kind === "plan" || kind === "revise") {
      const block = this.answerTurnPending ? undefined : this.blocks[kind].shift();
      if (block) {
        block.entered();
        const aborted = await Promise.race([
          block.released.then(() => false),
          new Promise<boolean>((r) => {
            if (signal.aborted) return r(true);
            signal.addEventListener("abort", () => r(true), { once: true });
          }),
        ]);
        if (aborted) return;
      }
    }
    if (kind === "revise" && this.askInNextRevise && !this.answerTurnPending) {
      this.askInNextRevise = false;
      this.answerTurnPending = true;
      yield assistantMsg([
        { type: "tool_use", id: "q", name: "mcp__uzi__ask_user", input: { questions: [{ question: "Keep the old API?" }] } },
      ]);
      yield resultMsg();
      return;
    }
    if (kind === "revise") {
      this.answerTurnPending = false;
      const n = ++this.revisions;
      yield assistantMsg([
        { type: "tool_use", id: `r${n}`, name: "mcp__uzi__submit_plan", input: { plan_md: revisedPlan(n), milestones: revisedMilestones(n) } },
      ]);
      yield resultMsg();
      return;
    }
    if (kind === "plan") {
      yield assistantMsg([
        { type: "tool_use", id: "p", name: "mcp__uzi__submit_plan", input: { plan_md: PLAN_V1, milestones: V1_MILESTONES } },
      ]);
      yield resultMsg();
      return;
    }
    yield assistantMsg([
      { type: "text", text: "done implementing" },
      { type: "tool_use", id: "d", name: "mcp__uzi__signal_done", input: {} },
    ]);
    yield resultMsg();
  }
}

// --- the scenario harness ------------------------------------------------------------------------

const tick = (ms = 5): Promise<void> => new Promise((r) => setTimeout(r, ms));

/** Poll `pred` until true or `ms` elapses; returns whether it became true (never throws). */
async function until(pred: () => boolean, ms = 8_000): Promise<boolean> {
  const end = Date.now() + ms;
  while (!pred()) {
    if (Date.now() > end) return false;
    await tick();
  }
  return true;
}

interface Flight {
  claim: ClaimResponse;
  runner: RunRunner;
  done: Promise<void>;
  finished: boolean;
  stateFrom: number;
  seqFrom: number;
  turnFrom: number;
  receiptFrom: number;
  timelineFrom: number;
}

let nextIid = 16040;

class Scenario {
  readonly model = new Model();
  readonly root = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1604-"));
  readonly home = path.join(this.root, "home");
  readonly base: ClaimResponse;
  private generation = 0;
  private nextId = 100;

  constructor(overrides: Partial<ClaimResponse> = {}) {
    simulateCommittedWork();
    api.strictReceiptGenerations = true;
    this.base = gitlabClaim(nextIid++, { agents: AGENTS, ...overrides });
  }

  get runId(): string {
    return this.base.run_id;
  }

  /** The next claim of the run: the next generation, messages continuing after the last seq. */
  claim(overrides: Partial<ClaimResponse> = {}): ClaimResponse {
    this.generation++;
    api.setInputClaimGeneration(this.runId, this.generation);
    const lastSeq = Math.max(0, ...api.messages(this.runId).map((m) => m.seq));
    return { ...this.base, claim_generation: this.generation, last_seq: lastSeq, ...overrides };
  }

  /** The claim the server assembles for a resumed, unapproved run at the gate: the last persisted
   *  plan (and its candidate milestones), the session kept unless `session` says otherwise. */
  resumeClaim(session: "kept" | "lost" | "none", overrides: Partial<ClaimResponse> = {}): ClaimResponse {
    const gate = this.persistedGate();
    assert.ok(gate, "a plan was persisted before the interruption");
    const common: Partial<ClaimResponse> = { plan_md: gate.plan_md, plan_source: "agent", plan_approved: false, milestones: gate.milestones ?? null };
    if (session === "none") {
      // The fixture shape: session_id NULL and no resume_phase (resumePhaseFor returns "").
      return this.claim({ ...common, plan_source: FIXTURE.plan_source, plan_approved: FIXTURE.plan_approved, session_id: FIXTURE.session_id, ...overrides });
    }
    if (session === "kept") this.writeTranscript();
    else fs.rmSync(path.join(this.home, ".claude", "projects"), { recursive: true, force: true });
    return this.claim({ ...common, resume_phase: "awaiting_approval", session_id: SID, ...overrides });
  }

  /** A resumed claim at the gate on the submitted plan (session kept), for a run whose earlier
   *  claim was itself a resume (so no gate report of its own was recorded). */
  resumeClaimAtV1(): ClaimResponse {
    this.writeTranscript();
    return this.claim({ resume_phase: "awaiting_approval", plan_md: PLAN_V1, milestones: V1_MILESTONES, plan_source: "agent", plan_approved: false, session_id: SID });
  }

  writeTranscript(): void {
    const dir = path.join(this.home, ".claude", "projects", "-clone");
    fs.mkdirSync(dir, { recursive: true });
    fs.writeFileSync(path.join(dir, `${SID}.jsonl`), "{}\n");
  }

  /** The plan the server holds: the last applied awaiting_approval report. */
  persistedGate(): StateRequest | undefined {
    return api.states.filter((s) => s.runId === this.runId && s.body.status === "awaiting_approval").at(-1)?.body;
  }

  input(kind: UserInput["kind"], body: string | null = null): UserInput {
    return { id: this.nextId++, kind, body };
  }

  send(...rows: UserInput[]): UserInput[] {
    api.appendInputs(this.runId, rows);
    return rows;
  }

  answer(questionId: string, answers = ["yes"]): UserInput {
    return this.input("answer", JSON.stringify({ question_id: questionId, answers }));
  }

  start(claim: ClaimResponse): Flight {
    const { gitlab } = fakeGitlab();
    const runner = runnerWith(
      () => ({ executor: new SdkExecutor(nullLogger(), this.home, { queryFn: this.model.queryFn() }), homeDir: this.home }),
      gitlab,
      undefined,
      nullLogger(),
      { recoveryRetryMs: 1 },
    );
    const flight: Flight = {
      claim,
      runner,
      done: Promise.resolve(),
      finished: false,
      stateFrom: api.states.length,
      seqFrom: claim.last_seq,
      turnFrom: this.model.turns.length,
      receiptFrom: api.inputReceiptCalls.length,
      timelineFrom: api.timeline.length,
    };
    flight.done = runner.execute(claim).then(
      () => {
        flight.finished = true;
      },
      () => {
        flight.finished = true;
      },
    );
    this.flights.push(flight);
    return flight;
  }

  /** Let a flight end, bounded: a flight that is still parked after `ms` is cancelled, then shut
   *  down, so a case the base code fails ends instead of hanging the file. */
  async finish(flight: Flight, ms = 8_000): Promise<void> {
    if (await until(() => flight.finished, ms)) return;
    this.send(this.input("cancel"));
    if (await until(() => flight.finished, 3_000)) return;
    flight.runner.shutdown();
    await Promise.race([flight.done, tick(10_000)]);
  }

  /** Shut a flight down (the worker's graceful stop) and wait for it to unwind. */
  async shutdown(flight: Flight): Promise<void> {
    flight.runner.shutdown();
    if (!(await until(() => flight.finished, 10_000))) this.send(this.input("cancel"));
    await Promise.race([flight.done, tick(5_000)]);
  }

  states(flight?: Flight): StateRequest[] {
    return api.states.slice(flight?.stateFrom ?? 0).filter((s) => s.runId === this.runId).map((s) => s.body);
  }

  statuses(flight?: Flight): string[] {
    return this.states(flight).map((s) => s.status);
  }

  gates(flight?: Flight): StateRequest[] {
    return this.states(flight).filter((s) => s.status === "awaiting_approval");
  }

  texts(flight?: Flight): string[] {
    return api
      .messages(this.runId)
      .filter((m) => m.seq > (flight?.seqFrom ?? 0) && m.kind === "status")
      .map((m) => String((m.payload as { text?: unknown }).text ?? ""));
  }

  kinds(flight?: Flight): string[] {
    return api.messages(this.runId).filter((m) => m.seq > (flight?.seqFrom ?? 0)).map((m) => m.kind);
  }

  turns(flight?: Flight): Turn[] {
    return this.model.turns.slice(flight?.turnFrom ?? 0);
  }

  /** Timeline index of the first 200 APPLIED reply covering `id` (after `from`), or -1. */
  appliedAt(id: number, from = 0): number {
    return api.timeline.findIndex(
      (e, i) => i >= from && e.type === "receipt_reply" && e.runId === this.runId && e.kind === "applied" && e.httpStatus === 200 && e.ids.includes(id),
    );
  }

  /** Timeline index of the first recorded awaiting_approval carrying `plan`, or -1. */
  gateAt(plan: string, from = 0): number {
    return api.timeline.findIndex((e, i) => i >= from && e.type === "state" && e.runId === this.runId && e.status === "awaiting_approval" && e.plan_md === plan);
  }

  /** Timeline index of the first recorded state `status` (after `from`), or -1. */
  stateAt(status: string, from = 0): number {
    return api.timeline.findIndex((e, i) => i >= from && e.type === "state" && e.runId === this.runId && e.status === status);
  }

  acks(id: number, flight?: Flight): number {
    return api.inputReceiptCalls
      .slice(flight?.receiptFrom ?? 0)
      .filter((c) => c.runId === this.runId && c.kind === "ack" && c.ids.includes(id)).length;
  }

  rowsOf(kind: UserInput["kind"]): UserInput[] {
    return api.inputRows(this.runId).filter((r) => r.kind === kind);
  }

  /** Reach the first gate (plan v1 persisted) on a fresh first claim. */
  async toFirstGate(overrides: Partial<ClaimResponse> = {}): Promise<Flight> {
    const flight = this.start(this.claim(overrides));
    assert.ok(await until(() => this.gates(flight).length >= 1 || flight.finished), "the first claim reached the plan gate");
    assert.equal(this.gates(flight)[0]?.plan_md, PLAN_V1, "the gate presented the submitted plan");
    return flight;
  }

  readonly flights: Flight[] = [];

  /** End every flight still running (a case the base code fails can throw with one parked): clear
   *  the injected faults, release the claim under it and nudge a receipt so the fenced flight ends
   *  quietly, then shut it down. Bounded; never throws. */
  async teardown(): Promise<void> {
    const live = this.flights.filter((f) => !f.finished);
    if (live.length > 0) {
      api.failInputGets(this.runId, 0);
      api.rawInputGets(this.runId, undefined, 0);
      api.delayInputGets(this.runId, 0, 0);
      api.delayInputReceipts("ack", 0);
      api.delayInputReceipts("applied", 0);
      api.failInputReceipts("ack", undefined);
      api.failInputReceipts("applied", undefined);
      api.failInputReceiptsTimes("ack", 0);
      api.failInputReceiptsTimes("applied", 0);
      api.dropStatesWhen(this.runId, () => false)();
      api.clearCredentialSwitch(this.runId);
      api.setInputClaimGeneration(this.runId, 1_000_000);
      api.setInputFenceReason(this.runId, "released");
      this.send(this.input("follow_up", "teardown nudge"));
      await until(() => live.every((f) => f.finished), 3_000);
      for (const f of live) if (!f.finished) f.runner.shutdown();
      await Promise.race([Promise.all(live.map((f) => f.done)), tick(5_000)]);
      for (const f of live) if (!f.finished) console.error(`#1604 teardown: a flight is still running, gen=${f.claim.claim_generation} statuses=${this.statuses(f).join(",")}`);
    }
    fs.rmSync(this.root, { recursive: true, force: true });
  }
}

async function scenario(fn: (s: Scenario) => Promise<void>, overrides: Partial<ClaimResponse> = {}): Promise<void> {
  const s = new Scenario(overrides);
  try {
    await fn(s);
  } finally {
    await s.teardown();
  }
}

/** Assert a resumed claim revised the submitted plan with `feedback` instead of re-presenting it. */
function assertRevisedOnResume(s: Scenario, flight: Flight, feedback: string, session: "kept" | "lost" | "none"): void {
  const gates = s.gates(flight);
  assert.ok(gates.length >= 1, `the resumed claim reached the gate: ${s.statuses(flight).join(",")}`);
  assert.ok(
    !gates.some((g) => g.plan_md === PLAN_V1),
    "no awaiting_approval after the resume carries the superseded plan",
  );
  const revise = s.turns(flight).filter((t) => t.kind === "revise");
  assert.equal(revise.length, 1, "exactly one revision turn ran on the resumed claim");
  assert.ok(revise[0]!.prompt.includes(feedback), "the revision turn carries the owner's feedback (recovered, not lost)");
  assert.equal(s.turns(flight).filter((t) => t.kind === "plan").length, 0, "no fresh planning turn re-planned the run");
  if (session === "kept") assert.equal(revise[0]!.resume, SID, "with the session kept the revision turn resumes it");
  else {
    assert.equal(revise[0]!.resume, undefined, "with no session the revision turn starts fresh");
    assert.ok(revise[0]!.prompt.includes("step one: the plan the owner asked to revise"), "a session-less revision prompt carries the prior plan text");
    assert.ok(revise[0]!.prompt.includes(PLAN_MARK), "a session-less revision prompt carries the full planning prompt");
  }
  assert.match(gates[0]!.plan_md ?? "", /# REVISED PLAN r\d/, "the resumed gate offers the revised plan");
  assert.ok(gates[0]!.milestones?.[0]?.id.startsWith("rm"), "the revised candidate milestones ride the gate");
  assert.ok(s.texts(flight).includes(STATUS_RESUME_WITH_REVISION), "the resume says it is revising instead of re-presenting");
  assert.ok(s.kinds(flight).includes("plan_feedback"), "the feedback is recorded on the feed");
  assert.ok(s.kinds(flight).includes("plan_revising"), "the revision round is announced on the feed");
}

// --- tests ---------------------------------------------------------------------------------------

describe("#1604 — a revise interrupted mid-revision turn is replayed and revises the submitted plan (c)", () => {
  for (const session of ["kept", "lost", "none"] as const) {
    const label = { kept: "resume_continued (session kept)", lost: "worker-side resume_lineage_break", none: "the no-session fixture claim" }[session];
    it(`worker shutdown mid-revision, reclaimed with ${label}`, () =>
      scenario(async (s) => {
        const first = await s.toFirstGate();
        const block = s.model.block("revise");
        const [revise] = s.send(s.input("revise_plan", FEEDBACK));
        await block.entered;
        await tick(60); // several poll ticks: on base the APPLIED goes out here
        await s.shutdown(first);
        assert.ok(!s.statuses(first).includes("failed"), "a shutdown never fails the run");
        assert.equal(api.isApplied(s.runId, revise!.id), false, "the revise stays unapplied while its revised plan is unpersisted");

        const resumed = s.start(s.resumeClaim(session));
        assert.ok(await until(() => s.gates(resumed).length >= 1 || resumed.finished), "the resumed claim reached a gate");
        const gateAt = s.gateAt(s.gates(resumed)[0]?.plan_md ?? "\u0000", resumed.timelineFrom);
        s.send(s.input("approve_plan"));
        await s.finish(resumed);

        if (session === "lost") assert.ok(s.texts(resumed).some((t) => t.includes("its earlier session could not be found")), "lineage break observed");
        assertRevisedOnResume(s, resumed, FEEDBACK, session);
        assert.equal(s.rowsOf("revise_plan").length, 1, "the replay created no new revise row");
        assert.ok(s.acks(revise!.id, resumed) >= 1, "the resumed claim read the same revise row again");
        const appliedAt = s.appliedAt(revise!.id, resumed.timelineFrom);
        assert.ok(appliedAt > gateAt && gateAt >= 0, "the replayed revise is applied only after its revised plan was persisted");
        assert.ok(s.statuses(resumed).includes("completed"), `the run completed after approving the revised plan: ${s.statuses(resumed).join(",")}`);
        assert.equal(s.model.count("implement", resumed.turnFrom), 1, "one implement turn, on the revised plan");
      }));
  }
});

/** Start a resumed claim, approve the first plan it offers, and let it end (bounded). Returns the
 *  flight and the timeline index of its first gate. */
async function resumeAndApprove(s: Scenario, claim: ClaimResponse, selection: string | null = null): Promise<{ flight: Flight; gateAt: number }> {
  const flight = s.start(claim);
  await until(() => s.gates(flight).length >= 1 || flight.finished);
  const plan = s.gates(flight)[0]?.plan_md;
  const gateAt = plan === undefined ? -1 : s.gateAt(plan, flight.timelineFrom);
  if (!flight.finished) s.send(s.input("approve_plan", selection));
  await s.finish(flight);
  return { flight, gateAt };
}

/** Park the first claim at its gate, then let a credential switch release it while `row` is sent. */
async function releaseAtGate(s: Scenario, send: () => UserInput, when: "unacked" | "acked"): Promise<{ first: Flight; row: UserInput }> {
  const first = await s.toFirstGate();
  if (when === "unacked") api.requestCredentialSwitch(s.runId, 1);
  else api.pendSwitchAfterNextAck(s.runId, 1);
  const row = send();
  await s.finish(first);
  return { first, row };
}

describe("#1604 — (a) a verdict still unACKed when a switch releases the claim is replayed on the reclaim", () => {
  it("revise: the reclaim revises the submitted plan instead of re-presenting it", () =>
    scenario(async (s) => {
      const { first, row } = await releaseAtGate(s, () => s.send(s.input("revise_plan", FEEDBACK))[0]!, "unacked");
      assert.ok(s.statuses(first).includes("credential_switch"), `the switch released the claim: ${s.statuses(first).join(",")}`);
      assert.equal(api.isAcked(s.runId, row.id), false, "the released claim never ACKed the revise");
      const { flight, gateAt } = await resumeAndApprove(s, s.resumeClaim("kept"));
      assertRevisedOnResume(s, flight, FEEDBACK, "kept");
      assert.ok(s.appliedAt(row.id, flight.timelineFrom) > gateAt, "applied only after the revised plan was persisted");
      assert.ok(s.statuses(flight).includes("completed"), s.statuses(flight).join(","));
    }));

  it("approve: the reclaim offers the submitted plan and the replayed approve (and its selection) applies", () =>
    scenario(async (s) => {
      const { first, row } = await releaseAtGate(s, () => s.send(s.input("approve_plan", SELECTION))[0]!, "unacked");
      assert.ok(s.statuses(first).includes("credential_switch"), s.statuses(first).join(","));
      const flight = s.start(s.resumeClaim("kept"));
      await s.finish(flight);
      assert.equal(s.gates(flight)[0]?.plan_md, PLAN_V1, "the submitted plan is offered");
      assert.ok(s.statuses(flight).includes("completed"), s.statuses(flight).join(","));
      assert.ok(s.texts(flight).includes(SELECTION_STATUS), `the approve's agent selection survived: ${s.texts(flight).join(" | ")}`);
      assert.equal(s.model.count("implement"), 1, "one implement turn");
      assert.equal(s.model.count("revise"), 0, "no revision");
      assert.ok(api.isApplied(s.runId, row.id), "the replayed approve is applied");
    }));

  it("reject: the reclaim reads the reject before offering the plan and fails, settling it with the transition", () =>
    scenario(async (s) => {
      const { first, row } = await releaseAtGate(s, () => s.send(s.input("reject_plan", "wrong approach"))[0]!, "unacked");
      assert.ok(s.statuses(first).includes("credential_switch"), s.statuses(first).join(","));
      const flight = s.start(s.resumeClaim("kept"));
      await s.finish(flight);
      const failed = s.states(flight).find((b) => b.status === "failed");
      assert.equal(failed?.failure_reason, "wrong approach", `the run failed with the reject reason: ${s.statuses(flight).join(",")}`);
      assert.equal(s.model.count("implement"), 0, "nothing was implemented");
      assert.equal(s.gates(flight).length, 0, "the plan is never offered once the reject is read");
      assert.ok(api.isApplied(s.runId, row.id), "the reject is applied with the plan_rejected failed transition");
    }));

  for (const verdict of ["approve_plan", "reject_plan"] as const) {
    it(`D5: a ${verdict} applied at a resumed gate gets one status line naming the verdict`, () =>
      scenario(async (s) => {
        await releaseAtGate(s, () => s.send(s.input(verdict, verdict === "approve_plan" ? SELECTION : "wrong approach"))[0]!, "unacked");
        const flight = s.start(s.resumeClaim("kept"));
        await s.finish(flight);
        const word = verdict === "approve_plan" ? /approv/i : /reject/i;
        const lines = s.texts(flight).filter((t) => word.test(t) && t !== STALE_APPROVE_NOTICE && t !== STALE_REJECT_NOTICE);
        assert.equal(lines.length, 1, `one status line names the verdict: ${JSON.stringify(s.texts(flight))}`);
      }));
  }
});

describe("#1604 — (b) a verdict ACKed and routed whose APPLIED is refused by a pending switch", () => {
  it("revise: the revised plan is persisted and the revise applied under switch_pending before the release; the reclaim shows it with no second revision", () =>
    scenario(async (s) => {
      const { first, row } = await releaseAtGate(s, () => s.send(s.input("revise_plan", FEEDBACK))[0]!, "acked");
      const v2At = s.gateAt(revisedPlan(1));
      const releasedAt = s.stateAt("credential_switch");
      assert.ok(v2At >= 0 && releasedAt > v2At, `the revised plan was persisted before the release: ${s.statuses(first).join(",")}`);
      const appliedAt = s.appliedAt(row.id);
      assert.ok(appliedAt > v2At && appliedAt < releasedAt, "the revise was applied after persistence, under switch_pending, before the release");
      const { flight } = await resumeAndApprove(s, s.resumeClaim("kept"));
      assert.equal(s.gates(flight)[0]?.plan_md, revisedPlan(1), "the reclaim offers the revised plan");
      assert.equal(s.model.count("revise", flight.turnFrom), 0, "no second revision");
      assert.ok(s.statuses(flight).includes("completed"), s.statuses(flight).join(","));
    }));

  it("approve: the refused APPLIED leaves it to replay; the reclaim applies it with its selection", () =>
    scenario(async (s) => {
      const { first, row } = await releaseAtGate(s, () => s.send(s.input("approve_plan", SELECTION))[0]!, "acked");
      assert.ok(!s.statuses(first).includes("failed") && !s.statuses(first).includes("completed"), s.statuses(first).join(","));
      assert.equal(api.isApplied(s.runId, row.id), false, "the refused approve stays unapplied");
      const flight = s.start(s.resumeClaim("kept"));
      await s.finish(flight);
      assert.ok(s.statuses(flight).includes("completed"), s.statuses(flight).join(","));
      assert.ok(s.texts(flight).includes(SELECTION_STATUS), s.texts(flight).join(" | "));
      assert.equal(s.model.count("implement"), 1, "exactly one implement turn across both claims");
    }));

  it("reject: the run fails and the reject is settled with the failed transition", () =>
    scenario(async (s) => {
      const { first, row } = await releaseAtGate(s, () => s.send(s.input("reject_plan", "wrong approach"))[0]!, "acked");
      const failed = s.states(first).find((b) => b.status === "failed");
      assert.equal(failed?.failure_reason, "wrong approach", s.statuses(first).join(","));
      assert.equal(s.model.count("implement"), 0, "nothing was implemented");
      assert.ok(api.isApplied(s.runId, row.id), "the reject is applied with the plan_rejected failed transition");
    }));
});

describe("#1604 — an approve interrupted before its running transition keeps its selection", () => {
  it("after routing, before APPLIED: the reclaim replays it", () =>
    scenario(async (s) => {
      const first = await s.toFirstGate();
      api.failInputReceipts("applied", 503);
      const [row] = s.send(s.input("approve_plan", SELECTION));
      assert.ok(await until(() => api.inputReceiptCalls.some((c) => c.kind === "applied" && c.ids.includes(row!.id))));
      await s.shutdown(first);
      api.failInputReceipts("applied", undefined);
      assert.ok(!s.statuses(first).includes("failed"), s.statuses(first).join(","));
      assert.equal(api.isApplied(s.runId, row!.id), false, "the approve stays unapplied");
      const flight = s.start(s.resumeClaim("kept"));
      await s.finish(flight);
      assert.ok(s.statuses(flight).includes("completed"), s.statuses(flight).join(","));
      assert.ok(s.texts(flight).includes(SELECTION_STATUS), s.texts(flight).join(" | "));
      assert.equal(s.model.count("implement"), 1);
      assert.equal(s.rowsOf("approve_plan").length, 1, "no new row");
    }));

  it("after APPLIED, before the running transition: the approved claim implements with the persisted selection, no gate", () =>
    scenario(async (s) => {
      const first = await s.toFirstGate();
      const stopDropping = api.dropStatesWhen(s.runId, () => true); // the worker dies: nothing it reports lands
      const [row] = s.send(s.input("approve_plan", SELECTION));
      assert.ok(await until(() => api.isApplied(s.runId, row!.id)), "the approve was applied");
      await s.shutdown(first);
      stopDropping();
      assert.equal(s.statuses(first).at(-1), "awaiting_approval", "no report after the gate landed");
      const flight = s.start(
        s.resumeClaim("kept", { plan_approved: true, resume_phase: "implementing", agent_selection: { source: "own", exclusions: ["reviewer"] } }),
      );
      await s.finish(flight);
      assert.equal(s.gates(flight).length, 0, "an approved resume never re-gates");
      assert.ok(s.texts(flight).includes(SELECTION_STATUS), s.texts(flight).join(" | "));
      assert.ok(!s.texts(flight).includes(DEFAULT_SELECTION_STATUS));
      assert.ok(s.statuses(flight).includes("completed"), s.statuses(flight).join(","));
    }));
});

describe("#1604 — a reject interrupted before its failed report is replayed and fails the run", () => {
  it("buffered before the gate waiter opens", () =>
    scenario(async (s) => {
      const block = s.model.block("plan");
      const first = s.start(s.claim());
      await block.entered;
      const stopDropping = api.dropStatesWhen(s.runId, (b) => b.status === "failed");
      const [row] = s.send(s.input("reject_plan", "wrong approach"));
      assert.ok(await until(() => api.isAcked(s.runId, row!.id)));
      await tick(60);
      block.release();
      await s.finish(first);
      stopDropping();
      assert.equal(s.gates(first).length, 1, "the plan was persisted before the buffered reject was taken");
      assert.ok(!s.statuses(first).includes("failed"), "the failed report never landed");
      assert.equal(api.isApplied(s.runId, row!.id), false, "the reject stays unapplied without its failed transition");
      const flight = s.start(s.resumeClaim("kept"));
      await s.finish(flight);
      assert.equal(s.states(flight).find((b) => b.status === "failed")?.failure_reason, "wrong approach", s.statuses(flight).join(","));
      assert.equal(s.gates(flight).length, 0, "the replayed reject is read before the plan is offered");
      assert.equal(s.model.count("implement"), 0, "never an implement turn");
      assert.ok(api.isApplied(s.runId, row!.id), "settled with the failed transition");
    }));

  it("taken at the gate", () =>
    scenario(async (s) => {
      const first = await s.toFirstGate();
      const stopDropping = api.dropStatesWhen(s.runId, (b) => b.status === "failed");
      const [row] = s.send(s.input("reject_plan", "wrong approach"));
      await s.finish(first);
      stopDropping();
      assert.ok(!s.statuses(first).includes("failed"), "the failed report never landed");
      assert.equal(api.isApplied(s.runId, row!.id), false, "the reject stays unapplied without its failed transition");
      const flight = s.start(s.resumeClaim("kept"));
      await s.finish(flight);
      assert.equal(s.states(flight).find((b) => b.status === "failed")?.failure_reason, "wrong approach", s.statuses(flight).join(","));
      assert.equal(s.model.count("implement"), 0, "never an implement turn");
      assert.ok(api.isApplied(s.runId, row!.id), "settled with the failed transition");
    }));
});

describe("#1604 — an already-applied reject with no replayable input (older-worker residual)", () => {
  it("the gate re-presents the submitted plan and waits; nothing is implemented", () =>
    scenario(async (s) => {
      s.writeTranscript();
      const flight = s.start(
        s.claim({ resume_phase: "awaiting_approval", plan_md: PLAN_V1, plan_source: "agent", plan_approved: false, session_id: SID }),
      );
      assert.ok(await until(() => s.gates(flight).length >= 1 || flight.finished), s.statuses(flight).join(","));
      await tick(300);
      assert.equal(s.gates(flight)[0]?.plan_md, PLAN_V1, "the submitted plan is re-presented");
      assert.equal(flight.finished, false, "the gate keeps waiting for a verdict");
      assert.equal(s.model.turns.length, 0, "no turn ran: nothing planned, revised or implemented");
      s.send(s.input("cancel"));
      await s.finish(flight);
    }));
});

describe("#1604 — (c) a switch requested during the revision turn", () => {
  for (const session of ["kept", "none"] as const) {
    it(`deferral holds, the revised plan and the revise settle before the release; the reclaim (${session === "kept" ? "session kept" : "no session"}) shows the revised plan with no second revision`, () =>
      scenario(async (s) => {
        const first = await s.toFirstGate();
        const block = s.model.block("revise");
        const [row] = s.send(s.input("revise_plan", FEEDBACK));
        await block.entered;
        api.requestCredentialSwitch(s.runId, 1);
        await tick(100);
        assert.ok(!s.statuses(first).includes("credential_switch"), "the switch is deferred while the revision turn runs");
        assert.equal(first.finished, false, "the revision turn is not interrupted");
        block.release();
        await s.finish(first);
        const v2At = s.gateAt(revisedPlan(1));
        const releasedAt = s.stateAt("credential_switch");
        assert.ok(v2At >= 0 && releasedAt > v2At, `revised plan persisted before the release: ${s.statuses(first).join(",")}`);
        const appliedAt = s.appliedAt(row!.id);
        assert.ok(appliedAt > v2At && appliedAt < releasedAt, "the revise applied under switch_pending, after persistence, before the release");
        const { flight } = await resumeAndApprove(s, s.resumeClaim(session));
        assert.equal(api.isApplied(s.runId, row!.id), true, "nothing is left to replay");
        assert.equal(s.model.count("revise", flight.turnFrom), 0, "no second revision");
        if (session === "kept") {
          assert.equal(s.gates(flight)[0]?.plan_md, revisedPlan(1), "the resumed gate shows the revised plan");
          assert.equal(s.model.count("plan", flight.turnFrom), 0, "no re-plan");
        } else {
          // The #1604 plan (D3): a "" resume with no session and nothing pending plans from
          // scratch, as before; the persisted revised plan is not re-presented.
          assert.equal(s.model.count("plan", flight.turnFrom), 1, "the no-session resume plans from scratch");
          assert.equal(s.gates(flight)[0]?.plan_md, PLAN_V1, "the fresh plan is gated");
        }
        assert.ok(s.statuses(flight).includes("completed"), s.statuses(flight).join(","));
        assert.equal(s.model.count("implement"), 1);
      }));
  }
});

describe("#1604 — boundaries of the revise receipt", () => {
  it("interrupted after the revised plan is persisted but before its APPLIED lands: the reclaim never offers the superseded plan", () =>
    scenario(async (s) => {
      const first = await s.toFirstGate();
      // The moment the revised plan lands, the claim is released under the flight (its later
      // receipts are refused), so any APPLIED still owed for the revise never lands.
      api.onState(s.runId, (b) => {
        if (b.status === "awaiting_approval" && b.plan_md === revisedPlan(1)) {
          api.setInputClaimGeneration(s.runId, 99);
          api.setInputFenceReason(s.runId, "released");
        }
      });
      s.send(s.input("revise_plan", FEEDBACK));
      assert.ok(await until(() => s.gateAt(revisedPlan(1)) >= 0 || first.finished), "the revised plan was persisted");
      // A harmless row makes a flight with no receipt still owed notice the release too.
      if (!(await until(() => first.finished, 1_000))) s.send(s.input("follow_up", "nudge"));
      assert.ok(await until(() => first.finished), "the released flight ended");
      api.onState(s.runId, () => {});
      const { flight } = await resumeAndApprove(s, s.resumeClaim("kept"));
      assert.ok(!s.gates(flight).some((g) => g.plan_md === PLAN_V1), "never the superseded plan");
      assert.match(s.gates(flight)[0]?.plan_md ?? "", /# REVISED PLAN/, "a revised plan is offered");
      assert.ok(s.statuses(flight).includes("completed"), s.statuses(flight).join(","));
      assert.equal(s.rowsOf("revise_plan").length, 1, "no new revise row");
    }));

  it("a lost APPLIED reply for the revise is retried; one revision, applied after persistence", () =>
    scenario(async (s) => {
      const first = await s.toFirstGate();
      api.loseNextInputReceiptReply("applied");
      const [row] = s.send(s.input("revise_plan", FEEDBACK));
      assert.ok(await until(() => s.gates(first).length >= 2 || first.finished), s.statuses(first).join(","));
      s.send(s.input("approve_plan"));
      await s.finish(first);
      assert.equal(s.model.revisions, 1, "one revision");
      assert.ok(api.isApplied(s.runId, row!.id), "the revise is applied");
      assert.ok(s.appliedAt(row!.id) > s.gateAt(revisedPlan(1)), "applied only after its revised plan was persisted");
      assert.ok(s.statuses(first).includes("completed"), s.statuses(first).join(","));
    }));

  it("an ask_user park and wake inside the revision turn: the question is answered, the revise settles after persistence", () =>
    scenario(async (s) => {
      s.model.askInNextRevise = true;
      const first = await s.toFirstGate();
      const answers: UserInput[] = [];
      api.onState(s.runId, (b) => {
        if (b.status === "awaiting_input" && b.open_question_id) answers.push(...s.send(s.answer(b.open_question_id)));
      });
      const [row] = s.send(s.input("revise_plan", FEEDBACK));
      assert.ok(await until(() => s.gates(first).length >= 2 || first.finished), s.statuses(first).join(","));
      s.send(s.input("approve_plan"));
      await s.finish(first);
      assert.ok(s.statuses(first).includes("awaiting_input"), `the revision turn parked on its question: ${s.statuses(first).join(",")}`);
      assert.equal(answers.length, 1, "one answer was sent");
      assert.ok(api.isApplied(s.runId, answers[0]!.id), "the answer is applied");
      assert.equal(s.gates(first)[1]?.plan_md, revisedPlan(1), "the revised plan is gated");
      assert.ok(s.appliedAt(row!.id) > s.gateAt(revisedPlan(1)), "the revise is applied only after its revised plan was persisted");
      assert.ok(s.statuses(first).includes("completed"), s.statuses(first).join(","));
    }));

  it("a mixed revise / follow_up / answer batch: the others settle promptly, the revise after persistence", () =>
    scenario(async (s) => {
      const first = await s.toFirstGate();
      const block = s.model.block("revise");
      const [rev, fu, ans] = s.send(
        s.input("revise_plan", FEEDBACK),
        s.input("follow_up", "also update the changelog"),
        s.answer("q-not-open"),
      );
      await block.entered;
      assert.ok(await until(() => api.isApplied(s.runId, fu!.id) && api.isApplied(s.runId, ans!.id), 3_000), "the follow_up and answer are applied while the revision runs");
      assert.equal(api.isApplied(s.runId, rev!.id), false, "the revise waits for its revised plan");
      block.release();
      assert.ok(await until(() => s.gates(first).length >= 2 || first.finished), s.statuses(first).join(","));
      s.send(s.input("approve_plan"));
      await s.finish(first);
      assert.ok(s.appliedAt(rev!.id) > s.gateAt(revisedPlan(1)), "the revise is applied after persistence");
      assert.equal(s.model.revisions, 1);
      // Not asserted: that the follow-up reaches an implement turn. A follow-up is folded into the
      // turn AFTER the current one, and this scripted lead signals done on its first implement
      // turn, so on the base code too it never reaches a prompt (independent of #1604).
      assert.ok(s.statuses(first).includes("completed"), s.statuses(first).join(","));
    }));

  it("repeated GETs of the deferred revise neither re-ACK nor re-route it, while newer inputs still flow", () =>
    scenario(async (s) => {
      const first = await s.toFirstGate();
      const block = s.model.block("revise");
      const [rev] = s.send(s.input("revise_plan", FEEDBACK));
      await block.entered;
      const gets = api.inputGets.get(s.runId) ?? 0;
      assert.ok(await until(() => (api.inputGets.get(s.runId) ?? 0) >= gets + 10, 3_000), "the channel keeps polling during the revision");
      const [fu] = s.send(s.input("follow_up", "keep the public API"));
      assert.ok(await until(() => api.isApplied(s.runId, fu!.id), 3_000), "a newer input is ACKed and applied during the deferral");
      assert.equal(api.isApplied(s.runId, rev!.id), false, "the revise is still deferred");
      assert.equal(s.acks(rev!.id), 1, "the deferred revise was ACKed once, not per GET");
      block.release();
      assert.ok(await until(() => s.gates(first).length >= 2 || first.finished), s.statuses(first).join(","));
      s.send(s.input("approve_plan"));
      await s.finish(first);
      assert.equal(s.model.revisions, 1, "one revision");
      assert.ok(s.statuses(first).includes("completed"), s.statuses(first).join(","));
    }));

  it("a declined awaiting_approval ack leaves the revise unapplied; the reclaim replays it", () =>
    scenario(async (s) => {
      const first = await s.toFirstGate();
      api.failStateWhen(s.runId, (b) => b.status === "awaiting_approval" && b.plan_md === revisedPlan(1), { httpStatus: 409, runStatus: "running" });
      const [row] = s.send(s.input("revise_plan", FEEDBACK));
      assert.ok(await until(() => s.model.revisions >= 1));
      await tick(150);
      assert.equal(api.isApplied(s.runId, row!.id), false, "a declined revised-plan report does not settle the revise");
      api.requestCredentialSwitch(s.runId, 1);
      await s.finish(first);
      assert.ok(s.statuses(first).includes("credential_switch"), s.statuses(first).join(","));
      assert.equal(s.persistedGate()?.plan_md, PLAN_V1, "the server still holds the submitted plan");
      const { flight, gateAt } = await resumeAndApprove(s, s.resumeClaim("kept"));
      assertRevisedOnResume(s, flight, FEEDBACK, "kept");
      assert.ok(s.appliedAt(row!.id, flight.timelineFrom) > gateAt, "applied after the replayed revision was persisted");
      assert.ok(s.statuses(flight).includes("completed"), s.statuses(flight).join(","));
    }));

  it("revision-limit exhaustion is explicit and the exhausted revise is applied", () =>
    scenario(async (s) => {
      const first = await s.toFirstGate();
      const [row] = s.send(s.input("revise_plan", FEEDBACK));
      assert.ok(await until(() => s.gates(first).length >= 2 || first.finished), s.statuses(first).join(","));
      s.send(s.input("approve_plan"));
      await s.finish(first);
      assert.ok(s.texts(first).includes("revision budget exhausted — not revising the plan further"), s.texts(first).join(" | "));
      assert.equal(s.model.count("revise"), 0, "no revision turn past the limit");
      assert.deepEqual(s.gates(first).map((g) => g.plan_md), [PLAN_V1, PLAN_V1], "the current plan is re-gated");
      assert.ok(api.isApplied(s.runId, row!.id), "the exhausted revise is applied");
      assert.ok(s.statuses(first).includes("completed"), s.statuses(first).join(","));
    }, { config: { plan_max_revisions: 0 } as ClaimResponse["config"] }));

  it("a stale reject (sent during the revision) is applied with the stale notice", () =>
    scenario(async (s) => {
      const first = await s.toFirstGate();
      const block = s.model.block("revise");
      s.send(s.input("revise_plan", FEEDBACK));
      await block.entered;
      const [rej] = s.send(s.input("reject_plan", "no"));
      assert.ok(await until(() => api.isAcked(s.runId, rej!.id)));
      block.release();
      assert.ok(await until(() => s.gates(first).length >= 2 || first.finished), s.statuses(first).join(","));
      s.send(s.input("approve_plan"));
      await s.finish(first);
      assert.ok(s.texts(first).includes(STALE_REJECT_NOTICE), s.texts(first).join(" | "));
      assert.ok(api.isApplied(s.runId, rej!.id), "the stale reject is applied");
      assert.ok(s.statuses(first).includes("completed"), s.statuses(first).join(","));
    }));

  it("a reject superseded in the buffer by a newer same-epoch verdict is applied with a notice", () =>
    scenario(async (s) => {
      const block = s.model.block("plan");
      const first = s.start(s.claim());
      await block.entered;
      const [rej, ap] = s.send(s.input("reject_plan", "no"), s.input("approve_plan"));
      assert.ok(await until(() => api.isAcked(s.runId, ap!.id)));
      block.release();
      await s.finish(first);
      assert.ok(s.texts(first).includes(SUPERSEDED_REJECT_NOTICE), s.texts(first).join(" | "));
      assert.ok(api.isApplied(s.runId, rej!.id), "the superseded reject is applied");
      assert.ok(s.statuses(first).includes("completed"), s.statuses(first).join(","));
    }));
});

describe("#1604 — receipt scheduling around a revise that becomes ready", () => {
  it("during another batch's slow ACK: the revise is applied while that ACK is in flight; later reports wait for both", () =>
    scenario(async (s) => {
      const first = await s.toFirstGate();
      const block = s.model.block("revise");
      const [rev] = s.send(s.input("revise_plan", FEEDBACK));
      await block.entered;
      api.delayInputReceipts("ack", 1_500);
      const [fu] = s.send(s.input("follow_up", "keep the changelog"));
      assert.ok(await until(() => api.inputReceiptCalls.some((c) => c.kind === "ack" && c.ids.includes(fu!.id))));
      block.release();
      assert.ok(await until(() => s.gates(first).length >= 2 || first.finished), s.statuses(first).join(","));
      api.delayInputReceipts("ack", 0);
      s.send(s.input("approve_plan"));
      await s.finish(first);
      const fuAckReply = api.timeline.findIndex((e) => e.type === "receipt_reply" && e.kind === "ack" && e.ids.includes(fu!.id) && e.httpStatus === 200);
      const revApplied = s.appliedAt(rev!.id);
      const v2At = s.gateAt(revisedPlan(1));
      assert.ok(revApplied > v2At, "the revise is applied after its revised plan was persisted");
      assert.ok(revApplied < fuAckReply, "the ready revise was applied while the other batch's slow ACK was in flight");
      const runningAt = s.stateAt("running", v2At);
      assert.ok(runningAt > Math.max(revApplied, s.appliedAt(fu!.id)), "the next report waited for both receipts");
      assert.ok(s.statuses(first).includes("completed"), s.statuses(first).join(","));
    }));

  it("during a slow APPLIED: both settle and the next report waits for both", () =>
    scenario(async (s) => {
      const first = await s.toFirstGate();
      const block = s.model.block("revise");
      const [rev] = s.send(s.input("revise_plan", FEEDBACK));
      await block.entered;
      api.delayInputReceipts("applied", 800);
      const [fu] = s.send(s.input("follow_up", "keep the changelog"));
      assert.ok(await until(() => api.inputReceiptCalls.some((c) => c.kind === "applied" && c.ids.includes(fu!.id))));
      block.release();
      assert.ok(await until(() => s.gates(first).length >= 2 || first.finished, 10_000), s.statuses(first).join(","));
      api.delayInputReceipts("applied", 0);
      s.send(s.input("approve_plan"));
      await s.finish(first);
      const v2At = s.gateAt(revisedPlan(1));
      assert.ok(s.appliedAt(rev!.id) > v2At, "the revise is applied after its revised plan was persisted");
      assert.ok(s.appliedAt(fu!.id) >= 0, "the follow-up is applied");
      const runningAt = s.stateAt("running", v2At);
      assert.ok(runningAt > Math.max(s.appliedAt(rev!.id), s.appliedAt(fu!.id)), "the next report waited for both receipts");
      assert.ok(s.statuses(first).includes("completed"), s.statuses(first).join(","));
    }));
});

/** A single resumed claim at the gate (session kept) with `rows` sent before it was claimed. */
function resumeWith(s: Scenario, ...rows: UserInput[]): { flight: Flight; rows: UserInput[] } {
  s.send(...rows);
  s.writeTranscript();
  const flight = s.start(
    s.claim({ resume_phase: "awaiting_approval", plan_md: PLAN_V1, milestones: V1_MILESTONES, plan_source: "agent", plan_approved: false, session_id: SID }),
  );
  return { flight, rows };
}

/** Wait for the flight's first gate, approve it, and let the flight end. */
async function approveFirstGate(s: Scenario, flight: Flight, ms = 8_000): Promise<void> {
  await until(() => s.gates(flight).length >= 1 || flight.finished, ms);
  if (!flight.finished) s.send(s.input("approve_plan"));
  await s.finish(flight);
}

const RECOVERY_REASON = "could not read plan-gate inputs after the resume";

describe("#1604 — delivery on resume: no plan is offered before the inputs sent before the release are read", () => {
  it("delayed GET", () =>
    scenario(async (s) => {
      api.delayInputGets(s.runId, 400);
      const { flight } = resumeWith(s, s.input("revise_plan", FEEDBACK));
      await approveFirstGate(s, flight);
      assertRevisedOnResume(s, flight, FEEDBACK, "kept");
      assert.ok(s.statuses(flight).includes("completed"), s.statuses(flight).join(","));
    }));

  it("delayed ACK", () =>
    scenario(async (s) => {
      api.delayInputReceipts("ack", 400);
      const { flight } = resumeWith(s, s.input("revise_plan", FEEDBACK));
      await until(() => api.inputReceiptReplies.some((r) => r.kind === "ack"));
      api.delayInputReceipts("ack", 0);
      await approveFirstGate(s, flight);
      assertRevisedOnResume(s, flight, FEEDBACK, "kept");
    }));

  for (const which of ["GET", "ACK"] as const) {
    it(`transient ${which} failures: the plan waits, one status line says why`, () =>
      scenario(async (s) => {
        if (which === "GET") api.failInputGets(s.runId, 5, 503);
        else api.failInputReceiptsTimes("ack", 5, 503);
        const { flight } = resumeWith(s, s.input("revise_plan", FEEDBACK));
        await approveFirstGate(s, flight);
        assert.equal(s.texts(flight).filter((t) => t === STATUS_WAITING_DELIVERY).length, 1, `one waiting status line: ${s.texts(flight).join(" | ")}`);
        assertRevisedOnResume(s, flight, FEEDBACK, "kept");
      }));
  }

  it("a bounded transient give-up parks in recovery_wait (non-terminal, verdicts unapplied); a re-claim recovers", () =>
    scenario(async (s) => {
      api.recoveryWaitRequiresRunning = true;
      api.failInputGets(s.runId, Infinity, 503);
      const { flight, rows } = resumeWith(s, s.input("revise_plan", FEEDBACK));
      await until(() => ["recovery_wait", "awaiting_approval", "failed"].some((st) => s.statuses(flight).includes(st)), 90_000);
      assert.equal(s.gates(flight).length, 0, "the plan was never offered");
      const park = s.states(flight).find((b) => b.status === "recovery_wait");
      assert.ok(park, `the give-up parked in recovery_wait: ${s.statuses(flight).join(",")}`);
      assert.ok(
        JSON.stringify(park).includes(RECOVERY_REASON) || s.texts(flight).some((t) => t.includes(RECOVERY_REASON)),
        "the park names why",
      );
      assert.ok(!s.statuses(flight).includes("failed"), "non-terminal");
      assert.equal(api.isApplied(s.runId, rows[0]!.id), false, "the revise is unapplied");
      api.failInputGets(s.runId, 0);
      await s.finish(flight, 2_000);
      const { flight: again, gateAt } = await resumeAndApprove(s, s.resumeClaimAtV1());
      assertRevisedOnResume(s, again, FEEDBACK, "kept");
      assert.ok(s.appliedAt(rows[0]!.id, again.timelineFrom) > gateAt);
      assert.ok(s.statuses(again).includes("completed"), s.statuses(again).join(","));
    }));

  it("the give-up on a clean plan-only clone (no implementation commits) captures its restore point and parks; the poller keeps polling", () =>
    scenario(async (s) => {
      api.recoveryWaitRequiresRunning = true;
      api.failInputGets(s.runId, Infinity, 503);
      // The GETs made by the time the park is recorded. The flight ends right after the park
      // (handleRecoveryExhausted is unchanged), so the poller's liveness is proven across the
      // give-up-to-park window: every GET fails, the give-up comes at the bound's 60th failure
      // (2 x the channel's ACTIVE_APPLY_ATTEMPTS), and the poller keeps reading after it.
      let getsAtPark = -1;
      api.onState(s.runId, (b) => {
        if (b.status === "recovery_wait" && getsAtPark < 0) getsAtPark = api.inputGets.get(s.runId) ?? 0;
      });
      const { flight } = resumeWith(s, s.input("revise_plan", FEEDBACK));
      await until(() => ["recovery_wait", "awaiting_approval", "failed"].some((st) => s.statuses(flight).includes(st)), 90_000);
      assert.ok(s.statuses(flight).includes("recovery_wait"), `reached recovery_wait: ${s.statuses(flight).join(",")}`);
      assert.ok(!s.statuses(flight).includes("failed"), "the capture of a clean plan-only clone did not fail the run");
      assert.ok(getsAtPark >= 60 + 3, `the steering poller kept polling after the give-up, through the park: ${getsAtPark} GETs`);
      api.onState(s.runId, () => {});
      api.failInputGets(s.runId, 0);
      s.send(s.input("cancel"));
      await s.finish(flight, 3_000);
    }));

  it("a definitive protocol failure fails the run explicitly", () =>
    scenario(async (s) => {
      api.rawInputGets(s.runId, { inputs: "not-a-list", receipts: true });
      const { flight } = resumeWith(s, s.input("revise_plan", FEEDBACK));
      await until(() => s.statuses(flight).includes("failed") || s.gates(flight).length > 0, 20_000);
      assert.equal(s.gates(flight).length, 0, "the plan was never offered");
      assert.match(s.states(flight).find((b) => b.status === "failed")?.failure_reason ?? "", /^plan-gate input delivery failed: /);
      api.rawInputGets(s.runId, undefined, 0);
      await s.finish(flight, 2_000);
    }));

  it("a fenced claim ends quietly", () =>
    scenario(async (s) => {
      const { flight, rows } = resumeWith(s, s.input("revise_plan", FEEDBACK));
      api.setInputClaimGeneration(s.runId, 7);
      api.setInputFenceReason(s.runId, "released");
      await s.finish(flight, 8_000);
      assert.deepEqual(s.statuses(flight).filter((st) => ["awaiting_approval", "failed", "completed"].includes(st)), [], s.statuses(flight).join(","));
      assert.equal(s.model.turns.length, 0, "no turn ran");
      assert.equal(api.isApplied(s.runId, rows[0]!.id), false, "the revise is left for the next claim");
    }));

  it("a cancel beats a pending revise", () =>
    scenario(async (s) => {
      const { flight } = resumeWith(s, s.input("revise_plan", FEEDBACK), s.input("cancel"));
      await s.finish(flight);
      assert.equal(s.gates(flight).length, 0, "no plan was offered");
      assert.equal(s.model.turns.length, 0, "no revision turn");
      assert.ok(s.statuses(flight).includes("failed"), s.statuses(flight).join(","));
    }));

  it("an approve submitted during delayed delivery goes stale; the revise wins", () =>
    scenario(async (s) => {
      api.delayInputGets(s.runId, 400);
      const { flight } = resumeWith(s, s.input("revise_plan", FEEDBACK));
      await tick(100);
      s.send(s.input("approve_plan"));
      await approveFirstGate(s, flight);
      assert.ok(s.texts(flight).includes(STALE_APPROVE_NOTICE), s.texts(flight).join(" | "));
      assertRevisedOnResume(s, flight, FEEDBACK, "kept");
      assert.ok(s.statuses(flight).includes("completed"), s.statuses(flight).join(","));
    }));
});

describe("#1604 — replay across claims", () => {
  it("claim N's late APPLIED is refused once claim N+1 holds the run; the replay makes no new row", () =>
    scenario(async (s) => {
      const first = await s.toFirstGate();
      const release = api.holdNextInputReceipt("applied");
      const block = s.model.block("revise");
      const [row] = s.send(s.input("revise_plan", FEEDBACK));
      await block.entered;
      await tick(60);
      first.runner.shutdown();
      await until(() => first.finished, 1_500);
      const claim = s.resumeClaim("kept");
      release();
      await s.finish(first, 5_000);
      assert.equal(api.isApplied(s.runId, row!.id), false, "claim 1's late APPLIED did not apply the revise");
      const { flight, gateAt } = await resumeAndApprove(s, claim);
      assertRevisedOnResume(s, flight, FEEDBACK, "kept");
      assert.equal(s.rowsOf("revise_plan").length, 1, "the replay created no new revise row");
      assert.ok(s.appliedAt(row!.id, flight.timelineFrom) > gateAt);
    }));
});

// Issue #1604: a plan-gate verdict (revise / approve / reject) interrupted between routing and its
// settlement must be neither lost nor applied against the wrong plan. Every case drives the REAL
// SdkExecutor (a scripted query) through the REAL RunRunner against the FakeApi with strict receipt
// generations; an interruption is a second execute() of the SAME run on a fresh runner with the
// next claim generation (the server's reclaim), whose claim carries what the server persisted.
//
// The target contract (the #1604 plan, D1-D6), asserted black-box:
//  - a taken revise stays unapplied until its revised plan is confirmed persisted (the revised
//    awaiting_approval report applied), so an interruption before that replays it;
//  - a taken or buffered reject is applied only with the `failed` transition (the server keys on
//    the stop_kind 'plan_rejected' the reject stamped), so an interruption before that replays it;
//  - a resumed, unapproved claim with a persisted plan reads the inputs sent before the release
//    BEFORE offering any plan, and a replayed revise revises the submitted plan instead of
//    re-presenting it (with no session, the revision prompt carries the prior plan);
//  - stale and superseded rejects and revises are applied with a feed notice; a stale or superseded
//    approve is NEVER applied (the server reads any applied approve_plan as the human approval), so
//    only an approve a gate took is applied; on a resumed claim a replayed verdict created before the
//    persisted plan was shown (claim.resume_plan_at) is stale with its own notice, a claim without
//    that field fails closed (every replayed approve/reject is stale), and a claim that does not
//    re-present the persisted plan bumps the epoch at its first gate.
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { SDKMessage } from "@anthropic-ai/claude-agent-sdk";
import { SdkExecutor, type SdkQueryFn } from "../src/sdk-executor.js";
import type { RunRunner, RunnerOptions } from "../src/runner.js";
import { type Executor } from "../src/executor.js";
import type { AgentTemplate, ClaimResponse, StateRequest, UserInput } from "../src/protocol.js";
import { nullLogger } from "./helpers.js";
import { api, fakeGitlab, gitlabClaim, runnerWith, simulateCommittedWork } from "./runner-harness.js";


// --- fixtures ----------------------------------------------------------------------------------

/** The server-assembled claim fields for an unapproved agent plan whose session_id is NULL
 *  (api/internal/workersvc asserts the live-DB claim assembly produces exactly these). */
const FIXTURE = JSON.parse(
  fs.readFileSync(new URL("../../fixtures/claims/resume-unapproved-no-session.json", import.meta.url), "utf8"),
) as { plan_md: string; plan_source: NonNullable<ClaimResponse["plan_source"]>; plan_approved: boolean; session_id: null };

export const PLAN_V1 = FIXTURE.plan_md;
export const V1_MILESTONES = [{ id: "m1", title: "first cut" }];
export const SID = "11111111-2222-4333-8444-555555555555";
export const FEEDBACK = "split the migration into its own milestone";
const REVISE_MARK = "The plan reviewer read your proposed plan";
const PLAN_MARK = "Produce a concrete implementation plan";
const AGENTS: AgentTemplate[] = [
  { name: "reviewer", description: "reviews the change", prompt_body: "Review the change." },
  { name: "coder", description: "writes the change", prompt_body: "Write the change." },
];
export const SELECTION = JSON.stringify({ source: "own", exclusions: ["reviewer"] });
export const SELECTION_STATUS = "implementing with your agent templates (coder)";
export const DEFAULT_SELECTION_STATUS = "implementing with your agent templates (reviewer, coder)";

// Issue #1920: shutdown must not wait out a full input-GET timeout or backoff.
// This generous bound guards that regression, rather than normal scheduling latency.
const SHUTDOWN_PROMPTNESS_MS = 15_000;
export async function promptly(done: Promise<void>): Promise<void> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  try {
    await Promise.race([
      done,
      new Promise<never>((_, reject) => {
        timer = setTimeout(() => reject(new Error("shutdown exceeded the #1920 promptness bound")), SHUTDOWN_PROMPTNESS_MS);
      }),
    ]);
  } finally {
    clearTimeout(timer);
  }
}

export const STATUS_RESUME_WITH_REVISION =
  "resuming at the plan gate with a revision the owner sent before the claim was released — revising the submitted plan instead of re-presenting it";
export const STATUS_WAITING_DELIVERY =
  "waiting to read plan-gate inputs sent before the claim was released — the plan is not offered for approval until they are read";
export const STALE_APPROVE_NOTICE = "Approval ignored — the plan changed; re-send if you still want it.";
export const STALE_REJECT_NOTICE = "Rejection ignored — the plan changed; re-send if you still want it.";
export const SUPERSEDED_REJECT_NOTICE = "an earlier plan rejection was superseded by a newer verdict";
export const STALE_REVISE_NOTICE = "Feedback ignored — it was written against an older plan version; re-send it.";
export const REPLAY_STALE_VERDICT_NOTICE = "A plan verdict sent before this plan was shown was ignored — re-send it if you still want it.";
export const REPLAY_STALE_REVISE_NOTICE = "Plan feedback sent before this plan was shown was ignored — re-send it if you still want it.";
export const REPLAY_UNJUDGED_NOTICE = "Could not confirm which plan this verdict was for — it was ignored; re-send it if you still want it.";
export const APPROVED_REVISE_NOTICE = "The plan is already approved — this revision request was ignored; cancel the run to stop it.";
export const APPROVED_REJECT_NOTICE = "The plan is already approved — this rejection was ignored; cancel the run to stop it.";

export const revisedPlan = (n: number): string => `# REVISED PLAN r${n}\n- the owner's feedback, applied`;
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
  private readonly blocks: Record<TurnKind, Array<{ entered: () => void; released: Promise<void> }>> = {
    plan: [],
    revise: [],
    implement: [],
  };
  /** The next revision turn calls ask_user first; its answer turn submits the revised plan. */
  askInNextRevise = false;
  private answerTurnPending = false;

  block(kind: TurnKind): Block {
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
    {
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

export const tick = (ms = 5): Promise<void> => new Promise((r) => setTimeout(r, ms));

/** Poll `pred` until true or `ms` elapses; returns whether it became true (never throws). */
export async function until(pred: () => boolean, ms = 8_000): Promise<boolean> {
  const end = Date.now() + ms;
  while (!pred()) {
    if (Date.now() > end) return false;
    await tick();
  }
  return true;
}

export interface Flight {
  claim: ClaimResponse;
  runner: RunRunner;
  done: Promise<void>;
  finished: boolean;
  error?: unknown;
  stateFrom: number;
  seqFrom: number;
  turnFrom: number;
  receiptFrom: number;
  timelineFrom: number;
}

let nextIid = 16040;

export class Scenario {
  readonly model = new Model();
  readonly root = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1604-"));
  readonly home = path.join(this.root, "home");
  readonly base: ClaimResponse;
  private generation = 0;
  private nextId = 100;

  constructor(overrides: Partial<ClaimResponse> = {}) {
    simulateCommittedWork();
    api.strictReceiptGenerations = true;
    api.stampInputCreatedAt = true;
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

  /** The claim the server assembles for a resumed run at the gate: the last persisted plan (and its
   *  candidate milestones), the session kept unless `session` says otherwise. Like the server, any
   *  applied approve_plan makes the claim plan_approved with resume_phase "implementing" (its
   *  human_plan_approved), whichever plan that approve was sent against. */
  resumeClaim(session: "kept" | "lost" | "none", overrides: Partial<ClaimResponse> = {}): ClaimResponse {
    const gate = this.persistedGate();
    assert.ok(gate, "a plan was persisted before the interruption");
    if (api.humanPlanApproved(this.runId)) {
      if (session === "kept") this.writeTranscript();
      return this.claim({
        plan_md: gate.plan_md,
        plan_source: "agent",
        plan_approved: true,
        milestones: gate.milestones ?? null,
        resume_phase: "implementing",
        session_id: session === "none" ? null : SID,
        ...overrides,
      });
    }
    // resume_plan_at: when the persisted, unapproved plan was last shown (the server's claim field).
    const shownAt = api.resumePlanAt(this.runId);
    const common: Partial<ClaimResponse> = {
      plan_md: gate.plan_md,
      plan_source: "agent",
      plan_approved: false,
      milestones: gate.milestones ?? null,
      ...(shownAt !== undefined ? { resume_plan_at: shownAt } : {}),
    };
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

  start(claim: ClaimResponse, opts: { executor?: () => Executor; runner?: Partial<RunnerOptions> } = {}): Flight {
    const { gitlab } = fakeGitlab();
    const runner = runnerWith(
      () => ({
        executor: opts.executor?.() ?? new SdkExecutor(nullLogger(), this.home, {
          queryFn: this.model.queryFn(),
        }),
        homeDir: this.home,
      }),
      gitlab,
      undefined,
      nullLogger(),
      { recoveryRetryMs: 1, ...opts.runner },
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
      (error) => {
        flight.error = error;
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

  /** Require graceful shutdown to settle promptly, without a full input-GET timeout. */
  async shutdown(flight: Flight): Promise<void> {
    flight.runner.shutdown();
    await promptly(flight.done);
    assert.equal(flight.finished, true, "shutdown settled the gate flight");
    assert.equal(flight.error, undefined, "shutdown completed without an execution error");
    assert.equal(this.rowsOf("cancel").length, 0, "shutdown needed no cancel input");
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
      api.signalForeignCredentialSwitch(this.runId, undefined);
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

/** Collect callback failures before Scenario.teardown changes the evidence; omit input and plan bodies. */
function scenarioFailureDiagnostic(s: Scenario, label: string): string {
  const text = (value: string | undefined): string =>
    value === undefined ? "<absent>" : value.length > 400 ? value.slice(0, 400) + "...<truncated>" : value;
  const bounded = <T,>(values: T[], limit: number): { total: number; items: T[]; omitted: number } => ({
    total: values.length,
    items: values.slice(0, limit),
    omitted: Math.max(0, values.length - limit),
  });
  const summarizeReports = (states: StateRequest[]) => {
    const failed = states.filter((state) => state.status === "failed");
    const gateReports = states.filter((state) => state.status === "awaiting_approval");
    return {
      statuses: bounded(states.map((state) => text(state.status)), 40),
      gateObservations: gateReports.length === 0 ? "<no gate report>" : bounded(gateReports.map((state) => ({
        planPresent: Boolean(state.plan_md),
        matchesPlanV1: state.plan_md === PLAN_V1,
        matchesRevisedPlan1: state.plan_md === revisedPlan(1),
      })), 8),
      failedReports: failed.length === 0 ? "<no failed report>" : bounded(failed.map((state) => ({
        failure_reason: text(state.failure_reason),
        fail_origin: text(state.fail_origin),
      })), 8),
    };
  };
  const stampedReports = new Map<number, StateRequest[]>();
  for (const state of api.states) {
    if (state.runId !== s.runId || state.body.claim_generation === undefined) continue;
    const generation = state.body.claim_generation;
    const reports = stampedReports.get(generation);
    if (reports === undefined) stampedReports.set(generation, [state.body]);
    else reports.push(state.body);
  }
  const generationReports = Array.from(stampedReports, ([generation, reports]) => ({
    generation,
    ...summarizeReports(reports),
  }));
  const arrivalWindowObservations = s.flights.map((flight, flightIndex) => {
    const stateFrom = flight.stateFrom;
    const stateTo = s.flights[flightIndex + 1]?.stateFrom ?? api.states.length;
    const reports = api.states
      .slice(stateFrom, stateTo)
      .filter((state) => state.runId === s.runId && state.body.claim_generation === undefined)
      .map((state) => state.body);
    return {
      label: "arrival-window observations",
      flightIndex,
      stateFrom,
      stateTo,
      ...summarizeReports(reports),
    };
  });
  const flights = s.flights.map((flight) => {
    const hasError = Object.hasOwn(flight, "error");
    const error = flight.error;
    const errorDescription = error instanceof Error
      ? `${text(error.name)}: ${text(error.message)}`
      : error === null || (typeof error !== "object" && typeof error !== "function")
        ? String(error)
        : `<${typeof error} rejection; body omitted>`;
    return {
      generation: flight.claim.claim_generation,
      finished: flight.finished,
      execution: hasError ? { rejected: true, error: text(errorDescription) } : "<no rejected execution>",
    };
  });
  return JSON.stringify({
    label: text(label),
    resumedFlight: s.flights.length > 1 ? "present" : "<no resumed flight>",
    flights: bounded(flights, 12),
    generationReports: bounded(generationReports, 12),
    arrivalWindowObservations: bounded(arrivalWindowObservations, 12),
    cancellationRows: s.rowsOf("cancel").length > 0 ? "present" : "<no cancellation row>",
    // Keep the end of long timelines: the finalization immediately before failure matters most.
    timeline: {
      total: api.timeline.filter((entry) => entry.runId === s.runId).length,
      items: api.timeline.map((entry, index) => ({ entry, index }))
        .filter(({ entry }) => entry.runId === s.runId)
        .map(({ entry, index }) => entry.type === "state"
          ? { index, type: entry.type, status: entry.status, planPresent: entry.plan_md !== undefined }
          : { index, ...entry }).slice(-200),
    },
    handlerExceptions: bounded(api.handlerExceptions.map((entry) => ({ ...entry, error: text(entry.error) })), 20),
    recentFeed: s.texts().slice(-30).map(text),
    modelTurns: bounded(s.model.turns.map(({ kind, resume }) => ({ kind, resumed: resume !== undefined })), 40),
  });
}

export async function scenario(
  fn: (s: Scenario) => Promise<void>,
  overrides: Partial<ClaimResponse> = {},
  diagnosticLabel = "unlabelled scenario",
): Promise<void> {
  const s = new Scenario(overrides);
  try {
    await fn(s);
  } catch (error) {
    try {
      console.error(`#1604 scenario failure: ${scenarioFailureDiagnostic(s, diagnosticLabel)}`);
    } catch {
      // A diagnostic failure must not replace the callback's original error.
    }
    throw error;
  } finally {
    await s.teardown();
  }
}


export function assertRevisedOnResume(s: Scenario, flight: Flight, feedback: string, session: "kept" | "lost" | "none"): void {
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

/** Issue #1604 round 4: a disposed (stale or superseded) approve never reaches /inputs/applied and
 *  never counts as the human approval. It is settled through /inputs/discarded (disposition
 *  superseded) once the discard lane lands, which takes it out of the replay list. */
export async function assertDisposedApprove(s: Scenario, id: number, what = "the stale approve"): Promise<void> {
  assert.ok(
    !api.inputReceiptCalls.some((c) => c.runId === s.runId && c.kind === "applied" && c.ids.includes(id)),
    `${what} is never sent to /inputs/applied`,
  );
  assert.ok(await until(() => api.isDiscarded(s.runId, id), 3_000), `${what} is discarded (disposition superseded)`);
}

/** The server counts no human approval for the run (a discarded approve is not one). */
export function assertNoApproval(s: Scenario): void {
  assert.equal(api.humanPlanApproved(s.runId), false, "the server counts no human approval");
}

// --- tests ---------------------------------------------------------------------------------------


export async function resumeAndApprove(s: Scenario, claim: ClaimResponse, selection: string | null = null): Promise<{ flight: Flight; gateAt: number }> {
  const flight = s.start(claim);
  await until(() => s.gates(flight).length >= 1 || flight.finished);
  const plan = s.gates(flight)[0]?.plan_md;
  const gateAt = plan === undefined ? -1 : s.gateAt(plan, flight.timelineFrom);
  if (!flight.finished) s.send(s.input("approve_plan", selection));
  await s.finish(flight);
  return { flight, gateAt };
}

/** Park the first claim at its gate, then let a credential switch release it while `row` is sent. */
export async function releaseAtGate(s: Scenario, send: () => UserInput, when: "unacked" | "acked"): Promise<{ first: Flight; row: UserInput }> {
  const first = await s.toFirstGate();
  if (when === "unacked") api.requestCredentialSwitch(s.runId, 1);
  else api.pendSwitchAfterNextAck(s.runId, 1);
  const row = send();
  await s.finish(first);
  return { first, row };
}


export function resumeWith(s: Scenario, ...rows: UserInput[]): { flight: Flight; rows: UserInput[] } {
  s.send(...rows);
  s.writeTranscript();
  const flight = s.start(
    s.claim({ resume_phase: "awaiting_approval", plan_md: PLAN_V1, milestones: V1_MILESTONES, plan_source: "agent", plan_approved: false, session_id: SID }),
  );
  return { flight, rows };
}

export function assertHeldInputGet(runId: string): void {
  const reads = api.inputGets.get(runId) ?? 0;
  assert.ok(reads > 0, "a GET entered before its reply was checked");
  assert.equal(api.inputGetSendAttempts.get(runId) ?? 0, reads - 1, "the entered GET has not attempted a response before release");
}

/** Wait for the flight's first gate, approve it, and let the flight end. */
export async function approveFirstGate(s: Scenario, flight: Flight, ms = 8_000): Promise<void> {
  await until(() => s.gates(flight).length >= 1 || flight.finished, ms);
  if (!flight.finished) s.send(s.input("approve_plan"));
  await s.finish(flight);
}

export const RECOVERY_REASON = "could not read plan-gate inputs after the resume";


export async function pathB(
  s: Scenario,
  kind: "approve_plan" | "reject_plan",
  opts: { omitResumePlanAt?: boolean } = {},
): Promise<{ flight: Flight; row: UserInput }> {
  const first = await s.toFirstGate();
  const block = s.model.block("revise");
  s.send(s.input("revise_plan", FEEDBACK));
  await block.entered;
  api.requestCredentialSwitch(s.runId, 1);
  await tick(30);
  const [row] = s.send(s.input(kind, kind === "approve_plan" ? SELECTION : "wrong approach"));
  await tick(60);
  assert.equal(api.isAcked(s.runId, row!.id), false, "the pending switch left the verdict unread");
  block.release();
  await s.finish(first);
  assert.ok(s.statuses(first).includes("credential_switch"), s.statuses(first).join(","));
  assert.equal(s.persistedGate()?.plan_md, revisedPlan(1), "plan B is persisted");
  // The server omits resume_plan_at when it cannot name the persisted plan's frame.
  api.omitResumePlanAt = opts.omitResumePlanAt === true;
  const claim = s.resumeClaim("kept");
  assert.equal(claim.resume_plan_at === undefined, opts.omitResumePlanAt === true, "the claim carries resume_plan_at unless omitted");
  const flight = s.start(claim);
  assert.ok(await until(() => s.gates(flight).length >= 1 || flight.finished), s.statuses(flight).join(","));
  await tick(200);
  return { flight, row: row! };
}

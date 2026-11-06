// Issue #3985: a clarification asked while planning a HUMAN revise_plan verdict on a plan cross-check
// run settles to awaiting_approval (`clarification_settled`), never to `running`, because the server
// refuses an answer as plan approval. Every case drives the REAL RunRunner callbacks (ctx.gatePlan,
// ctx.askUser, ctx.askPlanMissing, ctx.takeResumedGateEvent, ctx.continueExistingPlanGate), the real
// WorkerClient and the real state choke point against the FakeApi; only the server's settlement
// answer is scripted (api.stateAckWire), because the fake does not model that transition.
import assert from "node:assert/strict";
import fs from "node:fs";
import { describe, it } from "node:test";
import type { Executor, RunContext } from "../src/executor.js";
import type { ClaimResponse, StateRequest } from "../src/protocol.js";
import { performance } from "node:perf_hooks";
import { ActiveRunRegistry } from "../src/active-run-registry.js";
import { WorkerMemoryRuntime } from "../src/worker-memory-runtime.js";
import { nullLogger } from "./helpers.js";
import { api, client, fakeGitlab, installHarness, runner } from "./runner-harness.js";
import { freshClaim, newApi, row, send, until } from "./gate-revision-harness.js";

installHarness();

const FEATURE = "clarification_revision_settlement_v1";
const WIRE = JSON.parse(fs.readFileSync(
  new URL("../../fixtures/worker-protocol/clarification-settlement.json", import.meta.url), "utf8")) as {
  status: string; claim_generation: number; clarification_settlement: Record<string, unknown>;
};

type Ack = "applied" | "applied_false" | "no_disposition" | "other_status" | "conflict" | "owner_pause" | "wall_park" | "transport";

/** The server's answer to a clarification_settled report; undefined leaves the fake's own answer. */
function scriptSettlement(ack: Ack): void {
  api.stateAckWire = (body, run) => {
    if (body.status !== "clarification_settled") return undefined;
    switch (ack) {
      case "applied": return { status: 200, body: { run: { ...run, status: "awaiting_approval" }, disposition: "clarification_settled" } };
      case "applied_false": return { status: 409, body: { run: { ...run, status: "awaiting_approval" }, disposition: "clarification_settled" } };
      case "no_disposition": return { status: 200, body: { run: { ...run, status: "awaiting_approval" } } };
      case "other_status": return { status: 200, body: { run: { ...run, status: "running" }, disposition: "clarification_settled" } };
      case "conflict": return { status: 409, body: { run: { ...run, status: "running" } } };
      case "owner_pause": return { status: 409, body: { run: { ...run, status: "paused" } } };
      case "wall_park": return { status: 409, body: { run: { ...run, status: "paused", hold_reason: "budget_exhausted" } } };
      case "transport": return { status: 400, body: { error: "bad settlement" } };
    }
  };
}

interface Outcome {
  answer?: { kind: string };
  error?: unknown;
  /** Reports the flight sent from the moment the question was asked. */
  sent: StateRequest[];
  /** What the executor observed after the answer returned. */
  afterAnswer: boolean;
  /** The executor body (including `after`) ran to completion or failed. */
  settled: boolean;
}

interface Options {
  claim?: Partial<ClaimResponse>;
  negotiated?: boolean;
  ack?: Ack;
  ask?: "user" | "missing";
  /** Human revise verdict before the question (default true). */
  revise?: boolean;
  /** Drop the negotiated feature once the question is parked, before the answer is routed. */
  loseSupportWhileParked?: boolean;
  /** The old-server behaviour: refuse an unqualified running once a question is parked. */
  runningRefused?: boolean;
  /** Approve the plan presented at the second gate (the human revise is then consumed and applied). */
  approveSecondGate?: boolean;
  /** Run after the human revise and before the question is asked (still inside the executor). */
  beforeAsk?: (ctx: RunContext, settles: number | undefined) => Promise<void>;
  /** Run after the answer returned (still inside the executor). */
  after?: (ctx: RunContext, outcome: Outcome, settles: number | undefined) => Promise<void>;
}

const requiredGated = { plan_cross_check_required: true, auto_approve: false } as const;

function errorName(error: unknown): string {
  return error instanceof Error ? `${error.constructor.name}: ${error.message}` : String(error);
}

async function drive(t: { after: (fn: () => Promise<void>) => void }, opts: Options = {}): Promise<Outcome> {
  newApi();
  api.planRevisionRetries = false;
  const features = [...client.protocolFeatures];
  if (opts.negotiated !== false) features.push(FEATURE);
  client.protocolFeatures = features;
  scriptSettlement(opts.ack ?? "applied");
  if (opts.runningRefused) {
    const settlement = api.stateAckWire!;
    api.stateAckWire = (body, run) => body.status === "running" && api.states.some(s => s.body.status === "awaiting_input")
      ? { status: 409, body: { run: { ...run, status: "awaiting_approval" } } } : settlement(body, run);
  }
  const claim = freshClaim(1, { ...requiredGated, ...opts.claim });
  const id = claim.run_id;
  const outcome: Outcome = { sent: [], afterAnswer: false, settled: false };
  let gates = 0;
  api.onState(id, body => {
    if (body.status === "awaiting_approval" && ++gates === 1 && opts.revise !== false)
      send(id, row("revise_plan", "clarify before revising"));
    else if (body.status === "awaiting_approval" && gates === 2 && opts.approveSecondGate) send(id, row("approve_plan"));
    if (body.status === "awaiting_input" && body.open_question_id) {
      if (opts.loseSupportWhileParked) client.protocolFeatures = client.protocolFeatures.filter(f => f !== FEATURE);
      send(id, row("answer", JSON.stringify({ question_id: body.open_question_id, answers: ["continue"] })));
    }
  });
  const executor: Executor = {
    async run(ctx) {
      let settles: number | undefined;
      if (opts.revise !== false) {
        const verdict = await ctx.gatePlan!("# Plan A");
        assert.equal(verdict.kind, "revise");
        settles = verdict.kind === "revise" ? verdict.inputId : undefined;
      }
      try {
        await opts.beforeAsk?.(ctx, settles);
      } catch (error) {
        outcome.error = error;
        outcome.settled = true;
        throw error;
      }
      const from = api.states.length;
      try {
        outcome.answer = opts.ask === "missing"
          ? await ctx.askPlanMissing!()
          : await ctx.askUser!([{ question: "Continue planning?", header: "Plan" }]);
        outcome.afterAnswer = true;
        outcome.sent = api.states.slice(from).filter(s => s.runId === id).map(s => s.body);
        await opts.after?.(ctx, outcome, settles);
      } catch (error) {
        outcome.error = error;
        outcome.sent = api.states.slice(from).filter(s => s.runId === id).map(s => s.body);
        outcome.settled = true;
        throw error;
      }
      outcome.settled = true;
      throw new Error("fixture reached its end");
    },
  };
  const done = runner(executor, fakeGitlab().gitlab).execute(claim);
  t.after(async () => {
    send(id, row("cancel"));
    await done.catch(() => undefined);
  });
  await Promise.race([until(() => outcome.settled, 10_000), done.catch(() => undefined)]);
  return outcome;
}

const settlements = (o: Outcome) => o.sent.filter(s => s.status === "clarification_settled");
const runnings = (o: Outcome) => o.sent.filter(s => s.status === "running");

describe("#3985 clarification settlement selection", () => {
  for (const scenario of [
    { name: "askUser, issue-kind confirming caller", ask: "user", claim: {} },
    { name: "askUser, prompt-kind non-confirming caller", ask: "user", claim: { kind: "prompt" } },
    { name: "askUser, interactive issue non-confirming caller", ask: "user", claim: { interactive: true } },
    { name: "askPlanMissing", ask: "missing", claim: {} },
    { name: "gated (not required) claim with a persisted plan", ask: "user", claim: { plan_cross_check_required: false, plan_cross_check_gated: true, plan_md: "# Persisted" } },
  ] as const) {
    it(`${scenario.name} settles to awaiting_approval and never reports running`, async t => {
      const o = await drive(t, { ask: scenario.ask, claim: scenario.claim as Partial<ClaimResponse> });
      assert.equal(o.error, undefined, errorName(o.error));
      assert.equal(o.answer?.kind, "answer");
      assert.equal(settlements(o).length, 1);
      assert.equal(runnings(o).length, 0, "an answer to a revise clarification is not plan approval");
      const body = settlements(o)[0]!;
      assert.equal(body.claim_generation, 1);
      assert.equal(body.clarification_settlement?.revision_input_id !== undefined, true);
    });
  }

  it("the settlement binds the open question, the pending revise input and its gate revision", async t => {
    const o = await drive(t);
    const gate = api.states.filter(s => s.body.status === "awaiting_approval").map(s => s.body)[0]!;
    const question = api.states.find(s => s.body.status === "awaiting_input")!.body.open_question_id;
    const input = api.inputRows(api.states[0]!.runId).find(r => r.kind === "revise_plan")!;
    const settlement = settlements(o)[0]!.clarification_settlement!;
    assert.equal(settlement.question_id, question);
    assert.equal(settlement.revision_input_id, input.id);
    assert.equal(settlement.gate_revision, api.gateOf(api.states[0]!.runId)?.revision ?? settlement.gate_revision);
    assert.ok(gate.plan_md);
  });

  it("a run without cross-check keeps the ordinary running settlement after a human revise", async t => {
    const o = await drive(t, { claim: { plan_cross_check_required: false, plan_cross_check_gated: false } });
    assert.equal(o.answer?.kind, "answer");
    assert.equal(settlements(o).length, 0);
    assert.equal(runnings(o).length, 1);
  });

  it("initial planning (no revise) still sends the ordinary running", async t => {
    const o = await drive(t, { revise: false });
    assert.equal(o.answer?.kind, "answer");
    assert.equal(settlements(o).length, 0);
    await until(() => api.states.some(s => s.body.status === "running" && api.states.some(q => q.body.status === "awaiting_input")), 2_000);
  });

  it("a second clarification in the same revision settles again", async t => {
    const o = await drive(t, {
      after: async ctx => {
        const second = await ctx.askUser!([{ question: "Another?", header: "Plan" }]);
        assert.equal(second.kind, "answer");
      },
    });
    assert.equal(o.error, undefined, errorName(o.error));
    const all = api.states.map(s => s.body.status);
    assert.equal(all.filter(s => s === "clarification_settled").length, 2, all.join(","));
    assert.equal(all.filter((s, i) => s === "running" && all.indexOf("awaiting_input") < i).length, 0);
  });

  it("an automatic revise does not clear the pending human revise", async t => {
    const o = await drive(t, {
      after: async ctx => {
        await ctx.continueExistingPlanGate!(async () => ({ kind: "revise", automatic: true, feedback: "auto", round: 1 }));
        const second = await ctx.askUser!([{ question: "After automatic?", header: "Plan" }]);
        assert.equal(second.kind, "answer");
      },
      claim: { plan_cross_check_required: false, plan_cross_check_gated: true, plan_md: "# Persisted" },
    });
    assert.equal(o.error, undefined, errorName(o.error));
    assert.equal(api.states.filter(s => s.body.status === "clarification_settled").length, 2);
  });

  for (const verdict of [{ kind: "reject", reason: "no" }, { kind: "cancel" }] as const) {
    it(`a ${verdict.kind} verdict clears the pending revise: the next question settles with running`, async t => {
      const o = await drive(t, {
        after: async ctx => {
          await ctx.continueExistingPlanGate!(async () => verdict);
          const second = await ctx.askUser!([{ question: "After the verdict?", header: "Plan" }]);
          assert.equal(second.kind, "answer");
        },
        claim: { plan_cross_check_required: false, plan_cross_check_gated: true, plan_md: "# Persisted" },
      });
      assert.equal(o.error, undefined, errorName(o.error));
      assert.equal(api.states.filter(s => s.body.status === "clarification_settled").length, 1);
    });
  }

  it("a newer human revise replaces the pending revise identity", async t => {
    const o = await drive(t, {
      after: async ctx => {
        await ctx.continueExistingPlanGate!(async () => ({ kind: "revise", feedback: "again", inputId: 7777, sourceGateRevision: 4 }));
        await ctx.askUser!([{ question: "After newer?", header: "Plan" }]);
      },
      claim: { plan_cross_check_required: false, plan_cross_check_gated: true, plan_md: "# Persisted" },
    });
    assert.equal(o.error, undefined, errorName(o.error));
    const last = api.states.filter(s => s.body.status === "clarification_settled").at(-1)!.body;
    assert.deepEqual([last.clarification_settlement?.revision_input_id, last.clarification_settlement?.gate_revision], [7777, 4]);
  });

  it("a new presentation at a later gate revision clears the pending revise", async t => {
    const o = await drive(t, {
      claim: { plan_cross_check_required: false, plan_cross_check_gated: true, plan_md: "# Persisted" },
      after: async ctx => {
        // Plan B is presented (a later gate revision); the verdict never arrives while the next
        // question is asked, so the pending revise must already be gone.
        const gateB = ctx.gatePlan!("# Plan B", undefined, undefined, undefined);
        gateB.catch(() => undefined);
        await until(() => api.states.filter(s => s.body.status === "awaiting_approval").length >= 2, 4_000);
        const second = await ctx.askUser!([{ question: "After plan B?", header: "Plan" }]);
        assert.equal(second.kind, "answer");
      },
    });
    assert.equal(o.error, undefined, errorName(o.error));
    assert.equal(api.states.filter(s => s.body.status === "clarification_settled").length, 1);
  });

  it("a reclaimed required-mode claim picks up its context through takeResumedGateEvent", async t => {
    newApi();
    client.protocolFeatures = [...client.protocolFeatures, FEATURE];
    scriptSettlement("applied");
    const base = freshClaim(2, requiredGated);
    const id = base.run_id;
    api.seedGate(id, { revision: 2, presentationId: "0f5c2d4e-1111-4a2b-8c3d-000000000001", historical: [],
      payload: { plan_md: "# Persisted", milestones: [{ id: "m1", title: "first cut" }] } });
    const claim: ClaimResponse = {
      ...base, plan_md: "# Persisted", plan_source: "agent", plan_approved: false,
      milestones: [{ id: "m1", title: "first cut" }], resume_phase: "awaiting_approval",
      resume_plan_at: "2000-01-01T00:00:00.000000Z", session_id: null, ...api.gateResumeFields(id),
    };
    send(id, row("revise_plan", "feedback before the reclaim", { gate_binding: "bound", gate_revision: 2 }));
    api.onState(id, body => {
      if (body.status === "awaiting_input" && body.open_question_id)
        send(id, row("answer", JSON.stringify({ question_id: body.open_question_id, answers: ["go"] })));
    });
    let answered = false;
    let failure: unknown;
    const executor: Executor = {
      resumesAtGate: true,
      async run(ctx) {
        try {
          const pending = await ctx.takeResumedGateEvent!(ctx.signal);
          assert.equal(pending?.kind, "revise");
          await ctx.askUser!([{ question: "Continue?", header: "Plan" }]);
          answered = true;
        } catch (error) { failure = error; throw error; }
        throw new Error("fixture reached its end");
      },
    };
    const done = runner(executor, fakeGitlab().gitlab).execute(claim);
    t.after(async () => { send(id, row("cancel")); await done.catch(() => undefined); });
    await until(() => answered || failure !== undefined, 8_000);
    assert.equal(failure, undefined, errorName(failure));
    assert.equal(api.states.filter(s => s.body.status === "clarification_settled").length, 1);
    const statuses = api.states.map(s => s.body.status);
    assert.equal(statuses.slice(statuses.indexOf("awaiting_input")).includes("running"), false, statuses.join(","));
  });
});

describe("#3985 clarification settlement acknowledgement", () => {
  it("accepts only an applied awaiting_approval ACK carrying the clarification_settled disposition", async t => {
    const o = await drive(t, { ack: "applied" });
    assert.equal(o.answer?.kind, "answer");
    assert.equal(o.error, undefined);
  });

  for (const [ack, expected] of [
    ["applied_false", /clarification revision settlement was not confirmed/],
    ["no_disposition", /clarification revision settlement was not confirmed/],
    ["other_status", /clarification revision settlement was not confirmed/],
    ["conflict", /clarification revision settlement was not confirmed/],
  ] as const) {
    it(`refuses ${ack}: the run stops, no answer is returned and no running follows`, async t => {
      const o = await drive(t, { ack });
      assert.equal(o.answer, undefined);
      assert.match(errorName(o.error), expected);
      assert.equal(settlements(o).length, 1);
      assert.equal(runnings(o).length, 0, "a refusal never falls back to running");
    });
  }

  it("an owner pause maps to OrdinaryOwnerPaused", async t => {
    const o = await drive(t, { ack: "owner_pause" });
    assert.equal(o.answer, undefined);
    assert.match(errorName(o.error), /^OrdinaryOwnerPaused/);
    assert.equal(runnings(o).length, 0);
  });

  it("a wall park maps to ServerWallParkedError", async t => {
    const o = await drive(t, { ack: "wall_park" });
    assert.equal(o.answer, undefined);
    assert.match(errorName(o.error), /^ServerWallParkedError/);
    assert.equal(runnings(o).length, 0);
  });

  it("a transport or server error propagates unchanged and no running follows", async t => {
    const o = await drive(t, { ack: "transport" });
    assert.equal(o.answer, undefined);
    assert.ok(o.error !== undefined);
    assert.doesNotMatch(errorName(o.error), /clarification revision settlement was not confirmed/);
    assert.equal(runnings(o).length, 0);
    assert.equal(settlements(o).length, 1, "the settlement is sent once, never re-sent as another status");
  });
});

describe("#3985 clarification settlement capability", () => {
  it("never negotiated: a confirmed running is sent, never a settlement", async t => {
    const o = await drive(t, { negotiated: false });
    assert.equal(settlements(o).length, 0);
    assert.equal(runnings(o).length, 1);
    assert.equal(o.answer?.kind, "answer");
  });

  it("never negotiated and the old server refuses running: explained refusal, no answer returned", async t => {
    const o = await drive(t, { negotiated: false, runningRefused: true });
    assert.equal(o.answer, undefined);
    assert.match(errorName(o.error), /clarification running settlement was not confirmed/);
    assert.equal(settlements(o).length, 0);
  });

  it("selected then lost during the park: RetainedRecoveryStop and nothing is sent", async t => {
    const o = await drive(t, { loseSupportWhileParked: true });
    assert.equal(o.answer, undefined);
    assert.match(errorName(o.error), /^RetainedRecoveryStop/);
    assert.equal(o.sent.filter(s => s.status === "clarification_settled" || s.status === "running").length, 0);
  });
});

describe("#3985 clarification settlement wire", () => {
  it("the request body keys, nested keys and value types match fixtures/worker-protocol/clarification-settlement.json", async t => {
    const o = await drive(t);
    const body = settlements(o)[0]!;
    assert.deepEqual(Object.keys(body).sort(), Object.keys(WIRE).sort());
    assert.deepEqual(Object.keys(body.clarification_settlement!).sort(), Object.keys(WIRE.clarification_settlement).sort());
    assert.equal(body.status, WIRE.status);
    assert.equal(typeof body.claim_generation, typeof WIRE.claim_generation);
    for (const [key, value] of Object.entries(WIRE.clarification_settlement))
      assert.equal(typeof (body.clarification_settlement as unknown as Record<string, unknown>)[key], typeof value, key);
  });
});

describe("#3985 ci_fix and prompt kinds, every ask path", () => {
  const confirmed = [
    { name: "askUser, ci_fix claim", ask: "user", claim: { kind: "ci_fix" } },
    { name: "askPlanMissing, ci_fix claim", ask: "missing", claim: { kind: "ci_fix" } },
    { name: "askPlanMissing, prompt claim", ask: "missing", claim: { kind: "prompt" } },
  ] as const;
  for (const scenario of confirmed) {
    it(`${scenario.name}: the settlement binds question, revise input and gate revision, the answer returns once, no running`, async t => {
      const o = await drive(t, { ask: scenario.ask, claim: scenario.claim as Partial<ClaimResponse> });
      assert.equal(o.error, undefined, errorName(o.error));
      assert.equal(o.answer?.kind, "answer");
      assert.equal(settlements(o).length, 1);
      assert.equal(runnings(o).length, 0, "an answer to a revise clarification is not plan approval");
      const runId = api.states[0]!.runId;
      const question = api.states.find(s => s.body.status === "awaiting_input")!.body.open_question_id;
      const input = api.inputRows(runId).find(r => r.kind === "revise_plan")!;
      const body = settlements(o)[0]!;
      assert.equal(body.claim_generation, 1);
      assert.equal(body.clarification_settlement?.question_id, question);
      assert.equal(body.clarification_settlement?.revision_input_id, input.id);
      assert.equal(typeof body.clarification_settlement?.gate_revision, "number");
      assert.equal(api.states.filter(s => s.body.status === "awaiting_input").length, 1, "one question was asked");
    });
  }

  for (const kind of ["prompt", "ci_fix"] as const) {
    for (const ack of ["applied_false", "conflict", "no_disposition"] as const) {
      for (const ask of ["user", "missing"] as const) {
        it(`${kind} + ${ask} refuses a ${ack} ACK: stops, no answer, no running follows`, async t => {
          const o = await drive(t, { ack, ask, claim: { kind } });
          assert.equal(o.answer, undefined);
          assert.match(errorName(o.error), /clarification revision settlement was not confirmed/);
          assert.equal(settlements(o).length, 1);
          assert.equal(runnings(o).length, 0, "a refusal never falls back to running");
          await new Promise(r => setTimeout(r, 50));
          const parked = api.states.findIndex(q => q.body.status === "awaiting_input");
          assert.equal(api.states.slice(parked).filter(s => s.body.status === "running").length, 0,
            "no unqualified running is reported later either");
        });
      }
    }
  }
});

describe("#3985 positive control: an already-approved implementation keeps the ordinary running", () => {
  for (const kind of ["issue", "prompt", "ci_fix"] as const) {
    it(`${kind}: revise consumed by an approved later presentation, the implementation question reports running only`, async t => {
      const o = await drive(t, {
        claim: { kind },
        approveSecondGate: true,
        beforeAsk: async (ctx, settles) => {
          const verdict = await ctx.gatePlan!("# Plan B", undefined, undefined, settles);
          assert.equal(verdict.kind, "approve");
        },
      });
      assert.equal(o.error, undefined, errorName(o.error));
      assert.equal(o.answer?.kind, "answer");
      // A non-confirming caller reports running fire-and-forget, so wait for it.
      assert.ok(await until(() => api.states.some(s => s.body.status === "running"
        && api.states.findIndex(q => q.body.status === "awaiting_input") >= 0
        && api.states.indexOf(s) > api.states.findIndex(q => q.body.status === "awaiting_input")), 4_000),
      "the ordinary running report was sent after the question");
      const sent = api.states.slice(api.states.findIndex(s => s.body.status === "awaiting_input")).map(s => s.body);
      assert.equal(sent.filter(s => s.status === "clarification_settled").length, 0, "no settlement without a pending revise");
      assert.equal(api.states.filter(s => s.body.status === "clarification_settled").length, 0);
      assert.equal(sent.filter(s => s.status === "running").length >= 1, true);
    });
  }
});

describe("#3985 clarification settlement and the worker-memory phase hold", () => {
  type Mem = NonNullable<ReturnType<NonNullable<RunContext["capturedMemory"]>>>;
  type MemoryProof = { workerMemory?: { phaseToken?: unknown; revisionContext?: unknown; ended?: boolean; running?: boolean } };
  interface MemoryOutcome {
    atQuestion?: boolean;
    atRevise?: boolean;
    afterAnswer?: { admits: boolean; token: unknown; revisionContext: unknown; context: unknown; ended: boolean | undefined; running: boolean | undefined };
    error?: unknown;
    phases: string[];
    other?: object;
  }

  async function memoryDrive(
    t: { after: (fn: () => Promise<void>) => void },
    ack: Ack,
    during?: (ctx: RunContext, mem: Mem, outcome: MemoryOutcome) => Promise<void>,
  ): Promise<MemoryOutcome> {
    newApi();
    api.memoryRegisterNonce = "settlement-memory-registration";
    api.setRegisterProtocolFeatures(["worker_memory_pressure_v1", "claim_generation_fence", "gate_revision_v1", FEATURE]);
    await client.register("memory-fixture", "base");
    client.protocolFeatures = [...client.protocolFeatures, "gate_revision_v1", FEATURE];
    scriptSettlement(ack);
    const claim: ClaimResponse = freshClaim(7, {
      ...requiredGated, memory_episode: 3, memory_policy: { version: 1, max_interventions: 2 }, memory_intervention_count: 0,
    });
    claim.secrets = { ...claim.secrets, codex: { auth_mode: "api_key", capability: "fixture-capability", access_token: "fixture-access" } };
    api.setOwnershipStatus(claim.run_id, "running", 7);
    const id = claim.run_id;
    let sampleId = 0;
    const memory = new WorkerMemoryRuntime({ enabled: true, reserveBytes: 100, hysteresisBytes: 100, sampleMs: 2,
      responseBudgetMs: 200, rearmMs: 10, maxInterventions: 2 }, client, nullLogger(), {
      reader: { sample: () => {
        const time = performance.now();
        return { available: true, sampleId: ++sampleId, startedAtMs: time, completedAtMs: time, currentBytes: 0, limitBytes: 1000,
          stat: { anon: 1, shmem: 0, slab_unreclaimable: 0, unevictable: 0, inactive_file: 0 },
          pressure: { some: { avg10: 0, avg60: 0, avg300: 0, total: 0 }, full: { avg10: 0, avg60: 0, avg300: 0, total: 0 } } };
      } },
    });
    const registry = new ActiveRunRegistry();
    const outcome: MemoryOutcome = { phases: [] };
    const realSetPhase = registry.setPhase.bind(registry);
    registry.setPhase = (runId, phase) => { outcome.phases.push(phase); realSetPhase(runId, phase); };
    let gates = 0;
    let finished = false;
    api.onState(id, body => {
      if (body.status === "awaiting_approval" && ++gates === 1) send(id, row("revise_plan", "clarify before revising"));
      if (body.status === "awaiting_input" && body.open_question_id) {
        const mem = current;
        if (mem) outcome.atQuestion = mem.commands.phaseAdmits(mem.context);
        send(id, row("answer", JSON.stringify({ question_id: body.open_question_id, answers: ["continue"] })));
      }
    });
    let current: Mem | undefined;
    const executor: Executor = {
      async run(ctx) {
        const mem = ctx.capturedMemory!()!;
        current = mem;
        try {
          const verdict = await ctx.gatePlan!("# Plan A");
          assert.equal(verdict.kind, "revise");
          outcome.atRevise = mem.commands.phaseAdmits(mem.context);
          await during?.(ctx, mem, outcome);
          const proof = mem.flight as unknown as MemoryProof;
          const finish = () => {
            outcome.afterAnswer = { admits: mem.commands.phaseAdmits(mem.context), token: proof.workerMemory?.phaseToken,
              revisionContext: proof.workerMemory?.revisionContext, context: mem.context, ended: proof.workerMemory?.ended,
              running: proof.workerMemory?.running };
          };
          if (!during) {
            try { await ctx.askUser!([{ question: "Continue planning?", header: "Plan" }]); finish(); }
            catch (error) { outcome.error = error; finish(); }
          }
        } catch (error) { outcome.error ??= error; }
        finished = true;
        throw new Error("fixture reached its end");
      },
    };
    const done = runner(executor, fakeGitlab().gitlab, undefined, { memoryRuntime: memory, activeRuns: registry,
      codexBoundaryDeadlineMs: 1000 }).execute(claim);
    t.after(async () => { send(id, row("cancel")); await done.catch(() => undefined); await memory.stop(); });
    await until(() => finished, 10_000);
    return outcome;
  }

  it("an applied settlement releases exactly the captured hold, records the revision context and shows awaiting_approval", async t => {
    const o = await memoryDrive(t, "applied");
    assert.equal(o.error, undefined, errorName(o.error));
    assert.equal(o.atRevise, true, "the human revise released the gate hold");
    assert.equal(o.atQuestion, false, "the question park holds the phase");
    const after = o.afterAnswer!;
    assert.equal(after.admits, true, "the captured token was released");
    assert.equal(after.token, undefined);
    assert.equal(after.revisionContext, after.context);
    assert.equal(after.ended, false, "memory is not retired");
    assert.equal(after.running, false);
    assert.equal(o.phases.at(-1), "awaiting_approval");
  });

  for (const ack of ["applied_false", "no_disposition", "other_status"] as const) {
    it(`a ${ack} settlement ACK keeps the hold, releases nothing and records no context`, async t => {
      const o = await memoryDrive(t, ack);
      assert.match(errorName(o.error), /clarification revision settlement was not confirmed/);
      const after = o.afterAnswer!;
      assert.equal(after.admits, false, "the question hold is retained");
      assert.notEqual(after.token, undefined);
      assert.equal(after.revisionContext, undefined);
      assert.notEqual(o.phases.at(-1), "awaiting_approval", o.phases.join(","));
    });
  }

  it("an intervening report that advances the state sequence before the ACK blocks the release and the context", async t => {
    const release = (() => { let resolve!: () => void; const promise = new Promise<void>(r => { resolve = r; }); return { promise, resolve }; })();
    const o = await memoryDrive(t, "applied", async (ctx, mem, outcome) => {
      api.afterPersistState(api.states[0]!.runId, b => b.status === "clarification_settled", release.promise);
      const asking = ctx.askUser!([{ question: "Continue planning?", header: "Plan" }]).then(() => undefined, (e: unknown) => { outcome.error = e; });
      await until(() => api.states.some(s => s.body.status === "clarification_settled"), 8_000);
      await ctx.reportIteration!(1);
      release.resolve();
      await asking;
      const proof = mem.flight as unknown as MemoryProof;
      outcome.afterAnswer = { admits: mem.commands.phaseAdmits(mem.context), token: proof.workerMemory?.phaseToken,
        revisionContext: proof.workerMemory?.revisionContext, context: mem.context, ended: proof.workerMemory?.ended,
        running: proof.workerMemory?.running };
    });
    const after = o.afterAnswer!;
    assert.equal(after.revisionContext, undefined, "a newer report moved the phase on");
    assert.equal(o.phases.includes("awaiting_approval") && o.phases.at(-1) === "awaiting_approval", false, o.phases.join(","));
  });

  it("a different phase token installed before the ACK is not cleared by the captured token's release", async t => {
    const release = (() => { let resolve!: () => void; const promise = new Promise<void>(r => { resolve = r; }); return { promise, resolve }; })();
    const other = Object.freeze({ context: undefined as unknown, identity: "newer-token" });
    const o = await memoryDrive(t, "applied", async (ctx, mem, outcome) => {
      api.afterPersistState(api.states[0]!.runId, b => b.status === "clarification_settled", release.promise);
      const asking = ctx.askUser!([{ question: "Continue planning?", header: "Plan" }]).then(() => undefined, (e: unknown) => { outcome.error = e; });
      await until(() => api.states.some(s => s.body.status === "clarification_settled"), 8_000);
      (mem.flight as unknown as MemoryProof).workerMemory!.phaseToken = other;
      release.resolve();
      await asking;
      const proof = mem.flight as unknown as MemoryProof;
      outcome.afterAnswer = { admits: mem.commands.phaseAdmits(mem.context), token: proof.workerMemory?.phaseToken,
        revisionContext: proof.workerMemory?.revisionContext, context: mem.context, ended: proof.workerMemory?.ended,
        running: proof.workerMemory?.running };
    });
    assert.equal(o.error, undefined, errorName(o.error));
    assert.equal(o.afterAnswer!.admits, true, "the captured token was released");
    assert.equal(o.afterAnswer!.token, other, "the newer phase token survives");
  });
});

import assert from "node:assert/strict";
import { test } from "node:test";
import { checkPlan } from "../src/plan-cross-check-gate.js";
import { RequestError, type PlanCrossCheckCandidate, type PlanCrossCheckResponse, type WorkerClient } from "../src/client.js";
import { MessageBatcher, type CandidateTransportReservation } from "../src/batcher.js";
import { SteeringChannel } from "../src/steering.js";
import { nullLogger, withReceipts } from "./helpers.js";
import type { UserInput } from "../src/protocol.js";

const candidate: PlanCrossCheckCandidate = { plan_md: "plan", milestones: [], required_capabilities: [],
  required_tools: [], size_class: "s", base_commit: "b".repeat(40), planning_diff: "" };
type Snapshot = Extract<PlanCrossCheckResponse, { result: "candidate" }>;
function snapshot(overrides: Partial<Snapshot> = {}): Snapshot {
  return { result: "candidate", round: 2, candidate_generation: 3, candidate_digest: "a".repeat(64),
    candidate, checker_run_id: "00000000-0000-0000-0000-000000000001",
    deadline_at: new Date(Date.now() + 60_000).toISOString(), lead_last_seq: 0,
    verdict: "revise", reason_class: "revise", automatic_revision_limit: 4, automatic_rounds_enabled: true,
    findings: { summary: "check scope", items: [{ file: "store.ts", severity: "warning",
      summary: "race", rationale: "verify lock" }] }, ...overrides };
}
function harness(initial = snapshot(), options: {
  release?: boolean; cancelOnRelease?: boolean; preparation?: "transient" | "permanent";
  confirmed?: boolean; loss?: boolean; inactive?: boolean;
  submitError?: Error; polls?: PlanCrossCheckResponse[];
} = {}) {
  const events: string[] = [];
  const owner = new AbortController();
  const proofs: PlanCrossCheckResponse[] = [];
  const reservation: CandidateTransportReservation = {
    prepare: async () => { events.push("prepare"); return options.preparation
      ? { prepared: false, tail: 0, permanent: options.preparation === "permanent", reason: "usage_unconfirmed" }
      : { prepared: true, tail: 0 }; },
    markSubmitted: () => { events.push("submitted"); },
    cancel: () => { events.push("cancel"); },
    release: (proof) => {
      events.push("release"); proofs.push(proof);
      if (options.cancelOnRelease) owner.abort(new Error("owner cancelled"));
      return options.release ?? true;
    },
    releaseAppliedGate: () => { throw new Error("automatic path used applied human gate"); },
  };
  const batcher = { reserveCandidateTransport: () => { events.push("reserve"); return reservation; },
    usage: { drainConfirmed: async () => { events.push("usage"); return options.confirmed ?? true; },
      hasUnconfirmedLoss: options.loss ?? false, inactive: options.inactive ?? false } } as unknown as MessageBatcher;
  const submits: unknown[][] = [], reads: unknown[][] = [];
  const client = {
    submitPlanCrossCheck: async (...args: unknown[]) => {
      submits.push(args); events.push("submit");
      if (options.submitError) throw options.submitError;
      return initial;
    },
    planCrossCheckStatus: async (...args: unknown[]) => {
      reads.push(args); events.push("poll");
      const polled = options.polls?.shift();
      if (!polled) throw new Error("unexpected poll");
      return polled;
    },
  } as unknown as WorkerClient;
  const run = (extra: { round?: number; allowAutomaticRevision?: boolean } = {}) => checkPlan({
    client, runId: "run", generation: 3, candidate, batcher, signal: owner.signal,
    timing: { pollMs: 0, backoffMs: [0, 0] }, round: 2, allowAutomaticRevision: true, ...extra,
  });
  return { run, events, proofs, submits, reads, owner, reservation };
}

test("round two polls its own identity and releases the decided proof after usage before revise", async () => {
  const pending = snapshot({ verdict: "pending", reason_class: "", findings: null });
  const decided = { ...pending, verdict: "revise", reason_class: "revise",
    findings: snapshot().findings } satisfies Snapshot;
  const h = harness(pending, { polls: [decided] });
  const result = await h.run();
  assert.equal(result.kind, "revise");
  if (result.kind !== "revise") return;
  assert.deepEqual(result.response.findings, snapshot().findings);
  assert.equal(result.response.round, 2);
  assert.equal(result.response.automatic_revision_limit, 4);
  assert.deepEqual(h.proofs, [decided]);
  assert.equal(h.events.at(-1), "release");
  assert.deepEqual(h.events.slice(-3), ["prepare", "usage", "release"]);
  assert.equal(h.submits[0]![4], 2);
  assert.ok(h.submits[0]![3] instanceof AbortSignal);
  assert.equal(h.reads[0]![2], 2);
  assert.ok(h.reads[0]![3] instanceof AbortSignal);
  assert.equal("fields" in result, false);
});

test("decided revise received after deadline retains its server decision", async () => {
  const decided = snapshot({ deadline_at: new Date(Date.now() - 1_000).toISOString() });
  const h = harness(decided, { submitError: new Error("lost ACK"), polls: [decided] });
  assert.equal((await h.run()).kind, "revise");
  assert.deepEqual(h.proofs, [decided]);
});

test("ambiguous submit reads requested round and uses settled revise proof", async () => {
  const decided = snapshot();
  const h = harness(decided, { submitError: new Error("delivery timeout"), polls: [decided] });
  assert.equal((await h.run()).kind, "revise");
  assert.equal(h.reads[0]![2], 2);
  assert.deepEqual(h.proofs, [decided]);
});

for (const [name, patch] of Object.entries({
  round: { round: 3 }, digest: { candidate_digest: "c".repeat(64) },
  child: { checker_run_id: "00000000-0000-0000-0000-000000000002" },
  deadline: { deadline_at: new Date(Date.now() + 50_000).toISOString() },
  candidate: { candidate: { ...candidate, plan_md: "changed" } },
  budget: { automatic_revision_limit: 3 }, enabled: { automatic_rounds_enabled: false },
})) {
  test(`poll identity mutation (${name}) never returns automatic revise`, async () => {
    const pending = snapshot({ verdict: "pending", reason_class: "", findings: null });
    const h = harness(pending, { polls: [{ ...pending, verdict: "revise", reason_class: "revise", ...patch }] });
    const result = await h.run();
    assert.equal(result.kind, "human");
    if (result.kind === "human") assert.equal(result.fields.plan_cross_check_gate_reason, "checker_failed");
    assert.equal(h.proofs.length, 0);
  });
}

for (const [round, limit] of [[1, 0], [2, 1], [5, 4]]) {
  test(`enabled exhausted budget round ${round} limit ${limit} stays human`, async () => {
    const h = harness(snapshot({ round, automatic_revision_limit: limit }));
    const result = await h.run({ round });
    assert.equal(result.kind, "human");
    if (result.kind === "human") assert.equal(result.fields.plan_cross_check_gate_reason, "revisions_exhausted");
    assert.equal(h.proofs.length, 0);
  });
}

test("legacy-disabled and non-opted callers retain human revise with held reservation", async () => {
  for (const [enabled, opted] of [[false, true], [true, false]]) {
    const h = harness(snapshot({ round: 1, automatic_revision_limit: enabled ? 4 : 0,
      automatic_rounds_enabled: enabled }));
    const result = await h.run({ round: 1, allowAutomaticRevision: opted });
    assert.equal(result.kind, "human");
    if (result.kind === "human") {
      assert.equal(result.fields.plan_cross_check_gate_reason, "revise");
      assert.equal(result.reservation, h.reservation);
    }
    assert.equal(h.proofs.length, 0);
  }
});

test("server 409 revisions_exhausted retains human settlement", async () => {
  const h = harness(snapshot(), { submitError: new RequestError("POST", "/cross-checks", 409, '{"reason":"revisions_exhausted"}') });
  const result = await h.run();
  assert.equal(result.kind, "human");
  if (result.kind === "human") assert.equal(result.fields.plan_cross_check_gate_reason, "revisions_exhausted");
  assert.equal(h.proofs.length, 0);
});

test("false release is terminal and owner cancellation after release prevents revise", async () => {
  const failed = harness(snapshot(), { release: false });
  await assert.rejects(failed.run(), /revision settlement receipts unavailable/);
  assert.equal(failed.events.at(-1), "cancel");
  const cancelled = harness(snapshot(), { cancelOnRelease: true });
  await assert.rejects(cancelled.run(), /owner cancelled/);
  assert.equal(cancelled.proofs.length, 1);
});

test("D7 loss, inactive usage and permanent preparation errors are terminal", async () => {
  for (const options of [{ loss: true }, { inactive: true }, { preparation: "permanent" as const }]) {
    const h = harness(snapshot(), options);
    await assert.rejects(h.run(), /irrecoverably lost/);
    assert.equal(h.proofs.length, 0);
    assert.equal(h.events.at(-1), "cancel");
  }
});

test("transient preparation remains a held human gate without submitting", async () => {
  const h = harness(snapshot(), { preparation: "transient" });
  const result = await h.run();
  assert.equal(result.kind, "human");
  if (result.kind === "human") {
    assert.equal(result.reservation, h.reservation);
    assert.ok(result.completePreparation);
    await assert.rejects(result.completePreparation(h.owner.signal), /ACKs unrecoverable/);
  }
  assert.equal(h.submits.length, 0);
  assert.equal(h.proofs.length, 0);
});

test("unconfirmed usage cannot reach automatic release", async () => {
  const h = harness(snapshot(), { confirmed: false });
  assert.equal((await h.run()).kind, "human");
  assert.equal(h.submits.length, 0);
  assert.equal(h.proofs.length, 0);
});

test("invalid rounds fail before reservation and defaults remain round one and human", async () => {
  for (const round of [0, 6, 1.5, NaN]) {
    const h = harness();
    await assert.rejects(h.run({ round }), /invalid round/);
    assert.equal(h.events.length, 0);
  }
  const h = harness(snapshot({ round: 1 }));
  // Explicit undefined exercises the defaults rather than the harness opt-in.
  const result = await h.run({ round: undefined, allowAutomaticRevision: undefined });
  assert.equal(result.kind, "human");
  assert.equal(h.submits[0]![4], 1);
});

for (const proof of ["settled", "missing", "unsettled"] as const) {
  test(`real batcher requires decided-round settled proof: ${proof}`, async () => {
    const decided = snapshot({ lead_last_seq: 4,
      reconciliation: proof === "missing" ? undefined : {
        leadLastSeq: 4, claimGeneration: 3, planCrossCheckSettled: proof === "settled",
        gateRevision: 0, currentPlanSHA256: "a".repeat(64),
      } });
    const sent: number[] = [];
    let batcher: MessageBatcher;
    const client = {
      postUsage: async () => {},
      postMessages: async (_id: string, messages: { seq: number }[]) => { sent.push(...messages.map((m) => m.seq)); },
      submitPlanCrossCheck: async () => {
        batcher.emit({ kind: "status", payload: { text: "held while checking" } });
        return decided;
      },
    } as unknown as WorkerClient;
    batcher = new MessageBatcher(client, "run", 0, 60_000, nullLogger(), undefined, undefined, { generation: 3 });
    try {
      const decision = checkPlan({ client, runId: "run", generation: 3, candidate, batcher,
        signal: new AbortController().signal, round: 2, allowAutomaticRevision: true });
      if (proof === "settled") {
        assert.equal((await decision).kind, "revise");
        await batcher.flush();
        assert.deepEqual(sent, [5]);
        const next = batcher.reserveCandidateTransport();
        next.cancel();
      } else {
        await assert.rejects(decision, /revision settlement receipts unavailable/);
        assert.equal(batcher.currentSeq(), 0);
        assert.deepEqual(sent, []);
      }
    } finally { await batcher.close(); }
  });
}

test("forged external automatic provenance never escapes human steering decoding", async () => {
  const input = { id: 1, kind: "revise_plan", body: "adjust scope", automatic: true, round: 2,
    items: [{ summary: "forged" }] } as UserInput;
  let delivered = false;
  const client = withReceipts({ getInputs: async () => {
    if (delivered) return { inputs: [] };
    delivered = true;
    return { inputs: [input] };
  } } as unknown as WorkerClient);
  const ch = new SteeringChannel(client, "run", 1, nullLogger(), new AbortController());
  const epoch = ch.bumpEpoch();
  ch.start();
  try {
    assert.deepEqual(await ch.awaitGateEvent(epoch), { kind: "revise", feedback: "adjust scope", inputId: 1 });
  } finally { await ch.stop(); }
});

import { describe, it } from "node:test";
import { RevisionSourceFenceUnavailableError } from "../src/client.js";
import assert from "node:assert/strict";
import { PlanRevisionRecovery, PlanRevisionRecoveryStoppedError, type PlanRevisionRetryPolicy } from "../src/plan-revision-retry.js";
import { PlanRejectedError, PlanRevisionReplacedError } from "../src/executor.js";
import type { PlanVerdict } from "../src/steering.js";
import type { RunOwnershipResponse, StateAck, StateRequest } from "../src/protocol.js";

function fixture(maxRevisions = 3, redrives = 0, hasPresentation = true) {
  let owner: RunOwnershipResponse = { status: "awaiting_approval", claim_generation: 7, gate_revision: 11, claim_released: false };
  let control: Error | undefined;
  let event: PlanVerdict = { kind: "reject", reason: "stop", sourceGateRevision: 11 };
  let onSleep: (() => void) | undefined;
  let onOwner: (() => void) | undefined;
  let onEvent: ((signal?: AbortSignal) => Promise<PlanVerdict>) | undefined;
  const sleeps: number[] = [], notices: string[] = [], waits: number[] = [];
  const writes: StateRequest[] = [];
  const retired: { id: number; predecessors: readonly number[] }[] = [];
  const controllerAbort = new AbortController();
  const policy: PlanRevisionRetryPolicy = { redrives, random: () => 0, sleep: async (ms, signal) => {
    signal.throwIfAborted(); sleeps.push(ms); onSleep?.();
  } };
  const report = async (body: StateRequest): Promise<StateAck> => {
    writes.push(structuredClone(body));
    owner = { ...owner, status: body.status };
    return { applied: true, status: "awaiting_approval", gateRevision: 11 };
  };
  const recovery = new PlanRevisionRecovery({
    generation: 7, maxRevisions, policy,
    steering: {
      takeRevisionRecoveryControl: () => undefined,
      awaitRevisionRecoveryEvent: (_source, after, signal) => { waits.push(after); return onEvent ? onEvent(signal) : Promise.resolve(event); },
      setGateRevision: (r) => assert.equal(r, 11),
      retirePersistedRevision: (id, predecessors) => { retired.push({ id, predecessors }); },
    },
    ownership: async () => { onOwner?.(); return structuredClone(owner); },
    signal: () => controllerAbort.signal,
    checkControl: () => { if (control) throw control; controllerAbort.signal.throwIfAborted(); },
    notice: async (text) => { notices.push(text); },
  });
  const source: StateRequest = { status: "awaiting_approval", presentation_id: "00000000-0000-4000-8000-000000000001",
    plan_md: "# Source", milestones: [{ id: "one", title: "Original" }], plan_changed_files: [] };
  if (!hasPresentation) delete source.presentation_id;
  recovery.seed(source, 11, report);
  recovery.accept({ kind: "revise", inputId: 101, sourceGateRevision: 11, feedback: "I" });
  return { recovery, source, writes, notices, sleeps, waits, retired, report,
    setOwner: (next: RunOwnershipResponse) => { owner = next; },
    setEvent: (v: PlanVerdict) => { event = v; },
    onSleep: (fn: () => void) => { onSleep = fn; },
    onOwner: (fn: () => void) => { onOwner = fn; },
    onEvent: (fn: (signal?: AbortSignal) => Promise<PlanVerdict>) => { onEvent = fn; },
    cancel: () => { control = new Error("run cancelled"); },
    abort: () => { controllerAbort.abort(new Error("worker shutdown")); },
  };
}

describe("live-flight plan revision recovery", () => {
  it("allows three re-drives across continuation stages, then expires without another start", async () => {
    const f = fixture(3, 3);
    await f.recovery.beforeTurn();
    for (let i = 0; i < 3; i++) {
      const before = f.sleeps.reduce((a, b) => a + b, 0);
      await f.recovery.retry(new Error("provider exhausted"));
      assert.equal(f.sleeps.reduce((a, b) => a + b, 0) - before, [5000, 10000, 20000][i]);
      await f.recovery.beforeTurn();
      await f.recovery.beforeTurn(); // an answered clarification does not replenish the allowance
    }
    await assert.rejects(f.recovery.retry(new Error("provider exhausted")), PlanRejectedError);
    assert.equal(f.writes.length, 0, "already-held source needs no restoration");
    assert.match(f.notices.at(-1)!, /generation 7, source gate 11, pending revision 101/);
    assert.match(f.notices.at(-1)!, /Feedback remains pending/);
    assert.match(f.notices.at(-1)!, /live gate remains busy/);
    assert.deepEqual(f.retired, []);
  });

  it("restores the original presentation after clarification and defers I supersession until J persistence", async () => {
    const f = fixture();
    f.setOwner({ status: "running", claim_generation: 7, gate_revision: 11, claim_released: false });
    f.setEvent({ kind: "revise", inputId: 102, sourceGateRevision: 11, feedback: "J" });
    await assert.rejects(f.recovery.retry(new Error("provider exhausted")), PlanRevisionReplacedError);
    assert.equal(f.writes.length, 1);
    assert.deepEqual(f.writes[0], { ...f.source, expected_gate_revision: 11 });
    assert.deepEqual(f.retired, [], "taking J does not apply either feedback row");
    assert.deepEqual(f.recovery.fields(102), { expected_gate_revision: 11, revision_input_id: 102, superseded_revision_input_ids: [101] });
    const request = { ...f.source, plan_md: "# J", ...f.recovery.fields(102) };
    const ack: StateAck = { applied: true, status: "awaiting_approval", gateRevision: 12,
      revisionInputId: 102, supersededRevisionInputIds: [101] };
    await f.recovery.publish(request, async () => ack);
    assert.deepEqual(f.retired, [], "publication ACK is handled explicitly");
    f.recovery.published(request, ack, f.report);
    assert.deepEqual(f.retired, [{ id: 102, predecessors: [101] }]);
    assert.match(f.notices.at(-1)!, /superseded, not successfully answered/);
  });

  it("an id-less source still fences ownership before the original provider turn", async () => {
    const f = fixture(3, 3, false);
    f.setOwner({ status: "paused", claim_generation: 7, gate_revision: 11, claim_released: false });
    await assert.rejects(f.recovery.beforeTurn(), PlanRevisionRecoveryStoppedError);
    assert.deepEqual(f.writes, []);
  });

  it("id-less source exhaustion keeps compatibility without an unfenced restore or re-drive", async () => {
    const f = fixture(3, 3, false);
    await f.recovery.beforeTurn();
    const cause = new Error("provider exhausted");
    await assert.rejects(f.recovery.retry(cause), error => error === cause);
    assert.deepEqual(f.writes, []);
    assert.deepEqual(f.sleeps, []);
    assert.deepEqual(f.retired, []);
  });

  it("an expired gate is restored again if same-flight status returns to running", async () => {
    const f = fixture();
    let events = 0;
    f.onEvent(signal => {
      if (++events === 1) {
        f.setOwner({ status: "running", claim_generation: 7, gate_revision: 11, claim_released: false });
        return new Promise((_resolve, reject) => signal?.addEventListener("abort", () => reject(signal.reason), { once: true }));
      }
      return Promise.resolve({ kind: "reject", reason: "stop", sourceGateRevision: 11 });
    });
    await assert.rejects(f.recovery.retry(new Error("429")), PlanRejectedError);
    assert.deepEqual(f.writes, [{ ...f.source, expected_gate_revision: 11 }]);
    assert.deepEqual(f.retired, []);
  });

  it("lost source-fence support stops publication without an unfenced send", async () => {
    const f = fixture();
    const request = { ...f.source, ...f.recovery.fields(101) };
    await assert.rejects(f.recovery.publish(request, async () => { throw new RevisionSourceFenceUnavailableError(); }),
      PlanRevisionRecoveryStoppedError);
    assert.deepEqual(f.sleeps, []);
    assert.deepEqual(f.retired, []);
  });

  it("holds an I-to-J-to-K chain pending and refuses duplicate driver acceptance", async () => {
    const f = fixture();
    f.setEvent({ kind: "revise", inputId: 102, sourceGateRevision: 11, feedback: "J" });
    await assert.rejects(f.recovery.retry(new Error("429")), PlanRevisionReplacedError);
    f.setEvent({ kind: "revise", inputId: 103, sourceGateRevision: 11, feedback: "K" });
    await assert.rejects(f.recovery.retry(new Error("429")), PlanRevisionReplacedError);
    assert.deepEqual(f.recovery.fields(103).superseded_revision_input_ids, [101, 102]);
    assert.deepEqual(f.retired, []);
    assert.throws(() => f.recovery.accept({ kind: "revise", inputId: 103, sourceGateRevision: 11, feedback: "K" }),
      PlanRevisionRecoveryStoppedError);
  });

  for (const [name, patch] of [
    ["paused", { status: "paused" }], ["cancelled", { status: "cancelled" }], ["terminal", { status: "failed" }],
    ["released", { claim_released: true }], ["generation", { claim_generation: 8 }], ["gate", { gate_revision: 12 }],
  ] as const) {
    it("does not write or start a turn after " + name + " changes", async () => {
      const f = fixture(3, 3);
      f.setOwner({ status: "awaiting_approval", claim_generation: 7, gate_revision: 11, claim_released: false, ...patch });
      await assert.rejects(f.recovery.beforeTurn(), PlanRevisionRecoveryStoppedError);
      assert.deepEqual(f.retired, []);
      assert.deepEqual(f.writes, []);
    });
  }

  it("a stale feedback or wrong publication target cannot remove the source fence", () => {
    const f = fixture();
    assert.throws(() => f.recovery.accept({ kind: "revise", inputId: 102, sourceGateRevision: 10, feedback: "old" }),
      PlanRevisionRecoveryStoppedError);
    assert.throws(() => f.recovery.fields(102), PlanRevisionRecoveryStoppedError);
    assert.deepEqual(f.retired, []);
  });

  it("unknown ownership grants no start, write or allowance reset", async () => {
    const f = fixture(3, 3);
    f.setOwner({ status: "awaiting_approval", claim_generation: 7, gate_revision: 11 }); // release evidence absent
    let reads = 0;
    f.onOwner(() => { reads++; });
    f.onSleep(() => { if (reads === 3) f.cancel(); });
    await assert.rejects(f.recovery.beforeTurn(), /run cancelled/);
    assert.equal(reads, 3);
    assert.equal(f.writes.length, 0);
    assert.equal(f.notices.length, 0, "no charged re-drive");
  });

  it("a cancel during backoff stops before a re-drive is charged", async () => {
    const f = fixture(3, 3);
    f.onSleep(() => f.cancel());
    await assert.rejects(f.recovery.retry(new Error("429")), /run cancelled/);
    assert.equal(f.sleeps.length, 1);
    assert.equal(f.notices.length, 0);
    assert.deepEqual(f.retired, []);
  });

  it("one cancel after expiry completes without a replacement or receipt settlement", async () => {
    const f = fixture();
    f.setEvent({ kind: "cancel" });
    await assert.rejects(f.recovery.retry(new Error("429")), /run cancelled/);
    assert.equal(f.waits.length, 1);
    assert.deepEqual(f.retired, []);
  });

  it("cap exhaustion leaves only cancel/reject and the new-run path", async () => {
    const f = fixture(1);
    await assert.rejects(f.recovery.retry(new Error("429")), PlanRejectedError);
    assert.deepEqual(f.waits, [Number.MAX_SAFE_INTEGER]);
    assert.match(f.notices.at(-1)!, /PLAN_MAX_REVISIONS is exhausted: no further revise\/retry/);
    assert.match(f.notices.at(-1)!, /Cancel and source-bound reject remain available/);
    assert.match(f.notices.at(-1)!, /Approval and follow-up cannot resume/);
    assert.match(f.notices.at(-1)!, /dispatch a new run/);
  });

  it("replays a frozen successor after a lost ACK without inferring settlement from the advanced gate", async () => {
    const f = fixture();
    const request = { ...f.source, plan_md: "# New", ...f.recovery.fields(101) };
    const bodies: StateRequest[] = [];
    const ack: StateAck = { applied: true, status: "awaiting_approval", gateRevision: 12,
      revisionInputId: 101, supersededRevisionInputIds: [] };
    const result = await f.recovery.publish(request, async (body) => {
      bodies.push(structuredClone(body));
      if (bodies.length === 1) {
        f.setOwner({ status: "awaiting_approval", claim_generation: 7, gate_revision: 12, claim_released: false });
        throw new Error("ACK lost");
      }
      return ack;
    });
    assert.deepEqual(bodies, [request, request]);
    assert.deepEqual(f.retired, []);
    f.recovery.published(request, result, f.report);
    assert.deepEqual(f.retired, [{ id: 101, predecessors: [] }]);
  });

  it("a refused successor or unknown receipt cannot settle predecessors", async () => {
    const f = fixture();
    const request = { ...f.source, ...f.recovery.fields(101) };
    await assert.rejects(f.recovery.publish(request, async () => ({
      applied: false, status: "awaiting_approval", staleRevisionSource: true,
    })), PlanRevisionRecoveryStoppedError);
    assert.deepEqual(f.retired, []);
    f.onSleep(() => f.abort());
    await assert.rejects(f.recovery.publish(request, async () => ({
      applied: true, status: "awaiting_approval", gateRevision: 12,
    })), /worker shutdown/);
    assert.deepEqual(f.retired, []);
  });

  it("a reclaimed flight has a fresh allowance but no reconstructed predecessors", async () => {
    const old = fixture();
    old.setEvent({ kind: "revise", inputId: 102, sourceGateRevision: 11, feedback: "J" });
    await assert.rejects(old.recovery.retry(new Error("429")), PlanRevisionReplacedError);
    const reclaimed = fixture(3, 3);
    await reclaimed.recovery.retry(new Error("429"));
    await reclaimed.recovery.beforeTurn();
    assert.deepEqual(reclaimed.recovery.fields(101).superseded_revision_input_ids, []);
    assert.equal(reclaimed.notices.filter((text) => /re-driving/.test(text)).length, 1);
  });
});

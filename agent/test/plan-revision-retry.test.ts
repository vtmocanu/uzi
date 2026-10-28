import { describe, it } from "node:test";
import { RequestError, RevisionSourceFenceUnavailableError } from "../src/client.js";
import { TransientRecoveryError } from "../src/sdk-executor.js";
import assert from "node:assert/strict";
import { PlanRevisionRecovery, PlanRevisionRecoveryStoppedError, type PlanRevisionRetryPolicy } from "../src/plan-revision-retry.js";
import { PlanRejectedError, PlanRevisionReplacedError } from "../src/executor.js";
import type { PlanVerdict } from "../src/steering.js";
import type { RunOwnershipResponse, StateAck, StateRequest } from "../src/protocol.js";

function fixture(redrives = 0, hasPresentation = true) {
  let owner: RunOwnershipResponse = { status: "awaiting_approval", claim_generation: 7, gate_revision: 11, claim_released: false };
  let control: Error | undefined;
  let event: PlanVerdict = { kind: "reject", reason: "stop", sourceGateRevision: 11 };
  let onSleep: ((ms: number, signal: AbortSignal) => void | Promise<void>) | undefined;
  let onOwner: (() => void) | undefined;
  let onEvent: ((signal?: AbortSignal) => Promise<PlanVerdict>) | undefined;
  const sleeps: number[] = [], notices: string[] = [], waits: number[] = [];
  const writes: StateRequest[] = [];
  const retired: { id: number; predecessors: readonly number[] }[] = [];
  const controllerAbort = new AbortController();
  const policy: PlanRevisionRetryPolicy = { redrives, random: () => 0, sleep: async (ms, signal) => {
    signal.throwIfAborted(); sleeps.push(ms); await onSleep?.(ms, signal);
  } };
  const report = async (body: StateRequest): Promise<StateAck> => {
    writes.push(structuredClone(body));
    owner = { ...owner, status: body.status };
    return { applied: true, status: "awaiting_approval", gateRevision: 11 };
  };
  const recovery = new PlanRevisionRecovery({
    generation: 7, policy,
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
    onSleep: (fn: (ms: number, signal: AbortSignal) => void | Promise<void>) => { onSleep = fn; },
    onOwner: (fn: () => void) => { onOwner = fn; },
    onEvent: (fn: (signal?: AbortSignal) => Promise<PlanVerdict>) => { onEvent = fn; },
    cancel: () => { control = new Error("run cancelled"); },
    abort: () => { controllerAbort.abort(new Error("worker shutdown")); },
  };
}

async function settledUntil(check: () => boolean): Promise<void> {
  for (let i = 0; i < 100; i++) {
    if (check()) return;
    await Promise.resolve();
  }
  assert.fail("recovery did not reach the expected wait");
}

describe("live-flight plan revision recovery", () => {
  it("3466 revalidates known-owned idle recovery every 30 seconds and aborts each losing event wait", async () => {
    const f = fixture();
    let reads = 0, eventAborts = 0;
    const ticks: { ms: number; resolve: () => void }[] = [];
    const observed: boolean[] = [];
    f.onOwner(() => { assert.notEqual(observed.at(-1), true); reads++; });
    f.onEvent(signal => new Promise((_resolve, reject) => {
      signal!.addEventListener("abort", () => { eventAborts++; reject(signal!.reason); }, { once: true });
    }));
    f.onSleep((ms, signal) => new Promise<void>((resolve, reject) => {
      ticks.push({ ms, resolve });
      signal.addEventListener("abort", () => reject(signal.reason), { once: true });
    }));
    const done = assert.rejects(f.recovery.retry(new Error("429"), waiting => observed.push(waiting)), /worker shutdown/);
    try {
      await settledUntil(() => ticks.length === 1);
      const initialReads = reads;
      assert.equal(ticks[0]!.ms, 30_000);
      await Promise.resolve();
      assert.equal(reads, initialReads, "no ownership polling before the idle timer fires");
      ticks[0]!.resolve();
      await settledUntil(() => ticks.length === 2);
      assert.equal(reads, initialReads + 1);
      assert.equal(ticks[1]!.ms, 30_000);
      assert.equal(eventAborts, 1, "the timer winner removes the old steering wait");
    } finally {
      f.abort();
      await done;
    }
    assert.equal(eventAborts, 2);
    assert.deepEqual(observed, [true, false, true, false]);
  });

  for (const outcome of ["revise", "reject", "cancel", "shutdown"] as const) {
    it(`3466 idle ${outcome} wakes immediately and cancels the losing timer`, async () => {
      const f = fixture();
      let deliver!: (verdict: PlanVerdict) => void;
      let timerAborts = 0, eventAborts = 0;
      const observed: boolean[] = [];
      f.onOwner(() => assert.notEqual(observed.at(-1), true));
      f.onEvent(signal => new Promise((resolve, reject) => {
        deliver = resolve;
        signal!.addEventListener("abort", () => { eventAborts++; reject(signal!.reason); }, { once: true });
      }));
      f.onSleep((ms, signal) => {
        assert.equal(ms, 30_000);
        return new Promise<void>((_resolve, reject) => {
          signal.addEventListener("abort", () => { timerAborts++; reject(signal.reason); }, { once: true });
        });
      });
      const done = assert.rejects(f.recovery.retry(new Error("429"), waiting => observed.push(waiting)),
        outcome === "revise" ? PlanRevisionReplacedError :
        outcome === "reject" ? PlanRejectedError :
        outcome === "cancel" ? /run cancelled/ : /worker shutdown/);
      await settledUntil(() => deliver !== undefined && f.sleeps.length === 1);
      if (outcome === "shutdown") f.abort();
      else deliver(outcome === "revise"
        ? { kind: "revise", inputId: 102, sourceGateRevision: 11, feedback: "J" }
        : outcome === "reject" ? { kind: "reject", sourceGateRevision: 11, reason: "stop" }
        : { kind: "cancel" });
      await done;
      assert.deepEqual(observed, [true, false]);
      assert.equal(timerAborts, 1);
      assert.equal(eventAborts, 1);
      assert.deepEqual(f.retired, []);
    });
  }

  it("accepts server-bound newer feedback after many local rounds and on a reclaimed flight", async () => {
    const f = fixture();
    for (let id = 102; id <= 110; id++) {
      f.setEvent({ kind: "revise", inputId: id, sourceGateRevision: 11, feedback: "accepted" });
      await assert.rejects(f.recovery.retry(new Error("429")), PlanRevisionReplacedError);
      assert.equal(f.waits.at(-1), id - 1);
      await f.recovery.beforeTurn();
    }
    assert.deepEqual(f.recovery.fields(110).superseded_revision_input_ids, [101, 102, 103, 104, 105, 106, 107, 108, 109]);
    const reclaimed = fixture();
    reclaimed.setEvent({ kind: "revise", inputId: 111, sourceGateRevision: 11, feedback: "accepted after reclaim" });
    await assert.rejects(reclaimed.recovery.retry(new Error("429")), PlanRevisionReplacedError);
    assert.deepEqual(reclaimed.recovery.fields(111).superseded_revision_input_ids, [101]);
  });

  for (const budget of [0, 125, 625]) it(`3466 silent backoff stops at remaining wall=${budget}`, async () => {
    const f = fixture(3);
    let remaining = budget;
    f.onSleep(ms => { remaining -= ms; });
    await f.recovery.retry(new Error("silent recovery"), undefined, () => remaining);
    assert.deepEqual(f.sleeps, budget === 0 ? [] : budget === 125 ? [125] : [250, 250, 125]);
    assert.equal(remaining, 0);
    assert.deepEqual(f.writes, []);
    assert.deepEqual(f.waits, []);
  });

  it("allows three re-drives across continuation stages, then expires without another start", async () => {
    const f = fixture(3);
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

  it("3466 zero allowance replacement readiness waits instead of admitting construction", async () => {
    const f = fixture(0);
    const cause = new TransientRecoveryError("silence", { idleStallKind: "zero_evidence" });
    const observed: boolean[] = [];
    const observer = (waiting: boolean) => { observed.push(waiting); };
    f.onOwner(() => assert.notEqual(observed.at(-1), true));
    f.setEvent({ kind: "revise", inputId: 102, sourceGateRevision: 11, feedback: "J" });
    await assert.rejects(f.recovery.retry(cause, observer), PlanRevisionReplacedError);
    f.setEvent({ kind: "reject", reason: "stop", sourceGateRevision: 11 });
    await assert.rejects(f.recovery.beforeTurn(observer), PlanRejectedError);
    assert.ok(f.notices.every(text => !text.includes("re-driving the failed turn")));
    assert.deepEqual(f.recovery.fields(102).superseded_revision_input_ids, [101]);
    assert.deepEqual(f.retired, []);
    assert.deepEqual(observed, [true, false, true, false]);
  });

  for (const providerOrigin of [false, true]) it(`3506 revision retry and exhaustion providerOrigin=${providerOrigin}`, async () => {
    const f = fixture(3);
    const cause = new TransientRecoveryError("silent revision", {
      providerOrigin, idleStallKind: "zero_evidence", originalIdle: new Error("codex run idle timeout"),
    });
    const reason = providerOrigin ? "silence after a provider-reported rate limit or transport retry" : "No model output within the idle timeout";
    await f.recovery.beforeTurn();
    for (let attempt = 1; attempt <= 3; attempt++) {
      const slept = f.sleeps.reduce((sum, ms) => sum + ms, 0);
      await f.recovery.retry(cause);
      assert.equal(f.sleeps.reduce((sum, ms) => sum + ms, 0) - slept, [5000, 10000, 20000][attempt - 1]);
      await f.recovery.beforeTurn();
      await f.recovery.beforeTurn();
      assert.equal(f.notices.at(-1),
        `${reason} during plan revision 101; re-driving the failed turn (${attempt}/3).`);
    }
    assert.equal(f.sleeps.reduce((sum, ms) => sum + ms, 0), 35000);
    await assert.rejects(f.recovery.retry(cause), PlanRejectedError);
    assert.ok(f.notices.at(-1)!.startsWith(`${reason}. Plan revision retry allowance exhausted`));
    assert.match(f.notices.at(-1)!, /Feedback remains pending/);
    assert.ok(f.notices.every(text => !text.includes("Provider transient")));
    assert.deepEqual(f.writes, []);
    assert.deepEqual(f.retired, []);
  });

  for (const silence of [false, true]) it(`3466 owner replacement charges first rebuild only for measured silence=${silence}`, async () => {
    const f = fixture(3);
    const cause = new TransientRecoveryError("exhausted revision", silence
      ? { idleStallKind: "zero_evidence", originalIdle: new Error("codex run idle timeout") }
      : { providerOrigin: true });
    for (let attempt = 0; attempt < 3; attempt++) {
      await f.recovery.retry(cause);
      await f.recovery.beforeTurn();
    }
    f.setEvent({ kind: "revise", inputId: 102, sourceGateRevision: 11, feedback: "J" });
    await assert.rejects(f.recovery.retry(cause), PlanRevisionReplacedError);
    const before = f.notices.length;
    const slept = [...f.sleeps];
    await f.recovery.beforeTurn();
    await f.recovery.beforeTurn();
    assert.equal(f.notices.length, before + Number(silence), "repeated admission probes charge once");
    if (silence) assert.match(f.notices.at(-1)!, /revision 102; re-driving the failed turn \(1\/3\)/);
    assert.equal(f.sleeps.filter(ms => ms !== 30000).reduce((sum, ms) => sum + ms, 0), 35000);
    assert.deepEqual(f.sleeps, slept, "replacement admission does not invoke retry backoff");
    for (let attempt = Number(silence); attempt < 3; attempt++) {
      await f.recovery.retry(cause);
      await f.recovery.beforeTurn();
    }
    f.setEvent({ kind: "reject", reason: "stop", sourceGateRevision: 11 });
    await assert.rejects(f.recovery.retry(cause), PlanRejectedError);
    assert.deepEqual(f.recovery.fields(102).superseded_revision_input_ids, [101]);
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
    const f = fixture(3, false);
    f.setOwner({ status: "paused", claim_generation: 7, gate_revision: 11, claim_released: false });
    await assert.rejects(f.recovery.beforeTurn(), PlanRevisionRecoveryStoppedError);
    assert.deepEqual(f.writes, []);
  });

  it("id-less source exhaustion keeps compatibility without an unfenced restore or re-drive", async () => {
    const f = fixture(3, false);
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
      const f = fixture(3);
      f.setOwner({ status: "awaiting_approval", claim_generation: 7, gate_revision: 11, claim_released: false, ...patch });
      await assert.rejects(f.recovery.beforeTurn(), PlanRevisionRecoveryStoppedError);
      assert.deepEqual(f.retired, []);
      assert.deepEqual(f.writes, []);
    });

    it("idle revalidation detects " + name + " without human control", async () => {
      const f = fixture();
      f.onEvent(signal => new Promise((_resolve, reject) => {
        signal!.addEventListener("abort", () => reject(signal!.reason), { once: true });
      }));
      f.onSleep(ms => {
        assert.equal(ms, 30_000);
        f.setOwner({ status: "awaiting_approval", claim_generation: 7, gate_revision: 11, claim_released: false, ...patch });
      });
      await assert.rejects(f.recovery.retry(new Error("429")), PlanRevisionRecoveryStoppedError);
      assert.deepEqual(f.sleeps, [30_000]);
      assert.deepEqual(f.writes, []);
      assert.deepEqual(f.retired, []);
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
    const f = fixture(3);
    f.setOwner({ status: "awaiting_approval", claim_generation: 7, gate_revision: 11 }); // release evidence absent
    let reads = 0;
    f.onOwner(() => { reads++; });
    f.onSleep(() => { if (reads === 3) f.cancel(); });
    await assert.rejects(f.recovery.beforeTurn(), /run cancelled/);
    assert.equal(reads, 3);
    assert.equal(f.writes.length, 0);
    assert.equal(f.notices.length, 0, "no charged re-drive");
  });

  for (const control of ["404", "cancel"] as const)
    it(`3466 ownership ${control} keeps precedence over a spent wall`, async () => {
      const f = fixture();
      f.onOwner(() => {
        if (control === "cancel") f.cancel();
        throw new RequestError("GET", "/ownership", 404, "not found");
      });
      await assert.rejects(f.recovery.beforeTurn(undefined, () => 0),
        control === "404" ? PlanRevisionRecoveryStoppedError : /run cancelled/);
      assert.deepEqual(f.sleeps, []);
      assert.deepEqual(f.waits, []);
      assert.deepEqual(f.writes, []);
    });

  it("3466 replacement feedback survives wall exhaustion during ownership retries", async () => {
    const f = fixture();
    let taken = false, reads = 0, remaining = 250;
    f.onEvent(async () => {
      taken = true;
      return { kind: "revise", inputId: 102, feedback: "J", sourceGateRevision: 11 };
    });
    f.onOwner(() => {
      if (!taken) return;
      if (++reads >= 4) f.cancel(); // Bound the unfixed implementation's red direction.
      throw new RequestError("GET", "/ownership", 503, "unavailable");
    });
    f.onSleep(ms => { if (ms <= 250) remaining -= ms; });
    const cause = new TransientRecoveryError("silence", { idleStallKind: "zero_evidence" });
    assert.equal(await f.recovery.retry(cause, undefined, () => remaining), "wall");
    assert.equal(remaining, 0);
    assert.deepEqual(f.waits, [101]);
    assert.deepEqual(f.retired, []);
    remaining = 1000; // An owner extension refused the wall park.
    f.onOwner(() => {});
    await assert.rejects(f.recovery.beforeTurn(undefined, () => remaining), PlanRevisionReplacedError);
    assert.deepEqual(f.waits, [101], "do not take the pending replacement a second time");
    assert.deepEqual(f.recovery.fields(102).superseded_revision_input_ids, [101]);
    assert.deepEqual(f.retired, []);
  });

  it("a cancel during backoff stops before a re-drive is charged", async () => {
    const f = fixture(3);
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

  it("expiry describes the server cap conditionally and keeps ordinary revisions eligible", async () => {
    const f = fixture();
    await assert.rejects(f.recovery.retry(new Error("429")), PlanRejectedError);
    assert.deepEqual(f.waits, [101]);
    assert.match(f.notices.at(-1)!, /if the server's PLAN_MAX_REVISIONS permits/);
    assert.match(f.notices.at(-1)!, /If that cap is exhausted, no further revise\/retry/);
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
    const reclaimed = fixture(3);
    await reclaimed.recovery.retry(new Error("429"));
    await reclaimed.recovery.beforeTurn();
    assert.deepEqual(reclaimed.recovery.fields(101).superseded_revision_input_ids, []);
    assert.equal(reclaimed.notices.filter((text) => /re-driving/.test(text)).length, 1);
  });
});

import { it } from "node:test";
import assert from "node:assert/strict";
import { RunRunner } from "../src/runner.js";
import { CandidateReservationRefusedError, MessageBatcher } from "../src/batcher.js";
import type { WorkerClient, PlanCrossCheckResponse } from "../src/client.js";
import { nullLogger } from "./helpers.js";

type Flight = {
  runId: string;
  preserveRecoveryClone: boolean;
  preserveSession: boolean;
  owedFeedClosed?: boolean;
  owedLimitStop?: Error;
  reportedPreservationRefusals?: Set<string>;
  runLog: { warn(message: string, fields: unknown): void };
  batcher: { emit(message: unknown): void };
};
const seam = RunRunner.prototype as unknown as {
  preservationRefused(flight: Flight, reason: unknown): Error;
};
function probe(deliver?: (message: unknown, flight: Flight) => void) {
  const messages: unknown[] = [];
  const deliveries: Array<{
    preserveRecoveryClone: boolean;
    preserveSession: boolean;
    owedLimitStop: Error | undefined;
    owedMarked: boolean;
  }> = [];
  const logs: unknown[] = [];
  const flight: Flight = {
    runId: "preservation-diagnostic", preserveRecoveryClone: false, preserveSession: false,
    runLog: { warn(message, fields) { logs.push({ message, fields }); } },
    batcher: { emit(message) {
      // Observe inside delivery, assert outside the production catch boundary.
      deliveries.push({
        preserveRecoveryClone: flight.preserveRecoveryClone,
        preserveSession: flight.preserveSession,
        owedLimitStop: flight.owedLimitStop,
        owedMarked: flight.reportedPreservationRefusals?.has("owed_limit") ?? false,
      });
      messages.push(message);
      deliver?.(message, flight);
    } },
  };
  const refuse = (reason: unknown) => seam.preservationRefused.call(RunRunner.prototype, flight, reason);
  return { flight, messages, deliveries, logs, refuse };
}
const reasons = [
  "ownership_unknown", "preservation_failed", "owed_limit", "receipt_pending_failed",
  "tracking_update_failed", "owner_stamp_failed", "receipt_commit_failed", "wip_marker_unsafe",
];
const status = (reason: string) => ({
  kind: "status", agent: "worker", payload: {
    text: `tracking preservation refused (reason=${reason}); inspect retained-work status with uzi run recovery.`,
  },
});

it("emits exactly one fixed-class status per reason per flight and keeps the warning anchor", () => {
  const p = probe();
  assert.equal(p.flight.reportedPreservationRefusals, undefined, "lazy initialization");
  for (const reason of reasons) {
    for (let n = 0; n < 2; n++) {
      const result = p.refuse(reason);
      assert.equal(result.name, "PreservationRefusedError");
      if (reason === "owed_limit") assert.strictEqual(result, p.flight.owedLimitStop);
      else assert.equal(result.message, "tracking preservation refused");
    }
  }
  assert.deepEqual(p.messages, reasons.map(status));
  assert.equal(p.deliveries.length, reasons.length);
  for (const delivery of p.deliveries) {
    assert.equal(delivery.preserveRecoveryClone, true);
    assert.equal(delivery.preserveSession, true);
  }
  assert.deepEqual(p.logs, reasons.flatMap(reason => Array.from({ length: 2 }, () => ({
    message: "tracking preservation refused", fields: { run_id: p.flight.runId, reason },
  }))));
  const other = probe();
  other.refuse(reasons[0]);
  assert.deepEqual(other.messages, [status("ownership_unknown")], "a fresh flight reports again");
});

it("collapses hostile and unexpected values to one unknown key without coercion or disclosure", () => {
  const p = probe();
  const token = ["glpat-", "abcdefghijklmnopqrst"].join("");
  const hostile: unknown[] = [
    `refs/heads/private /private/repo https://example.test/?token=${token}\nerror`,
    "ownership_unknown\n", "OWED_LIMIT", "", null, undefined, 7, {},
    { toString() { throw new Error("must not coerce"); } },
  ];
  for (const value of hostile) assert.equal(p.refuse(value).message, "tracking preservation refused");
  for (const reason of reasons) p.refuse(reason);
  assert.equal(p.flight.reportedPreservationRefusals?.size, 9);
  assert.deepEqual(p.messages, [status("unknown"), ...reasons.map(status)]);
  assert.deepEqual(p.logs.slice(0, hostile.length), hostile.map(() => ({
    message: "tracking preservation refused", fields: { run_id: p.flight.runId, reason: "unknown" },
  })));
  assert.ok(!JSON.stringify([p.messages, p.logs]).includes(token));
});

it("marks closed-feed attempts without delivery or later retry", () => {
  const p = probe();
  p.flight.owedFeedClosed = true;
  p.refuse("preservation_failed");
  p.refuse("owed_limit");
  assert.ok(p.flight.owedLimitStop);
  assert.equal(p.messages.length, 0);
  assert.equal(p.flight.reportedPreservationRefusals?.size, 2);
  p.flight.owedFeedClosed = false;
  p.refuse("preservation_failed");
  p.refuse("owed_limit");
  assert.equal(p.messages.length, 0);
});

for (const error of [new Error("arbitrary delivery error"), new CandidateReservationRefusedError("cancelled"), "delivery throw"]) {
  it(`contains ${typeof error === "string" ? "non-Error throws" : error.name} and latches owed_limit before delivery`, () => {
    const p = probe(() => { throw error; });
    const first = p.refuse("owed_limit");
    assert.equal(p.deliveries.length, 1);
    assert.equal(p.deliveries[0]!.preserveRecoveryClone, true);
    assert.equal(p.deliveries[0]!.preserveSession, true);
    assert.equal(p.deliveries[0]!.owedMarked, true, "marked before emit");
    assert.strictEqual(p.deliveries[0]!.owedLimitStop, first, "latched before emit");
    assert.strictEqual(first, p.flight.owedLimitStop);
    assert.match(first.message, /retained owed-head limit/);
    assert.strictEqual(p.refuse("owed_limit"), first);
    assert.equal(p.messages.length, 1, "failed delivery is still an attempt");
    const ordinary = probe(() => { throw error; });
    assert.equal(ordinary.refuse("preservation_failed").message, "tracking preservation refused");
    assert.equal(ordinary.flight.owedLimitStop, undefined);
    assert.throws(() => { throw ordinary.refuse("preservation_failed"); }, /tracking preservation refused/);
    assert.equal(ordinary.messages.length, 1);
  });
}

for (const failedBefore of [false, true]) {
  it(`contains real ${failedBefore ? "already-failed reservation refusal" : "first-overflow plain Error"} without repairing transport`, async () => {
    let posted = 0;
    const b = new MessageBatcher({
      async postMessages() { posted++; }, async postUsage() {},
    } as unknown as WorkerClient, "preservation-diagnostic", 0, 60_000, nullLogger(), undefined, undefined,
    { generation: 3, spillBufferBytes: 180 });
    const reservation = b.reserveCandidateTransport();
    // An ordinary held event fits; the bounded diagnostic is allowed to cause the first overflow.
    b.emit({ kind: "status", payload: { text: "held" } });
    if (failedBefore) reservation.cancel();
    const p = probe(message => b.emit(message as Parameters<MessageBatcher["emit"]>[0]));
    try {
      const original = p.refuse("owed_limit");
      assert.strictEqual(original, p.flight.owedLimitStop);
      assert.strictEqual(p.refuse("owed_limit"), original);
      const refusalReason = failedBefore ? "cancelled" : "overflow";
      assert.deepEqual(await reservation.prepare(), {
        prepared: false, tail: 0, permanent: true, reason: refusalReason,
      });
      assert.throws(() => b.emit({ kind: "status", payload: { text: "later" } }), error => {
        assert.ok(error instanceof CandidateReservationRefusedError);
        assert.equal(error.reason, refusalReason);
        return true;
      });
      const proof = {
        result: "candidate", verdict: "revise", candidate_generation: 3, lead_last_seq: 0,
        reconciliation: { leadLastSeq: 0, claimGeneration: 3, planCrossCheckSettled: true,
          gateRevision: 0, currentPlanSHA256: "a".repeat(64) },
      } as PlanCrossCheckResponse;
      assert.equal(reservation.release(proof), false, "diagnostic containment grants no release authority");
      assert.equal(b.currentSeq(), 0);
      assert.equal(posted, 0);
      assert.equal(p.messages.length, 1);
    } finally {
      await b.close();
    }
    assert.equal(posted, 0);
  });
}

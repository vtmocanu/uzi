import { MaintenanceBudget } from "../src/maintenance-budget.js";
import { AsyncLocalStorage } from "node:async_hooks";
import { Readable } from "node:stream";
import { makeScratchRoot } from "./fixture-repo.js";
import { it } from "node:test";
import { RunRunner } from "../src/runner.js";
import { prepareFrozenHeld, type HeldGit } from "../src/frozen-held.js";
import { RequestError } from "../src/client.js";
import type { RecoveryExactHold, RecoveryFinalDisposition, RecoveryReserveRequest } from "../src/protocol.js";
import { execFileSync } from "node:child_process";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { RecoveryCoordinator, canonicalJson, type InventorySourceBoundaryContext, type InventorySourceDiagnostic, type RecoveryArchiveClient, type RecoveryRecord } from "../src/recovery.js";
import { createHash, createHmac } from "node:crypto";
import { GitCache, RecoveryBundleTooLargeError, type OwedCandidate, type PositiveOwedCandidateContext } from "../src/git.js";
import { nullLogger, testGitCacheOptions } from "./helpers.js";
import { buildRecoveryLiveTiming } from "../src/main.js";

const H = "a".repeat(40);
const H2 = "b".repeat(40);

it("retained owner attention paces FINAL retries without repeated reason writes", async () => {
  const { f, before } = await pendingReceiptFixture();
  try {
    const coordinator = f.make();
    const warnings: string[] = [];
    f.log.warn = message => { warnings.push(message); };
    f.state.finalError = new RequestError("POST", "/release", 403, JSON.stringify({ reason: "not_authorized" }));
    f.client.reconcileRecoveryCapture = async (run, id, request) => ({
      run_id: run, capture_id: id, generation: request.generation,
      outcome: "retained", reason: "capture_failure_requires_attention",
    });
    let writes = 0;
    const write = coordinator["writeRecordUnlocked"].bind(coordinator);
    Object.assign(coordinator, { writeRecordUnlocked: async (...args: Parameters<typeof write>) => {
      writes++; return write(...args);
    } });
    const initialFinals = f.finals.length;
    await coordinator.resumePending(undefined, [before]);
    const first = (await coordinator.inspect(before.runId))[0]!;
    assert.equal(first.reason, "capture_failure_requires_attention");
    const firstWrites = writes;
    await coordinator.resumePending(undefined, [first]);
    assert.equal(f.finals.length, initialFinals + 1, "same-clock pass must respect per-record backoff");
    assert.equal(writes, firstWrites);
    f.state.now += 60_000;
    await coordinator.resumePending(undefined, [first]);
    assert.equal(f.finals.length, initialFinals + 2, "due retry stays eligible");
    assert.equal(writes, firstWrites, "stable owner reason must not flip to refusal and back");
    assert.equal(warnings.filter(message => message === "recovery: inventory requires owner attention").length, 1);
    const after = (await coordinator.inspect(before.runId))[0]!;
    assert.equal(after.reason, first.reason);
    assert.deepEqual(after.finalRequest, before.finalRequest);
    assert.equal(after.finalAcknowledged, undefined);
    assert.equal(f.state.pinDeletes, 0);
  } finally { await f.close(); }
});

for (const [status, reason] of [[401, "hold_not_original_worker"],
  [403, "capture_failure_requires_attention"], [409, "capture_manifest_integrity_mismatch"]] as const) {
  it(`FINAL refusal ${status} preserves the reconciliation owner-attention reason`, async () => {
    const { f, before } = await pendingReceiptFixture();
    try {
      const warnings: Array<{ message: string; fields?: Record<string, unknown> }> = [];
      f.log.warn = (message, fields) => { warnings.push({ message, fields }); };
      f.state.finalError = new RequestError("POST", "/release", status, JSON.stringify({ reason: "not_authorized" }));
      f.client.reconcileRecoveryCapture = async (run, id, request) => ({
        run_id: run, capture_id: id, generation: request.generation, outcome: "retained", reason,
      });
      const bundle = await fs.readFile(before.bundlePath!);
      await f.make().resumePending(undefined, [before]);
      const after = (await f.coordinator.inspect(before.runId))[0]!;
      assert.equal(after.reason, reason);
      assert.ok(warnings.some(warning => warning.message === "recovery: inventory requires owner attention" &&
        warning.fields?.reason === reason));
      assert.deepEqual(after.finalRequest, before.finalRequest);
      assert.equal(after.finalAcknowledged, undefined);
      assert.deepEqual(await fs.readFile(before.bundlePath!), bundle);
      assert.equal(f.state.pinDeletes, 0);
      assert.equal(f.state.uploads, 1);
    } finally { await f.close(); }
  });
}

for (const gate of ["inactive", "feature", "coverage"] as const) {
  it(`advisory saved FINAL local ${gate} void preserves reason and immediate eligibility`, async () => {
    const { f, before } = await pendingReceiptFixture();
    try {
      const coordinator = f.make(true);
      if (gate === "inactive") f.state.status = "running";
      if (gate === "feature") f.state.feature = false;
      if (gate === "coverage") f.state.cloneReadable = false;
      const initial = f.finals.length;
      await coordinator.resumePending(undefined, [before]);
      const retained = (await coordinator.inspect(before.runId))[0]!;
      assert.deepEqual(retained, before, "local void must not rewrite the saved FINAL");
      assert.equal(f.finals.length, initial, "local gate must not send FINAL");
      assert.equal(coordinator["finalRetry"].size, 0, "local void must not create a retry delay");
      f.state.status = "failed"; f.state.feature = true; f.state.cloneReadable = true;
      await coordinator.resumePending(undefined, [retained]);
      assert.equal(f.finals.length, initial + 1, "gate clearing permits recovery at the same clock time");
      assert.equal((await coordinator.inspect(before.runId))[0]!.finalAcknowledged, true);
    } finally { await f.close(); }
  });
}

it("advisory due FINAL retry local gate does not advance an existing delay", async () => {
  const { f, before } = await pendingReceiptFixture();
  try {
    const coordinator = f.make();
    f.state.finalError = new RequestError("POST", "/release", 403, JSON.stringify({ reason: "not_authorized" }));
    await coordinator.resumePending(undefined, [before]);
    const retry = structuredClone(coordinator["finalRetry"].get(before.captureId));
    assert.ok(retry);
    const retained = (await coordinator.inspect(before.runId))[0]!;
    f.state.now = retry.nextAt;
    f.state.status = "running";
    await coordinator.resumePending(undefined, [retained]);
    assert.deepEqual(coordinator["finalRetry"].get(before.captureId), retry);
    assert.deepEqual((await coordinator.inspect(before.runId))[0], retained);
    assert.equal(f.finals.length, 2);
    f.state.status = "failed"; f.state.finalError = undefined;
    await coordinator.resumePending(undefined, [retained]);
    assert.equal(f.finals.length, 3, "clearing the gate does not owe another retry delay");
    assert.equal((await coordinator.inspect(before.runId))[0]!.finalAcknowledged, true);
  } finally { await f.close(); }
});

for (const answer of ["throw", "malformed", "available", "missing"] as const) {
  it(`advisory saved FINAL unknown reconciliation remains paced ${answer}`, async () => {
    const { f, before } = await pendingReceiptFixture();
    try {
      const coordinator = f.make();
      let reads = 0, reconciliations = 0;
      f.client.getRecoveryCaptureStatus = async () => { reads++; throw new Error("status unavailable"); };
      if (answer === "missing") delete f.client.reconcileRecoveryCapture;
      else f.client.reconcileRecoveryCapture = async (run, id, req) => {
        reconciliations++;
        if (answer === "throw") throw new Error("reconcile unavailable");
        return answer === "malformed" ? {} as never : {
          run_id: run, generation: req.generation, capture_id: id,
          outcome: "retained", reason: "capture_available",
        };
      };
      for (let pass = 0; pass < 2; pass++) await coordinator.resumePending(undefined, [before]);
      assert.equal(reads, 1, "unknown reconciliation must bound repeated status probes");
      assert.equal(reconciliations, answer === "missing" ? 0 : 1);
      assert.equal((await coordinator.inspect(before.runId))[0]!.reason, "final_retry_unknown");
      f.state.now += 60_000;
      await coordinator.resumePending();
      assert.equal(reads, 2, "unknown reconciliation remains eligible when due");
      assert.equal(f.finals.length, 1, "unhealthy capture never sends another FINAL");
    } finally { await f.close(); }
  });
}

it("advisory unchanged settled FINAL reason performs no journal write", async () => {
  const f = await v2FrozenFixture("ci_fix", true);
  try {
    f.state.finalError = new Error("save FINAL");
    await f.capture(f.record);
    const before = (await f.coordinator.inspect(f.context.runId))[0]!;
    const exact = await (f.client as RecoveryArchiveClient).getExactRecoveryHold!(before.runId, 7, before.exactHoldBinding!);
    await patchAuthenticated(f, before, {
      holdSettlement: closedExactFixture(before, exact!, "discarded"), reason: "inventory_hold_settled",
    });
    const coordinator = f.make(true);
    let writes = 0;
    const write = coordinator["writeRecordUnlocked"].bind(coordinator);
    Object.assign(coordinator, { writeRecordUnlocked: async (...args: Parameters<typeof write>) => {
      writes++; return write(...args);
    } });
    for (let pass = 0; pass < 3; pass++) await coordinator.resumePending();
    assert.equal(writes, 0, "unchanged settled reason is a diagnostic-only retention");
    assert.equal(f.finals.length, 1);
    assert.equal(f.state.pinDeletes, 0);
  } finally { await f.close(); }
});

it("advisory thrown finalization error bypasses pacing and keeps its diagnostic", async () => {
  const { f, before } = await pendingReceiptFixture();
  try {
    const coordinator = f.make();
    const errors: unknown[] = [];
    f.log.warn = (_message, fields) => { if (fields?.error) errors.push(fields.error); };
    Object.assign(coordinator, {
      withInventorySourceBoundary: async () => { throw new Error("original finalization failure"); },
      writeRecordUnlocked: async () => { throw new Error("pacing masked original"); },
    });
    await coordinator.resumePending(undefined, [before]);
    assert.ok(errors.some(error => String(error).includes("original finalization failure")));
    assert.ok(!errors.some(error => String(error).includes("pacing masked original")));
    assert.equal(coordinator["finalRetry"].size, 0, "thrown finalization cannot schedule pacing");
    assert.deepEqual((await coordinator.inspect(before.runId))[0], before);
  } finally { await f.close(); }
});

it("advisory finalization budget exhaustion does not enter pacing", async () => {
  const { f, before } = await pendingReceiptFixture();
  let mono = 0, expired = false, lateReads = 0;
  const budget = new MaintenanceBudget(600_000, () => mono);
  try {
    const coordinator = f.make();
    const read = coordinator["readLatest"].bind(coordinator);
    Object.assign(coordinator, {
      withInventorySourceBoundary: async () => {
        expired = true; mono = 600_000; budget.check();
      },
      readLatest: async (...args: Parameters<typeof read>) => {
        if (expired) lateReads++;
        return read(...args);
      },
    });
    await assert.rejects(MaintenanceBudget.run(budget, () => coordinator.resumePending(undefined, [before])),
      error => error === budget.signal.reason);
    assert.equal(expired, true, "the budget expires inside finalization");
    assert.equal(lateReads, 0, "expired finalization must not enter pacing journal IO");
    assert.equal(coordinator["finalRetry"].size, 0);
    assert.deepEqual((await coordinator.inspect(before.runId))[0], before);
  } finally { budget.dispose(); await f.close(); }
});

// M1 repair assertions were observed red on the unchanged product baseline.
for (const status of [403, 409, 429, 503, "network"] as const) {
  it(`M1 R2987-05a binding-less immutable FINAL capped retry ${status}`, async t => {
    const { f, before } = await pendingReceiptFixture();
    const warnings: unknown[] = [];
    f.log.warn = (message, fields) => { warnings.push({ message, fields }); };
    f.git.discoverOwedCandidates = async () => [];
    f.state.finalError = status === "network" ? new Error("network unavailable")
      : new RequestError("POST", "/api/worker/recovery/release", status,
        JSON.stringify({ error: "FINAL refused", ...(status === 403 || status === 409 ? { reason: "not_authorized" } : {}) }));
    const samples: Array<{ now: number; finals: number }> = [];
    try {
      assert.equal(before.exactHoldBinding, undefined);
      assert.ok(before.finalRequest);
      const start = f.state.now;
      // Probe before/at exponentially spaced deadlines, including the 15-minute cap.
      for (const elapsed of [0, 30_000, 60_000, 90_000, 180_000, 420_000, 900_000, 1_800_000, 2_700_000]) {
        f.state.now = start + elapsed;
        await f.coordinator.resumeLive({ isExecuting: () => false, authenticatedAtMs: f.state.now });
        samples.push({ now: elapsed, finals: f.finals.length - 1 });
      }
      const after = (await f.coordinator.inspect(before.runId))[0]!;
      t.diagnostic(JSON.stringify({ status, samples, warnings, finalRequest: after.finalRequest,
        finalAcknowledged: after.finalAcknowledged, uploads: f.state.uploads, produced: f.state.produced }));
      assert.deepEqual(after.finalRequest, before.finalRequest);
      assert.equal(after.finalAcknowledged, undefined);
      assert.equal(f.state.uploads, 1);
      assert.equal(f.state.produced, 1);
      assert.deepEqual(samples.map(s => s.finals), [1, 1, 2, 2, 3, 4, 5, 6, 7],
        "each refused saved FINAL backs off to a cap without suppressing future retries");
    } finally { await f.close(); }
  });
}

for (const status of [403, 409]) {
  it(`M1 R2987-05a structured FINAL diagnostic ${status}`, async t => {
    const { f, before } = await pendingReceiptFixture();
    try {
      const warnings: Array<{ message: string; fields?: Record<string, unknown> }> = [];
      f.log.warn = (message, fields) => { warnings.push({ message, fields }); };
      f.git.discoverOwedCandidates = async () => [];
      const unsafe = "diagnostic-untrusted-body" + "\u001b[31m\n" + "x".repeat(24_000);
      f.state.finalError = new RequestError("POST", "/api/worker/recovery/release", status,
        JSON.stringify({ error: unsafe, reason: "not_authorized" }));
      await f.coordinator.resumeLive({ isExecuting: () => false, authenticatedAtMs: f.state.now });
      t.diagnostic(JSON.stringify({ status, capture: before.captureId,
        warnings: warnings.map(warning => ({ message: warning.message,
          fields: Object.keys(warning.fields ?? {}), size: JSON.stringify(warning).length })) }));
      assert.ok(warnings.some(w => w.fields?.reason === "not_authorized" &&
        w.fields?.capture_id === before.captureId && !w.fields?.error),
        "emit the bounded structured reason for this record rather than only raw RequestError text");
      for (const warning of warnings) {
        const serialized = JSON.stringify(warning);
        assert.ok(!serialized.includes("diagnostic-untrusted-body"), "no additional warning exposes response payload");
        assert.ok(serialized.length <= 4096, "each diagnostic has bounded output");
      }
    } finally { await f.close(); }
  });
}

it("M1 R2987-05a per-record refusal backoff permits a new healthy sibling", async t => {
  const f = await fixture();
  try {
    f.state.candidates = [];
    f.git.discoverOwedCandidates = async () => [];
    f.state.finalError = new RequestError("POST", "/api/worker/recovery/release", 403,
      JSON.stringify({ error: "FINAL refused", reason: "not_authorized" }));
    const first = await f.freeze();
    assert.ok(first);
    await f.capture(first);
    const initial = f.finals.length;
    await f.coordinator.resumeLive({ isExecuting: () => false, authenticatedAtMs: f.state.now });
    const second = await f.coordinator.freezeInventory({
      context: { ...f.context, runId: "run-2" }, currentSha: H, defaultBranch: "main",
    });
    assert.ok(second);
    await f.coordinator.captureAndUpload({ record: second, barePath: f.context.barePath, defaultBranch: "main" });
    const release = f.client.releaseRecoveryCustody;
    f.client.releaseRecoveryCustody = async (...args) => {
      if (args[0] === "run-2") {
        f.state.finalError = undefined;
        try { return await release(...args); }
        finally { f.state.finalError = new RequestError("POST", "/api/worker/recovery/release", 403,
          JSON.stringify({ reason: "not_authorized" })); }
      }
      return release(...args);
    };
    f.state.now += 30_000;
    await f.coordinator.resumeLive({ isExecuting: () => false, authenticatedAtMs: f.state.now });
    const firstCalls = f.finals.filter(a => a[0] === "run-1").length - initial;
    const sibling = (await f.coordinator.inspect("run-2"))[0]!;
    t.diagnostic(JSON.stringify({ firstCalls, finals: f.finals, sibling }));
    assert.equal(sibling.finalAcknowledged, true, "refused peer does not delay a healthy new record");
    assert.equal(firstCalls, 1, "the refused record remains in its own 60-second delay");
  } finally { await f.close(); }
});

for (const closure of ["discarded", "different-receipt", "matching-receipt", "open", "unknown", "wrong-binding"] as const) {
  for (const health of ["healthy", "unhealthy", "throwing"] as const) {
    it(`M1 R2987-05b fresh bound archive closure ${closure} ${health}`, async t => {
      const f = await v2FrozenFixture("ci_fix", true);
      try {
        // ci_fix freezes the V2 authority but falls back to a self-contained archive.
        f.state.finalError = new Error("save FINAL without ACK");
        await f.capture(f.record);
        const before = (await f.coordinator.inspect(f.context.runId))[0]!;
        assert.equal(before.finalRequest?.disposition.kind, "archive");
        assert.equal(before.holdSettlement, undefined);
        const exact = (f.client as RecoveryArchiveClient).getExactRecoveryHold!;
        const original = (await exact(before.runId, 7, before.exactHoldBinding!))!;
        const closed: RecoveryExactHold = { ...original, state: closure === "open" ? "open" : "released",
          release_evidence: "archive", final_receipt: before.finalRequest!.disposition };
        if (closure === "discarded") {
          closed.state = "discarded"; closed.release_evidence = "owner_discard"; delete closed.final_receipt;
        }
        if (closure === "different-receipt") {
          closed.release_evidence = "held_publication";
          closed.final_receipt = {
            kind: "held_publication", publication_id: "00000000-0000-4000-8000-000000000009",
            source_sha: before.sourceSha, coverage_digest: before.coverageDigest!,
          };
        }
        if (closure === "open") { delete closed.release_evidence; delete closed.final_receipt; }
        else { delete closed.live_worker_id; delete closed.live_run_id; }
        if (closure === "matching-receipt" || closure === "different-receipt") {
          f.committedReceipts.set(7, Object.freeze(structuredClone(closed.final_receipt!)) as Readonly<RecoveryFinalDisposition>);
          f.state.open = false;
        }
        if (closure === "wrong-binding") closed.hold_id = "00000000-0000-4000-8000-000000000099";
        let reads = 0;
        Object.assign(f.client, { getExactRecoveryHold: async () => { reads++; return closure === "unknown" ? undefined : closed; } });
        const status = f.client.getRecoveryCaptureStatus;
        f.client.getRecoveryCaptureStatus = async (...args) => {
          if (health === "throwing") { f.state.statusReads++; throw new Error("status unavailable"); }
          const response = await status(...args);
          return health === "unhealthy" ? { ...response, state: "needs_action", reason: "archive integrity check failed" } : response;
        };
        f.state.finalError = undefined;
        f.git.discoverOwedCandidates = async () => [];
        const costs = [];
        for (let pass = 0; pass < 3; pass++) {
          f.state.now += 900_000;
          await f.coordinator.resumeLive({ isExecuting: () => false, authenticatedAtMs: f.state.now });
          costs.push({ coverage: f.aggregates.length, status: f.state.statusReads, exact: reads, finals: f.finals.length });
        }
        const after = (await f.coordinator.inspect(before.runId))[0]!;
        t.diagnostic(JSON.stringify({ closure, health, binding: before.exactHoldBinding, saved: before.finalRequest,
          observed: closed, persisted: after.holdSettlement, costs, ack: after.finalAcknowledged }));
        assert.deepEqual(after.finalRequest, before.finalRequest);
        if (closure === "unknown" || closure === "wrong-binding") assert.equal(after.holdSettlement, undefined);
        if (closure === "open" || closure === "unknown" || closure === "wrong-binding") {
          assert.equal(after.finalAcknowledged === true, health === "healthy");
        }
        if (closure === "discarded" || closure === "different-receipt") {
          assert.deepEqual(after.holdSettlement, closed, "persist fresh exact closure even if capture status is unusable");
          assert.equal(after.finalAcknowledged, undefined);
          assert.equal(f.finals.length, 1);
          assert.deepEqual(costs[2], costs[1], "settled different authority stops repeated coverage/status work");
        } else if (closure === "matching-receipt") {
          assert.equal(after.finalAcknowledged, true, "matching real receipt recovers the original ACK");
        }
      } finally { await f.close(); }
    });
  }
}

it("M1 C(skip) foreground capture rotates busy saved FINAL without waiting", async t => {
  const { f, before } = await pendingReceiptFixture();
  let release!: () => void, arrived!: () => void;
  const gate = new Promise<void>(resolve => { release = resolve; });
  const started = new Promise<void>(resolve => { arrived = resolve; });
  let pause = true, reads = 0;
  f.git.discoverOwedCandidates = async () => [];
  const status = f.client.getRecoveryCaptureStatus;
  f.client.getRecoveryCaptureStatus = async (...args) => {
    reads++;
    if (pause) { pause = false; arrived(); await gate; }
    return status(...args);
  };
  const runner = new RunRunner(f.client as never, f.git as never,
    () => { throw new Error("no executor"); }, f.log, 20, "local-worker-fixture", { recovery: f.coordinator });
  const foreground = f.capture(before);
  try {
    await started;
    await runner.resumeLiveRecoveries(f.state.now);
    const during = (await f.coordinator.inspect(before.runId))[0]!;
    t.diagnostic(JSON.stringify({ reads, finals: f.finals.length,
      lastAttempt: [...f.coordinator["lastAttemptAt"]], nextPassAt: f.coordinator["nextPassAt"] }));
    assert.equal(reads, 1, "C(skip) does not enter the record while foreground capture owns C");
    assert.equal(during.finalAcknowledged, undefined);
    assert.equal(f.coordinator["lastAttemptAt"].get(before.captureId), f.state.now, "busy record rotates");
  } finally {
    release();
    await foreground;
    await f.close();
  }
});

it("M1 R2987-05c persisted different closure skips repeated coverage and status", async t => {
  const f = await v2FrozenFixture("ci_fix", true);
  try {
    f.state.finalError = new Error("save immutable archive FINAL");
    await f.capture(f.record);
    const before = (await f.coordinator.inspect(f.context.runId))[0]!;
    const exact = (f.client as RecoveryArchiveClient).getExactRecoveryHold!;
    const closed = closedExactFixture(before, (await exact(before.runId, 7, before.exactHoldBinding!))!, "discarded");
    await patchAuthenticated(f, before, { holdSettlement: closed });
    f.git.discoverOwedCandidates = async () => [];
    const costs = [];
    for (let pass = 0; pass < 3; pass++) {
      f.state.now += 900_000;
      await f.coordinator.resumeLive({ isExecuting: () => false, authenticatedAtMs: f.state.now });
      costs.push({ coverage: f.aggregates.length, status: f.state.statusReads, finals: f.finals.length });
    }
    const after = (await f.coordinator.inspect(before.runId))[0]!;
    t.diagnostic(JSON.stringify({ binding: before.exactHoldBinding, saved: before.finalRequest, closed, costs }));
    assert.deepEqual(after.finalRequest, before.finalRequest);
    assert.equal(after.finalAcknowledged, undefined);
    assert.deepEqual(costs[2], costs[0], "persisted nonmatching closure must stop repeated expensive work");
  } finally { await f.close(); }
});


for (const [status, closure] of [[403, "discarded"], [409, "different-receipt"]] as const) {
  it(`M3 exact refusal survives heartbeat and restart ${status}`, async () => {
    const f = await v2FrozenFixture("ci_fix", true);
    try {
      f.state.finalError = new RequestError("POST", "/release", status, JSON.stringify({ reason: "not_authorized" }));
      await f.capture(f.record);
      const before = (await f.coordinator.inspect(f.context.runId))[0]!;
      const exact = (f.client as RecoveryArchiveClient).getExactRecoveryHold!;
      const closed = closedExactFixture(before, (await exact(before.runId, 7, before.exactHoldBinding!))!, "discarded");
      if (closure === "different-receipt") {
        closed.state = "released"; closed.release_evidence = "held_publication";
        closed.final_receipt = { kind: "held_publication", publication_id: "00000000-0000-4000-8000-000000000009",
          source_sha: before.sourceSha, coverage_digest: before.coverageDigest! };
      }
      delete closed.live_run_id; delete closed.live_worker_id;
      if (closed.final_receipt === undefined) delete closed.final_receipt;
      let reads = 0;
      Object.assign(f.client, { getExactRecoveryHold: async () => { reads++; return closed; } });
      f.client.getRecoveryCaptureStatus = async () => { throw new Error("must stop before capture status"); };
      f.git.discoverOwedCandidates = async () => [];
      const costs = { produced: f.state.produced, uploads: f.state.uploads, reserves: f.reserves(), coverage: f.aggregates.length };
      const bundle = await fs.readFile(before.bundlePath!);
      await f.coordinator.resumeLive({ isExecuting: () => false, authenticatedAtMs: f.state.now });
      const firstReads = reads;
      const restarted = f.make(true);
      await restarted.resumePending();
      f.state.now += 900_000;
      await restarted.resumeLive({ isExecuting: () => false, authenticatedAtMs: f.state.now });
      const after = (await restarted.inspect(before.runId))[0]!;
      assert.deepEqual(after.holdSettlement, closed);
      assert.equal(reads, firstReads, "authenticated persisted settlement needs no entry probe");
      assert.deepEqual(after.finalRequest, before.finalRequest);
      assert.deepEqual(after.originalRoots, before.originalRoots);
      assert.equal(after.finalAcknowledged, undefined);
      assert.equal(after.reason, "inventory_hold_settled");
      assert.equal(f.finals.length, 1);
      assert.equal(f.state.pinDeletes, 0);
      assert.deepEqual(await fs.readFile(before.bundlePath!), bundle);
      assert.deepEqual({ produced: f.state.produced, uploads: f.state.uploads, reserves: f.reserves(), coverage: f.aggregates.length }, costs);
      assert.deepEqual(Object.keys(after).sort(), [...new Set([...Object.keys(before), "holdSettlement", "reason"])].sort(),
        "only existing journal fields change");
    } finally { await f.close(); }
  });
}

it("M3 binding-less restart retries once then paces repeated boot and live work", async () => {
  const { f, before } = await pendingReceiptFixture();
  try {
    f.git.discoverOwedCandidates = async () => [];
    const warnings: unknown[] = [];
    f.log.warn = (message, fields) => { warnings.push({ message, fields }); };
    f.state.finalError = new RequestError("POST", "/release", 403,
      JSON.stringify({ reason: "hostile-refusal-sentinel" + "\\u001b" + "x".repeat(24_000) }));
    const restarted = f.make();
    const initial = f.finals.length;
    await restarted.resumePending();
    const costs = [f.state.statusReads, f.aggregates.length, f.finals.length];
    await restarted.resumePending();
    await restarted.resumeLive({ isExecuting: () => false, authenticatedAtMs: f.state.now });
    assert.deepEqual([f.state.statusReads, f.aggregates.length, f.finals.length], costs);
    assert.equal(f.finals.length, initial + 1);
    let after = (await restarted.inspect(before.runId))[0]!;
    assert.equal(after.reason, "final_retry_refused");
    assert.equal(after.holdSettlement, undefined);
    assert.deepEqual(after.finalRequest, before.finalRequest);
    f.state.now += 60_000;
    await restarted.resumePending();
    assert.equal(f.finals.length, initial + 2);
    assert.equal(warnings.length, 1, "one warning per unchanged refusal transition");
    for (const warning of warnings) {
      assert.ok(!JSON.stringify(warning).includes("hostile-refusal-sentinel"));
      assert.ok(JSON.stringify(warning).length <= 4096);
    }
    const fresh = f.make();
    assert.deepEqual((await fresh.inspect(before.runId))[0], (await restarted.inspect(before.runId))[0],
      "reason remains MAC readable by an unchanged record parser");
    f.state.finalError = undefined;
    await fresh.resumePending();
    after = (await fresh.inspect(before.runId))[0]!;
    assert.equal(after.finalAcknowledged, true);
  } finally { await f.close(); }
});

it("M3 FINAL refusal and failed reconciliation log once per predicate transition", async () => {
  const { f, before } = await pendingReceiptFixture();
  try {
    f.git.discoverOwedCandidates = async () => [];
    const warnings: Array<{ message: string; fields?: Record<string, unknown> }> = [];
    f.log.warn = (message, fields) => { warnings.push({ message, fields }); };
    const hostile = "paired-refusal-secret-sentinel" + "\u001b[31m\n" + "x".repeat(24_000);
    f.state.finalError = new RequestError("POST", "/release", 403,
      JSON.stringify({ reason: "not_authorized", error: hostile }));
    let reconciliations = 0;
    f.client.reconcileRecoveryCapture = async () => { reconciliations++; throw new Error(hostile); };
    const restarted = f.make();
    const bundle = await fs.readFile(before.bundlePath!);
    const costs = { produced: f.state.produced, uploads: f.state.uploads, reserves: f.reserves() };
    const initialFinals = f.finals.length;
    await restarted.resumePending();
    assert.equal(f.finals.length, initialFinals + 1);
    assert.equal(reconciliations, 1);
    assert.deepEqual(warnings.map(w => w.fields?.refusing_predicate),
      ["final_retry_refused", "final_retry_unknown"], "distinct predicates each get their first diagnostic");
    for (const [elapsed, boot] of [[60_000, true], [180_000, false], [420_000, true]] as const) {
      f.state.now = 1000 + elapsed;
      if (boot) await restarted.resumePending();
      else await restarted.resumeLive({ isExecuting: () => false, authenticatedAtMs: f.state.now });
      assert.equal(warnings.length, 2, "unchanged refusal/reconciliation pair adds no warnings");
    }
    assert.equal(f.finals.length, initialFinals + 4, "due boot/live attempts still retry FINAL");
    assert.equal(reconciliations, 4);
    f.state.finalError = new RequestError("POST", "/release", 409,
      JSON.stringify({ reason: "manifest_conflict", error: hostile }));
    f.state.now += 900_000;
    await restarted.resumeLive({ isExecuting: () => false, authenticatedAtMs: f.state.now });
    assert.equal(warnings.length, 3, "a changed protocol refusal emits a new diagnostic");
    assert.equal(warnings[2]!.fields?.reason, "manifest_conflict");
    assert.equal(warnings[2]!.fields?.refusal_code, 409);
    f.state.finalError = new RequestError("POST", "/release", 403,
      JSON.stringify({ reason: "not_authorized", error: hostile }));
    f.state.now += 900_000;
    await restarted.resumePending();
    assert.equal(warnings.length, 4, "returning to an earlier protocol refusal is a real transition");
    assert.equal(warnings[3]!.fields?.refusal_code, 403);
    const after = (await restarted.inspect(before.runId))[0]!;
    assert.equal(after.reason, "final_retry_unknown", "transport uncertainty stays retained");
    assert.equal(after.finalAcknowledged, undefined);
    assert.deepEqual(after.finalRequest, before.finalRequest);
    assert.deepEqual(after.originalRoots, before.originalRoots);
    assert.equal(after.serverCaptureId, before.serverCaptureId);
    assert.equal(after.reserveIdempotencyKey, before.reserveIdempotencyKey);
    assert.deepEqual(await fs.readFile(before.bundlePath!), bundle);
    assert.deepEqual({ produced: f.state.produced, uploads: f.state.uploads, reserves: f.reserves() }, costs);
    assert.equal(f.state.pinDeletes, 0);
    assert.equal(f.state.open, true);
    for (const warning of warnings) {
      assert.ok(!JSON.stringify(warning).includes("paired-refusal-secret-sentinel"));
      assert.ok(JSON.stringify(warning).length <= 4096);
    }
  } finally { await f.close(); }
});

for (const authority of ["unknown", "wrong-binding", "api-absent"] as const) {
  it(`M3 failed reconciliation remains retryable with ${authority} exact authority`, async () => {
    const f = await v2FrozenFixture("ci_fix", true);
    try {
      f.state.finalError = new Error("save FINAL");
      await f.capture(f.record);
      const before = (await f.coordinator.inspect(f.context.runId))[0]!;
      const exact = (f.client as RecoveryArchiveClient).getExactRecoveryHold!;
      const original = (await exact(before.runId, 7, before.exactHoldBinding!))!;
      Object.assign(f.client, { getExactRecoveryHold: authority === "api-absent" ? undefined :
        async () => authority === "unknown" ? undefined : { ...original, hold_id: "00000000-0000-4000-8000-000000000099" } });
      const warnings: unknown[] = [];
      f.log.warn = (message, fields) => { warnings.push({ message, fields }); };
      const status = f.client.getRecoveryCaptureStatus;
      let reads = 0, reconciliations = 0;
      const hostile = "reconciliation-secret-sentinel" + "\\u001b[31m\\n" + "x".repeat(24_000);
      f.client.getRecoveryCaptureStatus = async () => { reads++; throw new Error(hostile); };
      f.client.reconcileRecoveryCapture = async () => { reconciliations++; throw new Error(hostile); };
      f.git.discoverOwedCandidates = async () => [];
      const restarted = f.make(true);
      await restarted.resumePending();
      const first = (await restarted.inspect(before.runId))[0]!;
      assert.equal(first.reason, "final_retry_unknown");
      assert.equal(first.holdSettlement, undefined);
      assert.equal(first.finalAcknowledged, undefined);
      await restarted.resumePending();
      assert.equal(reads, 1); assert.equal(reconciliations, 1);
      assert.equal(warnings.length, 1);
      for (const warning of warnings) {
        assert.ok(!JSON.stringify(warning).includes("reconciliation-secret-sentinel"));
        assert.ok(JSON.stringify(warning).length <= 4096);
      }
      f.state.now += 60_000;
      f.state.finalError = undefined;
      f.client.getRecoveryCaptureStatus = status;
      await restarted.resumePending();
      const after = (await restarted.inspect(before.runId))[0]!;
      assert.equal(after.finalAcknowledged, true);
      assert.deepEqual(after.finalRequest, before.finalRequest);
      assert.equal(after.holdSettlement, undefined);
      assert.equal(f.state.produced, 1); assert.equal(f.state.uploads, 1);
      assert.equal(f.state.pinDeletes, 0);
    } finally { await f.close(); }
  });
}

it("M3 persisted matching receipt still observes cancellation budget before real ACK", async () => {
  const f = await v2FrozenFixture("ci_fix", true);
  let budget: MaintenanceBudget | undefined, factories = 0, finals = 0;
  try {
    f.state.finalError = new Error("save FINAL");
    await f.capture(f.record);
    const before = (await f.coordinator.inspect(f.context.runId))[0]!;
    const exact = (f.client as RecoveryArchiveClient).getExactRecoveryHold!;
    const closed = { ...(await exact(before.runId, 7, before.exactHoldBinding!))!,
      state: "released" as const, release_evidence: "archive" as const,
      final_receipt: before.finalRequest!.disposition, run_status: "cancelled" as const };
    delete closed.live_run_id; delete closed.live_worker_id;
    await patchAuthenticated(f, before, { holdSettlement: closed });
    Object.assign(f.client, { getExactRecoveryHold: async () => closed });
    f.git.discoverOwedCandidates = async () => [];
    f.state.finalError = undefined;
    const release = f.client.releaseRecoveryCustody;
    f.client.releaseRecoveryCustody = async (...args) => {
      finals++;
      assert.ok(budget); assert.equal(MaintenanceBudget.current(), budget);
      assert.equal(args[4], budget.signal);
      assert.deepEqual(args[3], before.finalRequest!.disposition);
      return release(...args);
    };
    const restarted = f.make(true, undefined, undefined, { maintenanceBudget: () => {
      factories++; return budget = new MaintenanceBudget();
    } });
    await restarted.resumePending();
    assert.equal(factories, 1); assert.equal(finals, 1);
    assert.equal((await restarted.inspect(before.runId))[0]!.finalAcknowledged, true);
  } finally { budget?.dispose(); await f.close(); }
});

it("M3 changed immutable request and disappeared record reset local retry identity", async () => {
  const { f, before } = await pendingReceiptFixture();
  try {
    f.git.discoverOwedCandidates = async () => [];
    f.state.finalError = new RequestError("POST", "/release", 409, JSON.stringify({ reason: "manifest_conflict" }));
    const restarted = f.make();
    await restarted.resumePending();
    const initial = f.finals.length;
    await restarted.resumePending();
    assert.equal(f.finals.length, initial, "unchanged request remains delayed");
    const changed = { ...before.finalRequest!, evidence: "archive" as const };
    await patchAuthenticated(f, before, { finalRequest: changed });
    await restarted.resumePending();
    assert.equal(f.finals.length, initial + 1, "different authenticated request gets its own first attempt");
    assert.deepEqual(f.finals.at(-1)![3], changed.disposition);
    assert.equal(f.finals.at(-1)![2], "archive");
    await restarted.resumePending();
    assert.equal(f.finals.length, initial + 1, "changed request establishes its own delay");
    const recordPath = path.join(f.root, "journal", before.runId, before.captureId + ".json");
    const saved = await fs.readFile(recordPath);
    await fs.unlink(recordPath);
    await restarted.resumePending();
    await fs.writeFile(recordPath, saved);
    await restarted.resumePending();
    assert.equal(f.finals.length, initial + 2, "an authoritative absent snapshot prunes process-local delay");
    assert.equal(f.state.uploads, 1); assert.equal(f.state.produced, 1);
    assert.equal(f.state.pinDeletes, 0);
  } finally { await f.close(); }
});

// V2 records retain original generation and preparation policy across worker drift.
async function v2FrozenFixture(kind: PositiveOwedCandidateContext["kind"] = "issue", receiptModel = false) {
  const f = await fixture(true, os.tmpdir(), receiptModel, receiptModel ? "00000000-0000-4000-8000-000000000001" : "run-1");
  const order: string[] = [];
  const holdId = "00000000-0000-4000-8000-000000000007";
  const repoId = "00000000-0000-4000-8000-000000000008";
  const publicationId = "00000000-0000-4000-8000-000000000009";
  let hold: "open" | "discarded" | "unknown" = "open";
  Object.assign(f.client, {
    hasHeldPublicationV2: () => true,
    listRecoveryHolds: async () => ({ run_id: f.context.runId, holds: [{
      hold_id: holdId, generation: 7, inventory_guarded: true, has_available_capture: false,
    }] }),
    getExactRecoveryHold: async () => hold === "unknown" ? undefined : ({
      run_id: f.context.runId, hold_id: holdId, repo_id: repoId, generation: 7, state: hold,
      original_worker_id: "original", original_worker_identity: "original", inventory_guarded: true,
      run_status: f.state.status, current_claim_generation: f.state.ownGeneration, claim_released: true,
      ...(hold === "open" ? { live_worker_id: "original", live_run_id: f.context.runId } : { release_evidence: "owner_discard" }),
    }),
    publishHeld: async (run: string, gen: number, tip: string, digest: string) => {
      order.push("held");
      assert.equal(gen, 7);
      const saved = (await f.coordinator.inspect(run)).find(r => r.generation === gen)!;
      assert.equal(tip, saved.sourceSha);
      assert.equal(digest, saved.coverageDigest);
      return { ok: true, body: { publication_id: publicationId, ref: `refs/uzi-held/${run}/${gen}`, tip, state: "created" } };
    },
  });
  Object.assign(f.git, {
    resolveCheckpointRange: async () => { order.push("range"); return { frozenRange: true }; },
    secretScanCheckpointRange: async (_bare: string, range: unknown) => {
      assert.deepEqual(range, { frozenRange: true }); order.push("scan");
      return { trusted: true, findings: [] };
    },
    rangeTouchesWorkflows: async () => { order.push("workflow"); return "clean"; },
    changedFilesNoRenames: async () => { order.push("ci"); return []; },
    checkpointPack: async (_bare: string, _branch: string, _overlay: unknown, range: unknown,
      _signal: unknown, tip: string) => {
      assert.deepEqual(range, { frozenRange: true }); order.push("pack");
      return { tipOid: tip, pack: Readable.from([Buffer.from("PACK")]), exited: Promise.resolve(0) };
    },
  });
  f.state.onProduce = () => order.push("archive");
  f.context.kind = kind;
  for (const candidate of f.state.candidates) for (const ctx of candidate.contexts) ctx.kind = kind;
  const record = await f.coordinator.freezeInventory({
    context: f.context, currentSha: H, defaultBranch: "main", repoId,
    heldPolicy: { version: 1, blocked: false, everKnown: false, flaggedCommits: [],
      ciGlobs: [".custom-ci/**"], extraFloors: [] },
  });
  assert.ok(record);
  f.state.ownGeneration = 8;
  const operate = () => f.coordinator.operateFrozenInventory({
    record, barePath: f.context.barePath, defaultBranch: "main",
  });
  return { ...f, order, record, operate, setHold: (state: typeof hold) => { hold = state; } };
}

function closedExactFixture(record: RecoveryRecord, exact: RecoveryExactHold, state: "released" | "discarded"): RecoveryExactHold {
  return { ...exact, state, live_worker_id: undefined, live_run_id: undefined,
    release_evidence: state === "discarded" ? "owner_discard" : "held_publication",
    final_receipt: state === "discarded" ? undefined : {
      kind: "held_publication", publication_id: "00000000-0000-4000-8000-000000000009",
      source_sha: record.sourceSha, coverage_digest: record.coverageDigest!,
    },
  };
}

for (const delivered of [false, true]) {
  it(`review G1 lost held create answer recovers on the same frozen identity delivered=${delivered}`, async () => {
    const f = await v2FrozenFixture();
    let calls = 0, creates = 0, invoked = false;
    const publish = Reflect.get(f.client, "publishHeld") as NonNullable<RecoveryArchiveClient["publishHeld"]>;
    Object.assign(f.client, { publishHeld: async (...args: Parameters<NonNullable<RecoveryArchiveClient["publishHeld"]>>) => {
      calls++;
      if (calls === 1) {
        if (delivered) { invoked = true; creates++; }
        throw new Error("connection lost");
      }
      // The existing API reconciles a committed invocation before reading the body.
      if (!invoked) {
        if (!(args[4] instanceof Buffer) || args[4].length === 0) return { ok: false, reason: "pack_invalid", httpStatus: 400 };
        invoked = true; creates++;
      }
      return publish(...args);
    } });
    try {
      await f.operate();
      const pending = (await f.coordinator.inspect(f.context.runId))[0]!;
      assert.equal(pending.heldCreateUncertain, true);
      await f.operate();
      const saved = (await f.coordinator.inspect(f.context.runId))[0]!;
      assert.equal(saved.finalAcknowledged, true, "a request never delivered must not remain an empty-pack probe forever");
      assert.equal(saved.sourceSha, f.record.sourceSha);
      assert.equal(saved.coverageDigest, f.record.coverageDigest);
      assert.equal(saved.captureId, f.record.captureId);
      assert.equal(creates, 1, "the API invocation marker prevents a second remote create");
      assert.equal(f.state.produced, 0);
    } finally { await f.close(); }
  });
}

it("review G2 V1 rollback replays a lost held FINAL without exact observations or new preservation", async () => {
  const f = await v2FrozenFixture("issue", true);
  const release = f.client.releaseRecoveryCustody;
  let loseAnswer = true;
  Object.assign(f.client, { releaseRecoveryCustody: async (...args: Parameters<typeof release>) => {
    const ack = await release(...args);
    if (loseAnswer) { loseAnswer = false; throw new Error("FINAL answer lost"); }
    return ack;
  } });
  try {
    await f.operate();
    const pending = (await f.coordinator.inspect(f.context.runId))[0]!;
    assert.ok(pending.finalRequest);
    assert.equal(pending.finalAcknowledged, undefined);
    const original = structuredClone(pending.finalRequest);
    Object.assign(f.client, { hasHeldPublicationV2: () => false, getExactRecoveryHold: async () => undefined });
    await f.operate();
    assert.equal(f.finals.length, 2, "old API receipt replay precedes V2 observation eligibility");
    assert.deepEqual(f.finals[1]![3], original.disposition);
    assert.equal((await f.coordinator.inspect(f.context.runId))[0]!.finalAcknowledged, true);
    assert.equal(f.order.filter(step => step === "held").length, 1);
    assert.equal(f.state.produced, 0);
  } finally { await f.close(); }
});

for (const shape of ["65 heads", "large commit", "non-UTF8 commit"] as const) {
  it(`review G3 archive FINAL accepts producer-valid coverage with ${shape}`, async () => {
    const f = await fixture();
    try {
      const data = path.join(f.root, "data");
      const bare = path.join(data, "repos", "repo.git");
      await fs.mkdir(path.dirname(bare), { recursive: true, mode: 0o700 });
      const gitEnv = Object.fromEntries(Object.entries(process.env).filter(([key]) => !key.startsWith("GIT_")));
      Object.assign(gitEnv, { GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" });
      execFileSync("git", ["init", "--bare", bare], { env: gitEnv, stdio: "pipe", timeout: 10000 });
      const git = (args: string[], input?: Buffer | string) => execFileSync("git", ["--git-dir", bare, ...args],
        { env: gitEnv, input, encoding: "utf8", timeout: 10000 }).trim();
      const tree = git(["mktree"], "");
      const roots: string[] = [];
      for (let i = 0; i < (shape === "65 heads" ? 65 : 1); i++) {
        const message = shape === "large commit" ? Buffer.alloc(70 * 1024, 120) :
          shape === "non-UTF8 commit" ? Buffer.from([0xff, 0xfe, 0x0a]) : Buffer.from(`root ${i}\n`);
        const body = Buffer.concat([Buffer.from(`tree ${tree}\nauthor Fixture <fixture@example.com> 946684800 +0000\ncommitter Fixture <fixture@example.com> 946684800 +0000\n\n`), message]);
        roots.push(git(["hash-object", "-t", "commit", "-w", "--stdin"], body));
      }
      f.context.barePath = bare;
      f.context.defaultIdentity = { ref: "refs/remotes/origin/main", sha: roots[0]! };
      f.state.candidates = roots.map(sha => ({ sha, pinRef: `refs/owed/${sha}`, contexts: [structuredClone(f.context)] }));
      f.state.ancestors = new Set(roots);
      const cache = new GitCache(data, nullLogger(), undefined, testGitCacheOptions());
      Object.assign(f.git, { buildRecoveryCoverage: cache.buildRecoveryCoverage.bind(cache),
        readRecoveryCoverageClaim: cache.readRecoveryCoverageClaim.bind(cache) });
      const record = await f.coordinator.freezeInventory({ context: f.context,
        currentSha: roots[0]!, originalSourceSha: roots[0]!, defaultBranch: "main" });
      assert.ok(record);
      assert.equal(await cache.readRecoveryCoverageClaim(bare, { sourceSha: record.sourceSha,
        fingerprint: record.coverageDigest!, roots: [...roots].sort(), currentSha: roots[0]! }), undefined,
      "optional redundancy proof intentionally refuses this producer-valid input");
      await f.capture(record);
      assert.equal(f.state.uploads, 1);
      assert.equal(f.finals.length, 1, "optional redundancy limits must not block ordinary archive FINAL");
      assert.equal((await f.coordinator.inspect(f.context.runId))[0]!.finalAcknowledged, true);
    } finally { await f.close(); }
  });
}

it("late cancellation releases queued capture before the holder and preserves FIFO", async () => {
  const f = await v2FrozenFixture();
  let release!: () => void, ready!: () => void;
  const held = new Promise<void>(resolve => { ready = resolve; });
  const enumerate = f.git.enumerateOwedCandidates!;
  let calls = 0;
  Object.assign(f.git, { enumerateOwedCandidates: async (...args: Parameters<typeof enumerate>) => {
    if (++calls === 1) { ready(); await new Promise<void>(resolve => { release = resolve; }); }
    return enumerate(...args);
  } });
  const input = { context: f.context, currentSha: H, defaultBranch: "main", locallyQuiescent: true };
  const holder = f.coordinator.freezeInventory(input);
  await held;
  let mono = 0, settled = false, expire!: () => void;
  let budget: MaintenanceBudget | undefined;
  const waiter = MaintenanceBudget.runAttempt("run-1:7", () => budget = new MaintenanceBudget(100, () => mono), () => {
    expire = AsyncLocalStorage.bind(() => { MaintenanceBudget.observeCancellation(); mono = 100; assert.throws(() => budget!.check()); });
    return f.coordinator.freezeInventory(input);
  });
  const result = waiter.then(() => { settled = true; }, () => { settled = true; });
  const sibling = f.coordinator.freezeInventory(input);
  try {
    await new Promise<void>(resolve => setImmediate(resolve));
    expire();
    await new Promise<void>(resolve => setImmediate(resolve));
    assert.equal(settled, true, "late cancellation must release queued capture waiter at its deadline");
    assert.equal(calls, 1, "sibling cannot overtake holder");
  } finally {
    release(); await Promise.all([holder, result, sibling]); budget?.dispose(); await f.close();
  }
  assert.equal(calls, 2, "expired callback never enumerates");
});

it("late cancellation releases queued journal pin without late mutation or FIFO overtake", async t => {
  const f = await fixture();
  let release!: () => void, ready!: () => void;
  const held = new Promise<void>(resolve => { ready = resolve; });
  const rename = fs.rename.bind(fs);
  let renames = 0;
  t.mock.method(fs, "rename", async (...args: Parameters<typeof rename>) => {
    if (++renames === 1) { ready(); await new Promise<void>(resolve => { release = resolve; }); }
    return rename(...args);
  });
  let observed!: () => void, observations = 0;
  const queued = new Promise<void>(resolve => { observed = resolve; });
  // The holder observes first (1). The waiter's and the sibling's journal reads finish in
  // either order, so the sibling waits for the waiter: expiry must find the waiter past its
  // read and queued on the lock, not still reading (#3220).
  let waiterObserved!: () => void;
  const waiterRead = new Promise<void>(resolve => { waiterObserved = resolve; });
  const writer = f.make(false, undefined, undefined, { maintenanceBudget: () => new MaintenanceBudget(),
    afterPinObserve: async () => {
      if (MaintenanceBudget.isAttempt("journal")) waiterObserved();
      else if (observations >= 1) await waiterRead;
      if (++observations === 3) observed();
    } });
  const pin = (sha: string) => writer.pin({ runId: "run-1", kind: "issue", branch: "task", sourceSha: sha });
  const holder = pin(H);
  await held;
  let mono = 0, outcome: "pending" | "fulfilled" | "rejected" = "pending", expire!: () => void;
  let budget: MaintenanceBudget | undefined;
  const waiter = MaintenanceBudget.runAttempt("journal", () => budget = new MaintenanceBudget(100, () => mono), () => {
    expire = AsyncLocalStorage.bind(() => { MaintenanceBudget.observeCancellation(); mono = 100; assert.throws(() => budget!.check()); });
    return pin(H2);
  });
  const result = waiter.then(() => { outcome = "fulfilled"; }, () => { outcome = "rejected"; });
  const sibling = pin("c".repeat(40));
  let drained: PromiseSettledResult<unknown>[] = [];
  try {
    try {
      await queued;
      await new Promise<void>(resolve => setImmediate(resolve));
      expire();
      await new Promise<void>(resolve => setImmediate(resolve));
      assert.equal(outcome, "rejected", "late journal waiter rejects while holder remains held");
      assert.equal(renames, 1);
    } finally {
      // Drain every pin before the fixture closes, even when one rejects.
      release();
      try { drained = await Promise.allSettled([holder, result, sibling]); } finally { budget?.dispose(); }
    }
    for (const settled of drained) if (settled.status === "rejected") throw settled.reason;
    assert.equal(renames, 2, "expired pin never writes");
    assert.deepEqual((await f.coordinator.inspect("run-1")).map(r => r.sourceSha).sort(), [H, "c".repeat(40)].sort());
  } finally { await f.close(); }
});

it("first cancellation inside freeze shares the flight budget through later maintenance", async () => {
  const f = await v2FrozenFixture();
  let mono = 0, observations = 0, builds = 0;
  const budget = new MaintenanceBudget(600_000, () => mono);
  const build = f.git.buildRecoveryCoverage!;
  Object.assign(f.client, { getRunOwnership: async () => ({
    status: "running", claim_generation: 8, inventory_guarded: true,
  }) });
  Object.assign(f.git, { buildRecoveryCoverage: async (...args: Parameters<typeof build>) => {
    assert.equal(MaintenanceBudget.current(), budget);
    builds++; mono += 200_000;
    return build(...args);
  } });
  f.state.status = "cancelled";
  try {
    const record = await f.coordinator.freezeInventory({
      context: f.context, currentSha: H, defaultBranch: "main", repoId: f.record.exactHoldBinding!.repoId,
      onCancellationObserved: () => { observations++; return budget; },
    });
    assert.ok(record);
    assert.equal(observations, 1);
    assert.equal(builds, 1);
    assert.equal(budget.remainingMs(), 400_000);
    mono = 600_000;
    assert.throws(() => MaintenanceBudget.run(budget, () => f.coordinator.operateFrozenInventory({
      record, barePath: f.context.barePath, defaultBranch: "main",
    })), /maintenance deadline/);
    assert.equal(f.order.includes("pack"), false);
    assert.equal(f.finals.length, 0);
  } finally { budget.dispose(); await f.close(); }
});

for (const mode of ["boot", "live"] as const) {
  it(`cancellation entry ${mode} reuses one exact attempt through Git and FINAL despite worker drift`, async () => {
    const f = await v2FrozenFixture();
    let mono = 0, factories = 0;
    const budget = new MaintenanceBudget(600_000, () => mono);
    const seen: string[] = [];
    const operationSignals: AbortSignal[] = [];
    const stage = (name: string) => {
      assert.equal(MaintenanceBudget.current(), budget);
      seen.push(name);
      mono += 50_000;
      f.state.now += seen.length % 2 ? 9_000_000 : -18_000_000;
    };
    const release = f.client.releaseRecoveryCustody;
    const pack = (f.git as unknown as HeldGit).checkpointPack!;
    Object.assign(f.git, {
      discoverOwedCandidates: async () => [],
      withRecoveryOperation: async (signal: AbortSignal, deadline: number, action: () => Promise<unknown>) => {
        assert.equal(deadline, Infinity);
        assert.equal(signal.aborted, false);
        // Pass ownership is installed before a cancellation observation creates its budget.
        if (MaintenanceBudget.current()) {
          stage("operation");
          operationSignals.push(signal);
        }
        return action();
      },
      checkpointPack: async (...args: Parameters<typeof pack>) => { stage("pack"); return pack(...args); },
    });
    Object.assign(f.client, {
      getRunOwnership: async () => { throw ownershipLost(); },
      releaseRecoveryCustody: async (...args: unknown[]) => {
        stage("FINAL");
        assert.ok(args[4] instanceof AbortSignal);
        assert.equal(args[4].aborted, false);
        operationSignals.push(args[4]);
        return release(...args);
      },
    });
    f.state.status = "cancelled";
    if (mode === "boot") f.state.ownGeneration = 7;
    const coordinator = f.make(true, undefined, undefined, { maintenanceBudget: () => { factories++; return budget; } });
    try {
      if (mode === "boot") await coordinator.resumePending(undefined, [f.record]);
      else await coordinator.resumeLive({ isExecuting: () => false, authenticatedAtMs: 1000 });
      assert.equal(factories, 1);
      assert.ok(seen.includes("pack")); assert.ok(seen.includes("FINAL"));
      assert.equal(f.order.includes("held"), true);
      mono = 600_000;
      assert.throws(() => budget.check(), /maintenance deadline/);
      assert.ok(operationSignals.length > 0);
      assert.ok(operationSignals.every(signal => signal.aborted), "all Git/FINAL signals retain the same attempt expiry");
    } finally { budget.dispose(); await f.close(); }
  });
}

it("cancellation entry expired boot queue is inert and a later pass gets a fresh attempt", async () => {
  const f = await v2FrozenFixture();
  let unblock!: () => void, entered!: () => void, scoped!: () => void;
  const blocked = new Promise<void>(resolve => { unblock = resolve; });
  const ready = new Promise<void>(resolve => { entered = resolve; });
  const active = new Promise<void>(resolve => { scoped = resolve; });
  let mono = 0, factories = 0;
  const budgets: MaintenanceBudget[] = [];
  const enumerate = f.git.enumerateOwedCandidates;
  Object.assign(f.git, {
    enumerateOwedCandidates: async () => { entered(); await blocked; return enumerate(); },
    withRecoveryOperation: async (_signal: AbortSignal, deadline: number, action: () => Promise<unknown>) => {
      assert.equal(deadline, Infinity);
      if (MaintenanceBudget.current()) scoped();
      return action();
    },
  });
  const holder = f.coordinator.freezeInventory({ context: f.context, currentSha: H, defaultBranch: "main" });
  await ready;
  f.state.status = "cancelled";
  const coordinator = f.make(true, undefined, undefined, { maintenanceBudget: () => {
    factories++; const b = new MaintenanceBudget(600_000, () => mono); budgets.push(b); return b;
  } });
  try {
    const pending = coordinator.resumePending(undefined, [f.record]);
    await active;
    await new Promise<void>(resolve => setImmediate(resolve));
    mono = 600_000;
    assert.throws(() => budgets[0]!.check(), /maintenance deadline/);
    await assert.rejects(pending, /maintenance deadline/);
    assert.equal(f.order.includes("held"), false);
    assert.equal(f.finals.length, 0); assert.equal(f.state.pinDeletes, 0);
    unblock(); await holder;
    await coordinator.resumePending(undefined, [f.record]);
    assert.equal(factories, 2);
    assert.equal(f.order.includes("held"), true);
  } finally { unblock(); await holder; for (const b of budgets) b.dispose(); await f.close(); }
});

it("cancellation entry does not renew an expired inherited attempt", async () => {
  const f = await v2FrozenFixture();
  let mono = 0, factories = 0;
  const budget = new MaintenanceBudget(1, () => mono);
  const coordinator = f.make(true, undefined, undefined, { maintenanceBudget: () => { factories++; return new MaintenanceBudget(); } });
  try {
    await MaintenanceBudget.run(budget, async () => {
      mono = 1;
      await assert.rejects(coordinator.resumePending(undefined, [f.record]), /maintenance deadline/);
    });
    assert.equal(factories, 0); assert.equal(f.order.length, 0);
  } finally { budget.dispose(); await f.close(); }
});

for (const mode of ["boot", "live"] as const) {
  it(`cancellation handoff ${mode} keeps cumulative expiry across materialization and repeated records`, async () => {
    const f = await v2FrozenFixture();
    let mono = 0, factories = 0;
    f.state.status = "cancelled";
    // Cancellation first appears in the inactive recheck, after the entry read was running.
    const exact = (f.client as RecoveryArchiveClient).getExactRecoveryHold!;
    let reads = 0;
    Object.assign(f.client, { getExactRecoveryHold: async () => {
      const result = await exact("run-1", 7, f.record.exactHoldBinding!);
      return ++reads === 1 ? { ...result!, run_status: "running" } : result;
    } });
    const coordinator = f.make(true, undefined, undefined, { maintenanceBudget: () => {
      factories++;
      return new MaintenanceBudget(600_000, () => mono);
    } });
    const materialize = coordinator.materializeBootInventory.bind(coordinator);
    Object.assign(coordinator, {
      materializeBootInventory: async (...args: Parameters<typeof materialize>) => {
        const records = await materialize(...args);
        mono = 600_000;
        return [...records, f.record];
      },
    });
    try {
      await coordinator.snapshotOwedInventory();
      if (mode === "boot") await assert.rejects(coordinator.resumePending(undefined, [f.record, f.record], true), /maintenance deadline/);
      else await coordinator.resumeLive({ isExecuting: () => false, authenticatedAtMs: 1000 });
      assert.equal(factories, 1);
      assert.equal(f.order.includes("pack"), false);
      assert.equal(f.finals.length, 0);
      assert.equal(f.state.pinDeletes, 0);
    } finally { await f.close(); }
  });
}

it("cancellation transition from running entry to exact terminal starts budget before new pack or FINAL", async () => {
  const f = await v2FrozenFixture();
  let mono = 0, factories = 0, reads = 0;
  const exact = (f.client as RecoveryArchiveClient).getExactRecoveryHold!;
  Object.assign(f.client, {
    getExactRecoveryHold: async () => {
      f.state.status = ++reads === 1 ? "running" : "cancelled";
      return exact("run-1", 7, f.record.exactHoldBinding!);
    },
  });
  const range = (f.git as unknown as HeldGit).resolveCheckpointRange!;
  Object.assign(f.git, {
    resolveCheckpointRange: async (...args: Parameters<typeof range>) => {
      assert.ok(MaintenanceBudget.current(), "the first terminal read activates the existing continuation");
      const result = await range(...args);
      mono = 600_000;
      return result;
    },
  });
  const coordinator = f.make(true, undefined, undefined, { maintenanceBudget: () => {
    factories++; return new MaintenanceBudget(600_000, () => mono);
  } });
  try {
    await assert.rejects(coordinator.operateFrozenInventory({ record: f.record, barePath: f.context.barePath, defaultBranch: "main" }), /maintenance deadline/);
    assert.equal(factories, 1);
    assert.equal(f.order.includes("pack"), false);
    assert.equal(f.finals.length, 0);
  } finally { await f.close(); }
});

it("maintenance expiry after held ACK journal completion cannot notify release or delete pins", async () => {
  const f = await v2FrozenFixture();
  let now = 0, released = 0;
  const budget = new MaintenanceBudget(600_000, () => now);
  const write = f.coordinator["writeExistingRecord"].bind(f.coordinator);
  Object.assign(f.coordinator, {
    onAuthoritativeGenerationReleased: () => { released++; },
    writeExistingRecord: async (...args: Parameters<typeof write>) => {
      const result = await write(...args);
      if (result.finalAcknowledged) now = 600_000;
      return result;
    },
  });
  try {
    await assert.rejects(MaintenanceBudget.run(budget, () => f.operate()), /maintenance deadline/);
    assert.equal(released, 0, "expiry after the join cannot create a cleanup hint");
    assert.equal(f.state.pinDeletes, 0);
  } finally { budget.dispose(); await f.close(); }
});

it("M3 remembered held scan finding survives restart and a later clean scan", async () => {
  const f = await v2FrozenFixture();
  try {
    f.state.failProduce = true;
    Object.assign(f.git, { secretScanCheckpointRange: async () => ({ trusted: true, findings: [{ commit: H }] }) });
    await f.operate();
    assert.equal((await f.coordinator.inspect("run-1"))[0]?.heldPolicy?.blocked, true);
    Object.assign(f.git, { secretScanCheckpointRange: async () => ({ trusted: true, findings: [] }) });
    await f.make(true).resumePending(undefined, [f.record]);
    assert.equal(f.order.includes("held"), false);
  } finally { await f.close(); }
});

it("V1 deferred failed inventory without held policy archives the same frozen record", async () => {
  const f = await fixture(true);
  let heldCalls = 0;
  Object.assign(f.client, {
    hasHeldPublicationV2: () => false,
    publishHeld: async () => { heldCalls++; throw new Error("missing policy cannot authorize held publication"); },
  });
  // Include a second owed root so archiving only the current head cannot pass.
  f.state.candidates.push({ sha: H2, pinRef: "refs/owed/b", contexts: [structuredClone(f.context)] });
  f.state.ancestors.add(H2);
  try {
    const record = await f.coordinator.freezeInventory({
      context: f.context, currentSha: H, defaultBranch: "main",
    });
    assert.ok(record);
    assert.equal(record.heldPolicy, undefined);
    assert.equal(f.state.produced, 0, "capture was deferred at freeze");
    const aggregates = structuredClone(f.aggregates);
    assert.deepEqual(aggregates, [[H, H2]]);
    const result = await f.coordinator.operateFrozenInventory({
      record, barePath: f.context.barePath, defaultBranch: "main",
    }, { foreground: true, terminal: { status: "failed" } });
    assert.equal(result.state, "uploaded");
    assert.equal(result.captureId, record.captureId);
    assert.equal(heldCalls, 0);
    assert.equal(f.state.produced, 1);
    assert.equal(f.state.uploads, 1);
    for (const roots of f.aggregates) assert.deepEqual(roots, aggregates[0], "coverage revalidation keeps every frozen root");
    assert.deepEqual(f.reserveKeys, [record.captureId]);
    assert.deepEqual(f.finals, [[record.runId, record.generation, undefined, {
      kind: "archive", capture_id: "server-1", source_sha: record.sourceSha,
      coverage_digest: record.coverageDigest,
    }]]);
    const [saved] = await f.coordinator.inspect(record.runId);
    assert.ok(saved);
    assert.equal(saved.captureId, record.captureId);
    assert.equal(saved.sourceSha, record.sourceSha);
    assert.equal(saved.coverageDigest, record.coverageDigest);
    assert.deepEqual(saved.originalRoots, record.originalRoots);
    assert.equal(saved.finalAcknowledged, true);
  } finally { await f.close(); }
});

for (const foreground of [false, true]) {
  it(`B exact terminal observation survives ownership drift foreground=${foreground}`, async () => {
    const f = await v2FrozenFixture();
    try {
      Object.assign(f.client, { getRunOwnership: async () => { throw ownershipLost(); } });
      const result = await f.coordinator.operateFrozenInventory({
        record: f.record, barePath: f.context.barePath, defaultBranch: "main",
      }, { foreground, terminal: foreground ? { status: "failed" } : undefined });
      assert.equal(result.held, "acked");
      assert.ok(f.order.includes("held"));
      assert.equal(f.state.produced, 0);
    } finally { await f.close(); }
  });
}

for (const invalid of ["generation", "status", "repo"] as const) {
  it(`B foreground refuses held publication on exact observation ${invalid} mismatch`, async () => {
    const f = await v2FrozenFixture();
    try {
      const observe = (f.client as RecoveryArchiveClient).getExactRecoveryHold!;
      Object.assign(f.client, { getExactRecoveryHold: async () => {
        const exact = await observe(f.record.runId, f.record.generation!, f.record.exactHoldBinding!);
        return { ...exact, ...(invalid === "generation" ? { generation: 8 } :
          invalid === "status" ? { run_status: "running" } : { repo_id: "foreign" }) };
      } });
      await f.coordinator.operateFrozenInventory({
        record: f.record, barePath: f.context.barePath, defaultBranch: "main",
      }, { foreground: true, terminal: { status: "failed" } });
      assert.equal(f.order.includes("held"), false);
    } finally { await f.close(); }
  });
}

for (const readback of ["created", "create_unknown", "refused", "pack_invalid", "mismatch", "settled"]) {
  it(`B unknown create restart readback ${readback} preserves the invocation fence`, async () => {
    const f = await v2FrozenFixture();
    try {
      let sends = 0, reads = 0;
      Object.assign(f.client, {
        publishHeld: async (run: string, gen: number, tip: string, digest: string, pack: Buffer) => {
          assert.equal(gen, f.record.generation);
          assert.equal(tip, f.record.sourceSha);
          assert.equal(digest, f.record.coverageDigest);
          if (pack.length) { sends++; throw new Error("lost create response"); }
          reads++;
          if (readback === "pack_invalid") return { ok: false, httpStatus: 400, reason: "pack_invalid" };
          return { ok: true, body: {
            publication_id: "00000000-0000-4000-8000-000000000009",
            ref: `refs/uzi-held/${run}/${gen}`,
            tip: readback === "mismatch" ? H2 : tip,
            state: readback === "mismatch" ? "created" : readback,
            ...(readback === "refused" ? { reason: "create_refused" } : {}),
          } };
        },
      });
      assert.equal((await f.operate()).held, "kept");
      assert.equal((await f.coordinator.inspect("run-1"))[0]?.heldCreateUncertain, true);
      const before = [...f.order];
      if (readback === "settled") f.setHold("discarded");
      const restarted = f.make(true);
      await restarted.resumePending(undefined, [f.record]);
      assert.equal(sends, readback === "pack_invalid" ? 2 : 1);
      assert.equal(reads, readback === "settled" ? 0 : 1);
      if (readback !== "pack_invalid") assert.deepEqual(f.order.filter(x => x !== "archive"), before);
      if (readback === "created") {
        assert.equal((f.finals[0]?.[3] as RecoveryFinalDisposition)?.kind, "held_publication");
      } else if (readback === "refused") {
        assert.equal(f.state.produced, 1);
      } else {
        assert.equal(f.state.produced, 0);
        assert.equal(f.finals.length, 0);
        assert.equal((await restarted.inspect("run-1"))[0]?.heldCreateUncertain, true);
      }
    } finally { await f.close(); }
  });
}


for (const hold of ["open", "discarded", "unknown"] as const) {
  it(`M4 saved held FINAL definitive refusal keeps archive fallback with hold ${hold}`, async () => {
    const f = await v2FrozenFixture();
    try {
      f.state.finalError = new Error("initial FINAL unavailable");
      await f.operate();
      const saved = (await f.coordinator.inspect(f.context.runId)).find(r => r.captureId === f.record.captureId)!;
      assert.equal(saved.finalRequest?.disposition.kind, "held_publication");
      assert.equal(f.order.filter(x => x === "held").length, 1);
      f.state.finalError = undefined;
      const release = f.client.releaseRecoveryCustody;
      Object.assign(f.client, {
        releaseRecoveryCustody: async (...args: Parameters<typeof release>) => {
          if ((args[3] as RecoveryFinalDisposition | undefined)?.kind !== "held_publication") return release(...args);
          f.setHold(hold);
          return { run_id: args[0], generation: args[1], released: false, holds_released: 0,
            retained: true, reason: "held_publication_ref_missing" };
        },
        publishHeld: async () => {
          f.order.push("held");
          throw new TypeError("held retry transport unavailable");
        },
      });
      await f.make(true).resumePending(undefined, [saved]);
      assert.equal(f.state.produced, hold === "open" ? 1 : 0,
        "definitive FINAL refusal must reach archive fallback only while the exact hold is open");
      assert.equal(f.order.filter(x => x === "held").length, 1, "saved FINAL fallback must not republish held work");
      const after = (await f.coordinator.inspect(f.context.runId)).find(r => r.captureId === saved.captureId)!;
      assert.equal(after.finalAcknowledged === true, hold === "open", "only the actual archive FINAL ACK grants release");
      if (hold === "open") {
        assert.equal(after.captureId, saved.captureId);
        assert.equal(after.sourceSha, saved.sourceSha);
        assert.equal(after.coverageDigest, saved.coverageDigest);
        assert.equal((f.finals.at(-1)?.[3] as RecoveryFinalDisposition)?.kind, "archive");
      } else {
        assert.equal(f.state.uploads, 0);
        assert.equal(f.state.pinDeletes, 0);
      }
    } finally { await f.close(); }
  });
}

for (const answer of ["refused", "invoked", "create_unknown", "503", "malformed"] as const) {
  it(`B V2 initial create ${answer} preserves protocol-specific fallback`, async () => {
    const f = await v2FrozenFixture();
    try {
      Object.assign(f.client, { publishHeld: async (run: string, gen: number, tip: string) => {
        if (answer === "503") return { ok: false, httpStatus: 503, reason: "forge_unavailable" };
        if (answer === "malformed") return { ok: false, reason: "malformed_response" };
        return { ok: true, body: {
          publication_id: "00000000-0000-4000-8000-000000000009",
          ref: `refs/uzi-held/${run}/${gen}`, tip, state: answer,
          ...(answer === "refused" ? { reason: "create_refused" } : {}),
        } };
      } });
      await f.operate();
      const saved = (await f.coordinator.inspect("run-1"))[0]!;
      assert.equal(saved.captureId, f.record.captureId);
      assert.equal(saved.sourceSha, f.record.sourceSha);
      assert.equal(saved.coverageDigest, f.record.coverageDigest);
      assert.equal(f.state.produced, answer === "refused" ? 1 : 0);
      assert.equal(saved.heldCreateUncertain, answer !== "refused");
      if (answer === "refused") {
        assert.equal(saved.finalAcknowledged, true, "the complete archive fallback has its own authoritative FINAL");
        assert.equal((f.finals[0]?.[3] as RecoveryFinalDisposition)?.kind, "archive");
      } else {
        assert.notEqual(saved.finalAcknowledged, true);
        assert.equal(f.finals.length, 0, "uncertain create cannot mint FINAL or ACK");
      }
    } finally { await f.close(); }
  });
}

it("B throwing pack stream waits for controlled child exit", async () => {
  const f = await v2FrozenFixture();
  try {
    let release!: (code: number) => void;
    let failed!: () => void;
    const streamFailed = new Promise<void>(resolve => { failed = resolve; });
    const exited = new Promise<number>(resolve => { release = resolve; });
    const pack = new Readable({ read() { failed(); this.destroy(new Error("stream failed")); } });
    Object.assign(f.git, { checkpointPack: async () => ({ tipOid: H, pack, exited }) });
    let settled = false, cancelled = 0;
    Object.assign(f.git, { checkpointPack: async () => ({ tipOid: H, pack, exited, cancel: () => { cancelled++; } }) });
    const preparing = prepareFrozenHeld({ git: f.git as unknown as HeldGit,
      barePath: f.context.barePath, branch: f.record.branch, sourceSha: H,
      policy: f.record.heldPolicy!, deadlineMs: 1000 });
    const checked = assert.rejects(preparing, /stream failed/).then(() => { settled = true; });
    await streamFailed;
    await new Promise<void>(resolve => setImmediate(resolve));
    assert.equal(settled, false);
    assert.equal(pack.destroyed, true);
    const stopRequested = cancelled;
    release(1);
    await checked;
    assert.equal(stopRequested, 1, "abandonment requests cancellation before waiting for close");
  } finally { await f.close(); }
});


for (const mode of ["boot", "live"] as const) {
  for (const refusal of ["none", "source proof", "unknown create", "FINAL refusal"] as const) {
    it(`M4 completed ISSUE current receipt earlier available thin NO receipt ${mode}/${refusal}`, async () => {
      const f = await v2FrozenFixture("issue", true);
      try {
        const runId = f.context.runId;
        f.state.status = "completed";
        f.state.thin = true;
        await f.capture(f.record);
        const earlier = (await f.coordinator.inspect(runId)).find(r => r.generation === 7)!;
        assert.equal(earlier.state, "uploaded");
        assert.equal(earlier.selfContained, false);
        assert.equal(earlier.completionReceipt, undefined);
        assert.equal(earlier.finalRequest, undefined);
        assert.equal(f.committedReceipts.size, 0);
        const captureBefore = structuredClone([...f.captures]);
        const bytes = await fs.readFile(earlier.bundlePath!);
        const listHolds = f.client.listRecoveryHolds;
        const uuid = "00000000-0000-4000-8000-000000000002";
        f.client.listRecoveryHolds = async () => ({ run_id: runId, holds: [
          { hold_id: uuid, generation: 8, inventory_guarded: true, has_available_capture: false },
          ...(await listHolds()).holds,
        ] });
        await f.coordinator.pin({ runId, generation: 8, kind: "issue", branch: "task",
          sourceSha: H2, inventoryGuarded: true });
        assert.equal(await f.coordinator.persistCompletionReceipt({
          hold_id: uuid, run_id: runId, owner_id: uuid, worker_id: uuid, generation: 8,
          final_head: H2, repo_id: f.record.exactHoldBinding!.repoId, connection_id: uuid,
          project_id: 1, forge_type: "github", base_url: "https://example.com",
          branch: "task", mr_iid: 1, observed_branch_head: H2,
        }), true);
        // The current receipt closed its own hold; only the predecessor remains open.
        f.client.listRecoveryHolds = listHolds;
        const current = (await f.coordinator.inspect(runId)).find(r => r.generation === 8)!;
        if (refusal === "source proof") f.state.sourceProofRefused = true;
        if (refusal === "unknown create") Object.assign(f.client, {
          publishHeld: async () => { f.order.push("held"); throw new Error("unknown create"); },
        });
        if (refusal === "FINAL refusal") f.state.finalError = new Error("FINAL unavailable");
        // Keep discovery focused on the existing immutable snapshot, as a boot journal replay does.
        Object.assign(f.git, { discoverOwedCandidates: async () => [] });
        const restarted = f.make(true);
        if (mode === "boot") await restarted.resumePending();
        else await restarted.resumeLive({ authenticatedAtMs: f.state.now, isExecuting: () => false });
        const records = await restarted.inspect(runId);
        assert.deepEqual(records.find(r => r.generation === 8), current, "current receipt remains byte-for-byte authority");
        const saved = records.find(r => r.captureId === earlier.captureId)!;
        assert.ok(saved);
        assert.equal(saved.sourceSha, earlier.sourceSha);
        assert.equal(saved.coverageDigest, earlier.coverageDigest);
        assert.equal(saved.completionReceipt, undefined, "current receipt never covers earlier generation");
        assert.deepEqual([...f.captures], captureBefore, "available thin manifest is frozen on the same record");
        assert.deepEqual(await fs.readFile(earlier.bundlePath!), bytes);
        assert.equal(f.reserves(), 1, "no replacement capture");
        assert.equal(f.state.produced, 1, "no extra archive");
        assert.equal(f.state.uploads, 1);
        if (refusal === "none") {
          assert.ok(f.order.includes("held"), "earlier generation must publish held despite current receipt");
          assert.equal(f.finals.length, 1);
          assert.equal(f.finals[0]![0], runId);
          assert.equal(f.finals[0]![1], 7, "FINAL targets exact earlier generation");
          assert.deepEqual(f.committedReceipts.get(7), {
            kind: "held_publication", publication_id: "00000000-0000-4000-8000-000000000009",
            source_sha: earlier.sourceSha, coverage_digest: earlier.coverageDigest,
          });
          assert.equal(saved.finalAcknowledged, true);
          assert.equal(f.state.open, false);
        } else {
          assert.equal(saved.finalAcknowledged, undefined, "refusal never fabricates ACK");
          assert.equal(f.committedReceipts.size, 0);
          assert.equal(f.state.open, true, "refusal keeps earlier custody");
          assert.equal(f.state.pinDeletes, 0);
          assert.equal(f.finals.length, refusal === "FINAL refusal" ? 1 : 0);
        }
      } finally { await f.close(); }
    });
  }
}

for (const kind of ["ci_fix", "prompt", "task"] as const) {
  it(`M4 completed independent ${kind} self-contained archive FINAL`, async () => {
    const f = await v2FrozenFixture(kind, true);
    try {
      f.state.status = "completed";
      await f.make(true).resumePending();
      assert.deepEqual(f.order, ["archive"]);
      assert.equal(f.finals.length, 1);
      assert.equal(f.finals[0]![1], 7);
      assert.equal(f.committedReceipts.get(7)?.kind, "archive");
      assert.equal(f.state.open, false);
    } finally { await f.close(); }
  });
}


for (const [kind, refusal] of [["ci_fix", "quota"], ["prompt", "oversize"], ["task", "thin"]] as const) {
  it(`M4 completed independent ${kind} ${refusal} falls back to exact held FINAL`, async () => {
    const f = await v2FrozenFixture(kind, true);
    try {
      f.state.status = "completed";
      if (refusal === "thin") f.state.thin = true;
      else f.state.reserveError = new RequestError("POST", "/reserve",
        refusal === "quota" ? 429 : 413, JSON.stringify({ reason: refusal }));
      await f.make(true).resumePending();
      assert.ok(f.order.indexOf("archive") < f.order.indexOf("held"));
      assert.equal(f.finals.length, 1);
      assert.equal(f.finals[0]![1], 7);
      assert.deepEqual(f.committedReceipts.get(7), {
        kind: "held_publication", publication_id: "00000000-0000-4000-8000-000000000009",
        source_sha: f.record.sourceSha, coverage_digest: f.record.coverageDigest,
      });
      assert.equal(f.state.open, false);
    } finally { await f.close(); }
  });
}

for (const refusal of ["empty roots without source proof", "unverified head"] as const) {
  it(`M4 completed earlier thin ${refusal} cannot manufacture empty proof or ACK`, async () => {
    const f = await v2FrozenFixture("issue", true);
    try {
      f.state.status = "completed";
      f.state.thin = true;
      await f.capture(f.record);
      const before = (await f.coordinator.inspect(f.context.runId))[0]!;
      if (refusal === "empty roots without source proof") {
        f.state.candidates = [];
        f.state.sourceProofRefused = true;
      } else f.state.cloneReadable = false;
      await f.make(true).resumePending();
      const saved = (await f.coordinator.inspect(f.context.runId))[0]!;
      assert.equal(saved.captureId, before.captureId);
      assert.equal(saved.finalAcknowledged, undefined);
      assert.equal(f.finals.length, 0);
      assert.equal(f.order.includes("held"), false);
      assert.equal(f.state.open, true);
      assert.equal(f.state.pinDeletes, 0);
      assert.equal(f.state.produced, 1);
    } finally { await f.close(); }
  });
}

for (const status of ["failed", "cancelled", "completed"]) {
  for (const kind of ["issue", "ci_fix", "prompt", "task"] as const) {
    it(`M3 V2 live older generation ${status}/${kind} uses same frozen record and correct order`, async () => {
      const f = await v2FrozenFixture(kind);
      try {
        f.state.status = status;
        f.state.thin = true;
        await f.make(true).resumePending(undefined, [f.record]);
        assert.equal(f.order.includes("held"), true);
        assert.equal(f.order.includes("archive"), status === "completed");
        if (status === "completed") assert.ok(f.order.indexOf("archive") < f.order.indexOf("held"));
        assert.deepEqual(f.order.filter(x => x !== "archive"), ["range", "scan", "workflow", "ci", "pack", "held"]);
        assert.equal(f.finals[0]?.[1], 7);
        assert.equal((f.finals[0]?.[3] as RecoveryFinalDisposition)?.kind, "held_publication");
      } finally { await f.close(); }
    });
  }
}

for (const refusal of ["local oversize", "typed quota", "typed oversize", "raw 413", "unverified", "self contained"]) {
  it(`M3 completed archive fallback requires typed refusal or thin capture: ${refusal}`, async () => {
    const f = await v2FrozenFixture();
    try {
      f.state.status = "completed";
      if (refusal === "local oversize") f.state.oversized = true;
      if (refusal === "unverified") f.state.failProduce = true;
      if (refusal === "typed quota") f.state.reserveError = new RequestError("POST", "/reserve", 429,
        JSON.stringify({ reason: "quota" }));
      if (refusal === "typed oversize") f.state.reserveError = new RequestError("POST", "/reserve", 413,
        JSON.stringify({ reason: "oversize" }));
      if (refusal === "raw 413") f.state.reserveError = new RequestError("POST", "/reserve", 413, "too big");
      await f.operate();
      assert.equal(f.order.includes("held"), ["local oversize", "typed quota", "typed oversize"].includes(refusal));
      assert.equal(f.order[0], "archive");
      const saved = await f.coordinator.inspect("run-1");
      assert.ok(saved.length <= 1);
      for (const r of saved) assert.equal(r.captureId, f.record.captureId);
    } finally { await f.close(); }
  });
}

for (const stop of ["running", "unknown source", "changed coverage", "CI", "closed during scan", "unknown create", "legacy policy"]) {
  it(`M3 V2 held fails closed: ${stop}`, async () => {
    const f = await v2FrozenFixture();
    try {
      if (stop === "running") f.state.status = "running";
      if (stop === "unknown source") f.state.sourceRefused = true;
      if (stop === "changed coverage") f.state.candidates.push({ sha: H2, pinRef: "refs/new", contexts: [f.context] });
      if (stop === "CI") Object.assign(f.git, { changedFilesNoRenames: async () => [".custom-ci/build.yml"] });
      if (stop === "closed during scan") Object.assign(f.git, {
        secretScanCheckpointRange: async () => { f.setHold("discarded"); return { trusted: true, findings: [] }; },
      });
      if (stop === "unknown create") Object.assign(f.client, {
        publishHeld: async (_run: string, _gen: number, _tip: string, _digest: string, pack: Buffer) => {
          if (pack.length) f.order.push("held");
          throw new Error("lost create response");
        },
      });
      if (stop === "legacy policy") {
        // Fresh legacy fixture has no policy or exact binding and cannot become a weaker V2 producer.
        const legacy = await fixture();
        try {
          Object.assign(legacy.client, { hasHeldPublicationV2: () => true });
          const record = await legacy.freeze(); assert.ok(record);
          const result = await legacy.coordinator.operateFrozenInventory({
            record, barePath: legacy.context.barePath, defaultBranch: "main",
          });
          assert.equal(result.state, "uploaded", "missing held policy retains the existing guarded archive path");
          assert.equal(legacy.state.produced, 1);
        } finally { await legacy.close(); }
        return;
      }
      await f.operate();
      if (stop === "unknown create") {
        await f.operate();
        assert.equal(f.order.filter(x => x === "held").length, 1);
        assert.equal(f.state.produced, 0);
      } else assert.equal(f.order.includes("held"), false);
      if (stop === "closed during scan") assert.equal(f.state.produced, 0);
    } finally { await f.close(); }
  });
}

for (const closed of ["released", "discarded", "unknown"] as const) {
  it(`exact ${closed} hold stops held fallback and new archive without acknowledging FINAL`, async () => {
    const f = await fixture();
    try {
      const holdId = "00000000-0000-4000-8000-000000000007";
      const repoId = "00000000-0000-4000-8000-000000000008";
      let state: "open" | "released" | "discarded" | "unknown" = "open";
      Object.assign(f.client, {
        listRecoveryHolds: async () => ({ run_id: "run-1", holds: [{
          hold_id: holdId, generation: 7, inventory_guarded: true, has_available_capture: false,
        }] }),
        getExactRecoveryHold: async (_run: string, generation: number, expected: unknown) => {
          assert.equal(generation, 7);
          assert.deepEqual(expected, { holdId, repoId });
          return state === "unknown" ? undefined : {
            run_id: "run-1", hold_id: holdId, repo_id: repoId, generation: 7, state,
            original_worker_id: "original", original_worker_identity: "original",
            inventory_guarded: true, run_status: "failed", current_claim_generation: 8,
            claim_released: true,
            ...(state === "open" ? { live_worker_id: "original", live_run_id: "run-1" } : { release_evidence: state === "released" ? "archive" : "owner_discard", ...(state === "released" ? { final_receipt: { kind: "archive", capture_id: "00000000-0000-4000-8000-000000000010", source_sha: H2, coverage_digest: "c".repeat(64) } } : {}) }),
          };
        },
      });
      const record = await f.coordinator.freezeInventory({
        context: f.context, currentSha: H, defaultBranch: "main", repoId,
      });
      assert.ok(record);
      state = closed;
      assert.equal(await f.coordinator.releaseHeldPublication(record, "00000000-0000-4000-8000-000000000009"),
        closed === "unknown" ? "kept" : "settled");
      const outcome = await f.capture(record);
      assert.equal(outcome.held, closed === "unknown" ? "kept" : "settled");
      assert.equal(f.state.produced, 0);
      assert.equal(f.state.uploads, 0);
      assert.equal(f.finals.length, 0);
      const saved = (await f.coordinator.inspect("run-1"))[0]!;
      assert.deepEqual(saved.exactHoldBinding, { holdId, repoId });
      assert.equal(saved.finalAcknowledged, undefined);
      if (closed !== "unknown") assert.equal(saved.holdSettlement?.state, closed);
    } finally { await f.close(); }
  });
}


it("exact closed hold permits only the saved archive FINAL replay after ACK loss", async () => {
  const f = await fixture(false, os.tmpdir(), true);
  try {
    f.client.reconcileRecoveryCapture = async () => { throw new Error("reconciliation unavailable"); };
    const holdId = "00000000-0000-4000-8000-000000000007";
    const repoId = "00000000-0000-4000-8000-000000000008";
    Object.assign(f.client, {
      listRecoveryHolds: async () => ({ run_id: "run-1", holds: f.state.open ? [{
        hold_id: holdId, generation: 7, inventory_guarded: true, has_available_capture: false,
      }] : [] }),
      getExactRecoveryHold: async () => ({
        run_id: "run-1", hold_id: holdId, repo_id: repoId, generation: 7,
        state: f.state.open ? "open" : "released", inventory_guarded: true,
        original_worker_id: "original", original_worker_identity: "original",
        run_status: "failed", current_claim_generation: 7, claim_released: true,
        ...(f.state.open ? { live_worker_id: "original", live_run_id: "run-1" } :
          { release_evidence: "archive", final_receipt: structuredClone(f.committedReceipts.get(7)) }),
      }),
    });
    const record = await f.coordinator.freezeInventory({ context: f.context, currentSha: H, defaultBranch: "main", repoId });
    assert.ok(record);
    f.state.loseAck = true;
    await f.capture(record);
    assert.equal(f.finals.length, 1);
    assert.equal((await f.coordinator.inspect("run-1"))[0]?.finalAcknowledged, undefined);
    f.state.loseAck = false;
    await f.make().resumePending();
    assert.equal(f.finals.length, 2);
    assert.deepEqual(f.finals[1], f.finals[0]);
    assert.equal((await f.coordinator.inspect("run-1"))[0]?.finalAcknowledged, true);
    assert.equal(f.reserves(), 1);
    assert.equal(f.state.uploads, 1);
  } finally { await f.close(); }
});

it("stub redrive wiring recaptures stale inventory and FINALs within the receipt window", async () => {
  const f = await fixture(true);
  try {
    f.state.finalError = new Error("API cut");
    f.state.sourceRefused = true;
    f.state.diagnostic = { check: "execution_tail_present" };
    const old = await f.freeze();
    assert.ok(old);
    await f.capture(old);
    f.state.sourceRefused = false;
    f.state.candidates.push({ sha: H2, pinRef: "refs/owed/new-work", contexts: [f.context] });
    f.state.ancestors.add(H2);
    const uploaded = (await f.coordinator.inspect("run-1"))[0]!;
    await f.coordinator.resumePending(undefined, [uploaded]);
    assert.equal((await f.coordinator.inspect("run-1"))[0]?.reason, "inventory_snapshot_changed");
    Object.assign(f.git, { recoveryRoot: path.join(f.root, "journal") });
    const runner = new RunRunner(f.client as never, f.git as never,
      () => { throw new Error("unreached executor"); }, f.log, 20, "local-worker-fixture", {
        now: () => f.state.now,
        recoveryLiveTiming: buildRecoveryLiveTiming("stub", f.log, { UZI_E2E_RECOVERY_FAST_REDRIVE: "1" }),
      });
    await runner.resumeLiveRecoveries(f.state.now);
    const recovery = (runner as unknown as { recovery: RecoveryCoordinator }).recovery;
    const fresh = (await recovery.inspect("run-1")).find(r => r.captureId !== old.captureId && r.state === "uploaded");
    assert.ok(fresh, "live discovery must recapture the changed inventory");
    assert.notEqual(fresh.coverageDigest, old.coverageDigest);
    assert.equal(fresh.finalAcknowledged, undefined, "cut still prevents FINAL");
    f.state.finalError = undefined;
    f.state.closeOnRelease = true;
    f.state.now += 5_000;
    await runner.resumeLiveRecoveries(f.state.now);
    assert.ok((await recovery.inspect("run-1")).some(r => r.captureId === fresh.captureId && r.finalAcknowledged),
      "next bounded pass must FINAL the fresh capture, not wait on production backoff");
    assert.equal(f.state.open, false);
    assert.equal(f.state.uploads, 2);
  } finally { await f.close(); }
});

for (const fast of [false, true]) {
it(`handoff receipt pacing preserves the uploaded task identity with cleanup backlog (${fast ? "stub clock" : "production clock"})`, async t => {
  const f = await fixture();
  try {
    const backlog = Array.from({ length: 20 }, (_, i) => `ack-${String(i).padStart(2, "0")}`);
    for (const runId of [...backlog, "fresh-task"]) {
      const context = { ...f.context, runId, kind: "task" as const };
      f.state.candidates = [{ sha: H, pinRef: `refs/owed/${runId}`, contexts: [context] }];
      const record = await f.coordinator.freezeInventory({ context, currentSha: H, defaultBranch: "main" });
      assert.ok(record);
      if (runId === "fresh-task") f.state.finalError = new Error("foreground tail withheld FINAL");
      await f.capture(record);
      if (runId !== "fresh-task") assert.ok((await f.coordinator.inspect(runId)).some(r => r.finalAcknowledged));
    }
    f.state.finalError = undefined;
    f.state.candidates = [];
    const timing = fast ? buildRecoveryLiveTiming("stub", f.log, { UZI_E2E_RECOVERY_FAST_REDRIVE: "1" }) : undefined;
    const live = new RecoveryCoordinator({ recoveryRoot: path.join(f.root, "journal"),
      workerToken: "local-worker-fixture", client: f.client as never, git: f.git as never,
      log: f.log, now: () => f.state.now, ...timing });
    const snapshot = live.snapshotBootRecords.bind(live);
    t.mock.method(live, "snapshotBootRecords", async () => (await snapshot())
      .sort((a, b) => Number(a.runId === "fresh-task") - Number(b.runId === "fresh-task")));
    const before = f.finals.length;
    const uploads = f.state.uploads;
    const started = f.state.now;
    const step = fast ? 1_000 : 30_000;
    const original = (await live.inspect("fresh-task")).find(r => r.coverageDigest)!;
    for (let pass = 0; pass < 5; pass++) {
      await live.resumeLive({ authenticatedAtMs: f.state.now, isExecuting: () => false });
      assert.equal(f.finals.length, before, `bounded queue still precedes this record at pass ${pass}`);
      f.state.now += step;
    }
    await live.resumeLive({ authenticatedAtMs: f.state.now, isExecuting: () => false });
    assert.equal(f.finals.length, before + 1, "bounded passes eventually reach the unchanged archive");
    assert.equal(f.state.uploads, uploads);
    assert.equal(f.state.now - started, fast ? 5_000 : 150_000);
    const final = f.finals.at(-1)![3] as RecoveryFinalDisposition;
    assert.equal(final.kind, "archive");
    if (final.kind === "archive") {
      assert.equal(final.capture_id, original.serverCaptureId);
      assert.equal(final.source_sha, original.sourceSha);
      assert.equal(final.coverage_digest, original.coverageDigest);
    }
  } finally { await f.close(); }
});
}

it("pending-report ACK cleanup leaves every live-pass slot available for a fresh FINAL", async t => {
  const f = await fixture();
  try {
    const pending = new Set<string>();
    for (const runId of ["ack-1", "ack-2", "ack-3", "ack-4", "fresh"]) {
      const context = { ...f.context, runId };
      f.state.candidates = [{ sha: H, pinRef: `refs/owed/${runId}`, contexts: [context] }];
      await f.coordinator.pin({ runId, generation: 7, kind: "issue", branch: "task", sourceSha: H, inventoryGuarded: true });
      const record = await f.coordinator.freezeInventory({ context, currentSha: H, defaultBranch: "main" });
      assert.ok(record);
      if (runId === "fresh") f.state.finalError = new Error("FINAL transport unavailable");
      await f.capture(record);
      if (runId !== "fresh") {
        pending.add(runId);
        assert.ok((await f.coordinator.inspect(runId)).some(r => r.finalAcknowledged));
      }
    }
    f.state.finalError = undefined;
    f.state.candidates = [];
    Object.assign(f.git, { cleanupRecoveryGeneration: async () => "removed" });
    const live = f.make(false, (runId, generation) => generation === 7 && pending.has(runId));
    // Pin the queue order independently of filesystem directory enumeration.
    // Every returned record still comes from the real authenticated journal reader.
    const snapshot = live.snapshotBootRecords.bind(live);
    t.mock.method(live, "snapshotBootRecords", async () => (await snapshot())
      .sort((a, b) => Number(a.runId === "fresh") - Number(b.runId === "fresh")));
    const before = f.finals.length;
    await live.resumeLive({ authenticatedAtMs: f.state.now, isExecuting: () => false });
    assert.equal(f.finals.length, before + 1, "fresh FINAL must run in the first pass, ahead of four deferred ACKs");
    for (const runId of pending) assert.ok((await live.inspect(runId)).some(r => r.finalAcknowledged),
      "pending report retains its authenticated ACK");
    pending.clear();
    f.state.now += 30_000;
    await live.resumeLive({ authenticatedAtMs: f.state.now, isExecuting: () => false });
    for (const runId of ["ack-1", "ack-2", "ack-3", "ack-4"]) assert.deepEqual(await live.inspect(runId), [],
      "report retirement makes deferred cleanup eligible again");
    assert.equal(f.finals.length, before + 1, "cleanup does not replay acknowledged FINALs");
  } finally { await f.close(); }
});

for (const failure of ["none", "bundle", "ack", "pins", "unreadable", "foreign", "uncovered", "attacker-path"] as const) {
  it(`M2 checked cleanup retains authority on ${failure} and retries without upload`, async t => {
    const f = await fixture();
    try {
      f.state.closeOnRelease = true;
      await f.coordinator.pin({ runId: "run-1", generation: 7, kind: "issue", branch: "task",
        sourceSha: H, inventoryGuarded: true });
      const record = await f.freeze();
      assert.ok(record);
      // Capture first with no helper: persisted ACK models a crash before local cleanup.
      await f.capture(record);
      const ack = (await f.coordinator.inspect("run-1")).find(r => r.captureId === record.captureId)!;
      assert.equal(ack.finalAcknowledged, true);
      assert.ok(ack.bundlePath);
      const journal = path.join(f.root, "journal", "run-1", ack.captureId + ".json");
      const saved = await fs.readFile(journal, "utf8");
      const roots = await f.coordinator.inspect("run-1");
      const source = roots.find(r => !r.coverageDigest)!;
      const sourceFile = path.join(f.root, "journal", "run-1", source.captureId + ".json");
      const sourceSaved = await fs.readFile(sourceFile, "utf8");
      const sign = async (file: string, payload: RecoveryRecord) => {
        const key = createHmac("sha256", "local-worker-fixture").update("uzi-recovery-journal-v1").digest();
        const mac = createHmac("sha256", key).update(canonicalJson(payload)).digest("hex");
        await fs.writeFile(file, JSON.stringify({ ...payload, mac }));
      };
      if (failure === "unreadable") { await fs.unlink(sourceFile); await fs.mkdir(sourceFile); }
      if (failure === "foreign") await sign(sourceFile, { ...source, runId: "foreign" });
      if (failure === "uncovered") await sign(sourceFile, { ...source, sourceSha: H2 });
      const attacker = path.join(f.root, "attacker.bundle");
      if (failure === "attacker-path") {
        await fs.writeFile(attacker, "keep");
        await sign(journal, { ...ack, bundlePath: attacker });
      }
      let pins = 0, pinFailure = failure === "pins";
      Object.assign(f.git, { cleanupRecoveryGeneration: async () => {
        pins++;
        if (pinFailure) throw new Error("pin readback failed");
        return "removed";
      } });
      const originalUnlink = fs.unlink;
      let unlinkFailure = failure === "bundle" || failure === "ack";
      t.mock.method(fs, "unlink", async (file: Parameters<typeof fs.unlink>[0]) => {
        if (unlinkFailure && String(file) === (failure === "bundle" ? ack.bundlePath : journal))
          throw Object.assign(new Error("unlink failure"), { code: "EACCES" });
        return originalUnlink(file);
      });
      const reserves = f.reserves(), uploads = f.state.uploads, finals = f.finals.length;
      const restarted = f.make();
      await restarted.resumePending();
      if (failure !== "none") {
        assert.equal((await restarted.inspect("run-1")).find(r => r.captureId === ack.captureId)?.finalAcknowledged, true);
        assert.ok(await fs.stat(journal));
        if (["unreadable", "foreign", "uncovered", "attacker-path"].includes(failure)) assert.equal(pins, 0);
        if (failure === "unreadable") await fs.rmdir(sourceFile);
        if (["unreadable", "foreign", "uncovered"].includes(failure)) await fs.writeFile(sourceFile, sourceSaved);
        if (failure === "attacker-path") {
          assert.equal(await fs.readFile(attacker, "utf8"), "keep");
          await fs.writeFile(journal, saved);
        }
        pinFailure = false; unlinkFailure = false;
        f.state.now += 100_000;
        await restarted.resumeLive({ authenticatedAtMs: f.state.now, isExecuting: () => false });
      }
      assert.deepEqual(await restarted.inspect("run-1"), []);
      await assert.rejects(fs.stat(path.join(f.root, "journal", "run-1")), { code: "ENOENT" });
      assert.equal(f.reserves(), reserves);
      assert.equal(f.state.uploads, uploads);
      assert.equal(f.finals.length, finals);
    } finally { await f.close(); }
  });
}

for (const protection of ["quarantine", "execution", "during-pins"] as const) {
  it(`M2 ACK cleanup observes ${protection} source protection`, async () => {
    const { latchResidueQuarantine, resetResidueQuarantineForTests } = await import("../src/residue-quarantine.js");
    const f = await fixture();
    try {
      f.state.closeOnRelease = true;
      const record = await f.freeze();
      assert.ok(record);
      await f.capture(record);
      let executing = protection === "execution", calls = 0;
      Object.assign(f.git, { cleanupRecoveryGeneration: async () => {
        calls++;
        if (protection === "during-pins") executing = true;
        return "removed";
      } });
      const live = f.make();
      if (protection === "quarantine") latchResidueQuarantine({ cause: "cleanup fixture", site: "cleanup" }, nullLogger());
      await live.resumeLive({ authenticatedAtMs: f.state.now, isExecuting: () => executing });
      assert.equal((await live.inspect("run-1"))[0]?.finalAcknowledged, true);
      const ack = (await live.inspect("run-1"))[0]!;
      assert.ok(await fs.stat(ack.bundlePath!));
      assert.equal(calls, protection === "during-pins" ? 1 : 0);
      resetResidueQuarantineForTests();
      executing = false;
      Object.assign(f.git, { cleanupRecoveryGeneration: async () => "removed" });
      f.state.now += 100_000;
      await live.resumeLive({ authenticatedAtMs: f.state.now, isExecuting: () => executing });
      assert.deepEqual(await live.inspect("run-1"), []);
      assert.equal(f.state.uploads, 1);
      assert.equal(f.finals.length, 1);
    } finally { resetResidueQuarantineForTests(); await f.close(); }
  });
}

it("M2 multiple ACK cleanup keeps one covering authority through every source unlink", async t => {
  const f = await fixture();
  try {
    await f.coordinator.pin({ runId: "run-1", generation: 7, kind: "issue", branch: "task",
      sourceSha: H, inventoryGuarded: true });
    const record = await f.freeze();
    assert.ok(record);
    await f.capture(record);
    const ack = (await f.coordinator.inspect("run-1")).find(r => r.finalAcknowledged)!;
    const copy = { ...ack, captureId: "second-ack",
      bundlePath: path.join(f.root, "journal", "run-1", "second-ack.bundle") };
    const dir = path.join(f.root, "journal", "run-1");
    const key = createHmac("sha256", "local-worker-fixture").update("uzi-recovery-journal-v1").digest();
    await fs.writeFile(path.join(dir, copy.captureId + ".json"), JSON.stringify({ ...copy,
      mac: createHmac("sha256", key).update(canonicalJson(copy)).digest("hex") }));
    Object.assign(f.git, { cleanupRecoveryGeneration: async () => "removed" });
    const unlink = fs.unlink;
    let failLast = true;
    const removed: string[] = [];
    t.mock.method(fs, "unlink", async (file: Parameters<typeof fs.unlink>[0]) => {
      if (String(file).endsWith(".json")) {
        const journals = await f.coordinator.inspect("run-1");
        assert.ok(journals.some(r => r.finalAcknowledged), "a covering ACK exists before every journal unlink");
        if (journals.length === 1 && failLast) throw new Error("last ACK unlink interruption");
        removed.push(String(file));
      }
      return unlink(file);
    });
    await assert.rejects(f.coordinator.forgetGeneration("run-1", 7), /last ACK unlink/);
    assert.equal((await f.coordinator.inspect("run-1")).length, 1);
    assert.equal((await f.coordinator.inspect("run-1"))[0]?.finalAcknowledged, true);
    assert.ok(!removed[0]!.endsWith(copy.captureId + ".json"), "non-ACK source journal goes first");
    failLast = false;
    await f.make().resumePending();
    assert.deepEqual(await f.coordinator.inspect("run-1"), []);
  } finally { await f.close(); }
});

it("M2 live ACK retry obeys pass cap and spacing without reserve or upload", async () => {
  const f = await fixture();
  try {
    const record = await f.freeze();
    assert.ok(record);
    await f.capture(record);
    let attempts = 0;
    Object.assign(f.git, { cleanupRecoveryGeneration: async () => { attempts++; throw new Error("retry"); } });
    const live = f.make();
    await live.resumeLive({ authenticatedAtMs: f.state.now, isExecuting: () => false });
    assert.equal(attempts, 1);
    await live.resumeLive({ authenticatedAtMs: f.state.now, isExecuting: () => false });
    assert.equal(attempts, 1);
    f.state.now += 100_000;
    await live.resumeLive({ authenticatedAtMs: f.state.now, isExecuting: () => false });
    assert.equal(attempts, 2);
    assert.equal(f.reserves(), 1);
    assert.equal(f.state.uploads, 1);
    assert.equal(f.finals.length, 1);
  } finally { await f.close(); }
});

it("M2 credential rejection still permits spaced local ACK cleanup", async () => {
  const f = await fixture();
  try {
    f.state.closeOnRelease = true;
    const record = await f.freeze();
    assert.ok(record);
    await f.capture(record);
    f.state.open = true;
    f.state.holdGeneration = 8;
    f.state.ownGeneration = 8;
    const next = await f.coordinator.freezeInventory({
      context: { ...f.context, generation: 8 }, currentSha: H, defaultBranch: "main",
    });
    assert.ok(next);
    f.state.reserveError = new RequestError("POST", "/api/worker/recovery", 401, "unauthorized");
    await f.capture(next);
    const reserves = f.reserves(), uploads = f.state.uploads, finals = f.finals.length;
    let attempts = 0;
    Object.assign(f.git, { cleanupRecoveryGeneration: async () => {
      attempts++;
      if (attempts === 1) throw new Error("local interruption");
      return "removed";
    } });
    const opts = { authenticatedAtMs: f.state.now, isExecuting: () => false };
    await f.coordinator.resumeLive(opts);
    assert.equal(attempts, 1, "same-age rejected credentials cannot block local ACKs");
    await f.coordinator.resumeLive(opts);
    assert.equal(attempts, 1, "local failure retains backoff");
    f.state.now += 100_000;
    await f.coordinator.resumeLive(opts);
    assert.equal(attempts, 2);
    assert.deepEqual((await f.coordinator.inspect("run-1")).map(r => r.generation), [8]);
    assert.equal(f.reserves(), reserves);
    assert.equal(f.state.uploads, uploads);
    assert.equal(f.finals.length, finals);
  } finally { await f.close(); }
});

it("M2 feature rollback cannot reopen an ACK-cleaned guarded journal", async () => {
  const f = await fixture();
  try {
    f.state.closeOnRelease = true;
    const record = await f.freeze();
    assert.ok(record);
    Object.assign(f.git, { cleanupRecoveryGeneration: async () => "removed" });
    await f.capture(record);
    assert.deepEqual(await f.coordinator.inspect("run-1"), []);
    f.state.feature = false;
    const fresh = f.make();
    assert.equal(await fresh.pin({ runId: "run-1", generation: 7, kind: "issue",
      branch: "task", sourceSha: H, inventoryGuarded: true }), undefined);
    assert.deepEqual(await fresh.inspect("run-1"), []);
    assert.equal(f.reserves(), 1);
    assert.equal(f.state.uploads, 1);
  } finally { await f.close(); }
});

for (const closure of ["ACK", "discard"] as const) {
  it(`M2 delayed guarded pin cannot recreate a journal after ${closure}`, async () => {
    const f = await fixture();
    try {
      f.state.closeOnRelease = true;
      const record = await f.freeze();
      assert.ok(record);
      await f.capture(record);
      let entered!: () => void, release!: () => void;
      const enteredCleanup = new Promise<void>(resolve => { entered = resolve; });
      const finishCleanup = new Promise<void>(resolve => { release = resolve; });
      Object.assign(f.git, { cleanupRecoveryGeneration: async () => {
        entered();
        await finishCleanup;
        return "removed";
      } });
      const cleaning = f.coordinator.forgetGeneration("run-1", 7);
      await enteredCleanup;
      // This real writer queues behind the generation cycle and observes absence only
      // after cleanup. No persisted tombstone can rescue its creation re-check.
      const writer = f.make();
      const pinning = writer.pin({ runId: "run-1", generation: 7, kind: "issue",
        branch: "task", sourceSha: H, inventoryGuarded: true });
      release();
      await cleaning;
      assert.equal(await pinning, undefined);
      if (closure === "discard") {
        f.state.open = true;
        assert.ok(await writer.pin({ runId: "run-1", generation: 7, kind: "issue",
          branch: "task", sourceSha: H, inventoryGuarded: true }));
        // Owner discard grants no deletion authority, but closes fresh creation authority.
        f.state.open = false;
        await writer.forgetGeneration("run-1", 7);
        assert.equal((await writer.inspect("run-1")).length, 1);
      }
      await writer.snapshotOwedInventory();
      assert.deepEqual(await writer.materializeBootInventory(), []);
      assert.equal(f.reserves(), 1);
      assert.equal(f.state.uploads, 1);
    } finally { await f.close(); }
  });
}

it("M2 successful persisted FINAL cleanup preserves sibling generations", async () => {
  const f = await fixture();
  try {
    const record = await f.freeze();
    assert.ok(record);
    Object.assign(f.git, { cleanupRecoveryGeneration: async () => "removed" });
    f.state.holdGeneration = 8;
    const sibling = await f.coordinator.pin({ runId: "run-1", generation: 8, kind: "issue", branch: "task",
      sourceSha: H2, inventoryGuarded: true });
    assert.ok(sibling);
    f.state.holdGeneration = 7;
    await f.capture(record);
    assert.deepEqual((await f.coordinator.inspect("run-1")).map(r => r.captureId), [sibling.captureId]);
    await assert.rejects(fs.stat(path.join(f.root, "journal", "run-1", record.captureId + ".bundle")), { code: "ENOENT" });
  } finally { await f.close(); }
});


it("review probe: a history of acknowledged generations does not cause exponential settlement work", async () => {
  const f = await fixture();
  try {
    f.state.closeOnRelease = true;
    const snapshot = await f.freeze();
    assert.ok(snapshot);
    await f.capture(snapshot);
    const template = (await f.coordinator.inspect("run-1"))[0]!;
    assert.equal(template.finalAcknowledged, true);
    const count = 12;
    const records = Array.from({ length: count }, (_, index) => ({
      ...structuredClone(template), captureId: `generation-${index + 1}`, generation: index + 1,
      coverageContext: { ...template.coverageContext!, generation: index + 1 },
    }));
    const inner = f.coordinator as unknown as {
      cleanupStateOf(records: RecoveryRecord[], generation: number): Promise<string>;
    };
    const cleanup = inner.cleanupStateOf.bind(inner);
    let calls = 0;
    inner.cleanupStateOf = (rs, generation) => { calls++; return cleanup(rs, generation); };
    assert.equal(await inner.cleanupStateOf(records, count), "acknowledged");
    assert.ok(calls <= count * count,
      `${calls} settlement evaluations for ${count} generations must not grow exponentially`);
  } finally { await f.close(); }
});

it("an initial guarded pin record does not stop a covering FINAL ACK from authorizing cleanup", async () => {
  const f = await fixture();
  try {
    f.state.closeOnRelease = true;
    await f.coordinator.pin({ runId: "run-1", generation: 7, kind: "issue", branch: "task", sourceSha: H, inventoryGuarded: true });
    const record = await f.freeze();
    assert.ok(record);
    await f.capture(record);
    assert.equal((await f.coordinator.inspect("run-1")).find(r => r.captureId === record.captureId)?.finalAcknowledged, true);
    assert.equal(await f.coordinator.inventoryCleanupState("run-1", 7), "acknowledged");
  } finally { await f.close(); }
});

it("issue1924 an acknowledged older generation's source journal does not block a later FINAL", async () => {
  const f = await fixture();
  try {
    f.state.closeOnRelease = true;
    // Generation 5 archived its head H3 and was acknowledged; its unfrozen source journal stays on disk.
    const H3 = "c".repeat(40);
    f.state.ancestors.add(H3);
    f.state.holdGeneration = 5; f.state.ownGeneration = 8;
    const ctx5 = { ...f.context, generation: 5 };
    f.state.candidates = [{ sha: H3, pinRef: "refs/owed/a", contexts: [structuredClone(ctx5)] }];
    await f.coordinator.pin({ runId: "run-1", generation: 5, kind: "issue", branch: "task", sourceSha: H3, inventoryGuarded: true });
    const rec5 = await f.coordinator.freezeInventory({ context: ctx5, currentSha: H3, defaultBranch: "main" });
    assert.ok(rec5);
    await f.capture(rec5);
    assert.equal(await f.coordinator.inventoryCleanupState("run-1", 5), "acknowledged");
    // The run resumed as generation 7 and rewrote H3 away; H3 stays durable via the generation-5 archive.
    f.state.holdGeneration = 7; f.state.open = true; f.state.candidates = [];
    await f.coordinator.pin({ runId: "run-1", generation: 7, kind: "issue", branch: "task", sourceSha: H, inventoryGuarded: true });
    const rec7 = await f.coordinator.freezeInventory({ context: f.context, currentSha: H, defaultBranch: "main" });
    assert.ok(rec7);
    f.state.refused.set(H3, rec7.sourceSha); // the generation-7 archive does not contain H3
    await f.capture(rec7);
    assert.equal((await f.coordinator.inspect("run-1")).find(r => r.captureId === rec7.captureId)?.finalAcknowledged, true,
      "an older generation whose own FINAL is acknowledged must not block this generation's FINAL");
    assert.equal(await f.coordinator.inventoryCleanupState("run-1", 7), "acknowledged",
      "a later FINAL accepted without the separately archived old head must authorize cleanup too");
  } finally { await f.close(); }
});

function ownershipLost() {
  return new RequestError("GET", "/api/worker/runs/" + "run-1/ownership", 404,
    JSON.stringify({ error: "run not found " + "for this worker" }), undefined, { complete: true, bytes: 41 });
}

it("issue1924 crossworker ownership404 archives all original roots under open hold", async () => {
  const f = await fixture();
  try {
    f.state.ownershipError = ownershipLost();
    const record = await f.freeze(H2);
    assert.ok(record);
    assert.deepEqual(record.originalRoots?.map(r => r.sha), [H]);
    assert.ok(f.aggregates.some(roots => roots.includes(H) && roots.includes(H2)));
    await f.capture(record);
    assert.equal(f.finals.length, 1);
    assert.equal((await f.coordinator.inspect("run-1"))[0]?.finalAcknowledged, true);
  } finally { await f.close(); }
});

for (const settled of [false, true]) {
  it(`issue1924 lost FINAL ACK foreign ownership404 closed hold exact replay settled=${settled}`, async () => {
    const f = await fixture();
    try {
      if (settled) { f.state.candidates = []; f.state.status = "completed"; }
      const record = settled ? await f.coordinator.freezeInventory({
        context: f.context, currentSha: H, defaultBranch: "main", settledEvidence: "publication",
      }) : await f.freeze();
      assert.ok(record);
      f.state.loseAck = true;
      await f.capture(record);
      assert.equal(f.finals.length, 1);
      const identity = (await f.coordinator.inspect("run-1"))[0]!.finalRequest;
      const reserves = f.reserves(), produced = f.state.produced;
      f.state.ownershipError = ownershipLost();
      f.state.loseAck = false;
      assert.equal(f.state.open, false);
      await f.make().resumePending(undefined, [record]);
      assert.equal(f.finals.length, 2);
      assert.deepEqual(f.finals[1], f.finals[0]);
      assert.deepEqual((await f.coordinator.inspect("run-1"))[0]!.finalRequest, identity);
      assert.equal((await f.coordinator.inspect("run-1"))[0]!.finalAcknowledged, true);
      assert.equal(f.reserves(), reserves);
      assert.equal(f.state.produced, produced);
    } finally { await f.close(); }
  });
}

it("issue1924 ownership loss never mints publication or reopens closed hold", async () => {
  const f = await fixture();
  try {
    f.state.candidates = [];
    f.state.ownershipError = ownershipLost();
    assert.equal(await f.coordinator.freezeInventory({
      context: f.context, currentSha: H, defaultBranch: "main",
      settledEvidence: "publication", locallyQuiescent: true,
    }), undefined);
    f.state.open = false;
    assert.equal(await f.freeze(), undefined);
    assert.equal(f.reserves(), 0);
    assert.equal(f.finals.length, 0);
  } finally { await f.close(); }
});

it("issue1924 invalid ownership route body network and HTTP failures never FINAL", async () => {
  for (const error of [
    new RequestError("GET", "/api/worker/runs/run-1/unknown", 404, ownershipLost().body),
    new RequestError("GET", ownershipLost().path, 404, '{"error":"unknown route"}'),
    new RequestError("GET", ownershipLost().path, 401, ownershipLost().body),
    new RequestError("GET", ownershipLost().path, 500, ownershipLost().body),
    new Error("network unavailable"),
  ]) {
    const f = await fixture();
    try {
      // Positive control: freeze while ownership is readable, so the record exists and the
      // FINAL boundary is reachable; only then does the injected error decide the outcome.
      const record = await f.freeze();
      assert.ok(record, "a frozen record exists before the error is injected");
      const reserves = f.reserves();
      f.state.ownershipError = error;
      // Drives the resume path, whose FINAL is gated by inactiveInventory (the ownership read).
      // captureAndUpload reaches the same gate through finalizeInventory, but it first uploads the
      // bundle (raising reserves), so asserting an unchanged reserve count would need resumePending
      // alone; both paths were checked to send no FINAL on each injected error.
      await f.make().resumePending(undefined, [record]).catch(() => undefined);
      assert.equal(f.finals.length, 0, "no FINAL is sent on an invalid ownership answer");
      assert.equal(f.reserves(), reserves);
      assert.notEqual((await f.coordinator.inspect("run-1"))[0]?.finalAcknowledged, true);
      assert.equal(f.state.open, true, "the hold stays open");
    } finally { await f.close(); }
  }
});

it("issue1924 a discovery that no longer returns a context drops it from the boot queue", async () => {
  const f = await fixture();
  try {
    await f.coordinator.snapshotOwedInventory();
    let holdReads = 0;
    Object.defineProperty(f.state, "open", { get: () => { holdReads++; return true; } });
    await f.coordinator.materializeBootInventory();
    const visited = holdReads;
    assert.ok(visited > 0, "the discovered context is visited");
    f.git.discoverOwedCandidates = async () => [];
    await f.coordinator.snapshotOwedInventory();
    await f.coordinator.materializeBootInventory();
    assert.equal(holdReads, visited, "a context whose pins are gone costs no further hold RPC");
  } finally { await f.close(); }
});

it("issue1924 a failed discovery keeps the queued contexts", async () => {
  const f = await fixture();
  try {
    await f.coordinator.snapshotOwedInventory();
    f.git.discoverOwedCandidates = async () => { throw new Error("unreadable refs"); };
    await assert.rejects(f.coordinator.snapshotOwedInventory());
    let holdReads = 0;
    Object.defineProperty(f.state, "open", { get: () => { holdReads++; return true; } });
    await f.coordinator.materializeBootInventory();
    assert.ok(holdReads > 0, "the retained context is still visited");
  } finally { await f.close(); }
});

it("issue1924 queue rotation survives repeated discovery so later contexts are not starved", async () => {
  const f = await fixture();
  try {
    const contexts = ["run-1", "run-2", "run-3"].map(runId => ({ ...f.context, runId }));
    f.git.discoverOwedCandidates = async () => contexts.map(context => ({
      context, candidates: structuredClone(f.state.candidates) }));
    const inner = f.coordinator as unknown as {
      liveMaxPerPass: number; client: { listRecoveryHolds(id: string): Promise<unknown> };
    };
    inner.liveMaxPerPass = 1;
    const visited: string[] = [];
    const list = inner.client.listRecoveryHolds.bind(inner.client);
    inner.client.listRecoveryHolds = async (id: string) => { visited.push(id); return list(id); };
    for (let pass = 0; pass < 3; pass++) {
      await f.coordinator.snapshotOwedInventory();
      await f.coordinator.materializeBootInventory();
    }
    assert.deepEqual([...new Set(visited)], ["run-1", "run-2", "run-3"]);
  } finally { await f.close(); }
});

it("issue1924 a symlinked ancestor of the recovery root still finalizes; a symlinked run directory does not", async () => {
  const outer = makeScratchRoot("inventory-link-");
  try {
    await fs.mkdir(path.join(outer, "real"));
    await fs.symlink(path.join(outer, "real"), path.join(outer, "link"), "dir");
    const f = await fixture(false, path.join(outer, "link"));
    try {
      const record = await f.freeze();
      assert.ok(record);
      await f.capture(record);
      assert.equal(f.finals.length, 1, "FINAL is sent although the data dir is reached through a symlink");
      assert.equal(await f.coordinator.inventoryCleanupState("run-1", 7), "acknowledged");
      const runDir = path.join(f.root, "journal", "run-1");
      await fs.rename(runDir, runDir + "-moved");
      await fs.symlink(runDir + "-moved", runDir, "dir");
      await assert.rejects(f.coordinator.inventoryCleanupState("run-1", 7), /unsafe recovery directory/);
    } finally { await f.close(); }
  } finally { await fs.rm(outer, { recursive: true, force: true }); }
});

const sourceDiagnosticCauses = [
  "attribution_unreadable", "clone_ancestor_invalid", "clone_path_invalid",
  "clone_head_unreadable", "git_or_filesystem_error", "other",
] as const;

for (const cause of [...sourceDiagnosticCauses, undefined, "unknown/raw/error"]) {
  for (const actionTime of [false, true]) it(`#2507 fallback cause ${cause} actionTime=${actionTime} authenticates across restart`, async () => {
    const f = await fixture();
    try {
      const record = await f.freeze();
      assert.ok(record);
      let reads = 0;
      f.git.readInventoryCloneHeads = async () => ++reads === (actionTime ? 3 : 1)
        ? { kind: "unknown", cause } as never : { kind: "verified", heads: [], clones: [], foreignOwners: [] };
      const outcome = await f.capture(record);
      const normalized = sourceDiagnosticCauses.includes(cause as never) ? cause : "other";
      const reason = "inventory_source_not_quiescent:inventory_read_not_verified:" + normalized;
      assert.equal(outcome.reason, reason);
      const before = (await f.coordinator.inspect("run-1"))[0]!;
      assert.equal(before.reason, reason);
      assert.equal(before.state, "uploaded");
      assert.equal(before.finalAcknowledged, undefined);
      assert.equal(f.state.open, true);
      assert.deepEqual(f.finals, []);
      assert.deepEqual(f.logs, [{ run_id: "run-1", generation: 7, capture_id: record.captureId,
        source_boundary_reason: "inventory_read_not_verified", inventory_read_cause: normalized }]);
      const restarted = f.make();
      assert.equal((await restarted.inspect("run-1"))[0]!.reason, reason, "fresh MAC-authenticated read");
      f.git.readInventoryCloneHeads = async () => ({ kind: "verified", heads: [], clones: [], foreignOwners: [] });
      await restarted.resumePending(undefined, [before]);
      const after = (await restarted.inspect("run-1"))[0]!;
      assert.equal(after.finalAcknowledged, true);
      assert.equal(after.reason, reason, "last failure survives success");
      for (const key of ["captureId", "serverCaptureId", "sourceSha", "checksum", "coverageDigest"] as const)
        assert.equal(after[key], before[key]);
      if (before.finalRequest) assert.deepEqual(after.finalRequest, before.finalRequest);
      assert.equal(f.state.uploads, 1);
      assert.equal(f.reserves(), 1);
    } finally { await f.close(); }
  });
}

for (const original of [false, true]) for (const actionTime of [false, true]) {
  it(`#2507 boot refusal logs independently of writes original=${original} actionTime=${actionTime}`, async () => {
    const f = await fixture(true);
    try {
      if (original) await f.coordinator.pin({ runId: "run-1", generation: 7, kind: "issue",
        branch: "task", sourceSha: H, inventoryGuarded: true });
      await f.coordinator.snapshotOwedInventory();
      const before = await f.coordinator.inspect("run-1");
      f.state.diagnostic = { check: "inventory_read_not_verified", cause: "attribution_unreadable" };
      f.state.sourceRefused = !actionTime;
      f.state.sourceProofRefused = actionTime;
      assert.deepEqual(await f.coordinator.materializeBootInventory(), []);
      assert.deepEqual(f.logs, [{ run_id: "run-1", generation: 7,
        ...(original ? { capture_id: before[0]!.captureId } : {}),
        source_boundary_reason: "inventory_read_not_verified", inventory_read_cause: "attribution_unreadable" }]);
      const after = await f.make(true).inspect("run-1");
      if (original && !actionTime) {
        assert.equal(after[0]!.reason, "inventory_source_not_quiescent:inventory_read_not_verified:attribution_unreadable");
        assert.equal(after[0]!.captureId, before[0]!.captureId);
      } else assert.deepEqual(after, before, "outer passed or no original must not write");
      assert.deepEqual(f.finals, []);
    } finally { await f.close(); }
  });
}

for (const source of ["missing", "unknown", "malformed", "empty", "physical", "legacy"] as const) {
  it(`#2507 coordinator boundary ${source} preserves custody decisions`, async () => {
    const f = await fixture(source === "legacy");
    try {
      const record = await f.freeze();
      assert.ok(record);
      if (source === "missing") delete (f.git as { readInventoryCloneHeads?: unknown }).readInventoryCloneHeads;
      if (source === "unknown") f.state.cloneReadable = false;
      if (source === "malformed") f.git.readInventoryCloneHeads = async () => ({ kind: "verified", clones: [null] }) as never;
      if (source === "physical") f.git.readInventoryCloneHeads = async () => ({ kind: "verified", heads: [H],
        clones: [{ clonePath: "/fixture/clone", runId: "run-1", branch: "task" }], foreignOwners: [] }) as never;
      if (source === "legacy") f.state.sourceRefused = true;
      await f.capture(record);
      const retained = (await f.make().inspect("run-1"))[0]!;
      if (source === "empty") {
        assert.equal(retained.finalAcknowledged, true);
        assert.deepEqual(f.logs, []);
      } else {
        const check = source === "legacy" ? "inventory_source_not_quiescent" :
          source === "physical" || source === "malformed" ? "physical_sources_without_boundary" : "inventory_read_not_verified";
        assert.equal(retained.reason, source === "legacy" ? check : "inventory_source_not_quiescent:" + check +
          (check === "inventory_read_not_verified" ? ":other" : ""));
        assert.equal(f.logs[0]!.source_boundary_reason, check);
        assert.equal(f.finals.length, 0);
        assert.equal(f.state.open, true);
        assert.equal(retained.finalAcknowledged, undefined);
      }
    } finally { await f.close(); }
  });
}

for (const kind of ["issue", "mr_rework"] as const) for (const cause of sourceDiagnosticCauses)
  for (const finalRead of [false, true]) {
    it(`#2507 real runner to coordinator completed ${kind} cause=${cause} finalRead=${finalRead}`, async () => {
      const f = await fixture("runner");
      try {
        f.context.kind = kind;
        f.state.candidates[0]!.contexts = [structuredClone(f.context)];
        f.state.status = "completed";
        const record = await f.freeze();
        assert.ok(record);
        let reads = 0;
        f.git.readInventoryCloneHeads = async () => ++reads === (finalRead ? 2 : 1)
          ? { kind: "unknown", cause } : { kind: "verified", heads: [], clones: [], foreignOwners: [] };
        const outcome = await f.capture(record);
        const reason = "inventory_source_not_quiescent:inventory_read_not_verified:" + cause;
        assert.equal(outcome.reason, reason);
        const retained = (await f.make("runner").inspect("run-1"))[0]!;
        assert.equal(retained.reason, reason, "diagnostic survives authenticated restart");
        assert.equal(retained.kind, kind);
        assert.equal(retained.state, "uploaded");
        assert.equal(retained.finalAcknowledged, undefined);
        assert.equal(f.state.open, true);
        assert.deepEqual(f.finals, []);
        assert.deepEqual(f.logs, [{ run_id: "run-1", generation: 7, capture_id: record.captureId,
          source_boundary_reason: "inventory_read_not_verified", inventory_read_cause: cause }]);
        assert.equal(f.state.uploads, 1);
        assert.equal(f.reserves(), 1);
      } finally { await f.close(); }
    });
  }

async function fixture(sourceBoundary: boolean | "runner" = false, rootParent = os.tmpdir(), receiptModel = false, runId = "run-1") {
  const root = makeScratchRoot("inventory-", rootParent);
  const context: PositiveOwedCandidateContext = {
    runId, generation: 7, kind: "issue", branch: "task",
    barePath: path.join(root, "repo.git"),
    defaultIdentity: { ref: "refs/remotes/origin/main", sha: "d".repeat(40) },
  };
  const state = {
    candidates: [{ sha: H, pinRef: "refs/owed/a", contexts: [structuredClone(context)] }] as OwedCandidate[],
    status: "failed", ownGeneration: 7, ownGuarded: true, feature: true, open: true, expires: "2099-01-01T00:00:00Z",
    ownershipError: undefined as Error | undefined,
    loseAck: false, wrongAck: false, closeOnRelease: false, holdGeneration: 7, ancestors: new Set<string>([H]), refused: new Map<string, string>(), failProduce: false, now: 1000,
    reserveError: undefined as Error | undefined,
    finalError: undefined as Error | undefined,
    oversized: false, cloneHeads: [] as string[], cloneReadable: true,
    sourceRefused: false, sourceProofRefused: false, diagnostic: undefined as InventorySourceDiagnostic | undefined, cloneCause: undefined as string | undefined, uploads: 0, pinDeletes: 0, statusReads: 0, thin: false,
    produceWait: undefined as Promise<void> | undefined, produced: 0,
    onProduce: undefined as (() => void) | undefined,
    beforeFinalAck: undefined as (() => Promise<void>) | undefined,
  };
  const logs: Array<Record<string, unknown>> = [];
  const aggregates: string[][] = [];
  const finals: unknown[][] = [];
  let reserves = 0;
  const reserveKeys: string[] = [];
  const committedReceipts = new Map<number, Readonly<RecoveryFinalDisposition>>();
  const captures = new Map<string, {
    id: string; request: RecoveryReserveRequest; state?: string; expires?: string; reason?: string;
    manifest?: { checksum: string; byte_size: number };
  }>();
  const client = {
    hasFeature: () => state.feature,
    getRunOwnership: async () => {
      if (state.ownershipError) throw state.ownershipError;
      return { status: state.status, claim_generation: state.ownGeneration, inventory_guarded: state.ownGuarded };
    },
    listRecoveryHolds: async (runId = context.runId) => ({ run_id: runId, holds: state.open ? [{
      hold_id: "hold-7", generation: state.holdGeneration, inventory_guarded: true, has_available_capture: false,
    }] : [] }),
    reserveRecoveryCapture: async (_run: string, request: RecoveryReserveRequest) => {
      reserves++;
      reserveKeys.push(request.idempotency_key);
      if (state.reserveError) throw state.reserveError;
      let capture = captures.get(request.idempotency_key);
      if (!capture) {
        capture = { id: "server-" + (captures.size + 1), request: structuredClone(request),
          expires: receiptModel ? state.expires : undefined };
        captures.set(request.idempotency_key, capture);
      }
      return { capture_id: capture.id, state: "preparing" };
    },
    getRecoveryCaptureStatus: async (_run: string, id: string) => {
      state.statusReads++;
      const capture = [...captures.values()].find(c => c.id === id);
      assert.ok(capture);
      return {
        capture_id: id, state: capture.state ?? (capture.manifest ? "available" : "preparing"), manifest_bound: !!capture.manifest,
        checksum: capture.manifest?.checksum, byte_size: capture.manifest?.byte_size, expires_at: capture.expires ?? state.expires, reason: capture.reason,
      };
    },
    uploadRecoveryBundle: async (_run: string, id: string, m: { checksum: string; byte_size: number }, stream: AsyncIterable<unknown>) => {
      for await (const chunk of stream) assert.ok(chunk);
      const capture = [...captures.values()].find(c => c.id === id);
      assert.ok(capture);
      state.uploads++;
      capture.manifest = structuredClone(m);
      return { capture_id: id, state: "available", manifest_bound: true };
    },
    ...(receiptModel ? {
      reconcileRecoveryCapture: async (run: string, id: string, request: {
        generation: number; source_sha: string; coverage_digest: string; checksum: string; byte_size: number;
      }) => {
        const response = { run_id: run, generation: request.generation, capture_id: id };
        const receipt = committedReceipts.get(request.generation);
        if (receipt) return { ...response, outcome: "accepted", final_receipt: structuredClone(receipt) };
        const capture = [...captures.values()].find(c => c.id === id);
        assert.ok(capture);
        assert.equal(request.generation, capture.request.generation);
        assert.equal(request.source_sha, capture.request.source_sha);
        assert.equal(request.coverage_digest, capture.request.coverage_digest);
        if ((capture.manifest && (request.checksum !== capture.manifest.checksum || request.byte_size !== capture.manifest.byte_size))) {
          return { ...response, outcome: "retained", reason: "capture_manifest_integrity_mismatch" };
        }
        if (capture.state === "available" && !capture.manifest) return { ...response, outcome: "retained", reason: "available_capture_manifest_unbound" };
        if (capture.reason === "archive integrity check failed" ||
            (capture.state === "needs_action" && !["upload_retry_window_exhausted", "storage quota exceeded", "upload failed; retry available"].includes(capture.reason ?? "")) ||
            (["preparing", "uploading", "discarded", "available"].includes(capture.state ?? "") && !!capture.reason)) {
          return { ...response, outcome: "retained", reason: "capture_failure_requires_attention" };
        }
        if (state.open && (capture.state === "expired" || Date.parse(capture.expires ?? "") <= state.now ||
            ["preparing", "uploading", "discarded", "needs_action"].includes(capture.state ?? ""))) {
          capture.state = "expired";
          return { ...response, outcome: "replaceable" };
        }
        return { ...response, outcome: "retained", reason: "capture_available" };
      },
    } : {}),
    releaseRecoveryCustody: async (...args: unknown[]) => {
      finals.push(structuredClone(args));
      if (state.finalError) throw state.finalError;
      if (receiptModel) {
        const generation = args[1] as number;
        const disposition = args[3] as RecoveryFinalDisposition;
        const committed = committedReceipts.get(generation);
        if (committed) {
          assert.deepEqual(disposition, committed, "committed FINAL receipts cannot be replaced");
        } else if (disposition.kind === "held_publication") {
          const record = (await coordinator.inspect(runId)).find(r => r.generation === generation)!;
          assert.equal(disposition.source_sha, record.sourceSha);
          assert.equal(disposition.coverage_digest, record.coverageDigest);
          assert.equal(disposition.publication_id, "00000000-0000-4000-8000-000000000009");
          committedReceipts.set(generation, Object.freeze(structuredClone(disposition)));
          state.open = false;
        } else {
          assert.equal(disposition.kind, "archive");
          const capture = [...captures.values()].find(c => c.id === disposition.capture_id);
          assert.ok(capture);
          if (capture.state === "expired" || Date.parse(capture.expires ?? "") <= state.now) {
            throw new RequestError("POST", "/api/worker/runs/run-1/recovery/release", 409, "capture expired");
          }
          assert.equal(disposition.source_sha, capture.request.source_sha);
          assert.equal(disposition.coverage_digest, capture.request.coverage_digest);
          assert.ok(capture.manifest);
          await state.beforeFinalAck?.();
          committedReceipts.set(generation, Object.freeze(structuredClone(disposition)));
          state.open = false;
        }
      }
      if (state.loseAck) { state.open = false; throw new Error("ACK lost"); }
      if (state.closeOnRelease && !state.wrongAck) state.open = false; // opt-in: a real server closes the hold
      return { run_id: state.wrongAck ? "other-run" : args[0], generation: args[1] as number, released: true, holds_released: 1 };
    },
  };
  const git = {
    recoverySettlementRoot: path.join(root, "settlement"),
    deleteRecoveryPin: async () => { state.pinDeletes++; state.candidates = []; },
    readInventoryCloneHeads: async () => state.cloneReadable
      ? ({ kind: "verified", heads: state.cloneHeads, clones: [], foreignOwners: [] }) : ({ kind: "unknown", cause: state.cloneCause }),
    ancestry: async (_bare: string, head: string, target?: string) =>
      state.ancestors.has(head) && state.refused.get(head) !== target ? "ancestor" : "unknown",
    enumerateOwedCandidates: async () => structuredClone(state.candidates),
    discoverOwedCandidates: async () => [{ context, candidates: structuredClone(state.candidates) }],
    resolveRecoveryBareDir: async () => context.barePath,
    buildRecoveryCoverage: async (_bare: string, _ctx: unknown, roots: string[], currentSha: string) => {
      aggregates.push(roots);
      const fingerprint = createHash("sha256").update(JSON.stringify({ roots: [...roots].sort(), currentSha })).digest("hex");
      return { sha: fingerprint.slice(0, 40), fingerprint };
    },
    fetchDefaultTip: async () => { throw new Error("guarded capture must be credential free"); },
    produceRecoveryBundle: async (_bare: string, opts: { outPath: string; sourceSha: string; forgeTip?: string; guardedDefaultBranch?: string }) => {
      state.produced++;
      state.onProduce?.();
      await state.produceWait;
      assert.equal(opts.forgeTip, undefined);
      if (state.oversized) throw new RecoveryBundleTooLargeError(100, 10);
      if (state.failProduce) throw new Error("bundle failure");
      assert.equal(opts.guardedDefaultBranch, "main");
      // Valid empty pack for the fake Git seam; the advertised source is exact.
      const pack = Buffer.concat([Buffer.from("PACK"), Buffer.from([0, 0, 0, 2, 0, 0, 0, 0])]);
      const bytes = Buffer.concat([Buffer.from(`# v2 git bundle\n${state.thin ? `-${H} retained dependency\n` : ""}${opts.sourceSha} refs/heads/recovered-source\n\n`),
        pack, createHash("sha1").update(pack).digest()]);
      await fs.writeFile(opts.outPath, bytes);
      return { bundlePath: opts.outPath, byteSize: bytes.length,
        checksum: createHash("sha256").update(bytes).digest("hex"),
        chunkCount: 1, prerequisiteShas: [], selfContained: true, alreadyPublished: false };
    },
  };
  const log = nullLogger();
  log.warn = (message, fields) => {
    if (message === "recovery: inventory source boundary retained") logs.push(fields!);
  };
  const make = (sourceBoundary: boolean | "runner" = false,
    pendingGenerationReport?: (runId: string, generation: number) => boolean,
    onAuthoritativeGenerationReleased?: (runId: string, generation: number) => void,
    testHooks?: { maintenanceBudget: () => MaintenanceBudget; afterPinObserve?: () => Promise<void> }) => {
    let physical: { withInventorySourceBoundary(context: InventorySourceBoundaryContext,
      action: (prove: () => Promise<boolean>) => Promise<void>): Promise<"passed" | "retained"> };
    const coordinator = new RecoveryCoordinator({
    recoveryRoot: path.join(root, "journal"), workerToken: "local-worker-fixture",
    log, client: client as never, git: git as never, now: () => state.now, pendingGenerationReport, onAuthoritativeGenerationReleased, testHooks,
    withInventorySourceBoundary: sourceBoundary === "runner" ? (context, action) =>
      physical.withInventorySourceBoundary(context, action) : sourceBoundary ? async (context, action) => {
      if (state.sourceRefused) {
        if (state.diagnostic) context.reportDiagnostic?.(state.diagnostic);
        return "retained";
      }
      await action(async () => {
        if (state.sourceRefused || state.sourceProofRefused) {
          if (state.diagnostic) context.reportDiagnostic?.(state.diagnostic);
          return false;
        }
        return true;
      });
      return "passed";
    } : undefined,
    });
    if (sourceBoundary === "runner") physical = new RunRunner(client as never, git as never,
      () => { throw new Error("unreached executor"); }, nullLogger(), 20, undefined,
      { recovery: coordinator }) as unknown as typeof physical;
    return coordinator;
  };
  const coordinator = make(sourceBoundary);
  const freeze = (originalSourceSha = H) => coordinator.freezeInventory({
    context, currentSha: H, originalSourceSha, defaultBranch: "main",
  });
  const capture = (record: RecoveryRecord) => coordinator.captureAndUpload({
    record, barePath: context.barePath, defaultBranch: "main",
  });
  return { root, context, state, logs, aggregates, finals, coordinator, make, freeze, capture, git,
    captures, reserveKeys, committedReceipts, client, log,
    reserves: () => reserves, close: () => fs.rm(root, { recursive: true, force: true }) };
}

async function pendingReceiptFixture(savedFinal = true) {
  const f = await fixture(false, os.tmpdir(), true);
  if (savedFinal) f.state.finalError = new Error("unaccepted FINAL");
  else f.state.expires = "1970-01-01T00:00:00Z";
  const record = await f.freeze();
  assert.ok(record);
  await f.capture(record);
  f.state.finalError = undefined;
  f.state.expires = "2099-01-01T00:00:00Z";
  const before = (await f.coordinator.inspect("run-1"))[0]!;
  return { f, before, capture: f.captures.get(f.reserveKeys[0]!)! };
}

for (const state of ["expired", "available", "preparing", "uploading", "discarded", "needs_action"]) {
  const reasons = state === "needs_action"
    ? ["upload_retry_window_exhausted", "storage quota exceeded", "upload failed; retry available"] : [undefined];
  for (const reason of reasons) for (const saved of [false, true]) {
    it(`issue2416 replacement allowlist ${state}/${reason} saved=${saved}`, async () => {
      const { f, before, capture } = await pendingReceiptFixture(saved);
      try {
        capture.state = state; capture.reason = reason; capture.expires = "1970-01-01T00:00:00Z";
        await f.make().resumePending(undefined, [before]);
        const after = (await f.coordinator.inspect("run-1"))[0]!;
        assert.equal(after.finalAcknowledged, true);
        assert.equal(after.captureId, before.captureId);
        assert.equal(after.bundlePath, before.bundlePath);
        assert.equal(f.state.produced, 1);
        assert.equal(f.reserveKeys.length, 2);
        assert.equal(f.reserveKeys[0], before.captureId, "legacy reserve key fallback");
        assert.match(after.reserveIdempotencyKey!, /^[0-9a-f-]{36}$/);
      } finally { await f.close(); }
    });
  }
}

for (const variant of ["checksum", "size", "expired-checksum", "expired-size", "discarded-checksum", "discarded-size", "integrity", "unknown", "absent", "unbound"]) {
  for (const saved of [false, true]) for (const expired of [false, true]) {
    it(`issue2416 retained ${variant} saved=${saved} expired=${expired} keeps bytes and cleanup pending`, async () => {
      const { f, before, capture } = await pendingReceiptFixture(saved);
      try {
        capture.expires = expired ? "1970-01-01T00:00:00Z" : "2099-01-01T00:00:00Z";
        capture.state = variant.startsWith("expired") ? "expired" : variant.startsWith("discarded") ? "discarded" :
          ["integrity", "unknown", "absent"].includes(variant) ? "needs_action" : "available";
        if (variant.includes("checksum")) capture.manifest!.checksum = "0".repeat(64);
        if (variant.includes("size")) capture.manifest!.byte_size++;
        if (variant === "integrity") capture.reason = "archive integrity check failed";
        if (variant === "unknown") capture.reason = "unexpected failure";
        if (variant === "unbound") delete capture.manifest;
        const bytes = await fs.readFile(before.bundlePath!);
        const pins = structuredClone(f.state.candidates);
        const finals = f.finals.length;
        const remote = structuredClone([...f.captures]);
        const outcome = await f.capture(before);
        const reason = variant.includes("checksum") || variant.includes("size") ? "capture_manifest_integrity_mismatch" :
          variant === "unbound" ? "available_capture_manifest_unbound" : "capture_failure_requires_attention";
        assert.equal(outcome.reason, reason, "foreground outcome exposes bounded owner attention");
        const expected = { ...before, reason };
        assert.deepEqual((await f.coordinator.inspect("run-1"))[0], expected, "only bounded reason may change");
        assert.deepEqual([...f.captures], remote, "remote manifests and captures are untouched");
        assert.equal(f.state.uploads, 1);
        assert.equal(f.reserveKeys.length, 1);
        assert.equal(f.finals.length, finals);
        assert.deepEqual(await fs.readFile(before.bundlePath!), bytes);
        assert.equal(await f.make().inventoryCleanupState("run-1", 7), "pending");
        await f.make().forgetGeneration("run-1", 7);
        assert.deepEqual((await f.coordinator.inspect("run-1"))[0], expected);
        assert.deepEqual(await fs.readFile(before.bundlePath!), bytes);
        assert.deepEqual(f.state.candidates, pins);
        assert.equal(f.state.pinDeletes, 0, "forget must not invoke the pin deletion seam before ACK");
      } finally { await f.close(); }
    });
  }
}

for (const state of ["preparing", "uploading", "needs_action"]) {
  it(`issue2416 incomplete unbound ${state} explicitly replaces before expiry`, async () => {
    const { f, before, capture } = await pendingReceiptFixture();
    try {
      capture.state = state;
      capture.expires = "2099-01-01T00:00:00Z";
      capture.reason = state === "needs_action" ? "upload_retry_window_exhausted" : undefined;
      delete capture.manifest;
      await f.make().resumePending(undefined, [before]);
      assert.equal(f.reserveKeys.length, 2);
      assert.equal((await f.coordinator.inspect("run-1"))[0]!.finalAcknowledged, true);
    } finally { await f.close(); }
  });
}

it("issue2416 replacement FINAL lost ACK preserves custody then exact accepted receipt avoids third key", async () => {
  const { f, before, capture } = await pendingReceiptFixture();
  try {
    capture.state = "expired";
    await patchAuthenticated(f, before, { recoveryPinBareDir: "repo.git" });
    const bytes = await fs.readFile(before.bundlePath!);
    const pins = structuredClone(f.state.candidates);
    const client = f.client as RecoveryArchiveClient;
    const reconcile = client.reconcileRecoveryCapture!;
    client.reconcileRecoveryCapture = async (...args) => {
      if (f.committedReceipts.has(7)) throw new Error("reconciliation temporarily unavailable after lost ACK");
      return reconcile(...args);
    };
    f.state.loseAck = true;
    await f.make().resumePending(undefined, [before]);
    const pending = (await f.coordinator.inspect("run-1"))[0]!;
    assert.ok(pending.finalRequest);
    assert.notDeepEqual(pending.finalRequest, before.finalRequest);
    assert.notEqual(pending.finalAcknowledged, true);
    assert.equal(await f.make().inventoryCleanupState("run-1", 7), "pending");
    await f.make().forgetGeneration("run-1", 7);
    assert.deepEqual((await f.make().inspect("run-1"))[0], pending);
    assert.deepEqual(await fs.readFile(pending.bundlePath!), bytes);
    assert.deepEqual(f.state.candidates, pins);
    assert.equal(f.state.pinDeletes, 0);
    assert.equal(f.reserveKeys.length, 2);
    const finals = f.finals.length;
    client.reconcileRecoveryCapture = reconcile;
    // Force reconciliation rather than a healthy status replay of FINAL.
    f.captures.get(pending.reserveIdempotencyKey!)!.state = "expired";
    await f.make().resumePending(undefined, [pending]);
    const accepted = (await f.make().inspect("run-1"))[0]!;
    assert.equal(accepted.finalAcknowledged, true);
    assert.deepEqual(accepted.finalRequest, pending.finalRequest);
    assert.equal(accepted.reserveIdempotencyKey, pending.reserveIdempotencyKey);
    assert.equal(f.reserveKeys.length, 2);
    assert.equal(f.finals.length, finals);
    assert.equal(await f.make().inventoryCleanupState("run-1", 7), "acknowledged");
  } finally { await f.close(); }
});

for (const newer of ["identity", "acknowledged"]) {
  it(`issue2416 stale replaceable response preserves newer ${newer} journal`, async () => {
    const { f, before, capture } = await pendingReceiptFixture();
    try {
      capture.state = "expired";
      const client = f.client as RecoveryArchiveClient;
      const reconcile = client.reconcileRecoveryCapture!;
      let latest: RecoveryRecord | undefined;
      client.reconcileRecoveryCapture = async (...args) => {
        const response = await reconcile(...args);
        const patch = newer === "acknowledged" ? { finalAcknowledged: true } : {
          serverCaptureId: "new-server-identity", reserveIdempotencyKey: "00000000-0000-4000-8000-000000000241",
          finalRequest: { disposition: { ...before.finalRequest!.disposition, capture_id: "new-server-identity" } },
        };
        await patchAuthenticated(f, before, patch);
        latest = (await f.make().inspect("run-1"))[0]!;
        return response;
      };
      await f.make().resumePending(undefined, [before]);
      assert.ok(latest);
      assert.deepEqual((await f.make().inspect("run-1"))[0], latest);
      assert.equal(f.reserveKeys.length, 1);
      assert.equal(f.state.uploads, 1);
    } finally { await f.close(); }
  });
}

it("issue2416 retained manifest invalid reason is bounded and stale reason writes are refused", async () => {
  const { f, before, capture } = await pendingReceiptFixture();
  try {
    capture.state = "expired";
    const client = f.client as RecoveryArchiveClient;
    client.reconcileRecoveryCapture = async (run, id, req) => ({
      run_id: run, capture_id: id, generation: req.generation,
      outcome: "retained", reason: "capture_manifest_invalid",
    });
    assert.equal((await f.capture(before)).reason, "capture_manifest_invalid");
    const current = (await f.make().inspect("run-1"))[0]!;
    client.reconcileRecoveryCapture = async (run, id, req) => {
      await patchAuthenticated(f, current, { reserveIdempotencyKey: "00000000-0000-4000-8000-000000000241" });
      return { run_id: run, capture_id: id, generation: req.generation,
        outcome: "retained", reason: "capture_failure_requires_attention" };
    };
    await f.capture(current);
    const latest = (await f.make().inspect("run-1"))[0]!;
    assert.equal(latest.reason, "capture_manifest_invalid");
    assert.equal(latest.reserveIdempotencyKey, "00000000-0000-4000-8000-000000000241");
    assert.deepEqual(latest.finalRequest, before.finalRequest);
    assert.equal(f.reserveKeys.length, 1);
  } finally { await f.close(); }
});

for (const same of [true, false]) {
  it(`issue2416 accepted expired receipt exact=${same} never reserves`, async () => {
    const f = await fixture(false, os.tmpdir(), true);
    try {
      f.state.loseAck = true;
      const reconcile = f.client.reconcileRecoveryCapture;
      delete f.client.reconcileRecoveryCapture;
      const record = await f.freeze(); assert.ok(record);
      await f.capture(record);
      f.client.reconcileRecoveryCapture = reconcile;
      const before = (await f.coordinator.inspect("run-1"))[0]!;
      f.captures.get(f.reserveKeys[0]!)!.state = "expired";
      f.captures.get(f.reserveKeys[0]!)!.manifest!.checksum = "0".repeat(64);
      if (!same) f.committedReceipts.set(7, { ...before.finalRequest!.disposition, source_sha: H2 });
      await f.make().resumePending(undefined, [before]);
      const after = (await f.coordinator.inspect("run-1"))[0]!;
      assert.equal(after.finalAcknowledged === true, same);
      assert.deepEqual(after.finalRequest, before.finalRequest);
      assert.equal(f.reserveKeys.length, 1);
      assert.equal(f.finals.length, 1);
    } finally { await f.close(); }
  });
}

for (const mode of ["foreground", "boot", "live", "concurrent"]) {
  it(`issue2416 ${mode} replacement uses existing generation lock`, async () => {
    const { f, before, capture } = await pendingReceiptFixture();
    try {
      capture.state = "expired";
      const restarted = f.make();
      if (mode === "foreground") await f.capture(before);
      if (mode === "boot") await restarted.resumePending(undefined, [before]);
      if (mode === "live") await restarted.resumeLive({ authenticatedAtMs: f.state.now, isExecuting: () => false });
      if (mode === "concurrent") {
        let active = 0, maximum = 0;
        let entered!: () => void, release!: () => void;
        const entry = new Promise<void>(resolve => { entered = resolve; });
        const barrier = new Promise<void>(resolve => { release = resolve; });
        const timer = setTimeout(release, 2000);
        const upload = f.client.uploadRecoveryBundle;
        f.client.uploadRecoveryBundle = async (...args) => {
          active++; maximum = Math.max(maximum, active); entered();
          try { await barrier; return await upload(...args); } finally { active--; }
        };
        const first = f.capture(before);
        const tasks: Promise<unknown>[] = [first];
        try {
          await Promise.race([entry, new Promise<never>((_, reject) => {
            const timeout = setTimeout(() => reject(new Error("upload barrier not reached")), 3000);
            entry.finally(() => clearTimeout(timeout));
          })]);
          const siblings = [restarted.resumePending(undefined, [before]), f.capture(before)];
          tasks.push(...siblings);
          await new Promise<void>(resolve => setTimeout(resolve, 25));
          assert.equal(active, 1);
          release();
          await Promise.all([first, ...siblings]);
          assert.equal(maximum, 1, "concurrent boot/foreground uploads never overlap");
          assert.equal(active, 0);
        } finally { clearTimeout(timer); release(); await Promise.allSettled(tasks); }
      }
      assert.equal(f.reserveKeys.length, 2);
      assert.equal(f.state.uploads, 2);
      assert.equal((await f.coordinator.inspect("run-1"))[0]!.finalAcknowledged, true);
    } finally { await f.close(); }
  });
}

it("issue2416 immediately unusable replacement is bounded to one reserve per pass", async () => {
  const { f, before, capture } = await pendingReceiptFixture();
  try {
    capture.state = "expired";
    f.state.expires = "1970-01-01T00:00:00Z";
    await f.make().resumePending(undefined, [before]);
    assert.equal(f.reserveKeys.length, 2);
    assert.equal(f.state.uploads, 2);
    assert.equal((await f.coordinator.inspect("run-1"))[0]!.finalAcknowledged, undefined);
    assert.equal(await f.make().inventoryCleanupState("run-1", 7), "pending");
  } finally { await f.close(); }
});

for (const lostAck of [true, false]) {
  it(`issue2416 persisted replacement key survives restart reserve failure lostAck=${lostAck}`, async () => {
    const { f, before, capture } = await pendingReceiptFixture();
    try {
      capture.state = "expired";
      await patchAuthenticated(f, before, { finalAcknowledged: false, recoveryPinBareDir: "repo.git" });
      const bytes = await fs.readFile(before.bundlePath!);
      const pins = structuredClone(f.state.candidates);
      const assertPending = async () => {
        assert.equal(await f.make().inventoryCleanupState("run-1", 7), "pending");
        const journal = await f.make().inspect("run-1");
        await f.make().forgetGeneration("run-1", 7);
        assert.deepEqual(await f.make().inspect("run-1"), journal);
        assert.deepEqual(await fs.readFile(before.bundlePath!), bytes);
        assert.deepEqual(f.state.candidates, pins);
        assert.equal(f.state.pinDeletes, 0);
      };
      const reserve = f.client.reserveRecoveryCapture;
      let fail = true;
      f.client.reserveRecoveryCapture = async (run, req) => {
        const persisted = (await f.coordinator.inspect("run-1"))[0]!;
        assert.equal(persisted.reserveIdempotencyKey, req.idempotency_key);
        assert.equal(Object.hasOwn(persisted, "finalAcknowledged"), false);
        assert.equal(persisted.finalRequest, undefined);
        if (!fail) return reserve(run, req);
        if (lostAck) await reserve(run, req);
        else { f.reserveKeys.push(req.idempotency_key); }
        throw new RequestError("POST", "/archives/reserve", lostAck ? 500 : 507, "storage quota exceeded");
      };
      await f.make().resumePending(undefined, [before]);
      const pending = (await f.coordinator.inspect("run-1"))[0]!;
      assert.ok(pending.reserveIdempotencyKey);
      assert.equal(pending.serverCaptureId, undefined);
      await assertPending();
      await f.make().resumePending(undefined, [pending]);
      assert.equal((await f.coordinator.inspect("run-1"))[0]!.reserveIdempotencyKey, pending.reserveIdempotencyKey);
      await assertPending();
      fail = false;
      await f.make().resumePending(undefined, [pending]);
      assert.equal((await f.coordinator.inspect("run-1"))[0]!.finalAcknowledged, true);
      assert.deepEqual(new Set(f.reserveKeys.slice(1)), new Set([pending.reserveIdempotencyKey]));
      assert.equal(await f.make().inventoryCleanupState("run-1", 7), "acknowledged");
      // #2464: an acknowledged guarded generation is removed only through guarded cleanup.
      Object.assign(f.git, { cleanupRecoveryGeneration: async () => "removed" });
      await f.make().forgetGeneration("run-1", 7);
      assert.deepEqual(await f.make().inspect("run-1"), []);
      await assert.rejects(fs.readFile(before.bundlePath!), { code: "ENOENT" });
    } finally { await f.close(); }
  });
}

it("issue2416 authenticated malformed reservation UUID and tampering are refused", async () => {
  const { f, before } = await pendingReceiptFixture();
  try {
    await patchAuthenticated(f, before, { reserveIdempotencyKey: "invalid-uuid" });
    assert.deepEqual(await f.make().inspect("run-1"), []);
    await patchAuthenticated(f, before, { reserveIdempotencyKey: "00000000-0000-4000-8000-000000000241" });
    assert.equal((await f.make().inspect("run-1"))[0]!.reserveIdempotencyKey, "00000000-0000-4000-8000-000000000241");
    const file = path.join(f.root, "journal", before.runId, before.captureId + ".json");
    await fs.writeFile(file, (await fs.readFile(file, "utf8")).replace("000000000241", "000000000242"));
    assert.deepEqual(await f.make().inspect("run-1"), []);
  } finally { await f.close(); }
});

for (const variant of ["missing", "route", "auth", "transport", "run", "generation", "capture", "outcome", "receipt", "evidence", "reason", "integrity-contradiction", "status", "hold", "null", "array", "retained-unknown", "retained-receipt", "retained-evidence"]) {
  it(`issue2416 reconciliation fails closed for ${variant}`, async () => {
    const { f, before, capture } = await pendingReceiptFixture();
    try {
      capture.state = "expired";
      const client = f.client as RecoveryArchiveClient;
      const reconcile = client.reconcileRecoveryCapture!;
      if (variant === "missing") delete client.reconcileRecoveryCapture;
      else client.reconcileRecoveryCapture = async (run, id, req) => {
        if (["route", "auth", "transport"].includes(variant)) throw new RequestError("POST", "/archives/reconcile", variant === "route" ? 404 : variant === "auth" ? 401 : 500, "unavailable");
        if (variant === "null") return null as never;
        if (variant === "array") return [] as never;
        const result = await reconcile(run, id, req);
        if (variant.startsWith("retained-")) {
          result.outcome = "retained";
          result.reason = variant === "retained-unknown" ? "unbounded raw failure " + "x".repeat(2048) : "capture_manifest_invalid";
          if (variant === "retained-receipt") result.final_receipt = before.finalRequest!.disposition;
          if (variant === "retained-evidence") result.release_evidence = "archive";
        }
        if (variant === "run") result.run_id = "other";
        if (variant === "generation") result.generation++;
        if (variant === "capture") result.capture_id = "other";
        if (variant === "outcome") result.outcome = "unknown" as never;
        if (variant === "receipt") result.final_receipt = before.finalRequest!.disposition;
        if (variant === "evidence") result.release_evidence = "archive";
        if (variant === "reason") result.reason = "unknown";
        if (variant === "integrity-contradiction" || variant === "status") return { run_id: run, generation: req.generation, capture_id: id, outcome: "replaceable" };
        return result;
      };
      if (variant === "integrity-contradiction") capture.manifest!.checksum = "0".repeat(64);
      if (variant === "status") client.getRecoveryCaptureStatus = async () => ({ capture_id: "other", state: "expired", manifest_bound: true });
      if (variant === "hold") client.listRecoveryHolds = async () => { throw new Error("hold transport failed"); };
      const finals = f.finals.length;
      await f.make().resumePending(undefined, [before]);
      assert.deepEqual((await f.coordinator.inspect("run-1"))[0], { ...before, reason: "final_retry_unknown" });
      assert.equal(f.reserveKeys.length, 1);
      assert.equal(f.finals.length, finals);
    } finally { await f.close(); }
  });
}

for (const seam of ["reconcile", "retained-reconcile", "hold"]) {
  it(`issue2416 quarantine arriving during ${seam} prevents reset and replacement RPCs`, async () => {
    const { latchResidueQuarantine, resetResidueQuarantineForTests } = await import("../src/residue-quarantine.js");
    const { f, before, capture } = await pendingReceiptFixture();
    try {
      capture.state = "expired";
      const client = f.client as RecoveryArchiveClient;
      if (seam === "reconcile" || seam === "retained-reconcile") {
        const reconcile = client.reconcileRecoveryCapture!;
        client.reconcileRecoveryCapture = async (...args) => {
          const result = await reconcile(...args);
          latchResidueQuarantine({ cause: "fixture", site: "reconcile_return" }, nullLogger());
          return seam === "retained-reconcile" ? { ...result, outcome: "retained", reason: "capture_manifest_invalid" } : result;
        };
      } else {
        const hold = client.listRecoveryHolds;
        client.listRecoveryHolds = async (...args) => {
          const result = await hold(...args);
          latchResidueQuarantine({ cause: "fixture", site: "hold_return" }, nullLogger());
          return result;
        };
      }
      const finals = f.finals.length;
      await f.make().resumePending(undefined, [before]);
      assert.deepEqual((await f.coordinator.inspect("run-1"))[0], before);
      assert.equal(f.reserveKeys.length, 1);
      assert.equal(f.state.uploads, 1);
      assert.equal(f.finals.length, finals);
    } finally { resetResidueQuarantineForTests(); await f.close(); }
  });
}

async function patchAuthenticated(f: Awaited<ReturnType<typeof fixture>>, record: RecoveryRecord, patch: Partial<RecoveryRecord>) {
  const file = path.join(f.root, "journal", record.runId, record.captureId + ".json");
  const envelope = JSON.parse(await fs.readFile(file, "utf8"));
  const { mac: _mac, ...payload } = envelope;
  Object.assign(payload, patch);
  const key = createHmac("sha256", "local-worker-fixture").update("uzi-recovery-journal-v1").digest();
  await fs.writeFile(file, JSON.stringify({ ...payload, mac: createHmac("sha256", key).update(canonicalJson(payload)).digest("hex") }));
}

for (const savedFinal of [false, true]) {
  it(`issue2416 core expired ${savedFinal ? "saved-unaccepted FINAL" : "uploaded no FINAL"} reserves replacement and acknowledges stable journal`, async () => {
    const f = await fixture(false, os.tmpdir(), true);
    try {
      if (savedFinal) f.state.finalError = new Error("FINAL rejected before acceptance");
      else f.state.expires = "1970-01-01T00:00:00Z";
      const record = await f.freeze();
      assert.ok(record);
      await f.capture(record);
      const before = (await f.coordinator.inspect("run-1")).find(r => r.captureId === record.captureId)!;
      assert.equal(before.state, "uploaded");
      assert.equal(!!before.finalRequest, savedFinal);
      assert.notEqual(before.finalAcknowledged, true);
      assert.equal(f.committedReceipts.size, 0, "the original FINAL was never accepted");
      assert.equal(f.state.open, true);
      assert.equal(f.reserveKeys.length, 1);
      const oldKey = f.reserveKeys[0]!;
      const oldCapture = f.captures.get(oldKey)!;
      oldCapture.state = "expired";
      oldCapture.expires = "1970-01-01T00:00:00Z";
      f.state.expires = "2099-01-01T00:00:00Z";
      f.state.finalError = undefined;
      const pins = structuredClone(f.state.candidates);
      const bundle = await fs.readFile(before.bundlePath!);
      const savedIdentity = structuredClone(before.finalRequest);
      let ackBoundaryChecked = false;
      f.state.beforeFinalAck = async () => {
        const pending = (await f.coordinator.inspect("run-1")).find(r => r.captureId === record.captureId)!;
        assert.ok(pending, "the original source journal survives replacement");
        assert.notEqual(pending.finalAcknowledged, true);
        assert.equal(pending.bundlePath, before.bundlePath);
        assert.deepEqual(await fs.readFile(pending.bundlePath!), bundle, "bundle bytes survive until ACK");
        assert.deepEqual(f.state.candidates, pins, "owed pins survive until ACK");
        assert.deepEqual(pending.originalRoots, before.originalRoots);
        assert.equal(pending.sourceSha, before.sourceSha);
        assert.equal(pending.coverageDigest, before.coverageDigest);
        assert.equal(await f.make().inventoryCleanupState("run-1", 7), "pending");
        ackBoundaryChecked = true;
      };
      await f.make().resumePending(undefined, [before]);
      assert.ok(f.reserveKeys.length > 1, "expired unaccepted capture must reserve a replacement");
      assert.notEqual(f.reserveKeys[1], oldKey, "replacement uses a distinct server idempotency key");
      assert.equal(f.state.produced, 1, "replacement reuses the verified local bundle");
      assert.equal(f.state.uploads, 2);
      assert.ok(ackBoundaryChecked, "replacement reaches FINAL while local custody is retained");
      const after = (await f.coordinator.inspect("run-1")).find(r => r.captureId === record.captureId)!;
      assert.ok(after, "replacement preserves the stable journal captureId");
      assert.equal(after.finalAcknowledged, true);
      assert.notEqual(after.serverCaptureId, before.serverCaptureId);
      assert.equal(after.finalRequest?.disposition.capture_id, after.serverCaptureId);
      assert.deepEqual(before.finalRequest, savedIdentity, "the saved request snapshot remains immutable");
      assert.deepEqual(f.committedReceipts.get(7), after.finalRequest?.disposition);
      assert.equal(f.committedReceipts.size, 1);
      assert.equal(f.state.open, false);
      assert.equal(await f.make().inventoryCleanupState("run-1", 7), "acknowledged");
    } finally { await f.close(); }
  });
}

it("guarded boot failure does not block a sibling run", async () => {
  const f = await fixture();
  try {
    f.state.candidates = [];
    f.state.status = "completed";
    const first = await f.coordinator.freezeInventory({
      context: f.context, currentSha: H, defaultBranch: "main", settledEvidence: "publication",
    });
    const second = await f.coordinator.freezeInventory({
      context: { ...f.context, runId: "run-2" }, currentSha: H, defaultBranch: "main", settledEvidence: "publication",
    });
    assert.ok(first && second);
    const visited: string[] = [];
    f.git.enumerateOwedCandidates = async (_bare?: string, run?: string) => {
      visited.push(run!);
      if (run === "run-1") throw new Error("unreadable owed inventory");
      return [];
    };
    await assert.doesNotReject(f.coordinator.resumePending(undefined, [first, second]));
    assert.deepEqual(visited, ["run-1", "run-2"]);
    assert.equal(f.finals.length, 1);
    assert.equal((await f.coordinator.inspect("run-2"))[0]!.finalAcknowledged, true);
    assert.equal((await f.coordinator.inspect("run-1"))[0]!.finalAcknowledged, undefined);
  } finally { await f.close(); }
});

it("issue2416 live FINAL success that forgets the generation reports uploaded, not record_removed", async () => {
  const f = await fixture(false, os.tmpdir(), true);
  try {
    const record = await f.freeze();
    assert.ok(record);
    f.state.finalError = new Error("network unavailable");
    assert.equal((await f.capture(record)).reason, "upload_transient");
    const pending = (await f.coordinator.inspect("run-1"))[0]!;
    assert.equal(pending.state, "uploaded");
    assert.ok(pending.finalRequest);
    const outcomes: unknown[] = [];
    f.log.info = (message, fields) => {
      if (message === "recovery live re-drive") outcomes.push((fields as { outcome?: unknown }).outcome);
    };
    Object.assign(f.git, { cleanupRecoveryGeneration: async () => "removed" });
    f.state.finalError = undefined;
    f.state.now += 60_000;
    await f.coordinator.resumeLive({ isExecuting: () => false, authenticatedAtMs: f.state.now });
    assert.equal(f.finals.length, 2, "the live pass replays the saved FINAL");
    assert.deepEqual(await f.coordinator.inspect("run-1"), [], "guarded cleanup removed the generation");
    await assert.rejects(fs.readFile(pending.bundlePath!), { code: "ENOENT" });
    assert.deepEqual(outcomes, ["uploaded"]);
  } finally { await f.close(); }
});

for (const variant of ["network", "5xx", "timeout", "generation_not_ended", "attention", "quota"] as const) {
  it(`issue2416 retained FINAL reconciliation preserves retry classification ${variant}`, async () => {
    const f = await fixture(false, os.tmpdir(), true);
    try {
      const warnings: string[] = [];
      f.log.warn = message => { warnings.push(message); };
      const record = await f.freeze();
      assert.ok(record);
      f.state.finalError = variant === "quota"
        ? new RequestError("POST", "/api/worker/runs/run-1/recovery/release", 507, JSON.stringify({ reason: "quota" }))
        : variant === "5xx"
        ? new RequestError("POST", "/api/worker/runs/run-1/recovery/release", 503, "service unavailable")
        : variant === "timeout" ? new DOMException("FINAL timed out", "TimeoutError")
        : new Error("network unavailable");
      const reason = variant === "generation_not_ended" ? "generation_not_ended"
        : variant === "attention" ? "capture_failure_requires_attention" : "capture_available";
      const reconcile = f.client.reconcileRecoveryCapture!;
      let reconciliations = 0;
      f.client.reconcileRecoveryCapture = async (run, id, request) => {
        reconciliations++;
        const response = await reconcile(run, id, request);
        assert.equal(response.outcome, "retained");
        assert.ok("reason" in response);
        assert.equal(response.reason, "capture_available");
        return { ...response, reason };
      };
      const outcome = await f.capture(record);
      assert.equal(reconciliations, 1, "the receipt endpoint is reached after FINAL fails");
      const expected = variant === "attention" ? reason : variant === "quota" ? "storage_quota_exceeded" : "upload_transient";
      assert.equal(outcome.reason, expected);
      const before = (await f.coordinator.inspect("run-1"))[0]!;
      assert.equal(before.state, "uploaded");
      assert.ok(before.finalRequest);
      assert.equal(before.finalAcknowledged, undefined);
      assert.equal(before.reason, variant === "attention" ? reason : undefined);
      const capture = [...f.captures.values()][0]!;
      assert.ok(capture.manifest, "the retained capture has healthy available bytes");
      assert.ok(Date.parse(capture.expires!) > f.state.now);
      const bytes = await fs.readFile(before.bundlePath!);
      const pins = structuredClone(f.state.candidates);
      const assertPending = async () => {
        const current = (await f.coordinator.inspect("run-1"))[0]!;
        assert.deepEqual(current, before, "retries preserve the journal and exact FINAL identity");
        assert.deepEqual(await fs.readFile(before.bundlePath!), bytes);
        assert.deepEqual(f.state.candidates, pins);
        assert.equal(f.state.pinDeletes, 0);
        assert.equal(await f.coordinator.inventoryCleanupState("run-1", 7), "pending");
        assert.equal(f.reserves(), 1);
        assert.equal(f.state.uploads, 1);
        assert.equal(f.state.produced, 1);
        assert.ok(f.finals.every(request => canonicalJson(request) === canonicalJson(f.finals[0])));
        assert.equal(warnings.includes("recovery: inventory requires owner attention"), variant === "attention");
      };
      const live = () => f.coordinator.resumeLive({ isExecuting: () => false, authenticatedAtMs: f.state.now });
      await live();
      assert.equal(f.finals.length, 2, "first transient live pass reaches FINAL");
      await assertPending();
      if (variant === "attention") {
        f.state.now += 30_000;
        await live();
        assert.equal(f.finals.length, 2, "owner-attention retry respects its per-record delay");
        await assertPending();
        f.state.now += 30_000;
        await live();
        assert.equal(f.finals.length, 3, "owner-attention retry remains eligible when due");
        await assertPending();
      } else {
        f.state.now += 60_000 - 1;
        await live();
        assert.equal(f.finals.length, 2, "first transient delay is 60 seconds");
        f.state.now++;
        await live();
        assert.equal(f.finals.length, 3, "second transient live pass reaches FINAL");
        await assertPending();
        f.state.now += 120_000 - 1;
        await live();
        assert.equal(f.finals.length, 3, "second transient delay doubles to 120 seconds");
        f.state.now++;
        f.state.finalError = undefined;
        await live();
        assert.equal(f.finals.length, 4);
        const after = (await f.coordinator.inspect("run-1"))[0]!;
        assert.equal(after.finalAcknowledged, true);
        assert.deepEqual(after.finalRequest, before.finalRequest);
        assert.deepEqual(await fs.readFile(before.bundlePath!), bytes);
        assert.deepEqual(f.state.candidates, pins);
        assert.equal(f.state.pinDeletes, 0);
      }
    } finally { await f.close(); }
  });
}

for (const phase of ["reserve-count", "upload"] as const) {
  it(`U1 guarded ${phase} quota retains source and bytes through live and restart refusals`, async () => {
    const f = await fixture();
    try {
      const quota = new RequestError("POST", "/api/worker/recovery", 507, JSON.stringify({ reason: "quota" }));
      let refused = true;
      let uploadAttempts = 0;
      const upload = f.client.uploadRecoveryBundle;
      f.client.uploadRecoveryBundle = async (run, id, manifest, stream) => {
        uploadAttempts++;
        if (refused && phase === "upload") {
          for await (const chunk of stream) assert.ok(chunk);
          throw quota;
        }
        return upload(run, id, manifest, stream);
      };
      if (phase === "reserve-count") f.state.reserveError = quota;
      const record = await f.freeze();
      assert.ok(record);
      assert.equal((await f.capture(record)).reason, "storage_quota_exceeded");
      const before = (await f.coordinator.inspect("run-1"))[0]!;
      assert.equal(before.state, "needs_action");
      assert.equal(before.reason, "storage_quota_exceeded");
      assert.equal(before.serverCaptureId, phase === "reserve-count" ? undefined : "server-1");
      const bytes = await fs.readFile(before.bundlePath!);
      const pins = structuredClone(f.state.candidates);
      const retained = async (coordinator: RecoveryCoordinator) => {
        assert.deepEqual((await coordinator.inspect("run-1"))[0], before);
        assert.deepEqual(await fs.readFile(before.bundlePath!), bytes);
        assert.deepEqual(f.state.candidates, pins);
        assert.equal(f.state.produced, 1);
        assert.equal(f.state.pinDeletes, 0);
        assert.equal(f.finals.length, 0);
        assert.equal(await coordinator.inventoryCleanupState("run-1", 7), "pending");
      };
      const attempts = () => phase === "reserve-count" ? f.reserves() : uploadAttempts;
      const live = () => f.coordinator.resumeLive({ isExecuting: () => false, authenticatedAtMs: f.state.now });
      await live();
      assert.equal(attempts(), 2, "guarded quota is eligible for live retry");
      await retained(f.coordinator);
      f.state.now += 60_000 - 1;
      await live();
      assert.equal(attempts(), 2, "quota keeps transient backoff");
      f.state.now++;
      await live();
      assert.equal(attempts(), 3);
      await retained(f.coordinator);
      f.state.now += 120_000 - 1;
      await live();
      assert.equal(attempts(), 3, "repeated quota doubles backoff");

      const restarted = f.make();
      await restarted.resumePending();
      assert.equal(attempts(), 4, "restart retries without reproducing");
      await retained(restarted);
      refused = false;
      f.state.reserveError = undefined;
      await restarted.resumeLive({ isExecuting: () => false, authenticatedAtMs: f.state.now });
      const after = (await restarted.inspect("run-1"))[0]!;
      assert.equal(after.state, "uploaded");
      assert.equal(after.reason, undefined);
      assert.equal(after.finalAcknowledged, true);
      assert.equal(after.serverCaptureId, "server-1");
      for (const key of ["captureId", "sourceSha", "bundlePath", "checksum", "byteSize", "chunkCount", "coverageDigest", "generation"] as const) {
        assert.equal(after[key], before[key], key);
      }
      assert.deepEqual(await fs.readFile(after.bundlePath!), bytes);
      assert.equal(f.state.produced, 1);
      assert.equal(f.captures.size, 1, "count refusals allocate no server capture");
      assert.equal(new Set(f.reserveKeys).size, 1, "every reserve reuses the original identity");
      assert.equal(f.finals.length, 1, "only successful upload permits FINAL");
    } finally { await f.close(); }
  });
}

for (const phase of ["capture", "FINAL"] as const) {
  for (const credential of [false, true]) {
    it(`guarded ${phase} ${credential ? "credential rejection" : "transient failure"} spaces live retries`, async () => {
      const f = await fixture();
      try {
        const record = await f.freeze();
        assert.ok(record);
        const error = credential
          ? new RequestError("POST", "/api/worker/recovery", 401, "unauthorized")
          : new Error("network unavailable");
        if (phase === "capture") f.state.reserveError = error;
        else f.state.finalError = error;
        const outcome = await f.capture(record);
        assert.equal(outcome.reason, credential ? "credential_rejected" : phase === "capture" ? "capture_error" : "upload_transient");
        const before = (await f.coordinator.inspect("run-1"))[0]!;
        await f.coordinator.resumeLive({ isExecuting: () => false, authenticatedAtMs: f.state.now + 1 });
        const pending = (await f.coordinator.inspect("run-1"))[0]!;
        assert.equal(pending.finalAcknowledged, undefined);
        if (phase === "FINAL") {
          assert.equal(pending.state, "uploaded");
          assert.deepEqual(pending.finalRequest, before.finalRequest);
        }
        const attempts = phase === "capture" ? f.reserves() : f.finals.length;
        f.state.now += 30_000;
        await f.coordinator.resumeLive({ isExecuting: () => false, authenticatedAtMs: credential ? 1000 : f.state.now });
        assert.equal(phase === "capture" ? f.reserves() : f.finals.length, attempts);
        f.state.now += 30_000;
        f.state.reserveError = undefined;
        f.state.finalError = undefined;
        await f.coordinator.resumeLive({ isExecuting: () => false, authenticatedAtMs: f.state.now });
        const after = (await f.coordinator.inspect("run-1"))[0]!;
        assert.equal(after.finalAcknowledged, true);
        if (phase === "FINAL") {
          assert.equal(before.state, "uploaded");
          assert.equal(after.state, "uploaded");
          assert.deepEqual(after.finalRequest, before.finalRequest);
          assert.ok(f.finals.every(request => canonicalJson(request) === canonicalJson(f.finals[0])));
          assert.equal(f.reserves(), 1);
        }
      } finally { await f.close(); }
    });
  }
}

it("final inventory ACK covers an earlier available aggregate without treating it as an adopted head", async () => {
  const f = await fixture();
  try {
    f.state.status = "running";
    const earlier = await f.coordinator.freezeInventory({
      context: f.context, currentSha: H, defaultBranch: "main", locallyQuiescent: true,
    });
    assert.ok(earlier);
    await f.capture(earlier);
    assert.equal(f.finals.length, 0, "an active generation cannot finalize its earlier archive");
    f.state.candidates.push({ sha: H2, pinRef: "refs/owed/b", contexts: [structuredClone(f.context)] });
    f.state.status = "failed";
    const covering = await f.freeze(H2);
    assert.ok(covering);
    assert.notEqual(covering.captureId, earlier.captureId);
    await f.capture(covering);
    const records = await f.coordinator.inspect(f.context.runId);
    const final = records.find(r => r.captureId === covering.captureId)!;
    assert.equal(final.finalAcknowledged, true);
    assert.notEqual(final.serverCaptureId, records.find(r => r.captureId === earlier.captureId)?.serverCaptureId);
    assert.equal(await f.coordinator.inventoryCleanupState(f.context.runId, f.context.generation), "acknowledged");
  } finally { await f.close(); }
});

it("exact disposition source absent from owed refs is included without invented production", async () => {
  const f = await fixture();
  try {
    const record = await f.freeze(H2);
    assert.ok(record);
    assert.ok(f.aggregates.at(-1)?.includes(H2));
    assert.equal(record.originalSourceSha, H2);
    assert.deepEqual(record.originalRoots?.map(r => r.sha), [H]);
    assert.equal((await f.coordinator.inspect("run-1")).length, 1);
  } finally { await f.close(); }
});

it("snapshot identity preserves producer context and creates a distinct immutable archive", async () => {
  const f = await fixture();
  try {
    const first = await f.freeze();
    assert.ok(first);
    assert.equal((await f.freeze())?.captureId, first.captureId);
    f.state.candidates[0]!.contexts[0]!.branch = "original-producer-branch";
    const next = await f.freeze();
    assert.ok(next);
    assert.notEqual(next.captureId, first.captureId);
    const records = await f.coordinator.inspect("run-1");
    assert.equal(records.length, 2);
    assert.deepEqual(records.find(r => r.captureId === first.captureId)?.originalRoots, first.originalRoots);
  } finally { await f.close(); }
});

it("boot uses a new exact source fingerprint when owed roots stayed unchanged", async () => {
  const f = await fixture();
  try {
    f.state.expires = "invalid";
    const old = await f.freeze();
    assert.ok(old);
    await f.capture(old);
    const before = (await f.coordinator.inspect("run-1")).find(r => r.captureId === old.captureId)!;
    await f.coordinator.pin({
      runId: "run-1", generation: 7, inventoryGuarded: true,
      sourceSha: H2, kind: "issue", branch: "task",
    });
    const restarted = f.make();
    await restarted.snapshotOwedInventory();
    const reconstructed = await restarted.materializeBootInventory();
    assert.equal(reconstructed.length, 1);
    assert.notEqual(reconstructed[0]!.captureId, old.captureId);
    assert.equal(reconstructed[0]!.originalSourceSha, H2);
    assert.equal(reconstructed[0]!.inventoryCurrentSha, H2);
    assert.notEqual(reconstructed[0]!.coverageDigest, old.coverageDigest);
    assert.deepEqual((await restarted.inspect("run-1")).find(r => r.captureId === old.captureId), before);
    assert.ok(f.aggregates.some(roots => roots.includes(H) && roots.includes(H2)));
  } finally { await f.close(); }
});

it("an original-source pin never rewrites a frozen aggregate in the same real generation", async () => {
  const f = await fixture();
  try {
    const frozen = await f.freeze();
    assert.ok(frozen);
    const before = structuredClone(frozen);
    const original = await f.coordinator.pin({
      runId: "run-1", generation: 7, inventoryGuarded: true,
      sourceSha: H2, kind: "issue", branch: "task",
    });
    assert.ok(original);
    assert.notEqual(original.captureId, frozen.captureId);
    assert.equal(original.sourceSha, H2);
    const records = await f.coordinator.inspect("run-1");
    assert.equal(records.length, 2);
    assert.deepEqual(records.find(record => record.captureId === frozen.captureId), before);
    assert.equal((await f.freeze())?.captureId, frozen.captureId);
  } finally { await f.close(); }
});

it("exact-generation clone-only source journal blocks older uploaded FINAL after restart", async () => {
  const f = await fixture();
  try {
    f.state.expires = "invalid";
    const record = await f.freeze();
    assert.ok(record);
    await f.capture(record);
    await f.coordinator.pin({
      runId: "run-1", generation: 7, inventoryGuarded: true,
      sourceSha: H2, kind: "issue", branch: "task",
    });
    f.state.expires = "2099-01-01T00:00:00Z";
    await f.make().resumePending(undefined, [record]);
    assert.deepEqual(f.finals, []);
    assert.equal(f.state.open, true);
  } finally { await f.close(); }
});

it("positively later-generation source does not invalidate an ACK-lost receipt", async () => {
  const f = await fixture();
  try {
    const record = await f.freeze();
    assert.ok(record);
    f.state.loseAck = true;
    await f.capture(record);
    await f.coordinator.pin({
      runId: "run-1", generation: 8, inventoryGuarded: true,
      sourceSha: H2, kind: "issue", branch: "task",
    });
    f.state.candidates[0]!.contexts.push({ ...f.context, generation: 8 });
    f.state.candidates.push({ sha: H2, pinRef: "refs/owed/later",
      contexts: [{ ...f.context, generation: 8 }] });
    f.state.cloneHeads = [H2];
    f.state.ownGeneration = 8;
    f.state.loseAck = false;
    await f.make().resumePending(undefined, [record]);
    assert.equal(f.finals.length, 2);
    assert.equal((await f.coordinator.inspect("run-1")).find(r => r.captureId === record.captureId)?.finalAcknowledged, true);
  } finally { await f.close(); }
});

it("retained clone blocks settled FINAL and unreadable clone evidence never means empty", async () => {
  const f = await fixture();
  try {
    f.state.candidates = [];
    const record = await f.coordinator.freezeInventory({
      context: f.context, currentSha: H, defaultBranch: "main", settledEvidence: "forge_no_output",
    });
    assert.ok(record);
    f.state.cloneReadable = false;
    await f.capture(record);
    assert.deepEqual(f.finals, []);
    f.state.cloneReadable = true;
    f.state.cloneHeads = [H2];
    await f.make().resumePending(undefined, [record]);
    assert.deepEqual(f.finals, []);
    assert.equal(f.state.open, true);
  } finally { await f.close(); }
});

it("ACK-lost archive receipt stays immutable when a new unattributed clone head appears", async () => {
  const f = await fixture();
  try {
    const record = await f.freeze();
    assert.ok(record);
    f.state.loseAck = true;
    await f.capture(record);
    const before = (await f.coordinator.inspect("run-1"))[0]!.finalRequest;
    assert.ok(before);
    f.state.cloneHeads = [H2];
    f.state.loseAck = false;
    await f.make().resumePending(undefined, [record]);
    assert.equal(f.finals.length, 1);
    const pending = (await f.coordinator.inspect("run-1"))[0]!;
    assert.deepEqual(pending.finalRequest, before);
    assert.equal(pending.finalAcknowledged, undefined);
  } finally { await f.close(); }
});

it("closed hold cannot reserve and active generation cannot send final RPC", async () => {
  const f = await fixture();
  try {
    const record = await f.freeze();
    assert.ok(record);
    f.state.open = false;
    await f.capture(record);
    assert.equal(f.reserves(), 0);
    assert.deepEqual(f.finals, []);
  } finally { await f.close(); }
});

it("malformed expiry never closes a guarded hold", async () => {
  const f = await fixture();
  try {
    const record = await f.freeze();
    assert.ok(record);
    f.state.expires = "not-a-date";
    await f.capture(record);
    assert.deepEqual(f.finals, []);
    assert.equal(f.reserves(), 1);
    assert.equal((await f.coordinator.inspect("run-1"))[0]?.state, "uploaded");
    assert.equal((await f.coordinator.inspect("run-1"))[0]?.finalRequest, undefined);
  } finally { await f.close(); }
});

it("discovery with no journal survives active API until a later spaced live pass", async () => {
  const f = await fixture();
  try {
    f.state.status = "running";
    await f.coordinator.snapshotOwedInventory();
    assert.deepEqual(await f.coordinator.materializeBootInventory(), []);
    f.state.status = "failed";
    await f.coordinator.resumeLive({ authenticatedAtMs: 1000, isExecuting: () => false });
    assert.equal((await f.coordinator.inspect("run-1")).length, 1);
    assert.equal(f.reserves(), 1);
  } finally { await f.close(); }
});


it("active API permits local reaped freeze but refuses final RPC until the generation ends", async () => {
  const f = await fixture();
  try {
    f.state.status = "running";
    assert.equal(await f.freeze(), undefined);
    const record = await f.coordinator.freezeInventory({
      context: f.context, currentSha: H, defaultBranch: "main", locallyQuiescent: true,
    });
    assert.ok(record);
    assert.equal((await f.capture(record)).state, "uploaded");
    assert.equal(f.reserves(), 1);
    assert.deepEqual(f.finals, []);
    f.state.status = "failed";
    await f.coordinator.resumePending(undefined, [record]);
    assert.equal(f.finals.length, 1);
    assert.equal((await f.coordinator.inspect("run-1"))[0]?.finalAcknowledged, true);
  } finally { await f.close(); }
});

it("earlier uploaded archive cannot release added roots; a new snapshot preserves it", async () => {
  const f = await fixture();
  try {
    f.state.expires = "invalid";
    const first = await f.freeze();
    assert.ok(first);
    assert.equal((await f.capture(first)).state, "uploaded");
    f.state.candidates.push({ sha: H2, pinRef: "refs/owed/b", contexts: [f.context] });
    f.state.expires = "2099-01-01T00:00:00Z";
    await f.coordinator.resumePending(undefined, [first]);
    assert.deepEqual(f.finals, []);
    const next = await f.freeze();
    assert.ok(next);
    assert.notEqual(first.captureId, next.captureId);
    const old = (await f.coordinator.inspect("run-1")).find(r => r.captureId === first.captureId)!;
    assert.equal(old.sourceSha, first.sourceSha);
    assert.equal(old.coverageDigest, first.coverageDigest);
    assert.equal(old.finalRequest, undefined);
  } finally { await f.close(); }
});

it("lost final ACK replays exact uploaded request after restart with closed hold; wrong ACK retains", async () => {
  const f = await fixture();
  try {
    const record = await f.freeze();
    assert.ok(record);
    f.state.loseAck = true;
    assert.equal((await f.capture(record)).state, "uploaded");
    assert.equal(f.finals.length, 1);
    const pending = (await f.coordinator.inspect("run-1"))[0]!;
    assert.ok(pending.finalRequest);
    assert.equal(pending.finalAcknowledged, undefined);
    f.state.loseAck = false;
    f.state.wrongAck = true;
    const restarted = f.make();
    const snapshot = await restarted.snapshotBootRecords();
    assert.equal(snapshot.length, 1);
    await restarted.resumePending(undefined, snapshot);
    assert.deepEqual(f.finals[1], f.finals[0]);
    assert.equal((await restarted.inspect("run-1"))[0]?.finalAcknowledged, undefined);
    f.state.wrongAck = false;
    await restarted.resumePending(undefined, snapshot);
    assert.equal(f.finals.length, 2, "immediate retry after a wrong ACK remains paced");
    assert.equal((await restarted.inspect("run-1"))[0]?.finalAcknowledged, undefined);
    f.state.now += 60_000;
    await restarted.resumePending(undefined, snapshot);
    assert.deepEqual(f.finals[2], f.finals[0]);
    assert.equal((await restarted.inspect("run-1"))[0]?.finalAcknowledged, true);
    assert.equal(f.reserves(), 1);
  } finally { await f.close(); }
});

it("settled ACK loss retries the same request after its exact hold closes", async () => {
  const f = await fixture();
  try {
    f.state.candidates = [];
    f.state.status = "completed";
    const record = await f.coordinator.freezeInventory({
      context: f.context, currentSha: H, defaultBranch: "main", settledEvidence: "publication",
    });
    assert.ok(record);
    f.state.loseAck = true;
    await f.capture(record);
    assert.equal(f.finals.length, 1);
    assert.equal(f.state.open, false);
    f.state.loseAck = false;
    const restarted = f.make();
    await restarted.resumePending();
    assert.equal(f.finals.length, 2);
    assert.deepEqual(f.finals[1], f.finals[0]);
    assert.equal((await restarted.inspect("run-1"))[0]?.finalAcknowledged, true);
    assert.equal(f.reserves(), 0);
  } finally { await f.close(); }
});

it("confirmed containment removes owed pins without changing an uploaded final archive retry", async () => {
  const f = await fixture();
  try {
    const record = await f.freeze();
    assert.ok(record);
    f.state.loseAck = true;
    await f.capture(record);
    const identity = (await f.coordinator.inspect("run-1"))[0]!.finalRequest;
    f.state.candidates = []; // Git reconcile removed only remotely confirmed roots.
    f.state.loseAck = false;
    const restarted = f.make();
    await restarted.resumePending();
    assert.equal(f.finals.length, 2);
    assert.deepEqual(f.finals[1], f.finals[0]);
    const acknowledged = (await restarted.inspect("run-1"))[0]!;
    assert.deepEqual(acknowledged.finalRequest, identity);
    assert.equal(acknowledged.finalAcknowledged, true);
    assert.deepEqual(acknowledged.originalRoots, record.originalRoots);
    assert.equal(f.reserves(), 1);
  } finally { await f.close(); }
});

it("a later active unguarded claim still proves the original guarded generation ended", async () => {
  const f = await fixture();
  try {
    f.state.ownGeneration = 8;
    f.state.ownGuarded = false;
    f.state.status = "running";
    const record = await f.freeze();
    assert.ok(record);
    await f.capture(record);
    assert.equal(f.finals.length, 1);
    assert.equal((await f.coordinator.inspect("run-1"))[0]?.finalAcknowledged, true);
  } finally { await f.close(); }
});

it("new roots after durable final request fail closed without replacing its identity", async () => {
  const f = await fixture();
  try {
    const record = await f.freeze();
    assert.ok(record);
    f.state.loseAck = true;
    await f.capture(record);
    const before = (await f.coordinator.inspect("run-1"))[0]!.finalRequest;
    f.state.candidates.push({ sha: H2, pinRef: "refs/owed/b", contexts: [f.context] });
    f.state.open = true;
    assert.equal(await f.freeze(), undefined);
    await f.coordinator.resumePending(undefined, [record]);
    const pending = (await f.coordinator.inspect("run-1"))[0]!;
    assert.deepEqual(pending.finalRequest, before);
    assert.equal(pending.reason, "inventory_quiescence_breach");
    assert.equal(f.finals.length, 1);
  } finally { await f.close(); }
});

it("feature loss blocks reserve and pending final retry without stripping coverage", async () => {
  const f = await fixture();
  try {
    const record = await f.freeze();
    assert.ok(record);
    f.state.feature = false;
    await f.capture(record);
    assert.equal(f.reserves(), 0);
    assert.deepEqual(f.finals, []);
    f.state.feature = true;
    f.state.loseAck = true;
    await f.capture(record);
    f.state.feature = false;
    await f.coordinator.resumePending(undefined, [record]);
    assert.equal(f.finals.length, 1);
    assert.equal((await f.coordinator.inspect("run-1"))[0]?.coverageDigest, record.coverageDigest);
  } finally { await f.close(); }
});

it("failed capture retains exact inventory and hold without final RPC", async () => {
  const f = await fixture();
  try {
    const record = await f.freeze();
    assert.ok(record);
    f.state.failProduce = true;
    assert.equal((await f.capture(record)).state, "needs_action");
    const retained = (await f.coordinator.inspect("run-1"))[0]!;
    assert.deepEqual(retained.originalRoots, record.originalRoots);
    assert.equal(retained.reason, "bundle_failed");
    await f.coordinator.forgetGeneration("run-1", 7);
    assert.equal((await f.coordinator.inspect("run-1")).length, 1);
    assert.equal(f.reserves(), 0);
    assert.deepEqual(f.finals, []);
  } finally { await f.close(); }
});

it("MAC tampering and authenticated malformed producer context never become empty settlement", async () => {
  const f = await fixture();
  try {
    const record = await f.freeze();
    assert.ok(record);
    const file = path.join(f.root, "journal", "run-1", record.captureId + ".json");
    const saved = await fs.readFile(file, "utf8");
    const tampered = JSON.parse(saved);
    tampered.originalRoots[0].contexts[0].branch = "tampered";
    await fs.writeFile(file, JSON.stringify(tampered));
    assert.deepEqual(await f.coordinator.inspect("run-1"), []);
    const malformed = JSON.parse(saved);
    malformed.originalRoots[0].contexts[0].kind = "invented-kind";
    const { mac: _oldMac, ...payload } = malformed;
    const key = createHmac("sha256", "local-worker-fixture").update("uzi-recovery-journal-v1").digest();
    malformed.mac = createHmac("sha256", key).update(canonicalJson(payload)).digest("hex");
    await fs.writeFile(file, JSON.stringify(malformed));
    await f.coordinator.resumePending(undefined, [record]);
    assert.deepEqual(await f.coordinator.inspect("run-1"), []);
    assert.deepEqual(f.finals, []);
  } finally { await f.close(); }
});

it("foreground, boot, live and forget serialize the whole generation without nested lock", async () => {
  const f = await fixture();
  let unblock!: () => void;
  try {
    const first = await f.freeze();
    f.state.candidates.push({ sha: H2, pinRef: "refs/owed/b", contexts: [f.context] });
    const second = await f.freeze();
    assert.ok(first);
    assert.ok(second);
    f.state.produceWait = new Promise<void>(resolve => { unblock = resolve; });
    const started = new Promise<void>(resolve => { f.state.onProduce = resolve; });
    const foreground = f.capture(first);
    await started;
    const boot = f.coordinator.resumePending(undefined, [second]);
    await f.coordinator.resumeLive({ authenticatedAtMs: 1000, isExecuting: () => false });
    const forget = f.coordinator.forgetGeneration("run-1", 7);
    assert.equal(f.state.produced, 1);
    unblock();
    await Promise.all([foreground, boot, forget]);
    assert.equal(f.state.produced, 2);
    assert.equal(f.finals.length, 1);
    assert.equal((await f.coordinator.inspect("run-1")).length, 2);
  } finally { unblock?.(); await f.close(); }
});


it("oversized capture keeps the guarded source and never reserves or finalizes", async () => {
  const f = await fixture();
  try {
    const record = await f.freeze();
    assert.ok(record);
    f.state.oversized = true;
    assert.equal((await f.capture(record)).reason, "oversized");
    assert.equal((await f.coordinator.inspect("run-1"))[0]?.coverageDigest, record.coverageDigest);
    assert.equal(f.reserves(), 0);
    assert.deepEqual(f.finals, []);
  } finally { await f.close(); }
});

it("permanent oversized inventory is retained without repeated boot or live production", async () => {
  const f = await fixture();
  try {
    f.state.oversized = true;
    const record = await f.freeze();
    assert.ok(record);
    assert.equal((await f.capture(record)).reason, "oversized");
    const restarted = f.make();
    await restarted.resumePending();
    f.state.now += 100_000;
    await restarted.resumeLive({ authenticatedAtMs: f.state.now, isExecuting: () => false });
    assert.equal(f.state.produced, 1);
    assert.equal(f.reserves(), 0);
    assert.deepEqual(f.finals, []);
    assert.equal((await restarted.inspect("run-1"))[0]?.reason, "oversized");
  } finally { await f.close(); }
});

it("real aggregate bundle imports H and exact disposition H2 with current tree and original production intact", async () => {
  const root = makeScratchRoot("inventory-git-");
  const gitEnv = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null",
    GIT_TERMINAL_PROMPT: "0" };
  const git = (cwd: string, args: string[]) =>
    execFileSync("git", ["-C", cwd, ...args], { env: gitEnv, encoding: "utf8", timeout: 10000 }).trim();
  try {
    const data = path.join(root, "data");
    const repos = path.join(data, "repos");
    const bare = path.join(repos, "repo.git");
    await fs.mkdir(repos, { recursive: true, mode: 0o700 });
    execFileSync("git", ["init", "--bare", bare], { env: gitEnv, stdio: "pipe", timeout: 10000 });
    const work = path.join(root, "work");
    await fs.mkdir(work);
    execFileSync("git", ["init", "-b", "main", work], { env: gitEnv, stdio: "pipe", timeout: 10000 });
    git(work, ["config", "user.name", "fixture"]);
    git(work, ["config", "user.email", "fixture@uzi.local"]);
    git(work, ["config", "commit.gpgsign", "false"]);
    await fs.writeFile(path.join(work, "work.txt"), "base");
    git(work, ["add", "work.txt"]);
    git(work, ["commit", "-m", "base"]);
    const base = git(work, ["rev-parse", "HEAD"]);
    await fs.writeFile(path.join(work, "work.txt"), "original H");
    git(work, ["commit", "-am", "H"]);
    const h = git(work, ["rev-parse", "HEAD"]);
    git(work, ["checkout", "--detach", base]);
    await fs.writeFile(path.join(work, "work.txt"), "current H2");
    git(work, ["commit", "-am", "H2"]);
    const h2 = git(work, ["rev-parse", "HEAD"]);
    git(work, ["push", bare, h + ":refs/heads/producer", h2 + ":refs/heads/current"]);
    const cache = new GitCache(data, nullLogger(), undefined, testGitCacheOptions());
    const producer: PositiveOwedCandidateContext = {
      runId: "run-real", generation: 1, kind: "issue", branch: "producer", barePath: bare,
      defaultIdentity: { ref: "refs/remotes/origin/main", sha: base },
    };
    assert.equal((await cache.updateTrackingRef(bare, producer.branch, h, { context: producer })).kind, "updated");
    assert.equal((await cache.committedTrackingOwnership(bare, producer.branch, producer.runId, h, 1)).kind, "owned");
    const context = { ...producer, generation: 7, branch: "current" };
    const coordinator = new RecoveryCoordinator({
      recoveryRoot: path.join(data, "recovery"), workerToken: "local-worker-fixture", log: nullLogger(),
      git: cache,
      client: {
        hasFeature: () => true,
        getRunOwnership: async () => ({ status: "failed", claim_generation: 7, inventory_guarded: true }),
        listRecoveryHolds: async () => ({ run_id: "run-real", holds: [{
          hold_id: "hold-real", generation: 7, inventory_guarded: true, has_available_capture: false,
        }] }),
      } as never,
    });
    const record = await coordinator.freezeInventory({
      context, currentSha: h2, originalSourceSha: h2, defaultBranch: "main",
    });
    assert.ok(record);
    assert.deepEqual(record.originalRoots?.map(r => r.sha), [h]);
    assert.equal(record.originalRoots?.[0]?.contexts[0]?.generation, 1);
    assert.equal(record.originalRoots?.[0]?.contexts[0]?.branch, "producer");
    const bundle = await cache.produceRecoveryBundle(bare, {
      sourceSha: record.sourceSha, outPath: path.join(root, "aggregate.bundle"), forgeTip: undefined,
    });
    assert.equal(bundle.selfContained, true);
    const imported = path.join(root, "import.git");
    execFileSync("git", ["init", "--bare", imported], { env: gitEnv, stdio: "pipe", timeout: 10000 });
    git(imported, ["bundle", "unbundle", bundle.bundlePath]);
    for (const sha of [h, h2]) {
      assert.equal(git(imported, ["cat-file", "-t", sha]), "commit");
      git(imported, ["merge-base", "--is-ancestor", sha, record.sourceSha]);
    }
    assert.equal(git(imported, ["rev-parse", record.sourceSha + "^{tree}"]),
      git(imported, ["rev-parse", h2 + "^{tree}"]));
    assert.deepEqual((await cache.enumerateOwedCandidates(bare, "run-real")).map(c => c.sha), [h]);
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});

it("restart from persisted divergent owed refs without a journal uploads an importable inventory", async () => {
  const root = makeScratchRoot("inventory-git-");
  const gitEnv = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null",
    GIT_TERMINAL_PROMPT: "0" };
  const git = (cwd: string, args: string[]) =>
    execFileSync("git", ["-C", cwd, ...args], { env: gitEnv, encoding: "utf8", timeout: 10000 }).trim();
  try {
    const data = path.join(root, "data");
    const repos = path.join(data, "repos");
    const bare = path.join(repos, "repo.git");
    await fs.mkdir(repos, { recursive: true, mode: 0o700 });
    execFileSync("git", ["init", "--bare", bare], { env: gitEnv, stdio: "pipe", timeout: 10000 });
    const work = path.join(root, "work");
    await fs.mkdir(work);
    execFileSync("git", ["init", "-b", "main", work], { env: gitEnv, stdio: "pipe", timeout: 10000 });
    git(work, ["config", "user.name", "fixture"]);
    git(work, ["config", "user.email", "fixture@uzi.local"]);
    git(work, ["config", "commit.gpgsign", "false"]);
    await fs.writeFile(path.join(work, "work.txt"), "base");
    git(work, ["add", "work.txt"]);
    git(work, ["commit", "-m", "base"]);
    const base = git(work, ["rev-parse", "HEAD"]);
    await fs.writeFile(path.join(work, "work.txt"), "original H");
    git(work, ["commit", "-am", "H"]);
    const h = git(work, ["rev-parse", "HEAD"]);
    git(work, ["checkout", "--detach", base]);
    await fs.writeFile(path.join(work, "work.txt"), "current H2");
    git(work, ["commit", "-am", "H2"]);
    const h2 = git(work, ["rev-parse", "HEAD"]);
    git(work, ["push", bare, h + ":refs/heads/producer", h2 + ":refs/heads/current"]);
    const cache = new GitCache(data, nullLogger(), undefined, testGitCacheOptions());
    const producer: PositiveOwedCandidateContext = {
      runId: "run-real", generation: 1, kind: "issue", branch: "producer", barePath: bare,
      defaultIdentity: { ref: "refs/remotes/origin/main", sha: base },
    };
    assert.equal((await cache.updateTrackingRef(bare, producer.branch, h, { context: producer })).kind, "updated");
    assert.equal((await cache.committedTrackingOwnership(bare, producer.branch, producer.runId, h, 1)).kind, "owned");
    const current: PositiveOwedCandidateContext = { ...producer, branch: "current" };
    assert.equal((await cache.updateTrackingRef(bare, current.branch, h2, { context: current })).kind, "updated");
    assert.equal((await cache.committedTrackingOwnership(bare, current.branch, current.runId, h2, 1)).kind, "owned");
    const recoveryRoot = path.join(data, "recovery");
    assert.equal(await fs.stat(recoveryRoot).then(() => true, () => false), false, "crash precedes any journal");
    // No cache or coordinator RAM crosses the restart boundary.
    const restartedCache = new GitCache(data, nullLogger(), undefined, testGitCacheOptions());
    restartedCache.fetchDefaultTip = async () => { throw new Error("restart capture must not use a PAT"); };
    let open = true, reserves = 0, uploads = 0, finals = 0;
    const serverId = "00000000-0000-4000-8000-000000001945";
    let manifest: { checksum: string; byte_size: number } | undefined;
    let uploaded = Buffer.alloc(0);
    const coordinator = new RecoveryCoordinator({
      recoveryRoot, workerToken: "local-worker-fixture", log: nullLogger(), git: restartedCache,
      client: {
        hasFeature: (name: string) => name === "recovery_inventory_v1",
        getRunOwnership: async () => ({ status: "failed", claim_generation: 1, inventory_guarded: true }),
        listRecoveryHolds: async (runId: string) => {
          assert.equal(runId, producer.runId);
          return { run_id: runId, holds: open ? [{
            hold_id: "persisted-hold", generation: 1, inventory_guarded: true, has_available_capture: false,
          }] : [] };
        },
        reserveRecoveryCapture: async (runId: string) => {
          assert.equal(runId, producer.runId);
          assert.equal(open, true);
          assert.equal(++reserves, 1, "one bounded reservation");
          return { capture_id: serverId, state: "preparing" };
        },
        uploadRecoveryBundle: async (runId: string, id: string,
          m: { checksum: string; byte_size: number }, stream: AsyncIterable<Uint8Array>) => {
          assert.equal(runId, producer.runId);
          assert.equal(id, serverId);
          assert.equal(open, true);
          const chunks: Buffer[] = [];
          for await (const chunk of stream) chunks.push(Buffer.from(chunk));
          uploaded = Buffer.concat(chunks);
          assert.equal(uploaded.length, m.byte_size);
          assert.equal(createHash("sha256").update(uploaded).digest("hex"), m.checksum);
          manifest = m;
          uploads++;
          return { capture_id: serverId, state: "available", manifest_bound: true };
        },
        getRecoveryCaptureStatus: async () => ({
          capture_id: serverId, state: manifest ? "available" : "preparing", manifest_bound: !!manifest,
          checksum: manifest?.checksum, byte_size: manifest?.byte_size, expires_at: "2099-01-01T00:00:00Z",
        }),
        releaseRecoveryCustody: async (runId: string, generation: number) => {
          assert.equal(runId, producer.runId);
          assert.equal(generation, 1);
          assert.equal(uploads, 1);
          assert.equal(open, true);
          finals++;
          open = false;
          return { run_id: runId, generation, released: true, holds_released: 1 };
        },
      } as never,
    });
    assert.deepEqual(await coordinator.snapshotBootRecords(), []);
    await coordinator.snapshotOwedInventory();
    const records = await coordinator.materializeBootInventory();
    assert.equal(records.length, 1);
    const record = records[0]!;
    assert.deepEqual(record.originalRoots?.map(r => r.sha).sort(), [h, h2].sort());
    assert.deepEqual(record.originalRoots?.find(r => r.sha === h)?.contexts, [producer]);
    assert.deepEqual(record.originalRoots?.find(r => r.sha === h2)?.contexts, [current]);
    await coordinator.resumeLive({ authenticatedAtMs: Date.now(), isExecuting: () => false });
    assert.equal(reserves, 1);
    assert.equal(uploads, 1);
    assert.equal(finals, 1);
    assert.deepEqual(await coordinator.inspect(producer.runId), []);
    await assert.rejects(fs.stat(path.join(recoveryRoot, producer.runId)), { code: "ENOENT" });
    assert.equal(git(bare, ["for-each-ref", "--format=%(refname)", "refs/uzi-coverage/", "refs/uzi-recovery-pin/", "refs/uzi-owed/"]), "");
    assert.deepEqual(await restartedCache.enumerateOwedCandidates(bare, producer.runId), []);
    const freshCache = new GitCache(data, nullLogger(), undefined, testGitCacheOptions());
    assert.equal((await freshCache.committedTrackingOwnership(bare, producer.branch, producer.runId, h, 1)).kind, "owned",
      "archive cleanup preserves receipt identity and does not retire publication proof");
    await coordinator.snapshotOwedInventory();
    assert.deepEqual(await coordinator.materializeBootInventory(), [], "retained contexts never reopen closed holds");
    assert.equal(reserves, 1);
    assert.equal(uploads, 1);
    const bundlePath = path.join(root, "download.bundle");
    await fs.writeFile(bundlePath, uploaded);
    const imported = path.join(root, "import");
    execFileSync("git", ["init", imported], { env: gitEnv, stdio: "pipe", timeout: 10000 });
    git(imported, ["bundle", "unbundle", bundlePath]);
    for (const [sha, contents] of [[h, "original H"], [h2, "current H2"]]) {
      assert.equal(git(imported, ["cat-file", "-t", sha!]), "commit");
      git(imported, ["merge-base", "--is-ancestor", sha!, record.sourceSha]);
      git(imported, ["checkout", "--detach", sha!]);
      assert.equal(await fs.readFile(path.join(imported, "work.txt"), "utf8"), contents);
    }
    assert.equal(git(imported, ["rev-parse", record.sourceSha + "^{tree}"]),
      git(imported, ["rev-parse", record.originalSourceSha + "^{tree}"]));
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});

it("a guarded journal never uses the legacy release RPC after feature loss", async () => {
  const root = makeScratchRoot("inventory-");
  const calls: unknown[] = [];
  let feature = true;
  const coordinator = new RecoveryCoordinator({
    recoveryRoot: root, workerToken: "local-worker-fixture", log: nullLogger(),
    client: {
      hasFeature: () => feature,
      releaseRecoveryCustody: async (...args: unknown[]) => {
        calls.push(args);
        return { run_id: "run-1", released: true, holds_released: 1, generation: 7 };
      },
      listRecoveryHolds: async () => ({ run_id: "run-1", holds: [
        { hold_id: "hold-7", generation: 7, inventory_guarded: true, has_available_capture: false },
      ] }),
    } as never,
    git: {} as never,
  });
  try {
    await coordinator.pin({
      runId: "run-1", generation: 7, sourceSha: "a".repeat(40),
      kind: "issue", branch: "task", inventoryGuarded: true,
    });
    feature = false;
    await coordinator.release("run-1", 7, "publication");
    assert.deepEqual(calls, []);
    assert.equal((await coordinator.inspect("run-1")).length, 1);
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});


const diagnosticRetryReasons = [
  "inventory_source_not_quiescent", "inventory_source_not_quiescent:inventory_read_not_verified",
  ...["execution_tail_present", "boundary_exception", "non_linux_host", "live_attempt_path",
    "process_not_quiescent_before_git", "process_not_quiescent_after_git", "worktree_status_dirty",
    "worktree_status_unreadable", "source_identity_changed", "physical_sources_without_boundary"]
    .map(check => "inventory_source_not_quiescent:" + check),
  ...sourceDiagnosticCauses.map(cause => "inventory_source_not_quiescent:inventory_read_not_verified:" + cause),
];
const unknownDiagnosticReasons = [
  "inventory_source_not_quiescent:invented",
  "inventory_source_not_quiescent:worktree_status_dirty:extra",
  "inventory_source_not_quiescent:inventory_read_not_verified:invented",
  "inventory_source_not_quiescent:inventory_read_not_verified:other:extra",
];

async function authenticateDiagnosticRecord(f: Awaited<ReturnType<typeof fixture>>, record: RecoveryRecord, state: string, reason: string) {
  const file = path.join(f.root, "journal", "run-1", record.captureId + ".json");
  const saved = JSON.parse(await fs.readFile(file, "utf8"));
  const { mac: _oldMac, ...payload } = { ...saved, state, reason };
  const key = createHmac("sha256", "local-worker-fixture").update("uzi-recovery-journal-v1").digest();
  const mac = createHmac("sha256", key).update(canonicalJson(payload)).digest("hex");
  await fs.writeFile(file, JSON.stringify({ ...payload, mac }));
}

for (const reason of [...diagnosticRetryReasons, ...unknownDiagnosticReasons]) {
  it(`#2507 needs_action exact retry allowlist ${reason}`, async () => {
    const f = await fixture(true);
    try {
      const record = await f.freeze();
      assert.ok(record);
      f.state.finalError = new Error("ACK unavailable");
      await f.capture(record);
      const before = (await f.coordinator.inspect("run-1"))[0]!;
      await authenticateDiagnosticRecord(f, before, "needs_action", reason);
      delete (f.git as { discoverOwedCandidates?: unknown }).discoverOwedCandidates;
      f.state.finalError = undefined;
      f.state.sourceRefused = true;
      f.state.diagnostic = { check: "worktree_status_dirty" };
      const restarted = f.make(true);
      await restarted.resumeLive({ isExecuting: () => false, authenticatedAtMs: f.state.now });
      const after = (await restarted.inspect("run-1"))[0]!;
      const eligible = diagnosticRetryReasons.includes(reason);
      assert.equal(after.reason, eligible ? "inventory_source_not_quiescent:worktree_status_dirty" : reason);
      assert.equal(after.finalAcknowledged, undefined);
      assert.deepEqual(after.finalRequest, before.finalRequest);
      assert.equal(f.logs.length, eligible ? 1 : 0);
      assert.equal(f.state.open, true);
      assert.equal(f.state.uploads, 1);
    } finally { await f.close(); }
  });
}

for (const reason of unknownDiagnosticReasons) {
  it(`#2507 uploaded unknown suffix still eligible without transient backoff ${reason}`, async () => {
    const f = await fixture();
    try {
      const record = await f.freeze();
      assert.ok(record);
      f.state.expires = "invalid";
      await f.capture(record);
      const before = (await f.coordinator.inspect("run-1"))[0]!;
      await authenticateDiagnosticRecord(f, before, "uploaded", reason);
      delete (f.git as { discoverOwedCandidates?: unknown }).discoverOwedCandidates;
      const restarted = f.make();
      for (let i = 0; i < 2; i++) {
        await restarted.resumeLive({ isExecuting: () => false, authenticatedAtMs: f.state.now });
        f.state.now += 30_000;
      }
      f.state.expires = "2099-01-01T00:00:00Z";
      await restarted.resumeLive({ isExecuting: () => false, authenticatedAtMs: f.state.now });
      const after = (await restarted.inspect("run-1"))[0]!;
      assert.equal(after.finalAcknowledged, true, "unknown reason does not grow transient backoff");
      assert.equal(after.reason, reason);
      assert.equal(after.captureId, before.captureId);
      assert.equal(f.state.uploads, 1);
    } finally { await f.close(); }
  });
}

for (const pendingFinal of [false, true]) for (const cause of [undefined, ...sourceDiagnosticCauses]) {
  it(`source refusal repeats backoff without replacing uploaded identity pendingFINAL=${pendingFinal} cause=${cause}`, async () => {
    const f = await fixture(true);
    try {
      const record = await f.freeze();
      assert.ok(record);
      if (pendingFinal) {
        f.state.finalError = new Error("ACK unavailable");
        await f.capture(record);
      } else {
        f.state.onProduce = () => { f.state.sourceRefused = true; };
        await f.capture(record);
      }
      f.state.sourceRefused = true;
      if (cause) f.state.diagnostic = { check: "inventory_read_not_verified", cause };
      const before = (await f.coordinator.inspect("run-1"))[0]!;
      const identity = (r: RecoveryRecord) => [r.captureId, r.serverCaptureId, r.sourceSha,
        r.coverageDigest, r.checksum, r.finalRequest];
      await f.coordinator.resumeLive({ isExecuting: () => false, authenticatedAtMs: f.state.now });
      const first = (await f.coordinator.inspect("run-1"))[0]!;
      assert.equal(first.reason, cause ? "inventory_source_not_quiescent:inventory_read_not_verified:" + cause : "inventory_source_not_quiescent");
      assert.equal(first.state, "uploaded");
      assert.deepEqual(identity(first), identity(before));
      f.state.now += 30_000;
      f.state.sourceRefused = false;
      f.state.finalError = undefined;
      await f.coordinator.resumeLive({ isExecuting: () => false, authenticatedAtMs: f.state.now });
      assert.equal((await f.coordinator.inspect("run-1"))[0]!.finalAcknowledged, undefined);
      f.state.sourceRefused = true;
      f.state.now += 30_000;
      await f.coordinator.resumeLive({ isExecuting: () => false, authenticatedAtMs: f.state.now });
      f.state.now += 60_000;
      f.state.sourceRefused = false;
      await f.coordinator.resumeLive({ isExecuting: () => false, authenticatedAtMs: f.state.now });
      assert.equal((await f.coordinator.inspect("run-1"))[0]!.finalAcknowledged, undefined);
      f.state.now += 60_000;
      await f.coordinator.resumeLive({ isExecuting: () => false, authenticatedAtMs: f.state.now });
      const after = (await f.coordinator.inspect("run-1"))[0]!;
      assert.equal(after.finalAcknowledged, true);
      assert.equal(after.captureId, before.captureId);
      assert.equal(after.serverCaptureId, before.serverCaptureId);
      if (pendingFinal) assert.deepEqual(after.finalRequest, before.finalRequest);
      assert.equal(f.reserves(), 1);
      assert.equal(f.state.uploads, 1);
      assert.equal(f.state.produced, 1);
      assert.equal(f.state.open, true);
    } finally { await f.close(); }
  });
}

it("physical clones fail closed without runner boundary even when heads are covered", async () => {
  const f = await fixture();
  try {
    f.git.readInventoryCloneHeads = async () => ({ kind: "verified", heads: [H],
      clones: [{ clonePath: "/retained/task", branch: "task", runId: "run-1" }], foreignOwners: [] }) as never;
    const record = await f.freeze();
    assert.ok(record);
    await f.capture(record);
    assert.equal(f.finals.length, 0);
    assert.equal(f.state.open, true);
    const retained = (await f.coordinator.inspect("run-1"))[0]!;
    assert.equal(retained.state, "uploaded", "available bytes remain uploaded while FINAL is refused");
    assert.equal(retained.reason, "inventory_source_not_quiescent:physical_sources_without_boundary");
    assert.equal(retained.finalAcknowledged, undefined);
  } finally { await f.close(); }
});

// issue #2213 / #1924: the guarded FINAL release is a custody release, so a latched worker must not send it.
type ClientSeam = Record<string, (...args: unknown[]) => Promise<unknown>>;
const clientOf = (f: Awaited<ReturnType<typeof fixture>>): ClientSeam =>
  (f.coordinator as unknown as { client: ClientSeam }).client;

it("issue2213 a quarantine latched before finalization holds the guarded FINAL and keeps the evidence", async () => {
  const { latchResidueQuarantine, resetResidueQuarantineForTests } = await import("../src/residue-quarantine.js");
  const f = await fixture();
  try {
    const client = clientOf(f);
    const upload = client.uploadRecoveryBundle!.bind(client);
    client.uploadRecoveryBundle = async (...args) => {
      const result = await upload(...args);
      latchResidueQuarantine({ cause: "review fixture", site: "after_upload" }, nullLogger());
      return result;
    };
    const record = await f.freeze();
    assert.ok(record);
    await f.capture(record);
    assert.equal(f.finals.length, 0, "no guarded release while quarantine is latched");
    const [kept] = await f.coordinator.inspect("run-1");
    assert.notEqual(kept?.finalAcknowledged, true);
    assert.equal(kept?.state, "uploaded");
    resetResidueQuarantineForTests();
    await f.capture(kept!);
    assert.equal(f.finals.length, 1, "the retained evidence finalizes once unlatched");
  } finally { resetResidueQuarantineForTests(); await f.close(); }
});

it("issue2213 a quarantine during the final status read prevents the guarded FINAL", async () => {
  const { latchResidueQuarantine, resetResidueQuarantineForTests } = await import("../src/residue-quarantine.js");
  const f = await fixture();
  try {
    const client = clientOf(f);
    const status = client.getRecoveryCaptureStatus!.bind(client);
    client.getRecoveryCaptureStatus = async (...args) => {
      const result = await status(...args);
      latchResidueQuarantine({ cause: "review fixture", site: "final_status_read" }, nullLogger());
      return result;
    };
    const record = await f.freeze();
    assert.ok(record);
    await f.capture(record);
    assert.equal(f.finals.length, 0, "no new guarded release while quarantine is latched");
    const [kept] = await f.coordinator.inspect("run-1");
    assert.equal(kept?.finalRequest, undefined, "no FINAL identity is journaled after the latch");
  } finally { resetResidueQuarantineForTests(); await f.close(); }
});

it("issue2213 a quarantine during the FINAL ACK keeps the journal unacknowledged and the bundle local", async () => {
  const { latchResidueQuarantine, resetResidueQuarantineForTests } = await import("../src/residue-quarantine.js");
  const f = await fixture();
  try {
    const client = clientOf(f);
    const release = client.releaseRecoveryCustody!.bind(client);
    client.releaseRecoveryCustody = async (...args) => {
      const result = await release(...args);
      latchResidueQuarantine({ cause: "review fixture", site: "final_ack" }, nullLogger());
      return result;
    };
    const released: string[] = [];
    (f.coordinator as unknown as { onAuthoritativeGenerationReleased?: (run: string, g: number) => void })
      .onAuthoritativeGenerationReleased = run => { released.push(run); };
    const record = await f.freeze();
    assert.ok(record);
    await f.capture(record);
    assert.equal(f.finals.length, 1, "the release was already in flight");
    const [kept] = await f.coordinator.inspect("run-1");
    assert.notEqual(kept?.finalAcknowledged, true, "a latched worker does not journal the ACK");
    assert.deepEqual(released, []);
    await fs.access(kept!.bundlePath!);
  } finally { resetResidueQuarantineForTests(); await f.close(); }
});

for (const uploadFails of [false, true]) {
  it(`guarded thin retained bytes retry without FINAL or reproduction uploadFails=${uploadFails}`, async () => {
    const f = await fixture();
    try {
      f.state.thin = true;
      const record = await f.freeze(); assert.ok(record);
      if (uploadFails) f.state.reserveError = new Error("network unavailable");
      await f.capture(record);
      const before = (await f.coordinator.inspect("run-1"))[0]!;
      assert.equal(before.selfContained, false);
      assert.deepEqual(before.prerequisiteShas, [H]);
      const bytes = await fs.readFile(before.bundlePath!);
      const produced = f.state.produced;
      f.state.reserveError = undefined;
      await f.make().resumePending(undefined, [before]);
      const uploaded = (await f.coordinator.inspect("run-1"))[0]!;
      assert.equal(uploaded.state, "uploaded");
      assert.equal(uploaded.captureId, before.captureId);
      assert.equal(uploaded.checksum, before.checksum);
      assert.deepEqual(uploaded.prerequisiteShas, before.prerequisiteShas);
      const statusReads = f.state.statusReads;
      await f.capture(uploaded);
      await f.make().resumePending(undefined, [uploaded]);
      assert.equal(f.state.statusReads, statusReads, "thin FINAL never polls status");
      assert.equal(f.finals.length, 0);
      assert.equal(f.state.produced, produced);
      assert.deepEqual(await fs.readFile(uploaded.bundlePath!), bytes);
      assert.equal(await f.coordinator.inventoryCleanupState("run-1", 7), "pending");
      await f.coordinator.forgetGeneration("run-1", 7);
      assert.equal((await f.coordinator.inspect("run-1")).length, 1);
      assert.equal(f.state.open, true);
    } finally { await f.close(); }
  });
}
for (const corruption of ["missing", "source", "ref", "dependencies", "oversized-header", "metadata"] as const) {
  it(`guarded queued archive FINAL refuses retained evidence ${corruption}`, async () => {
    const f = await fixture();
    try {
      f.state.loseAck = true;
      const record = await f.freeze(); assert.ok(record);
      await f.capture(record);
      const queued = (await f.coordinator.inspect("run-1"))[0]!;
      assert.ok(queued.finalRequest); assert.equal(f.finals.length, 1);
      if (corruption === "missing") await fs.rm(queued.bundlePath!);
      else if (corruption === "metadata") {
        const inner = f.coordinator as unknown as { writeExistingRecord(r: RecoveryRecord, fn: (r: RecoveryRecord) => RecoveryRecord): Promise<RecoveryRecord> };
        await inner.writeExistingRecord(queued, cur => ({ ...cur, selfContained: undefined, prerequisiteShas: undefined }));
      } else {
        const header = corruption === "source" ? `${H} refs/heads/recovered-source`
          : corruption === "ref" ? `${queued.sourceSha} refs/heads/other`
          : corruption === "dependencies" ? `-${H} prerequisite\n${queued.sourceSha} refs/heads/recovered-source`
          : "x".repeat(20 * 1024);
        await fs.writeFile(queued.bundlePath!, `# v2 git bundle\n${header}\n\nPACK`);
      }
      f.state.loseAck = false;
      await f.make().resumePending(undefined, [queued]);
      assert.equal(f.finals.length, 1);
      assert.notEqual((await f.coordinator.inspect("run-1"))[0]!.finalAcknowledged, true);
      assert.equal(await f.coordinator.inventoryCleanupState("run-1", 7), "pending");
    } finally { await f.close(); }
  });
}

it("maintenance late valid FINAL ACK cannot authorize cleanup and saved FINAL retry shares ambient budget", async () => {
  const { f, before } = await pendingReceiptFixture();
  let now = 0, released = 0;
  const budget = new MaintenanceBudget(600_000, () => now);
  const original = f.client.releaseRecoveryCustody;
  const reserves = f.reserves(), uploads = f.state.uploads;
  let settle!: () => void, entered!: () => void;
  const arrived = new Promise<void>(r => { entered = r; });
  const wait = new Promise<void>(r => { settle = r; });
  Object.assign(f.client, { releaseRecoveryCustody: async (...args: unknown[]) => {
    const passBudget = MaintenanceBudget.current();
    assert.ok(passBudget);
    assert.notEqual(passBudget, budget, "the pass inherits the caller clock without sharing cancellation");
    assert.ok(passBudget.remainingMs() <= budget.remainingMs());
    assert.ok(args[4] instanceof AbortSignal);
    entered(); await wait;
    assert.equal(passBudget.signal.aborted, true);
    assert.equal(args[4].aborted, true);
    return original(...args.slice(0, 4));
  } });
  const restarted = f.make(false, undefined, () => { released++; });
  try {
    let settled = false;
    const pending = MaintenanceBudget.run(budget, () => restarted.resumePending(undefined, [before]))
      .finally(() => { settled = true; });
    await Promise.race([arrived, pending]);
    now = 600_000; assert.throws(() => budget.check());
    await new Promise<void>(r => setImmediate(r));
    assert.equal(settled, false, "the pass joins actual RPC settlement after deadline");
    settle();
    await assert.rejects(pending, /maintenance deadline/);
    const kept = (await restarted.inspect("run-1"))[0]!;
    assert.notEqual(kept.finalAcknowledged, true);
    assert.deepEqual(kept.finalRequest, before.finalRequest);
    assert.equal(released, 0);
    assert.equal(f.state.pinDeletes, 0);
    assert.equal(f.reserves(), reserves); assert.equal(f.state.uploads, uploads);
    assert.throws(() => MaintenanceBudget.run(budget, () => f.make().resumePending(undefined, [kept])), /maintenance deadline/);
  } finally { settle(); budget.dispose(); await f.close(); }
});

it("late capture cancellation after three running exact reads activates once before production", async () => {
  const f = await v2FrozenFixture();
  let mono = 0, factories = 0, reads = 0, produced = false;
  let budget: MaintenanceBudget | undefined;
  const exact = (f.client as RecoveryArchiveClient).getExactRecoveryHold!;
  Object.assign(f.client, { getExactRecoveryHold: async () => {
    f.state.status = ++reads <= 3 ? "running" : "cancelled";
    return exact("run-1", 7, f.record.exactHoldBinding!);
  } });
  f.state.onProduce = () => {
    produced = true;
    assert.equal(MaintenanceBudget.current(), budget);
    assert.ok(budget);
    mono = 600_000;
  };
  const coordinator = f.make(true, undefined, undefined, { maintenanceBudget: () => {
    factories++; return budget = new MaintenanceBudget(600_000, () => mono);
  } });
  try {
    await assert.rejects(coordinator.operateFrozenInventory({ record: f.record, barePath: f.context.barePath, defaultBranch: "main" }), /maintenance deadline/);
    assert.equal(produced, true);
    assert.equal(factories, 1);
    assert.equal(f.finals.length, 0);
    assert.equal(f.state.pinDeletes, 0);
  } finally { budget?.dispose(); await f.close(); }
});

for (const state of ["released", "discarded"] as const) it(`late closed ${state} cancellation activates before settlement journal`, async () => {
  const f = await v2FrozenFixture();
  let reads = 0, factories = 0, writes = 0;
  let budget: MaintenanceBudget | undefined;
  const exact = (f.client as RecoveryArchiveClient).getExactRecoveryHold!;
  Object.assign(f.client, { getExactRecoveryHold: async () => {
    const result = await exact("run-1", 7, f.record.exactHoldBinding!);
    return { ...closedExactFixture(f.record, result!, state), run_status: ++reads === 1 ? "running" : "cancelled" };
  } });
  const coordinator = f.make(true, undefined, undefined, { maintenanceBudget: () => {
    factories++; return budget = new MaintenanceBudget();
  } });
  const write = coordinator["writeExistingRecord"].bind(coordinator);
  Object.assign(coordinator, { writeExistingRecord: async (...args: Parameters<typeof write>) => {
    writes++; assert.ok(budget); assert.equal(MaintenanceBudget.current(), budget);
    return write(...args);
  } });
  try {
    await coordinator.operateFrozenInventory({ record: f.record, barePath: f.context.barePath, defaultBranch: "main" });
    assert.equal(factories, 1); assert.ok(writes > 0);
    assert.equal(f.finals.length, 0); assert.deepEqual(f.order, []);
  } finally { budget?.dispose(); await f.close(); }
});

it("late cancellation during saved archive FINAL replay keeps identical bytes and rejects a late ACK", async () => {
  const { f, before } = await pendingReceiptFixture();
  let reads = 0, factories = 0, mono = 0;
  let budget: MaintenanceBudget | undefined;
  const reserves = f.reserves(), produced = f.state.produced, uploads = f.state.uploads;
  const release = f.client.releaseRecoveryCustody;
  Object.assign(f.client, {
    getRunOwnership: async () => ({
      status: ++reads === 1 ? "running" : "cancelled", claim_generation: 7, inventory_guarded: true,
    }),
    releaseRecoveryCustody: async (...args: unknown[]) => {
      assert.ok(budget);
      assert.equal(MaintenanceBudget.current(), budget);
      assert.equal(args[4], budget.signal);
      assert.deepEqual(args[3], before.finalRequest!.disposition);
      return release(...args);
    },
  });
  f.state.beforeFinalAck = async () => { mono = 600_000; };
  const coordinator = f.make(false, undefined, undefined, { maintenanceBudget: () => {
    factories++; return budget = new MaintenanceBudget(600_000, () => mono);
  } });
  try {
    await assert.rejects(coordinator.resumePending(undefined, [before]), /maintenance deadline/);
    assert.equal(factories, 1);
    assert.equal(f.finals.length, 2);
    assert.deepEqual(f.finals[1]![3], before.finalRequest!.disposition);
    const kept = (await coordinator.inspect("run-1"))[0]!;
    assert.notEqual(kept.finalAcknowledged, true);
    assert.deepEqual(kept.finalRequest, before.finalRequest);
    assert.equal(f.reserves(), reserves); assert.equal(f.state.produced, produced); assert.equal(f.state.uploads, uploads);
    assert.equal(f.state.pinDeletes, 0);
  } finally { budget?.dispose(); await f.close(); }
});

for (const state of ["released", "discarded"] as const) {
  it(`freeze refuses a newly closed ${state} exact hold before coverage or replacement FINAL`, async () => {
    const f = await v2FrozenFixture();
    let builds = 0;
    const exact = (f.client as RecoveryArchiveClient).getExactRecoveryHold!;
    Object.assign(f.client, {
      getRunOwnership: async () => ({ status: "running", claim_generation: 8, inventory_guarded: true }),
      getExactRecoveryHold: async () => closedExactFixture(f.record, (await exact("run-1", 7, f.record.exactHoldBinding!))!, state),
    });
    Object.assign(f.git, { buildRecoveryCoverage: async () => { builds++; throw new Error("must not freeze settled custody"); } });
    const before = await f.coordinator.inspect("run-1");
    try {
      const record = await f.coordinator.freezeInventory({
        context: f.context, currentSha: H, defaultBranch: "main", repoId: f.record.exactHoldBinding!.repoId,
      });
      assert.equal(record, undefined);
      assert.equal(builds, 0);
      assert.deepEqual(await f.coordinator.inspect("run-1"), before);
      assert.equal(f.finals.length, 0);
    } finally { await f.close(); }
  });
}

for (const entry of ["frozen", "capture"] as const) {
  it(`late ${entry} observation uses the flight-owned budget without starting a second window`, async () => {
    const f = await v2FrozenFixture();
    let mono = 0, reads = 0, callbacks = 0, internalFactories = 0;
    const budget = new MaintenanceBudget(600_000, () => mono);
    const exact = (f.client as RecoveryArchiveClient).getExactRecoveryHold!;
    Object.assign(f.client, { getExactRecoveryHold: async () => {
      f.state.status = ++reads <= (entry === "frozen" ? 3 : 1) ? "running" : "cancelled";
      return exact("run-1", 7, f.record.exactHoldBinding!);
    } });
    f.state.onProduce = () => {
      assert.equal(MaintenanceBudget.current(), budget);
      mono = 600_000;
    };
    const coordinator = f.make(true, undefined, undefined, { maintenanceBudget: () => {
      internalFactories++; return new MaintenanceBudget();
    } });
    const input = {
      record: f.record, barePath: f.context.barePath, defaultBranch: "main",
      onCancellationObserved: () => { callbacks++; return budget; },
    };
    try {
      await assert.rejects(entry === "frozen" ? coordinator.operateFrozenInventory(input) : coordinator.captureAndUpload(input), /maintenance deadline/);
      assert.equal(callbacks, 1); assert.equal(internalFactories, 0);
      assert.equal(budget.remainingMs(), 0);
      assert.throws(() => MaintenanceBudget.run(budget, () => coordinator.operateFrozenInventory(input)), /maintenance deadline/);
      assert.equal(callbacks, 1);
      assert.equal(f.finals.length, 0); assert.equal(f.state.uploads, 0); assert.equal(f.state.pinDeletes, 0);
    } finally { budget.dispose(); await f.close(); }
  });
}

// ---- issue3152: "recovery: re-drive exit" diagnostics -------------------------------------------------

type Issue3152Fixture = Awaited<ReturnType<typeof fixture>>;
type ExitLine = { path: string; reason: string; field?: string; run_id: string; generation: number; capture_id: string };
const ISSUE3152_MESSAGE = "recovery: re-drive exit";
const ISSUE3152_NEVER = 3_600_000;

function issue3152Lines(f: Issue3152Fixture): ExitLine[] {
  const lines: ExitLine[] = [];
  const info = f.log.info;
  f.log.info = (message, fields) => {
    if (message === ISSUE3152_MESSAGE) lines.push(structuredClone(fields) as unknown as ExitLine);
    else info.call(f.log, message, fields);
  };
  return lines;
}

async function issue3152LivePasses(f: Issue3152Fixture, c: RecoveryCoordinator, passes: number, isExecuting: () => boolean = () => false) {
  for (let i = 0; i < passes; i++) {
    f.state.now += ISSUE3152_NEVER;
    await c.resumeLive({ isExecuting, authenticatedAtMs: f.state.now });
  }
}

function issue3152Assert(lines: ExitLine[], from: number, expected: { path: string; reason: string; field?: string }, rec: RecoveryRecord) {
  const fresh = lines.slice(from);
  assert.equal(fresh.length, 1, `exactly one line, got ${JSON.stringify(fresh)}`);
  const line = fresh[0]!;
  assert.deepEqual(Object.keys(line).sort(), [...(expected.field ? ["field"] : []), "capture_id", "generation", "path", "reason", "run_id"].sort());
  assert.deepEqual(line, { ...expected, run_id: rec.runId, generation: rec.generation, capture_id: rec.captureId });
}

const issue3152Frozen = async (f: Issue3152Fixture, c: RecoveryCoordinator = f.coordinator) => {
  const rec = await c.freezeInventory({ context: f.context, currentSha: H, defaultBranch: "main" });
  assert.ok(rec);
  return rec;
};

/** Make `rec` carry a saved (unaccepted) FINAL by letting the release fail once, then restore the fake. */
async function issue3152SaveFinal(f: Issue3152Fixture, rec: RecoveryRecord): Promise<RecoveryRecord> {
  f.state.finalError = new Error("FINAL rejected before acceptance");
  await f.capture(rec);
  f.state.finalError = undefined;
  const saved = (await f.coordinator.inspect(rec.runId)).find(r => r.captureId === rec.captureId)!;
  assert.ok(saved.finalRequest);
  return saved;
}

function issue3152Backoff(c: RecoveryCoordinator, saved: RecoveryRecord) {
  (c as any).finalRetry.set(saved.captureId, { identity: (c as any).finalRetryIdentity(saved), failures: 1, nextAt: Number.MAX_SAFE_INTEGER });
}

{
  type Arrange = (f: Issue3152Fixture, c: RecoveryCoordinator, rec: RecoveryRecord, world: { executing: boolean }) => Promise<void> | void;
  const cases: Array<{ name: string; reason: string; field?: string; arrange: Arrange; pending?: boolean }> = [
    { name: "executing", reason: "executing", arrange: (_f, _c, _r, w) => { w.executing = true; } },
    { name: "not_live_candidate", reason: "not_live_candidate",
      arrange: (f, _c, r) => patchAuthenticated(f, r, { reason: "inventory_snapshot_changed", state: "needs_action" }) },
    { name: "final_retry_backoff", reason: "final_retry_backoff", arrange: async (f, c, r) => {
      issue3152Backoff(c, await issue3152SaveFinal(f, r));
    } },
    { name: "credentials_blocked", reason: "credentials_blocked", arrange: (_f, c) => { (c as any).credentialBlockedAt = Number.MAX_SAFE_INTEGER; } },
    { name: "inventory_feature", reason: "inventory_not_inactive", field: "inventory_feature", arrange: f => { f.state.feature = false; } },
    { name: "ownership_unsupported", reason: "inventory_not_inactive", field: "ownership_unsupported",
      arrange: f => { delete (f.client as { getRunOwnership?: unknown }).getRunOwnership; } },
    { name: "ownership_lookup_failed", reason: "inventory_not_inactive", field: "ownership_lookup_failed",
      arrange: f => { f.state.ownershipError = new Error("ownership unavailable"); } },
    { name: "ownership_lost_no_hold", reason: "inventory_not_inactive", field: "ownership_lost_no_hold", arrange: f => {
      const body = JSON.stringify({ error: "run not found for this worker" });
      f.state.ownershipError = new RequestError("GET", "/api/worker/runs/run-1/ownership", 404, body, undefined,
        { complete: true, bytes: Buffer.byteLength(body) });
      f.state.open = false;
    } },
    { name: "claim_generation", reason: "inventory_not_inactive", field: "claim_generation", arrange: f => { f.state.ownGeneration = 6; } },
    { name: "inventory_guarded", reason: "inventory_not_inactive", field: "inventory_guarded", arrange: f => { f.state.ownGuarded = false; } },
    { name: "status", reason: "inventory_not_inactive", field: "status", arrange: f => { f.state.status = "running"; } },
    { name: "bare_unresolved", reason: "bare_unresolved", arrange: f => { f.git.resolveRecoveryBareDir = async () => undefined as never; } },
  ];
  for (const c of cases) {
    it(`issue3152 live re-drive exit ${c.name} is logged once per change with a fixed field set`, async () => {
      const f = await fixture();
      try {
        const lines = issue3152Lines(f);
        const rec = await issue3152Frozen(f);
        const world = { executing: false };
        const live = (n: number) => issue3152LivePasses(f, f.coordinator, n, () => world.executing);
        // A first, different reason so this case is also a reason change.
        const first = c.name === "executing" ? "credentials_blocked" : "executing";
        if (first === "executing") world.executing = true; else (f.coordinator as any).credentialBlockedAt = Number.MAX_SAFE_INTEGER;
        await live(3);
        issue3152Assert(lines, 0, { path: "live", reason: first }, rec);
        world.executing = false; (f.coordinator as any).credentialBlockedAt = undefined;
        const before = lines.length;
        await c.arrange(f, f.coordinator, rec, world);
        const finalsBefore = f.finals.length;
        await live(3);
        issue3152Assert(lines, before, { path: "live", reason: c.reason, ...(c.field ? { field: c.field } : {}) }, rec);
        assert.equal(f.finals.length, finalsBefore, "an exit never reaches FINAL");
      } finally { await f.close(); }
    });
  }
}

it("issue3152 pending_generation_report is logged once for an ACKed guarded record", async () => {
  const f = await fixture();
  try {
    const lines = issue3152Lines(f);
    const c = f.make(false, () => true);
    const rec = await issue3152Frozen(f, c);
    await c.captureAndUpload({ record: rec, barePath: f.context.barePath, defaultBranch: "main" });
    const acked = (await c.inspect(rec.runId))[0];
    assert.equal(acked?.finalAcknowledged, true, "the ACKed record is retained while its report is pending");
    await issue3152LivePasses(f, c, 3);
    issue3152Assert(lines, 0, { path: "live", reason: "pending_generation_report" }, rec);
  } finally { await f.close(); }
});

it("issue3152 credentials_blocked is logged once for a guarded record skipped after a credential failure in the pass", async () => {
  const f = await fixture();
  try {
    f.state.candidates = [];
    f.state.status = "completed";
    const lines = issue3152Lines(f);
    const first = await issue3152Frozen(f);
    const second = await f.coordinator.freezeInventory({
      context: { ...f.context, runId: "run-2" }, currentSha: H, defaultBranch: "main",
    });
    assert.ok(second);
    f.state.finalError = new RequestError("POST", "/api/worker/runs/run-1/recovery/release", 401, "unauthorized");
    await issue3152LivePasses(f, f.coordinator, 1);
    const skipped = lines.filter(l => l.reason === "credentials_blocked");
    assert.equal(skipped.length, 1, JSON.stringify(lines));
    assert.equal(skipped[0]!.path, "live");
    assert.ok([first.captureId, second.captureId].includes(skipped[0]!.capture_id));
    assert.equal(f.finals.length, 1, "the second record never reached FINAL");
  } finally { await f.close(); }
});

it("issue3152 live_pass_cap is logged once per dropped record across passes", async () => {
  const f = await fixture();
  try {
    f.state.candidates = [];
    f.state.status = "completed";
    const lines = issue3152Lines(f);
    const a = await issue3152Frozen(f);
    const b = await f.coordinator.freezeInventory({ context: { ...f.context, runId: "run-2" }, currentSha: H, defaultBranch: "main" });
    assert.ok(b);
    (f.coordinator as any).liveMaxPerPass = 1;
    (f.coordinator as any).liveStep = async () => "ok";
    await issue3152LivePasses(f, f.coordinator, 4);
    const capped = lines.filter(l => l.reason === "live_pass_cap");
    assert.equal(capped.length, 1, JSON.stringify(lines));
    assert.ok([a.captureId, b.captureId].includes(capped[0]!.capture_id));
    assert.equal(capped[0]!.path, "live");
  } finally { await f.close(); }
});

it("issue3152 cycle_busy is logged once while another capture holds the record cycle", async () => {
  const f = await fixture();
  try {
    const lines = issue3152Lines(f);
    const rec = await issue3152Frozen(f);
    let release!: () => void;
    f.state.produceWait = new Promise<void>(resolve => { release = resolve; });
    const entered = new Promise<void>(resolve => { f.state.onProduce = resolve; });
    const held = f.capture(rec);
    await entered;
    f.git.discoverOwedCandidates = undefined as never;
    await issue3152LivePasses(f, f.coordinator, 3);
    issue3152Assert(lines, 0, { path: "live", reason: "cycle_busy" }, rec);
    release();
    await held;
  } finally { await f.close(); }
});

it("issue3152 boot path logs record_unavailable and final_retry_backoff with the boot label", async () => {
  const f = await fixture();
  try {
    const lines = issue3152Lines(f);
    const rec = await issue3152Frozen(f);
    // Snapshot still ready, current record in retry backoff: the pendingStep re-check.
    const saved = await issue3152SaveFinal(f, rec);
    const c = f.make();
    // The snapshot check clears a retry whose identity differs, so the backoff lands between the
    // snapshot check and pendingStep's re-read of the record.
    const read = (c as any).readRecord.bind(c);
    (c as any).readRecord = async (...args: unknown[]) => { issue3152Backoff(c, saved); return read(...args); };
    const stale = { ...saved, finalRequest: undefined } as RecoveryRecord;
    for (let i = 0; i < 3; i++) await c.resumePending(undefined, [stale]);
    issue3152Assert(lines, 0, { path: "boot", reason: "final_retry_backoff" }, rec);
    // Boot snapshot check: the snapshot itself is in backoff.
    const before = lines.length;
    const c4 = f.make();
    const read4 = (c4 as any).readRecord.bind(c4);
    (c4 as any).readRecord = async (...args: unknown[]) => { issue3152Backoff(c4, saved); return read4(...args); };
    await (c4 as any).pendingStep(stale, undefined, "terminal_source");
    assert.equal(lines.length, before + 1);
    assert.equal(lines[before]!.path, "terminal_source");
    const c2 = f.make();
    issue3152Backoff(c2, saved);
    const mark = lines.length;
    for (let i = 0; i < 3; i++) await c2.resumePending(undefined, [saved]);
    issue3152Assert(lines, mark, { path: "boot", reason: "final_retry_backoff" }, rec);
    // The record vanishes between the snapshot and the re-read.
    await fs.rm(path.join(f.root, "journal", rec.runId, rec.captureId + ".json"));
    const c3 = f.make();
    const gone = lines.length;
    for (let i = 0; i < 3; i++) await c3.resumePending(undefined, [stale]);
    issue3152Assert(lines, gone, { path: "boot", reason: "record_unavailable" }, rec);
  } finally { await f.close(); }
});

{
  const early: Array<{ name: string; reason: string; field?: string;
    arrange: (f: Issue3152Fixture, c: RecoveryCoordinator, rec: RecoveryRecord) => Promise<RecoveryRecord> | RecoveryRecord }> = [
    { name: "executing", reason: "executing", arrange: (_f, c, r) => { (c as any).isExecuting = () => true; return r; } },
    { name: "not_live_candidate", reason: "not_live_candidate",
      arrange: (_f, _c, r) => ({ ...r, state: "needs_action", reason: "inventory_snapshot_changed" } as RecoveryRecord) },
    // Only a record that is no longer guarded can be a live candidate without a coverage digest; the
    // guard is the caller's, so this code is a defensive exit reached by calling resumeInventory directly.
    { name: "no_coverage_digest", reason: "no_coverage_digest", arrange: (_f, _c, r) => ({ ...r, inventoryGuarded: false, state: "bundled",
      bundlePath: "bundle", byteSize: 1, checksum: "c", chunkCount: 1, coverageDigest: undefined } as RecoveryRecord) },
    { name: "final_retry_backoff", reason: "final_retry_backoff", arrange: async (f, c, r) => {
      const saved = await issue3152SaveFinal(f, r); issue3152Backoff(c, saved); return saved;
    } },
    { name: "generation", reason: "inventory_not_inactive", field: "generation", arrange: (_f, _c, r) => ({ ...r, generation: 0 }) },
    { name: "bare_dir_missing", reason: "bare_dir_missing", arrange: (_f, _c, r) => ({ ...r, bareDir: undefined }) },
  ];
  for (const e of early) {
    // These exits are unreachable through livePass (its filter takes them first) and through the boot
    // sweep (pendingStep re-checks first); resumeInventory is the smallest seam that reaches them.
    it(`issue3152 resumeInventory early exit ${e.name} is logged once across repeated calls`, async () => {
      const f = await fixture();
      try {
        const lines = issue3152Lines(f);
        const rec = await issue3152Frozen(f);
        const record = await e.arrange(f, f.coordinator, rec);
        const finalsBefore = f.finals.length;
        const from = lines.length;
        for (let i = 0; i < 3; i++) await (f.coordinator as any).resumeInventory(record, undefined, undefined, "live");
        issue3152Assert(lines, from, { path: "live", reason: e.reason, ...(e.field ? { field: e.field } : {}) }, record);
        assert.equal(f.finals.length, finalsBefore);
      } finally { await f.close(); }
    });
  }
}

it("issue3152 inventory_not_inactive reports field executing when the run starts executing after the early check", async () => {
  const f = await fixture();
  try {
    const lines = issue3152Lines(f);
    const rec = await issue3152Frozen(f);
    let calls = 0;
    (f.coordinator as any).isExecuting = () => ++calls > 1;
    for (let i = 0; i < 3; i++) { calls = 0; await (f.coordinator as any).resumeInventory(rec, undefined, undefined, "live"); }
    issue3152Assert(lines, 0, { path: "live", reason: "inventory_not_inactive", field: "executing" }, rec);
  } finally { await f.close(); }
});

it("issue3152 a pass that reaches frozenOperationCycle logs no re-drive exit and a later exit is a new transition", async () => {
  const f = await fixture();
  try {
    const lines = issue3152Lines(f);
    const rec = await issue3152Frozen(f);
    const world = { executing: true };
    await issue3152LivePasses(f, f.coordinator, 2, () => world.executing);
    assert.equal(lines.length, 1);
    world.executing = false;
    let cycles = 0;
    const real = (f.coordinator as any).frozenOperationCycle.bind(f.coordinator);
    (f.coordinator as any).frozenOperationCycle = async (...args: unknown[]) => { cycles++; return real(...args); };
    await issue3152LivePasses(f, f.coordinator, 1, () => world.executing);
    assert.equal(cycles, 1, "the pass reached frozenOperationCycle");
    assert.equal(lines.length, 1, "no exit line on the progressing pass");
    assert.equal((f.coordinator as any).redriveExitLogged.size, 0, "progress clears the stored reason");
    void rec;
  } finally { await f.close(); }
});

it("issue3152 the dedup map is bounded, evicts the oldest and prunes removed captures", async () => {
  const f = await fixture();
  try {
    const lines = issue3152Lines(f);
    const rec = await issue3152Frozen(f);
    const c = f.coordinator as any;
    const note = (n: number) => c.noteRedriveExit("live", { ...rec, captureId: `cap-${n}` }, "executing");
    for (let n = 0; n < 300; n++) note(n);
    assert.equal(c.redriveExitLogged.size, 256);
    assert.equal(lines.length, 300);
    note(299);
    assert.equal(lines.length, 300, "a retained key does not re-emit");
    note(0);
    assert.equal(lines.length, 301, "an evicted key re-emits");
    assert.equal(c.redriveExitLogged.size, 256);
    // A live pass drops entries whose capture left the journal; the real record's entry survives.
    c.noteRedriveExit("live", rec, "executing");
    await issue3152LivePasses(f, f.coordinator, 1, () => true);
    assert.deepEqual([...c.redriveExitLogged.values()].map((e: { captureId: string }) => e.captureId), [rec.captureId]);
  } finally { await f.close(); }
});

it("issue3152 two guarded records sharing a run and generation each log at most once", async () => {
  const f = await fixture();
  try {
    const lines = issue3152Lines(f);
    const pinned = await issue3152Frozen(f);
    const sibling = { ...pinned, captureId: "sibling-capture" } as RecoveryRecord;
    const file = path.join(f.root, "journal", pinned.runId, sibling.captureId + ".json");
    const key = createHmac("sha256", "local-worker-fixture").update("uzi-recovery-journal-v1").digest();
    const { mac: _m, ...payload } = JSON.parse(await fs.readFile(path.join(f.root, "journal", pinned.runId, pinned.captureId + ".json"), "utf8"));
    const body = { ...payload, captureId: sibling.captureId };
    await fs.writeFile(file, JSON.stringify({ ...body, mac: createHmac("sha256", key).update(canonicalJson(body)).digest("hex") }));
    (f.coordinator as any).isExecuting = () => true;
    await issue3152LivePasses(f, f.coordinator, 4, () => true);
    const byCapture = new Map<string, number>();
    for (const l of lines) byCapture.set(l.capture_id, (byCapture.get(l.capture_id) ?? 0) + 1);
    assert.deepEqual([...byCapture.keys()].sort(), [pinned.captureId, sibling.captureId].sort());
    for (const [capture, count] of byCapture) assert.equal(count, 1, `capture ${capture} logged ${count} times`);
  } finally { await f.close(); }
});

it("issue3152 boot sweep reports a resumeInventory outcome through the restart sweep line", async () => {
  const f = await fixture();
  try {
    const rec = await issue3152Frozen(f);
    const sweeps: Array<Record<string, unknown>> = [];
    const info = f.log.info;
    f.log.info = (message, fields) => {
      if (message === "recovery restart sweep") sweeps.push(structuredClone(fields) as Record<string, unknown>);
      else info.call(f.log, message, fields);
    };
    const c = f.make();
    (c as any).resumeInventory = async () => ({ state: "needs_action", reason: "upload_transient" });
    await c.resumePending(undefined, [rec]);
    assert.deepEqual(sweeps, [{ run_id: rec.runId, generation: rec.generation, outcome: "needs_action", reason: "upload_transient" }]);
  } finally { await f.close(); }
});

it("issue3152 the in-lock liveStep re-check logs its exit once on the live path", async () => {
  const f = await fixture();
  try {
    const lines = issue3152Lines(f);
    const rec = await issue3152Frozen(f);
    // Not executing when the pass filters, executing by the time liveStep re-checks under the lock.
    const c = f.coordinator as any;
    const realStep = c.liveStep.bind(c);
    let inLock = false;
    c.liveStep = async (...args: unknown[]) => { inLock = true; try { return await realStep(...args); } finally { inLock = false; } };
    await issue3152LivePasses(f, c, 3, () => inLock);
    issue3152Assert(lines, 0, { path: "live", reason: "executing" }, rec);
  } finally { await f.close(); }
});

it("issue3152 a saved FINAL replay clears the logged exit so the same exit logs again", async () => {
  const f = await fixture();
  try {
    const lines = issue3152Lines(f);
    const rec = await issue3152Frozen(f);
    const saved = await issue3152SaveFinal(f, rec);
    const c = f.coordinator as any;
    c.isExecuting = () => true;
    for (let i = 0; i < 3; i++) await c.resumeInventory(saved, undefined, undefined, "live");
    assert.equal(lines.length, 1);
    // A pass that replays the saved FINAL (and fails again, keeping the saved request).
    c.isExecuting = () => false;
    f.state.finalError = new Error("FINAL rejected again");
    const finalsBefore = f.finals.length;
    await c.resumeInventory(saved, undefined, undefined, "live");
    assert.ok(f.finals.length > finalsBefore, "the pass replayed the saved FINAL");
    assert.equal(lines.length, 1, "the replay itself logs no exit");
    f.state.finalError = undefined;
    c.finalRetry.clear();
    c.isExecuting = () => true;
    for (let i = 0; i < 3; i++) await c.resumeInventory(saved, undefined, undefined, "live");
    assert.equal(lines.length, 2, "the same exit after a FINAL replay is a new transition");
    assert.deepEqual(lines[1], lines[0]);
  } finally { await f.close(); }
});

it("issue3152 live_pass_cap is not logged for dropped records that are not inventory-guarded", async () => {
  const f = await fixture();
  try {
    f.state.candidates = [];
    f.state.status = "completed";
    const lines = issue3152Lines(f);
    const a = await issue3152Frozen(f);
    const b = await f.coordinator.freezeInventory({ context: { ...f.context, runId: "run-2" }, currentSha: H, defaultBranch: "main" });
    assert.ok(b);
    // Legacy (unguarded) bundled records, served from memory: a journaled unguarded record cannot keep the guarded-only fields.
    const legacy = [a, b].map(r => ({ ...r, inventoryGuarded: false, coverageDigest: undefined, state: "bundled",
      bundlePath: "bundle", byteSize: 1, checksum: "c", chunkCount: 1 }) as RecoveryRecord);
    // Unguarded records that FAIL the live filter: one whose run is executing, one that is already uploaded.
    legacy.push({ ...legacy[0]!, runId: "run-exec", captureId: "cap-exec" } as RecoveryRecord);
    legacy.push({ ...legacy[0]!, captureId: "cap-uploaded", state: "uploaded" } as RecoveryRecord);
    const c = f.coordinator as any;
    c.liveMaxPerPass = 1;
    c.snapshotBootRecords = async () => legacy;
    let steps = 0;
    c.liveStep = async () => { steps++; return "ok"; };
    await issue3152LivePasses(f, c, 3, (runId?: string) => runId === "run-exec");
    assert.equal(steps, 3, "the cap admitted exactly one record per pass, so the other was dropped");
    assert.deepEqual(lines, []);
  } finally { await f.close(); }
});

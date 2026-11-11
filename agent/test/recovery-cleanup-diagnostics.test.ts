import assert from "node:assert/strict";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { afterEach, test } from "node:test";
import type { RecoveryExactHold } from "../src/protocol.js";
import { createHash } from "node:crypto";
import { canonicalJson, type CompletedRetirement, type RecoveryRecord } from "../src/recovery.js";
import { binding, closed, patch, inventory, environment, fixture } from "./fixtures/recovery-bindingless-compat.js";

const roots: string[] = [];
afterEach(async () => { for (const root of roots.splice(0)) await fs.rm(root, { recursive: true, force: true }); });
async function world(variant: Parameters<typeof fixture>[1] = "acknowledged") {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), "cleanup-diagnostics-"));
  roots.push(root);
  const f = await fixture(root, variant);
  return { f, ...environment(f) };
}

test("cleanup evaluator attributes each clause without changing state or proof order; snapshot memo retains rejection", async () => {
  const { f, coordinator: c } = await world();
  const ack = f.record;
  const sibling: RecoveryRecord = { ...ack, captureId: "sibling", finalRequest: undefined, finalAcknowledged: false,
    coverageDigest: undefined, sourceSha: "1".repeat(40) };
  const git = c["git"];
  const originalResolve = git.resolveRecoveryBareDir;
  const calls: string[] = [];
  git.resolveRecoveryBareDir = async () => { calls.push("bare"); return f.context.barePath; };
  git.ancestry = async () => { calls.push("proof"); return "ancestor"; };
  const check = async (records: RecoveryRecord[], reason: string) => {
    assert.deepEqual(await c["cleanupDetailOf"](records, 7), { state: "pending", cleanupPendingReason: reason });
    assert.equal(await c["cleanupStateOf"](records, 7), "pending");
  };
  await check([{ ...ack, finalAcknowledged: false }], "missing_ack");
  await check([{ ...ack, reason: "inventory_quiescence_breach" }], "ack_quiescence_breach");
  await check([{ ...ack, reason: "inventory_snapshot_changed" }], "ack_snapshot_changed");
  await check([ack, { ...sibling, finalRequest: ack.finalRequest }], "unacknowledged_sibling_final");
  await check([ack, { ...sibling, coverageDigest: ack.coverageDigest, originalSourceSha: undefined }], "missing_frozen_source_heads");
  git.resolveRecoveryBareDir = async () => undefined;
  await check([ack, sibling], "source_coverage_bare_unavailable");
  git.resolveRecoveryBareDir = async () => "wrong";
  await check([ack, sibling], "source_coverage_bare_mismatch");
  git.resolveRecoveryBareDir = async () => { calls.push("bare"); return f.context.barePath; };
  git.ancestry = undefined;
  await check([ack, sibling], "source_coverage_proof_unavailable");
  git.ancestry = async () => "unknown";
  await check([ack, sibling], "source_coverage_unverified");
  calls.length = 0;
  git.ancestry = async () => { calls.push("proof"); return "ancestor"; };
  const records = [ack, sibling];
  assert.equal(await c["cleanupStateOf"](records, 7), "acknowledged");
  assert.equal(await c["cleanupStateOf"](records, 7), "acknowledged");
  assert.deepEqual(calls, ["bare", "proof"]);
  const failure = new Error("cancelled proof");
  git.ancestry = async () => { calls.push("rejection"); throw failure; };
  const rejected = [ack, sibling];
  await assert.rejects(c["cleanupStateOf"](rejected, 7), e => e === failure);
  await assert.rejects(c["cleanupStateOf"](rejected, 7), e => e === failure);
  assert.equal(calls.filter(x => x === "rejection").length, 1);
  git.resolveRecoveryBareDir = originalResolve;
});

test("operator structural pending diagnostics have no eligible digest", async () => {
  const { f, coordinator: c } = await world();
  const sig = { requestSignal: new AbortController().signal, shutdownSignal: new AbortController().signal, deadlineAt: Date.now() + 60000 };
  const base = { ...f.record, finalAcknowledged: false };
  // These branches precede external operator evidence and cannot select Case A replay or Case B.
  const cases: Array<[string, RecoveryRecord[]]> = [
    ["multiple_saved_finals", [base, { ...base, captureId: "second" }]],
    ["unpaired_final_evidence", [{ ...base, coverageDigest: undefined }]],
    ["missing_frozen_anchor", [{ ...base, finalRequest: undefined, coverageDigest: undefined }]],
    ["multiple_frozen_anchors", [{ ...base, finalRequest: undefined }, { ...base, captureId: "second", finalRequest: undefined }]],
  ];
  for (const [reason, records] of cases) {
    c["operatorJournal"] = async () => ({ kind: "ok", records, names: [], unbound: [] });
    c["operatorObserve"] = async () => ({ settlementNames: [], terminal: false, holds: new Set() });
    const result = await c.describeLocalRecords(f.context.runId, sig);
    assert.equal(result.kind, "described");
    if (result.kind !== "described") assert.fail();
    assert.equal(result.generations[0]?.classification, "cleanup_pending");
    assert.equal(result.generations[0]?.cleanupPendingReason, reason);
    assert.equal(result.generations[0]?.digest, undefined);
  }
});

test("resumeInventory routes five typed completion outcomes, deduplicates tuples, and contains logger failure", async () => {
  const { f, coordinator: c } = await world();
  const record = { ...f.record, completionReceipt: {} as NonNullable<RecoveryRecord["completionReceipt"]> };
  const lines: Array<Record<string, unknown>> = [];
  c["log"].info = (_message, fields) => { lines.push(fields!); };
  const outcomes: CompletedRetirement[] = [
    { kind: "pending", why: "executing" }, { kind: "invalid", why: "no_authority_record" },
    { kind: "uncertain", why: "verify_error" }, { kind: "unsupported", why: "no_boundary" }, { kind: "retired" },
  ];
  for (const outcome of outcomes) {
    c["forgetCompletedGeneration"] = async () => outcome;
    assert.equal(await c["resumeInventory"](record), undefined);
    assert.equal(await c["resumeInventory"](record), undefined);
    const line = lines.at(-1)!;
    assert.equal(line.reason, "completion_retirement");
    assert.equal(line.outcome, outcome.kind);
    assert.equal(line.why, "why" in outcome ? outcome.why : undefined);
  }
  assert.equal(lines.length, 5);
  c["log"].info = () => { throw new Error("logger unavailable"); };
  c["forgetCompletedGeneration"] = async () => ({ kind: "pending", why: "executing" });
  assert.equal(await c["resumeInventory"](record), undefined);
});

test("Case A reports the reason observed under lock, contains a failing sink, and bounds retirement tuples", async () => {
  const { f, coordinator: c } = await world();
  const records: RecoveryRecord[] = [{ ...f.record, finalAcknowledged: false }];
  c["checkedRecords"] = async () => records;
  c["guardedGeneration"] = async () => true;
  const lines: Array<Record<string, unknown>> = [];
  c["log"].info = (_message, fields) => { lines.push(fields!); };
  assert.equal(await c["resumeInventory"](f.record), undefined);
  assert.equal(lines[0]?.outcome, "cleanup_pending");
  assert.equal(lines[0]?.cleanupPendingReason, "missing_ack");
  assert.equal(await c["forgetGenerationWithinCycle"](f.context.runId, 7, undefined, undefined, undefined,
    () => { throw new Error("sink failed"); }), "cleanup_pending");
  records[0] = { ...f.record, reason: "inventory_snapshot_changed" };
  // A fresh array models the next authenticated snapshot rather than mutating the memo's snapshot.
  c["checkedRecords"] = async () => [...records];
  await c["resumeInventory"](f.record);
  await c["resumeInventory"](f.record);
  assert.equal(lines.length, 2);
  assert.equal(lines[1]?.cleanupPendingReason, "ack_snapshot_changed");
  for (let i = 0; i < 257; i++) await c["resumeInventory"]({ ...f.record, captureId: `diagnostic-${i}` });
  assert.equal(c["redriveExitLogged"].size, 256);
  const before = lines.length;
  await c["resumeInventory"](f.record);
  assert.equal(lines.length, before + 1);
});

const signals = () => ({ requestSignal: new AbortController().signal,
  shutdownSignal: new AbortController().signal, deadlineAt: Date.now() + 60000 });

test("real describe assessment emits every qualifying ACK rejection in evaluator order", async () => {
  const { f, coordinator: c, calls } = await world();
  const ack = f.record;
  const sibling: RecoveryRecord = { ...ack, captureId: "sibling", finalRequest: undefined,
    finalAcknowledged: false, coverageDigest: undefined, sourceSha: "1".repeat(40) };
  const git = c["git"];
  const cases: Array<[string, RecoveryRecord[], () => void]> = [
    ["ack_quiescence_breach", [{ ...ack, reason: "inventory_quiescence_breach" }, { ...sibling, finalRequest: ack.finalRequest }], () => {}],
    ["ack_snapshot_changed", [{ ...ack, reason: "inventory_snapshot_changed" }, sibling], () => {}],
    ["unacknowledged_sibling_final", [ack, { ...sibling, finalRequest: ack.finalRequest, coverageDigest: ack.coverageDigest, originalSourceSha: undefined }], () => {}],
    ["missing_frozen_source_heads", [ack, { ...sibling, coverageDigest: ack.coverageDigest, originalSourceSha: undefined }], () => {}],
    ["source_coverage_bare_unavailable", [ack, sibling], () => { git.resolveRecoveryBareDir = async () => undefined; }],
    ["source_coverage_bare_mismatch", [ack, sibling], () => { git.resolveRecoveryBareDir = async () => "wrong"; }],
    ["source_coverage_proof_unavailable", [ack, sibling], () => { git.ancestry = undefined; }],
    ["source_coverage_unverified", [ack, sibling], () => { git.ancestry = async () => "unknown"; }],
  ];
  for (const [reason, records, setup] of cases) {
    git.resolveRecoveryBareDir = async () => f.context.barePath;
    git.ancestry = async () => "ancestor";
    setup();
    c["operatorJournal"] = async () => ({ kind: "ok", records, names: [], unbound: [] });
    c["operatorObserve"] = async () => ({ settlementNames: [], terminal: false, holds: new Set() });
    const result = await c.describeLocalRecords(f.context.runId, signals());
    assert.equal(result.kind, "described");
    if (result.kind !== "described") assert.fail();
    assert.equal(result.generations[0]?.classification, "cleanup_pending", reason);
    assert.equal(result.generations[0]?.cleanupPendingReason, reason);
    assert.equal(result.generations[0]?.digest, undefined);
  }
  assert.deepEqual(calls, { produce: 0, reserve: 0, upload: 0, final: 0, held: 0, cleanup: 0 });
});

test("MAC-written ACK rejection is described through the real journal reader", async () => {
  const { f, coordinator: c } = await world();
  await patch(c, f.record, { reason: "inventory_snapshot_changed" });
  const result = await c.describeLocalRecords(f.context.runId, signals());
  assert.equal(result.kind, "described");
  if (result.kind !== "described") assert.fail();
  assert.equal(result.generations[0]?.classification, "cleanup_pending");
  assert.equal(result.generations[0]?.cleanupPendingReason, "ack_snapshot_changed");
  assert.equal(result.generations[0]?.digest, undefined);
});

test("memo belongs to the array snapshot, not generation or equivalent record identity", async () => {
  const { f, coordinator: c } = await world();
  const first = [{ ...f.record, reason: "inventory_snapshot_changed" }];
  const second = [{ ...f.record }];
  assert.deepEqual(await c["cleanupDetailOf"](first, 7), { state: "pending", cleanupPendingReason: "ack_snapshot_changed" });
  assert.deepEqual(await c["cleanupDetailOf"](second, 7), { state: "acknowledged" });
  assert.equal(await c["cleanupStateOf"](first, 7), "pending");
  assert.equal(await c["cleanupStateOf"](second, 7), "acknowledged");
});

test("real completion retirement results preserve retention/removal and log only correlated allowlisted fields", async () => {
  const { f, coordinator: c, cleanup, calls } = await world("completion-receipt");
  cleanup();
  const originalBoundary = c["withCompletionSourceBoundary"];
  const lines: Array<{ message: string; fields: Record<string, unknown> }> = [];
  c["log"].info = (message, fields) => { lines.push({ message, fields: fields! }); };
  const before = await c.inspect(f.context.runId);
  const retainedFiles = await inventory(f.root);
  const run = async (kind: string, why?: string, executing = false) => {
    assert.equal(await c["resumeInventory"](f.record, undefined, () => executing, "boot"), undefined);
    const line = lines.at(-1)!;
    assert.deepEqual(line, { message: "recovery: re-drive exit", fields: {
      path: "boot", reason: "completion_retirement", outcome: kind, ...(why ? { why } : {}),
      run_id: f.record.runId, generation: 7, capture_id: f.record.captureId,
    } });
    if (kind !== "retired") {
      assert.deepEqual(await c.inspect(f.context.runId), before);
      assert.deepEqual(await inventory(f.root), retainedFiles, "retained result changes no bytes or modes");
    }
  };
  await run("pending", "executing", true);
  // Same kind, changed why must emit; identical tuple must remain quiet.
  c["git"].cleanupRecoveryGeneration = async () => "retained";
  await run("pending", "preflight");
  const count = lines.length;
  await run("pending", "preflight");
  assert.equal(lines.length, count);
  cleanup();
  Object.assign(c, { withCompletionSourceBoundary: undefined });
  await run("unsupported", "no_boundary");
  Object.assign(c, { withCompletionSourceBoundary: async () => "retained" });
  await run("uncertain", "boundary_error");
  const resolve = c["git"].resolveRecoveryBareDir;
  c["git"].resolveRecoveryBareDir = async () => undefined;
  await run("invalid", "mismatch");
  c["git"].resolveRecoveryBareDir = resolve;
  Object.assign(c, { withCompletionSourceBoundary: originalBoundary });
  await run("retired");
  assert.deepEqual(await c.inspect(f.context.runId), []);
  await assert.rejects(fs.stat(f.record.bundlePath!), { code: "ENOENT" });
  assert.equal(calls.cleanup, 3); // uncertain preflight, invalid stops before preflight, retired check + removal
  assert.equal(lines.length, 6);
});

test("live snapshot prunes retirement tuples for vanished captures", async () => {
  const { f, coordinator: c } = await world();
  c["noteRedriveExit"]("live", f.record, "completion_retirement", undefined, false, { outcome: "pending", why: "executing" });
  assert.equal(c["redriveExitLogged"].size, 1);
  await fs.rm(c["runDir"](f.context.runId), { recursive: true });
  await c.resumeLive({ isExecuting: () => false, authenticatedAtMs: 1000 });
  assert.equal(c["redriveExitLogged"].size, 0);
});

test("fixed snapshot digests match the original v1 descriptors; missing ACK preserves replay/B and replay normalization", async () => {
  const { f, coordinator: c } = await world();
  const hash = (value: unknown) => createHash("sha256").update(canonicalJson(value)).digest("hex");
  const names = ["authenticated.json"], refs = ["refs/owed/fixture abc"], settlements: string[] = [];
  const snapshot = { devIno: "1:42", refs };
  c["git"].recoveryRefSnapshot = async () => snapshot;
  c["operatorSourceDescriptor"] = async () => ({ expectedClones: "[]", descriptor: { clones: [], foreignOwners: [], retained: [] } });
  let record: RecoveryRecord = { ...f.record, exactHoldBinding: binding };
  const request = record.finalRequest!;
  let exact: RecoveryExactHold = { ...closed(f), state: "released", release_evidence: request.evidence, final_receipt: request.disposition };
  c["client"].getExactRecoveryHold = async () => exact;
  const assess = (replayed = false) => c["withOperatorBudget"](signals(), io => c["operatorAssess"](io,
    f.context.runId, 7, { kind: "ok", records: [record], names, unbound: [] },
    { terminal: false, settlementNames: settlements, holds: new Set(), runOpenHolds: 0 }, replayed));
  const aBaseline = (kind: string) => {
    const { finalAcknowledged: _ack, ...withoutAck } = record;
    return hash(["retained-local-record-v1", f.context.runId, 7, kind, binding.holdId, binding.repoId,
      record.serverCaptureId ?? null,
      { hold_id: exact.hold_id, repo_id: exact.repo_id, state: exact.state, release_evidence: exact.release_evidence ?? null,
        final_receipt: exact.final_receipt ?? null, run_status: exact.run_status },
      snapshot.devIno, f.context.barePath, refs, names, settlements,
      [hash(kind === "case_a_replay" ? withoutAck : record)]]);
  };
  let result = await assess();
  assert.ok(result.ok);
  assert.equal(result.case, "case_a_ack");
  assert.equal(result.digest, aBaseline("case_a_ack"));
  record = { ...record, finalAcknowledged: false };
  result = await assess();
  assert.ok(result.ok);
  assert.equal(result.case, "case_a_replay");
  const replayDigest = aBaseline("case_a_replay");
  assert.equal(result.digest, replayDigest);
  record = { ...record, finalAcknowledged: true };
  result = await assess(true);
  assert.ok(result.ok);
  assert.equal(result.digest, replayDigest, "only the replay anchor ACK is normalized");
  const original = record;
  record = { ...record, createdAt: record.createdAt + 1 };
  const changed = await assess(true);
  assert.ok(changed.ok);
  assert.notEqual(changed.digest, replayDigest, "non-ACK record evidence remains bound");
  record = original;
  record = { ...record, finalAcknowledged: false };
  exact = { ...exact, state: "discarded", release_evidence: "owner_discard", final_receipt: undefined };
  result = await assess();
  assert.ok(result.ok);
  assert.equal(result.case, "case_b_owner_discard");
  assert.equal(result.digest, hash(["retained-local-record-case-b-v1", f.context.runId, 7, "case_b_owner_discard",
    binding.holdId, binding.repoId, record.captureId, record.serverCaptureId ?? null,
    { hold_id: exact.hold_id, repo_id: exact.repo_id, generation: exact.generation, state: exact.state,
      release_evidence: "owner_discard", final_receipt: null, run_status: exact.run_status,
      completed_publication_receipt: null, inventory_guarded: true,
      original_worker_id: exact.original_worker_id, original_worker_identity: exact.original_worker_identity },
    snapshot.devIno, f.context.barePath, refs, names, settlements, [hash(record)],
    { clones: [], foreignOwners: [], retained: [] }, []]));
});

test("request cancellation propagates an actual aborted signal to the in-flight exact read", async () => {
  const { f, coordinator: c } = await world();
  f.record = await patch(c, f.record, { exactHoldBinding: binding });
  c["operatorObserve"] = async () => ({ terminal: false, settlementNames: [], holds: new Set(), runOpenHolds: 0 });
  const request = new AbortController();
  let received: AbortSignal | undefined;
  let reached!: () => void;
  const ready = new Promise<void>(resolve => { reached = resolve; });
  c["client"].getExactRecoveryHold = async (_run, _generation, _binding, signal) => {
    received = signal;
    reached();
    await new Promise<void>((_resolve, reject) => {
      signal!.addEventListener("abort", () => reject(signal!.reason), { once: true });
    });
    assert.fail("aborted exact read cannot succeed");
  };
  const pending = c.describeLocalRecords(f.context.runId, { ...signals(), requestSignal: request.signal });
  await ready;
  assert.ok(received);
  assert.equal(received.aborted, false);
  request.abort(new Error("diagnostic cancellation"));
  assert.deepEqual(await pending, { kind: "refused", reason: "client_disconnected" });
  assert.equal(received.aborted, true);
  assert.equal((await c.inspect(f.context.runId))[0]?.finalAcknowledged, true);
});

test("real Case A retirement reports reachable outcomes without changing return or cleanup effects", async () => {
  for (const expected of ["cleanup_unavailable", "source_protected", "bare_mismatch",
    "bundle_path_unknown", "pending_generation_report", "cleanup_retained", "forgotten"] as const) {
    const { f, coordinator: c, cleanup, calls } = await world();
    cleanup();
    if (expected === "cleanup_unavailable") c["git"].cleanupRecoveryGeneration = undefined;
    if (expected === "source_protected") Object.assign(c, { isExecuting: () => true });
    if (expected === "bare_mismatch") c["git"].resolveRecoveryBareDir = async () => undefined;
    if (expected === "bundle_path_unknown") {
      await fs.rm(f.record.bundlePath!);
      await fs.mkdir(f.record.bundlePath!);
    }
    if (expected === "pending_generation_report") Object.assign(c, { pendingGenerationReport: () => true });
    if (expected === "cleanup_retained") c["git"].cleanupRecoveryGeneration = async () => "retained";
    const lines: Array<{ message: string; fields: Record<string, unknown> }> = [];
    c["log"].info = (message, fields) => { lines.push({ message, fields: fields! }); };
    assert.equal(await c["resumeInventory"](f.record), undefined);
    assert.deepEqual(lines, [{ message: "recovery: re-drive exit", fields: {
      path: "live", reason: "case_a_retirement", outcome: expected,
      run_id: f.record.runId, generation: 7, capture_id: f.record.captureId,
    } }], expected);
    assert.equal((await c.inspect(f.context.runId)).length, expected === "forgotten" ? 0 : 1, expected);
    if (expected === "forgotten" || expected === "pending_generation_report") {
      await assert.rejects(fs.stat(f.record.bundlePath!), { code: "ENOENT" });
    } else await fs.stat(f.record.bundlePath!);
    assert.equal(calls.cleanup, expected === "forgotten" ? 1 : 0, expected);
    assert.equal(calls.final + calls.reserve + calls.upload + calls.produce + calls.held, 0);
  }
});

test("retirement predicate precedence keeps completion before Case A and unsupported before executing", async () => {
  const { f, coordinator: c } = await world("completion-receipt");
  Object.assign(c, { withCompletionSourceBoundary: undefined });
  Object.assign(c, { isExecuting: () => true });
  const lines: Array<Record<string, unknown>> = [];
  c["log"].info = (_message, fields) => { lines.push(fields!); };
  await c["resumeInventory"]({ ...f.record, finalAcknowledged: true });
  assert.equal(lines[0]?.reason, "completion_retirement");
  assert.equal(lines[0]?.outcome, "unsupported");
  assert.equal(lines[0]?.why, "no_boundary");
  const ack = { ...f.record, completionReceipt: undefined, completionSource: undefined,
    finalAcknowledged: true, reason: "inventory_snapshot_changed" };
  c["checkedRecords"] = async () => [ack];
  c["guardedGeneration"] = async () => true;
  await c["resumeInventory"](ack);
  assert.equal(lines[1]?.reason, "case_a_retirement");
  // No qualifying saved FINAL: missing_ack precedes execution and missing cleanup.
  assert.equal(lines[1]?.outcome, "cleanup_pending");
  assert.equal(lines[1]?.cleanupPendingReason, "missing_ack");
});

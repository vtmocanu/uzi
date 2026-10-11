import assert from "node:assert/strict";
import { it } from "node:test";
import { CodeCrossCheckGate } from "../src/code-cross-check-gate.js";
import type { CodeCrossCheckDispositionBatch, CodeCrossCheckStatus } from "../src/code-cross-check-contract.js";

const finding = { id: "F_1", severity: "major" as const, path: "a.ts", line: 2, title: "Verify guard", detail: "untrusted advice" };
function rig(overrides: Partial<Exclude<CodeCrossCheckStatus, { result: string }>> = {}, lostAck = false) {
  let row: CodeCrossCheckStatus = {
    stage: "code", round: 1, candidate_generation: 7, base_commit: "a".repeat(40), head_commit: "b".repeat(40),
    candidate_digest: "c".repeat(64), checker_run_id: "child", checker_harness: "codex", checker_model: "m",
    checker_effort: "high", outcome: "completed", reason_class: null, findings: [finding],
    dispositions: null, finalized_at: null, interrupted_at: null, deadline_at: new Date(Date.now() + 1000).toISOString(),
    ...overrides,
  };
  let submits = 0, snapshots = 0;
  let deferredRead: Promise<CodeCrossCheckStatus> | undefined;
  const interrupt = new AbortController();
  const batches: CodeCrossCheckDispositionBatch[] = [];
  const gate = new CodeCrossCheckGate({
    client: {
      codeCrossCheckStatus: async () => {
        const pending = deferredRead;
        deferredRead = undefined;
        return pending ?? row;
      },
      submitCodeCrossCheck: async () => { submits++; return row; },
      reportCodeCrossCheckDispositions: async (_id, _gen, batch) => {
        batches.push(batch);
        assert.ok(!("result" in row));
        row = { ...row, finalized_at: new Date().toISOString(),
          dispositions: row.findings.map(f => batch.find(d => d.finding_id === f.id) ??
            { finding_id: f.id, disposition: "not_reported", reason: "" }) };
        if (lostAck) throw new Error("lost ACK");
        return row;
      },
    }, runId: "lead", generation: 7, signal: interrupt.signal,
    snapshot: async () => { snapshots++; return { reason_class: "snapshot_failed" }; },
  });
  return { gate, batches, interrupt, defer: (pending: Promise<CodeCrossCheckStatus>) => { deferredRead = pending; }, set: (next: CodeCrossCheckStatus) => { row = next; }, row: () => row, counts: () => ({ submits, snapshots }) };
}

it("one authorized repair then durable empty finalizer; later done never snapshots or submits", async () => {
  const r = rig();
  const first = await r.gate.done();
  assert.equal(first.action, "repair");
  if (first.action === "repair") {
    assert.match(first.followUp, /<advice_[a-f0-9]{16}>/);
    assert.match(first.followUp, /report_cross_check_dispositions/);
    assert.match(first.followUp, /affected checks/);
  }
  assert.equal((await r.gate.repair())?.findings[0]?.id, "F_1");
  assert.deepEqual(await r.gate.done(), { action: "proceed" });
  assert.deepEqual(r.batches, [[]]);
  assert.equal(await r.gate.repair(), undefined);
  await assert.rejects(r.gate.report([]), /not active/);
  assert.deepEqual(await r.gate.done(), { action: "proceed" });
  assert.equal(r.batches.length, 1);
  assert.deepEqual(r.counts(), { submits: 0, snapshots: 0 });
  const summary = await r.gate.persistedSummary();
  assert.ok(summary && !("incomplete" in summary));
  assert.equal(summary.dispositions?.[0]?.disposition, "not_reported");
});

it("transient status failure refuses report then fresh validation recovers within the single repair pass", async () => {
  const r = rig();
  assert.equal((await r.gate.done()).action, "repair");
  const batch: CodeCrossCheckDispositionBatch = [
    { finding_id: "F_1", disposition: "addressed", reason: "fixed; affected test passed" },
  ];
  r.defer(Promise.reject(new Error("transient status GET failure")));
  await assert.rejects(r.gate.report(batch), /not active/);
  assert.deepEqual(r.batches, [], "unavailable status must not authorize a POST");
  assert.equal(r.gate.repairActive(), true, "unknown status retains eligibility for fresh validation");

  let release!: (row: CodeCrossCheckStatus) => void;
  r.defer(new Promise(resolve => { release = resolve; }));
  const recovered = r.gate.report(batch);
  assert.deepEqual(r.batches, [], "cached evidence cannot authorize while the fresh GET is pending");
  release(r.row());
  await recovered;
  assert.deepEqual(r.batches, [batch], "fresh unchanged server row permits known-ID reporting");

  assert.deepEqual(await r.gate.done(), { action: "proceed" });
  assert.equal(r.gate.repairActive(), false);
  await assert.rejects(r.gate.report(batch), /not active/);
  assert.deepEqual(r.batches, [batch], "second done closes reporting without another POST");
  assert.deepEqual(r.counts(), { submits: 0, snapshots: 0 });
});

it("report refuses before/after authority, unknown/duplicate IDs and oversized UTF-8 reasons", async () => {
  const r = rig();
  await assert.rejects(r.gate.report([]), /not active/);
  await r.gate.done();
  for (const batch of [
    [{ finding_id: "unknown", disposition: "declined", reason: "no" }],
    [{ finding_id: "F_1", disposition: "declined", reason: "no" }, { finding_id: "F_1", disposition: "addressed", reason: "yes" }],
    [{ finding_id: "F_1", disposition: "declined", reason: "界".repeat(342) }],
  ] as CodeCrossCheckDispositionBatch[]) await assert.rejects(r.gate.report(batch), /invalid/);
  const batch: CodeCrossCheckDispositionBatch = [{ finding_id: "F_1", disposition: "addressed", reason: "fixed; affected test passed" }];
  await r.gate.report(batch);
  await r.gate.report(batch); // identical server ACK remains possible within the active pass
  assert.equal(r.batches.length, 2);
  await r.gate.done();
  await assert.rejects(r.gate.report(batch), /not active/);
});

for (const phase of ["before verdict", "before dispositions", "before publication"] as const) {
  it(`interruption ${phase} hides findings/counts and closes old generation authority`, async () => {
    const r = rig(phase === "before verdict" ? { outcome: "pending", findings: [], deadline_at: new Date().toISOString() } : {});
    if (phase !== "before verdict") await r.gate.done();
    if (phase === "before publication") await r.gate.report([{ finding_id: "F_1", disposition: "declined", reason: "verified existing guard" }]);
    const row = r.row();
    assert.ok(!("result" in row));
    r.set({ ...row, candidate_generation: 6, interrupted_at: new Date().toISOString() });
    assert.deepEqual(await r.gate.done(), { action: "proceed" });
    assert.equal(await r.gate.repair(), undefined);
    const summary = await r.gate.persistedSummary();
    assert.ok(summary && !("incomplete" in summary));
    assert.equal(summary.interrupted_at, (r.row() as typeof summary).interrupted_at);
    assert.equal(summary.reason_class, null, "interruption is separate persisted evidence");
    assert.equal(summary.head_commit, row.head_commit);
    assert.equal(summary.base_commit, row.base_commit);
    assert.deepEqual(summary.findings, []);
    assert.ok(!summary.dispositions?.length, "superseded dispositions cannot enter the summary");
    assert.deepEqual(r.row(), { ...row, candidate_generation: 6, interrupted_at: summary.interrupted_at },
      "masking preserves the persisted row");
    assert.deepEqual(r.counts(), { submits: 0, snapshots: 0 });
  });
}

for (const operation of ["repair", "report"] as const) {
  it(`deferred ${operation} read cannot regain authority after second done`, async () => {
    const r = rig();
    await r.gate.done();
    let release!: (row: CodeCrossCheckStatus) => void;
    r.defer(new Promise(resolve => { release = resolve; }));
    const pending = operation === "repair" ? r.gate.repair() : r.gate.report([]);
    const rejected = operation === "report" ? assert.rejects(pending, /not active/) : undefined;
    await r.gate.done();
    release(r.row());
    if (rejected) await rejected;
    else assert.equal(await pending, undefined);
    assert.deepEqual(r.batches, [[]], "only done's missing-disposition finalizer posts");
  });
}

it("deferred report read cannot post after interruption", async () => {
  const r = rig();
  await r.gate.done();
  let release!: (row: CodeCrossCheckStatus) => void;
  r.defer(new Promise(resolve => { release = resolve; }));
  const rejected = assert.rejects(r.gate.report([]));
  r.interrupt.abort();
  release(r.row());
  await rejected;
  assert.deepEqual(r.batches, []);
});

for (const reason_class of ["worker_unsupported", "snapshot_failed"]) {
  it(`no-SHA interrupted row preserves allowlisted ${reason_class} and actual timestamp`, async () => {
    const stamp = new Date().toISOString();
    const r = rig({ head_commit: null, base_commit: null, outcome: "failed", reason_class, findings: [], interrupted_at: stamp });
    await r.gate.done();
    assert.deepEqual(await r.gate.persistedSummary(), r.row());
    const row = r.row();
    assert.ok(!("result" in row));
    r.set({ ...row, candidate_generation: 6, interrupted_at: null });
    assert.deepEqual(await r.gate.persistedSummary(), { incomplete: "interrupted" });
    assert.equal((r.row() as typeof row).reason_class, reason_class);
  });
}

it("lost finalizer ACK re-reads persisted finalized marker and never sends again", async () => {
  const r = rig({}, true);
  await r.gate.done();
  await r.gate.done();
  await r.gate.done();
  assert.deepEqual(r.batches, [[]]);
  const summary = await r.gate.persistedSummary();
  assert.ok(summary && !("incomplete" in summary) && summary.finalized_at);
});

for (const overrides of [{ findings: [] }, { outcome: "failed" as const, findings: [], reason_class: "malformed" },
  { head_commit: null, base_commit: null, outcome: "failed" as const, findings: [], reason_class: "snapshot_failed" },
  { candidate_generation: 6 }, { interrupted_at: new Date().toISOString() }]) {
  it(`zero repair for ${JSON.stringify(overrides)}`, async () => {
    const r = rig(overrides);
    assert.deepEqual(await r.gate.done(), { action: "proceed" });
    assert.equal(await r.gate.repair(), undefined);
  });
}

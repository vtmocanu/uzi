import assert from "node:assert/strict";
import { type FileHandle, mkdtemp, open, rm, writeFile } from "node:fs/promises";
import path from "node:path";
import test, { mock } from "node:test";
import { fileURLToPath } from "node:url";
import { readControl, runProducer, validateControl, validatePaths } from "./calibration-producer.mjs";
import { fixture, projectFailedEvidence } from "./calibration-fixture.mjs";

const scratch = fileURLToPath(new URL("../../.uzi/scratch/", import.meta.url));

test("imports expose entrypoints without starting fixture, worker or load", () => {
  assert.equal(typeof fixture, "function");
  assert.equal(typeof runProducer, "function");
  assert.equal(process.getActiveResourcesInfo().includes("Worker"), false);
});

const emptyEvidence = (): Parameters<typeof projectFailedEvidence>[0] => ({
  trees: [null, null], exits: [], disposals: [], feedback: null, brokerReplies: [null, null],
  serviceRecords: [], acknowledgements: [], observations: [], signalWrites: [], schedule: [],
  survival: null, selectedIndex: undefined, binding: undefined, monitorState: null, history: null,
  leg: undefined, reportId: "report", cleanupStart: undefined, cleanupEnd: undefined,
});
const enrolledTree = {
  treeId: "first-tree", identity: { pid: 123, startIdentity: "456" },
  enrollment: { run_id: "run", worker_id: "worker", claim_generation: 1, memory_episode: 1,
    attempt_id: "attempt", command_id: "command", tool_id: "tool", command: "fixed producer" },
};

test("NOLOAD failed mid-leg report retains observed evidence and explicit unknowns", () => {
  const facts = emptyEvidence();
  facts.trees[0] = enrolledTree;
  facts.selectedIndex = 0;
  facts.feedback = { reason: "worker_memory_pressure", intervention_id: "intervention" };
  facts.brokerReplies[0] = { ok: false, memoryPressure: facts.feedback };
  facts.serviceRecords.push({ path: "/reservation", request: { intervention_id: "intervention" }, ack: { admitted: true } });
  facts.acknowledgements.push({ path: "/reservation", status: 200, endMs: 12, ack: { admitted: true } });
  facts.observations.push({ index: 0, startMs: 10, endMs: 11 });
  facts.signalWrites.push({ index: 0, startMs: 13, endMs: 14 });
  facts.schedule.push({ index: 0, step: 1, startMs: 9, endMs: null, kind: null });
  facts.disposals.push({ index: 0, startMs: 14, endMs: 15, clean: true, evidence: { clean: true } });
  facts.exits.push({ index: 0, atMs: 15, code: 0 });
  facts.binding = { reportId: "report", legId: "leg", interventionId: "intervention",
    treeId: "first-tree", toolId: "tool", reservationId: "intervention" };
  facts.cleanupStart = 16;
  const report = JSON.parse(JSON.stringify(projectFailedEvidence(facts)));
  assert.deepEqual(report.trees, [{ ...enrolledTree, exits: [{ index: 0, atMs: 15, code: 0 }],
    disposals: [{ index: 0, startMs: 14, endMs: 15, clean: true, evidence: { clean: true } }] }, null]);
  assert.deepEqual(report.settlement, {
    feedback: facts.feedback, broker: facts.brokerReplies[0], brokerReplies: facts.brokerReplies,
    serviceRecords: facts.serviceRecords, acknowledgements: facts.acknowledgements,
    observations: facts.observations, signalWrites: facts.signalWrites, schedule: facts.schedule,
    survival: null, selectedIndex: 0, binding: facts.binding, monitorState: null, history: null,
    exits: facts.exits, disposals: facts.disposals,
  });
  assert.deepEqual(report.cleanup, { confirmed: null, startMs: 16, endMs: null });
  assert.equal(report.response, null);
  assert.equal("status" in report, false, "projection cannot promote failed proof to success");
});

test("NOLOAD second setup failure retains first enrollment without inventing second identity", () => {
  const facts = emptyEvidence();
  facts.trees[0] = enrolledTree;
  facts.brokerReplies[1] = { failure: "second setup rejected" };
  const report = JSON.parse(JSON.stringify(projectFailedEvidence(facts)));
  assert.deepEqual(report.trees, [{ ...enrolledTree, exits: [], disposals: [] }, null]);
  assert.equal(report.settlement.feedback, null);
  assert.equal(report.settlement.broker, null);
  assert.deepEqual(report.settlement.brokerReplies, [null, { failure: "second setup rejected" }]);
  assert.equal(report.settlement.selectedIndex, null);
  assert.equal(report.settlement.binding, null);
  assert.deepEqual(report.cleanup, { confirmed: null, startMs: null, endMs: null });
  assert.deepEqual(projectFailedEvidence(emptyEvidence()).trees, [null, null]);
});

test("fixed producer protocol accepts only the next bounded step", () => {
  for (const rounds of [16, 32]) {
    assert.equal(validateControl({ step: 1 }, 0, rounds), 1);
    assert.equal(validateControl({ step: rounds }, rounds - 1, rounds), rounds);
    for (const value of [null, false, 0, [], {}, { step: "1" }, { step: 0 }, { step: 2 },
      { step: 1, chunkBytes: 1 }, { step: 1, rounds: 1 }]) {
      assert.throws(() => validateControl(value, 0, rounds), /invalid fixed producer control/);
    }
    assert.throws(() => validateControl({ step: rounds + 1 }, rounds, rounds));
  }
  assert.throws(() => validateControl({ step: 0 }, -1, 16));
  assert.throws(() => validateControl({ step: 1 }, 0, 1));
});

test("producer paths bind all files to the same fixed owned tree", async () => {
  for (const index of [0, 1]) {
    const control = "/proof/.uzi/scratch/control-" + index;
    validatePaths(control, "/calibration-disk/tree-" + index, "/proof/.uzi/scratch/tree-" + index);
    assert.throws(() => validatePaths(control, "/calibration-disk/tree-" + (1 - index),
      "/proof/.uzi/scratch/tree-" + index), /invalid owned/);
  }
  await assert.rejects(runProducer("/arbitrary", "/arbitrary", "/arbitrary", 16), /invalid owned/);
  await assert.rejects(runProducer("/proof/.uzi/scratch/control-0", "/calibration-disk/tree-0",
    "/proof/.uzi/scratch/tree-0", 999), /invalid producer rounds/);
});

test("control ingestion bounds bytes before decoding and rejects malformed input", async () => {
  const directory = await mkdtemp(path.join(scratch, "producer-test."));
  const file = path.join(directory, "control");
  try {
    await writeFile(file, '{"step":1}');
    assert.equal(validateControl(await readControl(file), 0, 16), 1);
    await writeFile(file, '{"step":1}' + " ".repeat(54)); // Exactly 64 bytes.
    assert.equal(validateControl(await readControl(file), 0, 16), 1);
    await writeFile(file, '{"step":1}' + " ".repeat(55));
    await assert.rejects(readControl(file), /oversized producer control/);
    await writeFile(file, Buffer.from([0xff]));
    await assert.rejects(readControl(file), /encoded data/);
    await writeFile(file, '{"step":');
    await assert.rejects(readControl(file), SyntaxError);
    // Sparse disk input, no memory load: a whole-file read would allocate 64MiB.
    const handle = await open(file, "w");
    await handle.truncate(64 * 1024 * 1024);
    const originalRead = handle.read;
    const reads: number[] = [];
    const tracker = mock.method(Object.getPrototypeOf(handle), "read", function (this: FileHandle, ...args: unknown[]) {
      const buffer = args[0] as Buffer;
      reads.push(Number(args[2] ?? buffer.byteLength));
      return Reflect.apply(originalRead, this, args);
    });
    await handle.close();
    try {
      await assert.rejects(readControl(file), /oversized producer control/);
      assert.ok(reads.length > 0, "bounded handle reads must occur");
      assert.ok(reads.reduce((sum, bytes) => sum + bytes, 0) <= 65, "at most 64 bytes plus sentinel");
    } finally { tracker.mock.restore(); }
    await rm(file);
    await assert.rejects(readControl(file), { code: "ENOENT" });
  } finally { await rm(directory, { recursive: true, force: true }); }
});

import { requireSupervisorObservationPasses } from "./supervisor-observation-proof.mjs";
import assert from "node:assert/strict";
import { execFile } from "node:child_process";
import { promisify } from "node:util";
import { type FileHandle, mkdtemp, open, readFile, rm, writeFile } from "node:fs/promises";
import path from "node:path";
import { runInNewContext } from "node:vm";
import test, { mock } from "node:test";
import { fileURLToPath } from "node:url";
import { publishAck, readControl, runProducer, validateControl, validatePaths } from "./calibration-producer.mjs";
import { captureEventCounters, calibrationUnits, createCalibrationBrokers, expectedPrimaryExit, fixture, freshPostCleanupDwell, oomEventEvidence, plannedChargeClasses, projectFailedEvidence } from "./calibration-fixture.mjs";
import { ExecutionRegistry, newLocalExecutionEpoch } from "../../agent/src/codex/registry.js";
import { boundedCommandWait } from "../../agent/src/codex/memory-command.js";
import type { SpawnCommandResult } from "../../agent/src/codex/broker.js";

const scratch = fileURLToPath(new URL("../../.uzi/scratch/", import.meta.url));

test("fixture abort settles only its exact producer callback while its sibling remains live", async () => {
  const registry = new ExecutionRegistry(newLocalExecutionEpoch(1));
  const controllers = [new AbortController(), new AbortController()] as const;
  const releases: (() => void)[] = [];
  const seen: (AbortSignal | undefined)[] = [];
  const brokers = createCalibrationBrokers({ registry, worktreePath: "/proof",
    grants: { role: "coder", phase: "implement", isRoot: true, allowedTools: new Set(["Bash"]), allowedSkills: new Set() },
    fileop: { op: async () => ({ ok: false }) }, delegate: async () => ({ ok: false }),
    spawnCommand: async (_argv, options) => new Promise<SpawnCommandResult>((resolve, reject) => {
      seen.push(options.signal);
      releases.push(() => resolve({ code: 0, stdout: "owned release", stderr: "" }));
      const abort = () => reject(new Error("owned producer aborted"));
      if (options.signal?.aborted) abort();
      else options.signal?.addEventListener("abort", abort, { once: true });
    }),
  }, [controllers[0].signal, controllers[1].signal]);
  const replies = brokers.map((broker, index) => broker.handleToolCall({
    threadId: "fixture-abort-thread", turnId: "fixture-abort-turn", callId: "fixture-abort-" + index,
  }, "Bash", { command: "printf fixture" }, "root"));
  let siblingSettled = false;
  void replies[1]!.finally(() => { siblingSettled = true; });
  try {
    assert.equal(seen.length, 2, "both real brokers reached their owned spawn seam");
    controllers[0].abort();
    const result = await boundedCommandWait(replies[0]!, 500);
    assert.equal(result.ok, false, "the exact aborted callback settles as a broker error");
    assert.equal(siblingSettled, false, "sibling callback is still live");
    assert.equal(seen[1]!.aborted, false);
    releases[1]!();
    const sibling = await boundedCommandWait(replies[1]!, 500);
    assert.equal(sibling.ok, true);
  } finally {
    for (const controller of controllers) controller.abort();
    for (const release of releases) release();
    await Promise.allSettled(replies);
  }
});

test("planned charge metadata names four fixed logical budgets without starting load", () => {
  for (const [rounds, budget] of [[16, 33554432], [32, 67108864]] as const) {
    const plan = { limitBytes: rounds / 16 * 1024 ** 3, ceilingBytes: rounds / 16 * 512 * 1024 ** 2,
      baselineBytes: 0, allocationBytes: 4 * budget, overheadBytes: 64 * 1024 ** 2,
      chunkBytes: 1048576, rounds, windowMs: 30000 };
    assert.deepEqual(plannedChargeClasses(plan), [
      "touched_anonymous", "dirty_disk", "fsynced_read_file_cache", "tmpfs_shmem",
    ].map((kind) => ({ kind, producerCount: 2, chunkBytes: 1048576,
      maxRoundsPerProducer: rounds, combinedBudgetBytes: budget })));
    for (const change of [{ rounds: 17 }, { rounds: 0 }, { chunkBytes: 1 },
      { allocationBytes: 1 }, { limitBytes: 1 }]) {
      assert.throws(() => plannedChargeClasses({ ...plan, ...change }), /invalid fixed charge budget/);
    }
  }
  assert.deepEqual(calibrationUnits, { memory: "bytes", monotonicTiming: "milliseconds",
    growth: "bytes_per_millisecond", psiAverages: "percent", psiTotal: "microseconds",
    events: "count", processStartIdentity: "opaque_kernel_start_ticks" });
});

test("OOM evidence preserves actual endpoint IDs, zero, partial unknowns and signed deltas", () => {
  const before = { oom: 7, oom_kill: 3, oom_group_kill: 0 };
  const after = { oom: 8, oom_kill: 1, oom_group_kill: 0 };
  // Reader availability does not erase separately captured memory.events.
  const records = [{ sample: { sampleId: 4, available: false }, events: before },
    { sample: { sampleId: 5, available: true }, events: null },
    { sample: { sampleId: 9, available: false }, events: after }];
  assert.deepEqual(oomEventEvidence(records), { fromSampleId: 4, toSampleId: 9,
    before, after, delta: { oom: 1, oom_kill: -2, oom_group_kill: 0 } });
  assert.deepEqual(oomEventEvidence([records[0]!, { sample: { sampleId: 10 }, events: { oom: 0 } }]),
    { fromSampleId: 4, toSampleId: 10, before, after: { oom: 0, oom_kill: null, oom_group_kill: null },
      delta: { oom: -7, oom_kill: null, oom_group_kill: null } });
  const zero = { oom: 0, oom_kill: 0, oom_group_kill: 0 };
  assert.deepEqual(oomEventEvidence([{ sample: { sampleId: 1 }, events: zero }]),
    { fromSampleId: 1, toSampleId: 1, before: zero, after: zero, delta: zero });
  assert.deepEqual(oomEventEvidence([{ sample: { sampleId: 1 }, events: null },
    { sample: { sampleId: 2 }, events: zero }]),
    { fromSampleId: 1, toSampleId: 2, before: { oom: null, oom_kill: null, oom_group_kill: null },
      after: zero, delta: { oom: null, oom_kill: null, oom_group_kill: null } });
  assert.equal(oomEventEvidence([]), null);
  assert.throws(() => oomEventEvidence([{ sample: { sampleId: 1 }, events: { oom: -1 } }]));
});

test("raw partial event capture preserves known OOM evidence while refusing completion", () => {
  const captured = captureEventCounters("oom 7\noom_kill 3\n");
  assert.deepEqual(captured.events, { oom: 7, oom_kill: 3 });
  assert.match(captured.failure!, /missing event oom_group_kill/);
  const evidence = oomEventEvidence([{ sample: { sampleId: 4 }, events: captured.events }]);
  assert.deepEqual(evidence, { fromSampleId: 4, toSampleId: 4,
    before: { oom: 7, oom_kill: 3, oom_group_kill: null },
    after: { oom: 7, oom_kill: 3, oom_group_kill: null },
    delta: { oom: 0, oom_kill: 0, oom_group_kill: null } });
});

test("event capture accepts complete counters and refuses invalid raw records", () => {
  const complete = "oom 7\noom_kill 3\noom_group_kill 0\n";
  assert.deepEqual(captureEventCounters(complete), {
    events: { oom: 7, oom_kill: 3, oom_group_kill: 0 }, failure: null,
  });
  for (const raw of [complete + "oom 8\n", complete + "low -1\n",
    complete + "low 9007199254740992\n", complete + "low 1 extra\n",
    complete + "low NaN\n", ""]) {
    const captured = captureEventCounters(raw);
    assert.equal(captured.events, null, raw);
    assert.ok(captured.failure, raw);
  }
});

test("only an expected owned SIGKILL exit can settle a fixed producer", () => {
  assert.equal(expectedPrimaryExit(true, 137), true);
  assert.equal(expectedPrimaryExit(false, 137), false);
  for (const code of [0, 1, -9, 137.5, false, true, null, undefined, "137"]) {
    assert.equal(expectedPrimaryExit(true, code), false);
  }
});

test("imports expose entrypoints without starting fixture, worker or load", () => {
  assert.equal(typeof fixture, "function");
  assert.equal(typeof runProducer, "function");
  assert.equal(process.getActiveResourcesInfo().includes("Worker"), false);
});

test("CJS load worker ACKs four churn steps under the locked tsx preload", async () => {
  const directory = await mkdtemp(path.join(scratch, "churn-preload-test."));
  const script = path.join(directory, "churn-preload.mjs");
  const loader = fileURLToPath(new URL("../../agent/node_modules/tsx/dist/loader.mjs", import.meta.url));
  const fixturePath = fileURLToPath(new URL("../../agent/test/fixtures/worker-memory-load.cjs", import.meta.url));
  try {
    // A file entrypoint keeps node:test flags out of inherited worker execArgv.
    await writeFile(script, `
import assert from "node:assert/strict";
import { mkdir, stat } from "node:fs/promises";
import path from "node:path";
import { Worker } from "node:worker_threads";
const [fixturePath, directory, loader] = process.argv.slice(2);
assert.deepEqual(process.execArgv, ["--import", loader]);
const diskDir = path.join(directory, "disk");
const shmemDir = path.join(directory, "shmem");
await mkdir(diskDir);
await mkdir(shmemDir);
// Ordinary directories exercise fixture IO; this is not a tmpfs/PSI proof.
const worker = new Worker(fixturePath, {
  workerData: { diskDir, shmemDir, rounds: 16, chunkBytes: 1024 * 1024 },
  resourceLimits: { maxOldGenerationSizeMb: 16, maxYoungGenerationSizeMb: 4, stackSizeMb: 1 },
});
let timer;
let acknowledgements = 0;
try {
  // Four sequential steps cover all four nested churn threads. Any failure
  // stops this case; one seven-second deadline bounds the entire sequence.
  await new Promise((resolve, reject) => {
    const fail = (reason) => reject(new Error("step " + (acknowledgements + 1) + " ACK failed: " + reason));
    timer = setTimeout(() => fail("deadline"), 7000);
    worker.on("error", (error) => fail(error.code + ": " + error.message));
    worker.on("exit", (code) => fail("unexpected worker exit " + code));
    worker.on("message", (message) => {
      if (message !== "step") { fail("unexpected reply " + message); return; }
      acknowledgements++;
      console.log("step " + acknowledgements + " ACK");
      if (acknowledgements === 4) resolve();
      else worker.postMessage("step");
    });
    worker.postMessage("step");
  });
  for (const file of [path.join(diskDir, "dirty"), path.join(diskDir, "cache"), path.join(shmemDir, "shmem")]) {
    assert.equal((await stat(file)).size, 4 * 1024 * 1024);
  }
} finally {
  clearTimeout(timer);
  await worker.terminate();
}
`);
    // The child is reaped before cleanup, including the ten-second hard limit.
    const { stdout } = await promisify(execFile)(process.execPath,
      ["--import", loader, script, fixturePath, directory, loader],
      { timeout: 10000, killSignal: "SIGKILL" });
    assert.equal(stdout, "step 1 ACK\nstep 2 ACK\nstep 3 ACK\nstep 4 ACK\n");
  } finally { await rm(directory, { recursive: true, force: true }); }
});

test("ACK publication keeps old JSON readable until the owned temporary file is renamed", async () => {
  const directory = await mkdtemp(path.join(scratch, "ack-test."));
  const control = path.join(directory, "control");
  try {
    await writeFile(control + ".ack", '{"step":1}');
    let barrierReached = false;
    await publishAck(control, 2, async () => {
      barrierReached = true;
      assert.deepEqual(JSON.parse(await readFile(control + ".ack.next", "utf8")), { step: 2 });
      assert.deepEqual(JSON.parse(await readFile(control + ".ack", "utf8")), { step: 1 });
    });
    assert.equal(barrierReached, true);
    assert.deepEqual(JSON.parse(await readFile(control + ".ack", "utf8")), { step: 2 });
    await assert.rejects(readFile(control + ".ack.next"), { code: "ENOENT" });
  } finally { await rm(directory, { recursive: true, force: true }); }
});

const dwellGuard = { reserveBytes: 100, hysteresisBytes: 20, sampleMs: 250, responseBudgetMs: 5000, rearmMs: 500 };
const lowSample = (at: number, sampleId: number, currentBytes = 50): Parameters<typeof freshPostCleanupDwell>[0][number] => ({
  available: true, sampleId, startedAtMs: at - 1, completedAtMs: at, currentBytes, limitBytes: 200,
  stat: { anon: 0, shmem: 0, slab_unreclaimable: 0, unevictable: 0 },
  pressure: { some: { avg10: 0, avg60: 0, avg300: 0, total: 0 }, full: { avg10: 0, avg60: 0, avg300: 0, total: 0 } },
});

test("synthetic post-cleanup dwell waits beyond an already armed monitor", () => {
  const samples = [1000, 1250, 1500, 1750].map((at, i) => lowSample(at, i + 1));
  // The monitor may arm at 1500 using the pre-cleanup low at 1000.
  assert.equal(freshPostCleanupDwell(samples.slice(0, 3), 1100, 1500, dwellGuard, 200), false);
  assert.equal(freshPostCleanupDwell(samples, 1100, 1750, dwellGuard, 200), true);
  assert.equal(freshPostCleanupDwell(samples, 1249, 1750, dwellGuard, 200), false, "start must be strictly after cleanup");
  assert.equal(freshPostCleanupDwell(samples.slice(0, 2), 900, 1250, dwellGuard, 200), false);
});

test("synthetic post-cleanup dwell resets at Q and refuses gaps, stale or invalid evidence", () => {
  const samples = [1250, 1500, 1750, 2000, 2250].map((at, i) => lowSample(at, i + 1, i === 1 ? 80 : 50));
  assert.equal(freshPostCleanupDwell(samples.slice(0, 4), 1100, 2000, dwellGuard, 200), false);
  assert.equal(freshPostCleanupDwell(samples, 1100, 2250, dwellGuard, 200), true);
  const gap = [lowSample(1250, 1), lowSample(7000, 2), lowSample(7250, 3), lowSample(7500, 4)];
  assert.equal(freshPostCleanupDwell(gap, 1100, 7500, dwellGuard, 200), false);
  const valid = [1250, 1500, 1750].map((at, i) => lowSample(at, i + 1));
  for (const change of [{ currentBytes: NaN }, { limitBytes: 201 }, { available: false, reason: "absent" },
    { sampleId: 1 }, { completedAtMs: 1498 }, { startedAtMs: NaN }]) {
    const invalid = valid.map((sample, i) => i === 1 ? { ...sample, ...change } : sample);
    assert.equal(freshPostCleanupDwell(invalid as typeof valid, 1100, 1750, dwellGuard, 200), false);
  }
  assert.equal(freshPostCleanupDwell(valid, 1100, 1750 + 5251, dwellGuard, 200), false);
  assert.equal(freshPostCleanupDwell(valid, 1100, 1749, dwellGuard, 200), false);
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
  facts.samplingSchedule = { periodMs: 250, anchorMs: 9,
    rows: [{ sampleId: 7, requestedOrdinal: 0, dueAtMs: 9, dispatchAtMs: 9.5 }],
    maxPositiveLatenessMs: null, maxAbsoluteJitterMs: null };
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
  assert.deepEqual(report.samplingSchedule, facts.samplingSchedule);
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
  assert.equal(projectFailedEvidence(emptyEvidence()).samplingSchedule, null);
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

test("real supervisor proof refuses absent, skipped or incomplete Go subtests", () => {
  const names = ["TestRealObservationSupervisedTrees",
    "TestRealObservationSupervisedTrees/repeat-and-detached-doublefork",
    "TestRealObservationSupervisedTrees/two-oversized-siblings-stay-alive"];
  const lines = names.map(name => `    --- PASS: ${name} (0.01s)`);
  const valid = lines.join("\n");
  requireSupervisorObservationPasses(valid);
  for (const output of ["ok [no tests to run]", lines[0]!, lines.slice(1).join("\n"), lines.slice(0, 2).join("\n"),
    valid.replace("--- PASS:", "--- SKIP:"), valid.replace("--- PASS:", "--- FAIL:"),
    valid.replace("repeat-and-detached-doublefork (", "repeat-and-detached-doublefork-extra (")]) {
    assert.throws(() => requireSupervisorObservationPasses(output));
  }
});


test("parallel proof children accept only the exact existing scratch symlink", async () => {
  const script = (await readFile(new URL("./image-layout.mjs", import.meta.url), "utf8"))
    .replace(/^import .* from "node:fs";$/m, "");
  let existing: string | undefined;
  const context = {
    symlinkSync: (target: string, _link: string) => {
      if (existing !== undefined) throw Object.assign(new Error("already exists"), { code: "EEXIST" });
      existing = target;
    },
    readlinkSync: (_link: string) => existing,
  };
  runInNewContext(script, { ...context });
  assert.equal(existing, "/proof/.uzi");
  runInNewContext(script, { ...context }); // The next Node test child sees the first child's link.
  existing = "/foreign";
  assert.throws(() => runInNewContext(script, { ...context }), /already exists/);
  assert.throws(() => runInNewContext(script, { ...context,
    symlinkSync: () => { throw Object.assign(new Error("permission denied"), { code: "EACCES" }); },
  }), /permission denied/);
});

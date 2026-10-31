import assert from "node:assert/strict";
import fs from "node:fs";
import { test } from "node:test";
import { compileFunction } from "node:vm";
import { inspectPodEpochBootstrap } from "../src/pod-epoch-bootstrap.js";
import { PodEpochSettlementError, runPodEpochShadow } from "../src/pod-epoch-reclaim.js";
import type { createPodEpoch, PodEpochOwnership } from "../src/pod-epoch-lock.js";

const source = fs.readFileSync(new URL("../src/main.ts", import.meta.url), "utf8");

// Bind to executable symbols within main, ignoring explanatory comments and imports.
function contract(text: string): void {
  const start = text.indexOf("async function main(): Promise<void> {");
  assert.notEqual(start, -1, "main boundary exists");
  const main = text.slice(start).replace(/\/\*[\s\S]*?\*\//g, "").replace(/\/\/[^\n]*/g, "");
  assert.match(main, /^async function main\(\): Promise<void> \{\s*recordMaintenanceBootstrap\(snapshotMaintenanceBootstrap\(\)\);/,
    "maintenance bootstrap is the first main statement");
  const at = (symbol: string): number => {
    const position = main.indexOf(symbol);
    assert.notEqual(position, -1, "required executable symbol: " + symbol);
    return position;
  };
  const runtime = at("const epochRuntime = epochEnabled ? createPodEpoch() : undefined");
  const bootstrap = at("inspectPodEpochBootstrap({");
  assert.match(main.slice(bootstrap, main.indexOf("const epochOwnership", bootstrap)),
    /UZI_POD_UID: process.env.UZI_POD_UID,[\s\S]*UZI_POD_EPOCH_RUNTIME: process.env.UZI_POD_EPOCH_RUNTIME,[\s\S]*UZI_POD_EPOCH_EVIDENCE_FILE: process.env.UZI_POD_EPOCH_EVIDENCE_FILE,[\s\S]*\}, epochRuntime\)/,
    "bootstrap reads the three environment fields using the native runtime");
  const acquire = at("await epochRuntime.acquire(");
  assert.equal(main.match(/createPodEpoch\(\)/g)?.length, 1, "one native runtime per participant");
  assert.ok(runtime < bootstrap && bootstrap < acquire, "read-only selector precedes acquisition");
  assert.ok(at('process.on(sig,') < acquire, "signal cancellation precedes epoch acquisition");
  assert.ok(at('controller.abort()') < acquire, "early signal handler aborts lock wait");
  const consume = at("await epochOwnership.consume(epochBootstrap.podUid)");
  assert.ok(acquire < consume, "consume requires acquired epoch ownership");
  const restore = at("restoreEpochRuntimeTmpdir(epochOwnership)");
  assert.ok(consume < restore, "runtime TMPDIR restoration follows ownership and consumption");
  for (const symbol of ["loadConfig()", "ensureRunnerTmpdir(", "resolveDockerWiring(",
    "await outbox.init()", "git.inspectRetireScratchAtStartup(", "reclaimStrandedRunHomes(",
    "jobRunner.reapStaleWorkspaces()", "reapCodexOrphansAtStartup(", "startupReapVerdict(",
    "await worker.run("]) {
    assert.ok(restore < at(symbol), "runtime TMPDIR restored before " + symbol);
    assert.ok(consume < at(symbol), "epoch consumed before " + symbol);
  }
  const acquisition = main.slice(acquire, main.indexOf(": undefined", acquire));
  assert.match(acquisition, /signal: controller.signal/, "bounded acquisition receives early cancellation");
  assert.match(acquisition, /firstMode: epochBootstrap.firstMode/, "selector chooses the first lock mode");
  const shadow = at("await runPodEpochShadow(");
  assert.ok(at('if (verdict === "shutdown")') < shadow, "reaper refusal and shutdown precede shadow");
  assert.ok(at("startupReapVerdict(") < shadow, "startup settlement precedes shadow");
  const admission = at("await worker.run(");
  assert.ok(shadow < at("if (controller.signal.aborted) return;"), "cancellation checked after shadow");
  assert.ok(at("if (controller.signal.aborted) return;") < admission, "cancellation prevents admission");
  assert.match(main.slice(shadow, admission), /if \(result !== undefined\) log.info\("pod_epoch_reclaim", \{ \.\.\.result \}\)/);
  assert.doesNotMatch(main, /epochOwnership\.(close|reacquireShared)\(/, "main retains lifetime ownership and delegates handover");
  const fatal = main.slice(at("main().catch((err) => {"));
  assert.ok(fatal.indexOf('fatalLog.error("fatal"') < fatal.indexOf("if (err instanceof PodEpochSettlementError) process.exit(1);"),
    "fatal settlement is logged before forced termination");
  assert.match(fatal, /if \(err instanceof PodEpochSettlementError\) process.exit\(1\);\s*process.exitCode = 1;/,
    "settlement uncertainty forces exit while ordinary errors retain exitCode handling");
}

test("pod epoch main startup orders ownership, consumption and runtime TMPDIR before writers and admission", () => {
  contract(source);
});

// Execute the actual main boundary statements with controlled ports, without starting
// config, native /data I/O, network clients or worker loops.
function executeBoundary(body: string, ports: Record<string, unknown>): Promise<void> {
  return compileFunction("return (async () => { " + body + "\n})();", Object.keys(ports))(...Object.values(ports));
}

function section(from: string, through: string): string {
  const start = source.indexOf(from);
  const end = source.indexOf(through, start);
  assert.ok(start >= 0 && end >= start, "main boundary extraction applies to candidate");
  return source.slice(start, end + through.length);
}

test("main uses one runtime, consumes the bootstrap UID and restores TMP before config", async () => {
  const events: string[] = [];
  const podUid = "12345678-1234-1234-1234-123456789abc";
  const environment = {
    UZI_EPOCH_LOCK: "1", UZI_DATA_DIR: "/data", UZI_POD_UID: podUid,
    UZI_POD_EPOCH_RUNTIME: "containerd-runc", UZI_POD_EPOCH_EVIDENCE_FILE: "/etc/uzi/pod-epoch/evidence",
  };
  const controller = new AbortController();
  const ownership: PodEpochOwnership = {
    mode: "exclusive",
    consume: async uid => {
      assert.equal(uid, podUid);
      events.push("consume");
      return { result: "fresh", mayEvaluate: true };
    },
    close: () => assert.fail("lifetime ownership must remain open"),
    reacquireShared: async () => assert.fail("handover belongs to shadow"),
    matchesEntry: () => false,
  };
  const runtime: ReturnType<typeof createPodEpoch> = {
    inspectConsumption: uid => { assert.equal(uid, podUid); events.push("peek"); return "fresh"; },
    acquire: async options => {
      assert.equal(options?.firstMode, "exclusive");
      assert.equal(options?.signal, controller.signal);
      events.push("acquire");
      // Consumption must use the bootstrap snapshot, not read the environment again.
      environment.UZI_POD_UID = "changed-after-bootstrap";
      return ownership;
    },
    seed: async () => assert.fail("startup must not seed"),
    prepare: () => assert.fail("startup must not prepare"),
    hold: async () => assert.fail("startup must not hold"),
  };
  const body = section('const epochEnabled =', "if (epochOwnership) restoreEpochRuntimeTmpdir(epochOwnership);");
  const ports = {
    process: { env: environment }, controller, inspectPodEpochBootstrap,
    createPodEpoch: () => { events.push("runtime"); return runtime; },
    restoreEpochRuntimeTmpdir: (value: PodEpochOwnership) => {
      assert.equal(value, ownership); events.push("restore");
    },
  };
  await executeBoundary(body, ports);
  assert.deepEqual(events, ["runtime", "peek", "acquire", "consume", "restore"]);
  events.length = 0;
  environment.UZI_EPOCH_LOCK = "0";
  await executeBoundary(body, ports);
  assert.deepEqual(events, [], "nonparticipants do not touch epoch runtime or TMP");
});

test("an undeclared runtime acquires SH, consumes the UID and yields values the shadow reports as undeclared", async () => {
  const events: string[] = [];
  const podUid = "12345678-1234-1234-1234-123456789abc";
  const environment = { UZI_EPOCH_LOCK: "1", UZI_DATA_DIR: "/data", UZI_POD_UID: podUid } as Record<string, string>;
  const controller = new AbortController();
  const ownership: PodEpochOwnership = {
    mode: "shared",
    consume: async uid => { assert.equal(uid, podUid); events.push("consume"); return { result: "fresh", mayEvaluate: false }; },
    close: () => assert.fail("lifetime ownership must remain open"),
    reacquireShared: async () => assert.fail("a shared first mode has no handover"),
    matchesEntry: () => assert.fail("no evidence is evaluated"),
  };
  const runtime: ReturnType<typeof createPodEpoch> = {
    inspectConsumption: () => { events.push("peek"); return "fresh"; },
    acquire: async options => { events.push("acquire:" + options?.firstMode); return ownership; },
    seed: async () => assert.fail("startup must not seed"),
    prepare: () => assert.fail("startup must not prepare"),
    hold: async () => assert.fail("startup must not hold"),
  };
  const body = section('const epochEnabled =', "if (epochOwnership) restoreEpochRuntimeTmpdir(epochOwnership);") +
    "\nreturn { epochBootstrap, epochOwnership, epochConsumption };";
  const boundary = await compileFunction("return (async () => { " + body + "\n})();", ["process", "controller",
    "inspectPodEpochBootstrap", "createPodEpoch", "restoreEpochRuntimeTmpdir"])(
    { env: environment }, controller, inspectPodEpochBootstrap, () => runtime, () => events.push("restore")) as
    { epochBootstrap: Parameters<typeof runPodEpochShadow>[0]["bootstrap"]; epochOwnership: PodEpochOwnership;
      epochConsumption: Parameters<typeof runPodEpochShadow>[0]["consumption"] };
  assert.deepEqual(events, ["acquire:shared", "consume", "restore"]);
  let reads = 0;
  const verdict = await runPodEpochShadow({
    bootstrap: boundary.epochBootstrap, ownership: boundary.epochOwnership, consumption: boundary.epochConsumption,
    signal: controller.signal, readEvidence: async () => { reads++; return ""; },
    git: { discoverEpochRetainedSources: async () => assert.fail("no census"), inspectRetainedSources: async () => assert.fail("no inspection") },
    client: { listRecoveryHolds: async () => assert.fail("no holds"), getRunOwnership: async () => assert.fail("no ownership") },
  });
  assert.equal(reads, 0);
  assert.equal(verdict?.reason, "runtime_not_declared");
  assert.deepEqual(verdict?.authority, { result: "undeclared", reason: "runtime_not_declared" });
});

const admissionBody = section("const verdict = startupReapVerdict(", "await worker.run(controller.signal);");

function admissionPorts(shadow: (options: Record<string, unknown>) => Promise<unknown>) {
  const events: string[] = [];
  const controller = new AbortController();
  const ports = {
    controller, reap: undefined, startupReapVerdict: () => "run",
    epochBootstrap: {}, epochOwnership: {}, epochConsumption: {},
    config: { dockerWiring: { dockerHost: "unix:///docker.sock" } }, git: {}, client: {},
    runPodEpochShadow: shadow,
    log: { info: (message: string, fields: unknown) => { events.push(message); assert.ok(fields); } },
    worker: { run: async (signal: AbortSignal) => { assert.equal(signal, controller.signal); events.push("run"); } },
  };
  return { ports, events, controller, run: () => executeBoundary('let lifecyclePhase;\n' + admissionBody, ports) };
}

test("main waits for shadow handover, forwards its ports and logs a defined verdict before admission", async () => {
  let resolve!: (value: unknown) => void;
  const promise = new Promise<unknown>(done => { resolve = done; });
  const fixture = admissionPorts(async options => {
    assert.equal(options.bootstrap, fixture.ports.epochBootstrap);
    assert.equal(options.ownership, fixture.ports.epochOwnership);
    assert.equal(options.consumption, fixture.ports.epochConsumption);
    assert.equal(options.dockerHost, "unix:///docker.sock");
    assert.equal(options.git, fixture.ports.git);
    assert.equal(options.client, fixture.ports.client);
    assert.equal(options.signal, fixture.controller.signal);
    fixture.events.push("shadow");
    return promise;
  });
  const running = fixture.run();
  assert.deepEqual(fixture.events, ["shadow"], "pending handover keeps admission closed");
  resolve({ verdict: "deferred", reason: "runtime_not_declared" });
  await running;
  assert.deepEqual(fixture.events, ["shadow", "pod_epoch_reclaim", "run"]);
});

test("main omits undefined verdict logging and refuses admission after shadow cancellation or rejection", async () => {
  const ordinary = admissionPorts(async () => undefined);
  await ordinary.run();
  assert.deepEqual(ordinary.events, ["run"]);

  const aborted = admissionPorts(async () => { aborted.controller.abort(); return undefined; });
  await aborted.run();
  assert.deepEqual(aborted.events, []);

  for (const error of [new PodEpochSettlementError(), new Error("native SH handover failed")]) {
    const failed = admissionPorts(async () => { throw error; });
    await assert.rejects(failed.run(), value => value === error);
    assert.deepEqual(failed.events, [], "failed settlement or native handover prevents admission");
  }
});

test("main fatal boundary logs then forces process exit only for settlement uncertainty", async () => {
  const body = section("main().catch((err) => {", "    process.exitCode = 1;")
    .replace("main().catch((err) => {", "");
  for (const error of [new PodEpochSettlementError(), new Error("ordinary startup failure")]) {
    const events: string[] = [];
    const terminated = new Error("simulated process termination");
    const processPort = {
      exitCode: 0,
      exit: (code: number) => { assert.equal(code, 1); events.push("exit"); throw terminated; },
    };
    const running = executeBoundary(body, {
      err: error, PodEpochSettlementError, errMessage: (err: Error) => err.message,
      fatalLog: { error: () => events.push("log") }, lifecyclePhase: "reap", process: processPort,
    });
    if (error instanceof PodEpochSettlementError) {
      await assert.rejects(running, value => value === terminated);
      assert.deepEqual(events, ["log", "exit"]);
      assert.equal(processPort.exitCode, 0, "forced exit does not fall through to passive exitCode");
    } else {
      await running;
      assert.deepEqual(events, ["log"]);
      assert.equal(processPort.exitCode, 1);
    }
  }
});

test("startup contract rejects omitted runtime TMPDIR restoration at its named assertion", () => {
  const call = "if (epochOwnership) restoreEpochRuntimeTmpdir(epochOwnership);";
  assert.ok(source.includes(call), "restore mutation applies to current candidate");
  assert.throws(() => contract(source.replace(call, "")),
    /required executable symbol: restoreEpochRuntimeTmpdir/, "omit restore must redden the restoration contract");
});

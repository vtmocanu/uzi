// The agent report (agent-report.ts) through the real launcher and reporter: counts against node's
// own tally, live progress past a held file, the hung-file watchdog, process-group cleanup, and
// the fail-closed states a consumer must never read as green.
import assert from "node:assert/strict";
import { spawn, spawnSync } from "node:child_process";
import { cpSync, existsSync, mkdirSync, mkdtempSync, readFileSync, readdirSync, realpathSync, rmSync, writeFileSync } from "node:fs";
import { createRequire } from "node:module";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";
import { DEFAULT_STALL_MS, hungFiles, type Progress, type Summary } from "./agent-report.js";

const agentRoot = fileURLToPath(new URL("../", import.meta.url));
const require = createRequire(import.meta.url);

/** A launcher fixture: the real launcher, reporter and shard selector over the given test files. */
function fixture(files: Record<string, string>): string {
  const root = mkdtempSync(path.join(tmpdir(), "uzi-agent-report-"));
  mkdirSync(path.join(root, "test/setup"), { recursive: true });
  mkdirSync(path.join(root, "node_modules"));
  writeFileSync(path.join(root, "package.json"), '{"type":"module"}\n');
  for (const dependency of ["tsx", "esbuild"]) {
    const installed = path.dirname(require.resolve(dependency + "/package.json"));
    cpSync(realpathSync(installed), path.join(root, "node_modules", dependency), { recursive: true, dereference: true });
  }
  cpSync(realpathSync(path.join(agentRoot, "node_modules/@esbuild")),
    path.join(root, "node_modules/@esbuild"), { recursive: true, dereference: true });
  for (const file of ["run-unit-tests.ts", "agent-test-shards.ts", "agent-report.ts", "agent-report-reporter.ts"]) {
    cpSync(path.join(agentRoot, "test", file), path.join(root, "test", file));
  }
  writeFileSync(path.join(root, "test-shards.json"), JSON.stringify({ "1/2": [], "2/2": [] }));
  writeFileSync(path.join(root, "test/setup/hermetic-proc.ts"), "export {};\n");
  for (const [name, body] of Object.entries(files)) writeFileSync(path.join(root, "test", name), body);
  return root;
}

function env(root: string, extra: NodeJS.ProcessEnv = {}): NodeJS.ProcessEnv {
  const base: NodeJS.ProcessEnv = { ...process.env, FIXTURE_ROOT: root };
  for (const key of ["NODE_TEST_CONTEXT", "UZI_AGENT_TEST_SHARD", "UZI_AGENT_TEST_CONCURRENCY", "UZI_AGENT_TEST_JUNIT",
    "UZI_AGENT_TEST_REPORT_DIR", "UZI_AGENT_TEST_REPORT_RUN_DIR",
    "UZI_AGENT_TEST_STALL_MS", "UZI_AGENT_TEST_STALL_TESTONLY"]) delete base[key];
  return { ...base, ...extra };
}

function latestDir(root: string): string {
  return (JSON.parse(readFileSync(path.join(root, ".test-report/latest.json"), "utf8")) as { dir: string }).dir;
}

function readJson<T>(file: string): T | null {
  try {
    return JSON.parse(readFileSync(file, "utf8")) as T;
  } catch {
    return null;
  }
}

function alive(pid: number): boolean {
  try {
    process.kill(pid, 0);
    return true;
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ESRCH") return false;
    throw error;
  }
}

async function until(predicate: () => boolean, ms: number, what: string): Promise<void> {
  const deadline = Date.now() + ms;
  while (!predicate()) {
    if (Date.now() >= deadline) assert.fail(`timed out waiting for ${what}`);
    await new Promise((resolve) => setTimeout(resolve, 50));
  }
}

function launch(root: string, extra: NodeJS.ProcessEnv = {}, preload?: string) {
  const imports = preload ? ["--import", preload] : [];
  const child = spawn(process.execPath, ["--import", "tsx", ...imports, "test/run-unit-tests.ts"], {
    cwd: root, env: env(root, extra), stdio: ["ignore", "pipe", "pipe"],
  });
  const output: string[] = [];
  child.stdout.on("data", (data: Buffer) => output.push(data.toString()));
  child.stderr.on("data", (data: Buffer) => output.push(data.toString()));
  const closed = new Promise<{ code: number | null; signal: NodeJS.Signals | null }>((resolve, reject) => {
    child.on("error", reject);
    child.on("close", (code, signal) => resolve({ code, signal }));
  });
  return { child, output, closed };
}

const outcomes = {
  "a-outcomes.test.ts": [
    'import test, { describe, it, before } from "node:test";',
    'test("plain pass", () => {});',
    'describe("suite", () => { it("nested pass", () => {}); it("nested fail", () => { throw new Error("nested boom"); }); });',
    'test("skipped", { skip: true }, () => {});',
    'test("todo", { todo: true }, () => {});',
    'test("times out", { timeout: 100 }, async () => { await new Promise((resolve) => setTimeout(resolve, 400)); });',
    'describe("hooked", () => { before(() => { throw new Error("hook boom"); }); it("under a failing hook", () => {}); });',
  ].join("\n"),
  "b-crash.test.ts": 'throw new Error("top-level crash");\n',
};

test("summary counts match node's own tally for every outcome, and the stdout tail names the failures", { timeout: 60_000 }, () => {
  const root = fixture(outcomes);
  try {
    const result = spawnSync(process.execPath, ["--import", "tsx", "test/run-unit-tests.ts"], {
      cwd: root, env: env(root), encoding: "utf8", timeout: 45_000,
    });
    assert.equal(result.status, 1, result.stdout + result.stderr);
    const summary = readJson<Summary>(path.join(latestDir(root), "summary.json"));
    assert.ok(summary, result.stdout + result.stderr);
    assert.equal(summary.state, "complete");
    assert.equal(summary.unit_green, false);
    assert.equal(summary.scope, "unit");
    assert.equal(summary.invocation.full_suite, true);
    const progress = summary.progress!;
    const node = progress.node_counts!;
    assert.ok(node, "node's final tally is recorded");
    // Differential check against node's own summary: same tests, same outcome buckets.
    // node counts a crashed file as one failed test; the report keeps it apart as a file failure.
    assert.equal(progress.counts.tests + progress.counts.file_failures, node.tests);
    assert.equal(progress.counts.cancelled, node.cancelled);
    assert.equal(progress.counts.skip, node.skipped);
    assert.equal(progress.counts.todo, node.todo);
    assert.equal(progress.counts.pass, node.passed);
    assert.equal(progress.counts.fail + progress.counts.file_failures, node.failed);
    assert.deepEqual(
      { pass: progress.counts.pass, fail: progress.counts.fail, cancelled: progress.counts.cancelled,
        file: progress.counts.file_failures, suite: progress.counts.suite_failures },
      { pass: 2, fail: 1, cancelled: 2, file: 1, suite: 1 },
    );
    const kinds = progress.failures.map((failure) => `${failure.kind}:${failure.name}`).sort();
    assert.deepEqual(kinds, ["cancelled:times out", "cancelled:under a failing hook", "fail:nested fail", "file:test/b-crash.test.ts",
      "suite:hooked"]);
    assert.match(progress.failures.find((failure) => failure.kind === "file")!.error ?? "", /top-level crash/);
    // The stdout tail is the compact summary an agent reads with `tail`.
    const tail = result.stdout.slice(result.stdout.lastIndexOf("agent unit tests:"));
    assert.match(tail, /^agent unit tests: COMPLETE, unit tests NOT green; exit=1/);
    assert.match(tail, /CANCELLED test\/a-outcomes\.test\.ts:\d+ "times out" testTimeoutFailure/);
    assert.match(tail, /FILE test\/b-crash\.test\.ts .*top-level crash/);
    assert.equal(readFileSync(path.join(latestDir(root), "summary.txt"), "utf8"), tail.trimStart());
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("a filtered run is labelled filtered and a clean run is unit-green", { timeout: 60_000 }, () => {
  const root = fixture({ "ok.test.ts": 'import test from "node:test";\ntest("ok", () => {});\ntest("other", () => {});\n' });
  try {
    const result = spawnSync(process.execPath, ["--import", "tsx", "test/run-unit-tests.ts", "--test-name-pattern=^ok$"], {
      cwd: root, env: env(root), encoding: "utf8", timeout: 45_000,
    });
    assert.equal(result.status, 0, result.stdout + result.stderr);
    const summary = readJson<Summary>(path.join(latestDir(root), "summary.json"))!;
    assert.equal(summary.state, "complete");
    assert.equal(summary.unit_green, true);
    assert.equal(summary.invocation.full_suite, false);
    assert.deepEqual(summary.invocation.filters, ["--test-name-pattern=^ok$"]);
    assert.match(result.stdout, /FILTERED \(shard=- filters=--test-name-pattern=\^ok\$\)/);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("a later file's failure is visible in progress while an earlier file is still held", { timeout: 60_000 }, async () => {
  const root = fixture({
    "a-hold.test.ts": [
      'import test from "node:test";',
      'import { existsSync } from "node:fs";',
      'test("held", async () => {',
      '  while (!existsSync(process.env.FIXTURE_ROOT + "/release")) await new Promise((resolve) => setTimeout(resolve, 25));',
      '});',
    ].join("\n"),
    "b-fail.test.ts": 'import test from "node:test";\ntest("fails early", () => { throw new Error("early"); });\n',
  });
  const run = launch(root, { UZI_AGENT_TEST_CONCURRENCY: "2" });
  try {
    await until(() => existsSync(path.join(root, ".test-report/latest.json")), 20_000, "latest.json");
    const progressFile = path.join(latestDir(root), "progress.json");
    // The held file is listed at its first inner event, which can trail the other file's failure.
    await until(() => {
      const seen = readJson<Progress>(progressFile);
      return (seen?.failures ?? []).some((failure) => failure.name === "fails early")
        && seen?.files["test/a-hold.test.ts"] !== undefined;
    }, 20_000, "the early failure and the held file in progress.json");
    const live = readJson<Progress>(progressFile)!;
    assert.equal(live.state, "running");
    assert.ok(live.files["test/a-hold.test.ts"] && live.files["test/a-hold.test.ts"].done_at === undefined,
      "the held file is still in flight");
    assert.equal(existsSync(path.join(latestDir(root), "summary.json")), false, "no final summary while running");
    writeFileSync(path.join(root, "release"), "");
    const result = await run.closed;
    assert.equal(result.code, 1, run.output.join(""));
    const summary = readJson<Summary>(path.join(latestDir(root), "summary.json"))!;
    assert.equal(summary.state, "complete");
    assert.equal(summary.progress!.counts.fail, 1);
  } finally {
    writeFileSync(path.join(root, "release"), "");
    if (run.child.exitCode === null && run.child.signalCode === null) run.child.kill("SIGKILL");
    await run.closed.catch(() => undefined);
    rmSync(root, { recursive: true, force: true });
  }
});

// A file whose test passes but whose process never exits: a ref'd timer holds its event loop, and a
// descendant in the same process group ignores SIGTERM.
const leaking = [
  'import test from "node:test";',
  'import { spawn } from "node:child_process";',
  'import { writeFileSync } from "node:fs";',
  'test("passes, then leaks", () => {',
  '  const child = spawn(process.execPath, ["-e", "process.on(\'SIGTERM\', () => {}); setInterval(() => {}, 1000);"], { stdio: "ignore" });',
  '  writeFileSync(process.env.FIXTURE_ROOT + "/leak.pid", String(process.pid));',
  '  writeFileSync(process.env.FIXTURE_ROOT + "/runner.pid", String(process.ppid));',
  '  writeFileSync(process.env.FIXTURE_ROOT + "/grandchild.pid", String(child.pid));',
  '  setInterval(() => {}, 1000);',
  '});',
].join("\n");

test("the watchdog stops a hung file only after every other file finished, and reaps its process group", { timeout: 60_000 }, async () => {
  const root = fixture({
    "a-leak.test.ts": leaking,
    "b-ok.test.ts": 'import test from "node:test";\ntest("ok", async () => { await new Promise((resolve) => setTimeout(resolve, 300)); });\n',
  });
  const run = launch(root, { UZI_AGENT_TEST_STALL_MS: "1500", UZI_AGENT_TEST_STALL_TESTONLY: "1", UZI_AGENT_TEST_CONCURRENCY: "2" });
  try {
    const result = await run.closed;
    const text = run.output.join("");
    assert.equal(result.code, 1, text);
    assert.equal(result.signal, null, text);
    assert.match(text, /HUNG test file\(s\) test\/a-leak\.test\.ts/);
    const summary = readJson<Summary>(path.join(latestDir(root), "summary.json"))!;
    assert.equal(summary.state, "incomplete");
    assert.equal(summary.unit_green, false);
    assert.equal(summary.termination, "watchdog");
    assert.deepEqual(summary.hung_files, ["test/a-leak.test.ts"]);
    assert.equal(summary.cleanup, "verified");
    assert.ok(summary.progress!.files["test/b-ok.test.ts"]!.done_at, "the other file completed first");
    assert.equal(summary.progress!.counts.pass, 2, "the leaking file's test itself passed");
    assert.equal(alive(Number(readFileSync(path.join(root, "leak.pid"), "utf8"))), false, "the hung file process is gone");
    assert.equal(alive(Number(readFileSync(path.join(root, "grandchild.pid"), "utf8"))), false,
      "a SIGTERM-ignoring descendant is gone before the launcher returns");
  } finally {
    for (const name of ["leak.pid", "grandchild.pid"]) {
      const pid = Number(readJson<number>(path.join(root, name)));
      if (pid > 1 && alive(pid)) process.kill(pid, "SIGKILL");
    }
    if (run.child.exitCode === null && run.child.signalCode === null) run.child.kill("SIGKILL");
    await run.closed.catch(() => undefined);
    rmSync(root, { recursive: true, force: true });
  }
});

test("a caller's signal after the watchdog fired is still re-raised", { timeout: 60_000 }, async () => {
  const root = fixture({
    "a-leak.test.ts": leaking,
    "b-ok.test.ts": 'import test from "node:test";\ntest("ok", () => {});\n',
  });
  const run = launch(root, { UZI_AGENT_TEST_STALL_MS: "1500", UZI_AGENT_TEST_STALL_TESTONLY: "1", UZI_AGENT_TEST_CONCURRENCY: "2" });
  try {
    // The SIGTERM-ignoring descendant holds cleanup open for its 2 s grace: signal inside that window.
    await until(() => /HUNG test file/.test(run.output.join("")), 30_000, "the watchdog");
    run.child.kill("SIGINT");
    const result = await run.closed;
    assert.equal(result.signal, "SIGINT", run.output.join(""));
    const summary = readJson<Summary>(path.join(latestDir(root), "summary.json"))!;
    assert.equal(summary.termination, "watchdog");
    assert.equal(summary.state, "incomplete");
  } finally {
    for (const name of ["leak.pid", "grandchild.pid"]) {
      const pid = Number(readJson<number>(path.join(root, name)));
      if (pid > 1 && alive(pid)) process.kill(pid, "SIGKILL");
    }
    if (run.child.exitCode === null && run.child.signalCode === null) run.child.kill("SIGKILL");
    await run.closed.catch(() => undefined);
    rmSync(root, { recursive: true, force: true });
  }
});

test("a runner killed from outside still has its process group reaped", { timeout: 60_000 }, async () => {
  const root = fixture({ "a-leak.test.ts": leaking });
  const run = launch(root);
  try {
    await until(() => existsSync(path.join(root, "grandchild.pid")), 20_000, "the leaking test");
    process.kill(Number(readFileSync(path.join(root, "runner.pid"), "utf8")), "SIGKILL");
    const result = await run.closed;
    assert.equal(result.signal, "SIGKILL", run.output.join(""));
    const summary = readJson<Summary>(path.join(latestDir(root), "summary.json"))!;
    assert.equal(summary.state, "incomplete");
    assert.equal(summary.termination, "none");
    assert.equal(summary.cleanup, "verified");
    for (const name of ["leak.pid", "grandchild.pid"]) {
      assert.equal(alive(Number(readFileSync(path.join(root, name), "utf8"))), false, `${name} is reaped`);
    }
  } finally {
    for (const name of ["leak.pid", "grandchild.pid"]) {
      const pid = Number(readJson<number>(path.join(root, name)));
      if (pid > 1 && alive(pid)) process.kill(pid, "SIGKILL");
    }
    if (run.child.exitCode === null && run.child.signalCode === null) run.child.kill("SIGKILL");
    await run.closed.catch(() => undefined);
    rmSync(root, { recursive: true, force: true });
  }
});

test("the watchdog does not fire while another file is still queued; SIGTERM then finalizes incomplete", { timeout: 60_000 }, async () => {
  const root = fixture({
    "a-leak.test.ts": leaking,
    "b-queued.test.ts": 'import test from "node:test";\ntest("queued", () => {});\n',
  });
  // Concurrency 1: the queued file cannot start while the hung one holds the only slot.
  const run = launch(root, { UZI_AGENT_TEST_STALL_MS: "500", UZI_AGENT_TEST_STALL_TESTONLY: "1", UZI_AGENT_TEST_CONCURRENCY: "1" });
  try {
    await until(() => existsSync(path.join(root, "grandchild.pid")), 20_000, "the leaking test");
    await new Promise((resolve) => setTimeout(resolve, 2_500)); // five stall windows
    assert.equal(run.child.exitCode, null, "still running: a queued file blocks the watchdog\n" + run.output.join(""));
    assert.doesNotMatch(run.output.join(""), /HUNG/);
    run.child.kill("SIGTERM");
    const result = await run.closed;
    assert.equal(result.signal, "SIGTERM", run.output.join(""));
    const summary = readJson<Summary>(path.join(latestDir(root), "summary.json"))!;
    assert.equal(summary.state, "incomplete");
    assert.equal(summary.termination, "signal");
    assert.deepEqual(summary.hung_files, []);
    assert.deepEqual(summary.in_flight_files, ["test/a-leak.test.ts"]);
    assert.equal(summary.cleanup, "verified");
    assert.equal(alive(Number(readFileSync(path.join(root, "grandchild.pid"), "utf8"))), false);
  } finally {
    for (const name of ["leak.pid", "grandchild.pid"]) {
      const pid = Number(readJson<number>(path.join(root, name)));
      if (pid > 1 && alive(pid)) process.kill(pid, "SIGKILL");
    }
    if (run.child.exitCode === null && run.child.signalCode === null) run.child.kill("SIGKILL");
    await run.closed.catch(() => undefined);
    rmSync(root, { recursive: true, force: true });
  }
});

test("a test registered by a helper module never counts as a file, so a queued file still blocks the watchdog", { timeout: 60_000 }, async () => {
  const root = fixture({
    "a-leak.test.ts": [
      'import test from "node:test";',
      'import { registerHelperTest } from "./helper.js";',
      'import { writeFileSync } from "node:fs";',
      'registerHelperTest();',
      'test("local, then leaks", () => {',
      '  writeFileSync(process.env.FIXTURE_ROOT + "/leak.pid", String(process.pid));',
      '  setInterval(() => {}, 1000);',
      '});',
    ].join("\n"),
    "helper.ts": 'import test from "node:test";\nexport function registerHelperTest(): void { test("from a helper", () => {}); }\n',
    "b-queued.test.ts": [
      'import test from "node:test";',
      'import { writeFileSync } from "node:fs";',
      'test("queued", () => { writeFileSync(process.env.FIXTURE_ROOT + "/queued-ran", ""); });',
    ].join("\n"),
  });
  const run = launch(root, { UZI_AGENT_TEST_STALL_MS: "500", UZI_AGENT_TEST_STALL_TESTONLY: "1", UZI_AGENT_TEST_CONCURRENCY: "1" });
  try {
    await until(() => existsSync(path.join(root, "leak.pid")), 20_000, "the leaking test");
    await new Promise((resolve) => setTimeout(resolve, 2_500)); // five stall windows
    assert.equal(run.child.exitCode, null, "a helper-located test must not stand in for the queued file\n" + run.output.join(""));
    assert.doesNotMatch(run.output.join(""), /HUNG/);
    const progress = readJson<Progress>(path.join(latestDir(root), "progress.json"))!;
    assert.deepEqual(Object.keys(progress.files), ["test/a-leak.test.ts"]);
    assert.ok(progress.unattributed_last_event_at, "the helper's events are recorded as unattributed");
    assert.equal(progress.counts.pass, 2, "helper-defined tests still count");
    run.child.kill("SIGTERM");
    await run.closed;
    assert.equal(existsSync(path.join(root, "queued-ran")), false);
  } finally {
    const pid = Number(readJson<number>(path.join(root, "leak.pid")));
    if (pid > 1 && alive(pid)) process.kill(pid, "SIGKILL");
    if (run.child.exitCode === null && run.child.signalCode === null) run.child.kill("SIGKILL");
    await run.closed.catch(() => undefined);
    rmSync(root, { recursive: true, force: true });
  }
});

// Imported by the fixture launcher alone, never by this test process or its node --test child.
// Only diagnostic group probes are faked. Cleanup probes and signals still use the real OS.
function observationPreload(root: string, mode: "absent" | "transient" | "persistent" | "signal" | "eperm"): string {
  const preload = path.join(root, "observation.mjs");
  writeFileSync(preload, `
import { writeFileSync } from "node:fs";
const mode = ${JSON.stringify(mode)};
const traceFile = ${JSON.stringify(path.join(root, "observation.json"))};
const trace = { probes: 0, timers: [], elapsed: 0, signals: [], cleanupProbes: 0 };
const save = () => writeFileSync(traceFile, JSON.stringify(trace));
const kill = process.kill.bind(process);
const now = Date.now.bind(Date);
const timer = globalThis.setTimeout;
let observationTimer = false;
let observationStart;
Date.now = () => observationStart === undefined ? now() : observationStart + trace.elapsed;
process.kill = (pid, signal) => {
  if (pid < 0 && signal === 0) {
    if (new Error().stack.includes("groupHasLiveMember")) {
      observationStart ??= now();
      trace.probes++;
      const present = mode !== "absent" && mode !== "eperm"
        && (mode !== "transient" || trace.probes < 3);
      observationTimer = present;
      save();
      if (!present) throw Object.assign(new Error("fixture probe"), { code: mode === "eperm" ? "EPERM" : "ESRCH" });
      return true;
    }
    trace.cleanupProbes++;
    save();
  } else if (pid < 0) {
    trace.signals.push(signal);
    save();
  }
  return kill(pid, signal);
};
globalThis.setTimeout = (callback, ms, ...args) => {
  if (!observationTimer || ms !== 50) return timer(callback, ms, ...args);
  observationTimer = false;
  trace.timers.push(ms);
  save();
  return timer(() => {
    trace.elapsed += ms;
    save();
    if (mode === "signal") {
      observationStart = undefined;
      process.emit("SIGTERM");
    }
    callback(...args);
  }, 0);
};
save();
`);
  return preload;
}

type ObservationTrace = {
  probes: number;
  timers: number[];
  elapsed: number;
  signals: string[];
  cleanupProbes: number;
};

for (const mode of ["absent", "eperm", "transient", "persistent", "signal"] as const) {
  test(`normal-exit observation: ${mode}`, { timeout: 60_000 }, async () => {
    const root = fixture({ "ok.test.ts": 'import test from "node:test";\ntest("ok", () => {});\n' });
    const run = launch(root, {}, observationPreload(root, mode));
    try {
      const result = await run.closed;
      const output = run.output.join("");
      const summary = readJson<Summary>(path.join(latestDir(root), "summary.json"))!;
      const trace = readJson<ObservationTrace>(path.join(root, "observation.json"))!;
      assert.ok(summary, output);
      assert.ok(trace, output);
      if (mode === "signal") {
        assert.equal(result.signal, "SIGTERM", output);
        assert.equal(summary.state, "incomplete");
        assert.equal(summary.unit_green, false);
        assert.equal(summary.termination, "signal");
        assert.equal(summary.cleanup, "verified", "interruption enters abnormal cleanup after observation");
        assert.deepEqual(trace.signals, ["SIGTERM"]);
        assert.ok(trace.cleanupProbes > 0, "the real abnormal group cleanup ran");
        assert.deepEqual(trace.timers, [50]);
        assert.equal(trace.probes, 1, "no diagnostic probing after interruption");
      } else {
        assert.equal(result.code, 0, output);
        assert.equal(result.signal, null, output);
        assert.equal(summary.state, "complete");
        assert.equal(summary.unit_green, true);
        assert.equal(summary.termination, "none");
        assert.deepEqual(trace.signals, [], "observation never signals descendants");
        assert.equal(trace.cleanupProbes, 0);
        if (mode === "persistent") {
          assert.equal(summary.cleanup, "leftover");
          assert.equal(trace.probes, 11, "initial probe plus ten polls");
          assert.deepEqual(trace.timers, Array(10).fill(50), "50ms polling through the fixed 500ms deadline");
          assert.equal(trace.elapsed, 500);
          assert.match(output, /processes from the test run are still alive after it exited/);
          assert.match(output, /LEFTOVER processes/);
        } else {
          assert.equal(summary.cleanup, "not-needed");
          assert.equal(trace.probes, mode === "transient" ? 3 : 1);
          assert.deepEqual(trace.timers, mode === "transient" ? [50, 50] : [],
            "initially absent (including EPERM) never waits; transient groups stop polling when gone");
          assert.equal(trace.elapsed, mode === "transient" ? 100 : 0);
        }
      }
      if (mode !== "persistent") {
        assert.doesNotMatch(output, /processes from the test run are still alive|LEFTOVER processes/);
      }
    } finally {
      if (run.child.exitCode === null && run.child.signalCode === null) run.child.kill("SIGKILL");
      await run.closed.catch(() => undefined);
      rmSync(root, { recursive: true, force: true });
    }
  });
}

test("an uninterrupted run that leaves a process behind is reported as leftover, not swept", { timeout: 60_000 }, () => {
  const root = fixture({
    "leaves.test.ts": [
      'import test from "node:test";',
      'import { spawn } from "node:child_process";',
      'import { writeFileSync } from "node:fs";',
      'test("spawns and forgets", () => {',
      '  const child = spawn(process.execPath, ["-e", "setInterval(() => {}, 1000);"], { stdio: "ignore" });',
      '  child.unref();',
      '  writeFileSync(process.env.FIXTURE_ROOT + "/left.pid", String(child.pid));',
      '});',
    ].join("\n"),
  });
  try {
    const result = spawnSync(process.execPath, ["--import", "tsx", "test/run-unit-tests.ts"], {
      cwd: root, env: env(root), encoding: "utf8", timeout: 45_000,
    });
    assert.equal(result.status, 0, result.stdout + result.stderr);
    const summary = readJson<Summary>(path.join(latestDir(root), "summary.json"))!;
    assert.equal(summary.state, "complete");
    assert.equal(summary.cleanup, "leftover");
    assert.match(result.stdout, /LEFTOVER processes/);
    assert.equal(alive(Number(readFileSync(path.join(root, "left.pid"), "utf8"))), true, "diagnostic only: not killed");
  } finally {
    const pid = Number(readJson<number>(path.join(root, "left.pid")));
    if (pid > 1 && alive(pid)) process.kill(pid, "SIGKILL");
    rmSync(root, { recursive: true, force: true });
  }
});

test("concurrent invocations write separate report directories", { timeout: 60_000 }, async () => {
  const root = fixture({ "ok.test.ts": 'import test from "node:test";\ntest("ok", () => {});\n' });
  try {
    const runs = [launch(root), launch(root)];
    const results = await Promise.all(runs.map((run) => run.closed));
    for (const [index, result] of results.entries()) assert.equal(result.code, 0, runs[index]!.output.join(""));
    const dirs = readdirSync(path.join(root, ".test-report"), { withFileTypes: true }).filter((entry) => entry.isDirectory());
    assert.equal(dirs.length, 2);
    for (const dir of dirs) {
      const summary = readJson<Summary>(path.join(root, ".test-report", dir.name, "summary.json"))!;
      assert.equal(summary.state, "complete");
      assert.equal(summary.invocation.id, dir.name);
    }
    assert.ok(dirs.some((dir) => path.join(realpathSync(root), ".test-report", dir.name) === latestDir(root)));
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("the stall override is refused outside the launcher's own tests", () => {
  const root = fixture({ "ok.test.ts": 'import test from "node:test";\ntest("ok", () => {});\n' });
  try {
    for (const extra of [{ UZI_AGENT_TEST_STALL_MS: "1000" },
      { UZI_AGENT_TEST_STALL_MS: "x", UZI_AGENT_TEST_STALL_TESTONLY: "1" }]) {
      const result = spawnSync(process.execPath, ["--import", "tsx", "test/run-unit-tests.ts"], {
        cwd: root, env: env(root, extra), encoding: "utf8", timeout: 30_000,
      });
      assert.equal(result.status, 1, result.stdout + result.stderr);
      assert.match(result.stderr, /UZI_AGENT_TEST_STALL_MS/);
      assert.doesNotMatch(result.stdout, /agent unit tests:/);
    }
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("hungFiles needs every pending file silent and every other file done", () => {
  const at = (ms: number): string => new Date(ms).toISOString();
  const progress = (files: Progress["files"]): Progress => ({
    schema: 1, scope: "unit", state: "running", started_at: at(0), heartbeat_at: at(0), last_event_at: at(0),
    unattributed_last_event_at: null, selection_known: true, files_total: 3, files, counts: { tests: 0, pass: 0, fail: 0, cancelled: 0, skip: 0, todo: 0, file_failures: 0, suite_failures: 0 },
    failures: [], failures_omitted: 0, node_counts: null,
  });
  const done = { started_at: at(0), last_event_at: at(0), done_at: at(0) };
  const silent = { started_at: at(0), last_event_at: at(0) };
  const active = { started_at: at(0), last_event_at: at(9_000) };
  assert.deepEqual(hungFiles(progress({ a: silent, b: done, c: done }), 3, 10_000, 5_000), ["a"]);
  assert.deepEqual(hungFiles(progress({ a: silent, b: done }), 3, 10_000, 5_000), [], "a file not yet started blocks it");
  assert.deepEqual(hungFiles(progress({ a: silent, b: active, c: done }), 3, 10_000, 5_000), [], "an active file blocks it");
  assert.deepEqual(hungFiles(progress({ a: done, b: done, c: done }), 3, 10_000, 5_000), []);
});

// Node caps each test at --test-timeout, but an explicit test or suite `timeout` option overrides that
// cap. A test allowed to run silently for longer than the stall window could be stopped as hung, so
// every timeout option must evaluate, with a strict grammar (numbers, same-file numeric constants,
// + - * / and parentheses), to well below the window. Anything else is an offender to check by hand.

/**
 * Index just past the bracket, string or comment construct starting at `open`. Every step advances,
 * so malformed input (an unterminated string or comment) ends at the end of the source, never loops.
 */
function skipBalanced(source: string, open: number): number {
  const pairs: Record<string, string> = { "(": ")", "[": "]", "{": "}" };
  const stack: string[] = [];
  let index = open;
  while (index < source.length) {
    const char = source[index]!;
    let next = index + 1;
    if (char === '"' || char === "'" || char === "`") {
      let scan = index + 1;
      while (scan < source.length && source[scan] !== char) scan += source[scan] === "\\" ? 2 : 1;
      next = Math.min(scan + 1, source.length);
    } else if (char === "/" && source[index + 1] === "/") {
      const newline = source.indexOf("\n", index);
      next = newline < 0 ? source.length : newline;
    } else if (char === "/" && source[index + 1] === "*") {
      const close = source.indexOf("*/", index + 2);
      next = close < 0 ? source.length : close + 2;
    } else if (pairs[char]) {
      stack.push(pairs[char]);
    } else if (char === stack[stack.length - 1]) {
      stack.pop();
    }
    index = next;
    if (stack.length === 0) return index;
  }
  return source.length;
}

/** Top-level comma-separated pieces of `text` (an argument list or an object body). */
function splitTopLevel(text: string): string[] {
  const parts: string[] = [];
  let begin = 0;
  for (let index = 0; index < text.length;) {
    const char = text[index]!;
    if ("([{\"'`".includes(char) || (char === "/" && (text[index + 1] === "/" || text[index + 1] === "*"))) {
      index = skipBalanced(text, index);
    } else {
      if (char === ",") {
        parts.push(text.slice(begin, index));
        begin = index + 1;
      }
      index++;
    }
  }
  parts.push(text.slice(begin));
  return parts.map((part) => part.trim()).filter(Boolean);
}

/** Strict arithmetic over numbers and same-file `const NAME = <expr>`; undefined for anything else. */
function evaluateTimeout(expression: string, source: string, seen: Set<string> = new Set()): number | undefined {
  const tokens = expression.match(/\d[\d_]*(?:\.\d+)?|[A-Za-z_$][\w$]*|[-+*/()]|\S/g) ?? [];
  let position = 0;
  const factor = (): number | undefined => {
    const token = tokens[position++];
    if (token === undefined) return undefined;
    if (/^\d/.test(token)) return Number(token.replaceAll("_", ""));
    if (token === "(") {
      const value = sum();
      return tokens[position++] === ")" ? value : undefined;
    }
    if (/^[A-Za-z_$]/.test(token) && !seen.has(token)) {
      const declared = new RegExp(`\\bconst\\s+${token}\\s*(?::\\s*number\\s*)?=\\s*([^;\\n]+)`).exec(source)?.[1];
      return declared === undefined ? undefined : evaluateTimeout(declared, source, new Set([...seen, token]));
    }
    return undefined;
  };
  const product = (): number | undefined => {
    let value = factor();
    while (value !== undefined && (tokens[position] === "*" || tokens[position] === "/")) {
      const operator = tokens[position++];
      const right = factor();
      value = right === undefined ? undefined : operator === "*" ? value * right : value / right;
    }
    return value;
  };
  const sum = (): number | undefined => {
    let value = product();
    while (value !== undefined && (tokens[position] === "+" || tokens[position] === "-")) {
      const operator = tokens[position++];
      const right = product();
      value = right === undefined ? undefined : operator === "+" ? value + right : value - right;
    }
    return value;
  };
  const value = sum();
  return position === tokens.length && value !== undefined && Number.isFinite(value) ? value : undefined;
}

/** The source with string and comment contents blanked (same length), so code inside a fixture string is not scanned. */
function blankLiterals(source: string): string {
  let out = "";
  for (let index = 0; index < source.length;) {
    const char = source[index]!;
    const comment = char === "/" && (source[index + 1] === "/" || source[index + 1] === "*");
    if (char === '"' || char === "'" || char === "`" || comment) {
      const end = skipBalanced(source, index);
      const body = source.slice(index, end);
      out += comment ? body.replace(/[^\n]/g, " ") : char + body.slice(1, -1).replace(/[^\n]/g, " ") + char;
      index = end;
    } else {
      out += char;
      index++;
    }
  }
  return out;
}

/**
 * Every literal-object `timeout` option passed to a test, suite or hook (incl. .only/.skip/.todo), as raw
 * expressions. Known gaps: options passed as a variable are not followed, and regex literals are not
 * recognised (a quote inside one can hide later options); the 100-option floor bounds the damage.
 */
function timeoutOptions(original: string): string[] {
  const source = blankLiterals(original);
  const found: string[] = [];
  const call = /\b(?:test|it|describe|suite|before|after|beforeEach|afterEach)(?:\.(?:only|skip|todo))?\s*\(/g;
  for (const match of source.matchAll(call)) {
    const open = match.index! + match[0].length - 1;
    const args = splitTopLevel(source.slice(open + 1, skipBalanced(source, open) - 1));
    for (const arg of args.slice(0, 2)) {
      if (!arg.startsWith("{")) continue;
      for (const property of splitTopLevel(arg.slice(1, -1))) {
        const pair = /^timeout\s*(?::\s*([\s\S]+))?$/.exec(property);
        if (pair) found.push(pair[1]?.trim() ?? "timeout");
      }
    }
  }
  return found;
}

function timeoutOffenders(file: string, source: string, limit: number): string[] {
  const offenders: string[] = [];
  for (const expression of timeoutOptions(source)) {
    const value = evaluateTimeout(expression, source);
    if (value === undefined) offenders.push(`${file}: unsupported timeout ${JSON.stringify(expression)}`);
    else if (value > limit) offenders.push(`${file}: timeout ${expression} = ${value} > ${limit}`);
  }
  return offenders;
}

const TIMEOUT_LIMIT = DEFAULT_STALL_MS - 120_000;

test("the timeout guard parses whole expressions and refuses what it cannot evaluate", () => {
  const offenders = (source: string): string[] => timeoutOffenders("f.ts", source, TIMEOUT_LIMIT);
  assert.deepEqual(offenders('test("a", { timeout: 10_000 }, () => {});'), []);
  assert.deepEqual(offenders('const PER = 120000;\nit("a", { timeout: PER }, () => {});'), []);
  assert.deepEqual(offenders('describe("a", { timeout: 2 * 60_000, concurrency: 1 }, () => {});'), []);
  assert.equal(offenders('test("slow", { timeout: 100 * 60_000 }, () => {});').length, 1, "spaced arithmetic");
  assert.equal(offenders('test("slow", { signal: { aborted: false }, timeout: 3_600_000 }, () => {});').length, 1,
    "nested option objects");
  assert.equal(offenders('test.only("slow", { timeout: (10 + 5) * 60_000 }, () => {});').length, 1);
  assert.equal(offenders('test("x", { timeout: Number.MAX_SAFE_INTEGER }, () => {});').length, 1, "unsupported");
  assert.equal(offenders('test("x", { timeout: ms() }, () => {});').length, 1, "unsupported");
  assert.equal(offenders('const timeout = 900_000;\ntest("x", { timeout }, () => {});').length, 1, "shorthand");
  assert.deepEqual(offenders('assert.deepEqual(calls, [{ timeout: 60_000 }]);\nconst opts = { timeout: 9e9 };'), [],
    "data fields outside test options are not options");
  assert.equal(offenders('before(() => {}, { timeout: 20 * 60_000 });').length, 1, "hook options");
  assert.deepEqual(offenders('/* unterminated comment test("x", { timeout: 1 })'), [], "malformed input terminates");
  assert.deepEqual(offenders('const s = "unterminated'), [], "malformed input terminates");
});

test("no test timeout option can outlast the hung-file window", () => {
  const offenders: string[] = [];
  let options = 0;
  for (const name of readdirSync(new URL("./", import.meta.url), { recursive: true })) {
    const file = String(name);
    if (!file.endsWith(".ts")) continue;
    const source = readFileSync(new URL(file, import.meta.url), "utf8");
    options += timeoutOptions(source).length;
    offenders.push(...timeoutOffenders(file, source, TIMEOUT_LIMIT));
  }
  assert.deepEqual(offenders, []);
  assert.ok(options >= 100, `the scan must see the suite's timeout options (saw ${options})`);
});

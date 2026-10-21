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
    "UZI_AGENT_TEST_REPORT_DIR", "UZI_AGENT_TEST_REPORT_RUN_DIR", "UZI_AGENT_TEST_REPORT_TOTAL",
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

function launch(root: string, extra: NodeJS.ProcessEnv = {}) {
  const child = spawn(process.execPath, ["--import", "tsx", "test/run-unit-tests.ts"], {
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
    await until(() => (readJson<Progress>(progressFile)?.failures ?? []).some((failure) => failure.name === "fails early"),
      20_000, "the early failure in progress.json");
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
    files_total: 3, files, counts: { tests: 0, pass: 0, fail: 0, cancelled: 0, skip: 0, todo: 0, file_failures: 0, suite_failures: 0 },
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
// keep every literal timeout option well below the window. A non-literal value is checked by hand:
// add it to KNOWN with its resolved value.
test("no test timeout option can outlast the hung-file window", () => {
  const limit = DEFAULT_STALL_MS - 120_000;
  const KNOWN: Record<string, number> = {};
  const option = /\b(?:test|it|describe|suite)(?:\.(?:only|skip|todo))?\(\s*(?:"(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'|`[^`]*`|[\w.]+)\s*,\s*\{([^{}]*)\}/g;
  const offenders: string[] = [];
  for (const name of readdirSync(new URL("./", import.meta.url), { recursive: true })) {
    const file = String(name);
    if (!file.endsWith(".ts")) continue;
    const source = readFileSync(new URL(file, import.meta.url), "utf8");
    for (const match of source.matchAll(option)) {
      const value = /(?:^|[\s,])timeout\s*:\s*([^,}\s]+)/.exec(match[1]!)?.[1];
      if (value === undefined) continue;
      const literal = /^[\d_]+$/.test(value) ? Number(value.replaceAll("_", "")) : undefined;
      const constant = new RegExp(`\\bconst\\s+${value.replace(/[^\w]/g, "")}\\s*=\\s*([\\d_]+)\\s*;`).exec(source)?.[1];
      const resolved = literal ?? (constant === undefined ? KNOWN[`${file}:${value}`] : Number(constant.replaceAll("_", "")));
      if (resolved === undefined) offenders.push(`${file}: unresolved timeout ${value}`);
      else if (resolved > limit) offenders.push(`${file}: timeout ${resolved} > ${limit}`);
    }
  }
  assert.deepEqual(offenders, []);
});

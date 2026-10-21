import assert from "node:assert/strict";
import { spawn, spawnSync } from "node:child_process";
import { cpSync, existsSync, mkdirSync, mkdtempSync, readFileSync, realpathSync, rmSync, unlinkSync, writeFileSync } from "node:fs";
import { createRequire } from "node:module";
import { fileURLToPath } from "node:url";
import { selectAgentTests } from "./agent-test-shards.js";
import { tmpdir } from "node:os";
import path from "node:path";
import test from "node:test";

const earlyFile = "000-unpinned-early.test.ts.mjs";
const survivingFiles = [
  "config.test.ts.mjs",
  "runner-held-publication-fallbacks.test.ts.mjs",
  "runner-midturn-checkpoint.test.ts.mjs",
  "runner-retained-episode.test.ts.mjs",
  "scheduler.test.ts.mjs",
  "unit-utils.test.ts.mjs",
  "worker.test.ts.mjs",
];

// Keep fixture suffix adaptation at the execution seam, outside production validation.
function shardInvocation(files: readonly string[], shard: number): string[] {
  const canonical = files.map((file) => `test/${file.slice(0, -4)}`);
  return selectAgentTests(canonical, {
    "1/2": ["test/runner-held-publication-fallbacks.test.ts"],
    "2/2": ["test/runner-midturn-checkpoint.test.ts", "test/runner-retained-episode.test.ts"],
  }, `${shard}/2`)
    .map((file) => `${file.slice(5)}.mjs`);
}

function writeFixture(directory: string, filename: string): void {
  writeFileSync(
    path.join(directory, filename),
    `import test from "node:test";\ntest(${JSON.stringify(`fixture:${filename}`)}, () => {});\n`,
  );
}

function observeMembership(directory: string, files: readonly string[]): Map<string, number> {
  const membership = new Map<string, number>();
  const env = { ...process.env };
  delete env.NODE_TEST_CONTEXT; // Each child must start an independent node:test runner.
  // Exactly two sequential children per observation; a failed child blocks the
  // observation. Each child has a 30s process bound as well as Node's test cap.
  for (const shard of [1, 2]) {
    const args = [
      "--test",
      "--test-reporter=tap",
      "--test-timeout=120000",
      ...shardInvocation(files, shard),
    ];
    const child = spawnSync(process.execPath, args, {
      cwd: directory,
      env,
      encoding: "utf8",
      timeout: 30_000,
      maxBuffer: 1024 * 1024,
    });
    const diagnostic = `shard ${shard}: ${child.stdout}\n${child.stderr}`;
    assert.ifError(child.error);
    assert.equal(child.signal, null, diagnostic);
    assert.equal(child.status, 0, diagnostic);
    assert.match(child.stdout, /^# fail 0$/m, diagnostic);
    assert.match(child.stdout, /^# cancelled 0$/m, diagnostic);

    const observed = [...child.stdout.matchAll(/^ok \d+ - fixture:(.+)$/gm)]
      .map((match) => match[1]!);
    assert.ok(observed.length > 0, `shard ${shard} must execute fixtures`);
    for (const filename of observed) {
      assert.ok(files.includes(filename), `unexpected fixture: ${filename}`);
      assert.ok(!membership.has(filename), `shards must be disjoint: ${filename}`);
      membership.set(filename, shard);
    }
  }
  assert.deepEqual(
    [...membership.keys()].sort(),
    [...files].sort(),
    "shard union must cover the entire fixture inventory",
  );
  return membership;
}

const emptyPins = { "1/2": [], "2/2": [] };
const vectors = [
  // SHA-256 first bytes: e7, 0c, 36, 68, 0b respectively.
  ["test/a.test.ts", "2/2"],
  ["test/b.test.ts", "1/2"],
  ["test/worker.test.ts", "1/2"],
  ["test/config.test.ts", "1/2"],
  ["test/é.test.ts", "2/2"],
] as const;
const inventory = vectors.map(([file]) => file);

test("exact UTF-8 SHA-256 vectors, sorted results, order independence, and pin override", () => {
  for (const shard of ["1/2", "2/2"]) {
    const expected = vectors.filter(([, expectedShard]) => expectedShard === shard)
      .map(([file]) => file).sort();
    assert.deepEqual(selectAgentTests(inventory, emptyPins, shard), expected);
    assert.deepEqual(selectAgentTests([...inventory].reverse(), emptyPins, shard), expected);
  }
  const pins = { "1/2": ["test/a.test.ts"], "2/2": ["test/b.test.ts"] };
  assert.ok(selectAgentTests(inventory, pins, "1/2").includes("test/a.test.ts"));
  assert.ok(selectAgentTests(inventory, pins, "2/2").includes("test/b.test.ts"));
  assert.deepEqual(selectAgentTests(inventory, pins), [...inventory].sort());
});

test("unpinned add/delete and rename never move another path or edit pins", () => {
  const pins = { "1/2": ["test/a.test.ts"], "2/2": [] };
  const original = JSON.stringify(pins);
  const variants = [
    [...inventory, "test/new.test.ts"],
    inventory.filter((file) => file !== "test/worker.test.ts"),
    inventory.map((file) => file === "test/worker.test.ts" ? "test/renamed.test.ts" : file),
  ];
  for (const changed of variants) {
    for (const shard of ["1/2", "2/2"]) {
      const before = selectAgentTests(inventory, pins, shard);
      const after = selectAgentTests(changed, pins, shard);
      for (const file of inventory.filter((file) => changed.includes(file))) {
        assert.equal(after.includes(file), before.includes(file), file);
      }
    }
  }
  assert.equal(JSON.stringify(pins), original);
  assert.throws(() => selectAgentTests(inventory.filter((file) => file !== "test/a.test.ts"), pins), /stale/);
});

test("pin structures, canonical names, sortedness, duplicates and stale entries fail closed", () => {
  const badPins: unknown[] = [
    null, [], {}, { ...emptyPins, extra: [] },
    { "1/2": [], "2/2": "test/a.test.ts" },
    { "1/2": [1], "2/2": [] },
    { "1/2": ["test/a.test.ts", "test/a.test.ts"], "2/2": [] },
    { "1/2": ["test/a.test.ts"], "2/2": ["test/a.test.ts"] },
    { "1/2": ["test/b.test.ts", "test/a.test.ts"], "2/2": [] },
    ...["test/missing.test.ts", "/test/a.test.ts", "test/../a.test.ts",
      "test/sub/a.test.ts", "test\\a.test.ts", "test/a.ts", "test/a.test.ts.mjs"]
      .map((file) => ({ "1/2": [file], "2/2": [] })),
  ];
  for (const pins of badPins) {
    for (const shard of [undefined, "1/2"]) {
      assert.throws(() => selectAgentTests(inventory, pins, shard), Error, JSON.stringify(pins));
    }
  }
  for (const invalid of ["1/3", "0/2", "3/2", "1", ""]) {
    assert.throws(() => selectAgentTests(inventory, emptyPins, invalid), /Shard/);
  }
  for (const invalid of ["../test/a.test.ts", "/test/a.test.ts", "test/sub/a.test.ts", "test/a.test.ts.mjs",
    "test/\u009bhidden.test.ts", "test/\u061chidden.test.ts", "test/\u200ehidden.test.ts",
    "test/\u2028hidden.test.ts", "test/\u202eforged.test.ts", "test/\u2066hidden.test.ts"]) {
    assert.throws(() => selectAgentTests([invalid], emptyPins), /Inventory/);
  }
  assert.throws(() => selectAgentTests([], emptyPins), /Inventory/);
  assert.throws(() => selectAgentTests([inventory[0]!, inventory[0]!], emptyPins), /Inventory/);
  assert.throws(() => selectAgentTests(["test/a.test.ts"], emptyPins, "1/2"), /Empty shard/);
});

const agentRoot = fileURLToPath(new URL("../", import.meta.url));
const fixtureNames = ["a", "b", "config", "worker"];
const require = createRequire(import.meta.url);

function launcherFixture(): string {
  const root = mkdtempSync(path.join(tmpdir(), "uzi-launcher-"));
  mkdirSync(path.join(root, "test/setup"), { recursive: true });
  mkdirSync(path.join(root, "node_modules"));
  writeFileSync(path.join(root, "package.json"), '{"type":"module"}\n');
  // Real installed dependencies, copied rather than symlinked across file boundaries.
  for (const dependency of ["tsx", "esbuild"]) {
    const installed = path.dirname(require.resolve(dependency + "/package.json"));
    cpSync(realpathSync(installed), path.join(root, "node_modules", dependency), { recursive: true, dereference: true });
  }
  cpSync(realpathSync(path.join(agentRoot, "node_modules/@esbuild")),
    path.join(root, "node_modules/@esbuild"), { recursive: true, dereference: true });
  for (const file of ["run-unit-tests.ts", "agent-test-shards.ts", "agent-report.ts", "agent-report-reporter.ts"]) {
    cpSync(path.join(agentRoot, "test", file), path.join(root, "test", file));
  }
  writeFileSync(path.join(root, "test-shards.json"), JSON.stringify(emptyPins));
  writeFileSync(path.join(root, "test/setup/hermetic-proc.ts"), [
    'process.env.FIXTURE_PRELOADED = "yes";',

  ].join("\n"));
  for (const name of fixtureNames) {
    writeFileSync(path.join(root, "test", name + ".test.ts"), [
      'import assert from "node:assert/strict";',
      'import test from "node:test";',
      'import { appendFileSync } from "node:fs";',
      'test("fixture:' + name + '", { only: ' + String(name === "a") + ' }, () => {',
      '  assert.equal(process.env.FIXTURE_PRELOADED, "yes");',
      '  assert.ok(process.execArgv.includes("--test-timeout=120000"));',
      '  if (process.env.UZI_AGENT_TEST_CONCURRENCY) assert.ok(process.execArgv.includes("--test-concurrency=" + process.env.UZI_AGENT_TEST_CONCURRENCY));',
      '  assert.ok(process.execArgv.includes("tsx"));',
      '  assert.ok(process.execArgv.includes("./test/setup/hermetic-proc.ts"));',
      '  assert.ok(!process.execArgv.some(arg => arg.startsWith("--test-shard")));',
      '  assert.equal(process.env.FIXTURE_INHERITED, "preserved");',
      '  if (process.env.FIXTURE_EXECUTED) appendFileSync(process.env.FIXTURE_EXECUTED, ' + JSON.stringify(name + "\n") + ');',
      '  if (process.env.FIXTURE_FAIL) assert.fail("intentional fixture failure");',
      '});',
    ].join("\n"));
  }
  return root;
}

function launcherEnv(extra: NodeJS.ProcessEnv = {}): NodeJS.ProcessEnv {
  const env: NodeJS.ProcessEnv = { ...process.env, FIXTURE_INHERITED: "preserved", ...extra };
  delete env.NODE_TEST_CONTEXT;
  delete env.UZI_AGENT_TEST_SHARD;
  delete env.UZI_AGENT_TEST_CONCURRENCY;
  delete env.UZI_AGENT_TEST_JUNIT;
  delete env.UZI_AGENT_TEST_REPORT_DIR;
  delete env.UZI_AGENT_TEST_REPORT_RUN_DIR;
  return { ...env, ...extra };
}

function runLauncher(root: string, extra: NodeJS.ProcessEnv = {}, args: string[] = []) {
  return spawnSync(process.execPath, ["--import", "tsx", "test/run-unit-tests.ts", ...args], {
    cwd: root, env: launcherEnv(extra), encoding: "utf8", timeout: 30_000,
  });
}

test("actual launcher runs the full inventory, each shard, filters, reporters and child failures", () => {
  const root = launcherFixture();
  try {
    const junit = path.join(root, "report.xml");
    const full = runLauncher(root, { UZI_AGENT_TEST_CONCURRENCY: "1", UZI_AGENT_TEST_JUNIT: junit });
    assert.ifError(full.error);
    assert.equal(full.status, 0, full.stdout + full.stderr);
    const xml = readFileSync(junit, "utf8");
    for (const name of fixtureNames) assert.ok(xml.includes("fixture:" + name), xml);
    assert.match(full.stdout, /fixture:a/);
    const union = new Set<string>();
    for (const shard of ["1/2", "2/2"]) {
      const result = runLauncher(root, { UZI_AGENT_TEST_SHARD: shard });
      assert.ifError(result.error);
      assert.equal(result.status, 0, result.stdout + result.stderr);
      const observed = [...result.stdout.matchAll(/(?:ok \d+ - |✔ )fixture:(\w+)/g)].map((match) => match[1]!);
      const expected = selectAgentTests(fixtureNames.map((name) => "test/" + name + ".test.ts"), emptyPins, shard)
        .map((file) => path.basename(file, ".test.ts"));
      assert.deepEqual(observed.sort(), expected.sort());
      for (const name of observed) {
        assert.ok(!union.has(name));
        union.add(name);
      }
    }
    assert.deepEqual([...union].sort(), fixtureNames.toSorted());
    const executed = path.join(root, "executed");
    const assertFiltered = (args: string[], expected: string[]): void => {
      writeFileSync(executed, "");
      const result = runLauncher(root, { FIXTURE_EXECUTED: executed }, args);
      assert.equal(result.status, 0, result.stdout + result.stderr);
      const observed = readFileSync(executed, "utf8").split("\n").filter(Boolean).sort();
      assert.deepEqual(observed, expected.toSorted(), "filters must control the exact executed fixture set");
    };
    assertFiltered(["--test-name-pattern", "fixture:a"], ["a"]);
    assertFiltered(["--test-skip-pattern=fixture:b"], ["a", "config", "worker"]);
    assertFiltered(["--test-name-pattern=--unmatched"], []);
    assertFiltered(["--test-only"], ["a"]);
    const exactExit = path.join(root, "exit.cjs");
    writeFileSync(exactExit, 'if (process.execArgv.includes("--test")) process.exit(7);\n');
    const exited = runLauncher(root, { NODE_OPTIONS: `--require=${JSON.stringify(exactExit)}` });
    assert.equal(exited.status, 7, exited.stdout + exited.stderr);
    const failed = runLauncher(root, { FIXTURE_FAIL: "1" });
    assert.equal(failed.status, 1, failed.stdout + failed.stderr);
    assert.match(failed.stdout, /intentional fixture failure/);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("actual launcher rejects widening/override arguments, bad settings and malformed pins before spawning", () => {
  const root = launcherFixture();
  try {
    for (const args of [
      ["test/a.test.ts"], ["--test-shard=1/2"], ["--test-timeout=1"],
      ["--test-reporter=tap"], ["--test-reporter-destination=elsewhere"],
      ["--import=other"], ["--"], ["--test-name-pattern"], ["--test-name-pattern=["],
    ]) {
      const result = runLauncher(root, {}, args);
      assert.equal(result.status, 1, args.join(" ") + result.stdout + result.stderr);
      assert.doesNotMatch(result.stdout, /fixture:/);
    }
    for (const extra of [
      { UZI_AGENT_TEST_SHARD: "1/3" }, { UZI_AGENT_TEST_CONCURRENCY: "0" },
      { UZI_AGENT_TEST_CONCURRENCY: "x2" },
    ]) assert.equal(runLauncher(root, extra).status, 1);
    writeFileSync(path.join(root, "test-shards.json"), "{");
    assert.equal(runLauncher(root).status, 1);
    writeFileSync(path.join(root, "test-shards.json"), '{"1/2":["test/missing.test.ts"],"2/2":[]}');
    assert.equal(runLauncher(root).status, 1);
    writeFileSync(path.join(root, "test-shards.json"), JSON.stringify(emptyPins));
    for (const name of fixtureNames) unlinkSync(path.join(root, "test", name + ".test.ts"));
    const empty = runLauncher(root);
    assert.equal(empty.status, 1, empty.stdout + empty.stderr);
    assert.match(empty.stderr, /Inventory/);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

for (const route of ["argument", "pin"] as const) {
  test("actual launcher escapes rejected " + route + " diagnostics onto one safe line", () => {
    const root = launcherFixture();
    const payload = "forged\nPASS: forged audit row\u001b[2K\u009b2K\u202e";
    try {
      if (route === "pin") {
        writeFileSync(path.join(root, "test-shards.json"), JSON.stringify({
          "1/2": ["test/" + payload + ".test.ts"], "2/2": [],
        }));
      }
      const result = runLauncher(root, {}, route === "argument" ? ["--unknown-" + payload] : []);
      assert.equal(result.status, 1, result.stdout + result.stderr);
      assert.equal(result.stderr.includes("\nPASS:"), false, "no forged diagnostic row");
      for (const control of ["\u001b", "\u009b", "\u202e"]) {
        assert.equal(result.stderr.includes(control), false, "no raw terminal or bidi controls");
      }
      assert.match(result.stderr, /\\nPASS: forged audit row/);
      assert.match(result.stderr, /\\u001b/);
      assert.match(result.stderr, /\\u009b/);
      assert.match(result.stderr, /\\u202e/);
      assert.equal(result.stderr.trimEnd().split("\n").length, 1);
    } finally {
      rmSync(root, { recursive: true, force: true });
    }
  });
}

test("package child-argv guard supports spaces and quotes in TMPDIR", () => {
  const root = mkdtempSync(path.join(tmpdir(), "uzi-shard-argv-"));
  const spaced = path.join(root, 'space "quoted" tmp');
  mkdirSync(spaced);
  // It runs the real package script from agent/: its stub launcher must not touch the real report
  // (the live gate's own latest.json, which agents follow).
  const latest = path.join(agentRoot, ".test-report", "latest.json");
  const before = existsSync(latest) ? readFileSync(latest, "utf8") : null;
  try {
    const script = fileURLToPath(new URL("../../scripts/real-procfs-skip-check.test.sh", import.meta.url));
    const source = readFileSync(script, "utf8");
    const start = source.indexOf("# (k) the other half of the cap");
    const end = source.indexOf("# (h) end to end through the real TypeScript recorder", start);
    assert.ok(start >= 0 && end > start, "use the actual package-child guard case");
    const body = 'set -eu\nROOT="$1"\nTMP="$2"\nfailures=0\n'
      + source.slice(start, end) + '\ntest "$failures" -eq 0\n';
    const result = spawnSync("sh", ["-c", body, "package-child-guard", path.resolve(agentRoot, ".."), spaced], {
      cwd: agentRoot, env: launcherEnv({ TMPDIR: spaced }), encoding: "utf8", timeout: 30_000,
    });
    assert.ifError(result.error);
    assert.equal(result.status, 0, result.stdout + result.stderr);
    assert.match(result.stdout, /package.json test script: UZI_AGENT_TEST_CONCURRENCY=1 passes/);
    assert.match(result.stdout, /unset UZI_AGENT_TEST_CONCURRENCY passes no/);
    assert.equal(existsSync(latest) ? readFileSync(latest, "utf8") : null, before, "the real agent/.test-report/latest.json is untouched");
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

function alive(pid: number): boolean {
  try { process.kill(pid, 0); return true; } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ESRCH") return false;
    throw error;
  }
}

for (const target of ["SIGINT", "SIGTERM", "child-SIGTERM", "SIGTERM-ignored"] as const) {
  test("actual launcher waits for owned child cleanup after " + target, { timeout: 15_000 }, async () => {
    const root = launcherFixture();
    const ready = path.join(root, "ready");
    const handshake = path.join(root, "ready.cjs");
    // NODE_OPTIONS preloads into the runner itself; --import runs in file workers.
    writeFileSync(handshake, [
      'if (process.execArgv.includes("--test")) {',
      ...(target === "SIGTERM-ignored" ? ['  process.on("SIGTERM", () => {});'] : []),
      '  require("node:fs").writeFileSync(process.env.FIXTURE_READY, String(process.pid));',
      // Bound the readiness hold to ten seconds; signals arrive before any file workers start.
      '  Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, 10000);',
      '  process.exit(99);',
      '}',
    ].join("\n"));
    const launcher = spawn(process.execPath, ["--import", "tsx", "test/run-unit-tests.ts"], {
      cwd: root, env: launcherEnv({ FIXTURE_READY: ready, NODE_OPTIONS: `--require=${JSON.stringify(handshake)}` }),
      stdio: ["ignore", "pipe", "pipe"],
    });
    const output: string[] = [];
    launcher.stdout.on("data", (data: Buffer) => output.push(data.toString()));
    launcher.stderr.on("data", (data: Buffer) => output.push(data.toString()));
    const closed = new Promise<{ code: number | null; signal: NodeJS.Signals | null }>((resolve, reject) => {
      launcher.on("error", reject);
      launcher.on("close", (code, signal) => resolve({ code, signal }));
    });
    let ownedPid: number | undefined;
    try {
      // Bounded readiness polling, rather than racing a signal against startup.
      const deadline = Date.now() + 8_000;
      while (!existsSync(ready) && Date.now() < deadline && launcher.exitCode === null) {
        await new Promise((resolve) => setTimeout(resolve, 20));
      }
      assert.ok(existsSync(ready), output.join(""));
      ownedPid = Number(readFileSync(ready, "utf8"));
      assert.ok(Number.isSafeInteger(ownedPid) && ownedPid > 1);
      assert.ok(alive(ownedPid));
      const sentAt = Date.now();
      if (target === "child-SIGTERM") process.kill(ownedPid, "SIGTERM");
      else launcher.kill(target === "SIGTERM-ignored" ? "SIGTERM" : target);
      const result = await closed;
      if (target === "SIGTERM-ignored") {
        assert.ok(Date.now() - sentAt < 6_000, "cleanup must escalate before the ten-second fixture hold expires");
      }
      assert.equal(result.signal, target === "SIGINT" ? "SIGINT" : "SIGTERM", output.join(""));
      assert.equal(result.code, null, output.join(""));
      assert.equal(alive(ownedPid), false, "owned test child must be reaped before launcher exits");
    } finally {
      if (ownedPid && alive(ownedPid)) process.kill(ownedPid, "SIGKILL");
      if (launcher.exitCode === null && launcher.signalCode === null) launcher.kill("SIGKILL");
      await closed;
      rmSync(root, { recursive: true, force: true });
    }
  });
}

// Separate top-level subtests ensure deletion is exercised even when addition fails.
for (const mutation of ["add", "delete"] as const) {
  test(`unpinned early-file ${mutation} preserves every surviving file membership`, () => {
    const directory = mkdtempSync(path.join(tmpdir(), "uzi-unit-test-shards-"));
    try {
      const beforeFiles = mutation === "add"
        ? [...survivingFiles]
        : [earlyFile, ...survivingFiles];
      for (const filename of beforeFiles) writeFixture(directory, filename);
      const before = observeMembership(directory, beforeFiles);

      if (mutation === "add") writeFixture(directory, earlyFile);
      else unlinkSync(path.join(directory, earlyFile));
      const afterFiles = mutation === "add"
        ? [earlyFile, ...survivingFiles]
        : [...survivingFiles];
      const after = observeMembership(directory, afterFiles);

      assert.deepEqual(
        survivingFiles.map((filename) => [filename, after.get(filename)]),
        survivingFiles.map((filename) => [filename, before.get(filename)]),
        `unpinned early-file ${mutation} must preserve every surviving file membership`,
      );
    } finally {
      rmSync(directory, { recursive: true, force: true });
    }
  });
}

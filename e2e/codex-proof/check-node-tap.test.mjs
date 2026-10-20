// Regression controls for check-node-tap.mjs (#3148). The fixtures are REAL `node --test
// --test-reporter=tap` streams produced by this Node, so a reporter format change fails here
// instead of silently passing a skipped packaged proof.
import { test } from "node:test";
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { checkTap } from "./check-node-tap.mjs";

const CHECKER = fileURLToPath(new URL("./check-node-tap.mjs", import.meta.url));

// A nested `node --test` that inherits NODE_TEST_CONTEXT from this runner reports to it in the
// internal child protocol instead of TAP, so the fixture runs get a clean environment.
const CHILD_ENV = { ...process.env };
delete CHILD_ENV.NODE_TEST_CONTEXT;

function realTap(body) {
  const dir = mkdtempSync(join(tmpdir(), "cdr-tap-"));
  try {
    const file = join(dir, "fixture.test.mjs");
    writeFileSync(file, `import { test, describe, it } from "node:test";\n${body}\n`);
    const r = spawnSync(process.execPath, ["--test", "--test-reporter=tap", file], { encoding: "utf8", env: CHILD_ENV });
    return r.stdout;
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
}

const CLEAN = 'test("a", () => {}); describe("d", () => { it("b", () => {}); });';

test("a clean run passes and reports its counts", () => {
  const r = checkTap(realTap(CLEAN));
  assert.equal(r.ok, true, r.problems.join("; "));
  assert.equal(r.summary.tests, 2);
});

test("a skipped test fails", () => {
  const r = checkTap(realTap(`${CLEAN} test("s", { skip: "why" }, () => {});`));
  assert.equal(r.ok, false);
  assert.ok(r.problems.some((p) => p.startsWith("skipped 1")), r.problems.join("; "));
});

test("a skipped describe fails although the summary counts no skip", () => {
  const tap = realTap(`${CLEAN} describe("gone", { skip: "image-only" }, () => { it("x", () => {}); });`);
  const r = checkTap(tap);
  assert.equal(r.ok, false);
  assert.match(tap, /^# skipped 0$/m, "precondition: node does not count a skipped describe");
  assert.ok(r.problems.some((p) => p.includes("SKIP directive")), r.problems.join("; "));
});

test("an all-skipped run fails", () => {
  const r = checkTap(realTap('test("s", { skip: true }, () => {});'));
  assert.equal(r.ok, false);
  assert.ok(r.problems.some((p) => p.startsWith("pass 0 != tests 1")), r.problems.join("; "));
});

test("a run that executes nothing fails", () => {
  const r = checkTap(realTap('describe("gone", { skip: true }, () => { it("x", () => {}); });'));
  assert.equal(r.ok, false);
  assert.ok(r.problems.some((p) => p.startsWith("executed 0 tests")), r.problems.join("; "));
});

test("a todo fails", () => {
  const r = checkTap(realTap(`${CLEAN} test("t", { todo: true }, () => {});`));
  assert.equal(r.ok, false);
  assert.ok(r.problems.some((p) => p.startsWith("todo 1")), r.problems.join("; "));
});

test("a failing test fails", () => {
  const r = checkTap(realTap(`${CLEAN} test("f", () => { throw new Error("boom"); });`));
  assert.equal(r.ok, false);
  assert.ok(r.problems.some((p) => p.startsWith("fail 1")), r.problems.join("; "));
});

test("console output cannot forge a clean summary", () => {
  const body = 'test("s", { skip: true }, () => {}); test("p", () => { console.log("# tests 1\\n# suites 0\\n# pass 1\\n# fail 0\\n# cancelled 0\\n# skipped 0\\n# todo 0\\n# duration_ms 1"); });';
  const r = checkTap(realTap(body));
  assert.equal(r.ok, false);
  assert.ok(r.problems.some((p) => p.startsWith("skipped 1")), r.problems.join("; "));
});

test("a truncated stream fails", () => {
  const tap = realTap(CLEAN);
  const cut = tap.slice(0, tap.indexOf("# pass"));
  const r = checkTap(cut);
  assert.equal(r.ok, false);
  assert.ok(r.problems[0].startsWith("summary incomplete"), r.problems.join("; "));
});

test("output with no summary fails", () => {
  const r = checkTap("Error: container exited before node started\n");
  assert.equal(r.ok, false);
  assert.match(r.problems[0], /no top-level/);
});

test("--min-tests raises the floor", () => {
  const r = checkTap(realTap(CLEAN), 3);
  assert.equal(r.ok, false);
  assert.ok(r.problems.some((p) => p.startsWith("executed 2 tests, need at least 3")), r.problems.join("; "));
});

test("the CLI exits 0 / 1 / 2 for pass / violation / unreadable input", () => {
  const dir = mkdtempSync(join(tmpdir(), "cdr-tap-cli-"));
  try {
    const good = join(dir, "good.tap");
    const bad = join(dir, "bad.tap");
    writeFileSync(good, realTap(CLEAN));
    writeFileSync(bad, realTap('test("s", { skip: true }, () => {});'));
    const run = (...a) => spawnSync(process.execPath, [CHECKER, ...a], { encoding: "utf8", env: CHILD_ENV }).status;
    assert.equal(run(good), 0);
    assert.equal(run(bad), 1);
    assert.equal(run(join(dir, "missing.tap")), 2);
    assert.equal(run(good, "--min-tests", "0"), 2);
    assert.equal(run(), 2);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

// Host launcher controls only; these tests never launch Docker or record proof evidence.
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { resolve } from "node:path";
import { test } from "node:test";

const launcher = resolve(__dirname, "../codex-private-ca/run.sh");
function helper(name: string, args: string[]) {
  return spawnSync("bash", ["-c", 'source "$1"; shift; "$@"', "private-ca-test", launcher, name, ...args], {
    encoding: "utf8", timeout: 5000, killSignal: "SIGKILL",
  });
}

test("portable private CA helper preserves successful output and nonzero child status", () => {
  const success = helper("bounded_child", ["2000", process.execPath, "-e", 'console.log("portable-child")']);
  assert.equal(success.error, undefined);
  assert.equal(success.status, 0);
  assert.equal(success.stdout, "portable-child\n");
  const failure = helper("bounded_child", ["2000", process.execPath, "-e", "process.exit(23)"]);
  assert.equal(failure.error, undefined);
  assert.equal(failure.status, 23);
});

test("portable private CA helper kills a TERM-resistant child and returns 124 at its deadline", () => {
  const started = Date.now();
  const result = helper("bounded_child", ["750", process.execPath, "-e",
    'process.on("SIGTERM", () => {}); console.log(process.pid); setInterval(() => {}, 1000)']);
  assert.equal(result.error, undefined);
  assert.equal(result.status, 124);
  assert.ok(Date.now() - started < 5000);
  const pid = Number(result.stdout.trim());
  assert.ok(Number.isSafeInteger(pid) && pid > 0, "child started before the deadline");
  assert.throws(() => process.kill(pid, 0), { code: "ESRCH" });
});

test("portable private CA helper rejects a missing executable", () => {
  const result = helper("bounded_child", ["2000", "/nonexistent/private-ca-cli"]);
  assert.equal(result.error, undefined);
  assert.equal(result.status, 1);
  assert.match(result.stderr, /ENOENT/);
});

test("portable private CA hashing matches the standard SHA-256 vector", () => {
  const dir = mkdtempSync(resolve(__dirname, "../../.uzi/scratch/ca-hash-"));
  try {
    const file = resolve(dir, "fixture with spaces");
    writeFileSync(file, "abc");
    const result = helper("fixture_hashes", [file]);
    assert.equal(result.error, undefined);
    assert.equal(result.status, 0);
    assert.equal(result.stdout, "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad  " + file + "\n");
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

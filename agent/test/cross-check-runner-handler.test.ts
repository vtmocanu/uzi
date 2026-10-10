import assert from "node:assert/strict";
import { execFile } from "node:child_process";
import { it } from "node:test";
import { fileURLToPath } from "node:url";

const testName = "outer checker uses real Read broker, actual HTTP verdict delivery and journaled child completion: approve round 1";
const pattern = "^" + testName.replace(/[.*+?^${}()|[\]\\]/g, "\\$&") + "$";
const cwd = fileURLToPath(new URL("../", import.meta.url));

for (const injection of ["verdict", "state", "both"]) {
 it("HTTP handler assertion reaches the test body after cleanup: " + injection, async () => {
  // Each child runs only the existing integration file. The timeout bounds one
  // attempt; a failure does not prevent the other injection cases from running.
  const env: NodeJS.ProcessEnv = { ...process.env, UZI_CROSS_CHECK_HANDLER_FAILURE: injection };
  // Start an independent runner, rather than inheriting the parent worker context.
  delete env.NODE_TEST_CONTEXT;
  const { error, stdout, stderr } = await new Promise<{
   error: (Error & { code?: string | number; killed?: boolean; signal?: string | null }) | null;
   stdout: string; stderr: string;
  }>((resolve) => {
   execFile(process.execPath, [
    "--import", "tsx", "--import", "./test/setup/hermetic-proc.ts",
    "--test", "--test-timeout=120000", "--test-reporter=tap",
    "--test-name-pattern", pattern, "test/cross-check-runner.test.ts",
   ], {
    cwd, env,
    timeout: 30_000, maxBuffer: 1024 * 1024,
   }, (error, stdout, stderr) => resolve({ error, stdout, stderr }));
  });
  const output = stdout + stderr;
  assert.ok(error, "injected assertion must fail the child\n" + output);
  assert.equal(error.killed, false, output);
  assert.equal(error.signal, null, output);
  assert.ok(typeof error.code === "number" && error.code > 0, output);
  assert.doesNotMatch(output, /unhandledRejection|unhandled rejection|UnhandledPromiseRejection/i);
  const observations = [...stdout.matchAll(/CROSS_CHECK_HANDLER_OBSERVATION (\{[^\n]*\})/g)];
  assert.equal(observations.length, 1, output);
  assert.deepEqual(JSON.parse(observations[0]![1]!), {
   injected: injection === "both" ? ["verdict", "state"] : [injection],
   executionSettled: true, executionFailed: false,
   verdictPosts: 1, terminalPosts: 1,
   journalCleared: true, registryRemoved: true, checkoutRemoved: true,
  });
  // Read the primary TAP error field, rather than a marker elsewhere in stdout.
  const primary = /^\s+error: (.+)$/m.exec(stdout);
  assert.ok(primary, output);
  assert.equal(primary[1]!.replace(/^['"]|['"]$/g, ""),
   "cross-check handler injected " + (injection === "state" ? "state" : "verdict") + " assertion", output);
  assert.match(stdout, /code: ['"]?ERR_ASSERTION['"]?/);
  assert.match(stdout, /# tests 1\b/);
  assert.match(stdout, /# fail 1\b/);
  assert.match(stdout, /# cancelled 0\b/);
 });
}

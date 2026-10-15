import { test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { spawnSync } from "node:child_process";

test("closure fixtures ignore poisoned inherited Git location variables in an isolated child", () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-closure-env-"));
  try {
    // Poison only a child environment, with paths beneath this disposable root.
    // Neither the parent environment nor any real repository is changed.
    const env: NodeJS.ProcessEnv = { ...process.env };
    delete env.NODE_TEST_CONTEXT; // The child must execute its own node:test runner.
    for (const key of Object.keys(env)) if (key.startsWith("GIT_")) delete env[key];
    Object.assign(env, {
      GIT_DIR: path.join(root, "absent-repository"),
      GIT_COMMON_DIR: path.join(root, "absent-common"),
      GIT_WORK_TREE: path.join(root, "absent-worktree"),
      GIT_INDEX_FILE: path.join(root, "absent-index"),
      GIT_OBJECT_DIRECTORY: path.join(root, "absent-objects"),
    });
    const result = spawnSync(process.execPath, [
      "--import", "tsx", "--import", "./test/setup/hermetic-proc.ts",
      "--test", "--test-timeout=120000", "--test-name-pattern=^N\\+1 shared-history",
      "test/git-recovery-closure-cache.test.ts",
    ], {
      cwd: path.resolve(import.meta.dirname, ".."), env,
      encoding: "utf8", timeout: 60_000, maxBuffer: 1024 * 1024,
    });
    assert.equal(result.error, undefined);
    assert.equal(result.signal, null);
    assert.equal(result.status, 0, result.stdout + result.stderr);
    assert.match(result.stdout, /pass 1/);
    assert.match(result.stdout, /fail 0/);
  } finally {
    fs.rmSync(root, { recursive: true, force: true });
  }
});
